package server_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server"
	"orbitbeacon/internal/server/backup"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/notify"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/timekey"
)

func TestRetentionDrainsAlertAndNotificationBacklogs(t *testing.T) {
	f := newHTTP(t, 1)
	h, err := server.NewHandler(testHandlerOptions(t, server.Options{
		Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL,
		Notifications: notify.New(f.db, nil, notify.Options{}),
	}))
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
	h, err := server.NewHandler(testHandlerOptions(t, server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL}))
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

func TestRetentionDrainsInboxAndBackupHistoryPreservingActivePasswords(t *testing.T) {
	f := newHTTP(t, 1)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	old := now.Add(-100 * 24 * time.Hour)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{9}, 32)
	if err := os.WriteFile(filepath.Join(dir, "secrets.key"), key, 0600); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(key, "orbitbeacon backup secrets v1")
	if err != nil {
		t.Fatal(err)
	}
	backups, err := backup.New(f.db, backup.Options{DataDir: dir, SecretBox: box})
	if err != nil {
		t.Fatal(err)
	}
	h, err := server.NewHandler(testHandlerOptions(t, server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL, Backups: backups}))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var userID string
	if err := tx.QueryRow("SELECT id FROM users WHERE username='admin-two'").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	const backlog = 1201
	for i := range backlog {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		if err := inbox.Insert(context.Background(), tx, inbox.Event{ID: id, Kind: "task", SourceID: id, Transition: "succeeded", Severity: "info", CreatedAt: old}); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO inbox_reads(user_id,notification_id,read_at) VALUES(?,?,?)", userID, id, timekey.Format(old)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO backup_tasks(id,kind,state,categories_json,destination_json,created_at,finished_at,password_ciphertext,password_expires_at) VALUES(?,'backup_remote','succeeded','["configuration"]','{}',?,?,X'01',?)`, id, timekey.Format(old), timekey.Format(old), timekey.Format(old.Add(90*24*time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range []string{"queued", "running"} {
		if _, err := tx.Exec(`INSERT INTO backup_tasks(id,kind,state,categories_json,destination_json,created_at,password_ciphertext,password_expires_at) VALUES(?,'backup_export',?,'["configuration"]','{}',?,X'01',?)`, state, state, timekey.Format(old), timekey.Format(old)); err != nil {
			t.Fatal(err)
		}
	}
	if err := inbox.Insert(context.Background(), tx, inbox.Event{ID: inboxAlertID, Kind: "alert", SourceID: inboxAlertID, Transition: "firing", Severity: "warning", CreatedAt: now.Add(-90 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	result, err := h.CleanupRetention(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if result.InboxNotifications != backlog || result.BackupTasks < backlog {
		t.Fatalf("undrained backlog: %+v", result)
	}
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"SELECT count(*) FROM inbox_notifications", 1},
		{"SELECT count(*) FROM inbox_reads", 0},
		{"SELECT count(*) FROM backup_tasks", 2},
		{"SELECT count(*) FROM backup_tasks WHERE state IN ('queued','running') AND password_ciphertext IS NOT NULL", 2},
	} {
		var got int
		if err := f.db.QueryRow(tc.query).Scan(&got); err != nil || got != tc.want {
			t.Fatalf("%s: got=%d want=%d err=%v", tc.query, got, tc.want, err)
		}
	}
}

func TestRetentionSweepsOnlyOwnedOrphanExportsAndLocalTemps(t *testing.T) {
	f := newHTTP(t, 1)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, 32)
	if err := os.WriteFile(filepath.Join(dir, "secrets.key"), key, 0600); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(key, "orbitbeacon backup secrets v1")
	if err != nil {
		t.Fatal(err)
	}
	backups, err := backup.New(f.db, backup.Options{DataDir: dir, SecretBox: box})
	if err != nil {
		t.Fatal(err)
	}
	h, err := server.NewHandler(testHandlerOptions(t, server.Options{Auth: f.auth, Nodes: f.nodes, PublicURL: publicURL, Backups: backups}))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	const planID = "00000000-0000-4000-8000-000000000003"
	const taskID = "00000000-0000-4000-8000-000000000004"
	const orphanID = "00000000-0000-4000-8000-000000000005"
	orphanName := "orbitbeacon-20261027T120000Z-" + orphanID + ".tar.gz.age"
	name := "orbitbeacon-20261027T120000Z-" + taskID + ".tar.gz.age"
	orphanDir := filepath.Join(dir, "exports", orphanID)
	localDir := filepath.Join(dir, "orbitbeacon-"+planID)
	for _, path := range []string{orphanDir, localDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	orphan := filepath.Join(orphanDir, orphanName)
	temporary := filepath.Join(localDir, "."+name+".tmp")
	saved := filepath.Join(localDir, name)
	foreign := filepath.Join(orphanDir, "unrelated.txt")
	for _, path := range []string{orphan, temporary, saved, foreign} {
		if err := os.WriteFile(path, []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	destination := fmt.Sprintf(`{"target":{"path":%q}}`, dir)
	if _, err := f.db.Exec(`INSERT INTO backup_tasks(id,kind,state,plan_id,categories_json,destination_json,created_at,finished_at,artifact_name,restored) VALUES(?,'backup_local','succeeded',?,'["configuration"]',?,?,?,?,0)`, taskID, planID, destination, timekey.Format(now), timekey.Format(now), name); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO backup_tasks(id,kind,state,categories_json,destination_json,created_at,finished_at,artifact_name,restored) VALUES(?,'backup_export','succeeded','["configuration"]','{}',?,?,?,1)`, orphanID, timekey.Format(now), timekey.Format(now), orphanName); err != nil {
		t.Fatal(err)
	}
	result, err := h.CleanupRetention(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if result.BackupOrphanFiles != 1 || result.BackupLocalTemps != 1 {
		t.Fatalf("sweep counts=%+v", result)
	}
	for _, path := range []string{orphan, temporary} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("owned stale file remains: %s err=%v", path, err)
		}
	}
	for _, path := range []string{saved, foreign} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unowned/saved file touched: %s err=%v", path, err)
		}
	}
}
