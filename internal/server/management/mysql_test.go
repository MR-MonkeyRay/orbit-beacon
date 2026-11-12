package management_test

import (
	"context"
	"io"
	"log/slog"

	"testing"
	"time"

	"orbitbeacon/internal/server/availability"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
	"orbitbeacon/internal/server/traffic"
	"orbitbeacon/internal/timekey"
)

func TestMySQLManagementAvailabilityAndTraffic(t *testing.T) {

	f := storagetest.Start(t)
	db := f.Open(t, storage.MainDomain)
	raw := f.Raw(t, storage.MainDomain)
	ctx := context.Background()
	now := time.Date(2026, 11, 15, 2, 3, 4, 987654321, time.UTC)
	m := management.NewService(db)
	for _, name := range []string{"A", "a", "a "} {
		if _, err := m.SaveGroup(ctx, "", name); err != nil {
			t.Fatal(err)
		}
	}
	groups, err := m.Groups(ctx)
	if err != nil || len(groups) != 3 {
		t.Fatalf("groups=%+v err=%v", groups, err)
	}
	if _, err := m.SetPublicPageEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetPublicPageEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	ns := nodes.NewService(db, nodes.Options{Now: func() time.Time { return now }})
	n, err := ns.CreatePending(ctx, nodes.CreateRequest{Name: "mysql"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE nodes SET status='unknown',token_hash=?,last_seen=?,first_enrolled_at=?,quota_bytes='18446744073709551615',billing_day=1,billing_mode='total' WHERE id=?`, []byte("01234567890123456789012345678901"), timekey.Format(now), timekey.Format(now), n.ID); err != nil {
		t.Fatal(err)
	}
	av := availability.NewService(db, func() time.Time { return now }, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err := av.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := av.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := av.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	availabilitySummary, err := av.Read(ctx, n.ID, 30, now)
	if err != nil || availabilitySummary.Percent == nil {
		t.Fatalf("availability=%+v err=%v", availabilitySummary, err)
	}
	if _, err := av.Cleanup(ctx, now.AddDate(0, 0, 100), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO traffic_hourly(node_id,epoch,period_start,rx_bytes,tx_bytes,updated_at) VALUES(?,'abc',?,100,200,?)`, n.ID, timekey.Format(now.Truncate(time.Hour)), timekey.Format(now)); err != nil {
		t.Fatal(err)
	}
	ts := traffic.NewService(db)
	if quota, err := ts.Quota(ctx, now); err != nil || quota[n.ID] <= 0 {
		t.Fatalf("quota=%v err=%v", quota, err)
	}
	if _, err := ts.Cleanup(ctx, now.Add(36*24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Cleanup(ctx, now.AddDate(2, 0, 0), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Events(ctx, 100, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CleanupAuditEvents(ctx, now.AddDate(2, 0, 0), 100); err != nil {
		t.Fatal(err)
	}
}
