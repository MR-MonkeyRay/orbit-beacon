package auth

import (
	"context"
	"crypto/sha256"
	"net"
	"net/netip"
	"time"
)

type loginPeerKey struct{}

// WithLoginPeer records only the direct transport peer, never forwarding headers.
func WithLoginPeer(ctx context.Context, remoteAddr string) context.Context {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	peer, _ := netip.ParseAddr(host)
	return context.WithValue(ctx, loginPeerKey{}, peer.Unmap())
}

type loginBucket struct {
	tokens float64
	at     time.Time
}

type loginIdentityKey struct {
	peer netip.Addr
	name [sha256.Size]byte
}

// Limit both source rotation across usernames and repeated guesses at one name.
// The maps are bounded independently; both budgets must allow an attempt before either is charged.
func (s *Service) admitLogin(ctx context.Context, username string) error {
	peer, _ := ctx.Value(loginPeerKey{}).(netip.Addr)
	key := loginIdentityKey{peer: peer, name: sha256.Sum256([]byte(username))}
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := s.now()
	identity := loginBucketFor(s.loginNames, key, now, 16, 1)
	if identity.tokens < 1 {
		s.loginNames[key] = identity
		return ErrRateLimited
	}
	source := loginBucketFor(s.loginPeers, peer, now, 32, 4)
	if source.tokens < 1 {
		s.loginPeers[peer] = source
		return ErrRateLimited
	}
	identity.tokens--
	source.tokens--
	storeLoginBucket(s.loginNames, key, identity)
	storeLoginBucket(s.loginPeers, peer, source)
	return nil
}

func loginBucketFor[K comparable](buckets map[K]loginBucket, key K, now time.Time, burst, refill float64) loginBucket {
	bucket, exists := buckets[key]
	if !exists {
		return loginBucket{tokens: burst, at: now}
	}
	if elapsed := now.Sub(bucket.at).Seconds(); elapsed > 0 {
		bucket.tokens = min(burst, bucket.tokens+elapsed*refill)
		bucket.at = now
	}
	return bucket
}

// Only admitted attempts may allocate a new identity or evict old history.
func storeLoginBucket[K comparable](buckets map[K]loginBucket, key K, bucket loginBucket) {
	if _, exists := buckets[key]; !exists && len(buckets) >= 1024 {
		var oldest K
		var at time.Time
		for candidate, value := range buckets {
			if at.IsZero() || value.at.Before(at) {
				oldest, at = candidate, value.at
			}
		}
		delete(buckets, oldest)
	}
	buckets[key] = bucket
}
