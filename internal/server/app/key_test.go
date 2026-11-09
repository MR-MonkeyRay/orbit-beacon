package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/storage"
)

func TestBuildRejectsMissingKeyWhenEncryptedDataExists(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := datadir.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO users(id,username,password_hash,totp_ciphertext,created_at) VALUES('11111111-1111-4111-8111-111111111111','retained-admin','retained-hash',X'01','2026-11-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "secrets.key")
	if err = os.Remove(key); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadServer(config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }, Flags: map[string]string{"data_dir": dir, "listen_addr": "127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	application, err := Build(cfg, slog.New(slog.DiscardHandler), context.Background())
	if application != nil {
		application.close(context.Background())
	}
	if err == nil {
		t.Fatal("started encrypted store with a replacement key")
	}
	if _, err = os.Stat(key); !os.IsNotExist(err) {
		t.Fatalf("missing key was replaced: %v", err)
	}
}
