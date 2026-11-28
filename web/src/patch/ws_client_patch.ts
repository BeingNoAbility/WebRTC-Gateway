import {
  isCookieDelta,
  isRecord,
  type ClientCookieMutation,
  type CookieDelta,
  type WsPortFromPortalMsg,
} from '../shared/ipc';
import { rewriteWebSocketTarget } from '../shared/url_rewrite';
import { makeRewriteCtx, type PrivateSWHandle, type RuntimePatchConfig } from './types';

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();

function truncateUtf8Bytes(str: string, maxBytes: number): string {
  const bytes = textEncoder.encode(str);
  if (bytes.byteLength <= maxBytes) return str;
  let end = maxBytes;
  while (end > 0 && (bytes[end] & 0xc0) === 0x80) {
    end--;
  }
  return textDecoder.decode(bytes.subarray(0, end));
}

function canonicalizeWebSocketUrl(rawUrl: string): string {
  try {
    const u = new URL(rawUrl, typeof document !== 'undefined' ? document.baseURI : location.href);
    if (u.protocol === 'http:') u.protocol = 'ws:';
    else if (u.protocol === 'https:') u.protocol = 'wss:';
    return u.href;
  } catch {
    return rawUrl;
  }
}

const WS_SUBPROTO_TOKEN_RE = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;

/**
 * Validates WHATWG / RFC 6455 WebSocket subprotocol list: every entry must be a non-empty
 * RFC 9110 HTTP token and duplicate subprotocols are forbidden (throws SyntaxError).
 */
export function validateWebSocketSubprotocols(protocols: string[]): void {
  const seen = new Set<string>();
  for (const p of protocols) {
    if (!p || !WS_SUBPROTO_TOKEN_RE.test(p) || seen.has(p)) {
      if (typeof DOMException === 'function') {
        throw new DOMException(`Invalid or duplicate WebSocket subprotocol '${p}'`, 'SyntaxError');
      }
      throw new SyntaxError(`Invalid or duplicate WebSocket subprotocol '${p}'`);
    }
    seen.add(p);
  }
}

