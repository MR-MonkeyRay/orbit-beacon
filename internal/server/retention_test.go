package server_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/notify"
	"orbitbeacon/internal/timekey"
)

func TestRetentionDrainsAlertAndNotificationBacklogs(t *testing.T) {
	f := newHTTP(t, 1)
	h, err := server.NewHandler(server.Options{
		Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL,
		Notifications: notify.New(f.db, nil, notify.Options{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	old := timekey.Format(now.Add(-100 * 24 * time.Hour))
	recent := timekey.Format(now)
	const backlog = 1201
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO notification_channels(id,name,kind,enabled,config_json,created_at,updated_at)
 VALUES('channel','channel','webhook',1,'{}',?,?)`, old, old); err != nil {
		t.Fatal(err)
	}
	for i := range backlog + 2 {
		state, deliveryState, updated := "resolved", "sent", old
		var resolved any = old
		if i == backlog {
			state, resolved, deliveryState = "firing", nil, "pending"
		} else if i == backlog+1 {
			resolved, updated = recent, recent
		} else if i%2 == 1 {
			deliveryState = "failed"
		}
		id := fmt.Sprintf("event-%04d", i)
		if _, err := tx.Exec(`INSERT INTO alert_events(id,rule_id,node_id,state,started_at,resolved_at,rule_name,node_name,rule_snapshot)
 VALUES(?,'deleted-rule','deleted-node',?,?,?,'rule','node','{}')`, id, state, old, resolved); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO notification_deliveries(id,channel_id,event_id,state,next_attempt_at,payload_json,created_at,updated_at)
 VALUES(?,'channel',?,?,?,'{}',?,?)`, id, id, deliveryState, old, old, updated); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	result, err := h.CleanupRetention(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if result.AlertEvents != backlog || result.NotificationDeliveries != backlog {
		t.Fatalf("single retention pass did not drain backlogs: %+v", result)
	}
	for _, table := range []string{"alert_events", "notification_deliveries"} {
		var remaining int
		if err := f.db.QueryRow("SELECT count(*) FROM " + table).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 2 {
			t.Errorf("%s remaining=%d, want only the active and recent rows", table, remaining)
		}
	}
}

func TestRetentionDeletesOnlyProbeHourlyRowsOlderThanSevenDays(t *testing.T) {
	f := newHTTP(t, 1)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if _, err := f.db.Exec(`INSERT INTO nodes(id,name,created_at) VALUES('probe-node','probe-node',?)`, timekey.Format(now)); err != nil {
		t.Fatal(err)
	}
	for _, hour := range []time.Time{now.Add(-8 * 24 * time.Hour), now.Add(-7 * 24 * time.Hour), now.Add(-6 * 24 * time.Hour)} {
		if _, err := f.db.Exec(`INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms) VALUES('probe-node','tcp://host:80',?,1,0,1)`, timekey.Format(hour)); err != nil {
			t.Fatal(err)
		}
	}
	h, err := server.NewHandler(server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL})
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.CleanupRetention(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if result.ProbeRows != 1 {
		t.Fatalf("removed probe rows=%d, want 1", result.ProbeRows)
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM probe_hourly WHERE node_id='probe-node'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("remaining probe rows=%d err=%v", count, err)
	}
}
