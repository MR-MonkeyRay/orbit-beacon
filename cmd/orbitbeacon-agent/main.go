package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"orbitbeacon/internal/agent/filelock"
	"orbitbeacon/internal/agent/service"
	"orbitbeacon/internal/buildinfo"
	"orbitbeacon/internal/config"
	v1 "orbitbeacon/internal/protocol/v1"
)

const exitIdentityConflict = 3

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var failure *service.HTTPError
	if errors.As(err, &failure) && failure.StatusCode == 409 && failure.Code == "identity_conflict" {
		return exitIdentityConflict
	}
	return 1
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "orbitbeacon-agent:", err)
		os.Exit(exitCode(err))
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("expected subcommand: run, enroll, doctor or version")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(os.Stdout, "Usage: orbitbeacon-agent {run|enroll|doctor|version} --help")
		return nil
	}
	var reload <-chan struct{}
	if args[0] == "run" {
		var stopReload func()
		reload, stopReload = reloadSignals()
		defer stopReload()
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	var file, policyPath, tokenFile string
	runtimeFlags := make(map[string]*string)
	var asJSON bool
	switch args[0] {
	case "version":
		fs.BoolVar(&asJSON, "json", false, "print machine-readable build information")
	case "run", "enroll", "doctor":
		fs.StringVar(&file, "config", "", "agent JSON configuration file")
		fs.StringVar(&policyPath, "install-policy", "", "administrator-owned immutable install policy JSON file")
		for _, key := range []string{"report_interval_seconds", "disk_interval_seconds", "interfaces", "disk_paths", "probe_targets"} {
			runtimeFlags[key] = fs.String(strings.ReplaceAll(key, "_", "-"), "", "local runtime override; lists use a JSON array of strings")
		}
		if args[0] == "enroll" {
			fs.StringVar(&tokenFile, "token-file", "", "optional enrollment token file; omit to recover saved node credentials")
		}
		if args[0] == "doctor" {
			fs.BoolVar(&asJSON, "json", false, "print machine-readable diagnostics")
		}
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
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
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(buildinfo.Current())
		}
		info := buildinfo.Current()
		fmt.Fprintf(os.Stdout, "OrbitBeacon agent %s (%s/%s, commit %s, built %s, %s)\n", info.Version, info.OS, info.Arch, info.Commit, info.BuildDate, info.GoVersion)
		return nil
	}
	if policyPath == "" {
		return errors.New("--install-policy is required; trust policy is never inferred")
	}
	policy, err := config.ReadInstallPolicy(policyPath, 0)
	if err != nil {
		return err
	}
	flags := make(map[string]string)
	fs.Visit(func(f *flag.Flag) {
		key := strings.ReplaceAll(f.Name, "-", "_")
		if value, ok := runtimeFlags[key]; ok {
			flags[key] = *value
		}
	})
	loadAgent, err := agentLoader(file, flags, policy)
	if err != nil {
		return err
	}
	cfg, err := loadAgent()
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return errors.New("runtime_identity_unsupported: Windows account enforcement is not implemented; no runtime command was performed")
	}
	if os.Geteuid() == 0 || os.Getegid() == 0 || os.Geteuid() != cfg.ExpectedUID || os.Getegid() != cfg.ExpectedGID {
		return errors.New("runtime_identity_mismatch: run as the non-root UID/GID fixed by the install policy")
	}
	if cfg.ExpectedAccount != "" {
		account, err := user.LookupId(fmt.Sprint(os.Geteuid()))
		if err != nil || account.Username != cfg.ExpectedAccount {
			return errors.New("runtime_identity_mismatch: account differs from install policy")
		}
	}
	if err := os.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin"); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if args[0] == "run" {
		unlock, err := lockRun(ctx, cfg.StateDir)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		defer unlock()
	}
	agent, err := service.OpenRuntime(ctx, cfg)
	if err != nil {
		if args[0] == "run" && errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	defer agent.Close()
	switch args[0] {
	case "run":
		err := agent.Run(ctx, reload, func() (v1.RuntimeSettings, error) {
			next, err := loadAgent()
			return next.RuntimeSettings, err
		})
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	case "enroll":
		requestCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		result, err := runEnrollment(requestCtx, tokenFile, agent.Enroll)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "Enrolled node %s\n", result.NodeID)
		return nil
	case "doctor":
		result, checkErr := agent.Doctor(ctx)
		if asJSON {
			if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(os.Stdout, "Configuration: %s\nIdentity file: %s\nConnectivity: %s\nNode authentication: %s\nCollection: %s\nInstall mode: %s\n", result.Configuration, result.Identity, result.Connectivity, result.Authentication, result.Collection, result.InstallMode)
			for metric, status := range result.MetricStatus {
				fmt.Fprintf(os.Stdout, "%s: %s\n", metric, status)
			}
		}
		return checkErr
	}
	return errors.New("unsupported agent command")
}

// Capture the verified policy, environment and explicit flags once. Reload only
// reopens the same local JSON file; trust roots cannot change with the process env.
func agentLoader(file string, flags map[string]string, policy config.InstallPolicy) (func() (config.Agent, error), error) {
	if file != "" {
		var err error
		file, err = filepath.Abs(file)
		if err != nil {
			return nil, err
		}
	}
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, "ORBITBEACON_") {
			environment[key] = value
		}
	}
	overrides := make(map[string]string, len(flags))
	for key, value := range flags {
		overrides[key] = value
	}
	opts := config.LoadOptions{File: file, Flags: overrides, LookupEnv: func(key string) (string, bool) {
		value, ok := environment[key]
		return value, ok
	}}
	return func() (config.Agent, error) { return config.LoadAgent(opts, &policy) }, nil
}

func lockRun(ctx context.Context, stateDir string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, errors.New("cannot prepare agent state directory for run lock")
	}
	return filelock.TryAcquire(ctx, filepath.Join(stateDir, "run.lock"))
}

// runEnrollment preserves the ticket on every unconfirmed outcome. Cleanup errors
// return the confirmed response as well, so callers can distinguish local cleanup
// failure from an enrollment that did not commit.
func runEnrollment(ctx context.Context, tokenFile string, enroll func(context.Context, string) (v1.EnrollmentResponse, error)) (v1.EnrollmentResponse, error) {
	if tokenFile == "" {
		return enroll(ctx, "")
	}
	ticket, err := openEnrollmentTicket(tokenFile)
	if err != nil {
		return v1.EnrollmentResponse{}, err
	}
	defer ticket.close()
	result, err := enroll(ctx, ticket.token)
	if err != nil {
		return result, err
	}
	if result.NodeID == "" {
		return result, errors.New("invalid enrollment response; enrollment ticket retained")
	}
	if err := ticket.remove(); err != nil {
		return result, errors.New("enrollment succeeded but enrollment ticket cleanup failed; verify the original directory and securely remove any remaining ticket or .orbitbeacon-ticket-* recovery files")
	}
	return result, nil
}
