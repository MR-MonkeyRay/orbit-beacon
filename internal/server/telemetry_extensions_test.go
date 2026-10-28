package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestReportAcceptsAndDiscardsUnknownTelemetry(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, token, _ := enrollHTTP(t, f, "extensible", 1)
	body := reportBody(1, 1, "extension-owner", 25)
	var original v1.Report
	if err := json.Unmarshal([]byte(body), &original); err != nil {
		t.Fatal(err)
	}
	body = strings.Replace(body, `"protocol_version":1`, `"future_envelope":{"flag":true},"protocol_version":1`, 1)
	body = strings.Replace(body, `"cpu_percent":25`, `"future_metric":[1,{"x":true}],"cpu_percent":25`, 1)
	body = strings.Replace(body, `"metric_status":{`, `"metric_status":{"future_metric":{"status":true},`, 1)
	body = strings.Replace(body, `"sources":{`, `"sources":{"future_family":[false,42],`, 1)
	body = strings.Replace(body, `"sample_id":"cpu-1"`, `"future_source_metadata":true,"sample_id":"cpu-1"`, 1)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, body), 1)
	latest, err := f.ingest.Latest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(latest.Metrics, original.Metrics) {
		t.Fatalf("unknown telemetry altered known metrics: got %#v want %#v", latest.Metrics, original.Metrics)
	}
}

func TestTelemetryDecodeFailuresDoNotRefreshAcceptedSnapshot(t *testing.T) {
	valid := reportBody(1, 2, "extension-owner", 25)
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"missing source", strings.Replace(valid, `"cpu":{"sample_id":"cpu-2","age_ms":0,"source_interval_ms":10000,"measurement_window_ms":10000},`, "", 1), http.StatusBadRequest},
		{"orphan disk status", strings.Replace(valid, `"metric_status":{`, `"metric_status":{"disk:/ghost":"ok",`, 1), http.StatusBadRequest},
		{"orphan network source", strings.Replace(valid, `"sources":{`, `"sources":{"network:ghost":false,`, 1), http.StatusBadRequest},
		{"known metadata type", strings.Replace(valid, `"cpu_percent":"ok"`, `"cpu_percent":{"status":"ok"}`, 1), http.StatusBadRequest},
		{"second JSON value", valid + `{}`, http.StatusBadRequest},
		{"oversized extension", strings.Replace(valid, `"protocol_version":1`, `"extension":"`+strings.Repeat("x", 64*1024)+`","protocol_version":1`, 1), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentHTTP(t, 1)
			id, token, _ := enrollHTTP(t, f, "reject-extension", 1)
			accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "extension-owner", 10)), 1)
			before := lastSeen(t, f, id)
			f.now = f.now.Add(time.Second)
			assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/report", token, tc.body), tc.status)
			latest, err := f.ingest.Latest(context.Background(), id)
			if err != nil || latest.Seq != 1 || lastSeen(t, f, id) != before {
				t.Fatalf("rejected report mutated state: latest=%#v err=%v", latest, err)
			}
		})
	}
}

func TestTelemetryExtensionsRemainInvalidInManagementWrites(t *testing.T) {
	f := newHTTP(t, 1)
	cookie, csrf := login(t, f, "admin")
	created := request(t, f.handler, http.MethodPost, "/api/v1/nodes", `{"name":"strict-control"}`, cookie, csrf, publicURL)
	assertHTTPStatus(t, created, http.StatusCreated)
	var node struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &node); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"future_envelope":{"flag":true}}`, `{"metric_status":{"future_metric":42}}`, `{"sources":{"future_family":false}}`} {
		apiError(t, request(t, f.handler, http.MethodPatch, "/api/v1/nodes/"+node.ID, body, cookie, csrf, publicURL), http.StatusBadRequest, "invalid_json")
	}
}
