package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/timekey"
)

func TestWebhookURLCiphertextAndOmittedURL(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	target := "https://hooks.example:8443/private-route/path-secret?token=query-secret"
	f.target(t, target)
	if _, err := f.s.Save(ctx, f.channel.ID, Patch{Config: json.RawMessage(`{"url":""}`)}); err == nil {
		t.Fatal("explicit empty URL accepted")
	}
	for _, p := range []Patch{{Name: ptr("renamed")}, {Config: json.RawMessage(`{}`)}, {Secret: json.RawMessage(`"new-signing-secret"`)}} {
		c, err := f.s.Save(ctx, f.channel.ID, p)
		if err != nil {
			t.Fatal(err)
		}
		if c.Config.URL != "" || c.Config.URLRedacted != "https://hooks.example:8443/…" || !c.Config.URLSet {
			t.Fatalf("unsafe channel: %+v", c.Config)
		}
	}
	var config string
	var encrypted, signing []byte
	if err := f.db.QueryRow("SELECT config_json,url_ciphertext,secret_ciphertext FROM notification_channels WHERE id=?", f.channel.ID).Scan(&config, &encrypted, &signing); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-route", "path-secret", "token", "query-secret"} {
		if strings.Contains(config, secret) || bytes.Contains(encrypted, []byte(secret)) {
			t.Fatal("URL secret stored in plaintext")
		}
	}
	plain, err := f.s.box.Open(f.channel.ID+":url", encrypted)
	if err != nil || string(plain) != target {
		t.Fatalf("URL did not decrypt: %v", err)
	}
	clear(plain)
	if _, err := f.s.box.Open(f.channel.ID, encrypted); err == nil {
		t.Fatal("URL ciphertext not bound to URL purpose")
	}
	c, secret, err := f.s.load(ctx, f.channel.ID)
	if err != nil || c.Config.URL != target || secret != "new-signing-secret" {
		t.Fatalf("load failed: %v", err)
	}
	// Even delivery objects cannot accidentally serialize their decrypted URL.
	raw, err := json.Marshal(c)
	if err != nil || bytes.Contains(raw, []byte("path-secret")) {
		t.Fatal("decrypted URL serialized")
	}
}

func ptr(s string) *string { return &s }

func TestSecretReplacementAndClearWithWrongKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	wrong, _ := secretbox.New(bytes.Repeat([]byte{8}, 32))
	f.s.box = wrong
	// Replacing either credential must not open the other credential.
	if _, err := f.s.Save(ctx, f.channel.ID, Patch{Secret: json.RawMessage(`"replacement"`)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.load(ctx, f.channel.ID); !errors.Is(err, ErrSecretUnreadable) {
		t.Fatalf("old URL should need repair: %v", err)
	}
	f.target(t, "https://hooks.example/repaired?key=secret")
	if _, secret, err := f.s.load(ctx, f.channel.ID); err != nil || secret != "replacement" {
		t.Fatalf("repair failed: %v", err)
	}
	old, _ := secretbox.New(bytes.Repeat([]byte{7}, 32))
	f.s.box = old
	if _, err := f.s.Save(ctx, f.channel.ID, Patch{Secret: json.RawMessage(`""`)}); err != nil {
		t.Fatal(err)
	}
	c, err := f.s.Get(ctx, f.channel.ID)
	if err != nil || c.SecretSet || !c.Config.URLSet {
		t.Fatalf("clear failed: %v", err)
	}
	// Telegram needs its retained token for validation, but replacement bypasses
	// decryption of the old token and repairs the channel under the current key.
	var p Patch
	if err := json.Unmarshal([]byte(`{"name":"bot","kind":"telegram","enabled":true,"config":{"chat_id":"-123"},"secret":"123:old"}`), &p); err != nil {
		t.Fatal(err)
	}
	bot, err := f.s.Save(ctx, "", p)
	if err != nil {
		t.Fatal(err)
	}
	f.s.box = wrong
	if _, err = f.s.Save(ctx, bot.ID, Patch{Name: ptr("rename")}); !errors.Is(err, ErrSecretUnreadable) {
		t.Fatalf("expected unreadable: %v", err)
	}
	if _, err = f.s.Save(ctx, bot.ID, Patch{Secret: json.RawMessage(`"123:new"`)}); err != nil {
		t.Fatal(err)
	}
	if _, token, err := f.s.load(ctx, bot.ID); err != nil || token != "123:new" {
		t.Fatalf("token not repaired: %v", err)
	}
}

func TestUnreadableDeliveryBackoffAndKeyRestoration(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer ts.Close()
	f.target(t, ts.URL+"/private-path?token=query-secret")
	f.s.Notify(context.Background(), f.note("restore-key"))
	original := f.s.box
	wrong, _ := secretbox.New(bytes.Repeat([]byte{8}, 32))
	f.s.box = wrong
	var logs bytes.Buffer
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(logger) })
	for i := range 2 {
		f.s.process(context.Background())
		st, attempts, code, next := state(t, f.db)
		if st != "pending" || attempts != i+1 || code != "secret_unreadable" || calls.Load() != 0 {
			t.Fatalf("not retryable: %s %d %s", st, attempts, code)
		}
		stamp, err := timekey.Parse(next)
		if err != nil || stamp.Sub(f.now) != backoffs[i] {
			t.Fatalf("wrong backoff: %s %v", next, err)
		}
		f.now = stamp
	}
	if strings.Count(logs.String(), "notification credentials unreadable") != 1 || !strings.Contains(logs.String(), f.channel.ID) {
		t.Fatalf("unbounded/missing log: %s", logs.String())
	}
	// Restart with the restored key and persisted pending work.
	restored := New(f.db, original, f.s.opts)
	restored.process(context.Background())
	if st, attempts, _, _ := state(t, f.db); st != "sent" || attempts != 3 || calls.Load() != 1 {
		t.Fatalf("did not resume: %s %d", st, attempts)
	}
	for _, secret := range []string{"private-path", "token", "query-secret", "never-expose-this-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("secret in log")
		}
	}
	f.now = f.now.Add(time.Hour)
	f.s.warnSecretUnreadable(f.channel.ID)
	if strings.Count(logs.String(), "notification credentials unreadable") != 2 {
		t.Fatal("hourly warning missing")
	}
}

func TestUnreadableDeliveryUsesNormalAttemptLimit(t *testing.T) {
	f := newFixture(t)
	f.s.Notify(context.Background(), f.note("exhaust-key"))
	f.s.box, _ = secretbox.New(bytes.Repeat([]byte{8}, 32))
	for i := range 5 {
		f.s.process(context.Background())
		st, attempts, code, next := state(t, f.db)
		want := "pending"
		if i == 4 {
			want = "failed"
		}
		if st != want || attempts != i+1 || code != "secret_unreadable" {
			t.Fatalf("attempt=%d state=%s count=%d code=%s", i, st, attempts, code)
		}
		stamp, err := timekey.Parse(next)
		if err != nil || stamp.Sub(f.now) > time.Hour {
			t.Fatalf("uncapped backoff: %s", next)
		}
		f.now = stamp
	}
}

func TestEmailPasswordReplacementAndClearWithWrongKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var p Patch
	if e := json.Unmarshal([]byte(`{"name":"mail","kind":"email","enabled":true,"config":{"host":"mail.example.com","port":587,"from":"from@example.com","to":["to@example.com"]},"secret":"old-password"}`), &p); e != nil {
		t.Fatal(e)
	}
	c, e := f.s.Save(ctx, "", p)
	if e != nil {
		t.Fatal(e)
	}
	f.s.box, _ = secretbox.New(bytes.Repeat([]byte{8}, 32))
	if _, e = f.s.Save(ctx, c.ID, Patch{Secret: json.RawMessage(`"replacement-password"`)}); e != nil {
		t.Fatal("replacement decrypted old password", e)
	}
	if _, password, e := f.s.load(ctx, c.ID); e != nil || password != "replacement-password" {
		t.Fatal("password not repaired", e)
	}
	if _, e = f.db.Exec("UPDATE notification_channels SET secret_ciphertext=x'01' WHERE id=?", c.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.Save(ctx, c.ID, Patch{Secret: json.RawMessage(`""`)}); e != nil {
		t.Fatal("clear decrypted old password", e)
	}
}
