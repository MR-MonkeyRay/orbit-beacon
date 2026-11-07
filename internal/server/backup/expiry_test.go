package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

func TestExpiryPayloadRejectsInvalidDates(t *testing.T) {
	db := validationDB(t, Configuration)
	if _, err := db.Exec(`INSERT INTO nodes(id,name,status,group_id,region,provider,notes,created_at,public_settings,billing_day,quota_bytes,billing_mode,token_hash,credential_epoch,installation_id,first_enrolled_at,run_generation,report_session_id) VALUES(?,'Node','pending',NULL,'','','',?,'{}',1,'0','total',NULL,0,NULL,NULL,'0','')`, validationID, validationTime); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		date  any
		valid bool
	}{
		{nil, true}, {"2028-02-29", true}, {"2026-11-05", true}, {"2026-02-30", false}, {"2026-2-01", false}, {"", false}, {"2026-11-05T00:00:00Z", false}, {123, false},
	} {
		if _, err := db.Exec("UPDATE nodes SET expires_on=?", tc.date); err != nil {
			t.Fatal(err)
		}
		err := ValidatePayloadData(context.Background(), db, Configuration, make([]byte, 32))
		if (err == nil) != tc.valid {
			t.Fatalf("date=%v valid=%t: %v", tc.date, tc.valid, err)
		}
	}
}

func TestExpiryBackupRoundTrip(t *testing.T) {
	for _, date := range []any{nil, "2028-02-29"} {
		t.Run("date", func(t *testing.T) {
			ctx := context.Background()
			source := restoreFixture(t, "source")
			db, err := storage.Open(ctx, filepath.Join(source, "orbitbeacon.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("UPDATE nodes SET expires_on=? WHERE id=?", date, restoreNode); err != nil {
				db.Close()
				t.Fatal(err)
			}
			db.Close()
			target := restoreFixture(t, "target")
			archive, password := restoreArchive(t, source, []Category{Configuration})
			if _, err = Restore(ctx, restoreConfig(target), archive, password, true); err != nil {
				t.Fatal(err)
			}
			var got sql.NullString
			if err = restoreDB(t, target).QueryRow("SELECT expires_on FROM nodes WHERE id=?", restoreNode).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if date == nil && got.Valid || date != nil && (!got.Valid || got.String != date) {
				t.Fatalf("got %+v want %v", got, date)
			}
		})
	}
}

func TestExpirySnapshotProjectionRealSQLite(t *testing.T) {
	for _, date := range []any{nil, "2028-02-29"} {
		t.Run("date", func(t *testing.T) {
			source := validationDB(t, Configuration)
			destination := validationDB(t, Configuration)
			if _, err := destination.Exec("DELETE FROM users"); err != nil {
				t.Fatal(err)
			}
			if _, err := source.Exec(`INSERT INTO nodes(id,name,status,group_id,region,provider,notes,expires_on,created_at,public_settings,billing_day,quota_bytes,billing_mode,token_hash,credential_epoch,installation_id,first_enrolled_at,run_generation,report_session_id) VALUES(?,'Node','pending',NULL,'','','',?,?,'{}',1,'0','total',NULL,0,NULL,NULL,'0','')`, validationID, date, validationTime); err != nil {
				t.Fatal(err)
			}
			if err := projectCategory(context.Background(), destination, source, nil, Configuration, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := ValidatePayloadData(context.Background(), destination, Configuration, make([]byte, 32)); err != nil {
				t.Fatal(err)
			}
			var got sql.NullString
			if err := destination.QueryRow("SELECT expires_on FROM nodes WHERE id=?", validationID).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if date == nil && got.Valid || date != nil && (!got.Valid || got.String != date) {
				t.Fatalf("got %+v want %v", got, date)
			}
		})
	}
}
