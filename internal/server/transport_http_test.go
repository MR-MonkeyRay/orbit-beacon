package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orbitbeacon/internal/server"
)

func TestSessionCookieUsesConfiguredOrigin(t *testing.T) {
	f := newHTTP(t, 5)
	for _, origin := range []string{"http://beacon.example.test:8080", "https://beacon.example.test"} {
		t.Run(origin, func(t *testing.T) {
			h, err := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: origin})
			if err != nil {
				t.Fatal(err)
			}
			for _, forwarded := range []string{"http", "https"} {
				req := httptest.NewRequest(http.MethodPost, "http://upstream.internal/api/v1/session", strings.NewReader(`{"username":"admin-two","password":"`+password+`"}`))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Origin", origin)
				req.Header.Set("X-Forwarded-Proto", forwarded)
				res := httptest.NewRecorder()
				h.ServeHTTP(res, req)
				if res.Code != http.StatusOK {
					t.Fatalf("login: %d %s", res.Code, res.Body)
				}
				cookies := res.Result().Cookies()
				if len(cookies) != 1 {
					t.Fatalf("cookies: %+v", cookies)
				}
				c := cookies[0]
				if c.Name != "orbitbeacon_session" || c.Secure != strings.HasPrefix(origin, "https://") || c.Domain != "" || !c.HttpOnly || c.Path != "/" || c.SameSite != http.SameSiteLaxMode {
					t.Fatalf("cookie does not follow configured origin: %+v", c)
				}
				req = httptest.NewRequest(http.MethodGet, "http://upstream.internal/api/v1/me", nil)
				req.AddCookie(c)
				res = httptest.NewRecorder()
				h.ServeHTTP(res, req)
				if res.Code != http.StatusOK {
					t.Fatalf("session: %d %s", res.Code, res.Body)
				}
			}
		})
	}
}
