package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStorageRechecksRevocationAfterWaitingForTransportSlot(t *testing.T) {
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var calls atomic.Int32
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		entered <- struct{}{}
		<-release
		w.WriteHeader(204)
	}))
	defer receiver.Close()
	s := newSender(Options{Now: time.Now, AllowPrivateNetworks: true})
	defer s.client.CloseIdleConnections()
	trustTestServer(s, receiver)
	channel := Channel{Kind: "webhook", Config: Config{URL: receiver.URL}}
	var alerts sync.WaitGroup
	for range 4 {
		alerts.Add(1)
		go func() { defer alerts.Done(); s.Send(t.Context(), channel, "", Payload{}) }()
	}
	for range 4 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			alerts.Wait()
			t.Fatal("transport slots did not fill")
		}
	}
	var allowed atomic.Bool
	allowed.Store(true)
	done := make(chan Result, 1)
	go func() {
		done <- s.sendStorage(t.Context(), channel, "", storageTestPayload(), func(context.Context) (bool, error) { return allowed.Load(), nil })
	}()
	allowed.Store(false)
	close(release)
	alerts.Wait()
	select {
	case result := <-done:
		if result.OK {
			t.Fatal("revoked queued storage delivery sent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revoked storage sender did not leave slot wait")
	}
	if calls.Load() != 4 {
		t.Fatalf("revoked destination received %d requests", calls.Load())
	}
}
