package management

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"orbitbeacon/internal/server/audit"
)

type PublicPageSettings struct {
	Enabled bool `json:"enabled"`
}

func readPublicPageSettings(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (PublicPageSettings, error) {
	var value string
	err := reader.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='public_page_enabled'").Scan(&value)
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
	return readPublicPageSettings(ctx, s.db)
}

func (s *Service) SetPublicPageEnabled(ctx context.Context, enabled bool) (PublicPageSettings, error) {
	tx, err := s.db.BeginTx(ctx, nil)
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
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('public_page_enabled',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", strconv.FormatBool(enabled)); err != nil {
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
