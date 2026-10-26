package service

import (
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Backoff struct {
	random  func() float64
	attempt uint
}

func NewBackoff(random func() float64) *Backoff {
	if random == nil {
		random = rand.Float64
	}
	return &Backoff{random: random}
}

func (b *Backoff) Reset() { b.attempt = 0 }

func (b *Backoff) Next(status int, retryAfter string, now time.Time) time.Duration {
	if status == http.StatusUnauthorized {
		return 60 * time.Second
	}
	seconds := 60
	if b.attempt < 6 {
		seconds = 1 << b.attempt
		b.attempt++
	}
	if (status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable) && retryAfter != "" {
		value, err := strconv.ParseInt(strings.TrimSpace(retryAfter), 10, 64)
		if err == nil {
			if value < 1 {
				return time.Second
			}
			if value > 300 {
				return 300 * time.Second
			}
			return time.Duration(value) * time.Second
		}
		if date, err := http.ParseTime(retryAfter); err == nil {
			delay := date.Sub(now)
			if delay < time.Second {
				return time.Second
			}
			if delay > 300*time.Second {
				return 300 * time.Second
			}
			return delay
		}
	}
	random := b.random()
	if random < 0 {
		random = 0
	}
	if random > 1 {
		random = 1
	}
	return time.Duration(float64(time.Duration(seconds)*time.Second) * (0.8 + 0.4*random))
}
