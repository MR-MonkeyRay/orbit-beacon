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
		{"rate minimum", "api_token_rate_limit_per_minute", "1", true},
		{"rate maximum", "api_token_rate_limit_per_minute", "100000", true},
		{"rate zero", "api_token_rate_limit_per_minute", "0", false},
		{"rate over", "api_token_rate_limit_per_minute", "100001", false},
		{"rate malformed", "api_token_rate_limit_per_minute", "oops", false},
		{"rate fractional", "api_token_rate_limit_per_minute", "1.5", false},
		{"rate null", "api_token_rate_limit_per_minute", nil, false},
		{"enabled", "public_page_enabled", "true", true},
		{"disabled", "public_page_enabled", "false", true},
		{"show groups", "public_page_show_groups", "true", true},
		{"hide groups", "public_page_show_groups", "false", true},
		{"groups uppercase", "public_page_show_groups", "TRUE", false},
		{"groups whitespace", "public_page_show_groups", " false", false},
		{"groups numeric", "public_page_show_groups", "1", false},
		{"groups empty", "public_page_show_groups", "", false},
		{"groups null", "public_page_show_groups", nil, false},
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
	if _, err = db.Exec("INSERT INTO settings(key,value) VALUES('public_page_enabled','false'),('public_page_show_groups','true'),('api_token_rate_limit_per_minute','37'),('private_setting','never-export')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	work := filepath.Join(framingTempDir(t), "payloads")
	if _, err = CreateSnapshotPayloads(ctx, snapshotSources(t, source), source, work, []Category{Configuration}, time.Now); err != nil {
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
	if err = payload.QueryRow("SELECT value FROM settings WHERE key='api_token_rate_limit_per_minute'").Scan(&value); err != nil || value != "37" {
		t.Fatal("rate snapshot", value, err)
	}
	if err = payload.QueryRow("SELECT value FROM settings WHERE key='public_page_show_groups'").Scan(&value); err != nil || value != "true" {
		t.Fatal("groups snapshot", value, err)
	}
	var count int
	if err = payload.QueryRow("SELECT count(*) FROM settings").Scan(&count); err != nil || count != 3 {
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
	if _, err = sourceDB.Exec("INSERT INTO settings(key,value) VALUES('public_page_enabled','false'),('public_page_show_groups','true'),('api_token_rate_limit_per_minute','37'),('private_setting','source-only')"); err != nil {
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
			// 保留的 Configuration 还必须过滤不相关的目标设置。
			if _, err = db.Exec("INSERT INTO settings(key,value) VALUES('public_page_enabled','true'),('public_page_show_groups','false'),('api_token_rate_limit_per_minute','99'),('private_setting','target-only')"); err != nil {
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
			if _, err = Restore(ctx, restoreConfig(target), archive, password, true); err != nil {
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
			wantGroups := "false"
			if mask&1 != 0 {
				wantGroups = "true"
			}
			if err = restored.QueryRow("SELECT value FROM settings WHERE key='public_page_show_groups'").Scan(&got); err != nil || got != wantGroups {
				t.Fatal("groups restore", got, err)
			}
			wantRate := "99"
			if mask&1 != 0 {
				wantRate = "37"
			}
			if err = restored.QueryRow("SELECT value FROM settings WHERE key='api_token_rate_limit_per_minute'").Scan(&got); err != nil || got != wantRate {
				t.Fatal("rate restore", got, err)
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
	if _, err = db.Exec("INSERT INTO settings(key,value) VALUES('public_page_enabled','false'),('public_page_show_groups','true'),('api_token_rate_limit_per_minute','12')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	archive, password := restoreArchive(t, source, []Category{Configuration})
	if _, err = Restore(ctx, restoreConfig(target), archive, password, true); err != nil {
		t.Fatal(err)
	}
	var value string
	if err = restoreDB(t, target).QueryRow("SELECT value FROM settings WHERE key='public_page_enabled'").Scan(&value); err != sql.ErrNoRows {
		t.Fatalf("absent archived setting did not replace current value: %q err=%v", value, err)
	}
	if err = restoreDB(t, target).QueryRow("SELECT value FROM settings WHERE key='public_page_show_groups'").Scan(&value); err != sql.ErrNoRows {
		t.Fatal("absent archived groups did not restore default", value, err)
	}
	if err = restoreDB(t, target).QueryRow("SELECT value FROM settings WHERE key='api_token_rate_limit_per_minute'").Scan(&value); err != sql.ErrNoRows {
		t.Fatal("absent archived rate did not restore default", value, err)
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
			if _, err = db.Exec("INSERT INTO settings(key,value) VALUES('public_page_enabled','false'),('public_page_show_groups','true')"); err != nil {
				t.Fatal(err)
			}
			db.Close()
			archive, password := restoreArchive(t, source, categories)
			if _, err = Restore(ctx, restoreConfig(target), archive, password, true); err != nil {
				t.Fatal(err)
			}
			var value string
			if err = restoreDB(t, target).QueryRow("SELECT value FROM settings WHERE key='public_page_enabled'").Scan(&value); err != nil || value != "false" {
				t.Fatalf("disabled setting not retained: %q err=%v", value, err)
			}
			if err = restoreDB(t, target).QueryRow("SELECT value FROM settings WHERE key='public_page_show_groups'").Scan(&value); err != nil || value != "true" {
				t.Fatal("groups setting not retained", value, err)
			}
		})
	}
}
