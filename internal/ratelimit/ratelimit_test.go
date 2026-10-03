package ratelimit

import (
	"testing"
	"time"
)

func TestRateLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	rl := New(2, 1)
	rl.now = func() time.Time { return now }
	if !rl.Allow("a") || !rl.Allow("a") || rl.Allow("a") {
		t.Fatal("burst of 2")
	}
	if !rl.Allow("b") {
		t.Fatal("keys are independent")
	}
	now = now.Add(1500 * time.Millisecond)
	if !rl.Allow("a") || rl.Allow("a") {
		t.Fatal("refills at the rate")
	}
}
