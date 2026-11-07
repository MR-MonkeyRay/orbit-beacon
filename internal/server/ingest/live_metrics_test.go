package ingest

import (
	"context"
	"crypto/sha256"
	"math"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

func liveFleetNode(t *testing.T, f *coalesceFixture) nodes.LiveNode {
	t.Helper()
	records, err := f.nodes.LiveRecords(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	return f.service.ProjectLiveRecord(&records[0], f.now)
}

func assertLiveMetrics(t *testing.T, node nodes.LiveNode, cpu, memory *float64) {
	t.Helper()
	equal := func(a, b *float64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
	if !equal(node.CPUPercent, cpu) || !equal(node.MemoryUsedPercent, memory) {
		t.Fatalf("metrics cpu=%v memory=%v, want cpu=%v memory=%v; node=%+v", node.CPUPercent, node.MemoryUsedPercent, cpu, memory, node)
	}
}

func TestLiveMetricsPublishedWithAcceptedReports(t *testing.T) {
	f := newCoalesceFixture(t, 30*time.Second)
	assertLiveMetrics(t, liveFleetNode(t, f), new(10.0), new(50.0))
	for _, delta := range []time.Duration{10 * time.Second, 30 * time.Second} {
		f.now = f.now.Add(delta)
		f.report.Seq++
		f.report.Metrics.CPUPercent = new(float64(f.report.Seq))
		f.report.Metrics.MemoryAvailableBytes = new(uint64(2048))
		if _, err := acceptReport(context.Background(), f.service, coalesceToken, f.report); err != nil {
			t.Fatal(err)
		}
		state := f.service.acquireExistingState(f.id)
		state.mu.Lock()
		if state.cpuPercent == nil || *state.cpuPercent != float64(f.report.Seq) || state.memoryUsedPercent == nil || *state.memoryUsedPercent != 75 {
			t.Fatal("accepted report did not publish summary")
		}
		state.mu.Unlock()
		f.service.releaseState(state)
		assertLiveMetrics(t, liveFleetNode(t, f), new(float64(f.report.Seq)), new(75.0))
	}
	// A duplicate must not replace the accepted summary, even on the durable path.
	f.now = f.now.Add(30 * time.Second)
	f.report.Metrics.CPUPercent = new(99.0)
	if _, err := acceptReport(context.Background(), f.service, coalesceToken, f.report); err != nil {
		t.Fatal(err)
	}
	assertLiveMetrics(t, liveFleetNode(t, f), new(3.0), new(75.0))
}

func TestLiveMetricsUnavailableAndZeroValues(t *testing.T) {
	for _, status := range []string{"ok", "stale", "disabled", "error", "unsupported", "permission_denied"} {
		t.Run(status, func(t *testing.T) {
			f := newCoalesceFixture(t, 30*time.Second)
			f.report.Seq++
			f.report.Metrics.CPUPercent = new(0.0)
			f.report.Metrics.MemoryAvailableBytes = f.report.Metrics.MemoryTotalBytes
			for _, key := range []string{v1.MetricCPUPercent, v1.MetricMemoryTotalBytes, v1.MetricMemoryAvailableBytes} {
				f.report.Metrics.MetricStatus[key] = status
			}
			if _, err := acceptReport(context.Background(), f.service, coalesceToken, f.report); err != nil {
				t.Fatal(err)
			}
			var want *float64
			if status == "ok" {
				want = new(0.0)
			}
			assertLiveMetrics(t, liveFleetNode(t, f), want, want)
		})
	}
	for _, missing := range []string{"cpu", "total", "available", "zero-total"} {
		t.Run(missing, func(t *testing.T) {
			f := newCoalesceFixture(t, 30*time.Second)
			f.report.Seq++
			cpu, memory := new(10.0), new(50.0)
			switch missing {
			case "cpu":
				f.report.Metrics.CPUPercent = nil
				f.report.Metrics.MetricStatus[v1.MetricCPUPercent] = "error"
				cpu = nil
			case "total":
				f.report.Metrics.MemoryTotalBytes = nil
				f.report.Metrics.MetricStatus[v1.MetricMemoryTotalBytes] = "error"
				memory = nil
			case "available":
				f.report.Metrics.MemoryAvailableBytes = nil
				f.report.Metrics.MetricStatus[v1.MetricMemoryAvailableBytes] = "disabled"
				memory = nil
			case "zero-total":
				f.report.Metrics.MemoryTotalBytes, f.report.Metrics.MemoryAvailableBytes = new(uint64(0)), new(uint64(0))
				memory = nil
			}
			if _, err := acceptReport(context.Background(), f.service, coalesceToken, f.report); err != nil {
				t.Fatal(err)
			}
			assertLiveMetrics(t, liveFleetNode(t, f), cpu, memory)
		})
	}
	// Missing status is rejected by protocol validation; the summary itself also requires ok.
	for _, m := range []v1.Metrics{
		{CPUPercent: new(10.0), MemoryTotalBytes: new(uint64(10)), MemoryAvailableBytes: new(uint64(5))},
		{CPUPercent: new(math.Inf(1)), MetricStatus: map[string]string{v1.MetricCPUPercent: "ok"}},
	} {
		snapshot := snapshotPublic(m, time.Time{})
		if liveMetricValue(snapshot.cpu) != nil || liveMetricValue(snapshot.memory) != nil {
			t.Fatal("unavailable metric became a live value")
		}
	}
}

func TestLiveMetricsNullWhenOfflineDisabledRevokedOrRestarted(t *testing.T) {
	for _, mutation := range []string{"offline", "future", "disabled", "revoke", "restart"} {
		t.Run(mutation, func(t *testing.T) {
			f := newCoalesceFixture(t, 30*time.Second)
			switch mutation {
			case "offline":
				f.now = f.now.Add(nodes.OnlineWindow + time.Nanosecond)
			case "future":
				f.now = f.now.Add(-time.Nanosecond)
			case "disabled":
				if err := f.nodes.SetDisabled(context.Background(), f.id, true); err != nil {
					t.Fatal(err)
				}
			case "revoke":
				if err := f.nodes.RevokeCredential(context.Background(), f.id); err != nil {
					t.Fatal(err)
				}
			case "restart":
				f.service = NewService(f.nodes.Database(), Options{Now: func() time.Time { return f.now }})
			}
			assertLiveMetrics(t, liveFleetNode(t, f), nil, nil)
		})
	}
}

func TestInvalidateIdentityClearsLatestAndLiveMetrics(t *testing.T) {
	credential := sha256.Sum256([]byte("credential"))
	state := &nodeState{credential: credential, credentialEpoch: 1, generation: 1, session: "session", latest: []byte("report"), cpuPercent: new(10.0), memoryUsedPercent: new(50.0)}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.invalidateIdentity(credential[:], 2, state.generation, state.session)
	if state.latest != nil || state.cpuPercent != nil || state.memoryUsedPercent != nil {
		t.Fatal("invalidated identity retained report or live summary")
	}
}

func TestLiveMetricSummaryUsesAvailableMemoryAndOKStatus(t *testing.T) {
	for _, tc := range []struct {
		name, cpuStatus, totalStatus, availableStatus string
		cpu                                           *float64
		total, available                              *uint64
		wantCPU, wantMemory                           *float64
	}{
		{"valid", "ok", "ok", "ok", new(10.0), new(uint64(8192)), new(uint64(2048)), new(10.0), new(75.0)},
		{"zero", "ok", "ok", "ok", new(0.0), new(uint64(8192)), new(uint64(8192)), new(0.0), new(0.0)},
		{"zero-total", "ok", "ok", "ok", new(10.0), new(uint64(0)), new(uint64(0)), new(10.0), nil},
		{"missing", "ok", "ok", "ok", nil, nil, nil, nil, nil},
		{"missing-status", "", "", "", new(10.0), new(uint64(10)), new(uint64(5)), nil, nil},
		{"stale-cpu-total", "stale", "stale", "ok", new(10.0), new(uint64(10)), new(uint64(5)), nil, nil},
		{"stale-available", "ok", "ok", "stale", new(10.0), new(uint64(10)), new(uint64(5)), new(10.0), nil},
		{"disabled", "disabled", "disabled", "disabled", new(10.0), new(uint64(10)), new(uint64(5)), nil, nil},
		{"error", "error", "error", "error", new(10.0), new(uint64(10)), new(uint64(5)), nil, nil},
		{"invalid-range", "ok", "ok", "ok", new(101.0), new(uint64(10)), new(uint64(11)), nil, nil},
		{"nonfinite", "ok", "ok", "ok", new(math.Inf(1)), new(uint64(10)), new(uint64(5)), nil, new(50.0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := snapshotPublic(v1.Metrics{CPUPercent: tc.cpu, MemoryTotalBytes: tc.total, MemoryAvailableBytes: tc.available, MetricStatus: map[string]string{v1.MetricCPUPercent: tc.cpuStatus, v1.MetricMemoryTotalBytes: tc.totalStatus, v1.MetricMemoryAvailableBytes: tc.availableStatus}}, time.Time{})
			assertLiveMetrics(t, nodes.LiveNode{CPUPercent: liveMetricValue(snapshot.cpu), MemoryUsedPercent: liveMetricValue(snapshot.memory)}, tc.wantCPU, tc.wantMemory)
		})
	}
}

func TestLiveMetricProjectionRequiresMatchingOnlineIdentity(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	credential := sha256.Sum256([]byte("credential"))
	for _, mutation := range []string{"none", "credential", "epoch", "generation", "session", "unregistered", "reset-generation", "reset-session", "no-latest", "offline", "disabled", "pending"} {
		t.Run(mutation, func(t *testing.T) {
			state := &nodeState{credential: credential, credentialEpoch: 1, generation: 1, session: "session", latest: []byte("invalid JSON must not be decoded"), receivedAt: now, cpuPercent: new(10.0), memoryUsedPercent: new(75.0)}
			service := NewService(nil, Options{})
			service.states["node"] = state
			record := nodes.LiveRecord{LiveNode: nodes.LiveNode{ID: "node", Status: "unknown", LastSeen: new(now), CPUPercent: new(99.0), MemoryUsedPercent: new(99.0)}, Identity: nodes.LiveIdentity{Credential: credential, CredentialEpoch: 1, Generation: 1, Session: "session", HasCredential: true}}
			switch mutation {
			case "credential":
				record.Identity.Credential = sha256.Sum256([]byte("other"))
			case "epoch":
				record.Identity.CredentialEpoch++
			case "generation":
				record.Identity.Generation++
			case "session":
				record.Identity.Session = "other"
			case "unregistered":
				record.Identity.HasCredential = false
			case "reset-generation":
				record.Identity.Generation = 0
			case "reset-session":
				record.Identity.Session = ""
			case "no-latest":
				state.latest = nil
			case "offline":
				state.receivedAt = now.Add(-nodes.OnlineWindow - time.Nanosecond)
				record.LastSeen = new(state.receivedAt)
			case "disabled":
				record.Status = "disabled"
			case "pending":
				record.Status = "pending"
			}
			result := service.ProjectLiveRecord(&record, now)
			if mutation == "none" {
				assertLiveMetrics(t, result, new(10.0), new(75.0))
			} else {
				assertLiveMetrics(t, result, nil, nil)
			}
		})
	}
}
