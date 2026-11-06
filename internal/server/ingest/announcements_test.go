package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	_ "modernc.org/sqlite"
	"orbitbeacon/internal/server/announcements"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

func TestAnnouncementsRealSQLitePublicCacheAndReader(t *testing.T) {
	ctx := context.Background()
	dsn := "file:orbit-c2-public-announcements?mode=memory&cache=shared"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE announcements(id TEXT PRIMARY KEY,level TEXT,title TEXT,body TEXT,starts_at TEXT,ends_at TEXT,created_at TEXT,updated_at TEXT);
 CREATE TABLE nodes(id TEXT,created_at TEXT,status TEXT,token_hash BLOB,first_enrolled_at TEXT,public_settings TEXT,last_seen TEXT,credential_epoch INTEGER,run_generation TEXT,report_session_id TEXT);
 CREATE TABLE node_availability_daily(node_id TEXT,day TEXT,online_seconds INTEGER,offline_seconds INTEGER);`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	ns := nodes.NewService(nil, nodes.Options{})
	connector := &availabilityCountingConnector{driver: db.Driver(), dsn: dsn}
	reader := sql.OpenDB(connector)
	defer reader.Close()
	ns.SetPublicReader(reader) // 选错连接池会直接解引用 nil。
	s := NewService(nil, Options{})
	svc := announcements.New(db, clock, ns.InvalidatePublicSafety)
	read := func(offset int) PublicPage {
		t.Helper()
		p, e := s.PublicPage(ctx, ns, 10, offset, time.Minute, clock)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	p := read(0)
	if got := connector.queries.Load(); got != 2 {
		t.Fatalf("first page queries=%d", got)
	}
	read(0)
	if got := connector.queries.Load(); got != 2 {
		t.Fatalf("cache hit queries=%d", got)
	}
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
	before := connector.queries.Load()
	// 翻页不能让维护公告消失。
	p = read(10)
	if got := connector.queries.Load(); got != before+2 {
		t.Fatalf("later page queries=%d", got-before)
	}
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
