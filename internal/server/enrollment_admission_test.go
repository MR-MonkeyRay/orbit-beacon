package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

func enrollmentAttempt(f *agentFixture, peer, token string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, publicURL+"/api/agent/v1/enroll", body)
	req.RemoteAddr = peer
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// 不可信的转发头绝不能创建新的直接对端桶。
	req.Header.Set("X-Forwarded-For", token)
	req.Header.Set("Forwarded", "for="+token)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, req)
	return response
}

func TestEnrollmentRejectsInvalidCredentialBeforeReadingBody(t *testing.T) {
	for _, kind := range []string{"missing", "overlong", "unknown", "revoked", "expired", "consumed"} {
		t.Run(kind, func(t *testing.T) {
			f := newAgentHTTP(t, 1)
			token := agentSecret(99)
			if kind == "missing" {
				token = ""
			}
			if kind == "overlong" {
				token = strings.Repeat("x", 257)
			}
			if kind == "revoked" || kind == "expired" || kind == "consumed" {
				node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: kind})
				if err != nil {
					t.Fatal(err)
				}
				ticket, err := f.enrollments.CreateSingle(context.Background(), node.ID)
				if err != nil {
					t.Fatal(err)
				}
				token = ticket.Token
				if kind == "revoked" {
					if err := f.enrollments.Revoke(context.Background(), ticket.ID); err != nil {
						t.Fatal(err)
					}
				} else if kind == "consumed" {
					assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/enroll", token, enrollBody("consumed-installation", agentSecret(1))), http.StatusOK)
				} else {
					f.now = ticket.ExpiresAt
				}
			}
			for _, path := range []string{"/api/agent/v1/enroll", "/api/agent/v1/enroll/check"} {
				body := &observedReportBody{reader: strings.NewReader(`{"protocol_version":`)}
				request := httptest.NewRequest(http.MethodPost, publicURL+path, body)
				request.RemoteAddr = "192.0.2.1:1000"
				request.Header.Set("Content-Type", "application/json")
				if token != "" {
					request.Header.Set("Authorization", "Bearer "+token)
				}
				response := httptest.NewRecorder()
				f.handler.ServeHTTP(response, request)
				if body.reads != 0 {
					t.Errorf("%s: invalid ticket caused %d body reads before admission", path, body.reads)
				}
				responseError := apiError(t, response, http.StatusForbidden, "forbidden")
				if string(responseError["message"]) != `"Enrollment ticket is unavailable"` {
					t.Fatal("invalid ticket returned a different public message")
				}
			}
		})
	}
}

func TestEnrollmentInvalidDirectPeerBurstIsBoundedAndCannotChargeValidTicket(t *testing.T) {
	f := newAgentHTTP(t, 1)
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "legitimate"})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := f.enrollments.CreateSingle(context.Background(), node.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 无论源端口、提供的凭据或攻击者可控的转发头如何，
	// 一个直接对端最多立即获得 32 次无效尝试。
	for attempt := range 33 {
		body := &observedReportBody{reader: strings.NewReader(enrollBody("attacker", agentSecret(99)))}
		response := enrollmentAttempt(f, fmt.Sprintf("192.0.2.10:%d", 1000+attempt), agentSecret(byte(attempt)), body)
		apiError(t, response, http.StatusForbidden, "forbidden")
		if body.reads != 0 {
			t.Fatal("invalid ticket admission read request body")
		}
		if limited := response.Header().Get("Retry-After") != ""; limited != (attempt == 32) {
			t.Fatalf("invalid attempt %d: rate limited=%v", attempt+1, limited)
		}
	}
	// 在全局预算未耗尽时，另一个直接对端保留自己的配额。
	// 无效对端绝不消耗合法票据的预算。
	other := enrollmentAttempt(f, "192.0.2.11:1234", agentSecret(98), strings.NewReader(`{`))
	apiError(t, other, http.StatusForbidden, "forbidden")
	if other.Header().Get("Retry-After") != "" {
		t.Fatal("invalid peer exhausted another peer's bucket")
	}
	assertHTTPStatus(t, enrollmentAttempt(f, "192.0.2.10:9999", ticket.Token, strings.NewReader(enrollBody("legitimate-installation", agentSecret(1)))), http.StatusOK)
	stored, err := f.enrollments.Get(context.Background(), ticket.ID)
	if err != nil || stored.Uses != 1 {
		t.Fatalf("invalid flood altered legitimate ticket uses: %+v %v", stored, err)
	}
}

func TestEnrollmentInvalidGlobalBurstIsBoundedAcrossFreshPeers(t *testing.T) {
	f := newAgentHTTP(t, 1)
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "global survivor"})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := f.enrollments.CreateSingle(context.Background(), node.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 即使每次尝试都有新的直接对端地址、因而无法命中先前用过的对端桶，
	// 也要限制未认证工作的总量。
	for attempt := range 257 {
		peer := fmt.Sprintf("198.18.%d.%d:1234", attempt/256, attempt%256)
		body := &observedReportBody{reader: strings.NewReader(enrollBody("attacker", agentSecret(99)))}
		response := enrollmentAttempt(f, peer, agentSecret(byte(attempt)), body)
		apiError(t, response, http.StatusForbidden, "forbidden")
		if body.reads != 0 {
			t.Fatal("global invalid admission read request body")
		}
		if limited := response.Header().Get("Retry-After") != ""; limited != (attempt == 256) {
			t.Fatalf("global invalid attempt %d: rate limited=%v", attempt+1, limited)
		}
	}
	assertHTTPStatus(t, enrollmentAttempt(f, "198.18.1.0:1234", ticket.Token, strings.NewReader(enrollBody("survivor-installation", agentSecret(1)))), http.StatusOK)
}

