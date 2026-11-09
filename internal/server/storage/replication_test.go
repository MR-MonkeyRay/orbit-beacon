package storage

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func replicaPair(t *testing.T, domain Domain) (*Database, *Database) {
	t.Helper()
	ctx := context.Background()
	open := func(name string) *Database {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		var db *Database
		var err error
		if domain == MainDomain {
			db, err = OpenMain(ctx, filepath.Join(dir, name))
		} else {
			db, err = OpenHistory(ctx, filepath.Join(dir, name))
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	source, target := open("source.db"), open("target.db")
	resource := "resource"
	epoch := "epoch"
	for i, db := range []*Database{source, target} {
		if err := installReplica(ctx, db, resource, fmt.Sprint(i), epoch, replicaProof{}); err != nil {
			t.Fatal(err)
		}
	}
	source.replica = &replicaDomain{source: source, target: target, resource: resource, epoch: epoch}
	if _, err := source.current.writer.ExecContext(ctx, "UPDATE replica_state SET capture_mode='source' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	return source, target
}
func replicaWrite(t *testing.T, db *Database, statements ...string) *Tx {
	t.Helper()
	ctx := context.Background()
	l, err := db.Acquire(ctx, Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	tx, err := l.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, q := range statements {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return tx
}
func TestReplicaTriggersCaptureEveryImageAndRollback(t *testing.T) {
	source, _ := replicaPair(t, MainDomain)
	ctx := context.Background()
	replicaWrite(t, source, "INSERT INTO settings VALUES('key','初始')", "UPDATE settings SET value='第二' WHERE `key`='key'", "UPDATE settings SET value=value WHERE `key`='key'")
	batch, err := readReplicaTransaction(ctx, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Changes) != 3 {
		t.Fatalf("changes=%d", len(batch.Changes))
	}
	for i, want := range []string{"初始", "第二", "第二"} {
		values, err := decodeValues(batch.Changes[i].After, columns(mainTables[10]))
		if err != nil || values[1] != want {
			t.Fatalf("image %d: %v %v", i, values, err)
		}
	}
	if _, err = source.current.writer.ExecContext(ctx, "INSERT INTO settings VALUES('bare','bad')"); err == nil {
		t.Fatal("bare write accepted")
	}
	l, _ := source.Acquire(ctx, Writer)
	defer l.Close()
	tx, _ := l.BeginWrite(ctx)
	if _, err = tx.ExecContext(ctx, "UPDATE settings SET `key`='new'"); err == nil {
		t.Fatal("PK mutation accepted")
	}
	tx.Rollback()
	tx, _ = l.BeginWrite(ctx)
	tx.ExecContext(ctx, "INSERT INTO settings VALUES('rollback','bad')")
	tx.Rollback()
	var count int
	if err = source.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM replica_transactions").Scan(&count); err != nil || count != 1 {
		t.Fatalf("rollback leaked log: %d %v", count, err)
	}
}
func TestReplicaStrictAtomicApplyDedupForkHole(t *testing.T) {
	source, target := replicaPair(t, MainDomain)
	ctx := context.Background()
	replicaWrite(t, source, "INSERT INTO settings VALUES('a','1')", "INSERT INTO settings VALUES('b','2')")
	batch, err := readReplicaTransaction(ctx, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = applyReplicaTransaction(ctx, target, batch); err != nil {
		t.Fatal(err)
	}
	if err = applyReplicaTransaction(ctx, target, batch); err != nil {
		t.Fatal("duplicate", err)
	}
	fork := *batch
	fork.ID = "different"
	if err = applyReplicaTransaction(ctx, target, &fork); !errors.Is(err, ErrReplicaDiverged) {
		t.Fatalf("fork: %v", err)
	}
	hole := *batch
	hole.Seq = 3
	hole.ID = "hole"
	if err = applyReplicaTransaction(ctx, target, &hole); !errors.Is(err, ErrReplicaDiverged) {
		t.Fatalf("hole: %v", err)
	}
	replicaWrite(t, source, "UPDATE settings SET value='3' WHERE `key`='a'", "UPDATE settings SET value='4' WHERE `key`='b'")
	batch, err = readReplicaTransaction(ctx, source, 1)
	if err != nil {
		t.Fatal(err)
	}
	target.current.writer.ExecContext(ctx, "DELETE FROM settings WHERE `key`='b'")
	if err = applyReplicaTransaction(ctx, target, batch); err == nil {
		t.Fatal("missing update accepted")
	}
	var value string
	target.current.reader.QueryRowContext(ctx, "SELECT value FROM settings WHERE `key`='a'").Scan(&value)
	if value != "1" {
		t.Fatal("partial apply", value)
	}
}
func TestReplicaTypedCodecBoundaries(t *testing.T) {
	cols := []ColumnSpec{{"null", Text, false}, {"blob", Blob, false}, {"text", Text, false}, {"uint", Text, false}, {"float", Float, false}}
	for _, f := range []float64{math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64, math.MaxFloat64, -math.MaxFloat64, math.Nextafter(1, 0), math.Nextafter(1, 2)} {
		raw, _ := json.Marshal([]any{nil, []string{"b", ""}, []string{"t", "e7958c00"}, []string{"t", "3138343436373434303733373039353531363135"}, []string{"f", fmt.Sprint(f)}})
		values, err := decodeValues(raw, cols)
		if err != nil {
			t.Fatal(err)
		}
		if values[0] != nil || len(values[1].([]byte)) != 0 || values[2] != "界\x00" || values[3] != "18446744073709551615" || math.Float64bits(values[4].(float64)) != math.Float64bits(f) {
			t.Fatalf("codec %v", values)
		}
	}
	for _, raw := range []string{`[["f","NaN"]]`, `[["i","9223372036854775808"]]`, `[["t","zz"]]`, `[["b","00"]]`} {
		if _, err := decodeValues([]byte(raw), []ColumnSpec{{"x", Float, false}}); err == nil {
			t.Fatal("malformed accepted", raw)
		}
	}
}
func TestReplicaRawBadConnectionSentinelDoesNotUnwrap(t *testing.T) {
	wrapped := fmt.Errorf("wrapped: %w", driver.ErrBadConn)
	if !errors.Is(wrapped, driver.ErrBadConn) {
		t.Fatal("invalid fixture")
	}
	if errors.Is(errRawDiscard, driver.ErrBadConn) {
		t.Fatal("Raw sentinel must not trigger sql.Conn close under Tx lock")
	}
}
func TestReplicaJournalPressureRevokesPinBeforePruning(t *testing.T) {
	source, _ := replicaPair(t, MainDomain)
	ctx := context.Background()
	replicaWrite(t, source, "INSERT INTO settings VALUES('a','1')")
	replicaWrite(t, source, "UPDATE settings SET value='2' WHERE `key`='a'")
	source.replica.pin = 0
	source.replica.pinned = true
	revoked := false
	source.replica.revoke = func(context.Context) error { revoked = true; return nil }
	if err := source.replica.prune(ctx, time.Now().Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !revoked || source.replica.pinned {
		t.Fatal("expired pinned journal retained eligibility")
	}
	var count int
	source.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM replica_transactions").Scan(&count)
	if count != 1 {
		t.Fatal("current receipt must survive", count)
	}
}
func TestStandaloneDoesNotCreateReplicaTables(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := OpenMain(ctx, filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	replicaWrite(t, db, "INSERT INTO settings VALUES('a','1')")
	var count int
	db.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name LIKE 'replica_%'").Scan(&count)
	if count != 0 {
		t.Fatal("standalone HA overhead", count)
	}
}
