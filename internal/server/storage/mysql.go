package storage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
	"orbitbeacon/internal/config"
	"orbitbeacon/internal/logging"
	"orbitbeacon/internal/server/securefs"
	"orbitbeacon/internal/timekey"
)

var ErrDeployment = errors.New("storage_mysql_deployment_invalid: require writable MySQL, transactional InnoDB, durable commits and binlog trigger prerequisites")
var ErrUnsupportedTopology = errors.New("storage_replication_not_available")

// OpenDatabases 在任一 MySQL 域失败时关闭全部连接；SQLite 保留可用 main。
func OpenDatabases(ctx context.Context, cfg config.DatabaseConfig, dataDir string) (*Databases, error) {
	return openConfigured(ctx, cfg, dataDir, false, "")
}

// OpenDomain 只打开明确选定的逻辑域，不要求另一个域可用，也不猜测 HA 权威。
func OpenDomain(ctx context.Context, endpoint config.DatabaseEndpoint, domain Domain, dataDir string) (*Database, error) {
	if domain != MainDomain && domain != HistoryDomain {
		return nil, ErrUnavailable
	}
	switch endpoint.Driver {
	case "", "sqlite":
		if domain == MainDomain {
			return OpenMain(ctx, filepath.Join(dataDir, "orbitbeacon.db"))
		}
		return OpenHistory(ctx, filepath.Join(dataDir, "metrics.db"))
	case "mysql":
		return openMySQL(ctx, endpoint, domain)
	default:
		return nil, ErrUnavailable
	}
}
func (dbs *Databases) Close() error {
	if dbs == nil {
		return nil
	}
	if err := dbs.StopReplication(); err != nil {
		return err
	}
	var a, b error
	if dbs.Main != nil {
		a = dbs.Main.Close()
	}
	if dbs.History != nil {
		b = dbs.History.Close()
	}
	var replicaErr error
	if dbs.replication != nil {
		for _, d := range dbs.replication.domains {
			if d.target != nil {
				replicaErr = errors.Join(replicaErr, d.target.Close())
			}
		}
		replicaErr = errors.Join(replicaErr, dbs.replication.control.Close())
	}
	return errors.Join(a, b, replicaErr)
}
func (db *Database) Domain() Domain { return db.domain }
func (db *Database) Dialect() (Dialect, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed || db.current == nil {
		return SQLite, false
	}
	return db.current.dialect, true
}

