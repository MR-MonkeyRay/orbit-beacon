package storage

import (
	"context"
	"database/sql"
	_ "modernc.org/sqlite"
	"testing"
)

func TestAnnouncementsSchema(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err = initializeMainSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if CurrentSchemaVersion != 2 {
		t.Fatal("unexpected schema baseline")
	}
	for _, values := range []struct {
		level, start string
		end          any
		valid        bool
	}{
		{"info", "2026-01-01", nil, true}, {"warning", "2026-01-01", "2026-02-01", true}, {"critical", "2026-01-01", nil, true},
		{"bad", "2026-01-01", nil, false}, {"info", "2026-01-01", "2026-01-01", false}, {"info", "2026-01-01", "2025-01-01", false},
	} {
		_, err = db.Exec(`INSERT INTO announcements VALUES(lower(hex(randomblob(16))),?,'T','B',?,?,'2026-01-01','2026-01-01')`, values.level, values.start, values.end)
		if (err == nil) != values.valid {
			t.Fatalf("%+v err=%v", values, err)
		}
	}
	var index int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name='announcements_window'").Scan(&index); err != nil || index != 1 {
		t.Fatalf("index %d %v", index, err)
	}
}
