package agent

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash/fnv"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"webrtc-gateway/pkg/signaling"
	"webrtc-gateway/pkg/state"
)

const (
	visitorAuthMaxFailures       = 10
	visitorAuthWindow            = 5 * time.Minute
	visitorAuthMaxKeys           = 4096
	visitorAuthGlobalMaxFailures = 60
	visitorAuthGlobalWindow      = 1 * time.Minute
	authKeyStripeCount           = 64

	DefaultCapListLimit  = 50
	MaxCapListLimit      = 200
	maxCapabilityJTILen  = 128
	maxCapabilityRecords = 4096
	maxCapabilityRevoked = 8192
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

// VisitorAuthLimiter wraps the shared O(1) signaling.AuthLockoutTable (per-IP lockout-preserving
// LRU/FIFO + global sliding-window failure cap) and provides per-IP attempt serialization to
// prevent concurrent brute-force races across multiple handshaking sessions.
type VisitorAuthLimiter struct {
	table   *signaling.AuthLockoutTable
	stripes [authKeyStripeCount]sync.Mutex
}

func NewVisitorAuthLimiter() *VisitorAuthLimiter {
	return &VisitorAuthLimiter{
		table: signaling.NewAuthLockoutTable(signaling.AuthLockoutConfig{
			MaxFailures:       visitorAuthMaxFailures,
			LockoutDuration:   visitorAuthWindow,
			MaxPendingKeys:    visitorAuthMaxKeys,
			MaxLockedKeys:     visitorAuthMaxKeys,
			GlobalMaxFailures: visitorAuthGlobalMaxFailures,
			GlobalWindow:      visitorAuthGlobalWindow,
		}),
	}
}

// LockKey serializes concurrent authentication attempts originating from the same IP bucket.
func (l *VisitorAuthLimiter) LockKey(rawIP string) func() {
	key := signaling.RateLimitKey(rawIP)
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	stripe := &l.stripes[h.Sum32()%authKeyStripeCount]
	stripe.Lock()
	return stripe.Unlock
}

func (l *VisitorAuthLimiter) Allow(rawIP string, now time.Time) (bool, time.Duration) {
	return l.table.Allow(rawIP, now)
}

func (l *VisitorAuthLimiter) RecordFailure(rawIP string, now time.Time) bool {
	return l.table.RecordFailure(rawIP, now)
}

func (l *VisitorAuthLimiter) Reset(rawIP string) {
	l.table.Reset(rawIP)
}

func (l *VisitorAuthLimiter) Len() int {
	return l.table.Len()
}

func (l *VisitorAuthLimiter) ListLen() int {
	return l.table.ListLen()
}

// ---- Persistent Cross-Process-Safe Capability Registry & Revocation Store ----

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
	mu          sync.Mutex
	store       *state.AtomicFileStore
	version     uint64
	fileModTime time.Time
	fileSize    int64
	records     map[string]*CapabilityRecord
	revoked     map[string]int64 // jti -> exp unix (monotonic union across processes)
	active      map[string]int
}

func NewCapabilityStore() *CapabilityStore {
	s, _ := NewCapabilityStoreWithFile("")
	return s
}

