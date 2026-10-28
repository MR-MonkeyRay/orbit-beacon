package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/history"
	"orbitbeacon/internal/server/inbox"
	"orbitbeacon/internal/server/secretbox"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

const restoreUser = "11111111-1111-4111-8111-111111111111"
const restoreNode = "22222222-2222-4222-8222-222222222222"
const restoreTask = "33333333-3333-4333-8333-333333333333"
const restorePlan = "44444444-4444-4444-8444-444444444444"

func restoreFixture(t *testing.T, marker string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := datadir.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	at := timekey.Format(time.Now().UTC())
	hash, err := auth.HashPassword("restore-fixture-password", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := datadir.LoadKey(filepath.Join(dir, "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(key, "orbitbeacon notification channel v1")
	clear(key)
	if err != nil {
		t.Fatal(err)
	}
	urlCipher, err := box.Seal("99999999-9999-4999-8999-999999999999:url", []byte("https://example.com/hook"))
	if err != nil {
		t.Fatal(err)
	}
	reportInterval, diskInterval, billingDay, quota := 5, 120, 2, "18446744073709551615"
	credentialEpoch := int64(23)
	if marker == "target" {
		reportInterval, diskInterval, billingDay, quota = 10, 60, 3, "1024"
		credentialEpoch = 41
	}
	runtime := fmt.Sprintf(`{"report_interval_seconds":%d,"disk_interval_seconds":%d,"interfaces":[],"disk_paths":["/srv/%s"],"probe_targets":[]}`, reportInterval, diskInterval, marker)
	statements := []struct {
		q string
		a []any
	}{
		{`INSERT INTO users(id,username,password_hash,created_at) VALUES(?,?,?,?)`, []any{restoreUser, marker, hash, at}},
		{`INSERT INTO nodes(id,name,status,token_hash,credential_epoch,installation_id,first_enrolled_at,last_seen,system,agent_runtime,billing_day,quota_bytes,run_generation,report_session_id,created_at) VALUES(?,?,'online',?,?,'restore-installation',?,?,?,?,?,?,'17','55555555-5555-4555-8555-555555555555',?)`, []any{restoreNode, marker, bytes.Repeat([]byte{3}, 32), credentialEpoch, at, at, fmt.Sprintf(`{"hostname":%q}`, marker), runtime, billingDay, quota, at}},
		{`INSERT INTO audit_events(id,actor_id,action,target_id,created_at,details) VALUES('66666666-6666-4666-8666-666666666666',NULL,'fixture',NULL,?,?)`, []any{at, fmt.Sprintf(`{"marker":%q}`, marker)}},
		{`INSERT INTO backup_plans(id,name,enabled,kind,categories_json,cron,destination_json,retain_count,next_run_at,created_at,updated_at) VALUES(?, 'plan',1,'local','["configuration"]','0 3 * * *','{"path":"/does/not/exist/on/new/machine"}',7,?,?,?)`, []any{restorePlan, at, at, at}},
		{`INSERT INTO notification_channels(id,name,kind,enabled,config_json,url_ciphertext,created_at,updated_at) VALUES('99999999-9999-4999-8999-999999999999','Hook','webhook',1,'{}',?,?,?)`, []any{urlCipher, at, at}},
		{`INSERT INTO sessions(token_hash,user_id,expires_at) VALUES(?,?,2000000000)`, []any{bytes.Repeat([]byte{8}, 32), restoreUser}},
		{`INSERT INTO enrollment_tokens(id,token_hash,node_id,expires_at) VALUES('77777777-7777-4777-8777-777777777777',?,?,?)`, []any{bytes.Repeat([]byte{7}, 32), restoreNode, at}},
	}
	for _, s := range statements {
		if _, err = db.Exec(s.q, s.a...); err != nil {
			t.Fatal(err)
		}
	}
	metrics, err := history.Open(context.Background(), dir, db, history.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = metrics.Close(); err != nil {
		t.Fatal(err)
	}
	buckets, err := history.OpenDatabase(context.Background(), filepath.Join(dir, "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	total := 11
	if marker == "target" {
		total = 22
	}
	if _, err = buckets.Exec(`INSERT INTO buckets(node_id,metric,step,t,total,peak,samples) VALUES(?,'cpu_percent',60,60,?,?,1)`, restoreNode, total, total); err != nil {
		t.Fatal(err)
	}
	if err = buckets.Close(); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func restoreArchive(t *testing.T, dir string, categories []Category) (string, string) {
	t.Helper()
	ctx := context.Background()
	work := t.TempDir()
	os.Chmod(work, 0700)
	payloads, err := CreateSnapshotPayloads(ctx, dir, filepath.Join(work, "payloads"), categories, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(work, "backup.tar.gz.age")
	password := "AbCdEf0123456789"
	_, err = WriteArchive(ctx, archive, Manifest{Format: "orbitbeacon-backup", Version: 1, ArchiveID: "88888888-8888-4888-8888-888888888888", CreatedAt: time.Now().UTC(), ServerVersion: "test", SchemaVersion: storage.CurrentSchemaVersion, HistorySchemaVersion: 1, Categories: categories, Snapshots: payloads.Snapshots}, payloads.Files, password)
	if err != nil {
		t.Fatal(err)
	}
	return archive, password
}

func restoreDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := storage.OpenReader(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func restoreCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return err.Error()
}
func restoreBytes(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func assertNoPrepared(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "restore-journal.json")); !os.IsNotExist(err) {
		t.Fatalf("journal exists: %v", err)
	}
}

func TestRestoreSevenCategoryCombinations(t *testing.T) {
	source := restoreFixture(t, "source")
	for mask := 1; mask < 8; mask++ {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			target := restoreFixture(t, "target")
			var cats []Category
			for i, c := range []Category{Configuration, Monitoring, Events} {
				if mask&(1<<i) != 0 {
					cats = append(cats, c)
				}
			}
			archive, password := restoreArchive(t, source, cats)
			result, err := Restore(context.Background(), target, archive, password, true)
			if err != nil {
				t.Fatal(err)
			}
			if result.ArchiveID != "88888888-8888-4888-8888-888888888888" || result.RollbackPath == "" {
				t.Fatalf("result=%+v", result)
			}
			db := restoreDB(t, target)
			var config, monitor, event string
			if err = db.QueryRow(`SELECT username FROM users WHERE id=?`, restoreUser).Scan(&config); err != nil {
				t.Fatal(err)
			}
			if err = db.QueryRow(`SELECT json_extract(system,'$.hostname') FROM nodes WHERE id=?`, restoreNode).Scan(&monitor); err != nil {
				t.Fatal(err)
			}
			if err = db.QueryRow(`SELECT json_extract(details,'$.marker') FROM audit_events WHERE action='fixture'`).Scan(&event); err != nil {
				t.Fatal(err)
			}
			for i, got := range []string{config, monitor, event} {
				want := "target"
				if mask&(1<<i) != 0 {
					want = "source"
				}
				if got != want {
					t.Fatalf("category %d=%q want %q", i, got, want)
				}
			}
			metrics, err := storage.OpenReader(context.Background(), filepath.Join(target, "metrics.db"))
			if err != nil {
				t.Fatal(err)
			}
			var total float64
			if err = metrics.QueryRow(`SELECT total FROM buckets WHERE node_id=?`, restoreNode).Scan(&total); err != nil {
				t.Fatal(err)
			}
			metrics.Close()
			wantTotal := float64(22)
			if mask&2 != 0 {
				wantTotal = 11
			}
			if total != wantTotal {
				t.Fatalf("metrics=%v want %v", total, wantTotal)
			}
			var enabled, sessions, tickets int
			var next sql.NullString
			if err = db.QueryRow(`SELECT enabled,next_run_at FROM backup_plans WHERE id=?`, restorePlan).Scan(&enabled, &next); err != nil {
				t.Fatal(err)
			}
			db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions)
			db.QueryRow(`SELECT count(*) FROM enrollment_tokens`).Scan(&tickets)
			if mask&1 != 0 {
				if enabled != 0 || next.Valid || sessions != 0 || tickets != 0 {
					t.Fatal("configuration replacement retained plan/session/ticket execution state")
				}
			} else if enabled != 1 || !next.Valid || sessions != 1 || tickets != 1 {
				t.Fatal("retained configuration changed")
			}
			var generation, session, runtime, quota string
			var billingDay int
			var credentialEpoch int64
			if err = db.QueryRow(`SELECT run_generation,report_session_id,agent_runtime,billing_day,quota_bytes,credential_epoch FROM nodes WHERE id=?`, restoreNode).Scan(&generation, &session, &runtime, &billingDay, &quota, &credentialEpoch); err != nil {
				t.Fatal(err)
			}
			if generation != "17" || session != "55555555-5555-4555-8555-555555555555" {
				t.Fatal("authentication fence lost")
			}
			wantCredentialEpoch := int64(41)
			if mask&1 != 0 {
				wantCredentialEpoch = 23
			}
			if credentialEpoch != wantCredentialEpoch {
				t.Fatalf("credential identity epoch=%d want %d", credentialEpoch, wantCredentialEpoch)
			}
			wantRuntime := `{"report_interval_seconds":10,"disk_interval_seconds":60,"interfaces":[],"disk_paths":["/srv/target"],"probe_targets":[]}`
			if mask&2 != 0 {
				wantRuntime = `{"report_interval_seconds":5,"disk_interval_seconds":120,"interfaces":[],"disk_paths":["/srv/source"],"probe_targets":[]}`
			}
			if runtime != wantRuntime {
				t.Fatalf("observed runtime=%s want %s", runtime, wantRuntime)
			}
			wantBilling, wantQuota := 3, "1024"
			if mask&1 != 0 {
				wantBilling, wantQuota = 2, "18446744073709551615"
			}
			if billingDay != wantBilling || quota != wantQuota {
				t.Fatalf("billing=%d/%s want %d/%s", billingDay, quota, wantBilling, wantQuota)
			}
		})
	}
}

func TestRestoreNormalizesObservedRuntime(t *testing.T) {
	const unnormalized = `{"probe_targets":null,"disk_paths":null,"interfaces":null,"disk_interval_seconds":60,"report_interval_seconds":10}`
	const canonical = `{"report_interval_seconds":10,"disk_interval_seconds":60,"interfaces":[],"disk_paths":[],"probe_targets":[]}`
	for _, tc := range []struct {
		name               string
		categories         []Category
		selectedMonitoring bool
		runtime            string
		want               string
	}{
		{"imported runtime", []Category{Monitoring}, true, unnormalized, canonical},
		{"retained runtime", []Category{Configuration}, false, unnormalized, canonical},
		{"no report", []Category{Monitoring}, true, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			source, target := restoreFixture(t, "source"), restoreFixture(t, "target")
			observationDir := target
			if tc.selectedMonitoring {
				observationDir = source
			}
			db, err := storage.Open(ctx, filepath.Join(observationDir, "orbitbeacon.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`UPDATE nodes SET agent_runtime=? WHERE id=?`, tc.runtime, restoreNode); err != nil {
				t.Fatal(err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			archive, password := restoreArchive(t, source, tc.categories)
			if _, err = Restore(ctx, target, archive, password, true); err != nil {
				t.Fatal(err)
			}
			var runtime string
			if err = restoreDB(t, target).QueryRow(`SELECT agent_runtime FROM nodes WHERE id=?`, restoreNode).Scan(&runtime); err != nil {
				t.Fatal(err)
			}
			if runtime != tc.want {
				t.Fatalf("persisted runtime=%s want %s", runtime, tc.want)
			}
		})
	}
}

func TestRestorePreservesAdministratorLoginsAndIndividualInboxReads(t *testing.T) {
	ctx := context.Background()
	source := restoreFixture(t, "admin-one")
	db, err := storage.Open(ctx, filepath.Join(source, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	accounts := auth.NewService(db, auth.Options{})
	second, err := accounts.CreateUser(ctx, "admin-two", "second administrator password")
	if err != nil {
		t.Fatal(err)
	}
	firstSession, err := accounts.Login(ctx, "admin-one", "restore-fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	secondSession, err := accounts.Login(ctx, second.Username, "second administrator password")
	if err != nil {
		t.Fatal(err)
	}
	task := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	alert := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	at := timekey.Format(time.Now().UTC())
	if _, err = db.Exec(`INSERT INTO inbox_notifications(id,kind,source_id,transition,severity,created_at,details_json) VALUES(?,'task','historical-task','succeeded','info',?,'{}'),(?,'alert','historical-alert','firing','warning',?,'{}')`, task, at, alert, at); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO inbox_reads(user_id,notification_id,read_at) VALUES(?,?,?),(?,?,?)`, restoreUser, task, at, second.ID, alert, at); err != nil {
		t.Fatal(err)
	}
	archive, password := restoreArchive(t, source, []Category{Configuration, Events})
	target := framingTempDir(t)
	if _, err = Restore(ctx, target, archive, password, false); err != nil {
		t.Fatal(err)
	}
	restored, err := storage.Open(ctx, filepath.Join(target, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	restoredAccounts := auth.NewService(restored, auth.Options{})
	for _, session := range []auth.Session{firstSession, secondSession} {
		if _, err = restoredAccounts.Authenticate(ctx, session.Token); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("restored original session for %s: %v", session.User.ID, err)
		}
	}
	for _, account := range []struct{ username, password, id, readID, unreadID string }{
		{"admin-one", "restore-fixture-password", restoreUser, task, alert},
		{second.Username, "second administrator password", second.ID, alert, task},
	} {
		session, err := restoredAccounts.Login(ctx, account.username, account.password)
		if err != nil || session.User.ID != account.id {
			t.Fatalf("restored administrator %s login=%+v err=%v", account.username, session.User, err)
		}
		notifications := inbox.New(restored)
		who := inbox.Identity{UserID: account.id}
		summary, err := notifications.Summary(ctx, who)
		if err != nil || summary.UnreadCount != 1 {
			t.Fatalf("restored administrator %s summary=%+v err=%v", account.username, summary, err)
		}
		read, err := notifications.Detail(ctx, who, account.readID)
		if err != nil || !read.Read {
			t.Fatalf("restored administrator %s read=%+v err=%v", account.username, read, err)
		}
		unread, err := notifications.Detail(ctx, who, account.unreadID)
		if err != nil || unread.Read {
			t.Fatalf("restored administrator %s unread=%+v err=%v", account.username, unread, err)
		}
	}
}

func TestRestoreEmptyAndRefusalNeverMutateLive(t *testing.T) {
	source := restoreFixture(t, "source")
	config, password := restoreArchive(t, source, []Category{Configuration})
	data, _ := restoreArchive(t, source, []Category{Events})
	empty := t.TempDir()
	os.Chmod(empty, 0700)
	if _, err := Restore(context.Background(), empty, data, password, false); restoreCode(err) != "configuration_required" {
		t.Fatalf("empty data=%v", err)
	}
	if _, err := os.Stat(filepath.Join(empty, "secrets.key")); !os.IsNotExist(err) {
		t.Fatal("key created on refusal")
	}
	if _, err := Restore(context.Background(), empty, config, password, false); err != nil {
		t.Fatal(err)
	}
	db := restoreDB(t, empty)
	var runtime, system string
	var lastSeen sql.NullString
	var credentialEpoch int64
	if err := db.QueryRow(`SELECT agent_runtime,system,last_seen,credential_epoch FROM nodes WHERE id=?`, restoreNode).Scan(&runtime, &system, &lastSeen, &credentialEpoch); err != nil {
		t.Fatal(err)
	}
	if runtime != "" || system != "{}" || lastSeen.Valid {
		t.Fatal("configuration-only restore imported monitoring observations")
	}
	if credentialEpoch != 23 {
		t.Fatalf("configuration-only restore lost credential identity epoch: %d", credentialEpoch)
	}
	target := restoreFixture(t, "target")
	before := restoreBytes(t, target)
	for _, test := range []struct {
		password string
		replace  bool
		code     string
	}{{password, false, "restore_requires_replace"}, {"wrong", true, ""}} {
		_, err := Restore(context.Background(), target, config, test.password, test.replace)
		if err == nil || test.code != "" && restoreCode(err) != test.code {
			t.Fatalf("refusal=%v", err)
		}
		if !bytes.Equal(before, restoreBytes(t, target)) {
			t.Fatal("refusal changed live")
		}
		assertNoPrepared(t, target)
	}
	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	tamper := filepath.Join(t.TempDir(), "tamper.age")
	if err = os.WriteFile(tamper, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Restore(context.Background(), target, tamper, password, true); err == nil {
		t.Fatal("tamper accepted")
	}
	if !bytes.Equal(before, restoreBytes(t, target)) {
		t.Fatal("tamper changed live")
	}
}

func addRestoreTask(t *testing.T, dir, state string, password bool) {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var cipher []byte
	if password {
		key, e := datadir.LoadKey(filepath.Join(dir, "secrets.key"))
		if e != nil {
			t.Fatal(e)
		}
		box, e := secretbox.New(key, "orbitbeacon backup secrets v1")
		clear(key)
		if e != nil {
			t.Fatal(e)
		}
		cipher, e = box.Seal("task-password:"+restoreTask, []byte("AbCdEf0123456789"))
		if e != nil {
			t.Fatal(e)
		}
	}
	at := timekey.Format(time.Now().UTC())
	var finished any
	if state == "succeeded" {
		finished = at
	}
	_, err = db.Exec(`INSERT INTO backup_tasks(id,kind,state,phase,categories_json,destination_json,created_at,started_at,finished_at,artifact_name,artifact_bytes,artifact_sha256,result_location,password_ciphertext,input_credentials_ciphertext) VALUES(?,'backup_export',?,'storing','["configuration"]','{}',?,?,?,'backup.tar.gz.age',123,?, '/archive/location',?,?)`, restoreTask, state, at, at, finished, fmt.Sprintf("%064d", 1), cipher, []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if state == "running" {
		_, err = db.Exec(`INSERT INTO notification_deliveries(id,channel_id,event_id,state,next_attempt_at,lease_until,payload_json,created_at,updated_at) VALUES('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','99999999-9999-4999-8999-999999999999','removed-event','pending',?,?,'{}',?,?)`, at, at, at, at)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRestoreActiveWorkAndRetainedPassword(t *testing.T) {
	source := restoreFixture(t, "source")
	for _, events := range []bool{false, true} {
		t.Run(fmt.Sprint(events), func(t *testing.T) {
			target := restoreFixture(t, "target")
			addRestoreTask(t, target, "running", true)
			cats := []Category{Configuration}
			if events {
				cats = append(cats, Events)
			}
			archive, password := restoreArchive(t, source, cats)
			if _, err := Restore(context.Background(), target, archive, password, true); err != nil {
				t.Fatal(err)
			}
			db := restoreDB(t, target)
			var count int
			db.QueryRow(`SELECT count(*) FROM backup_tasks WHERE id=?`, restoreTask).Scan(&count)
			if events {
				if count != 0 {
					t.Fatal("target task leaked into replacement events")
				}
				db.QueryRow(`SELECT count(*) FROM notification_deliveries`).Scan(&count)
				if count != 0 {
					t.Fatal("target pending delivery leaked into replacement events")
				}
				return
			}
			var deliveryState, deliveryCode, lease string
			if err := db.QueryRow(`SELECT state,last_error_code,lease_until FROM notification_deliveries`).Scan(&deliveryState, &deliveryCode, &lease); err != nil {
				t.Fatal(err)
			}
			if deliveryState != "failed" || deliveryCode != "restore_interrupted" || lease != "" {
				t.Fatal("pending delivery remained executable")
			}
			var state, code, warning string
			var cipher []byte
			var finish, expiry, cleanup, artifactExpiry sql.NullString
			var input []byte
			if err := db.QueryRow(`SELECT state,error_code,warning_code,password_ciphertext,finished_at,password_expires_at,artifact_cleanup_after,artifact_expires_at,input_credentials_ciphertext FROM backup_tasks WHERE id=?`, restoreTask).Scan(&state, &code, &warning, &cipher, &finish, &expiry, &cleanup, &artifactExpiry, &input); err != nil {
				t.Fatal(err)
			}
			if state != "failed" || code != "restore_interrupted" || warning != "storage_outcome_unknown" || !finish.Valid || !expiry.Valid || !cleanup.Valid || !artifactExpiry.Valid || len(input) != 0 {
				t.Fatal("interrupted task incorrectly finalized")
			}
			key, err := datadir.LoadKey(filepath.Join(target, "secrets.key"))
			if err != nil {
				t.Fatal(err)
			}
			defer clear(key)
			box, err := secretbox.New(key, "orbitbeacon backup secrets v1")
			if err != nil {
				t.Fatal(err)
			}
			plain, err := box.Open("task-password:"+restoreTask, cipher)
			defer clear(plain)
			if err != nil || string(plain) != "AbCdEf0123456789" {
				t.Fatal("retained password not rekeyed")
			}
			db.QueryRow(`SELECT count(*) FROM inbox_notifications WHERE source_id=? AND transition='failed'`, restoreTask).Scan(&count)
			if count != 1 {
				t.Fatal("no atomic interruption notification")
			}
		})
	}
}

func TestRestoreMissingOldKeyConditionalAndDamagedSources(t *testing.T) {
	source := restoreFixture(t, "source")
	config, password := restoreArchive(t, source, []Category{Configuration})
	all, _ := restoreArchive(t, source, []Category{Configuration, Monitoring, Events})
	for _, passwordPresent := range []bool{false, true} {
		t.Run(fmt.Sprint(passwordPresent), func(t *testing.T) {
			target := restoreFixture(t, "target")
			if passwordPresent {
				addRestoreTask(t, target, "succeeded", true)
			}
			if err := os.Remove(filepath.Join(target, "secrets.key")); err != nil {
				t.Fatal(err)
			}
			_, err := Restore(context.Background(), target, config, password, true)
			if passwordPresent {
				if restoreCode(err) != "restore_secret_conflict" {
					t.Fatalf("missing key=%v", err)
				}
				assertNoPrepared(t, target)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, name := range []string{"orbitbeacon.db", "metrics.db"} {
		t.Run(name, func(t *testing.T) {
			target := restoreFixture(t, "target")
			if err := os.WriteFile(filepath.Join(target, name), []byte("corrupted"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Restore(context.Background(), target, config, password, true); err == nil {
				t.Fatal("damaged omitted source accepted")
			}
			assertNoPrepared(t, target)
			if _, err := Restore(context.Background(), target, all, password, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRestoreMonitoringDependencyConflict(t *testing.T) {
	source := restoreFixture(t, "source")
	target := restoreFixture(t, "target")
	db, err := storage.Open(context.Background(), filepath.Join(target, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DELETE FROM nodes`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	archive, password := restoreArchive(t, source, []Category{Monitoring})
	before := restoreBytes(t, target)
	_, err = Restore(context.Background(), target, archive, password, true)
	if restoreCode(err) != "restore_dependency_conflict" {
		t.Fatalf("dependency=%v", err)
	}
	if !bytes.Equal(before, restoreBytes(t, target)) {
		t.Fatal("dependency failure mutated live")
	}
	assertNoPrepared(t, target)
}

func TestRestoreImportedEventsNeverOwnSecretsOrArtifacts(t *testing.T) {
	source := restoreFixture(t, "source")
	addRestoreTask(t, source, "succeeded", true)
	db, err := storage.Open(context.Background(), filepath.Join(source, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	at := timekey.Format(time.Now().UTC())
	if _, err = db.Exec(`INSERT INTO inbox_notifications(id,kind,source_id,transition,severity,created_at,details_json) VALUES('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','task',?,'succeeded','info',?,'{}'),('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','alert','removed-event','resolved','info',?,'{}')`, restoreTask, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO inbox_reads(user_id,notification_id,read_at) VALUES(?,'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',?)`, restoreUser, at); err != nil {
		t.Fatal(err)
	}
	db.Close()
	archive, password := restoreArchive(t, source, []Category{Events})
	target := restoreFixture(t, "target")
	if _, err = Restore(context.Background(), target, archive, password, true); err != nil {
		t.Fatal(err)
	}
	restored := restoreDB(t, target)
	var flag int
	var cipher []byte
	var expiry, artifactExpiry, cleanup sql.NullString
	if err = restored.QueryRow(`SELECT restored,password_ciphertext,password_expires_at,artifact_expires_at,artifact_cleanup_after FROM backup_tasks WHERE id=?`, restoreTask).Scan(&flag, &cipher, &expiry, &artifactExpiry, &cleanup); err != nil {
		t.Fatal(err)
	}
	if flag != 1 || len(cipher) > 0 || expiry.Valid || artifactExpiry.Valid || cleanup.Valid {
		t.Fatal("imported history acquired secret/download ownership")
	}
	var count int
	if err = restored.QueryRow(`SELECT count(*) FROM inbox_reads WHERE user_id=? AND notification_id='aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'`, restoreUser).Scan(&count); err != nil || count != 1 {
		t.Fatal("read UUID association lost")
	}
	var first string
	if err = restored.QueryRow(`SELECT id FROM inbox_notifications ORDER BY sequence LIMIT 1`).Scan(&first); err != nil || first != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
		t.Fatal("sequence not rebuilt in created_at/id order")
	}
}
