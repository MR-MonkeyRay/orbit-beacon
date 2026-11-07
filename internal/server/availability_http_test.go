package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/availability"
	"orbitbeacon/internal/server/nodes"
)

func TestAvailabilityAPI(t *testing.T) {
	f := newHTTP(t, 2)
	token, _ := login(t, f, "admin-two")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "availability"})
	if err != nil {
		t.Fatal(err)
	}
	route := "/api/v1/nodes/" + node.ID + "/availability"
	for _, query := range []string{"?days=0", "?days=31", "?days=030", "?days=", "?days=30&days=90", "?other=30", "?days=no"} {
		response := request(t, f.handler, http.MethodGet, route+query, "", token, "", "")
		assertHTTPStatus(t, response, 400)
	}
	for _, tc := range []struct {
		query string
		days  int
	}{{"", 30}, {"?days=30", 30}, {"?days=90", 90}} {
		response := request(t, f.handler, http.MethodGet, route+tc.query, "", token, "", "")
		assertHTTPStatus(t, response, 200)
		var out availability.Summary
		if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Days) != tc.days || out.Percent != nil || out.Days[0].Unknown != 86400 {
			t.Fatalf("response %s", response.Body.String())
		}
	}
	response := request(t, f.handler, http.MethodGet, "/api/v1/nodes/00000000-0000-4000-8000-000000000001/availability", "", token, "", "")
	assertHTTPStatus(t, response, 404)
	response = request(t, f.handler, http.MethodGet, route, "", nil, "", "")
	assertHTTPStatus(t, response, http.StatusUnauthorized)
}

func TestRetentionRemovesExpiredAvailability(t *testing.T) {
	f := newHTTP(t, 2)
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "availability"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, day := range []string{"2026-08-07", "2026-08-08"} {
		if _, err := f.db.Exec(`INSERT INTO node_availability_daily VALUES(?,?,60,0)`, node.ID, day); err != nil {
			t.Fatal(err)
		}
	}
	retention, ok := f.handler.(interface {
		CleanupRetention(context.Context, time.Time) (server.RetentionResult, error)
	})
	if !ok {
		t.Fatal("handler does not expose retention cleanup")
	}
	result, err := retention.CleanupRetention(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	var day string
	if err := f.db.QueryRow(`SELECT group_concat(day) FROM node_availability_daily`).Scan(&day); err != nil {
		t.Fatal(err)
	}
	if result.AvailabilityRows != 1 || day != "2026-08-08" {
		t.Fatalf("removed=%d kept=%q", result.AvailabilityRows, day)
	}
}
