package announcements

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
)

var testNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "announcements.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func patch(t *testing.T, raw string) Patch {
	t.Helper()
	var p Patch
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return p
}
func valid() Public { return Public{Level: "warning", Title: "Title", Body: "Body", StartsAt: testNow} }
func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Public)
		field  string
	}{
		{"level", func(p *Public) { p.Level = "bad" }, "level"},
		{"title empty", func(p *Public) { p.Title = "  " }, "title"},
		{"title bytes", func(p *Public) { p.Title = strings.Repeat("界", 34) }, "title"},
		{"title control", func(p *Public) { p.Title = "\tTitle" }, "title"},
		{"title utf8", func(p *Public) { p.Title = string([]byte{255}) }, "title"},
		{"body empty", func(p *Public) { p.Body = "\n " }, "body"},
		{"body bytes", func(p *Public) { p.Body = strings.Repeat("界", 334) }, "body"},
		{"body control", func(p *Public) { p.Body = "Body\x7f" }, "body"},
		{"body tab", func(p *Public) { p.Body = "\tBody" }, "body"},
		{"body cr", func(p *Public) { p.Body = "\rBody" }, "body"},
		{"body utf8", func(p *Public) { p.Body = string([]byte{255}) }, "body"},
		{"start", func(p *Public) { p.StartsAt = time.Time{} }, "starts_at"},
		{"equal end", func(p *Public) { end := testNow; p.EndsAt = &end }, "ends_at"},
		{"earlier end", func(p *Public) { end := testNow.Add(-time.Second); p.EndsAt = &end }, "ends_at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := valid()
			tc.change(&p)
			var e *validation.Error
			if !errors.As(Validate(&p), &e) || e.Fields[tc.field] == "" {
				t.Fatalf("expected %s: %v", tc.field, e)
			}
		})
	}
	for _, size := range []int{1000, 1001} {
		p := valid()
		p.Body = strings.Repeat("a", size)
		if err := Validate(&p); (err == nil) != (size == 1000) {
			t.Fatalf("body bytes %d: %v", size, err)
		}
	}
	for _, level := range []string{"info", "warning", "critical"} {
		p := valid()
		p.Level = level
		p.Title = " " + strings.Repeat("界", 33) + "a "
		p.Body = " \r\n" + strings.Repeat("界", 332) + "\r\nb \n"
		if err := Validate(&p); err != nil {
			t.Fatal(err)
		}
		if len(p.Title) != 100 || strings.Contains(p.Body, "\r") || !strings.Contains(p.Body, "\n") {
			t.Fatalf("normalization %+v", p)
		}
	}
}

