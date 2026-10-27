package enrollment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"orbitbeacon/internal/identity"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/audit"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/timekey"
)

type serviceError string

func (err serviceError) Error() string { return string(err) }

const (
	ErrUnauthenticated serviceError = "enrollment authentication required"
	ErrConflict        serviceError = "enrollment identity conflict"
	ErrRevoked         serviceError = "enrollment ticket revoked"
	ErrExpired         serviceError = "enrollment ticket expired"
	ErrNotFound        serviceError = "enrollment ticket not found"
	ErrInvalidInput    serviceError = "invalid enrollment request"
	ErrRateLimited     serviceError = "enrollment rate limited"
)

type Options struct {
	MaxNodes int
	Now      func() time.Time
	Random   io.Reader
}

type Service struct {
	db            *sql.DB
	maxNodes      int
	now           func() time.Time
	random        io.Reader
	randomMu      sync.Mutex
	admissionMu   sync.Mutex
	invalidPeers  map[string]invalidBucket
	invalidGlobal invalidBucket
}

func NewService(db *sql.DB, opts Options) *Service {
	if opts.MaxNodes <= 0 {
		opts.MaxNodes = 500
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Random == nil {
		opts.Random = rand.Reader
	}
	return &Service{db: db, maxNodes: opts.MaxNodes, now: opts.Now, random: opts.Random, invalidPeers: make(map[string]invalidBucket)}
}

// Ticket deliberately contains neither the enrollment secret nor its hash.
type Ticket struct {
	ID        string     `json:"id"`
	NodeID    *string    `json:"node_id"`
	GroupID   *string    `json:"group_id"`
	ExpiresAt time.Time  `json:"expires_at"`
	MaxUses   int        `json:"max_uses"`
	Uses      int        `json:"uses"`
	RevokedAt *time.Time `json:"revoked_at"`
}

// Only the creation result exposes the plaintext ticket, once.
type CreatedTicket struct {
	Ticket
	Token string `json:"token"`
}

func (s *Service) CreateSingle(ctx context.Context, nodeID string) (CreatedTicket, error) {
	if !identity.ValidUUID(nodeID) {
		return CreatedTicket{}, ErrInvalidInput
	}
	return s.create(ctx, &nodeID, nil, 15*time.Minute, 1)
}

func (s *Service) CreateBatch(ctx context.Context, groupID string) (CreatedTicket, error) {
	if strings.TrimSpace(groupID) == "" || len(groupID) > 255 {
		return CreatedTicket{}, ErrInvalidInput
	}
	return s.create(ctx, nil, &groupID, 30*time.Minute, 100)
}

func (s *Service) create(ctx context.Context, nodeID, groupID *string, lifetime time.Duration, maxUses int) (CreatedTicket, error) {
	s.randomMu.Lock()
	id, err := identity.UUID(s.random)
	var secret [32]byte
	if err == nil {
		_, err = io.ReadFull(s.random, secret[:])
	}
	s.randomMu.Unlock()
	if err != nil {
		return CreatedTicket{}, err
	}
	now := s.now().UTC()
	created := CreatedTicket{Ticket: Ticket{ID: id, NodeID: nodeID, GroupID: groupID, ExpiresAt: now.Add(lifetime), MaxUses: maxUses}, Token: base64.RawURLEncoding.EncodeToString(secret[:])}
	digest := sha256.Sum256([]byte(created.Token))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CreatedTicket{}, err
	}
	defer tx.Rollback()
	var exists int
	if nodeID != nil {
		err = tx.QueryRowContext(ctx, "SELECT count(*) FROM nodes WHERE id = ?", *nodeID).Scan(&exists)
	} else {
		err = tx.QueryRowContext(ctx, "SELECT count(*) FROM groups WHERE id = ?", *groupID).Scan(&exists)
	}
	if err != nil {
		return CreatedTicket{}, err
	}
	if exists == 0 {
		return CreatedTicket{}, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO enrollment_tokens(id, token_hash, node_id, group_id, expires_at, max_uses) VALUES (?, ?, ?, ?, ?, ?)`, id, digest[:], nodeID, groupID, timekey.Format(created.ExpiresAt), maxUses); err != nil {
		return CreatedTicket{}, err
	}
	if err := audit.Insert(ctx, tx, "enrollment.create", id, now); err != nil {
		return CreatedTicket{}, err
	}
	if err := tx.Commit(); err != nil {
		return CreatedTicket{}, err
	}
	return created, nil
}

type scanner interface{ Scan(...any) error }

const ticketColumns = "id, node_id, group_id, expires_at, max_uses, uses, revoked_at"

func scanTicket(row scanner) (Ticket, error) {
	var ticket Ticket
	var expires string
	var revoked sql.NullString
	if err := row.Scan(&ticket.ID, &ticket.NodeID, &ticket.GroupID, &expires, &ticket.MaxUses, &ticket.Uses, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Ticket{}, ErrNotFound
		}
		return Ticket{}, err
	}
	var err error
	ticket.ExpiresAt, err = timekey.Parse(expires)
	if err != nil {
		return Ticket{}, err
	}
	if revoked.Valid {
		timestamp, err := timekey.Parse(revoked.String)
		if err != nil {
			return Ticket{}, err
		}
		ticket.RevokedAt = &timestamp
	}
	return ticket, nil
}

func (s *Service) Get(ctx context.Context, id string) (Ticket, error) {
	return scanTicket(s.db.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM enrollment_tokens WHERE id = ?", id))
}

func (s *Service) List(ctx context.Context, limit, offset int) ([]Ticket, error) {
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, ErrInvalidInput
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+ticketColumns+" FROM enrollment_tokens ORDER BY expires_at DESC, id LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tickets := make([]Ticket, 0)
	for rows.Next() {
		ticket, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		tickets = append(tickets, ticket)
	}
	return tickets, rows.Err()
}

// CleanupExpiredTickets removes at most limit tickets whose expiration is
// older than before. Foreign-key cascade removes their enrollment receipts in
// the same statement, so an exact retry is retained until the configured
// receipt grace period has elapsed.
func (s *Service) CleanupExpiredTickets(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit < 1 {
		return 0, ErrInvalidInput
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM enrollment_tokens
WHERE id IN (
	SELECT id FROM enrollment_tokens
	WHERE expires_at < ?
	ORDER BY expires_at ASC, id ASC
	LIMIT ?
	)`, timekey.Format(before), limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Service) Revoke(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ticket, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM enrollment_tokens WHERE id = ?", id))
	if err != nil {
		return err
	}
	if ticket.RevokedAt != nil {
		return tx.Commit()
	}
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, "UPDATE enrollment_tokens SET revoked_at = ? WHERE id = ?", timekey.Format(now), id); err != nil {
		return err
	}
	if err := audit.Insert(ctx, tx, "enrollment.revoke", id, now); err != nil {
		return err
	}
	return tx.Commit()
}

