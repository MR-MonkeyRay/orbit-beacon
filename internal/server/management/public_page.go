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
	Enabled    bool `json:"enabled"`
	ShowGroups bool `json:"show_groups"`
}

// PublicPageSettingsPatch 中为 nil 的字段保持不变；HTTP 层负责 JSON 字段校验。
type PublicPageSettingsPatch struct {
	Enabled    *bool
	ShowGroups *bool
}

func readPublicPageSettings(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *storage.Row
}) (PublicPageSettings, error) {
	settings := PublicPageSettings{Enabled: true}
	for _, field := range []struct {
		key    string
		target *bool
	}{
		{"public_page_enabled", &settings.Enabled},
		{"public_page_show_groups", &settings.ShowGroups},
	} {
		var value string
		err := reader.QueryRowContext(ctx, "SELECT value FROM settings WHERE `key`=?", field.key).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return PublicPageSettings{}, err
		}
		switch value {
		case "true":
			*field.target = true
		case "false":
			*field.target = false
		default:
			return PublicPageSettings{}, fmt.Errorf("invalid persisted %s value", field.key)
		}
	}
	return settings, nil
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
	return s.PatchPublicPageSettings(ctx, PublicPageSettingsPatch{Enabled: &enabled})
}

func (s *Service) PatchPublicPageSettings(ctx context.Context, patch PublicPageSettingsPatch) (PublicPageSettings, error) {
	if patch.Enabled == nil && patch.ShowGroups == nil {
		return PublicPageSettings{}, ErrInvalidInput
	}
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
	changes := audit.Details{}
	for _, field := range []struct {
		name, key string
		value     *bool
		current   *bool
	}{
		{"enabled", "public_page_enabled", patch.Enabled, &current.Enabled},
		{"show_groups", "public_page_show_groups", patch.ShowGroups, &current.ShowGroups},
	} {
		if field.value == nil || *field.value == *field.current {
			continue
		}
		query := "INSERT INTO settings(`key`,value) VALUES(?,?) ON CONFLICT(`key`) DO UPDATE SET value=excluded.value"
		if tx.Dialect() == storage.MySQL {
			query = "INSERT INTO settings(`key`,value) VALUES(?,?) ON DUPLICATE KEY UPDATE value=VALUES(value)"
		}
		if _, err = tx.ExecContext(ctx, query, field.key, strconv.FormatBool(*field.value)); err != nil {
			return PublicPageSettings{}, err
		}
		*field.current = *field.value
		changes[field.name] = *field.value
	}
	if len(changes) == 0 {
		return current, nil
	}
	if err = audit.InsertDetails(ctx, tx, "public_page.update", "", time.Now(), changes); err != nil {
		return PublicPageSettings{}, err
	}
	if err = tx.Commit(); err != nil {
		return PublicPageSettings{}, err
	}
	return current, nil
}
