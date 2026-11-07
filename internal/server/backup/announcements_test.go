package backup

import (
	"context"
	"orbitbeacon/internal/server/storage"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAnnouncementPayloadValidation(t *testing.T) {
	for _, tc := range []struct {
		name, level, title, body, end string
		valid                         bool
	}{
		{"valid", "info", "T", "B", "2027-01-01T00:00:00.000000000Z", true},
		{"null end", "critical", "T", "B", "", true},
		{"level", "bad", "T", "B", "", false},
		{"title", "info", strings.Repeat("界", 34), "B", "", false},
		{"body", "info", "T", strings.Repeat("界", 334), "", false},
		{"control", "info", "T", "B\t", "", false},
		{"inverted", "info", "T", "B", "2026-01-01T00:00:00.000000000Z", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := validationDB(t, Configuration)
			var end any
			if tc.end != "" {
				end = tc.end
			}
			if _, err := db.Exec(`INSERT INTO announcements VALUES(?,?,?,?,?,?,?,?)`, validationID, tc.level, tc.title, tc.body, validationTime, end, validationTime, validationTime); err != nil {
				t.Fatal(err)
			}
			err := ValidatePayloadData(context.Background(), db, Configuration, make([]byte, 32))
			if tc.valid && err != nil || !tc.valid && err == nil {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
func TestAnnouncementBackupRoundTrip(t *testing.T) {
	ctx := context.Background()
	source := restoreFixture(t, "source")
	target := restoreFixture(t, "target")
	db, err := storage.Open(ctx, filepath.Join(source, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO announcements VALUES(?,'warning','Maintenance','one'||char(10)||'two',?,NULL,?,?)`, validationID, validationTime, validationTime, validationTime)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(framingTempDir(t), "orbit-c2-payloads")
	if _, err = CreateSnapshotPayloads(ctx, snapshotSources(t, source), source, work, []Category{Configuration}, time.Now); err != nil {
		t.Fatal(err)
	}
	payload, err := openPayload(ctx, filepath.Join(work, "configuration.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	var count int
	err = payload.QueryRow("SELECT count(*) FROM announcements").Scan(&count)
	payload.Close()
	if err != nil || count != 1 {
		t.Fatalf("snapshot %d %v", count, err)
	}
	archive, password := restoreArchive(t, source, []Category{Configuration})
	if _, err = Restore(ctx, target, archive, password, true); err != nil {
		t.Fatal(err)
	}
	var body string
	if err = restoreDB(t, target).QueryRow("SELECT body FROM announcements WHERE id=?", validationID).Scan(&body); err != nil || body != "one\ntwo" {
		t.Fatalf("roundtrip %q %v", body, err)
	}
}
