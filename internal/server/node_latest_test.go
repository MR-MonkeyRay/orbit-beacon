package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/ingest"
)

func TestManagementLatestSample(t *testing.T) {
	f := newAgentHTTP(t, 5)
	if _, err := f.auth.BootstrapAdministrator(context.Background(), "admin", password); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.CreateUser(context.Background(), "admin-two", password); err != nil {
		t.Fatal(err)
	}
	sessionFixture := fixture{handler: f.handler, auth: f.auth, nodes: f.nodes, db: f.db}
	second, _ := login(t, sessionFixture, "admin-two")
	id, token, _ := enrollHTTP(t, f, "latest", 81)
	path := "/api/v1/nodes/" + id + "/latest"
	apiError(t, request(t, f.handler, http.MethodGet, path, "", nil, "", ""), http.StatusUnauthorized, "unauthenticated")
	apiError(t, request(t, f.handler, http.MethodGet, path, "", second, "", ""), http.StatusNotFound, "node_not_found")
	body := strings.Replace(reportBody(1, 1, "private-session", 25), `"metrics":`, `"system":{"hostname":"test","os":"linux","arch":"amd64","agent_version":"v1.2.3"},"metrics":`, 1)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, body), 1)
	got := request(t, f.handler, http.MethodGet, path, "", second, "", "")
	assertHTTPStatus(t, got, http.StatusOK)
	var sample map[string]json.RawMessage
	if err := json.Unmarshal(got.Body.Bytes(), &sample); err != nil {
		t.Fatal(err)
	}
	if len(sample) != 5 {
		t.Fatalf("unexpected fields: %s", got.Body)
	}
	for _, key := range []string{"received_at", "sampled_at", "seq", "metrics", "system"} {
		if _, ok := sample[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
	if string(sample["seq"]) != "1" || !strings.Contains(string(sample["system"]), `"agent_version":"v1.2.3"`) || !strings.Contains(string(sample["metrics"]), `"cpu_percent":25`) {
		t.Fatalf("sample=%s", got.Body)
	}
	var received time.Time
	if err := json.Unmarshal(sample["received_at"], &received); err != nil || !received.Equal(f.now) {
		t.Fatalf("received_at=%s err=%v", sample["received_at"], err)
	}
	for _, secret := range []string{"run_generation", "session_id", token, "private-session", "token_hash"} {
		if strings.Contains(got.Body.String(), secret) {
			t.Fatalf("leaked %q: %s", secret, got.Body)
		}
	}
	node := request(t, f.handler, http.MethodGet, "/api/v1/nodes/"+id, "", second, "", "")
	assertHTTPStatus(t, node, http.StatusOK)
	if !strings.Contains(node.Body.String(), `"presence":"online"`) || !strings.Contains(node.Body.String(), `"state":"applied"`) || !strings.Contains(node.Body.String(), `"agent_version":"v1.2.3"`) {
		t.Fatalf("node=%s", node.Body)
	}
	// Optional system on a later report produces explicit null, while inventory
	// retains the previously persisted agent version.
	f.now = f.now.Add(10 * time.Second)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 2, "private-session", 30)), 2)
	got = request(t, f.handler, http.MethodGet, path, "", second, "", "")
	assertHTTPStatus(t, got, http.StatusOK)
	if !strings.Contains(got.Body.String(), `"system":null`) {
		t.Fatalf("sample=%s", got.Body)
	}
	// A restarted service has no report cache despite durable node metadata.
	restarted, err := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, Enrollments: f.enrollments, Ingest: ingest.NewService(f.db, ingest.Options{}), PublicURL: publicURL})
	if err != nil {
		t.Fatal(err)
	}
	apiError(t, request(t, restarted, http.MethodGet, path, "", second, "", ""), http.StatusNotFound, "node_not_found")
	if err := f.nodes.SetDisabled(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	apiError(t, request(t, f.handler, http.MethodGet, path, "", second, "", ""), http.StatusNotFound, "node_not_found")
	if err := f.nodes.SetDisabled(context.Background(), id, false); err != nil {
		t.Fatal(err)
	}
	if err := f.nodes.RevokeCredential(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	apiError(t, request(t, f.handler, http.MethodGet, path, "", second, "", ""), http.StatusNotFound, "node_not_found")
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/nodes/00000000-0000-4000-8000-000000000001/latest", "", second, "", ""), http.StatusNotFound, "node_not_found")
	wrongMethod := request(t, f.handler, http.MethodPost, path, "", second, "", "")
	assertHTTPStatus(t, wrongMethod, http.StatusMethodNotAllowed)
}
