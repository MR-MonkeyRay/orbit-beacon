package auth

import (
	"math"
	"time"
)

type APITokenRateLimitError struct{ RetryAfter int }

func (e *APITokenRateLimitError) Error() string { return string(ErrRateLimited) }
func (e *APITokenRateLimitError) Unwrap() error { return ErrRateLimited }

type apiTokenBucket struct {
	loginBucket
	limit int
}

// admitAPIToken 只在内存中计数：rateMu 同时保护登录限流，不能跨数据库查询持有。
func (s *Service) admitAPIToken(id string, limit int, now time.Time) error {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	bucket, exists := s.apiTokenBuckets[id]
	if !exists {
		bucket = apiTokenBucket{loginBucket: loginBucket{tokens: float64(limit), at: now}, limit: limit}
	} else {
		// 按本次读到的限额结算流逝时间，不发放新旧限额的差额：查询在锁外完成，
		// 过期的限额快照会让限额来回变化，每次变化都发放差额会多放行请求。
		bucket.loginBucket = refillLoginBucket(bucket.loginBucket, now, float64(limit), float64(limit)/60)
		bucket.tokens = min(bucket.tokens, float64(limit))
		bucket.limit = limit
	}
	var err error
	if bucket.tokens < 1 {
		err = &APITokenRateLimitError{RetryAfter: max(1, int(math.Ceil((1-bucket.tokens)*60/float64(limit))))}
	} else {
		bucket.tokens--
	}
	s.apiTokenBuckets[id] = bucket
	// 闲置一分钟的桶在任何限额下都已补满，删除不会改变额度；
	// 活跃的桶不能删除，否则下一次请求会按当前限额重建，等于发放新额度。
	if len(s.apiTokenBuckets) > maxAPITokens {
		for key, candidate := range s.apiTokenBuckets {
			if now.Sub(candidate.at) >= time.Minute {
				delete(s.apiTokenBuckets, key)
			}
		}
	}
	return err
}
