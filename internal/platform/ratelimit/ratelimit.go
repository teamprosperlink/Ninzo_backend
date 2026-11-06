// Package ratelimit counts requests per key, so one sender cannot use up the database or the SMS
// budget. It holds no business rules. See docs/decisions/0010-rate-limits-in-memory.md.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Rule is one limit: at most Limit requests per key in each Window.
type Rule struct {
	Name   string
	Limit  int
	Window time.Duration
}

// Limiter says whether a request may go ahead, and if not, how long to wait. A store that keeps
// counts elsewhere, such as Redis, needs only this one method.
type Limiter interface {
	Allow(ctx context.Context, key string, rule Rule) (bool, time.Duration, error)
}

// maxKeys bounds memory, because a sender chooses its own keys. At 100 bytes or so each, this is
// about 20 MB.
const (
	maxKeys    = 200_000
	sweepEvery = time.Minute
)

type entry struct {
	count   int
	resetAt time.Time
}

// Memory counts within this one process. Counts reset on restart, and each replica counts alone.
type Memory struct {
	mu        sync.Mutex
	entries   map[string]entry
	lastSweep time.Time
	max       int
	now       func() time.Time
}

// NewMemory returns an empty limiter.
func NewMemory() *Memory {
	return &Memory{entries: map[string]entry{}, max: maxKeys, now: time.Now}
}

// Allow counts this request in a fixed window. A full table lets new keys through: failing closed
// would let one attacker lock everybody out, while a key already counted is still limited.
func (m *Memory) Allow(_ context.Context, key string, rule Rule) (bool, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if now.Sub(m.lastSweep) >= sweepEvery {
		m.sweep(now)
	}
	k := rule.Name + "|" + key
	e, ok := m.entries[k]
	if !ok || !now.Before(e.resetAt) {
		if !ok && len(m.entries) >= m.max {
			m.sweep(now)
			if len(m.entries) >= m.max {
				return true, 0, nil
			}
		}
		m.entries[k] = entry{count: 1, resetAt: now.Add(rule.Window)}
		return true, 0, nil
	}
	if e.count >= rule.Limit {
		return false, e.resetAt.Sub(now), nil
	}
	e.count++
	m.entries[k] = e
	return true, 0, nil
}

func (m *Memory) sweep(now time.Time) {
	for k, e := range m.entries {
		if !now.Before(e.resetAt) {
			delete(m.entries, k)
		}
	}
	m.lastSweep = now
}
