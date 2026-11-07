package logging_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"orbitbeacon/internal/logging"
)

func TestMySQLErrorAttrsAreNumericAndNilSafe(t *testing.T) {
	failure := &mysql.MySQLError{Number: 1062, Message: "password SQL bound secret"}
	attrs := logging.ErrorAttrs(errors.Join(errors.New("private"), failure), "storage_error")
	values := map[string]any{}
	for _, attr := range attrs {
		values[attr.Key] = attr.Value.Any()
	}
	if values["error_kind"] != "mysql" || values["mysql_code"] != int64(1062) || values["error_code"] != "storage_error" {
		t.Fatalf("attrs=%v", values)
	}
	var nilFailure *mysql.MySQLError
	if attrs = logging.ErrorAttrs(nilFailure, ""); len(attrs) != 1 {
		t.Fatalf("typed nil attrs=%v", attrs)
	}
	var output bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(old)
	logging.MySQLLogger{}.Print("private DSN password", failure)
	text := output.String()
	if !strings.Contains(text, "1062") || strings.Contains(text, "password") || strings.Contains(text, "bound secret") || strings.Contains(text, "private") {
		t.Fatalf("unsafe driver log %q", text)
	}
}
