package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func trustTestServer(s *sender, ts *httptest.Server) {
	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	s.tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	s.client.Transport.(*http.Transport).TLSClientConfig = s.tlsConfig
}
func TestWebhookSignatureTimestampAndRedirects(t *testing.T) {
	now := time.Unix(1700000000, 0)
	opts := Options{Now: func() time.Time { return now }, AllowPrivateNetworks: true}
	s := newSender(opts)
	defer s.client.CloseIdleConnections()
	calls := 0
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte("secret"))
		mac.Write(raw)
		if r.Method != "POST" || r.Header.Get("X-OrbitBeacon-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) || r.Header.Get("X-OrbitBeacon-Timestamp") != "1700000000" {
			t.Error("bad signature or timestamp")
		}
		var p Payload
		if json.Unmarshal(raw, &p) != nil || p.EventID != "event-1" {
			t.Error("bad payload")
		}
		w.Header().Set("Location", "https://forbidden.example")
		w.WriteHeader(http.StatusFound)
		w.Write([]byte(strings.Repeat("credential", 2000)))
	}))
	defer ts.Close()
	trustTestServer(s, ts)
	result := s.Send(context.Background(), Channel{Kind: "webhook", Config: Config{URL: ts.URL}}, "secret", Payload{EventID: "event-1"})
	if result.OK || result.retry || result.ErrorCode != "redirect_denied" || calls != 1 {
		t.Fatalf("result %+v calls %d", result, calls)
	}
}
func TestTelegramPlainTextAndHTTPFailures(t *testing.T) {
	now := time.Now()
	s := newSender(Options{Now: func() time.Time { return now }, AllowPrivateNetworks: true})
	defer s.client.CloseIdleConnections()
	status := 200
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot123:token/sendMessage" {
			t.Errorf("path=%s", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if len(body) != 2 || body["chat_id"] != "-123" || !strings.Contains(body["text"].(string), "<not HTML>") {
			t.Errorf("bad Telegram body %v", body)
		}
		w.Header().Set("Retry-After", "7200")
		w.WriteHeader(status)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()
	trustTestServer(s, ts)
	s.telegramBase = ts.URL
	p := Payload{}
	p.Rule.Name = "<not HTML>"
	c := Channel{Kind: "telegram", Config: Config{ChatID: "-123"}}
	for _, code := range []int{200, 400, 429, 500} {
		status = code
		result := s.Send(context.Background(), c, "123:token", p)
		if result.OK != (code == 200) || result.retry != (code == 429 || code == 500) {
			t.Fatalf("HTTP %d: %+v", code, result)
		}
		if result.retry && result.retryAfter != time.Hour {
			t.Fatal("Retry-After not capped")
		}
	}
}
func TestRetryAfter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tt := range []struct {
		raw  string
		want time.Duration
	}{{"60", time.Minute}, {"7200", time.Hour}, {"-1", 0}, {"nonsense", 0}, {now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute}, {now.Add(-time.Minute).Format(http.TimeFormat), 0}, {"9223372036854775807", time.Hour}} {
		if got := parseRetryAfter(tt.raw, now); got != tt.want {
			t.Errorf("%q: %v want %v", tt.raw, got, tt.want)
		}
	}
}
func TestTLSVerificationRequired(t *testing.T) {
	s := newSender(Options{Now: time.Now, AllowPrivateNetworks: true})
	defer s.client.CloseIdleConnections()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("untrusted TLS accepted") }))
	defer ts.Close()
	if r := s.Send(context.Background(), Channel{Kind: "webhook", Config: Config{URL: ts.URL}}, "", Payload{}); r.OK {
		t.Fatal("untrusted TLS accepted")
	}
}

func TestRemoteBytesCannotEnterIdleTransportLogs(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	s := newSender(Options{Now: time.Now, AllowPrivateNetworks: true})
	defer s.client.CloseIdleConnections()
	if !s.client.Transport.(*http.Transport).DisableKeepAlives {
		t.Fatal("idle HTTP diagnostics may log unsolicited remote bytes")
	}
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, e := w.(http.Hijacker).Hijack()
		if e != nil {
			t.Error(e)
			return
		}
		defer conn.Close()
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\nremote-secret-unsolicited-data")
	}))
	defer ts.Close()
	trustTestServer(s, ts)
	result := s.Send(context.Background(), Channel{Kind: "webhook", Config: Config{URL: ts.URL}}, "signing-secret", Payload{})
	if !result.OK {
		t.Fatal(result)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(logs.String(), "remote-secret") {
		t.Fatal("unsolicited remote bytes logged")
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatal("remote data escaped into result")
	}
}

func TestPersistedHTTPWebhookObeysCurrentConfig(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	defer ts.Close()
	c := Channel{Kind: "webhook", Config: Config{URL: ts.URL}}
	for _, allow := range []bool{false, true} {
		s := newSender(Options{Now: time.Now, AllowPrivateNetworks: true, AllowInsecureWebhooks: allow})
		result := s.Send(context.Background(), c, "", Payload{})
		s.client.CloseIdleConnections()
		if result.OK != allow {
			t.Fatalf("insecure config ignored: %+v", result)
		}
	}
	if calls != 1 {
		t.Fatal("disabled insecure webhook dialed")
	}
}
