/**
 * Service Worker Per-Request Stream Bridge
 * - Never retries non-idempotent requests (POST/PUT/PATCH/DELETE) on ACK timeout,
 *   and gives non-idempotent requests a 15s ACK timeout so background-tab timer
 *   throttling never falsely aborts uploads.
 * - Uses `ByteLengthQueuingStrategy({ highWaterMark: 512 * 1024 })` on response `ReadableStream`
 *   for exact byte-accurate backpressure.
 * - Reuses `registry.ensureLivePort(swScope, session.tabId)` so detached ports recover cleanly.
 */

import type { HeaderField } from '../protocol/tlv';
import type { ClientCookieMutation, CookieDelta, RequestPortDownMsg } from '../shared/ipc';
import { isRecord } from '../shared/ipc';
import type { SessionRegistry, TabSession } from './session_registry';

const IDEMPOTENT_ACK_TIMEOUT_MS = 1_500;
const MUTATING_ACK_TIMEOUT_MS = 15_000;
const RESP_HEAD_TIMEOUT_MS = 120_000;
const UPLOAD_CREDIT_INITIAL = 256 * 1024;
const MAX_UPLOAD_SLICE_BYTES = 64 * 1024;
const UPLOAD_CREDIT_TIMEOUT_MS = 30_000;
const DOWNLOAD_CREDIT_BATCH = 128 * 1024;
const DOWNLOAD_STREAM_HWM_BYTES = 512 * 1024;

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

export function toSafeTransferableBuffer(chunk: Uint8Array): {
  buffer: ArrayBuffer;
  offset: number;
  length: number;
} {
  const isShared =
    typeof SharedArrayBuffer !== 'undefined' && chunk.buffer instanceof SharedArrayBuffer;
  if (!isShared && chunk.byteOffset === 0 && chunk.byteLength === chunk.buffer.byteLength) {
    return { buffer: chunk.buffer as ArrayBuffer, offset: 0, length: chunk.byteLength };
  }
  const copy = new Uint8Array(chunk.byteLength);
  copy.set(chunk);
  return { buffer: copy.buffer, offset: 0, length: copy.byteLength };
}

let reqSeq = 0;
function nextReqId(): string {
  reqSeq = (reqSeq + 1) >>> 0;
  return `r_${Date.now().toString(36)}_${reqSeq.toString(36)}`;
}

export async function executeTunnelRequest(
  swScope: ServiceWorkerGlobalScope,
  registry: SessionRegistry,
  session: TabSession,
  request: Request,
  target: string,
  headers: HeaderField[],
  hasBody: boolean,
  fetchCtx = 0,
  cookieMutations: ClientCookieMutation[] = []
): Promise<{ session: TabSession; response: TunnelResponseHead }> {
  const method = request.method.toUpperCase();
  const canRetryOnDeadPort = !hasBody && IDEMPOTENT_METHODS.has(method);

  let activeSession = session;
  if (!activeSession.port) {
    activeSession = await registry.ensureLivePort(swScope, activeSession.tabId);
  }

  try {
    const resp = await sendSingleAttempt(
      activeSession,
      request,
      target,
      headers,
      hasBody,
      canRetryOnDeadPort,
      fetchCtx,
      cookieMutations
    );
    return { session: activeSession, response: resp };
  } catch (err) {
    if (err instanceof DeadBridgePortError && canRetryOnDeadPort) {
      const alive = await registry.pingBridgePort(activeSession, 500);
      if (!alive) {
        registry.evictDeadPort(activeSession.tabId, activeSession.port);
      }
      const freshSession = await registry.ensureLivePort(swScope, activeSession.tabId);
      const resp = await sendSingleAttempt(
        freshSession,
        request,
        target,
        headers,
        hasBody,
        false,
        fetchCtx,
        cookieMutations
      );
      return { session: freshSession, response: resp };
    }
    throw err;
  }
}

