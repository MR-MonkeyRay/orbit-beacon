package logging_test

import (
	"database/sql"
	"errors"
	"log/slog"
	"testing"

	"modernc.org/sqlite"

	"orbitbeacon/internal/logging"
)

type sqliteLookalike struct{}

func (sqliteLookalike) Error() string { return "private SQL values" }
func (sqliteLookalike) Code() int     { return 2067 }

func TestErrorAttrsExposeOnlySafeMetadata(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec("SELECT missing_column")
	if err == nil {
		t.Fatal("expected sqlite failure")
	}

	attrs := logging.ErrorAttrs(errors.Join(errors.New("private value"), err), "storage_error")
	values := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		values[attr.Key] = attr.Value.Any()
	}
	if values["error_kind"] != "sqlite" || values["error_code"] != "storage_error" {
		t.Fatalf("attrs=%v", values)
	}
	if code, ok := values["sqlite_code"].(int64); !ok || code == 0 {
		t.Fatalf("sqlite_code=%v", values["sqlite_code"])
	}
	for key, value := range values {
		if key == "error" || value == err.Error() || value == "private value" {
			t.Fatalf("unsafe attribute %q=%v", key, value)
		}
	}
}

func TestErrorAttrsDoNotTrustSQLiteLookalikes(t *testing.T) {
	attrs := logging.ErrorAttrs(sqliteLookalike{}, "")
	if len(attrs) != 1 || attrs[0].Key != "error_kind" || attrs[0].Value.Kind() != slog.KindString {
		t.Fatalf("attrs=%v", attrs)
	}
	if got := attrs[0].Value.String(); got == "sqlite" || got == (sqliteLookalike{}).Error() {
		t.Fatalf("unsafe error kind %q", got)
	}
}

func TestErrorAttrsHandleTypedNilSQLiteError(t *testing.T) {
	var sqliteError *sqlite.Error
	var err error = sqliteError
	attrs := logging.ErrorAttrs(err, "storage_error")
	if len(attrs) != 2 || attrs[0].Key != "error_kind" || attrs[1].Key != "error_code" {
		t.Fatalf("attrs=%v", attrs)
	}
}
