package server

import (
	"net/http"
	"path"
	"strings"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/uuid"
)

type authLevel uint8

const (
	anonymous authLevel = iota
	agent
	publicPageGated
	pendingPasswordAllowed
	admin
)

type route struct {
	pattern string
	auth    authLevel
	handle  routeHandler
}

// routeHandler 接收 sessionGate 已认证的会话和令牌；
// 不使用会话的路由两者都为空。
type routeHandler func(h *handler, w http.ResponseWriter, r *http.Request, requestID string, session auth.Session, token string)

// 声明顺序保持每个路径 Allow 列表的传输顺序。
var routes = []route{
	{"GET /api/v1/services", admin, endpoint((*handler).servicesRoute)},
	{"POST /api/v1/services", admin, endpoint((*handler).servicesRoute)},
	{"GET /api/v1/services/{id}", admin, endpoint((*handler).servicesRoute)},
	{"PATCH /api/v1/services/{id}", admin, endpoint((*handler).servicesRoute)},
	{"DELETE /api/v1/services/{id}", admin, endpoint((*handler).servicesRoute)},
	{"GET /api/v1/services/{id}/history", admin, endpoint((*handler).servicesRoute)},
	{"PATCH /api/v1/api-tokens/{id}", admin, (*handler).apiTokens},
	{"GET /api/v1/api-tokens", admin, (*handler).apiTokens},
	{"POST /api/v1/api-tokens", admin, (*handler).apiTokens},
	{"DELETE /api/v1/api-tokens/{id}", admin, (*handler).apiTokens},
	{"GET /api/v1/account/totp", admin, (*handler).accountTOTP},
	{"POST /api/v1/account/totp/setup", admin, (*handler).accountTOTP},
	{"POST /api/v1/account/totp/confirm", admin, (*handler).accountTOTP},
	{"POST /api/v1/account/totp/disable", admin, (*handler).accountTOTP},
	{"POST /api/v1/account/totp/recovery-codes", admin, (*handler).accountTOTP},
	{"POST /api/v1/users/{id}/totp/reset", admin, (*handler).resetUserTOTP},
	{"GET /healthz/live", anonymous, endpoint((*handler).health)},
	{"GET /healthz/ready", anonymous, endpoint((*handler).health)},
	{"POST /api/v1/session", anonymous, endpoint((*handler).loginSession)},
	{"DELETE /api/v1/session", pendingPasswordAllowed, (*handler).endSession},
	{"GET /api/v1/me", pendingPasswordAllowed, withSession((*handler).currentSession)},
	{"PATCH /api/v1/me/password", pendingPasswordAllowed, withSession((*handler).changePassword)},
	{"GET /api/v1/live", admin, (*handler).liveSession},
	{"GET /api/v1/live/stream", admin, (*handler).liveSession},
	{"GET /api/v1/live/ws", admin, (*handler).liveSession},
	{"GET /api/public/v1/nodes", publicPageGated, endpoint((*handler).publicNodes)},
	{"GET /api/public/v1/stream", publicPageGated, endpoint((*handler).publicStream)},
	{"GET /api/public/v1/ws", publicPageGated, endpoint((*handler).publicWebSocket)},
	{"POST /api/agent/v1/enroll", agent, endpoint((*handler).agentRoute)},
	{"POST /api/agent/v1/enroll/check", agent, endpoint((*handler).agentRoute)},
	{"POST /api/agent/v1/report", agent, endpoint((*handler).agentRoute)},
	{"POST /api/agent/v1/recover", agent, endpoint((*handler).agentRoute)},
	{"GET /api/v1/data-management", admin, endpoint((*handler).dataManagement)},
	{"POST /api/v1/data-management/cleanup", admin, endpoint((*handler).dataManagement)},
	{"POST /api/v1/data-management/compact", admin, endpoint((*handler).dataManagement)},
	{"GET /api/v1/storage", admin, withSession((*handler).storageRoute)},
	{"PATCH /api/v1/storage/notifications", admin, withSession((*handler).storageRoute)},
	{"POST /api/v1/storage/operations/preview", admin, withSession((*handler).storageRoute)},
	{"GET /api/v1/storage/operations/{operation_id}", admin, withSession((*handler).storageRoute)},
	{"POST /api/v1/storage/operations/{operation_id}", admin, withSession((*handler).storageRoute)},
	{"GET /api/v1/settings/api-tokens", admin, (*handler).apiTokenSettings},
	{"PATCH /api/v1/settings/api-tokens", admin, (*handler).apiTokenSettings},
	{"GET /api/v1/settings/public-page", admin, endpoint((*handler).publicPageSettings)},
	{"PATCH /api/v1/settings/public-page", admin, endpoint((*handler).publicPageSettings)},
	{"GET /api/v1/audit", admin, endpoint((*handler).auditEvents)},
	{"GET /api/v1/users", admin, endpoint((*handler).users)},
	{"POST /api/v1/users", admin, endpoint((*handler).users)},
	{"GET /api/v1/users/{id}", admin, endpoint((*handler).users)},
	{"PATCH /api/v1/users/{id}", admin, endpoint((*handler).users)},
	{"DELETE /api/v1/users/{id}", admin, endpoint((*handler).users)},
	{"GET /api/v1/groups", admin, endpoint((*handler).groups)},
	{"POST /api/v1/groups", admin, endpoint((*handler).groups)},
	{"PUT /api/v1/groups/order", admin, endpoint((*handler).groupOrder)},
	{"GET /api/v1/groups/{id}", admin, endpoint((*handler).groups)},
	{"PATCH /api/v1/groups/{id}", admin, endpoint((*handler).groups)},
	{"DELETE /api/v1/groups/{id}", admin, endpoint((*handler).groups)},
	{"POST /api/v1/groups/{id}/commands", admin, withSession((*handler).groupCommandsRoute)},
	{"GET /api/v1/nodes", admin, endpoint((*handler).nodesRoute)},
	{"POST /api/v1/nodes", admin, endpoint((*handler).nodesRoute)},
	{"GET /api/v1/nodes/{id}", admin, endpoint((*handler).nodesRoute)},
	{"PATCH /api/v1/nodes/{id}", admin, endpoint((*handler).nodesRoute)},
	{"DELETE /api/v1/nodes/{id}", admin, endpoint((*handler).nodesRoute)},
	{"GET /api/v1/alert-silences", admin, endpoint((*handler).silencesRoute)},
	{"POST /api/v1/alert-silences", admin, endpoint((*handler).silencesRoute)},
	{"DELETE /api/v1/alert-silences/{id}", admin, endpoint((*handler).silencesRoute)},
	{"GET /api/v1/alert-rules", admin, endpoint((*handler).alertsRoute)},
	{"POST /api/v1/alert-rules", admin, endpoint((*handler).alertsRoute)},
	{"GET /api/v1/alert-rules/{id}", admin, endpoint((*handler).alertsRoute)},
	{"PATCH /api/v1/alert-rules/{id}", admin, endpoint((*handler).alertsRoute)},
	{"DELETE /api/v1/alert-rules/{id}", admin, endpoint((*handler).alertsRoute)},
	{"GET /api/v1/notification-channels", admin, endpoint((*handler).notificationsRoute)},
	{"POST /api/v1/notification-channels", admin, endpoint((*handler).notificationsRoute)},
	{"GET /api/v1/notification-channels/{id}", admin, endpoint((*handler).notificationsRoute)},
	{"PATCH /api/v1/notification-channels/{id}", admin, endpoint((*handler).notificationsRoute)},
	{"DELETE /api/v1/notification-channels/{id}", admin, endpoint((*handler).notificationsRoute)},
	{"GET /api/v1/backup-plans", admin, withSession((*handler).backupsRoute)},
	{"POST /api/v1/backup-plans", admin, withSession((*handler).backupsRoute)},
	{"GET /api/v1/backup-plans/{id}", admin, withSession((*handler).backupsRoute)},
	{"PATCH /api/v1/backup-plans/{id}", admin, withSession((*handler).backupsRoute)},
	{"DELETE /api/v1/backup-plans/{id}", admin, withSession((*handler).backupsRoute)},
	{"POST /api/v1/nodes/batch", admin, endpoint((*handler).nodesRoute)},
	{"GET /api/v1/nodes/{id}/commands", admin, withSession((*handler).commandsRoute)},
	{"POST /api/v1/nodes/{id}/commands", admin, withSession((*handler).commandsRoute)},
	{"POST /api/v1/nodes/{id}/commands/{command_id}/cancel", admin, withSession((*handler).commandsRoute)},
	{"GET /api/v1/nodes/{id}/latest", admin, endpoint((*handler).nodesRoute)},
	{"GET /api/v1/nodes/{id}/history", admin, endpoint((*handler).nodesRoute)},
	{"GET /api/v1/nodes/{id}/availability", admin, endpoint((*handler).nodesRoute)},
	{"GET /api/v1/nodes/{id}/traffic", admin, endpoint((*handler).nodesRoute)},
	{"GET /api/v1/nodes/{id}/probes", admin, endpoint((*handler).nodesRoute)},
	{"GET /api/v1/nodes/{id}/probes/history", admin, endpoint((*handler).nodesRoute)},
	{"GET /api/v1/nodes/{id}/traffic-settings", admin, endpoint((*handler).nodesRoute)},
	{"PATCH /api/v1/nodes/{id}/traffic-settings", admin, endpoint((*handler).nodesRoute)},
	{"GET /api/v1/nodes/{id}/public-settings", admin, endpoint((*handler).nodesRoute)},
	{"PATCH /api/v1/nodes/{id}/public-settings", admin, endpoint((*handler).nodesRoute)},
	{"DELETE /api/v1/nodes/{id}/credential", admin, endpoint((*handler).nodesRoute)},
	{"GET /api/v1/enrollments", admin, endpoint((*handler).enrollmentRoute)},
	{"POST /api/v1/enrollments", admin, endpoint((*handler).enrollmentRoute)},
	{"GET /api/v1/enrollments/{id}", admin, endpoint((*handler).enrollmentRoute)},
	{"DELETE /api/v1/enrollments/{id}", admin, endpoint((*handler).enrollmentRoute)},
	{"GET /api/v1/alerts", admin, endpoint((*handler).alertsRoute)},
	{"GET /api/v1/alerts/summary", admin, endpoint((*handler).alertsRoute)},
	{"GET /api/v1/alerts/{id}", admin, endpoint((*handler).alertsRoute)},
	{"GET /api/v1/notification-deliveries", admin, endpoint((*handler).notificationsRoute)},
	{"POST /api/v1/notification-channels/{id}/test", admin, endpoint((*handler).notificationsRoute)},
	{"POST /api/v1/backups/exports", admin, withSession((*handler).backupsRoute)},
	{"POST /api/v1/backup-plans/preview", admin, withSession((*handler).backupsRoute)},
	{"GET /api/v1/tasks", admin, withSession((*handler).backupsRoute)},
	{"GET /api/v1/tasks/{id}", admin, withSession((*handler).backupsRoute)},
	{"GET /api/v1/tasks/{id}/download", admin, withSession((*handler).backupsRoute)},
	{"POST /api/v1/tasks/{id}/password", admin, withSession((*handler).backupsRoute)},
	{"GET /api/v1/notifications", admin, withSession((*handler).inboxRoute)},
	{"GET /api/v1/notifications/summary", admin, withSession((*handler).inboxRoute)},
	{"POST /api/v1/notifications/read-all", admin, withSession((*handler).inboxRoute)},
	{"GET /api/v1/notifications/{id}", admin, withSession((*handler).inboxRoute)},
	{"PATCH /api/v1/notifications/{id}/read", admin, withSession((*handler).inboxRoute)},
}

