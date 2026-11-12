package notify

import (
	"context"
	"errors"

	"orbitbeacon/internal/server/services"
	"orbitbeacon/internal/server/storage"
)

// EnqueueService reuses the durable alert outbox, retry, rate and coalescing path.
func (s *Service) EnqueueService(ctx context.Context, tx *storage.Tx, event services.Event) error {
	// A canceled delivery worker may already have stopped during the monitor
	// shutdown flush. Persist its last events until explicit Shutdown closes admission.
	if s.stopCtx.Err() != nil {
		return ErrUnavailable
	}
	rows, e := tx.QueryContext(ctx, `SELECT id FROM notification_channels WHERE enabled=1 ORDER BY id`)
	if e != nil {
		return e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	if e = errors.Join(rows.Err(), rows.Close()); e != nil {
		return e
	}
	p := Payload{EventID: event.ID, State: event.Transition, Value: event.LatencyMS, StartedAt: event.OccurredAt, Service: &event}
	p.Rule.ID = event.ServiceID
	p.Rule.Name = event.Name
	p.Rule.Metric = "service"
	p.Rule.Target = &event.Target
	p.Rule.Severity = "warning"
	p.Node.Name = event.Name
	if event.Transition == "down" {
		p.Rule.Severity = "critical"
	}
	if event.Transition == "recovered" || event.Transition == "latency_recovered" {
		p.Rule.Severity = "info"
	}
	return s.enqueuePayload(ctx, tx, ids, event.ID, p)
}
