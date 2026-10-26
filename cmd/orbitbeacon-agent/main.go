package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"

	"orbitbeacon/internal/config"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "orbitbeacon-agent:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("expected subcommand: run, enroll, doctor, version or update")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(os.Stdout, "Usage: orbitbeacon-agent {run|enroll|doctor|version|update} --help")
		return nil
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	var file, policyPath, tokenFile, targetVersion string
	var asJSON bool
	switch args[0] {
	case "version":
		fs.BoolVar(&asJSON, "json", false, "print machine-readable build information")
	case "run", "enroll", "doctor", "update":
		fs.StringVar(&file, "config", "", "agent JSON configuration file")
		fs.StringVar(&policyPath, "install-policy", "", "administrator-owned immutable install policy JSON file")
		if args[0] == "enroll" {
			fs.StringVar(&tokenFile, "token-file", "", "enrollment token file; never pass tokens on the command line")
		}
		if args[0] == "doctor" {
			fs.BoolVar(&asJSON, "json", false, "print machine-readable diagnostics")
		}
		if args[0] == "update" {
			fs.StringVar(&targetVersion, "version", "", "signed release version")
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
			return json.NewEncoder(os.Stdout).Encode(struct {
				Version string `json:"version"`
				OS      string `json:"os"`
				Arch    string `json:"arch"`
			}{Version: "development", OS: runtime.GOOS, Arch: runtime.GOARCH})
		}
		fmt.Fprintf(os.Stdout, "OrbitBeacon agent development (%s/%s)\n", runtime.GOOS, runtime.GOARCH)
		return nil
	}
	if args[0] == "enroll" && tokenFile == "" {
		return errors.New("enroll requires --token-file")
	}
	if args[0] == "update" && targetVersion == "" {
		return errors.New("update requires --version")
	}
	if policyPath == "" {
		return errors.New("--install-policy is required; this foundation does not install or infer a trust policy")
	}
	info, err := os.Lstat(policyPath)
	if err != nil {
		return fmt.Errorf("read install policy: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return errors.New("install policy must be a regular file not writable by group or others")
	}
	data, err := os.ReadFile(policyPath)
	if err != nil {
		return err
	}
	var policy config.InstallPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		return fmt.Errorf("decode install policy: %w", err)
	}
	cfg, err := config.LoadAgent(config.LoadOptions{File: file}, &policy)
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if os.Geteuid() == 0 || os.Getegid() == 0 || os.Geteuid() != cfg.ExpectedUID || os.Getegid() != cfg.ExpectedGID {
			return errors.New("runtime_identity_mismatch: run as the non-root UID/GID fixed by the install policy")
		}
	}
	if args[0] == "doctor" {
		if asJSON {
			if err := json.NewEncoder(os.Stdout).Encode(struct {
				Configuration string `json:"configuration"`
				RuntimeChecks string `json:"runtime_checks"`
			}{Configuration: "valid", RuntimeChecks: "not_available"}); err != nil {
				return err
			}
		}
		return errors.New("doctor runtime checks are not yet available; only configuration and POSIX identity validation completed")
	}
	return fmt.Errorf("%s is not yet available in the foundation slice; no enrollment, monitoring or update was performed", args[0])
}
