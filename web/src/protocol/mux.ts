/**
 * P2PMux V8 TypeScript Multiplexer
 *
 * Unified single-framing multiplexer over:
 *   - 1 chunked control DataChannel (`p2pctrl`, id=0, streamId === 0)
 *   - N pre-negotiated ordered reliable data lanes (`p2p_01`..`p2p_12`) multiplexing both
 *     HTTP (PoolStream) and WebSocket (WSStream) streams by strictly monotonic streamId >= 1.
 *
 * Eliminates:
 *   - C-4: slot reuse Gen collisions (monotonic streamId is never reused)
 *   - C-5: 64B overhead / 8192-chunk wire credit deadlock (pure payload-byte CREDIT + chunk cadence)
 *   - H-5: WSStream preservePendingMessages drop & sessionInboundBytes leak
 *   - M-10: spurious onEnd after onReset & waiter timeout resets
 */

import {
  DEFAULT_CHUNK_SIZE,
  DEFAULT_POOL_SIZE,
  ErrorCode,
  FrameFlag,
  FrameType,
  HEADER_SIZE,
  MAX_CONTROL_CHUNKS,
  MAX_CONTROL_JSON_SIZE,
  MAX_HEADER_CHUNKS,
  MAX_POOL_SIZE,
  MAX_TOTAL_HEADERS_SIZE,
  MAX_WS_MESSAGE_BYTES,
  MIN_CHUNK_SIZE,
  MIN_NEGOTIATED_MAX_MSG_SIZE,
  decodeFrame,
  decodeGoAwayPayload,
  decodeResetPayload,
  decodeWSClosePayload,
  encodeCreditPayload,
  encodeFrame,
  encodeGoAwayPayload,
  encodeResetPayload,
  encodeWSClosePayload,
  isControlFrameType,
  isControlJsonFrameType,
  isStreamFrameType,
  type Frame,
} from './frame';

export const DEFAULT_STREAM_WINDOW = 512 * 1024;
export const MAX_STREAM_INBOUND_BYTES = 2 * 1024 * 1024;
export const CREDIT_THRESHOLD_BYTES = 128 * 1024;
export const CREDIT_THRESHOLD_CHUNKS = 256;
export const DEFAULT_SESSION_MEMORY_BUDGET = 32 * 1024 * 1024;
export const SCTP_HIGH_WATER = 512 * 1024;
export const SCTP_LOW_WATER = 128 * 1024;
export const WINDOW_WAIT_TIMEOUT_MS = 30_000;
export const MAX_CONCURRENT_STREAMS = 256;
const INBOUND_CHUNK_OVERHEAD = 64;

const utf8Enc = new TextEncoder();
const utf8Dec = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true });

export interface DataChannelLike {
  readyState: string;
  bufferedAmount: number;
  bufferedAmountLowThreshold: number;
  binaryType?: string;
  send(data: ArrayBuffer | ArrayBufferView | Uint8Array): void;
  close(): void;
  onmessage: ((ev: { data: unknown }) => void) | null;
  onopen: ((ev?: unknown) => void) | null;
  onclose: ((ev?: unknown) => void) | null;
  onerror: ((ev?: unknown) => void) | null;
  onbufferedamountlow: ((ev?: unknown) => void) | null;
}

export interface PoolMuxCallbacks {
  onControlFrame?: (frame: Frame) => void;
  onGoAway?: (code: number) => void;
  onProtocolError?: (err: Error) => void;
}

interface DataLane {
  id: number;
  dc: DataChannelLike | null;
  streams: Map<number, ManagedStream>;
  lowWaiters: Array<() => void>;
}

interface ManagedStream {
  readonly streamId: number;
  onInboundHeaders(flags: number, payload: Uint8Array): void;
  onInboundData(flags: number, payload: Uint8Array): void;
  onInboundWSChunk(op: number, fin: boolean, payload: Uint8Array): void;
  onInboundWSClose(payload: Uint8Array): void;
  onInboundCredit(delta: number): void;
  onPeerReset(code: number): void;
  abortLocal(reason: Error): void;
}

function toUint8Array(data: unknown): Uint8Array | null {
  if (data instanceof Uint8Array) return data;
  if (data instanceof ArrayBuffer) return new Uint8Array(data);
  if (ArrayBuffer.isView(data)) {
    return new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
  }
  return null;
}

function concatChunks(chunks: Uint8Array[], totalLen: number): Uint8Array {
  if (chunks.length === 1) return chunks[0];
  const out = new Uint8Array(totalLen);
  let off = 0;
  for (const c of chunks) {
    out.set(c, off);
    off += c.byteLength;
  }
  return out;
}

/**
 * Common flow-controlled stream base shared by HTTP (PoolStream) and WebSocket (WSStream).
 */
abstract class BaseStream implements ManagedStream {
  readonly streamId: number;
  protected readonly mux: PoolMux;
  protected readonly lane: DataLane;

  protected hdrChunks: Uint8Array[] = [];
  protected hdrBytes = 0;
  protected hdrComplete = false;
  protected hdrResolve: ((raw: Uint8Array) => void) | null = null;
  protected hdrReject: ((err: Error) => void) | null = null;
  protected hdrResult: Uint8Array | null = null;
  protected hdrError: Error | null = null;

  protected sendWindow = DEFAULT_STREAM_WINDOW;
  protected winWaiters: Array<{ resolve: () => void; reject: (err: Error) => void }> = [];

