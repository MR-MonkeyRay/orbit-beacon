package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func storageTestPayload() StoragePayload {
	role := "standby"
	return StoragePayload{Kind: "storage", EventID: "00000000-0000-4000-8000-000000000001", Domain: "main", Transition: "manual_required", ActiveRole: &role, ReasonCode: "recovery_timeout", OccurredAt: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
}

func TestStorageWebhookTLSFixedPayloadAndSignature(t *testing.T) {
	p := storageTestPayload()
	received := make(chan []byte, 1)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte("signing-secret"))
		mac.Write([]byte(r.Header.Get("X-OrbitBeacon-Timestamp") + "."))
		mac.Write(raw)
		if r.Header.Get("X-OrbitBeacon-Signature") != "v1="+hex.EncodeToString(mac.Sum(nil)) {
			t.Error("invalid signature")
		}
		received <- raw
		w.WriteHeader(204)
	}))
	defer ts.Close()
	s := newSender(Options{Now: func() time.Time { return p.OccurredAt }, AllowPrivateNetworks: true})
	defer s.client.CloseIdleConnections()
	c := Channel{Kind: "webhook", Config: Config{URL: ts.URL + "/private-token"}}
	if got := s.SendStorage(context.Background(), c, "signing-secret", p); got.OK {
		t.Fatal("untrusted TLS accepted")
	}
	trustTestServer(s, ts)
	if got := s.SendStorage(context.Background(), c, "signing-secret", p); !got.OK {
		t.Fatalf("send=%+v", got)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(<-received, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 7 || string(fields["kind"]) != `"storage"` || fields["rule"] != nil || fields["node"] != nil {
		t.Fatalf("unexpected fields: %v", fields)
	}
}

func TestStorageSMTPUsesTLSAndFixedRendering(t *testing.T) {
	cfg, cert, done := fakeSMTP(t, "success")
	cfg.Template = "{node} attacker template"
	s := newSender(Options{Now: time.Now, AllowPrivateNetworks: true})
	defer s.client.CloseIdleConnections()
	trustTestServer(s, cert)
	if got := s.SendStorage(context.Background(), Channel{Kind: "email", Config: cfg}, "smtp-password", storageTestPayload()); !got.OK {
		t.Fatalf("send=%+v", got)
	}
	raw := <-done
	if !strings.Contains(raw, "STARTTLS\r\n") || !strings.Contains(raw, "Subject: OrbitBeacon storage notification\r\n") {
		t.Fatal("missing TLS or storage subject")
	}
	parts := strings.SplitN(raw, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("missing body")
	}
	body, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSuffix(parts[1], "QUIT\r\n"), "\r\n", ""))
	if err != nil || !strings.Contains(string(body), "manual_required") || !strings.Contains(string(body), "main") || strings.Contains(string(body), "attacker") {
		t.Fatalf("invalid fixed body: %v", err)
	}
}
