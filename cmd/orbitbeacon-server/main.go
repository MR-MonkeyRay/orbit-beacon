package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"orbitbeacon/internal/buildinfo"
	"orbitbeacon/internal/bundle"
	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/backup"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/notify"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "orbitbeacon-server:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("expected subcommand: serve, restore, version, health or verify-bundle")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(os.Stdout, "Usage: orbitbeacon-server {serve|restore|version|health|verify-bundle} --help")
		return nil
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	var configPath, dataDir, input string
	var bundleDir, publicKey string
	var healthURL, healthCA string
	var passwordFile string
	var replace bool
	var asJSON bool
	if args[0] != "version" && args[0] != "health" && args[0] != "verify-bundle" {
		if args[0] != "restore" {
			fs.StringVar(&configPath, "config", "", "JSON configuration file")
		}
		fs.StringVar(&dataDir, "data-dir", "", "persistent local data directory")
	}
	switch args[0] {
	case "serve":
		fs.String("listen-addr", "", "HTTP listen address (default 127.0.0.1:8080)")
		fs.String("metrics-listen", "", "optional separate Prometheus metrics listen address")
		fs.Bool("access-log", false, "write one structured access log entry per request")
		fs.Bool("allow-insecure-webhooks", false, "permit HTTP webhook URLs on literal loopback only")
		fs.Bool("notification-allow-private-networks", false, "allow notification egress to private networks")
		fs.String("public-url", "", "public HTTP or HTTPS URL (default http://localhost:8080); used for browser origin validation")
		fs.Int("max-nodes", 500, "positive node capacity, including pending and disabled nodes")
		fs.Int("web-update-interval-seconds", 10, "browser update cadence in seconds (2-60, restart to apply)")
		fs.String("trusted-proxies", "", "comma-separated trusted proxy IP addresses or CIDR prefixes")
	case "restore":
		fs.StringVar(&input, "input", "", "source restore archive")
		fs.BoolVar(&replace, "replace", false, "replace selected categories in an existing data directory")
		fs.StringVar(&passwordFile, "password-file", "", "owned 0600 file containing the archive password")
	case "health":
		fs.StringVar(&healthURL, "url", "", "server HTTP or HTTPS origin")
		fs.StringVar(&healthCA, "ca-file", "", "optional CA PEM bundle")
	case "version":
		fs.BoolVar(&asJSON, "json", false, "print machine-readable build information")
	case "verify-bundle":
		fs.StringVar(&bundleDir, "directory", "", "signed bundle directory")
		fs.StringVar(&publicKey, "public-key", "", "Ed25519 public key PEM or raw file")
	default:
		return fmt.Errorf("unknown subcommand %q; expected serve, restore, version, health or verify-bundle", args[0])
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional argument %q", fs.Arg(0))
	}
	if args[0] == "version" {
		info := buildinfo.Current()
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(info)
		}
		fmt.Fprintf(os.Stdout, "OrbitBeacon server %s (%s/%s, commit %s, built %s, %s)\n", info.Version, info.OS, info.Arch, info.Commit, info.BuildDate, info.GoVersion)
		return nil
	}
	if args[0] == "health" {
		return checkHealth(healthURL, healthCA)
	}
	if args[0] == "verify-bundle" {
		manifest, err := bundle.VerifyBundle(bundleDir, publicKey)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "verified bundle %s with %d artifacts\n", manifest.Version, len(manifest.Artifacts))
		return nil
	}
	if args[0] == "restore" {
		if dataDir == "" || input == "" {
			return errors.New("restore requires --data-dir and --input")
		}
		password, err := readRestorePassword(passwordFile)
		if err != nil {
			return err
		}
		result, err := backup.Restore(context.Background(), dataDir, input, password, replace)
		password = ""
		if err != nil {
			var detail *backup.DependencyConflict
			if errors.As(err, &detail) {
				return fmt.Errorf("restore_dependency_conflict: %s", detail)
			}
			return err
		}
		fmt.Fprintln(os.Stdout, "Restored archive", result.ArchiveID)
		fmt.Fprintln(os.Stdout, "Rollback files:", result.RollbackPath)
		return nil
	}
	flags := make(map[string]string)
	fs.Visit(func(f *flag.Flag) {
		if f.Name != "config" {
			flags[strings.ReplaceAll(f.Name, "-", "_")] = f.Value.String()
		}
	})
	cfg, err := config.LoadServer(config.LoadOptions{File: configPath, Flags: flags})
	if err != nil {
		return err
	}
	dataLock, err := datadir.Lock(cfg.DataDir)
	if err != nil {
		return err
	}
	defer dataLock.Close()
	if err := backup.RecoverRestore(cfg.DataDir); err != nil {
		return err
	}
	if err := datadir.Prepare(cfg.DataDir); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	databasePath := filepath.Join(cfg.DataDir, "orbitbeacon.db")
	db, err := storage.Open(ctx, databasePath)
	if err != nil {
		return err
	}
	defer db.Close()
	authService := auth.NewService(db, auth.Options{})
	if err := initializeAdministrator(ctx, db, authService, os.LookupEnv); err != nil {
		return err
	}
	publicReader, err := storage.OpenReader(ctx, databasePath)
	if err != nil {
		return err
	}
	defer publicReader.Close()
	nodeService := nodes.NewService(db, nodes.Options{MaxNodes: cfg.MaxNodes})
	nodeService.SetPublicReader(publicReader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	metricHistory := openMetricHistory(ctx, cfg.DataDir, publicReader, logger)
	if metricHistory != nil {
		defer metricHistory.Close()
	}
	ingestOptions := ingest.Options{}
	if metricHistory != nil {
		ingestOptions.Samples = metricHistory.Samples()
	}
	ingestService := ingest.NewService(db, ingestOptions)
	key, err := datadir.LoadKey(filepath.Join(cfg.DataDir, "secrets.key"))
	if err != nil {
		return err
	}
	box, err := secretbox.New(key, "orbitbeacon notification channel v1")
	if err != nil {
		clear(key)
		return err
	}
	backupBox, err := secretbox.New(key, "orbitbeacon backup secrets v1")
	clear(key)
	if err != nil {
		return err
	}
	backups, err := backup.New(db, backup.Options{DataDir: cfg.DataDir, SecretBox: backupBox, ServerVersion: buildinfo.Current().Version, Uploader: backup.NewObjectUploader()})
	if err != nil {
		return err
	}
	notifications := notify.New(db, box, notify.Options{AllowInsecureWebhooks: cfg.AllowInsecureWebhooks, AllowPrivateNetworks: cfg.NotificationAllowPrivateNetworks})
	alertService := alerts.NewService(db, nil)
	evaluator := alerts.NewEvaluator(alertService, ingestService, alerts.EvaluatorOptions{Logger: logger, Notifier: notifications})
	handler, err := server.NewHandler(server.Options{Backups: backups, Notifications: notifications, Alerts: alertService, Ingest: ingestService, History: metricHistory, Auth: authService, Nodes: nodeService, TrustedProxies: cfg.TrustedProxies, PublicURL: cfg.PublicURL, WebUpdateIntervalSeconds: cfg.WebUpdateIntervalSeconds, Logger: logger, AccessLog: cfg.AccessLog, BuildVersion: buildinfo.Current().Version})
	if err != nil {
		return err
	}
	httpServer := newHTTPServer(cfg.ListenAddr, handler, logger)
	httpServer.RegisterOnShutdown(handler.CloseLiveStreams)
	var metricsServer *http.Server
	if cfg.MetricsListen != "" {
		metricsServer = newHTTPServer(cfg.MetricsListen, handler.MetricsHandler(), logger)
	}
	mainListener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	defer mainListener.Close()
	var metricsListener net.Listener
	if metricsServer != nil {
		metricsListener, err = net.Listen("tcp", cfg.MetricsListen)
		if err != nil {
			return err
		}
		defer metricsListener.Close()
	}
	// Background work must finish before run returns and closes the database.
	var background sync.WaitGroup
	background.Add(1)
	go func() { defer background.Done(); backups.Run(ctx) }()
	background.Add(1)
	go func() { defer background.Done(); notifications.Run(ctx) }()
	background.Add(1)
	go func() { defer background.Done(); evaluator.Run(ctx) }()
	background.Add(1)
	go func() {
		defer background.Done()
		ticker := time.NewTicker(server.RetentionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				result, err := handler.CleanupRetention(ctx, now)
				if err != nil {
					logger.Warn("retention cleanup failed", "error", err)
					continue
				}
				if result.NotificationDeliveries != 0 || result.AlertEvents != 0 || result.AuditEvents != 0 || result.EnrollmentTickets != 0 || result.Sessions != 0 || result.HistoryBuckets != 0 {
					logger.Info("retention cleanup complete", "notification_deliveries", result.NotificationDeliveries, "alert_events", result.AlertEvents, "audit_events", result.AuditEvents, "enrollment_tickets", result.EnrollmentTickets, "sessions", result.Sessions, "history_buckets", result.HistoryBuckets, "traffic_rows", result.TrafficRows)
				}
			}
		}
	}()
	// One goroutine owns HTTP shutdown. It runs on a signal, and also after a
	// listen or serve error because run cancels the context below.
	httpStopped := make(chan struct{})
	go func() {
		defer close(httpStopped)
		<-ctx.Done()
		// Report not-ready first so load balancers stop routing new traffic.
		handler.MarkNotReady()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		handler.CloseLiveStreams()
		var shutdown sync.WaitGroup
		shutdown.Add(1)
		go func() {
			defer shutdown.Done()
			if err := handler.WaitLiveStreams(shutdownCtx); err != nil {
				logger.Warn("live streams did not drain before HTTP shutdown", "error_kind", fmt.Sprintf("%T", err))
			}
		}()
		for _, current := range []*http.Server{httpServer, metricsServer} {
			if current == nil {
				continue
			}
			shutdown.Add(1)
			go func(server *http.Server) {
				defer shutdown.Done()
				if err := server.Shutdown(shutdownCtx); err != nil {
					_ = server.Close()
				}
			}(current)
		}
		shutdown.Wait()
	}()
	fmt.Fprintln(os.Stderr, "Serving OrbitBeacon on", cfg.ListenAddr)
	serveErrors := make(chan error, 2)
	serverCount := 1
	if metricsServer != nil {
		serverCount++
		fmt.Fprintln(os.Stderr, "Serving OrbitBeacon metrics on", cfg.MetricsListen)
		go func() { serveErrors <- metricsServer.Serve(metricsListener) }()
	}
	go func() {
		serveErrors <- httpServer.Serve(mainListener)
	}()
	err = <-serveErrors
	// ListenAndServe returns as soon as Shutdown starts, or on a listen error.
	// Stop both listeners and wait for HTTP handlers and tracked live queries,
	// then drain ingestion before the deferred database close.
	stop()
	<-httpStopped
	for range serverCount - 1 {
		<-serveErrors
	}
	// HTTP/live shutdown (10s) already waited for report handlers and streams,
	// so draining stays inside the 30s stop grace in deploy/.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
	if drainErr := handler.Shutdown(drainCtx); drainErr != nil {
		logger.Warn("report ingestion did not drain before shutdown", "error_kind", fmt.Sprintf("%T", drainErr))
	}
	cancelDrain()

	notifyCtx, cancelNotify := context.WithTimeout(context.Background(), 5*time.Second)
	if notifyErr := notifications.Shutdown(notifyCtx); notifyErr != nil {
		logger.Warn("notification shutdown deadline reached", "error_code", "shutdown_timeout")
	}
	cancelNotify()
	background.Wait()
	if metricHistory != nil {
		historyCtx, cancelHistory := context.WithTimeout(context.Background(), 5*time.Second)
		if flushErr := metricHistory.Shutdown(historyCtx); flushErr != nil {
			logger.Warn("metric history shutdown flush failed", "error", flushErr)
		}
		cancelHistory()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func initializeAdministrator(ctx context.Context, db *sql.DB, service *auth.Service, lookupEnv func(string) (string, bool)) error {
	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM users)").Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	username, set := lookupEnv("ORBITBEACON_ADMIN_USERNAME")
	if !set {
		username = "orbitbeacon"
	}
	password, set := lookupEnv("ORBITBEACON_ADMIN_PASSWORD")
	if !set {
		password = "orbitbeacon"
	}
	_, err := service.BootstrapAdministrator(ctx, username, password)
	if errors.Is(err, auth.ErrAdminExists) {
		return nil
	}
	return err
}

func newHTTPServer(address string, handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
}

func checkHealth(rawURL, caFile string) error {
	if rawURL == "" {
		return errors.New("health requires --url")
	}
	request, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("health URL: %w", err)
	}
	if err := config.ValidateServerURL(rawURL); err != nil {
		return fmt.Errorf("health URL: %w", err)
	}
	request.URL.Path = "/healthz/ready"
	if request.URL.Scheme == "http" && caFile != "" {
		return errors.New("health CA requires an HTTPS URL")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	if caFile != "" {
		data, err := os.ReadFile(caFile)
		if err != nil {
			return fmt.Errorf("read health CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return errors.New("health CA contains no certificates")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	client := &http.Client{
		Timeout:       3 * time.Second,
		Transport:     transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("health request: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("server is not ready: %s", response.Status)
	}
	fmt.Fprintln(os.Stdout, "ready")
	return nil
}
