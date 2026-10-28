package backup

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/secretbox"
)

const validationID = "00000000-0000-4000-8000-000000000001"
const validationTime = "2026-10-27T00:00:00.000000000Z"
const validationHash = "$argon2id$v=19$m=19456,t=1,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func validationDB(t *testing.T, category Category) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, table := range tablesFor(category) {
		columns := make([]string, 0, len(table.Columns))
		for _, c := range table.Columns {
			columns = append(columns, c.Name+" "+c.Type)
		}
		if _, err := db.Exec("CREATE TABLE " + table.Name + " (" + strings.Join(columns, ",") + ")"); err != nil {
			t.Fatal(err)
		}
	}
	if category == Configuration {
		if _, err := db.Exec("INSERT INTO users(id,username,password_hash,created_at) VALUES(?,?,?,?)", validationID, "admin", validationHash, validationTime); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestPayloadPasswordHashBounds(t *testing.T) {
	for _, hash := range []string{validationHash, strings.Replace(validationHash, "m=19456", "m=262144", 1)} {
		if err := auth.ValidatePasswordHash(hash); err != nil {
			t.Fatal(err)
		}
	}
	for _, hash := range []string{strings.Replace(validationHash, "m=19456", "m=4294967295", 1), strings.Replace(validationHash, "p=1", "p=257", 1), strings.Replace(validationHash, "t=1", "t=0", 1), "not-a-hash"} {
		if err := auth.ValidatePasswordHash(hash); err == nil {
			t.Fatal("accepted invalid hash")
		}
	}
	db := validationDB(t, Configuration)
	if err := ValidatePayloadData(context.Background(), db, Configuration, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	db.Exec("UPDATE users SET password_hash=?", strings.Replace(validationHash, "t=1", "t=999999999", 1))
	if err := ValidatePayloadData(context.Background(), db, Configuration, make([]byte, 32)); err == nil || !strings.Contains(err.Error(), "restore_invalid_data") {
		t.Fatalf("unsafe hash: %v", err)
	}
}

func TestPayloadRequiresAtLeastOneAdministrator(t *testing.T) {
	db := validationDB(t, Configuration)
	if _, err := db.Exec("DELETE FROM users"); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePayloadData(context.Background(), db, Configuration, make([]byte, 32)); err == nil {
		t.Fatal("accepted configuration without administrators")
	}
	if _, err := db.Exec("INSERT INTO users(id,username,password_hash,created_at) VALUES(?,?,?,?),(?,?,?,?)", validationID, "admin-one", validationHash, validationTime, "00000000-0000-4000-8000-000000000002", "admin-two", validationHash, validationTime); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePayloadData(context.Background(), db, Configuration, make([]byte, 32)); err != nil {
		t.Fatalf("two administrators rejected: %v", err)
	}
}

func TestPayloadPreservesExactUsernameContract(t *testing.T) {
	db := validationDB(t, Configuration)
	for _, username := range []string{" ", " admin ", "管理员", strings.Repeat("a", 128)} {
		if _, err := db.Exec("UPDATE users SET username=?", username); err != nil {
			t.Fatal(err)
		}
		if err := ValidatePayloadData(context.Background(), db, Configuration, make([]byte, 32)); err != nil {
			t.Fatalf("valid username %q rejected: %v", username, err)
		}
	}
	for _, username := range []string{"", strings.Repeat("a", 129), string([]byte{0xff})} {
		if _, err := db.Exec("UPDATE users SET username=?", username); err != nil {
			t.Fatal(err)
		}
		if err := ValidatePayloadData(context.Background(), db, Configuration, make([]byte, 32)); err == nil {
			t.Fatalf("invalid username %q accepted", username)
		}
	}
}

func TestPayloadMonitoringRejectsMalformedJSON(t *testing.T) {
	db := validationDB(t, Monitoring)
	for _, system := range []string{"{", "null", `{"hostname":42}`} {
		db.Exec("DELETE FROM node_observations")
		if _, err := db.Exec("INSERT INTO node_observations(node_id,last_seen,system,applied_revision,config_error) VALUES(?,NULL,?,0,'')", validationID, system); err != nil {
			t.Fatal(err)
		}
		if err := ValidatePayloadData(context.Background(), db, Monitoring, nil); err == nil {
			t.Fatal("accepted invalid system")
		}
	}
	db.Exec("UPDATE node_observations SET system='{}'")
	if err := ValidatePayloadData(context.Background(), db, Monitoring, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPayloadHistoricalReferencesAndFiringSnapshot(t *testing.T) {
	db := validationDB(t, Events)
	if _, err := db.Exec("INSERT INTO audit_events(id,actor_id,action,target_id,created_at,details) VALUES(?,NULL,'historical','',?,'{}')", validationID, validationTime); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePayloadData(context.Background(), db, Events, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO alert_events(id,rule_id,node_id,state,value,started_at,resolved_at,last_notified_at,rule_name,node_name,rule_snapshot) VALUES(?,?,?,'firing',NULL,?,NULL,NULL,'deleted rule','deleted node','{}')", validationID, validationID, validationID, validationTime); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePayloadData(context.Background(), db, Events, nil); err == nil {
		t.Fatal("accepted invalid firing snapshot")
	}
	db.Exec("UPDATE alert_events SET rule_snapshot=?", `{"name":"Offline","enabled":true,"scope_kind":"all","scope_id":null,"metric":"offline","comparator":null,"threshold":null,"for_seconds":180,"severity":"warning"}`)
	if err := ValidatePayloadData(context.Background(), db, Events, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPayloadChannelCiphertextUsesExistingPurposeAndAAD(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	box, err := secretbox.New(key, "orbitbeacon notification channel v1")
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := box.Seal(validationID, []byte("password"))
	if err != nil {
		t.Fatal(err)
	}
	db := validationDB(t, Configuration)
	if _, err := db.Exec("INSERT INTO notification_channels(id,name,kind,enabled,config_json,secret_ciphertext,url_ciphertext,created_at,updated_at) VALUES(?, 'Mail','email',1,?, ?,NULL,?,?)", validationID, `{"host":"mail.example.com","port":587,"username":"admin","from":"a@example.com","to":["b@example.com"]}`, cipher, validationTime, validationTime); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePayloadData(context.Background(), db, Configuration, key); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePayloadData(context.Background(), db, Configuration, []byte(strings.Repeat("x", 32))); err == nil {
		t.Fatal("accepted wrong key")
	}
	cipher, _ = box.Seal("other-channel", []byte("password"))
	db.Exec("UPDATE notification_channels SET secret_ciphertext=?", cipher)
	if err := ValidatePayloadData(context.Background(), db, Configuration, key); err == nil {
		t.Fatal("accepted wrong AAD")
	}
}

func TestPayloadNodeGenerationAndConfiguration(t *testing.T) {
	db := validationDB(t, Configuration)
	_, err := db.Exec(`INSERT INTO nodes(id,name,status,group_id,region,provider,notes,created_at,public_settings,desired_revision,report_interval_seconds,disk_interval_seconds,interfaces,disk_paths,probe_targets,billing_day,quota_bytes,token_hash,installation_id,first_enrolled_at,run_generation,report_session_id) VALUES(?,'Node','pending',NULL,'','','',?,'{}',1,10,60,'[]','[]','[]',1,'0',NULL,NULL,NULL,'0','')`, validationID, validationTime)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidatePayloadData(context.Background(), db, Configuration, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"01", "-1", "18446744073709551616", ""} {
		db.Exec("UPDATE nodes SET run_generation=?", value)
		if err = ValidatePayloadData(context.Background(), db, Configuration, make([]byte, 32)); err == nil {
			t.Fatal("accepted generation", value)
		}
	}
	db.Exec("UPDATE nodes SET run_generation='0',interfaces='[1]'")
	if err = ValidatePayloadData(context.Background(), db, Configuration, make([]byte, 32)); err == nil {
		t.Fatal("accepted invalid desired JSON")
	}
	db.Exec("UPDATE nodes SET interfaces='[]',public_settings='null'")
	if err = ValidatePayloadData(context.Background(), db, Configuration, make([]byte, 32)); err == nil {
		t.Fatal("accepted null public settings")
	}
}

func TestPayloadPlansValidateWithoutFilesystemAccess(t *testing.T) {
	db := validationDB(t, Configuration)
	key := []byte(strings.Repeat("k", 32))
	_, err := db.Exec(`INSERT INTO backup_plans(id,name,enabled,kind,categories_json,cron,destination_json,credentials_ciphertext,retain_count,created_at,updated_at) VALUES(?,'Local',1,'local','["configuration"]','0 3 * * *','{"path":"/this/backup/path/does/not/exist"}',NULL,7,?,?)`, validationID, validationTime, validationTime)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidatePayloadData(context.Background(), db, Configuration, key); err != nil {
		t.Fatal(err)
	}
	box, _ := secretbox.New(key, "orbitbeacon backup secrets v1")
	cipher, _ := box.Seal("plan:"+validationID, []byte(`{"access_key_id":"key","secret_access_key":"secret"}`))
	db.Exec(`UPDATE backup_plans SET kind='object',retain_count=NULL,destination_json='{"provider":"s3_compatible","endpoint":"https://objects.example.com","region":"test","bucket":"backups","prefix":"","path_style":true,"allow_private_networks":false}',credentials_ciphertext=?`, cipher)
	if err = ValidatePayloadData(context.Background(), db, Configuration, key); err != nil {
		t.Fatal(err)
	}
	cipher, _ = box.Seal("plan:wrong", []byte(`{"access_key_id":"key","secret_access_key":"secret"}`))
	db.Exec("UPDATE backup_plans SET credentials_ciphertext=?", cipher)
	if err = ValidatePayloadData(context.Background(), db, Configuration, key); err == nil {
		t.Fatal("accepted plan wrong AAD")
	}
}

func TestPayloadScheduleSyntax(t *testing.T) {
	for _, raw := range []string{"0 3 * * *", "0 3 * * 7", "0 3 * * 5-7", "0 3 * * 1-7/2", "0 3 * * */2", "0 3 * JAN MON"} {
		if _, err := ParseSchedule(raw); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{"@daily", "* * * * * *", "CRON_TZ=UTC 0 3 * * *", "0 3 ? * *", "0 3 * * 8", "0 3 * * 7-5"} {
		if _, err := ParseSchedule(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestPayloadScheduleWeekdayAndWildcardSemantics(t *testing.T) {
	now := time.Date(2026, 10, 27, 4, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ expression, want string }{
		{"0 3 * * 7", "2026-11-01T03:00:00Z"},
		{"0 3 * * 5-7", "2026-10-30T03:00:00Z"},
		{"0 3 * * 1-7/2", "2026-10-28T03:00:00Z"},
		{"0 3 1 * */2", "2026-11-01T03:00:00Z"},
		{"0 3 1 * 0,2,4,6", "2026-10-29T03:00:00Z"},
	} {
		schedule, err := ParseSchedule(tc.expression)
		if err != nil {
			t.Fatal(err)
		}
		if got := schedule.Next(now).Format(time.RFC3339); got != tc.want {
			t.Fatalf("%s: %s != %s", tc.expression, got, tc.want)
		}
	}
}

func TestPayloadObservationConfigurationError(t *testing.T) {
	db := validationDB(t, Monitoring)
	for _, raw := range []string{`{"revision":-1,"code":"invalid"}`, `{"revision":0,"code":""}`, `null`, `{"fields":[]}`} {
		db.Exec("DELETE FROM node_observations")
		if _, err := db.Exec("INSERT INTO node_observations(node_id,last_seen,system,applied_revision,config_error) VALUES(?,NULL,'{}',0,?)", validationID, raw); err != nil {
			t.Fatal(err)
		}
		if err := ValidatePayloadData(context.Background(), db, Monitoring, nil); err == nil {
			t.Fatal("accepted invalid config error")
		}
	}
}

func TestPayloadResolvedDeletedReferencesRemainLegal(t *testing.T) {
	db := validationDB(t, Events)
	if _, err := db.Exec(`INSERT INTO alert_events(id,rule_id,node_id,state,value,started_at,resolved_at,last_notified_at,rule_name,node_name,rule_snapshot) VALUES(?,'','','resolved',NULL,?,?,NULL,'','','{}')`, validationID, validationTime, validationTime); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePayloadData(context.Background(), db, Events, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPayloadWebhookURLCiphertextAndRuntimePolicy(t *testing.T) {
	db := validationDB(t, Configuration)
	key := []byte(strings.Repeat("k", 32))
	box, _ := secretbox.New(key, "orbitbeacon notification channel v1")
	cipher, _ := box.Seal(validationID+":url", []byte("http://127.0.0.1:1/never-contact-this"))
	if _, err := db.Exec(`INSERT INTO notification_channels(id,name,kind,enabled,config_json,secret_ciphertext,url_ciphertext,created_at,updated_at) VALUES(?,'Local','webhook',1,'{}',NULL,?,?,?)`, validationID, cipher, validationTime, validationTime); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePayloadData(context.Background(), db, Configuration, key); err != nil {
		t.Fatal(err)
	}
	cipher, _ = box.Seal(validationID, []byte("http://127.0.0.1:1/never-contact-this"))
	if _, err := db.Exec("UPDATE notification_channels SET url_ciphertext=?", cipher); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePayloadData(context.Background(), db, Configuration, key); err == nil {
		t.Fatal("accepted URL with secret AAD")
	}
}

func TestPayloadMonitoringConstraints(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"negative traffic", `INSERT INTO traffic_daily(node_id,day,rx_bytes,tx_bytes) VALUES('node','2026-10-27',-1,0)`},
		{"noncanonical hour", `INSERT INTO traffic_hourly(node_id,epoch,period_start,rx_bytes,tx_bytes,updated_at) VALUES('node','abcd','2026-10-27T00:00:00Z',0,0,'2026-10-27T00:00:00.000000000Z')`},
		{"invalid probe status", `INSERT INTO probe_latest(node_id,target,ok,latency_ms,error,status_code,checked_at) VALUES('node','https://example.com',1,NULL,'',200,'2026-10-27T00:00:00.000000000Z')`},
		{"invalid aggregate", `INSERT INTO probe_hourly(node_id,target,hour,checks,failures,latency_sum_ms) VALUES('node','https://example.com','2026-10-27T00:00:00.000000000Z',1,2,0)`},
		{"unsafe history", `INSERT INTO buckets(node_id,metric,step,t,total,peak,samples) VALUES('node','cpu_percent',60,0,101,101,1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := validationDB(t, Monitoring)
			if _, err := db.Exec(tc.query); err != nil {
				t.Fatal(err)
			}
			if err := ValidatePayloadData(context.Background(), db, Monitoring, nil); err == nil {
				t.Fatal("accepted invalid row")
			}
		})
	}
}