  protected localFin = false;
  protected remoteFin = false;
  protected headerHadFin = false;
  protected terminated = false;
  protected abortReason: Error | null = null;
  protected detached = false;

  constructor(mux: PoolMux, lane: DataLane, streamId: number) {
    this.mux = mux;
    this.lane = lane;
    this.streamId = streamId;
  }

  get isClosed(): boolean {
    return this.terminated || this.detached;
  }

  get hasResponseBody(): boolean {
    return !this.headerHadFin;
  }

  waitResponseHeaders(): Promise<Uint8Array> {
    if (this.hdrResult) return Promise.resolve(this.hdrResult);
    if (this.hdrError) return Promise.reject(this.hdrError);
    return new Promise<Uint8Array>((resolve, reject) => {
      this.hdrResolve = resolve;
      this.hdrReject = reject;
    });
  }

  onInboundHeaders(flags: number, payload: Uint8Array): void {
    if (this.terminated || this.hdrComplete) {
      this.reset(ErrorCode.PROTOCOL_ERROR);
      return;
    }
    this.hdrBytes += payload.byteLength;
    if (this.hdrChunks.length + 1 > MAX_HEADER_CHUNKS || this.hdrBytes > MAX_TOTAL_HEADERS_SIZE) {
      this.reset(ErrorCode.FRAME_SIZE_ERROR);
      return;
    }
    this.hdrChunks.push(new Uint8Array(payload));
    if ((flags & FrameFlag.MORE) !== 0) {
      return;
    }
    this.hdrComplete = true;
    const assembled = concatChunks(this.hdrChunks, this.hdrBytes);
    this.hdrChunks = [];
    this.hdrResult = assembled;
    if ((flags & FrameFlag.FIN) !== 0) {
      this.headerHadFin = true;
      this.onHeaderFin();
    }
    const res = this.hdrResolve;
    this.hdrResolve = null;
    this.hdrReject = null;
    if (res) res(assembled);
  }

  protected abstract onHeaderFin(): void;
  abstract onInboundData(flags: number, payload: Uint8Array): void;
  abstract onInboundWSChunk(op: number, fin: boolean, payload: Uint8Array): void;
  abstract onInboundWSClose(payload: Uint8Array): void;

  onInboundCredit(delta: number): void {
    if (this.terminated) return;
    if (this.sendWindow + delta > 0x7fffffff) {
      this.reset(ErrorCode.FLOW_CONTROL_ERROR);
      return;
    }
    this.sendWindow += delta;
    this.wakeWindowWaiters();
  }

  protected wakeWindowWaiters(): void {
    const list = this.winWaiters;
    this.winWaiters = [];
    for (const w of list) w.resolve();
  }

  protected failWindowWaiters(err: Error): void {
    const list = this.winWaiters;
    this.winWaiters = [];
    for (const w of list) w.reject(err);
  }

  protected async acquireSendWindow(want: number): Promise<number> {
    if (want <= 0) return 0;
    const deadline = Date.now() + WINDOW_WAIT_TIMEOUT_MS;
    while (!this.terminated && !this.localFin) {
      if (this.sendWindow > 0) {
        const grant = Math.min(want, this.sendWindow);
        this.sendWindow -= grant;
        return grant;
      }
      const rem = deadline - Date.now();
      if (rem <= 0) {
        this.reset(ErrorCode.FLOW_CONTROL_ERROR);
        throw new Error('P2P stream send window timeout');
      }
      await new Promise<void>((resolve, reject) => {
        const entry = {
          resolve: () => {
            clearTimeout(timer);
            resolve();
          },
          reject: (e: Error) => {
            clearTimeout(timer);
            reject(e);
          },
        };
        const timer = setTimeout(() => {
          const idx = this.winWaiters.indexOf(entry);
          if (idx >= 0) this.winWaiters.splice(idx, 1);
          reject(new Error('P2P stream send window timeout'));
        }, rem);
        this.winWaiters.push(entry);
      });
    }
    throw new Error('P2P stream closed');
  }

  protected async sendRawHeaders(raw: Uint8Array, fin: boolean): Promise<void> {
    const cs = this.mux.chunkSize;
    for (let off = 0; off < raw.byteLength; off += cs) {
      // Wait for SCTP writability only on subsequent chunks (caller already ensured writability before
      // allocating streamId so the opening HEADERS frame always hits dc.send() in strict streamId order!).
      if (off > 0) {
        await this.mux.waitLaneWritable(this.lane);
        if (this.terminated) throw new Error('P2P stream aborted');
      }
      const end = Math.min(off + cs, raw.byteLength);
      const isLast = end >= raw.byteLength;
      let flags: number = isLast ? FrameFlag.NONE : FrameFlag.MORE;
      if (isLast && fin) flags |= FrameFlag.FIN;
      this.mux.sendLaneFrame(this.lane, this.streamId, FrameType.HEADERS, flags, raw.subarray(off, end));
    }
    if (fin) {
      this.localFin = true;
      this.maybeDetach();
    }
  }

  protected sendCredit(delta: number): void {
    if (delta <= 0 || this.terminated || this.remoteFin) return;
    try {
      this.mux.sendLaneFrame(this.lane, this.streamId, FrameType.CREDIT, FrameFlag.NONE, encodeCreditPayload(delta));
    } catch {
      // ignore if lane already closed
    }
  }

