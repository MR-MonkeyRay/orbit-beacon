package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
)

func TestDeliveryReleasesWriteGateDuringNetwork(t *testing.T) {
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	done := make(chan struct{})
	defer server.Close()
	defer func() {
		close(release)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("delivery did not finish")
		}
	}()
	f.target(t, server.URL)
	f.enqueue(t, f.note("network-gate"))
	go func() { defer close(done); f.s.deliver(context.Background(), f.channel.ID) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("delivery did not reach receiver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lease, err := f.s.db.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatalf("network send retained write gate: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseCloseCancelsInflightDeliveryWithoutSavingResult(t *testing.T) {
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	defer close(release)
	f.target(t, server.URL)
	f.enqueue(t, f.note("database-cancel"))
	delivered := make(chan struct{})
	go func() { defer close(delivered); f.s.deliver(context.Background(), f.channel.ID) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("delivery did not reach receiver")
	}
	closed := make(chan error, 1)
	go func() { closed <- f.s.db.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("database close did not cancel network work and drain its lease")
	}
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("delivery outlived database drain")
	}
	if got, attempts, _, _ := state(t, f.db); got != "pending" || attempts != 1 {
		t.Fatalf("canceled delivery saved a result: %s attempts=%d", got, attempts)
	}
}
