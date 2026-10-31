package nodes_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

func TestObservedProbesReadsConfiguredTargetsAndHourlySummary(t *testing.T) {
	service, db := newNodes(t, 1)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO nodes(id,name,token_hash,run_generation,report_session_id,agent_runtime,created_at)
VALUES('node','node',X'01','1','session','{"probe_targets":["https://example.test"]}',?)`, timekey.Format(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO probe_latest(node_id,target,ok,latency_ms,error,status_code,checked_at)
VALUES('node','https://example.test',1,12.5,'',200,?)`, timekey.Format(now)); err != nil {
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
	if probe.Target != "https://example.test" || probe.Status != "up" || probe.LatencyMS == nil || *probe.LatencyMS != 12.5 || probe.StatusCode == nil || *probe.StatusCode != 200 || probe.CheckedAt == nil || !probe.CheckedAt.Equal(now) || probe.Checks24H != 4 || probe.Uptime24H == nil || *probe.Uptime24H != 75 {
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
