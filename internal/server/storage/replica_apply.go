package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	sqlite3 "modernc.org/sqlite/lib"
)

// Raw 返回此无 Unwrap 的哨兵，先结束 sql.Tx，再丢弃坏连接，避免 closemu 自锁。
var errRawDiscard = errors.New("storage_raw_connection_discard")

func replicaApplyFailure(err error) error {
	if code, ok := MySQLCode(err); ok {
		switch code {
		case 1048, 1062, 1451, 1452, 3819:
			return errors.Join(ErrReplicaDiverged, safeFailure(err))
		}
	}
	if code, ok := SQLiteCode(err); ok && code&0xff == sqlite3.SQLITE_CONSTRAINT {
		return errors.Join(ErrReplicaDiverged, safeFailure(err))
	}
	return safeFailure(err)
}

func readReplicaTransaction(ctx context.Context, source *Database, after int64) (*replicaTransaction, error) {
	if after < 0 || after == math.MaxInt64 {
		return nil, ErrReplicaDiverged
	}
	opts := &sql.TxOptions{ReadOnly: true}
	if source.current.dialect == MySQL {
		opts.Isolation = sql.LevelRepeatableRead
	}
	read, err := source.current.reader.BeginTx(ctx, opts)
	if err != nil {
		return nil, safeFailure(err)
	}
	defer read.Rollback()
	batch := &replicaTransaction{}
	if err = read.QueryRowContext(ctx, "SELECT i.resource_uuid,s.epoch_uuid FROM replica_identity i JOIN replica_state s ON s.id=i.id WHERE i.id=1").Scan(&batch.Resource, &batch.Epoch); err != nil {
		return nil, safeFailure(err)
	}
	err = read.QueryRowContext(ctx, "SELECT epoch_uuid,seq,tx_uuid,previous_tx_uuid,boot_id,barrier_ns,change_count,payload_bytes FROM replica_transactions WHERE epoch_uuid=? AND seq=?", batch.Epoch, after+1).Scan(&batch.Epoch, &batch.Seq, &batch.ID, &batch.Previous, &batch.Boot, &batch.Barrier, &batch.Count, &batch.Bytes)
	if err != nil {
		return nil, err
	}
	if batch.Bytes > maxTransactionBytes || batch.Count < 0 || batch.Count > maxTransactionBytes/64 {
		return nil, ErrTransactionTooLarge
	}
	rows, err := read.QueryContext(ctx, "SELECT ordinal,table_id,operation,primary_key,after_image FROM replica_changes WHERE epoch_uuid=? AND seq=? ORDER BY ordinal", batch.Epoch, batch.Seq)
	if err != nil {
		return nil, safeFailure(err)
	}
	defer rows.Close()
	bytes := transactionOverhead
	for rows.Next() {
		var c replicaChange
		var pk, after []byte
		if err = rows.Scan(&c.Ordinal, &c.Table, &c.Operation, &pk, &after); err != nil {
			return nil, safeFailure(err)
		}
		c.PK = pk
		c.After = after
		batch.Changes = append(batch.Changes, c)
		bytes += int64(len(pk) + len(after) + 64)
		if bytes > maxTransactionBytes {
			return nil, ErrTransactionTooLarge
		}
	}
	if err = rows.Err(); err != nil {
		return nil, safeFailure(err)
	}
	if int64(len(batch.Changes)) != batch.Count || bytes != batch.Bytes {
		return nil, ErrReplicaDiverged
	}
	return batch, nil
}

type replicaStatement struct {
	query string
	args  []any
}

func changeStatement(domain Domain, c replicaChange) (replicaStatement, error) {
	spec, ok := replicaTemplates[c.Table]
	if !ok || spec.domain != domain {
		return replicaStatement{}, ErrReplicaDiverged
	}
	table := &spec.table
	keys, err := decodeValues(c.PK, spec.keys)
	if err != nil {
		return replicaStatement{}, err
	}
	for _, key := range keys {
		if key == nil {
			return replicaStatement{}, ErrReplicaDiverged
		}
	}
	var s replicaStatement
	switch c.Operation {
	case "i", "u":
		cols := spec.cols
		values, err := decodeValues(c.After, cols)
		if err != nil {
			return s, err
		}
		for k, name := range table.PK {
			for i, col := range cols {
				if col.Name == name && !reflect.DeepEqual(values[i], keys[k]) {
					return s, ErrReplicaDiverged
				}
			}
		}
		if c.Operation == "i" {
			s.query = spec.insert
			s.args = values
		} else {
			s.query = spec.update
			s.args = append(values, keys...)
		}
	case "d":
		if len(c.After) != 0 && string(c.After) != "null" {
			return s, ErrReplicaDiverged
		}
		s.query = spec.delete
		s.args = keys
	default:
		return s, ErrReplicaDiverged
	}
	return s, nil
}

