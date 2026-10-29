package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestLiveWebSocketAcceptsExactIPv6OriginBehindChangedHost(t *testing.T) {
	public := newStreamFixture(t, false)
	private := newConsoleLiveFixture(t)
	for _, fixture := range []struct {
		name   string
		h      *handler
		path   string
		cookie *http.Cookie
	}{
		{"public", public.h, "/api/public/v1/ws", nil},
		{"private", private.h, "/api/v1/live/ws", private.cookie},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			fixture.h.origin = "https://[2001:db8::1]:8443"
			server := httptest.NewServer(fixture.h)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			headers := http.Header{"Origin": {fixture.h.origin}}
			if fixture.cookie != nil {
				headers.Set("Cookie", fixture.cookie.String())
			}
			connection, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+fixture.path, &websocket.DialOptions{HTTPHeader: headers})
			if err != nil {
				t.Fatalf("trusted IPv6 origin rejected behind changed upstream Host: response=%v err=%v", response, err)
			}
			defer connection.CloseNow()
			_, body, err := connection.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var frame struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(body, &frame); err != nil {
				t.Fatal(err)
			}
			if frame.Type != "snapshot" {
				t.Fatalf("first frame=%s, want snapshot", body)
			}
		})
	}
}
