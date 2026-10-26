package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"orbitbeacon/internal/identity"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/timekey"
)

// Publish a new safe revision before serving so upgraded agents need no manual
// intervention to escape a formerly accepted oversized persistent selection.
func repairReportSelections(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT id,disk_paths,interfaces FROM nodes")
	if err != nil {
		return err
	}
	defer rows.Close()
	var repairs []string
	for rows.Next() {
		var id, disks, interfaces string
		if err := rows.Scan(&id, &disks, &interfaces); err != nil {
			return err
		}
		var selection v1.DesiredConfig
		if err := json.Unmarshal([]byte(disks), &selection.DiskPaths); err != nil {
			return fmt.Errorf("invalid saved disk selection: %w", err)
		}
		if err := json.Unmarshal([]byte(interfaces), &selection.Interfaces); err != nil {
			return fmt.Errorf("invalid saved interface selection: %w", err)
		}
		if err := selection.ValidateSelection(); errors.Is(err, v1.ErrReportBudget) {
			repairs = append(repairs, id)
		} else if err != nil {
			return fmt.Errorf("invalid saved selection: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range repairs {
		result, err := tx.ExecContext(ctx, "UPDATE nodes SET disk_paths='[]',interfaces='[]',desired_revision=desired_revision+1 WHERE id=? AND desired_revision<9223372036854775807", id)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			return errors.New("cannot publish report-budget repair revision")
		}
		eventID, err := identity.UUID(rand.Reader)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO audit_events(id,actor_id,action,target_id,created_at,details) VALUES(?,NULL,'node.config.report_budget_repair',?,?,'{}')", eventID, id, timekey.Format(time.Now())); err != nil {
			return err
		}
	}
	return nil
}
