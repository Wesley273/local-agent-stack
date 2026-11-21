package a2a

import (
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Rate limiting (contract §2.4) — dual dimension: requests + tokens.
// Interface-first so the in-memory implementation can be swapped for Redis
// without changing any caller (production mapping in docs/05).
// ---------------------------------------------------------------------------

// LimitRule is one app's quota.
type LimitRule struct {
	RequestsPerMinute int `json:"requestsPerMinute"`
	TokensPerMinute   int `json:"tokensPerMinute"`
	Burst             int `json:"burst"`
}

// LimitDecision is the outcome of a pre-flight check.
type LimitDecision struct {
	Allowed    bool   `json:"allowed"`
	Reason     string `json:"reason,omitempty"`
	RetryAfter int    `json:"retryAfter,omitempty"` // seconds
	Remaining  int    `json:"remaining,omitempty"`
}

// Limiter is the abstraction (Redis implementation can replace this).
type Limiter interface {
	// AllowRequest consumes one request slot.
	AllowRequest(key string, rule LimitRule) LimitDecision
	// ReserveTokens pre-deducts an estimated token amount; returns a token id
	// used by SettleTokens after the real usage is known.
	ReserveTokens(key string, estimate int, rule LimitRule) (string, LimitDecision)
	// SettleTokens adjusts the reservation with actual usage.
	SettleTokens(tokenID string, actual int)
	// Stats returns current counters for diagnostics.
	Stats() map[string]any
}

// ---------------------------------------------------------------------------
// In-memory sliding-window implementation
// ---------------------------------------------------------------------------

type window struct {
	mu       sync.Mutex
	requests []int64 // unix ms timestamps
	tokens   []tokenEntry
	reserve  map[string]int // tokenID -> reserved amount
}

type tokenEntry struct {
	at     int64
	amount int
}

// MemoryLimiter is a concurrency-safe sliding-window limiter.
type MemoryLimiter struct {
	mu      sync.RWMutex
	buckets map[string]*window
	seq     int64
}

// NewMemoryLimiter creates an empty limiter.
func NewMemoryLimiter() *MemoryLimiter {
	return &MemoryLimiter{buckets: map[string]*window{}}
}

func (m *MemoryLimiter) bucket(key string) *window {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.buckets[key]
	if w == nil {
		w = &window{reserve: map[string]int{}}
		m.buckets[key] = w
	}
	return w
}

func pruneRequests(w *window, now int64) {
	cut := now - int64(time.Minute/time.Millisecond)
	i := 0
	for i < len(w.requests) && w.requests[i] < cut {
		i++
	}
	if i > 0 {
		w.requests = append([]int64(nil), w.requests[i:]...)
	}
}

func pruneTokens(w *window, now int64) {
	cut := now - int64(time.Minute/time.Millisecond)
	i := 0
	for i < len(w.tokens) && w.tokens[i].at < cut {
		i++
	}
	if i > 0 {
		w.tokens = append([]tokenEntry(nil), w.tokens[i:]...)
	}
}

func sumTokens(w *window) int {
	t := 0
	for _, e := range w.tokens {
		t += e.amount
	}
	return t
}

// AllowRequest implements Limiter.
func (m *MemoryLimiter) AllowRequest(key string, rule LimitRule) LimitDecision {
	w := m.bucket(key)
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now().UnixMilli()
	pruneRequests(w, now)
	limit := rule.RequestsPerMinute
	if limit <= 0 {
		limit = 120
	}
	if len(w.requests) >= limit {
		oldest := w.requests[0]
		retry := int((oldest+int64(time.Minute/time.Millisecond)-now)/1000) + 1
		if retry < 1 {
			retry = 1
		}
		return LimitDecision{Allowed: false, Reason: "requests per minute exceeded", RetryAfter: retry}
	}
	w.requests = append(w.requests, now)
	return LimitDecision{Allowed: true, Remaining: limit - len(w.requests)}
}

// ReserveTokens implements Limiter.
func (m *MemoryLimiter) ReserveTokens(key string, estimate int, rule LimitRule) (string, LimitDecision) {
	w := m.bucket(key)
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now().UnixMilli()
	pruneTokens(w, now)
	limit := rule.TokensPerMinute
	if limit <= 0 {
		limit = 60000
	}
	used := sumTokens(w)
	if used+estimate > limit {
		retry := 60
		if len(w.tokens) > 0 {
			retry = int((w.tokens[0].at+int64(time.Minute/time.Millisecond)-now)/1000) + 1
			if retry < 1 {
				retry = 1
			}
		}
		return "", LimitDecision{Allowed: false, Reason: "tokens per minute exceeded", RetryAfter: retry}
	}
	if estimate < 0 {
		estimate = 0
	}
	w.tokens = append(w.tokens, tokenEntry{at: now, amount: estimate})
	m.mu.Lock()
	m.seq++
	id := "tk-" + itoa(m.seq)
	m.mu.Unlock()
	w.reserve[id] = estimate
	return id, LimitDecision{Allowed: true, Remaining: limit - (used + estimate)}
}

// SettleTokens implements Limiter: correct the pre-deduction with real usage.
func (m *MemoryLimiter) SettleTokens(tokenID string, actual int) {
	if tokenID == "" {
		return
	}
	m.mu.RLock()
	var target *window
	for _, w := range m.buckets {
		w.mu.Lock()
		if _, ok := w.reserve[tokenID]; ok {
			target = w
			w.mu.Unlock()
			break
		}
		w.mu.Unlock()
	}
	m.mu.RUnlock()
	if target == nil {
		return
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	reserved, ok := target.reserve[tokenID]
	if !ok {
		return
	}
	delete(target.reserve, tokenID)
	delta := actual - reserved
	if delta == 0 {
		return
	}
	// adjust the most recent entry that matches the reservation
	for i := len(target.tokens) - 1; i >= 0; i-- {
		if target.tokens[i].amount == reserved {
			target.tokens[i].amount += delta
			if target.tokens[i].amount < 0 {
				target.tokens[i].amount = 0
			}
			break
		}
	}
}

// Stats implements Limiter.
func (m *MemoryLimiter) Stats() map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]any{"buckets": len(m.buckets)}
	per := map[string]any{}
	for k, w := range m.buckets {
		w.mu.Lock()
		pruneRequests(w, time.Now().UnixMilli())
		pruneTokens(w, time.Now().UnixMilli())
		per[k] = map[string]any{"requests": len(w.requests), "tokens": sumTokens(w)}
		w.mu.Unlock()
	}
	out["per"] = per
	return out
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
