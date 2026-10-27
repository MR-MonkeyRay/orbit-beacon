package server_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"orbitbeacon/internal/server"
)

func TestPublicEmbeddedAssetsAndRequiredShell(t *testing.T) {
	f := newHTTP(t, 2)
	h, err := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL})
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range []struct{ path, mime string }{{"/", "text/html"}, {"/_app/public.js", "javascript"}, {"/_app/public.css", "text/css"}} {
		response := request(t, h, http.MethodGet, asset.path, "", nil, "", "")
		if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), asset.mime) {
			t.Fatalf("asset %s status/type: %d %s", asset.path, response.Code, response.Header().Get("Content-Type"))
		}
		head := request(t, h, http.MethodHead, asset.path, "", nil, "", "")
		if head.Code != http.StatusOK || head.Body.Len() != 0 {
			t.Fatalf("HEAD %s: %d %d", asset.path, head.Code, head.Body.Len())
		}
	}
	if _, err := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL, Assets: fstest.MapFS{"200.html": &fstest.MapFile{Data: []byte("admin")}}}); err == nil || !strings.Contains(err.Error(), "public") {
		t.Fatalf("missing public shell: %v", err)
	}
	assertHTTPStatus(t, request(t, h, http.MethodGet, "/_app/public-missing.js", "", nil, "", ""), http.StatusNotFound)
	if err := h.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
