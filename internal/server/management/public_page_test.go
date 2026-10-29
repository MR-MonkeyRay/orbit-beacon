package management

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/storage"
)

func TestPublicPageSettingPersistenceAndAudit(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(t.Context(), filepath.Join(directory, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service := NewService(db)
	ctx := audit.WithActor(context.Background(), "public-page-admin")
	setting, err := service.PublicPageSettings(ctx)
	if err != nil || !setting.Enabled {
		t.Fatalf("missing default: %+v %v", setting, err)
	}
	count := func() int {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM audit_events WHERE action='public_page.update'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := service.SetPublicPageEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Fatal("idempotent default created audit")
	}
	setting, err = service.SetPublicPageEnabled(ctx, false)
	if err != nil || setting.Enabled {
		t.Fatalf("disable: %+v %v", setting, err)
	}
	if count() != 1 {
		t.Fatal("disable audit missing")
	}
	setting, err = NewService(db).PublicPageSettings(ctx)
	if err != nil || setting.Enabled {
		t.Fatalf("disabled not persistent: %+v %v", setting, err)
	}
	if _, err := service.SetPublicPageEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatal("idempotent false created audit")
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_public_audit BEFORE INSERT ON audit_events WHEN NEW.action='public_page.update' BEGIN SELECT RAISE(ABORT,'public audit failure');END`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetPublicPageEnabled(ctx, true); err == nil {
		t.Fatal("audit failure ignored")
	}
	setting, err = service.PublicPageSettings(ctx)
	if err != nil || setting.Enabled || count() != 1 {
		t.Fatalf("audit failure changed setting: %+v %v", setting, err)
	}
	if _, err := db.Exec("DROP TRIGGER fail_public_audit"); err != nil {
		t.Fatal(err)
	}
	setting, err = service.SetPublicPageEnabled(ctx, true)
	if err != nil || !setting.Enabled || count() != 2 {
		t.Fatalf("enable: %+v %v", setting, err)
	}
	for _, invalid := range []string{"TRUE", "1", "", " true", "null"} {
		if _, err := db.Exec("UPDATE settings SET value=? WHERE key='public_page_enabled'", invalid); err != nil {
			t.Fatal(err)
		}
		if _, err := service.PublicPageSettings(ctx); err == nil {
			t.Fatalf("corrupt setting %q opened public page", invalid)
		}
		if _, err := service.SetPublicPageEnabled(ctx, false); err == nil {
			t.Fatalf("corrupt setting %q silently replaced", invalid)
		}
	}
}
