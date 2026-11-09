package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/config"
)

func TestReplicaControlPrivateWALFullAndReadOnlyClusterID(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplicationClusterID(ctx, dir); err == nil {
		t.Fatal("missing control accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "storage-control.db")); !os.IsNotExist(err) {
		t.Fatal("read-only status initialized control", err)
	}
	r, err := openControl(ctx, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.control.Close()
	r.record = controlRecord{Cluster: "test-cluster", Binding: "[]", Domains: map[Domain]*memberRecord{MainDomain: {Active: "primary", Epoch: "epoch", State: "initializing"}}}
	if err = r.persistLocked(ctx); err != nil {
		t.Fatal(err)
	}
	var mode string
	var synchronous int
	if err = r.control.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatal("control is not WAL", mode, err)
	}
	if err = r.control.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatal("control is not FULL", synchronous, err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(filepath.Join(dir, "storage-control.db"+suffix))
		if suffix != "" && os.IsNotExist(err) {
			continue
		}
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("unsafe control component %q: %v", suffix, err)
		}
	}
	if err = r.control.Close(); err != nil {
		t.Fatal(err)
	}
	id, err := ReplicationClusterID(ctx, dir)
	if err != nil || id != "test-cluster" {
		t.Fatal("read-only control identity", id, err)
	}
}

func TestStandaloneStartStopReplicationCreatesNoControlOrJournal(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	dbs, err := OpenDatabases(ctx, config.DatabaseConfig{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	run, cancel := context.WithCancel(ctx)
	if err = dbs.StartReplication(run); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = dbs.StopReplication(); err != nil {
		t.Fatal(err)
	}
	if dbs.replication != nil {
		t.Fatal("standalone allocated a coordinator")
	}
	if _, err = os.Stat(filepath.Join(dir, "storage-control.db")); !os.IsNotExist(err) {
		t.Fatal("standalone created control", err)
	}
	for _, db := range []*Database{dbs.Main, dbs.History} {
		var n int
		if err = db.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name LIKE 'replica_%' OR name LIKE 'ob_replica_%'").Scan(&n); err != nil || n != 0 {
			t.Fatal("standalone journal/trigger overhead", n, err)
		}
	}
}
