package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

func TestSnapshotArchiveSevenIndependentCategoryCombinations(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := datadir.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(ctx, filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	at := timekey.Format(now)
	hash, err := auth.HashPassword("backup-fixture-password", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO users(id,username,password_hash,created_at) VALUES('11111111-1111-4111-8111-111111111111','configuration-canary',?,?)`, hash, at); err != nil {
		t.Fatal(err)
	}
	node := "22222222-2222-4222-8222-222222222222"
	if _, err = db.Exec(`INSERT INTO nodes(id,name,status,token_hash,installation_id,first_enrolled_at,last_seen,system,run_generation,report_session_id,created_at) VALUES(?,'node','online',?,'fixture-installation',?,?,?,'17','33333333-3333-4333-8333-333333333333',?)`, node, bytes.Repeat([]byte{3}, 32), at, at, `{"hostname":"monitoring-canary"}`, at); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO audit_events VALUES('44444444-4444-4444-8444-444444444444',NULL,'fixture',NULL,?,?)`, at, `{"marker":"events-canary"}`); err != nil {
		t.Fatal(err)
	}
	metrics, err := history.Open(ctx, dir, db, history.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = metrics.Close(); err != nil {
		t.Fatal(err)
	}
	for mask := 1; mask < 8; mask++ {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			var categories []Category
			for i, c := range [...]Category{Configuration, Monitoring, Events} {
				if mask&(1<<i) != 0 {
					categories = append(categories, c)
				}
			}
			work := filepath.Join(dir, fmt.Sprintf("work-%d", mask))
			payloads, err := CreateSnapshotPayloads(ctx, dir, work, categories, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			if (payloads.Snapshots.MetricsAt != nil) != (mask&2 != 0) {
				t.Fatal("metrics snapshot selection mismatch")
			}
			id := fmt.Sprintf("50000000-0000-4000-8000-%012d", mask)
			archive := filepath.Join(dir, fmt.Sprintf("%d.tar.gz.age", mask))
			password, err := GeneratePassword(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			manifest := Manifest{Format: "orbitbeacon-backup", Version: 1, ArchiveID: id, CreatedAt: now, ServerVersion: "test", SchemaVersion: storage.CurrentSchemaVersion, HistorySchemaVersion: 1, Categories: categories, Snapshots: payloads.Snapshots}
			info, err := WriteArchive(ctx, archive, manifest, payloads.Files, password)
			if err != nil {
				t.Fatal(err)
			}
			if info.Bytes <= 0 || len(info.SHA256) != 64 {
				t.Fatalf("invalid ciphertext metadata %+v", info)
			}
			stage := filepath.Join(dir, fmt.Sprintf("read-%d", mask))
			got, err := ReadArchive(ctx, archive, password, stage)
			if err != nil {
				t.Fatal(err)
			}
			if got.ArchiveID != id {
				t.Fatalf("archive identity changed: %s", got.ArchiveID)
			}
			for _, c := range categories {
				path := filepath.Join(stage, string(c)+".sqlite")
				reader, err := openPayload(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				if err = ValidatePayloadSchema(ctx, reader, c); err != nil {
					reader.Close()
					t.Fatal(err)
				}
				var key []byte
				if c == Configuration {
					key, err = datadir.LoadKey(filepath.Join(stage, "secrets.key"))
					if err != nil {
						reader.Close()
						t.Fatal(err)
					}
				}
				if err = ValidatePayloadData(ctx, reader, c, key); err != nil {
					clear(key)
					reader.Close()
					t.Fatal(err)
				}
				clear(key)
				if c == Configuration {
					var gen, session, status string
					var token []byte
					if err = reader.QueryRow(`SELECT run_generation,report_session_id,status,token_hash FROM nodes WHERE id=?`, node).Scan(&gen, &session, &status, &token); err != nil {
						t.Fatal(err)
					}
					if gen != "17" || session != "33333333-3333-4333-8333-333333333333" || status != "unknown" || !bytes.Equal(token, bytes.Repeat([]byte{3}, 32)) {
						t.Fatal("agent authentication fence lost")
					}
					raw, e := os.ReadFile(path)
					if e != nil {
						t.Fatal(e)
					}
					for _, canary := range []string{"events-canary", "monitoring-canary"} {
						if bytes.Contains(raw, []byte(canary)) {
							t.Fatalf("unselected data leaked: %s", canary)
						}
					}
				}
				reader.Close()
			}
			_, err = os.Stat(filepath.Join(stage, "secrets.key"))
			if mask&1 == 0 && !os.IsNotExist(err) {
				t.Fatalf("key leaked into data-only archive: %v", err)
			}
		})
	}
}

func TestMonitoringMissingHistoryFailsButConfigurationDoesNotRequireIt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := datadir.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(ctx, filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := CreateSnapshotPayloads(ctx, dir, filepath.Join(dir, "monitoring-work"), []Category{Monitoring}, time.Now); err == nil || err.Error() != "history_unavailable" {
		t.Fatalf("missing history=%v", err)
	}
	hash, err := auth.HashPassword("backup-fixture-password", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO users(id,username,password_hash,created_at) VALUES('11111111-1111-4111-8111-111111111111','admin',?,?)`, hash, timekey.Format(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err = CreateSnapshotPayloads(ctx, dir, filepath.Join(dir, "configuration-work"), []Category{Configuration}, time.Now); err != nil {
		t.Fatal(err)
	}
}
