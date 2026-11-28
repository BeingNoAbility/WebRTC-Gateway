package agent

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

	DefaultCapListLimit       = 50
	MaxCapListLimit           = 200
	maxCapabilityJTILen       = 128
	maxCapabilityRecords      = 4096
	maxCapabilityRevoked      = 8192
	maxCapabilityImmortalRevs = 2048
)

// ErrCapPersistFailed signals that a revocation took effect in memory immediately,
// but atomic persistence to disk failed.
var ErrCapPersistFailed = errors.New("agent: capability state updated in memory but failed to persist to disk")

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
	Revoked map[string]int64             `json:"revoked,omitempty"` // jti -> exp unix (0 = unknown/manual revocation)
}

// CapabilityStore manages share capability records, connection limits, and cross-process-safe
// revocation tombstones.
//
// Concurrency invariant: IsRevoked() is a 100% pure-memory lock-free atomic snapshot read (never
// performs disk I/O or OS flock), so callers holding session mutexes can check revocations safely.
// Disk synchronization happens via MaybeReload() outside session locks and via WriteAtomic under
// WithFileLock on mutating calls.
type CapabilityStore struct {
	stateFile string

	mu          sync.Mutex
	fileInfo    os.FileInfo
	records     map[string]*CapabilityRecord
	revoked     map[string]int64
	active      map[string]int
	revokedSnap atomic.Pointer[map[string]int64]
}

func NewCapabilityStore() *CapabilityStore {
	s, _ := NewCapabilityStoreWithFile("")
	return s
}

