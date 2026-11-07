// Package storagetest 提供显式启用、仅使用远程独立 schema 的 MySQL 行为夹具。
package storagetest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"orbitbeacon/internal/config"
	"orbitbeacon/internal/logging"
	"orbitbeacon/internal/server/securefs"
	"orbitbeacon/internal/server/storage"
)

type Fixture struct {
	runtime         string
	cfg             config.DatabaseConfig
	dbs             *storage.Databases
	raw             map[storage.Domain]*sql.DB
	admin           *sql.DB
	created         []string
	applicationUser string
}

func fixtureProfile(path string) (config.DatabaseEndpoint, error) {
	file, err := securefs.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return config.DatabaseEndpoint{}, errors.New("mysql_fixture_profile_unreadable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Mode().Perm() != 0600 {
		return config.DatabaseEndpoint{}, errors.New("mysql_fixture_profile_requires_0600")
	}
	server, err := config.LoadServer(config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		return config.DatabaseEndpoint{}, errors.New("mysql_fixture_defaults_invalid")
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&server); err != nil {
		return config.DatabaseEndpoint{}, errors.New("mysql_fixture_profile_invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return config.DatabaseEndpoint{}, errors.New("mysql_fixture_profile_invalid")
	}
	if err := server.Validate(); err != nil || server.Database.Primary.Driver != "mysql" {
		return config.DatabaseEndpoint{}, errors.New("mysql_fixture_profile_requires_mysql_primary")
	}
	return server.Database.Primary, nil
}

func fixtureDatabases(endpoint config.DatabaseEndpoint) config.DatabaseConfig {
	prefix := "orbitbeacon_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	primary, standby := endpoint, endpoint
	primary.MainDatabase, primary.HistoryDatabase = prefix+"_pm", prefix+"_ph"
	standby.MainDatabase, standby.HistoryDatabase = prefix+"_sm", prefix+"_sh"
	return config.DatabaseConfig{Primary: primary, Standby: &standby}
}

func Start(t testing.TB) *Fixture {
	t.Helper()
	if os.Getenv("ORBITBEACON_TEST_MYSQL") != "1" {
		t.Skip("set ORBITBEACON_TEST_MYSQL=1 and ORBITBEACON_TEST_MYSQL_CONFIG to run remote MySQL integration")
	}
	path := os.Getenv("ORBITBEACON_TEST_MYSQL_CONFIG")
	if path == "" {
		t.Fatal("explicit MySQL integration requires ORBITBEACON_TEST_MYSQL_CONFIG")
	}
	endpoint, err := fixtureProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := fixtureMySQLConfig(endpoint, "")
	if err != nil {
		t.Fatal(fixtureFailure("admin_configuration", err))
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal("mysql_fixture_connector_invalid")
	}
	f := &Fixture{runtime: t.TempDir(), cfg: fixtureDatabases(endpoint), raw: map[storage.Domain]*sql.DB{}, admin: sql.OpenDB(connector)}
	t.Cleanup(func() {
		if f.dbs != nil {
			_ = f.dbs.Close()
		}
		for _, db := range f.raw {
			_ = db.Close()
		}
		defer f.admin.Close()
		for _, name := range f.created {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, err := f.admin.ExecContext(ctx, "DROP DATABASE `"+name+"`")
			cancel()
			if err != nil {
				t.Error(fixtureFailure("drop_owned_schema", err))
			}
		}
		if f.applicationUser != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, err := f.admin.ExecContext(ctx, "DROP USER ?@'%'", f.applicationUser)
			cancel()
			if err != nil {
				t.Error(fixtureFailure("drop_owned_user", err))
			}
		}
	})
	if err := os.Chmod(f.runtime, 0700); err != nil {
		t.Fatal("mysql_fixture_runtime_permissions")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := f.admin.PingContext(ctx); err != nil {
		t.Fatal(fixtureFailure("admin_connect", err))
	}
	// 名称仅由固定前缀和 UUID 生成；不接管已有库，也不使用配置里的业务库名。
	for _, name := range []string{f.cfg.Primary.MainDatabase, f.cfg.Primary.HistoryDatabase, f.cfg.Standby.MainDatabase, f.cfg.Standby.HistoryDatabase} {
		if _, err := f.admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin"); err != nil {
			t.Fatal(fixtureFailure("create_schema", err))
		}
		f.created = append(f.created, name)
	}
	password := "Aa1!" + rand.Text()
	passwordFile := filepath.Join(f.runtime, "application.password")
	if err := os.WriteFile(passwordFile, []byte(password), 0600); err != nil {
		t.Fatal("mysql_fixture_application_password_unwritable")
	}
	user := "ob_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	if _, err := f.admin.ExecContext(ctx, "CREATE USER ?@'%' IDENTIFIED BY ?", user, password); err != nil {
		t.Fatal(fixtureFailure("create_application_user", err))
	}
	f.applicationUser = user
	for _, name := range f.created {
		if _, err := f.admin.ExecContext(ctx, "GRANT SELECT,INSERT,UPDATE,DELETE,CREATE,ALTER,DROP,INDEX,REFERENCES,TRIGGER ON `"+name+"`.* TO ?@'%'", user); err != nil {
			t.Fatal(fixtureFailure("grant_application_schema", err))
		}
	}
	f.cfg.Primary.User, f.cfg.Primary.PasswordFile = user, passwordFile
	f.cfg.Standby.User, f.cfg.Standby.PasswordFile = user, passwordFile
	return f
}

