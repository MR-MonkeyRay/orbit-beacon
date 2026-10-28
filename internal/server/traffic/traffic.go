// Package traffic reads retained traffic counters and rolls expired hourly
// counters into daily totals.
package traffic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"orbitbeacon/internal/timekey"
)

const (
	hourlyRetention = 35 * 24 * time.Hour
	dailyRetention  = 400 * 24 * time.Hour
	dayLayout       = "2006-01-02"
)

var (
	ErrNotFound     = errors.New("traffic node not found")
	ErrInvalidInput = errors.New("invalid traffic request")
)

type Service struct{ db *sql.DB }

func NewService(db *sql.DB) *Service { return &Service{db: db} }

type Summary struct {
	BillingDay *int       `json:"billing_day"`
	QuotaBytes *string    `json:"quota_bytes"`
	Cycle      Cycle      `json:"cycle"`
	Daily      []Daily    `json:"daily"`
	UpdatedAt  *time.Time `json:"updated_at"`
}

type Cycle struct {
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	RxBytes    string    `json:"rx_bytes"`
	TxBytes    string    `json:"tx_bytes"`
	TotalBytes string    `json:"total_bytes"`
}

type Daily struct {
	Day     string `json:"day"`
	RxBytes string `json:"rx_bytes"`
	TxBytes string `json:"tx_bytes"`
}

type counters struct {
	rx int64
	tx int64
}

func (c *counters) add(rx, tx int64) {
	c.rx = saturatingAdd(c.rx, rx)
	c.tx = saturatingAdd(c.tx, tx)
}

func saturatingAdd(left, right int64) int64 {
	if right > math.MaxInt64-left {
		return math.MaxInt64
	}
	return left + right
}

// CycleBounds returns the current UTC billing cycle. Invalid or absent billing
// days use the server default of the first day of the month.
func CycleBounds(now time.Time, billingDay int) (time.Time, time.Time) {
	now = now.UTC()
	if billingDay < 1 || billingDay > 28 {
		billingDay = 1
	}
	start := time.Date(now.Year(), now.Month(), billingDay, 0, 0, 0, 0, time.UTC)
	if now.Before(start) {
		start = time.Date(now.Year(), now.Month()-1, billingDay, 0, 0, 0, 0, time.UTC)
	}
	return start, start.AddDate(0, 1, 0)
}

