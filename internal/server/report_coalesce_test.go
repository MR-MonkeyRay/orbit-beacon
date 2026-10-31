package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
)

func reportWithSystem(body string, hostname string) string {
	return strings.Replace(body, `"traffic":[]`, `"system":{"hostname":"`+hostname+`","os":"linux","arch":"amd64"},"traffic":[]`, 1)
}

func reportAt(t *testing.T, f *agentFixture, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	return agentRequest(t, f, "/api/agent/v1/report", token, body)
}

func TestReportCoalescingFastPathAdvancesLatestAndPublicLiveness(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, token, _ := enrollHTTP(t, f, "coalesce", 1)
	if _, err := f.nodes.PatchPublicSettings(context.Background(), id, nodes.PublicSettingsPatch{
		Enabled: boolPtr(true), Name: stringPtr("Coalesce"),
	}); err != nil {
		t.Fatal(err)
	}
	accepted(t, reportAt(t, f, token, reportBody(1, 1, "coalesce-session", 10)), 1)
	persisted := lastSeen(t, f, id)
	first, err := f.ingest.Latest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReceivedAt != f.now {
		t.Fatalf("first ReceivedAt=%s want %s", first.ReceivedAt, f.now)
	}

	f.now = f.now.Add(time.Second)
	accepted(t, reportAt(t, f, token, reportBody(1, 2, "coalesce-session", 90)), 2)
	if got := lastSeen(t, f, id); got != persisted {
		t.Fatalf("fast path refreshed last_seen: got %v want %v", got, persisted)
	}
	latest, err := f.ingest.Latest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Seq != 2 || latest.Metrics.CPUPercent == nil || *latest.Metrics.CPUPercent != 90 {
		t.Fatalf("Latest did not advance to fast-path report: seq=%d cpu=%v", latest.Seq, latest.Metrics.CPUPercent)
	}
	if !latest.ReceivedAt.Equal(f.now) {
		t.Fatalf("fast-path ReceivedAt=%s want %s", latest.ReceivedAt, f.now)
	}

	// 即使持久化 last_seen 比公开离线阈值更旧，
	// 公开活跃性也必须跟随内存接收时间。
	f.now = f.now.Add(180 * time.Second)
	public, err := publicNode(context.Background(), f.nodes, f.ingest, id, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if public.Status != "online" {
		t.Fatalf("public status=%q after fast-path receive; want online", public.Status)
	}
}

func TestReportCoalescingFastPathDoesNotWaitForSQLiteWriter(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, token, _ := enrollHTTP(t, f, "coalesce-lock", 2)
	accepted(t, reportAt(t, f, token, reportBody(1, 1, "lock-session", 10)), 1)

	writer, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(context.Background(), "UPDATE nodes SET last_seen=last_seen WHERE id=?", id); err != nil {
		_ = writer.Rollback()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Rollback() })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, publicURL+"/api/agent/v1/report", strings.NewReader(reportBody(1, 2, "lock-session", 11)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		f.handler.ServeHTTP(response, req)
		close(done)
	}()
	select {
	case <-done:
		if response.Code != http.StatusOK {
			t.Fatalf("fast-path report status=%d body=%s", response.Code, response.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("fast-path report waited on the SQLite writer transaction")
	}
}

func TestReportCoalescingDurableChangesAndIntervalPersist(t *testing.T) {
	f := newAgentHTTP(t, 1)
	id, token, _ := enrollHTTP(t, f, "coalesce-durable", 3)
	accepted(t, reportAt(t, f, token, reportWithSystem(reportBody(1, 1, "durable-session", 10), "before")), 1)
	firstSeen := lastSeen(t, f, id)

	f.now = f.now.Add(time.Second)
	accepted(t, reportAt(t, f, token, reportWithSystem(reportBody(1, 2, "durable-session", 11), "after")), 2)
	secondSeen := lastSeen(t, f, id)
	if secondSeen == firstSeen {
		t.Fatal("system change did not force durable persistence")
	}

	f.now = f.now.Add(31 * time.Second)
	accepted(t, reportAt(t, f, token, reportBody(1, 3, "durable-session", 12)), 3)
	thirdSeen := lastSeen(t, f, id)
	if thirdSeen == secondSeen {
		t.Fatal("persistence interval did not force durable last_seen update")
	}
}

func TestReportAdmissionInvalidatedAfterCredentialMutation(t *testing.T) {
	t.Run("revoke", func(t *testing.T) {
		f := newAgentHTTP(t, 1)
		id, token, _ := enrollHTTP(t, f, "admit-revoke", 4)
		accepted(t, reportAt(t, f, token, reportBody(1, 1, "admit-revoke", 10)), 1)
		admission, err := f.ingest.AdmitToken(context.Background(), token)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.nodes.RevokeCredential(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if _, err := f.ingest.AcceptAdmitted(context.Background(), admission, mustReport(t, reportBody(1, 2, "admit-revoke", 10))); !errors.Is(err, nodes.ErrUnauthenticated) {
			t.Fatalf("admitted report after revoke: err=%v", err)
		}
		assertHTTPStatus(t, reportAt(t, f, token, reportBody(1, 1, "admit-revoke", 10)), http.StatusUnauthorized)
	})

	t.Run("disable", func(t *testing.T) {
		f := newAgentHTTP(t, 1)
		id, token, _ := enrollHTTP(t, f, "admit-disable", 5)
		accepted(t, reportAt(t, f, token, reportBody(1, 1, "admit-disable", 10)), 1)
		admission, err := f.ingest.AdmitToken(context.Background(), token)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.nodes.SetDisabled(context.Background(), id, true); err != nil {
			t.Fatal(err)
		}
		if _, err := f.ingest.AcceptAdmitted(context.Background(), admission, mustReport(t, reportBody(1, 2, "admit-disable", 10))); !errors.Is(err, nodes.ErrDisabled) {
			t.Fatalf("admitted report after disable: err=%v", err)
		}
		assertHTTPStatus(t, reportAt(t, f, token, reportBody(1, 1, "admit-disable", 10)), http.StatusForbidden)
	})

	t.Run("rotation", func(t *testing.T) {
		f := newAgentHTTP(t, 1)
		id, oldToken, _ := enrollHTTP(t, f, "admit-rotation", 6)
		accepted(t, reportAt(t, f, oldToken, reportBody(1, 1, "admit-rotation", 10)), 1)
		admission, err := f.ingest.AdmitToken(context.Background(), oldToken)
		if err != nil {
			t.Fatal(err)
		}
		ticket, err := f.enrollments.CreateSingle(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		newToken := agentSecret(7)
		rotationBody := strings.Replace(enrollBody("installation-admit-rotation", newToken), `"node_token":`, `"previous_node_token":"`+oldToken+`","node_token":`, 1)
		response := agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, rotationBody)
		assertHTTPStatus(t, response, http.StatusOK)
		if _, err := f.ingest.AcceptAdmitted(context.Background(), admission, mustReport(t, reportBody(1, 2, "admit-rotation", 10))); !errors.Is(err, nodes.ErrUnauthenticated) {
			t.Fatalf("admitted report after rotation: err=%v", err)
		}
		assertHTTPStatus(t, reportAt(t, f, oldToken, reportBody(1, 1, "admit-rotation", 10)), http.StatusUnauthorized)
		accepted(t, reportAt(t, f, newToken, reportBody(1, 1, "admit-rotation-new", 10)), 1)
	})
}

func boolPtr(value bool) *bool { return &value }

func stringPtr(value string) *string { return &value }
