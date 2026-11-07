// Package logging 提供诊断属性，不泄露错误文本。
package logging

import (
	"context"
	"errors"
	"log/slog"
	"reflect"

	"modernc.org/sqlite"
)

// ErrorAttrs 仅用错误类别和调用方提供的错误码描述失败。
// 错误消息可能包含 SQL、凭据或路径，一律不检查。
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
