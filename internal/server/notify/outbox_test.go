package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/timekey"
)

type outboxLatest struct {
	f     *fixture
	value float64
}

func (l *outboxLatest) Latest(context.Context, string) (ingest.LatestReport, error) {
	return ingest.LatestReport{ReceivedAt: l.f.now, Report: v1.Report{

		Metrics: v1.Metrics{CPUPercent: &l.value, MetricStatus: map[string]string{"cpu_percent": "ok"}, Sources: map[string]v1.SourceMetadata{"cpu": {SampleID: "cpu", SourceIntervalMS: 10000}}},
	}}, nil
}

func (f *fixture) evaluator(t *testing.T, nodes int) (*alerts.Evaluator, *outboxLatest) {
	t.Helper()
	tx, err := beginMainWrite(t, f.db, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range nodes {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		if _, err = tx.ExecContext(context.Background(), `INSERT INTO nodes(id,name,status,token_hash,first_enrolled_at,last_seen,created_at) VALUES(?,'node','unknown',?,?,?,?)`, id, []byte(id), timekey.Format(f.now), timekey.Format(f.now), timekey.Format(f.now)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	latest := &outboxLatest{f: f, value: 90}
	return alerts.NewEvaluator(alerts.NewService(mainHandle(t, f.db), nil), latest, alerts.EvaluatorOptions{Notifier: f.s, Now: func() time.Time { return f.now }}), latest
}

func (f *fixture) count(t *testing.T, query string, want int) {
	t.Helper()
	var got int
	if err := f.db.QueryRow(query).Scan(&got); err != nil || got != want {
		t.Fatalf("%s: got %d want %d: %v", query, got, want, err)
	}
}

func TestDeliveryFailureRollsBackAlertAndInboxAndRetries(t *testing.T) {
	f := newFixture(t)
	// 持续时间已满足后的失败不能重新开始计时。
	if _, err := alerts.NewService(mainHandle(t, f.db), nil).Save(context.Background(), f.rule.ID, alerts.Patch{ForSeconds: json.RawMessage(`60`)}); err != nil {
		t.Fatal(err)
	}
	e, latest := f.evaluator(t, 1)
	ctx := context.Background()
	if err := e.Evaluate(ctx, f.now); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	reject := func() {
		t.Helper()
		if _, err := f.db.Exec(`CREATE TRIGGER reject_delivery BEFORE INSERT ON notification_deliveries BEGIN SELECT RAISE(ABORT,'delivery unavailable'); END`); err != nil {
			t.Fatal(err)
		}
	}
	allow := func() {
		t.Helper()
		if _, err := f.db.Exec(`DROP TRIGGER reject_delivery`); err != nil {
			t.Fatal(err)
		}
	}
	reject()
	if err := e.Evaluate(ctx, f.now); err == nil {
		t.Fatal("delivery failure ignored")
	}
	f.count(t, "SELECT count(*) FROM alert_events", 0)
	f.count(t, "SELECT count(*) FROM inbox_notifications", 0)
	f.count(t, "SELECT count(*) FROM notification_deliveries", 0)
	if len(f.s.wake) != 0 {
		t.Fatal("failed transaction woke worker")
	}
	allow()
	if err := e.Evaluate(ctx, f.now); err != nil {
		t.Fatal(err)
	}
	f.count(t, "SELECT count(*) FROM alert_events WHERE state='firing'", 1)
	f.count(t, "SELECT count(*) FROM inbox_notifications", 1)
	f.count(t, "SELECT count(*) FROM notification_deliveries", 1)
	select {
	case <-f.s.wake:
	default:
		t.Fatal("successful commit did not wake worker")
	}
	latest.value = 10
	recoveredAt := f.now.Add(time.Second)
	f.now = recoveredAt
	reject()
	if err := e.Evaluate(ctx, f.now); err == nil {
		t.Fatal("resolution delivery failure ignored")
	}
	f.count(t, "SELECT count(*) FROM alert_events WHERE state='firing' AND resolved_at IS NULL", 1)
	f.count(t, "SELECT count(*) FROM inbox_notifications", 1)
	f.count(t, "SELECT count(*) FROM notification_deliveries", 1)
	if len(f.s.wake) != 0 {
		t.Fatal("failed resolution woke worker")
	}
	// 重试期间再次越限，仍须保留最初的恢复时间和值。
	latest.value = 90
	f.now = f.now.Add(time.Second)
	allow()
	if err := e.Evaluate(ctx, f.now); err != nil {
		t.Fatal(err)
	}
	f.count(t, "SELECT count(*) FROM alert_events WHERE state='resolved'", 1)
	f.count(t, "SELECT count(*) FROM inbox_notifications", 2)
	f.count(t, "SELECT count(*) FROM notification_deliveries", 2)
	var raw string
	if err := f.db.QueryRow("SELECT payload_json FROM notification_deliveries WHERE json_extract(payload_json,'$.state')='resolved'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	p, err := ParsePayload([]byte(raw))
	if err != nil || p.ResolvedAt == nil || !p.ResolvedAt.Equal(recoveredAt) || p.Value == nil || *p.Value != 10 {
		t.Fatalf("lost recovery intent: %+v %v", p, err)
	}
}

func TestCommittedAlertSurvivesServiceLossWithoutWorker(t *testing.T) {
	f := newFixture(t)
	var received []Payload
	var receivedMu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		receivedMu.Lock()
		received = append(received, p)
		receivedMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	f.target(t, ts.URL)
	e, latest := f.evaluator(t, 1)
	if err := e.Evaluate(context.Background(), f.now); err != nil {
		t.Fatal(err)
	}
	f.count(t, "SELECT count(*) FROM notification_deliveries WHERE state='pending'", 1)
	f.count(t, "SELECT count(*) FROM alert_events WHERE last_notified_at IS NULL", 1)
	receivedMu.Lock()
	beforeStart := len(received)
	receivedMu.Unlock()
	if beforeStart != 0 {
		t.Fatal("enqueue sent before worker start")
	}
	// 丢弃整个 Service（包括唤醒提示），只有同一数据库传给新实例。
	f.s = New(mainHandle(t, f.db), f.s.box, f.s.opts)
	e = alerts.NewEvaluator(alerts.NewService(mainHandle(t, f.db), nil), latest, alerts.EvaluatorOptions{Notifier: f.s})
	if err := e.Evaluate(context.Background(), f.now); err != nil {
		t.Fatal(err)
	}
	f.s.process(context.Background())
	f.s.process(context.Background())
	f.count(t, "SELECT count(*) FROM notification_deliveries WHERE state='sent' AND attempts=1", 1)
	f.count(t, "SELECT count(*) FROM alert_events WHERE last_notified_at IS NOT NULL", 1)
	receivedMu.Lock()
	defer receivedMu.Unlock()
	if len(received) != 1 {
		t.Fatalf("received %d times", len(received))
	}
}

func TestMoreThan256AlertEventsPersistAndCoalesceWithoutReplay(t *testing.T) {
	f := newFixture(t)
	const events = 300
	var received []Payload
	var receivedMu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		receivedMu.Lock()
		received = append(received, p)
		receivedMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	f.target(t, ts.URL)
	e, latest := f.evaluator(t, events)
	if err := e.Evaluate(context.Background(), f.now); err != nil {
		t.Fatal(err)
	}
	f.count(t, "SELECT count(*) FROM alert_events", events)
	f.count(t, "SELECT count(*) FROM inbox_notifications", events)
	f.count(t, "SELECT count(*) FROM notification_deliveries", MaxPendingPerChannel)
	f.count(t, "SELECT sum(coalesced_count) FROM notification_deliveries", events)
	// 丢弃唤醒，重建评估器和通知服务不能给已合并事件补发单条通知。
	f.s = New(mainHandle(t, f.db), f.s.box, f.s.opts)
	e = alerts.NewEvaluator(alerts.NewService(mainHandle(t, f.db), nil), latest, alerts.EvaluatorOptions{Notifier: f.s})
	if err := e.Evaluate(context.Background(), f.now); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		for range 25 {
			f.s.process(context.Background())
		}
		f.now = f.now.Add(time.Minute)
	}
	f.count(t, "SELECT count(*) FROM notification_deliveries WHERE state='pending'", 0)
	receivedMu.Lock()
	defer receivedMu.Unlock()
	if len(received) != MaxPendingPerChannel {
		t.Fatalf("deliveries=%d", len(received))
	}
	total, summaries := 0, 0
	seen := make(map[string]bool)
	for _, p := range received {
		if seen[p.EventID] {
			t.Fatalf("event sent twice: %s", p.EventID)
		}
		seen[p.EventID] = true
		total += max(1, p.SummaryCount)
		if p.SummaryCount > 1 {
			summaries++
		}
	}
	if total != events || summaries != 1 {
		t.Fatalf("represented events=%d summaries=%d", total, summaries)
	}
}

func TestEnqueueEnabledChannelsAndIdempotency(t *testing.T) {
	f := newFixture(t)
	n := f.note("same-event")
	f.enqueue(t, n)
	f.enqueue(t, n)
	f.count(t, "SELECT count(*) FROM notification_deliveries", 1)
	n.NodeName += " "
	f.enqueue(t, n)
	f.count(t, "SELECT count(*) FROM notification_deliveries", 2)
	if _, err := f.s.Save(context.Background(), f.channel.ID, Patch{Enabled: new(bool)}); err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, f.note("disabled-channel"))
	f.count(t, "SELECT count(*) FROM notification_deliveries", 2)
}

func TestCoalescingRollbackKeepsOriginalSummary(t *testing.T) {
	f := newFixture(t)
	for i := range MaxPendingPerChannel {
		f.enqueue(t, f.note(fmt.Sprintf("event-%d", i)))
	}
	tx, err := beginMainWrite(t, f.db, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = f.s.Enqueue(context.Background(), tx, f.note("rolled-back-summary")); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	f.count(t, "SELECT sum(coalesced_count) FROM notification_deliveries", MaxPendingPerChannel)
	f.count(t, "SELECT count(*) FROM notification_deliveries WHERE event_id='rolled-back-summary'", 0)
}

func TestCoalescingNeverChangesLeasedPayload(t *testing.T) {
	f := newFixture(t)
	for i := range MaxPendingPerChannel {
		f.enqueue(t, f.note(fmt.Sprintf("leased-%d", i)))
	}
	work, ok, err := f.s.claim(context.Background(), f.channel.ID)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	// 让已租约行排在摘要候选首位，确保只有 lease_until 过滤才能保护它。
	if _, err = f.db.Exec("UPDATE notification_deliveries SET coalesced_count=5 WHERE id=?", work.id); err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, f.note("new-summary"))
	var raw string
	var coalesced int
	if err = f.db.QueryRow("SELECT payload_json,coalesced_count FROM notification_deliveries WHERE id=?", work.id).Scan(&raw, &coalesced); err != nil || raw != work.raw || coalesced != 5 {
		t.Fatalf("leased payload changed: coalesced=%d err=%v", coalesced, err)
	}
	f.count(t, "SELECT sum(coalesced_count) FROM notification_deliveries", MaxPendingPerChannel+5)
}

func TestCoalescingContinuesToEveryChannel(t *testing.T) {
	f := newFixture(t)
	for i := range MaxPendingPerChannel {
		f.enqueue(t, f.note(fmt.Sprintf("flood-%d", i)))
	}
	var patch Patch
	if err := json.Unmarshal([]byte(`{"name":"second","kind":"webhook","enabled":true,"config":{"url":"https://example.com/second"}}`), &patch); err != nil {
		t.Fatal(err)
	}
	second, err := f.s.Save(context.Background(), "", patch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec("INSERT INTO alert_rule_channels(rule_id,channel_id) VALUES(?,?)", f.rule.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	// 两个通道都达到阈值，通道遍历顺序（随 UUID 变化）不影响结果。
	if _, err = f.db.Exec(`INSERT INTO notification_deliveries(id,channel_id,event_id,state,next_attempt_at,payload_json,created_at,updated_at)
		SELECT id||'-second',?,event_id,state,next_attempt_at,payload_json,created_at,updated_at FROM notification_deliveries`, second.ID); err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, f.note("after-threshold"))
	for _, channel := range []string{f.channel.ID, second.ID} {
		var count, total int
		if err = f.db.QueryRow("SELECT count(*),sum(coalesced_count) FROM notification_deliveries WHERE channel_id=?", channel).Scan(&count, &total); err != nil || count != MaxPendingPerChannel || total != MaxPendingPerChannel+1 {
			t.Fatalf("channel %s: deliveries=%d represented=%d err=%v", channel, count, total, err)
		}
	}
}

func TestWakeIsNonblockingAndConsumedAfterCommit(t *testing.T) {
	f := newFixture(t)
	// 饱和的提示信号不承担事件存储职责。
	for range 300 {
		f.s.Wake()
	}
	if len(f.s.wake) != 1 {
		t.Fatal("wake signals not coalesced")
	}
	select {
	case <-f.s.wake:
	default:
		t.Fatal("missing saturated wake signal")
	}
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	f.target(t, ts.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.s.Run(ctx)
	// 等待 Run 启动，再验证提交后的提示被消费。
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.s.mu.Lock()
		started := f.s.cancel != nil
		f.s.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker not started")
		}
		time.Sleep(time.Millisecond)
	}
	f.enqueue(t, f.note("wake"))
	for (calls.Load() == 0 || len(f.s.wake) != 0) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	shutdownCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := f.s.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("wake delivery count=%d", calls.Load())
	}
	if len(f.s.wake) != 0 {
		t.Fatal("worker did not consume wake")
	}
}

func TestSilenceSkipsPersistentOutboxUntilRelease(t *testing.T) {
	f := newFixture(t)
	e, latest := f.evaluator(t, 1)
	ctx := context.Background()
	service := alerts.NewService(mainHandle(t, f.db), func() time.Time { return f.now })
	raw, _ := json.Marshal(map[string]any{"reason": "maintenance", "scope_kind": "all", "ends_at": f.now.Add(time.Hour)})
	var req alerts.SilenceRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateSilence(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := e.Evaluate(ctx, f.now); err != nil {
		t.Fatal(err)
	}
	f.count(t, "SELECT count(*) FROM alert_events WHERE silenced=1", 1)
	f.count(t, "SELECT count(*) FROM alert_events WHERE last_notified_at IS NOT NULL", 0)
	f.count(t, "SELECT count(*) FROM notification_rate_limits", 0)
	f.count(t, "SELECT count(*) FROM notification_deliveries", 0)
	f.count(t, "SELECT count(*) FROM inbox_notifications", 0)
	if err := e.Evaluate(ctx, f.now); err != nil {
		t.Fatal(err)
	}
	f.count(t, "SELECT count(*) FROM notification_deliveries", 0)
	f.now = f.now.Add(time.Hour)
	if err := e.Evaluate(ctx, f.now); err != nil {
		t.Fatal(err)
	}
	if err := e.Evaluate(ctx, f.now); err != nil {
		t.Fatal(err)
	}
	f.count(t, "SELECT count(*) FROM notification_deliveries", 1)
	f.count(t, "SELECT count(*) FROM inbox_notifications WHERE transition='firing'", 1)
	latest.value = 10
	if err := e.Evaluate(ctx, f.now); err != nil {
		t.Fatal(err)
	}
	f.count(t, "SELECT count(*) FROM notification_deliveries", 2)
	f.count(t, "SELECT count(*) FROM inbox_notifications WHERE transition='resolved'", 1)
}

func TestSilenceReleaseDeliveryFailurePreservesFlag(t *testing.T) {
	f := newFixture(t)
	e, _ := f.evaluator(t, 1)
	ctx := context.Background()
	if _, err := f.db.Exec(`INSERT INTO alert_silences(id,reason,scope_kind,starts_at,ends_at,created_at) VALUES('silence','maintenance','all',?,?,?)`, timekey.Format(f.now), timekey.Format(f.now.Add(time.Hour)), timekey.Format(f.now)); err != nil {
		t.Fatal(err)
	}
	if err := e.Evaluate(ctx, f.now); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Hour)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_silence_delivery BEFORE INSERT ON notification_deliveries BEGIN SELECT RAISE(ABORT,'delivery failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := e.Evaluate(ctx, f.now); err == nil {
		t.Fatal("release failure ignored")
	}
	f.count(t, "SELECT count(*) FROM alert_events WHERE silenced=1 AND state='firing'", 1)
	f.count(t, "SELECT count(*) FROM inbox_notifications", 0)
	f.count(t, "SELECT count(*) FROM notification_deliveries", 0)
	if _, err := f.db.Exec("DROP TRIGGER reject_silence_delivery"); err != nil {
		t.Fatal(err)
	}
	if err := e.Evaluate(ctx, f.now); err != nil {
		t.Fatal(err)
	}
	f.count(t, "SELECT count(*) FROM alert_events WHERE silenced=0 AND state='firing'", 1)
	f.count(t, "SELECT count(*) FROM notification_deliveries", 1)
}