// Read merges hourly and rolled-up daily counters. It sums in Go with
// saturation so SQLite's signed 64-bit aggregate limit cannot fail reads.
func (s *Service) Read(ctx context.Context, nodeID string, days int, now time.Time) (Summary, error) {
	if s == nil || s.db == nil || nodeID == "" || days < 1 || days > 400 {
		return Summary{}, ErrInvalidInput
	}
	now = now.UTC()
	var billing sql.NullInt64
	var quota sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT billing_day,quota_bytes FROM nodes WHERE id=?`, nodeID).Scan(&billing, &quota); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Summary{}, ErrNotFound
		}
		return Summary{}, fmt.Errorf("read traffic node: %w", err)
	}

	billingValue := 1
	var billingOutput *int
	if billing.Valid && billing.Int64 != 0 {
		billingValue = int(billing.Int64)
		value := billingValue
		billingOutput = &value
	}
	var quotaOutput *string
	// A stored quota of 0 means unlimited, which the API reports as null.
	if quota.Valid && quota.String != "" && quota.String != "0" {
		value := quota.String
		quotaOutput = &value
	}
	cycleStart, cycleEnd := CycleBounds(now, billingValue)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dailyStart := today.AddDate(0, 0, -(days - 1))
	dailyEnd := today.AddDate(0, 0, 1)
	queryStart := dailyStart
	if cycleStart.Before(queryStart) {
		queryStart = cycleStart
	}
	queryEnd := dailyEnd
	if cycleEnd.After(queryEnd) {
		queryEnd = cycleEnd
	}

	byDay := make(map[string]*counters)
	add := func(day string, rx, tx int64) {
		value := byDay[day]
		if value == nil {
			value = &counters{}
			byDay[day] = value
		}
		value.add(rx, tx)
	}
	// UNION ALL runs under one SQLite read snapshot. A concurrent retention
	// transaction therefore cannot make a row disappear between separate daily
	// and hourly queries or make the same counters visible in both forms.
	rows, err := s.db.QueryContext(ctx, `
WITH counters(day,rx_bytes,tx_bytes) AS (
 SELECT day,rx_bytes,tx_bytes FROM traffic_daily WHERE node_id=? AND day>=? AND day<?
 UNION ALL
 SELECT substr(period_start,1,10),rx_bytes,tx_bytes FROM traffic_hourly WHERE node_id=? AND period_start>=? AND period_start<?
), latest(updated_at) AS (SELECT max(updated_at) FROM traffic_hourly WHERE node_id=?)
SELECT counters.day,counters.rx_bytes,counters.tx_bytes,latest.updated_at
FROM latest LEFT JOIN counters ON true ORDER BY counters.day`, nodeID, queryStart.Format(dayLayout), queryEnd.Format(dayLayout), nodeID, timekey.Format(queryStart), timekey.Format(queryEnd), nodeID)
	if err != nil {
		return Summary{}, fmt.Errorf("read traffic counters: %w", err)
	}
	var updated sql.NullString
	for rows.Next() {
		var day sql.NullString
		var rx, tx sql.NullInt64
		if err := rows.Scan(&day, &rx, &tx, &updated); err != nil {
			_ = rows.Close()
			return Summary{}, fmt.Errorf("scan traffic counters: %w", err)
		}
		if day.Valid {
			add(day.String, rx.Int64, tx.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return Summary{}, fmt.Errorf("read traffic counters: %w", err)
	}
	if err := rows.Close(); err != nil {
		return Summary{}, fmt.Errorf("close traffic counters: %w", err)
	}

	var updatedAt *time.Time
	if updated.Valid {
		stamp, err := timekey.Parse(updated.String)
		if err != nil {
			return Summary{}, fmt.Errorf("parse traffic update time: %w", err)
		}
		updatedAt = &stamp
	}

	result := Summary{BillingDay: billingOutput, QuotaBytes: quotaOutput, Daily: make([]Daily, 0), UpdatedAt: updatedAt}
	result.Cycle.Start, result.Cycle.End = cycleStart, cycleEnd
	var cycleRx, cycleTx int64
	cycleStartDay, cycleEndDay := cycleStart.Format(dayLayout), cycleEnd.Format(dayLayout)
	for day := queryStart; day.Before(queryEnd); day = day.AddDate(0, 0, 1) {
		key := day.Format(dayLayout)
		value := byDay[key]
		if value == nil {
			continue
		}
		if key >= cycleStartDay && key < cycleEndDay {
			cycleRx = saturatingAdd(cycleRx, value.rx)
			cycleTx = saturatingAdd(cycleTx, value.tx)
		}
		if !day.Before(dailyStart) && day.Before(dailyEnd) {
			result.Daily = append(result.Daily, Daily{Day: key, RxBytes: fmt.Sprint(value.rx), TxBytes: fmt.Sprint(value.tx)})
		}
	}
	cycleTotal := saturatingAdd(cycleRx, cycleTx)
	result.Cycle.RxBytes = fmt.Sprint(cycleRx)
	result.Cycle.TxBytes = fmt.Sprint(cycleTx)
	result.Cycle.TotalBytes = fmt.Sprint(cycleTotal)
	return result, nil
}

type hourlyRow struct {
	nodeID string
	epoch  string
	period string
	rx     int64
	tx     int64
}

type dailyKey struct {
	nodeID string
	day    string
}

// Cleanup rolls at most limit expired hourly rows into daily counters and uses
// any remaining batch capacity to delete expired daily rows. Each rollup and
// its source deletion commit in one transaction, so retries cannot double count.
func (s *Service) Cleanup(ctx context.Context, now time.Time, limit int) (int64, error) {
	if s == nil || s.db == nil || limit <= 0 {
		return 0, ErrInvalidInput
	}
	now = now.UTC()
	cutoff := now.Add(-hourlyRetention)
	hourlyBefore := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)
	dailyCutoff := now.Add(-dailyRetention).Format(dayLayout)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin traffic cleanup: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT node_id,epoch,period_start,rx_bytes,tx_bytes FROM traffic_hourly WHERE period_start<? ORDER BY period_start,node_id,epoch LIMIT ?`, timekey.Format(hourlyBefore), limit)
	if err != nil {
		return 0, fmt.Errorf("select hourly traffic cleanup: %w", err)
	}
	hourly := make([]hourlyRow, 0, limit)
	for rows.Next() {
		var row hourlyRow
		if err := rows.Scan(&row.nodeID, &row.epoch, &row.period, &row.rx, &row.tx); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan hourly traffic cleanup: %w", err)
		}
		hourly = append(hourly, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("select hourly traffic cleanup: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close hourly traffic cleanup: %w", err)
	}

	rollups := make(map[dailyKey]*counters)
	for _, row := range hourly {
		stamp, err := timekey.Parse(row.period)
		if err != nil {
			return 0, fmt.Errorf("parse hourly cleanup period: %w", err)
		}
		key := dailyKey{nodeID: row.nodeID, day: stamp.Format(dayLayout)}
		value := rollups[key]
		if value == nil {
			value = &counters{}
			rollups[key] = value
		}
		value.add(row.rx, row.tx)
	}
	for key, value := range rollups {
		var existingRx, existingTx int64
		err := tx.QueryRowContext(ctx, `SELECT rx_bytes,tx_bytes FROM traffic_daily WHERE node_id=? AND day=?`, key.nodeID, key.day).Scan(&existingRx, &existingTx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("read existing daily traffic: %w", err)
		}
		combinedRx := saturatingAdd(value.rx, existingRx)
		combinedTx := saturatingAdd(value.tx, existingTx)
		if _, err := tx.ExecContext(ctx, `INSERT INTO traffic_daily(node_id,day,rx_bytes,tx_bytes) VALUES(?,?,?,?) ON CONFLICT(node_id,day) DO UPDATE SET rx_bytes=excluded.rx_bytes,tx_bytes=excluded.tx_bytes`, key.nodeID, key.day, combinedRx, combinedTx); err != nil {
			return 0, fmt.Errorf("roll up daily traffic: %w", err)
		}
	}
	for _, row := range hourly {
		if _, err := tx.ExecContext(ctx, `DELETE FROM traffic_hourly WHERE node_id=? AND epoch=? AND period_start=?`, row.nodeID, row.epoch, row.period); err != nil {
			return 0, fmt.Errorf("delete rolled hourly traffic: %w", err)
		}
	}

	removed := int64(len(hourly))
	remaining := limit - len(hourly)
	if remaining > 0 {
		result, err := tx.ExecContext(ctx, `DELETE FROM traffic_daily WHERE (node_id,day) IN (SELECT node_id,day FROM traffic_daily WHERE day<? ORDER BY day,node_id LIMIT ?)`, dailyCutoff, remaining)
		if err != nil {
			return 0, fmt.Errorf("delete expired daily traffic: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count expired daily traffic: %w", err)
		}
		removed += deleted
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit traffic cleanup: %w", err)
	}
	return removed, nil
}
