package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLatePasswordResponseDoesNotClearNewSessionCookie(t *testing.T) {
	f := newConsoleLiveFixture(t)
	committed, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/me/password" {
			f.h.ServeHTTP(w, r)
			return
		}
		buffer := httptest.NewRecorder()
		f.h.ServeHTTP(buffer, r)
		close(committed)
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
	login := func(password string) string {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/session", strings.NewReader(`{"username":"admin","password":"`+password+`"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", f.h.origin)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("login:%d", response.StatusCode)
		}
		var session struct {
			CSRF string `json:"csrf_token"`
		}
		if err := json.NewDecoder(response.Body).Decode(&session); err != nil {
			t.Fatal(err)
		}
		return session.CSRF
	}
	csrf := login("private-live-password")
	request, _ := http.NewRequest(http.MethodPatch, server.URL+"/api/v1/me/password", strings.NewReader(`{"old_password":"private-live-password","new_password":"changed-private-live-password"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", f.h.origin)
	request.Header.Set("X-CSRF-Token", csrf)
	type result struct {
		status  int
		cookies []string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		response, err := client.Do(request)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer response.Body.Close()
		done <- result{status: response.StatusCode, cookies: response.Header.Values("Set-Cookie")}
	}()
	select {
	case <-committed:
	case <-time.After(30 * time.Second):
		t.Fatal("password transaction did not complete")
	}
	login("changed-private-live-password")
	unblock()
	changed := <-done
	if changed.err != nil || changed.status != 204 || len(changed.cookies) != 0 {
		t.Fatalf("password response:%+v", changed)
	}
	response, err := client.Get(server.URL + "/api/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("late password response cleared newer session: %d", response.StatusCode)
	}
}
