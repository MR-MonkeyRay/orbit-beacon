package ingest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	_ "modernc.org/sqlite"
	"orbitbeacon/internal/server/announcements"
	"orbitbeacon/internal/timekey"
)

func TestAnnouncementsRealSQLitePublicCacheAndReader(t *testing.T) {
	ctx := context.Background()
	f := newCoalesceFixture(t, time.Second)
	db := f.db
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	ns := f.nodes
	s := NewService(nil, Options{})
	svc := announcements.New(ns.Database(), clock, ns.InvalidatePublicSafety)
	read := func(offset int) PublicPage {
		t.Helper()
		p, e := s.PublicPage(ctx, ns, 10, offset, time.Minute, clock)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	p := read(0)
	if p.Announcements == nil || len(p.Announcements) != 0 {
		t.Fatal(p)
	}
	var patch announcements.Patch
	json.Unmarshal([]byte(`{"level":"warning","title":"first","body":"body"}`), &patch)
	version, wake := ns.PublicSafetyChanges()
	a, err := svc.Save(ctx, "", patch)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("stream not notified")
	}
	if ns.PublicSafetyVersion() == version {
		t.Fatal("safety version unchanged")
	}
	if p = read(0); len(p.Announcements) != 1 {
		t.Fatalf("cache not invalidated %+v", p)
	}
	// 翻页不能让维护公告消失。
	p = read(10)
	if len(p.Announcements) != 1 || p.Announcements[0].Title != "first" {
		t.Fatalf("later page lost announcements: %+v", p.Announcements)
	}
	// Scheduled start and exact end become visible at the cache TTL boundary.
	start := now.Add(time.Minute)
	end := start.Add(time.Minute)
	if _, err = db.Exec(`UPDATE announcements SET starts_at=?,ends_at=? WHERE id=?`, timekey.Format(start), timekey.Format(end), a.ID); err != nil {
		t.Fatal(err)
	}
	ns.InvalidatePublic()
	if p = read(0); len(p.Announcements) != 0 {
		t.Fatal("future visible")
	}
	now = start
	if p = read(0); len(p.Announcements) != 1 {
		t.Fatal("start == now hidden")
	}
	now = end
	if p = read(0); len(p.Announcements) != 0 {
		t.Fatal("end == now visible")
	}
}
