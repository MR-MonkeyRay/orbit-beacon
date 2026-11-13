package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestListServiceGroupsPreservesDisplayOrder(t *testing.T) {
	const body = `{"service_groups":[{"id":"00000000-0000-4000-8000-000000000002","name":"Zulu","display_order":2},{"id":"00000000-0000-4000-8000-000000000001","name":"Alpha","display_order":1}]}`
	client := newAPIClient(config{baseURL: "https://beacon.example.test", token: testToken})
	client.http.Transport = groupTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/service-groups" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	reply := call(t, client, "list_service_groups", map[string]any{})
	if reply.Error != nil {
		t.Fatal(reply.Error)
	}
	got := reply.Result.(toolResult)
	if got.IsError || len(got.Content) != 1 || got.Content[0].Text != body {
		t.Fatalf("group response changed: %+v", got)
	}
}
