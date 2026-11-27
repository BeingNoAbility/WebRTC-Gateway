package agent

import (
	"bytes"
	"container/list"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	"webrtc-gateway/pkg/signaling"
	"webrtc-gateway/pkg/state"
)

const (
	visitorAuthMaxFailures = 10
	visitorAuthWindow      = 5 * time.Minute
	visitorAuthMaxKeys     = 4096

	DefaultCapListLimit = 50
	MaxCapListLimit     = 200
)

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func randomTokenB64(nBytes int) string {
	return base64.RawURLEncoding.EncodeToString(randomBytes(nBytes))
}

// ---- O(1) Leak-Free Lockout-Preserving VisitorAuthLimiter ----

type authLimitEntry struct {
	key         string
	failures    int
	lockedUntil time.Time
	lastSeen    time.Time
	elem        *list.Element
}

// VisitorAuthLimiter tracks failed authentication attempts per client IP (/32 IPv4, /64 IPv6)
// and NEVER evicts actively locked-out entries during LRU pressure. Fails closed for unknown IPs
// when every slot holds an active unexpired lockout.
type VisitorAuthLimiter struct {
	mu             sync.Mutex
	entries        map[string]*authLimitEntry
	lru            *list.List
	maxKeys        int
	saturatedUntil time.Time
}

func NewVisitorAuthLimiter() *VisitorAuthLimiter {
	return &VisitorAuthLimiter{
		entries: make(map[string]*authLimitEntry),
		lru:     list.New(),
		maxKeys: visitorAuthMaxKeys,
	}
}

func (l *VisitorAuthLimiter) sweepUnlockedLocked(now time.Time) (evicted bool, earliestLock time.Time) {
	for elem := l.lru.Back(); elem != nil; {
		prev := elem.Prev()
		v := elem.Value.(*authLimitEntry)
		isLocked := !v.lockedUntil.IsZero() && now.Before(v.lockedUntil)
		if !isLocked {
			l.lru.Remove(elem)
			delete(l.entries, v.key)
			evicted = true
			if len(l.entries) < l.maxKeys {
				break
			}
		} else if earliestLock.IsZero() || v.lockedUntil.Before(earliestLock) {
			earliestLock = v.lockedUntil
		}
		elem = prev
	}
	return evicted, earliestLock
}

func (l *VisitorAuthLimiter) Allow(rawIP string, now time.Time) (bool, time.Duration) {
	key := signaling.RateLimitKey(rawIP)
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		if len(l.entries) >= l.maxKeys {
			if !l.saturatedUntil.IsZero() && now.Before(l.saturatedUntil) {
				return false, l.saturatedUntil.Sub(now)
			}
			evicted, earliest := l.sweepUnlockedLocked(now)
			if !evicted && len(l.entries) >= l.maxKeys {
				if earliest.IsZero() {
					earliest = now.Add(visitorAuthWindow)
				}
				l.saturatedUntil = earliest
				return false, earliest.Sub(now)
			}
			l.saturatedUntil = time.Time{}
		}
		return true, 0
	}
	if !e.lockedUntil.IsZero() && now.Before(e.lockedUntil) {
		return false, e.lockedUntil.Sub(now)
	}
	if !e.lockedUntil.IsZero() && !now.Before(e.lockedUntil) {
		l.lru.Remove(e.elem)
		delete(l.entries, key)
		l.saturatedUntil = time.Time{}
	}
	return true, 0
}

func (l *VisitorAuthLimiter) RecordFailure(rawIP string, now time.Time) bool {
	key := signaling.RateLimitKey(rawIP)
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok {
		if len(l.entries) >= l.maxKeys {
			evicted, earliest := l.sweepUnlockedLocked(now)
			if !evicted && len(l.entries) >= l.maxKeys {
				if earliest.IsZero() {
					earliest = now.Add(visitorAuthWindow)
				}
				l.saturatedUntil = earliest
				return true // Fail-Closed when all slots hold active lockouts.
			}
			l.saturatedUntil = time.Time{}
		}
		e = &authLimitEntry{key: key, lastSeen: now}
		e.elem = l.lru.PushFront(e)
		l.entries[key] = e
	} else {
		l.lru.MoveToFront(e.elem)
		if now.Sub(e.lastSeen) > visitorAuthWindow {
			e.failures = 0
			e.lockedUntil = time.Time{}
		}
		e.lastSeen = now
	}
	e.failures++
	if e.failures >= visitorAuthMaxFailures {
		e.lockedUntil = now.Add(visitorAuthWindow)
		return true
	}
	return false
}

func (l *VisitorAuthLimiter) Reset(rawIP string) {
	key := signaling.RateLimitKey(rawIP)
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[key]; ok {
		l.lru.Remove(e.elem)
		delete(l.entries, key)
		l.saturatedUntil = time.Time{}
	}
}

func (l *VisitorAuthLimiter) Len() int {
	l.mu.Lock()
	n := len(l.entries)
	l.mu.Unlock()
	return n
}

func (l *VisitorAuthLimiter) ListLen() int {
	l.mu.Lock()
	n := l.lru.Len()
	l.mu.Unlock()
	return n
}

// ---- Persistent Capability Registry & Revocation Store ----

type CapabilityRecord struct {
	Payload CapabilityPayload `json:"payload"`
	Revoked bool              `json:"revoked"`
	Active  int               `json:"active_conns"`
}

type persistedCapState struct {
	Records map[string]CapabilityPayload `json:"records,omitempty"`
	Revoked map[string]int64             `json:"revoked,omitempty"` // jti -> exp unix (0 = keep until pruned)
}

type CapabilityStore struct {
	mu      sync.Mutex
	store   *state.AtomicFileStore
	version uint64
	records map[string]*CapabilityRecord
	revoked map[string]int64 // jti -> exp unix (for automatic post-expiry GC)
	active  map[string]int
}

