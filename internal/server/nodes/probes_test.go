package nodes_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

func TestObservedProbesReadsReportedTargetsAndHourlySummary(t *testing.T) {
	service, db := newNodes(t, 1)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO nodes(id,name,token_hash,run_generation,report_session_id,created_at)
VALUES('node','node',X'01','1','session',?)`, timekey.Format(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO probe_latest(node_id,target,ok,latency_ms,error,status_code,checked_at,tls_expires_at)
VALUES('node','https://example.test',1,12.5,'',200,?,?)`, timekey.Format(now), timekey.Format(now.Add(24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms)
VALUES('node','https://example.test',?,4,1,50)`, timekey.Format(now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}

	probes, err := service.ObservedProbes(context.Background(), "node", now.Add(-23*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 1 {
		t.Fatalf("probes=%+v", probes)
	}
	probe := probes[0]
	if probe.TLSExpiresAt == nil || !probe.TLSExpiresAt.Equal(now.Add(24*time.Hour)) || probe.Target != "https://example.test" || probe.Status != "up" || probe.LatencyMS == nil || *probe.LatencyMS != 12.5 || probe.StatusCode == nil || *probe.StatusCode != 200 || probe.CheckedAt == nil || !probe.CheckedAt.Equal(now) || probe.Checks24H != 4 || probe.Uptime24H == nil || *probe.Uptime24H != 75 {
		t.Fatalf("probe=%+v", probe)
	}
}

func TestObservedProbesReportsMissingNode(t *testing.T) {
	service, _ := newNodes(t, 1)
	if _, err := service.ObservedProbes(context.Background(), "missing", time.Now()); !errors.Is(err, nodes.ErrNotFound) {
		t.Fatalf("error=%v, want ErrNotFound", err)
	}
}

func TestCleanupProbeHistoryDeletesOnlyOldestBoundedRows(t *testing.T) {
	service, db := newNodes(t, 1)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO nodes(id,name,created_at) VALUES('node','node',?)`, timekey.Format(now)); err != nil {
		t.Fatal(err)
	}
	for _, hour := range []time.Time{now.Add(-10 * time.Hour), now.Add(-9 * time.Hour), now.Add(-8 * time.Hour), now} {
		if _, err := db.Exec(`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms)
VALUES('node','https://example.test',?,1,0,1)`, timekey.Format(hour)); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := service.CleanupProbeHistory(context.Background(), now.Add(-time.Hour), 2)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed=%d, want 2", removed)
	}
	var oldest string
	if err := db.QueryRow(`SELECT min(hour) FROM probe_hourly`).Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	if oldest != timekey.Format(now.Add(-8*time.Hour)) {
		t.Fatalf("oldest=%q", oldest)
	}
}

func TestProbeHistoryWindowOrderingNullsAndIdentity(t *testing.T) {
	service, db := newNodes(t, 1)
	ctx := context.Background()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	const target = "https://example.test"
	if _, err := db.Exec(`INSERT INTO nodes(id,name,token_hash,run_generation,report_session_id,created_at) VALUES('node','node',X'01','1','session',?)`, timekey.Format(now)); err != nil {
		t.Fatal(err)
	}
	// Deliberately insert out of order; the window includes its boundary but not the preceding hour.
	for _, row := range []struct {
		hour             time.Time
		checks, failures int
		sum, max         float64
	}{
		{now, 4, 2, 30, 20}, {now.Add(-2 * time.Hour), 2, 2, 0, 0}, {now.Add(-time.Hour), 1, 0, 5, 5}, {now.Add(-3 * time.Hour), 1, 0, 99, 99},
	} {
		if _, err := db.Exec(`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms,latency_max_ms) VALUES('node',?,?,?,?,?,?)`, target, timekey.Format(row.hour), row.checks, row.failures, row.sum, row.max); err != nil {
			t.Fatal(err)
		}
	}
	points, err := service.ProbeHistory(ctx, "node", target, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 3 || !points[0].Time.Equal(now.Add(-2*time.Hour)) || !points[1].Time.Equal(now.Add(-time.Hour)) || !points[2].Time.Equal(now) {
		t.Fatalf("points=%+v", points)
	}
	if points[0].Avg != nil || points[0].Max != nil || points[0].Checks != 2 || points[0].Failures != 2 {
		t.Fatalf("all failed=%+v", points[0])
	}
	if points[2].Avg == nil || *points[2].Avg != 15 || points[2].Max == nil || *points[2].Max != 20 {
		t.Fatalf("mixed=%+v", points[2])
	}
	for _, update := range []string{
		`UPDATE nodes SET report_session_id=''`,
		`UPDATE nodes SET report_session_id='session',run_generation='0'`,
		`UPDATE nodes SET run_generation='1',token_hash=NULL`,
	} {
		if _, err := db.Exec(update); err != nil {
			t.Fatal(err)
		}
		points, err := service.ProbeHistory(ctx, "node", target, now.Add(-24*time.Hour))
		if err != nil || points == nil || len(points) != 0 {
			t.Fatalf("fenced points=%+v err=%v", points, err)
		}
	}
	if _, err := service.ProbeHistory(ctx, "missing", target, now); !errors.Is(err, nodes.ErrNotFound) {
		t.Fatalf("error=%v", err)
	}
}

func TestReportObservedTargetsBoundedAndDeterministic(t *testing.T) {
	service, db := newNodes(t, 1)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO nodes(id,name,token_hash,run_generation,report_session_id,created_at) VALUES('node','node',X'01','1','session',?)`, timekey.Format(now)); err != nil {
		t.Fatal(err)
	}
	for i := 8; i >= 0; i-- {
		if _, err := db.Exec(`INSERT INTO probe_latest(node_id,target,ok,error,checked_at) VALUES('node',?,0,'timeout',?)`, fmt.Sprintf("tcp://target%d:80", i), timekey.Format(now)); err != nil {
			t.Fatal(err)
		}
	}
	probes, err := service.ObservedProbes(t.Context(), "node", now.Add(-time.Hour))
	if err != nil || len(probes) != 5 {
		t.Fatalf("probes=%+v err=%v", probes, err)
	}
	for i, p := range probes {
		if p.Target != fmt.Sprintf("tcp://target%d:80", i) {
			t.Fatalf("nondeterministic order: %+v", probes)
		}
	}
	probes, err = service.ObservedProbes(t.Context(), "node", now.Add(-time.Hour), "tcp://target8:80")
	if err != nil || len(probes) != 1 || probes[0].Target != "tcp://target8:80" {
		t.Fatalf("current report target omitted: %+v %v", probes, err)
	}
	removed, err := service.CleanupProbeHistory(t.Context(), now.Add(time.Second), 3)
	if err != nil || removed != 3 {
		t.Fatalf("latest retention=%d %v", removed, err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM probe_latest`).Scan(&count); err != nil || count != 6 {
		t.Fatalf("latest count=%d %v", count, err)
	}
}