  protected maybeDetach(): void {
    if (this.localFin && this.remoteFin) {
      this.detach();
    }
  }

  protected detach(): void {
    if (this.detached) return;
    this.detached = true;
    this.mux.detachStream(this.lane, this.streamId, this);
  }

  reset(code: number = ErrorCode.CANCEL): void {
    if (this.terminated) return;
    const err = new Error(`P2P stream reset (${code})`);
    try {
      this.mux.sendLaneFrame(this.lane, this.streamId, FrameType.RESET, FrameFlag.NONE, encodeResetPayload(code));
    } catch {
      // ignore
    }
    this.abortLocal(err);
  }

  onPeerReset(code: number): void {
    this.abortLocal(new Error(`P2P stream reset (${code})`));
  }

  abortLocal(reason: Error): void {
    if (this.terminated) return;
    this.terminated = true;
    this.abortReason = reason;
    this.localFin = true;
    this.remoteFin = true;
    if (!this.hdrComplete) {
      this.hdrError = reason;
      const rej = this.hdrReject;
      this.hdrResolve = null;
      this.hdrReject = null;
      if (rej) rej(reason);
    }
    this.failWindowWaiters(reason);
    this.cleanupInbound();
    this.detach();
    this.notifyAbort(reason);
  }

  protected abstract cleanupInbound(): void;
  protected abstract notifyAbort(reason: Error): void;
}

/**
 * HTTP full-duplex stream over a V8 multiplexed lane with pure payload-byte flow control.
 */
export class PoolStream extends BaseStream {
  private dataCb: ((chunk: Uint8Array) => void) | null = null;
  private endCb: (() => void) | null = null;
  private resetCb: ((err: Error) => void) | null = null;

  private pendingChunks: Array<{ data: Uint8Array; memCost: number }> = [];
  private pendingEnd = false;
  private recvInFlightPayload = 0;
  private deliveredUnackedBytes = 0;
  private deliveredUnackedChunks = 0;
  private pendingCreditBytes = 0;
  private pendingCreditChunks = 0;

  async openWithHeaders(headersRaw: Uint8Array, hasBody: boolean): Promise<void> {
    await this.sendRawHeaders(headersRaw, !hasBody);
  }

  onData(cb: (chunk: Uint8Array) => void): void {
    this.dataCb = cb;
    this.drainPending();
  }

  onEnd(cb: () => void): void {
    this.endCb = cb;
    if (this.pendingEnd && this.pendingChunks.length === 0 && !this.terminated) {
      this.pendingEnd = false;
      cb();
    }
  }

  onReset(cb: (err: Error) => void): void {
    this.resetCb = cb;
    if (this.terminated) {
      cb(this.abortReason ?? new Error('P2P stream reset'));
    }
  }

  /**
   * Consumer acknowledges payload bytes read (pure payload bytes ONLY — fixes C-5!).
   * Replenishes wire CREDIT every 128 KiB OR 256 chunks so tiny 13B SSE tokens never stall or reset.
   */
  ackConsumed(payloadBytes: number, chunkCount = 1): void {
    if (this.terminated || this.remoteFin || payloadBytes <= 0) return;
    const ackBytes = Math.min(payloadBytes, this.deliveredUnackedBytes);
    if (ackBytes <= 0) return;
    this.deliveredUnackedBytes -= ackBytes;
    this.deliveredUnackedChunks = Math.max(0, this.deliveredUnackedChunks - chunkCount);
    this.pendingCreditBytes += ackBytes;
    this.pendingCreditChunks += chunkCount;
    if (
      this.pendingCreditBytes >= CREDIT_THRESHOLD_BYTES ||
      this.pendingCreditChunks >= CREDIT_THRESHOLD_CHUNKS
    ) {
      const delta = this.pendingCreditBytes;
      this.pendingCreditBytes = 0;
      this.pendingCreditChunks = 0;
      this.sendCredit(delta);
    }
  }

  async writeBody(data: Uint8Array): Promise<void> {
    if (data.byteLength === 0) return;
    let off = 0;
    while (off < data.byteLength) {
      if (this.terminated || this.localFin) throw new Error('P2P stream closed');
      await this.mux.waitLaneWritable(this.lane);
      const want = Math.min(this.mux.chunkSize, data.byteLength - off);
      const grant = await this.acquireSendWindow(want);
      if (this.terminated || this.localFin) throw new Error('P2P stream closed');
      this.mux.sendLaneFrame(
        this.lane,
        this.streamId,
        FrameType.DATA,
        FrameFlag.NONE,
        data.subarray(off, off + grant)
      );
      off += grant;
    }
  }

  async finishWrite(): Promise<void> {
    if (this.terminated || this.localFin) return;
    await this.mux.waitLaneWritable(this.lane);
    if (this.terminated || this.localFin) return;
    this.localFin = true;
    this.mux.sendLaneFrame(this.lane, this.streamId, FrameType.DATA, FrameFlag.FIN, new Uint8Array(0));
    this.maybeDetach();
  }

  protected onHeaderFin(): void {
    this.remoteFin = true;
    this.maybeDetach();
    if (this.endCb && !this.terminated) {
      this.endCb();
    } else {
      this.pendingEnd = true;
    }
  }

