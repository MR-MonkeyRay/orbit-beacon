package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
)

func TestConsoleLiveRoutesAuthenticateBeforeQueriesAndRejectInvalidSubscriptions(t *testing.T) {
	f := newAgentHTTP(t, 2)
	cookie, _ := loginAgent(t, f, "admin")
	for _, path := range []string{"/api/v1/live", "/api/v1/live/stream", "/api/v1/live/ws"} {
		for _, query := range []string{"?node_id=no", "?node_id=", "?node_id=00000000-0000-4000-8000-000000000001&node_id=00000000-0000-4000-8000-000000000001", "?command=subscribe"} {
			assertHTTPStatus(t, agentHTTPCall(t, f, http.MethodGet, path+query, "", nil, "", ""), http.StatusUnauthorized)
			assertHTTPStatus(t, agentHTTPCall(t, f, http.MethodGet, path+query, "", cookie, "", ""), http.StatusBadRequest)
		}
		for _, method := range []string{http.MethodPost, http.MethodHead, http.MethodPatch} {
			response := agentHTTPCall(t, f, method, path, "", nil, "", "")
			assertHTTPStatus(t, response, http.StatusMethodNotAllowed)
			if response.Header().Get("Allow") != "GET" {
				t.Fatalf("Allow=%q", response.Header().Get("Allow"))
			}
		}
	}
	for _, path := range []string{"/api/v1/live/extra", "/api/v1/live/stream/extra", "/api/v1/live/ws/"} {
		assertHTTPStatus(t, agentHTTPCall(t, f, http.MethodGet, path, "", cookie, "", ""), http.StatusNotFound)
	}
}

