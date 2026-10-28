package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

func TestReportAdmissionChargesAuthenticatedRejectedAttemptsBeforeDecode(t *testing.T) {
	valid := reportBody(2, 2, "owner", 10)
	for _, tt := range []struct {
		name   string
		body   string
		status int
	}{
		{"malformed JSON", `{"protocol_version":`, http.StatusBadRequest},
		{"oversized body", valid + strings.Repeat(" ", 64*1024+1), http.StatusRequestEntityTooLarge},
		{"unsupported protocol", strings.Replace(valid, `"protocol_version":1`, `"protocol_version":2`, 1), http.StatusUpgradeRequired},
		{"identity conflict", reportBody(2, 2, "clone", 10), http.StatusConflict},
		{"old generation", reportBody(1, 2, "owner", 10), http.StatusConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newAgentHTTP(t, 2)
			id, token, _ := enrollHTTP(t, f, "a", 1)
			_, otherToken, _ := enrollHTTP(t, f, "b", 2)
			accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(2, 1, "owner", 10)), 1)
			before := lastSeen(t, f, id)
			f.now = f.now.Add(3 * time.Second)
			for attempt := 1; attempt <= 4; attempt++ {
				r := agentRequest(t, f, "/api/agent/v1/report", token, tt.body)
				want := tt.status
				if attempt == 4 {
					want = http.StatusTooManyRequests
				}
				if r.Code != want {
					t.Fatalf("attempt %d: status=%d want=%d body=%s", attempt, r.Code, want, r.Body.String())
				}
				if attempt == 4 && r.Header().Get("Retry-After") == "" {
					t.Fatal("admission rejection lacks Retry-After")
				}
			}
			assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/report", token, valid), http.StatusTooManyRequests)
			if lastSeen(t, f, id) != before {
				t.Fatal("rejected attempt refreshed last_seen")
			}
			accepted(t, agentRequest(t, f, "/api/agent/v1/report", otherToken, reportBody(1, 1, "other", 10)), 1)
		})
	}
}

type observedReportBody struct {
	reads  int
	reader *strings.Reader
}

func (b *observedReportBody) Read(p []byte) (int, error) { b.reads++; return b.reader.Read(p) }

func TestReportAdmissionAuthenticatesBeforeReadingOrChargingBody(t *testing.T) {
	f := newAgentHTTP(t, 1)
	_, token, _ := enrollHTTP(t, f, "a", 1)
	for range 5 {
		body := &observedReportBody{reader: strings.NewReader(`{`)}
		req := httptest.NewRequest(http.MethodPost, publicURL+"/api/agent/v1/report", body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+agentSecret(99))
		r := httptest.NewRecorder()
		f.handler.ServeHTTP(r, req)
		assertHTTPStatus(t, r, http.StatusUnauthorized)
		if body.reads != 0 {
			t.Fatalf("unauthenticated report body read %d times", body.reads)
		}
	}
	for seq := uint64(1); seq <= 3; seq++ {
		accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, seq, "owner", 10)), seq)
	}
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 4, "owner", 10)), http.StatusTooManyRequests)
}

type resettingReportBody struct {
	reader *strings.Reader
	reset  func()
}

func (b *resettingReportBody) Read(p []byte) (int, error) {
	if b.reset != nil {
		reset := b.reset
		b.reset = nil
		reset()
	}
	return b.reader.Read(p)
}

func TestReportAdmissionRejectsExactABAResetDuringBodyRead(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, token, _ := enrollHTTP(t, f, "admission-epoch", 1)
	body := &resettingReportBody{reader: strings.NewReader(reportBody(1, 1, "old-admission", 10))}
	body.reset = func() {
		if err := f.nodes.RevokeCredential(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		ticket, err := f.enrollments.CreateSingle(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.enrollments.Enroll(context.Background(), ticket.Token, v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "installation-admission-epoch", NodeToken: token, System: v1.System{Hostname: "rebound", OS: "linux", Arch: "amd64"}}); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, publicURL+"/api/agent/v1/report", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	r := httptest.NewRecorder()
	f.handler.ServeHTTP(r, req)
	if r.Code != http.StatusUnauthorized && r.Code != http.StatusConflict {
		t.Fatalf("pre-reset admission accepted after body read: status=%d body=%s", r.Code, r.Body.String())
	}
	var runtime string
	var seen any
	if err := f.db.QueryRow(`SELECT agent_runtime,last_seen FROM nodes WHERE id=?`, id).Scan(&runtime, &seen); err != nil || runtime != "" || seen != nil {
		t.Fatalf("pre-reset request restored runtime=%q last_seen=%v err=%v", runtime, seen, err)
	}
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "fresh-admission", 10)), 1)
}
