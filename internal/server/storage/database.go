// Package storage 提供基于世代租约写门控、SQLite/MySQL 双引擎适配与主备异步行日志复制的存储层。
package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"github.com/google/uuid"
	"sync"
)

// Domain 标识存储实例所属的逻辑数据域（控制面主库或指标历史库）。
type Domain string

const (
	// MainDomain 表示保存配置、节点状态、告警与审计事件的控制面主数据库域。
	MainDomain Domain = "main"
	// HistoryDomain 表示保存时序聚合指标桶与服务拨测历史的历史数据库域。
	HistoryDomain Domain = "history"
)

// Pool 区分从主存储获取的读写连接池角色。
type Pool uint8

const (
	// Writer 请求具备写权限的主写连接池租约。
	Writer Pool = iota
	// Reader 请求只读并发查询连接池租约。
	Reader
)

// Dialect 标识当前活跃数据库世代所使用的 SQL 方言与驱动类型。
type Dialect uint8

const (
	// SQLite 表示本地单机或备节点托管的 SQLite 存储引擎。
	SQLite Dialect = iota
	// MySQL 表示远程高可用 MySQL/InnoDB 存储引擎。
	MySQL
)

// ErrUnavailable 表示存储当前不可用、正处于排空/故障冻结状态或租约已失效。
var ErrUnavailable = errors.New("storage_unavailable")

// Databases 汇总服务端主域、历史域数据库连接及可选的主备复制控制器。
type Databases struct {
	Main, History *Database
	replication   *replication
}

type generation struct {
	id                   uint64
	dialect              Dialect
	writer, reader       *sql.DB
	maintenanceConnector driver.Connector
	ctx                  context.Context
	cancel               context.CancelFunc
	refs                 int
	idle                 chan struct{}
}

// Database 维护单个逻辑域的活动世代池、排空屏障、待决提交回执与切换钩子。
type Database struct {
	mu              sync.Mutex
	domain          Domain
	current         *generation
	admitting       bool
	closed          bool
	writeGate       chan struct{}
	pending         *commitReceipt
	writeFailed     bool
	replica         *replicaDomain
	applyPool       *sql.DB
	ownedControl    *sql.DB
	restoreAllowed  bool
	hooks           SwitchHooks
	possible        possibleCommit
	journalEpoch    string
	journalCursor   string
	journalRequired int64
}

type leaseKey struct{}
type leaseBinding struct {
	lease  *Lease
	parent *leaseBinding
}

// Lease 代表绑定到特定活跃世代的连接池访问租约，世代切换或关闭时会触发其上下文取消。
type Lease struct {
	db     *Database
	gen    *generation
	pool   Pool
	ctx    context.Context
	cancel context.CancelFunc
	stop   func() bool
	mu     sync.Mutex
	closed bool
	refs   int
}

func newDatabase(domain Domain, writer, reader *sql.DB) *Database {
	ctx, cancel := context.WithCancel(context.Background())
	idle := make(chan struct{})
	close(idle)
	return &Database{domain: domain, current: &generation{id: 1, dialect: SQLite, writer: writer, reader: reader, ctx: ctx, cancel: cancel, idle: idle}, admitting: true, writeGate: make(chan struct{}, 1)}
}

// OpenMain 打开或初始化指定路径的主库 SQLite 数据库并建立读写分离连接池。
func OpenMain(ctx context.Context, path string) (*Database, error) {
	writer, err := Open(ctx, path)
	if err != nil {
		return nil, err
	}
	reader, err := OpenReader(ctx, path)
	if err != nil {
		_ = writer.Close()
		return nil, err
	}
	return newDatabase(MainDomain, writer, reader), nil
}

// Generation 不因探测或复制延迟变化；尚未选定可用成员时为零。
func (db *Database) Generation() uint64 {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.current == nil {
		return 0
	}
	return db.current.id
}

