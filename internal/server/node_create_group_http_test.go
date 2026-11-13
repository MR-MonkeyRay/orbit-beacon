package server_test

import (
	"database/sql"
	"net/http"
	"reflect"
	"testing"
)

func TestNodeCreateHTTPGroup(t *testing.T) {
	f := newHTTP(t, 10)
	cookie, csrf := login(t, f, "admin")
	group := createHTTPGroup(t, f, cookie, csrf, "New nodes")
	for _, tc := range []struct {
		name, body string
		group      any
	}{
		{"with group", `{"name":"Grouped","group_id":"` + group + `"}`, group},
		{"without group", `{"name":"Ungrouped"}`, nil},
		{"null group", `{"name":"Ungrouped null","group_id":null}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := request(t, f.handler, http.MethodPost, "/api/v1/nodes", tc.body, cookie, csrf, publicURL)
			assertHTTPStatus(t, response, http.StatusCreated)
			object := decodeObject(t, response)
			if object["group_id"] != tc.group || object["status"] != "pending" {
				t.Fatalf("created node=%v", object)
			}
			id := objectID(t, object)
			var stored sql.NullString
			if err := f.db.QueryRow("SELECT group_id FROM nodes WHERE id=?", id).Scan(&stored); err != nil || stored.Valid != (tc.group != nil) || (stored.Valid && stored.String != tc.group) {
				t.Fatalf("stored group=%+v err=%v", stored, err)
			}
			read := request(t, f.handler, http.MethodGet, "/api/v1/nodes/"+id, "", cookie, "", "")
			assertHTTPStatus(t, read, http.StatusOK)
			if got := decodeObject(t, read)["group_id"]; got != tc.group {
				t.Fatalf("read group=%v want %v", got, tc.group)
			}
		})
	}
	for _, tc := range []struct{ name, body, code string }{
		{"unknown group", `{"name":"Rejected","group_id":"00000000-0000-4000-8000-000000000099"}`, "validation_failed"},
		{"malformed group", `{"name":"Rejected","group_id":"not-a-uuid"}`, "validation_failed"},
		{"empty group", `{"name":"Rejected","group_id":""}`, "validation_failed"},
		{"wrong type", `{"name":"Rejected","group_id":123}`, "validation_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := managementState(t, f)
			apiError(t, request(t, f.handler, http.MethodPost, "/api/v1/nodes", tc.body, cookie, csrf, publicURL), 400, tc.code)
			if after := managementState(t, f); !reflect.DeepEqual(before, after) {
				t.Fatal("rejected node creation changed persisted state")
			}
		})
	}
}
