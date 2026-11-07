package server_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"orbitbeacon/internal/server"
)

func TestPublicEmbeddedAssetsAndRequiredShell(t *testing.T) {
	f := newHTTP(t, 2)
	h, err := server.NewHandler(testHandlerOptions(t, server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL}))
	if err != nil {
		t.Fatal(err)
	}
	// shell 文件引用从 web/ 构建的内容哈希包；跟随这些引用，
	// 而不是硬编码每次构建都会变化的名称。
	assets := []struct{ path, mime string }{{"/", "text/html"}, {"/overview", "text/html"}}
	for _, shell := range []string{"/", "/overview"} {
		body := request(t, h, http.MethodGet, shell, "", nil, "", "").Body.String()
		for _, match := range regexp.MustCompile(`(?:src|href)="(/_app/immutable/[^"]+)"`).FindAllStringSubmatch(body, -1) {
			mime := "text/css"
			if strings.HasSuffix(match[1], ".js") {
				mime = "javascript"
			}
			assets = append(assets, struct{ path, mime string }{match[1], mime})
		}
	}
	if len(assets) != 6 {
		t.Fatalf("expected a script and stylesheet per shell, got %v", assets)
	}
	for _, asset := range assets {
		response := request(t, h, http.MethodGet, asset.path, "", nil, "", "")
		if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), asset.mime) {
			t.Fatalf("asset %s status/type: %d %s", asset.path, response.Code, response.Header().Get("Content-Type"))
		}
		if strings.HasPrefix(asset.path, "/_app/immutable/") && !strings.Contains(response.Header().Get("Cache-Control"), "immutable") {
			t.Fatalf("asset %s cache-control: %q", asset.path, response.Header().Get("Cache-Control"))
		}
		head := request(t, h, http.MethodHead, asset.path, "", nil, "", "")
		if head.Code != http.StatusOK || head.Body.Len() != 0 {
			t.Fatalf("HEAD %s: %d %d", asset.path, head.Code, head.Body.Len())
		}
	}
	if _, err := server.NewHandler(testHandlerOptions(t, server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL, Assets: fstest.MapFS{"200.html": &fstest.MapFile{Data: []byte("admin")}}})); err == nil || !strings.Contains(err.Error(), "public") {
		t.Fatalf("missing public shell: %v", err)
	}
	assertHTTPStatus(t, request(t, h, http.MethodGet, "/_app/public-missing.js", "", nil, "", ""), http.StatusNotFound)
	if err := h.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
