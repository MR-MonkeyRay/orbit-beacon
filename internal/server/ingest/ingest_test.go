package ingest_test

import (
	"context"
	"encoding/json"
	"math"
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

func TestNonFiniteMetricsRejectedBeforeStateMutation(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run("nonfinite", func(t *testing.T) {
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
			n := nodes.NewService(db, nodes.Options{MaxNodes: 1})
			node, err := n.CreatePending(ctx, nodes.CreateRequest{Name: "invalid metric"})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
			enrollments := enrollment.NewService(db, enrollment.Options{MaxNodes: 1, Now: func() time.Time { return now }})
			ticket, err := enrollments.CreateSingle(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := enrollments.Enroll(ctx, ticket.Token, v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "installation-one", NodeToken: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}); err != nil {
				t.Fatal(err)
			}
			s := ingest.NewService(db, ingest.Options{Now: func() time.Time { return now }})
			var report v1.Report
			if err := json.Unmarshal([]byte(`{"protocol_version":1,"run_generation":1,"session_id":"one","seq":1,"sampled_at":"2026-10-26T12:00:00Z","runtime":{"report_interval_seconds":10,"disk_interval_seconds":60,"interfaces":[],"disk_paths":[],"probe_targets":[]},"metrics":{"cpu_percent":10,"load1":0,"load5":0,"load15":0,"memory_total_bytes":100,"memory_available_bytes":50,"swap_total_bytes":0,"swap_used_bytes":0,"uptime_seconds":10,"disks":[],"networks":[],"metric_status":{"cpu_percent":"ok","load1":"ok","load5":"ok","load15":"ok","memory_total_bytes":"ok","memory_available_bytes":"ok","swap_total_bytes":"ok","swap_used_bytes":"ok","uptime_seconds":"ok"},"sources":{"cpu":{"sample_id":"cpu-1","age_ms":0,"source_interval_ms":10000,"measurement_window_ms":10000},"load":{"sample_id":"load-1","age_ms":0,"source_interval_ms":10000},"memory":{"sample_id":"memory-1","age_ms":0,"source_interval_ms":10000},"swap":{"sample_id":"swap-1","age_ms":0,"source_interval_ms":10000},"uptime":{"sample_id":"uptime-1","age_ms":0,"source_interval_ms":10000}}},"traffic":[]}`), &report); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Accept(ctx, node.ID, report); err != nil {
				t.Fatal(err)
			}
			var before string
			if err := db.QueryRow("SELECT last_seen FROM nodes WHERE id=?", node.ID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			report.Seq = 2
			report.RunGeneration = 2
			report.Metrics.CPUPercent = &value
			if _, err := s.Accept(ctx, node.ID, report); err == nil {
				t.Fatal("nonfinite metric accepted through typed ingestion")
			}
			var after string
			if err := db.QueryRow("SELECT last_seen FROM nodes WHERE id=?", node.ID).Scan(&after); err != nil {
				t.Fatal(err)
			}
			latest, err := s.Latest(ctx, node.ID)
			if err != nil || latest.Seq != 1 || latest.RunGeneration != 1 || before != after {
				t.Fatalf("invalid report mutated state: %+v before=%s after=%s err=%v", latest, before, after, err)
			}
			// Invalid metrics must not advance the durable generation fence either.
			report.RunGeneration = 1
			cpu := 20.0
			report.Metrics.CPUPercent = &cpu
			if _, err := s.Accept(ctx, node.ID, report); err != nil {
				t.Fatalf("invalid generation advanced process fence: %v", err)
			}
		})
	}
}
