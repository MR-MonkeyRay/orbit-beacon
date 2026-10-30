package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"orbitbeacon/internal/buildinfo"
	"orbitbeacon/internal/bundle"
	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/app"
	"orbitbeacon/internal/server/backup"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := runWithLogger(os.Args[1:], logger); err != nil {
		// Command errors are operator-facing CLI output, not structured logs.
		fmt.Fprintln(os.Stderr, "orbitbeacon-server:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	return runWithLogger(args, slog.New(slog.NewJSONHandler(os.Stderr, nil)))
}

func runWithLogger(args []string, logger *slog.Logger) error {
	if len(args) == 0 {
		return errors.New("expected subcommand: serve, restore, version, health or verify-bundle")
	}
	switch args[0] {
	case "help", "--help", "-h":
		fmt.Fprintln(os.Stdout, "Usage: orbitbeacon-server {serve|restore|version|health|verify-bundle} --help")
		return nil
	case "serve":
		return runServe(args[1:], logger)
	case "restore":
		return runRestore(args[1:])
	case "version":
		return runVersion(args[1:])
	case "health":
		return runHealth(args[1:])
	case "verify-bundle":
		return runVerifyBundle(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q; expected serve, restore, version, health or verify-bundle", args[0])
	}
}

func parseCommand(fs *flag.FlagSet, args []string) (bool, error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, nil
		}
		return false, err
	}
	if fs.NArg() != 0 {
		return false, fmt.Errorf("unexpected positional argument %q", fs.Arg(0))
	}
	return true, nil
}

func runServe(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "", "JSON configuration file")
	fs.String("data-dir", "", "persistent local data directory")
	fs.String("listen-addr", "127.0.0.1:8080", "HTTP listen address")
	fs.String("metrics-listen", "", "optional separate Prometheus metrics listen address")
	fs.Bool("access-log", false, "write one structured access log entry per request")
	fs.Bool("allow-insecure-webhooks", false, "permit HTTP webhook URLs on literal loopback only")
	fs.Bool("notification-allow-private-networks", false, "allow notification egress to private networks")
	fs.String("public-url", "http://localhost:8080", "public HTTP or HTTPS URL used for browser origin validation")
	fs.Int("max-nodes", 500, "positive node capacity, including pending and disabled nodes")
	fs.Int("web-update-interval-seconds", 10, "browser update cadence in seconds (2-60, restart to apply)")
	fs.String("trusted-proxies", "", "comma-separated trusted proxy IP addresses or CIDR prefixes")
	if ok, err := parseCommand(fs, args); !ok {
		return err
	}
	flags := make(map[string]string)
	fs.Visit(func(f *flag.Flag) {
		if f.Name != "config" {
			flags[strings.ReplaceAll(f.Name, "-", "_")] = f.Value.String()
		}
	})
	cfg, err := config.LoadServer(config.LoadOptions{File: *configPath, Flags: flags})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	application, err := app.Build(cfg, logger, ctx)
	if err != nil {
		return err
	}
	return application.Run(ctx)
}

func runVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print machine-readable build information")
	if ok, err := parseCommand(fs, args); !ok {
		return err
	}
	info := buildinfo.Current()
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(info)
	}
	fmt.Fprintf(os.Stdout, "OrbitBeacon server %s (%s/%s, commit %s, built %s, %s)\n", info.Version, info.OS, info.Arch, info.Commit, info.BuildDate, info.GoVersion)
	return nil
}

func runHealth(args []string) error {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	url := fs.String("url", "", "server HTTP or HTTPS origin")
	ca := fs.String("ca-file", "", "optional CA PEM bundle")
	if ok, err := parseCommand(fs, args); !ok {
		return err
	}
	return checkHealth(*url, *ca)
}

func runVerifyBundle(args []string) error {
	fs := flag.NewFlagSet("verify-bundle", flag.ContinueOnError)
	directory := fs.String("directory", "", "signed bundle directory")
	key := fs.String("public-key", "", "Ed25519 public key PEM or raw file")
	if ok, err := parseCommand(fs, args); !ok {
		return err
	}
	manifest, err := bundle.VerifyBundle(*directory, *key)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "verified bundle %s with %d artifacts\n", manifest.Version, len(manifest.Artifacts))
	return nil
}

func runRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "persistent local data directory")
	input := fs.String("input", "", "source restore archive")
	replace := fs.Bool("replace", false, "replace selected categories in an existing data directory")
	passwordFile := fs.String("password-file", "", "owned 0600 file containing the archive password")
	if ok, err := parseCommand(fs, args); !ok {
		return err
	}
	if *dataDir == "" || *input == "" {
		return errors.New("restore requires --data-dir and --input")
	}
	password, err := readRestorePassword(*passwordFile)
	if err != nil {
		return err
	}
	result, err := backup.Restore(context.Background(), *dataDir, *input, password, *replace)
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
