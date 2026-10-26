package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

func TestRejectedNegativeConfigRevisionKeepsReportingAndRemoteRepairAlive(t *testing.T) {
	ctx := context.Background()
	f := newAgentHTTP(t, 1)
	state, cfg, _ := clientState(t)
	node, err := f.nodes.CreatePending(ctx, nodes.CreateRequest{Name: "invalid remote revision"})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := f.enrollments.CreateSingle(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	var inject sync.Once
	https := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/v1/report" {
			f.handler.ServeHTTP(w, r)
			return
		}
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, r)
		for key, values := range response.Header() {
			w.Header()[key] = values
		}
		if response.Code != http.StatusOK {
			w.WriteHeader(response.Code)
			_, _ = w.Write(response.Body.Bytes())
			return
		}
		var result v1.ReportResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		inject.Do(func() {
			invalid := cfg.Current()
			invalid.Revision = -1
			result.DesiredConfig = &invalid
		})
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer https.Close()
	client, err := service.NewClient(service.ClientOptions{ServerURL: https.URL, HTTPClient: https.Client(), Identity: state, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Enroll(ctx, ticket.Token); err != nil {
		t.Fatal(err)
	}
	run, err := state.BeginRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send := func(seq uint64) error {
		report := mustReport(t, reportBody(run.RunGeneration, seq, run.SessionID, 25))
		report.ConfigRevision = 0 // Let Client select the currently applied revision.
		_, err := client.Report(ctx, report)
		return err
	}
	if err := send(1); err != nil {
		t.Fatal(err)
	}
	if cfg.Current().Revision != 1 || cfg.ConfigError() == nil {
		t.Fatal("invalid remote configuration replaced or falsely acknowledged the valid revision")
	}
	interval := 5
	if _, err := f.nodes.PatchConfig(ctx, node.ID, nodes.ConfigPatch{ReportIntervalSeconds: &interval}); err != nil {
		t.Fatal(err)
	}
	if err := send(2); err != nil {
		t.Fatalf("rejected revision poisoned its own report and blocked remote repair: %v", err)
	}
	var encoded string
	var applied int64
	if err := f.db.QueryRow("SELECT config_error, applied_revision FROM nodes WHERE id=?", node.ID).Scan(&encoded, &applied); err != nil {
		t.Fatal(err)
	}
	var failure v1.ConfigError
	if err := json.Unmarshal([]byte(encoded), &failure); err != nil || failure.Revision != 0 || failure.Fields["revision"] == "" || applied != 1 {
		t.Fatalf("rejection was not legal, attributable, or acknowledgement-safe: %+v applied=%d err=%v", failure, applied, err)
	}
	if cfg.Current().Revision != 2 || cfg.ConfigError() != nil {
		t.Fatal("report response could not durably apply the later legal repair")
	}
	if err := send(3); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT config_error,applied_revision FROM nodes WHERE id=?", node.ID).Scan(&encoded, &applied); err != nil || encoded != "" || applied != 2 {
		t.Fatalf("repair did not clear rejection and acknowledge revision 2: error=%q applied=%d err=%v", encoded, applied, err)
	}
}
