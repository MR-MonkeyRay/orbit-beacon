package storagetest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"orbitbeacon/internal/config"
	"orbitbeacon/internal/server/storage"
)

func TestFixtureSchemasIgnoreProfileDatabaseNames(t *testing.T) {
	endpoint := config.DatabaseEndpoint{Driver: "mysql", MainDatabase: "existing_main", HistoryDatabase: "existing_history"}
	first := fixtureDatabases(endpoint)
	second := fixtureDatabases(endpoint)
	seen := make(map[string]bool)
	valid := regexp.MustCompile(`^orbitbeacon_test_[0-9a-f]{32}_(pm|ph|sm|sh)$`)
	for _, cfg := range []config.DatabaseConfig{first, second} {
		if cfg.Standby == nil {
			t.Fatal("missing standby schemas")
		}
		for _, name := range []string{cfg.Primary.MainDatabase, cfg.Primary.HistoryDatabase, cfg.Standby.MainDatabase, cfg.Standby.HistoryDatabase} {
			if len(name) > 64 || !valid.MatchString(name) || seen[name] {
				t.Fatalf("unsafe or reused fixture schema %q", name)
			}
			seen[name] = true
		}
	}
}

func TestFixtureConnectorProtectsPasswordFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "password")
	endpoint := config.DatabaseEndpoint{Driver: "mysql", Network: "tcp", Address: "127.0.0.1:3306", User: "fixture", PasswordFile: path}
	for _, content := range []string{"", "\n", strings.Repeat("x", 65537)} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := fixtureMySQLConfig(endpoint, ""); err == nil {
			t.Fatal("invalid password file accepted")
		}
	}
	if err := os.WriteFile(path, []byte(" keep spaces \n\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := fixtureMySQLConfig(endpoint, "owned_schema")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Passwd != " keep spaces \n" || cfg.DBName != "owned_schema" || cfg.TLS != nil || cfg.MultiStatements || !cfg.ClientFoundRows || !cfg.InterpolateParams {
		t.Fatal("connector changed password bytes or connection contract")
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureMySQLConfig(endpoint, ""); err == nil {
		t.Fatal("non-0600 password file accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	endpoint.PasswordFile = link
	if _, err := fixtureMySQLConfig(endpoint, ""); err == nil {
		t.Fatal("symlink password file accepted")
	}
}

func TestFixtureProfileRequiresPrivateValidMySQLConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "admin.json")
	for _, content := range []string{`{}`, `null`, `{"database":{"primary":{"driver":"mysql","address":"invalid","user":"fixture","password_file":"/private/password"}}}`, `{} {}`} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := fixtureProfile(path); err == nil {
			t.Fatal("invalid admin profile accepted")
		}
	}
	content := `{"database":{"primary":{"driver":"mysql","network":"tcp","address":"127.0.0.1:3306","user":"fixture","password_file":"/private/password","main_database":"existing_main","history_database":"existing_history"}}}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	endpoint, err := fixtureProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.TLS != nil || endpoint.User != "fixture" {
		t.Fatal("profile did not preserve optional TLS and admin identity")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureProfile(path); err == nil {
		t.Fatal("public admin profile accepted")
	}
}

func TestFixtureTLSIsOptionalAndVerifiedWhenConfigured(t *testing.T) {
	endpoint := config.DatabaseEndpoint{Network: "tcp", Address: "db.example:3306"}
	if cfg, err := fixtureTLS(endpoint); err != nil || cfg != nil {
		t.Fatal("omitted TLS should use plaintext")
	}
	endpoint.TLS = &config.DatabaseTLS{}
	cfg, err := fixtureTLS(endpoint)
	if err != nil || cfg == nil || cfg.ServerName != "db.example" || cfg.InsecureSkipVerify || cfg.MinVersion == 0 {
		t.Fatal("explicit TLS must verify the server")
	}
}

func TestFixtureErrorsDoNotExposeServerMessages(t *testing.T) {
	err := fixtureFailure("admin_connect", &mysql.MySQLError{Number: 1045, Message: "private password DSN SQL"})
	if err.Error() != "mysql_fixture_admin_connect mysql_code=1045" {
		t.Fatal("fixture exposed unsafe admin diagnostics")
	}
}

func TestMySQLFixtureApplicationCannotReadSystemAccounts(t *testing.T) {
	fixture := Start(t)
	var count int
	err := fixture.Raw(t, storage.MainDomain).QueryRow("SELECT count(*) FROM mysql.user").Scan(&count)
	if code, ok := storage.MySQLCode(err); !ok || code != 1142 {
		t.Fatalf("fixture application escaped its schema grants: code=%d classified=%v", code, ok)
	}
}
