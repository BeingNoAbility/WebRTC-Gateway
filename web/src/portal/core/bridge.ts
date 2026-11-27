/**
 * V7 Portal TunnelBridge (Untrusted Sandbox Boundary)
 * - Bridges Service Worker `bridgePort` <-> `PoolMux` (32 reusable HTTP pool DCs + dynamic `"p2pws"` DCs).
 * - Treats the ENTIRE sandbox origin as an untrusted tenant:
 *     * Strips forbidden/spoofed headers before sending V7 TLV to Go Agent.
 *     * Strictly bounds-checks `REQ_BODY_CHUNK` offsets/lengths & validates `WS_CLOSE` arguments.
 *     * Enforces active request-port (256) and WebSocket (64) concurrency caps.
 *     * Unconditionally resets half-open streams in `closeBoth()` so early responses on body-bearing
 *       requests never leak HTTP pool slots.
 *     * Binds `wsPort.onmessage` BEFORE `await openWSStream()` so early frames/closes are never dropped.
 *     * Registers one-time 128-bit `enterNonce` tokens with SW over `bridgePort` before mounting `appFrame`.
 *     * Propagates V7 `cookieDeltas` back to Portal so SW wakeups never roll back cookies.
 */

import { ErrorCode, MAX_WS_MESSAGE_BYTES } from '../../protocol/frame';
import type { DataChannelLike, PoolMux, PoolStream, WSStream } from '../../protocol/mux';
import {
  CookieOp,
  FETCH_CTX_VALID_MASK,
  MAX_REQUEST_COOKIE_MUTATIONS,
  StreamKind,
  decodeResponseHeaders,
  encodeRequestHeaders,
  type CookieTLVEntry,
  type HeaderField,
} from '../../protocol/tlv';
import {
  isClientCookieMutation,
  isRecord,
  type BackendMeta,
  type CookieDelta,
  type RequestPortUpMsg,
  type TunnelFetchRequestMsg,
  type TunnelWsOpenMsg,
  type WsPortToPortalMsg,
} from '../../shared/ipc';

const MAX_CONCURRENT_REQ_PORTS = 256;
const MAX_CONCURRENT_WS_STREAMS = 64;
const MAX_REQ_BODY_CHUNK_BYTES = 256 * 1024;
const WS_CLOSE_ACK_TIMEOUT_MS = 2_000;

const FORBIDDEN_SANDBOX_REQ_HEADERS = new Set([
  'host',
  'cookie',
  'x-client-cookie',
  'connection',
  'keep-alive',
  'transfer-encoding',
  'upgrade',
  'te',
  'trailer',
  'proxy-authorization',
  'proxy-authenticate',
  'x-forwarded-for',
  'x-forwarded-host',
  'x-forwarded-proto',
  'x-forwarded-port',
  'x-real-ip',
  'cf-connecting-ip',
  'true-client-ip',
  'x-original-url',
  'x-rewrite-url',
  'forwarded',
  'x-p2p-dest-host',
  'x-p2p-dest-scheme',
]);

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();

export interface TunnelBridgeCallbacks {
  onRouteSync?: (path: string) => void;
  onTitleSync?: (title: string) => void;
  onCookieDeltas?: (deltas: CookieDelta[]) => void;
  onLog?: (line: string) => void;
}

export function sanitizeSandboxHeaders(raw: unknown): HeaderField[] {
  if (!Array.isArray(raw)) return [];
  const out: HeaderField[] = [];
  for (const item of raw) {
    if (!isRecord(item) || typeof item.name !== 'string' || typeof item.value !== 'string') continue;
    const name = item.name.trim().toLowerCase();
    if (!name || FORBIDDEN_SANDBOX_REQ_HEADERS.has(name)) continue;
    out.push({ name, value: item.value });
    if (out.length >= 256) break;
  }
  return out;
}