func NewCapabilityStoreWithFile(stateFile string) (*CapabilityStore, error) {
	if stateFile != "" {
		if err := state.CheckWritable(stateFile); err != nil {
			return nil, err
		}
	}
	s := &CapabilityStore{
		stateFile: stateFile,
		records:   make(map[string]*CapabilityRecord),
		revoked:   make(map[string]int64),
		active:    make(map[string]int),
	}
	emptySnap := make(map[string]int64)
	s.revokedSnap.Store(&emptySnap)

	if stateFile != "" {
		if err := s.loadFromDisk(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *CapabilityStore) lockPath() string {
	if s.stateFile == "" {
		return ""
	}
	return s.stateFile + ".lock"
}

func (s *CapabilityStore) publishRevokedLocked() {
	cp := make(map[string]int64, len(s.revoked))
	for k, v := range s.revoked {
		cp[k] = v
	}
	s.revokedSnap.Store(&cp)
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
	s.publishRevokedLocked()
}

func (s *CapabilityStore) loadFromDisk() error {
	if s.stateFile == "" {
		return nil
	}
	return state.WithFileLock(s.lockPath(), func() error {
		raw, err := os.ReadFile(s.stateFile) // #nosec G304 -- operator-configured state file path
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		st, err := parsePersistedCapState(raw)
		if err != nil {
			return fmt.Errorf("agent: corrupt capability state file %q: %w", s.stateFile, err)
		}
		fi, serr := os.Stat(s.stateFile)
		s.mu.Lock()
		s.mergeStateLocked(st, time.Now().Unix())
		if serr == nil {
			s.fileInfo = fi
		}
		s.mu.Unlock()
		return nil
	})
}

// MaybeReload checks stat (inode/SameFile + mtime + size) and merges external disk updates
// (e.g. from CLI -mint-link) without blocking session locks.
func (s *CapabilityStore) MaybeReload() {
	if s.stateFile == "" {
		return
	}
	fi, err := os.Stat(s.stateFile)
	if err != nil {
		return
	}
	s.mu.Lock()
	unchanged := s.fileInfo != nil &&
		os.SameFile(s.fileInfo, fi) &&
		fi.ModTime().Equal(s.fileInfo.ModTime()) &&
		fi.Size() == s.fileInfo.Size()
	s.mu.Unlock()
	if unchanged {
		return
	}
	if err := s.loadFromDisk(); err != nil {
		log.Printf("[Agent] Reload capability state failed: %v", err)
	}
}

func (s *CapabilityStore) pruneExpiredLocked(nowUnix int64) bool {
	revChanged := false
	for jti, exp := range s.revoked {
		if exp > 0 && nowUnix >= exp {
			delete(s.revoked, jti)
			revChanged = true
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
	return revChanged
}

// PruneExpired garbage-collects expired capability records and expired revocation tombstones in memory.
func (s *CapabilityStore) PruneExpired(now time.Time) {
	s.mu.Lock()
	if s.pruneExpiredLocked(now.Unix()) {
		s.publishRevokedLocked()
	}
	s.mu.Unlock()
}

func (s *CapabilityStore) buildSnapshotLocked(nowUnix int64) *persistedCapState {
	s.pruneExpiredLocked(nowUnix)
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
	return out
}

func (s *CapabilityStore) applyAndPersist(mutate func(nowUnix int64) error, isRevoke bool) error {
	if s.stateFile == "" {
		s.mu.Lock()
		err := mutate(time.Now().Unix())
		if err == nil {
			s.publishRevokedLocked()
		}
		s.mu.Unlock()
		return err
	}
	return state.WithFileLock(s.lockPath(), func() error {
		var diskState *persistedCapState
		rawDisk, rerr := os.ReadFile(s.stateFile) // #nosec G304 -- operator-configured state file path
		if rerr == nil {
			st, perr := parsePersistedCapState(rawDisk)
			if perr != nil {
				// Fail-Closed: NEVER silently overwrite corrupt disk revocation history!
				return fmt.Errorf("agent: refusing to overwrite corrupt capability state %q: %w", s.stateFile, perr)
			}
			diskState = st
		} else if !os.IsNotExist(rerr) {
			return rerr
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
		s.publishRevokedLocked()
		snap := s.buildSnapshotLocked(nowUnix)
		s.mu.Unlock()

		raw, err := json.Marshal(snap)
		if err != nil {
			log.Printf("[Agent] Marshal capability state failed: %v", err)
			if isRevoke {
				return fmt.Errorf("%w: %v", ErrCapPersistFailed, err)
			}
			return err
		}
		if err := state.WriteAtomic(s.stateFile, raw, 0600); err != nil {
			log.Printf("[Agent] Persist capability state failed: %v", err)
			if isRevoke {
				return fmt.Errorf("%w: %v", ErrCapPersistFailed, err)
			}
			return err
		}
		if fi, serr := os.Stat(s.stateFile); serr == nil {
			s.mu.Lock()
			s.fileInfo = fi
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
	}, false)
}

func (s *CapabilityStore) Revoke(jti string) error {
	jti = strings.TrimSpace(jti)
	if jti == "" || len(jti) > maxCapabilityJTILen {
		return errors.New("agent: invalid capability jti")
	}
	return s.applyAndPersist(func(nowUnix int64) error {
		s.pruneExpiredLocked(nowUnix)
		var exp int64
		if r, ok := s.records[jti]; ok {
			r.Revoked = true
			exp = r.Payload.Exp
		}
		if _, exists := s.revoked[jti]; !exists {
			if len(s.revoked) >= maxCapabilityRevoked {
				return errors.New("agent: capability revocation table limit reached")
			}
			if exp == 0 {
				immortalCount := 0
				for _, e := range s.revoked {
					if e == 0 {
						immortalCount++
					}
				}
				if immortalCount >= maxCapabilityImmortalRevs {
					return errors.New("agent: unknown capability revocation limit reached")
				}
			}
		}
		s.revoked[jti] = exp
		return nil
	}, true)
}

// IsRevoked performs a lock-free pure-memory lookup against the latest published revocation snapshot.
// Never performs disk I/O or OS flock; safe to invoke while holding session mutexes.
func (s *CapabilityStore) IsRevoked(jti string) bool {
	if jti == "" {
		return false
	}
	snap := s.revokedSnap.Load()
	if snap == nil {
		return false
	}
	exp, ok := (*snap)[jti]
	if !ok {
		return false
	}
	return exp == 0 || time.Now().Unix() < exp
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
	cur := s.active[jti]
	if cur == 0 {
		delete(s.active, jti)
	}
	if r, ok := s.records[jti]; ok {
		r.Active = cur
	}
	s.mu.Unlock()
}

// ListPage returns a deterministically sorted, bounded page of capability records.
func (s *CapabilityStore) ListPage(now time.Time, offset, limit int) (items []CapabilityRecord, total int, hasMore bool) {
	s.MaybeReload()
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
