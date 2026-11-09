package storage_test

import (
	"context"
	"database/sql"
	"math"
	"strconv"
	"testing"

	"orbitbeacon/internal/server/storage"
)

func assertReplicaIncrementalTypes(t *testing.T, db *storage.Database, source, target *sql.DB, values []float64) {
	t.Helper()
	ctx := context.Background()
	text := "最终🙂\x00尾  "
	haWrite(t, db, func(tx *storage.Tx) {
		for i := range values {
			if _, err := tx.ExecContext(ctx, "UPDATE alert_events SET value=? WHERE id=?", values[len(values)-1-i], strconv.Itoa(i)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO alert_events(id,rule_id,node_id,state,value,started_at,resolved_at,rule_name,node_name,rule_snapshot) VALUES('null-after-seed','r','n','resolved',NULL,'now','later','规则','节点','{}')"); err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{"UPDATE users SET totp_ciphertext=NULL,totp_pending_ciphertext=X'' WHERE id='u'", "UPDATE nodes SET quota_bytes='0',run_generation='18446744073709551615' WHERE id='n'", "UPDATE nodes SET quota_bytes='18446744073709551615' WHERE id='n'"} {
			if _, err := tx.ExecContext(ctx, query); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE settings SET value=? WHERE `key`='image'", text); err != nil {
			t.Fatal(err)
		}
	})
	waitReplica(t, source, target)
	for i := range values {
		var a, b float64
		if err := source.QueryRow("SELECT value FROM alert_events WHERE id=?", strconv.Itoa(i)).Scan(&a); err != nil {
			t.Fatal(err)
		}
		if err := target.QueryRow("SELECT value FROM alert_events WHERE id=?", strconv.Itoa(i)).Scan(&b); err != nil {
			t.Fatal(err)
		}
		if math.Float64bits(a) != math.Float64bits(b) {
			t.Fatalf("incremental float bits %x != %x", math.Float64bits(a), math.Float64bits(b))
		}
	}
	var null, empty bool
	if err := target.QueryRow("SELECT totp_ciphertext IS NULL,length(totp_pending_ciphertext)=0 FROM users WHERE id='u'").Scan(&null, &empty); err != nil || !null || !empty {
		t.Fatal("incremental NULL/empty blob changed", null, empty, err)
	}
	if err := target.QueryRow("SELECT value IS NULL FROM alert_events WHERE id='null-after-seed'").Scan(&null); err != nil || !null {
		t.Fatal("incremental NULL float changed", null, err)
	}
	var quota, generation, got string
	if err := target.QueryRow("SELECT quota_bytes,run_generation FROM nodes WHERE id='n'").Scan(&quota, &generation); err != nil || quota != "18446744073709551615" || generation != quota {
		t.Fatal("incremental uint64 text changed", err)
	}
	if err := target.QueryRow("SELECT value FROM settings WHERE `key`='image'").Scan(&got); err != nil || got != text {
		t.Fatal("incremental UTF8/NUL/trailing spaces changed", err)
	}
}
