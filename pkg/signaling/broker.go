package signaling

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"

	"webrtc-gateway/pkg/state"
)

const (
	DefaultHalfOpenTimeout      = 30 * time.Second
	DefaultOfflineWaitTimeout   = 60 * time.Second
	DefaultOnlineUnOfferTimeout = 5 * time.Minute
	DefaultPostOfferIdleTimeout = 45 * time.Second
	DefaultMaxClientLifetime    = 2 * time.Minute
	DefaultBoundKeyOfflineTTL   = 7 * 24 * time.Hour
	MaxBoundNodeKeys            = 10000
	brokerCloseAllGraceTimeout  = 2 * time.Second
)

var (
	ErrNodeOwnershipConflict = errors.New("signaling: node_id is bound to a different Ed25519 identity key")
	ErrNodeWaitingQueueFull  = errors.New("signaling: offline waiting queue full for node")
	ErrNodeClientLimitFull   = errors.New("signaling: concurrent client limit reached for node")
	ErrBoundKeyCapacityFull  = errors.New("signaling: bound node key registry at capacity")
)

type boundKeyEntry struct {
	pk          ed25519.PublicKey
	lastOffline time.Time // zero while agent is actively online
}

type persistedBoundKey struct {
	PK          string `json:"pk"`
	LastOffline int64  `json:"last_offline,omitempty"`
}

// SignalBroker coordinates registered Agents, visitor Clients, and Ed25519 public-key ownership.
type SignalBroker struct {
	mu                   sync.RWMutex
	agents               map[string]*Session            // nodeID -> active Agent
	clients              map[string]*Session            // sessionID -> Client
	nodeClients          map[string]map[string]*Session // nodeID -> sessionID -> Client
	boundKeys            map[string]*boundKeyEntry      // nodeID -> Ed25519 public key (no secrets!)
	boundKeysStore       *state.AtomicFileStore
	boundKeysVer         uint64
	halfOpenTimeout      time.Duration
	offlineWaitTimeout   time.Duration
	onlineUnOfferTimeout time.Duration
	postOfferIdleTimeout time.Duration
	maxClientLifetime    time.Duration
	ctx                  context.Context
	cancel               context.CancelFunc
}

func NewSignalBroker() *SignalBroker {
	b, _ := NewSignalBrokerWithFile("")
	return b
}

func NewSignalBrokerWithFile(boundKeysFile string) (*SignalBroker, error) {
	ctx, cancel := context.WithCancel(context.Background())
	var store *state.AtomicFileStore
	if boundKeysFile != "" {
		store = state.NewAtomicFileStore(boundKeysFile)
	}
	b := &SignalBroker{
		agents:               make(map[string]*Session),
		clients:              make(map[string]*Session),
		nodeClients:          make(map[string]map[string]*Session),
		boundKeys:            make(map[string]*boundKeyEntry),
		boundKeysStore:       store,
		halfOpenTimeout:      DefaultHalfOpenTimeout,
		offlineWaitTimeout:   DefaultOfflineWaitTimeout,
		onlineUnOfferTimeout: DefaultOnlineUnOfferTimeout,
		postOfferIdleTimeout: DefaultPostOfferIdleTimeout,
		maxClientLifetime:    DefaultMaxClientLifetime,
		ctx:                  ctx,
		cancel:               cancel,
	}
	if boundKeysFile != "" {
		if err := b.loadBoundKeysLocked(); err != nil {
			cancel()
			return nil, err
		}
	}
	go b.reaperLoop()
	return b, nil
}

