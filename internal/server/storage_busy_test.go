package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"orbitbeacon/internal/server/nodes"
)

func TestWriterContentionReturnsRetryable503WithoutPartialWrite(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	existing, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "batch-existing"})
	if err != nil {
		t.Fatal(err)
	}
	f.db.SetMaxOpenConns(2)
	writer, err := f.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer writer.ExecContext(context.Background(), "ROLLBACK")
	// 只保留另一个池连接可用，并让夹具短暂等待。
	waiter, err := f.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waiter.ExecContext(context.Background(), "PRAGMA busy_timeout=1"); err != nil {
		t.Fatal(err)
	}
	if err := waiter.Close(); err != nil {
		t.Fatal(err)
	}
	response := request(t, f.handler, http.MethodPost, "/api/v1/nodes", `{"name":"retryable-node"}`, admin, csrf, publicURL)
	apiError(t, response, http.StatusServiceUnavailable, "storage_busy")
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("retryable storage rejection omitted Retry-After")
	}
	batch := request(t, f.handler, http.MethodPost, "/api/v1/nodes/batch", `{"node_ids":["`+existing.ID+`"],"action":"disable"}`, admin, csrf, publicURL)
	assertHTTPStatus(t, batch, http.StatusOK)
	var results struct {
		Results []nodes.BatchResult `json:"results"`
	}
	if err := json.Unmarshal(batch.Body.Bytes(), &results); err != nil || len(results.Results) != 1 || results.Results[0].Status != "failed" || results.Results[0].ErrorCode != "storage_busy" {
		t.Fatalf("batch hid retryable failure: %+v err=%v", results, err)
	}
	if _, err := writer.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	created := request(t, f.handler, http.MethodPost, "/api/v1/nodes", `{"name":"retryable-node"}`, admin, csrf, publicURL)
	assertHTTPStatus(t, created, http.StatusCreated)
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM nodes WHERE name='retryable-node'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("busy transaction left a partial node: count=%d err=%v", count, err)
	}
}
