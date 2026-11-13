package server_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestServicesHTTPCRUDTokensAndPublicProjection(t *testing.T) {
	f := newHTTP(t, 1)
	_, token, cookie, csrf := createHTTPAPIToken(t, f)
	post := request(t, f.handler, "POST", "/api/v1/services", `{"name":"Health","type":"http","target":"https://internal.example/private","public":true,"notify":true}`, cookie, csrf, publicURL)
	assertHTTPStatus(t, post, 201)
	id := objectID(t, decodeObject(t, post))
	path := "/api/v1/services/" + id
	collectionContains(t, request(t, f.handler, "GET", "/api/v1/services", "", cookie, "", ""), "services", id)
	assertHTTPStatus(t, request(t, f.handler, "GET", path, "", cookie, "", ""), 200)
	for _, r := range []string{"1h", "24h", "7d", "30d"} {
		response := request(t, f.handler, "GET", path+"/history?range="+r, "", cookie, "", "")
		assertHTTPStatus(t, response, 200)
		var data struct {
			Range   string            `json:"range"`
			Step    int               `json:"step_seconds"`
			Buckets []json.RawMessage `json:"buckets"`
		}
		if e := json.Unmarshal(response.Body.Bytes(), &data); e != nil || data.Range != r || data.Step != map[string]int{"1h": 300, "24h": 300, "7d": 3600, "30d": 86400}[r] || data.Buckets == nil {
			t.Fatalf("history %s %v", response.Body.String(), e)
		}
	}
	for _, q := range []string{"", "?range=no", "?range=1h&range=7d", "?range=1h&other=yes"} {
		assertHTTPStatus(t, request(t, f.handler, "GET", path+"/history"+q, "", cookie, "", ""), 400)
	}
	for _, body := range []string{`{}`, `{"name":"bad","type":"http","target":"https://a:b@example.com"}`, `{"name":"bad","type":"tcp","target":"example.com:0"}`, `{"name":"bad","type":"icmp","target":"example.com:80"}`, `{"name":"bad","type":"tcp","target":"a:1","interval_seconds":30,"timeout_seconds":30}`, `{"unknown":true}`, `{"name":"bad","type":"tcp","target":"a:1","enabled":null}`} {
		assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/services", body, cookie, csrf, publicURL), 400)
	}
	for _, route := range []string{"/api/v1/services", path, path + "/history?range=24h"} {
		assertHTTPStatus(t, apiTokenRequest(t, f.handler, "GET", route, "Bearer "+token, nil), 200)
	}
	assertHTTPStatus(t, apiTokenRequest(t, f.handler, "PATCH", path, "Bearer "+token, cookie), 403)
	assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{"name":"Private check"}`, cookie, "", publicURL), 403)
	public := request(t, f.handler, "GET", "/api/public/v1/nodes", "", nil, "", "")
	assertHTTPStatus(t, public, 200)
	var envelope struct {
		Data struct {
			Services []map[string]json.RawMessage `json:"services"`
		}
	}
	if e := json.Unmarshal(public.Body.Bytes(), &envelope); e != nil {
		t.Fatal(e)
	}
	if len(envelope.Data.Services) != 1 {
		t.Fatal(public.Body.String())
	}
	if strings.Contains(public.Body.String(), "internal.example") || strings.Contains(public.Body.String(), `"target"`) || strings.Contains(public.Body.String(), `"error"`) {
		t.Fatalf("public leak %s", public.Body.String())
	}
	assertHTTPStatus(t, request(t, f.handler, "PATCH", path, `{"public":false}`, cookie, csrf, publicURL), 200)
	public = request(t, f.handler, "GET", "/api/public/v1/nodes", "", nil, "", "")
	if e := json.Unmarshal(public.Body.Bytes(), &envelope); e != nil || len(envelope.Data.Services) != 0 {
		t.Fatalf("public cache stale %s %v", public.Body.String(), e)
	}
	assertHTTPStatus(t, request(t, f.handler, "DELETE", path, "", cookie, csrf, publicURL), 204)
	assertHTTPStatus(t, request(t, f.handler, "GET", path, "", cookie, "", ""), 404)
	var count int
	if e := f.db.QueryRow("SELECT count(*) FROM audit_events WHERE target_id=?", id).Scan(&count); e != nil || count != 3 {
		t.Fatalf("audits %d %v", count, e)
	}
}

func TestServicesHTTPCloneFieldsDetailAndThresholdValidation(t *testing.T) {
	f := newHTTP(t, 1)
	_, token, cookie, csrf := createHTTPAPIToken(t, f)
	group := createHTTPServiceGroup(t, f, cookie, csrf, "clone group")
	body := `{"name":"Clone","type":"tcp","target":"localhost:443","interval_seconds":120,"timeout_seconds":5,"latency_threshold_ms":150,"failure_threshold":5,"display_order":17,"public":true,"notify":true,"enabled":false,"group_id":"` + group + `"}`
	created := request(t, f.handler, "POST", "/api/v1/services", body, cookie, csrf, publicURL)
	assertHTTPStatus(t, created, 201)
	object := decodeObject(t, created)
	id := objectID(t, object)
	path := "/api/v1/services/" + id
	var copied map[string]any
	if err := json.Unmarshal([]byte(body), &copied); err != nil {
		t.Fatal(err)
	}
	for k, v := range copied {
		if object[k] != v {
			t.Fatalf("clone field %s=%v want=%v", k, object[k], v)
		}
	}
	for _, v := range []string{"null", "0", "11", "1.5", `"2"`, "true"} {
		response := request(t, f.handler, "PATCH", path, `{"failure_threshold":`+v+`}`, cookie, csrf, publicURL)
		apiError(t, response, 400, "validation_failed")
		fields, ok := decodeObject(t, response)["fields"].(map[string]any)
		if !ok || fields["failure_threshold"] == nil {
			t.Fatal(response.Body.String())
		}
	}
	defaults := request(t, f.handler, "POST", "/api/v1/services", `{"name":"Default","type":"icmp","target":"localhost"}`, cookie, csrf, publicURL)
	assertHTTPStatus(t, defaults, 201)
	if decodeObject(t, defaults)["failure_threshold"] != float64(2) {
		t.Fatal(defaults.Body.String())
	}
	list := apiTokenRequest(t, f.handler, "GET", "/api/v1/services", "Bearer "+token, nil)
	assertHTTPStatus(t, list, 200)
	entries := decodeObject(t, list)["services"].([]any)
	for _, entry := range entries {
		s := entry.(map[string]any)
		if _, ok := s["stats"]; ok {
			t.Fatal("stats in list")
		}
		if len(s["recent_hours"].([]any)) != 24 {
			t.Fatal(s)
		}
	}
	detail := apiTokenRequest(t, f.handler, "GET", path, "Bearer "+token, nil)
	assertHTTPStatus(t, detail, 200)
	stats := decodeObject(t, detail)["stats"].(map[string]any)
	assertLiteralKeys(t, stats, "availability_24h", "availability_7d", "availability_30d", "avg_latency_ms_24h")
	for _, v := range stats {
		if v != nil {
			t.Fatal(stats)
		}
	}
}

func TestServicesHTTPBatchValidationActionsAndTokens(t *testing.T) {
	f := newHTTP(t, 1)
	_, token, cookie, csrf := createHTTPAPIToken(t, f)
	create := func(name string) string {
		t.Helper()
		response := request(t, f.handler, "POST", "/api/v1/services", `{"name":"`+name+`","type":"icmp","target":"localhost"}`, cookie, csrf, publicURL)
		assertHTTPStatus(t, response, 201)
		return objectID(t, decodeObject(t, response))
	}
	a, b := create("first"), create("second")
	group := createHTTPServiceGroup(t, f, cookie, csrf, "destination")
	const missing = "00000000-0000-4000-8000-000000000999"
	ids := `["` + b + `","` + missing + `","` + a + `"]`
	for _, body := range []string{
		`{}`, `{"service_ids":[],"action":"pause"}`, `{"service_ids":["` + a + `","` + a + `"],"action":"pause"}`,
		`{"service_ids":["bad"],"action":"pause"}`, `{"service_ids":` + ids + `,"action":"delete"}`,
		`{"service_ids":` + ids + `,"action":"move"}`, `{"service_ids":` + ids + `,"action":"move","group_id":"` + missing + `"}`,
	} {
		apiError(t, request(t, f.handler, "POST", "/api/v1/services/batch", body, cookie, csrf, publicURL), 400, "validation_failed")
	}
	many := make([]string, 101)
	for i := range many {
		many[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1000)
	}
	raw, _ := json.Marshal(map[string]any{"service_ids": many, "action": "pause"})
	apiError(t, request(t, f.handler, "POST", "/api/v1/services/batch", string(raw), cookie, csrf, publicURL), 400, "validation_failed")
	assertHTTPStatus(t, apiTokenRequest(t, f.handler, "POST", "/api/v1/services/batch", "Bearer "+token, cookie), 403)
	assertHTTPStatus(t, request(t, f.handler, "POST", "/api/v1/services/batch", `{"service_ids":`+ids+`,"action":"pause"}`, cookie, "", publicURL), 403)
	for _, action := range []string{"pause", "pause", "resume", "move", "move", "delete"} {
		body := `{"service_ids":` + ids + `,"action":"` + action + `"`
		if action == "delete" {
			body += `,"confirm":true`
		}
		if action == "move" {
			if group != "" {
				body += `,"group_id":"` + group + `"`
				group = ""
			} else {
				body += `,"group_id":null`
			}
		}
		body += `}`
		response := request(t, f.handler, "POST", "/api/v1/services/batch", body, cookie, csrf, publicURL)
		assertHTTPStatus(t, response, 200)
		results := decodeObject(t, response)["results"].([]any)
		if len(results) != 3 {
			t.Fatal(results)
		}
		for i, id := range []string{b, missing, a} {
			result := results[i].(map[string]any)
			if result["service_id"] != id {
				t.Fatal(results)
			}
			if id == missing {
				if result["status"] != "failed" || result["error"].(map[string]any)["code"] != "service_not_found" {
					t.Fatal(result)
				}
			} else {
				assertLiteralKeys(t, result, "service_id", "status")
				if result["status"] != "succeeded" {
					t.Fatal(result)
				}
			}
		}
	}
	for _, id := range []string{a, b} {
		var updates, deletes int
		if err := f.db.QueryRow("SELECT count(*) FROM audit_events WHERE target_id=? AND action='service.update'", id).Scan(&updates); err != nil {
			t.Fatal(err)
		}
		if err := f.db.QueryRow("SELECT count(*) FROM audit_events WHERE target_id=? AND action='service.delete'", id).Scan(&deletes); err != nil || updates != 5 || deletes != 1 {
			t.Fatal(updates, deletes, err)
		}
	}
}

func TestServicesHTTPBatchAuditFailureRollsBack(t *testing.T) {
	for _, action := range []string{"pause", "resume", "move", "delete"} {
		t.Run(action, func(t *testing.T) {
			f := newHTTP(t, 1)
			cookie, csrf := login(t, f, "admin")
			response := request(t, f.handler, "POST", "/api/v1/services", `{"name":"health","type":"icmp","target":"localhost"}`, cookie, csrf, publicURL)
			assertHTTPStatus(t, response, 201)
			id := objectID(t, decodeObject(t, response))
			group := createHTTPServiceGroup(t, f, cookie, csrf, "group")
			if _, err := f.db.Exec(`CREATE TRIGGER reject_batch_audit BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			before := managementState(t, f)
			body := `{"service_ids":["` + id + `"],"action":"` + action + `","confirm":true`
			if action == "move" {
				body += `,"group_id":"` + group + `"`
			}
			body += `}`
			response = request(t, f.handler, "POST", "/api/v1/services/batch", body, cookie, csrf, publicURL)
			assertHTTPStatus(t, response, 200)
			result := decodeObject(t, response)["results"].([]any)[0].(map[string]any)
			if result["status"] != "failed" || result["error"].(map[string]any)["code"] != "storage_error" {
				t.Fatal(result)
			}
			if !reflect.DeepEqual(before, managementState(t, f)) {
				t.Fatal("audit failure mutated persistent state")
			}
		})
	}
}
