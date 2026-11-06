package server

import (
	"time"

	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/availability"
	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/traffic"
)

// 令牌响应单独声明，服务层或管理响应扩展字段时不自动扩大权限。
type tokenAlertView struct {
	ID             string     `json:"id"`
	RuleID         string     `json:"rule_id"`
	NodeID         string     `json:"node_id"`
	RuleName       string     `json:"rule_name"`
	Metric         string     `json:"metric"`
	Severity       string     `json:"severity"`
	NodeName       string     `json:"node_name"`
	State          string     `json:"state"`
	Value          *float64   `json:"value"`
	StartedAt      time.Time  `json:"started_at"`
	ResolvedAt     *time.Time `json:"resolved_at"`
	LastNotifiedAt *time.Time `json:"last_notified_at"`
	Silenced       bool       `json:"silenced"`
}

func tokenAlertDTO(e alerts.Event) tokenAlertView {
	return tokenAlertView{e.ID, e.RuleID, e.NodeID, e.RuleName, e.Metric, e.Severity, e.NodeName, e.State, e.Value, e.StartedAt, e.ResolvedAt, e.LastNotifiedAt, e.Silenced}
}
func tokenAlertDTOs(events []alerts.Event) []tokenAlertView {
	result := make([]tokenAlertView, 0, len(events))
	for _, e := range events {
		result = append(result, tokenAlertDTO(e))
	}
	return result
}

type tokenAlertPage struct {
	Events     []tokenAlertView `json:"events"`
	HasMore    bool             `json:"has_more"`
	NextCursor string           `json:"next_cursor,omitempty"`
}
type tokenFiringCounts struct {
	Total    int64 `json:"total"`
	Info     int64 `json:"info"`
	Warning  int64 `json:"warning"`
	Critical int64 `json:"critical"`
	Unknown  int64 `json:"unknown"`
}
type tokenAlertSummaryView struct {
	Firing tokenFiringCounts `json:"firing"`
	Newest []tokenAlertView  `json:"newest"`
}

func tokenAlertSummaryDTO(s alerts.AlertSummary) tokenAlertSummaryView {
	return tokenAlertSummaryView{tokenFiringCounts{s.Firing.Total, s.Firing.Info, s.Firing.Warning, s.Firing.Critical, s.Firing.Unknown}, tokenAlertDTOs(s.Newest)}
}

type tokenHistoryPoint struct {
	T   time.Time `json:"t"`
	Avg float64   `json:"avg"`
	Max float64   `json:"max"`
}
type tokenHistoryView struct {
	Metric      string              `json:"metric"`
	Unit        string              `json:"unit"`
	Range       string              `json:"range"`
	StepSeconds int                 `json:"step_seconds"`
	Points      []tokenHistoryPoint `json:"points"`
}

func tokenHistoryDTO(r history.Response) tokenHistoryView {
	points := make([]tokenHistoryPoint, 0, len(r.Points))
	for _, p := range r.Points {
		points = append(points, tokenHistoryPoint{p.T, p.Avg, p.Max})
	}
	return tokenHistoryView{r.Metric, r.Unit, r.Range, r.StepSeconds, points}
}

type tokenAvailabilityDay struct {
	Day     string   `json:"day"`
	Online  int64    `json:"online_seconds"`
	Offline int64    `json:"offline_seconds"`
	Unknown int64    `json:"unknown_seconds"`
	Percent *float64 `json:"percent"`
}
type tokenAvailabilityView struct {
	Days    []tokenAvailabilityDay `json:"days"`
	Percent *float64               `json:"percent"`
}

func tokenAvailabilityDTO(s availability.Summary) tokenAvailabilityView {
	days := make([]tokenAvailabilityDay, 0, len(s.Days))
	for _, d := range s.Days {
		days = append(days, tokenAvailabilityDay{d.Day, d.Online, d.Offline, d.Unknown, d.Percent})
	}
	return tokenAvailabilityView{days, s.Percent}
}

type tokenTrafficCycle struct {
	UsedBytes  string    `json:"used_bytes"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	RxBytes    string    `json:"rx_bytes"`
	TxBytes    string    `json:"tx_bytes"`
	TotalBytes string    `json:"total_bytes"`
}
type tokenTrafficDay struct {
	Day     string `json:"day"`
	RxBytes string `json:"rx_bytes"`
	TxBytes string `json:"tx_bytes"`
}
type tokenTrafficView struct {
	BillingMode string            `json:"billing_mode"`
	BillingDay  *int              `json:"billing_day"`
	QuotaBytes  *string           `json:"quota_bytes"`
	Cycle       tokenTrafficCycle `json:"cycle"`
	Daily       []tokenTrafficDay `json:"daily"`
	UpdatedAt   *time.Time        `json:"updated_at"`
}

func tokenTrafficDTO(s traffic.Summary) tokenTrafficView {
	daily := make([]tokenTrafficDay, 0, len(s.Daily))
	for _, d := range s.Daily {
		daily = append(daily, tokenTrafficDay{d.Day, d.RxBytes, d.TxBytes})
	}
	c := s.Cycle
	return tokenTrafficView{s.BillingMode, s.BillingDay, s.QuotaBytes, tokenTrafficCycle{c.UsedBytes, c.Start, c.End, c.RxBytes, c.TxBytes, c.TotalBytes}, daily, s.UpdatedAt}
}

type tokenGroupView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func tokenGroupDTOs(groups []management.Group) []tokenGroupView {
	result := make([]tokenGroupView, 0, len(groups))
	for _, g := range groups {
		result = append(result, tokenGroupView{g.ID, g.Name})
	}
	return result
}
