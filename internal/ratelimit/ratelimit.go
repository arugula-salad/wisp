// Package ratelimit is a token bucket per key, for events the host takes from
// sources it does not trust to pace themselves: a guest's reports, the
// network policy helper's denials.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a token bucket per key.
type Limiter struct {
	burst, rate float64
	now         func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

// New allows a burst of burst per key, then perSecond a second.
func New(burst, perSecond float64) *Limiter {
	return &Limiter{burst: burst, rate: perSecond, now: time.Now, buckets: map[string]*bucket{}}
}

// Allow takes a token from key's bucket, and reports false when it is empty.
func (rl *Limiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	b, ok := rl.buckets[key]
	if !ok {
		if len(rl.buckets) > 10000 { // keys are sprite IDs; this only bounds memory against churn
			clear(rl.buckets)
		}
		b = &bucket{tokens: rl.burst, at: now}
		rl.buckets[key] = b
	}
	b.tokens = min(rl.burst, b.tokens+now.Sub(b.at).Seconds()*rl.rate)
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
