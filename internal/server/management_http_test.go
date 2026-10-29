package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
)

func decodeObject(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
		t.Fatalf("response is not a JSON object: %v", err)
	}
	if object == nil {
		t.Fatal("response is a null object")
	}
	assertNoCredentialDTO(t, object)
	return object
}

func assertNoCredentialDTO(t *testing.T, value any) {
	t.Helper()
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			switch strings.ToLower(key) {
			case "password", "old_password", "new_password", "password_hash", "token", "token_hash", "credential_epoch", "node_token", "enrollment_token", "csrf_hash", "secrets_key":
				t.Errorf("management DTO exposes credential member %q", key)
			}
			assertNoCredentialDTO(t, child)
		}
	case []any:
		for _, child := range value {
			assertNoCredentialDTO(t, child)
		}
	}
}

func objectID(t *testing.T, object map[string]any) string {
	t.Helper()
	id, ok := object["id"].(string)
	if !ok || !uuidPattern.MatchString(id) {
		t.Fatalf("management object lacks UUID id: %+v", object)
	}
	return id
}

func collectionContains(t *testing.T, response *httptest.ResponseRecorder, key, id string) {
	t.Helper()
	assertHTTPStatus(t, response, http.StatusOK)
	object := decodeObject(t, response)
	items, ok := object[key].([]any)
	if !ok {
		t.Fatalf("collection lacks %q array: %+v", key, object)
	}
	for _, item := range items {
		if object, ok := item.(map[string]any); ok && object["id"] == id {
			return
		}
	}
	t.Errorf("%s collection does not include %s", key, id)
}

func createHTTPGroup(t *testing.T, f fixture, cookie *http.Cookie, csrf, name string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, f.handler, http.MethodPost, "/api/v1/groups", string(body), cookie, csrf, publicURL)
	assertHTTPStatus(t, response, http.StatusCreated)
	return objectID(t, decodeObject(t, response))
}

func TestManagementUsesOnlyCookieSessions(t *testing.T) {
	f := newHTTP(t, 5)
	admin, adminCSRF := login(t, f, "admin")
	second, secondCSRF := login(t, f, "admin-two")
	serve := func(method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, publicURL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer obt_legacy-management-credential")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", publicURL)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, req)
		return response
	}
	apiError(t, serve(http.MethodGet, "/api/v1/nodes", "", nil, ""), http.StatusUnauthorized, "unauthenticated")
	assertHTTPStatus(t, serve(http.MethodGet, "/api/v1/nodes", "", second, ""), http.StatusOK)
	assertHTTPStatus(t, serve(http.MethodPost, "/api/v1/groups", `{"name":"second administrator group"}`, second, secondCSRF), http.StatusCreated)
	apiError(t, serve(http.MethodPost, "/api/v1/groups", `{"name":"missing csrf"}`, admin, ""), http.StatusForbidden, "forbidden")
	response := serve(http.MethodPost, "/api/v1/groups", `{"name":"session group"}`, admin, adminCSRF)
	assertHTTPStatus(t, response, http.StatusCreated)
	id := objectID(t, decodeObject(t, response))
	collectionContains(t, request(t, f.handler, http.MethodGet, "/api/v1/groups", "", second, "", ""), "groups", id)
}

func TestRemovedManagementTokenEndpointsReturnNotFound(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	for _, tt := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/api-tokens", ""},
		{http.MethodPost, "/api/v1/api-tokens", `{"name":"removed credential"}`},
		{http.MethodDelete, "/api/v1/api-tokens/00000000-0000-4000-8000-000000000001", ""},
		{http.MethodHead, "/api/v1/api-tokens", ""},
	} {
		t.Run(tt.method, func(t *testing.T) {
			response := request(t, f.handler, tt.method, tt.path, tt.body, admin, csrf, publicURL)
			assertHTTPStatus(t, response, http.StatusNotFound)
		})
	}
}