func NewCapabilityStore() *CapabilityStore {
	s, _ := NewCapabilityStoreWithFile("")
	return s
}

func NewCapabilityStoreWithFile(stateFile string) (*CapabilityStore, error) {
	var store *state.AtomicFileStore
	if stateFile != "" {
		store = state.NewAtomicFileStore(stateFile)
	}
	s := &CapabilityStore{
		store:   store,
		records: make(map[string]*CapabilityRecord),
		revoked: make(map[string]int64),
		active:  make(map[string]int),
	}
	if stateFile != "" {
		if err := s.loadFromDisk(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *CapabilityStore) loadFromDisk() error {
	if s.store == nil || s.store.Path() == "" {
		return nil
	}
	raw, err := os.ReadFile(s.store.Path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var st persistedCapState
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	nowUnix := time.Now().Unix()
	for jti, exp := range st.Revoked {
		if exp > 0 && nowUnix >= exp {
			continue
		}
		s.revoked[jti] = exp
	}
	for jti, p := range st.Records {
		_, isRev := s.revoked[jti]
		if nowUnix >= p.Exp && !isRev {
			continue
		}
		s.records[jti] = &CapabilityRecord{Payload: p, Revoked: isRev}
	}
	return nil
}

func (s *CapabilityStore) pruneExpiredLocked(nowUnix int64) {
	for jti, exp := range s.revoked {
		if exp > 0 && nowUnix >= exp {
			delete(s.revoked, jti)
		}
	}
	for jti, r := range s.records {
		if nowUnix >= r.Payload.Exp {
			delete(s.records, jti)
			if s.active[jti] == 0 {
				delete(s.active, jti)
			}
		}
	}
}

// PruneExpired garbage-collects expired capability records and expired revocation tombstones in memory.
func (s *CapabilityStore) PruneExpired(now time.Time) {
	s.mu.Lock()
	s.pruneExpiredLocked(now.Unix())
	s.mu.Unlock()
}

func (s *CapabilityStore) snapshotLocked() (uint64, *persistedCapState) {
	if s.store == nil {
		return 0, nil
	}
	nowUnix := time.Now().Unix()
	s.pruneExpiredLocked(nowUnix)
	s.version++
	ver := s.version
	out := &persistedCapState{
		Records: make(map[string]CapabilityPayload, len(s.records)),
		Revoked: make(map[string]int64, len(s.revoked)),
	}
	for jti, exp := range s.revoked {
		out.Revoked[jti] = exp
	}
	for jti, r := range s.records {
		out.Records[jti] = r.Payload
	}
	return ver, out
}

func (s *CapabilityStore) persist(ver uint64, snap *persistedCapState) error {
	if s.store == nil || snap == nil {
		return nil
	}
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		log.Printf("[Agent] Marshal capability state failed: %v", err)
		return err
	}
	if err := s.store.Save(ver, raw); err != nil {
		log.Printf("[Agent] Persist capability state failed: %v", err)
		return err
	}
	return nil
}

func (s *CapabilityStore) Track(p CapabilityPayload) error {
	s.mu.Lock()
	s.records[p.JTI] = &CapabilityRecord{Payload: p}
	ver, snap := s.snapshotLocked()
	s.mu.Unlock()
	return s.persist(ver, snap)
}

func (s *CapabilityStore) Revoke(jti string) error {
	s.mu.Lock()
	var exp int64
	if r, ok := s.records[jti]; ok {
		r.Revoked = true
		exp = r.Payload.Exp
	}
	s.revoked[jti] = exp
	ver, snap := s.snapshotLocked()
	s.mu.Unlock()
	return s.persist(ver, snap)
}

func (s *CapabilityStore) IsRevoked(jti string) bool {
	s.mu.Lock()
	_, ok := s.revoked[jti]
	s.mu.Unlock()
	return ok
}

func (s *CapabilityStore) AcquireConn(p *CapabilityPayload) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, rev := s.revoked[p.JTI]; rev {
		return false
	}
	if p.MaxConns > 0 && s.active[p.JTI] >= p.MaxConns {
		return false
	}
	s.active[p.JTI]++
	if r, ok := s.records[p.JTI]; ok {
		r.Active = s.active[p.JTI]
	}
	return true
}

func (s *CapabilityStore) ReleaseConn(jti string) {
	if jti == "" {
		return
	}
	s.mu.Lock()
	if s.active[jti] > 0 {
		s.active[jti]--
	}
	if r, ok := s.records[jti]; ok {
		r.Active = s.active[jti]
	}
	s.mu.Unlock()
}

// ListPage returns a deterministically sorted, bounded page of capability records so CAP_RESP
// never overflows the 32 KiB control JSON frame limit.
func (s *CapabilityStore) ListPage(now time.Time, offset, limit int) (items []CapabilityRecord, total int, hasMore bool) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = DefaultCapListLimit
	}
	if limit > MaxCapListLimit {
		limit = MaxCapListLimit
	}

	s.mu.Lock()
	all := make([]CapabilityRecord, 0, len(s.records))
	nowUnix := now.Unix()
	for jti, r := range s.records {
		if nowUnix >= r.Payload.Exp && !r.Revoked {
			continue
		}
		cp := *r
		cp.Active = s.active[jti]
		all = append(all, cp)
	}
	s.mu.Unlock()

	sort.Slice(all, func(i, j int) bool {
		if all[i].Payload.Exp != all[j].Payload.Exp {
			return all[i].Payload.Exp > all[j].Payload.Exp
		}
		return all[i].Payload.JTI < all[j].Payload.JTI
	})

	total = len(all)
	if offset >= total {
		return []CapabilityRecord{}, total, false
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return all[offset:end], total, end < total
}
