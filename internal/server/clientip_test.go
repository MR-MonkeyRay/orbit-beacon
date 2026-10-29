package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"testing/fstest"

	"orbitbeacon/internal/server/auth"
)

func TestClientAddrTrustedProxyChain(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8:1::/48")}
	for _, tt := range []struct {
		name    string
		peer    string
		headers []string
		trust   bool
		want    string
	}{
		{"default ignores spoof", "10.0.0.1:1234", []string{"192.0.2.1"}, false, "10.0.0.1:1234"},
		{"untrusted ignores spoof", "198.51.100.1:1234", []string{"192.0.2.1, 10.0.0.2"}, true, "198.51.100.1:1234"},
		{"one proxy", "10.0.0.1:1234", []string{"192.0.2.1"}, true, "192.0.2.1:0"},
		{"multiple proxies", "10.0.0.1:1234", []string{" 192.0.2.1 , 10.0.0.2, 10.0.0.3 "}, true, "192.0.2.1:0"},
		{"nearest untrusted wins", "10.0.0.1:1234", []string{"192.0.2.1, 198.51.100.2, 10.0.0.2"}, true, "198.51.100.2:0"},
		{"multiple header lines", "10.0.0.1:1234", []string{"192.0.2.1", "10.0.0.2"}, true, "192.0.2.1:0"},
		{"IPv6 chain", "[2001:db8:1::1]:1234", []string{"2001:db8:2::5, 2001:db8:1::2"}, true, "[2001:db8:2::5]:0"},
		{"mapped peer and hop", "[::ffff:10.0.0.1]:1234", []string{"::ffff:192.0.2.1, ::ffff:10.0.0.2"}, true, "192.0.2.1:0"},
		{"missing header", "10.0.0.1:1234", nil, true, "10.0.0.1:1234"},
		{"empty header", "10.0.0.1:1234", []string{""}, true, "10.0.0.1:1234"},
		{"invalid left hop", "10.0.0.1:1234", []string{"unknown, 192.0.2.1"}, true, "10.0.0.1:1234"},
		{"invalid right hop", "10.0.0.1:1234", []string{"192.0.2.1, unknown"}, true, "10.0.0.1:1234"},
		{"empty hop", "10.0.0.1:1234", []string{"192.0.2.1,,10.0.0.2"}, true, "10.0.0.1:1234"},
		{"port is not IP", "10.0.0.1:1234", []string{"192.0.2.1:5678"}, true, "10.0.0.1:1234"},
		{"quoted IP", "10.0.0.1:1234", []string{`"192.0.2.1"`}, true, "10.0.0.1:1234"},
		{"bracketed IPv6", "10.0.0.1:1234", []string{"[2001:db8:2::5]"}, true, "10.0.0.1:1234"},
		{"scoped IPv6", "10.0.0.1:1234", []string{"fe80::1%eth0"}, true, "10.0.0.1:1234"},
		{"all trusted", "10.0.0.1:1234", []string{"2001:db8:1::2, 10.0.0.2"}, true, "10.0.0.1:1234"},
		{"invalid peer", "not-an-address", []string{"192.0.2.1"}, true, "not-an-address"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := &handler{}
			if tt.trust {
				h.trustedProxies = trusted
			}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.peer
			for _, value := range tt.headers {
				r.Header.Add("X-Forwarded-For", value)
			}
			if got := h.clientAddr(r); got != tt.want {
				t.Errorf("clientAddr = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoginRejectionLogsDerivedClientIP(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		var logs bytes.Buffer
		h := &handler{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
		want := "10.0.0.1"
		if trusted {
			h.trustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.1/32")}
			want = "2001:db8::5"
		}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/session", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		r.Header.Set("X-Forwarded-For", "2001:db8::5")
		h.logLoginRejection(auth.ErrUnauthenticated, r, "request-id")
		var entry struct{ Peer string }
		if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Peer != want {
			t.Errorf("trusted=%v: logged peer %q, want %q", trusted, entry.Peer, want)
		}
	}
}

func TestHSTSBelongsToExternalProxy(t *testing.T) {
	h := &handler{assets: fstest.MapFS{"public.html": {Data: []byte("<!doctype html><title>public</title>")}, "asset.js": {Data: []byte("export {}")}}}
	var err error
	h.htmlPolicies, err = htmlPolicies(h.assets, "https://beacon.test")
	if err != nil {
		t.Fatal(err)
	}
	for _, scheme := range []string{"http", "https"} {
		for _, route := range []struct{ method, path string }{
			{http.MethodGet, "/"}, {http.MethodGet, "/asset.js"}, {http.MethodGet, "/missing"},
			{http.MethodGet, "/api/unknown"}, {http.MethodGet, "/healthz/live"},
			{http.MethodGet, "/healthz/ready"}, {http.MethodPost, "/"},
		} {
			r := httptest.NewRequest(route.method, scheme+"://beacon.test"+route.path, nil)
			r.Header.Set("X-Forwarded-Proto", "https")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if got := w.Header().Get("Strict-Transport-Security"); got != "" {
				t.Errorf("%s %s %s: application emitted HSTS %q", scheme, route.method, route.path, got)
			}
		}
	}
}
