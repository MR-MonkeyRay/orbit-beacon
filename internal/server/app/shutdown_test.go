package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type shutdownTrace struct {
	mu        sync.Mutex
	events    []string
	deadlines []time.Time
}

func (s *shutdownTrace) add(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}
func (s *shutdownTrace) context(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deadline, _ := ctx.Deadline()
	s.deadlines = append(s.deadlines, deadline)
}

type traceHandler struct{ trace *shutdownTrace }

func (h traceHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}
func (h traceHandler) MarkNotReady()                                { h.trace.add("not_ready") }
func (h traceHandler) CloseLiveStreams()                            { h.trace.add("streams_stopped") }
func (h traceHandler) WaitLiveStreams(ctx context.Context) error {
	h.trace.context(ctx)
	h.trace.add("streams_drained")
	return nil
}
func (h traceHandler) RunRetention(context.Context) {}

type shutdownHTTP struct {
	trace *shutdownTrace
	stall bool
}

func (s shutdownHTTP) Serve(net.Listener) error { return http.ErrServerClosed }
func (s shutdownHTTP) Close() error             { s.trace.add("http_forced_closed"); return nil }
func (s shutdownHTTP) Shutdown(ctx context.Context) error {
	s.trace.context(ctx)
	if s.stall {
		<-ctx.Done()
		return ctx.Err()
	}
	s.trace.add("http_drained")
	return nil
}

type shutdownIngest struct{ trace *shutdownTrace }

func (s shutdownIngest) Shutdown(ctx context.Context) error {
	s.trace.context(ctx)
	s.trace.add("ingest_stopped")
	return nil
}

type shutdownNotify struct{ trace *shutdownTrace }

func (shutdownNotify) Run(context.Context) {}
func (s shutdownNotify) Shutdown(ctx context.Context) error {
	s.trace.context(ctx)
	s.trace.add("notify_stopped")
	return nil
}

type shutdownHistory struct{ trace *shutdownTrace }

func (s shutdownHistory) CloseContext(ctx context.Context) error {
	s.trace.context(ctx)
	s.trace.add("history_flushed_and_closed")
	return nil
}

type shutdownCloser struct {
	trace *shutdownTrace
	event string
}

func (s shutdownCloser) Close() error { s.trace.add(s.event); return nil }
func shutdownApp(trace *shutdownTrace, stallHTTP bool) *App {
	return &App{
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		handler: traceHandler{trace}, httpServer: shutdownHTTP{trace, stallHTTP},
		ingest: shutdownIngest{trace}, notifications: shutdownNotify{trace}, history: shutdownHistory{trace},
		reader: shutdownCloser{trace, "reader_closed"}, db: shutdownCloser{trace, "writer_closed"}, lock: shutdownCloser{trace, "lock_released"},
	}
}
func TestShutdownPreservesOrderAndDeadlines(t *testing.T) {
	trace := &shutdownTrace{}
	app := shutdownApp(trace, false)
	app.shutdown(&sync.WaitGroup{})
	if trace.events[0] != "not_ready" || trace.events[1] != "streams_stopped" {
		t.Fatalf("initial shutdown events: %v", trace.events)
	}
	for _, event := range []string{"streams_drained", "http_drained"} {
		if slices.Index(trace.events, event) > slices.Index(trace.events, "ingest_stopped") {
			t.Fatalf("ingest preceded %s: %v", event, trace.events)
		}
	}
	want := []string{"ingest_stopped", "notify_stopped", "history_flushed_and_closed", "reader_closed", "writer_closed", "lock_released"}
	if got := trace.events[len(trace.events)-len(want):]; !slices.Equal(got, want) {
		t.Fatalf("shutdown tail %v want %v", got, want)
	}
	// Live streams and HTTP share one deadline; each later drain gets its own.
	if len(trace.deadlines) != 5 || !trace.deadlines[0].Equal(trace.deadlines[1]) {
		t.Fatalf("contexts: %v", trace.deadlines)
	}
	for _, deadline := range trace.deadlines {
		if deadline.IsZero() {
			t.Fatalf("missing deadline: %v", trace.deadlines)
		}
	}
}

func TestStalledHTTPDoesNotConsumeLaterDrainBudgets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		trace := &shutdownTrace{}
		app := shutdownApp(trace, true)
		started := time.Now()
		app.shutdown(&sync.WaitGroup{})
		if elapsed := time.Since(started); elapsed != 10*time.Second {
			t.Fatalf("shutdown took %v", elapsed)
		}
		if !slices.Contains(trace.events, "http_forced_closed") || trace.events[len(trace.events)-1] != "lock_released" {
			t.Fatalf("stalled HTTP did not release resources: %v", trace.events)
		}
		for _, deadline := range trace.deadlines[2:] {
			if got := deadline.Sub(started); got != 15*time.Second {
				t.Fatalf("later drain deadline %v after start, want 15s: %v", got, trace.deadlines)
			}
		}
	})
}
