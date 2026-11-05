package commands

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

const testNode = "00000000-0000-4000-8000-000000000001"

func fixture(t *testing.T) (*sql.DB, *Service, time.Time) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(t.Context(), filepath.Join(dir, "commands.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	runtime, _ := json.Marshal(v1.DefaultRuntimeSettings())
	hash := sha256.Sum256([]byte("credential"))
	if _, err := db.Exec(`INSERT INTO nodes(id,name,status,token_hash,agent_runtime,run_generation,report_session_id,created_at) VALUES(?,'host','unknown',?,?,'1','session',?)`, testNode, hash[:], string(runtime), timekey.Format(now)); err != nil {
		t.Fatal(err)
	}
	return db, New(db, func() time.Time { return now }), now
}
func TestConcurrentCreateOnlyOneActiveAndPendingCancel(t *testing.T) {
	db, s, _ := fixture(t)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Create(context.Background(), testNode, "user", CreateRequest{Kind: "restart"})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrActive) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatal(success)
	}
	list, err := s.List(t.Context(), testNode)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	if err := s.Cancel(t.Context(), testNode, list[0].ID); err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(t.Context(), testNode, "user", CreateRequest{Kind: "stop", Confirm: "host"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Exchange(t.Context(), tx, testNode, 0, "1", nil, s.now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(t.Context(), testNode, r.ID); !errors.Is(err, ErrNotPending) {
		t.Fatal(err)
	}
}
func TestCommandCreateValidatesRuntimeAndClosedKinds(t *testing.T) {
	for _, test := range []struct {
		name, kind, confirm, sql string
		want                     error
	}{
		{"unknown", "shell", "", "", ErrInvalid}, {"upgrade", "upgrade", "", "", ErrInvalid}, {"confirm", "stop", "bad", "", ErrInvalid},
		{"no runtime", "restart", "", "agent_runtime=''", ErrNotAllowed},
		{"disabled locally", "restart", "", `agent_runtime='{"report_interval_seconds":10,"disk_interval_seconds":60,"interfaces":[],"disk_paths":[],"probe_targets":[],"remote_commands":[],"disabled_features":[]}'`, ErrNotAllowed},
		{"disabled", "restart", "", "status='disabled'", ErrNodeUnavailable}, {"unregistered", "restart", "", "token_hash=NULL", ErrNodeUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, s, _ := fixture(t)
			if test.sql != "" {
				if _, err := db.Exec(`UPDATE nodes SET ` + test.sql); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Create(t.Context(), testNode, "user", CreateRequest{Kind: test.kind, Confirm: test.confirm}); !errors.Is(err, test.want) {
				t.Fatal(err)
			}
		})
	}
}
func TestCommandCleanupRetainsLatestAndExpiresOfflineCommands(t *testing.T) {
	db, s, now := fixture(t)
	old := now.Add(-100 * 24 * time.Hour)
	for i := range 3 {
		at := old.Add(time.Duration(i) * time.Second)
		if _, err := db.Exec(`INSERT INTO agent_commands(id,node_id,credential_epoch,kind,state,requested_by,created_at,expires_at,finished_at) VALUES(?,?,0,'stop','succeeded','user',?,?,?)`, []string{"a", "b", "c"}[i], testNode, timekey.Format(at), timekey.Format(at.Add(Lifetime)), timekey.Format(at)); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.Cleanup(t.Context(), now, 128); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	var id string
	if err := db.QueryRow(`SELECT id FROM agent_commands`).Scan(&id); err != nil || id != "c" {
		t.Fatal(id, err)
	}
	r, err := s.Create(t.Context(), testNode, "user", CreateRequest{Kind: "restart"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cleanup(t.Context(), r.ExpiresAt, 128); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM agent_commands WHERE id=?`, r.ID).Scan(&state); err != nil || state != "expired" {
		t.Fatal(state, err)
	}
}
func TestCommandAuditCreateCancelAndDelivery(t *testing.T) {
	db, s, now := fixture(t)
	ctx := audit.WithActor(t.Context(), "operator")
	r, err := s.Create(ctx, testNode, "operator", CreateRequest{Kind: "restart"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(ctx, testNode, r.ID); err != nil {
		t.Fatal(err)
	}
	r, err = s.Create(ctx, testNode, "operator", CreateRequest{Kind: "restart"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Exchange(t.Context(), tx, testNode, 0, "1", nil, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM audit_events WHERE action IN ('command.pending','command.create','command.canceled','command.delivered')`).Scan(&count); err != nil || count != 4 {
		t.Fatal(count, err)
	}
}

func TestListShowsUnsweptExpiredCommandAsExpired(t *testing.T) {
	db, s, now := fixture(t)
	ctx := audit.WithSystemActor(context.Background(), "test")
	created, err := s.Create(ctx, testNode, "user", CreateRequest{Kind: "restart"})
	if err != nil {
		t.Fatal(err)
	}
	records, err := New(db, func() time.Time { return now.Add(Lifetime) }).List(ctx, testNode)
	if err != nil || len(records) != 1 || records[0].ID != created.ID || records[0].State != "expired" || records[0].ResultCode != "expired" {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	var stored string
	if err := db.QueryRow(`SELECT state FROM agent_commands WHERE id=?`, created.ID).Scan(&stored); err != nil || stored != "pending" {
		t.Fatalf("list wrote state %q: %v", stored, err)
	}
}
