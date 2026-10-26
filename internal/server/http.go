package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/identity"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/webui"
)

const sessionCookie = "__Host-orbitbeacon_session"

type Options struct {
	Auth        *auth.Service
	Nodes       *nodes.Service
	Management  *management.Service
	Enrollments *enrollment.Service
	Ingest      *ingest.Service
	PublicURL   string
	Assets      fs.FS
	Logger      *slog.Logger
}

type handler struct {
	auth        *auth.Service
	nodes       *nodes.Service
	management  *management.Service
	enrollments *enrollment.Service
	ingest      *ingest.Service
	origin      string
	assets      fs.FS
	logger      *slog.Logger
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
	if opts.Ingest == nil {
		opts.Ingest = ingest.NewService(opts.Nodes.Database(), ingest.Options{})
	}
	if opts.Assets == nil {
		opts.Assets = webui.Assets()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if _, err := fs.Stat(opts.Assets, "200.html"); err != nil {
		return nil, fmt.Errorf("UI shell: %w", err)
	}
	return &handler{auth: opts.Auth, nodes: opts.Nodes, management: opts.Management, enrollments: opts.Enrollments, ingest: opts.Ingest, origin: origin, assets: opts.Assets, logger: opts.Logger}, nil
}

func (h *handler) Shutdown(ctx context.Context) error {
	if h.ingest == nil {
		return nil
	}
	return h.ingest.Shutdown(ctx)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
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
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		fail(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body exceeds 64 KiB", requestID)
		return
	}
	fail(w, http.StatusBadRequest, "invalid_json", message, requestID)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID, err := identity.UUID(rand.Reader)
	if err != nil {
		h.logInternalError(err, "")
		http.Error(w, "Unable to generate request identity", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
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
	allowed := routeMethods(p)
	if len(allowed) == 0 {
		fail(w, http.StatusNotFound, "not_found", "Route not found", requestID)
		return
	}
	if !method(w, r, requestID, allowed...) {
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
		session, err := h.auth.Login(auth.WithLoginPeer(r.Context(), r.RemoteAddr), input.Username, input.Password)
		if err != nil {
			h.serviceError(w, err, requestID)
			return
		}
		http.SetCookie(w, cookie(session.Token, session.ExpiresAt, int(auth.SessionLifetime.Seconds())))
		writeJSON(w, http.StatusOK, session)
		return
	}
	parts := strings.Split(strings.TrimPrefix(p, "/api/v1/"), "/")
	c, err := r.Cookie(sessionCookie)
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
	user, err := h.auth.Authenticate(r.Context(), c.Value)
	if err != nil {
		h.serviceError(w, err, requestID)
		return
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
	r = r.WithContext(audit.WithActor(r.Context(), user.ID))
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
			list, err := h.nodes.List(r.Context(), limit, offset)
			if err != nil {
				h.serviceError(w, err, requestID)
				return
			}
			writeJSON(w, http.StatusOK, struct {
				Nodes []nodes.Node `json:"nodes"`
			}{Nodes: list})
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
		if len(parts) == 3 {
			var cfg nodes.DesiredConfig
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
	case errors.Is(err, auth.ErrRateLimited):
		w.Header().Set("Retry-After", "1")
		fail(w, http.StatusTooManyRequests, "rate_limited", "Password work admission is busy; retry later", requestID)
	case errors.Is(err, nodes.ErrCapacityReached):
		fail(w, http.StatusConflict, "node_capacity_reached", "Configured node capacity has been reached", requestID)
	case errors.Is(err, nodes.ErrNotFound):
		fail(w, http.StatusNotFound, "node_not_found", "Node not found", requestID)
	case errors.Is(err, auth.ErrNotFound), errors.Is(err, management.ErrNotFound):
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
	if p == "/" {
		target := "/login"
		if c, err := r.Cookie(sessionCookie); err == nil {
			if _, err := h.auth.Authenticate(r.Context(), c.Value); err == nil {
				target = "/overview"
				_ = h.auth.CleanupExpiredSessions(r.Context())
			}
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	name := strings.TrimPrefix(p, "/")
	uiRoute := p == "/login" || p == "/overview" || p == "/nodes" || p == "/alerts" || p == "/settings" || p == "/status" || (strings.HasPrefix(p, "/nodes/") && identity.ValidUUID(strings.TrimPrefix(p, "/nodes/")))
	if uiRoute {
		name = "200.html"
		if p == "/login" {
			if _, err := fs.Stat(h.assets, "login.html"); err == nil {
				name = "login.html"
			}
		}
	}
	if !fs.ValidPath(name) || path.Clean(name) != name {
		http.NotFound(w, r)
		return
	}
	file, err := h.assets.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || stat.IsDir() {
		http.NotFound(w, r)
		return
	}
	if uiRoute || strings.HasSuffix(name, ".html") {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
	}
	if strings.HasPrefix(name, "_app/immutable/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if seeker, ok := file.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, stat.ModTime(), seeker)
		return
	}
	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "Unable to read asset", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
