package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func TestStaticAssetVariantsAndCaching(t *testing.T) {
	html := []byte("<!doctype html><script>theme()</script><main>" + strings.Repeat("orbit beacon ", 100) + "</main>")
	javascript := []byte(strings.Repeat("export const beacon = 'orbit';\n", 100))
	assets := fstest.MapFS{
		"200.html":                       {Data: html},
		"public.html":                    {Data: html},
		"_app/assets/chunks/app.test.js": {Data: javascript},
	}
	policies, err := htmlPolicies(assets, "https://beacon.test")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareStaticAssets(assets)
	if err != nil {
		t.Fatal(err)
	}
	h := &handler{staticAssets: prepared, htmlPolicies: policies}
	target := "/_app/assets/chunks/app.test.js?v=" + testAssetVersion(javascript)

	plain := serveStatic(t, h, http.MethodGet, target, "", "")
	if plain.Code != http.StatusOK || !bytes.Equal(plain.Body.Bytes(), javascript) {
		t.Fatalf("plain response: status %d body length %d", plain.Code, plain.Body.Len())
	}
	if got := plain.Header().Get("Content-Length"); got != strconv.Itoa(len(javascript)) {
		t.Errorf("plain Content-Length = %q", got)
	}
	if got := plain.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q", got)
	}
	if !strings.Contains(plain.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("Cache-Control = %q", plain.Header().Get("Cache-Control"))
	}
	plainETag := plain.Header().Get("ETag")
	if len(plainETag) < 3 || plainETag[0] != '"' || strings.HasPrefix(plainETag, "W/") {
		t.Fatalf("ETag is not strong: %q", plainETag)
	}

	compressed := serveStatic(t, h, http.MethodGet, target, "gzip", "")
	if compressed.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", compressed.Header().Get("Content-Encoding"))
	}
	if compressed.Header().Get("ETag") == plainETag {
		t.Fatal("compressed and identity representations share an ETag")
	}
	if got := compressed.Header().Get("Content-Length"); got != strconv.Itoa(compressed.Body.Len()) {
		t.Errorf("gzip Content-Length = %q, body length %d", got, compressed.Body.Len())
	}
	reader, err := gzip.NewReader(compressed.Body)
	if err != nil {
		t.Fatal(err)
	}
	uncompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(uncompressed, javascript) {
		t.Fatal("gzip body does not contain the original asset")
	}

	notModified := serveStatic(t, h, http.MethodGet, target, "gzip", compressed.Header().Get("ETag"))
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
		t.Fatalf("conditional response: status %d body length %d", notModified.Code, notModified.Body.Len())
	}
	if notModified.Header().Get("Content-Length") != "" {
		t.Errorf("304 Content-Length = %q", notModified.Header().Get("Content-Length"))
	}

	head := serveStatic(t, h, http.MethodHead, target, "gzip", "")
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD response: status %d body length %d", head.Code, head.Body.Len())
	}
	if head.Header().Get("Content-Length") != compressed.Header().Get("Content-Length") || head.Header().Get("ETag") != compressed.Header().Get("ETag") {
		t.Errorf("HEAD representation headers differ from GET: HEAD %v GET %v", head.Header(), compressed.Header())
	}
	for _, response := range []*httptest.ResponseRecorder{plain, compressed, notModified, head} {
		if response.Header().Get("Access-Control-Allow-Origin") != "*" || response.Header().Get("Cross-Origin-Resource-Policy") != "cross-origin" || response.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("unsafe static CORS headers: %v", response.Header())
		}
	}
}

