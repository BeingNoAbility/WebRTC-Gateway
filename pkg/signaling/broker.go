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
	MaxGlobalUnknownWaiting     = 512 // Cap total waiting clients targeting completely unknown nodes
	MaxWaitingPerIPPerNode      = 8   // Cap waiting clients per IP (/32 or /64) per offline node
	MaxClientsPerIPPerNode      = 32  // Cap concurrent clients per IP (/32 or /64) per online node
	brokerCloseAllGraceTimeout  = time.Second
)

var (
	ErrNodeOwnershipConflict = errors.New("signaling: node_id is bound to a different Ed25519 identity key")
	ErrNodeWaitingQueueFull  = errors.New("signaling: offline waiting queue full for node")
	ErrNodeClientLimitFull   = errors.New("signaling: concurrent client limit reached for node")
	ErrBoundKeyCapacityFull  = errors.New("signaling: bound node key registry at capacity")
	ErrBrokerClosed          = errors.New("signaling: broker is shutting down")
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
	closed               bool
	agents               map[string]*Session            // nodeID -> active Agent
	clients              map[string]*Session            // sessionID -> Client
	nodeClients          map[string]map[string]*Session // nodeID -> sessionID -> Client
	boundKeys            map[string]*boundKeyEntry      // nodeID -> Ed25519 public key (no secrets!)
	boundKeysStore       *state.LatestFileWriter
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
	var store *state.LatestFileWriter
	if boundKeysFile != "" {
		store = state.NewLatestFileWriter(boundKeysFile)
		if err := store.CheckWritable(); err != nil {
			cancel()
			return nil, err
		}
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
	raw, err := json.Marshal(snapshot)
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
// PeerStatusDTO broadcasts are enqueued inside b.mu before disk persistence so JoinedDTO
// and online/offline transitions are strictly ordered without adding any new lock.
func (b *SignalBroker) RegisterAgent(sess *Session, pk ed25519.PublicKey, requireVisitorAuth, allowForceRekey bool) error {
	now := time.Now()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrBrokerClosed
	}

	if entry, ok := b.boundKeys[sess.NodeID]; ok {
		if !entry.lastOffline.IsZero() && now.Sub(entry.lastOffline) > DefaultBoundKeyOfflineTTL {
			delete(b.boundKeys, sess.NodeID)
		}
	}

	entry, hasBound := b.boundKeys[sess.NodeID]
	if hasBound {
		if !bytes.Equal(entry.pk, pk) && !allowForceRekey {
			b.mu.Unlock()
			return ErrNodeOwnershipConflict
		}
	} else if len(b.boundKeys) >= MaxBoundNodeKeys {
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

	// Atomically enqueue RegisteredDTO inside b.mu BEFORE mutating b.boundKeys/b.agents or
	// broadcasting PeerStatusDTO so no OfferRelayDTO can ever overtake RegisteredDTO.
	if sess.outCh != nil {
		if err := sess.TrySend(RegisteredDTO{Type: TypeRegistered, NodeID: sess.NodeID}); err != nil {
			b.mu.Unlock()
			return err
		}
	}

	if hasBound {
		if !bytes.Equal(entry.pk, pk) {
			entry.pk = append(ed25519.PublicKey(nil), pk...)
		}
		entry.lastOffline = time.Time{}
	} else {
		b.boundKeys[sess.NodeID] = &boundKeyEntry{pk: append(ed25519.PublicKey(nil), pk...)}
	}

	oldAgent := b.agents[sess.NodeID]
	if oldAgent != nil && oldAgent != sess {
		oldAgent.MarkEvicted()
	}
	b.agents[sess.NodeID] = sess
	sess.MarkRegistered(pk, requireVisitorAuth)
	pkB64 := sess.AgentPKBase64()
	snapVer, snap := b.snapshotBoundKeysLocked()

	// Enqueue PeerStatusDTO inside b.mu (TrySend is lock-free & non-blocking) so it can NEVER
	// race ahead of JoinedDTO or arrive after a subsequent offline CleanupSession!
	if cmap := b.nodeClients[sess.NodeID]; len(cmap) > 0 {
		statusMsg := PeerStatusDTO{
			Type:               TypePeerStatus,
			NodeID:             sess.NodeID,
			Online:             true,
			RequireVisitorAuth: requireVisitorAuth,
			AgentPK:            pkB64,
		}
		for _, c := range cmap {
			_ = c.TrySend(statusMsg)
		}
	}
	b.mu.Unlock()

	b.persistBoundKeys(snapVer, snap)

	if oldAgent != nil && oldAgent != sess {
		oldAgent.SendErrorAndCloseAsync("AGENT_REPLACED", "agent replaced by newer authenticated connection", websocket.StatusCode(4001))
	}
	return nil
}

// JoinClient attaches an immutable client session to its target node and enqueues its initial
// JoinedDTO strictly inside b.mu so no PeerStatusDTO can ever overtake JoinedDTO.
func (b *SignalBroker) JoinClient(sess *Session) (online bool, requireVisitorAuth bool, agentPK string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return false, false, "", ErrBrokerClosed
	}

	agent := b.agents[sess.NodeID]
	cmap := b.nodeClients[sess.NodeID]
	ipKey := RateLimitKey(sess.ClientIP)
	ipCountOnNode := 0
	for _, existing := range cmap {
		if RateLimitKey(existing.ClientIP) == ipKey {
			ipCountOnNode++
		}
	}
	if agent == nil {
		if len(cmap) >= MaxWaitingPerNode || ipCountOnNode >= MaxWaitingPerIPPerNode {
			return false, false, "", ErrNodeWaitingQueueFull
		}
		// Bound waiting clients targeting completely unknown nodes to resist anonymous queue floods.
		if _, known := b.boundKeys[sess.NodeID]; !known {
			unknownWaiters := 0
			for nid, m := range b.nodeClients {
				if b.agents[nid] == nil {
					if _, k := b.boundKeys[nid]; !k {
						unknownWaiters += len(m)
					}
				}
			}
			if unknownWaiters >= MaxGlobalUnknownWaiting {
				return false, false, "", ErrNodeWaitingQueueFull
			}
		}
	} else if len(cmap) >= MaxClientsPerNode || ipCountOnNode >= MaxClientsPerIPPerNode {
		return false, false, "", ErrNodeClientLimitFull
	}

	if cmap == nil {
		cmap = make(map[string]*Session)
		b.nodeClients[sess.NodeID] = cmap
	}
	cmap[sess.ID] = sess
	b.clients[sess.ID] = sess

	if agent != nil {
		online = true
		requireVisitorAuth = agent.RequireVisitorAuth()
		agentPK = agent.AgentPKBase64()
	}
	if sess.outCh != nil {
		if sendErr := sess.TrySend(JoinedDTO{
			Type:               TypeJoined,
			NodeID:             sess.NodeID,
			SessionID:          sess.ID,
			Online:             online,
			RequireVisitorAuth: requireVisitorAuth,
			AgentPK:            agentPK,
		}); sendErr != nil {
			delete(b.clients, sess.ID)
			delete(cmap, sess.ID)
			if len(cmap) == 0 {
				delete(b.nodeClients, sess.NodeID)
			}
			return false, false, "", sendErr
		}
	}
	return online, requireVisitorAuth, agentPK, nil
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

// CleanupSession removes an Agent or Client cleanly and enqueues offline notifications under b.mu.
func (b *SignalBroker) CleanupSession(sess *Session) {
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
				msg := PeerStatusDTO{
					Type:               TypePeerStatus,
					NodeID:             sess.NodeID,
					Online:             false,
					RequireVisitorAuth: false,
				}
				for _, c := range cmap {
					c.ClearOfferPending()
					_ = c.TrySend(msg)
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
		// Apply post-offer maximum lifetime ONLY after an Offer has been sent so online visitors
		// typing their password get the full DefaultOnlineUnOfferTimeout (5m) window!
		if c.HasSentOffer() && maxLife > 0 {
			offerSince := c.FirstOfferAt()
			if offerSince.IsZero() {
				offerSince = c.JoinedAt()
			}
			if now.Sub(offerSince) > maxLife {
				victims = append(victims, timedOutEntry{
					sess:   c,
					code:   "SIGNAL_LIFETIME_EXCEEDED",
					reason: "client signaling session lifetime exceeded",
				})
			}
		}
	}
	b.mu.RUnlock()

	for _, v := range victims {
		v.sess.MarkEvicted()
		v.sess.SendErrorAndCloseAsync(v.code, v.reason, websocket.StatusCode(4008))
		b.CleanupSession(v.sess)
	}
}

// CloseAll gracefully closes every active WebSocket session concurrently with a bounded grace deadline,
// falling back to CloseNow() so unresponsive peers never stall SIGTERM shutdown.
func (b *SignalBroker) CloseAll() {
	b.cancel()
	b.mu.Lock()
	b.closed = true
	var all []*Session
	now := time.Now()
	for nodeID, ag := range b.agents {
		ag.MarkEvicted()
		all = append(all, ag)
		if ent, ok := b.boundKeys[nodeID]; ok {
			ent.lastOffline = now
		}
	}
	for _, cl := range b.clients {
		cl.MarkEvicted()
		all = append(all, cl)
	}
	b.agents = make(map[string]*Session)
	b.clients = make(map[string]*Session)
	b.nodeClients = make(map[string]map[string]*Session)
	snapVer, snap := b.snapshotBoundKeysLocked()
	b.mu.Unlock()

	// Single consolidated flush on shutdown instead of N concurrent fsync storms!
	b.persistBoundKeys(snapVer, snap)

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

	select {
	case <-done:
	case <-time.After(brokerCloseAllGraceTimeout):
		for _, s := range all {
			s.CloseNow()
		}
	}
}
