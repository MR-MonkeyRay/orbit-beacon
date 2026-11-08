package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLRegisteredHistoryFailureKeepsApplicationServingMain(t *testing.T) {
	f := storagetest.Start(t)
	t.Setenv("ORBITBEACON_ADMIN_USERNAME", "history-isolation-admin")
	t.Setenv("ORBITBEACON_ADMIN_PASSWORD", "history-isolation-password")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadServer(config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }, Flags: map[string]string{"data_dir": dir, "listen_addr": "127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	standby := f.Endpoint("standby")
	cfg.Database.Primary, cfg.Database.Standby = f.Endpoint("primary"), &standby
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	first, err := Build(cfg, slog.New(slog.DiscardHandler), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	table := storage.Tables(storage.HistoryDomain)[0].Name
	if _, err = f.RawEndpoint(t, "primary", storage.HistoryDomain).Exec("DROP TABLE `" + table + "`"); err != nil {
		t.Fatal(err)
	}
	application, err := Build(cfg, slog.New(slog.DiscardHandler), ctx)
	if err != nil {
		t.Fatalf("registered history failure aborted actual application startup: %v", err)
	}
	running, stop := context.WithCancel(ctx)
	finished := make(chan error, 1)
	go func() { finished <- application.Run(running) }()
	defer func() {
		stop()
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(35 * time.Second)
	for {
		raw, metadataErr := application.replication.CaptureRestoreMetadata(ctx)
		var record struct {
			Domains map[storage.Domain]struct {
				Ready, Manual bool
				State         string
			}
		}
		if metadataErr == nil && json.Unmarshal(raw, &record) == nil && record.Domains[storage.MainDomain].Ready && record.Domains[storage.HistoryDomain].Manual {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("healthy main replication did not progress beside manual history", metadataErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	user, err := auth.NewService(application.replication.Main, auth.Options{}).CreateUser(ctx, "main-still-writable", "main-writer-password", false)
	if err != nil {
		t.Fatal("history failure stopped main mutation", err)
	}
	var users int
	if err = f.RawEndpoint(t, "primary", storage.MainDomain).QueryRow("SELECT count(*) FROM users WHERE id=? AND username=?", user.ID, user.Username).Scan(&users); err != nil || users != 1 {
		t.Fatal("main mutation was not persisted", users, err)
	}
	base := "http://" + application.listener.Addr().String()
	client := &http.Client{Timeout: 3 * time.Second}
	for _, path := range []string{"/healthz/live", "/healthz/ready", "/api/public/v1/nodes"} {
		response, getErr := client.Get(base + path)
		if getErr != nil {
			t.Fatal(getErr)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("history failure stopped actual main HTTP %s: %d", path, response.StatusCode)
		}
	}
	if application.replication.History.Ready() {
		t.Fatal("invalid registered history admitted queries")
	}
}
