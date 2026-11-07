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
	for _, kind := range []string{"missing", "unknown", "revoked", "expired"} {
		t.Run(kind, func(t *testing.T) {
			f := newAgentHTTP(t, 1)
			token := agentSecret(99)
			if kind == "missing" {
				token = ""
			}
			if kind == "revoked" || kind == "expired" {
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
				} else {
					f.now = ticket.ExpiresAt
				}
			}
			body := &observedReportBody{reader: strings.NewReader(`{"protocol_version":`)}
			response := enrollmentAttempt(f, "192.0.2.1:1000", token, body)
			if body.reads != 0 {
				t.Errorf("invalid enrollment credential caused %d body reads before admission", body.reads)
			}
			assertHTTPStatus(t, response, http.StatusUnauthorized)
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
		body := strings.NewReader(enrollBody("attacker", agentSecret(99)))
		response := enrollmentAttempt(f, fmt.Sprintf("192.0.2.10:%d", 1000+attempt), agentSecret(byte(attempt)), body)
		if response.Code != http.StatusUnauthorized && response.Code != http.StatusTooManyRequests {
			t.Fatalf("invalid attempt %d: status=%d body=%s", attempt+1, response.Code, response.Body.String())
		}
		if attempt == 32 {
			assertHTTPStatus(t, response, http.StatusTooManyRequests)
			if response.Header().Get("Retry-After") == "" {
				t.Fatal("invalid enrollment admission lacks Retry-After")
			}
		}
	}
	// 在全局预算未耗尽时，另一个直接对端保留自己的配额。
	// 无效对端绝不消耗合法票据的预算。
	assertHTTPStatus(t, enrollmentAttempt(f, "192.0.2.11:1234", agentSecret(98), strings.NewReader(`{`)), http.StatusUnauthorized)
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
		body := strings.NewReader(enrollBody("attacker", agentSecret(99)))
		response := enrollmentAttempt(f, peer, agentSecret(byte(attempt)), body)
		if response.Code != http.StatusUnauthorized && response.Code != http.StatusTooManyRequests {
			t.Fatalf("attempt %d returned %d", attempt+1, response.Code)
		}
		if attempt == 256 {
			assertHTTPStatus(t, response, http.StatusTooManyRequests)
			if response.Header().Get("Retry-After") == "" {
				t.Fatal("global invalid admission lacks Retry-After")
			}
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
		assertHTTPStatus(t, response, http.StatusUnauthorized)
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
	assertHTTPStatus(t, response, http.StatusUnauthorized)
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
		if response.Code != http.StatusUnauthorized && response.Code != http.StatusTooManyRequests {
			t.Fatalf("missing credential attempt %d returned %d", attempt+1, response.Code)
		}
		if attempt == 32 {
			assertHTTPStatus(t, response, http.StatusTooManyRequests)
			if response.Header().Get("Retry-After") == "" {
				t.Fatal("missing-credential limiter lacks Retry-After")
			}
		}
	}
}
