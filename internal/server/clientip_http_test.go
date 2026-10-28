package server_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/webui"
)

func TestHTMLCSPMatchesServedInlineScripts(t *testing.T) {
	f := newHTTP(t, 2)
	for _, assets := range []struct {
		name string
		fs   fs.FS
	}{
		{"embedded", webui.Assets()},
		{"multiple scripts", fstest.MapFS{
			"200.html":        {Data: []byte("<!doctype html><script>consoleTheme()</script><script type=\"module\">\nconsoleInit()\n</script><script src=\"/external.js\">externalBody()</script>")},
			"public.html":     {Data: []byte("<!doctype html><script>publicTheme()</script><script>publicTheme()</script>")},
			"login.html":      {Data: []byte("<!doctype html><script>loginTheme()</script>")},
			"extra/page.html": {Data: []byte("<!doctype html><script>extraTheme()</script>")},
		}},
	} {
		t.Run(assets.name, func(t *testing.T) {
			h, err := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL, Assets: assets.fs})
			if err != nil {
				t.Fatal(err)
			}
			paths := []string{"/", "/public.html", "/login", "/overview", "/nodes", "/alerts", "/settings", "/200.html"}
			_ = fs.WalkDir(assets.fs, ".", func(name string, entry fs.DirEntry, err error) error {
				if err != nil {
					t.Fatal(err)
				}
				if !entry.IsDir() && strings.HasSuffix(name, ".html") {
					paths = append(paths, "/"+name)
				}
				return nil
			})
			for _, path := range paths {
				t.Run(path, func(t *testing.T) {
					get := request(t, h, http.MethodGet, path, "", nil, "", "")
					if get.Code != http.StatusOK {
						t.Fatalf("HTML status %d", get.Code)
					}
					policy := get.Header().Get("Content-Security-Policy")
					if strings.Contains(policy, "'unsafe-inline'") {
						t.Fatal("CSP permits unsafe-inline")
					}
					for _, directive := range []string{"default-src 'self'", "style-src 'self'", "img-src 'self' data:", "font-src 'self'", "connect-src 'self'", "object-src 'none'", "base-uri 'none'", "form-action 'self'", "frame-ancestors 'none'"} {
						if !strings.Contains("; "+policy+";", "; "+directive+";") {
							t.Errorf("missing exact directive %q in %q", directive, policy)
						}
					}
					expected := map[string]bool{}
					for _, script := range regexp.MustCompile(`(?is)<script\s*([^>]*)>(.*?)</script\s*>`).FindAllStringSubmatch(get.Body.String(), -1) {
						if strings.Contains(script[1], "src=") {
							continue
						}
						digest := sha256.Sum256([]byte(script[2]))
						expected["'sha256-"+base64.StdEncoding.EncodeToString(digest[:])+"'"] = true
					}
					for hash := range expected {
						if !strings.Contains(policy, hash) {
							t.Errorf("served inline script lacks CSP hash %s", hash)
						}
					}
					for _, hash := range regexp.MustCompile(`'sha256-[^']+'`).FindAllString(policy, -1) {
						if !expected[hash] {
							t.Errorf("CSP includes hash for another file or external script: %s", hash)
						}
					}
					head := request(t, h, http.MethodHead, path, "", nil, "", "")
					if head.Header().Get("Content-Security-Policy") != policy {
						t.Error("HEAD and GET policies differ")
					}
				})
			}
		})
	}
}

func TestEnrollmentAdmissionUsesTrustedClientIP(t *testing.T) {
	for _, trust := range []bool{false, true} {
		f := newHTTP(t, 2)
		opts := server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL}
		if trust {
			opts.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.1/32")}
		}
		h, err := server.NewHandler(opts)
		if err != nil {
			t.Fatal(err)
		}
		attempt := func(ip string) int {
			r := httptest.NewRequest(http.MethodPost, publicURL+"/api/agent/v1/enroll/check", strings.NewReader("{}"))
			r.RemoteAddr = "10.0.0.1:1234"
			r.Header.Set("X-Forwarded-For", ip)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			return w.Code
		}
		for range 32 {
			if status := attempt("192.0.2.1"); status != http.StatusUnauthorized {
				t.Fatalf("trusted=%v: initial status %d", trust, status)
			}
		}
		if status := attempt("192.0.2.1"); status != http.StatusTooManyRequests {
			t.Fatalf("trusted=%v: exhausted status %d", trust, status)
		}
		want := http.StatusTooManyRequests
		if trust {
			want = http.StatusUnauthorized
		}
		if status := attempt("192.0.2.2"); status != want {
			t.Errorf("trusted=%v: different client status %d, want %d", trust, status, want)
		}
	}
}

func TestLoginUsesTrustedClientIP(t *testing.T) {
	f := newHTTP(t, 2)
	h, err := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.1/32")}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, publicURL+"/api/v1/session", strings.NewReader(`{"username":"admin","password":"`+password+`"}`))
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("Origin", publicURL)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-For", "2001:db8::5")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("login status %d: %s", w.Code, w.Body)
	}
	var raw string
	if err := f.db.QueryRow(`SELECT details FROM audit_events WHERE action = 'session.create'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var details struct{ Peer string }
	if err := json.Unmarshal([]byte(raw), &details); err != nil {
		t.Fatal(err)
	}
	if details.Peer != "2001:db8::5" {
		t.Errorf("login peer = %q", details.Peer)
	}
}
