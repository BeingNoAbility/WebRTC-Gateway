/**
 * Service Worker Per-Request Stream Bridge (Native Transferable ReadableStream)
 * - Transfers JS-backed ReadableStream<Uint8Array> directly over MessagePort for BOTH
 *   uploads and downloads — eliminating manual IPC chunk/credit loops while preserving
 *   native pull-based backpressure end-to-end.
 * - Handles Firefox's `request.body === undefined` via `request.arrayBuffer()` fallback.
 * - Invokes `onAck` immediately on `REQ_ACK` so cookie mutations commit as soon as the
 *   tunnel transmits the request headers instead of waiting for slow HTTP responses.
 */

import type { HeaderField } from '../protocol/tlv';
import type { ClientCookieMutation, CookieDelta, RequestPortDownMsg } from '../shared/ipc';
import { isRecord } from '../shared/ipc';
import { BRIDGE_PING_TIMEOUT_MS, type SessionRegistry, type TabSession } from './session_registry';

const IDEMPOTENT_ACK_TIMEOUT_MS = 3_000;
const MUTATING_ACK_TIMEOUT_MS = 15_000;
const RESP_HEAD_TIMEOUT_MS = 120_000;
const IDEMPOTENT_METHODS = new Set(['GET', 'HEAD', 'OPTIONS']);

export interface TunnelResponseHead {
  status: number;
  respFlags: number;
  headers: HeaderField[];
  cookieDeltas: CookieDelta[];
  hasBody: boolean;
  bodyStream: ReadableStream<Uint8Array> | null;
  done: Promise<void>;
  cancel: () => void;
}

export class DeadBridgePortError extends Error {
  constructor() {
    super('bridge port did not acknowledge request within timeout');
  }
}

/**
 * Extracts a guaranteed-transferable ReadableStream<Uint8Array> from a Fetch Request.
 * Works across Chromium, Safari, and Firefox (including Firefox where FetchEvent.request.body
 * can be `undefined` on body-bearing POST/PUT/PATCH requests).
 */
export async function createTransferableRequestBody(
  request: Request
): Promise<{ hasBody: boolean; bodyStream?: ReadableStream<Uint8Array> }> {
  const method = request.method.toUpperCase();
  if (method === 'GET' || method === 'HEAD') {
    return { hasBody: false };
  }

  const rawBody = request.body;
  if (rawBody !== null && rawBody !== undefined) {
    let reader: ReadableStreamDefaultReader<Uint8Array> | null = null;
    const transferable = new ReadableStream<Uint8Array>(
      {
        async pull(controller) {
          if (!reader) {
            reader = rawBody.getReader();
          }
          const { done, value } = await reader.read();
          if (done) {
            try {
              reader.releaseLock();
            } catch {
              // ignore
            }
            reader = null;
            controller.close();
            return;
          }
          if (value && value.byteLength > 0) {
            controller.enqueue(value);
          }
        },
        cancel(reason) {
          const r = reader;
          reader = null;
          if (r) {
            return r.cancel(reason).catch(() => {});
          }
          return rawBody.cancel(reason).catch(() => {});
        },
      },
      new CountQueuingStrategy({ highWaterMark: 4 })
    );
    return { hasBody: true, bodyStream: transferable };
  }

  // Firefox FetchEvent.request.body === undefined fallback: read via arrayBuffer().
  if (rawBody === undefined) {
    const ab = await request.arrayBuffer();
    if (ab.byteLength === 0) {
      return { hasBody: false };
    }
    const bytes = new Uint8Array(ab);
    const bodyStream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(bytes);
        controller.close();
      },
    });
    return { hasBody: true, bodyStream };
  }

  return { hasBody: false };
}

let reqSeq = 0;
function nextReqId(): string {
  reqSeq = (reqSeq + 1) >>> 0;
  return `r_${Date.now().toString(36)}_${reqSeq.toString(36)}`;
}

export async function executeTunnelRequest(
  _swScope: ServiceWorkerGlobalScope,
  registry: SessionRegistry,
  session: TabSession,
  request: Request,
  target: string,
  headers: HeaderField[],
  fetchCtx = 0,
  cookieMutations: ClientCookieMutation[] = [],
  onAck?: () => void
): Promise<{ session: TabSession; response: TunnelResponseHead }> {
  if (request.signal?.aborted) {
    throw new DOMException('Request aborted', 'AbortError');
  }

  if (!session.port) {
    registry.evictDeadPort(session.tabId, null);
    throw new DeadBridgePortError();
  }

  const { hasBody, bodyStream } = await createTransferableRequestBody(request);
  const method = request.method.toUpperCase();
  const isIdempotent = !hasBody && IDEMPOTENT_METHODS.has(method);

  try {
    const resp = await sendSingleAttempt(
      session,
      request,
      target,
      headers,
      hasBody,
      bodyStream,
      isIdempotent,
      fetchCtx,
      cookieMutations,
      onAck
    );
    return { session, response: resp };
  } catch (err) {
    if (err instanceof DeadBridgePortError) {
      const alive = await registry.pingBridgePort(session, BRIDGE_PING_TIMEOUT_MS);
      if (!alive) {
        registry.evictDeadPort(session.tabId, session.port);
      }
    }
    throw err;
  }
}

