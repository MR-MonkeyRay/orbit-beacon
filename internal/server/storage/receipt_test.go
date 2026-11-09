package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"testing"
)

type receiptConnector struct {
	mu                        sync.Mutex
	last                      string
	fail, commit, barrierFail bool
}

func (c *receiptConnector) Driver() driver.Driver { return rollbackDriver{} }
func (c *receiptConnector) Connect(context.Context) (driver.Conn, error) {
	return &receiptConn{c: c}, nil
}

type receiptConn struct {
	c       *receiptConnector
	pending string
}

func (c *receiptConn) Prepare(string) (driver.Stmt, error)                          { return nil, errors.New("not used") }
func (c *receiptConn) Close() error                                                 { return nil }
func (c *receiptConn) Begin() (driver.Tx, error)                                    { c.pending = ""; return &receiptDriverTx{c}, nil }
func (c *receiptConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) { return c.Begin() }
func (c *receiptConn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	c.c.mu.Lock()
	defer c.c.mu.Unlock()
	if c.c.barrierFail {
		return nil, io.EOF
	}
	return &receiptDriverRows{value: c.c.last}, nil
}
func (c *receiptConn) ExecContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Result, error) {
	if len(args) > 0 {
		c.pending = args[0].Value.(string)
	}
	return driver.RowsAffected(1), nil
}

type receiptDriverTx struct{ c *receiptConn }

func (tx *receiptDriverTx) Commit() error {
	c := tx.c.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.commit && tx.c.pending != "" {
		c.last = tx.c.pending
	}
	if c.fail {
		c.fail = false
		return io.ErrUnexpectedEOF
	}
	return nil
}
func (tx *receiptDriverTx) Rollback() error { return nil }

type receiptDriverRows struct {
	value string
	done  bool
}

func (r *receiptDriverRows) Columns() []string { return []string{"last_tx_uuid"} }
func (r *receiptDriverRows) Close() error      { return nil }
func (r *receiptDriverRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.value
	return nil
}

func TestUnknownCommitFreezesUntilGuardReceiptResolves(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "rolledback", true: "committed"}[committed], func(t *testing.T) {
			connector := &receiptConnector{fail: true, commit: committed}
			pool := sql.OpenDB(connector)
			db := newDatabase(MainDomain, pool, pool)
			db.current.dialect = MySQL
			defer db.Close()
			l, err := db.Acquire(context.Background(), Writer)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			tx, err := l.BeginWrite(l.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(); !errors.Is(err, ErrCommitUnknown) {
				t.Fatalf("commit=%v", err)
			}
			l.Close()
			if db.Ready() {
				t.Fatal("unknown domain reported ready")
			}
			connector.mu.Lock()
			connector.barrierFail = true
			connector.mu.Unlock()
			next, err := db.Acquire(context.Background(), Writer)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			if pending, err := next.BeginWrite(next.Context()); !errors.Is(err, ErrCommitUnknown) {
				if pending != nil {
					pending.Rollback()
				}
				t.Fatalf("overwrote unresolved receipt: %v", err)
			}
			next.Close()
			connector.mu.Lock()
			connector.barrierFail = false
			connector.mu.Unlock()
			got, err := tx.ResolveCommit(context.Background())
			if err != nil || got != committed {
				t.Fatalf("resolved=%v want=%v err=%v", got, committed, err)
			}
			if !db.Ready() {
				t.Fatal("resolved domain not ready")
			}
			// 后续事务不改变已保存的旧事务结果。
			next, err = db.Acquire(context.Background(), Writer)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			later, err := next.BeginWrite(next.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer later.Rollback()
			if err = later.Commit(); err != nil {
				t.Fatal(err)
			}
			next.Close()
			got, err = tx.ResolveCommit(context.Background())
			if err != nil || got != committed {
				t.Fatalf("old receipt changed: %v %v", got, err)
			}
		})
	}
}

func TestLaterWriterPreservesOriginalUnknownOutcome(t *testing.T) {
	connector := &receiptConnector{fail: true, commit: true}
	pool := sql.OpenDB(connector)
	db := newDatabase(MainDomain, pool, pool)
	db.current.dialect = MySQL
	defer db.Close()
	l, err := db.Acquire(context.Background(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	original, err := l.BeginWrite(l.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = original.Commit(); !errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("commit=%v", err)
	}
	l.Close()
	next, err := db.Acquire(context.Background(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	later, err := next.BeginWrite(next.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer later.Rollback()
	if err = later.Commit(); err != nil {
		t.Fatal(err)
	}
	connector.mu.Lock()
	last := connector.last
	connector.mu.Unlock()
	if last == original.ID() {
		t.Fatal("later transaction did not overwrite guard receipt")
	}
	committed, err := original.ResolveCommit(context.Background())
	if err != nil || !committed {
		t.Fatalf("original committed outcome lost after automatic resolution: %v %v", committed, err)
	}
}
