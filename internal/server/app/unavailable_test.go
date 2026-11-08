package app

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLRegisteredUnavailableStartupServesStaticWithoutBootstrap(t *testing.T) {
	f := storagetest.Start(t)
	proxy, primary := storagetest.ProxyEndpoint(t, f.Endpoint("primary"))
	standby := f.Endpoint("standby")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadServer(config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }, Flags: map[string]string{"data_dir": dir, "listen_addr": "127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = config.DatabaseConfig{Primary: primary, Standby: &standby}
	t.Setenv("ORBITBEACON_ADMIN_USERNAME", "original-admin")
	t.Setenv("ORBITBEACON_ADMIN_PASSWORD", "original-admin-password")
	first, err := Build(cfg, slog.New(slog.DiscardHandler), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = first.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	proxy.Pause()
	t.Setenv("ORBITBEACON_ADMIN_USERNAME", "must-not-be-created")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	application, err := Build(cfg, slog.New(slog.DiscardHandler), ctx)
	if err != nil {
		t.Fatal("registered outage prevented recovery process startup", err)
	}
	if application.replication.Main.Generation() != 0 {
		t.Fatal("unreachable startup invented a generation")
	}
	done := make(chan error, 1)
	go func() { done <- application.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	address := "http://" + application.listener.Addr().String()
	for _, path := range []string{"/healthz/live", "/login", "/settings"} {
		response, err := http.Get(address + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("static/liveness %s=%d", path, response.StatusCode)
		}
	}
	public, err := http.Get(address + "/")
	if err != nil {
		t.Fatal(err)
	}
	public.Body.Close()
	if public.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("unknown public settings did not fail closed", public.StatusCode)
	}
	response, err := http.Get(address + "/healthz/ready")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("unavailable main reported ready", response.StatusCode)
	}
	proxy.Resume()
	deadline := time.Now().Add(30 * time.Second)
	for !application.replication.Main.Ready() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !application.replication.Main.Ready() {
		t.Fatal("same authority did not recover")
	}
	raw := f.RawEndpoint(t, "primary", storage.MainDomain)
	var original, unexpected int
	if err = raw.QueryRow("SELECT count(*) FROM users WHERE username='original-admin'").Scan(&original); err != nil {
		t.Fatal(err)
	}
	if err = raw.QueryRow("SELECT count(*) FROM users WHERE username='must-not-be-created'").Scan(&unexpected); err != nil {
		t.Fatal(err)
	}
	if original != 1 || unexpected != 0 {
		t.Fatal("recovery recreated bootstrap identity", original, unexpected)
	}
	stop()
	proxy.Pause()
	key := filepath.Join(dir, "secrets.key")
	if err = os.Remove(key); err != nil {
		t.Fatal(err)
	}
	next, err := Build(cfg, slog.New(slog.DiscardHandler), context.Background())
	if next != nil {
		next.close(context.Background())
	}
	if err == nil {
		t.Fatal("registered outage accepted missing secrets.key")
	}
	if _, err = os.Stat(key); !os.IsNotExist(err) {
		t.Fatal("registered outage regenerated key", err)
	}
}
