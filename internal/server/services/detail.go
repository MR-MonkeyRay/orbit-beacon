package services

import (
	"context"
	"time"
)

type Stats struct {
	Availability24H *float64 `json:"availability_24h"`
	Availability7D  *float64 `json:"availability_7d"`
	Availability30D *float64 `json:"availability_30d"`
	AvgLatencyMS24H *float64 `json:"avg_latency_ms_24h"`
}

type Detail struct {
	Service
	Stats Stats `json:"stats"`
}

func (m *Manager) Detail(ctx context.Context, id string) (Detail, error) {
	now := m.now().UTC()
	list, err := m.list(ctx, now)
	if err != nil {
		return Detail{}, err
	}
	var detail Detail
	found := false
	for _, s := range list {
		if s.ID == id {
			detail.Service = s
			found = true
			break
		}
	}
	if !found {
		return Detail{}, ErrNotFound
	}
	// Include the five-minute bucket overlapping each trailing window's start,
	// matching availability_24h and history's sample-based boundary convention.
	destinations := []**float64{&detail.Stats.Availability24H, &detail.Stats.Availability7D, &detail.Stats.Availability30D}
	for i, days := range []int{1, 7, 30} {
		totals, err := m.totalsBetween(ctx, now.Add(-time.Duration(days)*24*time.Hour).Unix()/300*300, now.Unix()/300*300, 300, id)
		if err != nil {
			if ctx.Err() != nil {
				return Detail{}, ctx.Err()
			}
			m.logError(ctx, "service_history_read_failed", err)
			continue
		}
		a := totals[bucketKey{id, 0}]
		*destinations[i] = percent(a.Successes, a.Checks)
		if i == 0 && a.Successes > 0 {
			avg := a.Sum / float64(a.Successes)
			detail.Stats.AvgLatencyMS24H = &avg
		}
	}
	return detail, nil
}
