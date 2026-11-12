package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"orbitbeacon/internal/buildinfo"
)

const testToken = "obt_abcdefghijklmnopqrstuvwxyz0123456789_-ABCDE"
const testID = "00000000-0000-4000-8000-000000000001"

func environment(base, token string) func(string) string {
	return func(key string) string {
		switch key {
		case "ORBITBEACON_URL":
			return base
		case "ORBITBEACON_TOKEN":
			return token
		}
		return ""
	}
}
func newTestClient(t *testing.T, handler http.HandlerFunc) *apiClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected HTTP method %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "orbitbeacon-mcp/"+buildinfo.Current().Version {
			t.Error("HTTP headers differ from contract")
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	cfg, err := loadConfig(environment(server.URL, testToken))
	if err != nil {
		t.Fatal(err)
	}
	client := newAPIClient(cfg)
	t.Cleanup(client.http.CloseIdleConnections)
	return client
}
func wire(method string, id any, params any) string {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		m["id"] = id
	}
	if params != nil {
		m["params"] = params
	}
	b, _ := json.Marshal(m)
	return string(b) + "\n"
}
func decodeLines(t *testing.T, output string) []map[string]json.RawMessage {
	t.Helper()
	var replies []map[string]json.RawMessage
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if line == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			t.Fatalf("stdout line is not one JSON value: %.200s", line)
		}
		var r map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &r) != nil || string(r["jsonrpc"]) != `"2.0"` {
			t.Fatal("invalid JSON-RPC response")
		}
		replies = append(replies, r)
	}
	return replies
}
func toolFromResponse(t *testing.T, r map[string]json.RawMessage) toolResult {
	t.Helper()
	if r["error"] != nil {
		t.Fatalf("unexpected RPC error: %s", r["error"])
	}
	var result toolResult
	if err := json.Unmarshal(r["result"], &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func validArguments(name string) map[string]any {
	switch name {
	case "get_service_history":
		return map[string]any{"id": testID, "range": "1h"}
	case "get_node_history":
		return map[string]any{"id": testID, "metric": "cpu_percent", "range": "1h"}
	case "get_service", "get_node", "get_node_latest", "get_node_availability", "get_node_traffic", "get_node_probes", "get_alert":
		return map[string]any{"id": testID}
	default:
		return map[string]any{}
	}
}
func call(t *testing.T, client *apiClient, name string, args any) *response {
	t.Helper()
	s := session{initialized: true, client: client}
	return s.handle(context.Background(), []byte(wire("tools/call", 1, map[string]any{"name": name, "arguments": args})))
}

func TestCompleteSessionWithMemoryPipes(t *testing.T) {
	var calls atomic.Int32
	const body = "{\n  \"nodes\": []\n}\n"
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.RequestURI() != "/api/v1/nodes?limit=2&offset=0" {
			t.Errorf("target = %s", r.URL.RequestURI())
		}
		io.WriteString(w, body)
	})
	input, inputWriter := io.Pipe()
	outputReader, output := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- serve(context.Background(), input, output, client); output.Close() }()
	reader := bufio.NewReader(outputReader)
	exchange := func(request string) map[string]json.RawMessage {
		t.Helper()
		if _, err := io.WriteString(inputWriter, request); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return decodeLines(t, line)[0]
	}
	init := exchange(wire("initialize", 1, map[string]any{"protocolVersion": preferredVersion, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "test", "version": "1"}}))
	var initialized map[string]json.RawMessage
	json.Unmarshal(init["result"], &initialized)
	if string(initialized["capabilities"]) != `{"tools":{}}` || initialized["instructions"] != nil {
		t.Fatalf("initialize result = %s", init["result"])
	}
	var info map[string]string
	json.Unmarshal(initialized["serverInfo"], &info)
	if !reflect.DeepEqual(info, map[string]string{"name": "orbitbeacon", "version": buildinfo.Current().Version}) {
		t.Fatal("serverInfo mismatch")
	}
	if _, err := io.WriteString(inputWriter, wire("notifications/initialized", nil, nil)); err != nil {
		t.Fatal(err)
	}
	listed := exchange(wire("tools/list", 2, nil))
	var list struct {
		Tools []tool `json:"tools"`
	}
	if err := json.Unmarshal(listed["result"], &list); err != nil || len(list.Tools) != len(toolTable()) {
		t.Fatal("tools/list mismatch")
	}
	reply := exchange(wire("tools/call", 3, map[string]any{"name": "list_nodes", "arguments": map[string]any{"limit": 2, "offset": 0}}))
	result := toolFromResponse(t, reply)
	if result.IsError || result.Content[0].Text != body {
		t.Fatalf("tool result = %+v", result)
	}
	inputWriter.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	outputReader.Close()
	if calls.Load() != 1 {
		t.Fatal("expected one HTTP call")
	}
}

