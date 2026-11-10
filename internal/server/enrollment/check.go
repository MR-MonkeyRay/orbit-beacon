package enrollment

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"

	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

// CheckClaim 检查一个读取快照。它绝不消费票据、创建资产、
// 更改凭据/围栏或刷新节点活跃性。
func (s *Service) CheckClaim(ctx context.Context, token string, req v1.EnrollmentCheckRequest) (v1.EnrollmentCheckResponse, error) {
	if err := req.Validate(); err != nil {
		if errors.Is(err, v1.ErrProtocolUnsupported) {
			return v1.EnrollmentCheckResponse{}, err
		}
		return v1.EnrollmentCheckResponse{}, ErrInvalidInput
	}
	if token == "" || len(token) > 256 {
		return v1.EnrollmentCheckResponse{}, ErrInvalidTicket
	}
	digest := sha256.Sum256([]byte(token))
	actual := sha256.Sum256([]byte(req.NodeToken))
	var expires string
	var revoked, credentialActive bool
	var uses int
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return v1.EnrollmentCheckResponse{}, leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	err := lease.QueryRowContext(ctx, `SELECT t.expires_at, t.revoked_at IS NOT NULL, t.uses,
 EXISTS(SELECT 1 FROM nodes n WHERE n.installation_id=? AND n.token_hash=?)
 FROM enrollment_tokens t WHERE t.token_hash=?`, req.InstallationID, actual[:], digest[:]).Scan(&expires, &revoked, &uses, &credentialActive)
	if errors.Is(err, sql.ErrNoRows) {
		return v1.EnrollmentCheckResponse{}, ErrInvalidTicket
	}
	if err != nil {
		return v1.EnrollmentCheckResponse{}, err
	}
	expiresAt, err := timekey.Parse(expires)
	if err != nil {
		return v1.EnrollmentCheckResponse{}, err
	}
	now := s.now().UTC()
	if revoked || uses != 0 || !now.Before(expiresAt) {
		return v1.EnrollmentCheckResponse{}, ErrInvalidTicket
	}
	state := "unbound"
	if credentialActive {
		state = "active"
	}
	return v1.EnrollmentCheckResponse{Claimed: false, CredentialState: state, ServerTime: now}, nil
}
