package notify

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

type fixture struct {
	db      *sql.DB
	s       *Service
	now     time.Time
	channel Channel
	rule    alerts.Rule
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	db, e := storage.Open(context.Background(), filepath.Join(dir, "notify.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	f := &fixture{db: db, now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	box, _ := secretbox.New(bytes.Repeat([]byte{7}, 32), "orbitbeacon notification channel v1")
	f.s = New(db, box, Options{Now: func() time.Time { return f.now }, AllowPrivateNetworks: true, AllowInsecureWebhooks: true})
	var p Patch
	json.Unmarshal([]byte(`{"name":"webhook","kind":"webhook","enabled":true,"config":{"url":"https://example.com/hook"},"secret":"never-expose-this-secret"}`), &p)
	f.channel, e = f.s.Save(context.Background(), "", p)
	if e != nil {
		t.Fatal(e)
	}
	var rp alerts.Patch
	json.Unmarshal([]byte(`{"name":"CPU","enabled":true,"scope_kind":"all","metric":"cpu_percent","comparator":">","threshold":80,"for_seconds":0,"severity":"warning","channel_ids":["`+f.channel.ID+`"]}`), &rp)
	f.rule, e = alerts.NewService(db, func() time.Time { return f.now }).Save(context.Background(), "", rp)
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func (f *fixture) note(id string) alerts.Notification {
	return alerts.Notification{EventID: id, Rule: f.rule, NodeID: "node", NodeName: "server", State: "firing", StartedAt: f.now}
}
func (f *fixture) target(t *testing.T, url string) {
	t.Helper()
	raw, _ := json.Marshal(Config{URL: url})
	if _, e := f.s.Save(context.Background(), f.channel.ID, Patch{Config: raw}); e != nil {
		t.Fatal(e)
	}
}
func state(t *testing.T, db *sql.DB) (string, int, string, string) {
	t.Helper()
	var s, code, next string
	var n int
	if e := db.QueryRow("SELECT state,attempts,last_error_code,next_attempt_at FROM notification_deliveries LIMIT 1").Scan(&s, &n, &code, &next); e != nil {
		t.Fatal(e)
	}
	return s, n, code, next
}
func TestDurableRetryBackoffAndRestart(t *testing.T) {
	f := newFixture(t)
	status := http.StatusServiceUnavailable
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(status)
		w.Write([]byte("secret response must never escape"))
	}))
	defer ts.Close()
	f.target(t, ts.URL)
	f.s.Notify(context.Background(), f.note("event"))
	want := []time.Duration{2 * time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
	for i := range 5 {
		f.s.process(context.Background())
		s, n, code, next := state(t, f.db)
		if n != i+1 || code != "remote_unavailable" {
			t.Fatalf("attempt %d: %s %d %s", i, s, n, code)
		}
		if i < 4 {
			if s != "pending" {
				t.Fatal(s)
			}
			stamp, _ := timekey.Parse(next)
			if stamp.Sub(f.now) != want[i] {
				t.Fatalf("backoff=%v", stamp.Sub(f.now))
			}
			f.now = stamp
		} else if s != "failed" {
			t.Fatal(s)
		}
	}
	// 待处理工作由数据库持有，而非内存队列。
	f.db.Exec("DELETE FROM notification_deliveries")
	f.s.Notify(context.Background(), f.note("restart"))
	box, _ := secretbox.New(bytes.Repeat([]byte{7}, 32), "orbitbeacon notification channel v1")
	restarted := New(f.db, box, f.s.opts)
	status = 200
	restarted.process(context.Background())
	if s, n, _, _ := state(t, f.db); s != "sent" || n != 1 {
		t.Fatalf("reload %s %d", s, n)
	}
}
func TestRateLimitCoalescingAndRetention(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	var summary atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var p Payload
		json.NewDecoder(r.Body).Decode(&p)
		if p.SummaryCount > 1 {
			summary.Store(true)
		}
		w.WriteHeader(204)
	}))
	defer ts.Close()
	f.target(t, ts.URL)
	for i := range 205 {
		n := f.note("event-" + strings.Repeat("x", i))
		f.s.Notify(context.Background(), n)
	}
	var count, total int
	f.db.QueryRow("SELECT count(*),sum(coalesced_count) FROM notification_deliveries WHERE state='pending'").Scan(&count, &total)
	if count != 100 || total != 205 {
		t.Fatalf("coalesce %d %d", count, total)
	}
	for range 25 {
		f.s.process(context.Background())
	}
	if calls.Load() != 20 {
		t.Fatalf("rate=%d", calls.Load())
	}
	// 重启必须保留已耗尽的速率窗口。
	restarted := New(f.db, f.s.box, f.s.opts)
	restarted.process(context.Background())
	if calls.Load() != 20 {
		t.Fatal("restart bypassed rate limit")
	}
	for range 5 {
		f.now = f.now.Add(time.Minute)
		for range 20 {
			restarted.process(context.Background())
		}
	}
	if calls.Load() != 100 || !summary.Load() {
		t.Fatalf("calls=%d summary=%v", calls.Load(), summary.Load())
	}
	f.now = f.now.Add(31 * 24 * time.Hour)
	removed, e := f.s.Cleanup(context.Background(), f.now, 16)
	if e != nil || removed != 16 {
		t.Fatalf("bounded retention %d %v", removed, e)
	}
}
func TestShutdownCancelsInflightAndRejectsQueue(t *testing.T) {
	f := newFixture(t)
	entered := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	defer ts.Close()
	f.target(t, ts.URL)
	f.s.Notify(context.Background(), f.note("shutdown"))
	go f.s.Run(context.Background())
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if e := f.s.Shutdown(ctx); e != nil {
		t.Fatal(e)
	}
	if time.Since(started) > time.Second {
		t.Fatal("shutdown budget exceeded")
	}
	f.s.Notify(context.Background(), f.note("after-stop"))
	var n int
	f.db.QueryRow("SELECT count(*) FROM notification_deliveries").Scan(&n)
	if n != 1 {
		t.Fatal("accepted after shutdown")
	}
	s, _, _, _ := state(t, f.db)
	if s != "pending" {
		t.Fatal("canceled delivery lost")
	}
}
func TestChannelSecretsAuditAndValidation(t *testing.T) {
	f := newFixture(t)
	ctx := audit.WithActor(context.Background(), "admin")
	raw := json.RawMessage(`"another-secret"`)
	c, e := f.s.Save(ctx, f.channel.ID, Patch{Secret: raw})
	if e != nil {
		t.Fatal(e)
	}
	data, _ := json.Marshal(c)
	if bytes.Contains(data, []byte("another-secret")) || !c.SecretSet {
		t.Fatal("secret exposed")
	}
	var encrypted []byte
	f.db.QueryRow("SELECT secret_ciphertext FROM notification_channels WHERE id=?", c.ID).Scan(&encrypted)
	if bytes.Contains(encrypted, []byte("another-secret")) {
		t.Fatal("plaintext in database")
	}
	var details string
	f.db.QueryRow("SELECT details FROM audit_events WHERE target_id=?", c.ID).Scan(&details)
	if strings.Contains(details, "secret") {
		t.Fatal("secret in audit")
	}
	for _, raw := range []string{`{"name":null}`, `{"enabled":null}`, `{"secret":null}`, `{"oops":1}`} {
		var p Patch
		if json.Unmarshal([]byte(raw), &p) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, raw := range []string{`{"url":"http://example.com"}`, `{"url":"https://user:password@example.com"}`, `{"url":"https://example.com:0"}`, `{"url":"https://example.com","password":"bad"}`} {
		if _, e := f.s.Save(ctx, c.ID, Patch{Config: json.RawMessage(raw)}); e == nil {
			t.Fatalf("accepted config %s", raw)
		}
	}
	if _, e = f.s.Save(ctx, c.ID, Patch{Secret: json.RawMessage(`""`)}); e != nil {
		t.Fatal(e)
	}
	c, _ = f.s.Get(ctx, c.ID)
	if c.SecretSet {
		t.Fatal("clear did not work")
	}
	// 审计失败须回滚通道写入，并阻止测试发送。
	f.db.Exec(`CREATE TRIGGER reject_channel_audit BEFORE INSERT ON audit_events WHEN NEW.action LIKE 'notification_channel.%' BEGIN SELECT RAISE(ABORT,'audit failure'); END`)
	name := "changed"
	if _, e = f.s.Save(ctx, c.ID, Patch{Name: &name}); e == nil {
		t.Fatal("audit failure ignored")
	}
	c, _ = f.s.Get(ctx, c.ID)
	if c.Name == name {
		t.Fatal("audit rollback failed")
	}
	if _, e = f.s.Test(ctx, c.ID); e == nil {
		t.Fatal("test not audited before send")
	}
}
func TestSynchronousTestTimeoutAndPayload(t *testing.T) {
	f := newFixture(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p Payload
		json.NewDecoder(r.Body).Decode(&p)
		if p.State != "test" {
			t.Error("bad test body")
		}
		w.WriteHeader(204)
	}))
	defer ts.Close()
	f.target(t, ts.URL)
	ctx := audit.WithActor(context.Background(), "admin")
	r, e := f.s.Test(ctx, f.channel.ID)
	if e != nil || !r.OK {
		t.Fatalf("test %+v %v", r, e)
	}
	var n int
	f.db.QueryRow("SELECT count(*) FROM audit_events WHERE action='notification_channel.test'").Scan(&n)
	if n != 1 {
		t.Fatal("test audit missing")
	}
	if f.s.sender.client.Timeout != 10*time.Second {
		t.Fatal("HTTP timeout changed")
	}
}

