// Package logging provides diagnostic attributes without disclosing error text.
package logging

import (
	"context"
	"errors"
	"log/slog"
	"reflect"

	"modernc.org/sqlite"
)

// ErrorAttrs describes a failure using only its class and a caller-owned code.
// Error messages may contain SQL, credentials or paths and are never inspected.
func ErrorAttrs(err error, code string) []slog.Attr {
	kind := "unknown"
	if err != nil {
		kind = reflect.TypeOf(err).String()
	}
	switch {
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		kind = "deadline_exceeded"
	}
	attrs := []slog.Attr{slog.String("error_kind", kind)}
	if code != "" {
		attrs = append(attrs, slog.String("error_code", code))
	}
	var sqliteError *sqlite.Error
	if errors.As(err, &sqliteError) && sqliteError != nil {
		attrs[0] = slog.String("error_kind", "sqlite")
		attrs = append(attrs, slog.Int("sqlite_code", sqliteError.Code()))
	}
	return attrs
}