// GenerationDone 不持有租约，复用当前世代的取消信号供空闲任务退出等待。
func (db *Database) GenerationDone() <-chan struct{} {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.current == nil || db.current.id == 0 {
		return nil
	}
	return db.current.ctx.Done()
}

// Acquire 按 pool 角色申请当前活跃世代的连接租约，必要时等待待决写入状态解决。
func (db *Database) Acquire(ctx context.Context, pool Pool) (*Lease, error) {
	if pool != Writer && pool != Reader {
		return nil, ErrUnavailable
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil, ErrUnavailable
	}
	if pool == Writer && db.restoreAllowed && ctx.Value(restoreContextKey{}) != db {
		return nil, ErrRestorePending
	}
	gen := db.current
	bindings, _ := ctx.Value(leaseKey{}).(*leaseBinding)
	bound := false
	for binding := bindings; binding != nil; binding = binding.parent {
		if binding.lease.db == db {
			binding.lease.mu.Lock()
			live := !binding.lease.closed
			binding.lease.mu.Unlock()
			if live {
				gen, bound = binding.lease.gen, true
				break
			}
		}
	}
	if !bound {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !db.admitting || gen == nil || gen.writer == nil || gen.ctx.Err() != nil {
			return nil, ErrUnavailable
		}
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(gen.ctx, cancel)
	if gen.ctx.Err() != nil {
		cancel()
	}
	lease := &Lease{db: db, gen: gen, pool: pool, cancel: cancel, stop: stop, refs: 1}
	lease.ctx = context.WithValue(leaseCtx, leaseKey{}, &leaseBinding{lease: lease, parent: bindings})
	db.retainLocked(gen)
	return lease, nil
}

func (db *Database) retainLocked(gen *generation) {
	if gen.refs == 0 {
		gen.idle = make(chan struct{})
	}
	gen.refs++
}
func (db *Database) release(gen *generation) {
	db.mu.Lock()
	defer db.mu.Unlock()
	gen.refs--
	if gen.refs == 0 {
		close(gen.idle)
	}
}
func (l *Lease) retain() (func(), error) {
	l.db.mu.Lock()
	defer l.db.mu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ErrUnavailable
	}
	l.db.retainLocked(l.gen)
	l.refs++
	var once sync.Once
	return func() { once.Do(l.release) }, nil
}
func (l *Lease) release() {
	l.mu.Lock()
	l.refs--
	if l.refs == 0 {
		l.stop()
		l.cancel()
	}
	l.mu.Unlock()
	l.db.release(l.gen)
}

// Context 返回与当前租约及其底层世代生命周期绑定的上下文。
func (l *Lease) Context() context.Context {
	if l.gen.ctx.Err() != nil {
		l.cancel()
	}
	return l.ctx
}

// Generation 返回本租约所属数据库连接的单调递增世代编号。
func (l *Lease) Generation() uint64 { return l.gen.id }

// Dialect 返回本租约底层连接池所使用的 SQL 方言。
func (l *Lease) Dialect() Dialect { return l.gen.dialect }
func (l *Lease) failure(err error) error {
	if l.db.replica != nil && storageFailure(err) {
		l.db.replica.freeze(context.Background(), "active_unreachable")
	} else if l.Dialect() == MySQL && IsConnectionFailure(err) {
		l.db.mu.Lock()
		l.db.writeFailed = true
		l.db.mu.Unlock()
	}
	if l.Dialect() == MySQL {
		return safeFailure(err)
	}
	return err
}

// Close 释放当前租约引用（已派生的活动查询和事务会各自继续持有世代直至结束）。
func (l *Lease) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	// 资源另持世代引用，Close 不会提前解除 Rows/Stmt/Tx 的排空屏障。
	l.release()
	return nil
}
func (l *Lease) merge(ctx context.Context) (context.Context, func()) {
	if l.gen.ctx.Err() != nil {
		l.cancel()
	}
	if ctx == l.ctx {
		return ctx, func() {}
	}
	merged, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.ctx, cancel)
	if l.ctx.Err() != nil {
		cancel()
	}
	return merged, func() { stop(); cancel() }
}
func (l *Lease) poolDB() *sql.DB {
	if l.pool == Reader {
		return l.gen.reader
	}
	return l.gen.writer
}