  onInboundData(flags: number, payload: Uint8Array): void {
    if (this.terminated || this.remoteFin || !this.hdrComplete) {
      if (!this.hdrComplete && !this.terminated) this.reset(ErrorCode.PROTOCOL_ERROR);
      return;
    }
    const fin = (flags & FrameFlag.FIN) !== 0;
    const plen = payload.byteLength;
    if (plen > 0) {
      if (this.recvInFlightPayload + plen > MAX_STREAM_INBOUND_BYTES) {
        this.reset(ErrorCode.FLOW_CONTROL_ERROR);
        return;
      }
      const memCost = plen + INBOUND_CHUNK_OVERHEAD;
      if (!this.mux.reserveSessionBytes(memCost)) {
        this.reset(ErrorCode.FLOW_CONTROL_ERROR);
        return;
      }
      const copy = new Uint8Array(plen);
      copy.set(payload);
      if (this.dataCb) {
        this.mux.releaseSessionBytes(memCost);
        this.deliveredUnackedBytes += plen;
        this.deliveredUnackedChunks++;
        this.dataCb(copy);
      } else {
        this.recvInFlightPayload += plen;
        this.pendingChunks.push({ data: copy, memCost });
      }
    }
    if (fin) {
      this.remoteFin = true;
      this.maybeDetach();
      if (this.dataCb && this.pendingChunks.length === 0) {
        if (this.endCb && !this.terminated) {
          this.endCb();
        } else {
          this.pendingEnd = true;
        }
      } else {
        this.pendingEnd = true;
      }
    }
  }

  onInboundWSChunk(): void {
    this.reset(ErrorCode.PROTOCOL_ERROR);
  }

  onInboundWSClose(): void {
    this.reset(ErrorCode.PROTOCOL_ERROR);
  }

  private drainPending(): void {
    if (!this.dataCb || this.terminated) return;
    while (this.pendingChunks.length > 0 && !this.terminated) {
      const item = this.pendingChunks.shift()!;
      this.recvInFlightPayload = Math.max(0, this.recvInFlightPayload - item.data.byteLength);
      this.mux.releaseSessionBytes(item.memCost);
      this.deliveredUnackedBytes += item.data.byteLength;
      this.deliveredUnackedChunks++;
      this.dataCb(item.data);
    }
    if (this.pendingEnd && this.pendingChunks.length === 0 && !this.terminated && this.endCb) {
      this.pendingEnd = false;
      this.endCb();
    }
  }

  protected cleanupInbound(): void {
    let freed = 0;
    for (const item of this.pendingChunks) {
      freed += item.memCost;
    }
    this.pendingChunks = [];
    this.recvInFlightPayload = 0;
    this.deliveredUnackedBytes = 0;
    this.deliveredUnackedChunks = 0;
    this.pendingCreditBytes = 0;
    this.pendingCreditChunks = 0;
    if (freed > 0) this.mux.releaseSessionBytes(freed);
  }

  protected notifyAbort(reason: Error): void {
    // M-10: Never fire onEnd after onReset.
    this.pendingEnd = false;
    this.resetCb?.(reason);
  }
}

export type WSDeliveredMessage = { isBinary: boolean; data: string | Uint8Array };

/**
 * Streaming WebSocket over a V8 multiplexed lane (zero dynamic DCEP channel, fixes H-5!).
 */
export class WSStream extends BaseStream {
  private msgCb: ((msg: WSDeliveredMessage) => void) | null = null;
  private closeCb: ((code: number, reason: string, wasClean: boolean) => void) | null = null;

  private asmOp = 0;
  private asmChunks: Uint8Array[] = [];
  private asmBytes = 0;
  private asmMemCost = 0;

  private pendingMsgs: Array<{ msg: WSDeliveredMessage; memCost: number }> = [];
  private pendingClose: { code: number; reason: string; wasClean: boolean } | null = null;
  private remoteCloseReceived = false;
  private consumedBytes = 0;
  private consumedChunks = 0;

  async openWithHeaders(headersRaw: Uint8Array): Promise<void> {
    await this.sendRawHeaders(headersRaw, false);
  }

  onMessage(cb: (msg: WSDeliveredMessage) => void): void {
    this.msgCb = cb;
    // H-5a: Drain ALL queued messages even if remoteCloseReceived === true (only stop if hard-destroyed!).
    while (this.pendingMsgs.length > 0 && !this.terminated) {
      const item = this.pendingMsgs.shift()!;
      this.mux.releaseSessionBytes(item.memCost);
      cb(item.msg);
    }
    if (this.pendingClose && this.pendingMsgs.length === 0 && !this.terminated) {
      const cl = this.pendingClose;
      this.pendingClose = null;
      this.finishAfterCloseDelivery(cl.code, cl.reason, cl.wasClean);
    }
  }

  onClose(cb: (code: number, reason: string, wasClean: boolean) => void): void {
    this.closeCb = cb;
    if (this.terminated) {
      cb(1006, this.abortReason?.message || 'P2P stream reset', false);
      return;
    }
    if (this.pendingClose && this.pendingMsgs.length === 0) {
      const cl = this.pendingClose;
      this.pendingClose = null;
      this.finishAfterCloseDelivery(cl.code, cl.reason, cl.wasClean);
    }
  }

