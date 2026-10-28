package backup

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

func TestRestoreRejectsMalformedInboxDetailsBeforeInstall(t *testing.T) {
	ctx := context.Background()
	source := restoreFixture(t, "source")
	target := restoreFixture(t, "target")
	before := make(map[string][]byte)
	for _, name := range []string{"orbitbeacon.db", "metrics.db", "secrets.key"} {
		data, err := os.ReadFile(filepath.Join(target, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = data
	}
	for _, tc := range []struct{ name, kind, details string }{
		{"task categories object", "task", `{"categories":{}}`},
		{"task destination name object", "task", `{"destination":{"plan_name":{}}}`},
		{"alert metric object", "alert", `{"metric":{},"value":42}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := framingTempDir(t)
			payloads, err := CreateSnapshotPayloads(ctx, source, filepath.Join(work, "payloads"), []Category{Events}, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			// Start from a valid current-schema projection, then alter only row JSON.
			// WriteArchive authenticates the modified payload, exercising restore's
			// data validation rather than a framing/hash rejection.
			db, err := sql.Open("sqlite", filepath.Join(work, "payloads", "events.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec(`INSERT INTO inbox_notifications(id,kind,source_id,transition,audience,severity,created_at,details_json) VALUES(?,?, 'historical-source','snapshot','admin','info',?,?)`, validationID, tc.kind, validationTime, tc.details)
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(work, "malformed-inbox.tar.gz.age")
			password := "AbCdEf0123456789"
			_, err = WriteArchive(ctx, archive, Manifest{Format: "orbitbeacon-backup", Version: 1, ArchiveID: "88888888-8888-4888-8888-888888888888", CreatedAt: time.Now().UTC(), ServerVersion: "test", SchemaVersion: storage.CurrentSchemaVersion, HistorySchemaVersion: 1, Categories: []Category{Events}, Snapshots: payloads.Snapshots}, payloads.Files, password)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Restore(ctx, target, archive, password, true)
			if restoreCode(err) != "restore_invalid_data" {
				t.Errorf("malformed inbox details restore error = %v, want restore_invalid_data", err)
			}
			for name, want := range before {
				got, err := os.ReadFile(filepath.Join(target, name))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("malformed inbox details changed live %s", name)
				}
			}
			assertNoPrepared(t, target)
		})
	}
}
