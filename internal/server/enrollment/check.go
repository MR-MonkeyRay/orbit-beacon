package enrollment

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

// CheckClaim inspects one SQLite read snapshot. It never consumes a ticket,
// creates an asset, changes a credential/fence, or refreshes node liveness.
func (s *Service) CheckClaim(ctx context.Context, token string, req v1.EnrollmentCheckRequest) (v1.EnrollmentCheckResponse, error) {
	if err := req.Validate(); err != nil {
		if errors.Is(err, v1.ErrProtocolUnsupported) {
			return v1.EnrollmentCheckResponse{}, err
		}
		return v1.EnrollmentCheckResponse{}, ErrInvalidInput
	}
	if token == "" || len(token) > 256 {
		return v1.EnrollmentCheckResponse{}, ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(token))
	var expires string
	var revoked, active bool
	var uses, maxUses int
	var nodeID, claimed sql.NullString
	var candidate []byte
	err := s.db.QueryRowContext(ctx, `SELECT t.expires_at, t.revoked_at IS NOT NULL, t.uses, t.max_uses,
 c.node_id, c.token_hash, c.claimed_at,
 EXISTS(SELECT 1 FROM nodes n WHERE n.id=c.node_id AND n.token_hash=c.token_hash)
 FROM enrollment_tokens t LEFT JOIN enrollment_claims c ON c.enrollment_id=t.id AND c.installation_id=?
 WHERE t.token_hash=?`, req.InstallationID, digest[:]).Scan(&expires, &revoked, &uses, &maxUses, &nodeID, &candidate, &claimed, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return v1.EnrollmentCheckResponse{}, ErrUnauthenticated
	}
	if err != nil {
		return v1.EnrollmentCheckResponse{}, err
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return v1.EnrollmentCheckResponse{}, err
	}
	if revoked {
		return v1.EnrollmentCheckResponse{}, ErrRevoked
	}
	now := s.now().UTC()
	if !nodeID.Valid {
		if uses >= maxUses {
			return v1.EnrollmentCheckResponse{}, ErrConflict
		}
		if !now.Before(expiresAt) {
			return v1.EnrollmentCheckResponse{}, ErrExpired
		}
		return v1.EnrollmentCheckResponse{Claimed: false, ServerTime: now}, nil
	}
	actual := sha256.Sum256([]byte(req.NodeToken))
	if !active || subtle.ConstantTimeCompare(candidate, actual[:]) != 1 {
		return v1.EnrollmentCheckResponse{}, ErrConflict
	}
	claimedAt, err := time.Parse(time.RFC3339Nano, claimed.String)
	if err != nil {
		return v1.EnrollmentCheckResponse{}, err
	}
	if now.After(claimedAt.Add(24 * time.Hour)) {
		return v1.EnrollmentCheckResponse{}, ErrExpired
	}
	return v1.EnrollmentCheckResponse{Claimed: true, NodeID: nodeID.String, ServerTime: now}, nil
}