class DeadBridgePortError extends Error {
  constructor() {
    super('bridge port did not acknowledge request within timeout');
  }
}

function sendSingleAttempt(
  session: TabSession,
  request: Request,
  target: string,
  headers: HeaderField[],
  hasBody: boolean,
  allowAckRetry: boolean,
  fetchCtx: number,
  cookieMutations: ClientCookieMutation[]
): Promise<TunnelResponseHead> {
  return new Promise<TunnelResponseHead>((resolveHead, rejectHead) => {
    const bridgePort = session.port;
    if (!bridgePort) {
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
    let streamController: ReadableStreamDefaultController<Uint8Array> | null = null;
    let unackedDownBytes = 0;

    let resolveDone!: () => void;
    const donePromise = new Promise<void>((r) => {
      resolveDone = r;
    });

    // Upload backpressure state
    let uploadCredit = UPLOAD_CREDIT_INITIAL;
    let uploadCreditWaiter: {
      resolve: () => void;
      reject: (e: Error) => void;
      timer: ReturnType<typeof setTimeout>;
    } | null = null;
    let uploadStarted = false;
    let uploadAborted = false;
    let uploadReader: ReadableStreamDefaultReader<Uint8Array> | null = null;

    const cleanupPort = () => {
      if (closed) return;
      closed = true;
      uploadAborted = true;
      clearTimeout(ackTimer);
      clearTimeout(headTimer);
      if (uploadCreditWaiter) {
        clearTimeout(uploadCreditWaiter.timer);
        uploadCreditWaiter.reject(new Error('request closed'));
        uploadCreditWaiter = null;
      }
      if (uploadReader) {
        const r = uploadReader;
        uploadReader = null;
        void r.cancel().catch(() => {});
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
      } else if (streamController) {
        try {
          streamController.error(err);
        } catch {
          // ignore
        }
        streamController = null;
      }
    };

    const onRequestAbort = () => {
      failRequest(new DOMException('Request aborted', 'AbortError'));
    };

    if (request.signal?.aborted) {
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
    const ackTimer = setTimeout(() => {
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

    const headTimer = setTimeout(() => {
      if (!headResolved && !closed) {
        failRequest(new Error('Gateway timeout waiting for response headers'));
      }
    }, RESP_HEAD_TIMEOUT_MS);

    const startUploadPump = async () => {
      if (!hasBody || uploadStarted || !request.body) return;
      uploadStarted = true;
      const reader = request.body.getReader();
      uploadReader = reader;
      try {
        while (!uploadAborted && !closed) {
          const { done, value } = await reader.read();
          if (done) {
            if (!closed) {
              localPort.postMessage({ type: 'REQ_BODY_END' });
            }
            return;
          }
          if (!value || value.byteLength === 0) continue;

          let off = 0;
          while (off < value.byteLength && !uploadAborted && !closed) {
            while (uploadCredit <= 0 && !uploadAborted && !closed) {
              await new Promise<void>((resolve, reject) => {
                const timer = setTimeout(() => {
                  uploadCreditWaiter = null;
                  reject(new Error('upload backpressure credit timeout'));
                }, UPLOAD_CREDIT_TIMEOUT_MS);
                uploadCreditWaiter = {
                  resolve: () => {
                    clearTimeout(timer);
                    uploadCreditWaiter = null;
                    resolve();
                  },
                  reject: (e) => {
                    clearTimeout(timer);
                    uploadCreditWaiter = null;
                    reject(e);
                  },
                  timer,
                };
              });
            }
            if (uploadAborted || closed) return;

            const sliceLen = Math.min(value.byteLength - off, MAX_UPLOAD_SLICE_BYTES, uploadCredit);
            const piece = value.subarray(off, off + sliceLen);
            off += sliceLen;
            uploadCredit -= sliceLen;

            const xfer = toSafeTransferableBuffer(piece);
            localPort.postMessage(
              { type: 'REQ_BODY_CHUNK', buffer: xfer.buffer, offset: xfer.offset, length: xfer.length },
              [xfer.buffer]
            );
          }
        }
      } catch (err) {
        if (!uploadAborted && !closed) {
          const error = err instanceof Error ? err : new Error(String(err));
          failRequest(error);
        }
      } finally {
        uploadReader = null;
        try {
          reader.releaseLock();
        } catch {
          // ignore
        }
      }
    };

    localPort.onmessage = (ev: MessageEvent) => {
      if (closed) return;
      const msg = ev.data as RequestPortDownMsg;
      if (!isRecord(msg) || typeof msg.type !== 'string') return;

      switch (msg.type) {
        case 'REQ_ACK':
          if (!acked) {
            acked = true;
            clearTimeout(ackTimer);
            if (hasBody) {
              void startUploadPump();
            }
          }
          break;

        case 'REQ_BODY_CREDIT':
          if (typeof msg.bytes === 'number' && msg.bytes > 0) {
            uploadCredit += msg.bytes;
            if (uploadCredit > 0 && uploadCreditWaiter) {
              uploadCreditWaiter.resolve();
            }
          }
          break;

        case 'RESP_START': {
          if (headResolved) return;
          acked = true;
          headResolved = true;
          clearTimeout(ackTimer);
          clearTimeout(headTimer);

          let bodyStream: ReadableStream<Uint8Array> | null = null;
          if (msg.hasBody) {
            const queuingStrategy =
              typeof ByteLengthQueuingStrategy === 'function'
                ? new ByteLengthQueuingStrategy({ highWaterMark: DOWNLOAD_STREAM_HWM_BYTES })
                : undefined;
            bodyStream = new ReadableStream<Uint8Array>(
              {
                start(controller) {
                  streamController = controller;
                },
                pull() {
                  if (!closed && unackedDownBytes >= DOWNLOAD_CREDIT_BATCH) {
                    const delta = unackedDownBytes;
                    unackedDownBytes = 0;
                    try {
                      localPort.postMessage({ type: 'RESP_BODY_CREDIT', bytes: delta });
                    } catch {
                      // ignore
                    }
                  }
                },
                cancel() {
                  streamController = null;
                  sendCancelAndCleanup();
                },
              },
              queuingStrategy
            );
          } else {
            cleanupPort();
          }

          resolveHead({
            status: msg.status,
            respFlags: typeof msg.respFlags === 'number' ? msg.respFlags : 0,
            headers: msg.headers,
            cookieDeltas: Array.isArray(msg.cookieDeltas) ? msg.cookieDeltas : [],
            hasBody: msg.hasBody,
            bodyStream,
            done: donePromise,
            cancel: sendCancelAndCleanup,
          });
          break;
        }

        case 'RESP_CHUNK': {
          if (!streamController) return;
          const bytes = new Uint8Array(msg.buffer, msg.offset, msg.length);
          unackedDownBytes += bytes.byteLength;
          try {
            streamController.enqueue(bytes);
          } catch {
            sendCancelAndCleanup();
            return;
          }
          if (
            unackedDownBytes >= DOWNLOAD_CREDIT_BATCH &&
            (streamController.desiredSize ?? 1) > 0
          ) {
            const delta = unackedDownBytes;
            unackedDownBytes = 0;
            try {
              localPort.postMessage({ type: 'RESP_BODY_CREDIT', bytes: delta });
            } catch {
              // ignore
            }
          }
          break;
        }

        case 'RESP_END': {
          const ctrl = streamController;
          streamController = null;
          cleanupPort();
          if (ctrl) {
            try {
              ctrl.close();
            } catch {
              // ignore
            }
          }
          break;
        }

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
            failRequest(new Error(message));
          }
          break;
        }
      }
    };
    localPort.start?.();

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
          fetchCtx,
          cookieMutations,
        },
        [remotePort]
      );
    } catch {
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
