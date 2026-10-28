package server_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
)

type agentFixture struct {
	handler     http.Handler
	db          *sql.DB
	auth        *auth.Service
	nodes       *nodes.Service
	enrollments *enrollment.Service
	ingest      *ingest.Service
	now         time.Time
}

func newAgentHTTP(t *testing.T, capacity int) *agentFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &agentFixture{db: db, now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return f.now }
	f.nodes = nodes.NewService(db, nodes.Options{MaxNodes: capacity, Now: clock, Random: rand.NewChaCha8([32]byte{21})})
	f.enrollments = enrollment.NewService(db, enrollment.Options{MaxNodes: capacity, Now: clock, Random: rand.NewChaCha8([32]byte{22})})
	f.ingest = ingest.NewService(db, ingest.Options{Now: clock})
	f.auth = auth.NewService(db, auth.Options{Now: clock})
	f.handler, err = server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, Enrollments: f.enrollments, Ingest: f.ingest, PublicURL: publicURL, Now: clock, WebUpdateIntervalSeconds: 2})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func agentRequest(t *testing.T, f *agentFixture, route, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, publicURL+route, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, req)
	return response
}

func agentSecret(n byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{n}, 32))
}
func enrollBody(installation, token string) string {
	return fmt.Sprintf(`{"protocol_version":1,"installation_id":%q,"node_token":%q,"system":{"hostname":"identical-host","os":"linux","arch":"amd64"}}`, installation, token)
}

func enrollHTTP(t *testing.T, f *agentFixture, name string, secret byte) (string, string, string) {
	t.Helper()
	pending, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := f.enrollments.CreateSingle(context.Background(), pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	token := agentSecret(secret)
	response := agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, enrollBody("installation-"+name, token))
	assertHTTPStatus(t, response, http.StatusOK)
	var result v1.EnrollmentResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.NodeID != pending.ID {
		t.Fatalf("server guessed node from hostname: got %s want %s", result.NodeID, pending.ID)
	}
	return pending.ID, token, ticket.ID
}

func reportBody(generation, seq uint64, session string, cpu int) string {
	return fmt.Sprintf(`{"protocol_version":1,"run_generation":%d,"session_id":%q,"seq":%d,"sampled_at":"2026-10-26T12:00:00Z","runtime":{"report_interval_seconds":10,"disk_interval_seconds":60,"interfaces":[],"disk_paths":[],"probe_targets":[]},"metrics":{"cpu_percent":%d,"load1":0.5,"load5":0.5,"load15":0.5,"memory_total_bytes":8192,"memory_available_bytes":4096,"swap_total_bytes":0,"swap_used_bytes":0,"uptime_seconds":100,"disks":[],"networks":[],"metric_status":{"cpu_percent":"ok","load1":"ok","load5":"ok","load15":"ok","memory_total_bytes":"ok","memory_available_bytes":"ok","swap_total_bytes":"ok","swap_used_bytes":"ok","uptime_seconds":"ok"},"sources":{"cpu":{"sample_id":"cpu-%d","age_ms":0,"source_interval_ms":10000,"measurement_window_ms":10000},"load":{"sample_id":"load-1","age_ms":0,"source_interval_ms":10000},"memory":{"sample_id":"memory-1","age_ms":0,"source_interval_ms":10000},"swap":{"sample_id":"swap-1","age_ms":0,"source_interval_ms":10000},"uptime":{"sample_id":"uptime-1","age_ms":0,"source_interval_ms":10000}}},"traffic":[]}`, generation, session, seq, cpu, seq)
}

func accepted(t *testing.T, r *httptest.ResponseRecorder, seq uint64) v1.ReportResponse {
	t.Helper()
	assertHTTPStatus(t, r, http.StatusOK)
	var response v1.ReportResponse
	if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.AcceptedSeq != seq {
		t.Fatalf("accepted_seq=%d want %d", response.AcceptedSeq, seq)
	}
	return response
}