func TestGroupHTTPCRUDPersistsAndRemainsReadableToBothAdministrators(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	second, _ := login(t, f, "admin-two")
	id := createHTTPGroup(t, f, admin, csrf, "initial group")
	path := "/api/v1/groups/" + id
	collectionContains(t, request(t, f.handler, http.MethodGet, "/api/v1/groups", "", second, "", ""), "groups", id)
	response := request(t, f.handler, http.MethodGet, path, "", second, "", "")
	assertHTTPStatus(t, response, http.StatusOK)
	if got := decodeObject(t, response)["name"]; got != "initial group" {
		t.Errorf("group name = %v", got)
	}
	response = request(t, f.handler, http.MethodPatch, path, `{"name":"renamed group"}`, admin, csrf, publicURL)
	assertHTTPStatus(t, response, http.StatusOK)
	if got := decodeObject(t, response)["name"]; got != "renamed group" {
		t.Errorf("PATCH group name = %v", got)
	}
	var stored string
	if err := f.db.QueryRow("SELECT name FROM groups WHERE id = ?", id).Scan(&stored); err != nil || stored != "renamed group" {
		t.Errorf("group update not persisted: name=%q err=%v", stored, err)
	}
	response = request(t, f.handler, http.MethodDelete, path, "", admin, csrf, publicURL)
	assertHTTPStatus(t, response, http.StatusNoContent)
	assertHTTPStatus(t, request(t, f.handler, http.MethodGet, path, "", second, "", ""), http.StatusNotFound)
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM groups WHERE id = ?", id).Scan(&count); err != nil || count != 0 {
		t.Errorf("deleted group persisted: count=%d err=%v", count, err)
	}
}

func TestUserHTTPCollectionAndItemMethodsPersistWithoutCredentials(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	second, _ := login(t, f, "admin-two")
	response := request(t, f.handler, http.MethodPost, "/api/v1/users", `{"username":"managed","password":"`+password+`"}`, admin, csrf, publicURL)
	assertHTTPStatus(t, response, http.StatusCreated)
	id := objectID(t, decodeObject(t, response))
	path := "/api/v1/users/" + id
	collectionContains(t, request(t, f.handler, http.MethodGet, "/api/v1/users", "", second, "", ""), "users", id)
	response = request(t, f.handler, http.MethodGet, path, "", second, "", "")
	assertHTTPStatus(t, response, http.StatusOK)
	if got := decodeObject(t, response)["username"]; got != "managed" {
		t.Errorf("GET user username = %v", got)
	}
	response = request(t, f.handler, http.MethodPatch, path, `{"username":"renamed"}`, admin, csrf, publicURL)
	assertHTTPStatus(t, response, http.StatusOK)
	object := decodeObject(t, response)
	if object["username"] != "renamed" {
		t.Errorf("updated user DTO = %+v", object)
	}
	var username string
	if err := f.db.QueryRow("SELECT username FROM users WHERE id = ?", id).Scan(&username); err != nil || username != "renamed" {
		t.Errorf("user update not persisted: username=%q err=%v", username, err)
	}
	managed, _ := login(t, f, "renamed")
	response = request(t, f.handler, http.MethodDelete, path, "", admin, csrf, publicURL)
	assertHTTPStatus(t, response, http.StatusNoContent)
	assertHTTPStatus(t, request(t, f.handler, http.MethodGet, path, "", admin, "", ""), http.StatusNotFound)
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/me", "", managed, "", ""), http.StatusUnauthorized, "unauthenticated")
	var sessions int
	if err := f.db.QueryRow("SELECT count(*) FROM sessions WHERE user_id = ?", id).Scan(&sessions); err != nil || sessions != 0 {
		t.Errorf("user deletion left sessions: count=%d err=%v", sessions, err)
	}
}

