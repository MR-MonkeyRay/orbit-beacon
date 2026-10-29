package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"testing"

	"orbitbeacon/internal/server/nodes"
)

func TestConsoleLiveSnapshotSessionAndInventoryContract(t *testing.T) {
	f := newAgentHTTP(t, 3)
	anonymous := agentHTTPCall(t, f, http.MethodGet, "/api/v1/live", "", nil, "", "")
	assertHTTPStatus(t, anonymous, http.StatusUnauthorized)
	pending, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "private pending"})
	if err != nil {
		t.Fatal(err)
	}
	boundID, _, _ := enrollHTTP(t, f, "private bound", 171)
	cookie, _ := loginAgent(t, f, "admin")
	response := agentHTTPCall(t, f, http.MethodGet, "/api/v1/live", "", cookie, "", "")
	assertHTTPStatus(t, response, http.StatusOK)
	var frame struct {
		Type     string `json:"type"`
		Interval int    `json:"update_interval_seconds"`
		Data     struct {
			Nodes []map[string]json.RawMessage `json:"nodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "snapshot" || frame.Interval != 2 {
		t.Fatalf("unexpected frame: %s", response.Body)
	}
	presence := map[string]string{}
	for _, node := range frame.Data.Nodes {
		keys := make([]string, 0, len(node))
		for key := range node {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, []string{"agent_version", "id", "last_seen", "presence", "status"}) {
			t.Fatalf("noncompact fleet record: %v", keys)
		}
		var id, state string
		if err := json.Unmarshal(node["id"], &id); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(node["presence"], &state); err != nil {
			t.Fatal(err)
		}
		presence[id] = state
		if string(node["last_seen"]) != "null" {
			t.Fatalf("unreported node gained last_seen: %s", node["last_seen"])
		}
	}
	if !reflect.DeepEqual(presence, map[string]string{pending.ID: "pending", boundID: "never"}) {
		t.Fatalf("private inventory presence = %v", presence)
	}
}
