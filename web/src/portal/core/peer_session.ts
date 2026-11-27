/**
 * PeerSession — WebRTC / SDP / ICE / DTLS Lifecycle & Unified Silent Reconnect
 * - Hooks `SignalingClient.onJoined` for seamless signaling-reconnect recovery.
 * - Single-flight connection lock (`pcInFlight`) prevents duplicate PeerConnections.
 * - Buffers early local `onicecandidate` emissions until after `sendOffer` succeeds.
 * - Synchronously buffers control DC frames inside `ctrlDC.onopen` before async DTLS cert verify
 *   so early `SETTINGS`/`AUTH_CHALLENGE` frames are never dropped.
 * - Unified silent reconnect rebuilds a clean PeerConnection & PoolMux while preserving `appFrame`
 *   and reusing RAM-cached `(w0, w1)` + `cookieResumeToken` via `AuthClient`.
 */

import {
  CONTROL_CHANNEL_ID,
  CONTROL_CHANNEL_LABEL,
  DEFAULT_CHUNK_SIZE,
  DEFAULT_POOL_SIZE,
  FrameType,
  WS_CHANNEL_LABEL,
  poolChannelId,
  poolChannelLabel,
  type Frame,
} from '../../protocol/frame';
import { PoolMux, type DataChannelLike } from '../../protocol/mux';
import { base64UrlDecode, randomBase64Url } from '../../shared/crypto';
import { verifyPinnedOrTofuKey, type KeyVerifyResult } from '../../shared/identity';
import {
  extractSessionDescriptor,
  synthesizeSDP,
  validateSessionDescriptor,
  verifyAnswerTranscript,
  verifyPeerDtlsCertificate,
  type SessionDescriptor,
} from '../../shared/sdp';
import { AuthClient, type AuthSuccessInfo, type CapMintResult } from './auth_client';
import { buildIceServers, type IcePreferences } from './config';
import {
  SignalingClient,
  type SignalAnswerMsg,
  type SignalCandidateMsg,
  type SignalJoinedMsg,
} from './signaling_client';

export type { AuthSuccessInfo } from './auth_client';

const textEncoder = new TextEncoder();
const SILENT_RECONNECT_INITIAL_MS = 500;
const SILENT_RECONNECT_MAX_MS = 10_000;
const DISCONNECT_GRACE_MS = 2_500;
const INITIAL_HANDSHAKE_TIMEOUT_MS = 15_000;
const RECONNECT_HANDSHAKE_TIMEOUT_MS = 10_000;

export interface PeerSessionCallbacks {
  onStatus: (status: string, detail?: string) => void;
  onLog: (line: string) => void;
  onAuthRequired: (nodeId: string) => void;
  onAuthFailed: (code: string, message: string, retryAfterSec?: number) => void;
  onKeyMismatch: (result: KeyVerifyResult, isPinned: boolean) => void;
  onAuthenticated: (info: AuthSuccessInfo, mux: PoolMux) => void;
  onDisconnected: (reason: string, willReconnect: boolean) => void;
}

export class PeerSession {
  private generation = 0;
  private pc: RTCPeerConnection | null = null;
  private mux: PoolMux | null = null;
  private signaling: SignalingClient | null = null;
  private readonly authClient: AuthClient;

  private destroyed = false;
  private pcInFlight = false;
  private everAuthenticated = false;
  private silentReconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private silentReconnectBackoffMs = SILENT_RECONNECT_INITIAL_MS;
  private disconnectGraceTimer: ReturnType<typeof setTimeout> | null = null;
  private handshakeWatchdogTimer: ReturnType<typeof setTimeout> | null = null;

  private joinedAgentPk = '';
  private currentAgentPkB64 = '';
  private currentOfferDesc: SessionDescriptor | null = null;
  private currentAnswerDesc: SessionDescriptor | null = null;
  private currentVisitorNonce = '';
  private verifiedPkHash = '';
  private verifiedKeyStatus: KeyVerifyResult['status'] = 'tofu_new';

