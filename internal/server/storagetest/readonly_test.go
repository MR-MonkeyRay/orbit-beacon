package storagetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"orbitbeacon/internal/server/storage"
)

func TestScopedDatabaseReadOnlyFaultPreservesOtherDomain(t *testing.T) {
	f := Start(t)
	main := f.Open(t, storage.MainDomain)
	_ = f.Open(t, storage.HistoryDomain)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	name := f.Endpoint("primary").MainDatabase
	if _, err := f.admin.ExecContext(ctx, "ALTER DATABASE `"+name+"` READ ONLY = 1"); err != nil {
		var code *mysql.MySQLError
		if errors.As(err, &code) {
			t.Fatalf("scoped read-only unsupported mysql_code=%d", code.Number)
		}
		t.Fatalf("scoped read-only failed type=%T", err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := f.admin.ExecContext(cleanup, "ALTER DATABASE `"+name+"` READ ONLY = 0"); err != nil {
			t.Error("failed to restore only owned schema")
		}
	}()
	lease, err := main.Acquire(ctx, storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err == nil {
		_, err = tx.ExecContext(lease.Context(), "INSERT INTO settings VALUES('read-only-fault','never')")
		tx.Rollback()
	}
	if err == nil {
		t.Fatal("read-only schema remained writable")
	}
	var native *mysql.MySQLError
	if !errors.As(err, &native) {
		t.Fatalf("read-only did not expose numeric code type=%T", err)
	}
	t.Logf("owned read-only schema native mysql_code=%d classified_connection_failure=%v", native.Number, storage.IsConnectionFailure(err))
	var writable bool
	if err = f.RawEndpoint(t, "primary", storage.HistoryDomain).QueryRowContext(ctx, "SELECT @@transaction_read_only=0").Scan(&writable); err != nil || !writable {
		t.Fatal("scoped fault changed healthy other domain", err)
	}
}
