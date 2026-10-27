package ingest_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
)

func TestPublicRuntimeUsesCommittedIdentityAndPersistentLiveness(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(ctx, filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	ns := nodes.NewService(db, nodes.Options{Now: clock})
	n, err := ns.CreatePending(ctx, nodes.CreateRequest{Name: "private"})
	if err != nil {
		t.Fatal(err)
	}
	es := enrollment.NewService(db, enrollment.Options{Now: clock})
	ticket, err := es.CreateSingle(ctx, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = es.Enroll(ctx, ticket.Token, v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "private-install", NodeToken: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"})
	if err != nil {
		t.Fatal(err)
	}
	name, enabled := "Public", true
	if _, err := ns.PatchPublicSettings(ctx, n.ID, nodes.PublicSettingsPatch{Enabled: &enabled, Name: &name}); err != nil {
		t.Fatal(err)
	}
	s := ingest.NewService(db, ingest.Options{Now: clock})
	out, err := s.PublicNode(ctx, n.ID, now)
	if err != nil || out.Status != "unknown" || out.CPUPercent != nil {
		t.Fatalf("before reports: %+v %v", out, err)
	}
	cpu, load, uptime := 12.5, .5, 3600.0
	total, available := uint64(8192), uint64(2048)
	zero := uint64(0)
	window := int64(10000)
	m := v1.Metrics{CPUPercent: &cpu, Load1: &load, Load5: &load, Load15: &load, UptimeSeconds: &uptime, MemoryTotalBytes: &total, MemoryAvailableBytes: &available, SwapTotalBytes: &zero, SwapUsedBytes: &zero, Disks: []v1.DiskMetrics{}, Networks: []v1.NetworkMetrics{}, MetricStatus: map[string]string{}, Sources: map[string]v1.SourceMetadata{}}
	for _, key := range []string{"cpu_percent", "load1", "load5", "load15", "uptime_seconds", "memory_total_bytes", "memory_available_bytes", "swap_total_bytes", "swap_used_bytes"} {
		m.MetricStatus[key] = "ok"
	}
	for _, key := range []string{"cpu", "memory", "load", "swap", "uptime"} {
		m.Sources[key] = v1.SourceMetadata{SampleID: "private-sample", SourceIntervalMS: 10000}
	}
	source := m.Sources["cpu"]
	source.MeasurementWindowMS = &window
	m.Sources["cpu"] = source
	report := v1.Report{ProtocolVersion: 1, RunGeneration: 1, SessionID: "private-session", Seq: 1, SampledAt: now, ConfigRevision: 1, Metrics: m, Traffic: []v1.Traffic{}}
	if _, err := s.Accept(ctx, n.ID, report); err != nil {
		t.Fatal(err)
	}
	out, err = s.PublicNode(ctx, n.ID, now)
	if err != nil || out.Status != "online" || out.CPUPercent == nil || *out.CPUPercent != 12.5 || out.MemoryPercent == nil || *out.MemoryPercent != 75 {
		t.Fatalf("after report: %+v %v", out, err)
	}
	committed := now
	for _, test := range []struct {
		delta  time.Duration
		status string
		cpu    bool
	}{{45 * time.Second, "online", true}, {45*time.Second + time.Millisecond, "online", false}, {180 * time.Second, "online", false}, {180*time.Second + time.Nanosecond, "offline", false}, {-time.Nanosecond, "unknown", false}} {
		out, err = s.PublicNode(ctx, n.ID, committed.Add(test.delta))
		if err != nil || out.Status != test.status || (out.CPUPercent != nil) != test.cpu {
			t.Fatalf("at %v: %+v %v", test.delta, out, err)
		}
	}
	restarted := ingest.NewService(db, ingest.Options{Now: clock})
	out, err = restarted.PublicNode(ctx, n.ID, now)
	if err != nil || out.Status != "online" || out.CPUPercent != nil {
		t.Fatalf("restart: %+v %v", out, err)
	}
	now = now.Add(20 * time.Second)
	cpu = 99
	if _, err := s.Accept(ctx, n.ID, report); err != nil {
		t.Fatal(err)
	}
	out, err = s.PublicNode(ctx, n.ID, now)
	if err != nil || out.CPUPercent == nil || *out.CPUPercent != 12.5 {
		t.Fatalf("duplicate replaced snapshot: %+v %v", out, err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_report BEFORE UPDATE OF last_seen ON nodes BEGIN SELECT RAISE(ABORT,'reject report'); END`); err != nil {
		t.Fatal(err)
	}
	report.Seq = 2
	if _, err := s.Accept(ctx, n.ID, report); err == nil {
		t.Fatal("failed write accepted")
	}
	out, err = s.PublicNode(ctx, n.ID, now)
	if err != nil || out.CPUPercent == nil || *out.CPUPercent != 12.5 {
		t.Fatalf("failed write replaced snapshot: %+v %v", out, err)
	}
	if _, err := db.Exec("DROP TRIGGER reject_report"); err != nil {
		t.Fatal(err)
	}
	report.Metrics.MetricStatus["cpu_percent"] = "error"
	if _, err := s.Accept(ctx, n.ID, report); err != nil {
		t.Fatal(err)
	}
	out, err = s.PublicNode(ctx, n.ID, now)
	if err != nil || out.CPUPercent != nil || out.MemoryPercent == nil {
		t.Fatalf("unavailable new metric retained old CPU: %+v %v", out, err)
	}
	for _, timestamp := range []any{nil, "2026-10-28T00:00:00.000000000Z", "broken"} {
		if _, err := db.Exec("UPDATE nodes SET last_seen=? WHERE id=?", timestamp, n.ID); err != nil {
			t.Fatal(err)
		}
		out, err = s.PublicNode(ctx, n.ID, now)
		if timestamp == "broken" {
			if err == nil {
				t.Fatal("corrupt timestamp accepted")
			}
		} else if err != nil || out.Status != "unknown" || out.CPUPercent != nil {
			t.Fatalf("missing/future timestamp: %+v %v", out, err)
		}
	}
}
