package app

import (
	"sync"
	"time"
)

type rateLimiter struct {
	mu sync.Mutex
	m  map[string]rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

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
