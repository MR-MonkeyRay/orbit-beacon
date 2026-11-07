package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"orbitbeacon/internal/config"
)

func ReplicaTestEpochJournalPlan(t *testing.T, db *Database, endpoint *config.DatabaseEndpoint) {
	t.Helper()
	ctx := context.Background()
	epoch, other := uuid.NewString(), uuid.NewString()
	if err := installReplica(ctx, db, "plan-resource", "plan-instance", epoch, replicaProof{}); err != nil {
		t.Fatal(err)
	}
	boot, stamp, err := replicaClock()
	if err != nil {
		t.Fatal(err)
	}
	const head = 4096
	tx, err := db.current.writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO replica_epochs VALUES(?,'',0,'')", other); err != nil {
		t.Fatal(err)
	}
	previous := ""
	for start := 1; start <= head; start += 256 {
		var groups []string
		var args []any
		for seq := start; seq < start+256; seq++ {
			id := uuid.NewString()
			groups = append(groups, "(?,?,?,?,?,?,0,?)")
			args = append(args, epoch, seq, id, previous, boot, stamp-int64(2*time.Second), transactionOverhead)
			previous = id
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO replica_transactions VALUES"+strings.Join(groups, ","), args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, seq := range []int{1, 50000} {
		if _, err = tx.ExecContext(ctx, "INSERT INTO replica_transactions VALUES(?,?,?,'',?,?,0,?)", other, seq, uuid.NewString(), boot, stamp, transactionOverhead); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE replica_state SET epoch_uuid=?,head_seq=?,head_tx_uuid=?,retained_from_seq=1,capture_mode='source',journal_bytes=? WHERE id=1", epoch, head, previous, int64(head+2)*transactionOverhead); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE storage_write_guard SET last_tx_uuid=? WHERE id=1", previous); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	d := &replicaDomain{source: db, domain: db.domain, epoch: epoch, resource: "plan-resource"}
	db.replica = d
	var seen atomic.Int64
	var inspect atomic.Bool
	inspect.Store(true)
	if err = observeReplicaDatabase(ctx, db, endpoint, func(call context.Context, q string, values []driver.NamedValue) {
		upper := strings.ToUpper(strings.TrimSpace(q))
		if !inspect.Load() || strings.HasPrefix(upper, "EXPLAIN") || !strings.Contains(upper, "REPLICA_TRANSACTIONS") || (!strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "DELETE")) {
			return
		}
		seen.Add(1)
		args := make([]any, len(values))
		for i, v := range values {
			args[i] = v.Value
		}
		byUUID := strings.Contains(strings.Join(strings.Fields(upper), ""), "TX_UUID=?")
		if db.current.dialect == SQLite {
			rows, err := db.current.reader.QueryContext(call, "EXPLAIN QUERY PLAN "+q, args...)
			if err != nil {
				t.Error("SQLite EXPLAIN failed", err)
				return
			}
			defer rows.Close()
			found := false
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Error(err)
					return
				}
				text := strings.ToUpper(detail)
				if strings.Contains(text, "SCAN") || strings.Contains(text, "TEMP B-TREE") {
					t.Error("journal query scans or sorts", detail)
				}
				if strings.Contains(text, "REPLICA_TRANSACTIONS") || strings.HasPrefix(text, "SEARCH T ") {
					found = true
					if !byUUID && !strings.Contains(text, "EPOCH_UUID=?") {
						t.Error("journal query misses epoch PK prefix", detail)
					}
				}
			}
			if !found {
				t.Error("journal query plan did not expose transaction lookup")
			}
			if err = rows.Err(); err != nil {
				t.Error(err)
			}
			return
		}
		rows, err := db.current.reader.QueryContext(call, "EXPLAIN FORMAT=TRADITIONAL "+q, args...)
		if err != nil {
			t.Error("MySQL EXPLAIN failed", err)
			return
		}
		defer rows.Close()
		names, err := rows.Columns()
		if err != nil {
			t.Error(err)
			return
		}
		valuesOut := make([]sql.RawBytes, len(names))
		refs := make([]any, len(names))
		for i := range refs {
			refs[i] = &valuesOut[i]
		}
		found := false
		for rows.Next() {
			if err = rows.Scan(refs...); err != nil {
				t.Error(err)
				return
			}
			field := func(name string) string {
				for i, key := range names {
					if strings.EqualFold(key, name) {
						return string(valuesOut[i])
					}
				}
				return ""
			}
			name, access, key, extra := field("table"), field("type"), field("key"), field("Extra")
			lower := strings.ToLower(extra)
			if strings.Contains(lower, "filesort") || strings.Contains(lower, "temporary") || strings.Contains(lower, "skip scan") {
				t.Error("journal query scans or sorts", extra)
			}
			if name == "" && (strings.Contains(lower, "no matching row in const table") || strings.Contains(lower, "impossible where")) {
				found = true
				t.Logf("EXPLAIN template=%s empty=%s", q, extra)
				continue
			}
			if name != "replica_transactions" && name != "t" {
				continue
			}
			found = true
			length, _ := strconv.Atoi(field("key_len"))
			valid := access == "const" || access == "eq_ref" || access == "ref" || access == "range"
			expected := "PRIMARY"
			if byUUID {
				expected = "tx_uuid"
			}
			if !valid || key != expected || length < 66 {
				t.Errorf("journal does not use bounded keyed access: table=%s type=%s key=%s key_len=%d", name, access, key, length)
			}
			t.Logf("EXPLAIN template=%s table=%s type=%s key=%s key_len=%d extra=%s", q, name, access, key, length, extra)
		}
		if err = rows.Err(); err != nil {
			t.Error(err)
		}
		if !found {
			t.Error("MySQL journal plan did not expose transaction lookup")
		}
	}); err != nil {
		t.Fatal(err)
	}
	batch, err := readReplicaTransaction(ctx, db, head-1)
	if err != nil || batch.Epoch != epoch || batch.Seq != head {
		t.Fatal("current epoch seek failed", err)
	}
	if batch, err = readReplicaTransaction(ctx, db, head); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("next position crossed current epoch: batch=%v err=%v", batch != nil, err)
	}
	_, fresh, err := replicaClock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.current.writer.ExecContext(ctx, "UPDATE replica_transactions SET barrier_ns=? WHERE epoch_uuid=? AND seq=50000", fresh, other); err != nil {
		t.Fatal(err)
	}
	if err = d.barrier(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := position(ctx, db.current.reader)
	if err != nil || p.Epoch != epoch || p.Seq != head+1 {
		t.Error("foreign epoch suppressed active time barrier", p, err)
	}
	if err = d.prune(ctx, time.Now().Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	inspect.Store(false)
	var count int
	if err = db.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM replica_transactions WHERE epoch_uuid=?", other).Scan(&count); err != nil || count != 2 {
		t.Error("prune touched a different epoch", count, err)
	}
	if seen.Load() < 4 {
		t.Fatal("production log queries were not explained", seen.Load())
	}
	t.Log(fmt.Sprintf("actual %v engine, %d active headers, %d explained production queries", db.current.dialect, head, seen.Load()))
}
func TestReplicaJournalUsesEpochPrimaryKeySQLite(t *testing.T) {
	ReplicaTestEpochJournalPlan(t, replicaBareMain(t), nil)
}