  async sendMessage(data: string | Uint8Array): Promise<void> {
    if (this.terminated || this.localFin) throw new Error('WebSocket stream closed');
    const isBinary = data instanceof Uint8Array;
    const raw = isBinary ? data : utf8Enc.encode(data);
    if (raw.byteLength > MAX_WS_MESSAGE_BYTES) {
      throw new RangeError('WebSocket message exceeds 16 MiB');
    }
    const op = isBinary ? FrameType.WS_BINARY : FrameType.WS_TEXT;
    if (raw.byteLength === 0) {
      await this.mux.waitLaneWritable(this.lane);
      if (this.terminated || this.localFin) throw new Error('WebSocket stream closed');
      this.mux.sendLaneFrame(this.lane, this.streamId, op, FrameFlag.MSG_FIN, raw);
      return;
    }

    let off = 0;
    while (off < raw.byteLength) {
      if (this.terminated || this.localFin) throw new Error('WebSocket stream closed');
      await this.mux.waitLaneWritable(this.lane);
      const want = Math.min(this.mux.chunkSize, raw.byteLength - off);
      const grant = await this.acquireSendWindow(want);
      if (this.terminated || this.localFin) throw new Error('WebSocket stream closed');
      const isLast = off + grant >= raw.byteLength;
      this.mux.sendLaneFrame(
        this.lane,
        this.streamId,
        op,
        isLast ? FrameFlag.MSG_FIN : FrameFlag.NONE,
        raw.subarray(off, off + grant)
      );
      off += grant;
    }
  }

  close(code = 1000, reason = ''): void {
    if (this.terminated || this.localFin) return;
    this.localFin = true;
    try {
      this.mux.sendLaneFrame(
        this.lane,
        this.streamId,
        FrameType.WS_CLOSE,
        FrameFlag.NONE,
        encodeWSClosePayload(code, reason)
      );
    } catch {
      // ignore
    }
    this.maybeDetach();
  }

  protected onHeaderFin(): void {
    // Non-101 WS error response with FlagFin.
    this.remoteFin = true;
    this.remoteCloseReceived = true;
    this.maybeDetach();
  }

  onInboundData(): void {
    this.reset(ErrorCode.PROTOCOL_ERROR);
  }

  onInboundWSChunk(op: number, fin: boolean, payload: Uint8Array): void {
    if (this.terminated || this.remoteCloseReceived || !this.hdrComplete) {
      if (!this.hdrComplete && !this.terminated) this.reset(ErrorCode.PROTOCOL_ERROR);
      return;
    }
    if (this.asmOp !== 0 && this.asmOp !== op) {
      this.reset(ErrorCode.PROTOCOL_ERROR);
      return;
    }
    const plen = payload.byteLength;
    if (this.asmBytes + plen > MAX_WS_MESSAGE_BYTES) {
      // H-5c: Unconditionally send 1009 and clean up regardless of localFin!
      if (!this.localFin) {
        this.close(1009, 'message exceeds 16 MiB');
      }
      this.abortLocal(new Error('WebSocket message exceeds 16 MiB'));
      return;
    }
    const memCost = plen + INBOUND_CHUNK_OVERHEAD;
    if (!this.mux.reserveSessionBytes(memCost)) {
      this.reset(ErrorCode.FLOW_CONTROL_ERROR);
      return;
    }
    this.asmOp = op;
    if (plen > 0) {
      const copy = new Uint8Array(plen);
      copy.set(payload);
      this.asmChunks.push(copy);
      this.asmBytes += plen;
    }
    this.asmMemCost += memCost;

    // Replenish pure payload-byte CREDIT smoothly during streaming large messages.
    if (plen > 0) {
      this.consumedBytes += plen;
      this.consumedChunks++;
      if (this.consumedBytes >= CREDIT_THRESHOLD_BYTES || this.consumedChunks >= CREDIT_THRESHOLD_CHUNKS) {
        const delta = this.consumedBytes;
        this.consumedBytes = 0;
        this.consumedChunks = 0;
        this.sendCredit(delta);
      }
    }

    if (!fin) return;

    const assembled = concatChunks(this.asmChunks, this.asmBytes);
    const totalMemCost = this.asmMemCost;
    const isBinary = this.asmOp === FrameType.WS_BINARY;
    this.asmOp = 0;
    this.asmChunks = [];
    this.asmBytes = 0;
    this.asmMemCost = 0;

    let msg: WSDeliveredMessage;
    if (isBinary) {
      msg = { isBinary: true, data: assembled };
    } else {
      try {
        msg = { isBinary: false, data: utf8Dec.decode(assembled) };
      } catch {
        this.mux.releaseSessionBytes(totalMemCost);
        this.reset(ErrorCode.PROTOCOL_ERROR);
        return;
      }
    }

    if (this.msgCb) {
      this.mux.releaseSessionBytes(totalMemCost);
      this.msgCb(msg);
    } else {
      this.pendingMsgs.push({ msg, memCost: totalMemCost });
    }
  }

  onInboundWSClose(payload: Uint8Array): void {
    if (this.terminated || this.remoteCloseReceived) return;
    let code = 1000;
    let reason = '';
    try {
      const dec = decodeWSClosePayload(payload);
      code = dec.code;
      reason = dec.reason;
    } catch {
      this.reset(ErrorCode.PROTOCOL_ERROR);
      return;
    }
    this.remoteCloseReceived = true;
    this.remoteFin = true;
    // Free any half-assembled incomplete message immediately.
    if (this.asmMemCost > 0) {
      this.mux.releaseSessionBytes(this.asmMemCost);
      this.asmOp = 0;
      this.asmChunks = [];
      this.asmBytes = 0;
      this.asmMemCost = 0;
    }
    this.maybeDetach();

    if (this.pendingMsgs.length === 0 && this.closeCb) {
      this.finishAfterCloseDelivery(code, reason, true);
    } else {
      this.pendingClose = { code, reason, wasClean: true };
    }
  }

