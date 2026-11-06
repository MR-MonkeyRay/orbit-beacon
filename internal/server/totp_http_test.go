package server_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/secretbox"
)

func httpTOTPCode(secret string, now time.Time) string {
	key, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(now.Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(b[:])
	sum := mac.Sum(nil)
	offset := sum[19] & 15
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[offset:offset+4])&0x7fffffff)%1000000)
}
func TestTOTPHTTPGatesAndSession(t *testing.T) {
	f := newHTTP(t, 5)
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	box, err := secretbox.New(bytes.Repeat([]byte{4}, 32), "orbitbeacon account secrets v1")
	if err != nil {
		t.Fatal(err)
	}
	f.auth = auth.NewService(f.db, auth.Options{SecretBox: box, Now: func() time.Time { return now }})
	f.handler, err = server.NewHandler(testHandlerOptions(t, server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL, Assets: fstest.MapFS{
		"200.html": {Data: []byte("console")}, "public.html": {Data: []byte("public")}, "_app/immutable/app.js": {Data: []byte("export{}")},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := login(t, f, "admin")
	var id string
	f.db.QueryRow(`SELECT id FROM users WHERE username='admin-two'`).Scan(&id)
	routes := []struct{ method, path, body string }{
		{"GET", "/api/v1/account/totp", ""},
		{"POST", "/api/v1/account/totp/setup", `{"password":"` + password + `"}`},
		{"POST", "/api/v1/account/totp/confirm", `{"code":"123456"}`},
		{"POST", "/api/v1/account/totp/disable", `{"password":"` + password + `","otp":"123456"}`},
		{"POST", "/api/v1/account/totp/recovery-codes", `{"password":"` + password + `","otp":"123456"}`},
		{"POST", "/api/v1/users/" + id + "/totp/reset", `{"password":"` + password + `"}`},
	}
	for _, route := range routes {
		assertHTTPStatus(t, request(t, f.handler, route.method, route.path, route.body, nil, "", publicURL), 401)
		if route.method == "POST" {
			assertHTTPStatus(t, request(t, f.handler, route.method, route.path, route.body, cookie, "", publicURL), 403)
			assertHTTPStatus(t, request(t, f.handler, route.method, route.path, route.body, cookie, csrf, "https://untrusted.example"), 403)
		}
	}
	restricted, err := f.auth.CreateUser(context.Background(), "restricted-totp", password, true)
	if err != nil {
		t.Fatal(err)
	}
	restrictedSession, err := f.auth.Login(context.Background(), restricted.Username, password, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		assertHTTPStatus(t, request(t, f.handler, route.method, route.path, route.body, &http.Cookie{Name: "orbitbeacon_session", Value: restrictedSession.Token}, restrictedSession.CSRFToken, publicURL), 403)
	}
	setupResponse := request(t, f.handler, "POST", "/api/v1/account/totp/setup", routes[1].body, cookie, csrf, publicURL)
	assertHTTPStatus(t, setupResponse, 200)
	if setupResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("setup cached")
	}
	var setup auth.TOTPSetup
	if err = json.Unmarshal(setupResponse.Body.Bytes(), &setup); err != nil {
		t.Fatal(err)
	}
	if len(setup.Secret) != 32 || setup.QR.Size*setup.QR.Size != len(setup.QR.Modules) {
		t.Fatal("setup payload")
	}
	confirm := request(t, f.handler, "POST", "/api/v1/account/totp/confirm", `{"code":"`+httpTOTPCode(setup.Secret, now)+`"}`, cookie, csrf, publicURL)
	assertHTTPStatus(t, confirm, 200)
	if confirm.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("recovery cached")
	}
	var codes struct {
		Codes []string `json:"recovery_codes"`
	}
	if err = json.Unmarshal(confirm.Body.Bytes(), &codes); err != nil || len(codes.Codes) != 10 {
		t.Fatal("recovery payload", err)
	}
	loginBody := `{"username":"admin","password":"` + password + `"`
	response := request(t, f.handler, "POST", "/api/v1/session", loginBody+`}`, nil, "", publicURL)
	apiErrorAgent(t, response, 401, "totp_required")
	response = request(t, f.handler, "POST", "/api/v1/session", loginBody+`,"otp":"not-a-code"}`, nil, "", publicURL)
	apiErrorAgent(t, response, 401, "totp_invalid")
	response = request(t, f.handler, "POST", "/api/v1/session", loginBody+`,"otp":"`+strings.Repeat("a", 65)+`"}`, nil, "", publicURL)
	assertHTTPStatus(t, response, 401)
	now = now.Add(30 * time.Second)
	response = request(t, f.handler, "POST", "/api/v1/session", loginBody+`,"otp":"`+httpTOTPCode(setup.Secret, now)+`"}`, nil, "", publicURL)
	assertHTTPStatus(t, response, 200)
	if !strings.Contains(response.Body.String(), `"totp_enabled":true`) {
		t.Fatal("session lacks TOTP status")
	}
	response = request(t, f.handler, "POST", "/api/v1/account/totp/disable", `{"password":"`+password+`","otp":"`+codes.Codes[0]+`"}`, cookie, csrf, publicURL)
	assertHTTPStatus(t, response, 204)
}
