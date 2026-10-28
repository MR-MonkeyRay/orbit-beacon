package notify

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/timekey"
)

const MaxPendingPerChannel = 100

var backoffs = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

var _ alerts.Notifier = (*Service)(nil)

func notificationPayload(n alerts.Notification) Payload {
	p := Payload{EventID: n.EventID, State: n.State, Value: n.Value, StartedAt: n.StartedAt, ResolvedAt: n.ResolvedAt}
	p.Rule.ID = n.Rule.ID
	p.Rule.Name = n.Rule.Name
	p.Rule.Metric = n.Rule.Metric
	p.Rule.Severity = n.Rule.Severity
	p.Node.ID = n.NodeID
	p.Node.Name = n.NodeName
	return p
}
func (s *Service) Notify(ctx context.Context, n alerts.Notification) {
	if s.closed.Load() || ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(s.stopCtx, cancel)
	defer stop()
	if e := s.enqueue(ctx, n); e != nil && ctx.Err() == nil {
		slog.Warn("notification enqueue failed", "error_code", "storage_error")
	}
}
func (s *Service) enqueue(ctx context.Context, n alerts.Notification) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	rows, e := tx.QueryContext(ctx, `SELECT c.id FROM notification_channels c JOIN alert_rule_channels r ON r.channel_id=c.id WHERE r.rule_id=? AND c.enabled=1`, n.Rule.ID)
	if e != nil {
		return e
	}
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	now := timekey.Format(s.opts.Now())
	p := notificationPayload(n)
	for _, id := range ids {
		var count int
		if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM notification_deliveries WHERE channel_id=? AND state='pending'", id).Scan(&count); e != nil {
			return e
		}
		raw, _ := json.Marshal(p)
		if count >= MaxPendingPerChannel {
			// Keep the most recent pending row as a durable summary. The worker only
			// reads its payload within its claim transaction, fencing concurrent floods.
			var summaryID string
			var total int
			e = tx.QueryRowContext(ctx, `SELECT id,coalesced_count FROM notification_deliveries WHERE channel_id=? AND state='pending' AND lease_until='' ORDER BY coalesced_count DESC,created_at DESC,id DESC LIMIT 1`, id).Scan(&summaryID, &total)
			if e == nil {
				p.SummaryCount = total + 1
				raw, _ = json.Marshal(p)
				p.SummaryCount = 0
				_, e = tx.ExecContext(ctx, "UPDATE notification_deliveries SET payload_json=?,event_id=?,coalesced_count=?,attempts=0,last_error_code='',updated_at=? WHERE id=?", string(raw), n.EventID, total+1, now, summaryID)
				if e != nil {
					return e
				}
				continue
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return e
			}
		}
		deliveryID, e := identity.UUID(rand.Reader)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `INSERT OR IGNORE INTO notification_deliveries(id,channel_id,event_id,state,next_attempt_at,payload_json,created_at,updated_at) VALUES(?,?,?,'pending',?,?,?,?)`, deliveryID, id, n.EventID, now, string(raw), now, now)
		if e != nil {
			return e
		}
	}
	return tx.Commit()
}