  private finishAfterCloseDelivery(code: number, reason: string, wasClean: boolean): void {
    if (this.terminated) return;
    this.terminated = true;
    this.cleanupInbound();
    this.detach();
    this.closeCb?.(code, reason, wasClean);
  }

  protected cleanupInbound(): void {
    let freed = this.asmMemCost;
    this.asmOp = 0;
    this.asmChunks = [];
    this.asmBytes = 0;
    this.asmMemCost = 0;
    for (const item of this.pendingMsgs) {
      freed += item.memCost;
    }
    this.pendingMsgs = [];
    this.pendingClose = null;
    if (freed > 0) this.mux.releaseSessionBytes(freed);
  }

  protected notifyAbort(reason: Error): void {
    this.closeCb?.(1006, reason.message, false);
  }
}

export class PoolMux {
  readonly poolSize: number;
  private ctrlDC: DataChannelLike | null = null;
  private ctrlAsmType = 0;
  private ctrlAsmChunks: Uint8Array[] = [];
  private ctrlAsmBytes = 0;

  private lanes: DataLane[] = [];
  private nextStreamId = 1;
  private activeStreams = 0;
  private sessionInboundBytes = 0;
  private _chunkSize = DEFAULT_CHUNK_SIZE;
  private closed = false;
  private callbacks: PoolMuxCallbacks;
  private pingSeq = 1;
  private pingWaiters = new Map<string, { resolve: (ok: boolean) => void; timer: ReturnType<typeof setTimeout> }>();

  constructor(poolSize: number = DEFAULT_POOL_SIZE, callbacks: PoolMuxCallbacks = {}) {
    this.poolSize = Math.max(1, Math.min(poolSize, MAX_POOL_SIZE));
    this.callbacks = callbacks;
    for (let i = 0; i < this.poolSize; i++) {
      this.lanes.push({ id: i, dc: null, streams: new Map(), lowWaiters: [] });
    }
  }

  get chunkSize(): number {
    return this._chunkSize;
  }

  get sessionMemUsed(): number {
    return this.sessionInboundBytes;
  }

  get activeStreamCount(): number {
    return this.activeStreams;
  }

  setMaxMessageSize(mms: number): void {
    if (!Number.isFinite(mms) || mms < MIN_NEGOTIATED_MAX_MSG_SIZE) return;
    const nextChunk = Math.max(MIN_CHUNK_SIZE, Math.min(DEFAULT_CHUNK_SIZE, Math.floor(mms) - HEADER_SIZE));
    if (nextChunk < this._chunkSize) {
      this._chunkSize = nextChunk;
    }
  }

  reserveSessionBytes(n: number): boolean {
    if (n <= 0) return true;
    if (this.sessionInboundBytes + n > DEFAULT_SESSION_MEMORY_BUDGET) {
      return false;
    }
    this.sessionInboundBytes += n;
    return true;
  }

  releaseSessionBytes(n: number): void {
    if (n <= 0) return;
    this.sessionInboundBytes = Math.max(0, this.sessionInboundBytes - n);
  }

  attachControlDC(dc: DataChannelLike): void {
    this.ctrlDC = dc;
    dc.binaryType = 'arraybuffer';
    dc.onmessage = (ev) => {
      const bytes = toUint8Array(ev.data);
      if (bytes) this.handleControlMessage(bytes);
    };
  }

  attachLaneDC(laneIdx: number, dc: DataChannelLike): void {
    const lane = this.lanes[laneIdx];
    if (!lane) return;
    lane.dc = dc;
    dc.binaryType = 'arraybuffer';
    dc.bufferedAmountLowThreshold = SCTP_LOW_WATER;
    dc.onbufferedamountlow = () => {
      const waiters = lane.lowWaiters;
      lane.lowWaiters = [];
      for (const fn of waiters) fn();
    };
    dc.onmessage = (ev) => {
      const bytes = toUint8Array(ev.data);
      if (bytes) this.handleLaneMessage(laneIdx, bytes);
    };
    dc.onclose = () => {
      this.handleLaneClose(laneIdx);
    };
  }

  sendControlFrame(type: number, payload: Uint8Array): void {
    if (this.closed || !this.ctrlDC || this.ctrlDC.readyState !== 'open') {
      throw new Error('control channel not open');
    }
    if (!isControlFrameType(type)) {
      throw new Error(`invalid control frame type: 0x${type.toString(16)}`);
    }
    if (isControlJsonFrameType(type) && payload.byteLength > MAX_CONTROL_JSON_SIZE) {
      throw new RangeError('control JSON payload exceeds 128 KiB');
    }
    if (!isControlJsonFrameType(type) || payload.byteLength <= this._chunkSize) {
      this.ctrlDC.send(encodeFrame(0, type, FrameFlag.NONE, payload));
      return;
    }
    for (let off = 0; off < payload.byteLength; off += this._chunkSize) {
      const end = Math.min(off + this._chunkSize, payload.byteLength);
      const flags = end < payload.byteLength ? FrameFlag.MORE : FrameFlag.NONE;
      this.ctrlDC.send(encodeFrame(0, type, flags, payload.subarray(off, end)));
    }
  }