func (f *Fixture) Endpoint(role string) config.DatabaseEndpoint {
	if role == "standby" && f.cfg.Standby != nil {
		return *f.cfg.Standby
	}
	return f.cfg.Primary
}

func (f *Fixture) Open(t testing.TB, domain storage.Domain) *storage.Database {
	t.Helper()
	if f.dbs == nil {
		var err error
		f.dbs, err = storage.OpenDatabases(context.Background(), config.DatabaseConfig{Primary: f.cfg.Primary}, f.runtime)
		if err != nil {
			t.Fatal(err)
		}
	}
	if domain == storage.HistoryDomain {
		return f.dbs.History
	}
	return f.dbs.Main
}

func fixtureMySQLConfig(endpoint config.DatabaseEndpoint, database string) (*mysql.Config, error) {
	file, err := securefs.OpenFile(endpoint.PasswordFile, os.O_RDONLY, 0)
	if err != nil {
		return nil, errors.New("mysql_fixture_password_unreadable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Mode().Perm() != 0600 {
		return nil, errors.New("mysql_fixture_password_requires_0600")
	}
	password, err := io.ReadAll(io.LimitReader(file, 65537))
	defer clear(password)
	if err != nil || len(password) > 65536 {
		return nil, errors.New("mysql_fixture_password_invalid")
	}
	if len(password) > 0 && password[len(password)-1] == '\n' {
		password = password[:len(password)-1]
	}
	if len(password) == 0 {
		return nil, errors.New("mysql_fixture_password_empty")
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd = endpoint.User, string(password)
	cfg.Net, cfg.Addr, cfg.DBName = endpoint.Network, endpoint.Address, database
	if cfg.Net == "" {
		cfg.Net = "tcp"
	}
	cfg.TLS, err = fixtureTLS(endpoint)
	if err != nil {
		return nil, errors.New("mysql_fixture_tls_invalid")
	}
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 5*time.Second, 5*time.Second, 5*time.Second
	cfg.ClientFoundRows, cfg.InterpolateParams = true, true
	cfg.Logger = logging.MySQLLogger{}
	if err := cfg.Apply(mysql.Charset("utf8mb4", "")); err != nil {
		return nil, errors.New("mysql_fixture_charset_invalid")
	}
	cfg.Params = map[string]string{"sql_mode": "'STRICT_TRANS_TABLES,ONLY_FULL_GROUP_BY,NO_ZERO_DATE,NO_ZERO_IN_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'", "foreign_key_checks": "1", "collation_connection": "'utf8mb4_0900_bin'"}
	return cfg, nil
}

func (f *Fixture) Raw(t testing.TB, domain storage.Domain) *sql.DB {
	t.Helper()
	if db := f.raw[domain]; db != nil {
		return db
	}
	_ = f.Open(t, domain)
	endpoint := f.cfg.Primary
	name := endpoint.MainDatabase
	if domain == storage.HistoryDomain {
		name = endpoint.HistoryDatabase
	}
	cfg, err := fixtureMySQLConfig(endpoint, name)
	if err != nil {
		t.Fatal(fixtureFailure("raw_configuration", err))
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal("mysql_fixture_connector_invalid")
	}
	db := sql.OpenDB(connector)
	f.raw[domain] = db
	return db
}

func fixtureFailure(phase string, err error) error {
	var failure *mysql.MySQLError
	if errors.As(err, &failure) && failure != nil {
		return fmt.Errorf("mysql_fixture_%s mysql_code=%d", phase, failure.Number)
	}
	return fmt.Errorf("mysql_fixture_%s_failed", phase)
}
