package storage_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLMaintenancePreservesRowsAndQuotedIdentifiers(t *testing.T) {
	f := storagetest.Start(t)
	for _, domain := range []storage.Domain{storage.MainDomain, storage.HistoryDomain} {
		t.Run(string(domain), func(t *testing.T) {
			db := f.Open(t, domain)
			raw := f.Raw(t, domain)
			for _, query := range []string{
				"CREATE TABLE `compact``canary`(id BIGINT PRIMARY KEY, value LONGBLOB NOT NULL) ENGINE=InnoDB",
				"INSERT INTO `compact``canary` VALUES(1, x'0001FF')",
			} {
				if _, err := raw.ExecContext(t.Context(), query); err != nil {
					t.Fatal(err)
				}
			}
			lease, err := db.Acquire(t.Context(), storage.Writer)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			if err = lease.Compact(t.Context()); err != nil {
				t.Fatal(err)
			}
			var value []byte
			if err = raw.QueryRowContext(t.Context(), "SELECT value FROM `compact``canary` WHERE id=1").Scan(&value); err != nil || string(value) != "\x00\x01\xff" {
				t.Fatalf("compaction changed binary row: %x %v", value, err)
			}
		})
	}
}

func TestMySQLMaintenanceLongWaitAndTableError(t *testing.T) {
	f := storagetest.Start(t)
	db := f.Open(t, storage.HistoryDomain)
	raw := f.Raw(t, storage.HistoryDomain)
	for _, query := range []string{
		"CREATE TABLE `00_compact_gate`(id BIGINT PRIMARY KEY) ENGINE=InnoDB",
		"INSERT INTO `00_compact_gate` VALUES(7)",
		"CREATE TABLE `01_compact_gone`(id BIGINT PRIMARY KEY) ENGINE=InnoDB",
	} {
		if _, err := raw.ExecContext(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	blocker, err := raw.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	var id int
	if err = blocker.QueryRowContext(ctx, "SELECT id FROM `00_compact_gate`").Scan(&id); err != nil {
		t.Fatal(err)
	}
	lease, err := db.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	done := make(chan error, 1)
	go func() { done <- lease.Compact(ctx) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for waiting := false; !waiting; {
		select {
		case err := <-done:
			t.Fatalf("compaction ended before the metadata lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
			var count int
			if err = raw.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE DB=DATABASE() AND INFO LIKE 'OPTIMIZE %' AND STATE='Waiting for table metadata lock'").Scan(&count); err != nil {
				t.Fatal(err)
			}
			waiting = count > 0
		}
	}
	// A maintenance statement must outlive the normal five-second query read timeout.
	timer := time.NewTimer(6 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		t.Fatalf("maintenance inherited the ordinary query timeout: %v", err)
	case <-timer.C:
	}
	if _, err = raw.ExecContext(ctx, "DROP TABLE `01_compact_gone`"); err != nil {
		t.Fatal(err)
	}
	if err = blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("OPTIMIZE error result was reported as success")
		}
		if strings.Contains(err.Error(), "01_compact_gone") {
			t.Fatalf("maintenance exposed a table diagnostic: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !db.Ready() {
		t.Fatal("a table maintenance error made the database unavailable")
	}
	if err = raw.QueryRowContext(ctx, "SELECT id FROM `00_compact_gate`").Scan(&id); err != nil || id != 7 {
		t.Fatalf("earlier table lost data: %d %v", id, err)
	}
}
