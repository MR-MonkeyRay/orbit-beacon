package collect

import (
	"crypto/rand"
	"encoding/hex"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

// Each bucket keeps the epoch it was created under, so rotating the epoch never
// re-reports an existing bucket's cumulative total under a new identity.
type trafficBucket struct {
	epoch  string
	rx, tx uint64
	closed bool
	acked  bool
}

// TrafficAccumulator retains bounded, cumulative hourly totals for this process.
// Collection and reporting may use it concurrently. now is injectable for tests.
type TrafficAccumulator struct {
	mu           sync.Mutex
	now          func() time.Time
	epoch        string
	previous     map[string]NetworkCounter
	buckets      map[time.Time]*trafficBucket
	enabled      bool
	newestPeriod time.Time
}

func NewTrafficAccumulator(now func() time.Time) *TrafficAccumulator {
	if now == nil {
		now = time.Now
	}
	return &TrafficAccumulator{now: now, epoch: newTrafficEpoch(), previous: make(map[string]NetworkCounter), buckets: make(map[time.Time]*trafficBucket)}
}

func newTrafficEpoch() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

// Observe consumes only real counter reads, never cached rate observations.
// Selected interfaces retain baselines through read failures; deselection drops
// their baselines so a later re-selection cannot count the unselected interval.
func (a *TrafficAccumulator) Observe(selected []string, counters map[string]NetworkCounter) {
	a.ObserveAt(a.now(), selected, counters)
}

// ObserveAt uses the counter read's timestamp, even when processing crosses an
// hour boundary. Observe supplies the injectable clock when no timestamp exists.
// Samples older than the newest observed hour accumulate in that newest hour.
func (a *TrafficAccumulator) ObserveAt(at time.Time, selected []string, counters map[string]NetworkCounter) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for name := range a.previous {
		if !slices.Contains(selected, name) {
			delete(a.previous, name)
		}
	}
	if len(counters) == 0 {
		return
	}
	a.enabled = true
	period := a.advance(at)
	bucket := a.bucket(period)
	for _, name := range selected {
		current, ok := counters[name]
		if !ok {
			continue
		}
		if previous, ok := a.previous[name]; ok {
			rx, tx := counterDelta(current.RxBytes, previous.RxBytes), counterDelta(current.TxBytes, previous.TxBytes)
			if rx != 0 || tx != 0 {
				bucket.rx, bucket.tx = trafficSum(bucket.rx, rx), trafficSum(bucket.tx, tx)
				bucket.acked = false
			}
		}
		a.previous[name] = current
	}
	for start, bucket := range a.buckets {
		if start.Before(period) {
			bucket.closed = true
			if bucket.acked {
				delete(a.buckets, start)
			}
		}
	}
	a.trim()
}

func counterDelta(current, previous uint64) uint64 {
	if current < previous {
		return current
	}
	return current - previous
}

// SQLite INTEGER bounds the wire totals. Saturation keeps pathological kernel
// values from wrapping, decreasing cumulative totals, or blocking all reports.
func trafficSum(total, delta uint64) uint64 {
	return total + min(delta, uint64(math.MaxInt64)-total)
}

// period clamps wall-clock rollback to the newest sampled hour. Reporting and
// acknowledgements use the same bound so settled hours cannot be recreated.
// The caller holds mu.
// Only small steps are clamped: a larger rollback means the newest hour came
// from a bogus forward jump, and pinning to it would strand counts in a future
// hour that the server discards.
func (a *TrafficAccumulator) period(at time.Time) time.Time {
	period := at.UTC().Truncate(time.Hour)
	if period.Before(a.newestPeriod) && a.newestPeriod.Sub(period) <= time.Hour {
		return a.newestPeriod
	}
	return period
}

// advance moves the newest hour forward for a sample. The caller holds mu.
func (a *TrafficAccumulator) advance(at time.Time) time.Time {
	period := a.recover(at)
	a.newestPeriod = period
	return period
}

// recover handles recovery from a bogus forward jump. Samples and reports both
// call it, so neither can recreate a settled hour under a stale epoch. Reports
// never move the newest hour forward, so a delayed sample keeps its captured
// hour. The caller holds mu.
func (a *TrafficAccumulator) recover(at time.Time) time.Time {
	period := a.period(at)
	if period.Before(a.newestPeriod) {
		// The server never accepts future hours, so those buckets could only
		// occupy slots until trimmed.
		for start := range a.buckets {
			if start.After(period) {
				delete(a.buckets, start)
			}
		}
		// Hours settled during the jump may be recreated from zero; the server
		// keeps MAX per epoch, so continuing the old epoch would hide new bytes.
		a.epoch = newTrafficEpoch()
		a.newestPeriod = period
	}
	return period
}

func (a *TrafficAccumulator) bucket(period time.Time) *trafficBucket {
	bucket := a.buckets[period]
	if bucket == nil {
		bucket = &trafficBucket{epoch: a.epoch}
		a.buckets[period] = bucket
	}
	return bucket
}

func (a *TrafficAccumulator) periods() []time.Time {
	periods := make([]time.Time, 0, len(a.buckets))
	for period := range a.buckets {
		periods = append(periods, period)
	}
	slices.SortFunc(periods, func(a, b time.Time) int { return a.Compare(b) })
	return periods
}

func (a *TrafficAccumulator) trim() {
	periods := a.periods()
	for _, period := range periods[:max(0, len(periods)-v1.MaxTrafficEntries)] {
		delete(a.buckets, period)
	}
}

// Entries returns an owned snapshot, oldest first. An unsampled current hour
// (clamped to the newest sampled hour after a wall-clock rollback) is reported
// as zero only when a slot is free; it never evicts a sampled bucket.
// Unsupported platforms never enable accumulation and continue to send [].
func (a *TrafficAccumulator) Entries() []v1.Traffic {
	a.mu.Lock()
	defer a.mu.Unlock()
	entries := make([]v1.Traffic, 0, len(a.buckets))
	if !a.enabled {
		return entries
	}
	current := a.recover(a.now())
	for _, period := range a.periods() {
		bucket := a.buckets[period]
		entries = append(entries, v1.Traffic{Epoch: bucket.epoch, PeriodStart: period, RxBytes: v1.DecimalUint64(strconv.FormatUint(bucket.rx, 10)), TxBytes: v1.DecimalUint64(strconv.FormatUint(bucket.tx, 10))})
	}
	// Every bucket is at or before newestPeriod, which recover keeps at or
	// before current, so the zero entry stays last.
	if a.buckets[current] == nil && len(entries) < v1.MaxTrafficEntries {
		entries = append(entries, v1.Traffic{Epoch: a.epoch, PeriodStart: current, RxBytes: "0", TxBytes: "0"})
	}
	return entries
}

// Acknowledge remembers exact committed totals, including current-hour acks.
// A clock rollover alone cannot settle a bucket: a later observation must have
// closed it, ensuring the last delta has been attributed to its sample's hour.
func (a *TrafficAccumulator) Acknowledge(acks []v1.Traffic) {
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.period(a.now())
	for _, ack := range acks {
		bucket := a.buckets[ack.PeriodStart]
		if bucket == nil || ack.Epoch != bucket.epoch || string(ack.RxBytes) != strconv.FormatUint(bucket.rx, 10) || string(ack.TxBytes) != strconv.FormatUint(bucket.tx, 10) {
			continue
		}
		bucket.acked = true
		if bucket.closed && ack.PeriodStart.Before(current) {
			delete(a.buckets, ack.PeriodStart)
		}
	}
}
