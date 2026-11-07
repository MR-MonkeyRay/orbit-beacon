package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

func TestProbeRulePayloadValidation(t *testing.T) {
	for _, tc := range []struct {
		metric                        string
		target, comparator, threshold any
		valid                         bool
	}{
		{"cpu_percent", nil, ">", 80, true},
		{"cpu_percent", "https://example.test", ">", 80, false},
		{"probe_failed", "https://example.test", ">", 0, true},
		{"probe_failed", nil, ">", 0, false},
		{"probe_failed", "invalid", ">", 0, false},
		{"probe_failed", "https://example.test", "<", 0, false},
		{"probe_failed", "https://example.test", ">", 1, false},
		{"probe_latency_ms", "tcp://example.test:443", ">", 60000, true},
		{"probe_latency_ms", "tcp://example.test:443", "<", .5, true},
		{"probe_latency_ms", "tcp://example.test:443", ">", 60001, false},
		{"probe_latency_ms", "tcp://example.test:443", ">", 0, false},
		{"probe_tls_expiry_days", "https://example.test", "<", 3650, true},
		{"probe_tls_expiry_days", "https://example.test", ">", 30, false},
		{"probe_tls_expiry_days", "https://example.test", "<", 3651, false},
	} {
		t.Run(tc.metric, func(t *testing.T) {
			db := validationDB(t, Configuration)
			_, err := db.Exec(`INSERT INTO alert_rules(id,name,enabled,scope_kind,scope_id,metric,target,comparator,threshold,for_seconds,severity,created_at,updated_at) VALUES(?,'Probe',1,'all',NULL,?,?,?,?,0,'warning',?,?)`, validationID, tc.metric, tc.target, tc.comparator, tc.threshold, validationTime, validationTime)
			if err != nil {
				t.Fatal(err)
			}
			err = ValidatePayloadData(t.Context(), db, Configuration, make([]byte, 32))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestProbeRuleBackupRoundTrip(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "without target", true: "with target"}[present], func(t *testing.T) {
			ctx := context.Background()
			source := restoreFixture(t, "source")
			db, err := storage.Open(ctx, filepath.Join(source, "orbitbeacon.db"))
			if err != nil {
				t.Fatal(err)
			}
			var target any
			metric, threshold := "probes_down", 1
			if present {
				target, metric, threshold = "https://example.test", "probe_failed", 0
			}
			_, err = db.Exec(`INSERT INTO alert_rules(id,name,enabled,scope_kind,metric,target,comparator,threshold,for_seconds,severity,created_at,updated_at) VALUES(?,'Probe',1,'all',?,?,'>',?,0,'warning',?,?)`, validationID, metric, target, threshold, validationTime, validationTime)
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			archive, password := restoreArchive(t, source, []Category{Configuration})
			destination := restoreFixture(t, "target")
			if _, err = Restore(ctx, destination, archive, password, true); err != nil {
				t.Fatal(err)
			}
			var got sql.NullString
			var gotMetric string
			if err = restoreDB(t, destination).QueryRow("SELECT metric,target FROM alert_rules WHERE id=?", validationID).Scan(&gotMetric, &got); err != nil {
				t.Fatal(err)
			}
			if got.Valid != present || present && got.String != target || gotMetric != metric {
				t.Fatalf("metric=%s target=%+v", gotMetric, got)
			}
		})
	}
}

func TestProbeRuleSnapshotProjection(t *testing.T) {
	for _, target := range []any{nil, "https://example.test"} {
		source, destination := validationDB(t, Configuration), validationDB(t, Configuration)
		if _, err := destination.Exec("DELETE FROM users"); err != nil {
			t.Fatal(err)
		}
		metric := "probe_failed"
		if target == nil {
			metric = "probes_down"
		}
		if _, err := source.Exec(`INSERT INTO alert_rules(id,name,enabled,scope_kind,metric,target,comparator,threshold,for_seconds,severity,created_at,updated_at) VALUES(?,'Probe',1,'all',?,?,'>',0,0,'warning',?,?)`, validationID, metric, target, validationTime, validationTime); err != nil {
			t.Fatal(err)
		}
		if err := projectCategory(t.Context(), destination, source, nil, Configuration, time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
		if err := ValidatePayloadData(t.Context(), destination, Configuration, make([]byte, 32)); err != nil {
			t.Fatal(err)
		}
		var got sql.NullString
		if err := destination.QueryRow("SELECT target FROM alert_rules WHERE id=?", validationID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if (target != nil) != got.Valid || target != nil && got.String != target {
			t.Fatalf("target=%+v want=%v", got, target)
		}
	}
}