func TestFloodCoalescesAlreadyAttemptedRetries(t *testing.T) {
	f := newFixture(t)
	for i := range MaxPendingPerChannel {
		f.s.Notify(context.Background(), f.note("retry-"+strings.Repeat("x", i)))
	}
	_, e := f.db.Exec("UPDATE notification_deliveries SET attempts=1,next_attempt_at=?", timekey.Format(f.now.Add(time.Hour)))
	if e != nil {
		t.Fatal(e)
	}
	for i := range 20 {
		f.s.Notify(context.Background(), f.note("new-"+strings.Repeat("x", i)))
	}
	var count, total int
	f.db.QueryRow("SELECT count(*),sum(coalesced_count) FROM notification_deliveries").Scan(&count, &total)
	if count != 100 || total != 120 {
		t.Fatalf("retry flood not bounded: %d %d", count, total)
	}
	var id, raw string
	f.db.QueryRow("SELECT event_id,payload_json FROM notification_deliveries WHERE coalesced_count>1").Scan(&id, &raw)
	var p Payload
	json.Unmarshal([]byte(raw), &p)
	if id != p.EventID {
		t.Fatal("summary identity disagrees with payload")
	}
}
func TestShutdownCancelsBlockedEnqueue(t *testing.T) {
	f := newFixture(t)
	tx, e := f.db.BeginTx(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	done := make(chan struct{})
	go func() { defer close(done); f.s.Notify(context.Background(), f.note("blocked")) }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if e = f.s.Shutdown(ctx); e != nil {
		t.Fatal(e)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("shutdown waited on writer")
	}
	tx.Rollback()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("enqueue did not stop")
	}
}

