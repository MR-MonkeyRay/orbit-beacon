package management

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/storage"
)

type PublicPageSettings struct {
	Enabled bool `json:"enabled"`
}

func readPublicPageSettings(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *storage.Row
}) (PublicPageSettings, error) {
	var value string
	err := reader.QueryRowContext(ctx, "SELECT value FROM settings WHERE `key`='public_page_enabled'").Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return PublicPageSettings{Enabled: true}, nil
	}
	if err != nil {
		return PublicPageSettings{}, err
	}
	switch value {
	case "true":
		return PublicPageSettings{Enabled: true}, nil
	case "false":
		return PublicPageSettings{Enabled: false}, nil
	default:
		return PublicPageSettings{}, fmt.Errorf("invalid persisted public_page_enabled value")
	}
}

func (s *Service) PublicPageSettings(ctx context.Context) (PublicPageSettings, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return PublicPageSettings{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	return readPublicPageSettings(ctx, lease)
}

func (s *Service) SetPublicPageEnabled(ctx context.Context, enabled bool) (PublicPageSettings, error) {
	lease, leaseErr := s.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return PublicPageSettings{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return PublicPageSettings{}, err
	}
	defer tx.Rollback()
	current, err := readPublicPageSettings(ctx, tx)
	if err != nil {
		return PublicPageSettings{}, err
	}
	if current.Enabled == enabled {
		return current, nil
	}
	query := "INSERT INTO settings(`key`,value) VALUES('public_page_enabled',?) ON CONFLICT(`key`) DO UPDATE SET value=excluded.value"
	if tx.Dialect() == storage.MySQL {
		query = "INSERT INTO settings(`key`,value) VALUES('public_page_enabled',?) ON DUPLICATE KEY UPDATE value=VALUES(value)"
	}
	if _, err = tx.ExecContext(ctx, query, strconv.FormatBool(enabled)); err != nil {
		return PublicPageSettings{}, err
	}
	if err = audit.Insert(ctx, tx, "public_page.update", "", time.Now()); err != nil {
		return PublicPageSettings{}, err
	}
	if err = tx.Commit(); err != nil {
		return PublicPageSettings{}, err
	}
	return PublicPageSettings{Enabled: enabled}, nil
}
