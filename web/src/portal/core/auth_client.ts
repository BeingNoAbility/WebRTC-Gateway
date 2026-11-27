/**
 * Strict 3-State Authentication Client FSM (`WAIT_CHALLENGE -> WAIT_RESULT -> AUTHENTICATED`)
 * - Binds `AUTH_RESULT.role` strictly to `sentMethod` and unconditionally verifies `confirm_v`
 *   when `sentMethod === 'spake2p'` (prevents mutual-auth downgrade & TOFU poisoning).
 * - Rejects duplicate AUTH_CHALLENGE / AUTH_RESULT frames.
 * - Retains stretched `(w0, w1)` (never plaintext password) + `cookieResumeToken` in RAM across
 *   same-tab silent tunnel reconnects; clears on `destroy()` or auth failure.
 * - Strictly validates paginated `CAP_RESP` payloads at runtime.
 */

import { FrameType, HEADER_SIZE, type Frame } from '../../protocol/frame';
import type { PoolMux } from '../../protocol/mux';
import { base64UrlDecode, base64UrlEncode, randomBase64Url } from '../../shared/crypto';
import { commitTofuKey, type KeyVerifyResult } from '../../shared/identity';
import { isCookieDelta, isRecord, type BackendMeta } from '../../shared/ipc';
import type { SessionDescriptor } from '../../shared/sdp';
import {
  buildSPAKE2PContext,
  constantTimeEqual,
  deriveSPAKE2PProverCreds,
  spake2pProverCompute,
  type SPAKE2PProverCreds,
} from '../../shared/spake2p';

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();

export type AuthFsmState = 'WAIT_CHALLENGE' | 'WAIT_RESULT' | 'AUTHENTICATED' | 'FAILED';
export type AuthMethod = 'spake2p' | 'cap' | 'public';

export interface AuthSuccessInfo {
  role: 'owner' | 'capability' | 'public';
  meta: BackendMeta;
  pkHash: string;
  keyStatus: KeyVerifyResult['status'];
}

export interface CapabilityRecordDTO {
  payload: {
    v: number;
    jti: string;
    node: string;
    exp: number;
    path_prefix?: string;
    readonly?: boolean;
    max_conns?: number;
  };
  revoked: boolean;
  active_conns: number;
}

export interface CapMintResult {
  ok: boolean;
  error?: string;
  token?: string;
  jti?: string;
  pkHash?: string;
  records?: CapabilityRecordDTO[];
  total?: number;
  offset?: number;
  limit?: number;
  hasMore?: boolean;
}

export interface AuthChannelContext {
  mux: PoolMux;
  offerDesc: SessionDescriptor;
  answerDesc: SessionDescriptor;
  visitorNonce: string;
  agentPkB64: string;
  verifiedPkHash: string;
  verifiedKeyStatus: KeyVerifyResult['status'];
}

export interface AuthClientCallbacks {
  onStatus: (status: string, detail?: string) => void;
  onLog: (line: string) => void;
  onAuthRequired: (nodeId: string) => void;
  onAuthFailed: (code: string, message: string, retryAfterSec?: number) => void;
  onFatalSecurityError: (reason: string) => void;
  onAuthenticated: (info: AuthSuccessInfo, mux: PoolMux) => void;
}

interface PendingCapReq {
  resolve: (res: CapMintResult) => void;
  reject: (err: Error) => void;
  timer: ReturnType<typeof setTimeout>;
}

function isCapabilityRecordDTO(v: unknown): v is CapabilityRecordDTO {
  if (!isRecord(v) || !isRecord(v.payload)) return false;
  const p = v.payload;
  return (
    typeof p.v === 'number' &&
    typeof p.jti === 'string' &&
    p.jti.length > 0 &&
    typeof p.node === 'string' &&
    typeof p.exp === 'number' &&
    typeof v.revoked === 'boolean' &&
    typeof v.active_conns === 'number'
  );
}

export class AuthClient {
  private state: AuthFsmState = 'WAIT_CHALLENGE';
  private sentMethod: AuthMethod | null = null;
  private channelCtx: AuthChannelContext | null = null;
  private channelGen = 0;

  // Retained in RAM within this Tab across silent reconnects (never persisted, never plaintext).
  private spakeCreds: SPAKE2PProverCreds | null = null;
  private spakeCredsAgentPk = '';
  private cookieResumeToken = '';

  // Per-handshake ephemeral state.
  private expectedConfirmV: Uint8Array | null = null;
  private currentAgentNonce = '';
  private currentShareVB64 = '';
  private waitingForPasswordOnChallenge = false;
  private pendingCapReqs = new Map<string, PendingCapReq>();

