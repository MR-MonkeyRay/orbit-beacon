package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/config"
)

func TestRunStopsWorkersBeforeReturning(t *testing.T) {
	started := make(chan struct{})
	workerStopped := make(chan struct{})
	workerReady := make(chan struct{})
	handler := &workerLifecycleHandler{check: func(string, context.Context) {}}
	serveFailure := errors.New("listener failed")
	listener := &failingListener{ready: workerReady, failure: serveFailure}
	a := &App{
		logger: slog.New(slog.DiscardHandler), handler: handler,
		httpServer: &http.Server{Handler: handler}, listener: listener,
		background: []func(context.Context){func(ctx context.Context) {
			close(started)
			<-ctx.Done()
			close(workerStopped)
		}},
	}
	finished := make(chan error, 1)
	go func() { finished <- a.Run(context.Background()) }()
	<-started
	close(workerReady)
	if err := <-finished; !errors.Is(err, serveFailure) {
		t.Fatalf("Run error = %v, want listener failure", err)
	}
	select {
	case <-workerStopped:
	default:
		t.Fatal("Run returned before worker stopped")
	}
}

func TestBuildAndRunServeUntilCanceled(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadServer(config.LoadOptions{Flags: map[string]string{
		"data_dir": filepath.Join(parent, "data"), "listen_addr": "127.0.0.1:0",
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := Build(cfg, slog.New(slog.DiscardHandler), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.metricsServer != nil {
		t.Fatal("metrics server is set without metrics_listen")
	}
	address := a.listener.Addr().String()
	finished := make(chan error, 1)
	go func() { finished <- a.Run(ctx) }()
	response, err := http.Get("http://" + address + "/healthz/live")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("live status = %d", response.StatusCode)
	}
	cancel()
	if err := <-finished; err != nil {
		t.Fatalf("Run error = %v", err)
	}
}

type workerLifecycleHandler struct{ check func(string, context.Context) }

func (h *workerLifecycleHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}
func (h *workerLifecycleHandler) MarkNotReady()                                { h.check("marking not ready", nil) }
func (h *workerLifecycleHandler) CloseLiveStreams()                            { h.check("closing live streams", nil) }
func (h *workerLifecycleHandler) WaitLiveStreams(ctx context.Context) error {
	h.check("draining live streams", ctx)
	return nil
}
func (h *workerLifecycleHandler) RunRetention(context.Context) {}

type failingListener struct {
	ready   <-chan struct{}
	failure error
}

func (l *failingListener) Accept() (net.Conn, error) { <-l.ready; return nil, l.failure }
func (*failingListener) Close() error                { return nil }
func (*failingListener) Addr() net.Addr              { return &net.TCPAddr{} }
