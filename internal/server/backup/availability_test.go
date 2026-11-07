package backup

import (
	"context"
	"fmt"
	"orbitbeacon/internal/server/storage"
	"path/filepath"
	"testing"
)

func TestAvailabilityPayloadConstraints(t *testing.T) {
	for _, tc := range []struct {
		day             string
		online, offline any
		valid           bool
	}{
		{"2026-11-05", 0, 0, true}, {"2026-11-05", 86400, 0, true}, {"2026-11-05", 0, 86400, true},
		{"2026-02-30", 0, 0, false}, {"2026-11-05", -1, 0, false}, {"2026-11-05", 0, -1, false},
		{"2026-11-05", 86401, 0, false}, {"2026-11-05", 0, 86401, false}, {"2026-11-05", 86400, 1, false},
		{"2026-11-05", 1.5, 0, false}, {"2026-11-05", nil, 0, false},
	} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			db := validationDB(t, Monitoring)
			if _, err := db.Exec(`INSERT INTO node_availability_daily VALUES('node',?,?,?)`, tc.day, tc.online, tc.offline); err != nil {
				t.Fatal(err)
			}
			err := ValidatePayloadData(context.Background(), db, Monitoring, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
func TestAvailabilityBackupRoundTrip(t *testing.T) {
	ctx := context.Background()
	source := restoreFixture(t, "source")
	db, err := storage.Open(ctx, filepath.Join(source, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO node_availability_daily VALUES(?,'2026-11-05',123,45)`, restoreNode); err != nil {
		t.Fatal(err)
	}
	db.Close()
	archive, password := restoreArchive(t, source, []Category{Configuration, Monitoring})
	target := restoreFixture(t, "target")
	if _, err := Restore(ctx, target, archive, password, true); err != nil {
		t.Fatal(err)
	}
	restored := restoreDB(t, target)
	var online, offline int
	if err := restored.QueryRow(`SELECT online_seconds,offline_seconds FROM node_availability_daily WHERE node_id=?`, restoreNode).Scan(&online, &offline); err != nil || online != 123 || offline != 45 {
		t.Fatalf("round trip=%d,%d %v", online, offline, err)
	}
}
