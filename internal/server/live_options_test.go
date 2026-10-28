package server_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"orbitbeacon/internal/server"
)

func TestHandlerValidatesAndAdvertisesEffectiveWebInterval(t *testing.T) {
	f := newHTTP(t, 2)
	for _, seconds := range []int{-1, 1, 61} {
		if _, err := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL, WebUpdateIntervalSeconds: seconds}); err == nil {
			t.Fatalf("accepted invalid web interval %d", seconds)
		}
	}
	for _, seconds := range []int{0, 2, 10, 30, 60} {
		h, err := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL, WebUpdateIntervalSeconds: seconds})
		if err != nil {
			t.Fatal(err)
		}
		w := request(t, h, http.MethodGet, "/api/public/v1/nodes", "", nil, "", "")
		assertHTTPStatus(t, w, http.StatusOK)
		var frame struct {
			Interval int `json:"update_interval_seconds"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &frame); err != nil {
			t.Fatal(err)
		}
		want := seconds
		if want == 0 {
			want = 10
		}
		if frame.Interval != want {
			t.Fatalf("configured=%d advertised=%d want=%d", seconds, frame.Interval, want)
		}
	}
}