  constructor(
    readonly nodeId: string,
    readonly portalOrigin: string,
    readonly sameSiteMode: boolean,
    readonly pinnedPkHash: string,
    readonly capToken: string,
    private readonly callbacks: AuthClientCallbacks
  ) {}

  getState(): AuthFsmState {
    return this.state;
  }

  isAuthenticated(): boolean {
    return this.state === 'AUTHENTICATED';
  }

  hasCachedCredentialsFor(agentPkB64: string): boolean {
    return Boolean(this.capToken || (this.spakeCreds && this.spakeCredsAgentPk === agentPkB64));
  }

  /**
   * Derives V7 SPAKE2+ (w0, w1) in memory bound to (nodeId, agentPkB64).
   * Plaintext password is never retained.
   */
  async deriveOwnerCredentials(secret: string, agentPkB64: string): Promise<void> {
    if (!secret) throw new Error('empty password');
    if (!agentPkB64) throw new Error('missing verified Agent public key');
    const creds = await deriveSPAKE2PProverCreds(secret, this.nodeId, agentPkB64);
    this.spakeCreds = creds;
    this.spakeCredsAgentPk = agentPkB64;
  }

  /**
   * Resets per-connection FSM state for a newly opened DTLS control channel while preserving
   * cached RAM credentials (`spakeCreds`) and `cookieResumeToken` for silent reconnects.
   */
  beginChannel(ctx: AuthChannelContext): number {
    this.failPendingCapReqs('channel replaced');
    this.channelGen++;
    this.channelCtx = ctx;
    this.state = 'WAIT_CHALLENGE';
    this.sentMethod = null;
    this.expectedConfirmV = null;
    this.currentAgentNonce = '';
    this.currentShareVB64 = '';
    this.waitingForPasswordOnChallenge = false;
    return this.channelGen;
  }

  resetChannel(reason: string): void {
    this.channelGen++;
    this.failPendingCapReqs(reason);
    this.channelCtx = null;
    this.state = 'WAIT_CHALLENGE';
    this.sentMethod = null;
    this.expectedConfirmV = null;
    this.currentAgentNonce = '';
    this.currentShareVB64 = '';
    this.waitingForPasswordOnChallenge = false;
  }

  /**
   * Completes an in-flight challenge if the user entered the password after AUTH_CHALLENGE arrived.
   * Returns true if handled on the live channel; false if caller should open a fresh connection.
   */
  async tryResumePendingChallenge(): Promise<boolean> {
    if (
      !this.waitingForPasswordOnChallenge ||
      this.state !== 'WAIT_CHALLENGE' ||
      !this.channelCtx ||
      !this.currentAgentNonce ||
      !this.currentShareVB64 ||
      !this.spakeCreds ||
      this.spakeCredsAgentPk !== this.channelCtx.agentPkB64
    ) {
      return false;
    }
    this.waitingForPasswordOnChallenge = false;
    await this.sendAuthResponse(this.channelGen, 'spake2p');
    return true;
  }

