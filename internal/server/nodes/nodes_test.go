package nodes_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
)

type lockedReader struct {
	mu sync.Mutex
	r  io.Reader
}

func (r *lockedReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.r.Read(p)
}

func newNodes(t *testing.T, capacity int) (*nodes.Service, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "orbitbeacon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := nodes.NewService(db, nodes.Options{
		MaxNodes: capacity,
		Now:      func() time.Time { return time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC) },
		Random:   &lockedReader{r: rand.NewChaCha8([32]byte{23})},
	})
	return service, db
}

func pending(t *testing.T, service *nodes.Service, name string) nodes.Node {
	t.Helper()
	node, err := service.CreatePending(context.Background(), nodes.CreateRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if node.ID == "" || node.Status != "pending" {
		t.Fatalf("created node = %+v, want a persistent pending identity", node)
	}
	return node
}

func TestCapacityIncludesPendingAndDisabledNodes(t *testing.T) {
	service, db := newNodes(t, 2)
	ctx := context.Background()
	first := pending(t, service, "disabled")
	if err := service.SetDisabled(ctx, first.ID, true); err != nil {
		t.Fatal(err)
	}
	second := pending(t, service, "still pending")
	if _, err := service.CreatePending(ctx, nodes.CreateRequest{Name: "over capacity"}); !errors.Is(err, nodes.ErrCapacityReached) {
		t.Errorf("capacity error = %v, want ErrCapacityReached", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM nodes").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("failed creation changed node count to %d", count)
	}
	if err := service.Delete(ctx, second.ID, true); err != nil {
		t.Fatal(err)
	}
	pending(t, service, "reuses deleted capacity")
}

func TestConcurrentPendingCreationCannotOverbookCapacity(t *testing.T) {
	service, db := newNodes(t, 2)
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan error, 8)
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := service.CreatePending(ctx, nodes.CreateRequest{Name: fmt.Sprintf("node-%d", i)})
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	var successes, full int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, nodes.ErrCapacityReached):
			full++
		default:
			t.Errorf("unexpected concurrent creation error: %v", err)
		}
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM nodes").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if successes != 2 || full != 6 || count != 2 {
		t.Errorf("capacity reservation: successes=%d rejected=%d persisted=%d", successes, full, count)
	}
}

func TestBatchRequiresExplicitBoundedIDsAndReportsEachNode(t *testing.T) {
	service, _ := newNodes(t, 4)
	ctx := context.Background()
	selected := pending(t, service, "selected")
	untouched := pending(t, service, "not selected")
	for _, ids := range [][]string{nil, {}} {
		if _, err := service.Batch(ctx, nodes.BatchRequest{NodeIDs: ids, Action: "disable"}); err == nil {
			t.Fatal("batch accepted an implicit all-nodes selection")
		}
	}
	tooMany := make([]string, 501)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	}
	if _, err := service.Batch(ctx, nodes.BatchRequest{NodeIDs: tooMany, Action: "disable"}); err == nil {
		t.Fatal("batch accepted more than 500 explicit IDs")
	}
	const missing = "00000000-0000-4000-8000-000000009999"
	results, err := service.Batch(ctx, nodes.BatchRequest{NodeIDs: []string{selected.ID, missing}, Action: "disable"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("batch result count = %d, want one result per explicit ID", len(results))
	}
	byID := make(map[string]nodes.BatchResult)
	for _, result := range results {
		if _, exists := byID[result.NodeID]; exists {
			t.Fatalf("duplicate result for %s", result.NodeID)
		}
		byID[result.NodeID] = result
	}
	if result := byID[selected.ID]; result.Status != "succeeded" || result.ErrorCode != "" {
		t.Errorf("existing node result = %+v", result)
	}
	if result := byID[missing]; result.Status != "failed" || result.ErrorCode != "node_not_found" {
		t.Errorf("missing node result = %+v", result)
	}
	got, err := service.Get(ctx, selected.ID)
	if err != nil || got.Status != "disabled" {
		t.Errorf("selected node was not disabled: node=%+v err=%v", got, err)
	}
	got, err = service.Get(ctx, untouched.ID)
	if err != nil || got.Status != "pending" {
		t.Errorf("batch modified an unselected node: node=%+v err=%v", got, err)
	}
}

