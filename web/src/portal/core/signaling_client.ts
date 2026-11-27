/**
 * Portal Short-Lived Signaling Client (Single Authoritative Embedded Go Signal)
 * - Connects strictly to same-origin `location.origin` ONLY during WebRTC negotiation.
 * - Immediately closed by PeerSession as soon as in-DTLS authentication succeeds
 *   (no long-lived signaling connection or competing reconnect loop).
 * - Exchanges structured `SessionDescriptor` Offer/Answer over WebSocket.
 */

import { isRecord } from '../../shared/ipc';
import { validateSessionDescriptor, type SessionDescriptor } from '../../shared/sdp';

export interface SignalJoinedMsg {
  nodeId: string;
  sessionId: string;
  online: boolean;
  requireVisitorAuth: boolean;
  agentPk: string;
}

export interface SignalPeerStatusMsg {
  nodeId: string;
  online: boolean;
  requireVisitorAuth: boolean;
  agentPk: string;
}

export interface SignalAnswerMsg {
  nodeId: string;
  sessionId: string;
  desc: SessionDescriptor;
  pk: string;
  sig: string;
}

export interface SignalCandidateMsg {
  nodeId: string;
  sessionId: string;
  candidate: string;
  sdpMid: string;
  sdpMLineIndex?: number;
}

export interface SignalErrorMsg {
  code: string;
  error: string;
}

export interface OfferPayload {
  desc: SessionDescriptor;
  nonce: string;
}

export interface SignalingClientCallbacks {
  onJoined?: (msg: SignalJoinedMsg) => void;
  onPeerStatus?: (msg: SignalPeerStatusMsg) => void;
  onAnswer?: (msg: SignalAnswerMsg) => void;
  onCandidate?: (msg: SignalCandidateMsg) => void;
  onError?: (msg: SignalErrorMsg) => void;
  onDisconnected?: () => void;
}

export class SignalingClient {
  private ws: WebSocket | null = null;
  private connectPromise: Promise<SignalJoinedMsg> | null = null;
  private closedManually = false;
  private sessionId = '';
  private online = false;
  private requireVisitorAuth = true;
  private agentPk = '';

  constructor(
    readonly nodeId: string,
    private readonly callbacks: SignalingClientCallbacks
  ) {}

  isOpen(): boolean {
    return this.ws !== null && this.ws.readyState === WebSocket.OPEN;
  }

  private buildWsUrl(): string {
    const u = new URL(location.origin);
    const proto = u.protocol === 'https:' ? 'wss:' : 'ws:';
    return `${proto}//${u.host}/ws/client/${encodeURIComponent(this.nodeId)}`;
  }

