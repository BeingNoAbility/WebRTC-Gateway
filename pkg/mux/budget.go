package mux

import "sync/atomic"

const (
	DefaultGlobalMemoryBudget  int64 = 256 * 1024 * 1024 // 256 MiB process-wide inbound cap
	DefaultSessionMemoryBudget int64 = 32 * 1024 * 1024  // 32 MiB per-visitor inbound cap
)

// MemoryBudget enforces a lock-free atomic byte ceiling across all streams or one session.
type MemoryBudget struct {
	limit int64
	used  atomic.Int64
}

func NewMemoryBudget(limit int64) *MemoryBudget {
	if limit <= 0 {
		limit = DefaultGlobalMemoryBudget
	}
	return &MemoryBudget{limit: limit}
}

// TryReserve atomically reserves n bytes using an overflow-safe subtraction check.
func (b *MemoryBudget) TryReserve(n int64) bool {
	if b == nil || n <= 0 {
		return true
	}
	for {
		cur := b.used.Load()
		if n > b.limit-cur {
			return false
		}
		if b.used.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

func (b *MemoryBudget) Release(n int64) {
	if b == nil || n <= 0 {
		return
	}
	for {
		cur := b.used.Load()
		next := cur - n
		if next < 0 {
			next = 0
		}
		if b.used.CompareAndSwap(cur, next) {
			return
		}
	}
}

func (b *MemoryBudget) Used() int64 {
	if b == nil {
		return 0
	}
	return b.used.Load()
}