function toCookieTLVEntries(raw: unknown): CookieTLVEntry[] {
  if (!Array.isArray(raw)) return [];
  const out: CookieTLVEntry[] = [];
  for (const item of raw) {
    if (!isClientCookieMutation(item)) continue;
    out.push({
      op: item.op === 'del' ? CookieOp.DEL : CookieOp.SET,
      name: item.name,
      value: item.value ?? '',
      path: item.path ?? '/',
      expiresMs: typeof item.expires === 'number' && Number.isFinite(item.expires) ? Math.trunc(item.expires) : 0,
    });
    if (out.length >= MAX_REQUEST_COOKIE_MUTATIONS) break;
  }
  return out;
}

function fromCookieTLVEntries(entries: CookieTLVEntry[] | undefined): CookieDelta[] {
  if (!entries || entries.length === 0) return [];
  return entries.map((e) => {
    const out: CookieDelta = {
      op: e.op === CookieOp.DEL ? 'del' : 'set',
      name: e.name,
      path: e.path || '/',
    };
    if (out.op === 'set') {
      out.value = e.value;
    }
    if (e.expiresMs > 0) {
      out.expires = e.expiresMs;
    }
    return out;
  });
}

function isValidChunkBounds(up: { buffer?: unknown; offset?: unknown; length?: unknown }): boolean {
  if (!(up.buffer instanceof ArrayBuffer)) return false;
  if (typeof up.offset !== 'number' || typeof up.length !== 'number') return false;
  if (!Number.isInteger(up.offset) || !Number.isInteger(up.length)) return false;
  if (up.offset < 0 || up.length < 0 || up.length > MAX_REQ_BODY_CHUNK_BYTES) return false;
  if (up.offset + up.length > up.buffer.byteLength) return false;
  return true;
}

function sanitizeClientWSCloseCode(code: unknown): number {
  if (typeof code === 'number' && Number.isInteger(code)) {
    if (code === 1000 || (code >= 3000 && code <= 4999)) {
      return code;
    }
  }
  return 1000;
}

export class TunnelBridge {
  private mux: PoolMux | null = null;
  private openWSDCFn: (() => DataChannelLike) | null = null;
  private sandboxOrigin = '';
  private meta: BackendMeta | null = null;
  private appFrame: HTMLIFrameElement | null = null;
  private bridgePort: MessagePort | null = null;
  private activePorts = new Set<MessagePort>();
  private activeWSStreams = new Map<string, () => void>();
  private enterNonceWaiters = new Map<string, { resolve: () => void; reject: (err: Error) => void; timer: ReturnType<typeof setTimeout> }>();
  private readonly boundWindowMsg: (ev: MessageEvent) => void;

  constructor(private readonly callbacks: TunnelBridgeCallbacks = {}) {
    this.boundWindowMsg = (ev: MessageEvent) => this.handleWindowMessage(ev);
  }

  attachWindowListener(): void {
    window.addEventListener('message', this.boundWindowMsg);
  }

  detachWindowListener(): void {
    window.removeEventListener('message', this.boundWindowMsg);
  }

  setContext(
    mux: PoolMux,
    openWSDC: () => DataChannelLike,
    sandboxOrigin: string,
    meta: BackendMeta,
    appFrame: HTMLIFrameElement | null
  ): void {
    this.mux = mux;
    this.openWSDCFn = openWSDC;
    this.sandboxOrigin = sandboxOrigin;
    this.meta = meta;
    this.appFrame = appFrame;
    if (this.bridgePort) {
      try {
        this.bridgePort.postMessage({ type: 'BRIDGE_META_UPDATE', meta });
      } catch {
        // ignore
      }
    }
  }

  setAppFrame(appFrame: HTMLIFrameElement | null): void {
    this.appFrame = appFrame;
  }

