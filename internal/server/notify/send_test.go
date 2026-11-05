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

type webhookTestTransport func(*http.Request) (*http.Response, error)

func (f webhookTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWebhookSignatureTimestampAndRedirects(t *testing.T) {
	now := time.Unix(1700000000, 0)
	clockCalls := 0
	opts := Options{Now: func() time.Time {
		clockCalls++
		return now.Add(time.Duration(clockCalls-1) * time.Second)
	}}
	s := newSender(opts)
	defer s.client.CloseIdleConnections()
	calls := 0
	s.client.Transport = webhookTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		defer r.Body.Close()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		const body = `{"event_id":"event-1","state":"","rule":{"id":"","name":"","metric":"","severity":""},"node":{"id":"","name":""},"value":null,"started_at":"0001-01-01T00:00:00Z","resolved_at":null}`
		const signature = "v1=6d6a68cdabd8db678314c38915dd18185360d03dd05729e59546e22cc15a68be"
		if string(raw) != body {
			t.Fatalf("body=%s want %s", raw, body)
		}
		if r.Method != "POST" || r.Header.Get("X-OrbitBeacon-Signature") != signature || r.Header.Get("X-OrbitBeacon-Timestamp") != "1700000000" {
			t.Error("bad signature or timestamp")
		}
		got, err := hex.DecodeString(strings.TrimPrefix(r.Header.Get("X-OrbitBeacon-Signature"), "v1="))
		if err != nil {
			t.Fatal(err)
		}
		// 独立构造验证输入；只篡改时间戳时，原签名必须失效。
		for _, timestamp := range []string{"1700000000", "1700000001"} {
			mac := hmac.New(sha256.New, []byte("secret"))
			mac.Write([]byte(timestamp + "." + body))
			if hmac.Equal(got, mac.Sum(nil)) != (timestamp == "1700000000") {
				t.Errorf("unexpected signature verification for timestamp %s", timestamp)
			}
		}
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://forbidden.example"}}, Body: io.NopCloser(strings.NewReader(strings.Repeat("credential", 2000)))}, nil
	})
	result := s.Send(context.Background(), Channel{Kind: "webhook", Config: Config{URL: "https://example.com/hook"}}, "secret", Payload{EventID: "event-1"})
	if result.OK || result.retry || result.ErrorCode != "redirect_denied" || calls != 1 {
		t.Fatalf("result %+v calls %d", result, calls)
	}
	if clockCalls != 1 {
		t.Fatalf("clock called %d times, want 1", clockCalls)
	}
}

func TestWebhookRetryRefreshesSignature(t *testing.T) {
	now := time.Unix(1700000000, 0)
	s := newSender(Options{Now: func() time.Time { return now }})
	defer s.client.CloseIdleConnections()
	var timestamps, signatures, bodies []string
	s.client.Transport = webhookTestTransport(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		timestamps = append(timestamps, r.Header.Get("X-OrbitBeacon-Timestamp"))
		signatures = append(signatures, r.Header.Get("X-OrbitBeacon-Signature"))
		bodies = append(bodies, string(raw))
		status := http.StatusServiceUnavailable
		if len(timestamps) == 2 {
			status = http.StatusNoContent
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	c := Channel{Kind: "webhook", Config: Config{URL: "https://example.com/hook"}}
	p := Payload{EventID: "event-1"}
	if result := s.Send(context.Background(), c, "secret", p); result.OK || !result.retry {
		t.Fatalf("first attempt: %+v", result)
	}
	now = now.Add(time.Minute)
	if result := s.Send(context.Background(), c, "secret", p); !result.OK {
		t.Fatalf("retry: %+v", result)
	}
	if len(timestamps) != 2 {
		t.Fatalf("attempts=%d want 2", len(timestamps))
	}
	if timestamps[0] != "1700000000" || timestamps[1] != "1700000060" ||
		signatures[0] != "v1=6d6a68cdabd8db678314c38915dd18185360d03dd05729e59546e22cc15a68be" ||
		signatures[1] != "v1=33ac3e8c0acb0f6972cfa06f246dae6d672b16bd19baa9c9c76b15f68ed9c8f9" || bodies[0] != bodies[1] {
		t.Fatalf("timestamps=%v signatures=%v bodies=%v", timestamps, signatures, bodies)
	}
}

func TestWebhookWithoutSecret(t *testing.T) {
	s := newSender(Options{Now: func() time.Time { return time.Unix(1700000000, 0) }})
	defer s.client.CloseIdleConnections()
	calls := 0
	s.client.Transport = webhookTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		defer r.Body.Close()
		if r.Header.Get("X-OrbitBeacon-Timestamp") != "1700000000" {
			t.Error("bad timestamp")
		}
		if len(r.Header.Values("X-OrbitBeacon-Signature")) != 0 {
			t.Error("signature header present without secret")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	result := s.Send(context.Background(), Channel{Kind: "webhook", Config: Config{URL: "https://example.com/hook"}}, "", Payload{})
	if !result.OK || calls != 1 {
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
