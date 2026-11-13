package storage_test

import (
	"context"
	"errors"
	"orbitbeacon/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLStandaloneTypesConstraintsSnapshot(t *testing.T) {
	f := storagetest.Start(t)
	ctx := context.Background()
	db := f.Open(t, storage.MainDomain)
	history := f.Open(t, storage.HistoryDomain)
	l, err := db.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var connectionCollation, clientCharset string
	if err = l.QueryRowContext(ctx, "SELECT @@session.collation_connection,@@session.character_set_client").Scan(&connectionCollation, &clientCharset); err != nil || connectionCollation != "utf8mb4_0900_bin" || clientCharset != "utf8mb4" {
		t.Fatalf("production connection collation=%q charset=%q error=%v", connectionCollation, clientCharset, err)
	}
	tx, err := l.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, "INSERT INTO services(id,name,type,target,enabled,public,notify,created_at,updated_at) VALUES('service','HTTP','http','https://example.com',0,0,0,'now','now')"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE services SET interval_seconds=30,timeout_seconds=30 WHERE id='service'"); err == nil {
		t.Fatal("service timeout reached its interval")
	} else if code, ok := storage.MySQLCode(err); !ok || code != 3819 {
		t.Fatalf("service timeout CHECK: %v", err)
	}
	for _, name := range []string{"Alpha", "alpha", "alpha "} {
		if _, err = tx.ExecContext(ctx, "INSERT INTO `groups`(id,name) VALUES(?,?)", name, name); err != nil {
			t.Fatal(err)
		}
	}
	installation := strings.Repeat("界", 83) + "Aa\x00x  "
	session := strings.Repeat("s", 249) + "Zz\x00y  "
	if _, err = tx.ExecContext(ctx, "INSERT INTO nodes(id,name,group_id,quota_bytes,run_generation,created_at,installation_id,report_session_id) VALUES('node','节点','Alpha',?,?,?,?,?)", "18446744073709551615", "18446744073709551615", "2026-11-07T01:02:03.123456789Z", installation, session); err != nil {
		t.Fatal(err)
	}
	target := strings.Repeat("界", 341) + "x"
	if _, err = tx.ExecContext(ctx, "INSERT INTO probe_latest(node_id,target,ok,error,checked_at) VALUES('node',?,1,'',?)", target, "2026-11-07T01:02:03.123456789Z"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms) VALUES('node',?,'2026-11-07T01:00:00.000000000Z',2,1,1.25)", target); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE probe_hourly SET failures=3 WHERE node_id='node'"); err == nil {
		t.Fatal("probe failures exceeded checks")
	} else if code, ok := storage.MySQLCode(err); !ok || code != 3819 {
		t.Fatalf("probe failure CHECK: %v", err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO notification_channels(id,name,kind,enabled,config_json,secret_ciphertext,url_ciphertext,created_at,updated_at) VALUES('channel',?,'webhook',1,'{}',?,NULL,'now','now')", strings.Repeat("界", 100), []byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO alert_rules(id,name,enabled,scope_kind,scope_id,metric,for_seconds,severity,created_at,updated_at) VALUES('rule','规则',1,'group','Alpha','offline',0,'warning','now','now')"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	committed, err := tx.ResolveCommit(ctx)
	if err != nil || !committed {
		t.Fatalf("receipt=%v err=%v", committed, err)
	}
	h, err := history.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ht, err := h.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ht.Rollback()
	if _, err = ht.ExecContext(ctx, "INSERT INTO service_check_buckets VALUES('service',1,2,1,5,2,3)"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"UPDATE service_check_buckets SET successes=3 WHERE service_id='service'",
		"UPDATE service_check_buckets SET latency_max_ms=1 WHERE service_id='service'",
	} {
		if _, err = ht.ExecContext(ctx, query); err == nil {
			t.Fatal("invalid service check aggregate accepted")
		} else if code, ok := storage.MySQLCode(err); !ok || code != 3819 {
			t.Fatalf("service aggregate CHECK: %v", err)
		}
	}
	metric := "disk_used_percent:" + strings.Repeat("x", 1024)
	if _, err = ht.ExecContext(ctx, "INSERT INTO buckets VALUES('node',?,60,1,1.25,2.5,1)", metric); err != nil {
		t.Fatal(err)
	}
	if err = ht.Commit(); err != nil {
		t.Fatal(err)
	}
	h.Close()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	image := filepath.Join(dir, "image.db")
	if err = l.Snapshot(ctx, image); err != nil {
		t.Fatal(err)
	}
	copy, err := storage.OpenReader(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	var quota, generation, stamp string
	if err = copy.QueryRow("SELECT quota_bytes,run_generation,created_at FROM nodes WHERE id='node'").Scan(&quota, &generation, &stamp); err != nil || quota != "18446744073709551615" || generation != quota || stamp != "2026-11-07T01:02:03.123456789Z" {
		t.Fatalf("types lost: %q %q %q %v", quota, generation, stamp, err)
	}
	var copiedInstallation, copiedSession string
	if err = copy.QueryRow("SELECT installation_id,report_session_id FROM nodes WHERE id='node'").Scan(&copiedInstallation, &copiedSession); err != nil || copiedInstallation != installation || copiedSession != session {
		t.Fatalf("snapshot identity bytes changed: installation=%q session=%q err=%v", copiedInstallation, copiedSession, err)
	}
	var empty, null bool
	if err = copy.QueryRow("SELECT typeof(secret_ciphertext)='blob' AND length(secret_ciphertext)=0,url_ciphertext IS NULL FROM notification_channels").Scan(&empty, &null); err != nil || !empty || !null {
		t.Fatalf("empty/null lost: %v %v %v", empty, null, err)
	}
	tx, err = l.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	command := "INSERT INTO agent_commands(id,node_id,credential_epoch,kind,state,requested_by,created_at,expires_at) VALUES(?,'node',0,'restart','pending','actor','now','later')"
	if _, err = tx.ExecContext(ctx, command, "command-one"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, command, "command-two"); !storage.IsUnique(err) {
		t.Fatalf("active command uniqueness: %v", err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE agent_commands SET state='succeeded' WHERE id='command-one'"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, command, "command-two"); err != nil {
		t.Fatal(err)
	}
	task := "INSERT INTO backup_tasks(id,kind,state,plan_id,categories_json,destination_json,scheduled_at,created_at) VALUES(?,'backup_local','queued','plan','[]','{}',?,'now')"
	if _, err = tx.ExecContext(ctx, task, "task-one", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, task, "task-two", "second"); !storage.IsUnique(err) {
		t.Fatalf("active plan uniqueness: %v", err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE backup_tasks SET state='succeeded' WHERE id='task-one'"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, task, "task-two", "second"); err != nil {
		t.Fatal(err)
	}
	event := "INSERT INTO alert_events(id,rule_id,node_id,state,started_at,rule_name,node_name,rule_snapshot) VALUES(?,'rule','node','firing','now','rule','node','{}')"
	if _, err = tx.ExecContext(ctx, event, "event-one"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, event, "event-two"); !storage.IsUnique(err) {
		t.Fatalf("active event uniqueness: %v", err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE alert_events SET state='resolved',resolved_at='now' WHERE id='event-one'"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, event, "event-two"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO nodes(id,name,created_at) VALUES('command-parent','temporary','now')"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO agent_commands(id,node_id,credential_epoch,kind,state,requested_by,created_at,expires_at) VALUES('cascade-command','command-parent',0,'restart','pending','actor','now','later')"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM nodes WHERE id='command-parent'"); err != nil {
		t.Fatal(err)
	}
	var commands int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM agent_commands WHERE id='cascade-command'").Scan(&commands); err != nil || commands != 0 {
		t.Fatalf("active command cascade=%d err=%v", commands, err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO agent_commands(id,node_id,credential_epoch,kind,state,requested_by,created_at,expires_at) VALUES('orphan-command','command-parent',0,'restart','pending','actor','now','later')"); err == nil {
		t.Fatal("orphan command accepted")
	} else if code, ok := storage.MySQLCode(err); !ok || code != 1452 {
		t.Fatalf("command parent FK: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = l.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM `groups` WHERE id='Alpha'"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE nodes SET installation_id='changed',report_session_id='changed' WHERE id='node'"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = l.QueryRowContext(ctx, "SELECT count(*) FROM alert_rules").Scan(&count); err != nil || count != 0 {
		t.Fatalf("generated FK cascade=%d err=%v", count, err)
	}
	if err = storage.ReplaceDomain(ctx, db, image, "mysql-replace"); err != nil {
		t.Fatal(err)
	}
	if err = storage.ReplaceDomain(ctx, db, image, "mysql-replace"); err != nil {
		t.Fatal(err)
	}
	var restoredInstallation, restoredSession string
	if err = l.QueryRowContext(ctx, "SELECT installation_id,report_session_id FROM nodes WHERE id='node'").Scan(&restoredInstallation, &restoredSession); err != nil || restoredInstallation != installation || restoredSession != session {
		t.Fatalf("replace identity bytes changed: installation=%q session=%q err=%v", restoredInstallation, restoredSession, err)
	}
	if err = history.Close(); err != nil {
		t.Fatal(err)
	}
	raw := f.Raw(t, storage.HistoryDomain)
	if _, err = raw.Exec("UPDATE schema_migrations SET version=1"); err != nil {
		t.Fatal(err)
	}
	if opened, failure := storage.OpenDatabases(ctx, config.DatabaseConfig{Primary: f.Endpoint("primary")}, dir); opened != nil || !errors.Is(failure, storage.ErrSchemaIncompatible) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("old schema accepted: %v", failure)
	}
	if _, err = raw.Exec("UPDATE schema_migrations SET version=2"); err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec("DROP TABLE buckets"); err != nil {
		t.Fatal(err)
	}
	if opened, failure := storage.OpenDatabases(ctx, config.DatabaseConfig{Primary: f.Endpoint("primary")}, dir); opened != nil || !errors.Is(failure, storage.ErrSchemaIncompatible) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("partial schema accepted: %v", failure)
	}
}
