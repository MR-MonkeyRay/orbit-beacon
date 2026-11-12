package services

import (
	"context"
	"time"
)

// PublicService is independent of configuration and check errors: neither can leak.
type PublicService struct {
	Name         string     `json:"name"`
	Type         string     `json:"type"`
	Status       string     `json:"status"`
	LatencyMS    *float64   `json:"latency_ms"`
	Availability []*float64 `json:"availability"`
}

func (m *Manager) Public(ctx context.Context) ([]PublicService, error) {
	return m.PublicAt(ctx, m.now())
}

// PublicAt uses the enclosing public snapshot's projection time, including freshness and UTC days.
func (m *Manager) PublicAt(ctx context.Context, now time.Time) ([]PublicService, error) {
	now = now.UTC()
	list, e := m.list(ctx, now)
	if e != nil {
		return nil, e
	}
	start := now.Truncate(24*time.Hour).AddDate(0, 0, -29)
	totals, e := m.totals(ctx, start.Unix(), 86400)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		m.logError(ctx, "service_history_read_failed", e)
	}
	out := []PublicService{}
	for _, s := range list {
		if !s.Enabled || !s.Public {
			continue
		}
		p := PublicService{Name: s.Name, Type: s.Type, Status: "unknown", Availability: make([]*float64, 30)}
		if s.Latest != nil && !s.Latest.CheckedAt.After(now) && now.Sub(s.Latest.CheckedAt) <= time.Duration(2*s.IntervalSeconds+s.TimeoutSeconds)*time.Second {
			p.Status = s.Latest.Status
			if s.Latest.OK {
				p.LatencyMS = s.Latest.LatencyMS
			}
		}
		for i := range p.Availability {
			a := totals[bucketKey{s.ID, start.AddDate(0, 0, i).Unix()}]
			p.Availability[i] = percent(a.Successes, a.Checks)
		}
		out = append(out, p)
	}
	return out, nil
}
