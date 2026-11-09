package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orbitbeacon/internal/server/notify"
	"orbitbeacon/internal/server/storage"
)

func TestStorageRecoveryRoutesKeepSessionCSRFAndAPITokenBoundaries(t *testing.T) {
	f := newConsoleLiveFixture(t)
	session, err := f.h.auth.CurrentSession(context.Background(), f.cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string, authenticated bool, csrf, origin, bearer string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "https://beacon.test"+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if authenticated {
			request.AddCookie(f.cookie)
		}
		if csrf != "" {
			request.Header.Set("X-CSRF-Token", csrf)
		}
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		response := httptest.NewRecorder()
		f.h.ServeHTTP(response, request)
		return response
	}
	check := func(response *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if response.Code != status {
			t.Errorf("status=%d want=%d body=%s", response.Code, status, response.Body.String())
			return
		}
		if code != "" {
			var failure apiError
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Code != code {
				t.Errorf("code=%q want=%q decode=%v", failure.Code, code, err)
			}
		}
	}
	check(call(http.MethodGet, "/api/v1/storage", "", false, "", "", ""), http.StatusUnauthorized, "unauthenticated")
	response := call(http.MethodGet, "/api/v1/storage", "", true, "", "", "")
	check(response, http.StatusOK, "")
	if response.Code == http.StatusOK {
		var status struct {
			Enabled bool              `json:"enabled"`
			Domains []json.RawMessage `json:"domains"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || status.Enabled || len(status.Domains) != 2 {
			t.Errorf("standalone status=%+v decode=%v", status, err)
		}
	}
	preview := `{"domain":"main","action":"promote","target":"standby"}`
	check(call(http.MethodPost, "/api/v1/storage/operations/preview", preview, true, "", "https://beacon.test", ""), http.StatusForbidden, "forbidden")
	check(call(http.MethodPost, "/api/v1/storage/operations/preview", preview, true, session.CSRFToken, "https://untrusted.test", ""), http.StatusForbidden, "forbidden")
	method := call(http.MethodPut, "/api/v1/storage/operations/11111111-1111-4111-8111-111111111111", "", false, "", "", "")
	check(method, http.StatusMethodNotAllowed, "method_not_allowed")
	allowed := method.Header().Get("Allow")
	if !strings.Contains(allowed, http.MethodGet) || !strings.Contains(allowed, http.MethodPost) {
		t.Errorf("operation methods not advertised: %q", allowed)
	}
	check(call(http.MethodGet, "/api/v1/storage/operations/not-an-id", "", true, "", "", ""), http.StatusNotFound, "not_found")
	for _, body := range []string{`{"confirmation":"x"}`, `{"confirmation":"x","accept_data_loss":null}`, `{"confirmation":null,"accept_data_loss":false}`} {
		check(call(http.MethodPost, "/api/v1/storage/operations/11111111-1111-4111-8111-111111111111", body, true, session.CSRFToken, "https://beacon.test", ""), http.StatusBadRequest, "validation_failed")
	}
	for _, body := range []string{`{"domain":"other","action":"rebuild","target":"standby"}`, `{"domain":"main","action":null,"target":"standby"}`, `{"domain":"main","action":"rebuild","target":"active"}`} {
		check(call(http.MethodPost, "/api/v1/storage/operations/preview", body, true, session.CSRFToken, "https://beacon.test", ""), http.StatusBadRequest, "validation_failed")
	}
	created := call(http.MethodPost, "/api/v1/api-tokens", `{"name":"storage-boundary","expires_in_days":30,"password":"private-live-password"}`, true, session.CSRFToken, "https://beacon.test", "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create scoped token=%d %s", created.Code, created.Body.String())
	}
	var token struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	check(call(http.MethodGet, "/api/v1/storage", "", true, "", "", token.Token), http.StatusForbidden, "")
}

type storageRevokingBody struct {
	reader *strings.Reader
	revoke func()
}

func (body *storageRevokingBody) Read(p []byte) (int, error) {
	if body.revoke != nil {
		revoke := body.revoke
		body.revoke = nil
		revoke()
	}
	return body.reader.Read(p)
}

func TestStorageAcceptRechecksRevokedSessionAfterReadingBody(t *testing.T) {
	f := newConsoleLiveFixture(t)
	session, err := f.h.auth.CurrentSession(context.Background(), f.cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	body := &storageRevokingBody{reader: strings.NewReader(`{"confirmation":"main/standby/11111111-1111-4111-8111-111111111111","accept_data_loss":true}`), revoke: func() {
		if err := f.h.auth.Logout(context.Background(), f.cookie.Value); err != nil {
			t.Fatal(err)
		}
	}}
	request := httptest.NewRequest(http.MethodPost, "https://beacon.test/api/v1/storage/operations/11111111-1111-4111-8111-111111111111", body)
	request.AddCookie(f.cookie)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://beacon.test")
	request.Header.Set("X-CSRF-Token", session.CSRFToken)
	response := httptest.NewRecorder()
	f.h.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session reached recovery acceptance: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestStorageStatusKeepsMalformedPreferencesRepairable(t *testing.T) {
	f := newConsoleLiveFixture(t)
	f.h.notifications = notify.New(f.h.nodes.Database(), nil, notify.Options{})
	if _, err := f.db.Exec(`INSERT INTO settings(key,value) VALUES('storage_notifications','{"inbox":null,"channel_ids":[]}')`); err != nil {
		t.Fatal(err)
	}
	session, err := f.h.auth.CurrentSession(context.Background(), f.cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "https://beacon.test"+path, strings.NewReader(body))
		request.AddCookie(f.cookie)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "https://beacon.test")
		request.Header.Set("X-CSRF-Token", session.CSRFToken)
		response := httptest.NewRecorder()
		f.h.ServeHTTP(response, request)
		return response
	}
	response := call(http.MethodGet, "/api/v1/storage", "")
	if response.Code != http.StatusOK {
		t.Fatalf("malformed preferences hid storage repair: status=%d body=%s", response.Code, response.Body.String())
	}
	var status storage.RecoveryStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Domains) != 2 || status.NotificationSettings.SnapshotReady {
		t.Fatal("unavailable preferences were presented as synchronized")
	}
	response = call(http.MethodPatch, "/api/v1/storage/notifications", `{"inbox":false,"channel_ids":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("repair write failed: status=%d body=%s", response.Code, response.Body.String())
	}
	response = call(http.MethodGet, "/api/v1/storage", "")
	if response.Code != http.StatusOK {
		t.Fatal("repaired storage status unavailable", response.Code)
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.NotificationSettings.Inbox || status.NotificationSettings.ChannelIDs == nil {
		t.Fatal("repair did not persist explicit valid preferences")
	}
}
