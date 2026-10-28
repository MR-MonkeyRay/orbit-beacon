package server_test

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"
	"time"

	"orbitbeacon/internal/timekey"
)

func TestManagementListPaginationEnvelopes(t *testing.T) {
	f := newHTTP(t, 200)
	admin, _ := login(t, f, "admin")

	t.Run("nodes", func(t *testing.T) {
		assertPageEnvelope(t, managementPage(t, f, admin, "/api/v1/nodes?limit=2"), "nodes", 0, false, 0)
		insertPaginationNodes(t, f.db, 3)
		assertPageEnvelope(t, managementPage(t, f, admin, "/api/v1/nodes?limit=2"), "nodes", 2, true, 2)
		assertPageEnvelope(t, managementPage(t, f, admin, "/api/v1/nodes?limit=2&offset=1"), "nodes", 2, false, 0)

		if _, err := f.db.Exec("DELETE FROM nodes"); err != nil {
			t.Fatal(err)
		}
		insertPaginationNodes(t, f.db, 101)
		// The maximum-size page still permits a single-query lookahead row.
		assertPageEnvelope(t, managementPage(t, f, admin, "/api/v1/nodes?limit=100"), "nodes", 100, true, 100)
		assertPageEnvelope(t, managementPage(t, f, admin, "/api/v1/nodes?limit=100&offset=1"), "nodes", 100, false, 0)
	})

	t.Run("enrollments", func(t *testing.T) {
		assertPageEnvelope(t, managementPage(t, f, admin, "/api/v1/enrollments?limit=2"), "enrollments", 0, false, 0)
		insertPaginationEnrollments(t, f.db, 3)
		assertPageEnvelope(t, managementPage(t, f, admin, "/api/v1/enrollments?limit=2"), "enrollments", 2, true, 2)
		assertPageEnvelope(t, managementPage(t, f, admin, "/api/v1/enrollments?limit=2&offset=1"), "enrollments", 2, false, 0)

		if _, err := f.db.Exec("DELETE FROM enrollment_tokens"); err != nil {
			t.Fatal(err)
		}
		insertPaginationEnrollments(t, f.db, 101)
		assertPageEnvelope(t, managementPage(t, f, admin, "/api/v1/enrollments?limit=100"), "enrollments", 100, true, 100)
		assertPageEnvelope(t, managementPage(t, f, admin, "/api/v1/enrollments?limit=100&offset=1"), "enrollments", 100, false, 0)
	})

	t.Run("audit", func(t *testing.T) {
		clearAuditEvents(t, f.db)
		assertAuditPageEnvelope(t, managementPage(t, f, admin, "/api/v1/audit?limit=2"), 0, false, 0)
		insertPaginationAuditEvents(t, f.db, 3)
		assertAuditPageEnvelope(t, managementPage(t, f, admin, "/api/v1/audit?limit=2"), 2, true, 2)
		assertAuditPageEnvelope(t, managementPage(t, f, admin, "/api/v1/audit?limit=2&offset=1"), 2, false, 0)
	})
}

func TestAuditCursorPaginationIsStableAcrossNewEvents(t *testing.T) {
	f := newHTTP(t, 5)
	admin, _ := login(t, f, "admin")
	clearAuditEvents(t, f.db)

	stamp := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	original := []struct {
		id      string
		created time.Time
	}{
		{"00000000-0000-4000-8000-000000000005", stamp},
		{"00000000-0000-4000-8000-000000000004", stamp},
		{"00000000-0000-4000-8000-000000000003", stamp},
		{"00000000-0000-4000-8000-000000000002", stamp.Add(-time.Second)},
		{"00000000-0000-4000-8000-000000000001", stamp.Add(-2 * time.Second)},
	}
	for _, event := range original {
		insertPaginationAuditEvent(t, f.db, event.id, event.created)
	}

	first := managementPage(t, f, admin, "/api/v1/audit?limit=2")
	assertAuditPageEnvelope(t, first, 2, true, 2)
	firstIDs := pageIDs(t, first, "events")
	assertIDs(t, firstIDs, []string{original[0].id, original[1].id})
	firstCursor := pageCursor(t, first)

	insertPaginationAuditEvent(t, f.db, "00000000-0000-4000-8000-000000000006", stamp.Add(time.Second))
	second := managementPage(t, f, admin, "/api/v1/audit?limit=2&offset=999&before="+firstCursor)
	assertAuditPageEnvelope(t, second, 2, true, 2)
	secondIDs := pageIDs(t, second, "events")
	assertIDs(t, secondIDs, []string{original[2].id, original[3].id})

	third := managementPage(t, f, admin, "/api/v1/audit?limit=2&before="+pageCursor(t, second))
	assertAuditPageEnvelope(t, third, 1, false, 0)
	thirdIDs := pageIDs(t, third, "events")
	assertIDs(t, thirdIDs, []string{original[4].id})

	all := append(append(firstIDs, secondIDs...), thirdIDs...)
	want := []string{original[0].id, original[1].id, original[2].id, original[3].id, original[4].id}
	assertIDs(t, all, want)
}

