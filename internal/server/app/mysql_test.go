package app

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLApplicationServesWithoutLocalBusinessFiles(t *testing.T) {
	fixture := storagetest.Start(t)
	t.Setenv("ORBITBEACON_ADMIN_USERNAME", "mysql-app-admin")
	t.Setenv("ORBITBEACON_ADMIN_PASSWORD", "mysql-app-fixture-password")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadServer(config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }, Flags: map[string]string{"data_dir": dir, "listen_addr": "127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.Primary = fixture.Endpoint("primary")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	application, err := Build(cfg, slog.New(slog.DiscardHandler), ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- application.Run(ctx) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	response, err := http.Get("http://" + application.listener.Addr().String() + "/healthz/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("readiness=%d", response.StatusCode)
	}
	for _, name := range []string{"orbitbeacon.db", "metrics.db"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("remote store created local database %s: %v", name, err)
		}
	}
	if _, err := fixture.Raw(t, storage.MainDomain).Exec(`UPDATE users SET totp_ciphertext=X'01' WHERE username='mysql-app-admin'`); err != nil {
		t.Fatal(err)
	}
	stop()
	key := filepath.Join(dir, "secrets.key")
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if next, err := Build(cfg, slog.New(slog.DiscardHandler), context.Background()); err == nil {
		next.close(context.Background())
		t.Fatal("remote encrypted store started with a replacement key")
	}
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Fatalf("remote store replaced missing key: %v", err)
	}
}