func TestToolTableMatchesReadOnlyOpenAPI(t *testing.T) {
	expected := []string{"list_services", "get_service", "get_service_history", "list_nodes", "get_node", "get_node_latest", "get_node_history", "get_node_availability", "get_node_traffic", "get_node_probes", "list_alerts", "get_alert_summary", "get_alert", "list_groups"}
	var names []string
	actualPaths := map[string]bool{}
	for _, tool := range toolTable() {
		names = append(names, tool.Name)
		if actualPaths[tool.path] {
			t.Errorf("duplicate path %s", tool.path)
		}
		actualPaths[tool.path] = true
		if tool.Annotations != (annotations{true, false, true, false}) || tool.InputSchema.Type != "object" || tool.InputSchema.AdditionalProperties {
			t.Errorf("unsafe tool schema: %s", tool.Name)
		}
		if tool.Description == "" || strings.Contains(tool.Description, "\n") {
			t.Errorf("invalid description: %s", tool.Name)
		}
	}
	if !reflect.DeepEqual(names, expected) {
		t.Fatalf("tool names = %v", names)
	}
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Paths also contain parameters arrays, so decode operation maps separately.
	var document struct {
		Paths map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	expectedPaths := map[string]bool{}
	for path, node := range document.Paths {
		var operations map[string]yaml.Node
		if err := node.Decode(&operations); err != nil {
			t.Fatal(err)
		}
		if get, ok := operations["get"]; ok {
			var operation struct {
				ReadOnly bool `yaml:"x-read-only-token"`
			}
			if err := get.Decode(&operation); err != nil {
				t.Fatal(err)
			}
			if operation.ReadOnly {
				expectedPaths[path] = true
			}
		}
	}
	if !reflect.DeepEqual(actualPaths, expectedPaths) {
		t.Fatalf("tool paths = %v; read-only GET paths = %v", actualPaths, expectedPaths)
	}
	var calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		pattern := strings.Replace(r.URL.Path, testID, "{id}", 1)
		if !expectedPaths[pattern] {
			t.Errorf("HTTP path outside read-only GET contract: %s", r.URL.Path)
		}
		calls.Add(1)
		io.WriteString(w, `{"ok":true}`)
	})
	for _, tool := range toolTable() {
		reply := call(t, client, tool.Name, validArguments(tool.Name))
		if reply.Error != nil || reply.Result.(toolResult).IsError {
			t.Errorf("%s failed: %+v", tool.Name, reply)
		}
	}
	if calls.Load() != int32(len(expected)) {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestHistoryMetricsMatchOpenAPI(t *testing.T) {
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]struct {
			Get struct {
				Parameters []struct {
					Name   string `yaml:"name"`
					Schema struct {
						Enum []string `yaml:"enum"`
					} `yaml:"schema"`
				} `yaml:"parameters"`
			} `yaml:"get"`
		} `yaml:"paths"`
	}
	if err = yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, parameter := range doc.Paths["/api/v1/nodes/{id}/history"].Get.Parameters {
		if parameter.Name == "metric" {
			want = parameter.Schema.Enum
		}
	}
	for _, tool := range toolTable() {
		if tool.Name == "get_node_history" {
			got := tool.InputSchema.Properties["metric"].Enum.([]string)
			if len(want) == 0 || !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
				t.Fatalf("metrics=%v want %v", got, want)
			}
		}
	}
}

