package history

import (
	"context"
	"fmt"
	"math"
	"time"
)

// Retention defaults are deliberately named so configuration can be added later.
const (
	Retention1m = 48 * time.Hour
	Retention5m = 14 * 24 * time.Hour
	Retention1h = 90 * 24 * time.Hour
)

var metricUnits = map[string]string{
	"cpu_percent": "percent", "memory_used_percent": "percent", "swap_used_percent": "percent",
	"load1": "load", "disk_used_percent": "percent",
	"net_rx_bytes_per_second": "bytes_per_second", "net_tx_bytes_per_second": "bytes_per_second",
}

type Selection struct {
	Duration    time.Duration
	StepSeconds int
}

func SelectRange(value string) (Selection, error) {
	switch value {
	case "1h":
		return Selection{time.Hour, 60}, nil
	case "6h":
		return Selection{6 * time.Hour, 60}, nil
	case "24h":
		return Selection{24 * time.Hour, 300}, nil
	case "7d":
		return Selection{7 * 24 * time.Hour, 300}, nil
	case "30d":
		return Selection{30 * 24 * time.Hour, 3600}, nil
	default:
		return Selection{}, fmt.Errorf("unsupported history range")
	}
}

func ValidMetric(metric string) bool { _, ok := metricUnits[metric]; return ok }

type Point struct {
	T   time.Time `json:"t"`
	Avg float64   `json:"avg"`
	Max float64   `json:"max"`
}

type Response struct {
	Metric      string  `json:"metric"`
	Unit        string  `json:"unit"`
	Range       string  `json:"range"`
	StepSeconds int     `json:"step_seconds"`
	Points      []Point `json:"points"`
}

// Empty returns the same response shape when best-effort history is disabled.
func Empty(metric, requestedRange string) (Response, error) {
	selection, err := SelectRange(requestedRange)
	if err != nil {
		return Response{}, err
	}
	unit, ok := metricUnits[metric]
	if !ok {
		return Response{}, fmt.Errorf("unsupported history metric")
	}
	return Response{Metric: metric, Unit: unit, Range: requestedRange, StepSeconds: selection.StepSeconds, Points: []Point{}}, nil
}

func (s *Service) Query(ctx context.Context, nodeID, metric, requestedRange string, now time.Time) (Response, error) {
	response, err := Empty(metric, requestedRange)
	if err != nil {
		return Response{}, err
	}
	selection, _ := SelectRange(requestedRange)
	// Include the bucket intersecting the range's leading edge. No null gap rows
	// or synthetic zeros are produced, and pending buckets become visible on flush.
	start := now.Add(-selection.Duration).Truncate(time.Duration(selection.StepSeconds) * time.Second).Unix()
	rows, err := s.reader.QueryContext(ctx, `SELECT t, total / samples, peak FROM buckets WHERE node_id=? AND metric=? AND step=? AND t>=? AND t<=? ORDER BY t`, nodeID, metric, selection.StepSeconds, start, now.Unix())
	if err != nil {
		return Response{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var point Point
		var timestamp int64
		if err := rows.Scan(&timestamp, &point.Avg, &point.Max); err != nil {
			return Response{}, err
		}
		// Old or damaged stores may contain overflowing totals or peaks. Never
		// let one such point make the whole response impossible to encode.
		if math.IsNaN(point.Avg) || math.IsInf(point.Avg, 0) || math.IsNaN(point.Max) || math.IsInf(point.Max, 0) {
			continue
		}
		point.T = time.Unix(timestamp, 0).UTC()
		response.Points = append(response.Points, point)
	}
	return response, rows.Err()
}
