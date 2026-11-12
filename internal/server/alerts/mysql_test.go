package alerts

import (
	"context"
	"crypto/sha256"
	"encoding/json"

	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
	"orbitbeacon/internal/timekey"
)

func TestMySQLAlertsSnapshotsSilencesAndResolution(t *testing.T) {

	f := storagetest.Start(t)
	db := f.Open(t, storage.MainDomain)
	raw := f.Raw(t, storage.MainDomain)
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 2, 3, 4, 987654321, time.UTC)
	id := "00000000-0000-4000-8000-000000000001"
	credential := sha256.Sum256([]byte("mysql"))
	if _, err := raw.Exec(`INSERT INTO nodes(id,name,status,token_hash,first_enrolled_at,last_seen,created_at,run_generation,report_session_id) VALUES(?,'node','unknown',?,?,?,?,'1','session')`, id, credential[:], timekey.Format(now), timekey.Format(now), timekey.Format(now)); err != nil {
		t.Fatal(err)
	}
	s := NewService(db, func() time.Time { return now })
	rule, err := s.Save(ctx, "", patch(t, `{"name":"CPU","enabled":true,"scope_kind":"all","metric":"cpu_percent","comparator":">","threshold":80,"for_seconds":0,"severity":"warning"}`))
	if err != nil {
		t.Fatal(err)
	}
	latest := &latestStub{report: ingest.LatestReport{ReceivedAt: now, Report: v1.Report{Metrics: v1.Metrics{CPUPercent: ptr(90.0), MetricStatus: map[string]string{"cpu_percent": "ok"}, Sources: map[string]v1.SourceMetadata{"cpu": {SampleID: "cpu", SourceIntervalMS: 10000}}}}}}
	e := NewEvaluator(s, latest, EvaluatorOptions{Now: func() time.Time { return now }})
	var req SilenceRequest
	wire, _ := json.Marshal(map[string]any{"reason": "maintenance", "scope_kind": "all", "ends_at": now.Add(time.Hour)})
	if err := json.Unmarshal(wire, &req); err != nil {
		t.Fatal(err)
	}
	silence, err := s.CreateSilence(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Evaluate(ctx, now); err != nil {
		t.Fatal(err)
	}
	page, err := s.Events(ctx, "firing", 10, "")
	if err != nil || len(page.Events) != 1 || !page.Events[0].Silenced || page.Events[0].RuleID != rule.ID || page.Events[0].Target != nil {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	summary, err := s.Summary(ctx)
	if err != nil || summary.Firing.Warning != 1 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	if _, err := s.Event(ctx, page.Events[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSilence(ctx, silence.ID); err != nil {
		t.Fatal(err)
	}
	latest.report.Report.Metrics.CPUPercent = ptr(10.0)
	if err := e.Evaluate(ctx, now); err != nil {
		t.Fatal(err)
	}
	page, err = s.Events(ctx, "resolved", 10, "")
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("resolved=%+v err=%v", page, err)
	}
	if _, err := s.Cleanup(ctx, now.Add(time.Second), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CleanupSilences(ctx, now.Add(2*time.Hour), 10); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLAlertSnapshotTextSemantics(t *testing.T) {
	f := storagetest.Start(t)
	db := f.Open(t, storage.MainDomain)
	raw := f.Raw(t, storage.MainDomain)
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 2, 3, 4, 0, time.UTC)
	s := NewService(db, func() time.Time { return now })
	cases := []struct {
		id       string
		snapshot string
		target   *string
		severity string
	}{
		{"missing", `{"metric":"cpu_percent","severity":"warning"}`, nil, "warning"},
		{"null", `{"metric":"cpu_percent","severity":"warning","target":null}`, nil, "warning"},
		{"case", `{"metric":"cpu_percent","severity":"Warning","target":"NULL"}`, ptr("NULL"), "Warning"},
		{"space", `{"metric":"cpu_percent","severity":"warning ","target":"null "}`, ptr("null "), "warning "},
		{"unicode", `{"metric":"cpu_percent","severity":"critical","target":"https://example.test/健康/Aa"}`, ptr("https://example.test/健康/Aa"), "critical"},
	}
	for _, tc := range cases {
		if _, err := raw.Exec(`INSERT INTO alert_events(id,rule_id,node_id,state,started_at,rule_name,node_name,rule_snapshot)
 VALUES(?,'deleted-rule',?,'firing',?,'stored rule','stored node',?)`, tc.id, tc.id, timekey.Format(now), tc.snapshot); err != nil {
			t.Fatal(err)
		}
		event, err := s.Event(ctx, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if (event.Target == nil) != (tc.target == nil) || (tc.target != nil && *event.Target != *tc.target) || event.Metric != "cpu_percent" || event.Severity != tc.severity {
			t.Fatalf("%s: event=%+v want target=%v severity=%q", tc.id, event, tc.target, tc.severity)
		}
	}
	page, err := s.Events(ctx, "firing", 10, "")
	if err != nil || len(page.Events) != len(cases) {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	summary, err := s.Summary(ctx)
	want := FiringCounts{Total: 5, Warning: 2, Critical: 1, Unknown: 2}
	if err != nil || summary.Firing != want || len(summary.Newest) != len(cases) {
		t.Fatalf("summary=%+v want counts=%+v err=%v", summary, want, err)
	}
}
