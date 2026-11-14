package commands

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	hash := sha256.Sum256([]byte("credential"))
	if _, err := db.Exec(`INSERT INTO nodes(id,name,status,token_hash,run_generation,report_session_id,created_at) VALUES(?,'host','unknown',?,'1','session',?)`, testNode, hash[:], timekey.Format(now)); err != nil {
		t.Fatal(err)
	}
	stable, err := storage.OpenMain(t.Context(), filepath.Join(dir, "commands.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stable.Close() })
	return db, New(stable, func() time.Time { return now }), now
}
func TestConcurrentCreateOnlyOneActiveAndPendingCancel(t *testing.T) {
	_, s, _ := fixture(t)
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
	lease, err := s.db.Acquire(t.Context(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
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
func TestCommandCreateValidatesAvailabilityAndClosedKinds(t *testing.T) {
	for _, test := range []struct {
		name, kind, confirm, sql string
		want                     error
	}{
		{"unknown", "shell", "", "", ErrInvalid}, {"confirm", "stop", "bad", "", ErrInvalid},
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
func TestCommandAuditTargetsTheNodeAndSkipsDelivery(t *testing.T) {
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
	lease, err := s.db.Acquire(t.Context(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Exchange(t.Context(), tx, testNode, 0, "1", nil, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT action,target_id FROM audit_events WHERE action LIKE 'command.%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	actions := map[string]int{}
	for rows.Next() {
		var action, target string
		if err := rows.Scan(&action, &target); err != nil {
			t.Fatal(err)
		}
		if target != testNode {
			t.Fatalf("%s targets %s", action, target)
		}
		actions[action]++
	}
	if rows.Err() != nil || actions["command.create"] != 2 || actions["command.canceled"] != 1 || len(actions) != 2 {
		t.Fatalf("actions=%v err=%v", actions, rows.Err())
	}
}

func TestListShowsUnsweptExpiredCommandAsExpired(t *testing.T) {
	db, s, now := fixture(t)
	ctx := audit.WithSystemActor(context.Background(), "test")
	created, err := s.Create(ctx, testNode, "user", CreateRequest{Kind: "restart"})
	if err != nil {
		t.Fatal(err)
	}
	records, err := New(s.db, func() time.Time { return now.Add(Lifetime) }).List(ctx, testNode)
	if err != nil || len(records) != 1 || records[0].ID != created.ID || records[0].State != "expired" || records[0].ResultCode != "expired" {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	var stored string
	if err := db.QueryRow(`SELECT state FROM agent_commands WHERE id=?`, created.ID).Scan(&stored); err != nil || stored != "pending" {
		t.Fatalf("list wrote state %q: %v", stored, err)
	}
}

func TestCreateGroupIsolatesNodeTransactionFailuresAndAcceptsRestart(t *testing.T) {
	db, s, _ := fixture(t)
	const groupID = "00000000-0000-4000-8000-000000000070"
	const secondNode = "00000000-0000-4000-8000-000000000002"
	const outsideNode = "00000000-0000-4000-8000-000000000003"
	if _, err := db.Exec(`INSERT INTO groups(id,name) VALUES(?,'Production')`, groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE nodes SET group_id=? WHERE id=?`, groupID, testNode); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{secondNode, outsideNode} {
		hash := sha256.Sum256([]byte(id))
		if _, err := db.Exec(`INSERT INTO nodes(id,name,status,token_hash,run_generation,report_session_id,created_at,group_id) SELECT ?,?,'unknown',?,run_generation,report_session_id,created_at,? FROM nodes WHERE id=?`, id, id, hash[:], groupID, testNode); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE nodes SET group_id=NULL WHERE id=?`, outsideNode); err != nil {
		t.Fatal(err)
	}
	// The first node fails during INSERT, after it has passed the normal eligibility checks.
	if _, err := db.Exec(`CREATE TRIGGER fail_one_command BEFORE INSERT ON agent_commands WHEN NEW.node_id='00000000-0000-4000-8000-000000000001' BEGIN SELECT RAISE(ABORT,'node failure'); END`); err != nil {
		t.Fatal(err)
	}
	ctx := audit.WithSystemActor(t.Context(), "test")
	result, err := s.CreateGroup(ctx, groupID, "Alice", CreateRequest{Kind: "restart", Confirm: "Production"})
	if err != nil || result.Created != 1 || result.Skipped != 1 {
		t.Fatal(result, err)
	}
	list, err := s.List(t.Context(), secondNode)
	if err != nil || len(list) != 1 || list[0].RequestedBy != "Alice" {
		t.Fatal(list, err)
	}
	for _, id := range []string{testNode, outsideNode} {
		list, err := s.List(t.Context(), id)
		if err != nil || len(list) != 0 {
			t.Fatal(id, list, err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM audit_events WHERE action='command.bulk' AND target_id=?`, groupID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestCreateGroupHasNoMemberLimitAndHandlesEmptyGroups(t *testing.T) {
	db, s, now := fixture(t)
	const groupID = "00000000-0000-4000-8000-000000000070"
	if _, err := db.Exec(`INSERT INTO groups(id,name) VALUES(?,'Fleet')`, groupID); err != nil {
		t.Fatal(err)
	}
	empty, err := s.CreateGroup(t.Context(), groupID, "Alice", CreateRequest{Kind: "restart", Confirm: "Fleet"})
	if err != nil || empty != (BulkResult{}) {
		t.Fatal(empty, err)
	}
	for i := range 201 {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+100)
		hash := sha256.Sum256([]byte(id))
		if _, err := db.Exec(`INSERT INTO nodes(id,name,status,token_hash,run_generation,report_session_id,created_at,group_id) VALUES(?,?,'unknown',?,'1','session',?,?)`, id, id, hash[:], timekey.Format(now), groupID); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.CreateGroup(t.Context(), groupID, "Alice", CreateRequest{Kind: "restart", Confirm: "Fleet"})
	if err != nil || result.Created != 201 || result.Skipped != 0 {
		t.Fatal(result, err)
	}
}

func TestCreateRejectsStoppedNode(t *testing.T) {
	db, s, now := fixture(t)
	stopped, err := s.Create(t.Context(), testNode, "Alice", CreateRequest{Kind: "stop", Confirm: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE agent_commands SET state='succeeded',result_code='ok',delivered_generation='1',finished_at=? WHERE id=?`, timekey.Format(now), stopped.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(t.Context(), testNode, "Alice", CreateRequest{Kind: "restart"}); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatal(err)
	}
}

func TestUpdateCommandsLifetimeExchangeAndResult(t *testing.T) {
	for _, kind := range []string{"check_update", "upgrade"} {
		t.Run(kind, func(t *testing.T) {
			db, s, now := fixture(t)
			ctx := audit.WithSystemActor(t.Context(), "test")
			if _, err := db.Exec(`UPDATE nodes SET system='{"agent_version":"1.2.3"}' WHERE id=?`, testNode); err != nil {
				t.Fatal(err)
			}
			created, err := s.Create(ctx, testNode, "user", CreateRequest{Kind: kind})
			lifetime := Lifetime
			if kind == "upgrade" {
				lifetime = UpgradeLifetime
			}
			if err != nil || created.ExpiresAt.Sub(now) != lifetime {
				t.Fatal(created, err)
			}
			lease, err := s.db.Acquire(ctx, storage.Writer)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			tx, err := lease.BeginWrite(lease.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			command, err := Exchange(ctx, tx, testNode, 0, "1", nil, now)
			if err != nil || command == nil || command.Kind != kind {
				t.Fatal(command, err)
			}
			wire, err := json.Marshal(command)
			var fields map[string]any
			if err != nil || json.Unmarshal(wire, &fields) != nil || len(fields) != 3 {
				t.Fatal(string(wire), err)
			}
			update := &v1.UpdateInfo{Channel: "github", CurrentVersion: "1.2.3", LatestVersion: "1.2.3", CheckedAt: now}
			if next, err := Exchange(ctx, tx, testNode, 0, "1", []v1.CommandResult{{ID: created.ID, Status: "succeeded", Code: "ok", Update: update}}, now); err != nil || next != nil {
				t.Fatal(next, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			list, err := s.List(ctx, testNode)
			if err != nil || len(list) != 1 || list[0].State != "succeeded" || list[0].Update == nil || *list[0].Update != *update {
				t.Fatal(list, err)
			}
			var details map[string]any
			var raw string
			if err := db.QueryRow(`SELECT details FROM audit_events WHERE action='command.create' AND target_id=?`, testNode).Scan(&raw); err != nil || json.Unmarshal([]byte(raw), &details) != nil || details["target_version"] != nil {
				t.Fatal(raw, err)
			}
		})
	}
}

func TestUpgradeGroupIgnoresCurrentVersionAndSkipsStopped(t *testing.T) {
	db, s, now := fixture(t)
	const group = "00000000-0000-4000-8000-000000000070"
	if _, err := db.Exec(`INSERT INTO groups(id,name) VALUES(?,'Production')`, group); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE nodes SET group_id=? WHERE id=?`, group, testNode); err != nil {
		t.Fatal(err)
	}
	ids := []string{testNode, "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003", "00000000-0000-4000-8000-000000000004"}
	for _, id := range ids[1:] {
		hash := sha256.Sum256([]byte(id))
		if _, err := db.Exec(`INSERT INTO nodes(id,name,status,token_hash,run_generation,report_session_id,created_at,group_id) SELECT ?,?,'unknown',?,run_generation,report_session_id,created_at,? FROM nodes WHERE id=?`, id, id, hash[:], group, testNode); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE nodes SET system='{"agent_version":"1.2.4"}' WHERE id=?`, ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agent_commands(id,node_id,credential_epoch,kind,state,result_code,requested_by,created_at,expires_at,delivered_generation) VALUES('00000000-0000-4000-8000-000000000090',?,0,'stop','succeeded','ok','user',?,?,'1')`, ids[3], timekey.Format(now.Add(-time.Minute)), timekey.Format(now.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	result, err := s.CreateGroup(t.Context(), group, "user", CreateRequest{Kind: "upgrade", Confirm: "Production"})
	if err != nil || result.Created != 3 || result.Skipped != 1 {
		t.Fatal(result, err)
	}
	for _, id := range []string{ids[3]} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM agent_commands WHERE node_id=? AND kind='upgrade'`, id).Scan(&count); err != nil || count != 0 {
			t.Fatal(id, count, err)
		}
	}
}

func TestCreateRequestRejectsAllCommandArguments(t *testing.T) {
	for _, body := range []string{`{"kind":"upgrade","target_version":"1.2.3"}`, `{"kind":"upgrade","target_version":""}`, `{"kind":"upgrade","target_version":null}`, `{"kind":"check_update","url":"https://attacker.example"}`, `{"kind":"upgrade","hash":"abcd"}`, `{"kind":"restart","args":[]}`, `{"kind":"stop","target_version":null}`} {
		var req CreateRequest
		if err := json.Unmarshal([]byte(body), &req); err == nil {
			t.Fatal("accepted command arguments", body)
		}
	}
	for _, body := range []string{`{"kind":"restart"}`, `{"kind":"upgrade"}`, `{"kind":"check_update"}`} {
		var req CreateRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil || !req.valid() {
			t.Fatal(body, req, err)
		}
	}
}

func TestListPreservesLegacyPartialUpdateMetadata(t *testing.T) {
	db, s, _ := fixture(t)
	created, err := s.Create(t.Context(), testNode, "operator", CreateRequest{Kind: "upgrade"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE agent_commands SET state='canceled',update_info='{"latest_version":"1.2.4"}' WHERE id=?`, created.ID); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(t.Context(), testNode)
	if err != nil || len(list) != 1 || list[0].Update == nil || list[0].Update.LatestVersion != "1.2.4" {
		t.Fatal(list, err)
	}
}

func TestCheckUpdateGroupCreatesFixedCommands(t *testing.T) {
	db, s, _ := fixture(t)
	const groupID = "00000000-0000-4000-8000-000000000070"
	if _, err := db.Exec(`INSERT INTO groups(id,name) VALUES(?,'Fleet')`, groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE nodes SET group_id=? WHERE id=?`, groupID, testNode); err != nil {
		t.Fatal(err)
	}
	result, err := s.CreateGroup(t.Context(), groupID, "operator", CreateRequest{Kind: "check_update", Confirm: "Fleet"})
	if err != nil || result.Created != 1 || result.Skipped != 0 {
		t.Fatal(result, err)
	}
	list, err := s.List(t.Context(), testNode)
	if err != nil || len(list) != 1 || list[0].Kind != "check_update" {
		t.Fatal(list, err)
	}
}

func TestNonUpdateCommandRejectsUpdateResultMetadata(t *testing.T) {
	_, s, now := fixture(t)
	created, err := s.Create(t.Context(), testNode, "operator", CreateRequest{Kind: "restart"})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.db.Acquire(t.Context(), storage.Writer)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tx, err := lease.BeginWrite(lease.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := Exchange(t.Context(), tx, testNode, 0, "1", nil, now); err != nil {
		t.Fatal(err)
	}
	result := v1.CommandResult{ID: created.ID, Status: "succeeded", Code: "ok", Update: &v1.UpdateInfo{Channel: "github", CurrentVersion: "1.2.3", LatestVersion: "1.2.4", CheckedAt: now}}
	if _, err := Exchange(t.Context(), tx, testNode, 0, "1", []v1.CommandResult{result}, now); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
