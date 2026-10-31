package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/backup"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/traffic"
	"orbitbeacon/internal/server/validation"
)

func TestBackupErrorPreservesSQLiteDiagnostics(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE private_values (value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	_, failure := db.Exec("INSERT INTO private_values VALUES (NULL)")
	code, ok := storage.SQLiteCode(failure)
	if !ok {
		t.Fatalf("expected SQLite failure, got %v", failure)
	}
	var logs bytes.Buffer
	h := &handler{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	w := httptest.NewRecorder()
	h.backupError(w, fmt.Errorf("private-path secret: %w", failure), "backup-request")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", w.Code)
	}
	var entry struct {
		Kind      string `json:"error_kind"`
		Code      int    `json:"sqlite_code"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Kind != "sqlite" || entry.Code != code || entry.RequestID != "backup-request" {
		t.Fatalf("lost diagnostics: %+v", entry)
	}
	if strings.Contains(logs.String(), "private-path") || strings.Contains(logs.String(), "private_values") {
		t.Fatalf("private error leaked: %s", &logs)
	}
}

func TestBackupErrorRetriesSQLiteContention(t *testing.T) {
	db, err := sql.Open("sqlite", "file:backup_error_busy?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "CREATE TABLE busy_values (value TEXT); INSERT INTO busy_values VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.QueryContext(context.Background(), "SELECT * FROM busy_values")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no active row")
	}
	_, failure := conn.ExecContext(context.Background(), "DROP TABLE busy_values")
	if !storage.IsBusy(failure) {
		t.Fatalf("expected SQLite contention, got %v", failure)
	}
	var logs bytes.Buffer
	h := &handler{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	w := httptest.NewRecorder()
	h.backupError(w, fmt.Errorf("backup: %w", failure), "busy-request")
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("status=%d retry=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body)
	}
	if !strings.Contains(w.Body.String(), `"code":"storage_busy"`) || logs.Len() != 0 {
		t.Fatalf("unexpected response/log: %s %s", w.Body, &logs)
	}
}

func TestServiceErrorRejectsInvalidTrafficInput(t *testing.T) {
	var logs bytes.Buffer
	h := &handler{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	w := httptest.NewRecorder()
	h.serviceError(w, fmt.Errorf("traffic: %w", traffic.ErrInvalidInput), "traffic-request")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"validation_failed"`) || !strings.Contains(w.Body.String(), `"message":"Request values are invalid"`) {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if logs.Len() != 0 {
		t.Fatalf("validation logged as internal: %s", &logs)
	}
}

func TestLoginAlwaysUsesSPAShell(t *testing.T) {
	assets := fstest.MapFS{
		"200.html":   {Data: []byte("<script>consoleShell()</script>")},
		"login.html": {Data: []byte("<script>obsoleteLogin()</script>")},
	}
	prepared, err := prepareStaticAssets(assets)
	if err != nil {
		t.Fatal(err)
	}
	policies, err := htmlPolicies(assets, "https://example.test")
	if err != nil {
		t.Fatal(err)
	}
	h := &handler{staticAssets: prepared, htmlPolicies: policies}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		h.static(w, httptest.NewRequest(method, "/login", nil))
		if w.Code != http.StatusOK || w.Header().Get("ETag") != prepared["200.html"].etag || w.Header().Get("Content-Security-Policy") != policies["200.html"] {
			t.Fatalf("login did not use SPA shell: status %d headers %v", w.Code, w.Header())
		}
		if method == http.MethodGet && w.Body.String() != string(assets["200.html"].Data) {
			t.Fatalf("unexpected login shell: %s", w.Body)
		}
	}
}

func TestErrorMappersPreserveContext(t *testing.T) {
	var logs bytes.Buffer
	h := &handler{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	for _, test := range []struct {
		name                 string
		mapper               func(http.ResponseWriter, error, string)
		err                  error
		status               int
		code, message, retry string
	}{
		{"agent rate", h.agentError, errors.Join(ingest.ErrRateLimited, auth.ErrRateLimited), 429, "rate_limited", "Report rate limit exceeded", "1"},
		{"service rate", h.serviceError, errors.Join(ingest.ErrRateLimited, auth.ErrRateLimited), 429, "rate_limited", "Request rate limit reached; retry later", "1"},
		{"agent missing", h.agentError, errors.Join(enrollment.ErrNotFound, nodes.ErrNotFound), 404, "enrollment_not_found", "Enrollment not found", ""},
		{"service missing", h.serviceError, errors.Join(enrollment.ErrNotFound, nodes.ErrNotFound), 404, "node_not_found", "Node not found", ""},
		{"backup secret", h.backupError, &backup.Error{Code: "secret_unreadable"}, 409, "secret_unreadable", "The backup secret cannot be read with the server key", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			test.mapper(w, fmt.Errorf("wrapped: %w", test.err), "request")
			var response apiError
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if w.Code != test.status || response.Code != test.code || response.Message != test.message || response.RequestID != "request" || w.Header().Get("Retry-After") != test.retry {
				t.Fatalf("status %d response %+v headers %v", w.Code, response, w.Header())
			}
		})
	}
	if logs.Len() != 0 {
		t.Fatalf("expected no internal logs: %s", &logs)
	}
}

func TestDecodeErrorPreservesFieldDetailsAndLimits(t *testing.T) {
	for _, test := range []struct {
		name          string
		err           error
		status        int
		code, message string
		fields        bool
	}{
		{"fields precede body size", errors.Join(validation.Invalid("cron", "invalid schedule"), &http.MaxBytesError{Limit: 64 * 1024}), 400, "validation_failed", "Request values are invalid", true},
		{"body size", fmt.Errorf("body: %w", &http.MaxBytesError{Limit: 64 * 1024}), 413, "body_too_large", "Request body exceeds 64 KiB", false},
		{"decode context", errors.New("private decoder error"), 400, "invalid_json", "Request must contain exactly one JSON value", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			decodeError(w, test.err, "Request must contain exactly one JSON value", "request")
			var response apiError
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if w.Code != test.status || response.Code != test.code || response.Message != test.message || response.RequestID != "request" {
				t.Fatalf("status %d response %+v", w.Code, response)
			}
			if test.fields && response.Fields["cron"] != "invalid schedule" || !test.fields && len(response.Fields) != 0 {
				t.Fatalf("fields %+v", response.Fields)
			}
		})
	}
}
