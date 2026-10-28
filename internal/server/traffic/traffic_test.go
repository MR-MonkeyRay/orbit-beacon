package traffic_test

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/traffic"
	"orbitbeacon/internal/timekey"
)

func newFixture(t *testing.T) (*traffic.Service, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "traffic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return traffic.NewService(db), db
}

func insertNode(t *testing.T, db *sql.DB, id string, billingDay int, quota string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO nodes(id,name,billing_day,quota_bytes,created_at) VALUES(?,?,?,?,?)`, id, id, billingDay, quota, timekey.Format(time.Now())); err != nil {
		t.Fatal(err)
	}
}

func insertHourly(t *testing.T, db *sql.DB, node, epoch string, period time.Time, rx, tx int64, updated time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO traffic_hourly(node_id,epoch,period_start,rx_bytes,tx_bytes,updated_at) VALUES(?,?,?,?,?,?)`, node, epoch, timekey.Format(period), rx, tx, timekey.Format(updated)); err != nil {
		t.Fatal(err)
	}
}

func TestReadReportsZeroQuotaAsUnlimited(t *testing.T) {
	service, db := newFixture(t)
	insertNode(t, db, "node", 1, "0")
	got, err := service.Read(context.Background(), "node", 1, time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got.QuotaBytes != nil {
		t.Fatalf("quota 0 = %q, want null (unlimited)", *got.QuotaBytes)
	}
}

func TestReadMergesEpochsAndDailyRows(t *testing.T) {
	service, db := newFixture(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	insertNode(t, db, "node", 28, "12345678901234567890")
	insertHourly(t, db, "node", "epoch-a", now.Add(-2*time.Hour), 10, 20, now.Add(-time.Minute))
	insertHourly(t, db, "node", "epoch-b", now.Add(-2*time.Hour), 30, 40, now)
	if _, err := db.Exec(`INSERT INTO traffic_daily(node_id,day,rx_bytes,tx_bytes) VALUES(?,?,?,?)`, "node", now.AddDate(0, 0, -1).Format("2006-01-02"), 5, 7); err != nil {
		t.Fatal(err)
	}

	got, err := service.Read(context.Background(), "node", 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.BillingDay == nil || *got.BillingDay != 28 || got.QuotaBytes == nil || *got.QuotaBytes != "12345678901234567890" {
		t.Fatalf("config fields = billing %v quota %v", got.BillingDay, got.QuotaBytes)
	}
	if len(got.Daily) != 2 || got.Daily[0].RxBytes != "5" || got.Daily[1].RxBytes != "40" || got.Daily[1].TxBytes != "60" {
		t.Fatalf("daily = %+v", got.Daily)
	}
	if got.Cycle.RxBytes != "45" || got.Cycle.TxBytes != "67" || got.Cycle.TotalBytes != "112" {
		t.Fatalf("cycle = %+v", got.Cycle)
	}
	if got.UpdatedAt == nil || !got.UpdatedAt.Equal(now) {
		t.Fatalf("updated_at = %v", got.UpdatedAt)
	}
}

func TestReadNotFoundAndEmptyCounters(t *testing.T) {
	service, db := newFixture(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if _, err := service.Read(context.Background(), "missing", 30, now); err != traffic.ErrNotFound {
		t.Fatalf("missing node error = %v", err)
	}
	insertNode(t, db, "node", 1, "0")
	got, err := service.Read(context.Background(), "node", 30, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Daily == nil || len(got.Daily) != 0 || got.UpdatedAt != nil {
		t.Fatalf("empty result = %+v", got)
	}
	if got.Cycle.RxBytes != "0" || got.Cycle.TxBytes != "0" || got.Cycle.TotalBytes != "0" {
		t.Fatalf("empty cycle = %+v", got.Cycle)
	}
}

func TestReadSaturatesHourlyAndCycleTotals(t *testing.T) {
	service, db := newFixture(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	insertNode(t, db, "node", 1, "0")
	insertHourly(t, db, "node", "epoch-a", now.Truncate(time.Hour), math.MaxInt64, math.MaxInt64, now)
	insertHourly(t, db, "node", "epoch-b", now.Truncate(time.Hour), math.MaxInt64, math.MaxInt64, now)
	got, err := service.Read(context.Background(), "node", 1, now)
	if err != nil {
		t.Fatal(err)
	}
	want := "9223372036854775807"
	if len(got.Daily) != 1 || got.Daily[0].RxBytes != want || got.Daily[0].TxBytes != want {
		t.Fatalf("saturated daily = %+v", got.Daily)
	}
	if got.Cycle.RxBytes != want || got.Cycle.TxBytes != want || got.Cycle.TotalBytes != want {
		t.Fatalf("saturated cycle = %+v", got.Cycle)
	}
}

func TestReadSaturatesCycleAcrossDays(t *testing.T) {
	service, db := newFixture(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	insertNode(t, db, "node", 1, "0")
	insertHourly(t, db, "node", "epoch-a", now.Truncate(time.Hour), math.MaxInt64, 0, now)
	insertHourly(t, db, "node", "epoch-a", now.AddDate(0, 0, -1).Truncate(time.Hour), math.MaxInt64, 1, now)

	got, err := service.Read(context.Background(), "node", 2, now)
	if err != nil {
		t.Fatal(err)
	}
	want := "9223372036854775807"
	if got.Cycle.RxBytes != want || got.Cycle.TxBytes != "1" || got.Cycle.TotalBytes != want {
		t.Fatalf("saturated cycle = %+v", got.Cycle)
	}
}

func TestCycleBoundsAcrossMonthBoundary(t *testing.T) {
	tests := []struct {
		name       string
		now        time.Time
		billingDay int
		start      time.Time
		end        time.Time
	}{
		{
			name: "on or after day 28", now: time.Date(2026, 3, 28, 15, 0, 0, 0, time.FixedZone("west", -8*60*60)), billingDay: 28,
			start: time.Date(2026, 3, 28, 0, 0, 0, 0, time.UTC), end: time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "month before", now: time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC), billingDay: 28,
			start: time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC), end: time.Date(2026, 3, 28, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "default invalid", now: time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC), billingDay: 0,
			start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), end: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := traffic.CycleBounds(tt.now, tt.billingDay)
			if !start.Equal(tt.start) || !end.Equal(tt.end) {
				t.Fatalf("bounds=(%s,%s), want (%s,%s)", start, end, tt.start, tt.end)
			}
		})
	}
}

func TestCleanupRollsHourlyExactlyOnceAndDeletesExpiredDaily(t *testing.T) {
	service, db := newFixture(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	insertNode(t, db, "node", 1, "0")
	old := now.Add(-36 * 24 * time.Hour).Truncate(time.Hour)
	recent := now.Add(-34 * 24 * time.Hour).Truncate(time.Hour)
	insertHourly(t, db, "node", "epoch-a", old, 10, 20, now)
	insertHourly(t, db, "node", "epoch-b", old, 30, 40, now)
	insertHourly(t, db, "node", "epoch-a", recent, 50, 60, now)
	expiredDay := now.AddDate(0, 0, -401).Format("2006-01-02")
	if _, err := db.Exec(`INSERT INTO traffic_daily(node_id,day,rx_bytes,tx_bytes) VALUES(?,?,1,2)`, "node", expiredDay); err != nil {
		t.Fatal(err)
	}
	before, err := service.Read(context.Background(), "node", 400, now)
	if err != nil {
		t.Fatal(err)
	}

	removed, err := service.Cleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Fatalf("removed=%d, want two hourly and one daily", removed)
	}
	var rx, tx int64
	if err := db.QueryRow(`SELECT rx_bytes,tx_bytes FROM traffic_daily WHERE node_id=? AND day=?`, "node", old.Format("2006-01-02")).Scan(&rx, &tx); err != nil {
		t.Fatal(err)
	}
	if rx != 40 || tx != 60 {
		t.Fatalf("rollup=(%d,%d)", rx, tx)
	}
	after, err := service.Read(context.Background(), "node", 400, now)
	if err != nil {
		t.Fatal(err)
	}
	if before.Cycle != after.Cycle || len(before.Daily) != len(after.Daily) {
		t.Fatalf("cleanup changed retained totals: before=%+v after=%+v", before, after)
	}
	for i := range before.Daily {
		if before.Daily[i] != after.Daily[i] {
			t.Fatalf("cleanup changed day %d: before=%+v after=%+v", i, before.Daily[i], after.Daily[i])
		}
	}
	removed, err = service.Cleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("second cleanup removed=%d", removed)
	}
	if err := db.QueryRow(`SELECT rx_bytes,tx_bytes FROM traffic_daily WHERE node_id=? AND day=?`, "node", old.Format("2006-01-02")).Scan(&rx, &tx); err != nil {
		t.Fatal(err)
	}
	if rx != 40 || tx != 60 {
		t.Fatalf("second cleanup double counted: (%d,%d)", rx, tx)
	}
	var hourly int
	if err := db.QueryRow(`SELECT count(*) FROM traffic_hourly WHERE node_id=?`, "node").Scan(&hourly); err != nil {
		t.Fatal(err)
	}
	if hourly != 1 {
		t.Fatalf("recent hourly rows=%d", hourly)
	}
}

func TestCleanupIsBounded(t *testing.T) {
	service, db := newFixture(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	insertNode(t, db, "node", 1, "0")
	old := now.Add(-36 * 24 * time.Hour).Truncate(time.Hour)
	for i := range 3 {
		insertHourly(t, db, "node", string(rune('a'+i)), old, 1, 1, now)
	}
	removed, err := service.Cleanup(context.Background(), now, 2)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed=%d", removed)
	}
	var hourly int
	if err := db.QueryRow(`SELECT count(*) FROM traffic_hourly`).Scan(&hourly); err != nil {
		t.Fatal(err)
	}
	if hourly != 1 {
		t.Fatalf("remaining hourly=%d", hourly)
	}
}

func TestCleanupSaturatesRollupAndReadsIt(t *testing.T) {
	service, db := newFixture(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	insertNode(t, db, "node", 1, "0")
	old := now.Add(-36 * 24 * time.Hour).Truncate(time.Hour)
	insertHourly(t, db, "node", "epoch-a", old, math.MaxInt64, math.MaxInt64, now)
	insertHourly(t, db, "node", "epoch-b", old, math.MaxInt64, math.MaxInt64, now)

	removed, err := service.Cleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed=%d, want 2", removed)
	}
	var rx, tx int64
	if err := db.QueryRow(`SELECT rx_bytes,tx_bytes FROM traffic_daily WHERE node_id=? AND day=?`, "node", old.Format("2006-01-02")).Scan(&rx, &tx); err != nil {
		t.Fatal(err)
	}
	if rx != math.MaxInt64 || tx != math.MaxInt64 {
		t.Fatalf("rollup=(%d,%d), want saturation", rx, tx)
	}
	got, err := service.Read(context.Background(), "node", 400, now)
	if err != nil {
		t.Fatal(err)
	}
	want := "9223372036854775807"
	if len(got.Daily) != 1 || got.Daily[0].RxBytes != want || got.Daily[0].TxBytes != want {
		t.Fatalf("rolled daily = %+v", got.Daily)
	}
	if got.Cycle.RxBytes != "0" || got.Cycle.TxBytes != "0" || got.Cycle.TotalBytes != "0" {
		t.Fatalf("out-of-cycle totals = %+v", got.Cycle)
	}
}

func TestCleanupSaturatesExistingDailyAcrossBatches(t *testing.T) {
	service, db := newFixture(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	insertNode(t, db, "node", 1, "0")
	old := now.Add(-36 * 24 * time.Hour).Truncate(time.Hour)
	insertHourly(t, db, "node", "epoch-a", old, math.MaxInt64, math.MaxInt64, now)
	insertHourly(t, db, "node", "epoch-b", old, 1, 2, now)

	if removed, err := service.Cleanup(context.Background(), now, 1); err != nil || removed != 1 {
		t.Fatalf("first cleanup removed=%d err=%v", removed, err)
	}
	if removed, err := service.Cleanup(context.Background(), now, 1); err != nil || removed != 1 {
		t.Fatalf("second cleanup removed=%d err=%v", removed, err)
	}
	var rx, tx int64
	if err := db.QueryRow(`SELECT rx_bytes,tx_bytes FROM traffic_daily WHERE node_id=? AND day=?`, "node", old.Format("2006-01-02")).Scan(&rx, &tx); err != nil {
		t.Fatal(err)
	}
	if rx != math.MaxInt64 || tx != math.MaxInt64 {
		t.Fatalf("daily after two batches=(%d,%d)", rx, tx)
	}
}

func TestCleanupUsesUTCDateBoundaries(t *testing.T) {
	service, db := newFixture(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	insertNode(t, db, "node", 1, "0")
	hourlyBoundary := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	hourlyBefore := hourlyBoundary.Add(-time.Hour)
	insertHourly(t, db, "node", "boundary", hourlyBoundary, 1, 2, now)
	insertHourly(t, db, "node", "before", hourlyBefore, 3, 4, now)
	dailyBoundary := now.Add(-400 * 24 * time.Hour).Format("2006-01-02")
	dailyBefore := now.Add(-401 * 24 * time.Hour).Format("2006-01-02")
	if _, err := db.Exec(`INSERT INTO traffic_daily(node_id,day,rx_bytes,tx_bytes) VALUES(?,?,5,6),(?,?,7,8)`, "node", dailyBoundary, "node", dailyBefore); err != nil {
		t.Fatal(err)
	}

	removed, err := service.Cleanup(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed=%d, want one hourly and one daily", removed)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM traffic_hourly WHERE node_id=? AND period_start=?`, "node", timekey.Format(hourlyBoundary)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("hourly boundary rows=%d", count)
	}
	if err := db.QueryRow(`SELECT count(*) FROM traffic_hourly WHERE node_id=? AND period_start=?`, "node", timekey.Format(hourlyBefore)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("pre-boundary hourly rows=%d", count)
	}
	if err := db.QueryRow(`SELECT count(*) FROM traffic_daily WHERE node_id=? AND day=?`, "node", dailyBoundary).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("daily boundary rows=%d", count)
	}
	if err := db.QueryRow(`SELECT count(*) FROM traffic_daily WHERE node_id=? AND day=?`, "node", dailyBefore).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("pre-boundary daily rows=%d", count)
	}
}
