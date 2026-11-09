package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLInterruptedBootstrapKeyCreationKeepsEstablishedAndEncryptedStoresClosed(t *testing.T) {
	for _, test := range []struct {
		name                   string
		encrypted, established bool
	}{{name: "pending_empty"}, {name: "pending_encrypted", encrypted: true}, {name: "established_without_ciphertext", established: true}} {
		t.Run(test.name, func(t *testing.T) {
			f := storagetest.Start(t)
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
			opened, openErr := storage.OpenDatabases(ctx, cfg.Database, dir)
			if openErr != nil {
				t.Fatal(openErr)
			}
			defer opened.Close()
			if !opened.BootstrapRequired() {
				t.Fatal("first registered cluster was not awaiting initial bootstrap")
			}
			if test.encrypted || test.established {
				if err = datadir.Prepare(dir); err != nil {
					t.Fatal(err)
				}
				user, createErr := auth.NewService(opened.Main, auth.Options{}).CreateUser(ctx, "retained-bootstrap-user", "retained-bootstrap-password", false)
				if createErr != nil {
					t.Fatal(createErr)
				}
				if test.encrypted {
					lease, leaseErr := opened.Main.Acquire(ctx, storage.Writer)
					if leaseErr != nil {
						t.Fatal(leaseErr)
					}
					tx, txErr := lease.BeginWrite(lease.Context())
					if txErr != nil {
						lease.Close()
						t.Fatal(txErr)
					}
					_, err = tx.ExecContext(lease.Context(), "UPDATE users SET totp_ciphertext=? WHERE id=?", []byte{1}, user.ID)
					if err == nil {
						err = tx.Commit()
					} else {
						tx.Rollback()
					}
					lease.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
				if test.established {
					if err = opened.CompleteBootstrap(ctx); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err = opened.Close(); err != nil {
				t.Fatal(err)
			}
			keyPath := filepath.Join(dir, "secrets.key")
			if test.encrypted || test.established {
				if err = os.Remove(keyPath); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = os.Stat(keyPath); !os.IsNotExist(err) {
				t.Fatal("fixture must model crash or loss before key availability", err)
			}
			t.Setenv("ORBITBEACON_ADMIN_USERNAME", "interrupted-bootstrap-admin")
			t.Setenv("ORBITBEACON_ADMIN_PASSWORD", "interrupted-bootstrap-password")
			application, buildErr := Build(cfg, slog.New(slog.DiscardHandler), ctx)
			if application != nil {
				defer application.close(context.Background())
			}
			if test.encrypted || test.established {
				if buildErr == nil {
					t.Fatal("missing matching key was replaced for encrypted or established cluster")
				}
				if _, err = os.Stat(keyPath); !os.IsNotExist(err) {
					t.Fatal("refused startup created a replacement key", err)
				}
				var count int
				if err = f.RawEndpoint(t, "primary", storage.MainDomain).QueryRow("SELECT count(*) FROM users WHERE username='retained-bootstrap-user'").Scan(&count); err != nil || count != 1 {
					t.Fatal("failed startup changed existing administrator", count, err)
				}
				return
			}
			if buildErr != nil {
				t.Fatalf("registered pending bootstrap could not create its first key: %v", buildErr)
			}
			if application.replication.BootstrapRequired() {
				t.Fatal("initial bootstrap did not finish after key creation")
			}
			var count int
			if err = f.RawEndpoint(t, "primary", storage.MainDomain).QueryRow("SELECT count(*) FROM users WHERE username='interrupted-bootstrap-admin'").Scan(&count); err != nil || count != 1 {
				t.Fatal("authorized first administrator not created exactly once", count, err)
			}
		})
	}
}