func (b *SignalBroker) loadBoundKeysLocked() error {
	if b.boundKeysStore == nil || b.boundKeysStore.Path() == "" {
		return nil
	}
	raw, err := os.ReadFile(b.boundKeysStore.Path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var disk map[string]persistedBoundKey
	if err := json.Unmarshal(raw, &disk); err != nil {
		return err
	}
	now := time.Now()
	for nodeID, entry := range disk {
		if len(b.boundKeys) >= MaxBoundNodeKeys {
			break
		}
		normID, ok := NormalizeAndValidateNodeID(nodeID)
		if !ok {
			continue
		}
		pkBytes, err := base64.RawURLEncoding.DecodeString(entry.PK)
		if err != nil || len(pkBytes) != ed25519.PublicKeySize {
			continue
		}
		lastOff := now
		if entry.LastOffline > 0 {
			lastOff = time.Unix(entry.LastOffline, 0)
		}
		if now.Sub(lastOff) > DefaultBoundKeyOfflineTTL {
			continue
		}
		b.boundKeys[normID] = &boundKeyEntry{
			pk:          ed25519.PublicKey(pkBytes),
			lastOffline: lastOff,
		}
	}
	return nil
}

func (b *SignalBroker) snapshotBoundKeysLocked() (uint64, map[string]persistedBoundKey) {
	if b.boundKeysStore == nil {
		return 0, nil
	}
	b.boundKeysVer++
	ver := b.boundKeysVer
	now := time.Now()
	out := make(map[string]persistedBoundKey, len(b.boundKeys))
	for nodeID, ent := range b.boundKeys {
		var ts int64
		if !ent.lastOffline.IsZero() {
			if now.Sub(ent.lastOffline) > DefaultBoundKeyOfflineTTL {
				continue
			}
			ts = ent.lastOffline.Unix()
		}
		out[nodeID] = persistedBoundKey{
			PK:          base64.RawURLEncoding.EncodeToString(ent.pk),
			LastOffline: ts,
		}
	}
	return ver, out
}

func (b *SignalBroker) persistBoundKeys(ver uint64, snapshot map[string]persistedBoundKey) {
	if b.boundKeysStore == nil || snapshot == nil {
		return
	}
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		log.Printf("[Signal] Marshal bound keys failed: %v", err)
		return
	}
	if err := b.boundKeysStore.Save(ver, raw); err != nil {
		log.Printf("[Signal] Persist bound keys failed: %v", err)
	}
}

func (b *SignalBroker) AgentCount() int {
	b.mu.RLock()
	n := len(b.agents)
	b.mu.RUnlock()
	return n
}

func (b *SignalBroker) ClientCount() int {
	b.mu.RLock()
	n := len(b.clients)
	b.mu.RUnlock()
	return n
}

// RegisterAgent binds a verified Ed25519 public key to nodeID, evicting any prior socket of
// the SAME identity key (or rotating when allowForceRekey is true).
// NEVER evicts unexpired offline bindings within the 7-day TTL when at capacity!
func (b *SignalBroker) RegisterAgent(sess *Session, pk ed25519.PublicKey, requireVisitorAuth, allowForceRekey bool) error {
	now := time.Now()
	b.mu.Lock()

	// Prune expired offline key bindings for this node first.
	if entry, ok := b.boundKeys[sess.NodeID]; ok {
		if !entry.lastOffline.IsZero() && now.Sub(entry.lastOffline) > DefaultBoundKeyOfflineTTL {
			delete(b.boundKeys, sess.NodeID)
		}
	}

	if entry, ok := b.boundKeys[sess.NodeID]; ok {
		if !bytes.Equal(entry.pk, pk) {
			if !allowForceRekey {
				b.mu.Unlock()
				return ErrNodeOwnershipConflict
			}
			entry.pk = append(ed25519.PublicKey(nil), pk...)
		}
		entry.lastOffline = time.Time{}
	} else {
		if len(b.boundKeys) >= MaxBoundNodeKeys {
			// Sweep ONLY expired offline bindings (> 7d TTL). NEVER evict live or unexpired offline keys!
			for nid, ent := range b.boundKeys {
				if _, online := b.agents[nid]; online {
					continue
				}
				if !ent.lastOffline.IsZero() && now.Sub(ent.lastOffline) > DefaultBoundKeyOfflineTTL {
					delete(b.boundKeys, nid)
				}
			}
			if len(b.boundKeys) >= MaxBoundNodeKeys {
				b.mu.Unlock()
				return ErrBoundKeyCapacityFull
			}
		}
		b.boundKeys[sess.NodeID] = &boundKeyEntry{pk: append(ed25519.PublicKey(nil), pk...)}
	}

	oldAgent := b.agents[sess.NodeID]
	b.agents[sess.NodeID] = sess
	sess.MarkRegistered(pk, requireVisitorAuth)
	pkB64 := sess.AgentPKBase64()
	snapVer, snap := b.snapshotBoundKeysLocked()

	var notifyList []*Session
	if cmap := b.nodeClients[sess.NodeID]; len(cmap) > 0 {
		notifyList = make([]*Session, 0, len(cmap))
		for _, c := range cmap {
			notifyList = append(notifyList, c)
		}
	}
	b.mu.Unlock()

	b.persistBoundKeys(snapVer, snap)

	if oldAgent != nil && oldAgent != sess {
		oldAgent.SendErrorAndCloseAsync("AGENT_REPLACED", "agent replaced by newer authenticated connection", websocket.StatusCode(4001))
	}

	statusMsg := PeerStatusDTO{
		Type:               TypePeerStatus,
		NodeID:             sess.NodeID,
		Online:             true,
		RequireVisitorAuth: requireVisitorAuth,
		AgentPK:            pkB64,
	}
	for _, c := range notifyList {
		_ = c.TrySend(statusMsg)
	}
	return nil
}

