package signaling

import (
	"container/list"
	"sync"
	"time"
)

type authPendingEntry struct {
	key      string
	failures int
	lastSeen time.Time
	elem     *list.Element
}

type authLockedEntry struct {
	key         string
	lockedUntil time.Time
	elem        *list.Element
}

// AuthLockoutConfig configures an O(1) lockout-preserving authentication failure limiter.
type AuthLockoutConfig struct {
	MaxFailures       int           // Failures per key before lockout (default 10)
	LockoutDuration   time.Duration // Lockout & per-key failure window (default 5m)
	MaxPendingKeys    int           // Max tracked non-locked keys in LRU (default 4096)
	MaxLockedKeys     int           // Max active locked keys in FIFO (default 8192)
	GlobalMaxFailures int           // Optional global failures per GlobalWindow across all IPs (0 disables)
	GlobalWindow      time.Duration // Sliding window for GlobalMaxFailures (default 1m)
}

// AuthLockoutTable provides strictly O(1) amortized brute-force protection shared by
// SignalingServer and Agent:
//   - Separate `pending` LRU and `locked` monotonic FIFO (constant lockoutDuration guarantees
//     FIFO front is always the earliest-expiring lockout -> O(1) expiry pruning, zero O(N) scans).
//   - Actively locked entries are NEVER evicted by new keys; saturation fails closed.
//   - Optional O(1) global failure sliding ring blocks distributed / IPv6 /48 / 9-attempts-per-IP
//     LRU eviction bypass attacks.
type AuthLockoutTable struct {
	mu              sync.Mutex
	maxFailures     int
	lockoutDuration time.Duration
	maxPending      int
	maxLocked       int

	pending    map[string]*authPendingEntry
	pendingLRU *list.List

	locked     map[string]*authLockedEntry
	lockedFIFO *list.List

	globalRing   []time.Time
	globalHead   int
	globalWindow time.Duration
}

func NewAuthLockoutTable(cfg AuthLockoutConfig) *AuthLockoutTable {
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = authLockoutThreshold
	}
	if cfg.LockoutDuration <= 0 {
		cfg.LockoutDuration = authLockoutDuration
	}
	if cfg.MaxPendingKeys <= 0 {
		cfg.MaxPendingKeys = 4096
	}
	if cfg.MaxLockedKeys <= 0 {
		cfg.MaxLockedKeys = maxAuthFailedIPs
	}
	if cfg.GlobalWindow <= 0 {
		cfg.GlobalWindow = time.Minute
	}
	var ring []time.Time
	if cfg.GlobalMaxFailures > 0 {
		ring = make([]time.Time, cfg.GlobalMaxFailures)
	}
	return &AuthLockoutTable{
		maxFailures:     cfg.MaxFailures,
		lockoutDuration: cfg.LockoutDuration,
		maxPending:      cfg.MaxPendingKeys,
		maxLocked:       cfg.MaxLockedKeys,
		pending:         make(map[string]*authPendingEntry),
		pendingLRU:      list.New(),
		locked:          make(map[string]*authLockedEntry),
		lockedFIFO:      list.New(),
		globalRing:      ring,
		globalWindow:    cfg.GlobalWindow,
	}
}

func (t *AuthLockoutTable) pruneExpiredLocked(now time.Time) {
	for front := t.lockedFIFO.Front(); front != nil; {
		e := front.Value.(*authLockedEntry)
		if now.Before(e.lockedUntil) {
			break // Monotonic FIFO: all subsequent entries expire at or after e.lockedUntil.
		}
		next := front.Next()
		t.lockedFIFO.Remove(front)
		delete(t.locked, e.key)
		front = next
	}
}

// Allow reports whether rawIP may attempt authentication at now.
// Returns (false, retryAfter) when the key is locked, the locked table is saturated,
// or the global failure sliding window is exhausted.
func (t *AuthLockoutTable) Allow(rawIP string, now time.Time) (bool, time.Duration) {
	key := RateLimitKey(rawIP)
	t.mu.Lock()
	defer t.mu.Unlock()

	t.pruneExpiredLocked(now)

	if e, ok := t.locked[key]; ok {
		return false, e.lockedUntil.Sub(now)
	}
	if len(t.locked) >= t.maxLocked {
		earliest := t.lockedFIFO.Front().Value.(*authLockedEntry).lockedUntil
		return false, earliest.Sub(now)
	}
	if len(t.globalRing) > 0 {
		oldest := t.globalRing[t.globalHead]
		if !oldest.IsZero() {
			elapsed := now.Sub(oldest)
			if elapsed < t.globalWindow {
				return false, t.globalWindow - elapsed
			}
		}
	}
	return true, 0
}

// RecordFailure increments failure tracking for rawIP at now and returns true if the key
// is now locked out or the lockout table is saturated.
func (t *AuthLockoutTable) RecordFailure(rawIP string, now time.Time) bool {
	key := RateLimitKey(rawIP)
	t.mu.Lock()
	defer t.mu.Unlock()

	t.pruneExpiredLocked(now)

	if len(t.globalRing) > 0 {
		t.globalRing[t.globalHead] = now
		t.globalHead = (t.globalHead + 1) % len(t.globalRing)
	}

	if _, alreadyLocked := t.locked[key]; alreadyLocked {
		return true
	}
	if len(t.locked) >= t.maxLocked {
		return true // Fail-Closed immediately when locked table is saturated.
	}

	e, ok := t.pending[key]
	if !ok {
		for len(t.pending) >= t.maxPending {
			back := t.pendingLRU.Back()
			if back == nil {
				break
			}
			victim := back.Value.(*authPendingEntry)
			t.pendingLRU.Remove(back)
			delete(t.pending, victim.key)
		}
		e = &authPendingEntry{key: key, lastSeen: now}
		e.elem = t.pendingLRU.PushFront(e)
		t.pending[key] = e
	} else {
		t.pendingLRU.MoveToFront(e.elem)
		if now.Sub(e.lastSeen) > t.lockoutDuration {
			e.failures = 0
		}
		e.lastSeen = now
	}

	e.failures++
	if e.failures < t.maxFailures {
		return false
	}

	// Promote key from pending LRU to monotonic locked FIFO.
	t.pendingLRU.Remove(e.elem)
	delete(t.pending, key)

	if len(t.locked) >= t.maxLocked {
		return true // Fail-Closed without evicting active lockouts.
	}
	le := &authLockedEntry{
		key:         key,
		lockedUntil: now.Add(t.lockoutDuration),
	}
	le.elem = t.lockedFIFO.PushBack(le)
	t.locked[key] = le
	return true
}

// Reset clears any pending or active lockout entry for rawIP upon successful authentication.
func (t *AuthLockoutTable) Reset(rawIP string) {
	key := RateLimitKey(rawIP)
	t.mu.Lock()
	defer t.mu.Unlock()

	if e, ok := t.pending[key]; ok {
		t.pendingLRU.Remove(e.elem)
		delete(t.pending, key)
	}
	if le, ok := t.locked[key]; ok {
		t.lockedFIFO.Remove(le.elem)
		delete(t.locked, key)
	}
}

func (t *AuthLockoutTable) Len() int {
	t.mu.Lock()
	n := len(t.pending) + len(t.locked)
	t.mu.Unlock()
	return n
}

func (t *AuthLockoutTable) ListLen() int {
	t.mu.Lock()
	n := t.pendingLRU.Len() + t.lockedFIFO.Len()
	t.mu.Unlock()
	return n
}