func consoleLiveData(t *testing.T, f *agentFixture, cookie *http.Cookie, query string) map[string]json.RawMessage {
	t.Helper()
	response := agentHTTPCall(t, f, http.MethodGet, "/api/v1/live"+query, "", cookie, "", "")
	assertHTTPStatus(t, response, http.StatusOK)
	var frame struct {
		Type string                     `json:"type"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "snapshot" || frame.Data == nil {
		t.Fatalf("invalid snapshot: %s", response.Body)
	}
	return frame.Data
}

func TestConsoleLiveDetailDistinguishesUnreportedDeletedAndRestarted(t *testing.T) {
	f := newAgentHTTP(t, 2)
	id, token, _ := enrollHTTP(t, f, "detail", 172)
	cookie, _ := loginAgent(t, f, "admin")
	data := consoleLiveData(t, f, cookie, "?node_id="+id)
	if string(data["node"]) == "null" || string(data["latest"]) != "null" || string(data["runtime"]) != "null" {
		t.Fatalf("unreported detail: %s", data)
	}
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "detail", 25)), 1)
	data = consoleLiveData(t, f, cookie, "?node_id="+id)
	if string(data["latest"]) == "null" || string(data["runtime"]) == "null" {
		t.Fatalf("reported detail: %s", data)
	}
	if strings.Contains(string(data["latest"]), "session_id") || strings.Contains(string(data["latest"]), "run_generation") {
		t.Fatal("detail leaked process identity")
	}
	if err := f.nodes.RevokeCredential(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	data = consoleLiveData(t, f, cookie, "?node_id="+id)
	var node nodes.Node
	if err := json.Unmarshal(data["node"], &node); err != nil {
		t.Fatal(err)
	}
	if node.Presence != "pending" || node.LastSeen != nil || string(data["latest"]) != "null" || string(data["runtime"]) != "null" {
		t.Fatalf("revoked detail: %+v %s", node, data)
	}
	if err := f.nodes.Delete(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	data = consoleLiveData(t, f, cookie, "?node_id="+id)
	if string(data["node"]) != "null" || string(data["latest"]) != "null" || string(data["runtime"]) != "null" {
		t.Fatalf("deleted detail: %s", data)
	}
}

func TestConsoleLiveFleetUsesCoalescedReceptionAndRetainsOfflineInventory(t *testing.T) {
	f := newAgentHTTP(t, 3)
	id, token, _ := enrollHTTP(t, f, "coalesced", 173)
	cookie, _ := loginAgent(t, f, "admin")
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "fleet", 25)), 1)
	persisted := lastSeen(t, f, id)
	f.now = f.now.Add(10 * time.Second)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 2, "fleet", 30)), 2)
	if current := lastSeen(t, f, id); current != persisted {
		t.Fatal("fixture did not exercise coalescing")
	}
	for _, elapsed := range []time.Duration{0, nodes.OnlineWindow, nodes.OnlineWindow + time.Nanosecond} {
		t.Run(elapsed.String(), func(t *testing.T) {
			original := f.now
			f.now = f.now.Add(elapsed)
			defer func() { f.now = original }()
			data := consoleLiveData(t, f, cookie, "")
			var list []nodes.Node
			if err := json.Unmarshal(data["nodes"], &list); err != nil {
				t.Fatal(err)
			}
			want := "online"
			if elapsed > nodes.OnlineWindow {
				want = "offline"
			}
			if len(list) != 1 || list[0].Presence != want || list[0].LastSeen == nil || !list[0].LastSeen.Equal(original) {
				t.Fatalf("fleet=%+v want %s received %s", list, want, original)
			}
		})
	}
}

func TestConsoleLiveDetailRetainsDurableRuntimeWithoutInMemorySample(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, token, _ := enrollHTTP(t, f, "restart", 174)
	cookie, _ := loginAgent(t, f, "admin")
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "restart", 25)), 1)
	f.ingest = ingest.NewService(f.db, ingest.Options{Now: func() time.Time { return f.now }})
	var err error
	f.handler, err = server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, Enrollments: f.enrollments, Ingest: f.ingest, PublicURL: publicURL, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	data := consoleLiveData(t, f, cookie, "?node_id="+id)
	if string(data["node"]) == "null" || string(data["latest"]) != "null" || string(data["runtime"]) == "null" {
		t.Fatalf("restart detail=%s", data)
	}
}

func TestConsoleLiveOldAdmissionCannotRepopulateCredentialABADetailOrFleet(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, token, _ := enrollHTTP(t, f, "aba", 175)
	cookie, _ := loginAgent(t, f, "admin")
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "old", 25)), 1)
	admission, err := f.ingest.AdmitToken(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.nodes.RevokeCredential(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	ticket, err := f.enrollments.CreateSingle(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, enrollBody("installation-aba", token)), http.StatusOK)
	if _, err := f.ingest.AcceptAdmitted(context.Background(), admission, mustReport(t, reportBody(1, 2, "old", 90))); !errors.Is(err, nodes.ErrUnauthenticated) {
		t.Fatalf("old ABA admission accepted: %v", err)
	}
	data := consoleLiveData(t, f, cookie, "?node_id="+id)
	var node nodes.Node
	if err := json.Unmarshal(data["node"], &node); err != nil {
		t.Fatal(err)
	}
	if node.Presence != "never" || node.LastSeen != nil || string(data["runtime"]) != "null" || string(data["latest"]) != "null" {
		t.Fatalf("old identity in detail: %+v %s", node, data)
	}
	data = consoleLiveData(t, f, cookie, "")
	var fleet []nodes.Node
	if err := json.Unmarshal(data["nodes"], &fleet); err != nil {
		t.Fatal(err)
	}
	if len(fleet) != 1 || fleet[0].Presence != "never" || fleet[0].LastSeen != nil {
		t.Fatalf("old identity in fleet: %+v", fleet)
	}
}

func TestConsoleLiveFleetEmptyArrayAndDisabledInventory(t *testing.T) {
	f := newAgentHTTP(t, 2)
	cookie, _ := loginAgent(t, f, "admin")
	if data := consoleLiveData(t, f, cookie, ""); string(data["nodes"]) != "[]" {
		t.Fatalf("empty fleet=%s", data)
	}
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "disabled"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.nodes.SetDisabled(context.Background(), node.ID, true); err != nil {
		t.Fatal(err)
	}
	data := consoleLiveData(t, f, cookie, "")
	var fleet []nodes.Node
	if err := json.Unmarshal(data["nodes"], &fleet); err != nil {
		t.Fatal(err)
	}
	if len(fleet) != 1 || fleet[0].Presence != "disabled" || fleet[0].Status != "disabled" {
		t.Fatalf("disabled inventory=%s", data)
	}
}

func TestConsoleLiveCredentialRotationClearsEveryPrivateObservation(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, token, _ := enrollHTTP(t, f, "rotate-live", 176)
	cookie, _ := loginAgent(t, f, "admin")
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(7, 1, "old", 25)), 1)
	ticket, err := f.enrollments.CreateSingle(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"protocol_version": 1, "installation_id": "installation-rotate-live", "node_token": agentSecret(177), "previous_node_token": token, "system": map[string]string{"os": "linux", "arch": "amd64", "agent_version": "replacement"}})
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, string(body)), http.StatusOK)
	data := consoleLiveData(t, f, cookie, "?node_id="+id)
	var node nodes.Node
	if err := json.Unmarshal(data["node"], &node); err != nil {
		t.Fatal(err)
	}
	if node.Presence != "never" || node.LastSeen != nil || node.AgentVersion != "replacement" || string(data["latest"]) != "null" || string(data["runtime"]) != "null" {
		t.Fatalf("rotation reused old identity: %+v %s", node, data)
	}
	data = consoleLiveData(t, f, cookie, "")
	var fleet []nodes.Node
	if err := json.Unmarshal(data["nodes"], &fleet); err != nil {
		t.Fatal(err)
	}
	if len(fleet) != 1 || fleet[0].Presence != "never" || fleet[0].LastSeen != nil || fleet[0].AgentVersion != "replacement" {
		t.Fatalf("rotated fleet=%+v", fleet)
	}
}