// JoinClient attaches an immutable client session to its target node.
func (b *SignalBroker) JoinClient(sess *Session) (online bool, requireVisitorAuth bool, agentPK string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	agent := b.agents[sess.NodeID]
	cmap := b.nodeClients[sess.NodeID]
	if agent == nil && len(cmap) >= MaxWaitingPerNode {
		return false, false, "", ErrNodeWaitingQueueFull
	}
	if agent != nil && len(cmap) >= MaxClientsPerNode {
		return false, false, "", ErrNodeClientLimitFull
	}
	if cmap == nil {
		cmap = make(map[string]*Session)
		b.nodeClients[sess.NodeID] = cmap
	}
	cmap[sess.ID] = sess
	b.clients[sess.ID] = sess

	if agent != nil {
		return true, agent.RequireVisitorAuth(), agent.AgentPKBase64(), nil
	}
	return false, false, "", nil
}

// GetAgent returns the active Agent for nodeID.
func (b *SignalBroker) GetAgent(nodeID string) *Session {
	b.mu.RLock()
	ag := b.agents[nodeID]
	b.mu.RUnlock()
	return ag
}

// GetClientForNode returns a Client session only if it belongs to nodeID.
func (b *SignalBroker) GetClientForNode(nodeID, sessionID string) *Session {
	b.mu.RLock()
	c := b.clients[sessionID]
	b.mu.RUnlock()
	if c == nil || c.NodeID != nodeID {
		return nil
	}
	return c
}

// CleanupSession removes an Agent or Client cleanly.
func (b *SignalBroker) CleanupSession(sess *Session) {
	var offlineNotify []*Session
	var snapVer uint64
	var snap map[string]persistedBoundKey

	b.mu.Lock()
	switch sess.Role {
	case RoleAgent:
		if cur, ok := b.agents[sess.NodeID]; ok && cur == sess {
			delete(b.agents, sess.NodeID)
			if ent, ok := b.boundKeys[sess.NodeID]; ok {
				ent.lastOffline = time.Now()
			}
			snapVer, snap = b.snapshotBoundKeysLocked()
			if cmap := b.nodeClients[sess.NodeID]; len(cmap) > 0 {
				offlineNotify = make([]*Session, 0, len(cmap))
				for _, c := range cmap {
					c.ClearOfferPending()
					offlineNotify = append(offlineNotify, c)
				}
			}
		}
	case RoleClient:
		delete(b.clients, sess.ID)
		if cmap := b.nodeClients[sess.NodeID]; cmap != nil {
			delete(cmap, sess.ID)
			if len(cmap) == 0 {
				delete(b.nodeClients, sess.NodeID)
			}
		}
	}
	b.mu.Unlock()

	b.persistBoundKeys(snapVer, snap)

	if len(offlineNotify) > 0 {
		msg := PeerStatusDTO{
			Type:               TypePeerStatus,
			NodeID:             sess.NodeID,
			Online:             false,
			RequireVisitorAuth: false,
		}
		for _, c := range offlineNotify {
			_ = c.TrySend(msg)
		}
	}
}