func applyReplicaTransaction(ctx context.Context, target *Database, batch *replicaTransaction) error {
	if batch == nil || batch.ID == "" || batch.Seq < 1 || batch.Bytes > maxTransactionBytes || int64(len(batch.Changes)) != batch.Count {
		return ErrReplicaDiverged
	}
	size := transactionOverhead
	statements := make([]replicaStatement, len(batch.Changes))
	for i, c := range batch.Changes {
		if c.Ordinal != int64(i) {
			return ErrReplicaDiverged
		}
		size += int64(len(c.PK) + len(c.After) + 64)
		s, err := changeStatement(target.domain, c)
		if err != nil {
			return err
		}
		statements[i] = s
	}
	if size != batch.Bytes {
		return ErrReplicaDiverged
	}
	select {
	case target.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-target.writeGate }()
	pool := target.current.writer
	if target.applyPool != nil {
		pool = target.applyPool
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		return safeFailure(err)
	}
	defer conn.Close()
	opts := &sql.TxOptions{}
	if target.current.dialect == MySQL {
		opts.Isolation = sql.LevelReadCommitted
	}
	tx, err := conn.BeginTx(ctx, opts)
	if err != nil {
		return safeFailure(err)
	}
	defer tx.Rollback()
	lock := "SELECT last_tx_uuid FROM storage_write_guard WHERE id=1"
	if target.current.dialect == MySQL {
		lock += " FOR UPDATE"
	}
	var guard string
	if err = tx.QueryRowContext(ctx, lock).Scan(&guard); err != nil {
		return safeFailure(err)
	}
	resource, _, err := replicaIdentity(ctx, target)
	if err != nil {
		return err
	}
	if resource != batch.Resource {
		return ErrIdentityMismatch
	}
	p, err := position(ctx, tx)
	if err != nil {
		return err
	}
	if batch.Seq <= p.Seq {
		var id string
		err = tx.QueryRowContext(ctx, "SELECT tx_uuid FROM replica_transactions WHERE epoch_uuid=? AND seq=?", batch.Epoch, batch.Seq).Scan(&id)
		if batch.Seq == p.Seq && batch.Epoch == p.Epoch && batch.ID == p.ID {
			target.journalCommitted(batch.Bytes)
			return nil
		}
		if err == nil && id == batch.ID {
			target.journalCommitted(batch.Bytes)
			return nil
		}
		return ErrReplicaDiverged
	}
	if p.Epoch != batch.Epoch || batch.Seq != p.Seq+1 || p.ID != batch.Previous {
		return ErrReplicaDiverged
	}
	if _, err = tx.ExecContext(ctx, "UPDATE replica_state SET capture_mode='off' WHERE id=1"); err != nil {
		return safeFailure(err)
	}
	if target.current.dialect == MySQL {
		if err = applyMySQLStatements(ctx, conn, tx, statements); err != nil {
			return replicaApplyFailure(err)
		}
	} else {
		cache := map[string]*sql.Stmt{}
		defer func() {
			for _, s := range cache {
				s.Close()
			}
		}()
		for _, s := range statements {
			stmt := cache[s.query]
			if stmt == nil {
				stmt, err = tx.PrepareContext(ctx, s.query)
				if err != nil {
					return safeFailure(err)
				}
				cache[s.query] = stmt
			}
			r, err := stmt.ExecContext(ctx, s.args...)
			if err != nil {
				return replicaApplyFailure(err)
			}
			n, err := r.RowsAffected()
			if err != nil {
				return safeFailure(err)
			}
			if n != 1 {
				return ErrReplicaDiverged
			}
		}
	}
	local := &replicaDomain{source: target}
	if err = local.pruneTx(ctx, tx, time.Now(), batch.Bytes); err != nil {
		return err
	}
	if err = insertReplicaLog(ctx, tx, batch, target.current.dialect); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE replica_state SET head_seq=?,head_tx_uuid=?,journal_bytes=journal_bytes+?,open_tx_uuid='',next_ordinal=0,owner_connection=0 WHERE id=1", batch.Seq, batch.ID, batch.Bytes); err != nil {
		return safeFailure(err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE storage_write_guard SET last_tx_uuid=? WHERE id=1", batch.ID); err != nil {
		return safeFailure(err)
	}
	if err = tx.Commit(); err == nil {
		target.journalCommitted(batch.Bytes)
		return nil
	}
	// ACK 未知只读新 guard 解析，不重做业务行。
	_ = conn.Close()
	return resolveReplicaACK(ctx, target, batch)
}
func insertReplicaLog(ctx context.Context, tx *sql.Tx, b *replicaTransaction, d Dialect) error {
	limit := int64(1 << 20)
	if len(b.Changes) > 0 {
		var err error
		limit, err = replicaPacketLimit(ctx, tx, d)
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO replica_transactions VALUES(?,?,?,?,?,?,?,?)", b.Epoch, b.Seq, b.ID, b.Previous, b.Boot, b.Barrier, b.Count, b.Bytes); err != nil {
		return safeFailure(err)
	}
	for at := 0; at < len(b.Changes); {
		var groups []string
		var args []any
		size := 0
		end := at
		for end < len(b.Changes) && end-at < 256 {
			c := b.Changes[end]
			n := (len(c.PK)+len(c.After))*2 + 256
			if int64(n) > limit {
				return ErrTransactionTooLarge
			}
			if end > at && int64(size+n) > limit {
				break
			}
			var after any
			if len(c.After) > 0 {
				after = string(c.After)
			}
			groups = append(groups, "(?,?,?,?,?,?,?)")
			args = append(args, b.Epoch, b.Seq, c.Ordinal, c.Table, c.Operation, string(c.PK), after)
			size += n
			end++
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO replica_changes VALUES "+strings.Join(groups, ","), args...); err != nil {
			return safeFailure(err)
		}
		at = end
	}
	return nil
}
func resolveReplicaACK(ctx context.Context, target *Database, b *replicaTransaction) error {
	conn, err := target.current.writer.Conn(ctx)
	if err != nil {
		return ErrCommitUnknown
	}
	defer conn.Close()
	opts := &sql.TxOptions{}
	if target.current.dialect == MySQL {
		opts.Isolation = sql.LevelReadCommitted
	}
	tx, err := conn.BeginTx(ctx, opts)
	if err != nil {
		return ErrCommitUnknown
	}
	defer tx.Rollback()
	q := "SELECT last_tx_uuid FROM storage_write_guard WHERE id=1"
	if target.current.dialect == MySQL {
		q += " FOR UPDATE"
	}
	var id string
	if err = tx.QueryRowContext(ctx, q).Scan(&id); err != nil {
		return ErrCommitUnknown
	}
	p, err := position(ctx, tx)
	if err != nil {
		return ErrCommitUnknown
	}
	if p.Epoch == b.Epoch && p.Seq == b.Seq && p.ID == b.ID {
		target.journalCommitted(b.Bytes)
		return nil
	}
	if p.Epoch == b.Epoch && p.Seq == b.Seq-1 && p.ID == b.Previous {
		return errors.New("storage_replica_apply_rolled_back")
	}
	return ErrReplicaDiverged
}
func applyMySQLStatements(ctx context.Context, conn *sql.Conn, tx *sql.Tx, statements []replicaStatement) error {
	if len(statements) == 0 {
		return nil
	}
	limit, err := replicaPacketLimit(ctx, tx, MySQL)
	if err != nil {
		return err
	}
	for at := 0; at < len(statements); {
		var queries []string
		var args []driver.NamedValue
		var bytes int64
		end := at
		for end < len(statements) && end-at < 256 {
			s := statements[end]
			n := int64(len(s.query))
			for _, arg := range s.args {
				switch v := arg.(type) {
				case string:
					n += int64(len(v))*2 + 16
				case []byte:
					n += int64(len(v))*2 + 16
				default:
					n += 32
				}
			}
			if n > limit {
				return ErrTransactionTooLarge
			}
			if end > at && bytes+n > limit {
				break
			}
			queries = append(queries, s.query)
			for _, arg := range s.args {
				value, err := driver.DefaultParameterConverter.ConvertValue(arg)
				if err != nil {
					return err
				}
				args = append(args, driver.NamedValue{Ordinal: len(args) + 1, Value: value})
			}
			bytes += n
			end++
		}
		var original error
		err := conn.Raw(func(raw any) error {
			exec, ok := raw.(driver.ExecerContext)
			if !ok {
				original = errors.New("storage_replica_driver_unsupported")
				return errRawDiscard
			}
			result, e := exec.ExecContext(ctx, strings.Join(queries, ";"), args)
			if e != nil {
				original = e
				if errors.Is(e, driver.ErrBadConn) {
					return errRawDiscard
				}
				return e
			}
			multi, ok := result.(mysql.Result)
			if !ok {
				original = errors.New("storage_replica_result_unsupported")
				return errRawDiscard
			}
			counts := multi.AllRowsAffected()
			if len(counts) != end-at {
				return ErrReplicaDiverged
			}
			for _, n := range counts {
				if n != 1 {
					return ErrReplicaDiverged
				}
			}
			return nil
		})
		if err != nil {
			if original != nil {
				if errors.Is(original, driver.ErrBadConn) {
					_ = tx.Rollback()
					_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				}
				return original
			}
			return err
		}
		at = end
	}
	return nil
}