  /**
   * Registers a one-time 128-bit `enterNonce` with the Service Worker over `bridgePort`
   * BEFORE mounting `appFrame`, preventing external sites from invoking `/__p2p_gw__/enter`.
   */
  registerEnterNonce(nonce: string, nextPath: string, timeoutMs = 4_000): Promise<void> {
    if (!this.bridgePort) {
      return Promise.reject(new Error('bridgePort not attached'));
    }
    return new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.enterNonceWaiters.delete(nonce);
        reject(new Error('Timed out waiting for SW enterNonce registration ACK'));
      }, timeoutMs);
      this.enterNonceWaiters.set(nonce, { resolve, reject, timer });
      try {
        this.bridgePort!.postMessage({
          type: 'BRIDGE_REGISTER_ENTER_NONCE',
          nonce,
          nextPath,
        });
      } catch (err) {
        clearTimeout(timer);
        this.enterNonceWaiters.delete(nonce);
        reject(err instanceof Error ? err : new Error(String(err)));
      }
    });
  }

  attachBridgePort(port: MessagePort): void {
    if (this.bridgePort && this.bridgePort !== port) {
      this.bridgePort.onmessage = null;
      try {
        this.bridgePort.close();
      } catch {
        // ignore
      }
    }
    this.bridgePort = port;
    port.onmessage = (ev: MessageEvent) => {
      const data = ev.data;
      if (!isRecord(data) || typeof data.type !== 'string') return;

      switch (data.type) {
        case 'BRIDGE_PING':
          if (typeof data.pingId === 'string') {
            try {
              port.postMessage({ type: 'BRIDGE_PONG', pingId: data.pingId });
            } catch {
              // ignore
            }
          }
          break;

        case 'BRIDGE_ENTER_NONCE_ACK':
          if (typeof data.nonce === 'string') {
            const w = this.enterNonceWaiters.get(data.nonce);
            if (w) {
              this.enterNonceWaiters.delete(data.nonce);
              clearTimeout(w.timer);
              w.resolve();
            }
          }
          break;

        case 'TUNNEL_FETCH_REQUEST': {
          const reqPort = ev.ports?.[0];
          if (!reqPort) return;
          void this.handleFetchRequest(data as unknown as TunnelFetchRequestMsg, reqPort);
          break;
        }

        case 'TUNNEL_WS_OPEN': {
          const wsPort = ev.ports?.[0];
          if (!wsPort) return;
          void this.handleWsOpen(data as unknown as TunnelWsOpenMsg, wsPort);
          break;
        }

        case 'TUNNEL_WS_ABORT': {
          const wsId = typeof data.wsId === 'string' ? data.wsId : '';
          const abortFn = this.activeWSStreams.get(wsId);
          if (abortFn) {
            this.activeWSStreams.delete(wsId);
            abortFn();
          }
          break;
        }
      }
    };
    port.start?.();
  }

  /**
   * Handles top-level window postMessages ONLY for pure UX route/title sync.
   */
  handleWindowMessage(ev: MessageEvent): void {
    if (!this.sandboxOrigin || ev.origin !== this.sandboxOrigin) return;
    if (!this.appFrame || ev.source !== this.appFrame.contentWindow) return;
    const data = ev.data;
    if (!isRecord(data) || typeof data.type !== 'string') return;

    if (data.type === 'P2P_ROUTE_SYNC' && typeof data.path === 'string' && data.path.startsWith('/')) {
      this.callbacks.onRouteSync?.(data.path);
    } else if (data.type === 'P2P_TITLE_SYNC' && typeof data.title === 'string') {
      this.callbacks.onTitleSync?.(data.title.slice(0, 200));
    }
  }

  async handleFetchRequest(msg: TunnelFetchRequestMsg, reqPort: MessagePort): Promise<void> {
    try {
      reqPort.postMessage({ type: 'REQ_ACK', reqId: msg.reqId });
    } catch {
      this.closePort(reqPort);
      return;
    }

    if (!this.mux || !this.meta) {
      this.failReqPort(reqPort, 503, 'P2P tunnel is not connected');
      return;
    }
    if (this.activePorts.size >= MAX_CONCURRENT_REQ_PORTS) {
      this.failReqPort(reqPort, 503, 'Too many concurrent tunnel requests');
      return;
    }
    if (typeof msg.target !== 'string' || !msg.target.startsWith('/')) {
      this.failReqPort(reqPort, 400, 'Invalid request target');
      return;
    }

    const reqMethod = (msg.method || 'GET').toUpperCase();
    const fetchCtx =
      typeof msg.fetchCtx === 'number' && Number.isInteger(msg.fetchCtx)
        ? msg.fetchCtx & FETCH_CTX_VALID_MASK
        : 0;
    const cookieMutations = toCookieTLVEntries(msg.cookieMutations);
    let reqTLV: Uint8Array;
    try {
      reqTLV = encodeRequestHeaders({
        kind: StreamKind.HTTP,
        fetchCtx,
        method: reqMethod,
        target: msg.target,
        headers: sanitizeSandboxHeaders(msg.headers),
        cookieMutations,
      });
    } catch (err) {
      const reason = err instanceof Error ? err.message : 'Invalid request headers';
      this.failReqPort(reqPort, 400, reason);
      return;
    }

    this.activePorts.add(reqPort);
    let portClosed = false;
    let headersSentToSw = false;
    let activeStream: PoolStream | null = null;
    const queuedUpMsgs: RequestPortUpMsg[] = [];
    let writeChain: Promise<void> = Promise.resolve();
    const openAbort = new AbortController();

    // Always reset `activeStream` whenever `!activeStream.isClosed` on closeBoth() so early
    // responses (e.g. 204/3xx/4xx on POST) never leave writeClosed=false and leak pool slots.
    const closeBoth = (resetCode?: number) => {
      if (portClosed) return;
      portClosed = true;
      openAbort.abort();
      this.closePort(reqPort);
      if (activeStream && !activeStream.isClosed) {
        activeStream.reset(resetCode ?? ErrorCode.CANCEL);
      }
    };

    const dispatchUpMsg = (up: RequestPortUpMsg, stream: PoolStream) => {
      switch (up.type) {
        case 'REQ_BODY_CHUNK': {
          if (!isValidChunkBounds(up)) {
            this.failReqPort(reqPort, 400, 'Invalid request body chunk bounds');
            closeBoth(ErrorCode.PROTOCOL_ERROR);
            return;
          }
          const bytes = new Uint8Array(up.buffer, up.offset, up.length);
          const len = bytes.byteLength;
          writeChain = writeChain.then(async () => {
            if (portClosed || stream.isClosed) return;
            try {
              await stream.sendData(bytes, false);
              if (!portClosed) {
                reqPort.postMessage({ type: 'REQ_BODY_CREDIT', bytes: len });
              }
            } catch (err) {
              const reason = err instanceof Error ? err.message : 'Upload stream write failed';
              stream.reset(ErrorCode.INTERNAL_ERROR);
              this.failReqPort(reqPort, 502, reason);
              closeBoth();
            }
          });
          break;
        }
        case 'REQ_BODY_END': {
          writeChain = writeChain.then(async () => {
            if (portClosed || stream.isClosed) return;
            try {
              await stream.finishWrite();
            } catch (err) {
              const reason = err instanceof Error ? err.message : 'Upload stream close failed';
              stream.reset(ErrorCode.INTERNAL_ERROR);
              this.failReqPort(reqPort, 502, reason);
              closeBoth();
            }
          });
          break;
        }
        case 'RESP_BODY_CREDIT': {
          if (typeof up.bytes === 'number' && Number.isFinite(up.bytes) && up.bytes > 0) {
            stream.ackConsumed(Math.trunc(up.bytes));
          }
          break;
        }
        case 'RESP_CANCEL': {
          closeBoth(ErrorCode.CANCEL);
          break;
        }
      }
    };

    // Attach reqPort listener BEFORE awaiting pool slot so early RESP_CANCEL or chunks are captured!
    reqPort.onmessage = (ev: MessageEvent) => {
      if (portClosed) return;
      const up = ev.data as RequestPortUpMsg;
      if (!isRecord(up) || typeof up.type !== 'string') return;
      if (!activeStream) {
        if (up.type === 'RESP_CANCEL') {
          closeBoth(ErrorCode.CANCEL);
        } else if (queuedUpMsgs.length < 128) {
          queuedUpMsgs.push(up);
        } else {
          closeBoth(ErrorCode.FLOW_CONTROL_ERROR);
        }
        return;
      }
      dispatchUpMsg(up, activeStream);
    };
    reqPort.start?.();

    const finOnHeaders = !msg.hasBody;
    let stream: PoolStream;
    try {
      stream = await this.mux.openHTTPStream(reqTLV, finOnHeaders, true, openAbort.signal);
    } catch (err) {
      if (!portClosed) {
        const m = err instanceof Error ? err.message : 'Unable to open P2P HTTP stream';
        this.failReqPort(reqPort, 503, m);
      }
      return;
    }

    if (portClosed) {
      stream.reset(ErrorCode.CANCEL);
      return;
    }
    activeStream = stream;

    stream.onHeaders((raw) => {
      if (portClosed) return;
      let resp;
      try {
        resp = decodeResponseHeaders(raw);
      } catch (err) {
        const m = err instanceof Error ? err.message : 'Malformed response headers';
        this.failReqPort(reqPort, 502, m);
        closeBoth(ErrorCode.PROTOCOL_ERROR);
        return;
      }

      headersSentToSw = true;
      const isNullBody =
        reqMethod === 'HEAD' ||
        resp.status === 204 ||
        resp.status === 205 ||
        resp.status === 304 ||
        (resp.status >= 100 && resp.status < 200);
      const hasBody = !isNullBody && stream.hasResponseBody;
      const cookieDeltas = fromCookieTLVEntries(resp.cookieDeltas);
      if (cookieDeltas.length > 0) {
        this.callbacks.onCookieDeltas?.(cookieDeltas);
      }
      try {
        reqPort.postMessage({
          type: 'RESP_START',
          status: resp.status,
          respFlags: resp.respFlags ?? 0,
          headers: resp.headers,
          cookieDeltas,
          hasBody,
        });
      } catch {
        closeBoth(ErrorCode.CANCEL);
        return;
      }
      if (!hasBody) {
        closeBoth();
      }
    });

    stream.onData((chunk) => {
      if (portClosed) return;
      const copy = new Uint8Array(chunk.byteLength);
      copy.set(chunk);
      try {
        reqPort.postMessage(
          { type: 'RESP_CHUNK', buffer: copy.buffer, offset: 0, length: copy.byteLength },
          [copy.buffer]
        );
      } catch {
        closeBoth(ErrorCode.CANCEL);
      }
    });

    stream.onEnd(() => {
      if (portClosed) return;
      if (!headersSentToSw) {
        this.failReqPort(reqPort, 502, 'Upstream closed stream without response headers');
      } else {
        try {
          reqPort.postMessage({ type: 'RESP_END' });
        } catch {
          // ignore
        }
      }
      closeBoth();
    });

    stream.onReset((code) => {
      if (portClosed) return;
      this.failReqPort(reqPort, 502, `P2P stream reset (${code})`);
      closeBoth();
    });

    // Drain any upstream messages queued while waiting for a pool DC slot.
    for (const queued of queuedUpMsgs.splice(0)) {
      if (portClosed) break;
      dispatchUpMsg(queued, stream);
    }
  }

  private async handleWsOpen(msg: TunnelWsOpenMsg, wsPort: MessagePort): Promise<void> {
    if (!this.mux || !this.openWSDCFn) {
      this.rejectWsPort(wsPort, 'P2P tunnel is not connected');
      return;
    }
    if (this.activeWSStreams.size >= MAX_CONCURRENT_WS_STREAMS) {
      this.rejectWsPort(wsPort, 'Too many concurrent WebSocket connections');
      return;
    }
    if (typeof msg.target !== 'string' || !msg.target.startsWith('/')) {
      this.rejectWsPort(wsPort, 'Invalid WebSocket target');
      return;
    }

    const headers: HeaderField[] = [];
    const protocols = Array.isArray(msg.protocols)
      ? msg.protocols.filter((p): p is string => typeof p === 'string').slice(0, 16)
      : [];
    if (protocols.length > 0) {
      headers.push({ name: 'sec-websocket-protocol', value: protocols.join(', ') });
    }
    const cookieMutations = toCookieTLVEntries(msg.cookieMutations);
    const fetchCtx =
      typeof msg.fetchCtx === 'number' && Number.isInteger(msg.fetchCtx)
        ? msg.fetchCtx & FETCH_CTX_VALID_MASK
        : 0;

    let reqTLV: Uint8Array;
    try {
      reqTLV = encodeRequestHeaders({
        kind: StreamKind.WS,
        fetchCtx,
        method: 'GET',
        target: msg.target,
        headers,
        cookieMutations,
      });
    } catch (err) {
      this.rejectWsPort(wsPort, err instanceof Error ? err.message : 'Invalid WS headers');
      return;
    }

    this.activePorts.add(wsPort);
    let closed = false;
    let opened = false;
    let wsStream: WSStream | null = null;
    let writeChain: Promise<void> = Promise.resolve();
    let closeAckTimer: ReturnType<typeof setTimeout> | null = null;
    const queuedWsMsgs: WsPortToPortalMsg[] = [];
    const wsId = typeof msg.wsId === 'string' ? msg.wsId : '';

    const finishWs = (code: number, reason: string, notifyPort: boolean, resetStream: boolean) => {
      if (closed) return;
      closed = true;
      if (closeAckTimer) {
        clearTimeout(closeAckTimer);
        closeAckTimer = null;
      }
      if (wsId) this.activeWSStreams.delete(wsId);
      if (notifyPort) {
        try {
          wsPort.postMessage({ type: 'WS_CLOSE', code, reason });
        } catch {
          // ignore
        }
      }
      this.closePort(wsPort);
      if (resetStream && wsStream && !wsStream.isClosed) {
        wsStream.reset(ErrorCode.CANCEL);
      }
    };

    const dispatchWsPortMsg = (m: WsPortToPortalMsg, activeWS: WSStream) => {
      switch (m.type) {
        case 'WS_SEND': {
          const isBinary = m.opcode === 2;
          const payload = isBinary
            ? m.data instanceof ArrayBuffer
              ? new Uint8Array(m.data)
              : new Uint8Array(0)
            : textEncoder.encode(typeof m.data === 'string' ? m.data : '');
          if (payload.byteLength > MAX_WS_MESSAGE_BYTES) {
            finishWs(1009, 'Outbound WebSocket frame exceeds 16 MiB limit', true, true);
            return;
          }
          const ackLen = payload.byteLength;
          writeChain = writeChain.then(async () => {
            if (closed || activeWS.isClosed) return;
            try {
              await activeWS.sendWSMessage(isBinary, payload);
              if (!closed) {
                wsPort.postMessage({ type: 'WS_ACK', bytes: ackLen });
              }
            } catch (err) {
              const reason = err instanceof Error ? err.message : 'WS write failed';
              finishWs(1006, reason, true, true);
            }
          });
          break;
        }
        case 'WS_CLOSE': {
          const safeCode = sanitizeClientWSCloseCode(m.code);
          const safeReason = typeof m.reason === 'string' ? m.reason.slice(0, 256) : '';
          writeChain = writeChain.then(() => {
            if (closed) return;
            if (!activeWS.isClosed) {
              activeWS.sendWSClose(safeCode, safeReason);
            }
            // Wait up to 2s for peer WS_CLOSE echo so close code & wasClean propagate accurately.
            if (!closeAckTimer) {
              closeAckTimer = setTimeout(() => {
                finishWs(safeCode, safeReason, true, true);
              }, WS_CLOSE_ACK_TIMEOUT_MS);
            }
          });
          break;
        }
      }
    };

    // Bind wsPort.onmessage BEFORE awaiting openWSStream() so early WS_CLOSE/WS_SEND are never lost!
    wsPort.onmessage = (ev: MessageEvent) => {
      if (closed) return;
      const m = ev.data as WsPortToPortalMsg;
      if (!isRecord(m) || typeof m.type !== 'string') return;
      if (!wsStream) {
        if (m.type === 'WS_CLOSE') {
          const safeCode = sanitizeClientWSCloseCode(m.code);
          const safeReason = typeof m.reason === 'string' ? m.reason.slice(0, 256) : '';
          finishWs(safeCode, safeReason, false, false);
        } else if (queuedWsMsgs.length < 64) {
          queuedWsMsgs.push(m);
        }
        return;
      }
      dispatchWsPortMsg(m, wsStream);
    };
    wsPort.start?.();

    if (wsId) {
      this.activeWSStreams.set(wsId, () => finishWs(1001, 'client navigated away', false, true));
    }

    let wsDC: DataChannelLike;
    try {
      wsDC = this.openWSDCFn();
      wsStream = await this.mux.openWSStream(wsDC, reqTLV);
    } catch (err) {
      if (!closed) {
        const reason = err instanceof Error ? err.message : 'Unable to open WS DataChannel';
        this.rejectWsPort(wsPort, reason);
        finishWs(1006, reason, false, false);
      }
      return;
    }

    if (closed) {
      wsStream.reset(ErrorCode.CANCEL);
      return;
    }

    const activeWS = wsStream;
    activeWS.onHeaders((raw) => {
      if (closed) return;
      let resp;
      try {
        resp = decodeResponseHeaders(raw);
      } catch {
        this.rejectWsPort(wsPort, 'Malformed WS handshake response');
        finishWs(1006, 'malformed ws headers', false, true);
        return;
      }
      if (resp.status !== 101) {
        this.rejectWsPort(wsPort, `WebSocket handshake failed (HTTP ${resp.status})`);
        finishWs(1006, `HTTP ${resp.status}`, false, true);
        return;
      }
      let negotiatedProto = '';
      for (const h of resp.headers) {
        if (h.name === 'sec-websocket-protocol') {
          negotiatedProto = h.value;
          break;
        }
      }
      const cookieDeltas = fromCookieTLVEntries(resp.cookieDeltas);
      if (cookieDeltas.length > 0) {
        this.callbacks.onCookieDeltas?.(cookieDeltas);
      }
      opened = true;
      try {
        wsPort.postMessage({
          type: 'WS_OPEN',
          protocol: negotiatedProto,
          cookieDeltas,
        });
      } catch {
        finishWs(1006, 'ws port closed', false, true);
      }
    });

    activeWS.onMessage((isBinary, payload) => {
      if (closed) return;
      try {
        if (isBinary) {
          const copy = new Uint8Array(payload.byteLength);
          copy.set(payload);
          wsPort.postMessage({ type: 'WS_MSG', opcode: 2, data: copy.buffer }, [copy.buffer]);
        } else {
          wsPort.postMessage({ type: 'WS_MSG', opcode: 1, data: textDecoder.decode(payload) });
        }
      } catch {
        finishWs(1006, 'ws port closed', false, true);
      }
    });

    activeWS.onClose((code, reason) => {
      finishWs(code || 1000, reason || '', true, false);
    });

    activeWS.onReset((code) => {
      if (!opened) {
        this.rejectWsPort(wsPort, `WebSocket stream reset (${code})`);
        finishWs(1006, `reset ${code}`, false, false);
      } else {
        finishWs(1006, `WebSocket reset (${code})`, true, false);
      }
    });

    for (const queued of queuedWsMsgs.splice(0)) {
      if (closed) break;
      dispatchWsPortMsg(queued, activeWS);
    }
  }

  destroy(): void {
    for (const w of this.enterNonceWaiters.values()) {
      clearTimeout(w.timer);
      w.reject(new Error('bridge destroyed'));
    }
    this.enterNonceWaiters.clear();
    for (const abortFn of Array.from(this.activeWSStreams.values())) {
      abortFn();
    }
    this.activeWSStreams.clear();
    if (this.bridgePort) {
      this.bridgePort.onmessage = null;
      try {
        this.bridgePort.close();
      } catch {
        // ignore
      }
      this.bridgePort = null;
    }
    for (const port of this.activePorts) {
      port.onmessage = null;
      try {
        port.close();
      } catch {
        // ignore
      }
    }
    this.activePorts.clear();
    this.mux = null;
    this.openWSDCFn = null;
  }

  private failReqPort(port: MessagePort, status: number, message: string): void {
    try {
      port.postMessage({ type: 'RESP_ERROR', status, message });
    } catch {
      // ignore
    }
    this.closePort(port);
  }

  private rejectWsPort(port: MessagePort, message: string): void {
    try {
      port.postMessage({ type: 'WS_ERROR', message });
    } catch {
      // ignore
    }
    this.closePort(port);
  }

  private closePort(port: MessagePort): void {
    this.activePorts.delete(port);
    port.onmessage = null;
    try {
      port.close();
    } catch {
      // ignore
    }
  }
}