  sendGoAway(code: number): void {
    try {
      this.sendControlFrame(FrameType.GOAWAY, encodeGoAwayPayload(code));
    } catch {
      // ignore
    }
  }

  /**
   * Sends an 8-byte PING frame and verifies an exact 8-byte PONG echo within timeoutMs.
   * Immediately resolves false if mux closes while in flight.
   */
  ping(timeoutMs = 5_000): Promise<boolean> {
    if (this.closed || !this.ctrlDC || this.ctrlDC.readyState !== 'open') {
      return Promise.resolve(false);
    }
    const seq = this.pingSeq++;
    const payload = new Uint8Array(8);
    const dv = new DataView(payload.buffer);
    dv.setUint32(0, Math.floor(seq / 0x100000000) >>> 0, false);
    dv.setUint32(4, seq >>> 0, false);
    const key = Array.from(payload).join(',');

    return new Promise<boolean>((resolve) => {
      const timer = setTimeout(() => {
        this.pingWaiters.delete(key);
        resolve(false);
      }, timeoutMs);
      this.pingWaiters.set(key, { resolve, timer });
      try {
        this.sendControlFrame(FrameType.PING, payload);
      } catch {
        clearTimeout(timer);
        this.pingWaiters.delete(key);
        resolve(false);
      }
    });
  }

  handleControlMessage(raw: Uint8Array): void {
    if (this.closed) return;
    let f: Frame;
    try {
      f = decodeFrame(raw);
    } catch (err) {
      this.callbacks.onProtocolError?.(err instanceof Error ? err : new Error(String(err)));
      return;
    }
    if (f.streamId !== 0 || !isControlFrameType(f.type)) {
      this.callbacks.onProtocolError?.(new Error('non-control frame on p2pctrl'));
      return;
    }

    if (f.type === FrameType.PING) {
      try {
        this.sendControlFrame(FrameType.PONG, f.payload);
      } catch {
        // ignore
      }
      return;
    }
    if (f.type === FrameType.PONG) {
      const key = Array.from(f.payload).join(',');
      const waiter = this.pingWaiters.get(key);
      if (waiter) {
        clearTimeout(waiter.timer);
        this.pingWaiters.delete(key);
        waiter.resolve(true);
      }
      return;
    }
    if (f.type === FrameType.GOAWAY) {
      try {
        this.callbacks.onGoAway?.(decodeGoAwayPayload(f.payload));
      } catch (err) {
        this.callbacks.onProtocolError?.(err instanceof Error ? err : new Error(String(err)));
      }
      return;
    }

    const hasMore = (f.flags & FrameFlag.MORE) !== 0;
    if (this.ctrlAsmType === 0) {
      if (!hasMore) {
        if (f.payload.byteLength > MAX_CONTROL_JSON_SIZE) {
          this.callbacks.onProtocolError?.(new Error('control frame exceeds 128 KiB'));
          return;
        }
        this.callbacks.onControlFrame?.(f);
        return;
      }
      this.ctrlAsmType = f.type;
      this.ctrlAsmChunks = [new Uint8Array(f.payload)];
      this.ctrlAsmBytes = f.payload.byteLength;
      return;
    }

    if (f.type !== this.ctrlAsmType) {
      this.resetCtrlAsm();
      this.callbacks.onProtocolError?.(new Error('interleaved control JSON frame'));
      return;
    }
    this.ctrlAsmChunks.push(new Uint8Array(f.payload));
    this.ctrlAsmBytes += f.payload.byteLength;
    if (this.ctrlAsmChunks.length > MAX_CONTROL_CHUNKS || this.ctrlAsmBytes > MAX_CONTROL_JSON_SIZE) {
      this.resetCtrlAsm();
      this.callbacks.onProtocolError?.(new Error('assembled control JSON exceeds limit'));
      return;
    }
    if (hasMore) return;

    const assembled = concatChunks(this.ctrlAsmChunks, this.ctrlAsmBytes);
    const type = this.ctrlAsmType;
    this.resetCtrlAsm();
    this.callbacks.onControlFrame?.({ streamId: 0, type, flags: FrameFlag.NONE, payload: assembled });
  }

  private resetCtrlAsm(): void {
    this.ctrlAsmType = 0;
    this.ctrlAsmChunks = [];
    this.ctrlAsmBytes = 0;
  }

  handleLaneMessage(laneIdx: number, raw: Uint8Array): void {
    if (this.closed) return;
    const lane = this.lanes[laneIdx];
    if (!lane) return;
    let f: Frame;
    try {
      f = decodeFrame(raw);
    } catch (err) {
      this.callbacks.onProtocolError?.(err instanceof Error ? err : new Error(String(err)));
      return;
    }
    if (f.streamId === 0 || !isStreamFrameType(f.type)) {
      this.callbacks.onProtocolError?.(new Error('invalid stream frame on data lane'));
      return;
    }

    const st = lane.streams.get(f.streamId);
    if (!st) return; // Silently ignore late frames for already-closed streams (HTTP/2 semantics).

    switch (f.type) {
      case FrameType.HEADERS:
        st.onInboundHeaders(f.flags, f.payload);
        break;
      case FrameType.DATA:
        st.onInboundData(f.flags, f.payload);
        break;
      case FrameType.WS_TEXT:
      case FrameType.WS_BINARY:
        st.onInboundWSChunk(f.type, (f.flags & FrameFlag.MSG_FIN) !== 0, f.payload);
        break;
      case FrameType.WS_CLOSE:
        st.onInboundWSClose(f.payload);
        break;
      case FrameType.CREDIT:
        st.onInboundCredit(new DataView(f.payload.buffer, f.payload.byteOffset, 4).getUint32(0, false));
        break;
      case FrameType.RESET:
        st.onPeerReset(decodeResetPayload(f.payload));
        break;
    }
  }

