package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

func TestProbesAPIEnvelopeOrderingAccessAndMethods(t *testing.T) {
	f := newHTTP(t, 5)
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	ingester := ingest.NewService(f.nodes.Database(), ingest.Options{Now: func() time.Time { return now }})
	handler, err := server.NewHandler(testHandlerOptions(t, server.Options{Auth: f.auth, Nodes: f.nodes, Ingest: ingester, PublicURL: publicURL, Now: func() time.Time { return now }}))
	if err != nil {
		t.Fatal(err)
	}
	f.handler = handler
	_, _ = login(t, f, "admin")
	second, _ := login(t, f, "admin-two")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "probe api"})
	if err != nil {
		t.Fatal(err)
	}
	targets := []string{"icmp://pending.example", "https://up.example/health", "tcp://down.example:443"}
	token := agentSecret(29)
	hash := sha256.Sum256([]byte(token))
	if _, err := f.db.Exec(`UPDATE nodes SET token_hash=?,status='unknown',run_generation='1',report_session_id='probe-fixture' WHERE id=?`, hash[:], node.ID); err != nil {
		t.Fatal(err)
	}
	checked := timekey.Format(now.Add(-time.Minute))
	unconfigured := "https://old.example/removed"
	if _, err := f.db.Exec(`INSERT INTO probe_latest(node_id,target,ok,latency_ms,error,status_code,checked_at) VALUES(?,?,1,12.5,'',200,?),(?,?,0,NULL,'timeout',NULL,?),(?,?,0,NULL,'timeout',503,?)`, node.ID, targets[1], checked, node.ID, targets[2], checked, node.ID, unconfigured, checked); err != nil {
		t.Fatal(err)
	}
	expiry := now.Add(24 * time.Hour)
	if _, err := f.db.Exec(`UPDATE probe_latest SET tls_expires_at=? WHERE node_id=? AND target=?`, timekey.Format(expiry), node.ID, targets[1]); err != nil {
		t.Fatal(err)
	}
	hour := timekey.Format(now.Truncate(time.Hour))
	boundary := timekey.Format(now.Truncate(time.Hour).Add(-23 * time.Hour))
	old := timekey.Format(now.Truncate(time.Hour).Add(-24 * time.Hour))
	if _, err := f.db.Exec(`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms) VALUES(?,?,?,4,1,30),(?,?,?,2,2,0),(?,?,?,1,0,10),(?,?,?,100,100,0),(?,?,?,9,9,0)`, node.ID, targets[1], hour, node.ID, targets[2], hour, node.ID, targets[1], boundary, node.ID, targets[1], old, node.ID, unconfigured, hour); err != nil {
		t.Fatal(err)
	}
	var report v1.Report
	if err := json.Unmarshal([]byte(reportBody(1, 1, "probe-fixture", 10)), &report); err != nil {
		t.Fatal(err)
	}
	at := now.Add(-time.Minute)
	latency, code := 12.5, 200
	report.SampledAt = now
	report.ProbeResults = []v1.ProbeResult{
		{Target: targets[0], Error: "pending"},
		{Target: targets[1], OK: true, LatencyMS: &latency, StatusCode: &code, CheckedAt: at, TLSExpiresAt: &expiry},
		{Target: targets[2], Error: "timeout", CheckedAt: at},
	}
	admission, err := ingester.AdmitToken(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingester.AcceptAdmitted(t.Context(), admission, report); err != nil {
		t.Fatal(err)
	}
	route := "/api/v1/nodes/" + node.ID + "/probes"
	response := request(t, f.handler, http.MethodGet, route, "", second, "", "")
	assertHTTPStatus(t, response, http.StatusOK)
	var envelope struct {
		Probes []struct {
			Target       string     `json:"target"`
			Status       string     `json:"status"`
			LatencyMS    *float64   `json:"latency_ms"`
			Error        string     `json:"error"`
			StatusCode   *int       `json:"status_code"`
			CheckedAt    *string    `json:"checked_at"`
			TLSExpiresAt *time.Time `json:"tls_expires_at"`
			Uptime24H    *float64   `json:"uptime_24h"`
			Checks24H    int64      `json:"checks_24h"`
		} `json:"probes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Probes) != 3 {
		t.Fatalf("probes=%s", response.Body)
	}
	if bytes.Contains(response.Body.Bytes(), []byte(unconfigured)) {
		t.Fatalf("unconfigured target leaked into response: %s", response.Body)
	}
	var raw struct {
		Probes []map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"latency_ms", "status_code", "checked_at", "tls_expires_at", "uptime_24h"} {
		if value, ok := raw.Probes[1][key]; !ok || string(value) != "null" {
			t.Fatalf("pending %s=%s present=%t; body=%s", key, value, ok, response.Body)
		}
	}
	for i, target := range []string{targets[1], targets[0], targets[2]} {
		if envelope.Probes[i].Target != target {
			t.Fatalf("order=%s", response.Body)
		}
	}
	if p := envelope.Probes[1]; p.Status != "pending" || p.CheckedAt != nil || p.Uptime24H != nil || p.Checks24H != 0 {
		t.Fatalf("pending=%+v", p)
	}
	if p := envelope.Probes[0]; p.TLSExpiresAt == nil || !p.TLSExpiresAt.Equal(expiry) || p.Status != "up" || p.LatencyMS == nil || *p.LatencyMS != 12.5 || p.StatusCode == nil || *p.StatusCode != 200 || p.Uptime24H == nil || *p.Uptime24H != 80 || p.Checks24H != 5 {
		t.Fatalf("up=%+v", p)
	}
	if p := envelope.Probes[2]; p.Status != "down" || p.Error != "timeout" || p.LatencyMS != nil || p.Uptime24H == nil || *p.Uptime24H != 0 || p.Checks24H != 2 {
		t.Fatalf("down=%+v", p)
	}
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/nodes/00000000-0000-4000-8000-000000000001/probes", "", second, "", ""), http.StatusNotFound, "node_not_found")
	methodResponse := request(t, f.handler, http.MethodPost, route, "", second, "", "")
	apiError(t, methodResponse, http.StatusMethodNotAllowed, "method_not_allowed")
	if methodResponse.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("Allow=%q", methodResponse.Header().Get("Allow"))
	}
	empty, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "empty probe api"})
	if err != nil {
		t.Fatal(err)
	}
	emptyResponse := request(t, f.handler, http.MethodGet, "/api/v1/nodes/"+empty.ID+"/probes", "", second, "", "")
	assertHTTPStatus(t, emptyResponse, http.StatusOK)
	if emptyResponse.Body.String() != "{\"probes\":[]}\n" {
		t.Fatalf("empty probe envelope=%s", emptyResponse.Body)
	}
}

func TestProbeHistoryAPIValidationWindowsAndShape(t *testing.T) {
	f := newHTTP(t, 5)
	now := time.Date(2026, 11, 6, 12, 34, 56, 0, time.UTC)
	handler, err := server.NewHandler(testHandlerOptions(t, server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL, Now: func() time.Time { return now }}))
	if err != nil {
		t.Fatal(err)
	}
	f.handler = handler
	cookie, _ := login(t, f, "admin")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "history"})
	if err != nil {
		t.Fatal(err)
	}
	const target = "https://example.test/health?a=1&b=2"
	if _, err := f.db.Exec(`UPDATE nodes SET token_hash=zeroblob(32),run_generation='1',report_session_id='session' WHERE id=?`, node.ID); err != nil {
		t.Fatal(err)
	}
	route := "/api/v1/nodes/" + node.ID + "/probes/history"
	targetQuery := "target=" + url.QueryEscape(target)
	for _, tc := range []struct {
		query  string
		fields []string
	}{
		{"", []string{"target", "range"}}, {"?range=24h", []string{"target"}}, {"?" + targetQuery, []string{"range"}},
		{"?" + targetQuery + "&range=1h", []string{"range"}}, {"?target=ftp%3A%2F%2Fexample.test&range=24h", []string{"target"}},
		{"?" + targetQuery + "&range=24h&range=7d", []string{"range"}}, {"?" + targetQuery + "&target=tcp%3A%2F%2Fexample.test%3A80&range=24h", []string{"target"}},
	} {
		response := request(t, f.handler, http.MethodGet, route+tc.query, "", cookie, "", "")
		raw := apiError(t, response, http.StatusBadRequest, "validation_failed")
		var fields map[string]string
		if err := json.Unmarshal(raw["fields"], &fields); err != nil {
			t.Fatalf("fields: %v body=%s", err, response.Body)
		}
		for _, field := range tc.fields {
			if _, ok := fields[field]; !ok {
				t.Fatalf("missing field %s: %s", field, response.Body)
			}
		}
	}
	query := "?" + targetQuery + "&range=24h"
	empty := request(t, f.handler, http.MethodGet, route+query, "", cookie, "", "")
	assertHTTPStatus(t, empty, http.StatusOK)
	if !bytes.Contains(empty.Body.Bytes(), []byte(`"points":[]`)) {
		t.Fatalf("empty=%s", empty.Body)
	}
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/nodes/00000000-0000-4000-8000-000000000001/probes/history"+query, "", cookie, "", ""), http.StatusNotFound, "node_not_found")
	for _, hours := range []int{0, 1, 23, 24, 167, 168} {
		checks, failures, sum, maximum := 4, 2, 30, 20
		if hours == 1 {
			checks, failures, sum, maximum = 2, 2, 0, 0
		}
		if _, err := f.db.Exec(`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms,latency_max_ms) VALUES(?,?,?,?,?,?,?)`, node.ID, target, timekey.Format(now.Truncate(time.Hour).Add(-time.Duration(hours)*time.Hour)), checks, failures, sum, maximum); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		rangeName       string
		count, boundary int
	}{{"24h", 3, 23}, {"7d", 5, 167}} {
		response := request(t, f.handler, http.MethodGet, route+"?"+targetQuery+"&range="+tc.rangeName, "", cookie, "", "")
		assertHTTPStatus(t, response, http.StatusOK)
		var data struct {
			Target, Range, Unit string
			StepSeconds         int `json:"step_seconds"`
			Points              []struct {
				T                time.Time
				Avg, Max         *float64
				Checks, Failures int
			}
		}
		if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		if data.Target != target || data.Range != tc.rangeName || data.Unit != "ms" || data.StepSeconds != 3600 || len(data.Points) != tc.count || !data.Points[0].T.Equal(now.Truncate(time.Hour).Add(-time.Duration(tc.boundary)*time.Hour)) {
			t.Fatalf("response=%s", response.Body)
		}
		failed, mixed := data.Points[len(data.Points)-2], data.Points[len(data.Points)-1]
		if failed.Avg != nil || failed.Max != nil || failed.Checks != 2 || failed.Failures != 2 || mixed.Avg == nil || *mixed.Avg != 15 || mixed.Max == nil || *mixed.Max != 20 {
			t.Fatalf("points=%s", response.Body)
		}
	}
}
