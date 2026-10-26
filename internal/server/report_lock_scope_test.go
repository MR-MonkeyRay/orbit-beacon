package server_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReportWriterDoesNotBlockOtherNodeLatestSnapshot(t *testing.T) {
	f := newAgentHTTP(t, 2)
	a, tokenA, _ := enrollHTTP(t, f, "writer-a", 1)
	b, tokenB, _ := enrollHTTP(t, f, "reader-b", 2)
	accepted(t, agentRequest(t, f, "/api/agent/v1/report", tokenB, reportBody(1, 1, "session-b", 20)), 1)

	writer, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(context.Background(), "UPDATE nodes SET last_seen=last_seen WHERE id=?", a); err != nil {
		_ = writer.Rollback()
		t.Fatal(err)
	}
	defer writer.Rollback()

	reportDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, publicURL+"/api/agent/v1/report", strings.NewReader(reportBody(1, 1, "session-a", 30)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tokenA)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, req)
		reportDone <- response
	}()
	time.Sleep(100 * time.Millisecond)

	latestDone := make(chan error, 1)
	go func() {
		_, err := f.ingest.Latest(context.Background(), b)
		latestDone <- err
	}()
	select {
	case err := <-latestDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("a blocked node report held the global ingest lock over another node's latest snapshot")
	}
	if err := writer.Rollback(); err != nil && err != sql.ErrTxDone {
		t.Fatal(err)
	}
	select {
	case response := <-reportDone:
		if response.Code != http.StatusOK {
			t.Fatalf("blocked report did not complete after writer release: status=%d body=%s", response.Code, response.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked report did not complete after writer release")
	}
}