func lastSeen(t *testing.T, f *agentFixture, id string) sql.NullString {
	t.Helper()
	var value sql.NullString
	if err := f.db.QueryRow("SELECT last_seen FROM nodes WHERE id=?", id).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestAgentEnrollmentRouteExactRetryExpiryAndConflicts(t *testing.T) {
	f := newAgentHTTP(t, 4)
	pending, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "prebuilt"})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := f.enrollments.CreateSingle(context.Background(), pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	body := enrollBody("installation-a", agentSecret(1))
	first := agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, body)
	assertHTTPStatus(t, first, http.StatusOK)
	f.now = f.now.Add(24 * time.Hour)
	retry := agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, body)
	assertHTTPStatus(t, retry, http.StatusOK)
	var result v1.EnrollmentResponse
	if err := json.Unmarshal(retry.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.NodeID != pending.ID {
		t.Fatal("retry created a different asset")
	}
	f.now = f.now.Add(-23 * time.Hour)
	for _, conflict := range []string{enrollBody("installation-a", agentSecret(2)), enrollBody("installation-b", agentSecret(1))} {
		apiError(t, agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, conflict), http.StatusConflict, "enrollment_conflict")
	}
	secondTicket, err := f.enrollments.CreateSingle(context.Background(), pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	apiError(t, agentRequest(t, f, "/api/agent/v1/enroll", secondTicket.Token, enrollBody("different-installation", agentSecret(3))), http.StatusConflict, "enrollment_conflict")
	if err := f.enrollments.Revoke(context.Background(), ticket.ID); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, body), http.StatusUnauthorized)
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/enroll", "", body), http.StatusUnauthorized)
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM nodes").Scan(&count); err != nil || count != 1 {
		t.Fatalf("retry assets=%d err=%v", count, err)
	}
}

func TestReportUsesBearerNodeAndNeverClientTargetOrHostname(t *testing.T) {
	f := newAgentHTTP(t, 2)
	a, tokenA, _ := enrollHTTP(t, f, "a", 1)
	b, tokenB, _ := enrollHTTP(t, f, "b", 2)
	body := reportBody(1, 1, "session-a", 25)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", tokenA, body), 1)
	if !lastSeen(t, f, a).Valid || lastSeen(t, f, b).Valid {
		t.Fatal("report not isolated to authenticated node")
	}
	for _, target := range []string{`"node_id":"` + b + `",`, `"gid":"` + b + `",`} {
		f.now = f.now.Add(time.Second)
		accepted(t, agentRequest(t, f, "/api/agent/v1/report", tokenA, "{"+target+body[1:]), 1)
	}
	if lastSeen(t, f, b).Valid {
		t.Fatal("body target altered another node")
	}
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", tokenB, reportBody(1, 1, "session-b", 75)), 1)
	latestA, err := f.ingest.Latest(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	latestB, err := f.ingest.Latest(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if latestA.Metrics.CPUPercent == nil || *latestA.Metrics.CPUPercent != 25 || latestB.Metrics.CPUPercent == nil || *latestB.Metrics.CPUPercent != 75 {
		t.Fatal("latest metrics mixed nodes with identical hostnames")
	}
	req := httptest.NewRequest(http.MethodGet, publicURL+"/api/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+tokenA)
	r := httptest.NewRecorder()
	f.handler.ServeHTTP(r, req)
	assertHTTPStatus(t, r, http.StatusUnauthorized)
	if err := f.nodes.Delete(context.Background(), a, true); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/report", tokenA, body), http.StatusUnauthorized)
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/report", "", body), http.StatusUnauthorized)
}

func TestReportValidationNeverRefreshesLastSeen(t *testing.T) {
	valid := reportBody(1, 2, "session-a", 25)
	for _, tt := range []struct {
		name, body string
		status     int
	}{
		{"protocol", strings.Replace(valid, `"protocol_version":1`, `"protocol_version":2`, 1), http.StatusUpgradeRequired},
		{"negative CPU", strings.Replace(valid, `"cpu_percent":25`, `"cpu_percent":-1`, 1), http.StatusBadRequest},
		{"CPU over 100", strings.Replace(valid, `"cpu_percent":25`, `"cpu_percent":101`, 1), http.StatusBadRequest},
		{"NaN", strings.Replace(valid, `"cpu_percent":25`, `"cpu_percent":NaN`, 1), http.StatusBadRequest},
		{"infinite", strings.Replace(valid, `"cpu_percent":25`, `"cpu_percent":1e999`, 1), http.StatusBadRequest},
		{"memory range", strings.Replace(valid, `"memory_available_bytes":4096`, `"memory_available_bytes":8193`, 1), http.StatusBadRequest},
		{"negative load", strings.Replace(valid, `"load1":0.5`, `"load1":-1`, 1), http.StatusBadRequest},
		{"negative uptime", strings.Replace(valid, `"uptime_seconds":100`, `"uptime_seconds":-1`, 1), http.StatusBadRequest},
		{"negative age", strings.Replace(valid, `"age_ms":0`, `"age_ms":-1`, 1), http.StatusBadRequest},
		{"unknown status", strings.Replace(valid, `"cpu_percent":"ok"`, `"cpu_percent":"invented"`, 1), http.StatusBadRequest},
		{"swap range", strings.Replace(valid, `"swap_used_bytes":0`, `"swap_used_bytes":1`, 1), http.StatusBadRequest},
		{"disk range", strings.Replace(valid, `"disks":[]`, `"disks":[{"path":"/","total_bytes":100,"used_bytes":101,"sampled_at":"2026-10-26T12:00:00Z"}]`, 1), http.StatusBadRequest},
		{"negative network", strings.Replace(valid, `"networks":[]`, `"networks":[{"name":"eth0","rx_bytes_per_sec":-1,"tx_bytes_per_sec":0}]`, 1), http.StatusBadRequest},
		{"unmarked missing metric", strings.Replace(valid, `"cpu_percent":25`, `"cpu_percent":null`, 1), http.StatusBadRequest},
		{"body limit", valid + strings.Repeat(" ", 64*1024-len(valid)+1), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newAgentHTTP(t, 1)
			id, token, _ := enrollHTTP(t, f, "a", 1)
			accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "session-a", 10)), 1)
			before := lastSeen(t, f, id)
			f.now = f.now.Add(time.Second)
			r := agentRequest(t, f, "/api/agent/v1/report", token, tt.body)
			if tt.status == http.StatusUpgradeRequired {
				apiError(t, r, tt.status, "protocol_unsupported")
			} else {
				assertHTTPStatus(t, r, tt.status)
			}
			if lastSeen(t, f, id) != before {
				t.Fatal("invalid report refreshed last_seen")
			}
			latest, err := f.ingest.Latest(context.Background(), id)
			if err != nil || latest.Seq != 1 {
				t.Fatalf("invalid report replaced latest: seq=%d err=%v", latest.Seq, err)
			}
		})
	}
	f := newAgentHTTP(t, 1)
	_, token, _ := enrollHTTP(t, f, "boundary", 1)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, valid+strings.Repeat(" ", 64*1024-len(valid))), 2)
}

