package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync/atomic"

	"github.com/go-sql-driver/mysql"
	"orbitbeacon/internal/config"
)

type replicaHookConnector struct {
	driver.Connector
	before      func()
	afterCommit func(error)
	fired       atomic.Bool
	done        chan error
}

func (c *replicaHookConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &replicaHookConn{Conn: conn, owner: c}, nil
}

type replicaHookConn struct {
	driver.Conn
	owner *replicaHookConnector
}

func (c *replicaHookConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *replicaHookConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	begin := c.Conn.(driver.ConnBeginTx)
	tx, err := begin.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &replicaHookTx{Tx: tx, owner: c.owner}, nil
}
func (c *replicaHookConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	exec, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return exec.ExecContext(ctx, q, args)
}
func (c *replicaHookConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	query, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return query.QueryContext(ctx, q, args)
}
func (c *replicaHookConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	prepare, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Conn.Prepare(q)
	}
	return prepare.PrepareContext(ctx, q)
}
func (c *replicaHookConn) CheckNamedValue(v *driver.NamedValue) error {
	check, ok := c.Conn.(driver.NamedValueChecker)
	if !ok {
		return driver.ErrSkip
	}
	return check.CheckNamedValue(v)
}
func (c *replicaHookConn) ResetSession(ctx context.Context) error {
	reset, ok := c.Conn.(driver.SessionResetter)
	if !ok {
		return nil
	}
	return reset.ResetSession(ctx)
}
func (c *replicaHookConn) IsValid() bool {
	valid, ok := c.Conn.(driver.Validator)
	return !ok || valid.IsValid()
}

type replicaHookTx struct {
	driver.Tx
	owner *replicaHookConnector
}

func (tx *replicaHookTx) Commit() error {
	first := tx.owner.fired.CompareAndSwap(false, true)
	if first {
		tx.owner.before()
	}
	err := tx.Tx.Commit()
	if first && tx.owner.afterCommit != nil {
		tx.owner.afterCommit(err)
	}
	if first {
		tx.owner.done <- safeFailure(err)
	}
	return err
}

// 仅替换隔离夹具的连接器；业务、guard、捕获、Commit 和 ACK 仍走真实生产实现。
func ReplicaTestInstallCommitHook(db *Database, endpoint config.DatabaseEndpoint, apply bool, before func()) (<-chan error, error) {
	cfg, err := mysqlConfig(endpoint, db.domain)
	if err != nil {
		return nil, err
	}
	cfg.MultiStatements = apply
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, safeFailure(err)
	}
	hook := &replicaHookConnector{Connector: connector, before: before, done: make(chan error, 1)}
	pool := sql.OpenDB(hook)
	pool.SetMaxOpenConns(3)
	db.mu.Lock()
	var old *sql.DB
	if apply {
		old = db.applyPool
		db.applyPool = pool
	} else {
		old = db.current.writer
		db.current.writer = pool
	}
	db.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return hook.done, nil
}
func ReplicaTestStandby(dbs *Databases, domain Domain) *Database {
	return dbs.replication.domains[domain].target
}
