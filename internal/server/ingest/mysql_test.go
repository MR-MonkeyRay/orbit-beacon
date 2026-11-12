package ingest_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/commands"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
	"orbitbeacon/internal/server/traffic"
)

func TestMySQLIngestionProbeTrafficAndCommands(t *testing.T) {

	f := storagetest.Start(t)
	db := f.Open(t, storage.MainDomain)
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 2, 3, 4, 987654321, time.UTC)
	ns := nodes.NewService(db, nodes.Options{Now: func() time.Time { return now }})
	node, err := ns.CreatePending(ctx, nodes.CreateRequest{Name: "mysql"})
	if err != nil {
		t.Fatal(err)
	}
	en := enrollment.NewService(db, enrollment.Options{Now: func() time.Time { return now }})
	ticket, err := en.CreateSingle(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	token := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := en.Enroll(ctx, ticket.Token, v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "mysql", NodeToken: token}); err != nil {
		t.Fatal(err)
	}
	s := ingest.NewService(db, ingest.Options{Now: func() time.Time { return now }, PersistInterval: time.Second, DisablePersistOffset: true})
	var report v1.Report
	if err := json.Unmarshal([]byte(`{"protocol_version":1,"run_generation":18446744073709551615,"session_id":"one","seq":1,"metrics":{"cpu_percent":10,"load1":0,"load5":0,"load15":0,"memory_total_bytes":100,"memory_available_bytes":50,"swap_total_bytes":0,"swap_used_bytes":0,"uptime_seconds":10,"disks":[],"networks":[],"metric_status":{"cpu_percent":"ok","load1":"ok","load5":"ok","load15":"ok","memory_total_bytes":"ok","memory_available_bytes":"ok","swap_total_bytes":"ok","swap_used_bytes":"ok","uptime_seconds":"ok"},"sources":{"cpu":{"sample_id":"cpu-1","source_interval_ms":10000,"measurement_window_ms":10000},"load":{"sample_id":"load-1","source_interval_ms":10000},"memory":{"sample_id":"memory-1","source_interval_ms":10000},"swap":{"sample_id":"swap-1","source_interval_ms":10000},"uptime":{"sample_id":"uptime-1","source_interval_ms":10000}}},"traffic":[]}`), &report); err != nil {
		t.Fatal(err)
	}
	report.SampledAt = now
	report.SessionID = strings.Repeat("界", 83) + "Ss\x00x  "
	oversized := report
	oversized.SessionID += "x"
	if _, err := acceptReport(ctx, s, token, oversized); !errors.Is(err, ingest.ErrInvalidReport) {
		t.Fatalf("256-byte report session accepted: %v", err)
	}
	target := "https://example.com/health"
	latency := 2.5
	report.ProbeResults = []v1.ProbeResult{{Target: target, OK: true, LatencyMS: &latency, CheckedAt: now}}
	report.Traffic = []v1.Traffic{{Epoch: "abc", PeriodStart: now.Truncate(time.Hour), RxBytes: "100", TxBytes: "200"}}
	if _, err := acceptReport(ctx, s, token, report); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	report.Seq++
	report.SampledAt = now
	report.ProbeResults[0].CheckedAt = now
	report.ProbeResults[0].OK = false
	report.ProbeResults[0].LatencyMS = nil
	report.ProbeResults[0].Error = "timeout"
	report.Traffic[0].RxBytes = "120"
	if _, err := acceptReport(ctx, s, token, report); err != nil {
		t.Fatal(err)
	}
	command, err := commands.New(db, func() time.Time { return now }).Create(ctx, node.ID, "operator", commands.CreateRequest{Kind: "restart"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	report.Seq++
	report.SampledAt = now
	response, err := acceptReport(ctx, s, token, report)
	if err != nil || response.Command == nil || response.Command.ID != command.ID {
		t.Fatalf("command=%+v err=%v", response, err)
	}
	probes, err := ns.ObservedProbes(ctx, node.ID, now.Add(-24*time.Hour))
	if err != nil || len(probes) != 1 || probes[0].Checks24H != 2 {
		t.Fatalf("probes=%+v err=%v", probes, err)
	}
	summary, err := traffic.NewService(db).Read(ctx, node.ID, 30, now)
	if err != nil || summary.Cycle.RxBytes != "120" || summary.Cycle.TxBytes != "200" {
		t.Fatalf("traffic=%+v err=%v", summary, err)
	}
	report.ProbeResults = nil
	now = now.Add(time.Second)
	report.Seq++
	report.SampledAt = now
	if _, err := acceptReport(ctx, s, token, report); err != nil {
		t.Fatal(err)
	}
	lease, err := db.Acquire(ctx, storage.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var session, generation string
	if err := lease.QueryRowContext(ctx, "SELECT report_session_id,run_generation FROM nodes WHERE id=?", node.ID).Scan(&session, &generation); err != nil || session != report.SessionID || generation != "18446744073709551615" {
		t.Fatalf("report identity changed: session=%q generation=%q err=%v", session, generation, err)
	}
	if _, err := traffic.NewService(db).Cleanup(ctx, now.Add(36*24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := commands.New(db, nil).Cleanup(ctx, now.Add(91*24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
}
