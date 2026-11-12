package services

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"orbitbeacon/internal/logging"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
	"orbitbeacon/internal/uuid"
)

type Notifier interface {
	EnqueueService(context.Context, *storage.Tx, Event) error
	Wake()
}
type Manager struct {
	db, history *storage.Database
	now         func() time.Time
	logger      *slog.Logger
	notifier    Notifier
	changed     func()
	reload      chan struct{}
	mu          sync.Mutex
	latest      map[string]Result
	events      []Event
	buckets     map[bucketKey]aggregate
	generation  uint64
}
type bucketKey struct {
	ID string
	T  int64
}
type aggregate struct {
	Checks, Successes int64
	Sum, Min, Max     float64
}

func New(db, history *storage.Database, now func() time.Time, logger *slog.Logger, notifier Notifier, changed func()) *Manager {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{db: db, history: history, now: now, logger: logger, notifier: notifier, changed: changed, reload: make(chan struct{}, 1), latest: map[string]Result{}, buckets: map[bucketKey]aggregate{}}
}
func (m *Manager) signal() {
	if m.changed != nil {
		m.changed()
	}
	select {
	case m.reload <- struct{}{}:
	default:
	}
}

const configColumns = "id,name,type,target,interval_seconds,timeout_seconds,enabled,public,display_order,notify,latency_threshold_ms,created_at,updated_at"

type scanner interface{ Scan(...any) error }

