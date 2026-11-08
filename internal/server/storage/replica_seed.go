package storage

import (
	"context"
	"database/sql"
	"errors"
	"github.com/google/uuid"
	"strings"
)

// seed 的源 head 和全量行来自同一个 RR 快照；pin 在释放源写 gate 前登记。
func (d *replicaDomain) seed(parent context.Context) error {
	if err := d.control.revoke(parent, d.domain, "rebuilding"); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	select {
	case d.source.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := d.source.resolvePending(ctx); err != nil {
		<-d.source.writeGate
		return err
	}
	opts := &sql.TxOptions{ReadOnly: true}
	if d.source.current.dialect == MySQL {
		opts.Isolation = sql.LevelRepeatableRead
	}
	source, err := d.source.current.reader.BeginTx(ctx, opts)
	if err != nil {
		<-d.source.writeGate
		return safeFailure(err)
	}
	defer source.Rollback()
	p, err := position(ctx, source)
	if err != nil {
		<-d.source.writeGate
		return err
	}
	d.seedBlocked.Store(false)
	d.mu.Lock()
	d.pin = p.Seq
	d.pinned = true
	d.cancelSeed = cancel
	d.mu.Unlock()
	<-d.source.writeGate
	defer func() { d.mu.Lock(); d.pinned = false; d.cancelSeed = nil; d.mu.Unlock() }()
	resource, instance, err := replicaIdentity(ctx, d.target)
	if err != nil {
		return err
	}
	if resource != d.resource {
		return ErrIdentityMismatch
	}
	member := d.member()
	expected, active := member.Standby, member.Primary
	if member.Active == "standby" {
		expected, active = member.Primary, member.Standby
	}
	if instance != expected || instance == active {
		return ErrIdentityMismatch
	}
	op := &rebuildOperation{ID: uuid.NewString(), Source: active, Target: instance, Epoch: p.Epoch, Checkpoint: p, Phase: "clearing"}
	if err = d.update(ctx, func(m *memberRecord) { m.Rebuild = op }); err != nil {
		return err
	}
	if err = d.operationProgress(ctx, "clearing", 0); err != nil {
		return err
	}
	if err = validateReplicaSchema(ctx, d.target); err != nil {
		return err
	}
	packetLimit, err := replicaPacketLimit(ctx, d.target.current.reader, d.target.current.dialect)
	if err != nil {
		return err
	}
	// 每次重启从清空 inactive 目标重新开始，未完成的快照绝不标 ready。
	if err = d.targetTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE replica_state SET ready=0,capture_mode='off',open_tx_uuid='',owner_connection=0,next_ordinal=0 WHERE id=1"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM replica_changes"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM replica_transactions"); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	tables := Tables(d.domain)
	for i := len(tables) - 1; i >= 0; i-- {
		table := tables[i]
		for {
			// 先取有界 PK，再删除，避免 MySQL 同表子查询限制和无界事务。
			rows, err := d.target.current.reader.QueryContext(ctx, "SELECT "+columnNames(primaryColumns(table))+" FROM `"+table.Name+"` LIMIT 256")
			if err != nil {
				return safeFailure(err)
			}
			var keys [][]any
			for rows.Next() {
				values := make([]any, len(table.PK))
				refs := make([]any, len(values))
				for i := range values {
					refs[i] = &values[i]
				}
				if err = rows.Scan(refs...); err != nil {
					rows.Close()
					return safeFailure(err)
				}
				for i, c := range primaryColumns(table) {
					values[i], err = typedValue(values[i], c.Type)
					if err != nil {
						rows.Close()
						return err
					}
				}
				keys = append(keys, values)
			}
			err = errors.Join(rows.Err(), rows.Close())
			if err != nil {
				return safeFailure(err)
			}
			if len(keys) == 0 {
				break
			}
			if err = d.targetTransaction(ctx, func(tx *sql.Tx) error {
				var where []string
				for _, key := range table.PK {
					where = append(where, "`"+key+"`=?")
				}
				term := "(" + strings.Join(where, " AND ") + ")"
				for at := 0; at < len(keys); {
					var terms []string
					var args []any
					size := len(table.Name) + 32
					end := at
					for end < len(keys) {
						n := len(term) + 4
						for _, value := range keys[end] {
							switch v := value.(type) {
							case string:
								n += len(v)*2 + 16
							case []byte:
								n += len(v)*2 + 16
							default:
								n += 32
							}
						}
						if int64(n+len(table.Name)+32) > packetLimit {
							return ErrTransactionTooLarge
						}
						if end > at && int64(size+n) > packetLimit {
							break
						}
						terms = append(terms, term)
						args = append(args, keys[end]...)
						size += n
						end++
					}
					if _, err := tx.ExecContext(ctx, "DELETE FROM `"+table.Name+"` WHERE "+strings.Join(terms, " OR "), args...); err != nil {
						return err
					}
					at = end
				}
				return nil
			}); err != nil {
				return err
			}
		}
	}
	copying := *op
	copying.Phase = "copying"
	if err = d.update(ctx, func(m *memberRecord) { m.Rebuild = &copying }); err != nil {
		return err
	}
	if err = d.operationProgress(ctx, "copying", 0); err != nil {
		return err
	}
	var rowsCopied int64
	for _, table := range tables {
		cols := columns(table)
		rows, err := source.QueryContext(ctx, "SELECT "+columnNames(cols)+" FROM `"+table.Name+"`")
		if err != nil {
			return safeFailure(err)
		}
		var batch [][]any
		prefix := len(table.Name) + len(columnNames(cols)) + 32
		size := prefix
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			err := d.targetTransaction(ctx, func(tx *sql.Tx) error {
				q := "INSERT INTO `" + table.Name + "`(" + columnNames(cols) + ") VALUES "
				var groups []string
				var args []any
				for _, values := range batch {
					groups = append(groups, "("+strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")+")")
					args = append(args, values...)
				}
				_, err := tx.ExecContext(ctx, q+strings.Join(groups, ","), args...)
				return err
			})
			if err == nil {
				rowsCopied += int64(len(batch))
				err = d.operationProgress(ctx, "copying", rowsCopied)
			}
			batch = nil
			size = prefix
			return err
		}
		for rows.Next() {
			values := make([]any, len(cols))
			refs := make([]any, len(cols))
			for i := range values {
				refs[i] = &values[i]
			}
			if err = rows.Scan(refs...); err != nil {
				rows.Close()
				return safeFailure(err)
			}
			rowBytes := len(cols)*2 + 4
			for i, c := range cols {
				values[i], err = typedValue(values[i], c.Type)
				if err != nil {
					rows.Close()
					return err
				}
				switch v := values[i].(type) {
				case []byte:
					rowBytes += len(v)*2 + 16
				case string:
					rowBytes += len(v)*2 + 16
				default:
					rowBytes += 32
				}
			}
			if int64(prefix+rowBytes) > packetLimit {
				rows.Close()
				return ErrTransactionTooLarge
			}
			if len(batch) == 256 || int64(size+rowBytes) > packetLimit {
				if err = flush(); err != nil {
					rows.Close()
					return err
				}
			}
			batch = append(batch, values)
			size += rowBytes
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return safeFailure(err)
		}
		if err = flush(); err != nil {
			return err
		}
	}
	// epochs 是协议谱系，不是用户 payload；源读快照中的所有祖先一并复制。
	erows, err := source.QueryContext(ctx, "SELECT epoch_uuid,parent_epoch_uuid,parent_seq,parent_tx_uuid FROM replica_epochs")
	if err != nil {
		return safeFailure(err)
	}
	type epoch struct {
		id, parent, tx string
		seq            int64
	}
	var epochs []epoch
	for erows.Next() {
		var e epoch
		if err = erows.Scan(&e.id, &e.parent, &e.seq, &e.tx); err != nil {
			erows.Close()
			return safeFailure(err)
		}
		epochs = append(epochs, e)
	}
	err = errors.Join(erows.Err(), erows.Close())
	if err != nil {
		return safeFailure(err)
	}
	if err = d.targetTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM replica_epochs"); err != nil {
			return err
		}
		for _, e := range epochs {
			if _, err := tx.ExecContext(ctx, "INSERT INTO replica_epochs VALUES(?,?,?,?)", e.id, e.parent, e.seq, e.tx); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE replica_state SET epoch_uuid=?,head_seq=?,head_tx_uuid=?,retained_from_seq=?,journal_bytes=0,ready=0 WHERE id=1", p.Epoch, p.Seq, p.ID, p.Seq+1)
		return err
	}); err != nil {
		return err
	}
	d.applied.Store(p.Seq)
	catching := *op
	catching.Phase = "catching_up"
	if err = d.update(ctx, func(m *memberRecord) { m.Rebuild = &catching }); err != nil {
		return err
	}
	if err = d.operationProgress(ctx, "catching_up", rowsCopied); err != nil {
		return err
	}
	if err = d.catchUp(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	pinned := d.pinned
	d.mu.Unlock()
	if !pinned {
		return ErrUnavailable
	}
	if _, err = d.target.current.writer.ExecContext(ctx, "UPDATE replica_state SET ready=1 WHERE id=1"); err != nil {
		return safeFailure(err)
	}
	d.control.mu.Lock()
	defer d.control.mu.Unlock()
	if d.seedBlocked.Load() || ctx.Err() != nil {
		return ErrUnavailable
	}
	m := d.control.record.Domains[d.domain]
	old := *m
	m.Ready = true
	if m.Active == "standby" {
		m.State = "standby_active"
	} else {
		m.State = "healthy"
	}
	m.Rebuild = nil
	m.Epoch = p.Epoch
	m.Checkpoint = &replicaCheckpoint{Instance: instance, Position: p}
	d.control.record.Revision++
	if err = d.control.persistLocked(ctx); err != nil {
		*m = old
		d.control.record.Revision--
		return err
	}
	return nil
}
func (d *replicaDomain) targetTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	select {
	case d.target.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-d.target.writeGate }()
	tx, err := d.target.current.writer.BeginTx(ctx, nil)
	if err != nil {
		return safeFailure(err)
	}
	defer tx.Rollback()
	q := "SELECT last_tx_uuid FROM storage_write_guard WHERE id=1"
	if d.target.current.dialect == MySQL {
		q += " FOR UPDATE"
	}
	var id string
	if err = tx.QueryRowContext(ctx, q).Scan(&id); err != nil {
		return safeFailure(err)
	}
	if err = fn(tx); err != nil {
		return safeFailure(err)
	}
	return safeFailure(tx.Commit())
}