func TestStaticHTMLPreservesSecurityHeadersForVariants(t *testing.T) {
	html := []byte("<!doctype html><script>theme()</script><main>" + strings.Repeat("public page ", 100) + "</main>")
	assets := fstest.MapFS{"200.html": {Data: html}, "public.html": {Data: html}}
	policies, err := htmlPolicies(assets, "https://beacon.test")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareStaticAssets(assets)
	if err != nil {
		t.Fatal(err)
	}
	h := &handler{staticAssets: prepared, htmlPolicies: policies}

	for _, test := range []struct {
		name   string
		target string
		policy string
	}{
		{"shell route", "/overview", policies["200.html"]},
		{"direct HTML", "/200.html", policies["200.html"]},
	} {
		t.Run(test.name, func(t *testing.T) {
			plain := serveStatic(t, h, http.MethodGet, test.target, "", "")
			compressed := serveStatic(t, h, http.MethodGet, test.target, "gzip", "")
			notModified := serveStatic(t, h, http.MethodGet, test.target, "gzip", compressed.Header().Get("ETag"))
			head := serveStatic(t, h, http.MethodHead, test.target, "gzip", "")
			for _, response := range []*httptest.ResponseRecorder{plain, compressed, notModified, head} {
				if response.Header().Get("Cache-Control") != "no-store" {
					t.Errorf("Cache-Control = %q", response.Header().Get("Cache-Control"))
				}
				if response.Header().Get("Access-Control-Allow-Origin") != "" || response.Header().Get("Cross-Origin-Resource-Policy") != "" {
					t.Errorf("HTML received static CORS headers: %v", response.Header())
				}
				if response.Header().Get("Content-Security-Policy") != test.policy {
					t.Errorf("CSP = %q", response.Header().Get("Content-Security-Policy"))
				}
				if response.Header().Get("X-Frame-Options") != "DENY" {
					t.Errorf("X-Frame-Options = %q", response.Header().Get("X-Frame-Options"))
				}
				if response.Header().Get("Vary") != "Accept-Encoding" {
					t.Errorf("Vary = %q", response.Header().Get("Vary"))
				}
			}
			if compressed.Header().Get("Content-Encoding") != "gzip" {
				t.Errorf("Content-Encoding = %q", compressed.Header().Get("Content-Encoding"))
			}
			if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
				t.Errorf("conditional response: status %d body length %d", notModified.Code, notModified.Body.Len())
			}
			if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != compressed.Header().Get("Content-Length") {
				t.Errorf("HEAD response: status %d body length %d content length %q", head.Code, head.Body.Len(), head.Header().Get("Content-Length"))
			}
		})
	}
}

func TestAcceptsGzipHonorsExplicitExclusion(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{
		{"gzip", true},
		{"br, gzip;q=0.5", true},
		{"*", true},
		{"gzip;q=0, *;q=1", false},
		{"gzip;q=1.1", false},
		{"gzip;q=invalid", false},
		{"br", false},
	} {
		if got := acceptsGzip(test.value); got != test.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", test.value, got, test.want)
		}
	}
}

func TestStaticAssetVersionAndCORSScope(t *testing.T) {
	assets := fstest.MapFS{
		"200.html":           {Data: []byte("console")},
		"public.html":        {Data: []byte("public")},
		"_app/assets/app.js": {Data: []byte("export{}")},
	}
	prepared, err := prepareStaticAssets(assets)
	if err != nil {
		t.Fatal(err)
	}
	h := &handler{staticAssets: prepared}
	version := testAssetVersion(assets["_app/assets/app.js"].Data)
	for _, test := range []struct {
		target string
		status int
		cache  string
		cors   bool
	}{
		{"/_app/assets/app.js", 200, "no-cache", true},
		{"/_app/assets/app.js?v=" + version, 200, "public, max-age=31536000, immutable", true},
		{"/_app/assets/app.js?v=00000000000000000000000000000000", 404, "no-store", false},
		{"/_app/assets/app.js?v=bad", 404, "no-store", false},
		{"/_app/assets/app.js?v=", 404, "no-store", false},
		{"/_app/assets/app.js?v=" + version + "&v=" + version, 404, "no-store", false},
		{"/_app/assets/app.js?v=" + version + "%zz", 404, "no-store", false},
		{"/_app/assets/missing.js?v=" + version, 404, "no-store", false},
		{"/asset-manifest.json", 404, "no-store", false},
		{"/overview", 200, "no-store", false},
		{"/api/v1/missing", 404, "no-store", false},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+test.target, func(t *testing.T) {
				response := httptest.NewRecorder()
				h.ServeHTTP(response, httptest.NewRequest(method, "https://beacon.test"+test.target, nil))
				if response.Code != test.status || response.Header().Get("Cache-Control") != test.cache {
					t.Fatalf("status/cache = %d %q", response.Code, response.Header().Get("Cache-Control"))
				}
				if (response.Header().Get("Access-Control-Allow-Origin") == "*") != test.cors || (response.Header().Get("Cross-Origin-Resource-Policy") == "cross-origin") != test.cors || response.Header().Get("Access-Control-Allow-Credentials") != "" {
					t.Fatalf("CORS scope: %v", response.Header())
				}
				if method == http.MethodHead && response.Body.Len() != 0 {
					t.Fatal("HEAD returned a body")
				}
			})
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodOptions} {
		response := serveStatic(t, h, method, "/_app/assets/app.js?v="+version, "", "")
		if response.Code != 405 || response.Header().Get("Allow") != "GET, HEAD" || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("method response: %d %v", response.Code, response.Header())
		}
	}
}

