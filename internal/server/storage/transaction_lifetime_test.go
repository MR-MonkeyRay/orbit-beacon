package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"testing"
	"time"
)

// 阻塞 driver 的真实回滚，覆盖 database/sql 已标 TxDone、连接尚未释放的窗口。
type rollbackConnector struct{ started, finish chan struct{} }

func (c *rollbackConnector) Connect(context.Context) (driver.Conn, error) {
	return &rollbackConn{c}, nil
}
func (c *rollbackConnector) Driver() driver.Driver { return rollbackDriver{} }

type rollbackDriver struct{}

func (rollbackDriver) Open(string) (driver.Conn, error) { return nil, errors.New("connector required") }

type rollbackConn struct{ c *rollbackConnector }

func (c *rollbackConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not used") }
func (c *rollbackConn) Close() error                        { return nil }
func (c *rollbackConn) Begin() (driver.Tx, error)           { return &rollbackTx{c.c}, nil }
func (c *rollbackConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return c.Begin()
}

type rollbackTx struct{ c *rollbackConnector }

func (tx *rollbackTx) Commit() error   { return nil }
func (tx *rollbackTx) Rollback() error { close(tx.c.started); <-tx.c.finish; return nil }

func TestDrainWaitsForCanceledTransactionDriverExit(t *testing.T) {
	connector := &rollbackConnector{started: make(chan struct{}), finish: make(chan struct{})}
	pool := sql.OpenDB(connector)
	db := newDatabase(MainDomain, pool, pool)
	t.Cleanup(func() { _ = db.Close() })
	var once sync.Once
	finish := func() { once.Do(func() { close(connector.finish) }) }
	t.Cleanup(finish)
	ctx, cancel := context.WithCancel(context.Background())
	lease, err := db.Acquire(ctx, Writer)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-connector.started:
	case <-time.After(time.Second):
		t.Fatal("automatic rollback did not start")
	}
	_ = lease.Close()
	ended := make(chan error, 1)
	go func() { ended <- tx.Rollback() }()
	select {
	case err := <-ended:
		t.Fatalf("transaction released before driver exited: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	drained := make(chan error, 1)
	go func() { drained <- db.drain(context.Background()) }()
	select {
	case err := <-drained:
		t.Fatalf("generation drained before driver exited: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	finish()
	select {
	case err := <-ended:
		if !errors.Is(err, sql.ErrTxDone) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("transaction did not finish")
	}
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("generation did not drain")
	}
}
