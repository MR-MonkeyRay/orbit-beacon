package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"orbitbeacon/internal/server/ingest"
	"strings"
	"sync"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

func TestGeoPublicIP(t *testing.T) {
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !geoPublicIP(ip) {
			t.Errorf("public %s denied", ip)
		}
	}
	for _, ip := range []string{"", "garbage", "127.0.0.1", "10.1.1.1", "172.16.1.1", "192.168.1.1", "169.254.1.1", "100.100.1.1", "0.0.0.0", "224.0.0.1", "192.0.2.1", "198.51.100.1", "203.0.113.1", "198.18.1.1", "240.0.0.1", "::", "::1", "fe80::1", "fc00::1", "ff02::1", "2001:db8::1", "::ffff:192.0.2.1", "fe80::1%eth0", "64:ff9b::a00:1", "::8.8.8.8", "::ffff:0:8.8.8.8"} {
		if geoPublicIP(ip) {
			t.Errorf("non-public %s accepted", ip)
		}
	}
}

func geoTestService(t *testing.T) (*nodes.Service, *storage.Database) {
	t.Helper()
	_, db := testMainDatabases(t)
	return nodes.NewService(db, nodes.Options{}), db
}
func geoSQL(t *testing.T, db *storage.Database, query string, args ...any) {
	t.Helper()
	lease, err := db.Acquire(context.Background(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(lease.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
func TestGeoWorkerDedupePacingRegionAndIPChange(t *testing.T) {
	ns, db := geoTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var calls []time.Time
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "test-token" {
			t.Error("missing token")
		}
		mu.Lock()
		calls = append(calls, time.Now())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"country":"us","region":"California","city":"Other"}`))
	}))
	defer fake.Close()
	g := newGeoWorker(ns, "test-token")
	g.baseURL = fake.URL
	g.client = fake.Client()
	var ids []string
	for _, name := range []string{"a", "b", "manual", "private"} {
		n, err := ns.CreatePending(ctx, nodes.CreateRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, n.ID)
		ip, region := "8.8.8.8", ""
		if name == "manual" {
			region = "Tokyo"
		}
		if name == "private" {
			ip = "10.0.0.1"
		}
		geoSQL(t, db, "UPDATE nodes SET remote_ip=?,region=? WHERE id=?", ip, region, n.ID)
		for i := 0; i < 5; i++ {
			g.enqueue(n.ID)
		}
	}
	if len(g.queue) != 4 {
		t.Fatalf("dedupe queue=%d", len(g.queue))
	}
	done := make(chan struct{})
	go func() { defer close(done); g.run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		empty := len(g.pending) == 0
		g.mu.Unlock()
		if empty {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	copyCalls := append([]time.Time{}, calls...)
	mu.Unlock()
	if len(copyCalls) != 2 || copyCalls[1].Sub(copyCalls[0]) < time.Second {
		t.Fatalf("calls %v", calls)
	}
	for i, id := range ids {
		n, err := ns.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 && (n.GeoCountry != "US" || n.GeoLocation != "California") {
			t.Fatalf("geo %+v", n)
		}
		if i >= 2 && n.GeoLocation != "" {
			t.Fatal("ineligible looked up")
		}
	}
	// Fresh entries and manual regions do not call the service again.
	g.next = time.Time{}
	g.lookup(context.Background(), ids[0])
	if len(calls) != 2 {
		t.Fatal("fresh lookup repeated")
	}
	geoSQL(t, db, "UPDATE nodes SET remote_ip='1.1.1.1',geo_checked_at='' WHERE id=?", ids[0])
	g.lookup(context.Background(), ids[0])
	if len(calls) != 3 {
		t.Fatal("IP change not requalified")
	}
}
func TestGeoWorkerRejectsResponseAndBacksOff(t *testing.T) {
	ns, db := geoTestService(t)
	n, err := ns.CreatePending(context.Background(), nodes.CreateRequest{Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	geoSQL(t, db, "UPDATE nodes SET remote_ip='8.8.8.8' WHERE id=?", n.ID)
	for _, body := range []string{strings.Repeat("x", 16385), `{"country":"USA","region":"No"}`, `{"country":"US","region":"bad\nregion"}`, `not json`} {
		fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		g := newGeoWorker(ns, "")
		g.baseURL = fake.URL
		g.client = fake.Client()
		g.lookup(context.Background(), n.ID)
		fake.Close()
		got, err := ns.Get(context.Background(), n.ID)
		if err != nil || got.GeoCountry != "" || got.GeoLocation != "" {
			t.Fatalf("invalid body leaked %+v %v", got, err)
		}
		geoSQL(t, db, "UPDATE nodes SET geo_checked_at='' WHERE id=?", n.ID)
	}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer fake.Close()
	g := newGeoWorker(ns, "")
	g.baseURL = fake.URL
	g.client = fake.Client()
	g.lookup(context.Background(), n.ID)
	if g.backoff != time.Minute || time.Until(g.next) < 59*time.Second {
		t.Fatalf("backoff %v", g.backoff)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g.lookup(ctx, n.ID)
	// A checked timestamp uses the same storage format as all other observations.
	geoSQL(t, db, "UPDATE nodes SET geo_checked_at=? WHERE id=?", timekey.Format(time.Now()), n.ID)
}

func TestObservedIPHonorsTrustedProxy(t *testing.T) {
	ns, db := geoTestService(t)
	n, err := ns.CreatePending(context.Background(), nodes.CreateRequest{Name: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	h := &handler{nodes: ns, ingest: ingest.NewService(db, ingest.Options{}), logger: slog.Default(), trustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	for _, tc := range []struct{ peer, forwarded, want string }{{"127.0.0.1:1234", "8.8.8.8", "8.8.8.8"}, {"1.1.1.1:1234", "8.8.8.8", "1.1.1.1"}} {
		r := httptest.NewRequest(http.MethodPost, "/api/agent/v1/report", nil)
		r.RemoteAddr = tc.peer
		r.Header.Set("X-Forwarded-For", tc.forwarded)
		h.observeAgentIP(r, n.ID)
		got, err := ns.Get(context.Background(), n.ID)
		if err != nil || got.RemoteIP != tc.want {
			t.Fatalf("remote %q want %q: %v", got.RemoteIP, tc.want, err)
		}
	}
}

func TestTokenNodeEffectiveRegion(t *testing.T) {
	n := nodes.Node{Region: "", GeoLocation: "California", RemoteIP: "8.8.8.8"}
	if got := tokenNodeDTO(n); got.Region != "California" {
		t.Fatalf("fallback %+v", got)
	}
	n.Region = "Manual"
	if got := tokenNodeDTO(n); got.Region != "Manual" {
		t.Fatalf("manual %+v", got)
	}
}
