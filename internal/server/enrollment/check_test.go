package enrollment_test

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/nodes"
)

func TestClaimInspectionIsReadOnlyAndHonorsExactRetryBoundaries(t *testing.T) {
	s, n, db, now := setup(t, 1)
	ctx := context.Background()
	pending, err := n.CreatePending(ctx, nodes.CreateRequest{Name: "receipt check"})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := s.CreateSingle(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := v1.EnrollmentCheckRequest{ProtocolVersion: 1, InstallationID: "installation", NodeToken: nodeSecret(1)}
	// Pin a writer connection; both unclaimed and claimed inspection must remain
	// read-only and must not wait for SQLite writer admission.
	writer, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	inspect := func() v1.EnrollmentCheckResponse {
		t.Helper()
		if _, err := writer.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		defer writer.ExecContext(ctx, "ROLLBACK")
		readCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		response, err := s.CheckClaim(readCtx, ticket.Token, request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	if response := inspect(); response.Claimed || response.NodeID != "" {
		t.Fatalf("unclaimed inspection invented a receipt: %+v", response)
	}
	var uses, claims int
	if err := db.QueryRow("SELECT uses FROM enrollment_tokens WHERE id=?", ticket.ID).Scan(&uses); err != nil || uses != 0 {
		t.Fatalf("inspection consumed a ticket: uses=%d err=%v", uses, err)
	}
	if _, err := s.Enroll(ctx, ticket.Token, claim("installation", 1)); err != nil {
		t.Fatal(err)
	}
	claimedAt := *now
	for _, offset := range []time.Duration{0, 16 * time.Minute, 24 * time.Hour} {
		*now = claimedAt.Add(offset)
		if response := inspect(); !response.Claimed || response.NodeID != pending.ID {
			t.Fatalf("exact receipt missing at %s: %+v", offset, response)
		}
	}
	*now = claimedAt.Add(24*time.Hour + time.Nanosecond)
	if _, err := s.CheckClaim(ctx, ticket.Token, request); !errors.Is(err, enrollment.ErrExpired) {
		t.Fatalf("expired receipt inspection = %v", err)
	}
	*now = claimedAt
	wrong := request
	wrong.NodeToken = nodeSecret(2)
	if _, err := s.CheckClaim(ctx, ticket.Token, wrong); !errors.Is(err, enrollment.ErrConflict) {
		t.Fatalf("wrong receipt credential = %v", err)
	}
	if err := s.Revoke(ctx, ticket.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckClaim(ctx, ticket.Token, request); !errors.Is(err, enrollment.ErrRevoked) {
		t.Fatalf("revoked receipt inspection = %v", err)
	}
	if err := db.QueryRow("SELECT uses FROM enrollment_tokens WHERE id=?", ticket.ID).Scan(&uses); err != nil || uses != 1 {
		t.Fatalf("receipt inspection changed uses: uses=%d err=%v", uses, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM enrollment_claims").Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("receipt inspection changed receipts: claims=%d err=%v", claims, err)
	}
	if _, err := db.Exec("DELETE FROM nodes WHERE id=?", pending.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckClaim(ctx, ticket.Token, request); !errors.Is(err, enrollment.ErrUnauthenticated) {
		t.Fatalf("deleted node's cascaded single ticket inspection = %v", err)
	}
}
