package server

import (
	"bytes"
	"compress/gzip"
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
		"200.html":                          {Data: html},
		"public.html":                       {Data: html},
		"_app/immutable/chunks/app.test.js": {Data: javascript},
	}
	policies, err := htmlPolicies(assets)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareStaticAssets(assets)
	if err != nil {
		t.Fatal(err)
	}
	h := &handler{staticAssets: prepared, htmlPolicies: policies}

	plain := serveStatic(t, h, http.MethodGet, "/_app/immutable/chunks/app.test.js", "", "")
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

	compressed := serveStatic(t, h, http.MethodGet, "/_app/immutable/chunks/app.test.js", "gzip", "")
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

	notModified := serveStatic(t, h, http.MethodGet, "/_app/immutable/chunks/app.test.js", "gzip", compressed.Header().Get("ETag"))
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
		t.Fatalf("conditional response: status %d body length %d", notModified.Code, notModified.Body.Len())
	}
	if notModified.Header().Get("Content-Length") != "" {
		t.Errorf("304 Content-Length = %q", notModified.Header().Get("Content-Length"))
	}

	head := serveStatic(t, h, http.MethodHead, "/_app/immutable/chunks/app.test.js", "gzip", "")
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD response: status %d body length %d", head.Code, head.Body.Len())
	}
	if head.Header().Get("Content-Length") != compressed.Header().Get("Content-Length") || head.Header().Get("ETag") != compressed.Header().Get("ETag") {
		t.Errorf("HEAD representation headers differ from GET: HEAD %v GET %v", head.Header(), compressed.Header())
	}
}

func TestStaticHTMLPreservesSecurityHeadersForVariants(t *testing.T) {
	html := []byte("<!doctype html><script>theme()</script><main>" + strings.Repeat("public page ", 100) + "</main>")
	assets := fstest.MapFS{"200.html": {Data: html}, "public.html": {Data: html}}
	policies, err := htmlPolicies(assets)
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

func serveStatic(t *testing.T, h *handler, method, target, acceptEncoding, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "https://beacon.test"+target, nil)
	request.Header.Set("Accept-Encoding", acceptEncoding)
	request.Header.Set("If-None-Match", ifNoneMatch)
	response := httptest.NewRecorder()
	h.static(response, request)
	return response
}