func TestHTTPPasswordChangeRevokesEverySessionAndRequiresOldPassword(t *testing.T) {
	for _, username := range []string{"admin", "admin-two"} {
		t.Run(username, func(t *testing.T) {
			f := newHTTP(t, 2)
			first, csrf := login(t, f, username)
			second, _ := login(t, f, username)
			const path = "/api/v1/me/password"
			for _, tt := range []struct{ name, body, csrf, origin string }{
				{"wrong old password", `{"old_password":"wrong password","new_password":"new HTTP regression password"}`, csrf, publicURL},
				{"missing CSRF", `{"old_password":"` + password + `","new_password":"new HTTP regression password"}`, "", publicURL},
				{"foreign origin", `{"old_password":"` + password + `","new_password":"new HTTP regression password"}`, csrf, "https://attacker.example.test"},
			} {
				t.Run(tt.name, func(t *testing.T) {
					response := request(t, f.handler, http.MethodPatch, path, tt.body, first, tt.csrf, tt.origin)
					if response.Code != http.StatusBadRequest && response.Code != http.StatusUnauthorized && response.Code != http.StatusForbidden {
						t.Errorf("invalid password change returned %d, want authentication/validation rejection", response.Code)
					}
					for _, cookie := range []*http.Cookie{first, second} {
						assertHTTPStatus(t, request(t, f.handler, http.MethodGet, "/api/v1/me", "", cookie, "", ""), http.StatusOK)
					}
				})
			}
			response := request(t, f.handler, http.MethodPatch, path, `{"old_password":"`+password+`","new_password":"new HTTP regression password"}`, first, csrf, publicURL)
			assertHTTPStatus(t, response, http.StatusNoContent)
			for _, cookie := range []*http.Cookie{first, second} {
				apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/me", "", cookie, "", ""), http.StatusUnauthorized, "unauthenticated")
			}
			oldLogin := request(t, f.handler, http.MethodPost, "/api/v1/session", `{"username":"`+username+`","password":"`+password+`"}`, nil, "", publicURL)
			assertHTTPStatus(t, oldLogin, http.StatusUnauthorized)
			newLogin := request(t, f.handler, http.MethodPost, "/api/v1/session", `{"username":"`+username+`","password":"new HTTP regression password"}`, nil, "", publicURL)
			assertHTTPStatus(t, newLogin, http.StatusOK)
		})
	}
}

func TestNodeMetadataPatchPersistsGroupAndTagsWithoutExposingCredentials(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	second, _ := login(t, f, "admin-two")
	groupID := createHTTPGroup(t, f, admin, csrf, "metadata group")
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "original node"})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("private node credential"))
	if _, err := f.db.Exec("UPDATE nodes SET token_hash = ? WHERE id = ?", digest[:], node.ID); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/nodes/" + node.ID
	body := `{"name":"renamed node","notes":"rack 3","region":"eu-west","provider":"example cloud","group_id":"` + groupID + `","tags":["production","database"]}`
	response := request(t, f.handler, http.MethodPatch, path, body, admin, csrf, publicURL)
	assertHTTPStatus(t, response, http.StatusOK)
	assertNodeMetadata(t, decodeObject(t, response), groupID, []string{"database", "production"})
	response = request(t, f.handler, http.MethodGet, path, "", second, "", "")
	assertHTTPStatus(t, response, http.StatusOK)
	assertNodeMetadata(t, decodeObject(t, response), groupID, []string{"database", "production"})
	collectionContains(t, request(t, f.handler, http.MethodGet, "/api/v1/nodes", "", second, "", ""), "nodes", node.ID)
	var name, notes, region, provider, storedGroup string
	var storedToken []byte
	if err := f.db.QueryRow("SELECT name,notes,region,provider,group_id,token_hash FROM nodes WHERE id = ?", node.ID).Scan(&name, &notes, &region, &provider, &storedGroup, &storedToken); err != nil {
		t.Fatal(err)
	}
	if name != "renamed node" || notes != "rack 3" || region != "eu-west" || provider != "example cloud" || storedGroup != groupID || !reflect.DeepEqual(storedToken, digest[:]) {
		t.Errorf("metadata not persisted or credential changed: %q %q %q %q %q", name, notes, region, provider, storedGroup)
	}
	var tags int
	if err := f.db.QueryRow("SELECT count(*) FROM node_tags WHERE node_id = ? AND tag IN ('production','database')", node.ID).Scan(&tags); err != nil || tags != 2 {
		t.Errorf("node tags not persisted: count=%d err=%v", tags, err)
	}
	// Invalid references must not partially apply otherwise-valid metadata or tags.
	response = request(t, f.handler, http.MethodPatch, path, `{"notes":"must roll back","group_id":"00000000-0000-4000-8000-000000000999","tags":["wrong"]}`, admin, csrf, publicURL)
	if response.Code != http.StatusBadRequest && response.Code != http.StatusNotFound {
		t.Errorf("unknown group returned %d", response.Code)
	}
	response = request(t, f.handler, http.MethodGet, path, "", second, "", "")
	assertHTTPStatus(t, response, http.StatusOK)
	assertNodeMetadata(t, decodeObject(t, response), groupID, []string{"database", "production"})
	response = request(t, f.handler, http.MethodPatch, path, `{"group_id":null,"tags":[]}`, admin, csrf, publicURL)
	assertHTTPStatus(t, response, http.StatusOK)
	object := decodeObject(t, response)
	if object["group_id"] != nil {
		t.Errorf("explicit null did not clear group: %+v", object)
	}
	if tags, ok := object["tags"].([]any); !ok || len(tags) != 0 {
		t.Errorf("explicit empty tags did not clear tags: %+v", object)
	}
	if object["notes"] != "rack 3" || object["region"] != "eu-west" || object["provider"] != "example cloud" {
		t.Errorf("partial patch discarded omitted metadata: %+v", object)
	}
}

