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
	"orbitbeacon/internal/server/ingest"
)

// Decode as a consumer: all transports must carry the same typed snapshot,
// rather than a transport-specific envelope or a fabricated empty failure.
func decodeTransportSnapshot(t *testing.T, body []byte) ingest.PublicPage {
	t.Helper()
	var frame struct {
		Type                  string             `json:"type"`
		ServerTime            time.Time          `json:"server_time"`
		UpdateIntervalSeconds int                `json:"update_interval_seconds"`
		Data                  *ingest.PublicPage `json:"data"`
		Code                  string             `json:"code"`
	}
	if err := json.Unmarshal(body, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "snapshot" || frame.ServerTime.IsZero() || frame.UpdateIntervalSeconds < 2 || frame.UpdateIntervalSeconds > 60 || frame.Data == nil || frame.Code != "" {
		t.Fatalf("unusable snapshot frame: %s", body)
	}
	if frame.Data.Nodes == nil || frame.Data.HasMore || frame.Data.NextOffset != nil || frame.Data.GeneratedAt.IsZero() {
		t.Fatalf("empty inventory lost its valid pagination/time contract: %s", body)
	}
	return *frame.Data
}

func TestPublicTransportHTTPConsumerFrame(t *testing.T) {
	f := newStreamFixture(t, false)
	recorder := httptest.NewRecorder()
	f.h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "https://beacon.test/api/public/v1/nodes", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	page := decodeTransportSnapshot(t, recorder.Body.Bytes())
	if len(page.Nodes) != 0 {
		t.Fatalf("unexpected nodes: %+v", page.Nodes)
	}
}

func TestPublicTransportSSEConsumerFrame(t *testing.T) {
	f := newStreamFixture(t, false)
	recorder := newStreamRecorder()
	cancel, done := startPublicStream(t, f.h, recorder)
	defer func() { cancel(); waitStreamDone(t, done) }()
	text := nextFlush(t, recorder)
	var event, data string
	for _, line := range strings.Split(text, "\n") {
		if value, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(value)
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data += strings.TrimSpace(value)
		}
	}
	if event != "message" {
		t.Fatalf("event=%q; shared live clients require message", event)
	}
	decodeTransportSnapshot(t, []byte(data))
}

func TestPublicTransportWebSocketUpgradeAndOrigin(t *testing.T) {
	f := newStreamFixture(t, false)
	f.h.origin = "https://beacon.test"
	server := httptest.NewServer(f.h)
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/public/v1/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://beacon.test"}}})
	if err != nil {
		if response != nil {
			t.Fatalf("real wrapped upgrade status=%d: %v", response.StatusCode, err)
		}
		t.Fatal(err)
	}
	defer connection.CloseNow()
	kind, body, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if kind != websocket.MessageText {
		t.Fatalf("message type=%v", kind)
	}
	decodeTransportSnapshot(t, body)
	connection.CloseNow()
	for _, origin := range []string{"", "http://beacon.test", "https://beacon.test.evil", "https://beacon.test/"} {
		opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
		if origin != "" {
			opts.HTTPHeader.Set("Origin", origin)
		}
		other, denied, err := websocket.Dial(ctx, url, opts)
		if other != nil {
			other.CloseNow()
			t.Fatalf("accepted foreign Origin %q", origin)
		}
		if err == nil || denied == nil || denied.StatusCode != http.StatusForbidden {
			t.Fatalf("Origin %q status=%v err=%v", origin, denied, err)
		}
	}
}
