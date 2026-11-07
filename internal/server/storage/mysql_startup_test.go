package storage

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestMySQLStartupDiagnosticsExposeOnlyTrustedMetadata(t *testing.T) {
	failure := &mysql.MySQLError{Number: 3823, Message: "private SQL binding password DSN"}
	err := safeFailure(mysqlStartupFailure(MainDomain, "ddl", 17, failure))
	message := err.Error()
	for _, want := range []string{"domain=main", "phase=ddl", "ddl=17", "mysql_code=3823"} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing safe metadata %q: %s", want, message)
		}
	}
	for _, secret := range []string{"private", "SQL", "binding", "password", "DSN"} {
		if strings.Contains(message, secret) {
			t.Fatalf("unsafe startup diagnostic: %s", message)
		}
	}
	if code, ok := MySQLCode(err); !ok || code != 3823 {
		t.Fatalf("numeric classification lost: %d %v", code, ok)
	}
	if !errors.Is(mysqlStartupFailure(HistoryDomain, "schema", 0, ErrSchemaIncompatible), ErrSchemaIncompatible) {
		t.Fatal("schema classification lost")
	}
	if got := safeFailure(failure).Error(); got != "storage_database_error mysql_code=3823" {
		t.Fatalf("numeric diagnostic lost: %s", got)
	}
}
