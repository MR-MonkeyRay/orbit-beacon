package server

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublicLiveOrdinaryFrameRaceNeverSendsOldDataOrSafetyRenew(t *testing.T) {
	f := newStreamFixture(t, true)
	base := f.h.now()
	var calls atomic.Int64
	f.h.now = func() time.Time {
		if calls.Add(1) == 2 {
			f.store.mu.Lock()
			f.store.name = "Current public label"
			f.store.mu.Unlock()
			f.h.nodes.InvalidatePublic()
		}
		return base
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := reserveLiveTestStream(t, f.h, "ordinary-egress")
	f.h.runPublicLive(ctx, ctx, stream, 50, 0, func(_ context.Context, body []byte) error {
		var frame struct {
			Type string `json:"type"`
			Code string `json:"code"`
		}
		if err := json.Unmarshal(body, &frame); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "Public node") {
			t.Fatalf("known old public text escaped: %s", body)
		}
		if frame.Type == "close" && frame.Code == "renew" {
			t.Fatalf("ordinary data race was treated as safety withdrawal: %s", body)
		}
		cancel()
		return nil
	})
}
