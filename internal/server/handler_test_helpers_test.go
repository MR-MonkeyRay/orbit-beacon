package server

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/traffic"
)

func testHandlerOptions(t *testing.T, opts Options) Options {
	t.Helper()
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	db := opts.Nodes.Database()
	if opts.Management == nil {
		opts.Management = opts.Auth.Management()
	}
	if opts.Enrollments == nil {
		opts.Enrollments = enrollment.NewService(db, enrollment.Options{MaxNodes: opts.Nodes.Capacity(), Now: now})
	}
	nodeService := opts.Nodes
	opts.Enrollments.OnIdentityChange(func(replacedActiveCredential bool) {
		if replacedActiveCredential {
			nodeService.InvalidatePublicSafety()
			return
		}
		nodeService.InvalidatePublic()
	})
	if opts.Ingest == nil {
		ingestOptions := ingest.Options{Now: now}
		if opts.History != nil {
			ingestOptions.Samples = opts.History.Samples()
		}
		opts.Ingest = ingest.NewService(db, ingestOptions)
	}
	if opts.Alerts == nil {
		opts.Alerts = alerts.NewService(db, now)
	}
	if opts.Traffic == nil {
		opts.Traffic = traffic.NewService(db)
	}
	if opts.Inbox == nil {
		opts.Inbox = inbox.New(db)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return opts
}

func TestNewHandlerRequiresApplicationServices(t *testing.T) {
	for _, missing := range []struct {
		name  string
		clear func(*Options)
	}{
		{"auth", func(opts *Options) { opts.Auth = nil }},
		{"nodes", func(opts *Options) { opts.Nodes = nil }},
		{"management", func(opts *Options) { opts.Management = nil }},
		{"enrollments", func(opts *Options) { opts.Enrollments = nil }},
		{"ingest", func(opts *Options) { opts.Ingest = nil }},
		{"alerts", func(opts *Options) { opts.Alerts = nil }},
		{"traffic", func(opts *Options) { opts.Traffic = nil }},
		{"inbox", func(opts *Options) { opts.Inbox = nil }},
		{"logger", func(opts *Options) { opts.Logger = nil }},
	} {
		t.Run(missing.name, func(t *testing.T) {
			opts := Options{
				Auth:        new(auth.Service),
				Nodes:       new(nodes.Service),
				Management:  new(management.Service),
				Enrollments: new(enrollment.Service),
				Ingest:      new(ingest.Service),
				Alerts:      new(alerts.Service),
				Traffic:     new(traffic.Service),
				Inbox:       new(inbox.Service),
				Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			missing.clear(&opts)
			if _, err := NewHandler(opts); err == nil || err.Error() != missing.name+" service is required" {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