func endpoint(f func(*handler, http.ResponseWriter, *http.Request, string)) routeHandler {
	return func(h *handler, w http.ResponseWriter, r *http.Request, id string, _ auth.Session, _ string) {
		f(h, w, r, id)
	}
}

func withSession(f func(*handler, http.ResponseWriter, *http.Request, string, auth.Session)) routeHandler {
	return func(h *handler, w http.ResponseWriter, r *http.Request, id string, session auth.Session, _ string) {
		f(h, w, r, id, session)
	}
}

type routeRegistry struct {
	paths     *http.ServeMux
	methods   map[string][]string
	endpoints map[string]*routeEndpoint
}

var apiRoutes = newRouteRegistry()

// mux 只解析路径；方法对照路由表检查，
// 这样 Allow 列表保持精确，且 GET 绝不隐含 HEAD。
func newRouteRegistry() *routeRegistry {
	registry := &routeRegistry{paths: http.NewServeMux(), methods: make(map[string][]string), endpoints: make(map[string]*routeEndpoint)}
	for _, entry := range routes {
		method, label, _ := strings.Cut(entry.pattern, " ")
		endpoint := &routeEndpoint{route: entry, label: label}
		if len(registry.methods[label]) == 0 {
			registry.paths.Handle(label, endpoint)
		}
		registry.methods[label] = append(registry.methods[label], method)
		registry.endpoints[entry.pattern] = endpoint
	}
	return registry
}