// The queued job is seeded directly because scheduling signed releases belongs
// to a later slice; deletion must already honor persistent lifecycle state.
func seedCredentialAndUpdate(t *testing.T, db *sql.DB, nodeID string) (string, string) {
	t.Helper()
	ctx := context.Background()
	const token = "node-regression-token-with-at-least-32-bytes"
	const jobID = "00000000-0000-4000-8000-000000000123"
	digest := sha256.Sum256([]byte(token))
	if _, err := db.ExecContext(ctx, "UPDATE nodes SET token_hash = ?, installation_id = ? WHERE id = ?", digest[:], "00000000-0000-4000-8000-000000000456", nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO update_jobs(id, release_seq, created_at) VALUES (?, ?, ?)", jobID, 1, "2026-02-03T04:05:06Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO update_job_nodes(job_id, node_id, status) VALUES (?, ?, ?)", jobID, nodeID, "queued"); err != nil {
		t.Fatal(err)
	}
	return token, jobID
}

func queuedStatus(t *testing.T, db *sql.DB, jobID, nodeID string) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(context.Background(), "SELECT status FROM update_job_nodes WHERE job_id = ? AND node_id = ?", jobID, nodeID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestDeletionRequiresConfirmationAndRevokesCredentialsAndPendingUpdate(t *testing.T) {
	service, db := newNodes(t, 2)
	ctx := context.Background()
	node := pending(t, service, "delete me")
	token, jobID := seedCredentialAndUpdate(t, db, node.ID)
	if got, err := service.AuthenticateToken(ctx, token); err != nil || got.ID != node.ID {
		t.Fatalf("fixture credential is not active: node=%+v err=%v", got, err)
	}
	if err := service.Delete(ctx, node.ID, false); err == nil {
		t.Fatal("deleted node without confirmation")
	}
	if _, err := service.AuthenticateToken(ctx, token); err != nil {
		t.Fatalf("unconfirmed deletion revoked credentials: %v", err)
	}
	if got := queuedStatus(t, db, jobID, node.ID); got != "queued" {
		t.Fatalf("unconfirmed deletion changed update state to %q", got)
	}
	if err := service.Delete(ctx, node.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(ctx, node.ID); !errors.Is(err, nodes.ErrNotFound) {
		t.Errorf("deleted node still exists: %v", err)
	}
	if _, err := service.AuthenticateToken(ctx, token); !errors.Is(err, nodes.ErrUnauthenticated) {
		t.Errorf("deleted node credential still authenticates: %v", err)
	}
	if got := queuedStatus(t, db, jobID, node.ID); got != "cancelled" {
		t.Errorf("deleted node has pending update status %q", got)
	}
}

func TestDeletionFailureRollsBackAllIdentityAndUpdateChanges(t *testing.T) {
	for _, trigger := range []string{
		"CREATE TRIGGER reject_node_delete BEFORE DELETE ON nodes BEGIN SELECT RAISE(ABORT, 'injected deletion failure'); END",
		"CREATE TRIGGER reject_update_cancel BEFORE UPDATE OF status ON update_job_nodes WHEN NEW.status = 'cancelled' BEGIN SELECT RAISE(ABORT, 'injected cancellation failure'); END",
	} {
		t.Run(trigger, func(t *testing.T) {
			service, db := newNodes(t, 2)
			ctx := context.Background()
			node := pending(t, service, "keep on rollback")
			token, jobID := seedCredentialAndUpdate(t, db, node.ID)
			if _, err := db.ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			if err := service.Delete(ctx, node.ID, true); err == nil {
				t.Fatal("deletion ignored injected storage failure")
			}
			if got, err := service.Get(ctx, node.ID); err != nil || got.ID != node.ID {
				t.Errorf("failed deletion lost asset: node=%+v err=%v", got, err)
			}
			if got, err := service.AuthenticateToken(ctx, token); err != nil || got.ID != node.ID {
				t.Errorf("failed deletion revoked identity: node=%+v err=%v", got, err)
			}
			if got := queuedStatus(t, db, jobID, node.ID); got != "queued" {
				t.Errorf("failed deletion did not restore queued update: %q", got)
			}
		})
	}
}