func TestInvalidArgumentsNeverSendHTTP(t *testing.T) {
	var requests atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); io.WriteString(w, `{}`) })
	for _, tool := range toolTable() {
		cases := []map[string]any{}
		unknown := validArguments(tool.Name)
		unknown["unexpected"] = true
		cases = append(cases, unknown)
		for key, p := range tool.InputSchema.Properties {
			values := []any{nil, true, []any{}, map[string]any{}}
			if p.Type == "integer" {
				values = append(values, "1", 1.5, -1, 1e100)
			} else {
				values = append(values, 12, "")
			}
			if p.Maximum != nil {
				values = append(values, *p.Maximum+1)
			}
			if p.Minimum != nil {
				values = append(values, *p.Minimum-1)
			}
			if p.Enum != nil {
				if p.Type == "integer" {
					values = append(values, 31)
				} else {
					values = append(values, "invalid")
				}
			}
			if key == "id" {
				values = append(values, "../audit", "..%2Faudit", "not-a-uuid", "00000000-0000-0000-0000-000000000000")
			}
			for _, value := range values {
				args := validArguments(tool.Name)
				args[key] = value
				cases = append(cases, args)
			}
		}
		for _, key := range tool.InputSchema.Required {
			args := validArguments(tool.Name)
			delete(args, key)
			cases = append(cases, args)
		}
		for i, args := range cases {
			r := call(t, client, tool.Name, args)
			if r.Error != nil || !r.Result.(toolResult).IsError {
				t.Errorf("%s case %d accepted invalid arguments: %v", tool.Name, i, args)
			}
		}
		for _, args := range []any{nil, []any{}, "text", true, 1} {
			if r := call(t, client, tool.Name, args); r.Error != nil || !r.Result.(toolResult).IsError {
				t.Errorf("%s accepted non-object arguments", tool.Name)
			}
		}
	}
	unknown := call(t, client, "write_node", map[string]any{})
	if unknown.Error == nil || unknown.Error.Code != -32602 {
		t.Fatal("unknown tool must return -32602")
	}
	if requests.Load() != 0 {
		t.Fatalf("sent %d requests for invalid arguments", requests.Load())
	}
}

func TestQueryParametersAndBounds(t *testing.T) {
	cursor := base64.RawURLEncoding.EncodeToString([]byte("2026-01-01T00:00:00.000000000Z|" + testID))
	cases := []struct {
		name  string
		args  map[string]any
		query string
	}{
		{"list_nodes", map[string]any{"limit": 1, "offset": 0}, "limit=1&offset=0"},
		{"list_nodes", map[string]any{"limit": 100, "offset": 1000}, "limit=100&offset=1000"},
		{"list_alerts", map[string]any{"state": "firing", "limit": 100, "before": cursor}, "before=" + cursor + "&limit=100&state=firing"},
		{"list_alerts", map[string]any{"before": "cursor&state=all"}, "before=cursor%26state%3Dall"},
		{"get_node_history", map[string]any{"id": testID, "metric": "net_rx_bytes_per_second", "range": "90d"}, "metric=net_rx_bytes_per_second&range=90d"},
		{"get_node_availability", map[string]any{"id": testID, "days": 90}, "days=90"},
		{"get_node_traffic", map[string]any{"id": testID, "days": 400}, "days=400"},
		{"get_node_traffic", map[string]any{"id": testID, "days": 1}, "days=1"},
	}
	for _, tc := range cases {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != tc.query {
				t.Errorf("query = %s, want %s", r.URL.RawQuery, tc.query)
			}
			io.WriteString(w, `{}`)
		})
		reply := call(t, client, tc.name, tc.args)
		if reply.Error != nil || reply.Result.(toolResult).IsError {
			t.Fatalf("valid arguments rejected: %+v", tc)
		}
	}
}

func TestRedirectDoesNotForwardToken(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		t.Error("redirect target received a request")
	}))
	defer target.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", target.URL)
			w.WriteHeader(status)
			io.WriteString(w, `{"code":"redirected"}`)
		})
		result := client.get(context.Background(), "/api/v1/groups")
		if !result.IsError || result.Content[0].Text != fmt.Sprintf("HTTP %d redirected", status) {
			t.Fatalf("redirect result = %+v", result)
		}
	}
	if targetCalls.Load() != 0 {
		t.Fatal("redirect followed")
	}
}

