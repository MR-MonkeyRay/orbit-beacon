package main

import (
	"context"
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
	if _, err = os.Lstat(filepath.Join(dir, "secrets.key")); !os.IsNotExist(err) {
		t.Fatalf("snapshot created key: %v", err)
	}
}

func TestSnapshotRejectsMissingAndInexactSchemaWithoutMigration(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(strconv.FormatBool(missing), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(dir, "orbitbeacon.db")
			if !missing {
				db, err := storage.Open(context.Background(), source)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec(`DELETE FROM schema_migrations WHERE version=?`, storage.CurrentSchemaVersion); err != nil {
					t.Fatal(err)
				}
				if err = db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(source)
			if err := backup.SnapshotDatabase(context.Background(), source, filepath.Join(dir, "snapshot.db"), storage.CurrentSchemaVersion); err == nil {
				t.Fatal("invalid source accepted")
			}
			after, _ := os.ReadFile(source)
			if string(before) != string(after) {
				t.Fatal("source migrated or modified")
			}
			if _, err := os.Stat(filepath.Join(dir, "secrets.key")); !os.IsNotExist(err) {
				t.Fatal("snapshot generated key")
			}
		})
	}
}