func TestAuditBeforeCursorValidation(t *testing.T) {
	f := newHTTP(t, 5)
	admin, _ := login(t, f, "admin")
	clearAuditEvents(t, f.db)
	stamp := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 2; i++ {
		insertPaginationAuditEvent(t, f.db, fmt.Sprintf("00000000-0000-4000-8000-%012d", i), stamp)
	}
	valid := pageCursor(t, managementPage(t, f, admin, "/api/v1/audit?limit=1"))
	invalidTimestamp := base64.RawURLEncoding.EncodeToString([]byte("2026-99-27T12:00:00.000000000Z|00000000-0000-4000-8000-000000000001"))
	invalidUUID := base64.RawURLEncoding.EncodeToString([]byte("2026-10-27T12:00:00.000000000Z|00000000-0000-4000-8000-00000000000g"))
	invalidUUIDVersion := base64.RawURLEncoding.EncodeToString([]byte("2026-10-27T12:00:00.000000000Z|00000000-0000-0000-8000-000000000001"))
	noncanonicalPadBits := valid[:len(valid)-1] + string(valid[len(valid)-1]+1)
	for name, cursor := range map[string]string{
		"empty":                 "",
		"not base64url":         "not+a+cursor",
		"padded":                valid + "=",
		"invalid timestamp":     invalidTimestamp,
		"invalid uuid":          invalidUUID,
		"invalid uuid version":  invalidUUIDVersion,
		"noncanonical pad bits": noncanonicalPadBits,
		"trailing garbage":      valid + "A",
	} {
		t.Run(name, func(t *testing.T) {
			apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/audit?before="+cursor, "", admin, "", ""), http.StatusBadRequest, "validation_failed")
		})
	}
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/audit?limit=1&offset=-1&before="+valid, "", admin, "", ""), http.StatusBadRequest, "validation_failed")
}

func managementPage(t *testing.T, f fixture, cookie *http.Cookie, path string) map[string]any {
	t.Helper()
	response := request(t, f.handler, http.MethodGet, path, "", cookie, "", "")
	assertHTTPStatus(t, response, http.StatusOK)
	return decodeObject(t, response)
}

func assertPageEnvelope(t *testing.T, page map[string]any, key string, count int, hasMore bool, nextOffset int) {
	t.Helper()
	items, ok := page[key].([]any)
	if !ok {
		t.Fatalf("page lacks %q array: %+v", key, page)
	}
	if len(items) != count {
		t.Errorf("%s count = %d, want %d", key, len(items), count)
	}
	if got, ok := page["has_more"].(bool); !ok || got != hasMore {
		t.Errorf("has_more = %v, want %t", page["has_more"], hasMore)
	}
	gotOffset, exists := page["next_offset"]
	if !hasMore {
		if exists {
			t.Errorf("next_offset present on final page: %v", gotOffset)
		}
		return
	}
	if got, ok := gotOffset.(float64); !ok || got != float64(nextOffset) {
		t.Errorf("next_offset = %v, want %d", gotOffset, nextOffset)
	}
}

func assertAuditPageEnvelope(t *testing.T, page map[string]any, count int, hasMore bool, nextOffset int) {
	t.Helper()
	assertPageEnvelope(t, page, "events", count, hasMore, nextOffset)
	cursor, exists := page["next_cursor"]
	if !hasMore {
		if exists {
			t.Errorf("next_cursor present on final page: %v", cursor)
		}
		return
	}
	if value, ok := cursor.(string); !ok || len(value) != 90 {
		t.Errorf("next_cursor = %v, want 90-character opaque string", cursor)
	}
}

func pageCursor(t *testing.T, page map[string]any) string {
	t.Helper()
	cursor, ok := page["next_cursor"].(string)
	if !ok || cursor == "" {
		t.Fatalf("page lacks next_cursor: %+v", page)
	}
	return cursor
}

func pageIDs(t *testing.T, page map[string]any, key string) []string {
	t.Helper()
	items, ok := page[key].([]any)
	if !ok {
		t.Fatalf("page lacks %q array: %+v", key, page)
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s item is not an object: %v", key, item)
		}
		ids = append(ids, objectID(t, object))
	}
	return ids
}

func assertIDs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("IDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("IDs = %v, want %v", got, want)
		}
	}
}

func insertPaginationNodes(t *testing.T, db *sql.DB, count int) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 1; i <= count; i++ {
		id := fmt.Sprintf("10000000-0000-4000-8000-%012d", i)
		created := time.Date(2026, 10, 27, 12, 0, i%60, i, time.UTC)
		if _, err := tx.Exec("INSERT INTO nodes(id,name,created_at) VALUES(?,?,?)", id, fmt.Sprintf("node-%03d", i), timekey.Format(created)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func insertPaginationEnrollments(t *testing.T, db *sql.DB, count int) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 1; i <= count; i++ {
		id := fmt.Sprintf("20000000-0000-4000-8000-%012d", i)
		expires := time.Date(2026, 10, 27, 13, 0, i, 0, time.UTC)
		if _, err := tx.Exec("INSERT INTO enrollment_tokens(id,token_hash,expires_at) VALUES(?,?,?)", id, []byte(fmt.Sprintf("ticket-%03d", i)), timekey.Format(expires)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func insertPaginationAuditEvents(t *testing.T, db *sql.DB, count int) {
	t.Helper()
	stamp := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= count; i++ {
		insertPaginationAuditEvent(t, db, fmt.Sprintf("30000000-0000-4000-8000-%012d", i), stamp.Add(time.Duration(i)*time.Second))
	}
}

func insertPaginationAuditEvent(t *testing.T, db *sql.DB, id string, created time.Time) {
	t.Helper()
	if _, err := db.Exec("INSERT INTO audit_events(id,action,created_at,details) VALUES(?,?,?,'{}')", id, "pagination.test", timekey.Format(created)); err != nil {
		t.Fatal(err)
	}
}

func clearAuditEvents(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("DELETE FROM audit_events"); err != nil {
		t.Fatal(err)
	}
}