  connect(): Promise<SignalJoinedMsg> {
    this.closedManually = false;
    if (this.ws && this.ws.readyState === WebSocket.OPEN && this.sessionId) {
      return Promise.resolve({
        nodeId: this.nodeId,
        sessionId: this.sessionId,
        online: this.online,
        requireVisitorAuth: this.requireVisitorAuth,
        agentPk: this.agentPk,
      });
    }
    if (this.connectPromise) {
      return this.connectPromise;
    }

    this.connectPromise = new Promise<SignalJoinedMsg>((resolve, reject) => {
      let settled = false;
      const ws = new WebSocket(this.buildWsUrl());
      this.ws = ws;

      const failConnect = (err: Error) => {
        if (!settled) {
          settled = true;
          this.connectPromise = null;
          reject(err);
        }
      };

      const joinTimeout = setTimeout(() => {
        if (!settled) {
          try {
            ws.close();
          } catch {
            // ignore
          }
          failConnect(new Error('Signaling join timeout'));
        }
      }, 10_000);

      ws.onmessage = (ev: MessageEvent) => {
        if (this.ws !== ws || typeof ev.data !== 'string') return;
        let parsed: unknown;
        try {
          parsed = JSON.parse(ev.data);
        } catch {
          return;
        }
        if (!isRecord(parsed) || typeof parsed.type !== 'string') return;

        switch (parsed.type) {
          case 'joined': {
            const msg: SignalJoinedMsg = {
              nodeId: typeof parsed.node_id === 'string' ? parsed.node_id : this.nodeId,
              sessionId: typeof parsed.session_id === 'string' ? parsed.session_id : '',
              online: Boolean(parsed.online),
              requireVisitorAuth: Boolean(parsed.require_visitor_auth),
              agentPk: typeof parsed.agent_pk === 'string' ? parsed.agent_pk : '',
            };
            this.sessionId = msg.sessionId;
            this.online = msg.online;
            this.requireVisitorAuth = msg.requireVisitorAuth;
            this.agentPk = msg.agentPk;
            clearTimeout(joinTimeout);
            if (!settled) {
              settled = true;
              this.connectPromise = null;
              resolve(msg);
            }
            this.callbacks.onJoined?.(msg);
            break;
          }
          case 'peer_status': {
            const msg: SignalPeerStatusMsg = {
              nodeId: typeof parsed.node_id === 'string' ? parsed.node_id : this.nodeId,
              online: Boolean(parsed.online),
              requireVisitorAuth: Boolean(parsed.require_visitor_auth),
              agentPk: typeof parsed.agent_pk === 'string' ? parsed.agent_pk : '',
            };
            this.online = msg.online;
            this.requireVisitorAuth = msg.requireVisitorAuth;
            if (msg.agentPk) this.agentPk = msg.agentPk;
            this.callbacks.onPeerStatus?.(msg);
            break;
          }
          case 'answer': {
            if (
              validateSessionDescriptor(parsed.desc, false) &&
              typeof parsed.pk === 'string' &&
              typeof parsed.sig === 'string'
            ) {
              this.callbacks.onAnswer?.({
                nodeId: typeof parsed.node_id === 'string' ? parsed.node_id : this.nodeId,
                sessionId: typeof parsed.session_id === 'string' ? parsed.session_id : this.sessionId,
                desc: parsed.desc,
                pk: parsed.pk,
                sig: parsed.sig,
              });
            }
            break;
          }
          case 'candidate': {
            if (typeof parsed.candidate === 'string') {
              this.callbacks.onCandidate?.({
                nodeId: typeof parsed.node_id === 'string' ? parsed.node_id : this.nodeId,
                sessionId: typeof parsed.session_id === 'string' ? parsed.session_id : this.sessionId,
                candidate: parsed.candidate,
                sdpMid: typeof parsed.sdp_mid === 'string' ? parsed.sdp_mid : '',
                sdpMLineIndex:
                  typeof parsed.sdp_mline_index === 'number' ? parsed.sdp_mline_index : undefined,
              });
            }
            break;
          }
          case 'error': {
            this.callbacks.onError?.({
              code: typeof parsed.code === 'string' ? parsed.code : 'SIGNAL_ERROR',
              error: typeof parsed.error === 'string' ? parsed.error : 'Signaling error',
            });
            break;
          }
        }
      };

      ws.onerror = () => {
        if (this.ws !== ws) return;
        clearTimeout(joinTimeout);
        failConnect(new Error('Signaling WebSocket connection failed'));
      };

      ws.onclose = () => {
        clearTimeout(joinTimeout);
        if (this.ws !== ws) return;
        this.ws = null;
        this.sessionId = '';
        failConnect(new Error('Signaling WebSocket closed before join'));

        if (!this.closedManually) {
          this.callbacks.onDisconnected?.();
        }
      };
    });

    return this.connectPromise;
  }

  sendOffer(offer: OfferPayload): void {
    this.sendRaw({
      type: 'offer',
      desc: offer.desc,
      nonce: offer.nonce,
    });
  }

  sendCandidate(candidate: string, sdpMid: string | null, sdpMLineIndex: number | null): void {
    const payload: Record<string, unknown> = {
      type: 'candidate',
      candidate,
      sdp_mid: sdpMid ?? '',
    };
    if (sdpMLineIndex !== null) {
      payload.sdp_mline_index = sdpMLineIndex;
    }
    this.sendRaw(payload);
  }

  private sendRaw(obj: Record<string, unknown>): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
      throw new Error('Signaling WebSocket is not open');
    }
    this.ws.send(JSON.stringify(obj));
  }

  close(): void {
    this.closedManually = true;
    this.connectPromise = null;
    if (this.ws) {
      const ws = this.ws;
      this.ws = null;
      ws.onopen = null;
      ws.onmessage = null;
      ws.onerror = null;
      ws.onclose = null;
      try {
        ws.close(1000, 'client closed');
      } catch {
        // ignore
      }
    }
  }
}
