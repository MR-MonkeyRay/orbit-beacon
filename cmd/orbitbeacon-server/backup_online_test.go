package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"orbitbeacon/internal/server/backup"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/storage"
)

func TestSnapshotWhileServerLockedAllowsWriterProgress(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	path := filepath.Join(dir, "orbitbeacon.db")
	lock, err := datadir.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`PRAGMA wal_autocheckpoint=0; INSERT INTO settings VALUES('sentinel','committed'),('counter','0'); WITH RECURSIVE n(x) AS(VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<512) INSERT INTO settings SELECT 'payload-'||x,hex(randomblob(2048)) FROM n`); err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int64
	stop, started := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for i := int64(1); ; i++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if _, err := db.Exec(`UPDATE settings SET value=? WHERE key='counter'`, strconv.FormatInt(i, 10)); err != nil {
				if i == 1 {
					close(started)
				}
				done <- err
				return
			}
			writes.Store(i)
			if i == 1 {
				close(started)
			}
		}
	}()
	<-started
	before := writes.Load()
	output := filepath.Join(dir, "snapshot.db")
	err = backup.SnapshotDatabase(ctx, path, output, storage.CurrentSchemaVersion)
	after := writes.Load()
	close(stop)
	writerErr := <-done
	if err != nil || writerErr != nil {
		t.Fatal(err, writerErr)
	}
	if after <= before {
		t.Fatalf("writer blocked: before=%d after=%d", before, after)
	}
	reader, err := storage.OpenReader(ctx, output)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var value string
	var counter int64
	if err = reader.QueryRow(`SELECT value FROM settings WHERE key='sentinel'`).Scan(&value); err != nil || value != "committed" {
		t.Fatalf("snapshot sentinel=%q err=%v", value, err)
	}
	if err = reader.QueryRow(`SELECT value FROM settings WHERE key='counter'`).Scan(&counter); err != nil || counter < 1 || counter > writes.Load() {
		t.Fatalf("snapshot counter=%d err=%v", counter, err)
	}
	for name, database := range map[string]*sql.DB{"source": db, "snapshot": reader} {
		var versions, version int
		if err := database.QueryRow(`SELECT count(*), max(version) FROM schema_migrations`).Scan(&versions, &version); err != nil || versions != 1 || version != storage.CurrentSchemaVersion {
			t.Fatalf("%s ledger count=%d version=%d err=%v", name, versions, version, err)
		}
		if err := database.QueryRow(`SELECT value FROM settings WHERE key='sentinel'`).Scan(&value); err != nil || value != "committed" {
			t.Fatalf("%s sentinel=%q err=%v", name, value, err)
		}
	}
	if _, err = os.Lstat(filepath.Join(dir, "secrets.key")); !os.IsNotExist(err) {
		t.Fatalf("snapshot created key: %v", err)
	}
}

func TestSnapshotRejectsMissingAndInexactSchemaWithoutMigration(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		missing bool
		version int
	}{
		{name: "missing_source", missing: true},
		{name: "empty_ledger"},
		{name: "old_schema", version: storage.CurrentSchemaVersion - 1},
		{name: "future_schema", version: storage.CurrentSchemaVersion + 1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(dir, "orbitbeacon.db")
			if !fixture.missing {
				db, err := storage.Open(context.Background(), source)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec(`INSERT INTO settings(key,value) VALUES('sentinel','committed'); DELETE FROM schema_migrations`); err != nil {
					t.Fatal(err)
				}
				if fixture.version != 0 {
					if _, err = db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, fixture.version, "2026-10-28T00:00:00Z"); err != nil {
						t.Fatal(err)
					}
				}
				if err = db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(source)
			if fixture.missing {
				if !os.IsNotExist(err) {
					t.Fatalf("missing source fixture: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := backup.SnapshotDatabase(context.Background(), source, filepath.Join(dir, "snapshot.db"), storage.CurrentSchemaVersion); err == nil {
				t.Fatal("invalid source accepted")
			}
			after, err := os.ReadFile(source)
			if fixture.missing {
				if !os.IsNotExist(err) {
					t.Fatalf("snapshot created missing source: %v", err)
				}
			} else if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("source migrated or modified: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "secrets.key")); !os.IsNotExist(err) {
				t.Fatal("snapshot generated key")
			}
		})
	}
}
