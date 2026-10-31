package nodes_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

func TestManagementPresenceAndAgentVersion(t *testing.T) {
	service, db := newNodes(t, 20)
	ctx := context.Background()
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, tc := range []struct {
		name, status string
		credential   bool
		seen         *time.Time
		presence     string
	}{
		{"pending", "pending", false, nil, "pending"},
		{"revoked", "unknown", false, &now, "pending"},
		{"disabled pending", "disabled", false, nil, "disabled"},
		{"disabled enrolled", "disabled", true, &now, "disabled"},
		{"never", "unknown", true, nil, "never"},
		{"online", "unknown", true, &now, "online"},
		{"window boundary", "unknown", true, timePointer(now.Add(-nodes.OnlineWindow)), "online"},
		{"offline", "unknown", true, timePointer(now.Add(-nodes.OnlineWindow - time.Nanosecond)), "offline"},
		{"future", "unknown", true, timePointer(now.Add(time.Second)), "offline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := pending(t, service, tc.name)
			var credential any
			if tc.credential {
				credential = []byte(strings.Repeat(node.ID, 2)[:32])
			}
			var seen any
			if tc.seen != nil {
				seen = timekey.Format(*tc.seen)
			}
			_, err := db.Exec(`UPDATE nodes SET status=?, token_hash=?, last_seen=?, system=? WHERE id=?`, tc.status, credential, seen, `{"hostname":"private","agent_version":"v1.2.3+test"}`, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := service.Get(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Presence != tc.presence || got.Status != tc.status {
				t.Fatalf("node = %+v; want presence=%s status=%s", got, tc.presence, tc.status)
			}
			if got.LastSeen == nil != (tc.seen == nil) || got.LastSeen != nil && !got.LastSeen.Equal(*tc.seen) {
				t.Fatalf("last_seen=%v want %v", got.LastSeen, tc.seen)
			}
			if got.AgentVersion != "v1.2.3+test" {
				t.Fatalf("agent_version=%q", got.AgentVersion)
			}
			list, _, err := service.ListPage(ctx, 100, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, listed := range list {
				if listed.ID == got.ID && listed.Presence != got.Presence {
					t.Fatalf("list/get disagree: %+v %+v", listed, got)
				}
			}
		})
	}
	node := pending(t, service, "wire fields")
	encoded, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"last_seen":null`, `"presence":"pending"`, `"agent_version":""`} {
		if !strings.Contains(string(encoded), field) {
			t.Errorf("missing %s: %s", field, encoded)
		}
	}
}

func timePointer(t time.Time) *time.Time { return &t }

func TestListPageLookaheadAndValidation(t *testing.T) {
	service, _ := newNodes(t, 110)
	ctx := context.Background()
	for i := 0; i < 101; i++ {
		pending(t, service, "page")
	}
	first, more, err := service.ListPage(ctx, 100, 0)
	if err != nil || len(first) != 100 || !more {
		t.Fatalf("first: count=%d more=%v err=%v", len(first), more, err)
	}
	last, more, err := service.ListPage(ctx, 100, 100)
	if err != nil || len(last) != 1 || more {
		t.Fatalf("last: count=%d more=%v err=%v", len(last), more, err)
	}
	if first[99].ID == last[0].ID {
		t.Fatal("lookahead row repeated")
	}
	empty, more, err := service.ListPage(ctx, 100, 101)
	if err != nil || empty == nil || len(empty) != 0 || more {
		t.Fatalf("empty: %+v %v %v", empty, more, err)
	}
	for _, args := range [][2]int{{0, 0}, {101, 0}, {1, -1}} {
		if _, _, err := service.ListPage(ctx, args[0], args[1]); !errors.Is(err, nodes.ErrInvalidInput) {
			t.Fatalf("ListPage%v err=%v", args, err)
		}
	}
}

func TestManagementStatusToleratesUnreadableStoredAgentData(t *testing.T) {
	service, db := newNodes(t, 5)
	ctx := context.Background()
	node := pending(t, service, "unreadable")
	if _, err := db.Exec(`UPDATE nodes SET system='[' WHERE id=?`, node.ID); err != nil {
		t.Fatal(err)
	}
	got, err := service.Get(ctx, node.ID)
	if err != nil {
		t.Fatalf("unreadable stored agent data broke the node API: %v", err)
	}
	if got.AgentVersion != "" {
		t.Fatalf("agent_version=%q", got.AgentVersion)
	}
}
