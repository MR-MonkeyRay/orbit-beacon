package enrollment_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"testing"
	"time"

	"orbitbeacon/internal/config"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/enrollment"
	"orbitbeacon/internal/server/management"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/server/storagetest"
)

func TestMySQLEnrollmentClaimsQuotaAndGroupCascade(t *testing.T) {

	f := storagetest.Start(t)
	db := f.Open(t, storage.MainDomain)
	now := time.Date(2026, 11, 1, 2, 3, 4, 987654321, time.UTC)
	ctx := context.Background()
	m := management.NewService(db)
	group, err := m.SaveGroup(ctx, "", "Group")
	if err != nil {
		t.Fatal(err)
	}
	n := nodes.NewService(db, nodes.Options{MaxNodes: 2, Now: func() time.Time { return now }})
	s := enrollment.NewService(db, enrollment.Options{MaxNodes: 2, Now: func() time.Time { return now }})
	batch, err := s.CreateGroup(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	installation := strings.Repeat("界", 83) + "Aa\x00x  "
	if _, err := s.Enroll(ctx, batch.Token, claim(installation+"x", 1)); !errors.Is(err, enrollment.ErrInvalidInput) {
		t.Fatalf("256-byte installation accepted: %v", err)
	}
	first, err := s.Enroll(ctx, batch.Token, claim(installation, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll(ctx, batch.Token, claim(installation, 1)); !errors.Is(err, enrollment.ErrInvalidTicket) {
		t.Fatalf("consumed ticket retry=%v", err)
	}
	second, err := s.CreateGroup(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll(ctx, second.Token, claim(strings.Repeat("界", 83)+"aa\x00x  ", 2)); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.Recover(ctx, nodeSecret(1), v1.RecoveryRequest{InstallationID: installation})
	if err != nil || recovered.NodeID != first.NodeID {
		t.Fatalf("recovery=%+v err=%v", recovered, err)
	}
	if _, err := s.Recover(ctx, nodeSecret(1), v1.RecoveryRequest{InstallationID: installation + "x"}); !errors.Is(err, enrollment.ErrInvalidInput) {
		t.Fatalf("256-byte recovery accepted: %v", err)
	}
	lease, err := db.Acquire(ctx, storage.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var storedInstallation, claimedInstallation string
	if err := lease.QueryRowContext(ctx, "SELECT n.installation_id,c.installation_id FROM nodes n JOIN enrollment_claims c ON c.node_id=n.id WHERE n.id=?", first.NodeID).Scan(&storedInstallation, &claimedInstallation); err != nil || storedInstallation != installation || claimedInstallation != installation {
		t.Fatalf("installation bytes changed: node=%q claim=%q err=%v", storedInstallation, claimedInstallation, err)
	}
	full, err := s.CreateGroup(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enroll(ctx, full.Token, claim("mysql-c", 3)); !errors.Is(err, nodes.ErrCapacityReached) {
		t.Fatalf("node quota result=%v", err)
	}
	read, err := s.Get(ctx, full.ID)
	if err != nil || read.Uses != 0 {
		t.Fatalf("capacity failure consumed ticket: %+v err=%v", read, err)
	}
	node, err := n.Get(ctx, first.NodeID)
	if err != nil || node.GroupID == nil || *node.GroupID != group.ID {
		t.Fatalf("node=%+v err=%v", node, err)
	}
	if err := m.DeleteGroup(ctx, group.ID); err != nil {
		t.Fatal(err)
	}
	node, err = n.Get(ctx, first.NodeID)
	if err != nil || node.GroupID != nil {
		t.Fatalf("cascade=%+v err=%v", node, err)
	}
	now = now.Add(time.Hour)
	if _, err := s.CleanupExpiredTickets(ctx, now, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CleanupAuditEvents(ctx, now, 100); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLSingleUseTicketConcurrentClaims(t *testing.T) {
	for _, group := range []bool{false, true} {
		for _, sameIdentity := range []bool{false, true} {
			t.Run(fmt.Sprintf("group=%t/same=%t", group, sameIdentity), func(t *testing.T) {
				f := storagetest.Start(t)
				ctx := context.Background()
				cfg := config.DatabaseConfig{Primary: f.Endpoint("primary")}
				firstDir, secondDir := t.TempDir(), t.TempDir()
				for _, dir := range []string{firstDir, secondDir} {
					if err := os.Chmod(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				// 两个独立句柄及各自 writeGate；只能由数据库 guard 行锁串行化。
				first, err := storage.OpenDatabases(ctx, cfg, firstDir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = first.Close() })
				second, err := storage.OpenDatabases(ctx, cfg, secondDir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = second.Close() })
				now := time.Date(2026, 11, 1, 2, 3, 4, 987654321, time.UTC)
				opts := enrollment.Options{Now: func() time.Time { return now }}
				s := enrollment.NewService(first.Main, opts)
				n := nodes.NewService(first.Main, nodes.Options{Now: opts.Now})
				ticket := createTargetTicket(t, s, n, f.RawEndpoint(t, "primary", storage.MainDomain), group)
				raceSingleTicket(t, [2]*enrollment.Service{s, enrollment.NewService(second.Main, opts)}, first.Main, ticket, sameIdentity)
			})
		}
	}
}

func TestMySQLHistoricalMultiUseTicketsAreSingleUse(t *testing.T) {
	f := storagetest.Start(t)
	db := f.Open(t, storage.MainDomain)
	now := time.Date(2026, 11, 1, 2, 3, 4, 987654321, time.UTC)
	s := enrollment.NewService(db, enrollment.Options{Now: func() time.Time { return now }})
	n := nodes.NewService(db, nodes.Options{Now: func() time.Time { return now }})
	historicalTickets(t, s, n, f.Raw(t, storage.MainDomain), db)
}
