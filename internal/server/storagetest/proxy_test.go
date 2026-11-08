package storagetest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"orbitbeacon/internal/server/storage"
)

func TestProxyEndpointInterruptsOnlyOwnedConnectionsAndPreservesTLS(t *testing.T) {
	f := Start(t)
	endpoint := f.Endpoint("primary")
	proxy, through := ProxyEndpoint(t, endpoint)
	cfg, err := fixtureMySQLConfig(through, endpoint.MainDatabase)
	if err != nil {
		t.Fatal(err)
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "CREATE TABLE proxy_marker(value BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, "INSERT INTO proxy_marker VALUES(73)"); err != nil {
		t.Fatal(err)
	}
	var name, version string
	if err = conn.QueryRowContext(ctx, "SHOW SESSION STATUS LIKE 'Ssl_version'").Scan(&name, &version); err != nil {
		t.Fatal(err)
	}
	if endpoint.TLS != nil && version == "" {
		t.Fatal("TLS proxy connection downgraded")
	}
	proxy.Pause()
	fault, faultCancel := context.WithTimeout(context.Background(), time.Second)
	defer faultCancel()
	var value int
	if err = conn.QueryRowContext(fault, "SELECT value FROM proxy_marker").Scan(&value); err == nil {
		t.Fatal("paused existing link remained readable")
	}
	fresh, err := db.Conn(fault)
	if err == nil {
		err = fresh.PingContext(fault)
		fresh.Close()
	}
	if err == nil {
		t.Fatal("paused endpoint accepted new connection")
	}
	if err = f.RawEndpoint(t, "primary", storage.MainDomain).QueryRowContext(ctx, "SELECT value FROM proxy_marker").Scan(&value); err != nil || value != 73 {
		t.Fatal("proxy failure affected remote direct connection", value, err)
	}
	proxy.Resume()
	retry, retryCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer retryCancel()
	if err = db.QueryRowContext(retry, "SELECT value FROM proxy_marker").Scan(&value); err != nil || value != 73 {
		t.Fatal("resume lost original remote state", value, err)
	}
}
