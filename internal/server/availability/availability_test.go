package availability

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

func fixture(t *testing.T) (*Service, *sql.DB, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "availability.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewService(db, func() time.Time { return now }, nil), db, &now
}
func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
func add(t *testing.T, db *sql.DB, id string, seen time.Time) {
	t.Helper()
	exec(t, db, `INSERT INTO nodes(id,name,status,token_hash,last_seen,created_at) VALUES(?,?,'unknown',?,?,?)`, id, id, []byte(id), timekey.Format(seen), timekey.Format(seen))
}
func tick(t *testing.T, s *Service) {
	t.Helper()
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func counts(t *testing.T, db *sql.DB, id string) (int64, int64) {
	t.Helper()
	var a, b int64
	if err := db.QueryRow(`SELECT COALESCE(sum(online_seconds),0),COALESCE(sum(offline_seconds),0) FROM node_availability_daily WHERE node_id=?`, id).Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	return a, b
}
func want(t *testing.T, db *sql.DB, id string, a, b int64) {
	t.Helper()
	x, y := counts(t, db, id)
	if x != a || y != b {
		t.Fatalf("%s counts=%d,%d want %d,%d", id, x, y, a, b)
	}
}
func TestAccountingClockGraceAndEligibility(t *testing.T) {
	s, db, now := fixture(t)
	for _, id := range []string{"online", "offline", "disabled", "empty", "null", "unseen", "stopped"} {
		seen := *now
		if id == "offline" {
			seen = seen.Add(-time.Hour)
		}
		add(t, db, id, seen)
	}
	exec(t, db, `UPDATE nodes SET status='disabled' WHERE id='disabled'`)
	exec(t, db, `UPDATE nodes SET token_hash=x'' WHERE id='empty'`)
	exec(t, db, `UPDATE nodes SET token_hash=NULL WHERE id='null'`)
	exec(t, db, `UPDATE nodes SET last_seen=NULL WHERE id='unseen'`)
	exec(t, db, `INSERT INTO agent_commands(id,node_id,credential_epoch,kind,state,requested_by,created_at,expires_at,delivered_generation) VALUES('stop','stopped',0,'stop','succeeded','admin',?,?,'0')`, timekey.Format(*now), timekey.Format(now.Add(time.Hour)))
	tick(t, s)
	want(t, db, "online", 0, 0)
	*now = now.Add(time.Minute)
	tick(t, s)
	want(t, db, "online", 60, 0)
	want(t, db, "offline", 0, 0)
	*now = now.Add(time.Minute)
	tick(t, s)
	want(t, db, "online", 120, 0)
	*now = now.Add(time.Minute)
	tick(t, s)
	want(t, db, "online", 180, 0)
	want(t, db, "offline", 0, 60)
	*now = now.Add(time.Hour)
	tick(t, s)
	want(t, db, "offline", 0, 150)
	want(t, db, "online", 180, 90)
	*now = now.Add(-time.Minute)
	tick(t, s)
	want(t, db, "offline", 0, 150)
	tick(t, s)
	want(t, db, "offline", 0, 150)
	*now = now.Add(time.Minute)
	tick(t, s)
	want(t, db, "offline", 0, 210)
	for _, id := range []string{"disabled", "empty", "null", "unseen", "stopped"} {
		want(t, db, id, 0, 0)
	}
}
func TestCapsMidnightAndConstraints(t *testing.T) {
	s, db, now := fixture(t)
	*now = time.Date(2026, 11, 5, 23, 59, 30, 0, time.UTC)
	add(t, db, "online", *now)
	add(t, db, "offline", now.Add(-time.Hour))
	tick(t, s)
	s.started = now.Add(-time.Hour)
	*now = now.Add(time.Minute)
	tick(t, s)
	var day string
	if err := db.QueryRow(`SELECT day FROM node_availability_daily WHERE node_id='online'`).Scan(&day); err != nil || day != "2026-11-06" {
		t.Fatalf("midnight=%s %v", day, err)
	}
	exec(t, db, `UPDATE node_availability_daily SET online_seconds=86390,offline_seconds=0 WHERE node_id='online'`)
	exec(t, db, `UPDATE node_availability_daily SET online_seconds=80000,offline_seconds=6390 WHERE node_id='offline'`)
	*now = now.Add(time.Minute)
	tick(t, s)
	want(t, db, "online", 86400, 0)
	want(t, db, "offline", 80000, 6400)
	*now = now.Add(time.Hour)
	tick(t, s)
	want(t, db, "online", 86400, 0)
	want(t, db, "offline", 80000, 6400)
	for _, set := range []string{"online_seconds=-1", "offline_seconds=86401", "online_seconds=86401", "online_seconds=1,offline_seconds=86400"} {
		if _, err := db.Exec(`UPDATE node_availability_daily SET ` + set); err == nil {
			t.Fatalf("accepted %s", set)
		}
	}
	exec(t, db, `DELETE FROM nodes WHERE id='online'`)
	want(t, db, "online", 0, 0)
}
func TestConcurrentDeletion(t *testing.T) {
	s, db, now := fixture(t)
	tick(t, s)
	for i := 0; i < 20; i++ {
		id := fmt.Sprint(i)
		add(t, db, id, *now)
		*now = now.Add(time.Minute)
		var wg sync.WaitGroup
		wg.Add(1)
		errs := make(chan error, 1)
		go func() { defer wg.Done(); _, err := db.Exec(`DELETE FROM nodes WHERE id=?`, id); errs <- err }()
		tick(t, s)
		wg.Wait()
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		want(t, db, id, 0, 0)
	}
}
func TestReadUnknownPercentAndRetention(t *testing.T) {
	s, db, now := fixture(t)
	add(t, db, "node", *now)
	for _, d := range []Daily{{Day: "2026-11-04", Online: 86399, Offline: 1}, {Day: "2026-11-05", Online: 60, Offline: 30}, {Day: now.AddDate(0, 0, -89).Format(time.DateOnly), Online: 1}, {Day: now.AddDate(0, 0, -90).Format(time.DateOnly), Online: 1}} {
		exec(t, db, `INSERT INTO node_availability_daily VALUES(?,?,?,?)`, "node", d.Day, d.Online, d.Offline)
	}
	summary, err := s.Read(context.Background(), "node", 30, *now)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Days) != 30 || summary.Days[0].Unknown != 86400 || summary.Days[0].Percent != nil {
		t.Fatalf("missing day %+v", summary.Days[0])
	}
	yesterday, today := summary.Days[28], summary.Days[29]
	if yesterday.Unknown != 0 || *yesterday.Percent != 99.99 || today.Unknown != 43110 || *today.Percent != 66.67 || summary.Percent == nil {
		t.Fatalf("summary %+v %+v", yesterday, today)
	}
	if Percent(0, 0) != nil || *Percent(2591999, 1) != 99.99 || *Percent(1, 0) != 100 || *Percent(0, 1) != 0 {
		t.Fatal("percent null or cap")
	}
	empty := Fill(nil, 30, now.Truncate(24*time.Hour))
	if empty.Days[29].Unknown != 0 || empty.Percent != nil {
		t.Fatal(empty)
	}
	for _, days := range []int{30, 90} {
		out, err := s.Read(context.Background(), "node", days, *now)
		if err != nil || len(out.Days) != days {
			t.Fatalf("days=%d %v", days, err)
		}
	}
	if _, err := s.Read(context.Background(), "missing", 30, *now); err != nodes.ErrNotFound {
		t.Fatalf("not found %v", err)
	}
	removed, err := s.Cleanup(context.Background(), *now, 1)
	if err != nil || removed != 1 {
		t.Fatalf("retention %d %v", removed, err)
	}
	removed, err = s.Cleanup(context.Background(), *now, 1)
	if err != nil || removed != 0 {
		t.Fatalf("retention boundary %d %v", removed, err)
	}
}