  private offerSent = false;
  private pendingLocalCandidates: Array<{ candidate: string; sdpMid: string | null; sdpMLineIndex: number | null }> = [];
  private pendingRemoteCandidates: RTCIceCandidateInit[] = [];
  private remoteDescApplied = false;
  private waitingForPasswordBeforePC = false;

  constructor(
    readonly nodeId: string,
    readonly portalOrigin: string,
    readonly sameSiteMode: boolean,
    readonly pinnedPkHash: string,
    readonly capToken: string,
    private icePrefs: IcePreferences,
    private readonly callbacks: PeerSessionCallbacks
  ) {
    this.authClient = new AuthClient(
      nodeId,
      portalOrigin,
      sameSiteMode,
      pinnedPkHash,
      capToken,
      {
        onStatus: callbacks.onStatus,
        onLog: callbacks.onLog,
        onAuthRequired: callbacks.onAuthRequired,
        onAuthFailed: (code, msg, retryAfter) => {
          this.teardownPeerConnection('auth rejected');
          this.signaling?.close();
          this.signaling = null;
          callbacks.onAuthFailed(code, msg, retryAfter);
        },
        onFatalSecurityError: (reason) => {
          this.cancelSilentReconnect();
          this.teardownPeerConnection(reason);
          this.signaling?.close();
          this.signaling = null;
          callbacks.onStatus('error', reason);
        },
        onAuthenticated: (info, mux) => {
          this.everAuthenticated = true;
          this.silentReconnectBackoffMs = SILENT_RECONNECT_INITIAL_MS;
          this.cancelSilentReconnect();
          if (this.handshakeWatchdogTimer) {
            clearTimeout(this.handshakeWatchdogTimer);
            this.handshakeWatchdogTimer = null;
          }
          // Strictly short-lived signaling: close WS immediately once DTLS tunnel is authenticated!
          if (this.signaling) {
            this.signaling.close();
            this.signaling = null;
          }
          callbacks.onAuthenticated(info, mux);
        },
      }
    );
  }

  getPeerConnection(): RTCPeerConnection | null {
    return this.pc;
  }

  getMux(): PoolMux | null {
    return this.mux;
  }

  openWSDC(): DataChannelLike {
    if (!this.pc || !this.authClient.isAuthenticated()) {
      throw new Error('PeerConnection is not authenticated');
    }
    const dc = this.pc.createDataChannel(WS_CHANNEL_LABEL, { ordered: true });
    dc.binaryType = 'arraybuffer';
    return dc as unknown as DataChannelLike;
  }

  updateIcePreferences(prefs: IcePreferences): void {
    this.icePrefs = prefs;
  }

