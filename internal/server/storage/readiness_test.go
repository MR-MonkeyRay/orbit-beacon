package storage

import (
	"context"
	"database/sql"
	"github.com/go-sql-driver/mysql"
	"testing"
)

func TestMySQLReadinessRequiresSuccessfulWriteAfterFailure(t *testing.T) {
	connector := &receiptConnector{commit: true}
	pool := sql.OpenDB(connector)
	db := newDatabase(MainDomain, pool, pool)
	db.current.dialect = MySQL
	defer db.Close()
	l, err := db.Acquire(context.Background(), Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_ = l.failure(&mysql.MySQLError{Number: 1290, Message: "read only secret"})
	if db.Ready() {
		t.Fatal("read-only failure still ready")
	}
	read, err := l.BeginRead(l.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = read.Commit(); err != nil {
		t.Fatal(err)
	}
	if db.Ready() {
		t.Fatal("read probe restored write readiness")
	}
	tx, err := l.BeginWrite(l.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !db.Ready() {
		t.Fatal("successful guarded write did not restore readiness")
	}
}
