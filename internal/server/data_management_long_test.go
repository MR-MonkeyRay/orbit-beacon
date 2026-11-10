package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

func TestDataManagementCompactionOutlivesOrdinaryWriteDeadline(t *testing.T) {
	f := newHTTP(t, 1)
	cookie, csrf := login(t, f, "admin")
	lease, err := f.nodes.Database().Acquire(t.Context(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	entered := make(chan struct{})
	httpServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		f.handler.ServeHTTP(w, r)
	}))
	httpServer.Config.WriteTimeout = 50 * time.Millisecond
	httpServer.Start()
	defer httpServer.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpServer.URL+"/api/v1/data-management/compact", strings.NewReader(`{"domain":"main","confirmation":"COMPACT"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(cookie)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", publicURL)
	request.Header.Set("X-CSRF-Token", csrf)
	done := make(chan error, 1)
	go func() {
		response, err := httpServer.Client().Do(request)
		if err == nil {
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(response.Body)
				t.Errorf("compaction HTTP status=%d body=%s", response.StatusCode, body)
			}
		}
		done <- err
	}()
	<-entered
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatalf("ordinary HTTP write deadline interrupted maintenance: %v", err)
	}
}
