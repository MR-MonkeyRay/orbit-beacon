package inbox_test

import (
	"context"
	"encoding/json"
	"errors"

	"testing"
	"time"

	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLInboxSequenceThroughAndRollback(t *testing.T) {

	f := storagetest.Start(t)
	db := f.Open(t, storage.MainDomain)
	raw := f.Raw(t, storage.MainDomain)
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 2, 3, 4, 987654321, time.UTC)
	if _, err := raw.Exec(`INSERT INTO users(id,username,password_hash,created_at) VALUES('reader','reader','hash','now')`); err != nil {
		t.Fatal(err)
	}
	add := func(id string, at time.Time, commit bool) {
		t.Helper()
		lease, err := db.Acquire(ctx, storage.Writer)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		tx, err := lease.BeginWrite(lease.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := inbox.Insert(lease.Context(), tx, inbox.Event{ID: id, Kind: "alert", SourceID: id, Transition: "firing", Severity: "warning", CreatedAt: at, Details: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if commit {
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	first := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	late := "00000000-0000-4000-8000-000000000001"
	add(first, now, true)
	add(first, now, true)
	add("00000000-0000-4000-8000-000000000002", now, false)
	s := inbox.New(db)
	who := inbox.Identity{UserID: "reader"}
	page, err := s.List(ctx, who, inbox.Filter{Limit: 10})
	if err != nil || page.ReadThrough == nil || *page.ReadThrough != first || len(page.Notifications) != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	add(late, now.Add(-time.Hour), true)
	if err := s.MarkAllRead(ctx, who, first, now); err != nil {
		t.Fatal(err)
	}
	sum, err := s.Summary(ctx, who)
	if err != nil || sum.UnreadCount != 1 || sum.LatestID == nil || *sum.LatestID != late {
		t.Fatalf("summary=%+v err=%v", sum, err)
	}
	if err := s.MarkRead(ctx, who, late, now); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRead(ctx, who, late, now); err != nil {
		t.Fatal(err)
	}
	var counter int64
	if err := raw.QueryRow(`SELECT value FROM inbox_sequence WHERE id=1`).Scan(&counter); err != nil || counter != 2 {
		t.Fatalf("counter=%d err=%v", counter, err)
	}
	if _, err := s.Cleanup(ctx, now.Add(time.Hour), 10); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAllRead(ctx, who, first, now); !errors.Is(err, inbox.ErrNotFound) {
		t.Fatal(err)
	}
}
