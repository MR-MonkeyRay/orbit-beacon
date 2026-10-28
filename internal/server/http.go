package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/identity"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/notify"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/traffic"
	"orbitbeacon/internal/server/webui"
)

const sessionCookie = "__Host-orbitbeacon_session"

type Options struct {
	Notifications  *notify.Service
	Alerts         *alerts.Service
	History        *history.Service
	Auth           *auth.Service
	Nodes          *nodes.Service
	Management     *management.Service
	Enrollments    *enrollment.Service
	Ingest         *ingest.Service
	Traffic        *traffic.Service
	TrustedProxies []netip.Prefix
	PublicURL      string
	Assets         fs.FS
	Logger         *slog.Logger
	AccessLog      bool
	BuildVersion   string
	Now            func() time.Time
}

type handler struct {
	notifications   *notify.Service
	alerts          *alerts.Service
	history         *history.Service
	auth            *auth.Service
	nodes           *nodes.Service
	management      *management.Service
	enrollments     *enrollment.Service
	ingest          *ingest.Service
	traffic         *traffic.Service
	trustedProxies  []netip.Prefix
	htmlPolicies    map[string]string
	staticAssets    map[string]staticAsset
	accessLog       bool
	metrics         *httpMetrics
	origin          string
	assets          fs.FS
	logger          *slog.Logger
	now             func() time.Time
	lifecycleMu     sync.RWMutex
	ready           bool
	streams         publicStreams
	streamInterval  time.Duration
	streamHeartbeat time.Duration
	streamLifetime  time.Duration
}

type apiError struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	RequestID string            `json:"request_id"`
	Fields    map[string]string `json:"fields,omitempty"`
}

func NewHandler(opts Options) (*handler, error) {
	if opts.Auth == nil || opts.Nodes == nil {
		return nil, errors.New("auth and nodes services are required")
	}
	origin, err := config.CanonicalOrigin(opts.PublicURL)
	if err != nil {
		return nil, fmt.Errorf("public_url: %w", err)
	}
	if opts.Management == nil {
		opts.Management = opts.Auth.Management()
	}
	if opts.Enrollments == nil {
		opts.Enrollments = enrollment.NewService(opts.Nodes.Database(), enrollment.Options{MaxNodes: opts.Nodes.Capacity()})
	}
	// Wire invalidation here so injected services cannot serve stale public metrics.
	opts.Enrollments.OnIdentityChange(opts.Nodes.InvalidatePublic)
	if opts.Ingest == nil {
		ingestOptions := ingest.Options{Now: opts.Now}
		if opts.History != nil {
			ingestOptions.Samples = opts.History.Samples()
		}
		opts.Ingest = ingest.NewService(opts.Nodes.Database(), ingestOptions)
	}
	if opts.Alerts == nil {
		opts.Alerts = alerts.NewService(opts.Nodes.Database(), opts.Now)
	}
	if opts.Traffic == nil {
		opts.Traffic = traffic.NewService(opts.Nodes.Database())
	}
	if opts.Assets == nil {
		opts.Assets = webui.Assets()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if _, err := fs.Stat(opts.Assets, "200.html"); err != nil {
		return nil, fmt.Errorf("UI shell: %w", err)
	}
	if _, err := fs.Stat(opts.Assets, "public.html"); err != nil {
		return nil, fmt.Errorf("public UI shell: %w", err)
	}
	policies, err := htmlPolicies(opts.Assets)
	if err != nil {
		return nil, fmt.Errorf("UI policies: %w", err)
	}
	staticAssets, err := prepareStaticAssets(opts.Assets)
	if err != nil {
		return nil, fmt.Errorf("UI assets: %w", err)
	}
	h := &handler{notifications: opts.Notifications, alerts: opts.Alerts, history: opts.History, staticAssets: staticAssets, accessLog: opts.AccessLog, metrics: newHTTPMetrics(opts.BuildVersion), trustedProxies: append([]netip.Prefix(nil), opts.TrustedProxies...), htmlPolicies: policies, auth: opts.Auth, nodes: opts.Nodes, management: opts.Management, enrollments: opts.Enrollments, ingest: opts.Ingest, traffic: opts.Traffic, origin: origin, assets: opts.Assets, logger: opts.Logger, now: opts.Now, ready: true}
	return h, nil
}

// MarkNotReady makes /healthz/ready fail so load balancers stop routing new
// traffic before the HTTP server begins shutting down.
func (h *handler) MarkNotReady() {
	h.lifecycleMu.Lock()
	h.ready = false
	h.lifecycleMu.Unlock()
}

func (h *handler) Shutdown(ctx context.Context) error {
	h.ClosePublicStreams()
	h.lifecycleMu.Lock()
	h.ready = false
	h.lifecycleMu.Unlock()
	if h.ingest == nil {
		return nil
	}
	return h.ingest.Shutdown(ctx)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		body, _ = json.Marshal(apiError{Code: "internal_error", Message: "The operation could not be completed", RequestID: w.Header().Get("X-Request-ID")})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func fail(w http.ResponseWriter, status int, code, message, requestID string) {
	writeJSON(w, status, apiError{Code: code, Message: message, RequestID: requestID})
}

func decode(w http.ResponseWriter, r *http.Request, dst any, requestID string) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		fail(w, http.StatusBadRequest, "invalid_json", "Content-Type must be application/json", requestID)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	reader := bufio.NewReader(r.Body)
	for {
		first, err := reader.ReadByte()
		if err != nil {
			decodeError(w, err, "Request must contain a JSON object", requestID)
			return false
		}
		if first == ' ' || first == '\t' || first == '\r' || first == '\n' {
			continue
		}
		if first != '{' {
			fail(w, http.StatusBadRequest, "invalid_json", "Request must contain a JSON object", requestID)
			return false
		}
		if err := reader.UnreadByte(); err != nil {
			fail(w, http.StatusBadRequest, "invalid_json", "Unable to read JSON object", requestID)
			return false
		}
		break
	}
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		decodeError(w, err, "Request must contain a valid JSON object with known fields", requestID)
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		decodeError(w, err, "Request must contain exactly one JSON value", requestID)
		return false
	}
	return true
}

