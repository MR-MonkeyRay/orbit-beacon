package backup

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

func TestPublicPageSettingPayloadValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value any
		valid bool
	}{
		{"enabled", "public_page_enabled", "true", true},
		{"disabled", "public_page_enabled", "false", true},
		{"arbitrary key", "private_setting", "false", false},
		{"empty key", "", "true", false},
		{"uppercase", "public_page_enabled", "TRUE", false},
		{"whitespace", "public_page_enabled", " false", false},
		{"numeric", "public_page_enabled", "1", false},
		{"empty", "public_page_enabled", "", false},
		{"null", "public_page_enabled", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := validationDB(t, Configuration)
			if _, err := db.Exec("INSERT INTO settings(key,value) VALUES(?,?)", tc.key, tc.value); err != nil {
				t.Fatal(err)
			}
			err := ValidatePayloadData(context.Background(), db, Configuration, make([]byte, 32))
			if tc.valid && err != nil {
				t.Fatalf("valid setting rejected: %v", err)
			}
			if !tc.valid && restoreCode(err) != "restore_invalid_data" {
				t.Fatalf("invalid setting accepted: %v", err)
			}
		})
	}
	if err := ValidatePayloadData(context.Background(), validationDB(t, Configuration), Configuration, make([]byte, 32)); err != nil {
		t.Fatalf("absent setting must retain default semantics: %v", err)
	}
}

func TestPublicPageSettingSnapshotWhitelist(t *testing.T) {
	ctx := context.Background()
	source := restoreFixture(t, "source")
	db, err := storage.Open(ctx, filepath.Join(source, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO settings(key,value) VALUES('public_page_enabled','false'),('private_setting','never-export')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	work := filepath.Join(framingTempDir(t), "payloads")
	if _, err = CreateSnapshotPayloads(ctx, source, work, []Category{Configuration}, time.Now); err != nil {
		t.Fatal(err)
	}
	payload, err := openPayload(ctx, filepath.Join(work, "configuration.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer payload.Close()
	var value string
	if err = payload.QueryRow("SELECT value FROM settings WHERE key='public_page_enabled'").Scan(&value); err != nil || value != "false" {
		t.Fatalf("setting=%q err=%v", value, err)
	}
	var count int
	if err = payload.QueryRow("SELECT count(*) FROM settings").Scan(&count); err != nil || count != 1 {
		t.Fatalf("settings count=%d err=%v", count, err)
	}
}

func TestPublicPageSettingRestoreCategorySemantics(t *testing.T) {
	ctx := context.Background()
	source := restoreFixture(t, "source")
	sourceDB, err := storage.Open(ctx, filepath.Join(source, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sourceDB.Exec("INSERT INTO settings(key,value) VALUES('public_page_enabled','false'),('private_setting','source-only')"); err != nil {
		t.Fatal(err)
	}
	defer sourceDB.Close()
	for mask := 1; mask < 8; mask++ {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			target := restoreFixture(t, "target")
			db, err := storage.Open(ctx, filepath.Join(target, "orbitbeacon.db"))
			if err != nil {
				t.Fatal(err)
			}
			// A retained Configuration must filter unrelated target settings too.
			if _, err = db.Exec("INSERT INTO settings(key,value) VALUES('public_page_enabled','true'),('private_setting','target-only')"); err != nil {
				t.Fatal(err)
			}
			db.Close()
			var categories []Category
			for i, category := range []Category{Configuration, Monitoring, Events} {
				if mask&(1<<i) != 0 {
					categories = append(categories, category)
				}
			}
			archive, password := restoreArchive(t, source, categories)
			if _, err = Restore(ctx, target, archive, password, true); err != nil {
				t.Fatal(err)
			}
			restored := restoreDB(t, target)
			want := "true"
			if mask&1 != 0 {
				want = "false"
			}
			var got string
			if err = restored.QueryRow("SELECT value FROM settings WHERE key='public_page_enabled'").Scan(&got); err != nil || got != want {
				t.Fatalf("setting=%q want=%q err=%v", got, want, err)
			}
			if err = restored.QueryRow("SELECT value FROM settings WHERE key='private_setting'").Scan(&got); err != sql.ErrNoRows {
				t.Fatalf("non-whitelisted setting imported: %q err=%v", got, err)
			}
		})
	}
}

func TestConfigurationRestoreReplacesPublicPageSettingWithDefault(t *testing.T) {
	ctx := context.Background()
	source := restoreFixture(t, "source")
	target := restoreFixture(t, "target")
	db, err := storage.Open(ctx, filepath.Join(target, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO settings(key,value) VALUES('public_page_enabled','false')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	archive, password := restoreArchive(t, source, []Category{Configuration})
	if _, err = Restore(ctx, target, archive, password, true); err != nil {
		t.Fatal(err)
	}
	var value string
	if err = restoreDB(t, target).QueryRow("SELECT value FROM settings WHERE key='public_page_enabled'").Scan(&value); err != sql.ErrNoRows {
		t.Fatalf("absent archived setting did not replace current value: %q err=%v", value, err)
	}
}

func TestPublicPageSettingDataOnlyRestoreKeepsDisabled(t *testing.T) {
	ctx := context.Background()
	source := restoreFixture(t, "source")
	for _, categories := range [][]Category{{Monitoring}, {Events}, {Monitoring, Events}} {
		t.Run(fmt.Sprint(categories), func(t *testing.T) {
			target := restoreFixture(t, "target")
			db, err := storage.Open(ctx, filepath.Join(target, "orbitbeacon.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("INSERT INTO settings(key,value) VALUES('public_page_enabled','false')"); err != nil {
				t.Fatal(err)
			}
			db.Close()
			archive, password := restoreArchive(t, source, categories)
			if _, err = Restore(ctx, target, archive, password, true); err != nil {
				t.Fatal(err)
			}
			var value string
			if err = restoreDB(t, target).QueryRow("SELECT value FROM settings WHERE key='public_page_enabled'").Scan(&value); err != nil || value != "false" {
				t.Fatalf("disabled setting not retained: %q err=%v", value, err)
			}
		})
	}
}
