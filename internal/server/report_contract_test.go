package server_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
)

func TestInvalidMetricsAndProbeResultsDoNotRefreshState(t *testing.T) {
	valid := reportBody(9, 1, "invalid-owner", 25)
	for _, tc := range []struct {
		name     string
		body     string
		wantCode string
	}{
		{"zero generation", strings.Replace(valid, `"run_generation":9`, `"run_generation":0`, 1), "validation_failed"},
		{"malformed metrics", strings.Replace(valid, `"metrics":{`, `"metrics":"invalid","unused":{`, 1), "invalid_json"},
		{"invalid metric status", strings.Replace(valid, `"cpu_percent":"ok"`, `"cpu_percent":"unknown"`, 1), "validation_failed"},
		{"invalid probe result", strings.Replace(valid, `"traffic":[]`, `"traffic":[],"probe_results":[{"target":"tcp://host:7","ok":false,"latency_ms":null,"error":"","status_code":null,"checked_at":"2026-03-04T05:06:07Z"}]`, 1), "validation_failed"},
		{"invalid pending probe", strings.Replace(valid, `"traffic":[]`, `"traffic":[],"probe_results":[{"target":"tcp://host:7","ok":true,"error":"pending"}]`, 1), "validation_failed"},
		{"duplicate probe target", strings.Replace(valid, `"traffic":[]`, `"traffic":[],"probe_results":[{"target":"tcp://host:7","error":"pending"},{"target":"tcp://host:7","error":"pending"}]`, 1), "validation_failed"},
		{"malformed probe results", strings.Replace(valid, `"traffic":[]`, `"traffic":[],"probe_results":"invalid"`, 1), "invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentHTTP(t, 1)
			id, token, _ := enrollHTTP(t, f, "contract", 1)
			apiError(t, agentRequest(t, f, "/api/agent/v1/report", token, tc.body), http.StatusBadRequest, tc.wantCode)
			var generation, session string
			var seen sql.NullString
			if err := f.db.QueryRow("SELECT run_generation,report_session_id,last_seen FROM nodes WHERE id=?", id).Scan(&generation, &session, &seen); err != nil {
				t.Fatal(err)
			}
			if generation != "0" || session != "" || seen.Valid {
				t.Fatalf("invalid report changed state: generation=%s session=%s seen=%v", generation, session, seen)
			}
			if _, err := f.ingest.Latest(context.Background(), id); !errors.Is(err, nodes.ErrNotFound) {
				t.Fatalf("invalid report published latest snapshot: %v", err)
			}
			f.now = f.now.Add(time.Second)
			accepted(t, agentRequest(t, f, "/api/agent/v1/report", token, reportBody(1, 1, "valid-owner", 25)), 1)
		})
	}
}
