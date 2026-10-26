package enrollment_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
)

func setup(t *testing.T, capacity int) (*enrollment.Service, *nodes.Service, *sql.DB, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	n := nodes.NewService(db, nodes.Options{MaxNodes: capacity, Now: clock, Random: rand.NewChaCha8([32]byte{11})})
	s := enrollment.NewService(db, enrollment.Options{MaxNodes: capacity, Now: clock, Random: rand.NewChaCha8([32]byte{12})})
	return s, n, db, &now
}

func token256(t *testing.T, token string) {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		decoded, err = hex.DecodeString(token)
	}
	if err != nil || len(decoded) != 32 {
		t.Fatalf("token is not 256 bits: length=%d", len(token))
	}
}

func nodeSecret(n int) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{byte(n)}, 32))
}
func claim(installation string, n int) v1.EnrollmentRequest {
	return v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: installation, NodeToken: nodeSecret(n)}
}

func TestSingleTicketBindsPrebuiltNodeAndStoresOnlyHash(t *testing.T) {
	s, n, db, now := setup(t, 2)
	ctx := context.Background()
	pending, err := n.CreatePending(ctx, nodes.CreateRequest{Name: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateSingle(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	token256(t, created.Token)
	if created.NodeID == nil || *created.NodeID != pending.ID || created.GroupID != nil || created.MaxUses != 1 || !created.ExpiresAt.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("single binding/limits = %+v", created)
	}
	var stored []byte
	if err := db.QueryRow("SELECT token_hash FROM enrollment_tokens WHERE id=?", created.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(created.Token))
	if !bytes.Equal(stored, hash[:]) {
		t.Fatal("enrollment token not SHA-256 hash-only persisted")
	}
	read, err := s.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), created.Token) || strings.Contains(string(encoded), "token_hash") {
		t.Fatal("ticket read leaks credential")
	}
	result, err := s.Enroll(ctx, created.Token, claim("installation-a", 1))
	if err != nil {
		t.Fatal(err)
	}
	if result.NodeID != pending.ID {
		t.Fatalf("claim selected %s, want prebuilt %s", result.NodeID, pending.ID)
	}
	var nodeHash []byte
	var installation string
	if err := db.QueryRow("SELECT token_hash,installation_id FROM nodes WHERE id=?", pending.ID).Scan(&nodeHash, &installation); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(nodeSecret(1)))
	if !bytes.Equal(nodeHash, want[:]) || installation != "installation-a" {
		t.Fatal("node identity not bound/hash persisted")
	}
	if _, err := s.Enroll(ctx, created.Token, claim("installation-b", 2)); !errors.Is(err, enrollment.ErrConflict) {
		t.Fatalf("second single claim = %v", err)
	}
}

func TestBatchTicketGroupExpiryAndHundredUseLimit(t *testing.T) {
	s, _, db, now := setup(t, 101)
	ctx := context.Background()
	const group = "group-batch"
	if _, err := db.Exec("INSERT INTO groups(id,name) VALUES(?,?)", group, "batch"); err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateBatch(ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	token256(t, created.Token)
	if created.GroupID == nil || *created.GroupID != group || created.NodeID != nil || created.MaxUses != 100 || !created.ExpiresAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("batch binding/limits = %+v", created)
	}
	for i := 1; i <= 100; i++ {
		result, err := s.Enroll(ctx, created.Token, claim(fmt.Sprintf("installation-%d", i), i))
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		var got string
		if err := db.QueryRow("SELECT group_id FROM nodes WHERE id=?", result.NodeID).Scan(&got); err != nil || got != group {
			t.Fatalf("claim group=%q err=%v", got, err)
		}
	}
	if _, err := s.Enroll(ctx, created.Token, claim("over-limit", 101)); err == nil {
		t.Fatal("101st batch claim accepted")
	}
	var uses, count int
	if err := db.QueryRow("SELECT uses FROM enrollment_tokens WHERE id=?", created.ID).Scan(&uses); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM nodes").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if uses != 100 || count != 100 {
		t.Fatalf("uses=%d nodes=%d", uses, count)
	}
}

