package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestServiceEnhancementsPassThrough(t *testing.T) {
	hours := strings.TrimSuffix(strings.Repeat("null,", 23), ",") + ",100"
	service := fmt.Sprintf(`{"id":"00000000-0000-4000-8000-000000000001","failure_threshold":5,"recent_hours":[%s]}`, hours)
	for _, tc := range []struct {
		name, path, body string
		args             map[string]any
	}{
		{"list_services", "/api/v1/services", `{"services":[` + service + `]}`, map[string]any{}},
		{"get_service", "/api/v1/services/00000000-0000-4000-8000-000000000001", strings.TrimSuffix(service, "}") + `,"stats":{"availability_24h":100,"availability_7d":null,"availability_30d":null,"avg_latency_ms_24h":12}}`, map[string]any{"id": "00000000-0000-4000-8000-000000000001"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newAPIClient(config{baseURL: "https://beacon.example.test", token: testToken})
			client.http.Transport = groupTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Path != tc.path {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			reply := call(t, client, tc.name, tc.args)
			if reply.Error != nil {
				t.Fatal(reply.Error)
			}
			got := reply.Result.(toolResult)
			if got.IsError || len(got.Content) != 1 || got.Content[0].Text != tc.body {
				t.Fatal(got)
			}
		})
	}
}