func assertNodeMetadata(t *testing.T, object map[string]any, groupID string, tags []string) {
	t.Helper()
	for key, want := range map[string]string{"name": "renamed node", "notes": "rack 3", "region": "eu-west", "provider": "example cloud", "group_id": groupID} {
		if object[key] != want {
			t.Errorf("node %s = %v, want %q", key, object[key], want)
		}
	}
	var got []string
	if values, ok := object["tags"].([]any); ok {
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				t.Fatalf("non-string node tag: %v", value)
			}
			got = append(got, text)
		}
	} else {
		t.Fatalf("node DTO lacks tags array: %+v", object)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, tags) {
		t.Errorf("node tags = %v, want %v", got, tags)
	}
}

func TestSecondAdministratorCanManageGroupsUsersAndMetadata(t *testing.T) {
	f := newHTTP(t, 5)
	second, csrf := login(t, f, "admin-two")
	groupID := createHTTPGroup(t, f, second, csrf, "second administrator group")
	groupPath := "/api/v1/groups/" + groupID
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, groupPath, `{"name":"renamed group"}`, second, csrf, publicURL), http.StatusOK)
	assertHTTPStatus(t, request(t, f.handler, http.MethodDelete, groupPath, "", second, csrf, publicURL), http.StatusNoContent)
	created := request(t, f.handler, http.MethodPost, "/api/v1/users", `{"username":"managed-by-second","password":"`+password+`"}`, second, csrf, publicURL)
	assertHTTPStatus(t, created, http.StatusCreated)
	userPath := "/api/v1/users/" + objectID(t, decodeObject(t, created))
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, userPath, `{"username":"renamed-by-second"}`, second, csrf, publicURL), http.StatusOK)
	assertHTTPStatus(t, request(t, f.handler, http.MethodDelete, userPath, "", second, csrf, publicURL), http.StatusNoContent)
	node, err := f.nodes.CreatePending(context.Background(), nodes.CreateRequest{Name: "managed node"})
	if err != nil {
		t.Fatal(err)
	}
	patched := request(t, f.handler, http.MethodPatch, "/api/v1/nodes/"+node.ID, `{"notes":"managed by second","tags":["managed"]}`, second, csrf, publicURL)
	assertHTTPStatus(t, patched, http.StatusOK)
	if got := decodeObject(t, patched)["notes"]; got != "managed by second" {
		t.Fatalf("second administrator metadata mutation not persisted: %v", got)
	}
}