func scanService(row scanner) (Service, error) {
	var s Service
	var created, updated string
	e := row.Scan(&s.ID, &s.Name, &s.Type, &s.Target, &s.IntervalSeconds, &s.TimeoutSeconds, &s.Enabled, &s.Public, &s.DisplayOrder, &s.Notify, &s.LatencyThresholdMS, &created, &updated)
	if errors.Is(e, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	if e != nil {
		return s, e
	}
	s.CreatedAt, e = timekey.Parse(created)
	if e == nil {
		s.UpdatedAt, e = timekey.Parse(updated)
	}
	return s, e
}
func configArgs(s Service) []any {
	return []any{s.ID, s.Name, s.Type, s.Target, s.IntervalSeconds, s.TimeoutSeconds, s.Enabled, s.Public, s.DisplayOrder, s.Notify, s.LatencyThresholdMS, timekey.Format(s.CreatedAt), timekey.Format(s.UpdatedAt)}
}
func sameConfig(a, b Service) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Type == b.Type && a.Target == b.Target && a.IntervalSeconds == b.IntervalSeconds && a.TimeoutSeconds == b.TimeoutSeconds && a.Enabled == b.Enabled && a.Public == b.Public && a.DisplayOrder == b.DisplayOrder && a.Notify == b.Notify && a.LatencyThresholdMS == b.LatencyThresholdMS && a.UpdatedAt.Equal(b.UpdatedAt)
}
func (m *Manager) Save(ctx context.Context, id string, p Patch) (Service, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, e := m.db.Acquire(ctx, storage.Writer)
	if e != nil {
		return Service{}, e
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, e := lease.BeginWrite(ctx)
	if e != nil {
		return Service{}, e
	}
	defer tx.Rollback()
	s := Service{IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true}
	action := "service.create"
	var old Service
	if id != "" {
		s, e = scanService(tx.QueryRowContext(ctx, "SELECT "+configColumns+" FROM services WHERE id=?", id))
		if e != nil {
			return s, e
		}
		old = s
		action = "service.update"
	} else {
		var count int
		if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM services").Scan(&count); e != nil {
			return s, e
		}
		if count >= 100 {
			return s, ErrCapacity
		}
		s.ID, e = uuid.UUID(rand.Reader)
		if e != nil {
			return s, e
		}
		s.CreatedAt = m.now().UTC()
	}
	p.apply(&s)
	if e = Validate(s); e != nil {
		return Service{}, e
	}
	s.UpdatedAt = m.now().UTC()
	if id == "" {
		_, e = tx.ExecContext(ctx, "INSERT INTO services("+configColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", configArgs(s)...)
	} else {
		args := configArgs(s)
		_, e = tx.ExecContext(ctx, "UPDATE services SET name=?,type=?,target=?,interval_seconds=?,timeout_seconds=?,enabled=?,public=?,display_order=?,notify=?,latency_threshold_ms=?,created_at=?,updated_at=? WHERE id=?", append(args[1:], s.ID)...)
	}
	if e != nil {
		return s, e
	}
	// A new endpoint or hysteresis policy starts unknown. Old in-flight results are fenced by config.
	reset := id != "" && (old.Target != s.Target || old.Type != s.Type || old.Enabled != s.Enabled || old.LatencyThresholdMS != s.LatencyThresholdMS)
	var resetResult *Result
	if reset {
		// Configuration edits reset hysteresis, not the once-per-day certificate budget.
		var raw string
		previous, pending := m.latest[id]
		pending = pending && m.generation == lease.Generation()
		if !pending {
			previous = Result{}
			e = tx.QueryRowContext(ctx, "SELECT result_json FROM service_checks_latest WHERE service_id=?", id).Scan(&raw)
			if e == nil {
				e = json.Unmarshal([]byte(raw), &previous)
			}
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return s, e
			}
		}
		if previous.CertificateNotifiedDay != "" {
			resetResult = &Result{Status: "unknown", CheckedAt: previous.CheckedAt, CertificateNotifiedDay: previous.CertificateNotifiedDay}
			encoded, err := json.Marshal(resetResult)
			if err != nil {
				return s, err
			}
			update := " ON CONFLICT(service_id) DO UPDATE SET result_json=excluded.result_json"
			if tx.Dialect() == storage.MySQL {
				update = " ON DUPLICATE KEY UPDATE result_json=VALUES(result_json)"
			}
			_, e = tx.ExecContext(ctx, "INSERT INTO service_checks_latest(service_id,result_json) VALUES(?,?)"+update, id, string(encoded))
		} else {
			_, e = tx.ExecContext(ctx, "DELETE FROM service_checks_latest WHERE service_id=?", id)
		}
		if e != nil {
			return s, e
		}
	}
	if e = audit.InsertDetails(ctx, tx, action, s.ID, s.UpdatedAt, audit.Details{"name": s.Name, "type": s.Type}); e != nil {
		return s, e
	}
	if e = tx.Commit(); e != nil {
		return Service{}, e
	}
	if reset {
		delete(m.latest, id)
		if resetResult != nil {
			m.latest[id] = *resetResult
		}
	}
	m.signal()
	return s, nil
}
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, e := m.db.Acquire(ctx, storage.Writer)
	if e != nil {
		return e
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, e := lease.BeginWrite(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	s, e := scanService(tx.QueryRowContext(ctx, "SELECT "+configColumns+" FROM services WHERE id=?", id))
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM services WHERE id=?", id); e != nil {
		return e
	}
	if e = audit.InsertDetails(ctx, tx, "service.delete", id, m.now(), audit.Details{"name": s.Name}); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	delete(m.latest, id)
	m.signal()
	return nil
}
func (m *Manager) List(ctx context.Context) ([]Service, error) { return m.list(ctx, m.now()) }
func (m *Manager) list(ctx context.Context, now time.Time) ([]Service, error) {
	lease, e := m.db.Acquire(ctx, storage.Reader)
	if e != nil {
		return nil, e
	}
	defer lease.Close()
	ctx = lease.Context()
	// Separate query avoids ambiguous config columns and remains bounded at 100 services.
	rows, e := lease.QueryContext(ctx, "SELECT "+configColumns+" FROM services ORDER BY display_order DESC,name,id")
	if e != nil {
		return nil, e
	}
	out := make([]Service, 0)
	for rows.Next() {
		s, e := scanService(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		out = append(out, s)
	}
	if e = errors.Join(rows.Err(), rows.Close()); e != nil {
		return nil, e
	}
	latest, e := lease.QueryContext(ctx, "SELECT service_id,result_json FROM service_checks_latest")
	if e != nil {
		return nil, e
	}
	byID := map[string]Result{}
	for latest.Next() {
		var id, raw string
		var result Result
		if e = latest.Scan(&id, &raw); e == nil {
			e = json.Unmarshal([]byte(raw), &result)
		}
		if e != nil {
			latest.Close()
			return nil, e
		}
		byID[id] = result
	}
	if e = errors.Join(latest.Err(), latest.Close()); e != nil {
		return nil, e
	}
	m.mu.Lock()
	if m.generation == lease.Generation() {
		for id, r := range m.latest {
			byID[id] = r
		}
	}
	m.mu.Unlock()
	totals, e := m.totals(ctx, now.Add(-24*time.Hour).Unix()/300*300, 300)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		m.logError(ctx, "service_history_read_failed", e)
	}
	for i := range out {
		if r, ok := byID[out[i].ID]; ok {
			copy := r
			out[i].Latest = &copy
		}
		if a := totals[bucketKey{out[i].ID, 0}]; a.Checks > 0 {
			out[i].Availability24H = percent(a.Successes, a.Checks)
		}
	}
	return out, nil
}
func (m *Manager) Get(ctx context.Context, id string) (Service, error) {
	list, e := m.List(ctx)
	if e != nil {
		return Service{}, e
	}
	for _, s := range list {
		if s.ID == id {
			return s, nil
		}
	}
	return Service{}, ErrNotFound
}