func TestAssetManifestValidationAndCDNPolicy(t *testing.T) {
	javascript := []byte("export{}")
	version := testAssetVersion(javascript)
	manifest := func(base string, versions map[string]string) []byte {
		data, err := json.Marshal(map[string]any{"asset_base": base, "versions": versions})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	assets := fstest.MapFS{
		"200.html":            {Data: []byte("<script>theme()</script>")},
		"public.html":         {Data: []byte("<script>theme()</script>")},
		"_app/assets/app.js":  {Data: javascript},
		"asset-manifest.json": {Data: manifest("https://cdn.example.test/releases/42", map[string]string{"_app/assets/app.js": version})},
	}
	prepared, err := prepareStaticAssets(assets)
	if err != nil {
		t.Fatal(err)
	}
	if _, exposed := prepared["asset-manifest.json"]; exposed {
		t.Fatal("manifest is publicly served")
	}
	policies, err := htmlPolicies(assets, "https://beacon.test")
	if err != nil {
		t.Fatal(err)
	}
	for _, directive := range strings.Split(policies["200.html"], ";") {
		fields := strings.Fields(directive)
		if len(fields) == 0 {
			continue
		}
		cdnAllowed := fields[0] == "script-src" || fields[0] == "style-src" || fields[0] == "img-src" || fields[0] == "font-src"
		if strings.Contains(directive, "https://cdn.example.test") != cdnAllowed {
			t.Errorf("CDN scope: %s", directive)
		}
	}
	if !strings.Contains(policies["200.html"], "'sha256-") || !strings.Contains(policies["200.html"], "connect-src 'self' wss://beacon.test;") {
		t.Fatal(policies)
	}
	for _, base := range []string{
		"http://cdn.example.test", "//cdn.example.test", "https://user:pass@cdn.example.test", "https://cdn.example.test?q=x", "https://cdn.example.test?", "https://cdn.example.test#x", "https://cdn.example.test/../x", "https://cdn.example.test//x", "https://cdn.example.test/%2e%2e/x", "https://cdn.example.test/a\\b", "https://cdn.example.test/;connect-src https://evil.test", "https://cdn.example.test/\nscript-src",
		"https://cdn.example.test/a`b", "https://cdn.example.test/a\u00a0b", "https://cdn.example.test/a\u0085b", "https://cdn.example.test/a\u0001b",
	} {
		t.Run(base, func(t *testing.T) {
			assets["asset-manifest.json"].Data = manifest(base, map[string]string{"_app/assets/app.js": version})
			if _, err := htmlPolicies(assets, "https://beacon.test"); err == nil {
				t.Fatal("accepted invalid CDN base")
			}
		})
	}
	for _, data := range [][]byte{
		[]byte(`null`), []byte(`{}`), []byte(`{"asset_base":"","versions":null}`), []byte(`{"asset_base":null,"versions":{}}`), []byte(`{"asset_base":"","versions":{}} trailing`), []byte(`{"asset_base":"","versions":{},"unknown":true}`),
		manifest("", map[string]string{}), manifest("", map[string]string{"_app/assets/missing.js": version}), manifest("", map[string]string{"../app.js": version}), manifest("", map[string]string{"200.html": version}), manifest("", map[string]string{"_app/assets/app.js": strings.Repeat("0", 32)}), manifest("", map[string]string{"_app/assets/app.js": "bad"}),
	} {
		assets["asset-manifest.json"].Data = data
		if _, err := prepareStaticAssets(assets); err == nil {
			t.Errorf("accepted invalid manifest: %s", data)
		}
	}
}

func testAssetVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

func serveStatic(t *testing.T, h *handler, method, target, acceptEncoding, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "https://beacon.test"+target, nil)
	request.Header.Set("Accept-Encoding", acceptEncoding)
	request.Header.Set("If-None-Match", ifNoneMatch)
	response := httptest.NewRecorder()
	h.static(response, request)
	return response
}
