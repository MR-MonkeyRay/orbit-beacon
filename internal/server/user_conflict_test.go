package server_test

import (
	"net/http"
	"testing"
)

func TestDuplicateUsernamesReturnConflictWithoutPartialChanges(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	duplicate := request(t, f.handler, http.MethodPost, "/api/v1/users", `{"username":"admin","password":"`+password+`","role":"viewer"}`, admin, csrf, publicURL)
	apiError(t, duplicate, http.StatusConflict, "username_conflict")
	created := request(t, f.handler, http.MethodPost, "/api/v1/users", `{"username":"managed-conflict","password":"`+password+`","role":"viewer"}`, admin, csrf, publicURL)
	assertHTTPStatus(t, created, http.StatusCreated)
	id := objectID(t, decodeObject(t, created))
	managed, _ := login(t, f, "managed-conflict")
	path := "/api/v1/users/" + id
	conflict := request(t, f.handler, http.MethodPatch, path, `{"username":"admin","role":"admin"}`, admin, csrf, publicURL)
	apiError(t, conflict, http.StatusConflict, "username_conflict")
	read := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
	assertHTTPStatus(t, read, http.StatusOK)
	user := decodeObject(t, read)
	if user["username"] != "managed-conflict" || user["role"] != "viewer" {
		t.Fatalf("conflict partially updated user: %+v", user)
	}
	assertHTTPStatus(t, request(t, f.handler, http.MethodGet, "/api/v1/me", "", managed, "", ""), http.StatusOK)
	var patches int
	if err := f.db.QueryRow("SELECT count(*) FROM audit_events WHERE action='user.patch' AND target_id=?", id).Scan(&patches); err != nil || patches != 0 {
		t.Fatalf("conflict emitted success audit: patches=%d err=%v", patches, err)
	}
}
