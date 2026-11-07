package backup

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

func TestPayloadRejectsInvalidSilencesAndFlags(t *testing.T) {
	ctx := context.Background()
	db := validationDB(t, Configuration)
	end := timekey.Format(time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
	if _, err := db.Exec(`INSERT INTO alert_silences(id,reason,scope_kind,scope_id,starts_at,ends_at,created_at) VALUES(?,'maintenance','all',NULL,?,?,?)`, validationID, validationTime, end, validationTime); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePayloadData(ctx, db, Configuration, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	for _, update := range []string{
		"scope_kind='bad'", "scope_kind='node',scope_id=NULL", "scope_kind='group',scope_id='bad'", "scope_kind='all',scope_id='00000000-0000-4000-8000-000000000001'", "ends_at=starts_at", "ends_at='2026-10-26T00:00:00.000000000Z'", "reason=''",
	} {
		if _, err := db.Exec("UPDATE alert_silences SET " + update); err != nil {
			t.Fatal(err)
		}
		if err := ValidatePayloadData(ctx, db, Configuration, make([]byte, 32)); err == nil {
			t.Fatalf("accepted %s", update)
		}
		if _, err := db.Exec("UPDATE alert_silences SET reason='maintenance',scope_kind='all',scope_id=NULL,ends_at=?", end); err != nil {
			t.Fatal(err)
		}
	}
	events := validationDB(t, Events)
	if _, err := events.Exec(`INSERT INTO alert_events(id,rule_id,node_id,state,value,started_at,resolved_at,last_notified_at,rule_name,node_name,rule_snapshot,silenced) VALUES(?,'','','resolved',NULL,?,?,NULL,'','','{}',0)`, validationID, validationTime, validationTime); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []int{0, 1, -1, 2} {
		if _, err := events.Exec("UPDATE alert_events SET silenced=?", flag); err != nil {
			t.Fatal(err)
		}
		err := ValidatePayloadData(ctx, events, Events, nil)
		if (err == nil) != (flag == 0 || flag == 1) {
			t.Fatalf("silenced=%d err=%v", flag, err)
		}
	}
}

func TestSilenceBackupRoundtripPreservesFlag(t *testing.T) {
	ctx := context.Background()
	source := restoreFixture(t, "source")
	db, err := storage.Open(ctx, filepath.Join(source, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	start := timekey.Format(time.Now().UTC())
	end := timekey.Format(time.Now().UTC().Add(time.Hour))
	if _, err = db.Exec(`INSERT INTO alert_silences(id,reason,scope_kind,starts_at,ends_at,created_at) VALUES(?,'maintenance','all',?,?,?)`, validationID, start, end, start); err != nil {
		t.Fatal(err)
	}
	for i, flag := range []int{0, 1} {
		id := []string{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}[i]
		if _, err = db.Exec(`INSERT INTO alert_events(id,rule_id,node_id,state,started_at,resolved_at,rule_name,node_name,rule_snapshot,silenced) VALUES(?,'deleted','deleted','resolved',?,?,'rule','node','{}',?)`, id, start, start, flag); err != nil {
			t.Fatal(err)
		}
	}
	const snapshot = `{"name":"CPU","enabled":true,"scope_kind":"all","metric":"cpu_percent","comparator":">","threshold":80,"for_seconds":0,"severity":"warning"}`
	if _, err = db.Exec(`INSERT INTO alert_events(id,rule_id,node_id,state,started_at,rule_name,node_name,rule_snapshot,silenced) VALUES('cccccccc-cccc-4ccc-8ccc-cccccccccccc','deleted','deleted','firing',?,'rule','node',?,1)`, start, snapshot); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	archive, password := restoreArchive(t, source, []Category{Configuration, Events})
	target := restoreFixture(t, "target")
	if _, err = Restore(ctx, target, archive, password, true); err != nil {
		t.Fatal(err)
	}
	restored := restoreDB(t, target)
	var count, sum int
	if err = restored.QueryRow("SELECT count(*),sum(silenced) FROM alert_events").Scan(&count, &sum); err != nil || count != 3 || sum != 2 {
		t.Fatalf("events=%d flags=%d %v", count, sum, err)
	}
	var reason string
	if err = restored.QueryRow("SELECT reason FROM alert_silences WHERE id=?", validationID).Scan(&reason); err != nil || reason != "maintenance" {
		t.Fatalf("silence=%s %v", reason, err)
	}
}
