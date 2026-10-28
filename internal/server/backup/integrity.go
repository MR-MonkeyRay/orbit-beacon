package backup

import (
	"context"
	"database/sql"
	"errors"
)

// ValidateSQLite checks both physical integrity and enabled foreign-key
// relationships; it never migrates or checkpoints the supplied database.
func ValidateSQLite(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return &Error{Code: "restore_invalid_data", Err: err}
	}
	count := 0
	for rows.Next() {
		var result string
		if err = rows.Scan(&result); err != nil {
			rows.Close()
			return &Error{Code: "restore_invalid_data", Err: err}
		}
		if result != "ok" {
			rows.Close()
			return &Error{Code: "restore_invalid_data"}
		}
		count++
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return &Error{Code: "restore_invalid_data", Err: err}
	}
	if count != 1 {
		return &Error{Code: "restore_invalid_data"}
	}
	rows, err = db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return &Error{Code: "restore_dependency_conflict", Err: err}
	}
	conflict := rows.Next()
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return &Error{Code: "restore_dependency_conflict", Err: err}
	}
	if conflict {
		return &Error{Code: "restore_dependency_conflict"}
	}
	return nil
}