func TestEnrollmentUnknownCredentialDoesNotWaitForWriterLock(t *testing.T) {
	f := newAgentHTTP(t, 1)
	// 获取写锁前先打开连接池中的两个连接，避免被测路径包含连接建立或迁移。
	reader, err := f.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.PingContext(context.Background()); err != nil {
		_ = reader.Close()
		t.Fatal(err)
	}
	writer, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		_ = reader.Close()
		t.Fatal(err)
	}
	defer writer.Rollback()
	_ = reader.Close()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- enrollmentAttempt(f, "192.0.2.100:1234", agentSecret(99), strings.NewReader(enrollBody("invalid-installation", agentSecret(99))))
	}()
	select {
	case response := <-done:
		apiError(t, response, http.StatusForbidden, "forbidden")
	case <-time.After(500 * time.Millisecond):
		// 报告失败前先释放写锁，避免遗留的阻塞请求等满数据库五秒的忙等超时。
		_ = writer.Rollback()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("invalid request stayed blocked after writer release")
		}
		t.Fatal("invalid enrollment attempted a write transaction instead of rejecting by read-only credential lookup")
	}
}

type enrollmentMutationBody struct {
	reader     *strings.Reader
	beforeRead func()
}

func (b *enrollmentMutationBody) Read(p []byte) (int, error) {
	if b.beforeRead != nil {
		mutate := b.beforeRead
		b.beforeRead = nil
		mutate()
	}
	return b.reader.Read(p)
}

func TestEnrollmentRechecksRevocationInsideClaimTransaction(t *testing.T) {
	f := newAgentHTTP(t, 1)
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "revoked after admission"})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := f.enrollments.CreateSingle(context.Background(), node.ID)
	if err != nil {
		t.Fatal(err)
	}
	body := &enrollmentMutationBody{reader: strings.NewReader(enrollBody("racing-installation", agentSecret(1))), beforeRead: func() {
		if err := f.enrollments.Revoke(context.Background(), ticket.ID); err != nil {
			t.Fatal(err)
		}
	}}
	response := enrollmentAttempt(f, "192.0.2.20:1234", ticket.Token, body)
	apiError(t, response, http.StatusForbidden, "forbidden")
	var uses, claims, enrolled int
	if err := f.db.QueryRow("SELECT uses FROM enrollment_tokens WHERE id=?", ticket.ID).Scan(&uses); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT count(*) FROM enrollment_claims WHERE enrollment_id=?", ticket.ID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT count(*) FROM nodes WHERE token_hash IS NOT NULL").Scan(&enrolled); err != nil {
		t.Fatal(err)
	}
	if uses != 0 || claims != 0 || enrolled != 0 {
		t.Fatalf("revocation between admission and transaction still claimed identity: uses=%d claims=%d enrolled=%d", uses, claims, enrolled)
	}
	var result v1.EnrollmentResponse
	if json.Unmarshal(response.Body.Bytes(), &result) == nil && result.NodeID != "" {
		t.Fatal("revoked claim returned an enrollment success")
	}
}

func TestEnrollmentMissingBearerSharesBoundedInvalidAdmission(t *testing.T) {
	f := newAgentHTTP(t, 1)
	for attempt := range 33 {
		body := &observedReportBody{reader: strings.NewReader(`{`)}
		response := enrollmentAttempt(f, "192.0.2.50:1234", "", body)
		if body.reads != 0 {
			t.Fatal("missing credential read enrollment body")
		}
		apiError(t, response, http.StatusForbidden, "forbidden")
		if limited := response.Header().Get("Retry-After") != ""; limited != (attempt == 32) {
			t.Fatalf("missing credential attempt %d: rate limited=%v", attempt+1, limited)
		}
	}
}

func TestEnrollmentRechecksConsumptionInsideClaimTransaction(t *testing.T) {
	f := newAgentHTTP(t, 1)
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "consumed after admission"})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := f.enrollments.CreateSingle(context.Background(), node.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimBody := enrollBody("winning-installation", agentSecret(1))
	body := &enrollmentMutationBody{reader: strings.NewReader(claimBody), beforeRead: func() {
		assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, claimBody), http.StatusOK)
	}}
	response := enrollmentAttempt(f, "192.0.2.20:1234", ticket.Token, body)
	apiError(t, response, http.StatusForbidden, "forbidden")
	var uses, claims, audits int
	if err := f.db.QueryRow("SELECT uses FROM enrollment_tokens WHERE id=?", ticket.ID).Scan(&uses); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT count(*) FROM enrollment_claims WHERE enrollment_id=?", ticket.ID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT count(*) FROM audit_events WHERE action='enrollment.claim' AND target_id=?", node.ID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if uses != 1 || claims != 1 || audits != 1 {
		t.Fatalf("consumption race changed successful claim: uses=%d claims=%d audits=%d", uses, claims, audits)
	}
	if active, err := f.nodes.AuthenticateToken(context.Background(), agentSecret(1)); err != nil || active.ID != node.ID {
		t.Fatal("rejected replay changed the winning credential")
	}
}
