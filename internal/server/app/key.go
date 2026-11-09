package app

import (
	"context"
	"errors"
	"path/filepath"

	"orbitbeacon/internal/server/datadir"
	"orbitbeacon/internal/server/securefs"
	"orbitbeacon/internal/server/storage"
)

func prepareDatabaseKey(ctx context.Context, dir string, db *storage.Database, establishedHA bool) error {
	exists, err := securefs.ValidateFile(filepath.Join(dir, "secrets.key"))
	if err != nil {
		return err
	}
	if !exists {
		if establishedHA {
			return errors.New("secrets_key_missing: restore the matching key for this registered cluster")
		}
		lease, err := db.Acquire(ctx, storage.Reader)
		if err != nil {
			return err
		}
		defer lease.Close()
		var encrypted bool
		err = lease.QueryRowContext(lease.Context(), `SELECT EXISTS(
   SELECT 1 FROM users WHERE totp_ciphertext IS NOT NULL OR totp_pending_ciphertext IS NOT NULL
   UNION ALL SELECT 1 FROM notification_channels WHERE secret_ciphertext IS NOT NULL OR url_ciphertext IS NOT NULL
   UNION ALL SELECT 1 FROM backup_plans WHERE credentials_ciphertext IS NOT NULL
   UNION ALL SELECT 1 FROM backup_tasks WHERE input_credentials_ciphertext IS NOT NULL OR password_ciphertext IS NOT NULL
  )`).Scan(&encrypted)
		if err != nil {
			return err
		}
		if encrypted {
			return errors.New("secrets_key_missing: restore the matching key for existing encrypted data")
		}
	}
	return datadir.Prepare(dir)
}
