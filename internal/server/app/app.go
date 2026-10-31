// Package app 负责服务器的组装与生命周期。
package app

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"orbitbeacon/internal/buildinfo"
	"orbitbeacon/internal/config"
	"orbitbeacon/internal/logging"
	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/backup"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/notify"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/traffic"
)

type lifecycleHandler interface {
	http.Handler
	MarkNotReady()
	CloseLiveStreams()
	WaitLiveStreams(context.Context) error
	RunRetention(context.Context)
}

type notificationService interface{ Shutdown(context.Context) error }
type historyService interface{ CloseContext(context.Context) error }
type httpService interface {
	Serve(net.Listener) error
	Shutdown(context.Context) error
	Close() error
}

// App 是由 Build 组装的一次性服务器。Run 会释放其全部资源。
type App struct {
	cfg                       config.Server
	logger                    *slog.Logger
	version                   string
	lock                      io.Closer
	db, reader                io.Closer
	history                   historyService
	ingest                    interface{ Shutdown(context.Context) error }
	notifications             notificationService
	background                []func(context.Context)
	handler                   lifecycleHandler
	httpServer, metricsServer httpService
	listener, metricsListener net.Listener
}

func Build(cfg config.Server, logger *slog.Logger, ctx context.Context) (_ *App, err error) {
	if logger == nil {
		return nil, errors.New("logger is required")
	}
	a := &App{cfg: cfg, logger: logger, version: buildinfo.Current().Version}
	defer func() {
		if err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = errors.Join(err, a.close(cleanupCtx))
		}
	}()
	version := a.version
	dataLock, err := datadir.Lock(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	a.lock = dataLock
	if err := backup.RecoverRestore(cfg.DataDir); err != nil {
		return nil, err
	}
	if err := datadir.Prepare(cfg.DataDir); err != nil {
		return nil, err
	}
	databasePath := filepath.Join(cfg.DataDir, "orbitbeacon.db")
	db, err := storage.Open(ctx, databasePath)
	if err != nil {
		return nil, err
	}
	a.db = db
	authService := auth.NewService(db, auth.Options{})
	if err := authService.InitializeAdministrator(ctx, os.LookupEnv); err != nil {
		return nil, err
	}
	publicReader, err := storage.OpenReader(ctx, databasePath)
	if err != nil {
		return nil, err
	}
	a.reader = publicReader
	nodeService := nodes.NewService(db, nodes.Options{MaxNodes: cfg.MaxNodes})
	nodeService.SetPublicReader(publicReader)

	metricHistory := openMetricHistory(ctx, cfg.DataDir, publicReader, logger)
	if metricHistory != nil {
		a.history = metricHistory
	}
	ingestOptions := ingest.Options{}
	if metricHistory != nil {
		ingestOptions.Samples = metricHistory.Samples()
	}
	ingestService := ingest.NewService(db, ingestOptions)
	key, err := datadir.LoadKey(filepath.Join(cfg.DataDir, "secrets.key"))
	if err != nil {
		return nil, err
	}
	box, err := secretbox.New(key, "orbitbeacon notification channel v1")
	if err != nil {
		clear(key)
		return nil, err
	}
	backupBox, err := secretbox.New(key, "orbitbeacon backup secrets v1")
	clear(key)
	if err != nil {
		return nil, err
	}
	backups, err := backup.New(db, backup.Options{DataDir: cfg.DataDir, SecretBox: backupBox, ServerVersion: version, Uploader: backup.NewObjectUploader()})
	if err != nil {
		return nil, err
	}
	notifications := notify.New(db, box, notify.Options{Logger: logger, AllowInsecureWebhooks: cfg.AllowInsecureWebhooks, AllowPrivateNetworks: cfg.NotificationAllowPrivateNetworks})
	alertService := alerts.NewService(db, nil)
	evaluator := alerts.NewEvaluator(alertService, ingestService, alerts.EvaluatorOptions{Logger: logger, Notifier: notifications})
	enrollments := enrollment.NewService(db, enrollment.Options{MaxNodes: cfg.MaxNodes})
	enrollments.OnIdentityChange(func(replaced bool) {
		if replaced {
			nodeService.InvalidatePublicSafety()
		} else {
			nodeService.InvalidatePublic()
		}
	})
	handler, err := server.NewHandler(server.Options{Management: authService.Management(), Enrollments: enrollments, Traffic: traffic.NewService(db), Inbox: inbox.New(db), Backups: backups, Notifications: notifications, Alerts: alertService, Ingest: ingestService, History: metricHistory, Auth: authService, Nodes: nodeService, TrustedProxies: cfg.TrustedProxies, PublicURL: cfg.PublicURL, WebUpdateIntervalSeconds: cfg.WebUpdateIntervalSeconds, Logger: logger, AccessLog: cfg.AccessLog, BuildVersion: version})
	if err != nil {
		return nil, err
	}
	httpServer := newHTTPServer(cfg.ListenAddr, handler, logger)
	httpServer.RegisterOnShutdown(handler.CloseLiveStreams)
	var metricsServer *http.Server
	if cfg.MetricsListen != "" {
		metricsServer = newHTTPServer(cfg.MetricsListen, handler.MetricsHandler(), logger)
	}
	mainListener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, err
	}
	a.listener = mainListener
	var metricsListener net.Listener
	if metricsServer != nil {
		metricsListener, err = net.Listen("tcp", cfg.MetricsListen)
		if err != nil {
			return nil, err
		}
		// 仅赋非 nil 的 server：带类型的 nil 会使接口值非 nil。
		a.metricsServer, a.metricsListener = metricsServer, metricsListener
	}

	a.handler, a.httpServer = handler, httpServer
	a.ingest, a.notifications = ingestService, notifications
	a.background = []func(context.Context){backups.Run, notifications.Run, evaluator.Run, handler.RunRetention}
	return a, nil
}

