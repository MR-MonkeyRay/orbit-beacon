package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"orbitbeacon/internal/server/auth"
)

func TestLateLogoutResponseDoesNotClearNewSessionCookie(t *testing.T) {
	f := newConsoleLiveFixture(t)
	revoked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/session" {
			f.h.ServeHTTP(w, r)
			return
		}
		buffer := httptest.NewRecorder()
		f.h.ServeHTTP(buffer, r)
		close(revoked)
		<-release
		for name, values := range buffer.Header() {
			w.Header()[name] = values
		}
		w.WriteHeader(buffer.Code)
		_, _ = io.Copy(w, buffer.Body)
	}))
	defer server.Close()
	defer unblock()
	client := server.Client()
	client.Timeout = 30 * time.Second
	client.Jar, _ = cookiejar.New(nil)
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	login := func() string {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/session", strings.NewReader(`{"username":"admin","password":"private-live-password"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", f.h.origin)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("login status=%d", response.StatusCode)
		}
		var session struct {
			CSRF string `json:"csrf_token"`
		}
		if err := json.NewDecoder(response.Body).Decode(&session); err != nil {
			t.Fatal(err)
		}
		return session.CSRF
	}
	csrf := login()
	var oldCookie string
	for _, cookie := range client.Jar.Cookies(base) {
		if cookie.Name == sessionCookie {
			oldCookie = cookie.Value
		}
	}
	if oldCookie == "" {
		t.Fatal("login did not establish a secure session")
	}
	logout, _ := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/session", nil)
	logout.Header.Set("Origin", f.h.origin)
	logout.Header.Set("X-CSRF-Token", csrf)
	logoutResult := make(chan error, 1)
	go func() {
		response, err := client.Do(logout)
		if err == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				err = errors.New("logout did not revoke the original session")
			}
		}
		logoutResult <- err
	}()
	select {
	case <-revoked:
	case <-time.After(30 * time.Second):
		t.Fatal("logout did not reach the server")
	}
	if _, err := f.h.auth.Authenticate(context.Background(), oldCookie); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("original session not revoked: %v", err)
	}
	login()
	unblock()
	if err := <-logoutResult; err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL + "/api/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("late logout response cleared the newer session: /me status=%d", response.StatusCode)
	}
}