func TestResponseFailuresAreSanitized(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body, want string
	}{
		{"unauthorized", 401, `{"code":"unauthenticated","message":"sensitive-body ` + testToken + `"}`, "HTTP 401 unauthenticated"},
		{"forbidden", 403, `{"code":"token_forbidden","message":"sensitive-body ` + testToken + `"}`, "HTTP 403 token_forbidden"},
		{"internal", 500, `{"code":"internal_error","message":"sensitive-body ` + testToken + `"}`, "HTTP 500 internal_error"},
		{"unsafe-code", 500, `{"code":"sensitive body ` + testToken + `"}`, "HTTP 500 unknown_error"},
		{"non-json", 500, "sensitive-body " + testToken, "HTTP 500 unknown_error"},
		{"oversized", 200, `{"data":"` + strings.Repeat("a", maxResponseBytes) + `"}`, "HTTP 200 response_too_large"},
		{"oversized-error", 500, strings.Repeat("a", maxResponseBytes+1), "HTTP 500 response_too_large"},
		{"invalid-json", 200, "sensitive-body", "HTTP 200 invalid_json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) })
			input := wire("initialize", 1, map[string]string{"protocolVersion": preferredVersion}) + wire("tools/call", 2, map[string]any{"name": "list_groups", "arguments": map[string]any{}})
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), nil, environment(client.config.baseURL, testToken), strings.NewReader(input), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("run exit = %d", code)
			}
			lines := decodeLines(t, stdout.String())
			result := toolFromResponse(t, lines[1])
			if !result.IsError || result.Content[0].Text != tc.want {
				t.Fatalf("got %+v, want %s", result, tc.want)
			}
			if strings.Contains(stdout.String()+stderr.String(), testToken) || strings.Contains(stdout.String(), "sensitive-body") {
				t.Fatal("sensitive response leaked")
			}
		})
	}
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) })
	if client.http.Timeout != 15*time.Second {
		t.Fatal("HTTP timeout mismatch")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := client.get(ctx, "/api/v1/groups"); !got.IsError || got.Content[0].Text != "HTTP request failed" {
		t.Fatalf("transport error = %+v", got)
	}
}

func TestConfigValidationAndNoCredentialLeaks(t *testing.T) {
	accepted := []string{"http://127.0.0.1:1", "http://127.33.44.55", "http://[::1]:1", "http://localhost:8080/", "http://LOCALHOST", "https://example.com", "https://example.com/"}
	for _, base := range accepted {
		if _, err := loadConfig(environment(base, testToken)); err != nil {
			t.Errorf("rejected %s: %v", base, err)
		}
	}
	rejected := []string{"", "http://example.com", "ftp://127.0.0.1", "http://[::ffff:127.0.0.1]", "http://[::2]", "http://localhost.example.com", "https://example.com/path", "https://example.com/%2f", "https://example.com?x=1", "https://example.com?", "https://example.com#fragment", "https://example.com#", "https://user:password@example.com", "https://" + testToken + "@example.com", "https://example.com/" + testToken, "https://example.com?q=" + testToken}
	for _, base := range rejected {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), nil, environment(base, testToken), strings.NewReader(""), &stdout, &stderr); code == 0 || stderr.Len() == 0 || stdout.Len() != 0 {
			t.Errorf("invalid URL accepted: %s", base)
		}
		if strings.Contains(stdout.String()+stderr.String(), testToken) {
			t.Fatal("configuration leaked token")
		}
	}
	for _, token := range []string{"", "obt_", testToken + "A", testToken[:len(testToken)-1], strings.Replace(testToken, "a", "+", 1), testToken + "\n", "bad_" + testToken[4:]} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), nil, environment("https://example.com", token), strings.NewReader(""), &stdout, &stderr); code == 0 || stderr.Len() == 0 || stdout.Len() != 0 {
			t.Error("invalid token accepted")
		}
		if strings.Contains(stdout.String()+stderr.String(), testToken) {
			t.Fatal("invalid-token error leaked token")
		}
	}
	var stdout, stderr bytes.Buffer
	if run(context.Background(), []string{"--token", testToken}, environment("", testToken), strings.NewReader(""), &stdout, &stderr) == 0 || strings.Contains(stderr.String(), testToken) {
		t.Fatal("CLI credentials accepted or echoed")
	}
	for _, args := range [][]string{{"version"}, {"version", "--json"}} {
		stdout.Reset()
		stderr.Reset()
		if run(context.Background(), args, environment("", ""), strings.NewReader(""), &stdout, &stderr) != 0 || !strings.Contains(stdout.String(), buildinfo.Current().Version) || stderr.Len() != 0 {
			t.Fatal("version requires configuration or failed")
		}
	}
}

