package identity_test

import (
	"context"
	"crypto/sha256"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/agent/identity"
)

func TestTicketOperationsRetainExactCandidateAndParentAfterPromotion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	store, err := identity.Open(path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	initial := store.Current()
	if err := store.ConfirmEnrollment(context.Background(), initial.NodeToken, "node"); err != nil {
		t.Fatal(err)
	}
	ticket := sha256.Sum256([]byte("a-ticket"))
	pending, operation, err := store.StageEnrollment(context.Background(), ticket, true)
	if err != nil || operation.NodeToken != pending.PendingNodeToken || operation.PreviousNodeToken != initial.NodeToken {
		t.Fatalf("candidate and CAS parent were not staged together: %v", err)
	}
	if err := store.ConfirmEnrollment(context.Background(), operation.NodeToken, "node"); err != nil {
		t.Fatal(err)
	}
	reopened, err := identity.Open(path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	current, retry, err := reopened.StageEnrollment(context.Background(), ticket, true)
	if err != nil || retry != operation || current.PendingNodeToken != "" || current.NodeToken != operation.NodeToken {
		t.Fatalf("promotion lost the original operation: %v", err)
	}
	for i := byte(1); i <= 63; i++ {
		if _, _, err := reopened.RememberEnrollment(context.Background(), [32]byte{i}, operation.NodeToken); err != nil {
			t.Fatal(err)
		}
	}
	before := reopened.Current()
	if _, _, err := reopened.RememberEnrollment(context.Background(), [32]byte{64}, operation.NodeToken); err == nil {
		t.Fatal("unbounded aliases admitted")
	}
	if reopened.Current() != before {
		t.Fatal("rejected alias changed durable identity")
	}
	// A complete active-credential journal cannot block a new genuine rotation.
	newTicket := sha256.Sum256([]byte("new-ticket"))
	_, fresh, err := reopened.StageEnrollment(context.Background(), newTicket, true)
	if err != nil || fresh.NodeToken == operation.NodeToken || fresh.PreviousNodeToken != operation.NodeToken {
		t.Fatalf("bounded journal prevented rotation: %v", err)
	}
	if err := reopened.ConfirmEnrollment(context.Background(), fresh.NodeToken, "node"); err != nil {
		t.Fatal(err)
	}
	if _, _, exists, err := reopened.LookupEnrollment(context.Background(), ticket); err != nil || exists {
		t.Fatalf("obsolete secret operation was not pruned: exists=%v err=%v", exists, err)
	}
}
