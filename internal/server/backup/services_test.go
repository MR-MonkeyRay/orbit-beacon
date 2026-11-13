package backup

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/services"
	"orbitbeacon/internal/server/storage"
)

func TestServicesBackupRoundTrip(t *testing.T) {
	ctx := t.Context()
	source := restoreFixture(t, "source")
	db, e := storage.OpenMain(ctx, filepath.Join(source, "orbitbeacon.db"))
	if e != nil {
		t.Fatal(e)
	}
	hist, e := storage.OpenHistory(ctx, filepath.Join(source, "metrics.db"))
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	manager := services.New(db, hist, func() time.Time { return now }, nil, nil, nil)
	name, kind, target, public := "Health", "tcp", "localhost:443", true
	threshold := 5
	config, e := manager.Save(ctx, "", services.Patch{Name: &name, Type: &kind, Target: &target, Public: &public, FailureThreshold: &threshold})
	if e != nil {
		t.Fatal(e)
	}
	latency := 12.0
	if e = manager.Accept(ctx, config, services.Result{OK: true, LatencyMS: &latency, CheckedAt: now}); e != nil {
		t.Fatal(e)
	}
	if e = manager.Flush(ctx); e != nil {
		t.Fatal(e)
	}
	db.Close()
	hist.Close()
	archive, password := restoreArchive(t, source, []Category{Configuration, Monitoring})
	destination := restoreFixture(t, "target")
	if _, e = Restore(ctx, restoreConfig(destination), archive, password, true); e != nil {
		t.Fatal(e)
	}
	restored := restoreDB(t, destination)
	var gotName, raw string
	var gotThreshold int
	if e = restored.QueryRow(`SELECT name,failure_threshold FROM services WHERE id=?`, config.ID).Scan(&gotName, &gotThreshold); e != nil || gotName != name || gotThreshold != threshold {
		t.Fatalf("config: %s %v", gotName, e)
	}
	if e = restored.QueryRow(`SELECT result_json FROM service_checks_latest WHERE service_id=?`, config.ID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var result services.Result
	if e = json.Unmarshal([]byte(raw), &result); e != nil || result.Status != "up" || result.LatencyMS == nil || *result.LatencyMS != 12 {
		t.Fatalf("latest: %s %v", raw, e)
	}
	metrics, e := storage.OpenReader(ctx, filepath.Join(destination, "metrics.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer metrics.Close()
	var checks, successes int
	var sum float64
	if e = metrics.QueryRow(`SELECT checks,successes,latency_sum_ms FROM service_check_buckets WHERE service_id=?`, config.ID).Scan(&checks, &successes, &sum); e != nil || checks != 1 || successes != 1 || sum != 12 {
		t.Fatalf("buckets: %d %d %f %v", checks, successes, sum, e)
	}
}

func TestServicesPayloadValidation(t *testing.T) {
	id := "00000000-0000-4000-8000-000000000101"
	for _, tc := range []struct {
		name              string
		checks, successes int
		sum, min, max     float64
		valid             bool
	}{
		{"valid", 3, 2, 24, 10, 14, true}, {"all lost", 3, 0, 0, 0, 0, true}, {"too many successes", 3, 4, 24, 10, 14, false}, {"negative", 3, 2, -1, 10, 14, false}, {"reversed", 3, 2, 24, 15, 14, false}, {"above sum", 3, 2, 10, 10, 14, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := validationDB(t, Monitoring)
			if _, e := db.Exec(`INSERT INTO service_check_buckets VALUES(?,300,?,?,?,?,?)`, id, tc.checks, tc.successes, tc.sum, tc.min, tc.max); e != nil {
				t.Fatal(e)
			}
			if e := ValidatePayloadData(t.Context(), db, Monitoring, nil); (e == nil) != tc.valid {
				t.Fatalf("valid=%t error=%v", tc.valid, e)
			}
		})
	}
}

func TestServiceFailureThresholdPayloadValidation(t *testing.T) {
	for _, tc := range []struct {
		value any
		valid bool
	}{
		{1, true}, {2, true}, {5, true}, {10, true}, {0, false}, {11, false}, {1.5, false}, {"bad", false}, {nil, false},
	} {
		t.Run(fmt.Sprint(tc.value), func(t *testing.T) {
			db := validationDB(t, Configuration)
			// Payload schema deliberately lacks production CHECK constraints: validation owns this boundary.
			if _, err := db.Exec("DROP TABLE services; CREATE TABLE services(id TEXT,name TEXT,group_id TEXT,type TEXT,target TEXT,interval_seconds INTEGER,timeout_seconds INTEGER,enabled INTEGER,public INTEGER,display_order INTEGER,notify INTEGER,latency_threshold_ms REAL,failure_threshold INTEGER,created_at TEXT,updated_at TEXT)"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("INSERT INTO services VALUES(?,'health',NULL,'tcp','localhost:443',60,10,1,0,0,0,0,?,?,?)", validationID, tc.value, validationTime, validationTime); err != nil {
				t.Fatal(err)
			}
			if err := ValidatePayloadData(t.Context(), db, Configuration, make([]byte, 32)); (err == nil) != tc.valid {
				t.Fatalf("value=%v valid=%t err=%v", tc.value, tc.valid, err)
			}
		})
	}
}
