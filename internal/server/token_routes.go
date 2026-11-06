package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/nodes"
)

type tokenHandler func(*handler, http.ResponseWriter, *http.Request, string)

// 此表独立于管理路由；新管理接口默认不向 API 令牌开放。
var tokenRoutes = map[string]tokenHandler{
	"GET /api/v1/nodes":                   (*handler).tokenNodes,
	"GET /api/v1/nodes/{id}":              (*handler).tokenNode,
	"GET /api/v1/nodes/{id}/latest":       (*handler).tokenLatest,
	"GET /api/v1/nodes/{id}/history":      (*handler).tokenHistory,
	"GET /api/v1/nodes/{id}/availability": (*handler).tokenAvailability,
	"GET /api/v1/nodes/{id}/traffic":      (*handler).tokenTraffic,
	"GET /api/v1/nodes/{id}/probes":       (*handler).tokenProbes,
	"GET /api/v1/alerts":                  (*handler).tokenAlerts,
	"GET /api/v1/alerts/summary":          (*handler).tokenAlertSummary,
	"GET /api/v1/alerts/{id}":             (*handler).tokenAlert,
	"GET /api/v1/groups":                  (*handler).tokenGroups,
}

const apiTokenHeaderPrefix = "Bearer obt_"

// 其他 Authorization 头（例如反向代理的 Basic 认证）不是令牌请求，
// 仍走 Cookie 会话，否则控制台会被代理凭据挡住。
func carriesAPIToken(r *http.Request) bool {
	for _, value := range r.Header.Values("Authorization") {
		if strings.HasPrefix(value, apiTokenHeaderPrefix) {
			return true
		}
	}
	return false
}

func (h *handler) serveAPIToken(w http.ResponseWriter, r *http.Request, requestID, pattern string) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		h.serviceError(w, auth.ErrUnauthenticated, requestID)
		return
	}
	if err := h.auth.AuthenticateAPIToken(r.Context(), strings.TrimPrefix(values[0], "Bearer ")); err != nil {
		if errors.Is(err, auth.ErrRateLimited) && h.metrics != nil {
			h.metrics.apiTokenRateLimited.Add(1)
		}
		h.serviceError(w, err, requestID)
		return
	}
	handle, ok := tokenRoutes[pattern]
	if !ok {
		fail(w, http.StatusForbidden, "token_forbidden", "API token cannot access this route", requestID)
		return
	}
	handle(h, w, r, requestID)
}

// 显式 DTO 不嵌入 Node，避免未来新增管理字段自动扩展令牌权限。
type tokenNodeView struct {
	Stopped      bool       `json:"stopped"`
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Status       string     `json:"status"`
	ExpiresOn    *string    `json:"expires_on"`
	Region       string     `json:"region"`
	Provider     string     `json:"provider"`
	GroupID      *string    `json:"group_id"`
	Tags         []string   `json:"tags"`
	CreatedAt    time.Time  `json:"created_at"`
	LastSeen     *time.Time `json:"last_seen"`
	Presence     string     `json:"presence"`
	AgentVersion string     `json:"agent_version"`
}

