/**
 * Portal TunnelBridge (Untrusted Sandbox Boundary — P2PMux V8 + Transferable Streams)
 * - Bridges Service Worker `bridgePort` <-> P2PMux V8 (`PoolMux` 12 ordered lanes carrying both HTTP & WS).
 * - Uses native Transferable ReadableStreams over MessagePort for HTTP request & response bodies
 *   (zero manual IPC chunk/credit loops).
 * - Strips forbidden/spoofed headers, enforces concurrency caps (256 HTTP / 64 WS), sends `REQ_ACK`
 *   immediately after `openHTTPStream` puts `HEADERS` on the wire, and pushes WS `cookieDeltas`
 *   back to SW over the trusted `bridgePort`.
 */

import { ErrorCode, MAX_WS_MESSAGE_BYTES } from '../../protocol/frame';
import type { PoolMux, PoolStream, WSStream } from '../../protocol/mux';
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
  isCookieDelta,
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
const MAX_PENDING_WS_WRITE_BYTES = MAX_WS_MESSAGE_BYTES; // 16 MiB strict per-WS outbound buffer cap
const WS_CLOSE_ACK_TIMEOUT_MS = 2_000;
const DOWNLOAD_STREAM_HWM_BYTES = 512 * 1024;
const MAX_ROUTE_SYNC_LEN = 4096;

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
  'x-forwarded-prefix',
  'x-original-host',
  'x-host',
  'x-forwarded-ssl',
  'front-end-https',
  'x-client-ip',
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

export interface TunnelBridgeCallbacks {
  onRouteSync?: (path: string) => void;
  onTitleSync?: (title: string) => void;
  onCookieDeltas?: (deltas: CookieDelta[]) => void;
  onSwEvicted?: (path?: string) => void;
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
    const entry: CookieTLVEntry = {
      op: item.op === 'del' ? CookieOp.DEL : CookieOp.SET,
      name: item.name,
      value: item.value ?? '',
      path: item.path ?? '/',
      expiresMs: typeof item.expires === 'number' && Number.isFinite(item.expires) ? Math.trunc(item.expires) : 0,
    };
    if (item.secure) entry.secure = true;
    out.push(entry);
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
    if (e.secure) {
      out.secure = true;
    }
    return out;
  });
}

function sanitizeClientWSCloseCode(code: unknown): number {
  if (typeof code === 'number' && Number.isInteger(code)) {
    if (code === 1000 || (code >= 3000 && code <= 4999)) {
      return code;
    }
  }
  return 1000;
}

function toUint8Chunk(chunk: unknown): Uint8Array | null {
  if (chunk instanceof Uint8Array) return chunk;
  if (ArrayBuffer.isView(chunk)) {
    return new Uint8Array(chunk.buffer, chunk.byteOffset, chunk.byteLength);
  }
  if (chunk instanceof ArrayBuffer) return new Uint8Array(chunk);
  return null;
}

