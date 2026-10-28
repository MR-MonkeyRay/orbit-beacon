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

func TestAdministratorVisibilityReadsAndMonotonicWatermark(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(ctx, filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`INSERT INTO users(id,username,password_hash,created_at) VALUES('a','a','hash','now'),('b','b','hash','now')`)
	if err != nil {
		t.Fatal(err)
	}
	s := inbox.New(db)
	a := inbox.Identity{UserID: "a"}
	b := inbox.Identity{UserID: "b"}
	now := time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC)
	add := func(id, kind string, at time.Time) {
		t.Helper()
		tx, e := db.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		e = inbox.Insert(ctx, tx, inbox.Event{ID: id, Kind: kind, SourceID: id, Transition: "succeeded", Severity: "info", CreatedAt: at, Details: json.RawMessage(`{"name":"safe snapshot"}`)})
		if e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(); e != nil {
			t.Fatal(e)
		}
	}
	first := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	task := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	late := "00000000-0000-4000-8000-000000000001"
	add(first, "alert", now)
	add(task, "task", now)
	for _, who := range []inbox.Identity{a, b} {
		for _, id := range []string{first, task} {
			notification, err := s.Detail(ctx, who, id)
			if err != nil || notification.ID != id || notification.Read {
				t.Fatalf("administrator %s detail=%+v err=%v", who.UserID, notification, err)
			}
		}
	}
	page, e := s.List(ctx, a, inbox.Filter{Limit: 1})
	if e != nil {
		t.Fatal(e)
	}
	if len(page.Notifications) != 1 || !page.HasMore || page.ReadThrough == nil || *page.ReadThrough != task {
		t.Fatalf("page=%+v", page)
	}
	for _, who := range []inbox.Identity{a, b} {
		sum, err := s.Summary(ctx, who)
		if err != nil || sum.UnreadCount != 2 {
			t.Fatalf("GET changed administrator %s reads: %+v err=%v", who.UserID, sum, err)
		}
	}
	add(late, "alert", now.Add(-time.Hour))
	if e = s.MarkAllRead(ctx, a, *page.ReadThrough, now); e != nil {
		t.Fatal(e)
	}
	sum, e := s.Summary(ctx, a)
	if e != nil || sum.UnreadCount != 1 || sum.LatestID == nil || *sum.LatestID != late {
		t.Fatalf("admin summary=%+v err=%v", sum, e)
	}
	sum, e = s.Summary(ctx, b)
	if e != nil || sum.UnreadCount != 3 {
		t.Fatalf("second admin=%+v err=%v", sum, e)
	}
	beforeOnly, e := s.List(ctx, b, inbox.Filter{Limit: 1, Before: page.NextCursor})
	if e != nil || len(beforeOnly.Notifications) != 1 || beforeOnly.Notifications[0].ID != task || !beforeOnly.HasMore {
		t.Fatalf("before-only page=%+v err=%v", beforeOnly, e)
	}
	unreadOnly, e := s.List(ctx, a, inbox.Filter{Limit: 10, Unread: true})
	if e != nil || len(unreadOnly.Notifications) != 1 || unreadOnly.Notifications[0].ID != late || unreadOnly.HasMore {
		t.Fatalf("unread-only page=%+v err=%v", unreadOnly, e)
	}
	page, e = s.List(ctx, b, inbox.Filter{Limit: 1, Unread: true, Before: page.NextCursor})
	if e != nil || len(page.Notifications) != 1 || page.Notifications[0].ID != task || !page.HasMore {
		t.Fatalf("filtered page=%+v err=%v", page, e)
	}
	if e = s.MarkRead(ctx, b, task, now); e != nil {
		t.Fatal(e)
	}
	if e = s.MarkRead(ctx, b, task, now.Add(time.Minute)); e != nil {
		t.Fatalf("idempotent read failed: %v", e)
	}
	if e = s.MarkRead(ctx, b, "cccccccc-cccc-4ccc-8ccc-cccccccccccc", now); e != inbox.ErrNotFound {
		t.Fatalf("nonexistent notification read: %v", e)
	}
	deleted := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	add(deleted, "alert", now)
	if _, e = db.Exec(`DELETE FROM inbox_notifications WHERE id=?`, deleted); e != nil {
		t.Fatal(e)
	}
	for _, through := range []string{"cccccccc-cccc-4ccc-8ccc-cccccccccccc", deleted} {
		if e = s.MarkAllRead(ctx, b, through, now); e != inbox.ErrNotFound {
			t.Fatalf("invalid/deleted watermark %s: %v", through, e)
		}
		sum, e = s.Summary(ctx, b)
		if e != nil || sum.UnreadCount != 2 {
			t.Fatalf("invalid watermark changed reads=%+v err=%v", sum, e)
		}
	}
	sum, e = s.Summary(ctx, a)
	if e != nil || sum.UnreadCount != 1 {
		t.Fatalf("second administrator changed first reads=%+v err=%v", sum, e)
	}
}

func TestInsertRollsBackWithSourceAndDeduplicatesTransition(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	db, e := storage.Open(ctx, filepath.Join(dir, "main.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	event := inbox.Event{ID: "10000000-0000-4000-8000-000000000001", Kind: "alert", SourceID: "episode", Transition: "firing", Severity: "warning", CreatedAt: time.Now(), Details: json.RawMessage(`{}`)}
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = inbox.Insert(ctx, tx, event); e != nil {
		t.Fatal(e)
	}
	if e = tx.Rollback(); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = db.QueryRow(`SELECT count(*) FROM inbox_notifications`).Scan(&n); e != nil || n != 0 {
		t.Fatalf("rollback count=%d err=%v", n, e)
	}
	for i := range 2 {
		tx, e = db.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		event.ID = "20000000-0000-4000-8000-00000000000" + string(rune('1'+i))
		if e = inbox.Insert(ctx, tx, event); e != nil {
			tx.Rollback()
			t.Fatal(e)
		}
		if e = tx.Commit(); e != nil {
			t.Fatal(e)
		}
	}
	if e = db.QueryRow(`SELECT count(*) FROM inbox_notifications`).Scan(&n); e != nil || n != 1 {
		t.Fatalf("duplicate transition count=%d err=%v", n, e)
	}
}
