package storage_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLReplicaTriggerLimitIsTypedAndAtomic(t *testing.T) {
	f := storagetest.Start(t)
	ctx := context.Background()
	standby := f.Endpoint("standby")
	cfg := config.DatabaseConfig{Primary: f.Endpoint("primary"), Standby: &standby}
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	dbs, err := storage.OpenDatabases(ctx, cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dbs.Close()
	l, err := dbs.Main.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	tx, err := l.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO settings VALUES(?,?)")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	value := strings.Repeat("x", 256<<10)
	for i := range 140 {
		_, err = stmt.ExecContext(ctx, fmt.Sprint(i), value)
		if err != nil {
			break
		}
	}
	if !errors.Is(err, storage.ErrTransactionTooLarge) {
		t.Fatalf("typed trigger error: %v", err)
	}
	if err = tx.Commit(); !errors.Is(err, storage.ErrTransactionTooLarge) {
		t.Fatal("oversized transaction committed", err)
	}
	var rows, logs, head int
	if err = f.RawEndpoint(t, "primary", storage.MainDomain).QueryRow("SELECT (SELECT count(*) FROM settings),(SELECT count(*) FROM replica_transactions),head_seq FROM replica_state").Scan(&rows, &logs, &head); err != nil || rows != 0 || logs != 0 || head != 0 {
		t.Fatalf("partial transaction rows=%d logs=%d head=%d err=%v", rows, logs, head, err)
	}
}

func TestMySQLSQLiteMySQLStrictReverseCascade(t *testing.T) {
	f := storagetest.Start(t)
	ctx := context.Background()
	remote := f.Open(t, storage.MainDomain)
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	local, err := storage.OpenMain(ctx, filepath.Join(dir, "middle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if err = storage.ReplicaTestConnect(ctx, remote, local, config.DatabaseEndpoint{Driver: "sqlite"}); err != nil {
		t.Fatal(err)
	}
	haWrite(t, remote, func(tx *storage.Tx) {
		for _, q := range []string{"INSERT INTO `groups`(id,name) VALUES('g','组')", "INSERT INTO nodes(id,name,group_id,created_at) VALUES('n','节点','g','now')", "INSERT INTO enrollment_tokens(id,token_hash,node_id,group_id,expires_at) VALUES('e',X'01','n','g','later')", "INSERT INTO enrollment_claims VALUES('e','i','n',X'02','now')", "INSERT INTO settings VALUES('same','initial')"} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err = storage.ReplicaTestDrain(ctx, remote, local); err != nil {
		t.Fatal(err)
	}
	if err = storage.ReplicaTestConnect(ctx, local, remote, f.Endpoint("primary")); err != nil {
		t.Fatal(err)
	}
	haWrite(t, local, func(tx *storage.Tx) {
		for _, q := range []string{"UPDATE settings SET value='first' WHERE `key`='same'", "UPDATE settings SET value='second' WHERE `key`='same'", "UPDATE settings SET value=value WHERE `key`='same'", "DELETE FROM `groups` WHERE id='g'", "DELETE FROM nodes WHERE id='n'"} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err = storage.ReplicaTestDrain(ctx, local, remote); err != nil {
		t.Fatal(err)
	}
	raw := f.RawEndpoint(t, "primary", storage.MainDomain)
	var count int
	var value string
	if err = raw.QueryRow("SELECT count(*) FROM enrollment_claims").Scan(&count); err != nil || count != 0 {
		t.Fatal("reverse cascade", count, err)
	}
	if err = raw.QueryRow("SELECT value FROM settings WHERE `key`='same'").Scan(&value); err != nil || value != "second" {
		t.Fatal(value, err)
	}
	// 回到 MySQL 源，再确认无变化 UPDATE 在真实 multiStatements 目标连接上不被误判。
	if err = storage.ReplicaTestConnect(ctx, remote, local, config.DatabaseEndpoint{Driver: "sqlite"}); err != nil {
		t.Fatal(err)
	}
	haWrite(t, remote, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(ctx, "UPDATE settings SET value=value WHERE `key`='same'"); err != nil {
			t.Fatal(err)
		}
	})
	if err = storage.ReplicaTestDrain(ctx, remote, local); err != nil {
		t.Fatal(err)
	}
}