type Delivery struct {
	ID             string    `json:"id"`
	ChannelID      string    `json:"channel_id"`
	EventID        string    `json:"event_id"`
	State          string    `json:"state"`
	Attempts       int       `json:"attempts"`
	NextAttemptAt  time.Time `json:"next_attempt_at"`
	LastErrorCode  string    `json:"last_error_code"`
	CoalescedCount int       `json:"coalesced_count"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
type deliveryWork struct {
	id, channel, event, raw string
	attempts                int
}

// Run owns four bounded send lanes. Pending rows stay durable during network
// work. A restart retries uncertain sends (at-least-once delivery).
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	if s.cancel != nil || s.closed.Load() {
		s.mu.Unlock()
		return
	}
	workCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()
	defer close(s.done)
	defer s.closed.Store(true)
	stop := context.AfterFunc(s.stopCtx, cancel)
	defer stop()
	defer cancel()
	defer s.sender.client.CloseIdleConnections()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if workCtx.Err() != nil {
			return
		}
		s.process(workCtx)
		select {
		case <-workCtx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Service) process(ctx context.Context) {
	rows, e := s.db.QueryContext(ctx, `SELECT channel_id FROM notification_deliveries WHERE state='pending' AND next_attempt_at<=? GROUP BY channel_id ORDER BY min(next_attempt_at),channel_id LIMIT 4`, timekey.Format(s.opts.Now()))
	if e != nil {
		return
	}
	var channels []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			break
		}
		channels = append(channels, id)
	}
	rows.Close()
	if e != nil {
		return
	}
	var wg sync.WaitGroup
	for _, id := range channels {
		wg.Add(1)
		go func(id string) { defer wg.Done(); s.deliver(ctx, id) }(id)
	}
	wg.Wait()
}

// reserveRate is shared by test sends and the worker, persisted across restart.
func (s *Service) reserveRate(ctx context.Context, tx *sql.Tx, id string) (bool, time.Time, error) {
	now := s.opts.Now().UTC()
	var start string
	var count int
	e := tx.QueryRowContext(ctx, "SELECT window_start,count FROM notification_rate_limits WHERE channel_id=?", id).Scan(&start, &count)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return false, time.Time{}, e
	}
	window, parseErr := timekey.Parse(start)
	if parseErr != nil || !now.Before(window.Add(time.Minute)) {
		window = now
		count = 0
	}
	if count >= 20 {
		return false, window.Add(time.Minute), nil
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO notification_rate_limits(channel_id,window_start,count) VALUES(?,?,?) ON CONFLICT(channel_id) DO UPDATE SET window_start=excluded.window_start,count=excluded.count`, id, timekey.Format(window), count+1)
	return true, time.Time{}, e
}
func (s *Service) claim(ctx context.Context, channel string) (deliveryWork, bool, error) {
	var d deliveryWork
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return d, false, e
	}
	defer tx.Rollback()
	e = tx.QueryRowContext(ctx, `SELECT d.id,d.channel_id,d.event_id,d.payload_json,d.attempts FROM notification_deliveries d JOIN notification_channels c ON c.id=d.channel_id WHERE d.channel_id=? AND d.state='pending' AND d.next_attempt_at<=? AND d.lease_until<=? ORDER BY d.created_at,d.id LIMIT 1`, channel, timekey.Format(s.opts.Now()), timekey.Format(s.opts.Now())).Scan(&d.id, &d.channel, &d.event, &d.raw, &d.attempts)
	if errors.Is(e, sql.ErrNoRows) {
		return d, false, nil
	}
	if e != nil {
		return d, false, e
	}
	var enabled bool
	if e = tx.QueryRowContext(ctx, "SELECT enabled FROM notification_channels WHERE id=?", channel).Scan(&enabled); e != nil {
		return d, false, e
	}
	now := timekey.Format(s.opts.Now())
	if !enabled {
		_, e = tx.ExecContext(ctx, "UPDATE notification_deliveries SET state='failed',last_error_code='channel_disabled',updated_at=? WHERE id=?", now, d.id)
		if e != nil {
			return d, false, e
		}
		return d, false, tx.Commit()
	}
	allowed, next, e := s.reserveRate(ctx, tx, channel)
	if e != nil {
		return d, false, e
	}
	if !allowed {
		_, e = tx.ExecContext(ctx, "UPDATE notification_deliveries SET next_attempt_at=? WHERE channel_id=? AND state='pending' AND next_attempt_at<?", timekey.Format(next), channel, timekey.Format(next))
		if e != nil {
			return d, false, e
		}
		return d, false, tx.Commit()
	}
	if d.attempts >= 5 {
		_, e = tx.ExecContext(ctx, "UPDATE notification_deliveries SET state='failed',last_error_code='attempts_exhausted',updated_at=? WHERE id=?", now, d.id)
		if e != nil {
			return d, false, e
		}
		return d, false, tx.Commit()
	}
	d.attempts++
	// The lease protects this payload from flood coalescing while it is sent.
	_, e = tx.ExecContext(ctx, "UPDATE notification_deliveries SET attempts=?,next_attempt_at=?,lease_until=?,updated_at=? WHERE id=?", d.attempts, timekey.Format(s.opts.Now().Add(SendTimeout)), timekey.Format(s.opts.Now().Add(SendTimeout)), now, d.id)
	if e != nil {
		return d, false, e
	}
	return d, true, tx.Commit()
}
func (s *Service) deliver(ctx context.Context, channel string) {
	d, ok, e := s.claim(ctx, channel)
	if e != nil || !ok {
		return
	}
	c, secret, e := s.load(ctx, channel)
	result := failure("secret_unavailable", true)
	if errors.Is(e, ErrSecretUnreadable) || errors.Is(e, ErrUnavailable) {
		result = failure("secret_unreadable", true)
		s.warnSecretUnreadable(channel)
	}
	if e == nil {
		p, parseErr := ParsePayload([]byte(d.raw))
		if parseErr != nil {
			result = failure("invalid_payload", false)
		} else {
			result = s.sender.Send(ctx, c, secret, p)
		}
	}
	if ctx.Err() != nil {
		return
	} // Remains pending for restart on shutdown deadline.
	state := "failed"
	next := s.opts.Now()
	if result.OK {
		state = "sent"
	} else if result.retry && d.attempts < 5 {
		state = "pending"
		next = next.Add(max(backoffs[d.attempts-1], result.retryAfter))
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=?,next_attempt_at=?,last_error_code=?,lease_until='',updated_at=? WHERE id=?`, state, timekey.Format(next), result.ErrorCode, timekey.Format(s.opts.Now()), d.id)
	if e != nil {
		return
	}
	if result.OK {
		_, e = tx.ExecContext(ctx, "UPDATE alert_events SET last_notified_at=? WHERE id=?", timekey.Format(s.opts.Now()), d.event)
		if e != nil {
			return
		}
	}
	if e = tx.Commit(); e != nil {
		slog.Warn("notification result persistence failed", "error_code", "storage_error")
	}
}

// Bound repeated diagnostics across all pending rows for the same channel.
func (s *Service) warnSecretUnreadable(channel string) {
	now := s.opts.Now()
	s.mu.Lock()
	last, exists := s.secretWarnings[channel]
	if exists && now.Before(last.Add(time.Hour)) {
		s.mu.Unlock()
		return
	}
	// At most 20 channels exist. Prune old/deleted channel entries as well.
	for id, stamp := range s.secretWarnings {
		if !now.Before(stamp.Add(time.Hour)) {
			delete(s.secretWarnings, id)
		}
	}
	s.secretWarnings[channel] = now
	s.mu.Unlock()
	slog.Warn("notification credentials unreadable; restore key or replace credentials", "channel_id", channel, "error_code", "secret_unreadable")
}

// Shutdown rejects new queue writes and cancels sends immediately. The caller
// supplies at most five seconds; network I/O is closed on cancellation.
func (s *Service) Shutdown(ctx context.Context) error {
	s.closed.Store(true)
	s.stop()
	s.mu.Lock()
	cancel := s.cancel
	if cancel != nil {
		cancel()
	}
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Service) Test(ctx context.Context, id string) (Result, error) {
	if s.closed.Load() {
		return Result{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	stop := context.AfterFunc(s.stopCtx, cancel)
	defer stop()
	c, secret, e := s.load(ctx, id)
	if e != nil {
		return Result{}, e
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return Result{}, e
	}
	defer tx.Rollback()
	allowed, _, e := s.reserveRate(ctx, tx, id)
	if e != nil {
		return Result{}, e
	}
	// Commit the audited attempt and rate reservation before the external effect.
	if e = audit.Insert(ctx, tx, "notification_channel.test", id, s.opts.Now()); e != nil {
		return Result{}, e
	}
	if e = tx.Commit(); e != nil {
		return Result{}, e
	}
	if !allowed {
		return failure("rate_limited", false), nil
	}
	p := Payload{EventID: "test", State: "test", StartedAt: s.opts.Now()}
	p.Rule.Name = "Test notification"
	p.Rule.Severity = "info"
	p.Node.Name = "OrbitBeacon"
	return s.sender.Send(ctx, c, secret, p), nil
}
func (s *Service) Cleanup(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, invalid("limit", "must be between 1 and 1000")
	}
	r, e := s.db.ExecContext(ctx, `DELETE FROM notification_deliveries WHERE id IN (SELECT id FROM notification_deliveries WHERE state IN ('sent','failed') AND updated_at<? ORDER BY updated_at,id LIMIT ?)`, timekey.Format(now.Add(-30*24*time.Hour)), limit)
	if e != nil {
		return 0, e
	}
	return r.RowsAffected()
}
