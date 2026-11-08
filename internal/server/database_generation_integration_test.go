package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLHandlerSwitchPublishesNewPublicPolicyAndDrainsOldStreams(t *testing.T) {
	f := storagetest.Start(t)
	proxy, primary := storagetest.ProxyEndpoint(t, f.Endpoint("primary"))
	standby := f.Endpoint("standby")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	dbs, err := storage.OpenDatabases(ctx, config.DatabaseConfig{Primary: primary, Standby: &standby, Failover: config.FailoverConfig{FailureTimeout: time.Second, MaxLossWindow: 10 * time.Second, RecoveryStable: time.Minute, ManualAfter: time.Hour}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	a := auth.NewService(dbs.Main, auth.Options{})
	if _, err = a.CreateUser(ctx, "generation-admin", "generation-password", false); err != nil {
		t.Fatal(err)
	}
	if err = dbs.CompleteBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	n := nodes.NewService(dbs.Main, nodes.Options{})
	h, err := NewHandler(testHandlerOptions(t, Options{Auth: a, Nodes: n, Ingest: ingest.NewService(dbs.Main, ingest.Options{}), PublicURL: "https://beacon.test"}))
	if err != nil {
		t.Fatal(err)
	}
	dbs.SetMainSwitchHooks(storage.SwitchHooks{Prepare: h.PrepareDatabaseSwitch, Publish: h.PublishDatabaseGeneration})
	if _, err = h.management.SetPublicPageEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	h.publicPageDisabled.Store(true)
	if err = dbs.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	source, target := f.RawEndpoint(t, "primary", storage.MainDomain), f.RawEndpoint(t, "standby", storage.MainDomain)
	deadline := time.Now().Add(30 * time.Second)
	caught := false
	for time.Now().Before(deadline) {
		var a, b, c, d string
		var x, y int64
		var ready bool
		var log int
		if source.QueryRow("SELECT epoch_uuid,head_seq,head_tx_uuid FROM replica_state").Scan(&a, &x, &b) == nil && target.QueryRow("SELECT epoch_uuid,head_seq,head_tx_uuid,ready FROM replica_state").Scan(&c, &y, &d, &ready) == nil {
			_ = target.QueryRow("SELECT count(*) FROM replica_transactions WHERE epoch_uuid=? AND seq=?", c, y).Scan(&log)
			caught = ready && log == 1 && a == c && b == d && x == y
		}
		if caught {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !caught {
		t.Fatal("handler candidate lacked real acknowledged barrier")
	}
	stream, ok := h.streams.acquire("192.0.2.1")
	if !ok {
		t.Fatal("initial stream rejected")
	}
	drained := make(chan struct{})
	go func() { <-stream.ctx.Done(); stream.release(); close(drained) }()
	generation := dbs.Main.Generation()
	proxy.Pause()
	defer proxy.Resume()
	lease, err := dbs.Main.Acquire(ctx, storage.Reader)
	if err == nil {
		var v int
		err = lease.QueryRowContext(lease.Context(), "SELECT 1").Scan(&v)
		lease.Close()
	}
	if err == nil {
		t.Fatal("paused primary remained reachable")
	}
	deadline = time.Now().Add(40 * time.Second)
	for (!dbs.Main.Ready() || dbs.Main.Generation() <= generation) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !dbs.Main.Ready() || dbs.Main.Generation() <= generation {
		t.Fatal("handler switch did not publish real candidate")
	}
	select {
	case <-drained:
	default:
		t.Fatal("generation published before old streams drained")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/nodes", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("candidate disabled policy not published: %d", w.Code)
	}
	next, ok := h.streams.acquire("192.0.2.1")
	if !ok {
		t.Fatal("new generation streams not reopened")
	}
	next.release()
}