func NewCapabilityStoreWithFile(stateFile string) (*CapabilityStore, error) {
	var store *state.AtomicFileStore
	if stateFile != "" {
		store = state.NewAtomicFileStore(stateFile)
		if err := store.CheckWritable(); err != nil {
			return nil, err
		}
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

func (s *CapabilityStore) lockPath() string {
	if s.store == nil {
		return ""
	}
	return s.store.Path() + ".lock"
}

func parsePersistedCapState(raw []byte) (*persistedCapState, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return &persistedCapState{}, nil
	}
	var st persistedCapState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *CapabilityStore) mergeStateLocked(st *persistedCapState, nowUnix int64) {
	if st == nil {
		return
	}
	for jti, exp := range st.Revoked {
		if jti == "" || len(jti) > maxCapabilityJTILen {
			continue
		}
		if exp > 0 && nowUnix >= exp {
			continue
		}
		if prevExp, exists := s.revoked[jti]; !exists || (prevExp > 0 && (exp == 0 || exp > prevExp)) {
			s.revoked[jti] = exp
		}
		if r, ok := s.records[jti]; ok {
			r.Revoked = true
		}
	}
	for jti, p := range st.Records {
		if jti == "" || len(jti) > maxCapabilityJTILen {
			continue
		}
		_, isRev := s.revoked[jti]
		if nowUnix >= p.Exp && !isRev {
			continue
		}
		if existing, ok := s.records[jti]; ok {
			existing.Payload = p
			existing.Revoked = existing.Revoked || isRev
		} else {
			s.records[jti] = &CapabilityRecord{Payload: p, Revoked: isRev}
		}
	}
	s.pruneExpiredLocked(nowUnix)
}

func (s *CapabilityStore) loadFromDisk() error {
	if s.store == nil || s.store.Path() == "" {
		return nil
	}
	return state.WithFileLock(s.lockPath(), func() error {
		raw, err := os.ReadFile(s.store.Path())
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		st, err := parsePersistedCapState(raw)
		if err != nil {
			return err
		}
		fi, serr := os.Stat(s.store.Path())
		s.mu.Lock()
		s.mergeStateLocked(st, time.Now().Unix())
		if serr == nil {
			s.fileModTime = fi.ModTime()
			s.fileSize = fi.Size()
		}
		s.mu.Unlock()
		return nil
	})
}

// maybeReload performs a fast stat check and merges disk changes (e.g. from CLI -mint-link)
// using a monotonic union for revocations so concurrent processes never lose records or revocations.
func (s *CapabilityStore) maybeReload() {
	if s.store == nil || s.store.Path() == "" {
		return
	}
	fi, err := os.Stat(s.store.Path())
	if err != nil {
		return
	}
	s.mu.Lock()
	unchanged := fi.ModTime().Equal(s.fileModTime) && fi.Size() == s.fileSize
	s.mu.Unlock()
	if unchanged {
		return
	}
	_ = s.loadFromDisk()
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

func (s *CapabilityStore) buildSnapshotLocked(nowUnix int64) (uint64, *persistedCapState) {
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

func (s *CapabilityStore) applyAndPersist(mutate func(nowUnix int64) error) error {
	if s.store == nil {
		s.mu.Lock()
		err := mutate(time.Now().Unix())
		s.mu.Unlock()
		return err
	}
	return state.WithFileLock(s.lockPath(), func() error {
		var diskState *persistedCapState
		if raw, err := os.ReadFile(s.store.Path()); err == nil {
			if st, perr := parsePersistedCapState(raw); perr == nil {
				diskState = st
			}
		} else if !os.IsNotExist(err) {
			return err
		}

		nowUnix := time.Now().Unix()
		s.mu.Lock()
		if diskState != nil {
			s.mergeStateLocked(diskState, nowUnix)
		}
		if err := mutate(nowUnix); err != nil {
			s.mu.Unlock()
			return err
		}
		ver, snap := s.buildSnapshotLocked(nowUnix)
		s.mu.Unlock()

		raw, err := json.Marshal(snap)
		if err != nil {
			log.Printf("[Agent] Marshal capability state failed: %v", err)
			return err
		}
		if err := s.store.Save(ver, raw); err != nil {
			log.Printf("[Agent] Persist capability state failed: %v", err)
			return err
		}
		if fi, serr := os.Stat(s.store.Path()); serr == nil {
			s.mu.Lock()
			s.fileModTime = fi.ModTime()
			s.fileSize = fi.Size()
			s.mu.Unlock()
		}
		return nil
	})
}

func (s *CapabilityStore) Track(p CapabilityPayload) error {
	jti := strings.TrimSpace(p.JTI)
	if jti == "" || len(jti) > maxCapabilityJTILen {
		return errors.New("agent: invalid capability jti")
	}
	p.JTI = jti
	return s.applyAndPersist(func(nowUnix int64) error {
		s.pruneExpiredLocked(nowUnix)
		if _, exists := s.records[p.JTI]; !exists && len(s.records) >= maxCapabilityRecords {
			return errors.New("agent: capability record limit reached")
		}
		_, isRev := s.revoked[p.JTI]
		s.records[p.JTI] = &CapabilityRecord{Payload: p, Revoked: isRev}
		return nil
	})
}

func (s *CapabilityStore) Revoke(jti string) error {
	jti = strings.TrimSpace(jti)
	if jti == "" || len(jti) > maxCapabilityJTILen {
		return errors.New("agent: invalid capability jti")
	}
	return s.applyAndPersist(func(nowUnix int64) error {
		s.pruneExpiredLocked(nowUnix)
		if _, exists := s.revoked[jti]; !exists && len(s.revoked) >= maxCapabilityRevoked {
			return errors.New("agent: capability revocation table limit reached")
		}
		var exp int64
		if r, ok := s.records[jti]; ok {
			r.Revoked = true
			exp = r.Payload.Exp
		}
		s.revoked[jti] = exp
		return nil
	})
}

func (s *CapabilityStore) IsRevoked(jti string) bool {
	s.maybeReload()
	s.mu.Lock()
	_, ok := s.revoked[jti]
	s.mu.Unlock()
	return ok
}

func (s *CapabilityStore) AcquireConn(p *CapabilityPayload) bool {
	s.maybeReload()
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
	cur := s.active[jti]
	if cur == 0 {
		delete(s.active, jti)
	}
	if r, ok := s.records[jti]; ok {
		r.Active = cur
	}
	s.mu.Unlock()
}

// ListPage returns a deterministically sorted, bounded page of capability records so CAP_RESP
// never overflows the control JSON frame limit.
func (s *CapabilityStore) ListPage(now time.Time, offset, limit int) (items []CapabilityRecord, total int, hasMore bool) {
	s.maybeReload()
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