export class TunnelBridge {
  private mux: PoolMux | null = null;
  private sandboxOrigin = '';
  private meta: BackendMeta | null = null;
  private appFrame: HTMLIFrameElement | null = null;
  private bridgePort: MessagePort | null = null;
  private activePorts = new Set<MessagePort>();
  private wsKeySeq = 0;
  private activeWSStreams = new Map<number, () => void>();
  private wsAliasToKey = new Map<string, number>();
  private enterNonceWaiters = new Map<string, { resolve: () => void; reject: (err: Error) => void; timer: ReturnType<typeof setTimeout> }>();
  private swPingSeq = 0;
  private swPingWaiters = new Map<string, { resolve: (alive: boolean) => void; timer: ReturnType<typeof setTimeout> }>();
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
    sandboxOrigin: string,
    meta: BackendMeta,
    appFrame: HTMLIFrameElement | null
  ): void {
    this.mux = mux;
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

  pingSw(timeoutMs = 3_000): Promise<boolean> {
    if (!this.bridgePort) return Promise.resolve(false);
    const pingId = `sp_${++this.swPingSeq}`;
    return new Promise<boolean>((resolve) => {
      const timer = setTimeout(() => {
        this.swPingWaiters.delete(pingId);
        resolve(false);
      }, timeoutMs);
      this.swPingWaiters.set(pingId, { resolve, timer });
      try {
        this.bridgePort!.postMessage({ type: 'PORTAL_PING', pingId });
      } catch {
        clearTimeout(timer);
        this.swPingWaiters.delete(pingId);
        resolve(false);
      }
    });
  }

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

        case 'PORTAL_PONG':
          if (typeof data.pingId === 'string') {
            const w = this.swPingWaiters.get(data.pingId);
            if (w) {
              this.swPingWaiters.delete(data.pingId);
              clearTimeout(w.timer);
              w.resolve(true);
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

        case 'BRIDGE_CLIENT_COOKIE_DELTAS':
          if (Array.isArray(data.deltas)) {
            const valid = data.deltas.filter(isCookieDelta);
            if (valid.length > 0) {
              this.callbacks.onCookieDeltas?.(valid);
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
          if (!wsId) break;
          const key = this.wsAliasToKey.get(wsId);
          if (key !== undefined) {
            this.wsAliasToKey.delete(wsId);
            const abortFn = this.activeWSStreams.get(key);
            if (abortFn) {
              this.activeWSStreams.delete(key);
              abortFn();
            }
          }
          break;
        }
      }
    };
    port.start?.();
  }

  handleWindowMessage(ev: MessageEvent): void {
    if (ev.isTrusted === false) return;
    if (!this.sandboxOrigin || ev.origin !== this.sandboxOrigin) return;
    const data = ev.data;
    if (!isRecord(data) || typeof data.type !== 'string') return;

    // P2P_SW_EVICTED may originate from a nested child iframe on the same sandboxOrigin;
    // it only triggers a safe Portal-side SW liveness check / recovery.
    // Only accept a recovery path override when posted directly by the root appFrame.
    if (data.type === 'P2P_SW_EVICTED') {
      let recoveryPath: string | undefined;
      if (
        this.appFrame &&
        ev.source === this.appFrame.contentWindow &&
        typeof data.path === 'string' &&
        data.path.startsWith('/') &&
        !data.path.startsWith('//') &&
        data.path.length <= MAX_ROUTE_SYNC_LEN &&
        !/[\r\n\0]/.test(data.path)
      ) {
        recoveryPath = data.path;
      }
      this.callbacks.onSwEvicted?.(recoveryPath);
      return;
    }

    if (!this.appFrame || ev.source !== this.appFrame.contentWindow) return;
    if (
      data.type === 'P2P_ROUTE_SYNC' &&
      typeof data.path === 'string' &&
      data.path.startsWith('/') &&
      data.path.length <= MAX_ROUTE_SYNC_LEN &&
      !/[\x00-\x1f\x7f]/.test(data.path)
    ) {
      this.callbacks.onRouteSync?.(data.path);
    } else if (data.type === 'P2P_TITLE_SYNC' && typeof data.title === 'string') {
      this.callbacks.onTitleSync?.(data.title.slice(0, 200));
    }
  }

  async handleFetchRequest(msg: TunnelFetchRequestMsg, reqPort: MessagePort): Promise<void> {
    const reqBodyStream = msg.hasBody && msg.bodyStream ? msg.bodyStream : null;

    if (!this.mux || !this.meta) {
      void reqBodyStream?.cancel().catch(() => {});
      this.failReqPort(reqPort, 503, 'P2P tunnel is not connected');
      return;
    }
    if (this.activePorts.size >= MAX_CONCURRENT_REQ_PORTS) {
      void reqBodyStream?.cancel().catch(() => {});
      this.failReqPort(reqPort, 503, 'Too many concurrent tunnel requests');
      return;
    }
    if (typeof msg.target !== 'string' || !msg.target.startsWith('/')) {
      void reqBodyStream?.cancel().catch(() => {});
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
      void reqBodyStream?.cancel().catch(() => {});
      const reason = err instanceof Error ? err.message : 'Invalid request headers';
      this.failReqPort(reqPort, 400, reason);
      return;
    }

    this.activePorts.add(reqPort);
    let portClosed = false;
    let activeStream: PoolStream | null = null;
    let uploadReader: ReadableStreamDefaultReader<Uint8Array> | null = null;

    const closeBoth = (resetCode?: number, sendDone = false) => {
      if (portClosed) return;
      portClosed = true;
      if (uploadReader) {
        const r = uploadReader;
        uploadReader = null;
        void r.cancel().catch(() => {});
      } else if (reqBodyStream) {
        void reqBodyStream.cancel().catch(() => {});
      }
      if (sendDone) {
        try {
          reqPort.postMessage({ type: 'RESP_DONE' });
        } catch {
          // ignore
        }
      }
      this.closePort(reqPort);
      if (activeStream && !activeStream.isClosed) {
        activeStream.reset(resetCode ?? ErrorCode.CANCEL);
      }
    };

    reqPort.onmessage = (ev: MessageEvent) => {
      if (portClosed) return;
      const up = ev.data as RequestPortUpMsg;
      if (isRecord(up) && up.type === 'RESP_CANCEL') {
        closeBoth(ErrorCode.CANCEL);
      }
    };
    reqPort.start?.();

    const hasBody = Boolean(msg.hasBody && reqBodyStream);
    let stream: PoolStream;
    try {
      stream = await this.mux.openHTTPStream(reqTLV, hasBody);
    } catch (err) {
      if (!portClosed) {
        const m = err instanceof Error ? err.message : 'Unable to open P2P HTTP stream';
        this.failReqPort(reqPort, 503, m);
        closeBoth();
      }
      return;
    }

    if (portClosed) {
      stream.reset(ErrorCode.CANCEL);
      return;
    }
    activeStream = stream;

    // Emit REQ_ACK strictly AFTER openHTTPStream put HEADERS (with cookieMutations) on the wire!
    try {
      reqPort.postMessage({ type: 'REQ_ACK', reqId: msg.reqId });
    } catch {
      closeBoth(ErrorCode.CANCEL);
      return;
    }

    // Pump request body from transferred ReadableStream directly into P2PMux V8 (native backpressure).
    if (hasBody && reqBodyStream) {
      void (async () => {
        const reader = reqBodyStream.getReader();
        uploadReader = reader;
        try {
          while (!portClosed && !stream.isClosed) {
            const { done, value } = await reader.read();
            if (done) {
              if (!portClosed && !stream.isClosed) {
                await stream.finishWrite();
              }
              return;
            }
            const bytes = toUint8Chunk(value);
            if (!bytes) {
              this.failReqPort(reqPort, 400, 'Invalid request body chunk type');
              closeBoth(ErrorCode.PROTOCOL_ERROR);
              return;
            }
            if (bytes.byteLength === 0) continue;
            await stream.writeBody(bytes);
          }
        } catch (err) {
          if (!portClosed && !stream.isClosed) {
            const reason = err instanceof Error ? err.message : 'Upload stream write failed';
            this.failReqPort(reqPort, 502, reason);
            closeBoth(ErrorCode.INTERNAL_ERROR);
          }
        } finally {
          uploadReader = null;
          try {
            reader.releaseLock();
          } catch {
            // ignore
          }
        }
      })();
    }

    // Await response headers on P2PMux V8 stream.
    let rawRespHeaders: Uint8Array;
    try {
      rawRespHeaders = await stream.waitResponseHeaders();
    } catch (err) {
      if (!portClosed) {
        const m = err instanceof Error ? err.message : 'P2P stream reset before response headers';
        this.failReqPort(reqPort, 502, m);
        closeBoth();
      }
      return;
    }

    if (portClosed) return;

    let resp;
    try {
      resp = decodeResponseHeaders(rawRespHeaders);
    } catch (err) {
      const m = err instanceof Error ? err.message : 'Malformed response headers';
      this.failReqPort(reqPort, 502, m);
      closeBoth(ErrorCode.PROTOCOL_ERROR);
      return;
    }

    const isNullBody =
      reqMethod === 'HEAD' ||
      resp.status === 204 ||
      resp.status === 205 ||
      resp.status === 304 ||
      (resp.status >= 100 && resp.status < 200);
    const hasRespBody = !isNullBody && stream.hasResponseBody;
    const cookieDeltas = fromCookieTLVEntries(resp.cookieDeltas);
    if (cookieDeltas.length > 0) {
      this.callbacks.onCookieDeltas?.(cookieDeltas);
    }

    if (!hasRespBody) {
      try {
        reqPort.postMessage({
          type: 'RESP_START',
          status: resp.status,
          respFlags: resp.respFlags ?? 0,
          headers: resp.headers,
          cookieDeltas,
          hasBody: false,
        });
      } catch {
        // ignore
      }
      // Unconditionally reset half-open upload stream on early 204/3xx/4xx responses!
      closeBoth(ErrorCode.CANCEL, false);
      return;
    }

    // Create a native Transferable ReadableStream backed directly by PoolStream with byte-accurate backpressure.
    let streamController: ReadableStreamDefaultController<Uint8Array> | null = null;
    let unackedBytes = 0;
    let unackedChunks = 0;

    const flushCredit = () => {
      if (unackedBytes > 0) {
        const b = unackedBytes;
        const c = unackedChunks;
        unackedBytes = 0;
        unackedChunks = 0;
        stream.ackConsumed(b, c);
      }
    };

    const queuingStrategy =
      typeof ByteLengthQueuingStrategy === 'function'
        ? new ByteLengthQueuingStrategy({ highWaterMark: DOWNLOAD_STREAM_HWM_BYTES })
        : undefined;

    const respBodyStream = new ReadableStream<Uint8Array>(
      {
        start(controller) {
          streamController = controller;
        },
        pull() {
          flushCredit();
        },
        cancel() {
          streamController = null;
          closeBoth(ErrorCode.CANCEL, false);
        },
      },
      queuingStrategy
    );

    // Post RESP_START (transferring respBodyStream) BEFORE attaching stream.onData/onEnd so
    // buffered single-chunk responses never fire onEnd -> RESP_DONE -> closePort ahead of RESP_START!
    try {
      reqPort.postMessage(
        {
          type: 'RESP_START',
          status: resp.status,
          respFlags: resp.respFlags ?? 0,
          headers: resp.headers,
          cookieDeltas,
          hasBody: true,
          bodyStream: respBodyStream,
        },
        [respBodyStream as unknown as Transferable]
      );
    } catch {
      streamController = null;
      closeBoth(ErrorCode.CANCEL, false);
      return;
    }

    stream.onData((chunk) => {
      if (portClosed || !streamController) return;
      try {
        streamController.enqueue(chunk);
        unackedBytes += chunk.byteLength;
        unackedChunks++;
        if ((streamController.desiredSize ?? 1) > 0) {
          flushCredit();
        }
      } catch {
        streamController = null;
        closeBoth(ErrorCode.CANCEL, false);
      }
    });

    stream.onEnd(() => {
      const ctrl = streamController;
      streamController = null;
      if (ctrl) {
        try {
          ctrl.close();
        } catch {
          // ignore
        }
      }
      closeBoth(undefined, true);
    });

    stream.onReset((err) => {
      const ctrl = streamController;
      streamController = null;
      if (ctrl) {
        try {
          ctrl.error(err);
        } catch {
          // ignore
        }
      }
      closeBoth(undefined, true);
    });
  }

  async handleWsOpen(msg: TunnelWsOpenMsg, wsPort: MessagePort): Promise<void> {
    if (!this.mux) {
      this.rejectWsPort(wsPort, 'P2P tunnel is not connected');
      return;
    }
    if (
      this.activeWSStreams.size >= MAX_CONCURRENT_WS_STREAMS ||
      this.activePorts.size >= MAX_CONCURRENT_REQ_PORTS
    ) {
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
    const internalKey = ++this.wsKeySeq;
    let closed = false;
    let opened = false;
    let wsStream: WSStream | null = null;
    let pendingWsWriteBytes = 0;
    let queuedWsBytes = 0;
    let writeChain: Promise<void> = Promise.resolve();
    let closeAckTimer: ReturnType<typeof setTimeout> | null = null;
    const queuedWsMsgs: WsPortToPortalMsg[] = [];
    const wsId = typeof msg.wsId === 'string' && msg.wsId.length > 0 ? msg.wsId : '';

    const finishWs = (code: number, reason: string, notifyPort: boolean, resetStream: boolean) => {
      if (closed) return;
      closed = true;
      if (closeAckTimer) {
        clearTimeout(closeAckTimer);
        closeAckTimer = null;
      }
      this.activeWSStreams.delete(internalKey);
      if (wsId && this.wsAliasToKey.get(wsId) === internalKey) {
        this.wsAliasToKey.delete(wsId);
      }
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

    const estimateWsMsgBytes = (m: WsPortToPortalMsg): number => {
      if (m.type !== 'WS_SEND') return 0;
      if (m.opcode === 2) {
        return m.data instanceof ArrayBuffer ? m.data.byteLength : 0;
      }
      return typeof m.data === 'string' ? m.data.length * 3 : 0;
    };

    const dispatchWsPortMsg = (m: WsPortToPortalMsg, activeWS: WSStream) => {
      switch (m.type) {
        case 'WS_SEND': {
          const isBinary = m.opcode === 2;
          const payload: Uint8Array | string = isBinary
            ? m.data instanceof ArrayBuffer
              ? new Uint8Array(m.data)
              : new Uint8Array(0)
            : typeof m.data === 'string'
              ? m.data
              : '';
          const ackLen = typeof payload === 'string' ? textEncoder.encode(payload).byteLength : payload.byteLength;
          if (ackLen > MAX_WS_MESSAGE_BYTES) {
            finishWs(1009, 'Outbound WebSocket frame exceeds 16 MiB limit', true, true);
            return;
          }
          if (pendingWsWriteBytes + ackLen > MAX_PENDING_WS_WRITE_BYTES) {
            finishWs(1008, 'Outbound WebSocket send queue limit exceeded', true, true);
            return;
          }
          pendingWsWriteBytes += ackLen;
          writeChain = writeChain.then(async () => {
            try {
              if (closed || activeWS.isClosed) return;
              await activeWS.sendMessage(payload);
              if (!closed) {
                wsPort.postMessage({ type: 'WS_ACK', bytes: ackLen });
              }
            } catch (err) {
              const reason = err instanceof Error ? err.message : 'WS write failed';
              finishWs(1006, reason, true, true);
            } finally {
              pendingWsWriteBytes = Math.max(0, pendingWsWriteBytes - ackLen);
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
              activeWS.close(safeCode, safeReason);
            }
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

    wsPort.onmessage = (ev: MessageEvent) => {
      if (closed) return;
      const m = ev.data as WsPortToPortalMsg;
      if (!isRecord(m) || typeof m.type !== 'string') return;
      if (!wsStream || !opened) {
        if (m.type === 'WS_CLOSE') {
          const safeCode = sanitizeClientWSCloseCode(m.code);
          const safeReason = typeof m.reason === 'string' ? m.reason.slice(0, 256) : '';
          finishWs(safeCode, safeReason, false, Boolean(wsStream));
        } else {
          const est = estimateWsMsgBytes(m);
          if (queuedWsMsgs.length >= 64 || queuedWsBytes + est > MAX_PENDING_WS_WRITE_BYTES) {
            finishWs(1008, 'WebSocket pre-open queue limit exceeded', true, Boolean(wsStream));
            return;
          }
          queuedWsBytes += est;
          queuedWsMsgs.push(m);
        }
        return;
      }
      dispatchWsPortMsg(m, wsStream);
    };
    wsPort.start?.();

    this.activeWSStreams.set(internalKey, () => finishWs(1001, 'client navigated away', false, true));
    if (wsId) {
      this.wsAliasToKey.set(wsId, internalKey);
    }

    try {
      wsStream = await this.mux.openWSStream(reqTLV);
    } catch (err) {
      if (!closed) {
        const reason = err instanceof Error ? err.message : 'Unable to open WS stream';
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
    let rawRespHeaders: Uint8Array;
    try {
      rawRespHeaders = await activeWS.waitResponseHeaders();
    } catch (err) {
      if (!closed) {
        const reason = err instanceof Error ? err.message : 'WebSocket handshake failed';
        this.rejectWsPort(wsPort, reason);
        finishWs(1006, reason, false, false);
      }
      return;
    }

    if (closed) return;

    let resp;
    try {
      resp = decodeResponseHeaders(rawRespHeaders);
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
      // H-18c: Push authoritative WS cookie deltas directly to SW over trusted bridgePort.
      try {
        this.bridgePort?.postMessage({ type: 'BRIDGE_COOKIE_DELTAS', deltas: cookieDeltas });
      } catch {
        // ignore
      }
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
      return;
    }

    activeWS.onMessage((delivered) => {
      if (closed) return;
      try {
        if (delivered.isBinary) {
          const bytes = delivered.data as Uint8Array;
          const copy = new Uint8Array(bytes.byteLength);
          copy.set(bytes);
          wsPort.postMessage({ type: 'WS_MSG', opcode: 2, data: copy.buffer }, [copy.buffer]);
        } else {
          wsPort.postMessage({ type: 'WS_MSG', opcode: 1, data: delivered.data as string });
        }
      } catch {
        finishWs(1006, 'ws port closed', false, true);
      }
    });

    activeWS.onClose((code, reason) => {
      finishWs(code || 1000, reason || '', true, false);
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
    for (const w of this.swPingWaiters.values()) {
      clearTimeout(w.timer);
      w.resolve(false);
    }
    this.swPingWaiters.clear();
    for (const abortFn of Array.from(this.activeWSStreams.values())) {
      abortFn();
    }
    this.activeWSStreams.clear();
    this.wsAliasToKey.clear();
    if (this.bridgePort) {
      this.bridgePort.onmessage = null;
      try {
        this.bridgePort.postMessage({ type: 'BRIDGE_TAB_CLOSE' });
      } catch {
        // ignore
      }
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
