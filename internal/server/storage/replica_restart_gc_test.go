package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func restartedAcknowledgedGCFixture(t *testing.T, manual, offline bool) (*replication, *replicaDomain, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	r, d := crashDatabases(t, root, true)
	for _, statement := range []string{"INSERT INTO settings VALUES('ack-expiry-a','kept')", "INSERT INTO settings VALUES('ack-expiry-b','kept')", "INSERT INTO settings VALUES('ack-expiry-c','kept')"} {
		replicaWrite(t, d.source, statement)
	}
	if err := d.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	head, err := position(ctx, d.source.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	target, err := position(ctx, d.target.current.reader)
	if err != nil || target != head {
		t.Fatal("fixture did not acknowledge its real complete prefix", target, head, err)
	}
	// 模型化上一boot的已提交非head日志；事务/UUID/业务行及最新head仍是真实提交，非RPO或角色伪造。
	expiredBoot := uuid.NewString()
	for _, db := range []*Database{d.source, d.target} {
		if err = db.offlineGuard(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE replica_transactions SET boot_id=? WHERE epoch_uuid=? AND seq<?", expiredBoot, head.Epoch, head.Seq)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if manual {
		if err = d.update(ctx, func(m *memberRecord) { m.FailureSince = time.Now().UTC().Add(-25 * time.Hour) }); err != nil {
			t.Fatal(err)
		}
		if err = d.checkManual(ctx); err != nil {
			t.Fatal(err)
		}
		if !d.member().Manual {
			t.Fatal("fixture did not acquire its durable elapsed-incident manual lock")
		}
	}
	if err = d.source.Close(); err != nil {
		t.Fatal(err)
	}
	if err = d.target.Close(); err != nil {
		t.Fatal(err)
	}
	if err = r.control.Close(); err != nil {
		t.Fatal(err)
	}
	r, d = crashDatabases(t, root, false)
	// 与registered启动相同：新domain只有active，applied自然为0，inactive未打开。
	d.target.Close()
	d.target = nil
	source, err := OpenMain(ctx, filepath.Join(root, "primary", "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	d.source = source
	source.replica = d
	d.revoke = func(ctx context.Context) error { return r.revoke(ctx, MainDomain, "rebuild_required") }
	// 私有SQLite协议夹具以真实standby文件目录解析inactive；control仍是原control连接。
	r.dataDir = filepath.Join(root, "standby")
	if d.applied.Load() != 0 {
		t.Fatal("restart retained an in-memory ACK watermark")
	}
	if offline {
		if err = os.Rename(filepath.Join(root, "standby", "orbitbeacon.db"), filepath.Join(root, "standby", "unreachable.db")); err != nil {
			t.Fatal(err)
		}
	}
	return r, d, expiredBoot
}

func TestRestartGCVerifiesAcknowledgedCopyBeforePruningAndHonorsManualLock(t *testing.T) {
	for _, test := range []struct {
		name            string
		manual, offline bool
	}{{"caught_up", false, false}, {"healthy_manual_active", true, false}, {"inactive_unreachable_still_prunes", false, true}} {
		t.Run(test.name, func(t *testing.T) {
			r, d, expired := restartedAcknowledgedGCFixture(t, test.manual, test.offline)
			defer func() { r.control.Close() }()
			defer d.source.Close()
			defer func() {
				if d.target != nil {
					d.target.Close()
				}
			}()
			var revoked atomic.Bool
			control := newDatabase(MainDomain, r.control, r.control)
			if err := observeReplicaDatabase(t.Context(), control, nil, func(_ context.Context, q string, values []driver.NamedValue) {
				if !strings.HasPrefix(q, "INSERT INTO storage_control") {
					return
				}
				var raw []byte
				switch value := values[0].Value.(type) {
				case string:
					raw = []byte(value)
				case []byte:
					raw = value
				default:
					t.Error("unexpected control record binding")
					return
				}
				var record controlRecord
				if err := json.Unmarshal(raw, &record); err != nil {
					t.Error(err)
					return
				}
				if !record.Domains[MainDomain].Ready {
					revoked.Store(true)
				}
			}); err != nil {
				t.Fatal(err)
			}
			r.control = control.current.writer
			defer control.current.reader.Close()
			err := d.coordinatorStep(t.Context())
			if !test.offline && err != nil {
				t.Fatal(err)
			}
			var remaining, rows int
			if err := d.source.current.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM replica_transactions WHERE boot_id=?", expired).Scan(&remaining); err != nil || remaining != 0 {
				t.Fatal("inactive availability blocked actual source TTL cleanup", remaining, err)
			}
			member := d.member()
			if test.offline {
				if member.Ready || !revoked.Load() {
					t.Fatal("unknown inactive position stayed qualified after its needed interval was deleted")
				}
			} else {
				if revoked.Load() || !member.Ready || member.Rebuild != nil {
					t.Fatal("acknowledged copy lost qualification solely because the process-local ACK watermark restarted at zero", revoked.Load(), member.Ready, member.Rebuild)
				}
				if d.applied.Load() == 0 {
					t.Fatal("coordinator did not recover the actual same-epoch inactive position")
				}
			}
			if member.Manual != test.manual || !d.source.Ready() {
				t.Fatal("GC changed manual authorization or stopped healthy active service", member.Manual, d.source.Ready())
			}
			if err := d.source.current.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM settings").Scan(&rows); err != nil || rows != 4 {
				t.Fatal("GC changed committed business data", rows, err)
			}
		})
	}
}