func TestEnrollmentRetryWindowExpiryRevocationAndConflicts(t *testing.T) {
	s, n, db, now := setup(t, 4)
	ctx := context.Background()
	pending, err := n.CreatePending(ctx, nodes.CreateRequest{Name: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := s.CreateSingle(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := claim("original-installation", 1)
	first, err := s.Enroll(ctx, ticket.Token, request)
	if err != nil {
		t.Fatal(err)
	}
	claimedAt := *now
	for _, offset := range []time.Duration{16 * time.Minute, 24 * time.Hour} {
		*now = claimedAt.Add(offset)
		retry, err := s.Enroll(ctx, ticket.Token, request)
		if err != nil || retry.NodeID != first.NodeID {
			t.Fatalf("exact retry after %s: %+v %v", offset, retry, err)
		}
	}
	*now = claimedAt.Add(24*time.Hour + time.Nanosecond)
	if _, err := s.Enroll(ctx, ticket.Token, request); err == nil {
		t.Fatal("retry after 24h accepted")
	}
	*now = claimedAt.Add(time.Minute)
	for _, req := range []v1.EnrollmentRequest{claim("original-installation", 2), claim("other-installation", 1)} {
		if _, err := s.Enroll(ctx, ticket.Token, req); !errors.Is(err, enrollment.ErrConflict) {
			t.Fatalf("conflicting identity = %v", err)
		}
	}
	if err := s.Revoke(ctx, ticket.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, ticket.ID); err != nil {
		t.Fatalf("revoke not idempotent: %v", err)
	}
	if _, err := s.Enroll(ctx, ticket.Token, request); !errors.Is(err, enrollment.ErrRevoked) {
		t.Fatalf("revoked exact retry = %v", err)
	}
	var uses, claims int
	if err := db.QueryRow("SELECT uses FROM enrollment_tokens WHERE id=?", ticket.ID).Scan(&uses); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM enrollment_claims WHERE enrollment_id=?", ticket.ID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if uses != 1 || claims != 1 {
		t.Fatalf("retry consumed quota: uses=%d claims=%d", uses, claims)
	}
}

func TestFirstClaimExpiresAtTicketDeadline(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			s, n, db, now := setup(t, 3)
			ctx := context.Background()
			var token string
			var expires time.Time
			if batch {
				if _, err := db.Exec("INSERT INTO groups(id,name) VALUES('g','g')"); err != nil {
					t.Fatal(err)
				}
				created, err := s.CreateBatch(ctx, "g")
				if err != nil {
					t.Fatal(err)
				}
				token, expires = created.Token, created.ExpiresAt
			} else {
				pending, err := n.CreatePending(ctx, nodes.CreateRequest{Name: "pending"})
				if err != nil {
					t.Fatal(err)
				}
				created, err := s.CreateSingle(ctx, pending.ID)
				if err != nil {
					t.Fatal(err)
				}
				token, expires = created.Token, created.ExpiresAt
			}
			*now = expires
			if _, err := s.Enroll(ctx, token, claim("too-late", 1)); !errors.Is(err, enrollment.ErrExpired) {
				t.Fatalf("expired first claim = %v", err)
			}
		})
	}
}

func TestBatchCapacityIsTransactionalAndDisabledNodesCount(t *testing.T) {
	s, n, db, _ := setup(t, 2)
	ctx := context.Background()
	pending, err := n.CreatePending(ctx, nodes.CreateRequest{Name: "disabled still counts"})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.SetDisabled(ctx, pending.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO groups(id,name) VALUES('g','g')"); err != nil {
		t.Fatal(err)
	}
	ticket, err := s.CreateBatch(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 1; i <= 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.Enroll(ctx, ticket.Token, claim(fmt.Sprint(i), i))
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, full := 0, 0
	for err := range results {
		if err == nil {
			accepted++
		} else if errors.Is(err, nodes.ErrCapacityReached) {
			full++
		} else {
			t.Fatalf("unexpected race error: %v", err)
		}
	}
	var count, uses, claims int
	if err := db.QueryRow("SELECT count(*) FROM nodes").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT uses FROM enrollment_tokens WHERE id=?", ticket.ID).Scan(&uses); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM enrollment_claims WHERE enrollment_id=?", ticket.ID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if accepted != 1 || full != 1 || count != 2 || uses != 1 || claims != 1 {
		t.Fatalf("not atomic: accepted=%d full=%d nodes=%d uses=%d claims=%d", accepted, full, count, uses, claims)
	}
}