type routeEndpoint struct {
	route
	label string
}

func (endpoint *routeEndpoint) ServeHTTP(http.ResponseWriter, *http.Request) {
	panic("routes are dispatched through routeRegistry.serve")
}

// Handler 选择 ServeMux 注册，但不运行其重定向或 HTML 错误处理。
// 在询问 mux 之前拒绝非规范路径。
func (registry *routeRegistry) match(r *http.Request) *routeEndpoint {
	if path.Clean(r.URL.EscapedPath()) != r.URL.EscapedPath() {
		return nil
	}
	selected, _ := registry.paths.Handler(r)
	endpoint, ok := selected.(*routeEndpoint)
	if !ok {
		return nil
	}
	if strings.Contains(endpoint.label, "{id}") {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) != len(strings.Split(endpoint.label, "/")) || !uuid.ValidUUID(parts[4]) {
			return nil
		}
	}
	return endpoint
}

func (registry *routeRegistry) serve(h *handler, w http.ResponseWriter, r *http.Request, requestID string) {
	entry := registry.match(r)
	if entry == nil {
		fail(w, http.StatusNotFound, "not_found", "Route not found", requestID)
		return
	}
	r.Pattern = entry.label
	// 携带 API 令牌的管理请求在方法分派前进入令牌分支，
	// 因此白名单路径上未注册的写方法也不能落入普通处理函数。
	level := entry.auth
	if requested := registry.endpoints[r.Method+" "+entry.label]; requested != nil {
		level = requested.auth
	}
	tokenRequest := (level == admin || level == pendingPasswordAllowed) && carriesAPIToken(r)
	if !tokenRequest && !method(w, r, requestID, registry.methods[entry.label]...) {
		return
	}
	stream := entry.label == "/api/public/v1/stream" || entry.label == "/api/public/v1/ws" ||
		entry.label == "/api/v1/live/stream" || entry.label == "/api/v1/live/ws"
	if !stream && !strings.HasPrefix(entry.label, "/healthz/") {
		lease, err := h.nodes.Database().Acquire(r.Context(), storage.Reader)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		defer lease.Close()
		r = r.WithContext(lease.Context())
	}
	if tokenRequest {
		h.serveAPIToken(w, r, requestID, r.Method+" "+entry.label)
		return
	}
	entry, ok := registry.endpoints[r.Method+" "+entry.label]
	if !ok {
		fail(w, http.StatusNotFound, "not_found", "Route not found", requestID)
		return
	}
	var session auth.Session
	var token string
	switch entry.auth {
	case agent:
		// Agent 端点在读取请求体之前认证并准入其 Bearer 凭据；
		// 注册票据和节点凭据不同。
	case publicPageGated:
		if err := h.publicPageAccess(); err != nil {
			h.publicPageError(w, requestID)
			return
		}
	case admin, pendingPasswordAllowed:
		r, token, session, ok = h.sessionGate(w, r, requestID, entry.auth)
		if !ok {
			return
		}
	}
	entry.handle(h, w, r, requestID, session, token)
}

func (h *handler) currentSession(w http.ResponseWriter, r *http.Request, requestID string, session auth.Session) {
	writeJSON(w, http.StatusOK, session)
}

func (h *handler) endSession(w http.ResponseWriter, r *http.Request, requestID string, _ auth.Session, token string) {
	h.logoutSession(w, r, requestID, token)
}

func (h *handler) liveSession(w http.ResponseWriter, r *http.Request, requestID string, session auth.Session, token string) {
	h.consoleLive(w, r, requestID, token, session)
}