  handleLaneClose(laneIdx: number): void {
    const lane = this.lanes[laneIdx];
    if (!lane) return;
    lane.dc = null;
    const waiters = lane.lowWaiters;
    lane.lowWaiters = [];
    for (const fn of waiters) fn();

    const victims = Array.from(lane.streams.values());
    lane.streams.clear();
    for (const st of victims) {
      st.abortLocal(new Error('data lane closed'));
    }
  }

  private pickLane(): DataLane {
    let best: DataLane | null = null;
    let bestLoad = Infinity;
    let bestBuf = Infinity;
    for (const lane of this.lanes) {
      if (!lane.dc || lane.dc.readyState !== 'open') continue;
      const load = lane.streams.size;
      const buf = lane.dc.bufferedAmount;
      if (load < bestLoad || (load === bestLoad && buf < bestBuf)) {
        bestLoad = load;
        bestBuf = buf;
        best = lane;
        if (load === 0 && buf === 0) break;
      }
    }
    if (!best) throw new Error('No open P2P data lane available');
    return best;
  }

  async waitLaneWritable(lane: DataLane): Promise<void> {
    if (this.closed || !lane.dc || lane.dc.readyState !== 'open') {
      throw new Error('data lane not open');
    }
    if (lane.dc.bufferedAmount < SCTP_HIGH_WATER) return;
    const deadline = Date.now() + WINDOW_WAIT_TIMEOUT_MS;
    while (!this.closed && lane.dc && lane.dc.readyState === 'open' && lane.dc.bufferedAmount > SCTP_LOW_WATER) {
      const rem = deadline - Date.now();
      if (rem <= 0) throw new Error('SCTP backpressure write timeout');
      await new Promise<void>((resolve) => {
        const done = () => {
          clearTimeout(timer);
          resolve();
        };
        const timer = setTimeout(() => {
          const idx = lane.lowWaiters.indexOf(done);
          if (idx >= 0) lane.lowWaiters.splice(idx, 1);
          resolve();
        }, Math.min(25, rem));
        lane.lowWaiters.push(done);
      });
    }
  }

  sendLaneFrame(lane: DataLane, streamId: number, type: number, flags: number, payload: Uint8Array): void {
    if (this.closed || !lane.dc || lane.dc.readyState !== 'open') {
      throw new Error('data lane not open');
    }
    lane.dc.send(encodeFrame(streamId, type, flags, payload));
  }

  detachStream(lane: DataLane, streamId: number, st: ManagedStream): void {
    if (lane.streams.get(streamId) === st) {
      lane.streams.delete(streamId);
    }
    this.activeStreams = Math.max(0, this.activeStreams - 1);
  }

  private allocateStreamId(): number {
    if (this.nextStreamId >= 0xfffffff0) {
      throw new Error('stream ID space exhausted; reconnect required');
    }
    return this.nextStreamId++;
  }

  async openHTTPStream(headersRaw: Uint8Array, hasBody: boolean): Promise<PoolStream> {
    if (this.closed) throw new Error('PoolMux is closed');
    if (this.activeStreams >= MAX_CONCURRENT_STREAMS) {
      throw new Error('max concurrent P2P streams reached');
    }
    const lane = this.pickLane();
    await this.waitLaneWritable(lane);
    if (this.closed) throw new Error('PoolMux is closed');
    const streamId = this.allocateStreamId();
    const st = new PoolStream(this, lane, streamId);
    lane.streams.set(streamId, st);
    this.activeStreams++;
    try {
      await st.openWithHeaders(headersRaw, hasBody);
      return st;
    } catch (err) {
      st.abortLocal(err instanceof Error ? err : new Error(String(err)));
      throw err;
    }
  }

  async openWSStream(headersRaw: Uint8Array): Promise<WSStream> {
    if (this.closed) throw new Error('PoolMux is closed');
    if (this.activeStreams >= MAX_CONCURRENT_STREAMS) {
      throw new Error('max concurrent P2P streams reached');
    }
    const lane = this.pickLane();
    await this.waitLaneWritable(lane);
    if (this.closed) throw new Error('PoolMux is closed');
    const streamId = this.allocateStreamId();
    const st = new WSStream(this, lane, streamId);
    lane.streams.set(streamId, st);
    this.activeStreams++;
    try {
      await st.openWithHeaders(headersRaw);
      return st;
    } catch (err) {
      st.abortLocal(err instanceof Error ? err : new Error(String(err)));
      throw err;
    }
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.resetCtrlAsm();
    for (const [, w] of this.pingWaiters) {
      clearTimeout(w.timer);
      w.resolve(false);
    }
    this.pingWaiters.clear();
    if (this.ctrlDC) {
      try {
        this.ctrlDC.close();
      } catch {
        // ignore
      }
      this.ctrlDC = null;
    }
    for (let i = 0; i < this.lanes.length; i++) {
      const dc = this.lanes[i].dc;
      this.handleLaneClose(i);
      if (dc) {
        try {
          dc.close();
        } catch {
          // ignore
        }
      }
    }
  }
}