func tokenNodeDTO(n nodes.Node) tokenNodeView {
	return tokenNodeView{n.Stopped, n.ID, n.Name, n.Status, n.ExpiresOn, n.Region, n.Provider, n.GroupID, n.Tags, n.CreatedAt, n.LastSeen, n.Presence, n.AgentVersion}
}
func tokenNodeID(r *http.Request) string { return strings.Split(r.URL.Path, "/")[4] }
func (h *handler) tokenNodes(w http.ResponseWriter, r *http.Request, requestID string) {
	list, page, ok := h.readNodePage(w, r, requestID)
	if !ok {
		return
	}
	result := make([]tokenNodeView, 0, len(list))
	for _, n := range list {
		result = append(result, tokenNodeDTO(n))
	}
	writeJSON(w, http.StatusOK, struct {
		Nodes []tokenNodeView `json:"nodes"`
		nodePage
	}{result, page})
}
func (h *handler) tokenNode(w http.ResponseWriter, r *http.Request, requestID string) {
	node, err := h.nodes.Get(r.Context(), tokenNodeID(r))
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, tokenNodeDTO(node))
}
func (h *handler) tokenLatest(w http.ResponseWriter, r *http.Request, requestID string) {
	latest, err := h.latestSample(r.Context(), tokenNodeID(r))
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		ReceivedAt time.Time        `json:"received_at"`
		SampledAt  time.Time        `json:"sampled_at"`
		Seq        uint64           `json:"seq"`
		Metrics    tokenMetricsView `json:"metrics"`
		ServerTime time.Time        `json:"server_time"`
	}{latest.ReceivedAt, latest.SampledAt, latest.Seq, tokenMetricsDTO(latest.Metrics), h.now().UTC()})
}

type tokenProbeView struct {
	TLSExpiresAt *string  `json:"tls_expires_at"`
	Status       string   `json:"status"`
	LatencyMS    *float64 `json:"latency_ms"`
	StatusCode   *int     `json:"status_code"`
	CheckedAt    *string  `json:"checked_at"`
	Uptime24H    *float64 `json:"uptime_24h"`
	Checks24H    int64    `json:"checks_24h"`
}

func (h *handler) tokenProbes(w http.ResponseWriter, r *http.Request, requestID string) {
	observed, err := h.readProbes(r, tokenNodeID(r))
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	probes := make([]tokenProbeView, 0, len(observed))
	for _, p := range observed {
		probes = append(probes, tokenProbeView{p.TLSExpiresAt, p.Status, p.LatencyMS, p.StatusCode, p.CheckedAt, p.Uptime24H, p.Checks24H})
	}
	writeJSON(w, http.StatusOK, struct {
		Probes []tokenProbeView `json:"probes"`
	}{probes})
}

func (h *handler) tokenHistory(w http.ResponseWriter, r *http.Request, requestID string) {
	if response, ok := h.readHistory(w, r, tokenNodeID(r), requestID); ok {
		writeJSON(w, http.StatusOK, tokenHistoryDTO(response))
	}
}

func (h *handler) tokenAvailability(w http.ResponseWriter, r *http.Request, requestID string) {
	if summary, ok := h.readAvailability(w, r, tokenNodeID(r), requestID); ok {
		writeJSON(w, http.StatusOK, tokenAvailabilityDTO(summary))
	}
}

func (h *handler) tokenTraffic(w http.ResponseWriter, r *http.Request, requestID string) {
	if summary, ok := h.readTraffic(w, r, tokenNodeID(r), requestID); ok {
		writeJSON(w, http.StatusOK, tokenTrafficDTO(summary))
	}
}

func (h *handler) tokenAlerts(w http.ResponseWriter, r *http.Request, requestID string) {
	if page, ok := h.readAlertPage(w, r, requestID); ok {
		writeJSON(w, http.StatusOK, tokenAlertPage{tokenAlertDTOs(page.Events), page.HasMore, page.NextCursor})
	}
}

func (h *handler) tokenAlertSummary(w http.ResponseWriter, r *http.Request, requestID string) {
	summary, err := h.alerts.Summary(r.Context())
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, tokenAlertSummaryDTO(summary))
}

func (h *handler) tokenAlert(w http.ResponseWriter, r *http.Request, requestID string) {
	if event, ok := h.readAlert(w, r, requestID); ok {
		writeJSON(w, http.StatusOK, tokenAlertDTO(event))
	}
}

func (h *handler) tokenGroups(w http.ResponseWriter, r *http.Request, requestID string) {
	groups, err := h.management.Groups(r.Context())
	if err != nil {
		h.serviceError(w, err, requestID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": tokenGroupDTOs(groups)})
}