func TestProtocolNegotiationAndErrors(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected HTTP request") })
	for _, version := range []string{"2025-06-18", "2025-03-26", "2024-11-05", "2099-01-01"} {
		s := session{client: client}
		r := s.handle(context.Background(), []byte(wire("initialize", 1, map[string]string{"protocolVersion": version})))
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		want := version
		if version == "2099-01-01" {
			want = preferredVersion
		}
		if r.Result.(map[string]any)["protocolVersion"] != want {
			t.Fatal("version negotiation mismatch")
		}
	}
	cases := []struct {
		line        string
		initialized bool
		code        int
	}{
		{wire("tools/list", 1, nil), false, -32002},
		{wire("unknown", 1, nil), false, -32002},
		{wire("unknown", 1, nil), true, -32601},
		{"{", false, -32700},
		{"[]", false, -32600}, {"[{}]", false, -32600}, {"null", false, -32600}, {"42", false, -32600}, {`"string"`, false, -32600},
		{`{"jsonrpc":"1.0","id":1,"method":"ping"}`, false, -32600},
		{`{"jsonrpc":"2.0","id":{},"method":"ping"}`, false, -32600},
		{`{"jsonrpc":"2.0","id":true,"method":"ping"}`, false, -32600},
		{`{"jsonrpc":"2.0","id":1}`, false, -32600},
		{wire("initialize", 1, map[string]int{"protocolVersion": 1}), false, -32602},
		{wire("initialize", 1, nil), false, -32602},
		{wire("tools/list", 1, []any{}), true, -32602},
		{wire("tools/list", 1, map[string]string{"cursor": "x"}), true, -32602},
		{wire("tools/call", 1, map[string]any{"name": "list_groups", "extra": 1}), true, -32602},
		{wire("tools/call", 1, map[string]any{"name": 1}), true, -32602},
		{wire("tools/call", 1, []any{}), true, -32602},
	}
	for _, tc := range cases {
		s := session{initialized: tc.initialized, client: client}
		r := s.handle(context.Background(), []byte(tc.line))
		if r == nil || r.Error == nil || r.Error.Code != tc.code {
			t.Errorf("%s: response=%+v want %d", tc.line, r, tc.code)
		} else if tc.code == -32700 && string(r.ID) != "null" {
			t.Error("parse error ID must be null")
		}
	}
	for _, initialized := range []bool{false, true} {
		s := session{initialized: initialized, client: client}
		if r := s.handle(context.Background(), []byte(wire("ping", 1, nil))); r == nil || r.Error != nil {
			t.Fatal("ping unavailable")
		}
		for _, method := range []string{"notifications/initialized", "custom/notification", "ping", "tools/call"} {
			if r := s.handle(context.Background(), []byte(wire(method, nil, nil))); r != nil {
				t.Error("notification produced response")
			}
		}
	}
}

func TestMalformedAndOversizedLinesResume(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected HTTP request") })
	// One oversized line, many reader fragments, and a valid request after it.
	// The oversized line is valid JSON, so only the length limit can reject it.
	oversized := `{"jsonrpc":"2.0","id":8,"method":"ping","params":{"pad":"` + strings.Repeat("x", maxLineBytes*3) + `"}}`
	input := "invalid\n[]\n" + oversized + "\n" + wire("ping", 9, nil)
	var stdout bytes.Buffer
	if err := serve(context.Background(), strings.NewReader(input), &stdout, client); err != nil {
		t.Fatal(err)
	}
	lines := decodeLines(t, stdout.String())
	if len(lines) != 4 {
		t.Fatalf("got %d responses", len(lines))
	}
	for i, want := range []int{-32700, -32600, -32700} {
		var e rpcError
		json.Unmarshal(lines[i]["error"], &e)
		if e.Code != want || string(lines[i]["id"]) != "null" {
			t.Fatalf("error response = %s", lines[i]["error"])
		}
	}
	if string(lines[3]["id"]) != "9" || lines[3]["result"] == nil {
		t.Fatal("did not resume after oversized line")
	}
	// Exact limit is accepted, including an EOF without a trailing newline.
	request := strings.TrimSuffix(wire("ping", 1, nil), "\n")
	stdout.Reset()
	if err := serve(context.Background(), strings.NewReader(request+strings.Repeat(" ", maxLineBytes-len(request))), &stdout, client); err != nil {
		t.Fatal(err)
	}
	if lines := decodeLines(t, stdout.String()); len(lines) != 1 || lines[0]["error"] != nil {
		t.Fatal("exact line limit rejected")
	}
	stdout.Reset()
	if err := serve(context.Background(), bytes.NewReader([]byte{'"', 0xff, '"', '\n'}), &stdout, client); err != nil {
		t.Fatal(err)
	}
	var e rpcError
	json.Unmarshal(decodeLines(t, stdout.String())[0]["error"], &e)
	if e.Code != -32700 {
		t.Fatal("invalid UTF-8 accepted")
	}
}

