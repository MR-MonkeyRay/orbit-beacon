package history

import (
	"context"
	"fmt"
	"math"
	"time"
)

// 保留默认值特意以此命名，以便日后添加配置。
const (
	Retention1m = 48 * time.Hour
	Retention5m = 14 * 24 * time.Hour
	Retention1h = 90 * 24 * time.Hour
)

var metricUnits = map[string]string{
	"disk_read_bytes_per_second": "bytes_per_second", "disk_write_bytes_per_second": "bytes_per_second", "disk_busy_percent": "percent",
	"pressure_cpu_percent": "percent", "pressure_memory_percent": "percent", "pressure_io_percent": "percent", "temperature_celsius": "celsius",
	"cpu_percent": "percent", "memory_used_percent": "percent", "swap_used_percent": "percent",
	"load1": "load", "load5": "load", "load15": "load", "disk_used_percent": "percent", "inode_used_percent": "percent",
	"net_rx_bytes_per_second": "bytes_per_second", "net_tx_bytes_per_second": "bytes_per_second",
}

// Selection 表示历史时间范围映射出的查询跨度与聚合步长（秒）。
type Selection struct {
	Duration    time.Duration
	StepSeconds int
}

// SelectRange 将范围字符串（1h/6h/24h/7d/30d/90d）解析为对应的查询窗口与聚合步长。
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
	case "90d":
		return Selection{90 * 24 * time.Hour, 3600}, nil
	default:
		return Selection{}, fmt.Errorf("unsupported history range")
	}
}

// ValidMetric 判断指标名是否为受支持的历史查询序列。
func ValidMetric(metric string) bool { _, ok := metricUnits[metric]; return ok }

// Point 表示历史查询返回的单个时间桶平均值与峰值。
type Point struct {
	T   time.Time `json:"t"`
	Avg float64   `json:"avg"`
	Max float64   `json:"max"`
}

// Response 表示节点指标历史接口返回的序列元数据与时间点集合。
type Response struct {
	Metric      string  `json:"metric"`
	Unit        string  `json:"unit"`
	Range       string  `json:"range"`
	StepSeconds int     `json:"step_seconds"`
	Points      []Point `json:"points"`
}

// Empty 在历史记录不可用时返回结构相同的空响应。
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

// Query 按节点、指标、时间范围及可选分盘路径从历史库读取聚合序列点。
func (s *Service) Query(ctx context.Context, nodeID, metric, requestedRange string, now time.Time, disk string) (Response, error) {
	response, err := Empty(metric, requestedRange)
	if err != nil {
		return Response{}, err
	}
	series := metric
	if disk != "" {
		series += ":" + disk
	}
	selection, _ := SelectRange(requestedRange)
	// 包含与范围起点相交的桶；缺失数据不补 null 行或零值。
	// 待写入的桶在落盘后才可查询。
	start := now.Add(-selection.Duration).Truncate(time.Duration(selection.StepSeconds) * time.Second).Unix()
	points, err := s.store.Query(ctx, nodeID, series, selection.StepSeconds, start, now.Unix())
	if err != nil {
		return Response{}, err
	}
	for _, point := range points {
		// 旧的或损坏的存储可能包含溢出的总和或峰值。
		// 绝不让这样一个点导致整个响应无法编码。
		if math.IsNaN(point.Avg) || math.IsInf(point.Avg, 0) || math.IsNaN(point.Max) || math.IsInf(point.Max, 0) {
			continue
		}
		response.Points = append(response.Points, point)
	}
	return response, nil
}