func TestWorkerConcurrencyBound(t *testing.T) {
	f := newFixture(t)
	entered := make(chan struct{}, 10)
	release := make(chan struct{})
	var active, peak atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		n := active.Add(1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		active.Add(-1)
		w.WriteHeader(204)
	}))
	defer ts.Close()
	f.target(t, ts.URL)
	ids := []string{f.channel.ID}
	for range 4 {
		var p Patch
		json.Unmarshal([]byte(`{"name":"extra","kind":"webhook","enabled":true,"config":{"url":"`+ts.URL+`"}}`), &p)
		c, e := f.s.Save(context.Background(), "", p)
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, c.ID)
	}
	raw, _ := json.Marshal(ids)
	_, e := alerts.NewService(f.db, nil).Save(context.Background(), f.rule.ID, alerts.Patch{ChannelIDs: raw})
	if e != nil {
		t.Fatal(e)
	}
	f.s.Notify(context.Background(), f.note("parallel"))
	done := make(chan struct{})
	go func() { defer close(done); f.s.process(context.Background()) }()
	for range 4 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("four lanes did not run")
		}
	}
	if peak.Load() != 4 {
		t.Fatalf("concurrency=%d", peak.Load())
	}
	close(release)
	<-done
	var sent int
	f.db.QueryRow("SELECT count(*) FROM notification_deliveries WHERE state='sent'").Scan(&sent)
	if sent != 4 {
		t.Fatalf("sent=%d", sent)
	}
	f.s.process(context.Background())
	if peak.Load() > 4 {
		t.Fatal("concurrency exceeded four")
	}
}
func TestDeliveryHistoryIsSafeAndKeysetPaged(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		f.s.Notify(context.Background(), f.note("page-"+strings.Repeat("x", i)))
	}
	page, e := f.s.Deliveries(context.Background(), 2, "")
	if e != nil || len(page.Deliveries) != 2 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("page %+v %v", page, e)
	}
	next, e := f.s.Deliveries(context.Background(), 2, page.NextCursor)
	if e != nil || len(next.Deliveries) != 1 || next.HasMore {
		t.Fatalf("next %+v %v", next, e)
	}
	raw, _ := json.Marshal(page)
	if bytes.Contains(raw, []byte("never-expose")) || bytes.Contains(raw, []byte("payload_json")) || bytes.Contains(raw, []byte("example.com")) {
		t.Fatal("unsafe delivery projection")
	}
}