// Recover authenticates both credential and installation in one read-only
// statement. It deliberately does not use the ingest writer or update liveness.
func (s *Service) Recover(ctx context.Context, token string, req v1.RecoveryRequest) (v1.EnrollmentResponse, error) {
	if err := req.Validate(); err != nil {
		return v1.EnrollmentResponse{}, ErrInvalidInput
	}
	if len(token) < 32 || len(token) > 256 {
		return v1.EnrollmentResponse{}, ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(token))
	var nodeID string
	err := s.db.QueryRowContext(ctx, "SELECT id FROM nodes WHERE token_hash = ? AND installation_id = ?", digest[:], req.InstallationID).Scan(&nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return v1.EnrollmentResponse{}, ErrUnauthenticated
	}
	if err != nil {
		return v1.EnrollmentResponse{}, err
	}
	return v1.EnrollmentResponse{NodeID: nodeID, ServerTime: s.now().UTC()}, nil
}

// Enroll serializes identity binding, capacity, quota and the retry receipt in
// one BEGIN IMMEDIATE transaction, including across independent service instances.
func (s *Service) Enroll(ctx context.Context, token string, req v1.EnrollmentRequest) (v1.EnrollmentResponse, error) {
	if err := req.Validate(); err != nil {
		if errors.Is(err, v1.ErrProtocolUnsupported) {
			return v1.EnrollmentResponse{}, err
		}
		return v1.EnrollmentResponse{}, ErrInvalidInput
	}
	if token == "" || len(token) > 256 {
		return v1.EnrollmentResponse{}, ErrUnauthenticated
	}
	ticketHash := sha256.Sum256([]byte(token))
	nodeHash := sha256.Sum256([]byte(req.NodeToken))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return v1.EnrollmentResponse{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	ticket, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM enrollment_tokens WHERE token_hash = ?", ticketHash[:]))
	if errors.Is(err, ErrNotFound) {
		return v1.EnrollmentResponse{}, ErrUnauthenticated
	}
	if err != nil {
		return v1.EnrollmentResponse{}, err
	}
	if ticket.RevokedAt != nil {
		return v1.EnrollmentResponse{}, ErrRevoked
	}

	// A receipt precedes natural expiry/quota, but only an active credential
	// can retry through the inclusive 24h edge. Superseded claims cannot revive it.
	var claimNodeID, claimed string
	var claimHash []byte
	var claimActive bool
	err = tx.QueryRowContext(ctx, `SELECT node_id, token_hash, claimed_at, EXISTS(SELECT 1 FROM nodes WHERE id = enrollment_claims.node_id AND token_hash = enrollment_claims.token_hash) FROM enrollment_claims WHERE enrollment_id = ? AND installation_id = ?`, ticket.ID, req.InstallationID).Scan(&claimNodeID, &claimHash, &claimed, &claimActive)
	if err == nil {
		if !claimActive || subtle.ConstantTimeCompare(claimHash, nodeHash[:]) != 1 {
			return v1.EnrollmentResponse{}, ErrConflict
		}
		claimedAt, err := timekey.Parse(claimed)
		if err != nil {
			return v1.EnrollmentResponse{}, err
		}
		if now.After(claimedAt.Add(24 * time.Hour)) {
			return v1.EnrollmentResponse{}, ErrExpired
		}
		if err := tx.Commit(); err != nil {
			return v1.EnrollmentResponse{}, err
		}
		return v1.EnrollmentResponse{NodeID: claimNodeID, ServerTime: now}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return v1.EnrollmentResponse{}, err
	}
	if ticket.Uses >= ticket.MaxUses {
		return v1.EnrollmentResponse{}, ErrConflict
	}
	if !now.Before(ticket.ExpiresAt) {
		return v1.EnrollmentResponse{}, ErrExpired
	}

	var nodeID string
	if ticket.NodeID != nil {
		nodeID = *ticket.NodeID
		var installation sql.NullString
		var currentHash []byte
		if err := tx.QueryRowContext(ctx, "SELECT installation_id, token_hash FROM nodes WHERE id = ?", nodeID).Scan(&installation, &currentHash); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return v1.EnrollmentResponse{}, ErrConflict
			}
			return v1.EnrollmentResponse{}, err
		}
		if installation.Valid && installation.String != req.InstallationID {
			return v1.EnrollmentResponse{}, ErrConflict
		}
		// A ticket authorizes a change, not a stale overwrite. Concurrent
		// candidates may share a parent, but only the current parent can win.
		if len(currentHash) != 0 && subtle.ConstantTimeCompare(currentHash, nodeHash[:]) != 1 {
			if req.PreviousNodeToken == "" {
				return v1.EnrollmentResponse{}, ErrConflict
			}
			previousHash := sha256.Sum256([]byte(req.PreviousNodeToken))
			if subtle.ConstantTimeCompare(currentHash, previousHash[:]) != 1 {
				return v1.EnrollmentResponse{}, ErrConflict
			}
		}
	} else if ticket.GroupID == nil {
		// A deleted batch group must not silently turn into ungrouped admission.
		return v1.EnrollmentResponse{}, ErrConflict
	}

	var collision int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM nodes WHERE id <> ? AND (installation_id = ? OR token_hash = ?)`, nodeID, req.InstallationID, nodeHash[:]).Scan(&collision); err != nil {
		return v1.EnrollmentResponse{}, err
	}
	if collision != 0 {
		return v1.EnrollmentResponse{}, ErrConflict
	}
	if ticket.NodeID == nil {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM nodes").Scan(&count); err != nil {
			return v1.EnrollmentResponse{}, err
		}
		if count >= s.maxNodes {
			return v1.EnrollmentResponse{}, nodes.ErrCapacityReached
		}
		s.randomMu.Lock()
		nodeID, err = identity.UUID(s.random)
		s.randomMu.Unlock()
		if err != nil {
			return v1.EnrollmentResponse{}, err
		}
		name := req.System.Hostname
		if strings.TrimSpace(name) == "" {
			name = req.InstallationID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO nodes(id, name, group_id, created_at) VALUES (?, ?, ?, ?)`, nodeID, name, ticket.GroupID, timekey.Format(now)); err != nil {
			return v1.EnrollmentResponse{}, err
		}
	}
	system, err := json.Marshal(req.System)
	if err != nil {
		return v1.EnrollmentResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET run_generation = CASE WHEN token_hash = ? THEN run_generation ELSE '0' END, report_session_id = CASE WHEN token_hash = ? THEN report_session_id ELSE '' END, token_hash = ?, installation_id = ?, first_enrolled_at = COALESCE(first_enrolled_at, ?), system = ?, status = CASE WHEN status = 'disabled' THEN 'disabled' ELSE 'unknown' END WHERE id = ?`, nodeHash[:], nodeHash[:], nodeHash[:], req.InstallationID, timekey.Format(now), string(system), nodeID); err != nil {
		return v1.EnrollmentResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO enrollment_claims(enrollment_id, installation_id, node_id, token_hash, claimed_at) VALUES (?, ?, ?, ?, ?)`, ticket.ID, req.InstallationID, nodeID, nodeHash[:], timekey.Format(now)); err != nil {
		return v1.EnrollmentResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE enrollment_tokens SET uses = uses + 1 WHERE id = ?", ticket.ID); err != nil {
		return v1.EnrollmentResponse{}, err
	}
	if err := audit.Insert(ctx, tx, "enrollment.claim", nodeID, now); err != nil {
		return v1.EnrollmentResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return v1.EnrollmentResponse{}, err
	}
	return v1.EnrollmentResponse{NodeID: nodeID, ServerTime: now}, nil
}
