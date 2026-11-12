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
	"strings"
	"sync"
	"time"

	"orbitbeacon/internal/buildinfo"
	"orbitbeacon/internal/config"
	"orbitbeacon/internal/logging"
	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/availability"
	"orbitbeacon/internal/server/backup"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/notify"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/securefs"
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
	db                        io.Closer
	replication               *storage.Databases
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
	if err := backup.RecoverRestore(ctx, cfg); err != nil {
		return nil, err
	}
	if err := datadir.ValidateRestore(cfg.DataDir); err != nil {
		return nil, err
	}
	databases, openErr := storage.OpenDatabases(ctx, cfg.Database, cfg.DataDir)
	if openErr != nil {
		if databases == nil || databases.Main == nil || cfg.Database.Primary.Driver == "mysql" {
			return nil, openErr
		}
		logger.LogAttrs(ctx, slog.LevelWarn, "history_disabled", logging.ErrorAttrs(openErr, "history_open_failed")...)
	}
	a.db = databases
	a.replication = databases
	db := databases.Main
	establishedHA := cfg.Database.Standby != nil && !databases.BootstrapRequired()
	if err := prepareDatabaseKey(ctx, cfg.DataDir, db, establishedHA); err != nil {
		return nil, err
	}
	nodeService := nodes.NewService(db, nodes.Options{MaxNodes: cfg.MaxNodes})

	var saturation *server.SaturationMetrics
	var observeFlush func(time.Duration, int)
	if cfg.MetricsListen != "" {
		saturation = server.NewSaturationMetrics(db, cfg.DataDir)
		observeFlush = saturation.ObserveHistoryFlush
	}
	metricHistory := openMetricHistory(db, databases.History, logger, observeFlush)
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
	accountBox, err := secretbox.New(key, "orbitbeacon account secrets v1")
	if err != nil {
		clear(key)
		return nil, err
	}
	authService := auth.NewService(db, auth.Options{SecretBox: accountBox})
	if db.Ready() && databases.BootstrapRequired() {
		if err := authService.InitializeAdministrator(ctx, os.LookupEnv); err != nil {
			clear(key)
			return nil, err
		}
		if err := databases.CompleteBootstrap(ctx); err != nil {
			clear(key)
			return nil, err
		}
	}
	box, err := secretbox.New(key, "orbitbeacon notification channel v1")
	if err != nil {
		clear(key)
		return nil, err
	}
	backupBox, err := secretbox.New(key, "orbitbeacon backup secrets v1")
	if err != nil {
		clear(key)
		return nil, err
	}
	notificationOptions := notify.Options{Logger: logger, AllowInsecureWebhooks: cfg.AllowInsecureWebhooks, AllowPrivateNetworks: cfg.NotificationAllowPrivateNetworks}
	if cfg.Database.Standby != nil {
		notificationOptions.StorageBox, err = secretbox.New(key, "orbitbeacon storage notifications v1")
		if err != nil {
			clear(key)
			return nil, err
		}
		notificationOptions.Storage = databases
	}
	clear(key)
	backupOptions := backup.Options{DataDir: cfg.DataDir, SecretBox: backupBox, ServerVersion: version, Uploader: backup.NewObjectUploader()}
	if metricHistory != nil {
		backupOptions.History = metricHistory.Database()
	}
	backups, err := backup.New(db, backupOptions)
	if err != nil {
		return nil, err
	}
	notifications := notify.New(db, box, notificationOptions)
	alertService := alerts.NewService(db, nil)
	trafficService := traffic.NewService(db)
	availabilityService := availability.NewService(db, nil, logger, ingestService.LiveReceivedAt)
	evaluator := alerts.NewEvaluator(alertService, ingestService, alerts.EvaluatorOptions{Logger: logger, Notifier: notifications, Quota: trafficService.Quota})
	enrollments := enrollment.NewService(db, enrollment.Options{MaxNodes: cfg.MaxNodes})
	enrollments.OnIdentityChange(func(replaced bool) {
		if replaced {
			nodeService.InvalidatePublicSafety()
		} else {
			nodeService.InvalidatePublic()
		}
	})
	if cfg.GeoIPTokenFile != "" {
		file, err := securefs.OpenFile(cfg.GeoIPTokenFile, os.O_RDONLY, 0)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return nil, err
		}
		if info.Mode().Perm() != 0600 {
			file.Close()
			return nil, errors.New("geoip token file requires 0600")
		}
		data, err := io.ReadAll(io.LimitReader(file, 4097))
		file.Close()
		if err != nil {
			return nil, err
		}
		if len(data) > 4096 {
			return nil, errors.New("geoip token file too large")
		}
		cfg.GeoIPToken = strings.TrimSpace(string(data))
		clear(data)
	}
	handler, err := server.NewHandler(server.Options{GeoIPLookup: cfg.GeoIPLookup, GeoIPToken: cfg.GeoIPToken, Storage: databases, RecoverUnavailableMain: cfg.Database.Standby != nil, Management: authService.Management(), Enrollments: enrollments, Traffic: trafficService, Availability: availabilityService, Inbox: inbox.New(db), Backups: backups, Notifications: notifications, Alerts: alertService, Ingest: ingestService, History: metricHistory, Auth: authService, Nodes: nodeService, TrustedProxies: cfg.TrustedProxies, PublicURL: cfg.PublicURL, WebUpdateIntervalSeconds: cfg.WebUpdateIntervalSeconds, Logger: logger, AccessLog: cfg.AccessLog, BuildVersion: version, SaturationMetrics: saturation})
	if err != nil {
		return nil, err
	}
	databases.SetMainSwitchHooks(storage.SwitchHooks{
		Prepare: handler.PrepareDatabaseSwitch,
		Publish: func(ctx context.Context, lease *storage.Lease) error {
			if databases.BootstrapRequired() {
				if err := authService.InitializeAdministrator(lease.Context(), os.LookupEnv); err != nil {
					return err
				}
				if err := databases.CompleteBootstrap(lease.Context()); err != nil {
					return err
				}
			}
			if err := notifications.SyncStorageSnapshot(lease.Context()); err != nil {
				return err
			}
			return handler.PublishDatabaseGeneration(ctx, lease)
		},
	})
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
	a.background = []func(context.Context){availabilityService.Run, backups.Run, notifications.Run, evaluator.Run, handler.RunRetention, handler.RunGeo}
	if cfg.Database.Standby != nil {
		a.background = append(a.background, notifications.RunStorage)
	}
	return a, nil
}

func (a *App) Run(ctx context.Context) error {
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	if a.replication != nil {
		if err := a.replication.StartReplication(runCtx); err != nil {
			stop()
			a.shutdown(&sync.WaitGroup{})
			return err
		}
	}
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
// HTTP 与实时流共用 10 秒，后续组件各有独立关闭时限；
// 复制必须实际结束后才能关闭历史库和连接池。
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
	if err := a.stopReplication(); err != nil {
		a.logger.LogAttrs(context.Background(), slog.LevelError, "replication_stop_failed", logging.ErrorAttrs(err, "close_failed")...)
	}
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
	failures = append(failures, a.stopReplication())
	if a.history != nil {
		failures = append(failures, a.history.CloseContext(ctx))
		a.history = nil
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

func (a *App) stopReplication() error {
	if a.replication == nil {
		return nil
	}
	databases := a.replication
	a.replication = nil
	return databases.StopReplication()
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