func decodeError(w http.ResponseWriter, err error, message, requestID string) {
	var validation *nodes.ValidationError
	if errors.As(err, &validation) {
		writeJSON(w, http.StatusBadRequest, apiError{Code: "validation_failed", Message: validation.Error(), RequestID: requestID, Fields: validation.Fields})
		return
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		fail(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body exceeds 64 KiB", requestID)
		return
	}
	fail(w, http.StatusBadRequest, "invalid_json", message, requestID)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.observeRequest(w, r, h.serveHTTP)
}

func (h *handler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w = headResponseWriter{w}
	}
	if r.TLS != nil {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	requestID, err := identity.UUID(rand.Reader)
	if err != nil {
		h.logInternalError(err, "")
		http.Error(w, "Unable to generate request identity", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	if r.URL.Path == "/healthz/live" || r.URL.Path == "/healthz/ready" {
		h.health(w, r, requestID)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
		h.api(w, r, requestID)
		return
	}
	h.static(w, r)
}

func method(w http.ResponseWriter, r *http.Request, requestID string, allowed ...string) bool {
	for _, candidate := range allowed {
		if r.Method == candidate {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method is not supported for this route", requestID)
	return false
}

func (h *handler) api(w http.ResponseWriter, r *http.Request, requestID string) {
	p := r.URL.Path
	tokenRequest := strings.HasPrefix(p, "/api/v1/") && len(r.Header.Values("Authorization")) > 0
	if strings.HasPrefix(p, "/api/v1/") && tokenInQuery(r) {
		fail(w, http.StatusBadRequest, "token_in_query", "API tokens must be sent in the Authorization header", requestID)
		return
	}
	var tokenPrincipal auth.APIToken
	if tokenRequest {
		headers := r.Header.Values("Authorization")
		fields := strings.Fields(r.Header.Get("Authorization"))
		plaintext := ""
		if len(headers) == 1 && len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
			plaintext = fields[1]
		}
		var err error
		tokenPrincipal, err = h.auth.AuthenticateAPIToken(auth.WithLoginPeer(r.Context(), h.clientAddr(r)), plaintext)
		if err != nil {
			if errors.Is(err, auth.ErrInvalidToken) {
				fail(w, http.StatusUnauthorized, "invalid_token", "API token is invalid or expired", requestID)
			} else {
				h.serviceError(w, err, requestID)
			}
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			fail(w, http.StatusForbidden, "token_read_only", "API tokens only allow GET and HEAD requests", requestID)
			return
		}
		if !apiTokenReadAllowed(p) {
			fail(w, http.StatusForbidden, "token_forbidden", "API tokens cannot access this resource", requestID)
			return
		}
	}
	allowed := routeMethods(p)
	// HEAD shares GET routing only for token-authenticated management reads.
	if tokenRequest && r.Method == http.MethodHead && containsGET(allowed) {
		clone := r.Clone(r.Context())
		clone.Method = http.MethodGet
		r = clone
	}
	if len(allowed) == 0 {
		fail(w, http.StatusNotFound, "not_found", "Route not found", requestID)
		return
	}
	if !method(w, r, requestID, allowed...) {
		return
	}
	if p == "/api/public/v1/nodes" {
		h.publicNodes(w, r, requestID)
		return
	}
	if p == "/api/public/v1/stream" {
		h.publicStream(w, r, requestID)
		return
	}
	if strings.HasPrefix(p, "/api/agent/v1/") {
		h.agentRoute(w, r, requestID)
		return
	}
	if p == "/api/v1/session" && r.Method == http.MethodPost {
		if r.Header.Get("Origin") != h.origin {
			fail(w, http.StatusForbidden, "forbidden", "A trusted Origin is required", requestID)
			return
		}
		var input struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if !decode(w, r, &input, requestID) {
			return
		}
		session, err := h.auth.Login(auth.WithLoginPeer(r.Context(), h.clientAddr(r)), input.Username, input.Password)
		if err != nil {
			h.logLoginRejection(err, r, requestID)
			h.serviceError(w, err, requestID)
			return
		}
		http.SetCookie(w, cookie(session.Token, session.ExpiresAt, int(auth.SessionLifetime.Seconds())))
		writeJSON(w, http.StatusOK, session)
		return
	}
	parts := strings.Split(strings.TrimPrefix(p, "/api/v1/"), "/")
	var c *http.Cookie
	var user auth.User
	var err error
	if tokenRequest {
		user = auth.User{ID: tokenPrincipal.ID, Role: auth.RoleViewer}
		h.auth.TouchAPIToken(r.Context(), tokenPrincipal.ID)
	} else {
		c, err = r.Cookie(sessionCookie)
		if err != nil {
			fail(w, http.StatusUnauthorized, "unauthenticated", "A valid session is required", requestID)
			return
		}
		if p == "/api/v1/me" {
			session, err := h.auth.CurrentSession(r.Context(), c.Value)
			if err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			writeJSON(w, http.StatusOK, session)
			return
		}
		user, err = h.auth.Authenticate(r.Context(), c.Value)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}

	}
	mutation := r.Method != http.MethodGet && r.Method != http.MethodHead
	if mutation {
		if p != "/api/v1/session" && p != "/api/v1/me/password" && user.Role != auth.RoleAdmin {
			fail(w, http.StatusForbidden, "forbidden", "Administrator role is required", requestID)
			return
		}
		if r.Header.Get("Origin") != h.origin {
			fail(w, http.StatusForbidden, "forbidden", "A trusted Origin is required", requestID)
			return
		}
		valid, err := h.auth.VerifyCSRF(r.Context(), c.Value, r.Header.Get("X-CSRF-Token"))
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		if !valid {
			fail(w, http.StatusForbidden, "forbidden", "A session-bound CSRF token is required", requestID)
			return
		}
	}
	if !tokenRequest {
		r = r.WithContext(audit.WithActor(r.Context(), user.ID))
	}
	if h.apiTokensRoute(w, r, requestID, user) {
		return
	}
	if h.notificationsRoute(w, r, requestID) {
		return
	}
	if h.alertsRoute(w, r, requestID) {
		return
	}
	if h.managementRoute(w, r, requestID, user) {
		return
	}
	if h.enrollmentRoute(w, r, requestID, user) {
		return
	}
	switch p {
	case "/api/v1/session":
		if err := h.auth.Logout(r.Context(), c.Value); err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		http.SetCookie(w, cookie("", time.Unix(1, 0), -1))
		w.WriteHeader(http.StatusNoContent)
	case "/api/v1/nodes":
		if r.Method == http.MethodGet {
			limit, offset, err := pagination(r)
			if err != nil {
				h.serviceError(w, nodes.ErrInvalidInput, requestID)
				return
			}
			list, hasMore, err := h.nodes.ListPage(r.Context(), limit, offset)
			if err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			var nextOffset *int
			if hasMore {
				next := offset + limit
				if next > offset {
					nextOffset = &next
				}
			}
			writeJSON(w, http.StatusOK, struct {
				Nodes      []nodes.Node `json:"nodes"`
				HasMore    bool         `json:"has_more"`
				NextOffset *int         `json:"next_offset,omitempty"`
			}{Nodes: list, HasMore: hasMore, NextOffset: nextOffset})
			return
		}
		var input nodes.CreateRequest
		if !decode(w, r, &input, requestID) {
			return
		}
		node, err := h.nodes.CreatePending(r.Context(), input)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusCreated, node)
	case "/api/v1/nodes/batch":
		var input nodes.BatchRequest
		if !decode(w, r, &input, requestID) {
			return
		}
		results, err := h.nodes.Batch(r.Context(), input)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Results []nodes.BatchResult `json:"results"`
		}{Results: results})
	default:
		id := parts[1]
		if len(parts) == 3 && parts[2] == "history" {
			h.nodeHistory(w, r, id, requestID)
			return
		}
		if len(parts) == 3 && parts[2] == "traffic" {
			h.nodeTraffic(w, r, id, requestID)
			return
		}
		if len(parts) == 3 && parts[2] == "probes" {
			h.nodeProbes(w, r, id, requestID)
			return
		}
		if len(parts) == 3 && parts[2] == "latest" {
			latest, err := h.ingest.Latest(r.Context(), id)
			if err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			// Explicit projection excludes process identity and credentials.
			writeJSON(w, http.StatusOK, struct {
				ReceivedAt time.Time  `json:"received_at"`
				SampledAt  time.Time  `json:"sampled_at"`
				Seq        uint64     `json:"seq"`
				Metrics    v1.Metrics `json:"metrics"`
				System     *v1.System `json:"system"`
			}{latest.ReceivedAt, latest.SampledAt, latest.Seq, latest.Metrics, latest.System})
			return
		}
		if len(parts) == 3 && parts[2] == "public-settings" {
			h.publicSettings(w, r, id, requestID)
			return
		}
		if len(parts) == 3 && parts[2] == "credential" {
			if err := h.nodes.RevokeCredential(r.Context(), id); err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if len(parts) == 3 {
			var cfg v1.DesiredConfig
			if r.Method == http.MethodGet {
				cfg, err = h.nodes.GetConfig(r.Context(), id)
			} else {
				var patch nodes.ConfigPatch
				if !decode(w, r, &patch, requestID) {
					return
				}
				cfg, err = h.nodes.PatchConfig(r.Context(), id, patch)
			}
			if err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			writeJSON(w, http.StatusOK, cfg)
			return
		}
		if r.Method == http.MethodPatch {
			var patch nodes.MetadataPatch
			if !decode(w, r, &patch, requestID) {
				return
			}
			node, err := h.nodes.PatchMetadata(r.Context(), id, patch)
			if err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			writeJSON(w, http.StatusOK, node)
			return
		}
		if r.Method == http.MethodDelete {
			if err := h.nodes.Delete(r.Context(), id, r.URL.Query().Get("confirm") == "true"); err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		node, err := h.nodes.Get(r.Context(), id)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		writeJSON(w, http.StatusOK, node)
	}
}

func cookie(value string, expires time.Time, maxAge int) *http.Cookie {
	return &http.Cookie{Name: sessionCookie, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: maxAge}
}

func (h *handler) serviceError(w http.ResponseWriter, err error, requestID string) {
	var validation *nodes.ValidationError
	if errors.As(err, &validation) {
		writeJSON(w, http.StatusBadRequest, apiError{Code: "validation_failed", Message: validation.Error(), RequestID: requestID, Fields: validation.Fields})
		return
	}
	switch {
	case storage.IsBusy(err):
		w.Header().Set("Retry-After", "1")
		fail(w, http.StatusServiceUnavailable, "storage_busy", "Storage is busy; retry later", requestID)
	case errors.Is(err, auth.ErrUnauthenticated), errors.Is(err, nodes.ErrUnauthenticated):
		fail(w, http.StatusUnauthorized, "unauthenticated", "Credentials or session are invalid or expired", requestID)
	case errors.Is(err, auth.ErrTokenCapacity):
		fail(w, http.StatusConflict, "api_token_capacity_reached", "Maximum of 50 active API tokens reached", requestID)
	case errors.Is(err, auth.ErrWrongPassword):
		fail(w, http.StatusBadRequest, "invalid_old_password", "Current password is incorrect", requestID)
	case errors.Is(err, auth.ErrRateLimited):
		w.Header().Set("Retry-After", "1")
		fail(w, http.StatusTooManyRequests, "rate_limited", "Request rate limit reached; retry later", requestID)
	case errors.Is(err, nodes.ErrDisabled):
		fail(w, http.StatusForbidden, "node_disabled", "The node is disabled", requestID)
	case errors.Is(err, nodes.ErrCapacityReached):
		fail(w, http.StatusConflict, "node_capacity_reached", "Configured node capacity has been reached", requestID)
	case errors.Is(err, nodes.ErrNotFound):
		fail(w, http.StatusNotFound, "node_not_found", "Node not found", requestID)
	case errors.Is(err, traffic.ErrNotFound):
		fail(w, http.StatusNotFound, "node_not_found", "Node not found", requestID)
	case errors.Is(err, notify.ErrCapacity):
		fail(w, http.StatusConflict, "notification_channel_capacity_reached", "Maximum of 20 notification channels reached", requestID)
	case errors.Is(err, notify.ErrSecretUnreadable):
		fail(w, http.StatusConflict, "secret_unreadable", "Restore the notification encryption key or replace the affected secret and webhook URL", requestID)
	case errors.Is(err, notify.ErrUnavailable):
		fail(w, http.StatusServiceUnavailable, "not_available", "Notification secrets unavailable", requestID)
	case errors.Is(err, alerts.ErrCapacity):
		fail(w, http.StatusConflict, "alert_rule_capacity_reached", "Maximum of 200 alert rules reached", requestID)
	case errors.Is(err, notify.ErrNotFound), errors.Is(err, alerts.ErrNotFound), errors.Is(err, auth.ErrNotFound), errors.Is(err, management.ErrNotFound):
		fail(w, http.StatusNotFound, "not_found", "Resource not found", requestID)
	case errors.Is(err, auth.ErrLastAdmin):
		fail(w, http.StatusConflict, "last_administrator", "At least one administrator must remain", requestID)
	case errors.Is(err, auth.ErrUsernameConflict):
		fail(w, http.StatusConflict, "username_conflict", "Username is already in use", requestID)
	case errors.Is(err, nodes.ErrInvalidInput), errors.Is(err, auth.ErrInvalidInput), errors.Is(err, management.ErrInvalidInput):
		fail(w, http.StatusBadRequest, "validation_failed", "Request values are invalid", requestID)
	default:
		h.logInternalError(err, requestID)
		fail(w, http.StatusInternalServerError, "internal_error", "The operation could not be completed", requestID)
	}
}

func (h *handler) static(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	name := strings.TrimPrefix(p, "/")
	uiRoute := p == "/login" || p == "/overview" || p == "/nodes" || p == "/alerts" || p == "/settings" || (strings.HasPrefix(p, "/nodes/") && identity.ValidUUID(strings.TrimPrefix(p, "/nodes/")))
	if uiRoute {
		name = "200.html"
		if p == "/login" {
			if _, ok := h.staticAssets["login.html"]; ok {
				name = "login.html"
			}
		}
	}
	if p == "/" {
		name = "public.html"
	}
	if !fs.ValidPath(name) || path.Clean(name) != name {
		http.NotFound(w, r)
		return
	}
	asset, ok := h.staticAssets[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if uiRoute || strings.HasSuffix(name, ".html") {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", h.htmlPolicies[name])
		w.Header().Set("X-Frame-Options", "DENY")
	}
	if strings.HasPrefix(name, "_app/immutable/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	data := asset.data
	etag := asset.etag
	if asset.gzipData != nil {
		w.Header().Add("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header.Get("Accept-Encoding")) {
			data = asset.gzipData
			etag = asset.gzipETag
			w.Header().Set("Content-Encoding", "gzip")
		}
	}
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

var scriptElement = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)
var scriptSource = regexp.MustCompile(`(?i)(?:^|\s)src\s*=`)

func htmlPolicies(assets fs.FS) (map[string]string, error) {
	policies := make(map[string]string)
	err := fs.WalkDir(assets, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".html") {
			return nil
		}
		data, err := fs.ReadFile(assets, name)
		if err != nil {
			return err
		}
		policy := "default-src 'self'; script-src 'self'"
		seen := make(map[string]bool)
		for _, script := range scriptElement.FindAllSubmatch(data, -1) {
			if scriptSource.Match(script[1]) {
				continue
			}
			sum := sha256.Sum256(script[2])
			hash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			if !seen[hash] {
				policy += " " + hash
				seen[hash] = true
			}
		}
		policies[name] = policy + "; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
		return nil
	})
	return policies, err
}
