package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"time"

	"orbitbeacon/internal/server/securefs"
)

// SnapshotSQLite 仅复制租约固定的活动库，不从配置路径重新选择源。
func (l *Lease) SnapshotSQLite(ctx context.Context, output string, version int) (err error) {
	if l.Dialect() != SQLite {
		return ErrUnavailable
	}
	release, err := l.retain()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := l.merge(ctx)
	defer cancel()
	if err := securefs.ValidateDirectory(filepath.Dir(output)); err != nil {
		return err
	}
	file, err := securefs.OpenFile(output, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	conn, err := l.gen.reader.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&got); err != nil {
		return err
	}
	if !got.Valid || got.Int64 != int64(version) {
		return ErrSchemaIncompatible
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only=OFF"); err != nil {
		return err
	}
	defer func() {
		reset, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		if _, resetErr := conn.ExecContext(reset, "PRAGMA query_only=ON"); resetErr != nil {
			// 无活动 Tx，可直接丢弃无法恢复只读状态的连接。
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, resetErr)
		}
	}()
	if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", output); err != nil {
		return err
	}
	file, err = securefs.OpenFile(output, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}
