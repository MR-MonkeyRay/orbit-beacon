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
	n := nodes.NewService(stableDatabase(t, db), nodes.Options{MaxNodes: capacity, Now: clock, Random: rand.NewChaCha8([32]byte{11})})
	s := enrollment.NewService(stableDatabase(t, db), enrollment.Options{MaxNodes: capacity, Now: clock, Random: rand.NewChaCha8([32]byte{12})})
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
	if created.NodeID == nil || *created.NodeID != pending.ID || created.GroupID != nil || created.MaxUses != 1 || !created.ExpiresAt.Equal(now.Add(30*time.Minute)) {
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
	if _, err := s.Enroll(ctx, created.Token, claim("installation-b", 2)); !errors.Is(err, enrollment.ErrInvalidTicket) {
		t.Fatalf("second single claim = %v", err)
	}
}

func TestGroupTicketBindingAndLimits(t *testing.T) {
	s, _, db, now := setup(t, 2)
	ctx := context.Background()
	const group = "group-deploy"
	if _, err := db.Exec("INSERT INTO groups(id,name) VALUES(?,?)", group, "deploy"); err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateGroup(ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	token256(t, created.Token)
	if created.GroupID == nil || *created.GroupID != group || created.NodeID != nil || created.MaxUses != 1 || !created.ExpiresAt.Equal(now.Add(30*time.Minute)) {
		t.Fatal("invalid group issuance limits")
	}
	result, err := s.Enroll(ctx, created.Token, claim("group-installation", 1))
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := db.QueryRow("SELECT group_id FROM nodes WHERE id=?", result.NodeID).Scan(&got); err != nil || got != group {
		t.Fatalf("claim group=%q err=%v", got, err)
	}
}

func TestConsumedTicketRejectsAllReplays(t *testing.T) {
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
	if _, err := s.Enroll(ctx, ticket.Token, request); err != nil {
		t.Fatal(err)
	}
	claimedAt := *now
	for _, offset := range []time.Duration{0, 16 * time.Minute, 30 * time.Minute, 24*time.Hour + time.Nanosecond} {
		*now = claimedAt.Add(offset)
		for _, req := range []v1.EnrollmentRequest{request, claim("original-installation", 2), claim("other-installation", 1), claim("other-installation", 2)} {
			if _, err := s.Enroll(ctx, ticket.Token, req); !errors.Is(err, enrollment.ErrInvalidTicket) {
				t.Fatalf("consumed ticket retry after %s = %v", offset, err)
			}
		}
	}
	*now = claimedAt
	for range 2 {
		if err := s.Revoke(ctx, ticket.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Enroll(ctx, ticket.Token, request); !errors.Is(err, enrollment.ErrInvalidTicket) {
		t.Fatalf("revoked exact retry = %v", err)
	}
	assertSingleClaim(t, stableDatabase(t, db), ticket.ID)
	if _, err := db.Exec("DELETE FROM enrollment_tokens WHERE id=?", ticket.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll(ctx, ticket.Token, request); !errors.Is(err, enrollment.ErrInvalidTicket) {
		t.Fatalf("deleted ticket retry = %v", err)
	}
}

func TestFirstClaimExpiresAtTicketDeadline(t *testing.T) {
	for _, group := range []bool{false, true} {
		for _, offset := range []time.Duration{30*time.Minute - time.Nanosecond, 30 * time.Minute} {
			t.Run(fmt.Sprintf("group=%t/offset=%s", group, offset), func(t *testing.T) {
				s, n, db, now := setup(t, 3)
				ctx := context.Background()
				ticket := createTargetTicket(t, s, n, db, group)
				*now = now.Add(offset)
				_, err := s.Enroll(ctx, ticket.Token, claim("deadline", 1))
				if offset < 30*time.Minute {
					if err != nil {
						t.Fatalf("last nanosecond rejected: %v", err)
					}
				} else if !errors.Is(err, enrollment.ErrInvalidTicket) {
					t.Fatalf("deadline first claim = %v", err)
				}
			})
		}
	}
}

func TestGroupCapacityIsTransactionalAndDisabledNodesCount(t *testing.T) {
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
	tickets := []enrollment.CreatedTicket{createTargetTicket(t, s, n, db, true), createTargetTicket(t, s, n, db, true)}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 1; i <= 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.Enroll(ctx, tickets[i-1].Token, claim(fmt.Sprint(i), i))
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
	if err := db.QueryRow("SELECT sum(uses) FROM enrollment_tokens").Scan(&uses); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM enrollment_claims").Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if accepted != 1 || full != 1 || count != 2 || uses != 1 || claims != 1 {
		t.Fatalf("not atomic: accepted=%d full=%d nodes=%d uses=%d claims=%d", accepted, full, count, uses, claims)
	}
	// 容量失败必须保留原票，释放名额后可首次激活。
	var unused enrollment.CreatedTicket
	for _, ticket := range tickets {
		read, err := s.Get(ctx, ticket.ID)
		if err != nil {
			t.Fatal(err)
		}
		if read.Uses == 0 {
			unused = ticket
		}
	}
	if unused.Token == "" {
		t.Fatal("capacity failure consumed both tickets")
	}
	if err := n.Delete(ctx, pending.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll(ctx, unused.Token, claim("capacity-retry", 3)); err != nil {
		t.Fatalf("capacity failure left ticket unusable: %v", err)
	}
	assertSingleClaim(t, stableDatabase(t, db), unused.ID)
}

func TestDeletedGroupClaimCannotRecreateNode(t *testing.T) {
	s, n, db, _ := setup(t, 3)
	ctx := context.Background()
	if _, err := db.Exec("INSERT INTO groups(id,name) VALUES('g','g')"); err != nil {
		t.Fatal(err)
	}
	ticket, err := s.CreateGroup(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	request := claim("deleted-installation", 1)
	first, err := s.Enroll(ctx, ticket.Token, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Delete(ctx, first.NodeID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll(ctx, ticket.Token, request); !errors.Is(err, enrollment.ErrInvalidTicket) {
		t.Fatalf("deleted claim retry = %v, want invalid ticket", err)
	}
	if _, err := n.AuthenticateToken(ctx, request.NodeToken); !errors.Is(err, nodes.ErrUnauthenticated) {
		t.Fatalf("deleted credential = %v, want unauthenticated", err)
	}
	read, err := s.Get(ctx, ticket.ID)
	if err != nil || read.Uses != 1 {
		t.Fatalf("deleted retry consumed quota: %+v %v", read, err)
	}
	if _, err := s.Enroll(ctx, ticket.Token, claim("other-installation", 2)); !errors.Is(err, enrollment.ErrInvalidTicket) {
		t.Fatalf("deleted claim reused by another installation: %v", err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM nodes").Scan(&count); err != nil || count != 0 {
		t.Fatalf("nodes=%d err=%v, want no recreated nodes", count, err)
	}
}

var fixtureDatabases sync.Map

func stableDatabase(t *testing.T, db *sql.DB) *storage.Database {
	t.Helper()
	if stable, ok := fixtureDatabases.Load(db); ok {
		return stable.(*storage.Database)
	}
	var path string
	if err := db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	stable, err := storage.OpenMain(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	fixtureDatabases.Store(db, stable)
	t.Cleanup(func() {
		fixtureDatabases.Delete(db)
		_ = stable.Close()
	})
	return stable
}

func createTargetTicket(t *testing.T, s *enrollment.Service, n *nodes.Service, db *sql.DB, group bool) enrollment.CreatedTicket {
	t.Helper()
	ctx := context.Background()
	var ticket enrollment.CreatedTicket
	var err error
	if group {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM `groups` WHERE id='g'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			if _, err := db.Exec("INSERT INTO `groups`(id,name) VALUES('g','g')"); err != nil {
				t.Fatal(err)
			}
		}
		ticket, err = s.CreateGroup(ctx, "g")
	} else {
		pending, createErr := n.CreatePending(ctx, nodes.CreateRequest{Name: "pending"})
		if createErr != nil {
			t.Fatal(createErr)
		}
		ticket, err = s.CreateSingle(ctx, pending.ID)
	}
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func assertSingleClaim(t *testing.T, db *storage.Database, ticketID string) {
	t.Helper()
	ctx := context.Background()
	lease, err := db.Acquire(ctx, storage.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var uses, claims, bound, audits int
	if err := lease.QueryRowContext(ctx, `SELECT uses,
 (SELECT count(*) FROM enrollment_claims WHERE enrollment_id=t.id),
 (SELECT count(*) FROM nodes n JOIN enrollment_claims c ON c.node_id=n.id AND c.token_hash=n.token_hash WHERE c.enrollment_id=t.id),
 (SELECT count(*) FROM audit_events WHERE action='enrollment.claim' AND target_id=(SELECT node_id FROM enrollment_claims WHERE enrollment_id=t.id))
 FROM enrollment_tokens t WHERE id=?`, ticketID).Scan(&uses, &claims, &bound, &audits); err != nil {
		t.Fatal(err)
	}
	if uses != 1 || claims != 1 || bound != 1 || audits != 1 {
		t.Fatalf("single claim not atomic: uses=%d claims=%d bound=%d audits=%d", uses, claims, bound, audits)
	}
}

func raceSingleTicket(t *testing.T, services [2]*enrollment.Service, db *storage.Database, ticket enrollment.CreatedTicket, sameIdentity bool) {
	t.Helper()
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, service := range services {
		request := claim("same-installation", 1)
		if !sameIdentity {
			request = claim(fmt.Sprintf("installation-%d", i), i+1)
		}
		go func() {
			<-start
			_, err := service.Enroll(context.Background(), ticket.Token, request)
			results <- err
		}()
	}
	close(start)
	accepted, rejected := 0, 0
	for range services {
		err := <-results
		if err == nil {
			accepted++
		} else if errors.Is(err, enrollment.ErrInvalidTicket) {
			rejected++
		} else {
			t.Errorf("concurrent claim=%v", err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("concurrent claim: accepted=%d rejected=%d", accepted, rejected)
	}
	assertSingleClaim(t, db, ticket.ID)
}

func TestSingleUseTicketConcurrentClaims(t *testing.T) {
	for _, group := range []bool{false, true} {
		for _, sameIdentity := range []bool{false, true} {
			t.Run(fmt.Sprintf("group=%t/same=%t", group, sameIdentity), func(t *testing.T) {
				s, n, raw, now := setup(t, 10)
				var path string
				if err := raw.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
					t.Fatal(err)
				}
				other, err := storage.OpenMain(context.Background(), path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = other.Close() })
				second := enrollment.NewService(other, enrollment.Options{Now: func() time.Time { return *now }})
				ticket := createTargetTicket(t, s, n, raw, group)
				raceSingleTicket(t, [2]*enrollment.Service{s, second}, stableDatabase(t, raw), ticket, sameIdentity)
			})
		}
	}
}

func historicalTickets(t *testing.T, s *enrollment.Service, n *nodes.Service, db *sql.DB, stable *storage.Database) {
	t.Helper()
	ctx := context.Background()
	for _, group := range []bool{false, true} {
		for _, uses := range []int{0, 1, 99, 100} {
			t.Run(fmt.Sprintf("group=%t/uses=%d", group, uses), func(t *testing.T) {
				ticket := createTargetTicket(t, s, n, db, group)
				var originalExpiry string
				if err := db.QueryRow("SELECT expires_at FROM enrollment_tokens WHERE id=?", ticket.ID).Scan(&originalExpiry); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("UPDATE enrollment_tokens SET max_uses=100,uses=? WHERE id=?", uses, ticket.ID); err != nil {
					t.Fatal(err)
				}
				read, err := s.Get(ctx, ticket.ID)
				if err != nil || read.MaxUses != 1 || read.Uses != uses || !read.ExpiresAt.Equal(ticket.ExpiresAt) {
					t.Fatalf("historical ticket=%+v err=%v", read, err)
				}
				listed, err := s.List(ctx, 100, 0)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, item := range listed {
					if item.ID == ticket.ID {
						found = true
						if item.MaxUses != 1 || item.Uses != uses || !item.ExpiresAt.Equal(ticket.ExpiresAt) {
							t.Fatalf("historical list=%+v", item)
						}
					}
				}
				if !found {
					t.Fatal("historical ticket missing from list")
				}
				secret := uses + 1
				if group {
					secret += 101
				}
				request := claim(fmt.Sprintf("historical-%t-%d", group, uses), secret)
				check := v1.EnrollmentCheckRequest{ProtocolVersion: v1.EnrollmentCheckVersion, InstallationID: request.InstallationID, NodeToken: request.NodeToken}
				wantErr := error(enrollment.ErrInvalidTicket)
				if uses == 0 {
					wantErr = nil
				}
				if err := s.AdmitToken(ctx, ticket.Token, "127.0.0.1"); !errors.Is(err, wantErr) {
					t.Fatalf("historical admission=%v want=%v", err, wantErr)
				}
				if _, err := s.CheckClaim(ctx, ticket.Token, check); !errors.Is(err, wantErr) {
					t.Fatalf("historical check=%v want=%v", err, wantErr)
				}
				if _, err := s.Enroll(ctx, ticket.Token, request); !errors.Is(err, wantErr) {
					t.Fatalf("historical claim=%v want=%v", err, wantErr)
				}
				if uses == 0 {
					if _, err := s.Enroll(ctx, ticket.Token, claim("different-installation", 200)); !errors.Is(err, enrollment.ErrInvalidTicket) {
						t.Fatalf("historical multi-use replay=%v", err)
					}
					assertSingleClaim(t, stable, ticket.ID)
				}
				var storedMax, storedUses int
				var storedExpiry string
				if err := db.QueryRow("SELECT max_uses,uses,expires_at FROM enrollment_tokens WHERE id=?", ticket.ID).Scan(&storedMax, &storedUses, &storedExpiry); err != nil {
					t.Fatal(err)
				}
				wantUses := uses
				if uses == 0 {
					wantUses = 1
				}
				if storedMax != 100 || storedUses != wantUses || storedExpiry != originalExpiry {
					t.Fatalf("historical storage rewritten: max=%d uses=%d expiry changed=%t", storedMax, storedUses, storedExpiry != originalExpiry)
				}
			})
		}
	}
}

func TestHistoricalMultiUseTicketsAreSingleUse(t *testing.T) {
	s, n, db, _ := setup(t, 20)
	historicalTickets(t, s, n, db, stableDatabase(t, db))
}

func TestClaimAuditFailureRollsBackAndPreservesTicket(t *testing.T) {
	for _, group := range []bool{false, true} {
		t.Run(fmt.Sprint(group), func(t *testing.T) {
			s, n, db, _ := setup(t, 4)
			ctx := context.Background()
			ticket := createTargetTicket(t, s, n, db, group)
			var countBefore int
			if err := db.QueryRow("SELECT count(*) FROM nodes").Scan(&countBefore); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TRIGGER reject_claim BEFORE INSERT ON audit_events WHEN NEW.action='enrollment.claim' BEGIN SELECT RAISE(ABORT,'test rollback'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Enroll(ctx, ticket.Token, claim("installation", 1)); err == nil || errors.Is(err, enrollment.ErrInvalidTicket) {
				t.Fatalf("audit failure=%v", err)
			}
			var uses, claims, bound, audits, nodeCount int
			if err := db.QueryRow(`SELECT uses,
 (SELECT count(*) FROM enrollment_claims),
 (SELECT count(*) FROM nodes WHERE token_hash IS NOT NULL OR installation_id IS NOT NULL OR credential_epoch<>0),
 (SELECT count(*) FROM audit_events WHERE action='enrollment.claim'),
 (SELECT count(*) FROM nodes)
 FROM enrollment_tokens WHERE id=?`, ticket.ID).Scan(&uses, &claims, &bound, &audits, &nodeCount); err != nil {
				t.Fatal(err)
			}
			if uses != 0 || claims != 0 || bound != 0 || audits != 0 || nodeCount != countBefore {
				t.Fatalf("audit failure committed: uses=%d claims=%d bound=%d audits=%d nodes=%d", uses, claims, bound, audits, nodeCount)
			}
			if _, err := db.Exec("DROP TRIGGER reject_claim"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Enroll(ctx, ticket.Token, claim("installation", 1)); err != nil {
				t.Fatalf("rolled back ticket unusable: %v", err)
			}
			assertSingleClaim(t, stableDatabase(t, db), ticket.ID)
		})
	}
}

func TestUnconsumedTicketPreservesIdentityConflictsAndCAS(t *testing.T) {
	s, n, db, _ := setup(t, 4)
	ctx := context.Background()
	initial := createTargetTicket(t, s, n, db, false)
	if _, err := s.Enroll(ctx, initial.Token, claim("installation", 1)); err != nil {
		t.Fatal(err)
	}
	rotation, err := s.CreateSingle(ctx, *initial.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []v1.EnrollmentRequest{claim("other-installation", 2), claim("installation", 2), func() v1.EnrollmentRequest {
		request := claim("installation", 2)
		request.PreviousNodeToken = nodeSecret(3)
		return request
	}()} {
		if _, err := s.Enroll(ctx, rotation.Token, request); !errors.Is(err, enrollment.ErrConflict) {
			t.Fatalf("real identity conflict=%v", err)
		}
	}
	read, err := s.Get(ctx, rotation.ID)
	if err != nil || read.Uses != 0 {
		t.Fatalf("identity conflict consumed ticket: %+v err=%v", read, err)
	}
	request := claim("installation", 2)
	request.PreviousNodeToken = nodeSecret(1)
	if _, err := s.Enroll(ctx, rotation.Token, request); err != nil {
		t.Fatalf("correct CAS rejected: %v", err)
	}
	var epoch int
	if err := db.QueryRow("SELECT credential_epoch FROM nodes WHERE id=?", *initial.NodeID).Scan(&epoch); err != nil || epoch != 2 {
		t.Fatalf("rotation epoch=%d err=%v", epoch, err)
	}
	if _, err := n.AuthenticateToken(ctx, nodeSecret(1)); !errors.Is(err, nodes.ErrUnauthenticated) {
		t.Fatalf("old credential still active: %v", err)
	}
	if node, err := n.AuthenticateToken(ctx, nodeSecret(2)); err != nil || node.ID != *initial.NodeID {
		t.Fatalf("new credential inactive: node=%+v err=%v", node, err)
	}
}

func TestInvalidTicketBudgetsNeverBlockUsableTickets(t *testing.T) {
	s, n, db, _ := setup(t, 2)
	ctx := context.Background()
	ticket := createTargetTicket(t, s, n, db, false)
	for i := range 32 {
		if err := s.AdmitToken(ctx, "invalid", "127.0.0.1:1234"); !errors.Is(err, enrollment.ErrInvalidTicket) {
			t.Fatalf("invalid budget %d=%v", i, err)
		}
	}
	if err := s.AdmitToken(ctx, "invalid", "127.0.0.1:1234"); !errors.Is(err, enrollment.ErrRateLimited) {
		t.Fatalf("exhausted IP budget=%v", err)
	}
	for i := range 224 {
		if err := s.AdmitToken(ctx, "invalid", fmt.Sprintf("10.0.%d.%d:1234", i/250, i%250+1)); !errors.Is(err, enrollment.ErrInvalidTicket) {
			t.Fatalf("global invalid budget %d=%v", i, err)
		}
	}
	if err := s.AdmitToken(ctx, "invalid", "192.0.2.1:1234"); !errors.Is(err, enrollment.ErrRateLimited) {
		t.Fatalf("exhausted global budget=%v", err)
	}
	if err := s.AdmitToken(ctx, ticket.Token, "127.0.0.1:1234"); err != nil {
		t.Fatalf("invalid traffic blocked usable ticket: %v", err)
	}
}

func TestUnconsumedTicketPreservesGlobalCredentialCollisions(t *testing.T) {
	s, n, db, _ := setup(t, 4)
	ctx := context.Background()
	initial := createTargetTicket(t, s, n, db, true)
	if _, err := s.Enroll(ctx, initial.Token, claim("bound-installation", 1)); err != nil {
		t.Fatal(err)
	}
	ticket := createTargetTicket(t, s, n, db, true)
	for _, request := range []v1.EnrollmentRequest{claim("bound-installation", 2), claim("new-installation", 1)} {
		if _, err := s.Enroll(ctx, ticket.Token, request); !errors.Is(err, enrollment.ErrConflict) {
			t.Fatalf("global credential collision=%v", err)
		}
	}
	read, err := s.Get(ctx, ticket.ID)
	if err != nil || read.Uses != 0 {
		t.Fatalf("collision consumed ticket: %+v err=%v", read, err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM nodes").Scan(&count); err != nil || count != 1 {
		t.Fatalf("collision created node: count=%d err=%v", count, err)
	}
	if _, err := s.Enroll(ctx, ticket.Token, claim("new-installation", 2)); err != nil {
		t.Fatalf("collision rollback left ticket unusable: %v", err)
	}
	assertSingleClaim(t, stableDatabase(t, db), ticket.ID)
}
