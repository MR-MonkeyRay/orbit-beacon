package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/backup"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/notify"
	"orbitbeacon/internal/server/traffic"
	"orbitbeacon/internal/server/webui"
)

const sessionCookie = "orbitbeacon_session"

type Options struct {
	Backups                  *backup.Service
	Inbox                    *inbox.Service
	Notifications            *notify.Service
	Alerts                   *alerts.Service
	History                  *history.Service
	Auth                     *auth.Service
	Nodes                    *nodes.Service
	Management               *management.Service
	Enrollments              *enrollment.Service
	Ingest                   *ingest.Service
	Traffic                  *traffic.Service
	TrustedProxies           []netip.Prefix
	PublicURL                string
	WebUpdateIntervalSeconds int
	Assets                   fs.FS
	Logger                   *slog.Logger
	AccessLog                bool
	BuildVersion             string
	Now                      func() time.Time
}

type handler struct {
	backups                  *backup.Service
	inbox                    *inbox.Service
	notifications            *notify.Service
	alerts                   *alerts.Service
	history                  *history.Service
	auth                     *auth.Service
	nodes                    *nodes.Service
	management               *management.Service
	enrollments              *enrollment.Service
	ingest                   *ingest.Service
	traffic                  *traffic.Service
	trustedProxies           []netip.Prefix
	htmlPolicies             map[string]string
	staticAssets             map[string]staticAsset
	accessLog                bool
	metrics                  *httpMetrics
	origin                   string
	logger                   *slog.Logger
	now                      func() time.Time
	lifecycleMu              sync.RWMutex
	ready                    bool
	streams                  liveStreams
	streamInterval           time.Duration
	streamHeartbeat          time.Duration
	streamLifetime           time.Duration
	webUpdateIntervalSeconds int
	publicPageDisabled       atomic.Bool
	publicPageMu             sync.Mutex
}

func NewHandler(opts Options) (*handler, error) {
	for _, required := range []struct {
		name    string
		present bool
	}{
		{"auth", opts.Auth != nil},
		{"nodes", opts.Nodes != nil},
		{"management", opts.Management != nil},
		{"enrollments", opts.Enrollments != nil},
		{"ingest", opts.Ingest != nil},
		{"alerts", opts.Alerts != nil},
		{"traffic", opts.Traffic != nil},
		{"inbox", opts.Inbox != nil},
		{"logger", opts.Logger != nil},
	} {
		if !required.present {
			return nil, fmt.Errorf("%s service is required", required.name)
		}
	}
	origin, err := config.CanonicalOrigin(opts.PublicURL)
	if err != nil {
		return nil, fmt.Errorf("public_url: %w", err)
	}
	if opts.WebUpdateIntervalSeconds == 0 {
		opts.WebUpdateIntervalSeconds = 10
	}
	if opts.WebUpdateIntervalSeconds < 2 || opts.WebUpdateIntervalSeconds > 60 {
		return nil, errors.New("web_update_interval_seconds must be between 2 and 60")
	}
	publicPage, err := opts.Management.PublicPageSettings(context.Background())
	if err != nil {
		return nil, fmt.Errorf("public page settings: %w", err)
	}
	if opts.Assets == nil {
		opts.Assets = webui.Assets()
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
	policies, err := htmlPolicies(opts.Assets, origin)
	if err != nil {
		return nil, fmt.Errorf("UI policies: %w", err)
	}
	staticAssets, err := prepareStaticAssets(opts.Assets)
	if err != nil {
		return nil, fmt.Errorf("UI assets: %w", err)
	}
	h := &handler{notifications: opts.Notifications, alerts: opts.Alerts, history: opts.History, staticAssets: staticAssets, accessLog: opts.AccessLog, metrics: newHTTPMetrics(opts.BuildVersion), trustedProxies: append([]netip.Prefix(nil), opts.TrustedProxies...), htmlPolicies: policies, auth: opts.Auth, nodes: opts.Nodes, management: opts.Management, enrollments: opts.Enrollments, ingest: opts.Ingest, traffic: opts.Traffic, origin: origin, logger: opts.Logger, now: opts.Now, ready: true}
	h.backups, h.inbox = opts.Backups, opts.Inbox
	h.publicPageDisabled.Store(!publicPage.Enabled)
	h.webUpdateIntervalSeconds = opts.WebUpdateIntervalSeconds
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
	h.CloseLiveStreams()
	h.lifecycleMu.Lock()
	h.ready = false
	h.lifecycleMu.Unlock()
	if err := h.WaitLiveStreams(ctx); err != nil {
		return err
	}
	if h.ingest == nil {
		return nil
	}
	return h.ingest.Shutdown(ctx)
}
