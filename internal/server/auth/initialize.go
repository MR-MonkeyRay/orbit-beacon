package auth

import (
	"context"
	"errors"
)

func (s *Service) InitializeAdministrator(ctx context.Context, lookupEnv func(string) (string, bool)) error {
	var exists bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM users)").Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	username, set := lookupEnv("ORBITBEACON_ADMIN_USERNAME")
	if !set {
		username = "orbitbeacon"
	}
	password, set := lookupEnv("ORBITBEACON_ADMIN_PASSWORD")
	if !set {
		password = "orbitbeacon"
	}
	_, err := s.BootstrapAdministrator(ctx, username, password)
	if errors.Is(err, ErrAdminExists) {
		return nil
	}
	return err
}
