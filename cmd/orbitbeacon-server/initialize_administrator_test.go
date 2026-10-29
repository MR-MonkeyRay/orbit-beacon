package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbitbeacon/internal/server/auth"
	"orbitbeacon/internal/server/storage"
)

func initializationDB(t *testing.T) (*sql.DB, *auth.Service) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(t.Context(), filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, auth.NewService(db, auth.Options{})
}
func TestInitializeAdministratorCredentials(t *testing.T) {
	for _, tt := range []struct {
		name               string
		env                map[string]string
		username, password string
		invalid            bool
	}{
		{"defaults", nil, "orbitbeacon", "orbitbeacon", false},
		{"username only", map[string]string{"ORBITBEACON_ADMIN_USERNAME": "custom-admin"}, "custom-admin", "orbitbeacon", false},
		{"password only", map[string]string{"ORBITBEACON_ADMIN_PASSWORD": "custom-initial-password"}, "orbitbeacon", "custom-initial-password", false},
		{"both", map[string]string{"ORBITBEACON_ADMIN_USERNAME": "custom-admin", "ORBITBEACON_ADMIN_PASSWORD": " custom-password "}, "custom-admin", " custom-password ", false},
		{"empty username", map[string]string{"ORBITBEACON_ADMIN_USERNAME": ""}, "", "", true},
		{"empty password", map[string]string{"ORBITBEACON_ADMIN_PASSWORD": ""}, "", "", true},
		{"blank username", map[string]string{"ORBITBEACON_ADMIN_USERNAME": "  "}, "", "", true},
		{"long username", map[string]string{"ORBITBEACON_ADMIN_USERNAME": strings.Repeat("x", 129)}, "", "", true},
		{"short password", map[string]string{"ORBITBEACON_ADMIN_PASSWORD": "1234567"}, "", "", true},
		{"long password", map[string]string{"ORBITBEACON_ADMIN_PASSWORD": strings.Repeat("x", 1025)}, "", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db, service := initializationDB(t)
			lookup := func(key string) (string, bool) { value, ok := tt.env[key]; return value, ok }
			err := initializeAdministrator(t.Context(), db, service, lookup)
			var count int
			if scanErr := db.QueryRow("SELECT count(*) FROM users").Scan(&count); scanErr != nil {
				t.Fatal(scanErr)
			}
			if tt.invalid {
				if err == nil || count != 0 {
					t.Fatalf("invalid initialization mutated database: error=%v users=%d", err, count)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("users=%d", count)
			}
			session, err := service.Login(t.Context(), tt.username, tt.password)
			if err != nil || session.User.Username != tt.username {
				t.Fatalf("initialized credentials not usable: %v", err)
			}
			var originalHash string
			if err := db.QueryRow("SELECT password_hash FROM users").Scan(&originalHash); err != nil {
				t.Fatal(err)
			}
			if err := initializeAdministrator(t.Context(), db, service, func(string) (string, bool) { panic("existing database read initialization environment") }); err != nil {
				t.Fatal(err)
			}
			var currentHash string
			if err := db.QueryRow("SELECT password_hash FROM users").Scan(&currentHash); err != nil {
				t.Fatal(err)
			}
			if currentHash != originalHash {
				t.Fatal("restart rehashed administrator")
			}
		})
	}
}
func TestInitializeAdministratorDatabaseErrorsFailClosed(t *testing.T) {
	db, service := initializationDB(t)
	db.Close()
	if err := initializeAdministrator(context.Background(), db, service, func(string) (string, bool) { panic("database failure read credentials") }); err == nil {
		t.Fatal("database failure was ignored")
	}
}
func TestRemovedBootstrapCommand(t *testing.T) {
	if err := run([]string{"bootstrap"}); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("removed bootstrap accepted: %v", err)
	}
	if err := run([]string{"serve", "--username", "admin"}); err == nil {
		t.Fatal("removed username flag accepted")
	}
	if errors.Is(run([]string{"bootstrap"}), auth.ErrAdminExists) {
		t.Fatal("removed command entered authentication")
	}
}
