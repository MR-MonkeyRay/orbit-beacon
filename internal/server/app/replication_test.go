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

func TestMySQLStandbyFirstCopyContainsInitializedAdministrator(t *testing.T) {
	fixture := storagetest.Start(t)
	t.Setenv("ORBITBEACON_ADMIN_USERNAME", "replicated-bootstrap-admin")
	t.Setenv("ORBITBEACON_ADMIN_PASSWORD", "replicated-bootstrap-password")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadServer(config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }, Flags: map[string]string{"data_dir": dir, "listen_addr": "127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	standby := fixture.Endpoint("standby")
	cfg.Database.Primary, cfg.Database.Standby = fixture.Endpoint("primary"), &standby
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	application, err := Build(cfg, slog.New(slog.DiscardHandler), ctx)
	if err != nil {
		t.Fatal(err)
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
	response, err := http.Get("http://" + application.listener.Addr().String() + "/healthz/ready")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("primary readiness=%d", response.StatusCode)
	}
	deadline := time.Now().Add(20 * time.Second)
	copied := false
	target := fixture.RawEndpoint(t, "standby", storage.MainDomain)
	for time.Now().Before(deadline) {
		var username, hash string
		var ready int
		readErr := target.QueryRowContext(ctx, "SELECT username,password_hash FROM users WHERE username=?", "replicated-bootstrap-admin").Scan(&username, &hash)
		if readErr == nil {
			readErr = target.QueryRowContext(ctx, "SELECT ready FROM replica_state").Scan(&ready)
		}
		copied = readErr == nil && username == "replicated-bootstrap-admin" && hash != "" && ready == 1
		if copied {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !copied {
		t.Fatal("first promotable copy did not contain the initialized administrator")
	}
	stop()
	if _, err := os.Stat(filepath.Join(dir, "storage-control.db")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"orbitbeacon.db", "metrics.db"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("MySQL HA created local business replica %s: %v", name, err)
		}
	}
}
