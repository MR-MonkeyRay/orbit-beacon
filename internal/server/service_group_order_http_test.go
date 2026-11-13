package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"orbitbeacon/internal/server/services"
)

func serviceGroupList(t *testing.T, response *httptest.ResponseRecorder) []services.Group {
	t.Helper()
	assertHTTPStatus(t, response, http.StatusOK)
	object := decodeObject(t, response)
	assertLiteralKeys(t, object, "service_groups")
	for _, group := range object["service_groups"].([]any) {
		assertLiteralKeys(t, group, "id", "name", "display_order")
	}
	var body struct {
		Groups []services.Group `json:"service_groups"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Groups == nil {
		t.Fatal("groups must be an array")
	}
	return body.Groups
}

func TestServiceGroupOrderHTTP(t *testing.T) {
	f := newHTTP(t, 5)
	_, token, cookie, csrf := createHTTPAPIToken(t, f)
	zulu := createHTTPServiceGroup(t, f, cookie, csrf, "Zulu")
	alpha := createHTTPServiceGroup(t, f, cookie, csrf, "Alpha")
	beta := createHTTPServiceGroup(t, f, cookie, csrf, "Beta")
	list := func() []services.Group {
		return serviceGroupList(t, request(t, f.handler, http.MethodGet, "/api/v1/service-groups", "", cookie, "", ""))
	}
	want := []services.Group{{ID: alpha, Name: "Alpha"}, {ID: beta, Name: "Beta"}, {ID: zulu, Name: "Zulu"}}
	if got := list(); !reflect.DeepEqual(got, want) {
		t.Fatalf("default order=%+v want %+v", got, want)
	}
	if _, err := f.db.Exec("UPDATE service_groups SET display_order=CASE id WHEN ? THEN 10 WHEN ? THEN -1 ELSE 0 END", zulu, beta); err != nil {
		t.Fatal(err)
	}
	want = []services.Group{{ID: zulu, Name: "Zulu", DisplayOrder: 10}, {ID: alpha, Name: "Alpha"}, {ID: beta, Name: "Beta", DisplayOrder: -1}}
	if got := list(); !reflect.DeepEqual(got, want) {
		t.Fatalf("weighted order=%+v want %+v", got, want)
	}
	body := `{"ids":["` + beta + `","` + zulu + `","` + alpha + `"]}`
	want = []services.Group{{ID: beta, Name: "Beta", DisplayOrder: 3}, {ID: zulu, Name: "Zulu", DisplayOrder: 2}, {ID: alpha, Name: "Alpha", DisplayOrder: 1}}
	response := request(t, f.handler, http.MethodPut, "/api/v1/service-groups/order", body, cookie, csrf, publicURL)
	if got := serviceGroupList(t, response); !reflect.DeepEqual(got, want) {
		t.Fatalf("reorder=%+v want %+v", got, want)
	}
	if got := list(); !reflect.DeepEqual(got, want) {
		t.Fatalf("persistent order=%+v want %+v", got, want)
	}
	gotToken := serviceGroupList(t, apiTokenRequest(t, f.handler, http.MethodGet, "/api/v1/service-groups", "Bearer "+token, nil))
	if !reflect.DeepEqual(gotToken, want) {
		t.Fatalf("token order=%+v want %+v", gotToken, want)
	}
	var actor, target, details string
	var count int
	if err := f.db.QueryRow("SELECT count(*),actor_id,target_id,details FROM audit_events WHERE action='service_group.reorder' GROUP BY actor_id,target_id,details").Scan(&count, &actor, &target, &details); err != nil || count != 1 || target != "" || details != "{}" {
		t.Fatalf("reorder audit count=%d actor=%q target=%q err=%v", count, actor, target, err)
	}
	var admin string
	if err := f.db.QueryRow("SELECT id FROM users WHERE username='admin'").Scan(&admin); err != nil || actor != admin {
		t.Fatalf("reorder actor=%q want %q err=%v", actor, admin, err)
	}
	for _, tc := range []struct{ name, body, code string }{
		{"missing id", `{"ids":["` + beta + `","` + zulu + `"]}`, "validation_failed"},
		{"extra id", `{"ids":["` + beta + `","` + zulu + `","` + alpha + `","00000000-0000-4000-8000-000000000099"]}`, "validation_failed"},
		{"unknown id", `{"ids":["` + beta + `","` + zulu + `","00000000-0000-4000-8000-000000000099"]}`, "validation_failed"},
		{"duplicate id", `{"ids":["` + alpha + `","` + beta + `","` + beta + `"]}`, "validation_failed"},
		{"empty", `{"ids":[]}`, "validation_failed"},
		{"missing ids", `{}`, "validation_failed"},
		{"null ids", `{"ids":null}`, "validation_failed"},
		{"malformed id", `{"ids":["bad","` + beta + `","` + zulu + `"]}`, "validation_failed"},
		{"wrong ids type", `{"ids":"bad"}`, "invalid_json"},
		{"wrong item type", `{"ids":[1]}`, "invalid_json"},
		{"null item", `{"ids":[null,"` + beta + `","` + zulu + `"]}`, "validation_failed"},
		{"unknown field", `{"ids":[],"name":"bad"}`, "invalid_json"},
		{"invalid JSON", `{"ids":[`, "invalid_json"},
		{"array body", `[]`, "invalid_json"},
		{"null body", `null`, "invalid_json"},
		{"trailing JSON", body + `{}`, "invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := managementState(t, f)
			apiError(t, request(t, f.handler, http.MethodPut, "/api/v1/service-groups/order", tc.body, cookie, csrf, publicURL), 400, tc.code)
			if after := managementState(t, f); !reflect.DeepEqual(before, after) {
				t.Fatal("rejected reorder changed persisted state")
			}
		})
	}
	before := managementState(t, f)
	apiError(t, request(t, f.handler, http.MethodPut, "/api/v1/service-groups/order", body, nil, "", publicURL), 401, "unauthenticated")
	apiError(t, request(t, f.handler, http.MethodPut, "/api/v1/service-groups/order", body, cookie, "", publicURL), 403, "forbidden")
	apiError(t, request(t, f.handler, http.MethodPut, "/api/v1/service-groups/order", body, cookie, "wrong-csrf", publicURL), 403, "forbidden")
	apiError(t, request(t, f.handler, http.MethodPut, "/api/v1/service-groups/order", body, cookie, csrf, "https://untrusted.example"), 403, "forbidden")
	apiError(t, apiTokenRequest(t, f.handler, http.MethodPut, "/api/v1/service-groups/order", "Bearer "+token, cookie), 403, "token_forbidden")
	if after := managementState(t, f); !reflect.DeepEqual(before, after) {
		t.Fatal("unauthorized reorder changed persisted state")
	}
	rename := request(t, f.handler, http.MethodPatch, "/api/v1/service-groups/"+beta, `{"name":"Renamed"}`, cookie, csrf, publicURL)
	assertHTTPStatus(t, rename, http.StatusOK)
	if got := decodeObject(t, rename)["display_order"]; got != float64(3) {
		t.Fatalf("rename lost order: %v", got)
	}
	detail := request(t, f.handler, http.MethodGet, "/api/v1/service-groups/"+beta, "", cookie, "", "")
	assertHTTPStatus(t, detail, http.StatusOK)
	if got := decodeObject(t, detail)["display_order"]; got != float64(3) {
		t.Fatalf("detail lost order: %v", got)
	}
	for _, route := range []struct{ method, path string }{{http.MethodPost, "/api/v1/service-groups"}, {http.MethodPatch, "/api/v1/service-groups/" + beta}} {
		apiError(t, request(t, f.handler, route.method, route.path, `{"name":"bad","display_order":99}`, cookie, csrf, publicURL), 400, "invalid_json")
	}
	newID := createHTTPServiceGroup(t, f, cookie, csrf, "A new group")
	want[0].Name = "Renamed"
	want = append(want, services.Group{ID: newID, Name: "A new group"})
	if got := list(); !reflect.DeepEqual(got, want) {
		t.Fatalf("new group not last=%+v want %+v", got, want)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER reject_group_reorder_audit BEFORE INSERT ON audit_events WHEN NEW.action='service_group.reorder' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	before = managementState(t, f)
	apiError(t, request(t, f.handler, http.MethodPut, "/api/v1/service-groups/order", `{"ids":["`+newID+`","`+alpha+`","`+zulu+`","`+beta+`"]}`, cookie, csrf, publicURL), 500, "internal_error")
	if after := managementState(t, f); !reflect.DeepEqual(before, after) {
		t.Fatal("audit failure did not roll back reorder")
	}
}

func TestServiceGroupOrderHTTPEmpty(t *testing.T) {
	f := newHTTP(t, 5)
	cookie, csrf := login(t, f, "admin")
	for _, body := range []string{`{}`, `{"ids":null}`} {
		apiError(t, request(t, f.handler, http.MethodPut, "/api/v1/service-groups/order", body, cookie, csrf, publicURL), 400, "validation_failed")
	}
	if got := serviceGroupList(t, request(t, f.handler, http.MethodPut, "/api/v1/service-groups/order", `{"ids":[]}`, cookie, csrf, publicURL)); len(got) != 0 {
		t.Fatalf("empty reorder=%+v", got)
	}
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM audit_events WHERE action='service_group.reorder' AND target_id=''").Scan(&count); err != nil || count != 1 {
		t.Fatalf("empty reorder audit=%d err=%v", count, err)
	}
}
