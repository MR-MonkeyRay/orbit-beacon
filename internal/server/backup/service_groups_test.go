package backup

import (
	"fmt"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestServiceGroupsBackupRoundTrip(t *testing.T) {
	ctx := t.Context()
	source := restoreFixture(t, "source")
	db, err := storage.Open(ctx, filepath.Join(source, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	const first = "00000000-0000-4000-8000-000000000011"
	const last = "00000000-0000-4000-8000-000000000012"
	if _, err = db.Exec("INSERT INTO service_groups(id,name,display_order) VALUES(?,'Zulu',1000000),(?,'Alpha',-1000000)", first, last); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO services(id,name,group_id,type,target,enabled,public,notify,created_at,updated_at) VALUES(?,'health',?,'tcp','localhost:443',1,0,0,?,?)`, validationID, first, validationTime, validationTime); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	archive, password := restoreArchive(t, source, []Category{Configuration})
	destination := restoreFixture(t, "target")
	if _, err = Restore(ctx, restoreConfig(destination), archive, password, true); err != nil {
		t.Fatal(err)
	}
	restored := restoreDB(t, destination)
	var groupID string
	if err = restored.QueryRow("SELECT group_id FROM services WHERE id=?", validationID).Scan(&groupID); err != nil || groupID != first {
		t.Fatalf("restored membership=%s err=%v", groupID, err)
	}
	rows, err := restored.Query("SELECT id,display_order FROM service_groups ORDER BY display_order DESC,name,id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for i, want := range []string{first, last} {
		if !rows.Next() {
			t.Fatalf("missing group %s: %v", want, rows.Err())
		}
		var id string
		var order int
		if err = rows.Scan(&id, &order); err != nil || id != want || order != 1000000-i*2000000 {
			t.Fatalf("restored group=%s order=%d err=%v", id, order, err)
		}
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatalf("unexpected restored group: %v", rows.Err())
	}
}

func TestServiceGroupsPayloadDisplayOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		valid bool
	}{
		{"default", 0, true}, {"minimum", -1000000, true}, {"maximum", 1000000, true},
		{"below minimum", -1000001, false}, {"above maximum", 1000001, false},
		{"fraction", 1.5, false}, {"text", "wrong", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := validationDB(t, Configuration)
			if _, err := db.Exec("INSERT INTO service_groups(id,name,display_order) VALUES(?,'group',?)", validationID, tc.value); err != nil {
				t.Fatal(err)
			}
			if err := ValidatePayloadData(t.Context(), db, Configuration, make([]byte, 32)); (err == nil) != tc.valid {
				t.Fatalf("valid=%t error=%v", tc.valid, err)
			}
		})
	}
}

func TestServiceGroupsPayloadRejectsNullDisplayOrder(t *testing.T) {
	db := validationDB(t, Configuration)
	if _, err := db.Exec("DROP TABLE service_groups; CREATE TABLE service_groups(id TEXT,name TEXT,display_order INTEGER); INSERT INTO service_groups(id,name,display_order) VALUES(?,'group',NULL)", validationID); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePayloadData(t.Context(), db, Configuration, make([]byte, 32)); err == nil {
		t.Fatal("accepted null group display_order")
	}
}

func TestServiceGroupsPayloadCapacityAndReferences(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		group any
		valid bool
	}{
		{"ungrouped", 0, nil, true}, {"grouped", 1, validationID, true}, {"unknown", 0, validationID, false}, {"malformed", 1, "bad", false},
		{"empty string", 0, "", false}, {"at capacity", 100, validationID, true}, {"over capacity", 101, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := validationDB(t, Configuration)
			for i := 0; i < tc.count; i++ {
				id := validationID
				if i > 0 {
					id = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
				}
				if _, err := db.Exec("INSERT INTO service_groups(id,name) VALUES(?,?)", id, fmt.Sprint("group ", i)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`INSERT INTO services(id,name,group_id,type,target,interval_seconds,timeout_seconds,enabled,public,notify,latency_threshold_ms,created_at,updated_at) VALUES(?,'health',?,'tcp','localhost:443',60,10,1,0,0,0,?,?)`, validationID, tc.group, validationTime, validationTime); err != nil {
				t.Fatal(err)
			}
			if err := ValidatePayloadData(t.Context(), db, Configuration, make([]byte, 32)); (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
		})
	}
}
