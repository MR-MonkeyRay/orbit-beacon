package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

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
		return errors.New("expected subcommand: serve, bootstrap, backup, restore")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(os.Stdout, "Usage: orbitbeacon-server {serve|bootstrap|backup|restore} --help")
		return nil
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	var configPath, dataDir, output, input, username string
	fs.StringVar(&configPath, "config", "", "JSON configuration file")
	fs.StringVar(&dataDir, "data-dir", "", "persistent local data directory")
	switch args[0] {
	case "serve":
		fs.String("listen-addr", "", "HTTP listen address (default 127.0.0.1:8080)")
		fs.String("public-url", "", "public HTTPS URL; required for browser origin validation")
		fs.String("tls-cert-file", "", "optional TLS certificate PEM")
		fs.String("tls-key-file", "", "optional TLS private key PEM")
		fs.String("trusted-proxy-cidrs", "", "comma-separated trusted proxy CIDRs (forwarded headers are not consumed by this slice)")
		fs.Int("max-nodes", 500, "positive node capacity, including pending and disabled nodes")
	case "bootstrap":
		fs.StringVar(&username, "username", "", "first administrator username; prompted if omitted")
	case "backup":
		fs.StringVar(&output, "output", "", "destination backup archive")
	case "restore":
		fs.StringVar(&input, "input", "", "source backup archive")
	default:
		return fmt.Errorf("unknown subcommand %q; expected serve, bootstrap, backup or restore", args[0])
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
	if args[0] == "backup" {
		if output == "" {
			return errors.New("backup requires --output")
		}
		return errors.New("backup is not yet available; do not copy a live SQLite database file")
	}
	if args[0] == "restore" {
		if dataDir == "" || input == "" {
			return errors.New("restore requires --data-dir and --input")
		}
		return errors.New("restore is not yet available; no files have been changed")
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
		fmt.Fprint(os.Stderr, "Administrator password (at least 8 characters): ")
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
	handler, err := server.NewHandler(server.Options{Auth: authService, Nodes: nodeService, PublicURL: cfg.PublicURL})
	if err != nil {
		return err
	}
	httpServer := &http.Server{Addr: cfg.ListenAddr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := httpServer.Shutdown(shutdownCtx); err != nil {
				_ = httpServer.Close()
			}
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
