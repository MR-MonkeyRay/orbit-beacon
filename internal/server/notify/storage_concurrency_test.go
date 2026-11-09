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

func TestStorageAndAlertsShareFourTransportSlots(t *testing.T) {
	entered := make(chan struct{}, 8)
	var active, peak atomic.Int32
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		current := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
		}
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	defer receiver.Close()
	s := newSender(Options{Now: time.Now, AllowPrivateNetworks: true})
	defer s.client.CloseIdleConnections()
	trustTestServer(s, receiver)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			channel := Channel{Kind: "webhook", Config: Config{URL: receiver.URL}}
			if i%2 == 0 {
				s.SendStorage(ctx, channel, "", storageTestPayload())
			} else {
				s.Send(ctx, channel, "", Payload{})
			}
		}()
	}
	for range 4 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("four transport slots did not fill")
		}
	}
	select {
	case <-entered:
		t.Fatal("alert and storage senders exceeded shared limit")
	case <-time.After(100 * time.Millisecond):
	}
	if peak.Load() != 4 {
		t.Fatalf("peak concurrency=%d", peak.Load())
	}
}
