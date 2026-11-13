package services

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"

	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/uuid"
)

// Group errors mirror node-group validation and the service capacity contract.
var ErrInvalidGroup = errors.New("invalid service group input")
var ErrGroupCapacity = errors.New("maximum of 100 service groups reached")

type Group struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DisplayOrder int    `json:"display_order"`
}

func scanGroup(row interface{ Scan(...any) error }) (Group, error) {
	var group Group
	err := row.Scan(&group.ID, &group.Name, &group.DisplayOrder)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return group, err
}

func (m *Manager) Groups(ctx context.Context) ([]Group, error) {
	lease, leaseErr := m.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return nil, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	rows, err := lease.QueryContext(ctx, "SELECT id, name, display_order FROM service_groups ORDER BY display_order DESC, name, id")
	if err != nil {
		return nil, err
	}
	return scanGroups(rows)
}

func scanGroups(rows *storage.Rows) ([]Group, error) {
	defer rows.Close()
	groups := make([]Group, 0)
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (m *Manager) Group(ctx context.Context, id string) (Group, error) {
	lease, leaseErr := m.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return Group{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	return scanGroup(lease.QueryRowContext(ctx, "SELECT id, name, display_order FROM service_groups WHERE id = ?", id))
}

func (m *Manager) SaveGroup(ctx context.Context, id, name string) (Group, error) {
	lease, leaseErr := m.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return Group{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	if strings.TrimSpace(name) == "" || len(name) > 255 {
		return Group{}, ErrInvalidGroup
	}
	action := "service_group.update"
	create := id == ""
	if create {
		var err error
		id, err = uuid.UUID(rand.Reader)
		if err != nil {
			return Group{}, err
		}
		action = "service_group.create"
	}
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return Group{}, err
	}
	defer tx.Rollback()
	var duplicate int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM service_groups WHERE name = ? AND id <> ?", name, id).Scan(&duplicate); err != nil {
		return Group{}, err
	}
	if duplicate != 0 {
		return Group{}, ErrInvalidGroup
	}
	if create {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM service_groups").Scan(&count); err != nil {
			return Group{}, err
		}
		if count >= 100 {
			return Group{}, ErrGroupCapacity
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO service_groups(id, name) VALUES (?, ?)", id, name)
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, "UPDATE service_groups SET name = ? WHERE id = ?", name, id)
		if err == nil {
			err = requireRow(result)
		}
	}
	if err != nil {
		return Group{}, err
	}
	group, err := scanGroup(tx.QueryRowContext(ctx, "SELECT id, name, display_order FROM service_groups WHERE id = ?", id))
	if err != nil {
		return Group{}, err
	}
	if err := audit.InsertDetails(ctx, tx, action, id, m.now(), audit.Details{"name": group.Name}); err != nil {
		return Group{}, err
	}
	if err := tx.Commit(); err != nil {
		return Group{}, err
	}
	m.signal()
	return group, nil
}

func (m *Manager) ReorderGroups(ctx context.Context, ids []string) ([]Group, error) {
	lease, err := m.db.Acquire(ctx, storage.Writer)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if ids == nil || len(ids) > 100 {
		return nil, ErrInvalidGroup
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, name, display_order FROM service_groups")
	if err != nil {
		return nil, err
	}
	current, err := scanGroups(rows)
	if err != nil {
		return nil, err
	}
	if len(ids) != len(current) {
		return nil, ErrInvalidGroup
	}
	byID := make(map[string]Group, len(current))
	for _, group := range current {
		byID[group.ID] = group
	}
	groups := make([]Group, 0, len(ids))
	for i, id := range ids {
		group, ok := byID[id]
		if !ok {
			return nil, ErrInvalidGroup
		}
		delete(byID, id)
		group.DisplayOrder = len(ids) - i
		groups = append(groups, group)
	}
	for _, group := range groups {
		if _, err := tx.ExecContext(ctx, "UPDATE service_groups SET display_order = ? WHERE id = ?", group.DisplayOrder, group.ID); err != nil {
			return nil, err
		}
	}
	if err := audit.Insert(ctx, tx, "service_group.reorder", "", m.now()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	m.signal()
	return groups, nil
}

func requireRow(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (m *Manager) DeleteGroup(ctx context.Context, id string) error {
	lease, leaseErr := m.db.Acquire(ctx, storage.Writer)
	if leaseErr != nil {
		return leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	tx, err := lease.BeginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	group, err := scanGroup(tx.QueryRowContext(ctx, "SELECT id, name, display_order FROM service_groups WHERE id = ?", id))
	if err != nil {
		return err
	}
	// ON DELETE SET NULL detaches member services in this transaction.
	result, err := tx.ExecContext(ctx, "DELETE FROM service_groups WHERE id = ?", id)
	if err != nil {
		return err
	}
	if err := requireRow(result); err != nil {
		return err
	}
	if err := audit.InsertDetails(ctx, tx, "service_group.delete", id, m.now(), audit.Details{"name": group.Name}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	m.signal()
	return nil
}
