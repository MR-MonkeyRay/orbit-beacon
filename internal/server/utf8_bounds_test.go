package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestUTF8ByteBoundsAllowShortUnicodePasswordsAndRejectByteOverflow(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	username := strings.Repeat("界", 42) + "aa" // 128 bytes, not 128 characters.
	const unicodePassword = "天地玄黄"
	for _, input := range []struct {
		username string
		password string
		status   int
	}{
		{username, unicodePassword, http.StatusCreated},
		{strings.Repeat("界", 43), unicodePassword, http.StatusBadRequest},
		{"short-unicode-password", "天地", http.StatusBadRequest},
		{"maximum-unicode-password", strings.Repeat("😀", 256), http.StatusCreated},
		{"overflow-unicode-password", strings.Repeat("😀", 257), http.StatusBadRequest},
	} {
		body, err := json.Marshal(map[string]string{"username": input.username, "password": input.password, "role": "viewer"})
		if err != nil {
			t.Fatal(err)
		}
		response := request(t, f.handler, http.MethodPost, "/api/v1/users", string(body), admin, csrf, publicURL)
		assertHTTPStatus(t, response, input.status)
	}
	body, err := json.Marshal(map[string]string{"username": username, "password": unicodePassword})
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, request(t, f.handler, http.MethodPost, "/api/v1/session", string(body), nil, "", publicURL), http.StatusOK)
}

func TestUTF8AssetAndTagByteBoundariesDoNotPartiallyChangeMetadata(t *testing.T) {
	f := newHTTP(t, 5)
	admin, csrf := login(t, f, "admin")
	boundary, overflow := strings.Repeat("界", 85), strings.Repeat("界", 86)
	for _, path := range []string{"/api/v1/groups", "/api/v1/nodes"} {
		valid, _ := json.Marshal(map[string]string{"name": boundary})
		assertHTTPStatus(t, request(t, f.handler, http.MethodPost, path, string(valid), admin, csrf, publicURL), http.StatusCreated)
		invalid, _ := json.Marshal(map[string]string{"name": overflow})
		apiError(t, request(t, f.handler, http.MethodPost, path, string(invalid), admin, csrf, publicURL), http.StatusBadRequest, "validation_failed")
	}
	inventory := request(t, f.handler, http.MethodGet, "/api/v1/nodes", "", admin, "", "")
	var listed struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(inventory.Body.Bytes(), &listed); err != nil || len(listed.Nodes) != 1 {
		t.Fatalf("overflow created an asset: %s err=%v", inventory.Body, err)
	}
	path := "/api/v1/nodes/" + listed.Nodes[0].ID
	valid, _ := json.Marshal(map[string]any{"tags": []string{boundary}})
	assertHTTPStatus(t, request(t, f.handler, http.MethodPatch, path, string(valid), admin, csrf, publicURL), http.StatusOK)
	invalid, _ := json.Marshal(map[string]any{"name": "must-not-change", "tags": []string{overflow}})
	apiError(t, request(t, f.handler, http.MethodPatch, path, string(invalid), admin, csrf, publicURL), http.StatusBadRequest, "validation_failed")
	current := request(t, f.handler, http.MethodGet, path, "", admin, "", "")
	var node struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(current.Body.Bytes(), &node); err != nil || node.Name != boundary || len(node.Tags) != 1 || node.Tags[0] != boundary {
		t.Fatalf("byte overflow changed metadata: %+v err=%v", node, err)
	}
}
