package server_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/nodes"
)

func TestInternalFailureLogsCorrelatedCodeWithoutPrivateValues(t *testing.T) {
	f := newHTTP(t, 5)
	var logs bytes.Buffer
	var err error
	f.handler, err = server.NewHandler(testHandlerOptions(t, server.Options{
		Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	}))
	if err != nil {
		t.Fatal(err)
	}
	admin, csrf := login(t, f, "admin")
	const privateFailure = "private-password enrollment-secret csrf-private\nforged-log-line"
	if _, err := f.db.Exec("CREATE TRIGGER reject_private_insert BEFORE INSERT ON nodes BEGIN SELECT RAISE(ABORT, '" + privateFailure + "'); END"); err != nil {
		t.Fatal(err)
	}
	response := request(t, f.handler, http.MethodPost, "/api/v1/nodes?ticket=private-query", `{"name":"private-node-name"}`, admin, csrf, publicURL)
	apiError(t, response, http.StatusInternalServerError, "internal_error")
	var entry struct {
		RequestID  string `json:"request_id"`
		ErrorKind  string `json:"error_kind"`
		SQLiteCode int    `json:"sqlite_code"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("missing structured internal error: %v", err)
	}
	if entry.RequestID != response.Header().Get("X-Request-ID") || entry.ErrorKind != "sqlite" || entry.SQLiteCode != 1811 { // SQLITE_CONSTRAINT_TRIGGER
		t.Fatalf("uncorrelated or undiagnosable error: %+v", entry)
	}
	for _, private := range []string{privateFailure, "enrollment-secret", "forged-log-line", "private-query", "private-node-name", admin.Value, csrf, password} {
		if strings.Contains(logs.String(), private) || strings.Contains(response.Body.String(), private) {
			t.Fatalf("private value escaped into diagnostics: %q", private)
		}
	}
	logs.Reset()
	missing := request(t, f.handler, http.MethodGet, "/api/v1/nodes/00000000-0000-4000-8000-000000000001", "", admin, "", "")
	apiError(t, missing, http.StatusNotFound, "node_not_found")
	if logs.Len() != 0 {
		t.Fatal("expected domain failure was logged as an internal failure")
	}
}

type privateFailureReader struct{ failure error }

func (r privateFailureReader) Read([]byte) (int, error) { return 0, r.failure }

func TestNonSQLiteInternalFailureDoesNotLogErrorText(t *testing.T) {
	f := newHTTP(t, 5)
	var logs bytes.Buffer
	const privateFailure = "/private/credential-file: token=private-entropy-secret"
	failingNodes := nodes.NewService(f.db, nodes.Options{MaxNodes: 5, Random: privateFailureReader{errors.New(privateFailure)}})
	handler, err := server.NewHandler(testHandlerOptions(t, server.Options{
		Auth: f.auth, Nodes: failingNodes, PublicURL: publicURL,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	}))
	if err != nil {
		t.Fatal(err)
	}
	f.handler = handler
	admin, csrf := login(t, f, "admin")
	response := request(t, f.handler, http.MethodPost, "/api/v1/nodes", `{"name":"entropy-failure"}`, admin, csrf, publicURL)
	apiError(t, response, http.StatusInternalServerError, "internal_error")
	var entry struct {
		RequestID string `json:"request_id"`
		ErrorKind string `json:"error_kind"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.RequestID != response.Header().Get("X-Request-ID") || entry.ErrorKind == "" || entry.ErrorKind == "sqlite" {
		t.Fatalf("non-SQLite failure lost its diagnostic class: %+v", entry)
	}
	if strings.Contains(logs.String(), privateFailure) || strings.Contains(logs.String(), "private-entropy-secret") || strings.Contains(response.Body.String(), privateFailure) {
		t.Fatal("non-SQLite error text leaked")
	}
}
