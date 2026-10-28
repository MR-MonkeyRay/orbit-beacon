package collect

import (
	"encoding/hex"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

func trafficFixture() (*TrafficAccumulator, *time.Time) {
	now := time.Date(2026, 10, 27, 12, 59, 0, 0, time.FixedZone("local", -7*3600))
	return NewTrafficAccumulator(func() time.Time { return now }), &now
}

func observeTraffic(a *TrafficAccumulator, rx, tx uint64) {
	a.Observe([]string{"eth0"}, map[string]NetworkCounter{"eth0": {RxBytes: rx, TxBytes: tx}})
}

func TestTrafficDeltasResetsAndSelection(t *testing.T) {
	a, _ := trafficFixture()
	if entries := a.Entries(); entries == nil || len(entries) != 0 {
		t.Fatalf("unavailable counters must send []: %+v", entries)
	}
	observeTraffic(a, 100, 200)
	first := a.Entries()
	if len(first) != 1 || first[0].RxBytes != "0" || first[0].TxBytes != "0" {
		t.Fatalf("new interface needs baseline: %+v", first)
	}
	if id, err := hex.DecodeString(first[0].Epoch); err != nil || len(id) != 16 {
		t.Fatalf("epoch is not random 128-bit hex: %q", first[0].Epoch)
	}
	if other := NewTrafficAccumulator(nil).epoch; other == first[0].Epoch {
		t.Fatal("independent processes share epoch")
	}
	observeTraffic(a, 150, 300)
	observeTraffic(a, 5, 320) // RX reset must not discard TX's ordinary delta.
	if got := a.Entries()[0]; got.RxBytes != "55" || got.TxBytes != "120" {
		t.Fatalf("incorrect deltas/reset: %+v", got)
	}
	a.Observe([]string{"eth0", "eth1"}, map[string]NetworkCounter{"eth0": {RxBytes: 10, TxBytes: 3}, "eth1": {RxBytes: 900, TxBytes: 900}})
	if got := a.Entries()[0]; got.RxBytes != "60" || got.TxBytes != "123" {
		t.Fatalf("new interface counted history or reset lost: %+v", got)
	}
	a.Observe([]string{"eth1"}, nil)
	a.Observe([]string{"eth0", "eth1"}, map[string]NetworkCounter{"eth0": {RxBytes: 1000, TxBytes: 1000}, "eth1": {RxBytes: 910, TxBytes: 920}})
	if got := a.Entries()[0]; got.RxBytes != "70" || got.TxBytes != "143" {
		t.Fatalf("reselected interface did not baseline: %+v", got)
	}
	before := a.Entries()
	a.Observe([]string{"eth0", "eth1"}, nil) // A read failure does not consume counters.
	if got := a.Entries(); !reflect.DeepEqual(got, before) {
		t.Fatalf("read failure changed totals: %+v", got)
	}
}

func TestTrafficHourRolloverAndSettlement(t *testing.T) {
	a, now := trafficFixture()
	observeTraffic(a, 100, 200)
	observeTraffic(a, 150, 300)
	old := a.Entries()[0]
	if old.PeriodStart.Location() != time.UTC || old.PeriodStart.Minute() != 0 || old.PeriodStart.Second() != 0 || old.PeriodStart.Nanosecond() != 0 {
		t.Fatalf("noncanonical period: %v", old.PeriodStart)
	}
	low := old
	low.RxBytes = "49"
	a.Acknowledge([]v1.Traffic{low})
	*now = now.Add(2 * time.Minute)
	observeTraffic(a, 170, 340)
	entries := a.Entries()
	if len(entries) != 2 || entries[0] != old || entries[1].RxBytes != "20" || entries[1].TxBytes != "40" {
		t.Fatalf("rollover must attribute delta to sample hour and retain under-acked bucket: %+v", entries)
	}
	unknown := old
	unknown.Epoch = strings.Repeat("f", 32)
	missing := old
	missing.PeriodStart = missing.PeriodStart.Add(-time.Hour)
	higher := old
	higher.TxBytes = "101"
	a.Acknowledge([]v1.Traffic{unknown, missing, higher, low})
	if len(a.Entries()) != 2 {
		t.Fatal("unknown/non-exact ack settled bucket")
	}
	a.Acknowledge([]v1.Traffic{old, entries[1]})
	if got := a.Entries(); len(got) != 1 || got[0] != entries[1] {
		t.Fatalf("closed bucket must settle, current must stay: %+v", got)
	}
	observeTraffic(a, 180, 350)
	*now = now.Add(time.Hour)
	observeTraffic(a, 190, 360)
	if len(a.Entries()) != 2 {
		t.Fatal("ack predating growth settled bucket")
	}
}

func TestTrafficAckBeforeFinalSample(t *testing.T) {
	a, now := trafficFixture()
	observeTraffic(a, 1, 1)
	acked := a.Entries()[0]
	a.Acknowledge([]v1.Traffic{acked})
	if len(a.Entries()) != 1 {
		t.Fatal("acked current hour dropped")
	}
	*now = now.Add(2 * time.Minute)
	a.Acknowledge([]v1.Traffic{acked})
	if got := a.Entries(); len(got) != 2 {
		t.Fatalf("wall clock alone settled old bucket: %+v", got)
	}
	a.Observe([]string{"eth0"}, nil)
	if len(a.Entries()) != 2 {
		t.Fatal("failed read settled old bucket")
	}
	observeTraffic(a, 2, 3)
	if got := a.Entries(); len(got) != 1 || got[0].RxBytes != "1" || got[0].TxBytes != "2" {
		t.Fatalf("remembered exact ack not settled after sample: %+v", got)
	}
}

func TestTrafficClockRollbackAfterSettlement(t *testing.T) {
	a, now := trafficFixture()
	observeTraffic(a, 0, 0)
	observeTraffic(a, 1000, 2000)
	settled := a.Entries()[0]
	a.Acknowledge([]v1.Traffic{settled})
	*now = now.Add(2 * time.Minute)
	observeTraffic(a, 1010, 2020)
	current := a.Entries()
	if len(current) != 1 || !current[0].PeriodStart.Equal(settled.PeriodStart.Add(time.Hour)) {
		t.Fatalf("earlier hour did not settle: %+v", current)
	}
	a.Acknowledge(current)
	*now = now.Add(-2 * time.Minute)
	if got := a.Entries(); !reflect.DeepEqual(got, current) {
		t.Fatalf("report clock rollback recreated settled hour: %+v", got)
	}
	observeTraffic(a, 1020, 2040)
	a.Acknowledge([]v1.Traffic{settled})
	got := a.Entries()
	if len(got) != 1 || got[0].Epoch != settled.Epoch || got[0].PeriodStart != current[0].PeriodStart || got[0].RxBytes != "20" || got[0].TxBytes != "40" {
		t.Fatalf("rollback delta must accumulate in newest observed hour: %+v", got)
	}
	a.Acknowledge(got)
	*now = now.Add(3 * time.Minute)
	observeTraffic(a, 1025, 2050)
	if got := a.Entries(); len(got) != 1 || got[0].RxBytes != "25" || got[0].TxBytes != "50" {
		t.Fatalf("clock recovery lost cumulative totals: %+v", got)
	}
}

func TestTrafficClockRollbackWithinHour(t *testing.T) {
	a, now := trafficFixture()
	observeTraffic(a, 0, 0)
	observeTraffic(a, 1000, 2000)
	before := a.Entries()[0]
	a.Acknowledge([]v1.Traffic{before})
	*now = now.Add(-30 * time.Minute)
	observeTraffic(a, 1010, 2020)
	a.Acknowledge([]v1.Traffic{before}) // A stale ack cannot settle the new delta.
	got := a.Entries()
	if len(got) != 1 || got[0].Epoch != before.Epoch || got[0].PeriodStart != before.PeriodStart || got[0].RxBytes != "1010" || got[0].TxBytes != "2020" {
		t.Fatalf("same-hour rollback reset cumulative totals: %+v", got)
	}
	*now = now.Add(time.Hour)
	observeTraffic(a, 1015, 2030)
	got = a.Entries()
	if len(got) != 2 || got[0].RxBytes != "1010" || got[0].TxBytes != "2020" || got[1].RxBytes != "5" || got[1].TxBytes != "10" {
		t.Fatalf("stale ack discarded rollback delta at rollover: %+v", got)
	}
}

func TestTrafficRecoversFromBogusForwardJump(t *testing.T) {
	a, now := trafficFixture()
	real := now.UTC().Truncate(time.Hour)
	observeTraffic(a, 0, 0)
	*now = now.AddDate(5, 0, 0)
	observeTraffic(a, 10, 20)
	*now = now.AddDate(-5, 0, 0)
	observeTraffic(a, 15, 30)
	got := a.Entries()
	if last := got[len(got)-1]; len(got) != 1 || !last.PeriodStart.Equal(real) || last.RxBytes != "5" || last.TxBytes != "10" {
		t.Fatalf("corrected clock still pinned to the future hour: %+v", got)
	}
}

func TestTrafficForwardJumpRecoveryUsesNewEpochForSettledHour(t *testing.T) {
	a, now := trafficFixture()
	observeTraffic(a, 0, 0)
	observeTraffic(a, 100, 200)
	settled := a.Entries()[0]
	a.Acknowledge([]v1.Traffic{settled})
	*now = now.AddDate(1, 0, 0)
	observeTraffic(a, 110, 220) // closes and deletes the acknowledged hour
	*now = now.AddDate(-1, 0, 0)
	observeTraffic(a, 115, 230)
	got := a.Entries()
	last := got[len(got)-1]
	if !last.PeriodStart.Equal(settled.PeriodStart) || last.Epoch == settled.Epoch || last.RxBytes != "5" || last.TxBytes != "10" {
		t.Fatalf("recreated hour must use a fresh epoch so the server sums it: settled=%+v got=%+v", settled, got)
	}
}

func TestTrafficReportAfterForwardJumpRecoveryUsesNewEpoch(t *testing.T) {
	a, now := trafficFixture()
	observeTraffic(a, 0, 0)
	observeTraffic(a, 100, 200)
	settled := a.Entries()[0]
	a.Acknowledge([]v1.Traffic{settled})
	*now = now.AddDate(1, 0, 0)
	observeTraffic(a, 110, 220)
	*now = now.AddDate(-1, 0, 0)
	a.Entries() // a report built before the next sample must not reuse the old epoch
	observeTraffic(a, 115, 230)
	got := a.Entries()
	last := got[len(got)-1]
	if len(got) != 1 || !last.PeriodStart.Equal(settled.PeriodStart) || last.Epoch == settled.Epoch || last.RxBytes != "5" {
		t.Fatalf("report-first recovery recreated the settled hour under its old epoch: %+v", got)
	}
}

func TestTrafficCapsOldestAt24(t *testing.T) {
	a, now := trafficFixture()
	start := now.UTC().Truncate(time.Hour)
	for i := range 30 {
		observeTraffic(a, uint64(i), uint64(i*2))
		*now = now.Add(time.Hour)
	}
	entries := a.Entries()
	// The unsampled current hour must not evict the oldest sampled bucket.
	if len(entries) != 24 || !entries[0].PeriodStart.Equal(start.Add(6*time.Hour)) || !entries[23].PeriodStart.Equal(start.Add(29*time.Hour)) {
		t.Fatalf("not bounded to newest 24 sampled hours: %+v", entries)
	}
	for i := 1; i < len(entries); i++ {
		if !entries[i-1].PeriodStart.Before(entries[i].PeriodStart) {
			t.Fatal("entries not ordered oldest first")
		}
	}
}

func TestTrafficSaturatesAtSQLiteIntegerLimit(t *testing.T) {
	a, _ := trafficFixture()
	observeTraffic(a, 0, 0)
	observeTraffic(a, math.MaxUint64, math.MaxUint64)
	observeTraffic(a, 100, 100)
	got := a.Entries()[0]
	if got.RxBytes != "9223372036854775807" || got.TxBytes != got.RxBytes || got.Validate() != nil {
		t.Fatalf("overflow produced invalid/decreasing wire values: %+v", got)
	}
}

func TestTrafficConcurrentReporting(t *testing.T) {
	a := NewTrafficAccumulator(func() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) })
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 100 {
			observeTraffic(a, uint64(i), uint64(i))
		}
	})
	wg.Go(func() {
		for range 100 {
			a.Acknowledge(a.Entries())
		}
	})
	wg.Wait()
	if got := a.Entries(); len(got) != 1 || got[0].RxBytes != "99" {
		t.Fatalf("concurrent reporting lost totals: %+v", got)
	}
}