func mysqlConfig(endpoint config.DatabaseEndpoint, domain Domain) (*mysql.Config, error) {
	file, err := securefs.OpenFile(endpoint.PasswordFile, os.O_RDONLY, 0)
	if err != nil {
		return nil, safeFailure(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, safeFailure(err)
	}
	if info.Mode().Perm() != 0600 {
		return nil, errors.New("storage_password_file_requires_0600")
	}
	password, err := io.ReadAll(io.LimitReader(file, 65537))
	defer func() { clear(password) }()
	if err != nil {
		return nil, safeFailure(err)
	}
	if len(password) > 65536 {
		return nil, errors.New("storage_password_file_too_large")
	}
	if len(password) > 0 && password[len(password)-1] == '\n' {
		password = password[:len(password)-1]
	}
	if len(password) == 0 {
		return nil, errors.New("storage_password_file_empty")
	}
	cfg := mysql.NewConfig()
	cfg.User = endpoint.User
	cfg.Passwd = string(password)
	cfg.Net = endpoint.Network
	if cfg.Net == "" {
		cfg.Net = "tcp"
	}
	cfg.Addr = endpoint.Address
	cfg.DBName = endpoint.MainDatabase
	if domain == HistoryDomain {
		cfg.DBName = endpoint.HistoryDatabase
	}
	if cfg.DBName == "" {
		cfg.DBName = "orbitbeacon"
		if domain == HistoryDomain {
			cfg.DBName = "orbitbeacon_history"
		}
	}
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	cfg.ClientFoundRows = true
	cfg.InterpolateParams = true
	cfg.MultiStatements = false
	cfg.Logger = logging.MySQLLogger{}
	// 0900_bin 的 ID 超出握手单字节范围；先 SET NAMES，再固定会话排序规则。
	if err = cfg.Apply(mysql.Charset("utf8mb4", "")); err != nil {
		return nil, safeFailure(err)
	}
	cfg.Params = map[string]string{"sql_mode": "'STRICT_TRANS_TABLES,ONLY_FULL_GROUP_BY,NO_ZERO_DATE,NO_ZERO_IN_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'", "foreign_key_checks": "1", "collation_connection": "'utf8mb4_0900_bin'"}
	if cfg.Net == "tcp" {
		host, port, err := net.SplitHostPort(endpoint.Address)
		number, parseErr := strconv.ParseUint(port, 10, 16)
		if err != nil || host == "" || parseErr != nil || number == 0 {
			return nil, errors.New("storage_mysql_address_invalid")
		}
		if endpoint.TLS != nil {
			cfg.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
			if endpoint.TLS.ServerName != "" {
				cfg.TLS.ServerName = endpoint.TLS.ServerName
			}
			if endpoint.TLS.CAFile != "" {
				pem, err := os.ReadFile(endpoint.TLS.CAFile)
				if err != nil {
					return nil, safeFailure(err)
				}
				roots := x509.NewCertPool()
				if !roots.AppendCertsFromPEM(pem) {
					return nil, errors.New("storage_mysql_ca_invalid")
				}
				cfg.TLS.RootCAs = roots
			}
		}
	} else if cfg.Net != "unix" || !filepath.IsAbs(cfg.Addr) || endpoint.TLS != nil {
		return nil, errors.New("storage_mysql_network_invalid")
	}
	if cfg.User == "" {
		return nil, errors.New("storage_mysql_user_required")
	}
	return cfg, nil
}
func openMySQL(ctx context.Context, endpoint config.DatabaseEndpoint, domain Domain) (*Database, error) {
	cfg, err := mysqlConfig(endpoint, domain)
	if err != nil {
		return nil, mysqlStartupFailure(domain, "configuration", 0, err)
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, mysqlStartupFailure(domain, "connector", 0, err)
	}
	writer := sql.OpenDB(connector)
	writer.SetMaxOpenConns(3)
	writer.SetMaxIdleConns(3)
	fail := func(err error) (*Database, error) { _ = writer.Close(); return nil, safeFailure(err) }
	if err = writer.PingContext(ctx); err != nil {
		return fail(mysqlStartupFailure(domain, "writer_connect", 0, err))
	}
	if err = checkMySQLDeployment(ctx, writer, domain); err != nil {
		return fail(err)
	}
	if err = initializeMySQLSchema(ctx, writer, domain); err != nil {
		return fail(mysqlStartupFailure(domain, "schema", 0, err))
	}
	reader := sql.OpenDB(connector)
	reader.SetMaxOpenConns(4)
	reader.SetMaxIdleConns(4)
	if err = reader.PingContext(ctx); err != nil {
		_ = reader.Close()
		return fail(mysqlStartupFailure(domain, "reader_connect", 0, err))
	}
	db := newDatabase(domain, writer, reader)
	db.current.dialect = MySQL
	return db, nil
}

func checkMySQLDeployment(ctx context.Context, db *sql.DB, domain Domain) error {
	var durable, binlog, syncBinlog, trust, readOnly int
	var support, transactions string
	if err := db.QueryRowContext(ctx, "SELECT @@innodb_flush_log_at_trx_commit,@@log_bin,@@sync_binlog,@@log_bin_trust_function_creators,@@read_only,SUPPORT,TRANSACTIONS FROM information_schema.ENGINES WHERE ENGINE='InnoDB'").Scan(&durable, &binlog, &syncBinlog, &trust, &readOnly, &support, &transactions); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDeployment
		}
		return mysqlStartupFailure(domain, "deployment", 0, err)
	}
	if (support != "DEFAULT" && support != "YES") || transactions != "YES" || durable != 1 || (binlog != 0 && (syncBinlog != 1 || trust != 1)) {
		return ErrDeployment
	}
	if readOnly != 0 {
		return mysqlStartupFailure(domain, "read_only", 0, errMySQLReadOnly)
	}
	return nil
}
func initializeMySQLSchema(ctx context.Context, db *sql.DB, domain Domain) error {
	var tables int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables); err != nil {
		return err
	}
	expected := CurrentSchemaVersion
	ddl := mysqlMainSchema
	if domain == HistoryDomain {
		expected = HistorySchemaVersion
		ddl = mysqlHistorySchema
	}
	if tables == 0 {
		for ordinal, statement := range ddl {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				return mysqlStartupFailure(domain, "ddl", ordinal+1, err)
			}
		}
		if _, err := db.ExecContext(ctx, mysqlGuardSchema); err != nil {
			return mysqlStartupFailure(domain, "ddl", len(ddl)+1, err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO storage_write_guard(id,last_tx_uuid,restore_operation) VALUES(1,'','')"); err != nil {
			return mysqlStartupFailure(domain, "ddl", len(ddl)+2, err)
		}
		if _, err := db.ExecContext(ctx, "CREATE TABLE schema_migrations(version BIGINT PRIMARY KEY,applied_at VARCHAR(40) NOT NULL) ENGINE=InnoDB"); err != nil {
			return mysqlStartupFailure(domain, "ddl", len(ddl)+3, err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO schema_migrations VALUES(?,?)", expected, timekey.Format(time.Now())); err != nil {
			return mysqlStartupFailure(domain, "ddl", len(ddl)+4, err)
		}
	}
	var count int
	var version sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT count(*),max(version) FROM schema_migrations").Scan(&count, &version); err != nil {
		return ErrSchemaIncompatible
	}
	if version.Valid && version.Int64 > int64(expected) {
		return ErrSchemaTooNew
	}
	if count != 1 || !version.Valid || version.Int64 != int64(expected) {
		return ErrSchemaIncompatible
	}
	// 标记是最后一步；非空半成品或手工删表不能作为空库重建。
	rows, err := db.QueryContext(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema=DATABASE() AND engine='InnoDB'")
	if err != nil {
		return err
	}
	found := make(map[string]bool)
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		found[name] = true
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, table := range Tables(domain) {
		if !found[table.Name] {
			return ErrSchemaIncompatible
		}
	}
	var guard int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM storage_write_guard WHERE id=1").Scan(&guard); err != nil || guard != 1 {
		return ErrSchemaIncompatible
	}
	return nil
}
