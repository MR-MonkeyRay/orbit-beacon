package collect

import (
	"context"
	"os"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/disk"

	v1 "orbitbeacon/internal/protocol/v1"
)

type diskReading struct {
	path  string
	usage *disk.UsageStat
	err   error
	at    time.Time
}

type diskCollection struct {
	epoch uint64
	count int
	reads [8]diskReading
	done  chan struct{}
}

func (c *Collector) finishDisks(job *diskCollection) {
	if job.epoch == c.diskEpoch {
		interval := time.Duration(c.config.DiskIntervalSeconds) * time.Second
		for _, reading := range job.reads[:job.count] {
			sample := diskSample{metric: v1.DiskMetrics{Path: reading.path, SampledAt: reading.at.UTC()}, at: reading.at, status: "ok", source: c.source("disk:"+reading.path, reading.at, interval)}
			if reading.err != nil || reading.usage == nil || reading.usage.Used > reading.usage.Total {
				sample.status = failureStatus(reading.err)
			} else {
				sample.metric.TotalBytes, sample.metric.UsedBytes = &reading.usage.Total, &reading.usage.Used
			}
			c.disks[reading.path] = sample
		}
	}
	c.diskPending = nil
}

func (c *Collector) collectDisks(ctx context.Context, m *v1.Metrics) {
	paths := c.config.DiskPaths
	if len(paths) == 0 {
		root := "/"
		if runtime.GOOS == "windows" {
			root = os.Getenv("SystemDrive") + `\`
		}
		paths = []string{root}
	}
	if c.disks == nil {
		c.disks = make(map[string]diskSample, len(paths))
	}
	if job := c.diskPending; job != nil {
		select {
		case <-job.done:
			c.finishDisks(job)
		default:
		}
	}
	interval := time.Duration(c.config.DiskIntervalSeconds) * time.Second
	for _, path := range paths {
		if _, exists := c.disks[path]; !exists {
			at := time.Now()
			c.disks[path] = diskSample{metric: v1.DiskMetrics{Path: path, SampledAt: at.UTC()}, at: at, status: "stale", source: c.source("disk:"+path, at, interval)}
		}
	}
	if c.diskPending == nil && ctx.Err() == nil {
		var due [8]string
		count := 0
		for _, path := range paths {
			sample := c.disks[path]
			if sample.status == "stale" || time.Since(sample.at) >= interval {
				due[count] = path
				count++
			}
		}
		if count != 0 {
			job := &diskCollection{epoch: c.diskEpoch, count: count, done: make(chan struct{})}
			for i, path := range due[:count] {
				job.reads[i].path = path
			}
			c.diskPending = job
			read := c.diskUsage
			go func() {
				defer close(job.done)
				for i := range job.reads[:job.count] {
					reading := &job.reads[i]
					reading.err = checked(ctx, func() (err error) {
						reading.usage, err = read(ctx, reading.path)
						return err
					})
					reading.at = time.Now()
				}
			}()
			// Only a newly started read may use this snapshot's remaining budget.
			// A previous hung syscall is never awaited again or replaced by more
			// workers. Closing done publishes all readings without sharing cache.
			select {
			case <-job.done:
				c.finishDisks(job)
			case <-ctx.Done():
			}
		}
	}
	for _, path := range paths {
		sample := c.disks[path]
		sample.source.AgeMS = max(int64(0), time.Since(sample.at).Milliseconds())
		if !sample.source.Fresh() {
			sample.status = "stale"
			sample.metric.TotalBytes, sample.metric.UsedBytes = nil, nil
		}
		m.Disks = append(m.Disks, sample.metric)
		m.MetricStatus["disk:"+path], m.Sources["disk:"+path] = sample.status, sample.source
	}
}
