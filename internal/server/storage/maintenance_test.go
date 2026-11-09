package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceCompactReclaimsSQLiteFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "maintenance.db")
	db, err := OpenMain(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	lease, err := db.Acquire(t.Context(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 64 {
		if _, err = tx.ExecContext(lease.Context(), "INSERT INTO settings(`key`,value) VALUES(?,?)", strings.Repeat("k", i+1), strings.Repeat("x", 32768)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = lease.Compact(lease.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err = lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(lease.Context(), "DELETE FROM settings"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_, free, err := lease.DiskUsage(lease.Context())
	if err != nil || free == nil || *free == 0 {
		t.Fatalf("deletion did not expose free pages: %v %v", free, err)
	}
	if err = lease.Compact(lease.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, free, err = lease.DiskUsage(lease.Context())
	if err != nil || *free != 0 || after.Size() >= before.Size() {
		t.Fatalf("compaction did not reclaim file: before=%d after=%d free=%v err=%v", before.Size(), after.Size(), free, err)
	}
}

func TestMaintenanceCompactHonorsWriteGateAndGenerationCancellation(t *testing.T) {
	db := leaseDatabase(t)
	lease, err := db.Acquire(t.Context(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(lease.Context(), 20*time.Millisecond)
	defer cancel()
	if err = lease.Compact(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("compaction bypassed write gate: %v", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	db.current.cancel()
	if err = lease.Compact(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("compaction used canceled generation: %v", err)
	}
}

func TestMaintenanceCompactBusyReaderIsNotStorageFailure(t *testing.T) {
	db := leaseDatabase(t)
	db.current.writer.SetMaxOpenConns(1)
	writer, err := db.Acquire(t.Context(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := db.Acquire(t.Context(), Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	read, err := reader.BeginRead(reader.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback()
	var n int
	if err = read.QueryRowContext(reader.Context(), "SELECT COUNT(*) FROM settings").Scan(&n); err != nil {
		t.Fatal(err)
	}
	write, err := writer.BeginWrite(writer.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = write.ExecContext(writer.Context(), "INSERT INTO settings VALUES('reader','blocked')"); err != nil {
		t.Fatal(err)
	}
	if err = write.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = db.current.writer.ExecContext(t.Context(), "PRAGMA busy_timeout=1"); err != nil {
		t.Fatal(err)
	}
	if err = writer.Compact(writer.Context()); !IsBusy(err) {
		t.Fatalf("busy checkpoint not surfaced: %v", err)
	}
	if !db.Ready() {
		t.Fatal("busy reader poisoned readiness")
	}
}

func TestMaintenanceDeleteBatchIsCapturedAndReplicated(t *testing.T) {
	source, target := replicaPair(t, HistoryDomain)
	replicaWrite(t, source, "INSERT INTO buckets VALUES('n','cpu',60,1,10,10,1)")
	batch, err := readReplicaTransaction(t.Context(), source, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = applyReplicaTransaction(t.Context(), target, batch); err != nil {
		t.Fatal(err)
	}
	lease, err := source.Acquire(t.Context(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	n, err := DeleteBatch(lease.Context(), tx, "SELECT node_id,metric,step,t FROM buckets WHERE t<? ORDER BY t LIMIT ?", "DELETE FROM buckets WHERE node_id=? AND metric=? AND step=? AND t=?", 2, 128)
	if err != nil || n != 1 {
		t.Fatalf("delete: %d %v", n, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	batch, err = readReplicaTransaction(t.Context(), source, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Changes) != 1 {
		t.Fatalf("cleanup missing captured deletion: %+v", batch)
	}
	if err = applyReplicaTransaction(t.Context(), target, batch); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = target.current.reader.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM buckets").Scan(&count); err != nil || count != 0 {
		t.Fatalf("cleanup not replicated: %d %v", count, err)
	}
	if err = lease.Compact(lease.Context()); err != nil {
		t.Fatal(err)
	}
}