// Run the actual main in an isolated test process to prove exit status and
// signal handling while stdin remains open, without a separate go build.
func TestMCPProcessHelper(t *testing.T) {
	if os.Getenv("ORBITBEACON_MCP_TEST_PROCESS") != "1" {
		return
	}
	os.Args = []string{"orbitbeacon-mcp"}
	main()
}
func TestProcessEOFAndSignals(t *testing.T) {
	for _, signal := range []os.Signal{nil, syscall.SIGTERM, os.Interrupt} {
		t.Run(fmt.Sprint(signal), func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestMCPProcessHelper$")
			// Avoid duplicate inherited credentials or helper flags.
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "ORBITBEACON_") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "ORBITBEACON_MCP_TEST_PROCESS=1", "ORBITBEACON_URL=https://example.com", "ORBITBEACON_TOKEN="+testToken)
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			io.WriteString(input, wire("ping", 1, nil))
			line, err := bufio.NewReader(output).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if len(decodeLines(t, line)) != 1 {
				t.Fatal("invalid process stdout")
			}
			if signal == nil {
				input.Close()
			} else if err := cmd.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatalf("process exit: %v; stderr: %s", err, stderr.String())
			}
			input.Close()
			if strings.Contains(line+stderr.String(), testToken) {
				t.Fatal("process leaked token")
			}
		})
	}
}

func TestProcessConfigurationFailureExitStatus(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestMCPProcessHelper$")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "ORBITBEACON_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "ORBITBEACON_MCP_TEST_PROCESS=1", "ORBITBEACON_URL=https://example.com/"+testToken, "ORBITBEACON_TOKEN="+testToken)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("config failure exit: %v, stdout=%q, stderr=%q", err, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), testToken) {
		t.Fatal("configuration error leaked credential")
	}
}

func TestHistoryDiskArgumentEncoding(t *testing.T) {
	for _, tool := range toolTable() {
		if tool.Name != "get_node_history" {
			continue
		}
		if _, ok := tool.InputSchema.Properties["disk"]; !ok {
			t.Fatal("missing optional disk property")
		}
		if slices.Contains(tool.InputSchema.Required, "disk") {
			t.Fatal("disk must be optional")
		}
		path, ok := tool.requestTarget(json.RawMessage(`{"id":"` + testID + `","metric":"inode_used_percent","range":"1h","disk":"/data:a?b&c"}`))
		if !ok {
			t.Fatal("disk argument rejected")
		}
		parsed, err := url.Parse(path)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Query().Get("disk") != "/data:a?b&c" || parsed.Query().Get("metric") != "inode_used_percent" {
			t.Fatalf("path=%s", path)
		}
		return
	}
	t.Fatal("missing history tool")
}

func TestHostHistoryMetricsRequestTargets(t *testing.T) {
	for _, tool := range toolTable() {
		if tool.Name != "get_node_history" {
			continue
		}
		for _, metric := range []string{"disk_read_bytes_per_second", "disk_write_bytes_per_second", "disk_busy_percent", "pressure_cpu_percent", "pressure_memory_percent", "pressure_io_percent", "temperature_celsius"} {
			raw, _ := json.Marshal(map[string]string{"id": testID, "metric": metric, "range": "1h"})
			path, ok := tool.requestTarget(raw)
			if !ok {
				t.Fatal(metric)
			}
			parsed, err := url.Parse(path)
			if err != nil || parsed.Query().Get("metric") != metric {
				t.Fatal(path, err)
			}
		}
	}
}

func TestRateLimitedToolDoesNotRetry(t *testing.T) {
	var calls atomic.Int64
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		io.WriteString(w, `{"code":"rate_limited"}`)
	})
	got := call(t, client, "list_groups", map[string]any{})
	if got.Error != nil {
		t.Fatal(got.Error)
	}
	value := got.Result.(toolResult)
	if !value.IsError || value.Content[0].Text != "HTTP 429 rate_limited" || calls.Load() != 1 {
		t.Fatalf("rate limited result=%+v calls=%d", value, calls.Load())
	}
}