func TestNodeCreationWritesAuditEventReadableToBothAdministrators(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	second, _ := login(t, f, "admin-two")
	response := request(t, f.handler, http.MethodPost, "/api/v1/nodes", `{"name":"audited node"}`, admin, csrf, publicURL)
	assertHTTPStatus(t, response, http.StatusCreated)
	nodeID := objectID(t, decodeObject(t, response))
	var actorID string
	if err := f.db.QueryRow("SELECT id FROM users WHERE username = 'admin'").Scan(&actorID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM audit_events WHERE actor_id = ? AND target_id = ? AND action <> ''", actorID, nodeID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Error("administrator mutation did not persist an attributed audit event")
	}
	for _, cookie := range []*http.Cookie{admin, second} {
		response := request(t, f.handler, http.MethodGet, "/api/v1/audit", "", cookie, "", "")
		assertHTTPStatus(t, response, http.StatusOK)
		object := decodeObject(t, response)
		events, ok := object["events"].([]any)
		if !ok {
			t.Fatalf("audit response lacks events array: %+v", object)
		}
		found := false
		for _, value := range events {
			event, ok := value.(map[string]any)
			if !ok {
				t.Fatalf("invalid audit event: %v", value)
			}
			if event["target_id"] != nodeID {
				continue
			}
			found = true
			objectID(t, event)
			if event["actor_id"] != actorID || event["action"] == "" || event["action"] == nil {
				t.Errorf("audit event lacks actor/action: %+v", event)
			}
			created, ok := event["created_at"].(string)
			if !ok {
				t.Fatalf("audit timestamp is not a string: %+v", event)
			}
			stamp, err := time.Parse(time.RFC3339Nano, created)
			_, offset := stamp.Zone()
			if err != nil || offset != 0 {
				t.Errorf("audit timestamp is not UTC RFC3339: %q", created)
			}
		}
		if !found {
			t.Error("audit collection omitted committed administrator mutation")
		}
	}
	apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/audit", "", nil, "", ""), http.StatusUnauthorized, "unauthenticated")
}

func TestAdministratorHTTPSelfRenameDeletionAndLastAccountProtection(t *testing.T) {
	f := newHTTP(t, 5)
	first, firstCSRF := login(t, f, "admin")
	second, secondCSRF := login(t, f, "admin-two")
	secondTab, _ := login(t, f, "admin-two")
	me := request(t, f.handler, http.MethodGet, "/api/v1/me", "", second, "", "")
	assertHTTPStatus(t, me, http.StatusOK)
	secondID := objectID(t, decodeObject(t, me)["user"].(map[string]any))
	path := "/api/v1/users/" + secondID
	renamed := request(t, f.handler, http.MethodPatch, path, `{"username":"admin-two-renamed"}`, second, secondCSRF, publicURL)
	assertHTTPStatus(t, renamed, http.StatusOK)
	if got := decodeObject(t, renamed)["username"]; got != "admin-two-renamed" {
		t.Fatalf("self rename returned %v", got)
	}
	for _, cookie := range []*http.Cookie{second, secondTab} {
		current := request(t, f.handler, http.MethodGet, "/api/v1/me", "", cookie, "", "")
		assertHTTPStatus(t, current, http.StatusOK)
		if got := decodeObject(t, current)["user"].(map[string]any)["username"]; got != "admin-two-renamed" {
			t.Fatalf("rename not reflected in retained session: %v", got)
		}
	}
	assertHTTPStatus(t, request(t, f.handler, http.MethodDelete, path, "", second, secondCSRF, publicURL), http.StatusNoContent)
	for _, cookie := range []*http.Cookie{second, secondTab} {
		apiError(t, request(t, f.handler, http.MethodGet, "/api/v1/me", "", cookie, "", ""), http.StatusUnauthorized, "unauthenticated")
	}
	me = request(t, f.handler, http.MethodGet, "/api/v1/me", "", first, "", "")
	assertHTTPStatus(t, me, http.StatusOK)
	firstID := objectID(t, decodeObject(t, me)["user"].(map[string]any))
	var sessionsBefore, auditsBefore int
	if err := f.db.QueryRow("SELECT count(*) FROM sessions").Scan(&sessionsBefore); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT count(*) FROM audit_events").Scan(&auditsBefore); err != nil {
		t.Fatal(err)
	}
	apiError(t, request(t, f.handler, http.MethodDelete, "/api/v1/users/"+firstID, "", first, firstCSRF, publicURL), http.StatusConflict, "last_administrator")
	var usersAfter, sessionsAfter, auditsAfter int
	if err := f.db.QueryRow("SELECT count(*) FROM users").Scan(&usersAfter); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT count(*) FROM sessions").Scan(&sessionsAfter); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT count(*) FROM audit_events").Scan(&auditsAfter); err != nil {
		t.Fatal(err)
	}
	if usersAfter != 1 || sessionsAfter != sessionsBefore || auditsAfter != auditsBefore {
		t.Fatalf("last-administrator rejection changed state: users=%d sessions=%d/%d audits=%d/%d", usersAfter, sessionsBefore, sessionsAfter, auditsBefore, auditsAfter)
	}
	assertHTTPStatus(t, request(t, f.handler, http.MethodGet, "/api/v1/me", "", first, "", ""), http.StatusOK)
}