func TestTrafficExactHourBoundaryClosesAckedBucket(t *testing.T) {
	a, now := trafficFixture()
	observeTraffic(a, 100, 200)
	a.Acknowledge(a.Entries())
	*now = now.UTC().Truncate(time.Hour).Add(time.Hour)
	observeTraffic(a, 110, 220)
	if got := a.Entries(); len(got) != 1 || !got[0].PeriodStart.Equal(*now) || got[0].RxBytes != "10" || got[0].TxBytes != "20" {
		t.Fatalf("boundary observation did not close old hour: %+v", got)
	}
}

func TestTrafficUsesCapturedSampleTime(t *testing.T) {
	a, now := trafficFixture()
	observeTraffic(a, 100, 200)
	sampledAt := *now
	*now = now.Add(2 * time.Minute) // Processing/reporting happens in the next hour.
	a.ObserveAt(sampledAt, []string{"eth0"}, map[string]NetworkCounter{"eth0": {RxBytes: 110, TxBytes: 220}})
	entries := a.Entries()
	if len(entries) != 2 || entries[0].RxBytes != "10" || entries[0].TxBytes != "20" || entries[1].RxBytes != "0" {
		t.Fatalf("delta attributed to processing time instead of sample time: %+v", entries)
	}
}

func TestTrafficReportDoesNotEvictSampledBucketAfterForwardJump(t *testing.T) {
	a, now := trafficFixture()
	start := now.UTC().Truncate(time.Hour)
	for i := range 25 {
		observeTraffic(a, uint64(i), uint64(i))
		*now = now.Add(time.Hour)
	}
	*now = now.Add(-time.Hour)
	*now = now.AddDate(5, 0, 0)
	a.Entries() // A report alone during a bogus jump.
	*now = now.AddDate(-5, 0, 0)
	entries := a.Entries()
	if len(entries) != 24 || !entries[0].PeriodStart.Equal(start.Add(time.Hour)) || entries[0].RxBytes != "1" {
		t.Fatalf("report-only forward jump evicted a sampled bucket: %+v", entries)
	}
}

func TestTrafficReportDoesNotAdvanceSampleHour(t *testing.T) {
	a, now := trafficFixture()
	observeTraffic(a, 100, 200)
	sampledAt := *now
	*now = now.Add(2 * time.Minute)
	if got := a.Entries(); len(got) != 2 || got[1].RxBytes != "0" {
		t.Fatalf("current hour not reported as zero: %+v", got)
	}
	a.ObserveAt(sampledAt, []string{"eth0"}, map[string]NetworkCounter{"eth0": {RxBytes: 110, TxBytes: 220}})
	entries := a.Entries()
	if len(entries) != 2 || !entries[0].PeriodStart.Equal(sampledAt.UTC().Truncate(time.Hour)) || entries[0].RxBytes != "10" || entries[1].RxBytes != "0" {
		t.Fatalf("report moved a delayed sample into the next hour: %+v", entries)
	}
}
