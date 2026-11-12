package services

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"orbitbeacon/internal/server/storage"
)

// Clock/Timer make scheduling and jitter testable without sleeping.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}
type realClock struct{}
type realTimer struct{ *time.Timer }

func (realClock) Now() time.Time                 { return time.Now() }
func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }
func (t realTimer) C() <-chan time.Time          { return t.Timer.C }

type scheduled struct {
	service Service
	next    time.Time
	running bool
}
type completed struct {
	service Service
	result  Result
}

func (m *Manager) configs(ctx context.Context) ([]Service, error) {
	lease, e := m.db.Acquire(ctx, storage.Reader)
	if e != nil {
		return nil, e
	}
	defer lease.Close()
	rows, e := lease.QueryContext(lease.Context(), "SELECT "+configColumns+" FROM services WHERE enabled=1 ORDER BY display_order,id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Service{}
	for rows.Next() {
		s, e := scanService(rows)
		if e != nil {
			return nil, e
		}
		s.generation = lease.Generation()
		out = append(out, s)
	}
	return out, rows.Err()
}
func (m *Manager) Run(ctx context.Context) { m.RunWith(ctx, realClock{}, NewChecker(), rand.Float64) }
func (m *Manager) RunWith(ctx context.Context, clock Clock, checker Checker, random func() float64) {
	jobs := make(chan Service)
	done := make(chan completed, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case s := <-jobs:
					r := checker.Check(ctx, s)
					select {
					case done <- completed{s, r}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	defer func() {
		workers.Wait()
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		m.logError(flushCtx, "service_shutdown_flush_failed", m.Flush(flushCtx))
	}()
	entries := map[string]*scheduled{}
	inFlight := map[string]bool{}
	flushAt, reloadAt := clock.Now().Add(10*time.Second), clock.Now()
	paused := false
	storagePaused := false
	configPaused := false
	jitter := func(s Service) time.Duration { return jitteredInterval(s.IntervalSeconds, random()) }
	reload := func() {
		list, e := m.configs(ctx)
		m.logError(ctx, "service_reload_failed", e)
		reloadAt = clock.Now().Add(time.Minute)
		configPaused = e != nil
		if configPaused {
			paused = true
			return
		}
		next := map[string]*scheduled{}
		for _, s := range list {
			old := entries[s.ID]
			if old != nil && sameConfig(old.service, s) && old.service.generation == s.generation {
				next[s.ID] = old
			} else {
				at := clock.Now().Add(time.Duration(random() * float64(time.Duration(s.IntervalSeconds)*time.Second)))
				next[s.ID] = &scheduled{service: s, next: at}
				next[s.ID].running = inFlight[s.ID]
			}
		}
		entries = next
		paused = storagePaused || configPaused
	}
	for {
		if ctx.Err() != nil {
			return
		}
		now := clock.Now()
		if !now.Before(reloadAt) {
			reload()
		}
		if !now.Before(flushAt) {
			e := m.Flush(ctx)
			m.logError(ctx, "service_flush_failed", e)
			storagePaused = e != nil
			paused = storagePaused || configPaused
			flushAt = now.Add(10 * time.Second)
		}
		wake := minTime(flushAt, reloadAt)
		if !paused {
			for _, entry := range entries {
				if entry.running {
					continue
				}
				if !now.Before(entry.next) {
					select {
					case jobs <- entry.service:
						entry.running = true
						entry.next = now.Add(jitter(entry.service))
						inFlight[entry.service.ID] = true
					default:
						entry.next = now.Add(100 * time.Millisecond)
					}
				}
				if !entry.running {
					wake = minTime(wake, entry.next)
				}
			}
		}
		timer := clock.NewTimer(max(time.Millisecond, wake.Sub(clock.Now())))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.reload:
			timer.Stop()
			reload()
		case c := <-done:
			timer.Stop()
			delete(inFlight, c.service.ID)
			if entry := entries[c.service.ID]; entry != nil {
				entry.running = false
			}
			if e := m.Accept(ctx, c.service, c.result); e != nil && !errors.Is(e, context.Canceled) {
				m.logError(ctx, "service_result_failed", e)
			}
		case <-timer.C():
		}
	}
}

func jitteredInterval(seconds int, random float64) time.Duration {
	return time.Duration(float64(time.Duration(seconds)*time.Second) * (0.9 + 0.2*random))
}
