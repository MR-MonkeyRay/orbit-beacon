package nodes_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
)

func publishRecord(t *testing.T, db *sql.DB, node nodes.Node, created, seen time.Time, name string, generation uint64, session string) [32]byte {
	t.Helper()
	credential := sha256.Sum256([]byte("credential:" + node.ID))
	settings := `{"enabled":true,"name":"` + name + `","region":"region","provider":"provider"}`
	if _, err := db.Exec(`UPDATE nodes SET created_at=?, public_settings=?, status='unknown', token_hash=?, first_enrolled_at=?, last_seen=?, run_generation=?, report_session_id=? WHERE id=?`,
		created.Format(time.RFC3339Nano), settings, credential[:], created.Format(time.RFC3339Nano), seen.Format(time.RFC3339Nano), generation, session, node.ID); err != nil {
		t.Fatal(err)
	}
	return credential
}

func TestListPublicRecordsVisibilityOrderingPaginationAndIdentity(t *testing.T) {
	service, db := newNodes(t, 10)
	ctx := context.Background()
	base := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)

	first := pending(t, service, "private-first")
	second := pending(t, service, "private-second")
	third := pending(t, service, "private-third")
	malformed := pending(t, service, "malformed")
	unpublished := pending(t, service, "unpublished")
	firstCredential := publishRecord(t, db, first, base, base.Add(time.Minute), "First", 7, "session-first")
	publishRecord(t, db, second, base.Add(time.Second), base.Add(2*time.Minute), "Second", 8, "session-second")
	publishRecord(t, db, third, base.Add(2*time.Second), base.Add(3*time.Minute), "Third", 9, "session-third")
	if _, err := db.Exec(`UPDATE nodes SET created_at=?, public_settings='not json', status='unknown', token_hash=randomblob(32), first_enrolled_at=?, last_seen=? WHERE id=?`,
		base.Add(-time.Second).Format(time.RFC3339Nano), base.Format(time.RFC3339Nano), base.Format(time.RFC3339Nano), malformed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE nodes SET public_settings='{"enabled":true,"name":"Pending"}' WHERE id=?`, unpublished.ID); err != nil {
		t.Fatal(err)
	}

	records, more, err := service.ListPublicRecords(ctx, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !more || len(records) != 2 || records[0].ID != first.ID || records[1].ID != second.ID {
		t.Fatalf("first page = %+v, more=%v; want first/second and more", records, more)
	}
	wantFirst := nodes.PublicRecord{
		ID: first.ID, Settings: nodes.PublicSettings{Enabled: true, Name: "First", Region: "region", Provider: "provider"},
		LastSeen: base.Add(time.Minute), Credential: firstCredential, Generation: 7, Session: "session-first",
	}
	if records[0] != wantFirst {
		t.Errorf("first record = %+v, want %+v", records[0], wantFirst)
	}
	records, more, err = service.ListPublicRecords(ctx, 2, 2)
	if err != nil || more || len(records) != 1 || records[0].ID != third.ID {
		t.Fatalf("second page = %+v, more=%v, err=%v; want third only", records, more, err)
	}
	records, more, err = service.ListPublicRecords(ctx, 2, 3)
	if err != nil || more || len(records) != 0 {
		t.Fatalf("empty page = %+v, more=%v, err=%v", records, more, err)
	}
	for _, args := range [][2]int{{0, 0}, {101, 0}, {1, -1}} {
		if _, _, err := service.ListPublicRecords(ctx, args[0], args[1]); !errors.Is(err, nodes.ErrInvalidInput) {
			t.Errorf("pagination %v error = %v, want ErrInvalidInput", args, err)
		}
	}
}

func TestListPublicRecordsUsesConfiguredReaderAndNilFallsBack(t *testing.T) {
	service, db := newNodes(t, 2)
	node := pending(t, service, "public")
	now := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	publishRecord(t, db, node, now, now, "Public", 1, "session")

	closed, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "closed.db")+"?mode=memory")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	service.SetPublicReader(closed)
	if _, _, err := service.ListPublicRecords(context.Background(), 1, 0); err == nil {
		t.Fatal("ListPublicRecords succeeded through a configured closed reader")
	}
	service.SetPublicReader(nil)
	records, more, err := service.ListPublicRecords(context.Background(), 1, 0)
	if err != nil || more || len(records) != 1 || records[0].ID != node.ID {
		t.Fatalf("fallback page = %+v, more=%v, err=%v", records, more, err)
	}
}

func TestPublicVisibilityVersionTracksOnlyCommittedPublicMutations(t *testing.T) {
	service, db := newNodes(t, 20)
	ctx := audit.WithSystemActor(context.Background(), "test")
	assertVersion := func(want uint64) {
		t.Helper()
		if got := service.PublicVisibilityVersion(); got != want {
			t.Fatalf("public version = %d, want %d", got, want)
		}
	}
	assertVersion(0)

	first := pending(t, service, "first")
	name := "Published"
	enabled := true
	if _, err := service.PatchPublicSettings(ctx, first.ID, nodes.PublicSettingsPatch{Name: &name, Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	assertVersion(1)
	if err := service.SetDisabled(ctx, first.ID, true); err != nil {
		t.Fatal(err)
	}
	assertVersion(2)
	if err := service.RevokeCredential(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	assertVersion(3)
	if err := service.Delete(ctx, first.ID, true); err != nil {
		t.Fatal(err)
	}
	assertVersion(4)

	batchA := pending(t, service, "batch-a")
	batchB := pending(t, service, "batch-b")
	results, err := service.Batch(ctx, nodes.BatchRequest{NodeIDs: []string{batchA.ID, "00000000-0000-4000-8000-000000009999", batchB.ID}, Action: "disable"})
	if err != nil || len(results) != 3 || results[1].Status != "failed" {
		t.Fatalf("batch results = %+v, err=%v", results, err)
	}
	assertVersion(6)

	privateName := "changed-private-name"
	if _, err := service.PatchMetadata(ctx, batchA.ID, nodes.MetadataPatch{Name: &privateName}); err != nil {
		t.Fatal(err)
	}
	assertVersion(6)

	blank := ""
	if _, err := service.PatchPublicSettings(ctx, batchA.ID, nodes.PublicSettingsPatch{Name: &blank, Enabled: &enabled}); err == nil {
		t.Fatal("invalid public settings patch succeeded")
	}
	if err := service.SetDisabled(ctx, "00000000-0000-4000-8000-000000008888", true); !errors.Is(err, nodes.ErrNotFound) {
		t.Fatalf("missing disable error = %v", err)
	}
	if err := service.RevokeCredential(ctx, "invalid"); !errors.Is(err, nodes.ErrInvalidInput) {
		t.Fatalf("invalid revoke error = %v", err)
	}
	if err := service.Delete(ctx, batchA.ID, false); !errors.Is(err, nodes.ErrInvalidInput) {
		t.Fatalf("unconfirmed delete error = %v", err)
	}
	assertVersion(6)

	if _, err := db.Exec(`CREATE TRIGGER reject_public_patch BEFORE INSERT ON audit_events WHEN NEW.action='node.public_settings.patch' BEGIN SELECT RAISE(ABORT, 'test rollback'); END`); err != nil {
		t.Fatal(err)
	}
	newName := "Would Roll Back"
	if _, err := service.PatchPublicSettings(ctx, batchB.ID, nodes.PublicSettingsPatch{Name: &newName}); err == nil {
		t.Fatal("audit-triggered rollback unexpectedly succeeded")
	}
	assertVersion(6)
	settings, err := service.GetPublicSettings(ctx, batchB.ID)
	if err != nil || settings.Name == newName {
		t.Fatalf("rolled-back public settings = %+v, err=%v", settings, err)
	}
}

func TestListPublicRecordsWithSeparateReadOnlyPool(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "orbitbeacon.db")
	db, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reader, err := storage.OpenReader(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	service := nodes.NewService(db, nodes.Options{MaxNodes: 2})
	node := pending(t, service, "public")
	now := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	publishRecord(t, db, node, now, now, "Public", 1, "session")
	service.SetPublicReader(reader)
	records, more, err := service.ListPublicRecords(context.Background(), 1, 0)
	if err != nil || more || len(records) != 1 || records[0].ID != node.ID {
		t.Fatalf("reader page = %+v, more=%v, err=%v", records, more, err)
	}
}
