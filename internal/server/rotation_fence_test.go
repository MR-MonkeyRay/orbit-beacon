package server_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/ingest"
)

func TestCredentialRotationReleasesPoisonedFenceWithoutResettingExactRetries(t *testing.T) {
	for _, generation := range []uint64{1, math.MaxUint64} {
		t.Run(reportGenerationName(generation), func(t *testing.T) {
			f := newAgentHTTP(t, 1)
			id, oldToken, _ := enrollHTTP(t, f, "rotating", 1)
			accepted(t, agentRequest(t, f, "/api/agent/v1/report", oldToken, reportBody(generation, 100, "old-session", 99)), 100)
			ticket, err := f.enrollments.CreateSingle(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			newToken := agentSecret(2)
			body, err := json.Marshal(v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: "installation-rotating", NodeToken: newToken, PreviousNodeToken: oldToken, System: v1.System{Hostname: "identical-host", OS: "linux", Arch: "amd64"}})
			if err != nil {
				t.Fatal(err)
			}
			claim := string(body)
			assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, claim), http.StatusOK)
			assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/report", oldToken, reportBody(generation, 101, "old-session", 99)), http.StatusUnauthorized)
			f.now = f.now.Add(time.Second)
			// 被污染的围栏和瞬时的序列所有权都必须限定在已撤销的凭据内，
			// 而不是持久的资产身份。
			accepted(t, agentRequest(t, f, "/api/agent/v1/report", newToken, reportBody(1, 1, "new-session", 25)), 1)
			latest, err := f.ingest.Latest(context.Background(), id)
			if err != nil || latest.SessionID != "new-session" || latest.Seq != 1 || *latest.Metrics.CPUPercent != 25 {
				t.Fatalf("rotation retained revoked snapshot: %+v, %v", latest, err)
			}
			assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/enroll", ticket.Token, claim), http.StatusOK)
			restarted := ingest.NewService(f.db, ingest.Options{Now: func() time.Time { return f.now }})
			if _, err := acceptReport(context.Background(), restarted, newToken, mustReport(t, reportBody(1, 2, "clone", 50))); err == nil {
				t.Fatal("exact enrollment retry reset established session fence")
			}
			if _, err := acceptReport(context.Background(), restarted, newToken, mustReport(t, reportBody(1, 2, "new-session", 30))); err != nil {
				t.Fatalf("rotated reporting failed after restart: %v", err)
			}
		})
	}
}

func reportGenerationName(generation uint64) string {
	if generation == math.MaxUint64 {
		return "poisoned durable generation"
	}
	return "same generation different credential"
}
