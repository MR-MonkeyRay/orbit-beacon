package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func createHTTPServiceGroup(t *testing.T, f fixture, cookie *http.Cookie, csrf, name string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, f.handler, http.MethodPost, "/api/v1/service-groups", string(body), cookie, csrf, publicURL)
	assertHTTPStatus(t, response, http.StatusCreated)
	object := decodeObject(t, response)
	assertLiteralKeys(t, object, "id", "name", "display_order")
	if object["name"] != name || object["display_order"] != float64(0) {
		t.Fatalf("created group=%+v", object)
	}
	return objectID(t, object)
}

func TestServiceGroupsHTTPCRUDValidationAndIsolation(t *testing.T) {
	f := newHTTP(t, 5)
	cookie, csrf := login(t, f, "admin")
	second, secondCSRF := login(t, f, "admin-two")
	nodeGroup := createHTTPGroup(t, f, cookie, csrf, "Independent")
	id := createHTTPServiceGroup(t, f, second, secondCSRF, "Independent")
	path := "/api/v1/service-groups/" + id
	collectionContains(t, request(t, f.handler, "GET", "/api/v1/service-groups", "", second, "", ""), "service_groups", id)
	response := request(t, f.handler, "GET", path, "", cookie, "", "")
	assertHTTPStatus(t, response, 200)
	assertLiteralKeys(t, decodeObject(t, response), "id", "name", "display_order")
	for _, name := range []string{"", " \t\n", strings.Repeat("a", 256), strings.Repeat("界", 86), "Independent"} {
		raw, _ := json.Marshal(map[string]string{"name": name})
		before := managementState(t, f)
		apiError(t, request(t, f.handler, "POST", "/api/v1/service-groups", string(raw), cookie, csrf, publicURL), 400, "validation_failed")
		if !reflect.DeepEqual(before, managementState(t, f)) {
			t.Fatal("invalid create changed state")
		}
	}
	other := createHTTPServiceGroup(t, f, cookie, csrf, strings.Repeat("a", 255))
	for _, body := range []string{`{}`, `{"name":" "}`, `{"name":"Independent"}`} {
		apiError(t, request(t, f.handler, "PATCH", "/api/v1/service-groups/"+other, body, cookie, csrf, publicURL), 400, "validation_failed")
	}
	// Like node groups, surrounding whitespace is preserved, not normalized.
	spaced := createHTTPServiceGroup(t, f, cookie, csrf, " Independent ")
	assertHTTPStatus(t, request(t, f.handler, "DELETE", "/api/v1/service-groups/"+spaced, "", cookie, csrf, publicURL), 204)
	assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{"name":"Renamed"}`, second, secondCSRF, publicURL), 200)
	assertHTTPStatus(t, request(t, f.handler, "DELETE", path, "", cookie, csrf, publicURL), 204)
	for _, method := range []string{"GET", "PATCH", "DELETE"} {
		apiError(t, request(t, f.handler, method, path, `{"name":"gone"}`, cookie, csrf, publicURL), 404, "not_found")
	}
	var name string
	if err := f.db.QueryRow("SELECT name FROM groups WHERE id=?", nodeGroup).Scan(&name); err != nil || name != "Independent" {
		t.Fatalf("node group changed: %s %v", name, err)
	}
	rows, err := f.db.Query("SELECT action,details FROM audit_events WHERE target_id=? ORDER BY action", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[string]string{"service_group.create": "Independent", "service_group.update": "Renamed", "service_group.delete": "Renamed"}
	for rows.Next() {
		var action, raw string
		if err := rows.Scan(&action, &raw); err != nil {
			t.Fatal(err)
		}
		var details map[string]string
		if err := json.Unmarshal([]byte(raw), &details); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(details, map[string]string{"name": want[action]}) || want[action] == "" {
			t.Fatalf("audit %s: %s", action, raw)
		}
		delete(want, action)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(want) != 0 {
		t.Fatalf("missing audits: %v", want)
	}
}

func TestServiceGroupsHTTPCap(t *testing.T) {
	f := newHTTP(t, 1)
	cookie, csrf := login(t, f, "admin")
	// Seed 99, then test the boundary through HTTP. Service/node capacities are independent.
	for i := 0; i < 99; i++ {
		if _, err := f.db.Exec("INSERT INTO service_groups(id,name) VALUES(?,?)", fmt.Sprintf("00000000-0000-4000-8000-%012d", i), fmt.Sprintf("seed %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	id := createHTTPServiceGroup(t, f, cookie, csrf, "Hundredth")
	before := managementState(t, f)
	apiError(t, request(t, f.handler, "POST", "/api/v1/service-groups", `{"name":"Overflow"}`, cookie, csrf, publicURL), 409, "service_group_capacity_reached")
	if !reflect.DeepEqual(before, managementState(t, f)) {
		t.Fatal("capacity failure changed state")
	}
	assertHTTPStatus(t, request(t, f.handler, "PATCH", "/api/v1/service-groups/"+id, `{"name":"Rename at capacity"}`, cookie, csrf, publicURL), 200)
	assertHTTPStatus(t, request(t, f.handler, "DELETE", "/api/v1/service-groups/"+id, "", cookie, csrf, publicURL), 204)
	createHTTPServiceGroup(t, f, cookie, csrf, "Replacement")
}

func TestServicesHTTPGroupID(t *testing.T) {
	f := newHTTP(t, 5)
	_, token, cookie, csrf := createHTTPAPIToken(t, f)
	group := createHTTPServiceGroup(t, f, cookie, csrf, "Private monitor group")
	nodeGroup := createHTTPGroup(t, f, cookie, csrf, "Not a monitor group")
	const base = `{"name":"Health","type":"tcp","target":"localhost:443","public":true`
	for _, value := range []string{`"` + group + `"`, `null`, `""`, ""} {
		body := base
		var want any
		if value != "" {
			body += `,"group_id":` + value
		}
		if value == `"`+group+`"` {
			want = group
		}
		body += "}"
		response := request(t, f.handler, "POST", "/api/v1/services", body, cookie, csrf, publicURL)
		assertHTTPStatus(t, response, 201)
		object := decodeObject(t, response)
		id := objectID(t, object)
		if got, ok := object["group_id"]; !ok || got != want {
			t.Fatalf("create group_id=%v want=%v", got, want)
		}
		path := "/api/v1/services/" + id
		for _, step := range []struct {
			body string
			want any
		}{
			{`{"group_id":"` + group + `"}`, group}, {`{"name":"Unchanged group"}`, group}, {`{"group_id":null}`, nil},
			{`{"group_id":"` + group + `"}`, group}, {`{"group_id":""}`, nil}, {`{"group_id":"` + group + `"}`, group},
		} {
			response = request(t, f.handler, "PATCH", path, step.body, cookie, csrf, publicURL)
			assertHTTPStatus(t, response, 200)
			if got := decodeObject(t, response)["group_id"]; got != step.want {
				t.Fatalf("patch %s: %v want %v", step.body, got, step.want)
			}
		}
		for _, r := range []struct {
			path string
			list bool
		}{{path, false}, {"/api/v1/services", true}} {
			for _, useToken := range []bool{false, true} {
				response = request(t, f.handler, "GET", r.path, "", cookie, "", "")
				if useToken {
					response = apiTokenRequest(t, f.handler, "GET", r.path, "Bearer "+token, nil)
				}
				assertHTTPStatus(t, response, 200)
				got := decodeObject(t, response)
				if r.list {
					found := false
					for _, item := range got["services"].([]any) {
						o := item.(map[string]any)
						if o["id"] == id {
							got = o
							found = true
							break
						}
					}
					if !found {
						t.Fatal("service not listed")
					}
				}
				if got["group_id"] != group {
					t.Fatalf("GET %s: %+v", r.path, got)
				}
			}
		}
	}
	list := request(t, f.handler, "GET", "/api/v1/services", "", cookie, "", "")
	id := decodeObject(t, list)["services"].([]any)[0].(map[string]any)["id"].(string)
	for _, value := range []string{`"00000000-0000-4000-8000-000000000099"`, `"bad"`, `"` + nodeGroup + `"`, `123`, `{}`, `[]`, `true`} {
		for _, route := range []struct{ method, path, body string }{{"POST", "/api/v1/services", base + `,"group_id":` + value + `}`}, {"PATCH", "/api/v1/services/" + id, `{"group_id":` + value + `}`}} {
			before := managementState(t, f)
			result := apiError(t, request(t, f.handler, route.method, route.path, route.body, cookie, csrf, publicURL), 400, "validation_failed")
			var fields map[string]string
			if err := json.Unmarshal(result["fields"], &fields); err != nil || fields["group_id"] == "" {
				t.Fatalf("missing group_id field: %v %v", fields, err)
			}
			if !reflect.DeepEqual(before, managementState(t, f)) {
				t.Fatal("invalid group assignment changed state")
			}
		}
	}
	public := request(t, f.handler, "GET", "/api/public/v1/nodes", "", nil, "", "")
	assertHTTPStatus(t, public, 200)
	var envelope struct {
		Data struct {
			Services []map[string]any `json:"services"`
		} `json:"data"`
	}
	if err := json.Unmarshal(public.Body.Bytes(), &envelope); err != nil || len(envelope.Data.Services) != 4 {
		t.Fatalf("public services missing: %s err=%v", public.Body, err)
	}
	for _, service := range envelope.Data.Services {
		assertLiteralKeys(t, service, "name", "type", "status", "latency_ms", "availability")
	}
	for _, secret := range []string{"group_id", "service_groups", group, "Private monitor group"} {
		if strings.Contains(public.Body.String(), secret) {
			t.Fatalf("public leak %q: %s", secret, public.Body)
		}
	}
	assertHTTPStatus(t, request(t, f.handler, "DELETE", "/api/v1/service-groups/"+group, "", cookie, csrf, publicURL), 204)
	response := request(t, f.handler, "GET", "/api/v1/services", "", cookie, "", "")
	assertHTTPStatus(t, response, 200)
	for _, item := range decodeObject(t, response)["services"].([]any) {
		service := item.(map[string]any)
		if value, ok := service["group_id"]; !ok || value != nil {
			t.Fatalf("delete left stale group: %+v", service)
		}
		detail := request(t, f.handler, "GET", "/api/v1/services/"+service["id"].(string), "", cookie, "", "")
		assertHTTPStatus(t, detail, 200)
		if decodeObject(t, detail)["group_id"] != nil {
			t.Fatal("detail returned stale group")
		}
	}
}

func TestAPITokenHTTPServiceGroupsReadOnly(t *testing.T) {
	f := newHTTP(t, 1)
	_, token, cookie, csrf := createHTTPAPIToken(t, f)
	id := createHTTPServiceGroup(t, f, cookie, csrf, "Read only")
	got := serviceGroupList(t, apiTokenRequest(t, f.handler, "GET", "/api/v1/service-groups", "Bearer "+token, nil))
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("token list=%+v", got)
	}
	before := managementState(t, f)
	for _, route := range []struct{ method, path string }{{"POST", "/api/v1/service-groups"}, {"PATCH", "/api/v1/service-groups/" + id}, {"DELETE", "/api/v1/service-groups/" + id}, {"PUT", "/api/v1/service-groups/order"}, {"GET", "/api/v1/service-groups/" + id}} {
		apiError(t, apiTokenRequest(t, f.handler, route.method, route.path, "Bearer "+token, cookie), 403, "token_forbidden")
	}
	if !reflect.DeepEqual(before, managementState(t, f)) {
		t.Fatal("token changed state")
	}
}
