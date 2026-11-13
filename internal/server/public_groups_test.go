package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/ingest"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/services"
	"orbitbeacon/internal/server/validation"
	"orbitbeacon/internal/timekey"
)

func TestPublicGroupsSnapshotFrameAndChangeDetection(t *testing.T) {
	names := []string{"First", "Second"}
	var index *int
	page := ingest.PublicPage{GeneratedAt: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), NodeGroups: &names, ServiceGroups: &names, Nodes: []ingest.PublicNode{{Name: "Node", Group: &index}}, Services: []services.PublicService{{Name: "Monitor", Group: &index}}}
	h := &handler{now: func() time.Time { return time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC) }}
	body, err := json.Marshal(snapshotFrame(h, page))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"type":"snapshot"`, `"node_groups":["First","Second"]`, `"service_groups":["First","Second"]`, `"group":null`} {
		if !strings.Contains(string(body), key) {
			t.Fatalf("missing %s: %s", key, body)
		}
	}
	changed := page
	renamed := []string{"Renamed", "Second"}
	changed.NodeGroups = &renamed
	if publicPagesEqual(page, changed) {
		t.Fatal("node group rename invisible to live comparison")
	}
	changed = page
	changed.ServiceGroups = &renamed
	if publicPagesEqual(page, changed) {
		t.Fatal("service group rename invisible to live comparison")
	}
	changed = page
	changed.NodeGroups = nil
	changed.ServiceGroups = nil
	if publicPagesEqual(page, changed) {
		t.Fatal("toggle invisible to live comparison")
	}
	changed = page
	changed.Nodes = append([]ingest.PublicNode{}, page.Nodes...)
	mapped := new(0)
	changed.Nodes[0].Group = &mapped
	if publicPagesEqual(page, changed) {
		t.Fatal("membership invisible to live comparison")
	}
	changed = page
	changed.GeneratedAt = page.GeneratedAt.Add(time.Second)
	if !publicPagesEqual(page, changed) {
		t.Fatal("transport time alone changed page")
	}
}

func patchPublicGroupsForTest(t *testing.T, h *handler, body string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/settings/public-page", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(audit.WithSystemActor(r.Context(), "groups-test"))
	w := httptest.NewRecorder()
	h.publicPageSettings(w, r, "groups-test")
	if w.Code != 200 {
		t.Fatalf("settings patch: %d %s", w.Code, w.Body)
	}
}

func TestPublicGroupsLiveSSEAndWebSocketMutations(t *testing.T) {
	for _, transport := range []string{"sse", "ws"} {
		t.Run(transport, func(t *testing.T) {
			f := newConsoleLiveFixture(t)
			h := f.h
			h.streamInterval = 20 * time.Millisecond
			h.streamHeartbeat = time.Hour
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			a, err := h.management.SaveGroup(ctx, "", "First")
			if err != nil {
				t.Fatal(err)
			}
			b, err := h.management.SaveGroup(ctx, "", "Second")
			if err != nil {
				t.Fatal(err)
			}
			n, err := h.nodes.CreatePending(ctx, nodes.CreateRequest{Name: "Node"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE nodes SET status='unknown',token_hash=?,first_enrolled_at=?,public_settings='{"enabled":true,"fields":[]}',group_id=? WHERE id=?`, []byte(strings.Repeat("x", 32)), timekey.Format(h.now()), a.ID, n.ID); err != nil {
				t.Fatal(err)
			}
			sg, err := h.services.SaveGroup(ctx, "", "Monitors")
			if err != nil {
				t.Fatal(err)
			}
			name, kind, target, yes := "Health", "tcp", "localhost:443", true
			monitor, err := h.services.Save(ctx, "", services.Patch{Name: &name, Type: &kind, Target: &target, Public: &yes, GroupID: json.RawMessage(`"` + sg.ID + `"`)})
			if err != nil {
				t.Fatal(err)
			}
			patchPublicGroupsForTest(t, h, `{"show_groups":true}`)
			serviceNames := []string{"Monitors"}
			var base string
			if transport == "ws" {
				s := httptest.NewServer(h)
				defer s.Close()
				base = "ws" + strings.TrimPrefix(s.URL, "http") + "/api/public/v1/ws"
			}
			connect := func() (func() map[string]json.RawMessage, func()) {
				t.Helper()
				if transport == "sse" {
					w := newStreamRecorder()
					stop, done := startPublicStream(t, h, w)
					return func() map[string]json.RawMessage { return lastLiveTestFrame(t, nextFlush(t, w)) }, func() { stop(); waitStreamDone(t, done) }
				}
				connection, _, err := websocket.Dial(ctx, base, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {h.origin}}})
				if err != nil {
					t.Fatal(err)
				}
				return func() map[string]json.RawMessage {
					_, body, err := connection.Read(ctx)
					if err != nil {
						t.Fatal(err)
					}
					var frame map[string]json.RawMessage
					if err := json.Unmarshal(body, &frame); err != nil {
						t.Fatal(err)
					}
					return frame
				}, func() { _ = connection.CloseNow() }
			}
			read, closeStream := connect()
			defer func() { closeStream() }()
			check := func(names []string, grouped bool) {
				t.Helper()
				frame := read()
				if string(frame["type"]) != `"snapshot"` {
					t.Fatalf("expected snapshot: %s", objectJSON(t, frame))
				}
				raw := frame["data"]
				var page map[string]json.RawMessage
				if err := json.Unmarshal(raw, &page); err != nil {
					t.Fatal(err)
				}
				if grouped {
					var actual []string
					if err := json.Unmarshal(page["node_groups"], &actual); err != nil || !reflect.DeepEqual(actual, names) {
						t.Fatalf("node groups=%s want=%v err=%v", page["node_groups"], names, err)
					}
					var actualServices []string
					if err := json.Unmarshal(page["service_groups"], &actualServices); err != nil || !reflect.DeepEqual(actualServices, serviceNames) {
						t.Fatalf("service groups=%s want=%v err=%v", page["service_groups"], serviceNames, err)
					}
					if page["service_groups"] == nil || !strings.Contains(string(raw), `"group":`) {
						t.Fatalf("missing group projection: %s", raw)
					}
				} else {
					if page["node_groups"] != nil || page["service_groups"] != nil || strings.Contains(string(raw), `"group":`) {
						t.Fatalf("off group projection: %s", raw)
					}
				}
				// Receiving a frame may precede completion of the server write.
				// Leave the writer idle before the next safety mutation.
				for _, stream := range h.streams.snapshot(false) {
					for {
						stream.mu.Lock()
						writing := stream.writing
						stream.mu.Unlock()
						if !writing {
							break
						}
						select {
						case <-ctx.Done():
							t.Fatal("writer did not become idle")
						case <-time.After(time.Millisecond):
						}
					}
				}
				for _, secret := range []string{a.ID, b.ID, n.ID, sg.ID, monitor.ID, "group_id"} {
					if strings.Contains(string(raw), secret) {
						t.Fatalf("ID leaked: %s", raw)
					}
				}
			}
			renew := func() {
				t.Helper()
				frame := read()
				if string(frame["type"]) != `"close"` || string(frame["code"]) != `"renew"` {
					t.Fatalf("expected renewal: %s", objectJSON(t, frame))
				}
				closeStream()
				read, closeStream = connect()
			}
			check([]string{"First"}, true)
			if _, err := h.management.SaveGroup(ctx, a.ID, "Renamed"); err != nil {
				t.Fatal(err)
			}
			check([]string{"Renamed"}, true)
			// Both groups have a public node, making console-order changes observable.
			second, err := h.nodes.CreatePending(ctx, nodes.CreateRequest{Name: "Other"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE nodes SET status='unknown',token_hash=?,first_enrolled_at=?,public_settings='{"enabled":true,"fields":[]}',group_id=? WHERE id=?`, []byte(strings.Repeat("y", 32)), timekey.Format(h.now()), b.ID, second.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := h.management.ReorderGroups(ctx, []string{b.ID, a.ID}); err != nil {
				t.Fatal(err)
			}
			check([]string{"Second", "Renamed"}, true)
			if _, err := h.nodes.PatchMetadata(ctx, n.ID, nodes.MetadataPatch{GroupID: json.RawMessage(`"` + b.ID + `"`)}); err != nil {
				t.Fatal(err)
			}
			check([]string{"Second"}, true)
			if err := h.management.DeleteGroup(ctx, b.ID); err != nil {
				t.Fatal(err)
			}
			check([]string{}, true)
			if _, err := h.services.SaveGroup(ctx, sg.ID, "Renamed monitors"); err != nil {
				t.Fatal(err)
			}
			serviceNames = []string{"Renamed monitors"}
			renew()
			check([]string{}, true)
			otherGroup, err := h.services.SaveGroup(ctx, "", "Other monitors")
			if err != nil {
				t.Fatal(err)
			}
			renew()
			check([]string{}, true)
			_, err = h.services.Save(ctx, "", services.Patch{Name: &name, Type: &kind, Target: &target, Public: &yes, GroupID: json.RawMessage(`"` + otherGroup.ID + `"`)})
			if err != nil {
				t.Fatal(err)
			}
			serviceNames = []string{"Other monitors", "Renamed monitors"}
			renew()
			check([]string{}, true)
			if _, err := h.services.ReorderGroups(ctx, []string{sg.ID, otherGroup.ID}); err != nil {
				t.Fatal(err)
			}
			serviceNames = []string{"Renamed monitors", "Other monitors"}
			renew()
			check([]string{}, true)
			if _, err := h.services.Save(ctx, monitor.ID, services.Patch{GroupID: json.RawMessage(`"` + otherGroup.ID + `"`)}); err != nil {
				t.Fatal(err)
			}
			serviceNames = []string{"Other monitors"}
			renew()
			check([]string{}, true)
			if err := h.services.DeleteGroup(ctx, sg.ID); err != nil {
				t.Fatal(err)
			}
			renew()
			check([]string{}, true)
			patchPublicGroupsForTest(t, h, `{"show_groups":false}`)
			renew()
			check(nil, false)
			patchPublicGroupsForTest(t, h, `{"show_groups":true}`)
			renew()
			check([]string{}, true)
		})
	}
}

func TestPublicPageSettingsPatchJSON(t *testing.T) {
	for _, tc := range []struct{ body, field string }{
		{`{"enabled":true}`, ""}, {`{"show_groups":true}`, ""}, {`{"enabled":false,"show_groups":false}`, ""},
		{`{}`, "enabled"}, {`{"enabled":null}`, "enabled"}, {`{"show_groups":null}`, "show_groups"},
		{`{"enabled":0}`, "enabled"}, {`{"show_groups":"true"}`, "show_groups"}, {`{"show_groups":[]}`, "show_groups"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			var patch publicPageSettingsInput
			err := json.Unmarshal([]byte(tc.body), &patch)
			if tc.field == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var fields *validation.Error
			if !errors.As(err, &fields) || fields.Fields[tc.field] == "" {
				t.Fatalf("expected %s field error, got %v", tc.field, err)
			}
		})
	}
	var patch publicPageSettingsInput
	if err := json.Unmarshal([]byte(`{"unknown":true}`), &patch); err == nil {
		t.Fatal("unknown key accepted")
	}
}
