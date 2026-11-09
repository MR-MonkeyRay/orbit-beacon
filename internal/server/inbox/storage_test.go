package inbox_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/storage"
)

func TestStorageProjectionInsertIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := storage.Open(ctx, filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	event := inbox.Event{Kind: "storage", SourceID: "00000000-0000-4000-8000-000000000001", Transition: "manual_required", Severity: "critical", CreatedAt: time.Now(), Details: json.RawMessage(`{"domain":"main","transition":"manual_required","active_role":null,"reason_code":"recovery_timeout","occurred_at":"2026-11-07T12:00:00Z"}`)}
	for range 2 {
		tx, err := beginMainWrite(t, raw, ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = inbox.Insert(ctx, tx, event); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	var count, value int
	if err = raw.QueryRow(`SELECT count(*) FROM inbox_notifications WHERE kind='storage'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if err = raw.QueryRow(`SELECT value FROM inbox_sequence WHERE id=1`).Scan(&value); err != nil || value != 1 {
		t.Fatalf("sequence=%d err=%v", value, err)
	}
}

func TestStorageDetailsRequireValidatedFixedMetadata(t *testing.T) {
	valid := `{"domain":"main","transition":"manual_required","active_role":null,"reason_code":"recovery_timeout","occurred_at":"2026-11-07T12:00:00Z"}`
	if err := inbox.ValidateDetails("storage", json.RawMessage(valid)); err != nil {
		t.Fatal(err)
	}
	for _, patch := range []string{`{}`, `{"domain":"other"}`, `{"transition":"firing"}`, `{"active_role":"other"}`, `{"occurred_at":"tomorrow"}`, `{"reason_code":null}`, `{"url":"https://secret"}`} {
		var fields, changes map[string]json.RawMessage
		json.Unmarshal([]byte(valid), &fields)
		json.Unmarshal([]byte(patch), &changes)
		if patch == `{}` {
			delete(fields, "domain")
		} else {
			for k, v := range changes {
				fields[k] = v
			}
		}
		raw, _ := json.Marshal(fields)
		if err := inbox.ValidateDetails("storage", raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
