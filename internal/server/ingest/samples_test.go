package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/nodes"
)

func TestSamplesDurableCoalescedDuplicateAndRejected(t *testing.T) {
	f := newCoalesceFixture(t, 0)
	samples := make(chan history.Sample, 4)
	f.service = NewService(f.db, Options{Now: func() time.Time { return f.now }, Samples: samples})
	ctx := context.Background()
	// New service durably reclaims this report; only the successful snapshot counts.
	if _, err := f.service.Accept(ctx, f.id, f.report); err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 {
		t.Fatal("durable path did not record")
	}
	first := <-samples
	if first.NodeID != f.id || !first.ReceivedAt.Equal(f.now) || first.Values["cpu_percent"] != 10 {
		t.Fatalf("sample=%+v", first)
	}
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	*f.report.Metrics.CPUPercent = 30
	if _, err := f.service.Accept(ctx, f.id, f.report); err != nil {
		t.Fatal(err)
	}
	second := <-samples
	if f.writes(t) != 2 || second.Values["cpu_percent"] != 30 || !second.ReceivedAt.Equal(f.now) {
		t.Fatalf("fast sample=%+v writes=%d", second, f.writes(t))
	}
	*f.report.Metrics.CPUPercent = 70
	if second.Values["cpu_percent"] != 30 {
		t.Fatal("snapshot aliases report")
	}
	// Duplicate on the fast path, then on the interval-due durable path.
	if _, err := f.service.Accept(ctx, f.id, f.report); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(31 * time.Second)
	if _, err := f.service.Accept(ctx, f.id, f.report); err != nil {
		t.Fatal(err)
	}
	if len(samples) != 0 {
		t.Fatal("duplicate generated history")
	}
	f.report.Seq++
	f.report.RunGeneration = 0
	if _, err := f.service.Accept(ctx, f.id, f.report); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("invalid=%v", err)
	}
	f.report.RunGeneration = 1
	if err := f.nodes.RevokeCredential(ctx, f.id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Accept(ctx, f.id, f.report); !errors.Is(err, nodes.ErrUnauthenticated) {
		t.Fatalf("revoked=%v", err)
	}
	if len(samples) != 0 {
		t.Fatal("rejected report generated history")
	}
}

func TestSamplesFullSinkNeverBlocksAndCountsDrops(t *testing.T) {
	f := newCoalesceFixture(t, 0)
	samples := make(chan history.Sample, 1)
	samples <- history.Sample{}
	f.service = NewService(f.db, Options{Now: func() time.Time { return f.now }, Samples: samples})
	result := make(chan error, 1)
	go func() {
		_, err := f.service.Accept(context.Background(), f.id, f.report)
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("full sink blocked durable acceptance")
	}
	f.now = f.now.Add(time.Second)
	f.report.Seq++
	go func() { _, err := f.service.Accept(context.Background(), f.id, f.report); result <- err }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("full sink blocked coalesced acceptance")
	}
	if f.service.SamplesDropped() != 2 {
		t.Fatalf("drops=%d", f.service.SamplesDropped())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.service.Shutdown(ctx); err != nil {
		t.Fatalf("full sink blocked drain: %v", err)
	}
}
