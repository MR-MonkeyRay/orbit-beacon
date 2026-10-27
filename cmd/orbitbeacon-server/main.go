package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"orbitbeacon/internal/buildinfo"
	"orbitbeacon/internal/bundle"
	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/nodes"
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
		return errors.New("expected subcommand: serve, bootstrap, backup, restore, version, health or verify-bundle")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(os.Stdout, "Usage: orbitbeacon-server {serve|bootstrap|backup|restore|version|health|verify-bundle} --help")
		return nil
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	var configPath, dataDir, output, input, username string
	var bundleDir, publicKey string
	var healthURL, healthCA string
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
		fs.String("public-url", "", "public HTTPS URL; required for browser origin validation")
		fs.String("tls-cert-file", "", "optional TLS certificate PEM")
		fs.String("tls-key-file", "", "optional TLS private key PEM")
		fs.Int("max-nodes", 500, "positive node capacity, including pending and disabled nodes")
	case "bootstrap":
		fs.StringVar(&username, "username", "", "first administrator username; prompted if omitted")
	case "backup":
		fs.StringVar(&output, "output", "", "destination backup archive")
	case "restore":
		fs.StringVar(&input, "input", "", "source restore archive")
	case "health":
		fs.StringVar(&healthURL, "url", "", "server HTTPS origin")
		fs.StringVar(&healthCA, "ca-file", "", "optional CA PEM bundle")
	case "version":
		fs.BoolVar(&asJSON, "json", false, "print machine-readable build information")
	case "verify-bundle":
		fs.StringVar(&bundleDir, "directory", "", "signed bundle directory")
		fs.StringVar(&publicKey, "public-key", "", "Ed25519 public key PEM or raw file")
	default:
		return fmt.Errorf("unknown subcommand %q; expected serve, bootstrap, backup, restore, version, health or verify-bundle", args[0])
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
	if args[0] == "backup" {
		if output == "" {
			return errors.New("backup requires --output")
		}
		toolFlags := make(map[string]string)
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "data-dir" {
				toolFlags["data_dir"] = f.Value.String()
			}
		})
		cfg, err := config.LoadServer(config.LoadOptions{File: configPath, Flags: toolFlags})
		if err != nil {
			return err
		}
		return backupDatabase(cfg.DataDir, output)
	}
	if args[0] == "restore" {
		if dataDir == "" || input == "" {
			return errors.New("restore requires --data-dir and --input")
		}
		return restoreDatabase(dataDir, input)
	}
	flags := make(map[string]string)
	fs.Visit(func(f *flag.Flag) {
		if f.Name != "config" && f.Name != "username" {
			flags[strings.ReplaceAll(f.Name, "-", "_")] = f.Value.String()
		}
	})
	cfg, err := config.LoadServer(config.LoadOptions{File: configPath, Flags: flags})
	if err != nil {
		return err
	}
	dataLock, err := lockDataDir(cfg.DataDir)
	if err != nil {
		return err
	}
	defer dataLock.Close()
	if err := prepareDataDir(cfg.DataDir); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := storage.Open(ctx, filepath.Join(cfg.DataDir, "orbitbeacon.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	authService := auth.NewService(db, auth.Options{})
	if args[0] == "bootstrap" {
		reader := bufio.NewReader(io.LimitReader(os.Stdin, 4096))
		if username == "" {
			fmt.Fprint(os.Stderr, "Administrator username: ")
			line, err := reader.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			username = strings.TrimSpace(line)
		}
		fmt.Fprint(os.Stderr, "Administrator password (8–1024 UTF-8 bytes): ")
		var password []byte
		if term.IsTerminal(int(os.Stdin.Fd())) {
			password, err = term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
		} else {
			line, readErr := reader.ReadString('\n')
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
			password = []byte(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"))
		}
		if err != nil {
			return fmt.Errorf("read password: %w", err)
		}
		_, err = authService.BootstrapAdministrator(ctx, username, string(password))
		clear(password)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "First administrator created.")
		return nil
	}
	nodeService := nodes.NewService(db, nodes.Options{MaxNodes: cfg.MaxNodes})
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	handler, err := server.NewHandler(server.Options{Auth: authService, Nodes: nodeService, PublicURL: cfg.PublicURL, Logger: logger})
	if err != nil {
		return err
	}
	httpServer := &http.Server{Addr: cfg.ListenAddr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	go func() {
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
				if result.AuditEvents != 0 || result.EnrollmentTickets != 0 {
					logger.Info("retention cleanup complete", "audit_events", result.AuditEvents, "enrollment_tickets", result.EnrollmentTickets)
				}
			}
		}
	}()
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := httpServer.Shutdown(shutdownCtx); err != nil {
				_ = httpServer.Close()
			}
			_ = handler.Shutdown(shutdownCtx)
		case <-stopped:
		}
	}()
	fmt.Fprintln(os.Stderr, "Serving OrbitBeacon on", cfg.ListenAddr)
	if cfg.TLSCertFile != "" {
		err = httpServer.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	} else {
		err = httpServer.ListenAndServe()
	}
	close(stopped)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func checkHealth(rawURL, caFile string) error {
	if rawURL == "" {
		return errors.New("health requires --url")
	}
	if err := config.ValidateHTTPS(rawURL); err != nil {
		return fmt.Errorf("health URL: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
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
		Timeout:       5 * time.Second,
		Transport:     transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	request, err := http.NewRequest(http.MethodGet, strings.TrimRight(rawURL, "/")+"/healthz/ready", nil)
	if err != nil {
		return err
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
