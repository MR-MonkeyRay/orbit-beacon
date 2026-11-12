package server_test

import (
	"encoding/json"
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
