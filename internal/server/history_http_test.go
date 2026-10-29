package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/nodes"
)

func TestHistoryAPIValidationSecondAdministratorAndDisabledHistory(t *testing.T) {
	f := newHTTP(t, 5)
	second, _ := login(t, f, "admin-two")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "history"})
	if err != nil {
		t.Fatal(err)
	}
	route := "/api/v1/nodes/" + node.ID + "/history"
	valid := "?metric=cpu_percent&range=1h"
	apiError(t, request(t, f.handler, http.MethodGet, route+valid, "", nil, "", ""), 401, "unauthenticated")
	for _, query := range []string{"", "?metric=cpu_percent", "?range=1h", "?metric=unknown&range=1h", "?metric=cpu_percent&range=48h", "?metric=cpu_percent&range=1H", "?metric=cpu_percent&metric=load1&range=1h", "?metric=cpu_percent&range=1h&range=6h", "?metric=cpu_percent&range=1h&extra=x", "?metric=cpu_percent&range=%zz"} {
		t.Run(query, func(t *testing.T) {
			apiError(t, request(t, f.handler, http.MethodGet, route+query, "", second, "", ""), 400, "validation_failed")
		})
	}
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/nodes/00000000-0000-4000-8000-000000000001/history"+valid, "", second, "", ""), 404, "node_not_found")
	response := request(t, f.handler, http.MethodGet, route+valid, "", second, "", "")
	assertHTTPStatus(t, response, 200)
	if !strings.Contains(response.Body.String(), `"points":[]`) {
		t.Fatalf("response=%s", response.Body)
	}
	assertHTTPStatus(t, request(t, f.handler, http.MethodPost, route+valid, "", second, "", ""), 405)
}

func TestHistoryHTTPIngestFlushTiersAndDeleteVisibility(t *testing.T) {
	f := newAgentHTTP(t, 5)
	if _, err := f.auth.CreateUser(context.Background(), "admin", password, false); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	hs, err := history.Open(context.Background(), dir, f.db, history.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer hs.Close()
	// NewHandler wires its default ingest service to the history sink.
	h, err := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, Enrollments: f.enrollments, History: hs, PublicURL: publicURL, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	f.handler = h
	cookie, _ := login(t, fixture{handler: h, auth: f.auth, nodes: f.nodes, db: f.db}, "admin")
	id, token, _ := enrollHTTP(t, f, "history", 90)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "private-session", 25)), 1)
	if err := h.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := hs.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Querying remains available after writer shutdown, before database close.
	for _, test := range []struct {
		metric, rangeName string
		step              int
		unit              string
		avg               float64
	}{
		{"cpu_percent", "1h", 60, "percent", 25}, {"cpu_percent", "6h", 60, "percent", 25},
		{"cpu_percent", "24h", 300, "percent", 25}, {"cpu_percent", "7d", 300, "percent", 25}, {"cpu_percent", "30d", 3600, "percent", 25},
		{"memory_used_percent", "1h", 60, "percent", 50}, {"load1", "1h", 60, "load", 0.5},
	} {
		response := request(t, h, http.MethodGet, "/api/v1/nodes/"+id+"/history?metric="+test.metric+"&range="+test.rangeName, "", cookie, "", "")
		assertHTTPStatus(t, response, 200)
		var result history.Response
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.StepSeconds != test.step || result.Unit != test.unit || len(result.Points) != 1 || result.Points[0].Avg != test.avg || result.Points[0].Max != test.avg || result.Points[0].T.Location() != time.UTC {
			t.Fatalf("response=%s", response.Body)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
			t.Fatal(err)
		}
		if len(object) != 5 {
			t.Fatalf("unexpected API fields: %s", response.Body)
		}
	}
	if err := f.nodes.Delete(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	apiError(t, request(t, h, http.MethodGet, "/api/v1/nodes/"+id+"/history?metric=cpu_percent&range=1h", "", cookie, "", ""), 404, "node_not_found")
}
