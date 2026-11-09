package storage

import (
	"context"
	"time"
)

type notificationRateFloor struct {
	Channel string
	Window  time.Time
	Count   int
}

// SeedNotificationRate 在业务 guard 内吸收旧 SQL 窗口；已消耗的本地配额不能被业务回退减掉。
func (dbs *Databases) SeedNotificationRate(ctx context.Context, channel string, window time.Time, count int, now time.Time) (configured bool, err error) {
	if dbs == nil || dbs.replication == nil {
		return false, nil
	}
	if count < 0 || count > 20 || channel == "" || count > 0 && (window.IsZero() || window.After(now)) {
		return true, ErrIdentityMismatch
	}
	if count == 0 || !now.Before(window.Add(time.Minute)) {
		return true, nil
	}
	r := dbs.replication
	r.mu.Lock()
	defer r.mu.Unlock()
	key := channel + "/" + window.UTC().Format(time.RFC3339Nano)
	o := &r.record.Outage
	floor := o.Floors[key]
	if floor.Count >= count {
		return true, nil
	}
	old := copyOutage(*o)
	if o.Floors == nil {
		o.Floors = map[string]notificationRateFloor{}
	}
	if o.Starts == nil {
		o.Starts = map[string][]time.Time{}
	}
	// 旧 SQL 只有窗口总数，没有逐次时刻；记作现在，最多保守等待一分钟。
	for range count - floor.Count {
		o.Starts[channel] = append(o.Starts[channel], now)
	}
	o.Floors[key] = notificationRateFloor{Channel: channel, Window: window, Count: count}
	if err = r.persistLocked(ctx); err != nil {
		r.record.Outage = old
		return true, err
	}
	return true, nil
}
