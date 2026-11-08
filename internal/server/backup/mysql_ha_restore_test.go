package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLHARestore(t *testing.T) { testMySQLHARestore(t, false) }

func TestMySQLHAMixedActiveRestore(t *testing.T) { testMySQLHARestore(t, true) }

func testMySQLHARestore(t *testing.T, mixed bool) {
	for _, standbyDriver := range []string{"mysql", "sqlite"} {
		t.Run(standbyDriver, func(t *testing.T) {
			for _, boundary := range []string{"success", "prepared:durable", "restore:pending:durable", "install:main:committed", "install:history:committed", "install:key:durable", "committed:durable", "finish:storage:durable"} {
				t.Run(boundary, func(t *testing.T) {
					fixture, cfg, old := haRestoreFixtureMode(t, standbyDriver, mixed)
					next := restoreFixture(t, "source")
					archive, password := restoreArchive(t, next, []Category{Configuration, Monitoring, Events})
					oldKey, err := datadir.LoadKey(filepath.Join(cfg.DataDir, "secrets.key"))
					if err != nil {
						t.Fatal(err)
					}
					defer clear(oldKey)
					if _, err = Restore(t.Context(), cfg, archive, password, false); restoreCode(err) != "restore_requires_replace" {
						t.Fatalf("nonempty HA restore=%v", err)
					}
					if boundary == "success" {
						if _, err = Restore(t.Context(), cfg, archive, password, true); err != nil {
							t.Fatal(err)
						}
					} else {
						configPath := filepath.Join(t.TempDir(), "server.json")
						body, err := json.Marshal(cfg)
						if err != nil {
							t.Fatal(err)
						}
						if err = os.WriteFile(configPath, body, 0600); err != nil {
							t.Fatal(err)
						}
						runRemoteCrash(t, configPath, next, boundary, "commit")
						if boundary != "committed:durable" && boundary != "finish:storage:durable" {
							// 回滚每域均可再次退出，重复恢复仍收敛到相同 old 决定。
							runRemoteCrash(t, configPath, next, "rollback:main:committed", "recover")
							runRemoteCrash(t, configPath, next, "rollback:history:committed", "recover")
							runRemoteCrash(t, configPath, next, "finish:storage:durable", "recover")
						}
						if err = RecoverRestore(t.Context(), cfg); err != nil {
							t.Fatal(err)
						}
						if err = RecoverRestore(t.Context(), cfg); err != nil {
							t.Fatal(err)
						}
					}
					want, total := "target", float64(22)
					expectedKey := oldKey
					if boundary == "success" || boundary == "committed:durable" || boundary == "finish:storage:durable" {
						want, total = "source", 11
						expectedKey, err = datadir.LoadKey(filepath.Join(next, "secrets.key"))
						if err != nil {
							t.Fatal(err)
						}
						defer clear(expectedKey)
					}
					key, err := datadir.LoadKey(filepath.Join(cfg.DataDir, "secrets.key"))
					if err != nil || !bytes.Equal(key, expectedKey) {
						t.Fatalf("HA key mismatch: %v", err)
					}
					clear(key)
					domains, err := storage.OpenOfflineDatabases(t.Context(), cfg.Database, cfg.DataDir)
					if err != nil {
						t.Fatal(err)
					}
					defer domains.Close()
					if boundary == "success" {
						work := framingTempDir(t)
						if _, err = CreateSnapshotPayloads(t.Context(), SnapshotSources{Main: domains.Main, History: domains.History}, cfg.DataDir, work, []Category{Configuration, Monitoring, Events}, time.Now); err != nil {
							t.Fatal(err)
						}
						for _, category := range []Category{Configuration, Monitoring, Events} {
							payload, err := openPayload(t.Context(), filepath.Join(work, string(category)+".sqlite"))
							if err != nil {
								t.Fatal(err)
							}
							var count int
							err = payload.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name LIKE 'replica_%' OR name='storage_write_guard'`).Scan(&count)
							payload.Close()
							if err != nil || count != 0 {
								t.Fatalf("HA协议进入归档类别 %s: count=%d err=%v", category, count, err)
							}
						}
					}
					for i, db := range []*storage.Database{domains.Main, domains.History} {
						id, err := db.InstanceID(t.Context())
						if err != nil || id != old[i].instance {
							t.Fatalf("active identity changed: domain=%d err=%v", i, err)
						}
						lease, err := db.Acquire(t.Context(), storage.Reader)
						if err != nil {
							t.Fatal(err)
						}
						var epoch string
						if err = lease.QueryRowContext(lease.Context(), `SELECT epoch_uuid FROM replica_state`).Scan(&epoch); err != nil || epoch == old[i].epoch {
							t.Fatalf("restore reused old epoch: domain=%d err=%v", i, err)
						}
						if i == 0 {
							var name string
							if err = lease.QueryRowContext(lease.Context(), `SELECT name FROM nodes WHERE id=?`, restoreNode).Scan(&name); err != nil || name != want {
								t.Fatalf("main=%q want=%q err=%v", name, want, err)
							}
						} else {
							var got float64
							if err = lease.QueryRowContext(lease.Context(), `SELECT total FROM buckets WHERE node_id=?`, restoreNode).Scan(&got); err != nil || got != total {
								t.Fatalf("history=%v want=%v err=%v", got, total, err)
							}
						}
						lease.Close()
						var ready bool
						inactive := haStandbyReader(t, fixture, cfg, db.Domain())
						if mixed && db.Domain() == storage.MainDomain {
							inactive = fixture.RawEndpoint(t, "primary", db.Domain())
						}
						if err = inactive.QueryRow(`SELECT ready FROM replica_state`).Scan(&ready); err != nil || ready {
							t.Fatalf("old standby retained eligibility: domain=%d ready=%t err=%v", i, ready, err)
						}
					}
					lease, err := domains.Main.Acquire(t.Context(), storage.Writer)
					if err != nil {
						t.Fatal(err)
					}
					var maximum int64
					if err = lease.QueryRowContext(lease.Context(), `SELECT COALESCE(max(sequence),0) FROM inbox_notifications`).Scan(&maximum); err != nil {
						t.Fatal(err)
					}
					tx, err := lease.BeginWrite(lease.Context())
					if err != nil {
						t.Fatal(err)
					}
					if err = insertRestoredNotification(lease.Context(), tx); err != nil {
						t.Fatal(err)
					}
					if err = tx.Commit(); err != nil {
						t.Fatal(err)
					}
					var sequence, counter int64
					if err = lease.QueryRowContext(lease.Context(), `SELECT sequence FROM inbox_notifications WHERE source_id='post-restore'`).Scan(&sequence); err != nil {
						t.Fatal(err)
					}
					if err = lease.QueryRowContext(lease.Context(), `SELECT value FROM inbox_sequence WHERE id=1`).Scan(&counter); err != nil || counter != sequence || sequence <= maximum {
						t.Fatalf("counter=%d sequence=%d err=%v", counter, sequence, err)
					}
					lease.Close()
					if boundary == "success" {
						if err = domains.Close(); err != nil {
							t.Fatal(err)
						}
						domains, err = storage.OpenDatabases(t.Context(), cfg.Database, cfg.DataDir)
						if err != nil {
							t.Fatal(err)
						}
						defer domains.Close()
						if err = domains.StartReplication(t.Context()); err != nil {
							t.Fatal(err)
						}
						defer domains.StopReplication()
						reader := haStandbyReader(t, fixture, cfg, storage.MainDomain)
						deadline := time.Now().Add(30 * time.Second)
						for {
							var ready bool
							var name string
							var value int64
							readyErr := reader.QueryRow(`SELECT ready FROM replica_state`).Scan(&ready)
							nameErr := reader.QueryRow(`SELECT name FROM nodes WHERE id=?`, restoreNode).Scan(&name)
							counterErr := reader.QueryRow(`SELECT value FROM inbox_sequence WHERE id=1`).Scan(&value)
							if readyErr == nil && nameErr == nil && counterErr == nil && ready && name == want && value == counter {
								break
							}
							if time.Now().After(deadline) {
								t.Fatal("restored HA cluster did not reseed standby data and counter")
							}
							time.Sleep(20 * time.Millisecond)
						}
					}
				})
			}
		})
	}
}

type haRestorePosition struct{ instance, epoch string }

func haRestoreFixture(t *testing.T, standbyDriver string) (*storagetest.Fixture, config.Server, [2]haRestorePosition) {
	return haRestoreFixtureMode(t, standbyDriver, false)
}

func haRestoreFixtureMode(t *testing.T, standbyDriver string, mixed bool) (*storagetest.Fixture, config.Server, [2]haRestorePosition) {
	t.Helper()
	fixture := storagetest.Start(t)
	dir := framingTempDir(t)
	old := restoreFixture(t, "target")
	if err := copyRestoreFile(t.Context(), filepath.Join(old, "secrets.key"), filepath.Join(dir, "secrets.key")); err != nil {
		t.Fatal(err)
	}
	standby := fixture.Endpoint("standby")
	if standbyDriver == "sqlite" {
		standby = config.DatabaseEndpoint{Driver: "sqlite"}
	}
	cfg, err := config.LoadServer(config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = dir
	cfg.Database.Primary = fixture.Endpoint("primary")
	cfg.Database.Standby = &standby
	if mixed {
		cfg.Database.Failover = config.FailoverConfig{FailureTimeout: time.Second, MaxLossWindow: 10 * time.Second, RecoveryStable: 60 * time.Second, ManualAfter: time.Hour}
	}
	for i, domain := range []storage.Domain{storage.MainDomain, storage.HistoryDomain} {
		db, err := storage.OpenDomain(t.Context(), cfg.Database.Primary, domain, dir)
		if err != nil {
			t.Fatal(err)
		}
		if err = storage.ReplaceDomain(t.Context(), db, filepath.Join(old, restoreInstallFiles[i]), uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	domains, err := storage.OpenDatabases(t.Context(), cfg.Database, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer domains.Close()
	if err = domains.CompleteBootstrap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = domains.StartReplication(t.Context()); err != nil {
		t.Fatal(err)
	}
	var positions [2]haRestorePosition
	for i, db := range []*storage.Database{domains.Main, domains.History} {
		positions[i].instance, err = db.InstanceID(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		lease, err := db.Acquire(t.Context(), storage.Reader)
		if err != nil {
			t.Fatal(err)
		}
		err = lease.QueryRowContext(lease.Context(), `SELECT epoch_uuid FROM replica_state`).Scan(&positions[i].epoch)
		lease.Close()
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(30 * time.Second)
		reader := haStandbyReader(t, fixture, cfg, db.Domain())
		for {
			var ready bool
			err = reader.QueryRow(`SELECT ready FROM replica_state`).Scan(&ready)
			if err == nil && ready {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("standby did not become complete")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if mixed {
		lease, err := domains.Main.Acquire(t.Context(), storage.Writer)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := lease.BeginWrite(lease.Context())
		if err == nil {
			err = tx.Commit()
		}
		lease.Close()
		if err != nil {
			t.Fatal(err)
		}
		var expectedEpoch, expectedID string
		var expectedSeq int64
		if err = fixture.RawEndpoint(t, "primary", storage.MainDomain).QueryRow("SELECT epoch_uuid,head_seq,head_tx_uuid FROM replica_state WHERE id=1").Scan(&expectedEpoch, &expectedSeq, &expectedID); err != nil {
			t.Fatal(err)
		}
		candidate := haStandbyReader(t, fixture, cfg, storage.MainDomain)
		deadline := time.Now().Add(30 * time.Second)
		for {
			var count int
			if candidate.QueryRow("SELECT count(*) FROM replica_transactions WHERE epoch_uuid=? AND seq=? AND tx_uuid=? AND boot_id<>'' AND barrier_ns>0", expectedEpoch, expectedSeq, expectedID).Scan(&count) == nil && count == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("mixed candidate never ACKed real bounded barrier")
			}
			time.Sleep(20 * time.Millisecond)
		}
		// 一域真实只读故障，另一域仍在原主服务；不直接改控制记录伪造角色。
		fixture.SetReadOnly(t, "primary", storage.MainDomain, true)
		lease, acquireErr := domains.Main.Acquire(t.Context(), storage.Writer)
		if acquireErr == nil {
			tx, writeErr := lease.BeginWrite(lease.Context())
			if writeErr == nil {
				writeErr = tx.Commit()
			}
			lease.Close()
			if writeErr == nil {
				t.Fatal("scoped read-only fault did not reject write")
			}
		}
		deadline = time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			id, idErr := domains.Main.InstanceID(t.Context())
			if idErr == nil && id != positions[0].instance && domains.Main.Ready() && domains.History.Ready() {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !domains.Main.Ready() || !domains.History.Ready() {
			t.Fatal("mixed active roles never became serviceable")
		}
		id, err := domains.Main.InstanceID(t.Context())
		if err != nil || id == positions[0].instance {
			t.Fatal("main did not actually promote", err)
		}
		positions[0].instance = id
		lease, err = domains.Main.Acquire(t.Context(), storage.Reader)
		if err != nil {
			t.Fatal(err)
		}
		err = lease.QueryRowContext(lease.Context(), "SELECT epoch_uuid FROM replica_state WHERE id=1").Scan(&positions[0].epoch)
		lease.Close()
		if err != nil {
			t.Fatal(err)
		}
		id, err = domains.History.InstanceID(t.Context())
		if err != nil || id != positions[1].instance {
			t.Fatal("healthy history changed authority", err)
		}
		if err = domains.StopReplication(); err != nil {
			t.Fatal(err)
		}
		fixture.SetReadOnly(t, "primary", storage.MainDomain, false)
	}
	if err = domains.StopReplication(); err != nil {
		t.Fatal(err)
	}
	return fixture, cfg, positions
}

func haStandbyReader(t *testing.T, fixture *storagetest.Fixture, cfg config.Server, domain storage.Domain) *sql.DB {
	t.Helper()
	if cfg.Database.Standby.Driver == "mysql" {
		return fixture.RawEndpoint(t, "standby", domain)
	}
	name := "orbitbeacon.db"
	if domain == storage.HistoryDomain {
		name = "metrics.db"
	}
	db, err := storage.OpenReader(context.Background(), filepath.Join(cfg.DataDir, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMySQLHARestorePendingRequiresControlAndActiveTarget(t *testing.T) {
	_, cfg, _ := haRestoreFixture(t, "sqlite")
	next := restoreFixture(t, "source")
	configPath := filepath.Join(t.TempDir(), "server.json")
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	runRemoteCrash(t, configPath, next, "install:main:committed", "commit")
	journalPath := filepath.Join(cfg.DataDir, restoreJournalName)
	pending, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if domains, err := storage.OpenDatabases(t.Context(), cfg.Database, cfg.DataDir); err == nil {
		domains.Close()
		t.Fatal("ordinary open served a partially restored HA cluster")
	}
	control := filepath.Join(cfg.DataDir, "storage-control.db")
	if err = os.Rename(control, control+".held"); err != nil {
		t.Fatal(err)
	}
	if err = RecoverRestore(t.Context(), cfg); err == nil {
		t.Fatal("missing control allowed recovery to guess primary")
	}
	if err = os.Rename(control+".held", control); err != nil {
		t.Fatal(err)
	}
	wrong := cfg
	wrong.Database.Primary.PasswordFile = filepath.Join(t.TempDir(), "unavailable.password")
	if err = os.WriteFile(wrong.Database.Primary.PasswordFile, []byte("not-the-database-password"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = RecoverRestore(t.Context(), wrong); err == nil {
		t.Fatal("unavailable active target allowed partial recovery")
	}
	after, err := os.ReadFile(journalPath)
	if err != nil || !bytes.Equal(pending, after) {
		t.Fatalf("unavailable recovery changed pending decision: %v", err)
	}
	if err = RecoverRestore(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
}
