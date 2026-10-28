package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

func TestConfigPatchRejectsEmptyNullAndUnknownFieldsAtomically(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "patch presence"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/nodes/" + node.ID + "/config"
	before := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
	for _, body := range []string{
		`{}`, `{"report_interval_seconds":null}`, `{"disk_interval_seconds":null}`,
		`{"interfaces":null}`, `{"disk_paths":null}`, `{"billing_day":null}`, `{"quota_bytes":null}`,
		`{"probe_targets":null}`,
		`{"report_interval_seconds":null,"quota_bytes":"01"}`,
	} {
		t.Run(body, func(t *testing.T) {
			response := request(t, f.handler, http.MethodPatch, path, body, admin, csrf, publicURL)
			apiError(t, response, http.StatusBadRequest, "validation_failed")
			after := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
			if before.Body.String() != after.Body.String() {
				t.Fatal("rejected PATCH changed configuration or revision")
			}
		})
	}
	unknown := request(t, f.handler, http.MethodPatch, path, `{"report_interval_seconds":10,"surprise":true}`, admin, csrf, publicURL)
	apiError(t, unknown, http.StatusBadRequest, "invalid_json")
	var events int
	if err := f.nodes.Database().QueryRow("SELECT count(*) FROM audit_events WHERE action='node.config.patch' AND target_id=?", node.ID).Scan(&events); err != nil || events != 0 {
		t.Fatalf("invalid PATCH emitted audit events: count=%d err=%v", events, err)
	}
}

func TestConfigPatchProbeTargetsValidationAuditAndRemovalPurge(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "probe config"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/nodes/" + node.ID + "/config"
	targets := []string{"tcp://db.internal:5432", "https://example.test/health?token=secret"}
	body, _ := json.Marshal(map[string]any{"probe_targets": targets})
	response := request(t, f.handler, http.MethodPatch, path, string(body), admin, csrf, publicURL)
	assertHTTPStatus(t, response, http.StatusOK)
	var cfg v1.DesiredConfig
	if err := json.Unmarshal(response.Body.Bytes(), &cfg); err != nil || !slices.Equal(cfg.ProbeTargets, targets) {
		t.Fatalf("probe targets=%v err=%v", cfg.ProbeTargets, err)
	}
	var details string
	if err := f.db.QueryRow(`SELECT details FROM audit_events WHERE action='node.config.patch' AND target_id=?`, node.ID).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(details, "secret") || strings.Contains(details, "example.test") || details != `{"probe_targets":{"count":2}}` {
		t.Fatalf("unsafe probe audit details=%s", details)
	}
	checked := "2026-10-27T01:02:03.000000000Z"
	if _, err := f.db.Exec(`INSERT INTO probe_latest(node_id,target,ok,latency_ms,error,status_code,checked_at) VALUES(?,?,1,1.5,'',200,?),(?,?,1,2.5,'',NULL,?)`, node.ID, targets[0], checked, node.ID, targets[1], checked); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms) VALUES(?,?,?,1,0,1.5),(?,?,?,1,0,2.5)`, node.ID, targets[0], checked, node.ID, targets[1], checked); err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(map[string]any{"probe_targets": targets[1:]})
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, path, string(body), admin, csrf, publicURL), http.StatusOK)
	for _, table := range []string{"probe_latest", "probe_hourly"} {
		var count int
		if err := f.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE node_id=? AND target=?`, node.ID, targets[0]).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s removed target count=%d err=%v", table, count, err)
		}
		if err := f.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE node_id=? AND target=?`, node.ID, targets[1]).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s retained target count=%d err=%v", table, count, err)
		}
	}
	before := cfg.Revision + 1
	oversized, _ := json.Marshal(map[string]any{"probe_targets": []string{"icmp://" + strings.Repeat("a", 1018)}})
	whitespace, _ := json.Marshal(map[string]any{"probe_targets": []string{"https://example.test/health check"}})
	for _, invalid := range append([]string{
		`{"probe_targets":["udp://host:7"]}`,
		`{"probe_targets":["tcp://host:80","tcp://host:80"]}`,
		`{"probe_targets":["icmp://a","icmp://b","icmp://c","icmp://d","icmp://e","icmp://f"]}`,
		`{"probe_targets":["https://user:password@example.test/health"]}`,
		`{"probe_targets":["https://example.test/health#fragment"]}`,
	}, string(whitespace), string(oversized)) {
		object := apiError(t, request(t, f.handler, http.MethodPatch, path, invalid, admin, csrf, publicURL), http.StatusBadRequest, "validation_failed")
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(object["fields"], &fields); err != nil || fields["probe_targets"] == nil {
			t.Fatalf("missing probe_targets error: %+v", object)
		}
	}
	var after v1.DesiredConfig
	get := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
	if err := json.Unmarshal(get.Body.Bytes(), &after); err != nil || after.Revision != before || !slices.Equal(after.ProbeTargets, targets[1:]) {
		t.Fatalf("invalid patches changed config: %+v err=%v", after, err)
	}
}

func TestConfigPatchProbeRemovalRollsBackWhenAuditFails(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "probe rollback"})
	if err != nil {
		t.Fatal(err)
	}
	target := "tcp://rollback.example:443"
	path := "/api/v1/nodes/" + node.ID + "/config"
	body, _ := json.Marshal(map[string]any{"probe_targets": []string{target}})
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, path, string(body), admin, csrf, publicURL), http.StatusOK)
	checked := "2026-10-27T01:02:03.000000000Z"
	if _, err := f.db.Exec(`INSERT INTO probe_latest(node_id,target,ok,latency_ms,error,status_code,checked_at) VALUES(?,?,1,1.5,'',NULL,?)`, node.ID, target, checked); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms) VALUES(?,?,?,1,0,1.5)`, node.ID, target, checked); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER reject_probe_patch_audit BEFORE INSERT ON audit_events WHEN NEW.action='node.config.patch' BEGIN SELECT RAISE(ABORT,'test rollback'); END`); err != nil {
		t.Fatal(err)
	}
	response := request(t, f.handler, http.MethodPatch, path, `{"probe_targets":[]}`, admin, csrf, publicURL)
	apiError(t, response, http.StatusInternalServerError, "internal_error")
	for _, table := range []string{"probe_latest", "probe_hourly"} {
		var count int
		if err := f.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE node_id=? AND target=?`, node.ID, target).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s rollback count=%d err=%v", table, count, err)
		}
	}
	var cfg v1.DesiredConfig
	get := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
	if err := json.Unmarshal(get.Body.Bytes(), &cfg); err != nil || !slices.Equal(cfg.ProbeTargets, []string{target}) {
		t.Fatalf("rollback config=%+v err=%v", cfg, err)
	}
}

