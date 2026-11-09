package backup

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestProbeHistoryPayloadValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		maximum any
		sum     float64
		valid   bool
	}{
		{"zero", 0, 0, true}, {"valid", 25.5, 40, true}, {"upper boundary", 60000, 120000, true},
		{"negative", -1, 40, false}, {"above bound", 60001, 120000, false}, {"above sum", 41, 40, false},
		{"infinite", math.Inf(1), 40, false}, {"negative infinite", math.Inf(-1), 40, false}, {"nan", math.NaN(), 40, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := validationDB(t, Monitoring)
			if _, err := db.Exec(`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms,latency_max_ms) VALUES('node','https://example.test',?,2,0,?,?)`, validationTime, tc.sum, tc.maximum); err != nil {
				t.Fatal(err)
			}
			if err := ValidatePayloadData(t.Context(), db, Monitoring, nil); (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
		})
	}
}

func TestProbeHistoryBackupRoundTrip(t *testing.T) {
	ctx := context.Background()
	source := restoreFixture(t, "source")
	db, err := storage.Open(ctx, filepath.Join(source, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms,latency_max_ms) VALUES(?,'https://example.test',?,3,1,40,25.5)`, restoreNode, validationTime); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	archive, password := restoreArchive(t, source, []Category{Configuration, Monitoring})
	target := restoreFixture(t, "target")
	if _, err := Restore(ctx, restoreConfig(target), archive, password, true); err != nil {
		t.Fatal(err)
	}
	var maximum, sum float64
	if err := restoreDB(t, target).QueryRow(`SELECT latency_sum_ms,latency_max_ms FROM probe_hourly WHERE node_id=? AND target=?`, restoreNode, "https://example.test").Scan(&sum, &maximum); err != nil {
		t.Fatal(err)
	}
	if sum != 40 || maximum != 25.5 {
		t.Fatalf("sum=%v max=%v", sum, maximum)
	}
}
