package enrollment

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/netip"
	"time"

	"orbitbeacon/internal/timekey"
)

const maxInvalidPeers = 1024

type invalidBucket struct {
	tokens   float64
	refilled time.Time
}

// AdmitToken performs only indexed reads before the handler touches the body.
// Admission is not authorization: Enroll repeats every check in its transaction.
func (s *Service) AdmitToken(ctx context.Context, token, directPeer string) error {
	err := s.usableTicket(ctx, token)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrUnauthenticated) && !errors.Is(err, ErrRevoked) && !errors.Is(err, ErrExpired) {
		return err
	}
	if !s.chargeInvalid(directPeer) {
		return ErrRateLimited
	}
	return err
}

func (s *Service) usableTicket(ctx context.Context, token string) error {
	if token == "" || len(token) > 256 {
		return ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(token))
	ticket, err := scanTicket(s.db.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM enrollment_tokens WHERE token_hash = ?", digest[:]))
	if errors.Is(err, ErrNotFound) {
		return ErrUnauthenticated
	}
	if err != nil {
		return err
	}
	if ticket.RevokedAt != nil {
		return ErrRevoked
	}
	now := s.now().UTC()
	if now.Before(ticket.ExpiresAt) {
		return nil
	}
	// An expired ticket may still authenticate an exact retry. Without reading
	// its body we cannot select a receipt, so admit only if a live one exists;
	// the transaction verifies installation and credential, quota, and expiry.
	rows, err := s.db.QueryContext(ctx, "SELECT claimed_at FROM enrollment_claims WHERE enrollment_id = ?", ticket.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var claimed string
		if err := rows.Scan(&claimed); err != nil {
			return err
		}
		claimedAt, err := timekey.Parse(claimed)
		if err != nil {
			return err
		}
		if !now.After(claimedAt.Add(24 * time.Hour)) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return ErrExpired
}

func refillInvalid(bucket invalidBucket, now time.Time, burst float64) invalidBucket {
	if bucket.refilled.IsZero() {
		return invalidBucket{tokens: burst, refilled: now}
	}
	if elapsed := now.Sub(bucket.refilled).Seconds(); elapsed > 0 {
		bucket.tokens = min(burst, bucket.tokens+elapsed)
		bucket.refilled = now
	}
	return bucket
}

func (s *Service) chargeInvalid(remote string) bool {
	// Only the connection peer is authoritative. Ports and forwarding headers
	// must never create new buckets. Malformed peers share one bounded bucket.
	peer := "unknown"
	if host, _, err := net.SplitHostPort(remote); err == nil {
		if address, err := netip.ParseAddr(host); err == nil {
			peer = address.Unmap().WithZone("").String()
		}
	}
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	now := s.now().UTC()
	s.invalidGlobal = refillInvalid(s.invalidGlobal, now, 256)
	bucket := refillInvalid(s.invalidPeers[peer], now, 32)
	if s.invalidGlobal.tokens < 1 || bucket.tokens < 1 {
		return false
	}
	if _, exists := s.invalidPeers[peer]; !exists && len(s.invalidPeers) >= maxInvalidPeers {
		var oldestPeer string
		var oldest time.Time
		for key, value := range s.invalidPeers {
			if oldest.IsZero() || value.refilled.Before(oldest) {
				oldestPeer, oldest = key, value.refilled
			}
		}
		delete(s.invalidPeers, oldestPeer)
	}
	s.invalidGlobal.tokens--
	bucket.tokens--
	s.invalidPeers[peer] = bucket
	return true
}
