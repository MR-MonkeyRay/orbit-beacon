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

func TestInvalidReportRevisionAndUnsupportedProbesDoNotRefreshState(t *testing.T) {
	valid := reportBody(9, 1, "invalid-owner", 25)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"zero revision", strings.Replace(valid, `"config_revision":1`, `"config_revision":0`, 1)},
		{"missing revision", strings.Replace(valid, `"config_revision":1,`, "", 1)},
		{"unimplemented probes", strings.Replace(valid, `"traffic":[]`, `"traffic":[],"probe_results":[{"arbitrary":"ignored-before-fix"}]`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentHTTP(t, 1)
			id, token, _ := enrollHTTP(t, f, "contract", 1)
			apiError(t, agentRequest(t, f, "/api/agent/v1/report", token, tc.body), http.StatusBadRequest, "validation_failed")
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
