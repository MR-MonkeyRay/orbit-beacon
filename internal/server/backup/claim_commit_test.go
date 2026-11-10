package backup

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"orbitbeacon/internal/server/storage"
)

// Only the commit acknowledgement and guard availability are faulted; task SQL,
// receipt resolution, snapshot creation and archive publication remain real.
type claimFaultConnector struct {
	base      driver.Driver
	dsn       string
	committed bool
	fired     atomic.Bool
	claims    atomic.Int32
	barriers  atomic.Int32
	failures  atomic.Int32
	resolving chan struct{}
}

func (c *claimFaultConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.base.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &claimFaultConn{Conn: conn, owner: c}, nil
}

func (c *claimFaultConnector) Driver() driver.Driver { return c.base }

type claimFaultConn struct {
	driver.Conn
	owner *claimFaultConnector
	tx    *claimFaultTx
}

func (c *claimFaultConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *claimFaultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.tx = &claimFaultTx{Tx: tx, owner: c.owner}
	return c.tx, nil
}

func (c *claimFaultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if strings.HasPrefix(query, "UPDATE backup_tasks SET state='running'") {
		c.tx.claim = true
		c.owner.claims.Add(1)
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *claimFaultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if query == "SELECT last_tx_uuid FROM storage_write_guard WHERE id=1 FOR UPDATE" && c.owner.fired.Load() {
		if c.owner.barriers.Add(1) == 1 {
			close(c.owner.resolving)
		}
		if c.owner.failures.Add(-1) >= 0 {
			return nil, io.ErrUnexpectedEOF
		}
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, strings.TrimSuffix(query, " FOR UPDATE"), args)
}

type claimFaultTx struct {
	driver.Tx
	owner *claimFaultConnector
	claim bool
}

func (tx *claimFaultTx) Commit() error {
	if !tx.claim || !tx.owner.fired.CompareAndSwap(false, true) {
		return tx.Tx.Commit()
	}
	var err error
	if tx.owner.committed {
		err = tx.Tx.Commit()
	} else {
		err = tx.Tx.Rollback()
	}
	if err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

func claimFaultFixture(t *testing.T, committed bool, failures int32) (*Service, *sql.DB, *claimFaultConnector, *recordingUploader, context.CancelFunc) {
	t.Helper()
	raw, opts := workerFixture(t)
	db := mainHandle(t, raw)
	uri := url.URL{Scheme: "file", Path: filepath.Join(opts.DataDir, "orbitbeacon.db")}
	uri.RawQuery = url.Values{"_pragma": {"journal_mode(WAL)", "foreign_keys(ON)", "busy_timeout(5000)", "synchronous(FULL)"}}.Encode()
	fault := &claimFaultConnector{base: raw.Driver(), dsn: uri.String(), committed: committed, resolving: make(chan struct{})}
	fault.failures.Store(failures)
	pool := sql.OpenDB(fault)
	pool.SetMaxOpenConns(1)
	// Storage has no public pool-injection API. Keep this dialect/driver bridge
	// local to this fixture instead of adding a production testing interface.
	generation := reflect.ValueOf(db).Elem().FieldByName("current").Elem()
	field := func(name string) reflect.Value {
		v := generation.FieldByName(name)
		return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
	}
	old := field("writer").Interface().(*sql.DB)
	field("writer").Set(reflect.ValueOf(pool))
	field("dialect").Set(reflect.ValueOf(storage.MySQL))
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	cancelGeneration := field("cancel").Interface().(context.CancelFunc)
	uploader := &recordingUploader{inspect: func(_ context.Context, path, _ string) error {
		info, err := os.Stat(path)
		if err == nil && info.Size() == 0 {
			return errors.New("empty archive uploaded")
		}
		return err
	}}
	opts.Uploader = uploader
	s, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	return s, raw, fault, uploader, cancelGeneration
}

func TestWorkerClaimUnknownCommitPublishesOnce(t *testing.T) {
	for _, committed := range []bool{true, false} {
		t.Run(map[bool]string{true: "committed_response_lost", false: "rolledback_reclaimed"}[committed], func(t *testing.T) {
			s, raw, fault, uploader, _ := claimFaultFixture(t, committed, 2)
			id := "10000000-0000-4000-8000-000000000011"
			enqueueRemote(t, s, id, time.Now())
			generation, done := s.db.Generation(), s.db.GenerationDone()
			runWorker(t, s)
			waitWorkerTask(t, raw, id, "succeeded")
			if !fault.fired.Load() || fault.barriers.Load() < 3 {
				t.Fatalf("fault not resolved after retries: fired=%v barriers=%d", fault.fired.Load(), fault.barriers.Load())
			}
			wantClaims := int32(1)
			if !committed {
				wantClaims = 2
			}
			if fault.claims.Load() != wantClaims || uploader.calls.Load() != 1 {
				t.Fatalf("claim attempts=%d want=%d uploads=%d", fault.claims.Load(), wantClaims, uploader.calls.Load())
			}
			if s.db.Generation() != generation || s.db.GenerationDone() != done {
				t.Fatal("single-database commit recovery changed generation")
			}
			task, err := s.loadTask(t.Context(), id)
			if err != nil || task.ArtifactBytes == nil || *task.ArtifactBytes == "0" || task.ArtifactSHA256 == nil || task.ResultLocation == nil || task.FinishedAt == nil {
				t.Fatalf("missing published archive metadata: %+v err=%v", task, err)
			}
			lease, err := s.db.Acquire(t.Context(), storage.Writer)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			tx, err := lease.BeginWrite(lease.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := planBusy(lease.Context(), tx, id); err != nil {
				t.Fatalf("terminal task still blocks plan: %v", err)
			}
			var notifications int
			if err := raw.QueryRow(`SELECT count(*) FROM inbox_notifications WHERE source_id=? AND transition='succeeded'`, id).Scan(&notifications); err != nil || notifications != 1 {
				t.Fatalf("terminal notifications=%d err=%v", notifications, err)
			}
		})
	}
}

func TestWorkerClaimUnresolvedCommitCancellationDoesNotPublish(t *testing.T) {
	for _, generationCanceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "worker_context", true: "old_generation"}[generationCanceled], func(t *testing.T) {
			s, raw, fault, uploader, cancelGeneration := claimFaultFixture(t, true, 1000)
			id := "10000000-0000-4000-8000-000000000012"
			enqueueRemote(t, s, id, time.Now())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				task, err := s.claim(ctx)
				if err == nil {
					s.execute(ctx, task)
				}
				done <- err
			}()
			select {
			case <-fault.resolving:
			case err := <-done:
				t.Fatalf("claim discarded unresolved receipt before cancellation: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("claim never attempted receipt resolution")
			}
			if generationCanceled {
				cancelGeneration()
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled claim error=%v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("unresolved claim did not stop on cancellation")
			}
			var state, phase string
			var artifact sql.NullString
			if err := raw.QueryRow(`SELECT state,phase,artifact_name FROM backup_tasks WHERE id=?`, id).Scan(&state, &phase, &artifact); err != nil {
				t.Fatal(err)
			}
			if state != "running" || phase != "snapshot" || artifact.Valid || fault.claims.Load() != 1 || uploader.calls.Load() != 0 {
				t.Fatalf("uncertain canceled task executed: state=%s phase=%s artifact=%v claims=%d uploads=%d", state, phase, artifact.Valid, fault.claims.Load(), uploader.calls.Load())
			}
			if entries, err := os.ReadDir(filepath.Join(s.opts.DataDir, "backup-work")); !os.IsNotExist(err) && (err != nil || len(entries) != 0) {
				t.Fatalf("canceled claim started staging: entries=%v err=%v", entries, err)
			}
		})
	}
}