func TestCRUDPatchAuditAtomicityAndCapacity(t *testing.T) {
	db := testDB(t)
	calls := 0
	clockCalls := 0
	s := New(mainHandle(t, db), func() time.Time { clockCalls++; return testNow }, func() {
		calls++
		var n int
		if err := db.QueryRow("SELECT count(*) FROM audit_events").Scan(&n); err != nil || n != calls {
			t.Errorf("callback before committed audit: %d %v", n, err)
		}
	})
	if clockCalls != 0 {
		t.Fatal("constructor called clock")
	}
	ctx := audit.WithSystemActor(context.Background(), "test")
	a, err := s.Save(ctx, "", patch(t, `{"level":"warning","title":" Title ","body":"one\r\ntwo","ends_at":"2027-01-01T00:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Active || a.Title != "Title" || a.Body != "one\ntwo" || !a.StartsAt.Equal(testNow) {
		t.Fatalf("created %+v", a)
	}
	a, err = s.Save(ctx, a.ID, patch(t, `{"title":"Changed"}`))
	if err != nil || a.EndsAt == nil || a.Body != "one\ntwo" {
		t.Fatalf("omitted patch: %+v %v", a, err)
	}
	a, err = s.Save(ctx, a.ID, patch(t, `{"ends_at":null}`))
	if err != nil || a.EndsAt != nil {
		t.Fatalf("null patch: %+v %v", a, err)
	}
	for _, field := range []string{"level", "title", "body", "starts_at"} {
		if _, err = s.Save(ctx, a.ID, patch(t, fmt.Sprintf(`{"%s":null}`, field))); err == nil {
			t.Fatalf("accepted null %s", field)
		}
	}
	if _, err = s.Save(ctx, a.ID, patch(t, "{\"title\":\""+string([]byte{255})+"\"}")); err == nil {
		t.Fatal("invalid UTF8 accepted")
	}
	if _, err = s.Save(ctx, "missing", Patch{}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err = s.Delete(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, a.ID, patch(t, `{"title":"rolled back"}`)); err == nil {
		t.Fatal("audit update succeeded")
	}
	if err = s.Delete(ctx, a.ID); err == nil {
		t.Fatal("audit delete succeeded")
	}
	if _, err = s.Save(ctx, "", patch(t, `{"level":"info","title":"rolled back","body":"body"}`)); err == nil {
		t.Fatal("audit create succeeded")
	}
	items, err := s.List(ctx)
	if err != nil || len(items) != 1 || items[0].Title != "Changed" || calls != 3 {
		t.Fatalf("rollback %+v %d %v", items, calls, err)
	}
	if _, err = db.Exec("DROP TRIGGER reject_audit"); err != nil {
		t.Fatal(err)
	}
	if err = s.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("delete invalidations=%d", calls)
	}
	s.invalidate = nil
	for i := 0; i < 100; i++ {
		_, err = s.Save(context.Background(), "", patch(t, `{"level":"info","title":"T","body":"B"}`))
		if err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err = s.Save(context.Background(), "", patch(t, `{"level":"info","title":"T","body":"B"}`)); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	// 重新打开已结束的公告同样不能超过容量。
	ended := timekey.Format(testNow.Add(-time.Hour))
	started := timekey.Format(testNow.Add(-2 * time.Hour))
	if _, err = db.Exec(`INSERT INTO announcements VALUES('ended','info','T','B',?,?,?,?)`, started, ended, started, started); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(context.Background(), "ended", patch(t, `{"ends_at":null}`)); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
}

func TestWindowsListPublicAndCleanup(t *testing.T) {
	db := testDB(t)
	s := New(mainHandle(t, db), func() time.Time { return testNow }, nil)
	ctx := context.Background()
	for _, end := range []string{"2026-11-05T12:00:00Z", "2026-11-05T11:00:00Z"} {
		if _, err := s.Save(ctx, "", patch(t, fmt.Sprintf(`{"level":"info","title":"T","body":"B","starts_at":"2026-01-01T00:00:00Z","ends_at":%q}`, end))); err == nil {
			t.Fatal("accepted ended creation")
		}
	}
	for i := 0; i < 9; i++ {
		level := []string{"info", "warning", "critical"}[i%3]
		start := testNow.Add(-time.Duration(i) * time.Hour)
		var end any
		if i == 7 {
			start = testNow.Add(time.Hour)
		}
		if i == 8 {
			end = timekey.Format(testNow)
		}
		_, err := db.Exec(`INSERT INTO announcements VALUES(?,?,?,?,?,?,?,?)`, fmt.Sprint(i), level, fmt.Sprint(i), "B", timekey.Format(start), end, timekey.Format(testNow), timekey.Format(testNow))
		if err != nil {
			t.Fatal(err)
		}
	}
	public, err := ListPublic(ctx, mainHandle(t, db), testNow)
	if err != nil {
		t.Fatal(err)
	}
	// Critical 2,5; warning 1,4; then info 0. Scheduled 7 and ended 8 excluded.
	if len(public) != 5 {
		t.Fatalf("%+v", public)
	}
	for i, want := range []string{"2", "5", "1", "4", "0"} {
		if public[i].Title != want {
			t.Fatalf("order %+v", public)
		}
	}
	raw, _ := json.Marshal(public)
	var wire []map[string]any
	json.Unmarshal(raw, &wire)
	for _, item := range wire {
		if len(item) != 5 {
			t.Fatalf("public keys %s", raw)
		}
	}
	items, err := s.List(ctx)
	if err != nil || len(items) != 8 || items[0].Active {
		t.Fatalf("admin list %+v %v", items, err)
	}
	if _, err = s.Cleanup(ctx, testNow, 0); err == nil {
		t.Fatal("invalid cleanup limit")
	}
	old := timekey.Format(testNow.Add(-100 * 24 * time.Hour))
	older := timekey.Format(testNow.Add(-101 * 24 * time.Hour))
	if _, err = db.Exec(`INSERT INTO announcements VALUES('old','info','old','body',?,?,?,?)`, older, old, older, older); err != nil {
		t.Fatal(err)
	}
	cutoff := testNow.Add(-90 * 24 * time.Hour)
	if _, err = db.Exec(`INSERT INTO announcements VALUES('edge','info','edge','body',?,?,?,?)`, older, timekey.Format(cutoff), older, older); err != nil {
		t.Fatal(err)
	}
	n, err := s.Cleanup(ctx, cutoff, 10)
	if err != nil || n != 1 {
		t.Fatalf("cleanup %d %v", n, err)
	}
	var count int
	db.QueryRow("SELECT count(*) FROM announcements WHERE ends_at IS NULL").Scan(&count)
	if count != 8 {
		t.Fatal(count)
	}
}

func TestPublicAnnouncementSortBreaksTiesByID(t *testing.T) {
	db := testDB(t)
	stamp := timekey.Format(testNow)
	for _, id := range []string{"b", "a"} {
		if _, err := db.Exec(`INSERT INTO announcements VALUES(?,'critical',?,'B',?,NULL,?,?)`, id, id, stamp, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	items, err := ListPublic(context.Background(), mainHandle(t, db), testNow)
	if err != nil || len(items) != 2 || items[0].Title != "a" || items[1].Title != "b" {
		t.Fatalf("tie order %+v %v", items, err)
	}
}
