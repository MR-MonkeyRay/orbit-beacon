package backup

import (
	"context"
	"encoding/json"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/storage"
	"path/filepath"
	"testing"
	"time"
)

func TestRestoreSequenceCounterIncludesImportedAndGeneratedNotifications(t *testing.T) {
	ctx := context.Background()
	source := restoreFixture(t, "source")
	db := writableRestoreFixture(t, source)
	_, err := db.Exec(`INSERT INTO inbox_notifications(sequence,id,kind,source_id,transition,severity,created_at,details_json) VALUES(50,'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','alert','old','resolved','info','2026-10-01T00:00:00.000000000Z','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	archive, password := restoreArchive(t, source, []Category{Events})
	target := restoreFixture(t, "target")
	if _, err = Restore(ctx, restoreConfig(target), archive, password, true); err != nil {
		t.Fatal(err)
	}
	restored, err := storage.Open(ctx, filepath.Join(target, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var maximum, counter int64
	if err = restored.QueryRow(`SELECT max(sequence) FROM inbox_notifications`).Scan(&maximum); err != nil {
		t.Fatal(err)
	}
	if err = restored.QueryRow(`SELECT value FROM inbox_sequence WHERE id=1`).Scan(&counter); err != nil || counter != maximum {
		t.Fatalf("counter=%d max=%d err=%v", counter, maximum, err)
	}
	handle := mainHandle(t, restored)
	lease, err := handle.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		lease.Close()
		t.Fatal(err)
	}
	if err = inbox.Insert(lease.Context(), tx, inbox.Event{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Kind: "alert", SourceID: "new", Transition: "resolved", Severity: "info", CreatedAt: time.Now().UTC(), Details: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	lease.Close()
	var next int64
	if err = restored.QueryRow(`SELECT sequence FROM inbox_notifications WHERE id='bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'`).Scan(&next); err != nil || next <= maximum {
		t.Fatalf("next=%d max=%d err=%v", next, maximum, err)
	}
}
