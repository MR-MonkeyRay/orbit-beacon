package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
)

func TestHealthEndpointsExposeLivenessAndReadinessWithoutAuthentication(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	handler, err := NewHandler(Options{
		Auth:      auth.NewService(db, auth.Options{}),
		Nodes:     nodes.NewService(db, nodes.Options{}),
		PublicURL: "https://beacon.example.test",
		Assets:    fstest.MapFS{"200.html": &fstest.MapFile{Data: []byte("shell")}, "public.html": &fstest.MapFile{Data: []byte("public shell")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "https://beacon.example.test"+path, nil))
		return recorder
	}
	if response := request("/healthz/live"); response.Code != http.StatusOK || response.Body.String() != `{"status":"ok"}
` {
		t.Fatalf("liveness response=%d %q", response.Code, response.Body.String())
	}
	if response := request("/healthz/ready"); response.Code != http.StatusOK || response.Body.String() != `{"status":"ready"}
` {
		t.Fatalf("readiness response=%d %q", response.Code, response.Body.String())
	}
	statuses := make(chan int, 20)
	var wait sync.WaitGroup
	for range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			statuses <- request("/healthz/ready").Code
		}()
	}
	shutdown := make(chan error, 1)
	go func() { shutdown <- handler.Shutdown(context.Background()) }()
	wait.Wait()
	close(statuses)
	if err := <-shutdown; err != nil {
		t.Fatal(err)
	}
	for status := range statuses {
		if status != http.StatusOK && status != http.StatusServiceUnavailable {
			t.Fatalf("concurrent readiness status=%d", status)
		}
	}
	if response := request("/healthz/live"); response.Code != http.StatusOK {
		t.Fatalf("liveness after shutdown=%d", response.Code)
	}
	if response := request("/healthz/ready"); response.Code != http.StatusServiceUnavailable || response.Body.String() != `{"status":"not_ready"}
` {
		t.Fatalf("readiness after shutdown=%d %q", response.Code, response.Body.String())
	}
}
