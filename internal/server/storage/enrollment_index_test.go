package storage_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestEnrollmentListUsesOrderingIndexAndForeignKeyIndexesExist(t *testing.T) {
	db, err := storage.Open(context.Background(), filepath.Join(privateDirectory(t), "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN SELECT id, node_id, group_id, expires_at, max_uses, uses, revoked_at FROM enrollment_tokens ORDER BY expires_at DESC, id LIMIT 100 OFFSET 0`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	usedOrdering := false
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "enrollment_tokens_expiry_id") {
			usedOrdering = true
		}
		if strings.Contains(detail, "USE TEMP B-TREE") {
			t.Fatalf("enrollment list still sorts into a temporary B-tree: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !usedOrdering {
		t.Fatal("enrollment list did not use enrollment_tokens_expiry_id")
	}
	for _, name := range []string{"enrollment_tokens_node", "enrollment_tokens_group", "enrollment_claims_node", "nodes_group", "update_job_nodes_node"} {
		var count int
		if err := db.QueryRowContext(context.Background(), "SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?", name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("missing foreign-key index %s", name)
		}
	}
}