// Accept is serialized with config mutations; stale/deleted/disabled work cannot publish.
func (m *Manager) Accept(ctx context.Context, config Service, result Result) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, e := m.db.Acquire(ctx, storage.Reader)
	if e != nil {
		return e
	}
	defer lease.Close()
	ctx = lease.Context()
	s, e := scanService(lease.QueryRowContext(ctx, "SELECT "+configColumns+" FROM services WHERE id=?", config.ID))
	if errors.Is(e, ErrNotFound) {
		return nil
	}
	if e != nil {
		return e
	}
	if !s.Enabled || !sameConfig(s, config) || config.generation != 0 && config.generation != lease.Generation() {
		return nil
	}
	if m.generation != lease.Generation() {
		m.latest = map[string]Result{}
		m.events = nil
		m.buckets = map[bucketKey]aggregate{}
		m.generation = lease.Generation()
	}
	var previous *Result
	if r, ok := m.latest[s.ID]; ok {
		previous = &r
	} else {
		var raw string
		e = lease.QueryRowContext(ctx, "SELECT result_json FROM service_checks_latest WHERE service_id=?", s.ID).Scan(&raw)
		if e == nil {
			previous = &Result{}
			e = json.Unmarshal([]byte(raw), previous)
		}
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
	}
	result, events := Advance(s, previous, result)
	for i := range events {
		events[i].ID, e = uuid.UUID(rand.Reader)
		if e != nil {
			return e
		}
	}
	m.latest[s.ID] = result
	m.events = append(m.events, events...)
	k := bucketKey{s.ID, result.CheckedAt.UTC().Unix() / 300 * 300}
	a := m.buckets[k]
	a.Checks++
	if result.OK {
		a.Successes++
		if result.LatencyMS != nil {
			v := *result.LatencyMS
			a.Sum += v
			if a.Successes == 1 || v < a.Min {
				a.Min = v
			}
			a.Max = max(a.Max, v)
		}
	}
	m.buckets[k] = a
	return nil
}
func (m *Manager) Flush(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.latest) > 0 || len(m.events) > 0 {
		lease, e := m.db.Acquire(ctx, storage.Writer)
		if e != nil {
			return e
		}
		if m.generation != lease.Generation() {
			lease.Close()
			m.latest = map[string]Result{}
			m.events = nil
			m.buckets = map[bucketKey]aggregate{}
			return nil
		}
		e = m.flushMain(lease.Context(), lease)
		lease.Close()
		if e != nil {
			return e
		}
		m.latest = map[string]Result{}
		m.events = nil
		if m.notifier != nil {
			m.notifier.Wake()
		}
		if m.changed != nil {
			m.changed()
		}
	}
	if m.history == nil {
		m.buckets = map[bucketKey]aggregate{}
		return nil
	}
	if len(m.buckets) == 0 {
		return nil
	}
	// History is best-effort and isolated from durable status/inbox/outbox writes.
	// Drop only this history batch on failure rather than grow memory or stop checks.
	defer func() { m.buckets = map[bucketKey]aggregate{} }()
	if e := m.flushHistory(ctx); e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m.logError(ctx, "service_history_flush_failed", e)
	}
	return nil
}
func (m *Manager) flushHistory(ctx context.Context) error {
	lease, e := m.history.Acquire(ctx, storage.Writer)
	if e != nil {
		return e
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, e := lease.BeginWrite(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	update := ` ON CONFLICT(service_id,t) DO UPDATE SET checks=service_check_buckets.checks+excluded.checks, successes=service_check_buckets.successes+excluded.successes,latency_sum_ms=service_check_buckets.latency_sum_ms+excluded.latency_sum_ms,latency_min_ms=CASE WHEN service_check_buckets.successes=0 THEN excluded.latency_min_ms WHEN excluded.successes=0 THEN service_check_buckets.latency_min_ms ELSE MIN(service_check_buckets.latency_min_ms,excluded.latency_min_ms) END,latency_max_ms=MAX(service_check_buckets.latency_max_ms,excluded.latency_max_ms)`
	if tx.Dialect() == storage.MySQL {
		update = ` ON DUPLICATE KEY UPDATE latency_min_ms=CASE WHEN service_check_buckets.successes=0 THEN VALUES(latency_min_ms) WHEN VALUES(successes)=0 THEN service_check_buckets.latency_min_ms ELSE LEAST(service_check_buckets.latency_min_ms,VALUES(latency_min_ms)) END, checks=service_check_buckets.checks+VALUES(checks),successes=service_check_buckets.successes+VALUES(successes),latency_sum_ms=service_check_buckets.latency_sum_ms+VALUES(latency_sum_ms),latency_max_ms=GREATEST(service_check_buckets.latency_max_ms,VALUES(latency_max_ms))`
	}
	for k, a := range m.buckets {
		if _, e = tx.ExecContext(ctx, `INSERT INTO service_check_buckets(service_id,t,checks,successes,latency_sum_ms,latency_min_ms,latency_max_ms) VALUES(?,?,?,?,?,?,?)`+update, k.ID, k.T, a.Checks, a.Successes, a.Sum, a.Min, a.Max); e != nil {
			return e
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	m.buckets = map[bucketKey]aggregate{}
	return nil
}
func (m *Manager) flushMain(ctx context.Context, lease *storage.Lease) error {
	tx, e := lease.BeginWrite(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	update := ` ON CONFLICT(service_id) DO UPDATE SET result_json=excluded.result_json`
	if tx.Dialect() == storage.MySQL {
		update = ` ON DUPLICATE KEY UPDATE result_json=VALUES(result_json)`
	}
	for id, result := range m.latest {
		raw, e := json.Marshal(result)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO service_checks_latest(service_id,result_json) SELECT id,? FROM services WHERE id=?`+update, string(raw), id); e != nil {
			return e
		}
	}
	for _, event := range m.events {
		raw, e := json.Marshal(event)
		if e != nil {
			return e
		}
		severity := "warning"
		if event.Transition == "down" {
			severity = "critical"
		}
		if event.Transition == "recovered" || event.Transition == "latency_recovered" {
			severity = "info"
		}
		if e = inbox.Insert(ctx, tx, inbox.Event{Kind: "service", SourceID: event.ID, Transition: event.Transition, Severity: severity, CreatedAt: event.OccurredAt, Details: raw}); e != nil {
			return e
		}
		if m.notifier != nil {
			if e = m.notifier.EnqueueService(ctx, tx, event); e != nil {
				return e
			}
		}
	}
	return tx.Commit()
}
func (m *Manager) logError(ctx context.Context, code string, e error) {
	if e != nil && ctx.Err() == nil {
		m.logger.LogAttrs(ctx, slog.LevelWarn, code, logging.ErrorAttrs(e, code)...)
	}
}
func percent(successes, checks int64) *float64 {
	if checks == 0 {
		return nil
	}
	v := math.Round(float64(successes)*10000/float64(checks)) / 100
	if successes < checks {
		v = min(v, 99.99)
	}
	return &v
}

type Point struct {
	Time         time.Time `json:"time"`
	Availability *float64  `json:"availability"`
	AvgLatencyMS *float64  `json:"avg_latency_ms"`
	MaxLatencyMS *float64  `json:"max_latency_ms"`
}
type History struct {
	Range       string  `json:"range"`
	StepSeconds int     `json:"step_seconds"`
	Buckets     []Point `json:"buckets"`
}

func (m *Manager) totals(ctx context.Context, start int64, step int64) (map[bucketKey]aggregate, error) {
	// Keep the committed rows and pending overlay in one view of Flush.
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[bucketKey]aggregate{}
	if m.history == nil {
		return out, nil
	}
	lease, e := m.history.Acquire(ctx, storage.Reader)
	if e != nil {
		return nil, e
	}
	defer lease.Close()
	ctx = lease.Context()
	// Aggregate in SQL: public reads return at most 30 rows per service, not raw buckets.
	group := "0"
	args := []any{}
	if step != 300 {
		group = "(t / ?)*?"
		if lease.Dialect() == storage.MySQL {
			group = "FLOOR(t / ?)*?"
		}
		args = append(args, step, step)
	}
	args = append(args, start)
	rows, e := lease.QueryContext(ctx, `SELECT service_id,`+group+` AS bucket,SUM(checks),SUM(successes),SUM(latency_sum_ms),MAX(latency_max_ms) FROM service_check_buckets WHERE t>=? GROUP BY service_id,bucket`, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	add := func(id string, t int64, a aggregate) {
		if step == 300 {
			t = 0
		} else {
			t = t / step * step
		}
		k := bucketKey{id, t}
		b := out[k]
		b.Checks += a.Checks
		b.Successes += a.Successes
		b.Sum += a.Sum
		b.Max = max(b.Max, a.Max)
		out[k] = b
	}
	for rows.Next() {
		var id string
		var t int64
		var a aggregate
		if e = rows.Scan(&id, &t, &a.Checks, &a.Successes, &a.Sum, &a.Max); e != nil {
			return nil, e
		}
		add(id, t, a)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	for k, a := range m.buckets {
		if k.T >= start {
			add(k.ID, k.T, a)
		}
	}
	return out, nil
}
func (m *Manager) History(ctx context.Context, id, r string) (History, error) {
	h := History{Range: r, StepSeconds: 300, Buckets: []Point{}}
	duration := time.Hour
	switch r {
	case "1h":
	case "24h":
		duration = 24 * time.Hour
	case "7d":
		duration = 7 * 24 * time.Hour
		h.StepSeconds = 3600
	case "30d":
		duration = 30 * 24 * time.Hour
		h.StepSeconds = 86400
	default:
		return h, validationRange()
	}
	if _, e := m.Get(ctx, id); e != nil {
		return h, e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	start := now.Add(-duration).Unix() / int64(h.StepSeconds) * int64(h.StepSeconds)
	if m.history == nil {
		return h, nil
	}
	lease, e := m.history.Acquire(ctx, storage.Reader)
	if e != nil {
		return h, e
	}
	defer lease.Close()
	rows, e := lease.QueryContext(lease.Context(), `SELECT t,checks,successes,latency_sum_ms,latency_max_ms FROM service_check_buckets WHERE service_id=? AND t>=? ORDER BY t`, id, start)
	if e != nil {
		return h, e
	}
	values := map[int64]aggregate{}
	add := func(t int64, a aggregate) {
		t = t / int64(h.StepSeconds) * int64(h.StepSeconds)
		b := values[t]
		b.Checks += a.Checks
		b.Successes += a.Successes
		b.Sum += a.Sum
		b.Max = max(b.Max, a.Max)
		values[t] = b
	}
	for rows.Next() {
		var t int64
		var a aggregate
		if e = rows.Scan(&t, &a.Checks, &a.Successes, &a.Sum, &a.Max); e != nil {
			rows.Close()
			return h, e
		}
		add(t, a)
	}
	if e = errors.Join(rows.Err(), rows.Close()); e != nil {
		return h, e
	}
	for k, a := range m.buckets {
		if k.ID == id && k.T >= start {
			add(k.T, a)
		}
	}
	for t := start; t <= now.Unix(); t += int64(h.StepSeconds) {
		a := values[t]
		p := Point{Time: time.Unix(t, 0).UTC(), Availability: percent(a.Successes, a.Checks)}
		if a.Successes > 0 {
			avg := a.Sum / float64(a.Successes)
			max := a.Max
			p.AvgLatencyMS = &avg
			p.MaxLatencyMS = &max
		}
		h.Buckets = append(h.Buckets, p)
	}
	return h, nil
}
func (m *Manager) Cleanup(ctx context.Context, now time.Time, limit int) (int64, error) {
	if m.history == nil {
		return 0, nil
	}
	lease, e := m.history.Acquire(ctx, storage.Writer)
	if e != nil {
		return 0, e
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, e := lease.BeginWrite(ctx)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	n, e := storage.DeleteBatch(ctx, tx, `SELECT service_id,t FROM service_check_buckets WHERE t<? ORDER BY t,service_id LIMIT ?`, `DELETE FROM service_check_buckets WHERE service_id=? AND t=?`, now.UTC().Add(-30*24*time.Hour).Unix(), limit)
	if e != nil {
		return 0, e
	}
	if e = tx.Commit(); e != nil {
		return 0, e
	}
	return n, nil
}

func validationRange() error { return validation.Invalid("range", "must be one of 1h, 24h, 7d, 30d") }
