// Package availability 只统计服务器运行期间观测到的时长。
package availability

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"time"

	"orbitbeacon/internal/logging"
	"orbitbeacon/internal/server/commands"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

type Service struct {
	db                *storage.Database
	now               func() time.Time
	logger            *slog.Logger
	started, previous time.Time
}

func NewService(db *storage.Database, clock func() time.Time, logger *slog.Logger) *Service {
	if clock == nil {
		clock = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{db: db, now: clock, logger: logger}
}

// Tick 只能串行调用。基线只存内存，停机时长才会成为未知。
func (s *Service) Tick(ctx context.Context) error {
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	now := s.now().UTC()
	previous := s.previous
	s.previous = now
	if previous.IsZero() {
		s.started = now
		return nil
	}
	if !now.After(previous) {
		return nil
	}
	seconds := int64(min(now.Sub(previous), 90*time.Second) / time.Second)
	if seconds == 0 {
		return nil
	}
	tx, txErr := lease.BeginWrite(ctx)
	if txErr != nil {
		return txErr
	}
	defer tx.Rollback()
	_, err := tx.ExecContext(ctx, `INSERT INTO node_availability_daily(node_id,day,online_seconds,offline_seconds)
 SELECT id,?,CASE WHEN last_seen>=? THEN ? ELSE 0 END,CASE WHEN last_seen<? THEN ? ELSE 0 END
 FROM nodes WHERE status!='disabled' AND length(token_hash)>0 AND last_seen IS NOT NULL
 AND NOT `+commands.StoppedSQL+` AND (last_seen>=? OR ?)
 ON CONFLICT(node_id,day) DO UPDATE SET
 online_seconds=MIN(86400-node_availability_daily.offline_seconds,node_availability_daily.online_seconds+excluded.online_seconds),
 offline_seconds=MIN(86400-node_availability_daily.online_seconds,node_availability_daily.offline_seconds+excluded.offline_seconds)`,
		now.Format(time.DateOnly), timekey.Format(now.Add(-nodes.OnlineWindow)), seconds, timekey.Format(now.Add(-nodes.OnlineWindow)), seconds, timekey.Format(now.Add(-nodes.OnlineWindow)), now.Sub(s.started) >= nodes.OnlineWindow)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	// 第一次调用只设基线，不写库。
	_ = s.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
				s.logger.LogAttrs(ctx, slog.LevelWarn, "availability_tick_failed", logging.ErrorAttrs(err, "tick_failed")...)
			}
		}
	}
}
func (s *Service) Cleanup(ctx context.Context, now time.Time, limit int) (int64, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return 0, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, txErr := lease.BeginWrite(ctx)
	if txErr != nil {
		return 0, txErr
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM node_availability_daily WHERE (node_id,day) IN
 (SELECT node_id,day FROM node_availability_daily WHERE day<? ORDER BY day,node_id LIMIT ?)`, now.UTC().AddDate(0, 0, -89).Format(time.DateOnly), limit)
	if err != nil {
		return 0, err
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return 0, commitErr
	}
	return result.RowsAffected()
}

type Daily struct {
	Day     string   `json:"day"`
	Online  int64    `json:"online_seconds"`
	Offline int64    `json:"offline_seconds"`
	Unknown int64    `json:"unknown_seconds"`
	Percent *float64 `json:"percent"`
}
type Summary struct {
	Days    []Daily  `json:"days"`
	Percent *float64 `json:"percent"`
}
type Public struct {
	StartDay string     `json:"start_day"`
	Percent  *float64   `json:"percent"`
	Days     []*float64 `json:"days"`
}

func Percent(online, offline int64) *float64 {
	if online+offline == 0 {
		return nil
	}
	value := math.Round(10000*float64(online)/float64(online+offline)) / 100
	if offline > 0 {
		value = min(value, 99.99)
	}
	return &value
}
func Fill(rows []Daily, days int, now time.Time) Summary {
	now = now.UTC()
	today := now.Truncate(24 * time.Hour)
	byDay := map[string]Daily{}
	for _, d := range rows {
		byDay[d.Day] = d
	}
	out := Summary{Days: make([]Daily, 0, days)}
	var online, offline int64
	for i := days - 1; i >= 0; i-- {
		day := today.AddDate(0, 0, -i).Format(time.DateOnly)
		d := byDay[day]
		d.Day = day
		elapsed := int64(86400)
		if i == 0 {
			elapsed = int64(now.Sub(today) / time.Second)
		}
		d.Unknown = max(0, elapsed-d.Online-d.Offline)
		d.Percent = Percent(d.Online, d.Offline)
		out.Days = append(out.Days, d)
		online += d.Online
		offline += d.Offline
	}
	out.Percent = Percent(online, offline)
	return out
}
func Project(raw string, now time.Time) (Public, error) {
	var rows []Daily
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &rows); err != nil {
			return Public{}, err
		}
	}
	summary := Fill(rows, 30, now)
	out := Public{StartDay: summary.Days[0].Day, Percent: summary.Percent, Days: make([]*float64, 30)}
	for i, d := range summary.Days {
		out.Days[i] = d.Percent
	}
	return out, nil
}
func (s *Service) Read(ctx context.Context, id string, days int, now time.Time) (Summary, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return Summary{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if days != 30 && days != 90 {
		return Summary{}, errors.New("invalid availability days")
	}
	now = now.UTC()
	start := now.AddDate(0, 0, -days+1).Format(time.DateOnly)
	rows, err := lease.QueryContext(ctx, `SELECT a.day,a.online_seconds,a.offline_seconds FROM nodes n LEFT JOIN node_availability_daily a ON a.node_id=n.id AND a.day>=? AND a.day<=? WHERE n.id=? ORDER BY a.day`, start, now.Format(time.DateOnly), id)
	if err != nil {
		return Summary{}, err
	}
	defer rows.Close()
	found := false
	var values []Daily
	for rows.Next() {
		found = true
		var day sql.NullString
		var online, offline sql.NullInt64
		if err := rows.Scan(&day, &online, &offline); err != nil {
			return Summary{}, err
		}
		if day.Valid {
			values = append(values, Daily{Day: day.String, Online: online.Int64, Offline: offline.Int64})
		}
	}
	if err := rows.Err(); err != nil {
		return Summary{}, err
	}
	if !found {
		return Summary{}, nodes.ErrNotFound
	}
	return Fill(values, days, now), nil
}
