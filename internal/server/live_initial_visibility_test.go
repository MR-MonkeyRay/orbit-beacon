package server

import (
	"sync"
	"testing"
	"time"
)

func TestPublicLiveSafetyChangeCancelsBlockedInitialProjection(t *testing.T) {
	f := newStreamFixture(t, false)
	f.h.streamLifetime = time.Hour
	entered := make(chan struct{})
	var once sync.Once
	f.store.onQuery = func() { once.Do(func() { close(entered) }) }
	f.store.queryWait = make(chan struct{})
	w := newStreamRecorder()
	cancel, done := startPublicStream(t, f.h, w)
	defer func() { cancel(); waitStreamDone(t, done) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("initial public projection did not start")
	}
	f.h.nodes.InvalidatePublicSafety()
	frame := lastLiveTestFrame(t, nextFlush(t, w))
	if string(frame["type"]) != `"close"` || string(frame["code"]) != `"renew"` || frame["data"] != nil {
		t.Fatalf("blocked initial projection did not renew without data: %s", w.Body())
	}
	waitStreamDone(t, done)
}