  async connect(isSilentReconnect = false): Promise<void> {
    if (this.destroyed) return;
    const gen = ++this.generation;
    this.cancelSilentReconnect();
    this.teardownPeerConnection('reconnecting');
    if (this.signaling) {
      this.signaling.close();
      this.signaling = null;
    }
    this.pcInFlight = false;
    this.waitingForPasswordBeforePC = false;
    this.joinedAgentPk = '';

    if (!isSilentReconnect) {
      this.callbacks.onStatus('connecting', 'Connecting to signaling server...');
    }
    this.callbacks.onLog(`[Signal] Connecting to /ws/client/${this.nodeId}`);

    const sig = new SignalingClient(this.nodeId, {
      onJoined: (msg) => {
        if (this.generation !== gen || this.destroyed) return;
        void this.handleSignalingJoined(gen, msg);
      },
      onPeerStatus: (msg) => {
        if (this.generation !== gen || this.destroyed || this.authClient.isAuthenticated()) return;
        this.callbacks.onLog(`[Signal] Agent status: ${msg.online ? 'ONLINE' : 'OFFLINE'}`);
        if (msg.online) {
          if (!this.pc && !this.pcInFlight && !this.waitingForPasswordBeforePC) {
            this.cancelSilentReconnect();
            void this.handleAgentReady(gen, msg.agentPk, msg.requireVisitorAuth);
          }
        } else {
          if (this.pc) {
            this.teardownPeerConnection('Agent went offline during handshake');
          }
          if (this.everAuthenticated) {
            this.callbacks.onDisconnected('Agent went offline', true);
          } else {
            this.callbacks.onStatus('waiting', 'Waiting for Agent to come online...');
          }
        }
      },
      onAnswer: (msg) => {
        if (this.generation !== gen || this.destroyed || this.authClient.isAuthenticated()) return;
        void this.handleSignalingAnswer(gen, msg);
      },
      onCandidate: (msg) => {
        if (this.generation !== gen || this.destroyed || this.authClient.isAuthenticated()) return;
        void this.handleSignalingCandidate(gen, msg);
      },
      onError: (msg) => {
        if (this.generation !== gen || this.destroyed || this.authClient.isAuthenticated()) return;
        this.callbacks.onLog(`[Signal] Error [${msg.code}]: ${msg.error}`);
        if (msg.code === 'PEER_OFFLINE' || msg.code === 'AGENT_OFFLINE') {
          this.teardownPeerConnection('Agent is offline');
          if (this.everAuthenticated) {
            this.callbacks.onDisconnected('Agent is currently offline', true);
          } else {
            this.callbacks.onStatus('waiting', 'Agent is currently offline...');
          }
        } else if (this.everAuthenticated) {
          this.teardownPeerConnection(`Signaling error: ${msg.error}`);
          this.scheduleSilentReconnect(`Signaling error: ${msg.error}`);
        } else {
          this.teardownPeerConnection(`Signaling error: ${msg.error}`);
          this.callbacks.onStatus('error', msg.error);
        }
      },
      onDisconnected: () => {
        if (this.generation !== gen || this.destroyed || this.authClient.isAuthenticated()) return;
        if (this.everAuthenticated) {
          this.teardownPeerConnection('Signaling closed before reconnect completed');
          this.scheduleSilentReconnect('Signaling disconnected');
        } else if (!this.waitingForPasswordBeforePC && !this.pc) {
          this.callbacks.onStatus('disconnected', 'Signaling connection closed');
        }
      },
    });
    this.signaling = sig;

    try {
      await sig.connect();
    } catch (err) {
      if (this.generation !== gen || this.destroyed) return;
      const msg = err instanceof Error ? err.message : 'Signaling failed';
      if (this.everAuthenticated) {
        this.scheduleSilentReconnect(msg);
      } else {
        this.callbacks.onStatus('error', msg);
      }
    }
  }

  private async handleSignalingJoined(gen: number, joined: SignalJoinedMsg): Promise<void> {
    if (this.generation !== gen || this.destroyed) return;
    if (!joined.online) {
      if (!this.authClient.isAuthenticated() && !this.everAuthenticated) {
        this.callbacks.onStatus('waiting', 'Agent is offline — waiting for Agent...');
      }
      return;
    }
    if (!this.pc && !this.pcInFlight && !this.waitingForPasswordBeforePC) {
      this.cancelSilentReconnect();
      await this.handleAgentReady(gen, joined.agentPk, joined.requireVisitorAuth);
    }
  }