func TestChannelKindValidationAndInsecureOptIn(t *testing.T) {
	f := newFixture(t)
	for _, body := range []string{
		`{"name":"telegram","kind":"telegram","enabled":true,"config":{"chat_id":"-123"},"secret":"123:token"}`,
		`{"name":"email","kind":"email","enabled":true,"config":{"host":"smtp.example.com","port":587,"username":"user","from":"from@example.com","to":["to@example.com"]},"secret":"password"}`,
	} {
		var p Patch
		if e := json.Unmarshal([]byte(body), &p); e != nil {
			t.Fatal(e)
		}
		c, e := f.s.Save(context.Background(), "", p)
		if e != nil || !c.SecretSet {
			t.Fatalf("valid kind %s: %v", body, e)
		}
	}
	for _, body := range []string{
		`{"name":"telegram","kind":"telegram","enabled":true,"config":{"chat_id":"bad chat"},"secret":"123:token"}`,
		`{"name":"telegram","kind":"telegram","enabled":true,"config":{"chat_id":"-123"},"secret":"bad/token"}`,
		`{"name":"email","kind":"email","enabled":true,"config":{"host":"bad host","port":587,"from":"from@example.com","to":["to@example.com"]}}`,
		`{"name":"email","kind":"email","enabled":true,"config":{"host":"smtp.example.com","port":65536,"from":"from@example.com","to":["to@example.com"]}}`,
		`{"name":"email","kind":"email","enabled":true,"config":{"host":"smtp.example.com","port":587,"username":"user","from":"from@example.com","to":["to@example.com"]}}`,
		`{"name":"email","kind":"email","enabled":true,"config":{"host":"smtp.example.com","port":587,"from":"from@example.com\r\nBcc: injected","to":["to@example.com"]}}`,
		`{"name":"hook","kind":"webhook","enabled":true,"config":{"url":"https://bad..example"}}`,
		`{"name":"hook","kind":"webhook","enabled":true,"config":{"url":"https://example.com","port":0}}`,
	} {
		var p Patch
		if json.Unmarshal([]byte(body), &p) == nil {
			if _, e := f.s.Save(context.Background(), "", p); e == nil {
				t.Fatalf("invalid kind accepted: %s", body)
			}
		}
	}
	for _, opts := range []Options{{}, {AllowPrivateNetworks: true}, {AllowInsecureWebhooks: true}, {AllowPrivateNetworks: true, AllowInsecureWebhooks: true}} {
		s := New(f.db, f.s.box, opts)
		var p Patch
		json.Unmarshal([]byte(`{"name":"local","kind":"webhook","enabled":true,"config":{"url":"http://127.0.0.1:8080"}}`), &p)
		_, e := s.Save(context.Background(), "", p)
		if (e == nil) != opts.AllowInsecureWebhooks {
			t.Fatalf("insecure setting ignored: %+v", opts)
		}
	}
}
