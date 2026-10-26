package server_test

import (
	"context"
	"encoding/json"
	"net/http"
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