func TestConfigPatchUnchangedValuesDoNotPublishAnotherRevision(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "idempotent patch"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/nodes/" + node.ID + "/config"
	initial := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
	var cfg v1.DesiredConfig
	if err := json.Unmarshal(initial.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"report_interval_seconds": cfg.ReportIntervalSeconds, "disk_interval_seconds": cfg.DiskIntervalSeconds, "billing_day": cfg.BillingDay, "quota_bytes": cfg.QuotaBytes, "interfaces": cfg.Interfaces, "disk_paths": cfg.DiskPaths})
	if err != nil {
		t.Fatal(err)
	}
	unchanged := request(t, f.handler, http.MethodPatch, path, string(body), admin, csrf, publicURL)
	assertHTTPStatus(t, unchanged, http.StatusOK)
	if unchanged.Body.String() != initial.Body.String() {
		t.Fatal("unchanged values published a new configuration")
	}
	interval := cfg.ReportIntervalSeconds + 1
	if interval > 60 {
		interval = 5
	}
	body, err = json.Marshal(map[string]int{"report_interval_seconds": interval})
	if err != nil {
		t.Fatal(err)
	}
	changed := request(t, f.handler, http.MethodPatch, path, string(body), admin, csrf, publicURL)
	assertHTTPStatus(t, changed, http.StatusOK)
	var next v1.DesiredConfig
	if err := json.Unmarshal(changed.Body.Bytes(), &next); err != nil || next.Revision != cfg.Revision+1 {
		t.Fatalf("real change revision=%d err=%v", next.Revision, err)
	}
	retry := request(t, f.handler, http.MethodPatch, path, string(body), admin, csrf, publicURL)
	assertHTTPStatus(t, retry, http.StatusOK)
	if retry.Body.String() != changed.Body.String() {
		t.Fatal("same-value retry advanced revision")
	}
	var events int
	if err := f.nodes.Database().QueryRow("SELECT count(*) FROM audit_events WHERE action='node.config.patch' AND target_id=?", node.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("no-op PATCH changed audit history: count=%d err=%v", events, err)
	}
}
