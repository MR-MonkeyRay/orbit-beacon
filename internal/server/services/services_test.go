package services

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/egress"
	"orbitbeacon/internal/server/storage"
)

func ptr[T any](v T) *T { return &v }
func validService() Service {
	return Service{Name: "Health", Type: "http", Target: "https://example.com/health", IntervalSeconds: 60, TimeoutSeconds: 10, FailureThreshold: 2, Enabled: true}
}
func fixture(t *testing.T, notifier Notifier) (*Manager, *storage.Database, *storage.Database) {
	t.Helper()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	db, e := storage.OpenMain(t.Context(), filepath.Join(dir, "main.db"))
	if e != nil {
		t.Fatal(e)
	}
	hist, e := storage.OpenHistory(t.Context(), filepath.Join(dir, "history.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { hist.Close(); db.Close() })
	return New(db, hist, func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) }, nil, notifier, nil), db, hist
}
func create(t *testing.T, m *Manager) Service {
	t.Helper()
	s, e := m.Save(t.Context(), "", Patch{Name: ptr("Health"), Type: ptr("http"), Target: ptr("https://example.com"), Public: ptr(true), Notify: ptr(true)})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestValidation(t *testing.T) {
	for _, tt := range []struct {
		kind, target string
		valid        bool
	}{{"http", "https://example.com/a?b=c", true}, {"http", "http://127.0.0.1:8080", true}, {"http", "https://[::1]/", true}, {"http", "https://a:b@example.com", false}, {"http", "/relative", false}, {"http", "file:///etc/passwd", false}, {"http", "https://example.com:65536", false}, {"tcp", "localhost:1", true}, {"tcp", "[::1]:65535", true}, {"tcp", "a:0", false}, {"tcp", "a:65536", false}, {"tcp", "http://a:80", false}, {"icmp", "::1", true}, {"icmp", "localhost", true}, {"icmp", "example.com:80", false}, {"icmp", "[::1]", false}, {"icmp", "http://example.com", false}} {
		t.Run(tt.kind+tt.target, func(t *testing.T) {
			s := validService()
			s.Type = tt.kind
			s.Target = tt.target
			if e := Validate(s); (e == nil) != tt.valid {
				t.Fatalf("validation %v", e)
			}
		})
	}
	for _, mutate := range []func(*Service){func(s *Service) { s.Name = "" }, func(s *Service) { s.Name = strings.Repeat("界", 101) }, func(s *Service) { s.Name = "a\nb" }, func(s *Service) { s.IntervalSeconds = 29 }, func(s *Service) { s.IntervalSeconds = 3601 }, func(s *Service) { s.TimeoutSeconds = 0 }, func(s *Service) { s.TimeoutSeconds = 31 }, func(s *Service) { s.IntervalSeconds = 30; s.TimeoutSeconds = 30 }, func(s *Service) { s.LatencyThresholdMS = -1 }} {
		s := validService()
		mutate(&s)
		if Validate(s) == nil {
			t.Fatal("invalid config accepted")
		}
	}
	s := validService()
	s.Name = strings.Repeat("界", 100)
	if e := Validate(s); e != nil {
		t.Fatal(e)
	}
}
func TestStateMachineAndCertificatePacing(t *testing.T) {
	s := validService()
	s.Notify = true
	s.LatencyThresholdMS = 50
	at := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	expiry := at.Add(13 * 24 * time.Hour)
	var previous *Result
	step := func(ok bool, latency float64, types ...string) {
		t.Helper()
		r, events := Advance(s, previous, Result{OK: ok, LatencyMS: ptr(latency), CheckedAt: at, TLSExpiresAt: &expiry, Error: map[bool]string{true: "", false: "timeout"}[ok]})
		if len(events) != len(types) {
			t.Fatalf("events=%+v want %v", events, types)
		}
		for i, event := range events {
			if event.Transition != types[i] || event.Name != s.Name || event.Target != s.Target {
				t.Fatalf("event=%+v", event)
			}
		}
		previous = &r
		at = at.Add(time.Minute)
	}
	step(false, 0, "certificate_expiring")
	if previous.Status != "unknown" {
		t.Fatal(previous)
	}
	step(false, 0, "down")
	step(false, 0)
	step(true, 100, "recovered")
	step(true, 100)
	step(true, 100, "latency_high")
	step(true, 100)
	step(true, 30, "latency_recovered")
	at = at.Add(24 * time.Hour)
	step(true, 20, "certificate_expiring")
	s.Notify = false
	step(false, 0)
	step(false, 0)
}
func TestHTTPAndTCPCheckers(t *testing.T) {
	var ua string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.UserAgent()
		if r.URL.Path == "/bad" {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/loop" {
			http.Redirect(w, r, "/loop", 302)
			return
		}
		if r.URL.Path == "/metadata" {
			http.Redirect(w, r, "http://169.254.169.254/", 302)
			return
		}
		if r.URL.Path == "/headers" {
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	c := NewChecker()
	s := validService()
	s.Target = server.URL
	r := c.Check(t.Context(), s)
	if !r.OK || r.StatusCode == nil || *r.StatusCode != 204 || ua != "OrbitBeacon" || r.LatencyMS == nil {
		t.Fatalf("%+v ua=%s", r, ua)
	}
	s.Target = server.URL + "/bad"
	r = c.Check(t.Context(), s)
	if r.OK || r.Error != "http_status" || *r.StatusCode != 503 {
		t.Fatal(r)
	}
	s.Target = server.URL + "/loop"
	if r = c.Check(t.Context(), s); r.OK {
		t.Fatal("redirect loop accepted")
	}
	s.Target = server.URL + "/metadata"
	if r = c.Check(t.Context(), s); r.Error != "denied" {
		t.Fatal(r)
	}
	s.Target = server.URL + "/headers"
	r = c.Check(t.Context(), s)
	if r.LatencyMS == nil || *r.LatencyMS > 90 {
		t.Fatalf("latency included body %v", r.LatencyMS)
	}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer tlsServer.Close()
	tlsClient := tlsServer.Client()
	transport := tlsClient.Transport.(*http.Transport)
	transport.DialContext = c.dialer.DialContext
	c.client = tlsClient
	s.Target = tlsServer.URL
	if r = c.Check(t.Context(), s); !r.OK || r.TLSExpiresAt == nil || !r.TLSExpiresAt.Equal(tlsServer.Certificate().NotAfter) {
		t.Fatal(r)
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	s.Type = "tcp"
	s.Target = listener.Addr().String()
	if r = c.Check(t.Context(), s); !r.OK || r.LatencyMS == nil {
		t.Fatal(r)
	}
	s.Target = "169.254.169.254:80"
	if r = c.Check(t.Context(), s); r.Error != "denied" {
		t.Fatal(r)
	}
}
func TestICMPEgressAndChecksum(t *testing.T) {
	c := NewChecker()
	s := validService()
	s.Type = "icmp"
	s.Target = "169.254.169.254"
	r := c.Check(t.Context(), s)
	expected := "denied"
	if !pingSupported {
		expected = "unsupported"
	}
	if r.Error != expected {
		t.Fatal(r)
	}
	packet := []byte{8, 0, 0, 0, 0, 1, 0, 1}
	sum := checksum(packet)
	packet[2] = byte(sum >> 8)
	packet[3] = byte(sum)
	if checksum(packet) != 0 {
		t.Fatal("checksum")
	}
	if errorCode(egress.ErrDenied) != "denied" || errorCode(errUnsupported) != "unsupported" {
		t.Fatal("error classification")
	}
}

type recordingNotifier struct {
	events []Event
	wakes  int
	fail   bool
}

func (n *recordingNotifier) EnqueueService(_ context.Context, tx *storage.Tx, e Event) error {
	if n.fail {
		return errors.New("outbox unavailable")
	}
	var count int
	if err := tx.QueryRowContext(context.Background(), "SELECT count(*) FROM inbox_notifications WHERE source_id=?", e.ID).Scan(&count); err != nil || count != 1 {
		return errors.New("inbox must precede outbox")
	}
	n.events = append(n.events, e)
	return nil
}
func (n *recordingNotifier) Wake() { n.wakes++ }
func TestStorageBufferNotificationsPublicAndRetention(t *testing.T) {
	notifier := &recordingNotifier{}
	m, db, hist := fixture(t, notifier)
	s := create(t, m)
	at := m.now()
	for i := range 2 {
		if e := m.Accept(t.Context(), s, Result{OK: false, Error: "timeout", CheckedAt: at.Add(time.Duration(i) * time.Second)}); e != nil {
			t.Fatal(e)
		}
	}
	if notifier.wakes != 0 || len(notifier.events) != 0 {
		t.Fatal("notified before flush")
	}
	lease, e := db.Acquire(t.Context(), storage.Reader)
	if e != nil {
		t.Fatal(e)
	}
	var n int
	lease.QueryRowContext(t.Context(), "SELECT count(*) FROM service_checks_latest").Scan(&n)
	lease.Close()
	if n != 0 {
		t.Fatal("unbuffered write")
	}
	notifier.fail = true
	if e = m.Flush(t.Context()); e == nil {
		t.Fatal("outbox failure ignored")
	}
	notifier.fail = false
	if e = m.Flush(t.Context()); e != nil {
		t.Fatal(e)
	}
	if notifier.wakes != 1 || len(notifier.events) != 1 {
		t.Fatalf("notifications %+v", notifier)
	}
	if e = m.Accept(t.Context(), s, Result{OK: true, LatencyMS: ptr(15.0), CheckedAt: at.Add(time.Minute)}); e != nil {
		t.Fatal(e)
	}
	if e = m.Flush(t.Context()); e != nil {
		t.Fatal(e)
	}
	history, e := m.History(t.Context(), s.ID, "1h")
	if e != nil {
		t.Fatal(e)
	}
	var sampled Point
	for _, p := range history.Buckets {
		if p.Availability != nil {
			sampled = p
		}
	}
	if sampled.Availability == nil || *sampled.Availability != 33.33 || *sampled.AvgLatencyMS != 15 || *sampled.MaxLatencyMS != 15 {
		t.Fatalf("history %+v", history)
	}
	public, e := m.Public(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(public)
	if len(public) != 1 || len(public[0].Availability) != 30 || strings.Contains(string(raw), "target") || strings.Contains(string(raw), "example.com") || strings.Contains(string(raw), "error") {
		t.Fatalf("public %s", raw)
	}
	recreated := New(db, hist, m.now, nil, notifier, nil)
	if e = recreated.Accept(t.Context(), s, Result{OK: false, Error: "timeout", CheckedAt: at.Add(2 * time.Minute)}); e != nil {
		t.Fatal(e)
	}
	if e = recreated.Flush(t.Context()); e != nil {
		t.Fatal(e)
	}
	current, e := recreated.Get(t.Context(), s.ID)
	if e != nil || current.Latest.Status != "up" {
		t.Fatalf("restart hysteresis %+v %v", current, e)
	}
	// Expired rows are removed in bounded history-domain batches; live config survives.
	lease, e = hist.Acquire(t.Context(), storage.Writer)
	if e != nil {
		t.Fatal(e)
	}
	tx, e := lease.BeginWrite(lease.Context())
	if e != nil {
		t.Fatal(e)
	}
	_, e = tx.ExecContext(lease.Context(), `INSERT INTO service_check_buckets VALUES(?,?,1,1,10,10,10)`, s.ID, at.Add(-31*24*time.Hour).Unix()/300*300)
	if e == nil {
		e = tx.Commit()
	}
	lease.Close()
	if e != nil {
		t.Fatal(e)
	}
	if n, e := m.Cleanup(t.Context(), at, 1); e != nil || n != 1 {
		t.Fatalf("cleanup %d %v", n, e)
	}
	if _, e = m.Get(t.Context(), s.ID); e != nil {
		t.Fatal(e)
	}
	_, e = m.Save(t.Context(), s.ID, Patch{Public: ptr(false)})
	if e != nil {
		t.Fatal(e)
	}
	public, e = m.Public(t.Context())
	if e != nil || len(public) != 0 {
		t.Fatal(public, e)
	}
}
func TestConfigMutationFencesResultsAndAudits(t *testing.T) {
	m, db, _ := fixture(t, nil)
	ctx := audit.WithSystemActor(t.Context(), "test")
	s, e := m.Save(ctx, "", Patch{Name: ptr("Health"), Type: ptr("tcp"), Target: ptr("localhost:443")})
	if e != nil {
		t.Fatal(e)
	}
	updated, e := m.Save(ctx, s.ID, Patch{Target: ptr("localhost:80")})
	if e != nil {
		t.Fatal(e)
	}
	if e = m.Accept(t.Context(), s, Result{OK: true, LatencyMS: ptr(1.0), CheckedAt: m.now()}); e != nil {
		t.Fatal(e)
	}
	if e = m.Flush(t.Context()); e != nil {
		t.Fatal(e)
	}
	got, e := m.Get(t.Context(), s.ID)
	if e != nil || got.Latest != nil {
		t.Fatalf("stale result %+v %v", got, e)
	}
	if e = m.Delete(ctx, updated.ID); e != nil {
		t.Fatal(e)
	}
	if e = m.Accept(t.Context(), updated, Result{OK: true, CheckedAt: m.now()}); e != nil {
		t.Fatal(e)
	}
	lease, e := db.Acquire(t.Context(), storage.Reader)
	if e != nil {
		t.Fatal(e)
	}
	defer lease.Close()
	var count int
	if e = lease.QueryRowContext(lease.Context(), "SELECT count(*) FROM audit_events WHERE target_id=?", s.ID).Scan(&count); e != nil || count != 3 {
		t.Fatalf("audit %d %v", count, e)
	}
}

type fakeTimer struct {
	ch      chan time.Time
	clock   *fakeClock
	at      time.Time
	stopped bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }
func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{ch: make(chan time.Time, 1), clock: c, at: c.now.Add(d)}
	c.timers = append(c.timers, t)
	return t
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.timers {
		if !t.stopped && !t.at.After(c.now) {
			t.stopped = true
			t.ch <- c.now
		}
	}
}

type fakeChecker struct {
	active, peak, calls atomic.Int32
	started             chan Service
	release             chan struct{}
	clock               Clock
}

func (c *fakeChecker) Check(ctx context.Context, s Service) Result {
	a := c.active.Add(1)
	for {
		p := c.peak.Load()
		if a <= p || c.peak.CompareAndSwap(p, a) {
			break
		}
	}
	defer c.active.Add(-1)
	c.calls.Add(1)
	c.started <- s
	select {
	case <-ctx.Done():
	case <-c.release:
	}
	return Result{OK: true, LatencyMS: ptr(1.0), CheckedAt: c.clock.Now()}
}
func TestSchedulerPoolReloadAndCancellation(t *testing.T) {
	m, _, _ := fixture(t, nil)
	for range 12 {
		create(t, m)
	}
	clock := &fakeClock{now: m.now()}
	checker := &fakeChecker{started: make(chan Service, 100), release: make(chan struct{}), clock: clock}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { m.RunWith(ctx, clock, checker, func() float64 { return 0 }); close(done) }()
	defer cancel()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for range 8 {
		select {
		case <-checker.started:
		case <-deadline.C:
			t.Fatal("workers did not start")
		}
	}
	if checker.peak.Load() != 8 {
		t.Fatalf("pool %d", checker.peak.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler failed to stop")
	}
	if checker.active.Load() != 0 {
		t.Fatal("workers leaked")
	}
}
func TestSchedulerFakeClockJitterAndReload(t *testing.T) {
	m, _, _ := fixture(t, nil)
	s := create(t, m)
	clock := &fakeClock{now: m.now()}
	checker := &fakeChecker{started: make(chan Service, 10), release: make(chan struct{}), clock: clock}
	close(checker.release)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { m.RunWith(ctx, clock, checker, func() float64 { return 0.5 }); close(done) }()
	defer func() { cancel(); <-done }()
	// Initial phase spreads checks across one interval. Step the fake clock only after a timer exists.
	waitTimer := func() {
		t.Helper()
		until := time.Now().Add(5 * time.Second)
		for time.Now().Before(until) {
			clock.mu.Lock()
			n := len(clock.timers)
			clock.mu.Unlock()
			if n > 0 {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("no timer")
	}
	waitTimer()
	clock.advance(31 * time.Second)
	select {
	case <-checker.started:
	case <-time.After(5 * time.Second):
		t.Fatal("initial check missing")
	}
	// Disabling reloads without waiting for the next interval.
	if _, e := m.Save(t.Context(), s.ID, Patch{Enabled: ptr(false)}); e != nil {
		t.Fatal(e)
	}
	clock.advance(120 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if checker.calls.Load() != 1 {
		t.Fatalf("disabled service checked %d", checker.calls.Load())
	}
}

func TestCapacityAndUnavailableHistoryBuffer(t *testing.T) {
	m, db, _ := fixture(t, nil)
	var last Service
	for range 100 {
		last = create(t, m)
	}
	if _, e := m.Save(t.Context(), "", Patch{Name: ptr("Extra"), Type: ptr("icmp"), Target: ptr("localhost")}); !errors.Is(e, ErrCapacity) {
		t.Fatalf("capacity: %v", e)
	}
	if _, e := m.Save(t.Context(), last.ID, Patch{Name: ptr("Updated")}); e != nil {
		t.Fatal(e)
	}
	noHistory := New(db, nil, m.now, nil, nil, nil)
	configs, e := noHistory.configs(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	if e = noHistory.Accept(t.Context(), configs[0], Result{OK: true, LatencyMS: ptr(1.0), CheckedAt: m.now()}); e != nil {
		t.Fatal(e)
	}
	if e = noHistory.Flush(t.Context()); e != nil {
		t.Fatal(e)
	}
	if len(noHistory.buckets) != 0 {
		t.Fatal("history-disabled buffer retained")
	}
	stale := configs[0]
	stale.generation++
	if e = noHistory.Accept(t.Context(), stale, Result{OK: false, CheckedAt: m.now()}); e != nil {
		t.Fatal(e)
	}
	if len(noHistory.latest) != 0 {
		t.Fatal("old generation result accepted")
	}
}

func TestIntervalJitterBounds(t *testing.T) {
	for _, seconds := range []int{30, 60, 3600} {
		for _, random := range []float64{0, 0.5, 0.999999} {
			got := jitteredInterval(seconds, random)
			interval := time.Duration(seconds) * time.Second
			if got < interval*9/10 || got > interval*11/10 {
				t.Fatalf("jitter %v for %v", got, interval)
			}
		}
	}
}

func TestHistoryFailureDoesNotStopStatusOrPublicPage(t *testing.T) {
	m, _, history := fixture(t, nil)
	s := create(t, m)
	if e := history.Close(); e != nil {
		t.Fatal(e)
	}
	if e := m.Accept(t.Context(), s, Result{OK: true, LatencyMS: ptr(2.0), CheckedAt: m.now()}); e != nil {
		t.Fatal(e)
	}
	if e := m.Flush(t.Context()); e != nil {
		t.Fatal(e)
	}
	if len(m.buckets) != 0 {
		t.Fatal("failed history batch retained")
	}
	got, e := m.Get(t.Context(), s.ID)
	if e != nil || got.Latest == nil || got.Latest.Status != "up" || got.Availability24H != nil {
		t.Fatalf("main status %+v %v", got, e)
	}
	public, e := m.Public(t.Context())
	if e != nil || len(public) != 1 || public[0].Status != "up" {
		t.Fatalf("public %+v %v", public, e)
	}
	if _, e = m.History(t.Context(), s.ID, "1h"); e == nil {
		t.Fatal("explicit history read concealed failure")
	}
}

func TestPublicProjectionUsesOneClockAndEnabledWhitelist(t *testing.T) {
	m, _, _ := fixture(t, nil)
	s := create(t, m)
	at := m.now()
	if e := m.Accept(t.Context(), s, Result{OK: true, LatencyMS: ptr(3.0), CheckedAt: at}); e != nil {
		t.Fatal(e)
	}
	m.now = func() time.Time { t.Fatal("PublicAt called another clock"); return time.Time{} }
	current, e := m.PublicAt(t.Context(), at)
	if e != nil || len(current) != 1 || current[0].Status != "up" || current[0].LatencyMS == nil {
		t.Fatalf("current %+v %v", current, e)
	}
	stale, e := m.PublicAt(t.Context(), at.Add(131*time.Second))
	if e != nil || stale[0].Status != "unknown" || stale[0].LatencyMS != nil {
		t.Fatalf("stale %+v %v", stale, e)
	}
	m.now = func() time.Time { return at }
	if _, e = m.Save(t.Context(), s.ID, Patch{Enabled: ptr(false)}); e != nil {
		t.Fatal(e)
	}
	current, e = m.PublicAt(t.Context(), at)
	if e != nil || len(current) != 0 {
		t.Fatalf("disabled service published: %+v %v", current, e)
	}
}

func TestCertificateDailyBudgetSurvivesConfigReset(t *testing.T) {
	m, db, _ := fixture(t, nil)
	s := create(t, m)
	expiry := m.now().Add(13 * 24 * time.Hour)
	check := func(config Service) {
		t.Helper()
		if e := m.Accept(t.Context(), config, Result{OK: true, LatencyMS: ptr(1.0), TLSExpiresAt: &expiry, CheckedAt: m.now()}); e != nil {
			t.Fatal(e)
		}
	}
	check(s)
	// Exercise both pending and persisted certificate pacing across policy/endpoint edits.
	for _, patch := range []Patch{{LatencyThresholdMS: ptr(10.0)}, {Target: ptr("https://other.example")}} {
		var e error
		s, e = m.Save(t.Context(), s.ID, patch)
		if e != nil {
			t.Fatal(e)
		}
		check(s)
		if e = m.Flush(t.Context()); e != nil {
			t.Fatal(e)
		}
	}
	lease, e := db.Acquire(t.Context(), storage.Reader)
	if e != nil {
		t.Fatal(e)
	}
	defer lease.Close()
	var count int
	if e = lease.QueryRowContext(lease.Context(), "SELECT count(*) FROM inbox_notifications WHERE kind='service' AND transition='certificate_expiring'").Scan(&count); e != nil || count != 1 {
		t.Fatalf("certificate notifications=%d: %v", count, e)
	}
}
