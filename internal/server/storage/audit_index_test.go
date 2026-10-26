package storage_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestAuditNewestFirstQueryUsesOrderingIndex(t *testing.T) {
	db, err := storage.Open(context.Background(), filepath.Join(privateDirectory(t), "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN SELECT id, actor_id, action, target_id, created_at, details FROM audit_events ORDER BY created_at DESC, id DESC LIMIT 50 OFFSET 0`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	usedIndex := false
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "audit_events_created_id") {
			usedIndex = true
		}
		if strings.Contains(detail, "USE TEMP B-TREE") {
			t.Fatalf("audit newest-first query still sorts into a temporary B-tree: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !usedIndex {
		t.Fatal("audit newest-first query did not use audit_events_created_id")
	}
}
