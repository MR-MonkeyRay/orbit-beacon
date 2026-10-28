// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	gnet "github.com/shirou/gopsutil/v4/net"

	v1 "orbitbeacon/internal/protocol/v1"
)

func networkCounters(ctx context.Context) ([]gnet.IOCountersStat, error) {
	if root := os.Getenv("HOST_PROC"); runtime.GOOS == "linux" && root != "" {
		// Match the PID 1 namespace used by defaultInterface on host proc mounts.
		return gnet.IOCountersByFileWithContext(ctx, true, filepath.Join(root, "1", "net", "dev"))
	}
	return gnet.IOCountersWithContext(ctx, true)
}

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
	name    string
	counter NetworkCounter
	at      time.Duration
}

var differentialSequence atomic.Uint64
var differentialEpoch = strconv.FormatInt(time.Now().UnixNano(), 36)

const maxNetworkBaselines = 8

// NetworkDifferencer computes nonblocking, per-interface rates. Its zero value
// is ready to use. Fixed slots match the protocol's maximum of eight network
// series and avoid map allocation/churn when interface selections change.
type NetworkDifferencer struct {
	previous [maxNetworkBaselines]networkBaseline
	count    int
	interval time.Duration
}

func (d *NetworkDifferencer) forget(name string) {
	for i := 0; i < d.count; i++ {
		if d.previous[i].name != name {
			continue
		}
		d.count--
		d.previous[i] = d.previous[d.count]
		d.previous[d.count] = networkBaseline{}
		return
	}
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
	index := -1
	for i := 0; i < d.count; i++ {
		if d.previous[i].name == name {
			index = i
			break
		}
	}
	exists := index >= 0
	if !exists {
		if d.count < len(d.previous) {
			index = d.count
			d.count++
		} else {
			index = 0
			for i := 1; i < d.count; i++ {
				if d.previous[i].at < d.previous[index].at {
					index = i
				}
			}
		}
	}
	previous := d.previous[index]
	d.previous[index] = networkBaseline{name: name, counter: counter, at: at}
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
