package management

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/storage"
)

func TestPublicPageGroupsPersistenceAuditAndRollback(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(t.Context(), filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service := NewService(mainHandle(t, db))
	ctx := audit.WithActor(context.Background(), "groups-admin")
	got, err := service.PublicPageSettings(ctx)
	if err != nil || got != (PublicPageSettings{Enabled: true}) {
		t.Fatalf("defaults: %+v %v", got, err)
	}
	yes, no := true, false
	for _, tc := range []struct {
		patch   PublicPageSettingsPatch
		want    PublicPageSettings
		details string
	}{
		{PublicPageSettingsPatch{ShowGroups: &yes}, PublicPageSettings{true, true}, `{"show_groups":true}`},
		{PublicPageSettingsPatch{Enabled: &no}, PublicPageSettings{false, true}, `{"enabled":false}`},
		{PublicPageSettingsPatch{Enabled: &yes, ShowGroups: &no}, PublicPageSettings{true, false}, `{"enabled":true,"show_groups":false}`},
	} {
		before := 0
		if err := db.QueryRow("SELECT count(*) FROM audit_events").Scan(&before); err != nil {
			t.Fatal(err)
		}
		got, err := service.PatchPublicPageSettings(ctx, tc.patch)
		if err != nil || got != tc.want {
			t.Fatalf("patch: %+v %v", got, err)
		}
		rows, err := db.Query("SELECT details FROM audit_events WHERE action='public_page.update'")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var details string
			if err := rows.Scan(&details); err != nil {
				t.Fatal(err)
			}
			found = found || details == tc.details
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatalf("audit missing details %s", tc.details)
		}
		if _, err := service.PatchPublicPageSettings(ctx, tc.patch); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRow("SELECT count(*) FROM audit_events").Scan(&count); err != nil || count != before+1 {
			t.Fatalf("no-op audit: %d %v", count, err)
		}
		reloaded, err := NewService(mainHandle(t, db)).PublicPageSettings(ctx)
		if err != nil || reloaded != tc.want {
			t.Fatalf("reload: %+v %v", reloaded, err)
		}
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_groups_audit BEFORE INSERT ON audit_events WHEN NEW.action='public_page.update' BEGIN SELECT RAISE(ABORT,'audit failure');END`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PatchPublicPageSettings(ctx, PublicPageSettingsPatch{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty patch: %v", err)
	}
	if _, err := service.PatchPublicPageSettings(ctx, PublicPageSettingsPatch{Enabled: &no, ShowGroups: &yes}); err == nil {
		t.Fatal("audit failure ignored")
	}
	got, err = service.PublicPageSettings(ctx)
	if err != nil || got != (PublicPageSettings{true, false}) {
		t.Fatalf("rollback: %+v %v", got, err)
	}
	for _, bad := range []string{"TRUE", "1", "", " false", "null"} {
		if _, err := db.Exec("UPDATE settings SET value=? WHERE key='public_page_show_groups'", bad); err != nil {
			t.Fatal(err)
		}
		if _, err := service.PublicPageSettings(ctx); err == nil || !strings.Contains(err.Error(), "public_page_show_groups") {
			t.Fatalf("corrupt read: %v", err)
		}
		if _, err := service.SetPublicPageEnabled(ctx, false); err == nil {
			t.Fatal("corrupt value silently replaced")
		}
	}
}