  private async handleAgentReady(
    gen: number,
    agentPk: string,
    requireVisitorAuth: boolean
  ): Promise<void> {
    if (this.generation !== gen || this.destroyed || this.pc || this.pcInFlight) return;
    this.pcInFlight = true;
    try {
      if (agentPk) {
        this.joinedAgentPk = agentPk;
        const pkBytes = base64UrlDecode(agentPk);
        if (pkBytes.byteLength !== 32) {
          throw new Error('Invalid agent_pk length from signaling');
        }
        const preCheck = await verifyPinnedOrTofuKey(
          this.portalOrigin,
          this.nodeId,
          pkBytes,
          this.pinnedPkHash,
          { commitNew: false }
        );
        if (this.generation !== gen || this.destroyed) return;
        if (!preCheck.ok) {
          this.callbacks.onLog(
            `[Security] PRE-HANDSHAKE KEY MISMATCH (${preCheck.status}): expected=${preCheck.expectedHash} actual=${preCheck.actualHash}`
          );
          this.cancelSilentReconnect();
          this.teardownPeerConnection('Agent identity key mismatch');
          this.callbacks.onKeyMismatch(preCheck, preCheck.status === 'pinned_mismatch');
          return;
        }
      }

      if (requireVisitorAuth && !this.authClient.hasCachedCredentialsFor(agentPk)) {
        this.waitingForPasswordBeforePC = true;
        this.callbacks.onStatus('auth_required', 'Access secret required');
        this.callbacks.onAuthRequired(this.nodeId);
        return;
      }

      await this.startPeerConnection(gen);
    } catch (err) {
      if (this.generation !== gen || this.destroyed) return;
      const msg = err instanceof Error ? err.message : 'Failed to initialize peer connection';
      if (this.everAuthenticated) {
        this.scheduleSilentReconnect(msg);
      } else {
        this.callbacks.onStatus('error', msg);
      }
    } finally {
      if (this.generation === gen) {
        this.pcInFlight = false;
      }
    }
  }

  async submitOwnerPassword(secret: string): Promise<void> {
    if (!secret) throw new Error('empty password');
    const gen = this.generation;
    const pkForSalt = this.joinedAgentPk || this.currentAgentPkB64;
    if (!pkForSalt) {
      throw new Error('Agent identity key not yet received');
    }
    await this.authClient.deriveOwnerCredentials(secret, pkForSalt);
    if (this.generation !== gen || this.destroyed) return;

    if (this.waitingForPasswordBeforePC) {
      this.waitingForPasswordBeforePC = false;
      if (this.signaling?.isOpen()) {
        await this.startPeerConnection(gen);
      } else {
        await this.connect();
      }
      return;
    }

    if (await this.authClient.tryResumePendingChallenge()) {
      return;
    }

    await this.connect();
  }

  requestCapabilityOp(payload: {
    action: 'mint' | 'list' | 'revoke';
    ttlSec?: number;
    pathPrefix?: string;
    readonly?: boolean;
    maxConns?: number;
    jti?: string;
    offset?: number;
    limit?: number;
  }): Promise<CapMintResult> {
    return this.authClient.requestCapabilityOp(payload);
  }

  destroy(): void {
    this.destroyed = true;
    this.generation++;
    this.cancelSilentReconnect();
    this.teardownPeerConnection('client destroyed');
    if (this.signaling) {
      this.signaling.close();
      this.signaling = null;
    }
    this.authClient.destroy();
  }

  private cancelSilentReconnect(): void {
    if (this.silentReconnectTimer) {
      clearTimeout(this.silentReconnectTimer);
      this.silentReconnectTimer = null;
    }
  }

  private scheduleSilentReconnect(reason: string): void {
    if (this.destroyed || this.silentReconnectTimer) return;
    const jitter = Math.floor(Math.random() * 300);
    const delay = Math.min(this.silentReconnectBackoffMs + jitter, SILENT_RECONNECT_MAX_MS);
    this.silentReconnectBackoffMs = Math.min(this.silentReconnectBackoffMs * 2, SILENT_RECONNECT_MAX_MS);
    this.callbacks.onLog(`[Tunnel] Scheduling silent reconnect in ${delay}ms (${reason})`);
    this.callbacks.onDisconnected(reason, true);
    this.silentReconnectTimer = setTimeout(() => {
      this.silentReconnectTimer = null;
      if (this.destroyed) return;
      void this.connect(true);
    }, delay);
  }