// Stats 返回当前活跃世代读写连接池的 Go database/sql 运行时统计信息。
func (db *Database) Stats() (writer, reader sql.DBStats, ok bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed || db.current == nil || db.current.writer == nil {
		return writer, reader, false
	}
	return db.current.writer.Stats(), db.current.reader.Stats(), true
}

// Ready 报告当前存储域是否已就绪且允许接纳新的请求租约。
func (db *Database) Ready() bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	return !db.closed && !db.restoreAllowed && db.admitting && !db.writeFailed && db.pending == nil && db.current != nil && db.current.ctx.Err() == nil
}
func (db *Database) drain(ctx context.Context) error {
	db.mu.Lock()
	db.admitting = false
	gen := db.current
	if gen == nil {
		db.mu.Unlock()
		return nil
	}
	gen.cancel()
	idle := gen.idle
	db.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (db *Database) publishGeneration(ctx context.Context, candidate *generation) error {
	if err := db.drain(ctx); err != nil {
		return err
	}
	db.mu.Lock()
	if db.closed {
		db.mu.Unlock()
		return ErrUnavailable
	}
	old := db.current
	genCtx, cancel := context.WithCancel(context.Background())
	idle := make(chan struct{})
	close(idle)
	db.current = &generation{id: old.id + 1, dialect: candidate.dialect, writer: candidate.writer, reader: candidate.reader, maintenanceConnector: candidate.maintenanceConnector, ctx: genCtx, cancel: cancel, idle: idle}
	db.admitting = true
	db.mu.Unlock()
	return errors.Join(old.reader.Close(), old.writer.Close())
}

// Close 排空全部活跃世代引用并关闭主备及控制面数据库连接。
func (db *Database) Close() error {
	if err := db.drain(context.Background()); err != nil {
		return err
	}
	db.mu.Lock()
	if db.closed {
		db.mu.Unlock()
		return nil
	}
	db.closed = true
	gen := db.current
	db.mu.Unlock()
	var applyErr error
	if db.applyPool != nil {
		applyErr = db.applyPool.Close()
	}
	var controlErr error
	if db.ownedControl != nil {
		controlErr = db.ownedControl.Close()
	}
	if gen == nil || gen.writer == nil {
		return errors.Join(applyErr, controlErr)
	}
	return errors.Join(gen.reader.Close(), gen.writer.Close(), applyErr, controlErr)
}

// QueryContext 在当前租约对应的连接池上执行查询并返回受生命周期追踪的 Rows 游标。
func (l *Lease) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error) {
	release, err := l.retain()
	if err != nil {
		return nil, err
	}
	ctx, cancel := l.merge(ctx)
	rows, err := l.poolDB().QueryContext(ctx, query, args...)
	if err != nil {
		cancel()
		release()
		return nil, l.failure(err)
	}
	r := newRows(rows, func() { cancel(); release() })
	r.lease = l
	return r, nil
}

// QueryRowContext 在当前租约上执行单行查询；底层连接引用将在 Scan 完成时释放。
func (l *Lease) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	rows, err := l.QueryContext(ctx, query, args...)
	return &Row{rows: rows, err: err}
}

// BeginRead 在租约上开启只读数据库事务。
func (l *Lease) BeginRead(ctx context.Context) (*Tx, error) { return l.begin(ctx, false) }

