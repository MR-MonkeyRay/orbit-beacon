package enrollment_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/nodes"
)

func TestClaimInspectionIsReadOnlyAndNeverAuthorizesReceipts(t *testing.T) {
	s, n, db, now := setup(t, 1)
	ctx := context.Background()
	pending, err := n.CreatePending(ctx, nodes.CreateRequest{Name: "claim check"})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := s.CreateSingle(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := v1.EnrollmentCheckRequest{ProtocolVersion: v1.EnrollmentCheckVersion, InstallationID: "installation", NodeToken: nodeSecret(1)}
	writer, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	inspect := func(token string, request v1.EnrollmentCheckRequest, state string, wantErr error) {
		t.Helper()
		// 检查不能等待写事务，即使票据已消费。
		if _, err := writer.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		defer writer.ExecContext(ctx, "ROLLBACK")
		readCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		response, err := s.CheckClaim(readCtx, token, request)
		if !errors.Is(err, wantErr) {
			t.Fatalf("inspection error=%v want=%v", err, wantErr)
		}
		if err == nil && (response.Claimed || response.NodeID != "" || response.CredentialState != state || !response.ServerTime.Equal(*now)) {
			t.Fatalf("inspection=%+v want state=%s", response, state)
		}
	}
	inspect(ticket.Token, request, "unbound", nil)
	var uses, claims int
	if err := db.QueryRow("SELECT uses FROM enrollment_tokens WHERE id=?", ticket.ID).Scan(&uses); err != nil || uses != 0 {
		t.Fatalf("inspection consumed ticket: uses=%d err=%v", uses, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM enrollment_claims").Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("inspection created claim: claims=%d err=%v", claims, err)
	}
	if _, err := s.Enroll(ctx, ticket.Token, claim("installation", 1)); err != nil {
		t.Fatal(err)
	}
	claimedAt := *now
	for _, offset := range []time.Duration{0, 16 * time.Minute, 30 * time.Minute, 24*time.Hour + time.Nanosecond} {
		*now = claimedAt.Add(offset)
		inspect(ticket.Token, request, "", enrollment.ErrInvalidTicket)
	}
	*now = claimedAt
	wrong := request
	wrong.NodeToken = nodeSecret(2)
	inspect(ticket.Token, wrong, "", enrollment.ErrInvalidTicket)
	fresh, err := s.CreateSingle(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	inspect(fresh.Token, request, "active", nil)
	inspect(fresh.Token, wrong, "unbound", nil)
	if err := db.QueryRow("SELECT uses FROM enrollment_tokens WHERE id=?", fresh.ID).Scan(&uses); err != nil || uses != 0 {
		t.Fatalf("active inspection consumed ticket: uses=%d err=%v", uses, err)
	}
	var epoch int
	var generation, session string
	var seen any
	if err := db.QueryRow("SELECT credential_epoch,run_generation,report_session_id,last_seen FROM nodes WHERE id=?", pending.ID).Scan(&epoch, &generation, &session, &seen); err != nil || epoch != 1 || generation != "0" || session != "" || seen != nil {
		t.Fatalf("inspection changed node activity: epoch=%d generation=%s session=%s seen=%v err=%v", epoch, generation, session, seen, err)
	}
	if err := s.Revoke(ctx, ticket.ID); err != nil {
		t.Fatal(err)
	}
	inspect(ticket.Token, request, "", enrollment.ErrInvalidTicket)
	assertSingleClaim(t, stableDatabase(t, db), ticket.ID)
	if _, err := db.Exec("DELETE FROM nodes WHERE id=?", pending.ID); err != nil {
		t.Fatal(err)
	}
	inspect(ticket.Token, request, "", enrollment.ErrInvalidTicket)
}

func TestUnavailableTicketsRejectAdmissionCheckAndClaim(t *testing.T) {
	for _, state := range []string{"missing", "long", "unknown", "expired", "revoked", "consumed-after-check", "deleted"} {
		t.Run(state, func(t *testing.T) {
			s, n, db, now := setup(t, 2)
			ctx := context.Background()
			ticket := createTargetTicket(t, s, n, db, false)
			request := v1.EnrollmentCheckRequest{ProtocolVersion: v1.EnrollmentCheckVersion, InstallationID: "installation", NodeToken: nodeSecret(1)}
			token := ticket.Token
			switch state {
			case "missing":
				token = ""
			case "long":
				token = strings.Repeat("x", 257)
			case "unknown":
				token = "unknown"
			case "expired":
				*now = ticket.ExpiresAt
			case "revoked":
				if err := s.Revoke(ctx, ticket.ID); err != nil {
					t.Fatal(err)
				}
			case "consumed-after-check":
				if _, err := s.CheckClaim(ctx, token, request); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Enroll(ctx, token, claim("installation", 1)); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if _, err := db.Exec("DELETE FROM enrollment_tokens WHERE id=?", ticket.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.AdmitToken(ctx, token, "127.0.0.1"); !errors.Is(err, enrollment.ErrInvalidTicket) {
				t.Fatalf("admission=%v", err)
			}
			if _, err := s.CheckClaim(ctx, token, request); !errors.Is(err, enrollment.ErrInvalidTicket) {
				t.Fatalf("check=%v", err)
			}
			if _, err := s.Enroll(ctx, token, claim("installation", 1)); !errors.Is(err, enrollment.ErrInvalidTicket) {
				t.Fatalf("claim=%v", err)
			}
		})
	}
}
