package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/auth"
)

func TestInitialAdministratorRequiresPasswordChange(t *testing.T) {
	service, db, _ := newAuth(t)
	ctx := context.Background()
	user, err := service.BootstrapAdministrator(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	// 同时检查传输契约和存储状态：false 绝不能从响应中消失，
	// 初始化也绝不能默认为 false。
	encoded, err := json.Marshal(user)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["must_change_password"] != true {
		t.Fatalf("bootstrap user must require a password change: %s", encoded)
	}
	var required int
	if err := db.QueryRowContext(ctx, "SELECT must_change_password FROM users WHERE id=?", user.ID).Scan(&required); err != nil || required != 1 {
		t.Fatalf("stored bootstrap requirement=%d err=%v", required, err)
	}
	first, err := service.Login(ctx, user.Username, initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Login(ctx, user.Username, initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	if session, err := service.CurrentSession(ctx, first.Token); err != nil || session.User.ID != user.ID || session.CSRFToken != first.CSRFToken {
		t.Fatalf("restricted session lost its identity or CSRF: %+v, %v", session, err)
	}
	if _, err := service.Authenticate(ctx, first.Token); !errors.Is(err, auth.ErrPasswordChangeRequired) {
		t.Fatal("initial administrator received unrestricted management access")
	}
	if err := service.ChangePassword(ctx, user.ID, "wrong old password", "wrong old password"); !errors.Is(err, auth.ErrWrongPassword) {
		t.Fatalf("wrong old password must be checked before unchanged password: %v", err)
	}
	if err := service.ChangePassword(ctx, user.ID, initialPassword, initialPassword); !errors.Is(err, auth.ErrPasswordUnchanged) {
		t.Fatal("unchanged password cleared the initial requirement")
	}
	if err := service.ChangePassword(ctx, user.ID, initialPassword, strings.Repeat("x", 1025)); !errors.Is(err, auth.ErrInvalidInput) {
		t.Fatalf("oversized password accepted: %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT must_change_password FROM users WHERE id=?", user.ID).Scan(&required); err != nil || required != 1 {
		t.Fatalf("failed password changes altered requirement=%d err=%v", required, err)
	}
	for _, token := range []string{first.Token, second.Token} {
		if _, err := service.CurrentSession(ctx, token); err != nil {
			t.Fatalf("failed password change revoked restricted session: %v", err)
		}
	}
	if err := service.ChangePassword(ctx, user.ID, initialPassword, "changed initial password"); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{first.Token, second.Token} {
		if _, err := service.CurrentSession(ctx, token); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("initial password change did not revoke every old session: %v", err)
		}
	}
	if _, err := service.Login(ctx, user.Username, initialPassword); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("initial password still accepted: %v", err)
	}
	fresh, err := service.Login(ctx, user.Username, "changed initial password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, fresh.Token); err != nil {
		t.Fatalf("new password did not unlock management: %v", err)
	}
	encoded, err = json.Marshal(fresh.User)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["must_change_password"] != false {
		t.Fatalf("normal user must explicitly return false: %s", encoded)
	}
}

func TestInitialPasswordChangeRollsBackRequirementWhenAuditFails(t *testing.T) {
	service, db, _ := newAuth(t)
	ctx := context.Background()
	user, err := service.BootstrapAdministrator(ctx, "admin", initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.Login(ctx, user.Username, initialPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_initial_password_audit BEFORE INSERT ON audit_events
		WHEN NEW.action='user.password.change' BEGIN SELECT RAISE(ABORT, 'forced initial password audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := service.ChangePassword(audit.WithActor(ctx, user.ID), user.ID, initialPassword, "changed initial password"); err == nil || !strings.Contains(err.Error(), "forced initial password audit failure") {
		t.Fatalf("password change did not reach failing audit: %v", err)
	}
	var required int
	var hash string
	if err := db.QueryRowContext(ctx, "SELECT must_change_password,password_hash FROM users WHERE id=?", user.ID).Scan(&required, &hash); err != nil || required != 1 {
		t.Fatalf("audit failure changed restriction=%d err=%v", required, err)
	}
	if ok, err := auth.VerifyPassword(initialPassword, hash); err != nil || !ok {
		t.Fatalf("audit failure changed original hash: ok=%t err=%v", ok, err)
	}
	if _, err := service.CurrentSession(ctx, session.Token); err != nil {
		t.Fatalf("audit failure revoked restricted session: %v", err)
	}
}

func TestCreatedUserPasswordRequirementSurvivesReadAndRename(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "restricted"}[required], func(t *testing.T) {
			service, _, _ := newAuth(t)
			ctx := context.Background()
			user, err := service.CreateUser(ctx, "admin", initialPassword, required)
			if err != nil {
				t.Fatal(err)
			}
			if user.MustChangePassword != required {
				t.Fatalf("created user requirement=%t, want %t", user.MustChangePassword, required)
			}
			got, err := service.GetUser(ctx, user.ID)
			if err != nil || got.MustChangePassword != required {
				t.Fatalf("get user=%+v err=%v", got, err)
			}
			listed, err := service.ListUsers(ctx)
			if err != nil || len(listed) != 1 || listed[0].MustChangePassword != required {
				t.Fatalf("listed users=%+v err=%v", listed, err)
			}
			name := "renamed-admin"
			got, err = service.PatchUser(ctx, user.ID, auth.UserPatch{Username: &name})
			if err != nil || got.MustChangePassword != required || got.Username != name {
				t.Fatalf("renamed user=%+v err=%v", got, err)
			}
			session, err := service.Login(ctx, name, initialPassword)
			if err != nil || session.User.MustChangePassword != required {
				t.Fatalf("login user=%+v err=%v", session.User, err)
			}
			current, err := service.CurrentSession(ctx, session.Token)
			if err != nil || current.User.MustChangePassword != required {
				t.Fatalf("current user=%+v err=%v", current.User, err)
			}
			_, err = service.Authenticate(ctx, session.Token)
			if required && !errors.Is(err, auth.ErrPasswordChangeRequired) || !required && err != nil {
				t.Fatalf("management authentication required=%t err=%v", required, err)
			}
			if _, err := service.CreateUser(ctx, "other-admin", initialPassword, false); err != nil {
				t.Fatal(err)
			}
			if err := service.DeleteUser(ctx, user.ID); err != nil {
				t.Fatalf("delete user with password requirement=%t: %v", required, err)
			}
			if _, err := service.CurrentSession(ctx, session.Token); !errors.Is(err, auth.ErrUnauthenticated) {
				t.Fatalf("deleted user session survived: %v", err)
			}
		})
	}
}
