package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func TestUnknownCommitNeverInvitesAutomaticMutationRetry(t *testing.T) {
	for _, mapper := range []func(http.ResponseWriter, error, string){(&handler{}).serviceError, (&handler{}).agentError, (&handler{}).backupError} {
		w := httptest.NewRecorder()
		mapper(w, errors.Join(storage.ErrCommitUnknown, storage.ErrUnavailable), "commit-request")
		var body apiError
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if w.Code != 503 || body.Code != "storage_commit_unknown" || w.Header().Get("Retry-After") != "" {
			t.Fatalf("unsafe unknown commit retry hint: %d %+v %v", w.Code, body, w.Header())
		}
	}
}
