package auth

import (
	"context"
	"database/sql"
	"errors"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/timekey"
)

// UserPatch 仅更新非敏感的用户元数据；密码通过独立流程修改。
type UserPatch struct {
	Username *string `json:"username"`
}

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var user User
	var created string
	if err := row.Scan(&user.ID, &user.Username, &created, &user.MustChangePassword); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	var err error
	user.CreatedAt, err = timekey.Parse(created)
	if err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, username, created_at, must_change_password FROM users ORDER BY username, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return users, nil
}

func (s *Service) GetUser(ctx context.Context, id string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT id, username, created_at, must_change_password FROM users WHERE id = ?", id))
}

func (s *Service) PatchUser(ctx context.Context, id string, patch UserPatch) (User, error) {
	if patch.Username != nil && !validUsername(*patch.Username) {
		return User{}, ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	user, err := scanUser(tx.QueryRowContext(ctx, "SELECT id, username, created_at, must_change_password FROM users WHERE id = ?", id))
	if err != nil {
		return User{}, err
	}
	if patch.Username != nil {
		if *patch.Username != user.Username {
			if err := requireAvailableUsername(ctx, tx, *patch.Username, id); err != nil {
				return User{}, err
			}
		}
		user.Username = *patch.Username
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET username = ? WHERE id = ?", user.Username, id); err != nil {
		return User{}, err
	}
	if err := audit.Insert(ctx, tx, "user.patch", id, s.now()); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *Service) DeleteUser(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = scanUser(tx.QueryRowContext(ctx, "SELECT id, username, created_at, must_change_password FROM users WHERE id = ?", id))
	if err != nil {
		return err
	}
	if err := preserveAdministrator(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM users WHERE id = ?", id); err != nil {
		return err
	}
	if err := audit.Insert(ctx, tx, "user.delete", id, s.now()); err != nil {
		return err
	}
	return tx.Commit()
}

// 立即写事务可防止并发删除各自独立地决定删除最后一名管理员。
func preserveAdministrator(ctx context.Context, tx *sql.Tx) error {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&count); err != nil {
		return err
	}
	if count <= 1 {
		return ErrLastAdmin
	}
	return nil
}

func requireAvailableUsername(ctx context.Context, tx *sql.Tx, username, excludeID string) error {
	var taken bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE username=? AND id<>?)", username, excludeID).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return ErrUsernameConflict
	}
	return nil
}
