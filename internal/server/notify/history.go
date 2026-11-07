package notify

import (
	"context"

	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
)

type DeliveryPage struct {
	Deliveries []Delivery `json:"deliveries"`
	HasMore    bool       `json:"has_more"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

func (s *Service) Deliveries(ctx context.Context, limit int, before string) (DeliveryPage, error) {
	page := DeliveryPage{Deliveries: make([]Delivery, 0)}
	if limit < 1 || limit > 100 {
		return page, validation.Invalid("limit", "must be between 1 and 100")
	}
	query := `SELECT id,channel_id,event_id,state,attempts,next_attempt_at,last_error_code,coalesced_count,created_at,updated_at FROM notification_deliveries WHERE 1=1`
	args := []any{}
	if before != "" {
		stamp, id, e := management.DecodeEventCursor(before)
		if e != nil {
			return page, validation.Invalid("before", "invalid cursor")
		}
		query += " AND (created_at,id)<(?,?)"
		args = append(args, stamp, id)
	}
	query += " ORDER BY created_at DESC,id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, e := s.db.QueryContext(ctx, query, args...)
	if e != nil {
		return page, e
	}
	defer rows.Close()
	for rows.Next() {
		var d Delivery
		var next, created, updated string
		if e = rows.Scan(&d.ID, &d.ChannelID, &d.EventID, &d.State, &d.Attempts, &next, &d.LastErrorCode, &d.CoalescedCount, &created, &updated); e != nil {
			return page, e
		}
		if d.NextAttemptAt, e = timekey.Parse(next); e != nil {
			return page, e
		}
		if d.CreatedAt, e = timekey.Parse(created); e != nil {
			return page, e
		}
		if d.UpdatedAt, e = timekey.Parse(updated); e != nil {
			return page, e
		}
		page.Deliveries = append(page.Deliveries, d)
	}
	if e = rows.Err(); e != nil {
		return page, e
	}
	if len(page.Deliveries) > limit {
		page.HasMore = true
		page.Deliveries = page.Deliveries[:limit]
		last := page.Deliveries[limit-1]
		page.NextCursor = management.EncodeEventCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}