export function installVirtualWebSocket(
  cfg: RuntimePatchConfig,
  swHandle: PrivateSWHandle | null,
  getPendingMutations: () => { list: ClientCookieMutation[]; maxSeq: number },
  onMutationsSent: (maxSeq: number) => void,
  onCookieDeltas: (deltas: CookieDelta[]) => void
): void {
  const NativeWS = window.WebSocket;
  if (!swHandle) return;

  const openSockets = new Set<VirtualWebSocket>();
  window.addEventListener('pagehide', () => {
    for (const sock of Array.from(openSockets)) {
      sock.closeOnUnload();
    }
  });

  type HandlerMap = {
    open: ((this: WebSocket, ev: Event) => unknown) | null;
    message: ((this: WebSocket, ev: MessageEvent) => unknown) | null;
    error: ((this: WebSocket, ev: Event) => unknown) | null;
    close: ((this: WebSocket, ev: CloseEvent) => unknown) | null;
  };

  class VirtualWebSocket extends EventTarget {
    static readonly CONNECTING = 0;
    static readonly OPEN = 1;
    static readonly CLOSING = 2;
    static readonly CLOSED = 3;
    readonly CONNECTING = 0;
    readonly OPEN = 1;
    readonly CLOSING = 2;
    readonly CLOSED = 3;

    // Support `ws instanceof window.WebSocket` for BOTH virtual and pass-through native WebSockets.
    static [Symbol.hasInstance](instance: unknown): boolean {
      if (instance === null || typeof instance !== 'object') return false;
      return (
        Object.prototype.isPrototypeOf.call(VirtualWebSocket.prototype, instance) ||
        instance instanceof NativeWS
      );
    }

    readonly url: string = '';
    readyState: number = VirtualWebSocket.CONNECTING;
    bufferedAmount = 0;
    extensions = '';
    protocol = '';
    binaryType: BinaryType = 'blob';

    private port: MessagePort | null = null;
    private sendQueue: Promise<void> = Promise.resolve();
    private closeQueued = false;
    private closeFallbackTimer: ReturnType<typeof setTimeout> | null = null;

    private readonly idlHandlers: HandlerMap = { open: null, message: null, error: null, close: null };
    private readonly idlWrappers: Record<keyof HandlerMap, EventListener | null> = {
      open: null,
      message: null,
      error: null,
      close: null,
    };

    private setIdlHandler<K extends keyof HandlerMap>(type: K, fn: HandlerMap[K]): void {
      const prev = this.idlWrappers[type];
      if (prev) {
        this.removeEventListener(type, prev);
        this.idlWrappers[type] = null;
      }
      this.idlHandlers[type] = typeof fn === 'function' ? fn : null;
      if (this.idlHandlers[type]) {
        const bound: EventListener = (ev: Event) => {
          (this.idlHandlers[type] as ((this: WebSocket, e: Event) => unknown)).call(
            this as unknown as WebSocket,
            ev
          );
        };
        this.idlWrappers[type] = bound;
        this.addEventListener(type, bound);
      }
    }

    get onopen(): HandlerMap['open'] {
      return this.idlHandlers.open;
    }
    set onopen(fn: HandlerMap['open']) {
      this.setIdlHandler('open', fn);
    }
    get onmessage(): HandlerMap['message'] {
      return this.idlHandlers.message;
    }
    set onmessage(fn: HandlerMap['message']) {
      this.setIdlHandler('message', fn);
    }
    get onerror(): HandlerMap['error'] {
      return this.idlHandlers.error;
    }
    set onerror(fn: HandlerMap['error']) {
      this.setIdlHandler('error', fn);
    }
    get onclose(): HandlerMap['close'] {
      return this.idlHandlers.close;
    }
    set onclose(fn: HandlerMap['close']) {
      this.setIdlHandler('close', fn);
    }

    constructor(rawUrl: string | URL, protocols?: string | string[]) {
      super();
      const urlStr = String(rawUrl);
      const target = rewriteWebSocketTarget(urlStr, makeRewriteCtx(cfg));
      if (!target) {
        return new NativeWS(rawUrl, protocols) as unknown as VirtualWebSocket;
      }

      this.url = canonicalizeWebSocketUrl(urlStr);
      const protoList = Array.isArray(protocols)
        ? protocols.map(String)
        : protocols !== undefined
          ? [String(protocols)]
          : [];
      validateWebSocketSubprotocols(protoList);

      const ch = new MessageChannel();
      this.port = ch.port1;
      this.port.onmessage = (ev: MessageEvent) => this.handlePortMessage(ev.data);
      this.port.start?.();
      openSockets.add(this);

      const pendingMuts = getPendingMutations();
      const openPayload = {
        type: 'SW_WS_OPEN',
        target,
        protocols: protoList,
        mutations: pendingMuts.list,
        seq: pendingMuts.maxSeq,
      };

      const dispatchToSW = (sw: ServiceWorker | null) => {
        if (!sw) {
          ch.port2.close();
          queueMicrotask(() => this.dispatchErrorAndClose('Service Worker controller unavailable'));
          return;
        }
        try {
          sw.postMessage(openPayload, [ch.port2]);
          if (pendingMuts.maxSeq > 0) {
            onMutationsSent(pendingMuts.maxSeq);
          }
        } catch {
          ch.port2.close();
          queueMicrotask(() => this.dispatchErrorAndClose('Failed to post SW_WS_OPEN'));
        }
      };

      const ctrl = swHandle!.getController();
      if (ctrl) {
        dispatchToSW(ctrl);
      } else {
        swHandle!
          .getReady()
          .then((reg) => dispatchToSW(reg.active))
          .catch(() => dispatchToSW(null));
      }
    }

    send(data: string | ArrayBufferLike | Blob | ArrayBufferView): void {
      if (this.readyState === VirtualWebSocket.CONNECTING) {
        throw new DOMException(
          "Failed to execute 'send' on 'WebSocket': Still in CONNECTING state.",
          'InvalidStateError'
        );
      }
      if (this.closeQueued || this.readyState !== VirtualWebSocket.OPEN || !this.port) return;

      let snapshotKind: 'text' | 'blob' | 'ab';
      let textVal = '';
      let blobVal: Blob | null = null;
      let abVal: ArrayBuffer | null = null;
      let byteLen = 0;

      if (typeof data === 'string') {
        snapshotKind = 'text';
        textVal = data;
        byteLen = textEncoder.encode(data).byteLength;
      } else if (data instanceof Blob) {
        snapshotKind = 'blob';
        blobVal = data;
        byteLen = data.size;
      } else if (ArrayBuffer.isView(data)) {
        snapshotKind = 'ab';
        const copy = new Uint8Array(data.byteLength);
        copy.set(new Uint8Array(data.buffer, data.byteOffset, data.byteLength));
        abVal = copy.buffer;
        byteLen = copy.byteLength;
      } else if (data instanceof ArrayBuffer) {
        snapshotKind = 'ab';
        abVal = data.slice(0);
        byteLen = abVal.byteLength;
      } else if (typeof SharedArrayBuffer !== 'undefined' && data instanceof SharedArrayBuffer) {
        snapshotKind = 'ab';
        const copy = new Uint8Array(data.byteLength);
        copy.set(new Uint8Array(data));
        abVal = copy.buffer;
        byteLen = copy.byteLength;
      } else {
        snapshotKind = 'text';
        textVal = String(data);
        byteLen = textEncoder.encode(textVal).byteLength;
      }

      this.bufferedAmount += byteLen;

      this.sendQueue = this.sendQueue
        .then(async () => {
          if (this.readyState === VirtualWebSocket.CLOSED || !this.port) {
            this.bufferedAmount = Math.max(0, this.bufferedAmount - byteLen);
            return;
          }
          if (snapshotKind === 'text') {
            this.port.postMessage({ type: 'WS_SEND', opcode: 1, data: textVal });
          } else if (snapshotKind === 'blob' && blobVal) {
            const buf = await blobVal.arrayBuffer();
            if (this.readyState === VirtualWebSocket.CLOSED || !this.port) {
              this.bufferedAmount = Math.max(0, this.bufferedAmount - byteLen);
              return;
            }
            this.port.postMessage({ type: 'WS_SEND', opcode: 2, data: buf }, [buf]);
          } else if (abVal) {
            this.port.postMessage({ type: 'WS_SEND', opcode: 2, data: abVal }, [abVal]);
          }
        })
        .catch(() => {
          this.bufferedAmount = Math.max(0, this.bufferedAmount - byteLen);
        });
    }

    close(code?: number, reason = ''): void {
      if (code !== undefined) {
        if (!Number.isInteger(code) || (code !== 1000 && (code < 3000 || code > 4999))) {
          throw new DOMException(
            `Failed to execute 'close' on 'WebSocket': The code must be either 1000, or between 3000 and 4999. ${code} is neither.`,
            'InvalidAccessError'
          );
        }
      }
      const safeCode = code ?? 1000;
      const reasonStr = String(reason);
      if (textEncoder.encode(reasonStr).byteLength > 123) {
        throw new DOMException(
          "Failed to execute 'close' on 'WebSocket': The message must not be greater than 123 bytes.",
          'SyntaxError'
        );
      }

      if (this.closeQueued || this.readyState === VirtualWebSocket.CLOSING || this.readyState === VirtualWebSocket.CLOSED) {
        return;
      }
      const wasConnecting = this.readyState === VirtualWebSocket.CONNECTING;
      this.closeQueued = true;
      this.readyState = VirtualWebSocket.CLOSING;
      const safeReason = truncateUtf8Bytes(reasonStr, 123);

      // Per WHATWG WebSocket spec, closing while CONNECTING immediately aborts the handshake
      // without waiting 2s for a peer close echo!
      if (wasConnecting) {
        try {
          this.port?.postMessage({ type: 'WS_CLOSE', code: safeCode, reason: safeReason });
        } catch {
          // ignore
        }
        queueMicrotask(() => this.finalizeClose(1006, 'WebSocket is closed before the connection is established.', false));
        return;
      }

      this.sendQueue = this.sendQueue
        .then(() => {
          if (this.readyState === VirtualWebSocket.CLOSED) return;
          try {
            this.port?.postMessage({ type: 'WS_CLOSE', code: safeCode, reason: safeReason });
          } catch {
            this.finalizeClose(safeCode, safeReason, false);
            return;
          }
          if (!this.closeFallbackTimer) {
            this.closeFallbackTimer = setTimeout(() => {
              this.finalizeClose(safeCode, safeReason, false);
            }, 2_000);
          }
        })
        .catch(() => {
          this.finalizeClose(safeCode, safeReason, false);
        });
    }

    closeOnUnload(): void {
      if (this.readyState === VirtualWebSocket.CLOSED) return;
      this.closeQueued = true;
      try {
        this.port?.postMessage({ type: 'WS_CLOSE', code: 1001, reason: 'page unloading' });
      } catch {
        // ignore
      }
      this.finalizeClose(1001, 'page unloading', false);
    }

    private dispatchErrorAndClose(msg: string): void {
      this.dispatchEvent(new Event('error'));
      this.finalizeClose(1006, msg, false);
    }

    private handlePortMessage(raw: unknown): void {
      if (!isRecord(raw) || typeof raw.type !== 'string') return;
      const msg = raw as WsPortFromPortalMsg;

      switch (msg.type) {
        case 'WS_OPEN': {
          if (this.readyState !== VirtualWebSocket.CONNECTING) return;
          this.readyState = VirtualWebSocket.OPEN;
          this.protocol = msg.protocol || '';
          if (Array.isArray(msg.cookieDeltas) && msg.cookieDeltas.length > 0) {
            onCookieDeltas(msg.cookieDeltas.filter(isCookieDelta));
          }
          this.dispatchEvent(new Event('open'));
          break;
        }
        case 'WS_ACK': {
          this.bufferedAmount = Math.max(0, this.bufferedAmount - msg.bytes);
          break;
        }
        case 'WS_MSG': {
          if (this.readyState !== VirtualWebSocket.OPEN && this.readyState !== VirtualWebSocket.CLOSING) return;
          let payload: string | ArrayBuffer | Blob;
          if (msg.opcode === 1) {
            payload = typeof msg.data === 'string' ? msg.data : textDecoder.decode(msg.data);
          } else {
            const ab = msg.data instanceof ArrayBuffer ? msg.data : new ArrayBuffer(0);
            payload = this.binaryType === 'arraybuffer' ? ab : new Blob([ab]);
          }
          this.dispatchEvent(new MessageEvent('message', { data: payload, origin: location.origin }));
          break;
        }
        case 'WS_ERROR': {
          this.dispatchErrorAndClose(msg.message || 'WebSocket error');
          break;
        }
        case 'WS_CLOSE': {
          const code = typeof msg.code === 'number' ? msg.code : 1000;
          const reason = typeof msg.reason === 'string' ? msg.reason : '';
          this.finalizeClose(code, reason, code !== 1006);
          break;
        }
      }
    }

    private finalizeClose(code: number, reason: string, wasClean: boolean): void {
      if (this.readyState === VirtualWebSocket.CLOSED) return;
      this.readyState = VirtualWebSocket.CLOSED;
      if (this.closeFallbackTimer) {
        clearTimeout(this.closeFallbackTimer);
        this.closeFallbackTimer = null;
      }
      openSockets.delete(this);
      if (this.port) {
        this.port.onmessage = null;
        try {
          this.port.close();
        } catch {
          // ignore
        }
        this.port = null;
      }
      this.dispatchEvent(new CloseEvent('close', { code, reason, wasClean }));
    }
  }

  (window as unknown as { WebSocket: typeof WebSocket }).WebSocket =
    VirtualWebSocket as unknown as typeof WebSocket;
}