func (b *SignalBroker) reaperLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case now := <-ticker.C:
			b.reapTimedOutClients(now)
		}
	}
}

func (b *SignalBroker) reapTimedOutClients(now time.Time) {
	type timedOutEntry struct {
		sess   *Session
		code   string
		reason string
	}
	var victims []timedOutEntry

	b.mu.RLock()
	hoTimeout := b.halfOpenTimeout
	owTimeout := b.offlineWaitTimeout
	onIdleTimeout := b.onlineUnOfferTimeout
	postOfferIdle := b.postOfferIdleTimeout
	maxLife := b.maxClientLifetime
	for _, c := range b.clients {
		if pendingAt := c.OfferPendingSince(); !pendingAt.IsZero() && now.Sub(pendingAt) > hoTimeout {
			victims = append(victims, timedOutEntry{
				sess:   c,
				code:   "SDP_TIMEOUT",
				reason: "SDP negotiation timed out",
			})
			continue
		}
		if b.agents[c.NodeID] == nil && !c.HasSentOffer() && now.Sub(c.JoinedAt()) > owTimeout {
			victims = append(victims, timedOutEntry{
				sess:   c,
				code:   "OFFLINE_WAIT_TIMEOUT",
				reason: "timed out waiting for offline node",
			})
			continue
		}
		if b.agents[c.NodeID] != nil && !c.HasSentOffer() && now.Sub(c.JoinedAt()) > onIdleTimeout {
			victims = append(victims, timedOutEntry{
				sess:   c,
				code:   "IDLE_CLIENT_TIMEOUT",
				reason: "idle client never initiated offer",
			})
			continue
		}
		if c.HasSentOffer() && postOfferIdle > 0 && now.Sub(c.LastActivity()) > postOfferIdle {
			victims = append(victims, timedOutEntry{
				sess:   c,
				code:   "SIGNAL_IDLE_TIMEOUT",
				reason: "client signaling socket idle after handshake",
			})
			continue
		}
		if maxLife > 0 && now.Sub(c.JoinedAt()) > maxLife {
			victims = append(victims, timedOutEntry{
				sess:   c,
				code:   "SIGNAL_LIFETIME_EXCEEDED",
				reason: "client signaling session lifetime exceeded",
			})
		}
	}
	b.mu.RUnlock()

	for _, v := range victims {
		v.sess.SendErrorAndCloseAsync(v.code, v.reason, websocket.StatusCode(4008))
		b.CleanupSession(v.sess)
	}
}

// CloseAll gracefully closes every active WebSocket session concurrently with a bounded grace deadline,
// falling back to CloseNow() so unresponsive peers never stall SIGTERM shutdown.
func (b *SignalBroker) CloseAll() {
	b.cancel()
	b.mu.Lock()
	var all []*Session
	for _, ag := range b.agents {
		all = append(all, ag)
	}
	for _, cl := range b.clients {
		all = append(all, cl)
	}
	b.agents = make(map[string]*Session)
	b.clients = make(map[string]*Session)
	b.nodeClients = make(map[string]map[string]*Session)
	b.mu.Unlock()

	if len(all) == 0 {
		return
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, s := range all {
		wg.Add(1)
		go func(sess *Session) {
			defer wg.Done()
			sess.Close(websocket.StatusGoingAway, "server shutting down")
		}(s)
	}
	go func() {
		wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(brokerCloseAllGraceTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		for _, s := range all {
			s.CloseNow()
		}
	}
}