function sendSingleAttempt(
  session: TabSession,
  request: Request,
  target: string,
  headers: HeaderField[],
  hasBody: boolean,
  bodyStream: ReadableStream<Uint8Array> | undefined,
  allowAckRetry: boolean,
  fetchCtx: number,
  cookieMutations: ClientCookieMutation[],
  onAck?: () => void
): Promise<TunnelResponseHead> {
  return new Promise<TunnelResponseHead>((resolveHead, rejectHead) => {
    const bridgePort = session.port;
    if (!bridgePort) {
      void bodyStream?.cancel().catch(() => {});
      rejectHead(new DeadBridgePortError());
      return;
    }

    const reqId = nextReqId();
    const channel = new MessageChannel();
    const localPort = channel.port1;
    const remotePort = channel.port2;

    let acked = false;
    let headResolved = false;
    let closed = false;
    let ackTimer: ReturnType<typeof setTimeout> | undefined;
    let headTimer: ReturnType<typeof setTimeout> | undefined;

    let resolveDone!: () => void;
    const donePromise = new Promise<void>((r) => {
      resolveDone = r;
    });

    const fireAck = () => {
      if (acked) return;
      acked = true;
      if (ackTimer !== undefined) {
        clearTimeout(ackTimer);
        ackTimer = undefined;
      }
      onAck?.();
    };

    const cleanupPort = () => {
      if (closed) return;
      closed = true;
      if (ackTimer !== undefined) {
        clearTimeout(ackTimer);
        ackTimer = undefined;
      }
      if (headTimer !== undefined) {
        clearTimeout(headTimer);
        headTimer = undefined;
      }
      if (request.signal) {
        request.signal.removeEventListener('abort', onRequestAbort);
      }
      localPort.onmessage = null;
      try {
        localPort.close();
      } catch {
        // ignore
      }
      resolveDone();
    };

    const sendCancelAndCleanup = () => {
      if (!closed) {
        try {
          localPort.postMessage({ type: 'RESP_CANCEL' });
        } catch {
          // ignore
        }
      }
      cleanupPort();
    };

    const failRequest = (err: Error) => {
      sendCancelAndCleanup();
      if (!headResolved) {
        headResolved = true;
        rejectHead(err);
      }
    };

    const onRequestAbort = () => {
      failRequest(new DOMException('Request aborted', 'AbortError'));
    };

    if (request.signal?.aborted) {
      void bodyStream?.cancel().catch(() => {});
      try {
        remotePort.close();
      } catch {
        // ignore
      }
      cleanupPort();
      rejectHead(new DOMException('Request aborted', 'AbortError'));
      return;
    }
    if (request.signal) {
      request.signal.addEventListener('abort', onRequestAbort, { once: true });
    }

    const ackTimeoutMs = allowAckRetry ? IDEMPOTENT_ACK_TIMEOUT_MS : MUTATING_ACK_TIMEOUT_MS;
    ackTimer = setTimeout(() => {
      if (!acked && !closed) {
        if (allowAckRetry) {
          sendCancelAndCleanup();
          if (!headResolved) {
            headResolved = true;
            rejectHead(new DeadBridgePortError());
          }
        } else {
          failRequest(new Error('Portal failed to acknowledge tunnel request'));
        }
      }
    }, ackTimeoutMs);

    headTimer = setTimeout(() => {
      if (!headResolved && !closed) {
        failRequest(new Error('Gateway timeout waiting for response headers'));
      }
    }, RESP_HEAD_TIMEOUT_MS);

    localPort.onmessage = (ev: MessageEvent) => {
      if (closed) return;
      const msg = ev.data as RequestPortDownMsg;
      if (!isRecord(msg) || typeof msg.type !== 'string') return;

      switch (msg.type) {
        case 'REQ_ACK':
          fireAck();
          break;

        case 'RESP_START': {
          if (headResolved) return;
          fireAck();
          headResolved = true;
          if (headTimer !== undefined) {
            clearTimeout(headTimer);
            headTimer = undefined;
          }
          const respStream = msg.hasBody && msg.bodyStream ? msg.bodyStream : null;
          if (!respStream) {
            cleanupPort();
          }
          resolveHead({
            status: msg.status,
            respFlags: typeof msg.respFlags === 'number' ? msg.respFlags : 0,
            headers: msg.headers,
            cookieDeltas: Array.isArray(msg.cookieDeltas) ? msg.cookieDeltas : [],
            hasBody: Boolean(respStream),
            bodyStream: respStream,
            done: donePromise,
            cancel: sendCancelAndCleanup,
          });
          break;
        }

        case 'RESP_DONE':
          cleanupPort();
          break;

        case 'RESP_ERROR': {
          const message = msg.message || `Gateway error (${msg.status || 502})`;
          if (!headResolved) {
            headResolved = true;
            cleanupPort();
            resolveHead({
              status: msg.status || 502,
              respFlags: 0,
              headers: [{ name: 'content-type', value: 'text/plain; charset=utf-8' }],
              cookieDeltas: [],
              hasBody: true,
              bodyStream: new ReadableStream<Uint8Array>({
                start(c) {
                  c.enqueue(new TextEncoder().encode(message));
                  c.close();
                },
              }),
              done: donePromise,
              cancel: () => {},
            });
          } else {
            cleanupPort();
          }
          break;
        }
      }
    };
    localPort.start?.();

    const transferList: Transferable[] = [remotePort];
    if (bodyStream) {
      transferList.push(bodyStream as unknown as Transferable);
    }

    try {
      bridgePort.postMessage(
        {
          type: 'TUNNEL_FETCH_REQUEST',
          reqId,
          tabId: session.tabId,
          method: request.method.toUpperCase(),
          target,
          headers,
          hasBody,
          bodyStream,
          fetchCtx,
          cookieMutations,
        },
        transferList
      );
    } catch {
      void bodyStream?.cancel().catch(() => {});
      try {
        remotePort.close();
      } catch {
        // ignore
      }
      cleanupPort();
      if (!headResolved) {
        headResolved = true;
        rejectHead(allowAckRetry ? new DeadBridgePortError() : new Error('Failed to post to bridge port'));
      }
    }
  });
}
