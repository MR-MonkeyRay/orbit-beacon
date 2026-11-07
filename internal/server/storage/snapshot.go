package storage

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"orbitbeacon/internal/server/securefs"
)

// Snapshot 将租约固定的同一读快照复制为可信 SQLite 镜像，不带端点协议身份。
func (l *Lease) Snapshot(ctx context.Context, output string) (err error) {
	if err = securefs.ValidateDirectory(filepath.Dir(output)); err != nil {
		return err
	}
	file, err := securefs.OpenFile(output, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(output)
			_ = os.Remove(output + "-wal")
			_ = os.Remove(output + "-shm")
		}
	}()
	var target *sql.DB
	if l.db.domain == HistoryDomain {
		target, err = OpenHistorySQLite(ctx, output)
	} else {
		target, err = Open(ctx, output)
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, target.Close()) }()
	source, err := l.BeginRead(ctx)
	if err != nil {
		return err
	}
	defer source.Rollback()
	expected := CurrentSchemaVersion
	if l.db.domain == HistoryDomain {
		expected = HistorySchemaVersion
	}
	var count, version int
	if err = source.QueryRowContext(ctx, "SELECT count(*),max(version) FROM schema_migrations").Scan(&count, &version); err != nil || count != 1 || version != expected {
		return ErrSchemaIncompatible
	}
	dest, err := target.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer dest.Rollback()
	if err = clearTables(ctx, dest, l.db.domain); err != nil {
		return err
	}
	if err = copyTables(ctx, source, dest, l.db.domain); err != nil {
		return err
	}
	if err = dest.Commit(); err != nil {
		return err
	}
	var busy, logFrames, checkpointed int
	if err = target.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 || logFrames != checkpointed {
		return errors.New("storage_snapshot_checkpoint_busy")
	}
	var mode string
	if err = target.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		return err
	}
	if mode != "delete" {
		return errors.New("storage_snapshot_not_single_file")
	}
	if err = target.Close(); err != nil {
		return err
	}
	file, err = securefs.OpenFile(output, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

type rowReader interface {
	QueryContext(context.Context, string, ...any) (*Rows, error)
}
type rowWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func columns(table TableSpec) []ColumnSpec {
	out := make([]ColumnSpec, 0, len(table.Columns))
	for _, c := range table.Columns {
		if !c.Generated {
			out = append(out, c)
		}
	}
	return out
}
func columnNames(cols []ColumnSpec) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = "`" + c.Name + "`"
	}
	return strings.Join(names, ",")
}
func clearTables(ctx context.Context, dest rowWriter, domain Domain) error {
	tables := Tables(domain)
	for i := len(tables) - 1; i >= 0; i-- {
		if _, err := dest.ExecContext(ctx, "DELETE FROM `"+tables[i].Name+"`"); err != nil {
			return safeFailure(err)
		}
	}
	return nil
}
func copyTables(ctx context.Context, source rowReader, dest rowWriter, domain Domain) error {
	for _, table := range Tables(domain) {
		cols := columns(table)
		names := columnNames(cols)
		rows, err := source.QueryContext(ctx, "SELECT "+names+" FROM `"+table.Name+"`")
		if err != nil {
			return safeFailure(err)
		}
		query := "INSERT INTO `" + table.Name + "`(" + names + ") VALUES(" + strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",") + ")"
		values := make([]any, len(cols))
		pointers := make([]any, len(cols))
		for i := range values {
			pointers[i] = &values[i]
		}
		for rows.Next() {
			if err = rows.Scan(pointers...); err != nil {
				_ = rows.Close()
				return safeFailure(err)
			}
			for i, c := range cols {
				values[i], err = typedValue(values[i], c.Type)
				if err != nil {
					_ = rows.Close()
					return err
				}
			}
			if _, err = dest.ExecContext(ctx, query, values...); err != nil {
				_ = rows.Close()
				return safeFailure(err)
			}
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return safeFailure(err)
		}
	}
	return nil
}
func typedValue(value any, kind ValueType) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch kind {
	case Text:
		switch v := value.(type) {
		case string:
			return v, nil
		case []byte:
			return string(v), nil
		}
	case Blob:
		switch v := value.(type) {
		case []byte:
			if len(v) == 0 {
				return []byte{}, nil
			}
			return v, nil
		case string:
			return []byte(v), nil
		}
	case Integer:
		var n int64
		var err error
		switch v := value.(type) {
		case int64:
			return v, nil
		case []byte:
			n, err = strconv.ParseInt(string(v), 10, 64)
		case string:
			n, err = strconv.ParseInt(v, 10, 64)
		default:
			return nil, errors.New("storage_snapshot_type_invalid")
		}
		if err == nil {
			return n, nil
		}
	case Float:
		var f float64
		var err error
		switch v := value.(type) {
		case float64:
			f = v
		case int64:
			f = float64(v)
		case []byte:
			f, err = strconv.ParseFloat(string(v), 64)
		case string:
			f, err = strconv.ParseFloat(v, 64)
		default:
			return nil, errors.New("storage_snapshot_type_invalid")
		}
		if err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return f, nil
		}
	}
	return nil, errors.New("storage_snapshot_type_invalid")
}

type sqliteRows struct{ tx *sql.Tx }

func (s sqliteRows) QueryContext(ctx context.Context, q string, args ...any) (*Rows, error) {
	r, err := s.tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return newRows(r, func() {}), nil
}

// ReplaceDomain 只接受可信 schema2 镜像；操作收据和全部逻辑行在同一目标事务中。
func ReplaceDomain(ctx context.Context, target *Database, sourcePath, operationID string) error {
	if operationID == "" || len(operationID) > 64 {
		return errors.New("storage_restore_operation_invalid")
	}
	source, err := OpenReader(ctx, sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	read, err := source.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer read.Rollback()
	expected := CurrentSchemaVersion
	if target.domain == HistoryDomain {
		expected = HistorySchemaVersion
	}
	var count, version int
	if err = read.QueryRowContext(ctx, "SELECT count(*),max(version) FROM schema_migrations").Scan(&count, &version); err != nil || count != 1 || version != expected {
		return ErrSchemaIncompatible
	}
	lease, err := target.Acquire(ctx, Writer)
	if err != nil {
		return err
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var installed string
	if err = tx.QueryRowContext(ctx, "SELECT restore_operation FROM storage_write_guard WHERE id=1").Scan(&installed); err != nil {
		return safeFailure(err)
	}
	if installed == operationID {
		return nil
	}
	if err = clearTables(ctx, tx, target.domain); err != nil {
		return err
	}
	if err = copyTables(ctx, sqliteRows{read}, tx, target.domain); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE storage_write_guard SET restore_operation=? WHERE id=1", operationID); err != nil {
		return safeFailure(err)
	}
	err = tx.Commit()
	if errors.Is(err, ErrCommitUnknown) {
		_ = lease.Close()
		committed, resolveErr := tx.ResolveCommit(ctx)
		if resolveErr != nil {
			return resolveErr
		}
		if committed {
			return nil
		}
		return errors.New("storage_restore_rolled_back")
	}
	return safeFailure(err)
}
