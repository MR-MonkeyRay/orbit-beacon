package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestOversizedStorageTransactionIsConflictWithoutAutomaticRetry(t *testing.T) {
	h := &handler{logger: slog.New(slog.DiscardHandler)}
	for _, mapper := range []struct {
		name  string
		write func(http.ResponseWriter, error, string)
	}{{"service", h.serviceError}, {"agent", h.agentError}, {"backup", h.backupError}} {
		t.Run(mapper.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			mapper.write(response, fmt.Errorf("privateSQL secret: %w", storage.ErrTransactionTooLarge), "oversized-transaction")
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"storage_transaction_too_large"`) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
			if response.Header().Get("Retry-After") != "" || strings.Contains(response.Body.String(), "privateSQL") || strings.Contains(response.Body.String(), "secret") {
				t.Fatalf("unsafe oversized transaction response: headers=%v body=%s", response.Header(), response.Body)
			}
		})
	}
	response := httptest.NewRecorder()
	h.serviceError(response, errors.Join(storage.ErrCommitUnknown, storage.ErrTransactionTooLarge), "unknown-first")
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"storage_commit_unknown"`) || response.Header().Get("Retry-After") != "" {
		t.Fatalf("commit uncertainty lost precedence: %d %v %s", response.Code, response.Header(), response.Body)
	}
}
