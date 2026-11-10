package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"github.com/google/uuid"
	"sync"
)

type Domain string

const (
	MainDomain    Domain = "main"
	HistoryDomain Domain = "history"
)

type Pool uint8

const (
	Writer Pool = iota
	Reader
)

type Dialect uint8

const (
	SQLite Dialect = iota
	MySQL
)

var ErrUnavailable = errors.New("storage_unavailable")

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
func (l *Lease) Context() context.Context {
	if l.gen.ctx.Err() != nil {
		l.cancel()
	}
	return l.ctx
}
func (l *Lease) Generation() uint64 { return l.gen.id }
func (l *Lease) Dialect() Dialect   { return l.gen.dialect }
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
func (db *Database) Stats() (writer, reader sql.DBStats, ok bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed || db.current == nil || db.current.writer == nil {
		return writer, reader, false
	}
	return db.current.writer.Stats(), db.current.reader.Stats(), true
}
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
func (l *Lease) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	rows, err := l.QueryContext(ctx, query, args...)
	return &Row{rows: rows, err: err}
}
func (l *Lease) BeginRead(ctx context.Context) (*Tx, error)  { return l.begin(ctx, false) }
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
func (tx *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := tx.rejectPoisoned(); err != nil {
		return nil, err
	}
	ctx, cancel := tx.lease.merge(ctx)
	defer cancel()
	result, err := tx.tx.ExecContext(ctx, query, args...)
	return result, tx.captureFailure(err)
}
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
func (tx *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	rows, err := tx.QueryContext(ctx, query, args...)
	return &Row{rows: rows, err: err}
}
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

type Rows struct {
	rows    *sql.Rows
	once    sync.Once
	release func()
	lease   *Lease
	tx      *Tx
}

func newRows(rows *sql.Rows, release func()) *Rows { return &Rows{rows: rows, release: release} }
func (r *Rows) Close() error                       { err := r.rows.Close(); r.once.Do(r.release); return r.failure(err) }
func (r *Rows) Next() bool {
	next := r.rows.Next()
	if !next {
		_ = r.Close()
	}
	return next
}
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
func (r *Rows) Scan(dest ...any) error { return r.failure(r.rows.Scan(dest...)) }
func (r *Rows) Err() error             { return r.failure(r.rows.Err()) }
func (r *Rows) Columns() ([]string, error) {
	columns, err := r.rows.Columns()
	return columns, r.failure(err)
}
func (r *Rows) ColumnTypes() ([]*sql.ColumnType, error) {
	columns, err := r.rows.ColumnTypes()
	return columns, r.failure(err)
}

type Row struct {
	rows *Rows
	err  error
}

func (r *Row) Err() error { return r.err }
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

type Stmt struct {
	stmt    *sql.Stmt
	lease   *Lease
	once    sync.Once
	release func()
	tx      *Tx
}

func (s *Stmt) Close() error {
	err := s.stmt.Close()
	s.once.Do(s.release)
	return s.lease.failure(err)
}
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
func (s *Stmt) QueryRowContext(ctx context.Context, args ...any) *Row {
	rows, err := s.QueryContext(ctx, args...)
	return &Row{rows: rows, err: err}
}
