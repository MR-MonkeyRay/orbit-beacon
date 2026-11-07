package storage

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"github.com/go-sql-driver/mysql"
	"io"
	"net"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// IsBusy 只依据双引擎的数值错误码识别锁争用。
// 数据损坏、I/O 失败和约束冲突不据此重试，也不按错误文本判定。
func IsBusy(err error) bool {
	if code, ok := MySQLCode(err); ok {
		return code == 1205 || code == 1213 || code == 3572
	}
	code, ok := SQLiteCode(err)
	if !ok {
		return false
	}
	code &= 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}

// SQLiteCode 提取被包装驱动的数值结果码，不暴露错误文本、SQL、路径或绑定值。
func SQLiteCode(err error) (int, bool) {
	var failure *sqlite.Error
	if !errors.As(err, &failure) || failure == nil {
		return 0, false
	}
	return failure.Code(), true
}

func MySQLCode(err error) (uint16, bool) {
	var failure *mysql.MySQLError
	if !errors.As(err, &failure) || failure == nil {
		return 0, false
	}
	return failure.Number, true
}
func IsUnique(err error) bool {
	if code, ok := MySQLCode(err); ok {
		return code == 1062
	}
	code, ok := SQLiteCode(err)
	return ok && (code == sqlite3.SQLITE_CONSTRAINT_UNIQUE || code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
}
func IsConnectionFailure(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, mysql.ErrInvalidConn) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var network net.Error
	if errors.As(err, &network) {
		return true
	}
	code, ok := MySQLCode(err)
	return ok && (code == 1290 || code == 2006 || code == 2013 || code == 1053)
}

type safeError struct{ cause error }

func (e safeError) Error() string {
	if code, ok := MySQLCode(e.cause); ok {
		return fmt.Sprintf("storage_database_error mysql_code=%d", code)
	}
	return "storage_database_error"
}
func (e safeError) Unwrap() error { return e.cause }
func safeFailure(err error) error {
	if err == nil {
		return nil
	}
	var startup *mysqlStartupError
	if errors.As(err, &startup) && startup != nil {
		return startup
	}
	if _, ok := err.(safeError); ok {
		return err
	}
	if errors.Is(err, ErrSchemaIncompatible) || errors.Is(err, ErrSchemaTooNew) || errors.Is(err, ErrDeployment) || errors.Is(err, ErrCommitUnknown) || errors.Is(err, ErrUnavailable) {
		return err
	}
	return safeError{err}
}

// 初始化诊断只使用固定域、阶段和编译期 DDL 序号，不输出底层错误文本。
type mysqlStartupError struct {
	domain  Domain
	phase   string
	ordinal int
	cause   error
}

func (e *mysqlStartupError) Error() string {
	message := fmt.Sprintf("storage_database_error domain=%s phase=%s ddl=%d", e.domain, e.phase, e.ordinal)
	if code, ok := MySQLCode(e.cause); ok {
		message += fmt.Sprintf(" mysql_code=%d", code)
	}
	return message
}
func (e *mysqlStartupError) Unwrap() error { return e.cause }
func mysqlStartupFailure(domain Domain, phase string, ordinal int, err error) error {
	if err == nil {
		return nil
	}
	var existing *mysqlStartupError
	if errors.As(err, &existing) && existing != nil {
		return existing
	}
	return &mysqlStartupError{domain: domain, phase: phase, ordinal: ordinal, cause: err}
}
