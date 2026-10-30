package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
)

var openAPIMethodNames = map[string]bool{
	"get": true, "post": true, "patch": true, "delete": true, "put": true, "options": true, "head": true, "trace": true,
}

var openAPIPathItemFields = map[string]bool{
	"$ref": true, "summary": true, "description": true, "servers": true, "parameters": true,
}

func openAPIPathOperations(t *testing.T) map[string][]string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "../../api/openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return parseOpenAPIPathOperations(t, data)
}

func parseOpenAPIPathOperations(t *testing.T, data []byte) map[string][]string {
	t.Helper()
	operations, err := parseOpenAPIPathOperationsData(data)
	if err != nil {
		t.Fatal(err)
	}
	return operations
}

func parseOpenAPIPathOperationsData(data []byte) (map[string][]string, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("parse OpenAPI YAML: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("OpenAPI YAML contains more than one document")
		}
		return nil, fmt.Errorf("read OpenAPI YAML documents: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("OpenAPI document is not a mapping")
	}
	root, err := uniqueMapping(document.Content[0], "OpenAPI document")
	if err != nil {
		return nil, err
	}
	paths, ok := root["paths"]
	if !ok || paths.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("OpenAPI paths is not a mapping")
	}
	pathEntries, err := uniqueMapping(paths, "OpenAPI paths")
	if err != nil {
		return nil, err
	}
	operations := make(map[string][]string)
	for path, item := range pathEntries {
		if strings.HasPrefix(path, "x-") {
			continue
		}
		if !strings.HasPrefix(path, "/") || item.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("invalid OpenAPI path item %q", path)
		}
		fields, err := uniqueMapping(item, "OpenAPI path item "+path)
		if err != nil {
			return nil, err
		}
		for name, value := range fields {
			if openAPIMethodNames[name] {
				if value.Kind != yaml.MappingNode {
					return nil, fmt.Errorf("OpenAPI operation %s %s is not a mapping", name, path)
				}
				operations[path] = append(operations[path], strings.ToUpper(name))
				continue
			}
			if !openAPIPathItemFields[name] && !strings.HasPrefix(name, "x-") {
				return nil, fmt.Errorf("unsupported OpenAPI path-item field %q on %s", name, path)
			}
		}
		if len(operations[path]) == 0 {
			return nil, fmt.Errorf("OpenAPI path %s has no checkable operation", path)
		}
		slices.Sort(operations[path])
	}
	if len(operations) == 0 {
		return nil, fmt.Errorf("OpenAPI paths is empty")
	}
	return operations, nil
}

func uniqueMapping(node *yaml.Node, context string) (map[string]*yaml.Node, error) {
	if node.Kind != yaml.MappingNode || len(node.Content)%2 != 0 {
		return nil, fmt.Errorf("%s is not a valid mapping", context)
	}
	values := make(map[string]*yaml.Node, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("%s contains a non-scalar key", context)
		}
		if _, exists := values[key.Value]; exists {
			return nil, fmt.Errorf("%s contains duplicate key %q", context, key.Value)
		}
		values[key.Value] = node.Content[index+1]
	}
	return values, nil
}

func concreteOpenAPIPath(path string) string {
	return strings.ReplaceAll(path, "{id}", "00000000-0000-4000-8000-000000000001")
}

func TestOpenAPIOperationsMatchRouteTable(t *testing.T) {
	for path, expected := range openAPIPathOperations(t) {
		var actual []string
		for _, entry := range routes {
			method, pattern, _ := strings.Cut(entry.pattern, " ")
			if pattern == path {
				actual = append(actual, method)
			}
		}
		slices.Sort(actual)
		if !slices.Equal(actual, expected) {
			t.Errorf("%s OpenAPI methods=%v, route methods=%v", path, expected, actual)
		}
	}
}

func TestOpenAPIVersionMatchesReleaseVersion(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(filename), "../..")
	data, err := os.ReadFile(filepath.Join(root, "api/openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Info struct{ Version string } `yaml:"info"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	release, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSpace(string(release)); document.Info.Version != want {
		t.Fatalf("OpenAPI info.version = %q, want VERSION %q", document.Info.Version, want)
	}
}

func TestOpenAPIParserHandlesQuotedPathsCommentsFlowAndExtensions(t *testing.T) {
	operations := parseOpenAPIPathOperations(t, []byte(`
openapi: 3.1.0
paths:
  x-owner: {value: test}
  "/api/v2/nodes": # quoted path with a comment
    get: {responses: {200: {description: ok}}}
    x-owner: {value: test}
  /api/v2/groups:
    post:
      responses:
        "201":
          description: created
`))
	if !slices.Equal(operations["/api/v2/nodes"], []string{"GET"}) || !slices.Equal(operations["/api/v2/groups"], []string{"POST"}) {
		t.Fatalf("parsed operations=%v", operations)
	}
}

func TestOpenAPIParserRejectsDuplicateKeysAndDocuments(t *testing.T) {
	for name, data := range map[string][]byte{
		"duplicate root paths": []byte("paths: {}\npaths: {}\n"),
		"duplicate path":       []byte("paths:\n  /api/v2/nodes:\n    get: {}\n  /api/v2/nodes:\n    post: {}\n"),
		"duplicate operation":  []byte("paths:\n  /api/v2/nodes:\n    get: {}\n    get: {}\n"),
		"multiple documents":   []byte("paths:\n  /api/v2/nodes:\n    get: {}\n---\npaths:\n  /api/v2/other:\n    get: {}\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseOpenAPIPathOperationsData(data); err == nil {
				t.Fatal("accepted invalid OpenAPI YAML")
			}
		})
	}
}

func openAPITestHandler(t *testing.T) *handler {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	handler, err := NewHandler(testHandlerOptions(t, Options{
		Auth:      auth.NewService(db, auth.Options{}),
		Nodes:     nodes.NewService(db, nodes.Options{}),
		PublicURL: "https://beacon.example.test",
		Assets:    fstest.MapFS{"200.html": &fstest.MapFile{Data: []byte("shell")}, "public.html": &fstest.MapFile{Data: []byte("public shell")}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Shutdown(context.Background()) })
	return handler
}

func TestOpenAPIOperationsDoNotDriftToNotFoundOrMethodNotAllowed(t *testing.T) {
	operations := openAPIPathOperations(t)
	paths := make([]string, 0, len(operations))
	for path := range operations {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		for _, method := range operations[path] {
			t.Run(method+" "+path, func(t *testing.T) {
				handler := openAPITestHandler(t)
				request := httptest.NewRequest(method, "https://beacon.example.test"+concreteOpenAPIPath(path), strings.NewReader(`{}`))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code == http.StatusNotFound || response.Code == http.StatusMethodNotAllowed {
					t.Fatalf("documented operation drifted to route status %d", response.Code)
				}
			})
		}
	}
}
