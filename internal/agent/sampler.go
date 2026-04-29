package agent

import (
	"fmt"
	"sync"
	"time"
)

type AccessSampler struct {
	mu sync.Mutex

	windowKey   string // YYYYMMDDHHMM
	perUser     map[int64]int
	perUserDest map[string]int

	maxPerUser     int
	maxPerUserDest int
}

func NewAccessSampler(maxPerUser, maxPerUserDest int) *AccessSampler {
	if maxPerUser <= 0 {
		maxPerUser = 30
	}
	if maxPerUserDest <= 0 {
		maxPerUserDest = 5
	}
	return &AccessSampler{
		perUser:        make(map[int64]int),
		perUserDest:    make(map[string]int),
		maxPerUser:     maxPerUser,
		maxPerUserDest: maxPerUserDest,
	}
}

func (s *AccessSampler) Allow(userID int64, destination string, at time.Time) bool {
	if at.IsZero() {
		at = time.Now()
	}
	key := at.UTC().Format("200601021504")

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.windowKey != key {
		s.windowKey = key
		s.perUser = make(map[int64]int)
		s.perUserDest = make(map[string]int)
	}

	if userID <= 0 {
		return false
	}
	u := s.perUser[userID]
	if u >= s.maxPerUser {
		return false
	}
	s.perUser[userID] = u + 1

	if destination != "" {
		k := fmt.Sprintf("%d|%s", userID, destination)
		d := s.perUserDest[k]
		if d >= s.maxPerUserDest {
			// Undo per-user increment: destination limit should not consume user budget.
			s.perUser[userID] = u
			return false
		}
		s.perUserDest[k] = d + 1
	}

	return true
}
