package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net/url"
	"strings"

	"github.com/go-sql-driver/mysql"
	"orbitbeacon/internal/config"
)

type replicaObservedConnector struct {
	driver.Connector
	before func(context.Context, string, []driver.NamedValue)
}

func (c *replicaObservedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &replicaObservedConn{Conn: conn, before: c.before}, nil
}

type replicaObservedConn struct {
	driver.Conn
	before func(context.Context, string, []driver.NamedValue)
}

func (c *replicaObservedConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, o)
	}
	return c.Conn.Begin()
}
func (c *replicaObservedConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.before(ctx, q, args)
	if e, ok := c.Conn.(driver.ExecerContext); ok {
		return e.ExecContext(ctx, q, args)
	}
	return nil, driver.ErrSkip
}
func (c *replicaObservedConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.before(ctx, q, args)
	if e, ok := c.Conn.(driver.QueryerContext); ok {
		return e.QueryContext(ctx, q, args)
	}
	return nil, driver.ErrSkip
}
func (c *replicaObservedConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, q)
	}
	return c.Conn.Prepare(q)
}
func (c *replicaObservedConn) CheckNamedValue(v *driver.NamedValue) error {
	if check, ok := c.Conn.(driver.NamedValueChecker); ok {
		return check.CheckNamedValue(v)
	}
	return driver.ErrSkip
}
func (c *replicaObservedConn) ResetSession(ctx context.Context) error {
	if reset, ok := c.Conn.(driver.SessionResetter); ok {
		return reset.ResetSession(ctx)
	}
	return nil
}
func (c *replicaObservedConn) IsValid() bool {
	valid, ok := c.Conn.(driver.Validator)
	return !ok || valid.IsValid()
}

type replicaSQLiteConnector struct {
	dsn    string
	driver driver.Driver
}

func (c *replicaSQLiteConnector) Driver() driver.Driver { return c.driver }
func (c *replicaSQLiteConnector) Connect(context.Context) (driver.Conn, error) {
	return c.driver.Open(c.dsn)
}

// 只观察/同步真实驱动调用；不代替 SQL 结果，也不改生产 SQL。
func observeReplicaDatabase(ctx context.Context, db *Database, endpoint *config.DatabaseEndpoint, before func(context.Context, string, []driver.NamedValue)) error {
	var writerBase, readerBase driver.Connector
	if db.current.dialect == MySQL {
		cfg, err := mysqlConfig(*endpoint, db.domain)
		if err != nil {
			return err
		}
		base, err := mysql.NewConnector(cfg)
		if err != nil {
			return err
		}
		writerBase, readerBase = base, base
	} else {
		var index int
		var name, path string
		if err := db.current.reader.QueryRowContext(ctx, "PRAGMA database_list").Scan(&index, &name, &path); err != nil {
			return err
		}
		u := url.URL{Scheme: "file", Path: path}
		q := url.Values{"_pragma": {"journal_mode(WAL)", "foreign_keys(ON)", "busy_timeout(5000)", "synchronous(FULL)", "mmap_size(0)", "cache_size(-8192)"}, "_txlock": {"immediate"}}
		u.RawQuery = q.Encode()
		writerBase = &replicaSQLiteConnector{dsn: u.String(), driver: db.current.writer.Driver()}
		q = url.Values{"mode": {"ro"}, "_pragma": {"query_only(ON)", "busy_timeout(5000)", "mmap_size(0)", "cache_size(-8192)"}}
		u.RawQuery = q.Encode()
		readerBase = &replicaSQLiteConnector{dsn: u.String(), driver: db.current.reader.Driver()}
	}
	writer := sql.OpenDB(&replicaObservedConnector{Connector: writerBase, before: before})
	reader := sql.OpenDB(&replicaObservedConnector{Connector: readerBase, before: before})
	writer.SetMaxOpenConns(3)
	reader.SetMaxOpenConns(4)
	db.mu.Lock()
	oldWriter, oldReader := db.current.writer, db.current.reader
	db.current.writer, db.current.reader = writer, reader
	db.mu.Unlock()
	oldWriter.Close()
	oldReader.Close()
	return nil
}
func replicaProtocolObjects(ctx context.Context, db *Database) ([]string, error) {
	rows, err := db.current.reader.QueryContext(ctx, "SELECT name,type,sql FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, kind, ddl string
		if err = rows.Scan(&name, &kind, &ddl); err != nil {
			return nil, err
		}
		out = append(out, strings.Join([]string{name, kind, ddl}, "|"))
	}
	return out, rows.Err()
}
