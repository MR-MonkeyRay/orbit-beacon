package enrollment

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/netip"
	"time"

	"orbitbeacon/internal/server/storage"
)

const maxInvalidPeers = 1024

type invalidBucket struct {
	tokens   float64
	refilled time.Time
}

// AdmitToken 在处理程序触碰请求体之前只做索引读取。
// 准入不是授权：Enroll 在其事务中重复每一项检查。
func (s *Service) AdmitToken(ctx context.Context, token, directPeer string) error {
	err := s.usableTicket(ctx, token)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrInvalidTicket) {
		return err
	}
	if !s.chargeInvalid(directPeer) {
		return ErrRateLimited
	}
	return err
}

func (s *Service) usableTicket(ctx context.Context, token string) error {
	if token == "" || len(token) > 256 {
		return ErrInvalidTicket
	}
	digest := sha256.Sum256([]byte(token))
	lease, leaseErr := s.db.Acquire(ctx, storage.Reader)
	if leaseErr != nil {
		return leaseErr
	}
	defer lease.Close()
	ctx = lease.Context()
	ticket, err := scanTicket(lease.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM enrollment_tokens WHERE token_hash = ?", digest[:]))
	if errors.Is(err, ErrNotFound) {
		return ErrInvalidTicket
	}
	if err != nil {
		return err
	}
	if ticket.RevokedAt != nil || ticket.Uses != 0 || !s.now().UTC().Before(ticket.ExpiresAt) {
		return ErrInvalidTicket
	}
	return nil
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
	// HTTP 层提供连接对端，或通过配置的可信代理推导的客户端；
	// 端口绝不能创建新桶。格式错误的对端共享一个有界桶。
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
