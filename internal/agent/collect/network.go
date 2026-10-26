// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"strconv"
	"sync/atomic"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

// NetworkCounter is an interface's cumulative kernel byte counters.
type NetworkCounter struct {
	RxBytes uint64
	TxBytes uint64
}

// NetworkObservation describes one real counter observation, including warm-up.
type NetworkObservation struct {
	Network v1.NetworkMetrics
	Source  v1.SourceMetadata
	Status  string
}

type networkBaseline struct {
	counter NetworkCounter
	at      time.Duration
}

var differentialSequence atomic.Uint64
var differentialEpoch = strconv.FormatInt(time.Now().UnixNano(), 36)

// NetworkDifferencer computes nonblocking, per-interface rates. Its zero value
// is ready to use. Like Collector, it is called by the single sampling loop.
type NetworkDifferencer struct {
	previous map[string]networkBaseline
	interval time.Duration
}

// Observe uses elapsed monotonic time, never wall time. Counter resets and
// nonpositive windows establish a new baseline without inventing a rate.
func (d *NetworkDifferencer) Observe(name string, counter NetworkCounter, at time.Duration) NetworkObservation {
	interval := d.interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	window := int64(0)
	result := NetworkObservation{
		Network: v1.NetworkMetrics{Name: name},
		Source: v1.SourceMetadata{
			SampleID:            "net-" + differentialEpoch + "-" + strconv.FormatUint(differentialSequence.Add(1), 36),
			SourceIntervalMS:    interval.Milliseconds(),
			MeasurementWindowMS: &window,
		},
		Status: "stale",
	}
	if d.previous == nil {
		d.previous = make(map[string]networkBaseline, 8)
	}
	previous, exists := d.previous[name]
	if !exists && len(d.previous) >= 8 {
		var oldest string
		var oldestAt time.Duration
		for key, baseline := range d.previous {
			if oldest == "" || baseline.at < oldestAt {
				oldest, oldestAt = key, baseline.at
			}
		}
		delete(d.previous, oldest)
	}
	d.previous[name] = networkBaseline{counter: counter, at: at}
	elapsed := at - previous.at
	if !exists || elapsed <= 0 || counter.RxBytes < previous.counter.RxBytes || counter.TxBytes < previous.counter.TxBytes {
		return result
	}
	rx := float64(counter.RxBytes-previous.counter.RxBytes) / elapsed.Seconds()
	tx := float64(counter.TxBytes-previous.counter.TxBytes) / elapsed.Seconds()
	window = elapsed.Milliseconds()
	result.Network.RxBytesPerSec, result.Network.TxBytesPerSec = &rx, &tx
	result.Status = "ok"
	return result
}