// BeginWrite 在 Writer 租约上开启具备写门控与可选行日志捕获的写事务。
func (l *Lease) BeginWrite(ctx context.Context) (*Tx, error) { return l.begin(ctx, true) }
func (l *Lease) begin(ctx context.Context, write bool) (*Tx, error) {
	if write && l.pool != Writer {
		return nil, ErrUnavailable
	}
	if write {
		if pending, allowed := l.db.restoreAccess(ctx); pending && !allowed {
			return nil, ErrRestorePending
		}
	}
	release, err := l.retain()
	if err != nil {
		return nil, err
	}
	ctx, cancel := l.merge(ctx)
	finish := func() { cancel(); release() }
	pool := l.gen.reader
	options := &sql.TxOptions{ReadOnly: true}
	if l.Dialect() == MySQL {
		options.Isolation = sql.LevelRepeatableRead
	}
	if write {
		if probe, _ := ctx.Value(replicaProbeKey{}).(bool); probe {
			select {
			case l.db.writeGate <- struct{}{}:
			default:
				finish()
				return nil, errReplicaProbeBusy
			}
		} else {
			select {
			case l.db.writeGate <- struct{}{}:
			case <-ctx.Done():
				finish()
				return nil, ctx.Err()
			}
		}
		finish = func() { <-l.db.writeGate; cancel(); release() }
		pool, options = l.gen.writer, nil
		if l.Dialect() == MySQL || l.db.replica != nil {
			if err := l.db.resolvePending(ctx); err != nil {
				finish()
				return nil, err
			}
		}
		if l.Dialect() == MySQL {
			options = &sql.TxOptions{Isolation: sql.LevelReadCommitted}
		}
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		finish()
		return nil, l.failure(err)
	}
	tx, err := conn.BeginTx(ctx, options)
	if err != nil {
		_ = conn.Close()
		finish()
		return nil, l.failure(err)
	}
	var receipt *commitReceipt
	if write && l.Dialect() == MySQL {
		var previous string
		if err = tx.QueryRowContext(ctx, "SELECT last_tx_uuid FROM storage_write_guard WHERE id=1 FOR UPDATE").Scan(&previous); err != nil {
			err = l.failure(err)
			_ = tx.Rollback()
			_ = conn.Close()
			finish()
			return nil, err
		}
		receipt = &commitReceipt{id: uuid.NewString()}
	}
	result := &Tx{tx: tx, lease: l, ctx: ctx, receipt: receipt, finish: func() {
		// sql.Tx 已标记完成时，自动回滚仍可能占用连接；先等待 driver 退出。
		_ = conn.Close()
		finish()
	}}
	if write && l.db.replica != nil {
		if err = result.beginCapture(); err != nil {
			_ = result.Rollback()
			return nil, l.failure(err)
		}
	}
	return result, nil
}

// Tx 封装基于租约与写门控受控提交协议的数据库事务，并在开启复制时捕获行级变更日志。
type Tx struct {
	tx           *sql.Tx
	lease        *Lease
	ctx          context.Context
	mu           sync.Mutex
	finish       func()
	receipt      *commitReceipt
	capture      *replicaTransaction
	captureError error
	captureMu    sync.Mutex
}

