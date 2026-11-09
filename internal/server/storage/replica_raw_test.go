package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

type rawReplicaConnector struct {
	mu                 sync.Mutex
	rolledBack, closed bool
	mode               string
	calls              int
}

func (c *rawReplicaConnector) Driver() driver.Driver { return rollbackDriver{} }
func (c *rawReplicaConnector) Connect(context.Context) (driver.Conn, error) {
	return &rawReplicaConn{c}, nil
}

type rawReplicaConn struct{ c *rawReplicaConnector }

func (c *rawReplicaConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *rawReplicaConn) Close() error {
	c.c.mu.Lock()
	defer c.c.mu.Unlock()
	c.c.closed = true
	return nil
}
func (c *rawReplicaConn) Begin() (driver.Tx, error) { return &rawReplicaTx{c.c}, nil }
func (c *rawReplicaConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return c.Begin()
}
func (c *rawReplicaConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &rawPacketRows{}, nil
}
func (c *rawReplicaConn) ExecContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	c.c.mu.Lock()
	c.c.calls++
	mode := c.c.mode
	c.c.mu.Unlock()
	switch mode {
	case "bad":
		return nil, fmt.Errorf("wrapped zero-byte write: %w", driver.ErrBadConn)
	case "cancel":
		<-ctx.Done()
		return nil, ctx.Err()
	case "counts":
		return rawReplicaResult{1, 0}, nil
	default:
		return rawReplicaResult{1, 1}, nil
	}
}

type rawReplicaTx struct{ c *rawReplicaConnector }

func (t *rawReplicaTx) Commit() error { return nil }
func (t *rawReplicaTx) Rollback() error {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	t.c.rolledBack = true
	return nil
}

type rawPacketRows struct{ done bool }

func (r *rawPacketRows) Columns() []string { return []string{"packet"} }
func (r *rawPacketRows) Close() error      { return nil }
func (r *rawPacketRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(4 << 20)
	return nil
}

type rawReplicaResult []int64

func (r rawReplicaResult) LastInsertId() (int64, error) { return 0, nil }
func (r rawReplicaResult) RowsAffected() (int64, error) { return r[len(r)-1], nil }
func (r rawReplicaResult) AllRowsAffected() []int64     { return r }
func (r rawReplicaResult) AllLastInsertIds() []int64    { return make([]int64, len(r)) }

func TestReplicaRawActiveTransactionBadConnAndCancellationDoNotDeadlock(t *testing.T) {
	for _, mode := range []string{"bad", "cancel", "counts", "ok"} {
		t.Run(mode, func(t *testing.T) {
			c := &rawReplicaConnector{mode: mode}
			pool := sql.OpenDB(c)
			defer pool.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			conn, err := pool.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			done := make(chan error, 1)
			go func() {
				done <- applyMySQLStatements(ctx, conn, tx, []replicaStatement{{query: "UPDATE a SET x=?", args: []any{int64(1)}}, {query: "DELETE FROM a WHERE x=?", args: []any{int64(2)}}})
			}()
			select {
			case err = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Raw held active Tx closemu forever")
			}
			switch mode {
			case "bad":
				if !errors.Is(err, driver.ErrBadConn) {
					t.Fatal(err)
				}
				c.mu.Lock()
				rolled, closed := c.rolledBack, c.closed
				c.mu.Unlock()
				if !rolled || !closed {
					t.Fatalf("bad connection release order: rollback=%v closed=%v", rolled, closed)
				}
			case "cancel":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			case "counts":
				if !errors.Is(err, ErrReplicaDiverged) {
					t.Fatal(err)
				}
			case "ok":
				if err != nil {
					t.Fatal(err)
				}
				c.mu.Lock()
				calls := c.calls
				c.mu.Unlock()
				if calls != 1 {
					t.Fatalf("per-row roundtrips: %d", calls)
				}
			}
		})
	}
}