func TestReportRateLimitAverageOnePerSecondBurstThreePerNode(t *testing.T) {
	f := newAgentHTTP(t, 2)
	_, tokenA, _ := enrollHTTP(t, f, "a", 1)
	_, tokenB, _ := enrollHTTP(t, f, "b", 2)
	for seq := uint64(1); seq <= 3; seq++ {
		accepted(t, agentRequest(t, f, "/api/agent/v1/report", tokenA, reportBody(1, seq, "a", 10)), seq)
	}
	r := agentRequest(t, f, "/api/agent/v1/report", tokenA, reportBody(1, 4, "a", 10))
	assertHTTPStatus(t, r, http.StatusTooManyRequests)
	if r.Header().Get("Retry-After") == "" {
		t.Fatal("rate limit lacks Retry-After")
	}
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", tokenB, reportBody(1, 1, "b", 10)), 1)
	f.now = f.now.Add(999 * time.Millisecond)
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/report", tokenA, reportBody(1, 4, "a", 10)), http.StatusTooManyRequests)
	f.now = f.now.Add(time.Millisecond)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", tokenA, reportBody(1, 4, "a", 10)), 4)
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/report", tokenA, reportBody(1, 5, "a", 10)), http.StatusTooManyRequests)
}

func TestReportGenerationSessionAndSequenceOrdering(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, token, _ := enrollHTTP(t, f, "a", 1)
	send := func(generation, seq uint64, session string, cpu int) *httptest.ResponseRecorder {
		f.now = f.now.Add(time.Second)
		return agentRequest(t, f, "/api/agent/v1/report", token, reportBody(generation, seq, session, cpu))
	}
	accepted(t, send(5, 10, "current", 10), 10)
	before := lastSeen(t, f, id)
	for _, seq := range []uint64{10, 9, 1} {
		accepted(t, send(5, seq, "current", 99), 10)
		if lastSeen(t, f, id) != before {
			t.Fatal("duplicate/out-of-order refreshed last_seen")
		}
		latest, err := f.ingest.Latest(context.Background(), id)
		if err != nil || latest.Seq != 10 || latest.Metrics.CPUPercent == nil || *latest.Metrics.CPUPercent != 10 {
			t.Fatalf("duplicate replaced latest: %+v %v", latest, err)
		}
	}
	assertHTTPStatus(t, send(4, 100, "old", 99), http.StatusConflict)
	apiError(t, send(5, 11, "clone", 99), http.StatusConflict, "identity_conflict")
	if lastSeen(t, f, id) != before {
		t.Fatal("old/clone process refreshed last_seen")
	}
	accepted(t, send(6, 1, "replacement", 60), 1)
	assertHTTPStatus(t, send(5, 999, "current", 99), http.StatusConflict)
	latest, err := f.ingest.Latest(context.Background(), id)
	if err != nil || latest.RunGeneration != 6 || latest.SessionID != "replacement" || latest.Seq != 1 {
		t.Fatalf("replacement not authoritative: %+v %v", latest, err)
	}
	// A new server service loses only transient seq deduplication, not the process fence.
	restarted := ingest.NewService(f.db, ingest.Options{Now: func() time.Time { return f.now }})
	if _, err := restarted.Accept(context.Background(), id, mustReport(t, reportBody(5, 1000, "current", 99))); err == nil {
		t.Fatal("generation fence lost on server restart")
	}
	if _, err := restarted.Accept(context.Background(), id, mustReport(t, reportBody(6, 2, "other-session", 99))); err == nil {
		t.Fatal("session fence lost on server restart")
	}
}

