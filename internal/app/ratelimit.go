package app

import (
	"sync"
	"time"
)

type rateLimiter struct {
	mu        sync.Mutex
	m         map[string]rateWindow
	lastSweep time.Time
}

type rateWindow struct {
	start time.Time
	count int
}

// rateLimiterSweepEvery bounds how often the expired-window sweep runs;
// rateLimiterMaxIdle is how long past its window start an entry may linger.
const (
	rateLimiterSweepEvery = 5 * time.Minute
	rateLimiterMaxIdle    = 15 * time.Minute
)

func newRateLimiter() *rateLimiter {
	return &rateLimiter{m: make(map[string]rateWindow)}
}

func (rl *rateLimiter) Allow(key string, limit int, window time.Duration, now time.Time) bool {
	if rl == nil || limit <= 0 || window <= 0 || key == "" {
		return true
	}
	if now.IsZero() {
		now = time.Now()
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Keys are partly attacker-controlled (IPs, usernames), so expired
	// windows must be evicted or the map grows without bound.
	if rl.lastSweep.IsZero() {
		rl.lastSweep = now
	} else if now.Sub(rl.lastSweep) >= rateLimiterSweepEvery {
		rl.lastSweep = now
		for k, w := range rl.m {
			if now.Sub(w.start) >= rateLimiterMaxIdle {
				delete(rl.m, k)
			}
		}
	}

	w := rl.m[key]
	if w.start.IsZero() || now.Sub(w.start) >= window {
		rl.m[key] = rateWindow{start: now, count: 1}
		return true
	}
	if w.count >= limit {
		return false
	}
	w.count++
	rl.m[key] = w
	return true
}
