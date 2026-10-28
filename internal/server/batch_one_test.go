package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
)

func TestDisabledNodeIsRejectedUntilEnabled(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, token, _ := enrollHTTP(t, f, "a", 1)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "a", 10)), 1)
	if err := f.nodes.SetDisabled(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	before := lastSeen(t, f, id)
	f.now = f.now.Add(time.Second)
	response := agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 2, "a", 10))
	apiErrorAgent(t, response, http.StatusForbidden, "node_disabled")
	if after := lastSeen(t, f, id); after != before {
		t.Fatalf("disabled report refreshed last_seen: %v -> %v", before, after)
	}
	recovery := agentRequest(t, f, "/api/agent/v1/recover", token, `{"installation_id":"installation-a"}`)
	apiErrorAgent(t, recovery, http.StatusForbidden, "node_disabled")
	if err := f.nodes.SetDisabled(context.Background(), id, false); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 3, "a", 10)), 3)
}

func TestRevokedCredentialKeepsAssetAndAllowsReenrollment(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, token, _ := enrollHTTP(t, f, "a", 1)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "a", 10)), 1)
	enabled, name := true, "Tokyo"
	if _, err := f.nodes.PatchPublicSettings(context.Background(), id, nodes.PublicSettingsPatch{Enabled: &enabled, Name: &name}); err != nil {
		t.Fatal(err)
	}
	if err := f.nodes.RevokeCredential(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := f.nodes.RevokeCredential(context.Background(), id); err != nil {
		t.Fatalf("repeated revoke failed: %v", err)
	}
	node, err := f.nodes.Get(context.Background(), id)
	if err != nil || node.Status != "pending" || node.Name != "a" {
		t.Fatalf("revoked node = %+v, %v", node, err)
	}
	f.now = f.now.Add(time.Second)
	response := agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 2, "a", 10))
	apiErrorAgent(t, response, http.StatusUnauthorized, "unauthenticated")
	ticket, err := f.enrollments.CreateSingle(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	fresh := agentSecret(9)
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, enrollBody("installation-new", fresh)), http.StatusOK)
	f.now = f.now.Add(time.Second)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", fresh, reportBody(1, 1, "new", 10)), 1)
	settings, err := f.nodes.GetPublicSettings(context.Background(), id)
	if err != nil || !settings.Enabled || settings.Name != "Tokyo" {
		t.Fatalf("revocation lost public settings: %+v %v", settings, err)
	}
	if errors.Is(f.nodes.RevokeCredential(context.Background(), "00000000-0000-4000-8000-000000000009"), nodes.ErrNotFound) == false {
		t.Fatal("revoking an unknown node did not report not found")
	}
}

func TestEnrollmentClaimAndSessionsAreAudited(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, _, ticketID := enrollHTTP(t, f, "a", 1)
	var actor *string
	var details string
	if err := f.db.QueryRow("SELECT actor_id, details FROM audit_events WHERE action='enrollment.claim' AND target_id=?", id).Scan(&actor, &details); err != nil {
		t.Fatalf("enrollment claim was not audited: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(details), &decoded); err != nil {
		t.Fatal(err)
	}
	if actor != nil || decoded["actor_kind"] != "agent" || decoded["enrollment_id"] != ticketID || decoded["rotation"] != false {
		t.Fatalf("claim audit actor=%v details=%s", actor, details)
	}

	h := newHTTP(t, 1)
	cookie, csrf := login(t, h, "admin")
	var created int
	if err := h.db.QueryRow("SELECT count(*) FROM audit_events WHERE action='session.create' AND actor_id=target_id").Scan(&created); err != nil || created != 1 {
		t.Fatalf("login audit count=%d err=%v", created, err)
	}
	assertHTTPStatus(t, request(t, h.handler, http.MethodDelete, "/api/v1/session", "", cookie, csrf, publicURL), http.StatusNoContent)
	var deleted int
	if err := h.db.QueryRow("SELECT count(*) FROM audit_events WHERE action='session.delete'").Scan(&deleted); err != nil || deleted != 1 {
		t.Fatalf("logout audit count=%d err=%v", deleted, err)
	}
}

func TestNewerSchemaIsRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "server.db")
	db, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES (?, '2026-01-01T00:00:00.000000000Z')", storage.CurrentSchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Open(context.Background(), path); !errors.Is(err, storage.ErrSchemaTooNew) {
		t.Fatalf("newer schema opened: %v", err)
	}
}

func apiErrorAgent(t *testing.T, response interface {
	Result() *http.Response
}, status int, code string) {
	t.Helper()
	result := response.Result()
	defer result.Body.Close()
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(result.Body).Decode(&body)
	if result.StatusCode != status || body.Code != code {
		t.Fatalf("status=%d code=%q, want %d %q", result.StatusCode, body.Code, status, code)
	}
}
