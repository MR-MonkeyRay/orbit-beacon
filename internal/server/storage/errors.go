package storage

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// IsBusy recognizes primary and extended contention codes, including wrapped
// errors. Corruption, I/O failures, constraints and error text are not retries.
func IsBusy(err error) bool {
	code, ok := SQLiteCode(err)
	if !ok {
		return false
	}
	code &= 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}

// SQLiteCode extracts a wrapped driver's numeric result code without exposing
// error text, SQL, paths or bound values.
func SQLiteCode(err error) (int, bool) {
	var failure *sqlite.Error
	if !errors.As(err, &failure) {
		return 0, false
	}
	return failure.Code(), true
}
