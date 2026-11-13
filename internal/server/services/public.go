package services

import (
	"context"
	"time"
)

// PublicService is independent of configuration and check errors: neither can leak.
type PublicService struct {
	// Group is nil when disabled, a pointer to nil when ungrouped, or an index.
	Group        **int      `json:"group,omitempty"`
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
	out, _, err := m.PublicAtWithGroups(ctx, now, false)
	return out, err
}

func (m *Manager) PublicAtWithGroups(ctx context.Context, now time.Time, showGroups bool) ([]PublicService, []string, error) {
	now = now.UTC()
	list, e := m.list(ctx, now)
	if e != nil {
		return nil, nil, e
	}
	var groupNames []string
	indexes := map[string]int{}
	if showGroups {
		groups, err := m.Groups(ctx)
		if err != nil {
			return nil, nil, err
		}
		visible := map[string]bool{}
		for _, service := range list {
			if service.Enabled && service.Public && service.GroupID != nil {
				visible[*service.GroupID] = true
			}
		}
		groupNames = []string{}
		for _, group := range groups {
			if visible[group.ID] {
				indexes[group.ID] = len(groupNames)
				groupNames = append(groupNames, group.Name)
			}
		}
	}
	start := now.Truncate(24*time.Hour).AddDate(0, 0, -29)
	totals, e := m.totals(ctx, start.Unix(), 86400)
	if e != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		m.logError(ctx, "service_history_read_failed", e)
	}
	out := []PublicService{}
	for _, s := range list {
		if !s.Enabled || !s.Public {
			continue
		}
		p := PublicService{Name: s.Name, Type: s.Type, Status: "unknown", Availability: make([]*float64, 30)}
		if showGroups {
			var group *int
			if s.GroupID != nil {
				if index, ok := indexes[*s.GroupID]; ok {
					group = &index
				}
			}
			p.Group = &group
		}
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
	return out, groupNames, nil
}