  async handleControlFrame(gen: number, frame: Frame): Promise<void> {
    if (gen !== this.channelGen || !this.channelCtx) return;
    const ctx = this.channelCtx;

    let parsed: unknown;
    try {
      parsed = JSON.parse(textDecoder.decode(frame.payload));
    } catch {
      return;
    }
    if (!isRecord(parsed)) return;

    switch (frame.type) {
      case FrameType.SETTINGS: {
        if (typeof parsed.max_frame_payload === 'number' && parsed.max_frame_payload > 0) {
          ctx.mux.setMaxMessageSize(parsed.max_frame_payload + HEADER_SIZE);
        }
        break;
      }

      case FrameType.AUTH_CHALLENGE: {
        // Strict 3-state FSM: AUTH_CHALLENGE is ONLY valid once in WAIT_CHALLENGE!
        if (this.state !== 'WAIT_CHALLENGE' || this.currentAgentNonce !== '') {
          this.failSecurity('Unexpected duplicate AUTH_CHALLENGE frame');
          return;
        }
        const agentNonce = typeof parsed.agent_nonce === 'string' ? parsed.agent_nonce : '';
        const authMethods = Array.isArray(parsed.auth_methods)
          ? parsed.auth_methods.filter((m): m is string => typeof m === 'string')
          : [];
        const shareV = typeof parsed.share_v === 'string' ? parsed.share_v : '';
        if (!agentNonce) {
          this.failSecurity('Malformed AUTH_CHALLENGE missing agent_nonce');
          return;
        }
        this.currentAgentNonce = agentNonce;
        this.currentShareVB64 = shareV;

        if (this.capToken) {
          await this.sendAuthResponse(gen, 'cap');
        } else if (authMethods.includes('public') && !this.hasCachedCredentialsFor(ctx.agentPkB64)) {
          await this.sendAuthResponse(gen, 'public');
        } else if (this.spakeCreds && this.spakeCredsAgentPk === ctx.agentPkB64 && shareV) {
          await this.sendAuthResponse(gen, 'spake2p');
        } else {
          this.waitingForPasswordOnChallenge = true;
          this.callbacks.onStatus('auth_required', 'Access secret required');
          this.callbacks.onAuthRequired(this.nodeId);
        }
        break;
      }

      case FrameType.AUTH_RESULT: {
        // Strict 3-state FSM: AUTH_RESULT is ONLY valid once in WAIT_RESULT!
        if (this.state !== 'WAIT_RESULT' || this.sentMethod === null) {
          this.failSecurity('Unexpected AUTH_RESULT outside WAIT_RESULT state');
          return;
        }
        const sentMethod = this.sentMethod;
        const expectedConfirmV = this.expectedConfirmV;
        this.expectedConfirmV = null;

        const ok = Boolean(parsed.ok);
        if (!ok) {
          this.state = 'FAILED';
          const code = typeof parsed.code === 'string' ? parsed.code : 'AUTH_FAILED';
          const errMsg = typeof parsed.error === 'string' ? parsed.error : 'Authentication failed';
          const retryAfter =
            typeof parsed.retry_after_sec === 'number' ? parsed.retry_after_sec : undefined;
          if (sentMethod === 'spake2p') {
            this.spakeCreds = null;
            this.spakeCredsAgentPk = '';
          }
          this.callbacks.onAuthFailed(code, errMsg, retryAfter);
          return;
        }

        // Strict sentMethod -> role binding (blocks auth downgrade from spake2p to public).
        const rawRole = typeof parsed.role === 'string' ? parsed.role : '';
        const expectedRole: 'owner' | 'capability' | 'public' =
          sentMethod === 'spake2p' ? 'owner' : sentMethod === 'cap' ? 'capability' : 'public';
        if (rawRole !== expectedRole) {
          this.failSecurity(
            `Auth role downgrade rejected: sent method=${sentMethod} requires role=${expectedRole}, got ${rawRole || '<empty>'}`
          );
          return;
        }

        // Unconditional mutual SPAKE2+ verifier confirmation check whenever sentMethod === 'spake2p'!
        if (sentMethod === 'spake2p') {
          const confirmVB64 = typeof parsed.confirm_v === 'string' ? parsed.confirm_v : '';
          let confirmOk = false;
          try {
            if (confirmVB64 && expectedConfirmV) {
              confirmOk = constantTimeEqual(base64UrlDecode(confirmVB64), expectedConfirmV);
            }
          } catch {
            confirmOk = false;
          }
          if (!confirmOk) {
            this.failSecurity('SPAKE2+ verifier key confirmation failed');
            return;
          }
        }

        // Commit TOFU key ONLY AFTER Ed25519 signature + DTLS cert + strict channel auth succeed!
        if (!this.pinnedPkHash && ctx.verifiedPkHash) {
          try {
            await commitTofuKey(this.portalOrigin, this.nodeId, ctx.verifiedPkHash);
          } catch (err) {
            this.callbacks.onLog(
              `[Security] Warning: failed to persist TOFU key: ${err instanceof Error ? err.message : String(err)}`
            );
          }
        }
        if (gen !== this.channelGen || this.channelCtx !== ctx) return;

        if (typeof parsed.cookie_resume_token === 'string' && parsed.cookie_resume_token) {
          this.cookieResumeToken = parsed.cookie_resume_token;
        }

        const rawMeta = isRecord(parsed.meta) ? parsed.meta : {};
        const initialCookies = Array.isArray(rawMeta.initialCookies)
          ? rawMeta.initialCookies.filter(isCookieDelta)
          : [];
        const meta: BackendMeta = {
          nodeId: this.nodeId,
          backendOrigin: typeof rawMeta.backendOrigin === 'string' ? rawMeta.backendOrigin : '',
          isStaticMode: Boolean(rawMeta.isStaticMode),
          sameSiteMode: this.sameSiteMode,
          pinnedPk: this.pinnedPkHash || undefined,
          initialCookies,
        };

        this.state = 'AUTHENTICATED';
        this.callbacks.onLog(`[Auth] DTLS channel authenticated (role=${expectedRole})`);
        this.callbacks.onAuthenticated(
          {
            role: expectedRole,
            meta,
            pkHash: ctx.verifiedPkHash,
            keyStatus: ctx.verifiedKeyStatus,
          },
          ctx.mux
        );
        break;
      }

      case FrameType.CAP_RESP: {
        if (this.state !== 'AUTHENTICATED') return;
        const reqId = typeof parsed.req_id === 'string' ? parsed.req_id : '';
        const pending = this.pendingCapReqs.get(reqId);
        if (!pending) return;
        this.pendingCapReqs.delete(reqId);
        clearTimeout(pending.timer);

        const payloadObj = isRecord(parsed.payload) ? parsed.payload : null;
        const jti =
          typeof parsed.jti === 'string'
            ? parsed.jti
            : payloadObj && typeof payloadObj.jti === 'string'
              ? payloadObj.jti
              : undefined;
        const rawItems = Array.isArray(parsed.items)
          ? parsed.items
          : Array.isArray(parsed.records)
            ? parsed.records
            : undefined;
        const records = rawItems ? rawItems.filter(isCapabilityRecordDTO) : undefined;

        pending.resolve({
          ok: Boolean(parsed.ok),
          error: typeof parsed.error === 'string' ? parsed.error : undefined,
          token: typeof parsed.token === 'string' ? parsed.token : undefined,
          jti,
          pkHash: typeof parsed.pk_hash === 'string' ? parsed.pk_hash : undefined,
          records,
          total: typeof parsed.total === 'number' ? parsed.total : undefined,
          offset: typeof parsed.offset === 'number' ? parsed.offset : undefined,
          limit: typeof parsed.limit === 'number' ? parsed.limit : undefined,
          hasMore: typeof parsed.has_more === 'boolean' ? parsed.has_more : undefined,
        });
        break;
      }
    }
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
    if (this.state !== 'AUTHENTICATED' || !this.channelCtx) {
      return Promise.reject(new Error('Not authenticated'));
    }
    const mux = this.channelCtx.mux;
    const reqId = randomBase64Url(12);
    const body = {
      req_id: reqId,
      action: payload.action,
      ttl_sec: payload.ttlSec,
      path_prefix: payload.pathPrefix,
      readonly: payload.readonly,
      max_conns: payload.maxConns,
      jti: payload.jti,
      offset: payload.offset,
      limit: payload.limit,
    };

    return new Promise<CapMintResult>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pendingCapReqs.delete(reqId);
        reject(new Error('Capability request timed out'));
      }, 10_000);
      this.pendingCapReqs.set(reqId, { resolve, reject, timer });
      try {
        mux.sendControlFrame(FrameType.CAP_REQ, textEncoder.encode(JSON.stringify(body)));
      } catch (err) {
        clearTimeout(timer);
        this.pendingCapReqs.delete(reqId);
        reject(err instanceof Error ? err : new Error(String(err)));
      }
    });
  }

  destroy(): void {
    this.resetChannel('client destroyed');
    this.spakeCreds = null;
    this.spakeCredsAgentPk = '';
    this.cookieResumeToken = '';
  }

  private async sendAuthResponse(gen: number, method: AuthMethod): Promise<void> {
    if (gen !== this.channelGen || !this.channelCtx || this.state !== 'WAIT_CHALLENGE') return;
    const ctx = this.channelCtx;
    const resp: Record<string, unknown> = { method };
    if (this.cookieResumeToken) {
      resp.cookie_resume_token = this.cookieResumeToken;
    }

    if (method === 'cap') {
      resp.cap_token = this.capToken;
    } else if (method === 'spake2p') {
      if (!this.spakeCreds || this.spakeCredsAgentPk !== ctx.agentPkB64 || !this.currentShareVB64) {
        return;
      }
      const spakeCtx = buildSPAKE2PContext(
        this.nodeId,
        ctx.offerDesc.fp,
        ctx.answerDesc.fp,
        ctx.agentPkB64,
        ctx.visitorNonce,
        this.currentAgentNonce
      );
      const res = await spake2pProverCompute(
        this.spakeCreds,
        spakeCtx,
        base64UrlDecode(this.currentShareVB64)
      );
      if (gen !== this.channelGen || this.channelCtx !== ctx || this.state !== 'WAIT_CHALLENGE') {
        return;
      }
      this.expectedConfirmV = res.expectedConfirmV;
      resp.share_p = base64UrlEncode(res.shareP);
      resp.confirm_p = base64UrlEncode(res.confirmP);
    }

    // Transition WAIT_CHALLENGE -> WAIT_RESULT strictly BEFORE sending wire frame.
    this.sentMethod = method;
    this.state = 'WAIT_RESULT';
    ctx.mux.sendControlFrame(FrameType.AUTH_RESPONSE, textEncoder.encode(JSON.stringify(resp)));
  }

  private failSecurity(reason: string): void {
    this.state = 'FAILED';
    this.expectedConfirmV = null;
    this.callbacks.onLog(`[Security] BLOCKED: ${reason}`);
    this.callbacks.onFatalSecurityError(reason);
  }

  private failPendingCapReqs(reason: string): void {
    for (const [, p] of this.pendingCapReqs) {
      clearTimeout(p.timer);
      p.reject(new Error(reason));
    }
    this.pendingCapReqs.clear();
  }
}