func mustReport(t *testing.T, body string) v1.Report {
	t.Helper()
	var report v1.Report
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestNodeTokenCollisionIsConflictAndRotationRevokesOldCredential(t *testing.T) {
	f := newAgentHTTP(t, 2)
	a, oldToken, _ := enrollHTTP(t, f, "a", 1)
	b, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	ticketB, err := f.enrollments.CreateSingle(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	apiError(t, agentRequest(t, f, "/api/agent/v1/enroll", ticketB.Token, enrollBody("installation-b", oldToken)), http.StatusConflict, "enrollment_conflict")
	var uses int
	if err := f.db.QueryRow("SELECT uses FROM enrollment_tokens WHERE id=?", ticketB.ID).Scan(&uses); err != nil || uses != 0 {
		t.Fatalf("conflict consumed ticket: uses=%d err=%v", uses, err)
	}
	rotation, err := f.enrollments.CreateSingle(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	newToken := agentSecret(3)
	body, err := json.Marshal(v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "installation-a", NodeToken: newToken, PreviousNodeToken: oldToken, System: v1.System{Hostname: "identical-host", OS: "linux", Arch: "amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	r := agentRequest(t, f, "/api/agent/v1/enroll", rotation.Token, string(body))
	assertHTTPStatus(t, r, http.StatusOK)
	var result v1.EnrollmentResponse
	if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil || result.NodeID != a {
		t.Fatalf("rotation changed binding: %+v %v", result, err)
	}
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/report", oldToken, reportBody(1, 1, "a", 10)), http.StatusUnauthorized)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", newToken, reportBody(1, 1, "a", 10)), 1)
}

func TestSecondAdministratorCanIssueReadAndRevokeEnrollment(t *testing.T) {
	f := newAgentHTTP(t, 2)
	second, csrf := loginAgent(t, f, "admin-two")
	created := agentHTTPCall(t, f, http.MethodPost, "/api/v1/nodes", `{"name":"second administrator node"}`, second, csrf, publicURL)
	assertHTTPStatus(t, created, http.StatusCreated)
	nodeID := objectID(t, decodeObject(t, created))
	body := `{"node_id":"` + nodeID + `"}`
	apiError(t, agentHTTPCall(t, f, http.MethodPost, "/api/v1/enrollments", body, nil, "", publicURL), http.StatusUnauthorized, "unauthenticated")
	apiError(t, agentHTTPCall(t, f, http.MethodPost, "/api/v1/enrollments", body, second, "", publicURL), http.StatusForbidden, "forbidden")
	apiError(t, agentHTTPCall(t, f, http.MethodPost, "/api/v1/enrollments", body, second, csrf, "https://foreign.test"), http.StatusForbidden, "forbidden")
	response := agentHTTPCall(t, f, http.MethodPost, "/api/v1/enrollments", body, second, csrf, publicURL)
	assertHTTPStatus(t, response, http.StatusCreated)
	var ticket enrollment.CreatedTicket
	if err := json.Unmarshal(response.Body.Bytes(), &ticket); err != nil || ticket.Token == "" || !uuidPattern.MatchString(ticket.ID) {
		t.Fatalf("enrollment creation=%s err=%v", response.Body, err)
	}
	path := "/api/v1/enrollments/" + ticket.ID
	read := agentHTTPCall(t, f, http.MethodGet, path, "", second, "", "")
	assertHTTPStatus(t, read, http.StatusOK)
	if objectID(t, decodeObject(t, read)) != ticket.ID {
		t.Fatal("second administrator cannot read its issued enrollment")
	}
	collectionContains(t, agentHTTPCall(t, f, http.MethodGet, "/api/v1/enrollments", "", second, "", ""), "enrollments", ticket.ID)
	first, _ := loginAgent(t, f, "admin")
	assertHTTPStatus(t, agentHTTPCall(t, f, http.MethodGet, path, "", first, "", ""), http.StatusOK)
	assertHTTPStatus(t, agentHTTPCall(t, f, http.MethodDelete, path, "", second, csrf, publicURL), http.StatusNoContent)
	apiError(t, agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, enrollBody("revoked-installation", agentSecret(90))), http.StatusUnauthorized, "unauthenticated")
}
