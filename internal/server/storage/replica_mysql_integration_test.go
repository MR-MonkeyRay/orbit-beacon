package storage_test

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func waitReplica(t *testing.T, source, target *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var se, si, te, ti string
		var ss, ts int64
		var ready bool
		seErr := source.QueryRow("SELECT epoch_uuid,head_seq,head_tx_uuid FROM replica_state").Scan(&se, &ss, &si)
		teErr := target.QueryRow("SELECT epoch_uuid,head_seq,head_tx_uuid,ready FROM replica_state").Scan(&te, &ts, &ti, &ready)
		if seErr == nil && teErr == nil && ready && se == te && ss == ts && si == ti {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("replica did not reach exact source checkpoint")
}
func haWrite(t *testing.T, db *storage.Database, fn func(*storage.Tx)) {
	t.Helper()
	ctx := context.Background()
	l, err := db.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	tx, err := l.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	fn(tx)
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
func TestMySQLReplicationTypedTriggersAndRestart(t *testing.T) {
	for _, sqliteTarget := range []bool{false, true} {
		t.Run(fmt.Sprint("sqlite=", sqliteTarget), func(t *testing.T) {
			f := storagetest.Start(t)
			ctx := context.Background()
			standby := f.Endpoint("standby")
			if sqliteTarget {
				standby = config.DatabaseEndpoint{Driver: "sqlite"}
			}
			cfg := config.DatabaseConfig{Primary: f.Endpoint("primary"), Standby: &standby}
			dir := t.TempDir()
			os.Chmod(dir, 0700)
			dbs, err := storage.OpenDatabases(ctx, cfg, dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { dbs.Close() }()
			raw := f.RawEndpoint(t, "primary", storage.MainDomain)
			var target *sql.DB
			if sqliteTarget {
				target, err = storage.OpenReader(ctx, filepath.Join(dir, "orbitbeacon.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer target.Close()
			} else {
				target = f.RawEndpoint(t, "standby", storage.MainDomain)
			}
			var ready bool
			if err = target.QueryRow("SELECT ready FROM replica_state").Scan(&ready); err != nil || ready {
				t.Fatalf("standby ready before bootstrap: %v %v", ready, err)
			}
			values := []float64{0, math.Copysign(0, -1), math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64, math.MaxFloat64, -math.MaxFloat64, math.Nextafter(1, 0), math.Nextafter(1, 2)}
			haWrite(t, dbs.Main, func(tx *storage.Tx) {
				for _, q := range []string{"INSERT INTO users(id,username,password_hash,created_at,totp_ciphertext) VALUES('u','用户','hash','now',X'')", "INSERT INTO `groups` VALUES('g','组')", "INSERT INTO nodes(id,name,group_id,quota_bytes,created_at) VALUES('n','节点','g','18446744073709551615','now')", "INSERT INTO enrollment_tokens(id,token_hash,node_id,group_id,expires_at,max_uses,uses) VALUES('enroll',X'01','n','g','later',10,0)", "INSERT INTO enrollment_claims VALUES('enroll','install','n',X'02','now')"} {
					if _, err := tx.ExecContext(ctx, q); err != nil {
						t.Fatal(err)
					}
				}
				for i, v := range values {
					if _, err := tx.ExecContext(ctx, "INSERT INTO alert_events(id,rule_id,node_id,state,value,started_at,resolved_at,rule_name,node_name,rule_snapshot) VALUES(?,'r','n','resolved',?,'now','later','规则','节点','{}')", strconv.Itoa(i), v); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := tx.ExecContext(ctx, "INSERT INTO settings VALUES('image',?)", "界\x00尾 "); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(ctx, "UPDATE settings SET value='second' WHERE `key`='image'"); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(ctx, "UPDATE settings SET value=value WHERE `key`='image'"); err != nil {
					t.Fatal(err)
				}
			})
			// 真触发器必须留下同一行每次修改的当时值，而不是最终值三次。
			rows, err := raw.Query("SELECT after_image FROM replica_changes WHERE table_id=11 ORDER BY ordinal")
			if err != nil {
				t.Fatal(err)
			}
			var images []string
			for rows.Next() {
				var raw []byte
				if err = rows.Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var a [][]string
				if err = json.Unmarshal(raw, &a); err != nil {
					t.Fatal(err)
				}
				b, err := hex.DecodeString(a[1][1])
				if err != nil {
					t.Fatal(err)
				}
				images = append(images, string(b))
			}
			rows.Close()
			if len(images) != 3 || images[0] != "界\x00尾 " || images[1] != "second" || images[2] != "second" {
				t.Fatalf("transaction images=%q", images)
			}
			if _, err = raw.Exec("INSERT INTO settings VALUES('bare','bad')"); err == nil {
				t.Fatal("bare source write accepted")
			}
			l, _ := dbs.Main.Acquire(ctx, storage.Writer)
			tx, err := l.BeginWrite(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.ExecContext(ctx, "UPDATE settings SET `key`='mutated'"); err == nil {
				t.Fatal("PK mutation accepted")
			}
			tx.Rollback()
			tx, err = l.BeginWrite(ctx)
			if err != nil {
				t.Fatal(err)
			}
			tx.ExecContext(ctx, "INSERT INTO settings VALUES('rolled-back','bad')")
			tx.Rollback()
			l.Close()
			if err = dbs.StartReplication(ctx); err != nil {
				t.Fatal(err)
			}
			waitReplica(t, raw, target)
			var empty, null bool
			var quota string
			if err = target.QueryRow("SELECT length(totp_ciphertext)=0,totp_pending_ciphertext IS NULL FROM users WHERE id='u'").Scan(&empty, &null); err != nil || !empty || !null {
				t.Fatalf("empty/null %v %v %v", empty, null, err)
			}
			target.QueryRow("SELECT quota_bytes FROM nodes WHERE id='n'").Scan(&quota)
			if quota != "18446744073709551615" {
				t.Fatal(quota)
			}
			for i := range values {
				var a, b float64
				if err = raw.QueryRow("SELECT value FROM alert_events WHERE id=?", strconv.Itoa(i)).Scan(&a); err != nil {
					t.Fatal(err)
				}
				if err = target.QueryRow("SELECT value FROM alert_events WHERE id=?", strconv.Itoa(i)).Scan(&b); err != nil {
					t.Fatal(err)
				}
				if math.Float64bits(a) != math.Float64bits(b) {
					t.Fatalf("float bits %x != %x", math.Float64bits(a), math.Float64bits(b))
				}
			}
			assertReplicaIncrementalTypes(t, dbs.Main, raw, target, values)
			haWrite(t, dbs.Main, func(tx *storage.Tx) {
				if _, err := tx.ExecContext(ctx, "DELETE FROM `groups` WHERE id='g'"); err != nil {
					t.Fatal(err)
				}
			})
			waitReplica(t, raw, target)
			if err = target.QueryRow("SELECT group_id IS NULL FROM nodes WHERE id='n'").Scan(&null); err != nil || !null {
				t.Fatalf("SET NULL %v %v", null, err)
			}
			haWrite(t, dbs.Main, func(tx *storage.Tx) {
				if _, err := tx.ExecContext(ctx, "DELETE FROM nodes WHERE id='n'"); err != nil {
					t.Fatal(err)
				}
			})
			waitReplica(t, raw, target)
			var n int
			target.QueryRow("SELECT count(*) FROM enrollment_claims").Scan(&n)
			if n != 0 {
				t.Fatal("multilevel CASCADE", n)
			}
			if err = dbs.Close(); err != nil {
				t.Fatal(err)
			}
			dbs, err = storage.OpenDatabases(ctx, cfg, dir)
			if err != nil {
				t.Fatal(err)
			}
			if err = dbs.StartReplication(ctx); err != nil {
				t.Fatal(err)
			}
			haWrite(t, dbs.Main, func(tx *storage.Tx) {
				if _, err := tx.ExecContext(ctx, "INSERT INTO settings VALUES('restart','ok')"); err != nil {
					t.Fatal(err)
				}
			})
			waitReplica(t, raw, target)
		})
	}
}

func TestMySQLReplicationRejectsForeignTargetControlLossAndUnsafeRemoval(t *testing.T) {
	f := storagetest.Start(t)
	ctx := context.Background()
	standby := f.Endpoint("standby")
	cfg := config.DatabaseConfig{Primary: f.Endpoint("primary"), Standby: &standby}
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	foreign := f.RawEndpoint(t, "standby", storage.MainDomain)
	if _, err := foreign.Exec("CREATE TABLE foreign_data(value BIGINT)"); err != nil {
		t.Fatal(err)
	}
	foreign.Exec("INSERT INTO foreign_data VALUES(7)")
	if dbs, err := storage.OpenDatabases(ctx, cfg, dir); err == nil {
		dbs.Close()
		t.Fatal("foreign target registered")
	}
	var value int
	if err := foreign.QueryRow("SELECT value FROM foreign_data").Scan(&value); err != nil || value != 7 {
		t.Fatal("foreign target changed", value, err)
	}
	foreign.Exec("DROP TABLE foreign_data")
	dbs, err := storage.OpenDatabases(ctx, cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	dbs.Close()
	standalone := config.DatabaseConfig{Primary: cfg.Primary}
	if dbs, err = storage.OpenDatabases(ctx, standalone, dir); !errors.Is(err, storage.ErrIdentityMismatch) {
		if dbs != nil {
			dbs.Close()
		}
		t.Fatalf("standby removal %v", err)
	}
	id, err := storage.ReplicationClusterID(ctx, dir)
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	if err = storage.DisableReplication(ctx, cfg, dir, "wrong"); !errors.Is(err, storage.ErrIdentityMismatch) {
		t.Fatal("wrong confirmation", err)
	}
	if err = storage.DisableReplication(ctx, cfg, dir, id); err != nil {
		t.Fatal(err)
	}
	dbs, err = storage.OpenDatabases(ctx, standalone, dir)
	if err != nil {
		t.Fatal(err)
	}
	haWrite(t, dbs.Main, func(tx *storage.Tx) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings VALUES('standalone','ok')"); err != nil {
			t.Fatal(err)
		}
	})
	dbs.Close()
	// 已清退身份也不能在另一 data_dir 重新猜测其归属。
	other := t.TempDir()
	os.Chmod(other, 0700)
	if dbs, err = storage.OpenDatabases(ctx, cfg, other); !errors.Is(err, storage.ErrIdentityMismatch) {
		if dbs != nil {
			dbs.Close()
		}
		t.Fatal("lost control", err)
	}
}
