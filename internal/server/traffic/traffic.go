// Package traffic 读取保留的流量计数，并把过期的小时计数汇总为按天合计。
package traffic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"orbitbeacon/internal/server/storage"
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

type Service struct{ db *storage.Database }

func NewService(db *storage.Database) *Service { return &Service{db: db} }

type Summary struct {
	BillingMode string     `json:"billing_mode"`
	BillingDay  *int       `json:"billing_day"`
	QuotaBytes  *string    `json:"quota_bytes"`
	Cycle       Cycle      `json:"cycle"`
	Daily       []Daily    `json:"daily"`
	UpdatedAt   *time.Time `json:"updated_at"`
}

type Cycle struct {
	UsedBytes  string    `json:"used_bytes"`
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

// CycleBounds 返回当前 UTC 计费周期。无效或缺失的计费日使用服务器默认值，
// 即每月第一天。
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

// Read 合并小时计数和汇总后的按天计数。它在 Go 中带饱和地求和，
// 这样 SQLite 有符号 64 位聚合的限制不会导致读取失败。
func (s *Service) Read(ctx context.Context, nodeID string, days int, now time.Time) (Summary, error) {
	if s == nil || s.db == nil || nodeID == "" || days < 1 || days > 400 {
		return Summary{}, ErrInvalidInput
	}
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return Summary{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	now = now.UTC()
	var billing sql.NullInt64
	var quota sql.NullString
	var mode string
	if err := lease.QueryRowContext(ctx, `SELECT billing_day,quota_bytes,billing_mode FROM nodes WHERE id=?`, nodeID).Scan(&billing, &quota, &mode); err != nil {
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
	// 存储的配额为 0 表示不限量，API 将其报告为 null。
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
	// UNION ALL 在同一个 SQLite 读取快照下运行。因此并发的保留事务
	// 不会让某行在独立的按天和按小时查询之间消失，
	// 也不会让同一计数同时以两种形式可见。
	rows, err := lease.QueryContext(ctx, `
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

	result := Summary{BillingMode: mode, BillingDay: billingOutput, QuotaBytes: quotaOutput, Daily: make([]Daily, 0), UpdatedAt: updatedAt}
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
	result.Cycle.UsedBytes = fmt.Sprint(usedBytes(mode, cycleRx, cycleTx))
	return result, nil
}

// usedBytes 按周期收发总量计算计费流量，保持 int64 饱和语义。
func usedBytes(mode string, rx, tx int64) int64 {
	switch mode {
	case "max":
		return max(rx, tx)
	case "rx":
		return rx
	case "tx":
		return tx
	default:
		return saturatingAdd(rx, tx)
	}
}

// Quota 一次读取所有有配额节点及两级计数；账期在 Go 中逐节点计算。
func (s *Service) Quota(ctx context.Context, now time.Time) (map[string]float64, error) {
	if s == nil || s.db == nil {
		return nil, ErrInvalidInput
	}
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	start, end := CycleBounds(now, 1)
	for day := 2; day <= 28; day++ {
		a, b := CycleBounds(now, day)
		if a.Before(start) {
			start = a
		}
		if b.After(end) {
			end = b
		}
	}
	// 节点设置和 UNION ALL 共用读取快照，保留事务不会造成漏计或重复。
	// 按天浮点聚合只用于百分比，避免整数 SUM 溢出。
	rows, err := lease.QueryContext(ctx, `
WITH eligible AS (SELECT id,billing_day,billing_mode,quota_bytes FROM nodes WHERE quota_bytes NOT IN ('0','')),
 counters(node_id,day,rx_bytes,tx_bytes) AS (
 SELECT n.id,t.day,t.rx_bytes,t.tx_bytes FROM eligible n JOIN traffic_daily t ON t.node_id=n.id WHERE t.day>=? AND t.day<?
 UNION ALL
 SELECT n.id,substr(t.period_start,1,10),t.rx_bytes,t.tx_bytes FROM eligible n JOIN traffic_hourly t ON t.node_id=n.id WHERE t.period_start>=? AND t.period_start<?
), daily AS (SELECT node_id,day,COALESCE(SUM(1e0*rx_bytes),0.0) AS rx,COALESCE(SUM(1e0*tx_bytes),0.0) AS tx FROM counters GROUP BY node_id,day)
SELECT n.id,n.billing_day,n.billing_mode,n.quota_bytes,d.day,d.rx,d.tx
FROM eligible n LEFT JOIN daily d ON d.node_id=n.id`, start.Format(dayLayout), end.Format(dayLayout), timekey.Format(start), timekey.Format(end))
	if err != nil {
		return nil, fmt.Errorf("read traffic quotas: %w", err)
	}
	defer rows.Close()
	type usage struct {
		rx, tx     float64
		mode       string
		quota      float64
		start, end string
	}
	byNode := map[string]*usage{}
	for rows.Next() {
		var id, mode, quota string
		var billing int
		var day sql.NullString
		var rx, tx sql.NullFloat64
		if err := rows.Scan(&id, &billing, &mode, &quota, &day, &rx, &tx); err != nil {
			return nil, fmt.Errorf("scan traffic quotas: %w", err)
		}
		u := byNode[id]
		if u == nil {
			q, err := strconv.ParseUint(quota, 10, 64)
			if err != nil || q == 0 {
				return nil, fmt.Errorf("invalid traffic quota for node %s", id)
			}
			a, b := CycleBounds(now, billing)
			u = &usage{mode: mode, quota: float64(q), start: a.Format(dayLayout), end: b.Format(dayLayout)}
			byNode[id] = u
		}
		if day.Valid && day.String >= u.start && day.String < u.end {
			u.rx += rx.Float64
			u.tx += tx.Float64
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read traffic quotas: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close traffic quotas: %w", err)
	}
	result := make(map[string]float64, len(byNode))
	for id, u := range byNode {
		used := u.rx + u.tx
		switch u.mode {
		case "max":
			used = max(u.rx, u.tx)
		case "rx":
			used = u.rx
		case "tx":
			used = u.tx
		}
		result[id] = 100 * min(used, math.MaxInt64) / u.quota
	}
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

// Cleanup 最多把 limit 行过期的小时计数汇总为按天计数，
// 并用剩余的批次容量删除过期的按天行。每次汇总及其源行删除都在一个事务中提交，
// 因此重试不会重复计数。
func (s *Service) Cleanup(ctx context.Context, now time.Time, limit int) (int64, error) {
	if s == nil || s.db == nil || limit <= 0 {
		return 0, ErrInvalidInput
	}
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return 0, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	now = now.UTC()
	cutoff := now.Add(-hourlyRetention)
	hourlyBefore := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)
	dailyCutoff := now.Add(-dailyRetention).Format(dayLayout)

	tx, err := lease.BeginWrite(ctx)
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
		query := `INSERT INTO traffic_daily(node_id,day,rx_bytes,tx_bytes) VALUES(?,?,?,?) ON CONFLICT(node_id,day) DO UPDATE SET rx_bytes=excluded.rx_bytes,tx_bytes=excluded.tx_bytes`
		if tx.Dialect() == storage.MySQL {
			query = `INSERT INTO traffic_daily(node_id,day,rx_bytes,tx_bytes) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE rx_bytes=VALUES(rx_bytes),tx_bytes=VALUES(tx_bytes)`
		}
		if _, err := tx.ExecContext(ctx, query, key.nodeID, key.day, combinedRx, combinedTx); err != nil {
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
		deleted, err := storage.DeleteBatch(ctx, tx, `SELECT node_id,day FROM traffic_daily WHERE day<? ORDER BY day,node_id LIMIT ?`, `DELETE FROM traffic_daily WHERE node_id=? AND day=?`, dailyCutoff, remaining)
		if err != nil {
			return 0, fmt.Errorf("delete expired daily traffic: %w", err)
		}
		removed += deleted
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit traffic cleanup: %w", err)
	}
	return removed, nil
}
