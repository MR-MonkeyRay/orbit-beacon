// orbitbeacon-mcp exposes only the read-only token HTTP API over MCP stdio.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"orbitbeacon/internal/buildinfo"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Inherited stdin may be a blocking descriptor that Close cannot
		// interrupt. Terminate even when idle or stdout is backpressured.
		os.Exit(0)
	}()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, in io.Reader, out, diagnostics io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		info := buildinfo.Current()
		fmt.Fprintf(out, "OrbitBeacon mcp %s (%s/%s, commit %s, built %s, %s)\n", info.Version, info.OS, info.Arch, info.Commit, info.BuildDate, info.GoVersion)
		return 0
	}
	if len(args) == 2 && args[0] == "version" && args[1] == "--json" {
		if json.NewEncoder(out).Encode(buildinfo.Current()) != nil {
			return 1
		}
		return 0
	}
	if len(args) != 0 {
		fmt.Fprintln(diagnostics, "usage: orbitbeacon-mcp [version [--json]]; configure credentials through environment variables")
		return 1
	}
	cfg, err := loadConfig(getenv)
	if err != nil {
		fmt.Fprintln(diagnostics, err)
		return 1
	}
	client := newAPIClient(cfg)
	defer client.http.CloseIdleConnections()
	if serve(ctx, in, out, client) != nil && ctx.Err() == nil {
		fmt.Fprintln(diagnostics, "MCP stdio I/O failed")
		return 1
	}
	return 0
}
