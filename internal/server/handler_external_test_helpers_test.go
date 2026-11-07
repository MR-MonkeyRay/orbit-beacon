package server_test

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/alerts"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/traffic"
)

func testHandlerOptions(t *testing.T, opts server.Options) server.Options {
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
