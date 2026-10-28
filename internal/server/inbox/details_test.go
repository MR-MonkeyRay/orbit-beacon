package inbox_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/storage"
)

func TestInsertValidatesDetailsShapes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(ctx, filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct {
		name, kind, details string
		valid               bool
	}{
		{"sparse task", "task", `{}`, true},
		{"sparse alert", "alert", `{}`, true},
		{"current task", "task", `{"id":"10000000-0000-4000-8000-000000000001","kind":"backup_export","state":"succeeded","phase":"storing","plan_id":null,"actor_id":"deleted-user","categories":["configuration","events"],"destination":{"plan_name":"","actor_name":"Admin","target":null},"created_at":"2026-10-28T00:00:00Z","started_at":"2026-10-28T00:00:00.123Z","finished_at":"2026-10-28T00:01:00Z","scheduled_at":null,"artifact_name":"backup.tar.gz.age","artifact_bytes":"123","artifact_sha256":"` + strings.Repeat("a", 64) + `","result_location":"/backups/archive","artifact_expires_at":null,"password_expires_at":null,"error_code":"","warning_code":"","restored":false,"download_available":true,"password_available":true,"has_password":true}`, true},
		{"restore interruption", "task", `{"kind":"backup_local","categories":["monitoring"],"state":"failed","phase":"storing","error_code":"restore_interrupted","warning_code":"storage_outcome_unknown","finished_at":"2026-10-28T00:00:00.000000000Z"}`, true},
		{"current alert", "alert", `{"rule_id":"deleted-rule","rule_name":"CPU","node_id":"deleted-node","node_name":"Node","metric":"cpu_percent","severity":"warning","transition":"firing","value":85.5,"started_at":"2026-10-28T00:00:00Z","resolved_at":null}`, true},
		{"historical alert", "alert", `{"rule_id":"","node_id":"","rule_name":"","node_name":"","value":null}`, true},
		{"empty categories", "task", `{"categories":[]}`, true},
		{"local destination", "task", `{"destination":{"plan_name":"Daily","actor_name":"","target":{"path":"/not/required/to/exist","retain_count":7}}}`, true},
		{"object destination", "task", `{"destination":{"target":{"provider":"s3_compatible","endpoint":"https://objects.example.com","region":"test","bucket":"backups","prefix":"","path_style":true,"allow_private_networks":false}}}`, true},
		{"categories object", "task", `{"categories":{}}`, false},
		{"categories string", "task", `{"categories":"events"}`, false},
		{"categories null", "task", `{"categories":null}`, false},
		{"category object", "task", `{"categories":[{}]}`, false},
		{"category null", "task", `{"categories":[null]}`, false},
		{"unknown category", "task", `{"categories":["other"]}`, false},
		{"destination array", "task", `{"destination":[]}`, false},
		{"destination name object", "task", `{"destination":{"plan_name":{}}}`, false},
		{"destination actor number", "task", `{"destination":{"actor_name":3}}`, false},
		{"target string", "task", `{"destination":{"target":"/backup"}}`, false},
		{"target path object", "task", `{"destination":{"target":{"path":{}}}}`, false},
		{"target flag string", "task", `{"destination":{"target":{"path_style":"true"}}}`, false},
		{"task kind object", "task", `{"kind":{}}`, false},
		{"task phase array", "task", `{"phase":[]}`, false},
		{"task timestamp object", "task", `{"finished_at":{}}`, false},
		{"task timestamp invalid", "task", `{"finished_at":"yesterday"}`, false},
		{"task bytes object", "task", `{"artifact_bytes":{}}`, false},
		{"task bytes negative", "task", `{"artifact_bytes":"-1"}`, false},
		{"task availability string", "task", `{"has_password":"true"}`, false},
		{"rule name object", "alert", `{"rule_name":{}}`, false},
		{"node id object", "alert", `{"node_id":{}}`, false},
		{"metric object", "alert", `{"metric":{}}`, false},
		{"unknown metric", "alert", `{"metric":"other"}`, true},
		{"severity array", "alert", `{"severity":[]}`, false},
		{"unknown severity", "alert", `{"severity":"other"}`, true},
		{"empty historical metadata", "alert", `{"metric":"","severity":"","rule_id":"","node_id":""}`, true},
		{"value object", "alert", `{"value":{}}`, false},
		{"value string", "alert", `{"value":"85"}`, false},
		{"value overflow", "alert", `{"value":1e999}`, false},
		{"alert timestamp array", "alert", `{"started_at":[]}`, false},
		{"alert timestamp invalid", "alert", `{"resolved_at":"yesterday"}`, false},
		{"null document", "task", `null`, false},
		{"array document", "alert", `[]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validationErr := inbox.ValidateDetails(tc.kind, json.RawMessage(tc.details))
			if tc.valid && validationErr != nil || !tc.valid && validationErr != inbox.ErrInvalidInput {
				t.Fatalf("pure validation error = %v, valid = %t", validationErr, tc.valid)
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			err = inbox.Insert(ctx, tx, inbox.Event{Kind: tc.kind, SourceID: "historical-source", Transition: "snapshot", Audience: "admin", Severity: "info", CreatedAt: time.Now(), Details: json.RawMessage(tc.details)})
			if tc.valid && err != nil {
				t.Fatalf("valid details rejected: %v", err)
			}
			if !tc.valid && err != inbox.ErrInvalidInput {
				t.Fatalf("invalid details accepted: %v", err)
			}
		})
	}
}