func (a *App) Run(ctx context.Context) error {
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	// 后台任务随 runCtx 结束，须在数据库关闭前完成。
	var background sync.WaitGroup
	for _, run := range a.background {
		background.Add(1)
		go func(run func(context.Context)) { defer background.Done(); run(runCtx) }(run)
	}

	mainListener, metricsListener := a.listener, a.metricsListener
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-runCtx.Done()
		a.shutdown(&background)
	}()
	a.logger.Info("server_started", "listen_addr", a.cfg.ListenAddr, "public_url", a.cfg.PublicURL, "version", a.version)
	serveErrors := make(chan error, 2)
	count := 1
	if a.metricsServer != nil {
		count++
		a.logger.Info("metrics_started", "listen_addr", a.cfg.MetricsListen)
		go func() { serveErrors <- a.metricsServer.Serve(metricsListener) }()
	}
	go func() { serveErrors <- a.httpServer.Serve(mainListener) }()
	err := <-serveErrors
	stop()
	<-stopped
	for range count - 1 {
		<-serveErrors
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// shutdown 在 Run 的上下文结束时执行（收到信号或 HTTP 服务出错后）。
// HTTP 与实时流共用 10 秒；后续每个组件各有 5 秒关闭时限，
// 总时长不超过 deploy/ 中配置的 30 秒停机宽限期。
func (a *App) shutdown(background *sync.WaitGroup) {
	// 先上报 not-ready，让负载均衡器停止转发新流量。
	a.handler.MarkNotReady()
	a.drainHTTP()
	if a.ingest != nil {
		a.withDeadline(a.ingest.Shutdown, "ingest_shutdown_timeout", "shutdown_timeout")
	}
	if a.notifications != nil {
		a.withDeadline(a.notifications.Shutdown, "notification_shutdown_timeout", "shutdown_timeout")
	}
	background.Wait()
	if a.history != nil {
		a.withDeadline(a.history.CloseContext, "history_shutdown_failed", "history_flush_failed")
		a.history = nil
	}
	if err := a.close(context.Background()); err != nil {
		a.logger.LogAttrs(context.Background(), slog.LevelError, "server_close_failed", logging.ErrorAttrs(err, "close_failed")...)
	}
	a.logger.Info("server_stopped")
}

func (a *App) drainHTTP() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.handler.CloseLiveStreams()
	var shutdown sync.WaitGroup
	shutdown.Add(1)
	go func() {
		defer shutdown.Done()
		if err := a.handler.WaitLiveStreams(ctx); err != nil {
			a.logger.LogAttrs(ctx, slog.LevelWarn, "live_stream_shutdown_timeout", logging.ErrorAttrs(err, "shutdown_timeout")...)
		}
	}()
	for _, current := range []httpService{a.httpServer, a.metricsServer} {
		if current == nil {
			continue
		}
		shutdown.Add(1)
		go func(current httpService) {
			defer shutdown.Done()
			if err := current.Shutdown(ctx); err != nil {
				a.logger.LogAttrs(ctx, slog.LevelWarn, "http_shutdown_failed", logging.ErrorAttrs(err, "shutdown_failed")...)
				_ = current.Close()
			}
		}(current)
	}
	shutdown.Wait()
}

func (a *App) withDeadline(step func(context.Context) error, event, code string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := step(ctx); err != nil {
		a.logger.LogAttrs(ctx, slog.LevelWarn, event, logging.ErrorAttrs(err, code)...)
	}
}

func (a *App) close(ctx context.Context) error {
	var failures []error
	if a.metricsListener != nil {
		_ = a.metricsListener.Close()
		a.metricsListener = nil
	}
	if a.listener != nil {
		_ = a.listener.Close()
		a.listener = nil
	}
	if a.history != nil {
		failures = append(failures, a.history.CloseContext(ctx))
		a.history = nil
	}
	if a.reader != nil {
		failures = append(failures, a.reader.Close())
		a.reader = nil
	}
	if a.db != nil {
		failures = append(failures, a.db.Close())
		a.db = nil
	}
	if a.lock != nil {
		failures = append(failures, a.lock.Close())
		a.lock = nil
	}
	return errors.Join(failures...)
}

func newHTTPServer(address string, handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ErrorLog:          log.New(httpErrorWriter{logger}, "", 0),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
}

// net/http 会输出自由格式的传输层错误。
// 将其与应用错误一样，限制在仅记录元数据的边界内。
type httpErrorWriter struct{ logger *slog.Logger }

func (w httpErrorWriter) Write(p []byte) (int, error) {
	w.logger.Warn("http_transport_error", "component", "http_server", "error_kind", "http", "error_code", "transport_error")
	return len(p), nil
}
