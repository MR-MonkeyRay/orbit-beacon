package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
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
	// 跟随构建生成的稳定资源名及版本查询，不把构建细节写入测试。
	assets := []struct{ path, mime string }{{"/", "text/html"}, {"/overview", "text/html"}}
	for _, shell := range []string{"/", "/overview"} {
		body := request(t, h, http.MethodGet, shell, "", nil, "", "").Body.String()
		for _, match := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(body, -1) {
			u, err := url.Parse(match[1])
			if err != nil {
				t.Fatal(err)
			}
			assetStart := strings.Index(u.Path, "/_app/assets/")
			if assetStart < 0 || (!strings.HasSuffix(u.Path, ".css") && !strings.HasSuffix(u.Path, ".js")) {
				continue
			}
			mime := "text/css"
			if strings.HasSuffix(u.Path, ".js") {
				mime = "javascript"
			}
			u.Path, u.RawPath, u.Scheme, u.Host = u.Path[assetStart:], "", "", ""
			assets = append(assets, struct{ path, mime string }{u.RequestURI(), mime})
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
		if strings.HasPrefix(asset.path, "/_app/assets/") && !strings.Contains(response.Header().Get("Cache-Control"), "immutable") {
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

func TestHandlerRejectsInvalidAssetManifestAtStartup(t *testing.T) {
	f := newHTTP(t, 2)
	javascript := []byte("export{}")
	digest := sha256.Sum256(javascript)
	version := hex.EncodeToString(digest[:16])
	for _, test := range []struct {
		name    string
		base    string
		version string
	}{
		{"HTTP CDN", "http://cdn.example.test", version},
		{"CSP injection", "https://cdn.example.test/;connect-src https://evil.test", version},
		{"content mismatch", "https://cdn.example.test/releases/42", strings.Repeat("0", 32)},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest, err := json.Marshal(map[string]any{"asset_base": test.base, "versions": map[string]string{"_app/assets/app.js": test.version}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = server.NewHandler(testHandlerOptions(t, server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL, Assets: fstest.MapFS{
				"200.html":            {Data: []byte("console")},
				"public.html":         {Data: []byte("public")},
				"_app/assets/app.js":  {Data: javascript},
				"asset-manifest.json": {Data: manifest},
			}}))
			if err == nil || !strings.Contains(err.Error(), "UI assets") {
				t.Fatalf("startup accepted invalid manifest: %v", err)
			}
		})
	}
}

func TestHandlerCDNPolicyAndAssetExposure(t *testing.T) {
	f := newHTTP(t, 2)
	javascript := []byte("export{}")
	digest := sha256.Sum256(javascript)
	version := hex.EncodeToString(digest[:16])
	manifest, err := json.Marshal(map[string]any{"asset_base": "https://cdn.example.test/releases/42/", "versions": map[string]string{"_app/assets/app.js": version}})
	if err != nil {
		t.Fatal(err)
	}
	assets := fstest.MapFS{
		"200.html":            {Data: []byte("<script>theme()</script>")},
		"public.html":         {Data: []byte("<script>theme()</script>")},
		"_app/assets/app.js":  {Data: javascript},
		"asset-manifest.json": {Data: manifest},
	}
	h, err := server.NewHandler(testHandlerOptions(t, server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL, Assets: assets}))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/", "/overview"} {
		response := request(t, h, http.MethodGet, target, "", nil, "", "")
		policy := response.Header().Get("Content-Security-Policy")
		if response.Code != 200 || !strings.Contains(policy, "https://cdn.example.test") || strings.Contains(policy, "/releases/42") || !strings.Contains(policy, "connect-src 'self' wss://beacon.example.test;") {
			t.Fatalf("CDN policy: %d %q", response.Code, policy)
		}
	}
	response := request(t, h, http.MethodGet, "/_app/assets/app.js?v="+version, "", nil, "", "")
	if response.Code != 200 || response.Header().Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(response.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("CDN asset: %d %v", response.Code, response.Header())
	}
	response = request(t, h, http.MethodGet, "/asset-manifest.json", "", nil, "", "")
	if response.Code != 404 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("manifest exposed: %d %v", response.Code, response.Header())
	}
}