// Commit 完成复制日志密封并提交事务；若提交状态未知则设置待决屏障并返回 ErrCommitUnknown。
func (tx *Tx) Commit() error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.finish == nil {
		return sql.ErrTxDone
	}
	var err error
	if err = tx.lease.gen.ctx.Err(); err == nil {
		err = tx.lease.ctx.Err()
	}
	if err == nil {
		err = tx.ctx.Err()
	}
	if err != nil {
		_ = tx.tx.Rollback()
	} else {
		if tx.capture != nil {
			err = tx.finishCapture()
		}
		if tx.receipt != nil && err == nil {
			_, err = tx.tx.ExecContext(tx.ctx, "UPDATE storage_write_guard SET last_tx_uuid=? WHERE id=1", tx.receipt.id)
		}
		if err != nil {
			_ = tx.tx.Rollback()
		} else {
			if tx.capture != nil {
				tx.lease.db.notePossibleCommit(tx.capture.Boot, tx.capture.replicaPosition)
			}
			err = tx.tx.Commit()
			if err != nil && tx.receipt != nil {
				tx.lease.db.mu.Lock()
				tx.lease.db.pending = tx.receipt
				tx.lease.db.mu.Unlock()
				err = ErrCommitUnknown
			}
		}
	}
	if tx.receipt != nil && !errors.Is(err, ErrCommitUnknown) {
		tx.receipt.mu.Lock()
		tx.receipt.known = true
		tx.receipt.committed = err == nil
		tx.receipt.mu.Unlock()
	}
	if tx.receipt != nil && err == nil {
		tx.lease.db.mu.Lock()
		tx.lease.db.writeFailed = false
		tx.lease.db.mu.Unlock()
		tx.lease.db.journalCommitted(tx.receipt.journalBytes)
	}
	err = tx.lease.failure(err)
	if tx.finish != nil {
		tx.finish()
		tx.finish = nil
	}
	return err
}

// Rollback 回滚未提交事务并释放关联的世代与回执状态。
func (tx *Tx) Rollback() error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	err := tx.tx.Rollback()
	if tx.receipt != nil {
		tx.receipt.mu.Lock()
		if !tx.receipt.known && tx.finish != nil {
			tx.receipt.known = true
		}
		tx.receipt.mu.Unlock()
	}
	err = tx.lease.failure(err)
	if tx.finish != nil {
		tx.finish()
		tx.finish = nil
	}
	return err
}

// ExecContext 在事务内执行写语句或 DDL/DML，并对复制事务校验毒化状态。
func (tx *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := tx.rejectPoisoned(); err != nil {
		return nil, err
	}
	ctx, cancel := tx.lease.merge(ctx)
	defer cancel()
	result, err := tx.tx.ExecContext(ctx, query, args...)
	return result, tx.captureFailure(err)
}

// QueryContext 在事务内执行查询并返回追踪租约引用的 Rows 游标。
func (tx *Tx) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error) {
	release, err := tx.lease.retain()
	if err != nil {
		return nil, err
	}
	ctx, cancel := tx.lease.merge(ctx)
	rows, err := tx.tx.QueryContext(ctx, query, args...)
	if err != nil {
		cancel()
		release()
		return nil, tx.captureFailure(err)
	}
	r := newRows(rows, func() { cancel(); release() })
	r.lease = tx.lease
	r.tx = tx
	return r, nil
}

// QueryRowContext 在事务内执行预期最多返回一行的查询。
func (tx *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	rows, err := tx.QueryContext(ctx, query, args...)
	return &Row{rows: rows, err: err}
}

// PrepareContext 在事务内预编译 SQL 语句并返回绑定事务与租约引用的 Stmt。
func (tx *Tx) PrepareContext(ctx context.Context, query string) (*Stmt, error) {
	release, err := tx.lease.retain()
	if err != nil {
		return nil, err
	}
	ctx, cancel := tx.lease.merge(ctx)
	stmt, err := tx.tx.PrepareContext(ctx, query)
	cancel()
	if err != nil {
		release()
		return nil, tx.lease.failure(err)
	}
	return &Stmt{stmt: stmt, lease: tx.lease, release: release, tx: tx}, nil
}

// Rows 封装 *sql.Rows 并持有世代排空屏障引用，关闭或耗尽时释放连接引用。
type Rows struct {
	rows    *sql.Rows
	once    sync.Once
	release func()
	lease   *Lease
	tx      *Tx
}

func newRows(rows *sql.Rows, release func()) *Rows { return &Rows{rows: rows, release: release} }

// Close 关闭结果集、释放一次租约世代引用并将底层连接故障映射为安全错误。
func (r *Rows) Close() error { err := r.rows.Close(); r.once.Do(r.release); return r.failure(err) }

