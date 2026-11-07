package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"orbitbeacon/internal/server/nodes"
)

func TestExpiryHTTPPatch(t *testing.T) {
	f := newHTTP(t, 2)
	cookie, csrf := login(t, f, "admin")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "expiry"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/nodes/" + node.ID
	for _, tc := range []struct {
		body string
		want any
	}{
		{`{}`, nil}, {`{"expires_on":"2028-02-29"}`, "2028-02-29"}, {`{"notes":"preserve date"}`, "2028-02-29"}, {`{"expires_on":null}`, nil},
	} {
		res := request(t, f.handler, http.MethodPatch, path, tc.body, cookie, csrf, publicURL)
		assertHTTPStatus(t, res, 200)
		got := decodeObject(t, res)
		if v, ok := got["expires_on"]; !ok || v != tc.want {
			t.Fatalf("%s: %+v", tc.body, got)
		}
		got = decodeObject(t, request(t, f.handler, http.MethodGet, path, "", cookie, "", ""))
		if got["expires_on"] != tc.want {
			t.Fatalf("GET: %+v", got)
		}
	}
	for _, raw := range []string{`"2026-02-30"`, `"2026-2-01"`, `""`, `123`, `true`, `{}`} {
		res := request(t, f.handler, http.MethodPatch, path, `{"expires_on":`+raw+`}`, cookie, csrf, publicURL)
		body := apiError(t, res, 400, "validation_failed")
		var fields map[string]string
		if err := json.Unmarshal(body["fields"], &fields); err != nil || fields["expires_on"] == "" {
			t.Fatalf("field error: %s %v", res.Body, err)
		}
	}
	apiError(t, request(t, f.handler, http.MethodPatch, path, `{"expires_on":null,"future":true}`, cookie, csrf, publicURL), 400, "invalid_json")
	var audits int
	if err := f.db.QueryRow("SELECT count(*) FROM audit_events WHERE action='node.metadata.patch' AND target_id=?", node.ID).Scan(&audits); err != nil || audits != 4 {
		t.Fatalf("audits=%d: %v", audits, err)
	}
}

func TestExpiryHTTPRuleCreateAndValidation(t *testing.T) {
	f := newHTTP(t, 1)
	cookie, csrf := login(t, f, "admin")
	expiry := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(validAlertRule, "cpu_percent", "node_expiry_days"), `"comparator":">"`, `"comparator":"<"`), `"threshold":80`, `"threshold":30`)
	for _, tc := range []struct{ body, field string }{
		{strings.Replace(expiry, `"threshold":30`, `"threshold":0`, 1), "threshold"},
		{strings.Replace(expiry, `"threshold":30`, `"threshold":366`, 1), "threshold"},
		{strings.Replace(expiry, `"comparator":"<"`, `"comparator":">"`, 1), "comparator"},
	} {
		res := request(t, f.handler, "POST", "/api/v1/alert-rules", tc.body, cookie, csrf, publicURL)
		body := apiError(t, res, 400, "validation_failed")
		var fields map[string]string
		if err := json.Unmarshal(body["fields"], &fields); err != nil || fields[tc.field] == "" {
			t.Fatalf("fields=%s err=%v", res.Body, err)
		}
	}
	for _, metric := range []string{"node_expiry_days", "traffic_quota_percent", "cpu_percent", "memory_used_percent", "swap_used_percent", "disk_used_percent", "load1", "probes_down", "offline"} {
		body := strings.Replace(validAlertRule, "cpu_percent", metric, 1)
		if metric == "node_expiry_days" {
			body = expiry
		}
		if metric == "probes_down" {
			body = strings.Replace(body, `"threshold":80`, `"threshold":1`, 1)
		}
		if metric == "offline" {
			body = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(body, `"threshold":80`, `"threshold":null`), `"comparator":">"`, `"comparator":null`), `"for_seconds":60`, `"for_seconds":180`)
		}
		res := request(t, f.handler, "POST", "/api/v1/alert-rules", body, cookie, csrf, publicURL)
		if res.Code != 201 {
			t.Fatalf("metric %s: %d %s", metric, res.Code, res.Body)
		}
	}
}
