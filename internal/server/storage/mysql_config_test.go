package storage

import (
	"context"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orbitbeacon/internal/config"
)

func TestMySQLConfigSecretAndTransport(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	secret := filepath.Join(dir, "password")
	endpoint := config.DatabaseEndpoint{Driver: "mysql", Network: "tcp", Address: "localhost:3306", User: "test", PasswordFile: secret, MainDatabase: "orbitbeacon", HistoryDatabase: "orbitbeacon_history"}
	for _, content := range []string{"", "\n", strings.Repeat("x", 65537)} {
		if err := os.WriteFile(secret, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := mysqlConfig(endpoint, MainDomain); err == nil {
			t.Fatal("invalid secret accepted")
		}
	}
	if err := os.WriteFile(secret, []byte(" space\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := mysqlConfig(endpoint, MainDomain)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Passwd != " space\r" || cfg.Timeout != 5*time.Second || cfg.ReadTimeout != 5*time.Second || cfg.WriteTimeout != 5*time.Second || !cfg.ClientFoundRows || !cfg.InterpolateParams || cfg.MultiStatements || cfg.TLS != nil || cfg.TLSConfig != "" || cfg.AllowFallbackToPlaintext {
		t.Fatal("unsafe MySQL configuration")
	}
	if cfg.Collation != "" || cfg.Params["collation_connection"] != "'utf8mb4_0900_bin'" {
		t.Fatal("high-ID collation must be set after the UTF-8 handshake")
	}
	if err = os.Chmod(secret, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err = mysqlConfig(endpoint, MainDomain); err == nil {
		t.Fatal("non-0600 secret accepted")
	}
	_ = os.Chmod(secret, 0600)
	endpoint.Network = "unix"
	endpoint.Address = filepath.Join(dir, "mysql.sock")
	cfg, err = mysqlConfig(endpoint, HistoryDomain)
	if err != nil || cfg.TLS != nil || cfg.DBName != "orbitbeacon_history" {
		t.Fatalf("unix config: %v", err)
	}
	endpoint.TLS = &config.DatabaseTLS{}
	if _, err = mysqlConfig(endpoint, MainDomain); err == nil {
		t.Fatal("TLS on unix accepted")
	}
}

func TestMySQLConfigExplicitTLSAndPlaintextAddressValidation(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "password")
	if err := os.WriteFile(secret, []byte("test-password"), 0600); err != nil {
		t.Fatal(err)
	}
	endpoint := config.DatabaseEndpoint{Driver: "mysql", Address: "localhost:3306", User: "test", PasswordFile: secret, TLS: &config.DatabaseTLS{}}
	cfg, err := mysqlConfig(endpoint, MainDomain)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLS == nil || cfg.TLS.MinVersion != tls.VersionTLS12 || cfg.TLS.ServerName != "localhost" || cfg.TLS.RootCAs != nil || cfg.TLS.InsecureSkipVerify || cfg.AllowFallbackToPlaintext {
		t.Fatal("explicit TLS must verify hostname with system roots and forbid plaintext fallback")
	}
	endpoint.TLS.ServerName = "database.test"
	cfg, err = mysqlConfig(endpoint, MainDomain)
	if err != nil || cfg.TLS == nil || cfg.TLS.ServerName != "database.test" || cfg.TLS.InsecureSkipVerify || cfg.AllowFallbackToPlaintext {
		t.Fatal("explicit TLS server name was not preserved")
	}
	ca := filepath.Join(dir, "ca.pem")
	endpoint.TLS.CAFile = ca
	if cfg, err = mysqlConfig(endpoint, MainDomain); err == nil || cfg != nil {
		t.Fatal("unreadable explicit CA did not fail closed")
	}
	if err = os.WriteFile(ca, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = mysqlConfig(endpoint, MainDomain); err == nil || cfg != nil {
		t.Fatal("invalid explicit CA did not fail closed")
	}
	endpoint.TLS = nil
	for _, address := range []string{"localhost", ":3306", "localhost:0", "localhost:not-a-port", "localhost:65536"} {
		endpoint.Address = address
		if _, err = mysqlConfig(endpoint, MainDomain); err == nil {
			t.Fatalf("invalid plaintext TCP address accepted: %s", address)
		}
	}
}

func TestMySQLDeploymentRejectsUnsafeCapabilities(t *testing.T) {
	for _, test := range []struct {
		name                                         string
		durable, binlog, syncBinlog, trust, readOnly int64
		support, transactions                        string
		valid                                        bool
	}{
		{"durable writable", 1, 1, 1, 1, 0, "DEFAULT", "YES", true},
		{"InnoDB available", 1, 1, 1, 1, 0, "YES", "YES", true},
		{"binlog disabled", 1, 0, 0, 0, 0, "DEFAULT", "YES", true},
		{"unsafe durability", 2, 1, 1, 1, 0, "DEFAULT", "YES", false},
		{"unsafe binlog", 1, 1, 0, 1, 0, "DEFAULT", "YES", false},
		{"missing trigger prerequisite", 1, 1, 1, 0, 0, "DEFAULT", "YES", false},
		{"read only", 1, 1, 1, 1, 1, "DEFAULT", "YES", false},
		{"InnoDB unavailable", 1, 1, 1, 1, 0, "NO", "NO", false},
		{"InnoDB without transactions", 1, 1, 1, 1, 0, "YES", "NO", false},
		{"InnoDB disabled", 1, 1, 1, 1, 0, "DISABLED", "YES", false},
		{"InnoDB missing", 1, 1, 1, 1, 0, "", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			connector := &deploymentConnector{variables: []driver.Value{test.durable, test.binlog, test.syncBinlog, test.trust, test.readOnly}, engine: []driver.Value{test.support, test.transactions}}
			db := sql.OpenDB(connector)
			defer db.Close()
			err := checkMySQLDeployment(t.Context(), db, MainDomain)
			if test.valid && err != nil {
				t.Fatalf("valid capabilities rejected: %v", err)
			}
			if !test.valid && test.readOnly != 0 {
				if !storageFailure(err) || errors.Is(err, ErrDeployment) {
					t.Fatalf("read-only endpoint lost its recoverable classification: %v", err)
				}
			} else if !test.valid && !errors.Is(err, ErrDeployment) {
				t.Fatalf("unsafe deployment accepted: %v", err)
			}
		})
	}
}

type deploymentConnector struct {
	variables []driver.Value
	engine    []driver.Value
}

func (c *deploymentConnector) Driver() driver.Driver { return rollbackDriver{} }
func (c *deploymentConnector) Connect(context.Context) (driver.Conn, error) {
	return &deploymentConn{c}, nil
}

type deploymentConn struct{ c *deploymentConnector }

func (c *deploymentConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not used") }
func (c *deploymentConn) Close() error                        { return nil }
func (c *deploymentConn) Begin() (driver.Tx, error)           { return nil, errors.New("not used") }
func (c *deploymentConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	values := c.c.variables
	if strings.Contains(query, "information_schema.ENGINES") {
		if c.c.engine[0] == "" {
			return &deploymentRows{done: true}, nil
		}
		values = append(values, c.c.engine...)
	}
	return &deploymentRows{values: values}, nil
}

type deploymentRows struct {
	values []driver.Value
	done   bool
}

func (r *deploymentRows) Columns() []string { return make([]string, len(r.values)) }
func (r *deploymentRows) Close() error      { return nil }
func (r *deploymentRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, r.values)
	return nil
}

func TestOpenDatabasesPreservesSQLiteMainWhenHistoryFails(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	if err := os.WriteFile(filepath.Join(dir, "metrics.db"), []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	dbs, err := OpenDatabases(context.Background(), config.DatabaseConfig{}, dir)
	if err == nil || dbs == nil || dbs.Main == nil || dbs.History != nil {
		t.Fatalf("databases=%v err=%v", dbs, err)
	}
	defer dbs.Close()
	if !dbs.Main.Ready() {
		t.Fatal("history failure disabled main")
	}
}

func TestTypedValuesPreserveEmptyBlobAndRejectNonfinite(t *testing.T) {
	for _, value := range []any{[]byte{}, ""} {
		got, err := typedValue(value, Blob)
		if err != nil || got == nil {
			t.Fatal("empty blob became NULL")
		}
	}
	for _, value := range []any{[]byte("NaN"), []byte("+Inf")} {
		if _, err := typedValue(value, Float); err == nil {
			t.Fatal("nonfinite accepted")
		}
	}
	got, err := typedValue([]byte("18446744073709551615"), Text)
	if err != nil || got != "18446744073709551615" {
		t.Fatal("uint64 text changed")
	}
}

func TestDeleteBatchClosesRowsBeforeDelete(t *testing.T) {
	db := leaseDatabase(t)
	ctx := context.Background()
	l, err := db.Acquire(ctx, Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	tx, err := l.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('a','1'),('b','2'),('c','3')"); err != nil {
		t.Fatal(err)
	}
	count, err := DeleteBatch(ctx, tx, "SELECT key FROM settings ORDER BY key LIMIT ?", "DELETE FROM settings WHERE key=?", 2)
	if err != nil || count != 2 {
		t.Fatalf("deleted=%d err=%v", count, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenDomainMainDoesNotRequireHistory(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	history := filepath.Join(dir, "metrics.db")
	if err := os.WriteFile(history, []byte("corrupt history sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDomain(context.Background(), config.DatabaseEndpoint{}, MainDomain, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if !db.Ready() {
		t.Fatal("main unavailable")
	}
	body, err := os.ReadFile(history)
	if err != nil || string(body) != "corrupt history sentinel" {
		t.Fatalf("unrelated history touched: %q %v", body, err)
	}
}