// Next 推进至下一行，遍历结束时自动关闭并释放世代引用。
func (r *Rows) Next() bool {
	next := r.rows.Next()
	if !next {
		_ = r.Close()
	}
	return next
}

// NextResultSet 推进至多结果集中的下一个结果集，若无更多结果则自动关闭游标。
func (r *Rows) NextResultSet() bool {
	next := r.rows.NextResultSet()
	if !next {
		_ = r.Close()
	}
	return next
}
func (r *Rows) failure(err error) error {
	if r.tx != nil {
		return r.tx.captureFailure(err)
	}
	if r.lease != nil {
		return r.lease.failure(err)
	}
	return err
}

// Scan 将当前行各列扫描到目标变量并映射底层存储故障。
func (r *Rows) Scan(dest ...any) error { return r.failure(r.rows.Scan(dest...)) }

// Err 返回迭代过程中遇到的首个错误（已映射连接故障与安全错误码）。
func (r *Rows) Err() error { return r.failure(r.rows.Err()) }

// Columns 返回当前结果集的列名列表。
func (r *Rows) Columns() ([]string, error) {
	columns, err := r.rows.Columns()
	return columns, r.failure(err)
}

// ColumnTypes 返回当前结果集各列的数据库元数据与类型描述。
func (r *Rows) ColumnTypes() ([]*sql.ColumnType, error) {
	columns, err := r.rows.ColumnTypes()
	return columns, r.failure(err)
}

// Row 封装单行查询结果并在 Scan 时自动关闭底层 Rows 游标。
type Row struct {
	rows *Rows
	err  error
}

// Err 返回单行查询预检查阶段捕获的错误。
func (r *Row) Err() error { return r.err }

// Scan 将单行结果扫描到目标变量，无结果时返回 sql.ErrNoRows 并关闭底层游标。
func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer r.rows.Close()
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if err := r.rows.Scan(dest...); err != nil {
		return err
	}
	return r.rows.Close()
}

// Stmt 封装预编译 SQL 语句并绑定对应租约或事务生命周期。
type Stmt struct {
	stmt    *sql.Stmt
	lease   *Lease
	once    sync.Once
	release func()
	tx      *Tx
}

// Close 关闭预编译语句并释放一次世代租约引用。
func (s *Stmt) Close() error {
	err := s.stmt.Close()
	s.once.Do(s.release)
	return s.lease.failure(err)
}

// ExecContext 执行预编译写语句，在关联事务中同时维护毒化检测与行日志捕获。
func (s *Stmt) ExecContext(ctx context.Context, args ...any) (sql.Result, error) {
	if s.tx != nil {
		if err := s.tx.rejectPoisoned(); err != nil {
			return nil, err
		}
	}
	ctx, cancel := s.lease.merge(ctx)
	defer cancel()
	result, err := s.stmt.ExecContext(ctx, args...)
	if s.tx != nil {
		return result, s.tx.captureFailure(err)
	}
	return result, s.lease.failure(err)
}

// QueryContext 执行预编译查询语句并返回受世代追踪的 Rows 游标。
func (s *Stmt) QueryContext(ctx context.Context, args ...any) (*Rows, error) {
	release, err := s.lease.retain()
	if err != nil {
		return nil, err
	}
	ctx, cancel := s.lease.merge(ctx)
	rows, err := s.stmt.QueryContext(ctx, args...)
	if err != nil {
		cancel()
		release()
		if s.tx != nil {
			return nil, s.tx.captureFailure(err)
		}
		return nil, s.lease.failure(err)
	}
	r := newRows(rows, func() { cancel(); release() })
	r.lease = s.lease
	r.tx = s.tx
	return r, nil
}

// QueryRowContext 执行预期返回单行的预编译查询语句。
func (s *Stmt) QueryRowContext(ctx context.Context, args ...any) *Row {
	rows, err := s.QueryContext(ctx, args...)
	return &Row{rows: rows, err: err}
}
