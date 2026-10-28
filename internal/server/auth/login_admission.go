package auth

import (
	"context"
	"crypto/sha256"
	"net"
	"net/netip"
	"time"
)

type loginPeerKey struct{}

// WithLoginPeer records the client address chosen by the HTTP layer: the
// direct transport peer, or the forwarded client only via configured trusted proxies.
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
	return s.loginAdmission(ctx, username, true)
}

func (s *Service) checkLogin(ctx context.Context, username string) error {
	return s.loginAdmission(ctx, username, false)
}

func (s *Service) loginAdmission(ctx context.Context, username string, charge bool) error {
	peer := loginRatePeer(ctx)
	name := sha256.Sum256([]byte(username))
	key := loginIdentityKey{peer: peer, name: name}
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := s.now()
	// Recheck while charging so a failure recorded concurrently cannot be bypassed.
	if !s.loginUsernameAllowedLocked(key, name, now) {
		return ErrRateLimited
	}
	identity := loginBucketFor(s.loginNames, key, s.loginNameOverflow, now, 10, 1.0/30)
	if identity.tokens < 1 {
		return ErrRateLimited
	}
	source := loginBucketFor(s.loginPeers, peer, s.loginPeerOverflow, now, 32, 4)
	if source.tokens < 1 {
		return ErrRateLimited
	}
	if !charge {
		return nil
	}
	identity.tokens--
	source.tokens--
	storeLoginBucket(s.loginNames, key, &s.loginNameOverflow, identity, now, 10, 1.0/30)
	storeLoginBucket(s.loginPeers, peer, &s.loginPeerOverflow, source, now, 32, 4)
	return nil
}

// Eligibility checks never charge or allocate limiter state.
func (s *Service) checkLoginUsername(ctx context.Context, username string) error {
	name := sha256.Sum256([]byte(username))
	key := loginIdentityKey{peer: loginRatePeer(ctx), name: name}
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if !s.loginUsernameAllowedLocked(key, name, s.now()) {
		return ErrRateLimited
	}
	return nil
}

func (s *Service) loginUsernameAllowedLocked(key loginIdentityKey, name [sha256.Size]byte, now time.Time) bool {
	if key.peer.IsValid() {
		if last, ok := s.loginSuccesses[key]; ok {
			elapsed := now.Sub(last)
			if elapsed >= 0 && elapsed < 24*time.Hour {
				return true
			}
		}
	}
	bucket := loginBucketFor(s.loginFailures, name, s.loginFailureOverflow, now, 20, 1.0/60)
	return bucket.tokens >= 1
}

func (s *Service) recordLoginFailure(_ context.Context, username string) {
	name := sha256.Sum256([]byte(username))
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := s.now()
	bucket := loginBucketFor(s.loginFailures, name, s.loginFailureOverflow, now, 20, 1.0/60)
	if bucket.tokens >= 1 {
		bucket.tokens--
	}
	storeLoginBucket(s.loginFailures, name, &s.loginFailureOverflow, bucket, now, 20, 1.0/60)
}

func (s *Service) recordLoginSuccess(ctx context.Context, username string) {
	name := sha256.Sum256([]byte(username))
	peer := loginRatePeer(ctx)
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	delete(s.loginFailures, name)
	if !peer.IsValid() {
		return
	}
	storeLoginSuccess(s.loginSuccesses, loginIdentityKey{peer: peer, name: name}, s.now())
}

func (s *Service) admitPasswordChange(userID string) error {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := s.now()
	bucket := loginBucketFor(s.passwordChanges, userID, s.passwordChangeOverflow, now, 5, 1.0/60)
	if bucket.tokens < 1 {
		return ErrRateLimited
	}
	bucket.tokens--
	storeLoginBucket(s.passwordChanges, userID, &s.passwordChangeOverflow, bucket, now, 5, 1.0/60)
	return nil
}

func loginRatePeer(ctx context.Context) netip.Addr {
	peer, _ := ctx.Value(loginPeerKey{}).(netip.Addr)
	peer = peer.Unmap()
	if peer.Is6() {
		peer = peer.WithZone("")
		return netip.PrefixFrom(peer, 64).Masked().Addr()
	}
	return peer
}

func loginBucketFor[K comparable](buckets map[K]loginBucket, key K, overflow loginBucket, now time.Time, burst, refill float64) loginBucket {
	if bucket, exists := buckets[key]; exists {
		return refillLoginBucket(bucket, now, burst, refill)
	}
	if len(buckets) >= 1024 {
		if _, ok := reclaimableLoginBucket(buckets, now, burst, refill); !ok {
			return refillLoginBucket(overflow, now, burst, refill)
		}
	}
	return loginBucket{tokens: burst, at: now}
}

func refillLoginBucket(bucket loginBucket, now time.Time, burst, refill float64) loginBucket {
	if elapsed := now.Sub(bucket.at).Seconds(); elapsed > 0 {
		bucket.tokens = min(burst, bucket.tokens+elapsed*refill)
		bucket.at = now
	}
	return bucket
}

func reclaimableLoginBucket[K comparable](buckets map[K]loginBucket, now time.Time, burst, refill float64) (K, bool) {
	for key, bucket := range buckets {
		if refillLoginBucket(bucket, now, burst, refill).tokens >= burst {
			return key, true
		}
	}
	var zero K
	return zero, false
}

// Forget only fully replenished budgets; churn must never reset depleted history.
func storeLoginBucket[K comparable](buckets map[K]loginBucket, key K, overflow *loginBucket, bucket loginBucket, now time.Time, burst, refill float64) {
	if _, exists := buckets[key]; !exists && len(buckets) >= 1024 {
		if candidate, ok := reclaimableLoginBucket(buckets, now, burst, refill); ok {
			delete(buckets, candidate)
		} else {
			*overflow = bucket
			return
		}
	}
	buckets[key] = bucket
}

func storeLoginSuccess(successes map[loginIdentityKey]time.Time, key loginIdentityKey, at time.Time) {
	if _, exists := successes[key]; !exists && len(successes) >= 1024 {
		var expired loginIdentityKey
		found := false
		for candidate, candidateAt := range successes {
			if at.Sub(candidateAt) >= 24*time.Hour {
				expired, found = candidate, true
				break
			}
		}
		if !found {
			// Sharing trust across overflow identities would grant an exemption.
			return
		}
		delete(successes, expired)
	}
	successes[key] = at
}