func TestCalculationWithoutDatabase(t *testing.T) {
	now := time.Date(2026, 11, 5, 0, 0, 30, 0, time.UTC)
	out := Fill([]Daily{{Day: "2026-11-04", Online: 86399, Offline: 1}, {Day: "2026-11-05", Online: 20, Offline: 20}}, 30, now)
	if len(out.Days) != 30 || out.Days[0].Unknown != 86400 || out.Days[0].Percent != nil || out.Days[28].Unknown != 0 || out.Days[29].Unknown != 0 {
		t.Fatalf("unknown %+v", out)
	}
	if *out.Days[28].Percent != 99.99 || *out.Days[29].Percent != 50 || *out.Percent != 99.98 {
		t.Fatalf("percent %+v", out)
	}
	for _, tc := range []struct {
		online, offline int64
		percent         float64
	}{{2591999, 1, 99.99}, {1, 0, 100}, {0, 1, 0}} {
		if p := Percent(tc.online, tc.offline); p == nil || *p != tc.percent {
			t.Fatalf("percent %v", p)
		}
	}
	if Percent(0, 0) != nil {
		t.Fatal("no observations must be null")
	}
	empty, err := Project("", now)
	if err != nil || len(empty.Days) != 30 || empty.Percent != nil || empty.StartDay != "2026-10-07" {
		t.Fatalf("empty %+v %v", empty, err)
	}
	if _, err := Project("broken", now); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}
