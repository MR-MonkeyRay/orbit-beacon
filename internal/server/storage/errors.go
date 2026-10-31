package storage

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// IsBusy 识别 SQLite 锁争用的主错误码和扩展错误码，支持包装后的错误。
// 数据损坏、I/O 失败和约束冲突不据此重试，也不按错误文本判定。
func IsBusy(err error) bool {
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
	if !errors.As(err, &failure) {
		return 0, false
	}
	return failure.Code(), true
}