  private teardownPeerConnection(reason: string): void {
    if (this.disconnectGraceTimer) {
      clearTimeout(this.disconnectGraceTimer);
      this.disconnectGraceTimer = null;
    }
    if (this.handshakeWatchdogTimer) {
      clearTimeout(this.handshakeWatchdogTimer);
      this.handshakeWatchdogTimer = null;
    }
    this.authClient.resetChannel(reason);
    if (this.mux) {
      this.mux.close();
      this.mux = null;
    }
    if (this.pc) {
      const pc = this.pc;
      this.pc = null;
      pc.onicecandidate = null;
      pc.onconnectionstatechange = null;
      try {
        pc.close();
      } catch {
        // ignore
      }
    }
    this.offerSent = false;
    this.pendingLocalCandidates = [];
    this.pendingRemoteCandidates = [];
    this.remoteDescApplied = false;
    this.currentOfferDesc = null;
    this.currentAnswerDesc = null;
    this.currentVisitorNonce = '';
  }

  private async startPeerConnection(gen: number): Promise<void> {
    if (this.generation !== gen || this.destroyed || !this.signaling) return;
    this.teardownPeerConnection('new pc');
    if (!this.everAuthenticated) {
      this.callbacks.onStatus('connecting', 'Negotiating P2P WebRTC tunnel...');
    }

    const rtcConfig: RTCConfiguration = {
      iceServers: buildIceServers(this.icePrefs),
      iceTransportPolicy: this.icePrefs.relayOnly ? 'relay' : 'all',
    };
    const pc = new RTCPeerConnection(rtcConfig);
    this.pc = pc;

    const watchdogMs = this.everAuthenticated
      ? RECONNECT_HANDSHAKE_TIMEOUT_MS
      : INITIAL_HANDSHAKE_TIMEOUT_MS;
    this.handshakeWatchdogTimer = setTimeout(() => {
      this.handshakeWatchdogTimer = null;
      if (this.generation !== gen || this.pc !== pc || this.destroyed) return;
      if (!this.authClient.isAuthenticated()) {
        if (this.everAuthenticated) {
          this.callbacks.onLog('[Tunnel] Reconnect handshake timed out — retrying silently');
          this.teardownPeerConnection('WebRTC reconnect handshake timed out');
          this.scheduleSilentReconnect('WebRTC reconnect handshake timed out');
        } else {
          this.callbacks.onLog('[Tunnel] Initial WebRTC handshake timed out');
          this.teardownPeerConnection('WebRTC handshake timed out');
          this.signaling?.close();
          this.signaling = null;
          this.callbacks.onStatus('error', 'WebRTC handshake timed out');
        }
      }
    }, watchdogMs);

    // Pre-negotiate 1 Control DC (id=0) + 32 Reusable HTTP Pool DCs (id=1..32).
    const ctrlDC = pc.createDataChannel(CONTROL_CHANNEL_LABEL, {
      negotiated: true,
      id: CONTROL_CHANNEL_ID,
      ordered: true,
    });
    ctrlDC.binaryType = 'arraybuffer';

    const poolDCs: RTCDataChannel[] = [];
    for (let i = 0; i < DEFAULT_POOL_SIZE; i++) {
      const dc = pc.createDataChannel(poolChannelLabel(i), {
        negotiated: true,
        id: poolChannelId(i),
        ordered: true,
      });
      dc.binaryType = 'arraybuffer';
      poolDCs.push(dc);
    }

    let muxStarted = false;
    ctrlDC.onopen = () => {
      if (this.generation !== gen || this.pc !== pc || muxStarted) return;
      muxStarted = true;
      // SYNCHRONOUSLY buffer early control frames before async DTLS cert verification,
      // and serialize control frame handling strictly in wire order!
      const earlyControlFrames: Frame[] = [];
      let dtlsVerified = false;
      let authGen = -1;
      let controlChain: Promise<void> = Promise.resolve();

      const enqueueControlFrame = (frame: Frame) => {
        controlChain = controlChain.then(async () => {
          if (this.generation !== gen || this.pc !== pc) return;
          await this.authClient.handleControlFrame(authGen, frame);
        });
      };

      const mux = new PoolMux(
        {
          onControlFrame: (frame) => {
            if (this.generation !== gen || this.pc !== pc) return;
            if (!dtlsVerified) {
              if (earlyControlFrames.length < 32) {
                earlyControlFrames.push(frame);
              }
              return;
            }
            enqueueControlFrame(frame);
          },
          onGoAway: () => {
            if (this.generation !== gen || this.pc !== pc) return;
            this.callbacks.onLog('[Mux] Received GOAWAY — rebuilding tunnel silently');
            this.teardownPeerConnection('Agent requested tunnel rotation (GOAWAY)');
            this.scheduleSilentReconnect('Agent requested tunnel rotation (GOAWAY)');
          },
        },
        poolDCs.length
      );
      mux.attachControlDC(ctrlDC as unknown as DataChannelLike);
      for (let i = 0; i < poolDCs.length; i++) {
        mux.attachPoolDC(i, poolDCs[i] as unknown as DataChannelLike);
      }
      this.mux = mux;

      void (async () => {
        if (this.generation !== gen || this.pc !== pc || !this.currentAnswerDesc || !this.currentOfferDesc) {
          return;
        }
        const certOk = await verifyPeerDtlsCertificate(pc, this.currentAnswerDesc.fp);
        if (this.generation !== gen || this.pc !== pc || this.mux !== mux) return;
        if (!certOk) {
          const reason = 'Negotiated remote DTLS certificate does not match signed Agent fingerprint';
          this.callbacks.onLog(`[Security] BLOCKED: ${reason}`);
          this.cancelSilentReconnect();
          this.teardownPeerConnection(reason);
          this.signaling?.close();
          this.signaling = null;
          this.callbacks.onStatus('error', reason);
          return;
        }

        if (!this.everAuthenticated) {
          this.callbacks.onStatus('authenticating', 'Verifying in-DTLS channel authentication...');
        }

        mux.setMaxMessageSize(this.currentAnswerDesc.max_msg_size);
        const sctpMax = pc.sctp?.maxMessageSize;
        if (typeof sctpMax === 'number' && Number.isFinite(sctpMax) && sctpMax > 0) {
          mux.setMaxMessageSize(sctpMax);
        }

        authGen = this.authClient.beginChannel({
          mux,
          offerDesc: this.currentOfferDesc,
          answerDesc: this.currentAnswerDesc,
          visitorNonce: this.currentVisitorNonce,
          agentPkB64: this.currentAgentPkB64,
          verifiedPkHash: this.verifiedPkHash,
          verifiedKeyStatus: this.verifiedKeyStatus,
        });

        try {
          mux.sendControlFrame(
            FrameType.SETTINGS,
            textEncoder.encode(
              JSON.stringify({
                max_frame_payload: mux.effectiveChunkSize() || DEFAULT_CHUNK_SIZE,
              })
            )
          );
        } catch {
          // ignore
        }

        // Drain early frames sequentially BEFORE flipping dtlsVerified=true so late frames cannot overtake them!
        while (earlyControlFrames.length > 0) {
          const batch = earlyControlFrames.splice(0);
          for (const early of batch) {
            if (this.generation !== gen || this.pc !== pc) return;
            await this.authClient.handleControlFrame(authGen, early);
          }
        }
        if (this.generation !== gen || this.pc !== pc) return;
        dtlsVerified = true;
      })();
    };

    ctrlDC.onclose = () => {
      if (this.generation !== gen || this.pc !== pc || this.destroyed) return;
      this.teardownPeerConnection('Control DataChannel closed');
      if (this.everAuthenticated) {
        this.scheduleSilentReconnect('Control DataChannel closed');
      } else {
        this.signaling?.close();
        this.signaling = null;
        this.callbacks.onDisconnected('Control DataChannel closed', false);
      }
    };

    // Buffer local ICE candidates until AFTER sendOffer() succeeds so signaling never returns PRE_OFFER_CANDIDATE!
    pc.onicecandidate = (ev) => {
      if (this.generation !== gen || this.pc !== pc || !ev.candidate) return;
      const item = {
        candidate: ev.candidate.candidate,
        sdpMid: ev.candidate.sdpMid,
        sdpMLineIndex: ev.candidate.sdpMLineIndex,
      };
      if (!this.offerSent) {
        if (this.pendingLocalCandidates.length < 64) {
          this.pendingLocalCandidates.push(item);
        }
        return;
      }
      if (!this.signaling?.isOpen()) return;
      try {
        this.signaling.sendCandidate(item.candidate, item.sdpMid, item.sdpMLineIndex);
      } catch {
        // ignore transient disconnect
      }
    };

    pc.onconnectionstatechange = () => {
      if (this.generation !== gen || this.pc !== pc || this.destroyed) return;
      const st = pc.connectionState;
      this.callbacks.onLog(`[WebRTC] connectionState=${st}`);
      if (st === 'connected') {
        if (this.disconnectGraceTimer) {
          clearTimeout(this.disconnectGraceTimer);
          this.disconnectGraceTimer = null;
        }
      } else if (st === 'disconnected') {
        if ((this.authClient.isAuthenticated() || this.everAuthenticated) && !this.disconnectGraceTimer) {
          this.disconnectGraceTimer = setTimeout(() => {
            this.disconnectGraceTimer = null;
            if (this.generation !== gen || this.pc !== pc || this.destroyed) return;
            if (pc.connectionState === 'disconnected') {
              this.teardownPeerConnection('WebRTC ICE disconnected');
              this.scheduleSilentReconnect('WebRTC ICE disconnected');
            }
          }, DISCONNECT_GRACE_MS);
        }
      } else if (st === 'failed' || st === 'closed') {
        this.teardownPeerConnection(`WebRTC ICE connection ${st}`);
        if (this.everAuthenticated) {
          this.scheduleSilentReconnect(`WebRTC ICE connection ${st}`);
        } else {
          this.callbacks.onDisconnected(`WebRTC ICE connection ${st}`, false);
        }
      }
    };

    const offer = await pc.createOffer();
    if (this.generation !== gen || this.pc !== pc) return;
    await pc.setLocalDescription(offer);
    if (this.generation !== gen || this.pc !== pc) return;

    const sdp = pc.localDescription?.sdp || offer.sdp || '';
    const offerDesc = extractSessionDescriptor(sdp, 2);
    const visitorNonce = randomBase64Url(32);
    this.currentOfferDesc = offerDesc;
    this.currentVisitorNonce = visitorNonce;

    this.signaling.sendOffer({ desc: offerDesc, nonce: visitorNonce });
    this.offerSent = true;
    this.callbacks.onLog(`[WebRTC] Sent structured Offer (${DEFAULT_POOL_SIZE} pool DCs + p2pctrl)`);

    // Flush any early ICE candidates gathered between setLocalDescription and sendOffer!
    for (const c of this.pendingLocalCandidates.splice(0)) {
      if (!this.signaling?.isOpen()) break;
      try {
        this.signaling.sendCandidate(c.candidate, c.sdpMid, c.sdpMLineIndex);
      } catch {
        break;
      }
    }
  }