func TestAdministratorHTTPContract(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	response := request(t, f.handler, http.MethodPost, "/api/v1/users", `{"username":"admin-contract","password":"`+password+`"}`, admin, csrf, publicURL)
	assertHTTPStatus(t, response, http.StatusCreated)
	object := decodeObject(t, response)
	id := objectID(t, object)
	assertUser := func(object map[string]any) {
		t.Helper()
		if len(object) != 4 || object["id"] == nil || object["username"] == nil || object["created_at"] == nil {
			t.Errorf("user DTO must contain id, username, created_at and must_change_password: %+v", object)
		}
		if _, ok := object["must_change_password"].(bool); !ok {
			t.Errorf("user DTO must contain a boolean password-change requirement: %+v", object)
		}
		assertNoCredentialDTO(t, object)
	}
	assertUser(object)
	loginResponse := request(t, f.handler, http.MethodPost, "/api/v1/session", `{"username":"admin-contract","password":"`+password+`"}`, nil, "", publicURL)
	assertHTTPStatus(t, loginResponse, http.StatusOK)
	assertUser(decodeObject(t, loginResponse)["user"].(map[string]any))
	second, secondCSRF := login(t, f, "admin-contract")
	for _, path := range []string{"/api/v1/users/" + id, "/api/v1/me"} {
		response := request(t, f.handler, http.MethodGet, path, "", second, "", "")
		assertHTTPStatus(t, response, http.StatusOK)
		value := decodeObject(t, response)
		if path == "/api/v1/me" {
			value = value["user"].(map[string]any)
		}
		assertUser(value)
	}
	listResponse := request(t, f.handler, http.MethodGet, "/api/v1/users", "", second, "", "")
	assertHTTPStatus(t, listResponse, http.StatusOK)
	for _, value := range decodeObject(t, listResponse)["users"].([]any) {
		assertUser(value.(map[string]any))
	}
	var usersBefore, auditBefore int
	if err := f.db.QueryRow("SELECT count(*) FROM users").Scan(&usersBefore); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT count(*) FROM audit_events").Scan(&auditBefore); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path, body string }{
		{"POST", "/api/v1/users", `{"username":"old-role","password":"` + password + `","role":"admin"}`},
		{"PATCH", "/api/v1/users/" + id, `{"username":"old-role","role":"admin"}`},
	} {
		apiError(t, request(t, f.handler, route.method, route.path, route.body, second, secondCSRF, publicURL), 400, "invalid_json")
	}
	var usersAfter, auditAfter int
	if err := f.db.QueryRow("SELECT count(*) FROM users").Scan(&usersAfter); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT count(*) FROM audit_events").Scan(&auditAfter); err != nil {
		t.Fatal(err)
	}
	if usersAfter != usersBefore || auditAfter != auditBefore {
		t.Fatalf("old fields changed users/audit: %d/%d -> %d/%d", usersBefore, auditBefore, usersAfter, auditAfter)
	}
	current := request(t, f.handler, http.MethodGet, "/api/v1/users/"+id, "", second, "", "")
	assertHTTPStatus(t, current, http.StatusOK)
	if got := decodeObject(t, current)["username"]; got != "admin-contract" {
		t.Fatalf("rejected old field renamed account: %v", got)
	}
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/users", `{"username":"admin-third","password":"`+password+`"}`, second, secondCSRF, publicURL), 201)
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/nodes", `{"name":"second-admin-node"}`, second, secondCSRF, publicURL), 201)
}
