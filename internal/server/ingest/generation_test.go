package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

func (s *Service) accept(ctx context.Context, nodeID string, credential []byte, credentialEpoch int64, report v1.Report) (v1.ReportResponse, error) {
	return s.acceptGeneration(ctx, nodeID, credential, credentialEpoch, 0, report)
}

func TestPublicProjectionRejectsRetiredDatabaseGeneration(t *testing.T) {
	f := newCoalesceFixture(t, time.Minute)
	records, _, err := f.nodes.ListPublicRecords(context.Background(), 100, 0, f.now)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%v err=%v", records, err)
	}
	before, err := f.service.ProjectPublicRecord(records[0], f.now)
	if err != nil || before.CPUPercent == nil {
		t.Fatalf("initial projection=%+v err=%v", before, err)
	}
	if err := f.nodes.Database().Close(); err != nil {
		t.Fatal(err)
	}
	after, err := f.service.ProjectPublicRecord(records[0], f.now)
	if err != nil {
		t.Fatal(err)
	}
	if after.CPUPercent != nil || after.MemoryPercent != nil || after.Load1 != nil || after.UptimeSeconds != nil {
		t.Fatalf("retired database exposed cached metrics: %+v", after)
	}
}

func TestAdmissionRejectsStaleStorageGeneration(t *testing.T) {
	f := newCoalesceFixture(t, time.Minute)
	admission, err := f.service.AdmitToken(context.Background(), coalesceToken)
	if err != nil {
		t.Fatal(err)
	}
	admission.storageGeneration++
	f.report.Seq++
	if _, err := f.service.AcceptAdmitted(context.Background(), admission, f.report); !errors.Is(err, nodes.ErrUnauthenticated) {
		t.Fatalf("stale admission accepted: %v", err)
	}
	if f.writes(t) != 1 {
		t.Fatal("stale admission wrote a report")
	}
}

func TestStorageGenerationRevalidatesPrivatePendingWithoutDoubleCounting(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-identity", true: "changed-identity"}[changed], func(t *testing.T) {
			f := newCoalesceFixture(t, time.Hour, "https://example.com")
			samples := make(chan history.Sample, 4)
			f.service.samples = samples
			f.now = f.now.Add(time.Second)
			f.report.Seq++
			f.report.Traffic = []v1.Traffic{trafficEntry("a", f.now.Truncate(time.Hour), "150", "250")}
			f.report.ProbeResults = []v1.ProbeResult{probeResult("https://example.com", f.now, true)}
			if _, err := acceptReport(context.Background(), f.service, coalesceToken, f.report); err != nil {
				t.Fatal(err)
			}
			<-samples
			state := f.service.acquireExistingState(f.id)
			defer f.service.releaseState(state)
			state.mu.Lock()
			state.storageGeneration++
			state.traffic.committed = map[trafficKey]trafficCounters{{"b", timekey.Format(f.now.Truncate(time.Hour))}: {999, 999}}
			state.mu.Unlock()
			f.service.rateMu.Lock()
			tokens, refilled := state.tokens, state.refilled
			f.service.rateMu.Unlock()
			if changed {
				if _, err := f.db.Exec("UPDATE nodes SET credential_epoch=credential_epoch+1 WHERE id=?", f.id); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.Latest(context.Background(), f.id); !errors.Is(err, nodes.ErrNotFound) {
				t.Fatalf("old generation latest remained visible: %v", err)
			}
			assertLiveMetrics(t, liveFleetNode(t, f), nil, nil)
			out, err := publicNode(context.Background(), f.nodes, f.service, f.id, f.now)
			if err != nil || out.CPUPercent != nil {
				t.Fatalf("old generation public report=%+v err=%v", out, err)
			}
			retry := f.report
			retry.ProbeResults, retry.Traffic = nil, nil
			if _, err := f.service.accept(context.Background(), f.id, nil, 0, retry); err != nil {
				t.Fatal(err)
			}
			var checks, rx int
			if err := f.db.QueryRow("SELECT COALESCE(SUM(checks),0) FROM probe_hourly WHERE node_id=?", f.id).Scan(&checks); err != nil {
				t.Fatal(err)
			}
			if err := f.db.QueryRow("SELECT COALESCE(SUM(rx_bytes),0) FROM traffic_hourly WHERE node_id=?", f.id).Scan(&rx); err != nil {
				t.Fatal(err)
			}
			wantChecks, wantRX, wantSamples := 1, 150, 0
			if changed {
				wantChecks, wantRX, wantSamples = 0, 0, 1
			}
			if checks != wantChecks || rx != wantRX || len(samples) != wantSamples {
				t.Fatalf("checks=%d rx=%d samples=%d", checks, rx, len(samples))
			}
			state.mu.Lock()
			_, oldACK := state.traffic.committed[trafficKey{"b", timekey.Format(f.now.Truncate(time.Hour))}]
			state.mu.Unlock()
			if oldACK {
				t.Fatal("old generation durable ACK survived")
			}
			f.service.rateMu.Lock()
			defer f.service.rateMu.Unlock()
			if state.tokens != tokens || !state.refilled.Equal(refilled) {
				t.Fatal("storage generation reset rate limiter")
			}
		})
	}
}
