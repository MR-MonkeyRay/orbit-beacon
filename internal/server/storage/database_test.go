package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func leaseDatabase(t *testing.T) *Database {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := OpenMain(context.Background(), filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestNestedLeasePinsGenerationDuringDrain(t *testing.T) {
	db := leaseDatabase(t)
	outer, err := db.Acquire(context.Background(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan error, 1)
	go func() { drained <- db.drain(context.Background()) }()
	select {
	case <-outer.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("drain did not cancel generation")
	}
	nested, err := db.Acquire(outer.Context(), Reader)
	if err != nil {
		t.Fatal(err)
	}
	if nested.Generation() != outer.Generation() {
		t.Fatal("nested lease crossed generation")
	}
	_ = outer.Close()
	select {
	case err := <-drained:
		t.Fatalf("drained with nested lease: %v", err)
	default:
	}
	_ = nested.Close()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("nested lease deadlocked drain")
	}
}

func TestRowsAndStatementsHoldGenerationUntilClosed(t *testing.T) {
	for _, resource := range []string{"rows", "statement"} {
		t.Run(resource, func(t *testing.T) {
			db := leaseDatabase(t)
			lease, err := db.Acquire(context.Background(), Writer)
			if err != nil {
				t.Fatal(err)
			}
			var closeResource func() error
			if resource == "rows" {
				rows, err := lease.QueryContext(lease.Context(), "SELECT 1 UNION ALL SELECT 2")
				if err != nil {
					t.Fatal(err)
				}
				closeResource = rows.Close
			} else {
				tx, err := lease.BeginWrite(lease.Context())
				if err != nil {
					t.Fatal(err)
				}
				stmt, err := tx.PrepareContext(lease.Context(), "SELECT 1")
				if err != nil {
					t.Fatal(err)
				}
				closeResource = stmt.Close
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			_ = lease.Close()
			drained := make(chan error, 1)
			go func() { drained <- db.drain(context.Background()) }()
			select {
			case err := <-drained:
				t.Fatalf("drained with open %s: %v", resource, err)
			case <-time.After(20 * time.Millisecond):
			}
			_ = closeResource()
			select {
			case err := <-drained:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("resource release did not drain")
			}
		})
	}
}

func TestWriteGateCancellationAndDomainIsolation(t *testing.T) {
	main, history := leaseDatabase(t), leaseDatabase(t)
	lease, err := main.Acquire(context.Background(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithTimeout(lease.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := lease.BeginWrite(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("gate error = %v", err)
	}
	other, err := history.Acquire(context.Background(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	otherTx, err := other.BeginWrite(other.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer otherTx.Rollback()
	if _, err := otherTx.ExecContext(other.Context(), "INSERT INTO settings(key,value) VALUES('independent','yes')"); err != nil {
		t.Fatal(err)
	}
	if err := otherTx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	next, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Rollback()
	if _, err := next.ExecContext(lease.Context(), "INSERT INTO settings(key,value) VALUES('released','yes')"); err != nil {
		t.Fatal(err)
	}
	if err := next.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestDrainTimeoutDoesNotReopenAdmission(t *testing.T) {
	db := leaseDatabase(t)
	lease, err := db.Acquire(context.Background(), Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := db.drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain = %v", err)
	}
	if _, err := db.Acquire(context.Background(), Writer); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("admission reopened: %v", err)
	}
}

func TestGenerationPublicationWaitsForRequestAndRejectsOldSQL(t *testing.T) {
	db, replacement := leaseDatabase(t), leaseDatabase(t)
	request, err := db.Acquire(context.Background(), Reader)
	if err != nil {
		t.Fatal(err)
	}
	var exists int
	if err := request.QueryRowContext(request.Context(), "SELECT count(*) FROM users").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	published := make(chan error, 1)
	go func() { published <- db.publishGeneration(context.Background(), replacement.current) }()
	select {
	case <-request.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("request was not canceled")
	}
	nested, err := db.Acquire(request.Context(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	if nested.Generation() != request.Generation() {
		t.Fatal("authenticated request changed database")
	}
	if tx, err := nested.BeginWrite(context.Background()); err == nil {
		_ = tx.Rollback()
		t.Fatal("old request wrote after cancellation")
	}
	_ = nested.Close()
	select {
	case err := <-published:
		t.Fatalf("published before response finished: %v", err)
	default:
	}
	_ = request.Close()
	select {
	case err := <-published:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("publication blocked after request finished")
	}
	lease, err := db.Acquire(context.Background(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if lease.Generation() != request.Generation()+1 {
		t.Fatal("generation did not advance")
	}
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(lease.Context(), "INSERT INTO settings(key,value) VALUES('new','active')"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotPinsActiveGenerationAndPreservesReadOnlyPool(t *testing.T) {
	db := leaseDatabase(t)
	lease, err := db.Acquire(context.Background(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(lease.Context(), "INSERT INTO settings(key,value) VALUES('snapshot','active')"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := os.Chmod(filepath.Dir(output), 0700); err != nil {
		t.Fatal(err)
	}
	if err := lease.Snapshot(lease.Context(), output); err != nil {
		t.Fatal(err)
	}
	copy, err := OpenReader(context.Background(), output)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	var value string
	if err := copy.QueryRow("SELECT value FROM settings WHERE key='snapshot'").Scan(&value); err != nil || value != "active" {
		t.Fatalf("snapshot=%q err=%v", value, err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(output + suffix); !os.IsNotExist(err) {
			t.Fatalf("snapshot created sidecar %s: %v", suffix, err)
		}
	}
	var readonly int
	if err := lease.gen.reader.QueryRow("PRAGMA query_only").Scan(&readonly); err != nil || readonly != 1 {
		t.Fatalf("query_only=%d err=%v", readonly, err)
	}
}

func TestCanceledLeaseCannotCommitWithUnrelatedContext(t *testing.T) {
	db := leaseDatabase(t)
	requestCtx, cancel := context.WithCancel(context.Background())
	lease, err := db.Acquire(requestCtx, Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(context.Background(), "INSERT INTO settings(key,value) VALUES('canceled','must rollback')"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := tx.Commit(); err == nil {
		t.Fatal("canceled lease committed a write")
	}
	fresh, err := db.Acquire(context.Background(), Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	var count int
	if err := fresh.QueryRowContext(fresh.Context(), "SELECT count(*) FROM settings WHERE key='canceled'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("canceled transaction was persisted")
	}
}
