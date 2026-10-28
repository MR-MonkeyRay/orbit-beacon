package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

func TestProbesAPIEnvelopeOrderingAccessAndMethods(t *testing.T) {
	f := newHTTP(t, 5)
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	handler, err := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	f.handler = handler
	admin, csrf := login(t, f, "admin")
	second, _ := login(t, f, "admin-two")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "probe api"})
	if err != nil {
		t.Fatal(err)
	}
	targets := []string{"icmp://pending.example", "https://up.example/health", "tcp://down.example:443"}
	body, _ := json.Marshal(map[string]any{"probe_targets": targets})
	path := "/api/v1/nodes/" + node.ID + "/config"
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, path, string(body), admin, csrf, publicURL), http.StatusOK)
	checked := timekey.Format(now.Add(-time.Minute))
	unconfigured := "https://old.example/removed"
	if _, err := f.db.Exec(`INSERT INTO probe_latest(node_id,target,ok,latency_ms,error,status_code,checked_at) VALUES(?,?,1,12.5,'',200,?),(?,?,0,NULL,'timeout',NULL,?),(?,?,0,NULL,'timeout',503,?)`, node.ID, targets[1], checked, node.ID, targets[2], checked, node.ID, unconfigured, checked); err != nil {
		t.Fatal(err)
	}
	hour := timekey.Format(now.Truncate(time.Hour))
	boundary := timekey.Format(now.Truncate(time.Hour).Add(-23 * time.Hour))
	old := timekey.Format(now.Truncate(time.Hour).Add(-24 * time.Hour))
	if _, err := f.db.Exec(`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms) VALUES(?,?,?,4,1,30),(?,?,?,2,2,0),(?,?,?,1,0,10),(?,?,?,100,100,0),(?,?,?,9,9,0)`, node.ID, targets[1], hour, node.ID, targets[2], hour, node.ID, targets[1], boundary, node.ID, targets[1], old, node.ID, unconfigured, hour); err != nil {
		t.Fatal(err)
	}
	route := "/api/v1/nodes/" + node.ID + "/probes"
	response := request(t, f.handler, http.MethodGet, route, "", second, "", "")
	assertHTTPStatus(t, response, http.StatusOK)
	var envelope struct {
		Probes []struct {
			Target     string   `json:"target"`
			Status     string   `json:"status"`
			LatencyMS  *float64 `json:"latency_ms"`
			Error      string   `json:"error"`
			StatusCode *int     `json:"status_code"`
			CheckedAt  *string  `json:"checked_at"`
			Uptime24H  *float64 `json:"uptime_24h"`
			Checks24H  int64    `json:"checks_24h"`
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
	for _, key := range []string{"latency_ms", "status_code", "checked_at", "uptime_24h"} {
		if value, ok := raw.Probes[0][key]; !ok || string(value) != "null" {
			t.Fatalf("pending %s=%s present=%t; body=%s", key, value, ok, response.Body)
		}
	}
	for i, target := range targets {
		if envelope.Probes[i].Target != target {
			t.Fatalf("order=%s", response.Body)
		}
	}
	if p := envelope.Probes[0]; p.Status != "pending" || p.CheckedAt != nil || p.Uptime24H != nil || p.Checks24H != 0 {
		t.Fatalf("pending=%+v", p)
	}
	if p := envelope.Probes[1]; p.Status != "up" || p.LatencyMS == nil || *p.LatencyMS != 12.5 || p.StatusCode == nil || *p.StatusCode != 200 || p.Uptime24H == nil || *p.Uptime24H != 80 || p.Checks24H != 5 {
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
