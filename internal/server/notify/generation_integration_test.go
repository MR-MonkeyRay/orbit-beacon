package notify

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLPromotionCancelsNotificationAndDoesNotPublishOldResult(t *testing.T) {
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
	if err = dbs.CompleteBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()
	defer close(release)
	box, err := secretbox.New(bytes.Repeat([]byte{7}, 32), "orbitbeacon notification channel v1")
	if err != nil {
		t.Fatal(err)
	}
	s := New(dbs.Main, box, Options{AllowPrivateNetworks: true, AllowInsecureWebhooks: true})
	raw, _ := json.Marshal(Config{URL: receiver.URL})
	kind, name, enabled := "webhook", "switch-outage", true
	channel, err := s.Save(ctx, "", Patch{Name: &name, Kind: &kind, Enabled: &enabled, Config: raw})
	if err != nil {
		t.Fatal(err)
	}
	var rulePatch alerts.Patch
	if err = json.Unmarshal([]byte(`{"name":"CPU","enabled":true,"scope_kind":"all","metric":"cpu_percent","comparator":">","threshold":80,"for_seconds":0,"severity":"warning","channel_ids":["`+channel.ID+`"]}`), &rulePatch); err != nil {
		t.Fatal(err)
	}
	rule, err := alerts.NewService(dbs.Main, time.Now).Save(ctx, "", rulePatch)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := dbs.Main.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		lease.Close()
		t.Fatal(err)
	}
	err = s.Enqueue(lease.Context(), tx, alerts.Notification{EventID: "generation-delivery", Rule: rule, NodeID: "node", NodeName: "node", State: "firing", StartedAt: time.Now()})
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	lease.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = dbs.StartReplication(ctx); err != nil {
		t.Fatal(err)
	}
	source, target := f.RawEndpoint(t, "primary", storage.MainDomain), f.RawEndpoint(t, "standby", storage.MainDomain)
	caught := func() bool {
		var a, b, c, d string
		var x, y int64
		var ready bool
		if source.QueryRow("SELECT epoch_uuid,head_seq,head_tx_uuid FROM replica_state").Scan(&a, &x, &b) != nil || target.QueryRow("SELECT epoch_uuid,head_seq,head_tx_uuid,ready FROM replica_state").Scan(&c, &y, &d, &ready) != nil {
			return false
		}
		var headers int
		_ = target.QueryRow("SELECT count(*) FROM replica_transactions WHERE epoch_uuid=? AND seq=?", c, y).Scan(&headers)
		return ready && headers == 1 && a == c && b == d && x == y
	}
	deadline := time.Now().Add(30 * time.Second)
	for !caught() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !caught() {
		t.Fatal("delivery candidate never acquired real barrier")
	}
	delivered := make(chan struct{})
	go func() { defer close(delivered); s.deliver(ctx, channel.ID) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("delivery did not reach actual receiver")
	}
	generation := dbs.Main.Generation()
	proxy.Pause()
	defer proxy.Resume()
	lease, err = dbs.Main.Acquire(ctx, storage.Reader)
	if err == nil {
		var one int
		err = lease.QueryRowContext(lease.Context(), "SELECT 1").Scan(&one)
		lease.Close()
	}
	if err == nil {
		t.Fatal("fault proxy left primary readable")
	}
	select {
	case <-delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("old generation network send prevented draining")
	}
	deadline = time.Now().Add(40 * time.Second)
	for (!dbs.Main.Ready() || dbs.Main.Generation() <= generation) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !dbs.Main.Ready() || dbs.Main.Generation() <= generation {
		t.Fatal("actual coordinator did not publish candidate")
	}
	for _, db := range []*sql.DB{source, target} {
		var sent int
		if err = db.QueryRow("SELECT count(*) FROM notification_deliveries WHERE state='sent'").Scan(&sent); err != nil || sent != 0 {
			t.Fatal("old network result crossed database generation", sent, err)
		}
	}
}
