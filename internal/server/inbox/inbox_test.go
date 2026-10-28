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

func TestVisibilityReadsAndMonotonicWatermark(t *testing.T) {
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
	_, err = db.Exec(`INSERT INTO users(id,username,password_hash,role,created_at) VALUES('a','a','hash','admin','now'),('b','b','hash','admin','now'),('v','v','hash','viewer','now')`)
	if err != nil {
		t.Fatal(err)
	}
	s := inbox.New(db)
	a := inbox.Identity{UserID: "a", Admin: true}
	b := inbox.Identity{UserID: "b", Admin: true}
	v := inbox.Identity{UserID: "v"}
	now := time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC)
	add := func(id, kind, audience string, at time.Time) {
		t.Helper()
		tx, e := db.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		e = inbox.Insert(ctx, tx, inbox.Event{ID: id, Kind: kind, SourceID: id, Transition: "succeeded", Audience: audience, Severity: "info", CreatedAt: at, Details: json.RawMessage(`{"name":"safe snapshot"}`)})
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
	add(first, "alert", "authenticated", now)
	add(task, "task", "admin", now)
	page, e := s.List(ctx, a, inbox.Filter{Limit: 1})
	if e != nil {
		t.Fatal(e)
	}
	if len(page.Notifications) != 1 || !page.HasMore || page.ReadThrough == nil || *page.ReadThrough != task {
		t.Fatalf("page=%+v", page)
	}
	add(late, "alert", "authenticated", now.Add(-time.Hour))
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
	sum, e = s.Summary(ctx, v)
	if e != nil || sum.UnreadCount != 2 {
		t.Fatalf("viewer=%+v err=%v", sum, e)
	}
	if _, e = s.Detail(ctx, v, task); e != inbox.ErrNotFound {
		t.Fatalf("viewer read task=%v", e)
	}
	if e = s.MarkRead(ctx, v, task, now); e != inbox.ErrNotFound {
		t.Fatalf("viewer marked task=%v", e)
	}
	if e = s.MarkAllRead(ctx, v, task, now); e != inbox.ErrNotFound {
		t.Fatalf("viewer used private watermark=%v", e)
	}
	sum, e = s.Summary(ctx, v)
	if e != nil || sum.UnreadCount != 2 {
		t.Fatalf("invalid watermark changed reads=%+v err=%v", sum, e)
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
	event := inbox.Event{ID: "10000000-0000-4000-8000-000000000001", Kind: "alert", SourceID: "episode", Transition: "firing", Audience: "authenticated", Severity: "warning", CreatedAt: time.Now(), Details: json.RawMessage(`{}`)}
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
