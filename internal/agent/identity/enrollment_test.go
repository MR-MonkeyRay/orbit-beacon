package identity_test

import (
	"context"
	"crypto/sha256"
	"path/filepath"
	"testing"

	"orbitbeacon/internal/agent/identity"
)

func TestTicketOperationsRetainExactCandidateAndParentAfterPromotion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "identity.json")
	store, err := identity.Open(t.Context(), path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	initial := store.Current()
	if err := store.ConfirmEnrollment(ctx, initial.NodeToken, "node"); err != nil {
		t.Fatal(err)
	}
	ticket := sha256.Sum256([]byte("a-ticket"))
	pending, operation, err := store.StageEnrollment(ctx, ticket, true)
	if err != nil || operation.NodeToken != pending.PendingNodeToken || operation.PreviousNodeToken != initial.NodeToken {
		t.Fatalf("candidate and CAS parent were not staged together: %v", err)
	}
	for i := byte(1); i <= 63; i++ {
		_, alias, err := store.StageEnrollment(ctx, [32]byte{i}, true)
		if err != nil || alias != operation {
			t.Fatalf("pending candidate alias differs: %v", err)
		}
	}
	before := store.Current()
	if _, _, err := store.StageEnrollment(ctx, [32]byte{64}, true); err == nil {
		t.Fatal("unbounded aliases admitted")
	}
	if store.Current() != before {
		t.Fatal("rejected alias changed durable identity")
	}
	if err := store.ConfirmEnrollment(ctx, operation.NodeToken, "node"); err != nil {
		t.Fatal(err)
	}
	reopened, err := identity.Open(t.Context(), path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	current, retry, err := reopened.StageEnrollment(ctx, ticket, true)
	if err != nil || retry != operation || current.PendingNodeToken != "" || current.NodeToken != operation.NodeToken {
		t.Fatalf("promotion lost the original operation: %v", err)
	}
	// 已提升凭据的关联达到上限，仍可开始下一轮轮换。
	newTicket := sha256.Sum256([]byte("new-ticket"))
	_, fresh, err := reopened.StageEnrollment(ctx, newTicket, true)
	if err != nil || fresh.NodeToken == operation.NodeToken || fresh.PreviousNodeToken != operation.NodeToken {
		t.Fatalf("bounded journal prevented rotation: %v", err)
	}
	if err := reopened.ConfirmEnrollment(ctx, fresh.NodeToken, "node"); err != nil {
		t.Fatal(err)
	}
	if _, _, exists, err := reopened.LookupEnrollment(ctx, ticket); err != nil || exists {
		t.Fatalf("obsolete secret operation was not pruned: exists=%v err=%v", exists, err)
	}
}
