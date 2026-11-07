package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

func TestProbeTLSExpiryPayloadValidation(t *testing.T) {
	expiry := timekey.Format(time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC))
	for _, tc := range []struct {
		name, target string
		expiry       any
		valid        bool
	}{
		{"https nil", "https://example.test", nil, true},
		{"https expiry", "https://example.test", expiry, true},
		{"http expiry", "http://example.test", expiry, false},
		{"tcp expiry", "tcp://example.test:443", expiry, false},
		{"icmp expiry", "icmp://example.test", expiry, false},
		{"dns expiry", "dns://example.test", expiry, false},
		{"invalid time", "https://example.test", "bad", false},
		{"noncanonical", "https://example.test", "2027-01-02T03:04:05Z", false},
		{"zero time", "https://example.test", timekey.Format(time.Time{}), false},
		{"integer", "https://example.test", 123, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := validationDB(t, Monitoring)
			if _, err := db.Exec(`INSERT INTO probe_latest(node_id,target,ok,latency_ms,error,status_code,checked_at,tls_expires_at) VALUES('node',?,1,1,'',NULL,?,?)`, tc.target, validationTime, tc.expiry); err != nil {
				t.Fatal(err)
			}
			err := ValidatePayloadData(t.Context(), db, Monitoring, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
		})
	}
}

func TestProbeTLSExpiryBackupRoundTrip(t *testing.T) {
	for _, expiry := range []any{nil, "2027-01-02T03:04:05.000000000Z"} {
		t.Run("expiry", func(t *testing.T) {
			ctx := context.Background()
			source := restoreFixture(t, "source")
			db, err := storage.Open(ctx, filepath.Join(source, "orbitbeacon.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO probe_latest(node_id,target,ok,latency_ms,error,status_code,checked_at,tls_expires_at) VALUES(?,'https://example.test',0,NULL,'http_status',302,?,?)`, restoreNode, validationTime, expiry); err != nil {
				db.Close()
				t.Fatal(err)
			}
			db.Close()
			archive, password := restoreArchive(t, source, []Category{Configuration, Monitoring})
			target := restoreFixture(t, "target")
			if _, err := Restore(ctx, restoreConfig(target), archive, password, true); err != nil {
				t.Fatal(err)
			}
			var got sql.NullString
			if err := restoreDB(t, target).QueryRow(`SELECT tls_expires_at FROM probe_latest WHERE node_id=?`, restoreNode).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if expiry == nil && got.Valid || expiry != nil && (!got.Valid || got.String != expiry) {
				t.Fatalf("got=%v want=%v", got, expiry)
			}
		})
	}
}