  private async handleSignalingAnswer(gen: number, msg: SignalAnswerMsg): Promise<void> {
    if (this.generation !== gen || !this.pc || !this.currentOfferDesc) return;
    const pc = this.pc;

    if (!validateSessionDescriptor(msg.desc, false)) {
      this.teardownPeerConnection('Invalid answer SessionDescriptor');
      this.callbacks.onStatus('error', 'Invalid answer descriptor from Agent');
      return;
    }

    if (this.joinedAgentPk && msg.pk !== this.joinedAgentPk) {
      const reason = 'Answer public key does not match signaled Agent identity';
      this.callbacks.onLog(`[Security] BLOCKED: ${reason}`);
      this.cancelSilentReconnect();
      this.teardownPeerConnection(reason);
      this.callbacks.onStatus('error', reason);
      return;
    }

    let pkBytes: Uint8Array;
    try {
      pkBytes = base64UrlDecode(msg.pk);
      const sigOk = await verifyAnswerTranscript(
        msg.pk,
        this.nodeId,
        this.currentVisitorNonce,
        this.currentOfferDesc,
        msg.desc,
        msg.sig
      );
      if (!sigOk) {
        throw new Error('Ed25519 answer signature verification failed');
      }
    } catch (err) {
      if (this.generation !== gen) return;
      const reason = err instanceof Error ? err.message : 'Ed25519 signature verification failed';
      this.callbacks.onLog(`[Security] BLOCKED: ${reason}`);
      this.cancelSilentReconnect();
      this.teardownPeerConnection(reason);
      this.callbacks.onStatus('error', reason);
      return;
    }

    if (this.generation !== gen || this.pc !== pc) return;

    let keyRes: KeyVerifyResult;
    try {
      keyRes = await verifyPinnedOrTofuKey(
        this.portalOrigin,
        this.nodeId,
        pkBytes,
        this.pinnedPkHash,
        { commitNew: false }
      );
    } catch (err) {
      if (this.generation !== gen) return;
      const reason = err instanceof Error ? err.message : 'TOFU identity store error';
      this.cancelSilentReconnect();
      this.teardownPeerConnection(reason);
      this.callbacks.onStatus('error', reason);
      return;
    }
    if (this.generation !== gen || this.pc !== pc) return;

    if (!keyRes.ok) {
      this.callbacks.onLog(
        `[Security] KEY MISMATCH (${keyRes.status}): expected=${keyRes.expectedHash} actual=${keyRes.actualHash}`
      );
      this.cancelSilentReconnect();
      this.teardownPeerConnection('Agent identity key mismatch');
      this.callbacks.onKeyMismatch(keyRes, keyRes.status === 'pinned_mismatch');
      return;
    }

    this.verifiedPkHash = keyRes.actualHash;
    this.verifiedKeyStatus = keyRes.status;
    this.currentAgentPkB64 = msg.pk;
    this.currentAnswerDesc = msg.desc;

    try {
      const synthSdp = synthesizeSDP(msg.desc, false);
      await pc.setRemoteDescription({ type: 'answer', sdp: synthSdp });
      if (this.generation !== gen || this.pc !== pc) return;
      this.remoteDescApplied = true;

      for (const c of this.pendingRemoteCandidates.splice(0)) {
        await pc.addIceCandidate(c).catch(() => {});
      }
      this.callbacks.onLog(`[Security] Ed25519 Answer verified (${keyRes.status}: ${keyRes.actualHash})`);
    } catch (err) {
      if (this.generation !== gen) return;
      const reason = err instanceof Error ? err.message : 'setRemoteDescription failed';
      if (this.everAuthenticated) {
        this.scheduleSilentReconnect(reason);
      } else {
        this.callbacks.onStatus('error', reason);
      }
    }
  }

  private async handleSignalingCandidate(gen: number, msg: SignalCandidateMsg): Promise<void> {
    if (this.generation !== gen || !this.pc) return;
    const init: RTCIceCandidateInit = {
      candidate: msg.candidate,
      sdpMid: msg.sdpMid || undefined,
      sdpMLineIndex: msg.sdpMLineIndex,
    };
    if (!this.remoteDescApplied) {
      if (this.pendingRemoteCandidates.length < 64) {
        this.pendingRemoteCandidates.push(init);
      }
      return;
    }
    await this.pc.addIceCandidate(init).catch(() => {});
  }
}
