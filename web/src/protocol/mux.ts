/**
 * P2PMux V7 DC-Pool Multiplexer (TypeScript Browser Side)
 * Architecture:
 *   - 1 Control DC (id=0, "p2pctrl"): SETTINGS / AUTH / CAP / PING / GOAWAY
 *   - 32 Pre-Negotiated Reusable HTTP Pool DCs (id=1..32, "p2p_01".."p2p_32"):
 *       Strict 5-state single-slot FSM (IDLE -> RECEIVING_HEADERS -> OPEN -> HALF_CLOSED -> CLOSED),
 *       overhead-aware memory budgets (+64B/chunk), 5s header assembly timeout,
 *       preserved unread short-response bodies until onData() drains, AbortSignal + 30s pool acquire.
 *   - Dynamic Dedicated WebSocket DCs ("p2pws", DCEP ordered, Gen === 1):
 *       Bounded concurrency (DEFAULT_MAX_WS_CONNS = 64), sessionInboundBytes accounting,
 *       downstream-consumption-driven CREDIT replenishment up to 16 MiB.
 */

import {
  DEFAULT_CHUNK_SIZE,
  DEFAULT_MAX_WS_CONNS,
  DEFAULT_POOL_SIZE,
  ErrorCode,
  FrameFlag,
  FrameType,
  HEADER_SIZE,
  MAX_CONTINUATION_FRAMES,
  MAX_CONTROL_JSON_SIZE,
  MAX_TOTAL_HEADERS_SIZE,
  MAX_WS_MESSAGE_BYTES,
  MIN_CHUNK_SIZE,
  decodeCreditPayload,
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
  isPoolDataFrameType,
  isWSDataFrameType,
  type Frame,
} from './frame';

export const DEFAULT_STREAM_WINDOW_SIZE = 2 * 1024 * 1024; // 2 MiB
export const MAX_STREAM_INBOUND_BYTES = 4 * 1024 * 1024; // 4 MiB
export const MAX_SESSION_INBOUND_BYTES = 32 * 1024 * 1024; // 32 MiB
export const PER_CHUNK_MEM_OVERHEAD = 64;
export const MAX_INBOUND_QUEUE_LEN = 4096;
export const HEADER_ASSEMBLY_TIMEOUT_MS = 5_000;
export const POOL_ACQUIRE_TIMEOUT_MS = 30_000;
export const SEND_WINDOW_TIMEOUT_MS = 30_000;
export const DC_HIGH_WATER_MARK = 512 * 1024;
export const DC_LOW_WATER_MARK = 128 * 1024;
export const GOAWAY_GEN_THRESHOLD = 0xffff0000;

type RxState = 'IDLE' | 'RECEIVING_HEADERS' | 'OPEN' | 'HALF_CLOSED' | 'CLOSED';

export interface DataChannelLike {
  readonly readyState: string;
  readonly bufferedAmount: number;
  bufferedAmountLowThreshold: number;
  binaryType?: string;
  send(data: Uint8Array | ArrayBuffer): void;
  close(): void;
  addEventListener(type: string, listener: (ev: Event) => void): void;
  removeEventListener(type: string, listener: (ev: Event) => void): void;
}

export interface PoolMuxCallbacks {
  onControlFrame?: (frame: Frame) => void;
  onGoAway?: (code: number) => void;
}

interface Waiter {
  resolve: () => void;
  reject: (err: Error) => void;
  timer: ReturnType<typeof setTimeout>;
}

interface SlotWaiter {
  resolve: (slot: PoolSlot) => void;
  reject: (err: Error) => void;
  cleanup: () => void;
}

interface PoolSlot {
  readonly slotIdx: number;
  dc: DataChannelLike | null;
  localGen: number;
  active: PoolStream | null;
  closed: boolean;
}

function toUint8Array(data: unknown): Uint8Array | null {
  if (data instanceof Uint8Array) return data;
  if (data instanceof ArrayBuffer) return new Uint8Array(data);
  if (ArrayBuffer.isView(data)) {
    return new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
  }
  return null;
}

async function waitForDCBackpressure(dc: DataChannelLike, isCancelled: () => boolean): Promise<void> {
  while (dc.bufferedAmount >= DC_HIGH_WATER_MARK) {
    if (isCancelled() || dc.readyState !== 'open') {
      throw new Error('DataChannel closed during backpressure wait');
    }
    dc.bufferedAmountLowThreshold = DC_LOW_WATER_MARK;
    await new Promise<void>((resolve) => {
      let done = false;
      const finish = () => {
        if (done) return;
        done = true;
        dc.removeEventListener('bufferedamountlow', finish);
        dc.removeEventListener('close', finish);
        clearTimeout(t);
        resolve();
      };
      const t = setTimeout(finish, 25);
      dc.addEventListener('bufferedamountlow', finish);
      dc.addEventListener('close', finish);
    });
  }
}

/**
 * Reusable-pool HTTP Stream bound to a single ordered DataChannel slot + monotonic generation.
 */
export class PoolStream {
  readonly gen: number;
  readonly slotIdx: number;
  private readonly mux: PoolMux;
  private readonly slot: PoolSlot;
  private readonly manualFlowControl: boolean;

  private sendWindow = DEFAULT_STREAM_WINDOW_SIZE;
  private recvWindowRem = DEFAULT_STREAM_WINDOW_SIZE;
  private sendWaiters: Waiter[] = [];

  private rxState: RxState = 'IDLE';
  private headerTimer: ReturnType<typeof setTimeout> | null = null;
  private headerChunks: Uint8Array[] = [];
  private headerBytes = 0;
  private headerReservedBytes = 0;
  private respHeadersHadFin = false;

  private inboundChunks: Uint8Array[] = [];
  private inboundBytes = 0; // payload + PER_CHUNK_MEM_OVERHEAD per queued chunk
  private unackedDeliveredBytes = 0;
  private unackedChunkLens: number[] = [];
  private consumedForCredit = 0;

  private localClosed = false;
  private remoteClosed = false;
  private terminated = false;
  private detached = false;

  private headersHandler: ((raw: Uint8Array) => void) | null = null;
  private dataHandler: ((chunk: Uint8Array) => void) | null = null;
  private endHandler: (() => void) | null = null;
  private resetHandler: ((code: number) => void) | null = null;

  private pendingHeaders: Uint8Array | null = null;
  private pendingResetCode: number | null = null;

  constructor(mux: PoolMux, slot: PoolSlot, gen: number, manualFlowControl: boolean) {
    this.mux = mux;
    this.slot = slot;
    this.slotIdx = slot.slotIdx;
    this.gen = gen;
    this.manualFlowControl = manualFlowControl;
  }

  get isClosed(): boolean {
    return this.terminated;
  }

  get hasResponseBody(): boolean {
    return !this.respHeadersHadFin;
  }

  onHeaders(cb: (raw: Uint8Array) => void): void {
    this.headersHandler = cb;
    if (this.pendingHeaders) {
      const h = this.pendingHeaders;
      this.pendingHeaders = null;
      cb(h);
    }
  }

  onData(cb: (chunk: Uint8Array) => void): void {
    this.dataHandler = cb;
    if (this.inboundChunks.length > 0) {
      const queued = this.inboundChunks;
      const queuedBudget = this.inboundBytes;
      this.inboundChunks = [];
      this.inboundBytes = 0;
      this.mux._releaseSessionBytes(queuedBudget);
      let payloadSum = 0;
      for (const ch of queued) {
        this.unackedDeliveredBytes += ch.byteLength;
        this.unackedChunkLens.push(ch.byteLength);
        payloadSum += ch.byteLength;
        cb(ch);
      }
      if (!this.manualFlowControl) {
        this.ackConsumed(payloadSum);
      }
    }
    if (this.remoteClosed && !this.terminated && this.pendingResetCode === null && this.endHandler) {
      const eh = this.endHandler;
      this.endHandler = null;
      eh();
    }
    this.checkFullClose();
  }

  onEnd(cb: () => void): void {
    if (this.remoteClosed && this.inboundChunks.length === 0 && this.pendingResetCode === null) {
      cb();
      return;
    }
    this.endHandler = cb;
  }

  onReset(cb: (code: number) => void): void {
    if (this.pendingResetCode !== null) {
      cb(this.pendingResetCode);
      return;
    }
    this.resetHandler = cb;
  }

  /**
   * Issues in-band CREDIT strictly clamped to bytes actually delivered and unacknowledged,
   * including exact per-chunk overhead conservation (+64B/chunk).
   */
  ackConsumed(bytes: number): void {
    if (this.terminated || this.remoteClosed || bytes <= 0 || !Number.isFinite(bytes)) return;
    let rem = Math.min(Math.floor(bytes), this.unackedDeliveredBytes);
    if (rem <= 0) return;
    this.unackedDeliveredBytes -= rem;
    let creditCost = rem;
    while (rem > 0 && this.unackedChunkLens.length > 0) {
      if (rem >= this.unackedChunkLens[0]) {
        rem -= this.unackedChunkLens.shift()!;
        creditCost += PER_CHUNK_MEM_OVERHEAD;
      } else {
        this.unackedChunkLens[0] -= rem;
        rem = 0;
      }
    }
    this.consumedForCredit += creditCost;
    if (this.consumedForCredit >= DEFAULT_STREAM_WINDOW_SIZE / 4) {
      const delta = this.consumedForCredit;
      this.consumedForCredit = 0;
      this.recvWindowRem += delta;
      try {
        this.sendSlotFrame(FrameType.CREDIT, FrameFlag.NONE, encodeCreditPayload(delta));
      } catch {
        this.terminate(ErrorCode.CANCEL, true);
      }
    }
  }

  async sendHeaders(tlv: Uint8Array, fin: boolean): Promise<void> {
    if (tlv.byteLength > MAX_TOTAL_HEADERS_SIZE) {
      throw new RangeError('HEADERS exceeds 64 KiB');
    }
    if (this.terminated || this.localClosed) {
      throw new Error('stream write closed');
    }
    const chunkSize = this.mux.effectiveChunkSize();
    const totalChunks = Math.max(1, Math.ceil(tlv.byteLength / chunkSize));
    if (totalChunks - 1 > MAX_CONTINUATION_FRAMES) {
      throw new RangeError('HEADERS exceeds max continuation frame count');
    }

    if (totalChunks === 1) {
      const flags = fin ? FrameFlag.FIN : FrameFlag.NONE;
      this.sendSlotFrame(FrameType.HEADERS, flags, tlv);
    } else {
      let off = 0;
      for (let i = 0; i < totalChunks; i++) {
        const end = Math.min(tlv.byteLength, off + chunkSize);
        const isFirst = i === 0;
        const isLast = i === totalChunks - 1;
        const type = isFirst ? FrameType.HEADERS : FrameType.CONTINUATION;
        let flags = FrameFlag.NONE;
        if (isFirst && fin) flags |= FrameFlag.FIN;
        if (!isLast) flags |= FrameFlag.MORE_HEADERS;
        this.sendSlotFrame(type, flags, tlv.subarray(off, end));
        off = end;
      }
    }
    if (fin) {
      this.localClosed = true;
      this.checkFullClose();
    }
  }

  async sendData(payload: Uint8Array, fin = false): Promise<void> {
    if (this.terminated || this.localClosed) {
      throw new Error('stream write closed');
    }
    const dc = this.slot.dc;
    if (!dc || dc.readyState !== 'open') {
      throw new Error('pool DataChannel not open');
    }
    let off = 0;
    while (off < payload.byteLength) {
      await waitForDCBackpressure(dc, () => this.terminated || this.localClosed);
      const chunkMax = this.mux.effectiveChunkSize();
      const want = Math.min(payload.byteLength - off, chunkMax);
      const grant = await this.consumeSendWindow(want);
      const end = off + grant;
      const isLastAndFin = fin && end === payload.byteLength;
      this.sendSlotFrame(FrameType.DATA, isLastAndFin ? FrameFlag.FIN : FrameFlag.NONE, payload.subarray(off, end));
      off = end;
    }
    if (fin && payload.byteLength === 0) {
      this.sendSlotFrame(FrameType.DATA, FrameFlag.FIN, new Uint8Array(0));
    }
    if (fin) {
      this.localClosed = true;
      this.checkFullClose();
    }
  }

  async finishWrite(): Promise<void> {
    if (this.terminated || this.localClosed) return;
    await this.sendData(new Uint8Array(0), true);
  }

  reset(code: number = ErrorCode.CANCEL): void {
    if (this.terminated) return;
    try {
      this.sendSlotFrame(FrameType.RESET, FrameFlag.NONE, encodeResetPayload(code));
    } catch {
      // ignore send failure during teardown
    }
    this.terminate(code, false);
  }

  /** @internal */
  _handleFrame(f: Frame): void {
    if (this.terminated || f.gen !== this.gen) return;
    switch (f.type) {
      case FrameType.HEADERS:
        if (this.rxState !== 'IDLE') {
          this.reset(ErrorCode.PROTOCOL_ERROR);
          return;
        }
        this.rxState = 'RECEIVING_HEADERS';
        this.respHeadersHadFin = (f.flags & FrameFlag.FIN) !== 0;
        this.startHeaderTimer();
        this.appendHeaderChunk(f.payload, (f.flags & FrameFlag.MORE_HEADERS) !== 0);
        break;
      case FrameType.CONTINUATION:
        if (this.rxState !== 'RECEIVING_HEADERS') {
          this.reset(ErrorCode.PROTOCOL_ERROR);
          return;
        }
        if (f.payload.byteLength === 0 && (f.flags & FrameFlag.MORE_HEADERS) !== 0) {
          this.reset(ErrorCode.PROTOCOL_ERROR);
          return;
        }
        this.appendHeaderChunk(f.payload, (f.flags & FrameFlag.MORE_HEADERS) !== 0);
        break;
      case FrameType.DATA:
        if (this.rxState !== 'OPEN') {
          this.reset(ErrorCode.PROTOCOL_ERROR);
          return;
        }
        this.handleData(f.payload, (f.flags & FrameFlag.FIN) !== 0);
        break;
      case FrameType.CREDIT: {
        const delta = decodeCreditPayload(f.payload);
        if (this.sendWindow + delta > 0x7fffffff) {
          this.reset(ErrorCode.FLOW_CONTROL_ERROR);
          return;
        }
        this.sendWindow += delta;
        this.wakeSendWaiters();
        break;
      }
      case FrameType.RESET: {
        const code = decodeResetPayload(f.payload);
        this.terminate(code, true);
        break;
      }
    }
  }

  /** @internal */
  _terminateFromMux(code: number): void {
    this.terminate(code, true);
  }

  private startHeaderTimer(): void {
    this.clearHeaderTimer();
    this.headerTimer = setTimeout(() => {
      this.headerTimer = null;
      if (!this.terminated && this.rxState === 'RECEIVING_HEADERS') {
        this.reset(ErrorCode.PROTOCOL_ERROR);
      }
    }, HEADER_ASSEMBLY_TIMEOUT_MS);
  }

  private clearHeaderTimer(): void {
    if (this.headerTimer) {
      clearTimeout(this.headerTimer);
      this.headerTimer = null;
    }
  }

  private appendHeaderChunk(chunk: Uint8Array, more: boolean): void {
    if (
      this.headerBytes + chunk.byteLength > MAX_TOTAL_HEADERS_SIZE ||
      this.headerChunks.length >= 1 + MAX_CONTINUATION_FRAMES
    ) {
      this.reset(ErrorCode.FRAME_SIZE_ERROR);
      return;
    }
    const cost = chunk.byteLength + PER_CHUNK_MEM_OVERHEAD;
    if (!this.mux._tryReserveSessionBytes(cost)) {
      this.reset(ErrorCode.REFUSED_STREAM);
      return;
    }
    const cp = new Uint8Array(chunk.byteLength);
    cp.set(chunk);
    this.headerChunks.push(cp);
    this.headerBytes += cp.byteLength;
    this.headerReservedBytes += cost;
    if (more) return;

    this.clearHeaderTimer();
    const assembled = new Uint8Array(this.headerBytes);
    let off = 0;
    for (const c of this.headerChunks) {
      assembled.set(c, off);
      off += c.byteLength;
    }
    this.headerChunks = [];
    this.headerBytes = 0;
    if (this.headerReservedBytes > 0) {
      this.mux._releaseSessionBytes(this.headerReservedBytes);
      this.headerReservedBytes = 0;
    }
    this.rxState = this.respHeadersHadFin ? 'HALF_CLOSED' : 'OPEN';

    if (this.headersHandler) {
      this.headersHandler(assembled);
    } else {
      this.pendingHeaders = assembled;
    }

    if (this.respHeadersHadFin) {
      this.remoteClosed = true;
      if (this.inboundChunks.length === 0 && this.endHandler) {
        const eh = this.endHandler;
        this.endHandler = null;
        eh();
      }
      this.checkFullClose();
    }
  }

  private handleData(payload: Uint8Array, fin: boolean): void {
    if (this.remoteClosed) {
      this.reset(ErrorCode.PROTOCOL_ERROR);
      return;
    }
    const len = payload.byteLength;
    if (len === 0 && !fin) {
      this.reset(ErrorCode.PROTOCOL_ERROR);
      return;
    }
    if (len > 0) {
      const cost = len + PER_CHUNK_MEM_OVERHEAD;
      if (cost > this.recvWindowRem) {
        this.reset(ErrorCode.FLOW_CONTROL_ERROR);
        return;
      }
      this.recvWindowRem -= cost;
      const cp = new Uint8Array(len);
      cp.set(payload);

      if (this.dataHandler) {
        this.unackedDeliveredBytes += len;
        this.unackedChunkLens.push(len);
        this.dataHandler(cp);
        if (!this.manualFlowControl) {
          this.ackConsumed(len);
        }
      } else {
        if (
          this.inboundChunks.length >= MAX_INBOUND_QUEUE_LEN ||
          this.inboundBytes + cost > MAX_STREAM_INBOUND_BYTES ||
          !this.mux._tryReserveSessionBytes(cost)
        ) {
          this.reset(ErrorCode.FLOW_CONTROL_ERROR);
          return;
        }
        this.inboundChunks.push(cp);
        this.inboundBytes += cost;
      }
    }
    if (fin) {
      this.remoteClosed = true;
      this.rxState = 'HALF_CLOSED';
      if (this.inboundChunks.length === 0 && this.endHandler) {
        const eh = this.endHandler;
        this.endHandler = null;
        eh();
      }
      this.checkFullClose();
    }
  }

  // Never purge unread short-response inboundChunks before onData() drains them.
  private checkFullClose(): void {
    if (this.localClosed && this.remoteClosed && this.inboundChunks.length === 0) {
      this.terminated = true;
      this.rxState = 'CLOSED';
      this.purgeInbound();
      this.detach();
    }
  }

  private terminate(code: number, notifyReset: boolean): void {
    if (this.terminated) return;
    this.terminated = true;
    this.localClosed = true;
    this.remoteClosed = true;
    this.rxState = 'CLOSED';
    this.clearHeaderTimer();
    this.purgeInbound();
    this.failSendWaiters(new Error(`stream reset (${code})`));
    this.detach();
    if (notifyReset) {
      this.pendingResetCode = code;
      if (this.resetHandler) {
        const rh = this.resetHandler;
        this.resetHandler = null;
        rh(code);
      }
    }
  }

  private purgeInbound(): void {
    this.clearHeaderTimer();
    const toRelease = this.inboundBytes + this.headerReservedBytes;
    if (toRelease > 0) {
      this.mux._releaseSessionBytes(toRelease);
      this.inboundBytes = 0;
      this.headerReservedBytes = 0;
    }
    this.inboundChunks = [];
    this.headerChunks = [];
    this.headerBytes = 0;
  }

  private detach(): void {
    if (this.detached) return;
    this.detached = true;
    this.mux._releasePoolSlot(this.slot, this);
  }

  private sendSlotFrame(type: number, flags: number, payload: Uint8Array): void {
    const dc = this.slot.dc;
    if (!dc || dc.readyState !== 'open' || this.slot.active !== this) {
      throw new Error('pool DataChannel not open or stream detached');
    }
    dc.send(encodeFrame(this.gen, type, flags, payload));
  }

  private async consumeSendWindow(want: number): Promise<number> {
    while (this.sendWindow <= PER_CHUNK_MEM_OVERHEAD) {
      if (this.terminated || this.localClosed) {
        throw new Error('stream closed waiting for credit');
      }
      await new Promise<void>((resolve, reject) => {
        const waiter: Waiter = {
          resolve,
          reject,
          timer: setTimeout(() => {
            const idx = this.sendWaiters.indexOf(waiter);
            if (idx >= 0) this.sendWaiters.splice(idx, 1);
            this.reset(ErrorCode.FLOW_CONTROL_ERROR);
            reject(new Error('flow control send window timeout'));
          }, SEND_WINDOW_TIMEOUT_MS),
        };
        this.sendWaiters.push(waiter);
      });
    }
    const take = Math.min(want, this.sendWindow - PER_CHUNK_MEM_OVERHEAD);
    this.sendWindow -= take + PER_CHUNK_MEM_OVERHEAD;
    return take;
  }

  private wakeSendWaiters(): void {
    while (this.sendWindow > PER_CHUNK_MEM_OVERHEAD && this.sendWaiters.length > 0) {
      const w = this.sendWaiters.shift()!;
      clearTimeout(w.timer);
      w.resolve();
    }
  }

  private failSendWaiters(err: Error): void {
    const list = this.sendWaiters.splice(0);
    for (const w of list) {
      clearTimeout(w.timer);
      w.reject(err);
    }
  }
}

/**
 * Dedicated WebSocket Stream bound to its own dynamic DCEP DataChannel ("p2pws", Gen === 1).
 * Accounts all inbound chunks & header fragments to sessionInboundBytes and issues CREDIT
 * strictly under session memory budget & downstream message delivery.
 */
export class WSStream {
  readonly gen = 1;
  private readonly mux: PoolMux;
  private readonly dc: DataChannelLike;

  private sendWindow = DEFAULT_STREAM_WINDOW_SIZE;
  private recvWindowRem = DEFAULT_STREAM_WINDOW_SIZE;
  private sendWaiters: Waiter[] = [];

  private rxState: RxState = 'IDLE';
  private headerTimer: ReturnType<typeof setTimeout> | null = null;
  private headerChunks: Uint8Array[] = [];
  private headerBytes = 0;
  private headerReservedBytes = 0;

  // Streaming WS message reassembly accounted to mux.sessionInboundBytes.
  private wsChunks: Uint8Array[] = [];
  private wsBytes = 0;
  private wsReservedBytes = 0;
  private wsCreditedBytes = 0;
  private wsIsBinary = false;
  private wsInProgress = false;
  private pendingMessages: Array<{ isBinary: boolean; payload: Uint8Array; reserved: number; uncredited: number }> = [];
  private pendingMessagesReserved = 0;
  private consumedForCredit = 0;

  private closed = false;
  private headersHandler: ((raw: Uint8Array) => void) | null = null;
  private msgHandler: ((isBinary: boolean, payload: Uint8Array) => void) | null = null;
  private closeHandler: ((code: number, reason: string) => void) | null = null;
  private resetHandler: ((code: number) => void) | null = null;
  private pendingHeaders: Uint8Array | null = null;
  private pendingClose: { code: number; reason: string } | null = null;
  private pendingResetCode: number | null = null;
  private localCloseSent = false;

  constructor(mux: PoolMux, dc: DataChannelLike) {
    this.mux = mux;
    this.dc = dc;
    dc.binaryType = 'arraybuffer';
    dc.addEventListener('message', this.onDCMessage);
    dc.addEventListener('close', this.onDCClose);
    dc.addEventListener('error', this.onDCClose);
  }

  get isClosed(): boolean {
    return this.closed;
  }

  onHeaders(cb: (raw: Uint8Array) => void): void {
    this.headersHandler = cb;
    if (this.pendingHeaders) {
      const h = this.pendingHeaders;
      this.pendingHeaders = null;
      cb(h);
    }
  }

  onMessage(cb: (isBinary: boolean, payload: Uint8Array) => void): void {
    this.msgHandler = cb;
    if (this.pendingMessages.length > 0) {
      const queued = this.pendingMessages.splice(0);
      const totalReserved = this.pendingMessagesReserved;
      this.pendingMessagesReserved = 0;
      if (totalReserved > 0) {
        this.mux._releaseSessionBytes(totalReserved);
      }
      for (const item of queued) {
        cb(item.isBinary, item.payload);
        this.creditConsumed(item.uncredited);
      }
    }
  }

  onClose(cb: (code: number, reason: string) => void): void {
    if (this.pendingClose) {
      const pc = this.pendingClose;
      this.pendingClose = null;
      cb(pc.code, pc.reason);
      return;
    }
    this.closeHandler = cb;
  }

  onReset(cb: (code: number) => void): void {
    if (this.pendingResetCode !== null) {
      const code = this.pendingResetCode;
      this.pendingResetCode = null;
      cb(code);
      return;
    }
    this.resetHandler = cb;
  }

  async sendHeaders(tlv: Uint8Array): Promise<void> {
    if (this.closed) throw new Error('WS stream closed');
    if (tlv.byteLength > MAX_TOTAL_HEADERS_SIZE) throw new RangeError('WS headers exceed 64 KiB');
    const chunkSize = this.mux.effectiveChunkSize();
    const totalChunks = Math.max(1, Math.ceil(tlv.byteLength / chunkSize));
    if (totalChunks - 1 > MAX_CONTINUATION_FRAMES) throw new RangeError('WS headers exceed chunk limit');
    let off = 0;
    for (let i = 0; i < totalChunks; i++) {
      const end = Math.min(tlv.byteLength, off + chunkSize);
      const isFirst = i === 0;
      const isLast = i === totalChunks - 1;
      const type = isFirst ? FrameType.HEADERS : FrameType.CONTINUATION;
      const flags = isLast ? FrameFlag.NONE : FrameFlag.MORE_HEADERS;
      this.sendFrame(type, flags, tlv.subarray(off, end));
      off = end;
    }
  }

  async sendWSMessage(isBinary: boolean, payload: Uint8Array): Promise<void> {
    if (this.closed || this.localCloseSent) throw new Error('WS stream closed');
    if (payload.byteLength > MAX_WS_MESSAGE_BYTES) {
      throw new RangeError('WS message exceeds 16 MiB limit');
    }
    const firstOp = isBinary ? FrameType.WS_BINARY : FrameType.WS_TEXT;
    if (payload.byteLength === 0) {
      await this.consumeSendWindow(0, true);
      this.sendFrame(firstOp, FrameFlag.MSG_FIN, payload);
      return;
    }
    let off = 0;
    let first = true;
    while (off < payload.byteLength) {
      await waitForDCBackpressure(this.dc, () => this.closed);
      const chunkMax = this.mux.effectiveChunkSize();
      const want = Math.min(payload.byteLength - off, chunkMax);
      const grant = await this.consumeSendWindow(want, false);
      const end = off + grant;
      const op = first ? firstOp : FrameType.WS_CONT;
      first = false;
      const flags = end === payload.byteLength ? FrameFlag.MSG_FIN : FrameFlag.NONE;
      this.sendFrame(op, flags, payload.subarray(off, end));
      off = end;
    }
  }

  sendWSClose(code = 1000, reason = '', immediateCleanup = false): void {
    if (this.closed || this.localCloseSent) return;
    this.localCloseSent = true;
    try {
      this.sendFrame(FrameType.WS_CLOSE, FrameFlag.NONE, encodeWSClosePayload(code, reason));
    } catch {
      this.cleanup(null);
      return;
    }
    if (immediateCleanup) {
      this.cleanup(null);
    }
  }

  reset(code: number = ErrorCode.CANCEL): void {
    if (this.closed) return;
    try {
      this.sendFrame(FrameType.RESET, FrameFlag.NONE, encodeResetPayload(code));
    } catch {
      // ignore send failure during reset
    }
    this.cleanup(null);
  }

  /** @internal */
  _closeFromMux(code: number): void {
    this.cleanup(code);
  }

  private startHeaderTimer(): void {
    this.clearHeaderTimer();
    this.headerTimer = setTimeout(() => {
      this.headerTimer = null;
      if (!this.closed && this.rxState === 'RECEIVING_HEADERS') {
        this.reset(ErrorCode.PROTOCOL_ERROR);
      }
    }, HEADER_ASSEMBLY_TIMEOUT_MS);
  }

  private clearHeaderTimer(): void {
    if (this.headerTimer) {
      clearTimeout(this.headerTimer);
      this.headerTimer = null;
    }
  }

  private readonly onDCMessage = (ev: Event): void => {
    if (this.closed) return;
    const raw = toUint8Array((ev as MessageEvent).data);
    if (!raw) return;
    let f: Frame;
    try {
      f = decodeFrame(raw);
    } catch {
      this.reset(ErrorCode.PROTOCOL_ERROR);
      return;
    }
    if (f.gen !== this.gen || !isWSDataFrameType(f.type)) {
      this.reset(ErrorCode.PROTOCOL_ERROR);
      return;
    }

    switch (f.type) {
      case FrameType.HEADERS:
        if (this.rxState !== 'IDLE') {
          this.reset(ErrorCode.PROTOCOL_ERROR);
          return;
        }
        this.rxState = 'RECEIVING_HEADERS';
        this.startHeaderTimer();
        this.handleHeaderFrame(f);
        break;
      case FrameType.CONTINUATION:
        if (this.rxState !== 'RECEIVING_HEADERS') {
          this.reset(ErrorCode.PROTOCOL_ERROR);
          return;
        }
        if (f.payload.byteLength === 0 && (f.flags & FrameFlag.MORE_HEADERS) !== 0) {
          this.reset(ErrorCode.PROTOCOL_ERROR);
          return;
        }
        this.handleHeaderFrame(f);
        break;
      case FrameType.CREDIT: {
        const delta = decodeCreditPayload(f.payload);
        if (this.sendWindow + delta > 0x7fffffff) {
          this.reset(ErrorCode.FLOW_CONTROL_ERROR);
          return;
        }
        this.sendWindow += delta;
        this.wakeSendWaiters();
        break;
      }
      case FrameType.RESET: {
        const code = decodeResetPayload(f.payload);
        this.cleanup(code);
        break;
      }
      case FrameType.WS_TEXT:
      case FrameType.WS_BINARY:
      case FrameType.WS_CONT:
        if (this.rxState !== 'OPEN') {
          this.reset(ErrorCode.PROTOCOL_ERROR);
          return;
        }
        this.handleWSChunk(f);
        break;
      case FrameType.WS_CLOSE: {
        if (this.rxState !== 'OPEN') {
          this.reset(ErrorCode.PROTOCOL_ERROR);
          return;
        }
        const { code, reason } = decodeWSClosePayload(f.payload);
        const ch = this.closeHandler;
        this.closeHandler = null;
        this.cleanup(null);
        if (ch) {
          ch(code, reason);
        } else {
          this.pendingClose = { code, reason };
        }
        break;
      }
    }
  };

  private readonly onDCClose = (): void => {
    if (!this.closed) {
      this.cleanup(ErrorCode.CANCEL);
    }
  };

  private handleHeaderFrame(f: Frame): void {
    if (
      this.headerBytes + f.payload.byteLength > MAX_TOTAL_HEADERS_SIZE ||
      this.headerChunks.length >= 1 + MAX_CONTINUATION_FRAMES
    ) {
      this.reset(ErrorCode.FRAME_SIZE_ERROR);
      return;
    }
    const cost = f.payload.byteLength + PER_CHUNK_MEM_OVERHEAD;
    if (!this.mux._tryReserveSessionBytes(cost)) {
      this.reset(ErrorCode.REFUSED_STREAM);
      return;
    }
    const cp = new Uint8Array(f.payload.byteLength);
    cp.set(f.payload);
    this.headerChunks.push(cp);
    this.headerBytes += cp.byteLength;
    this.headerReservedBytes += cost;
    if ((f.flags & FrameFlag.MORE_HEADERS) !== 0) return;

    this.clearHeaderTimer();
    const assembled = new Uint8Array(this.headerBytes);
    let off = 0;
    for (const c of this.headerChunks) {
      assembled.set(c, off);
      off += c.byteLength;
    }
    this.headerChunks = [];
    this.headerBytes = 0;
    if (this.headerReservedBytes > 0) {
      this.mux._releaseSessionBytes(this.headerReservedBytes);
      this.headerReservedBytes = 0;
    }
    this.rxState = 'OPEN';
    if (this.headersHandler) {
      this.headersHandler(assembled);
    } else {
      this.pendingHeaders = assembled;
    }
  }

  private creditConsumed(cost: number): void {
    if (this.closed || cost <= 0) return;
    this.consumedForCredit += cost;
    if (this.consumedForCredit >= DEFAULT_STREAM_WINDOW_SIZE / 8) {
      const delta = this.consumedForCredit;
      this.consumedForCredit = 0;
      this.recvWindowRem += delta;
      try {
        this.sendFrame(FrameType.CREDIT, FrameFlag.NONE, encodeCreditPayload(delta));
      } catch {
        this.cleanup(ErrorCode.CANCEL);
      }
    }
  }

  private handleWSChunk(f: Frame): void {
    const len = f.payload.byteLength;
    const cost = len + PER_CHUNK_MEM_OVERHEAD;
    if (cost > this.recvWindowRem) {
      this.reset(ErrorCode.FLOW_CONTROL_ERROR);
      return;
    }
    this.recvWindowRem -= cost;

    if (f.type === FrameType.WS_TEXT || f.type === FrameType.WS_BINARY) {
      if (this.wsInProgress) {
        this.reset(ErrorCode.PROTOCOL_ERROR);
        return;
      }
      this.wsInProgress = true;
      this.wsIsBinary = f.type === FrameType.WS_BINARY;
      this.wsChunks = [];
      this.wsBytes = 0;
      this.wsReservedBytes = 0;
      this.wsCreditedBytes = 0;
    } else if (!this.wsInProgress) {
      this.reset(ErrorCode.PROTOCOL_ERROR);
      return;
    }

    if (this.wsChunks.length >= MAX_INBOUND_QUEUE_LEN) {
      this.reset(ErrorCode.FLOW_CONTROL_ERROR);
      return;
    }
    if (this.wsBytes + len > MAX_WS_MESSAGE_BYTES) {
      this.sendWSClose(1009, 'Message Too Big', true);
      return;
    }
    if (!this.mux._tryReserveSessionBytes(cost)) {
      this.reset(ErrorCode.FLOW_CONTROL_ERROR);
      return;
    }
    this.wsReservedBytes += cost;
    if (len > 0) {
      const cp = new Uint8Array(len);
      cp.set(f.payload);
      this.wsChunks.push(cp);
      this.wsBytes += len;
    }

    const isFin = (f.flags & FrameFlag.MSG_FIN) !== 0;
    if (!isFin) {
      // For multi-chunk messages exceeding the 2 MiB window (up to 16 MiB), replenish flow
      // credit ONLY while sessionInboundBytes budget reservation succeeded AND downstream is attached.
      if (this.msgHandler) {
        this.wsCreditedBytes += cost;
        this.creditConsumed(cost);
      }
      return;
    }

    let full: Uint8Array;
    if (this.wsChunks.length === 0) {
      full = new Uint8Array(0);
    } else if (this.wsChunks.length === 1) {
      full = this.wsChunks[0];
    } else {
      full = new Uint8Array(this.wsBytes);
      let off = 0;
      for (const c of this.wsChunks) {
        full.set(c, off);
        off += c.byteLength;
      }
    }
    const isBin = this.wsIsBinary;
    const totalReserved = this.wsReservedBytes;
    const uncredited = Math.max(0, totalReserved - this.wsCreditedBytes);
    this.wsChunks = [];
    this.wsBytes = 0;
    this.wsReservedBytes = 0;
    this.wsCreditedBytes = 0;
    this.wsInProgress = false;

    if (this.msgHandler) {
      this.mux._releaseSessionBytes(totalReserved);
      this.msgHandler(isBin, full);
      this.creditConsumed(uncredited);
    } else {
      // Downstream hasn't attached yet: retain sessionInboundBytes & withhold CREDIT until onMessage drains.
      if (this.pendingMessages.length >= MAX_INBOUND_QUEUE_LEN) {
        this.mux._releaseSessionBytes(totalReserved);
        this.reset(ErrorCode.FLOW_CONTROL_ERROR);
        return;
      }
      this.pendingMessages.push({ isBinary: isBin, payload: full, reserved: totalReserved, uncredited });
      this.pendingMessagesReserved += totalReserved;
    }
  }

  private sendFrame(type: number, flags: number, payload: Uint8Array): void {
    if (this.dc.readyState !== 'open') {
      throw new Error('WS DataChannel not open');
    }
    this.dc.send(encodeFrame(this.gen, type, flags, payload));
  }

  private async consumeSendWindow(want: number, allowZeroPayload: boolean): Promise<number> {
    const minNeeded = allowZeroPayload ? PER_CHUNK_MEM_OVERHEAD : PER_CHUNK_MEM_OVERHEAD + 1;
    while (this.sendWindow < minNeeded) {
      if (this.closed) throw new Error('WS stream closed');
      await new Promise<void>((resolve, reject) => {
        const w: Waiter = {
          resolve,
          reject,
          timer: setTimeout(() => {
            const idx = this.sendWaiters.indexOf(w);
            if (idx >= 0) this.sendWaiters.splice(idx, 1);
            this.reset(ErrorCode.FLOW_CONTROL_ERROR);
            reject(new Error('WS flow control timeout'));
          }, SEND_WINDOW_TIMEOUT_MS),
        };
        this.sendWaiters.push(w);
      });
    }
    const take = Math.min(want, this.sendWindow - PER_CHUNK_MEM_OVERHEAD);
    this.sendWindow -= take + PER_CHUNK_MEM_OVERHEAD;
    return take;
  }

  private wakeSendWaiters(): void {
    while (this.sendWindow >= PER_CHUNK_MEM_OVERHEAD && this.sendWaiters.length > 0) {
      const w = this.sendWaiters.shift()!;
      clearTimeout(w.timer);
      w.resolve();
    }
  }

  private cleanup(resetCode: number | null): void {
    if (this.closed) return;
    this.closed = true;
    this.rxState = 'CLOSED';
    this.clearHeaderTimer();
    const toRelease = this.wsReservedBytes + this.headerReservedBytes + this.pendingMessagesReserved;
    if (toRelease > 0) {
      this.mux._releaseSessionBytes(toRelease);
      this.wsReservedBytes = 0;
      this.headerReservedBytes = 0;
      this.pendingMessagesReserved = 0;
    }
    this.wsChunks = [];
    this.wsBytes = 0;
    this.pendingMessages = [];
    this.headerChunks = [];
    this.headerBytes = 0;

    const list = this.sendWaiters.splice(0);
    for (const w of list) {
      clearTimeout(w.timer);
      w.reject(new Error('WS stream closed'));
    }
    this.dc.removeEventListener('message', this.onDCMessage);
    this.dc.removeEventListener('close', this.onDCClose);
    this.dc.removeEventListener('error', this.onDCClose);
    try {
      this.dc.close();
    } catch {
      // ignore
    }
    this.mux._unregisterWSStream(this);
    if (resetCode !== null) {
      const rh = this.resetHandler;
      this.resetHandler = null;
      if (rh) {
        rh(resetCode);
      } else {
        this.pendingResetCode = resetCode;
      }
    }
  }
}

function waitForDataChannelOpen(dc: DataChannelLike, timeoutMs = 10_000): Promise<void> {
  if (dc.readyState === 'open') return Promise.resolve();
  if (dc.readyState === 'closed' || dc.readyState === 'closing') {
    return Promise.reject(new Error('DataChannel already closed'));
  }
  return new Promise<void>((resolve, reject) => {
    let settled = false;
    const cleanup = () => {
      dc.removeEventListener('open', onOpen);
      dc.removeEventListener('close', onFail);
      dc.removeEventListener('error', onFail);
      clearTimeout(timer);
    };
    const onOpen = () => {
      if (settled) return;
      settled = true;
      cleanup();
      resolve();
    };
    const onFail = () => {
      if (settled) return;
      settled = true;
      cleanup();
      reject(new Error('DataChannel closed before opening'));
    };
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      cleanup();
      reject(new Error('Timed out waiting for DataChannel to open'));
    }, timeoutMs);
    dc.addEventListener('open', onOpen);
    dc.addEventListener('close', onFail);
    dc.addEventListener('error', onFail);
  });
}

export class PoolMux {
  private readonly callbacks: PoolMuxCallbacks;
  private readonly poolSize: number;
  private readonly maxWSConns: number;
  private ctrlDC: DataChannelLike | null = null;
  private readonly poolSlots: PoolSlot[] = [];
  private readonly idleSlots: PoolSlot[] = [];
  private readonly slotWaiters: SlotWaiter[] = [];
  private readonly wsStreams = new Set<WSStream>();
  private readonly pingWaiters = new Map<
    string,
    { resolve: (alive: boolean) => void; timer: ReturnType<typeof setTimeout> }
  >();
  private pingSeq = 0;

  private chunkSize = DEFAULT_CHUNK_SIZE;
  private sessionInboundBytes = 0;
  private activeHTTPCount = 0;
  private goAwaySent = false;
  private closed = false;

  constructor(
    callbacks: PoolMuxCallbacks = {},
    poolSize = DEFAULT_POOL_SIZE,
    maxWSConns = DEFAULT_MAX_WS_CONNS
  ) {
    this.callbacks = callbacks;
    this.poolSize = Math.max(1, poolSize);
    this.maxWSConns = Math.max(1, maxWSConns);
    for (let i = 0; i < this.poolSize; i++) {
      this.poolSlots.push({
        slotIdx: i,
        dc: null,
        localGen: 0,
        active: null,
        closed: false,
      });
    }
  }

  getActiveStreamCount(): number {
    return this.activeHTTPCount + this.wsStreams.size;
  }

  getPoolSize(): number {
    return this.poolSize;
  }

  getSessionInboundBytes(): number {
    return this.sessionInboundBytes;
  }

  effectiveChunkSize(): number {
    return this.chunkSize;
  }

  /**
   * Clamps frame payload size strictly DOWNWARD.
   */
  setMaxMessageSize(maxMsg: number): void {
    if (!Number.isFinite(maxMsg) || maxMsg <= HEADER_SIZE) return;
    const candidate = Math.max(MIN_CHUNK_SIZE, Math.min(DEFAULT_CHUNK_SIZE, Math.floor(maxMsg) - HEADER_SIZE));
    if (candidate < this.chunkSize) {
      this.chunkSize = candidate;
    }
  }

  attachControlDC(dc: DataChannelLike): void {
    this.ctrlDC = dc;
    dc.binaryType = 'arraybuffer';
    dc.addEventListener('message', (ev: Event) => {
      if (this.closed) return;
      const raw = toUint8Array((ev as MessageEvent).data);
      if (!raw) return;
      this.handleControlBytes(raw);
    });
  }

  attachPoolDC(slotIdx: number, dc: DataChannelLike): void {
    if (slotIdx < 0 || slotIdx >= this.poolSize) return;
    const slot = this.poolSlots[slotIdx];
    slot.dc = dc;
    dc.binaryType = 'arraybuffer';
    dc.addEventListener('message', (ev: Event) => {
      if (this.closed || slot.closed) return;
      const raw = toUint8Array((ev as MessageEvent).data);
      if (!raw) return;
      this.handlePoolBytes(slot, raw);
    });
    dc.addEventListener('close', () => {
      slot.closed = true;
      const idx = this.idleSlots.indexOf(slot);
      if (idx >= 0) this.idleSlots.splice(idx, 1);
      if (slot.active) {
        slot.active._terminateFromMux(ErrorCode.CANCEL);
      }
      this.rejectWaitersIfAllSlotsDead();
    });
    if (dc.readyState === 'open') {
      this.makeSlotAvailable(slot);
    } else {
      dc.addEventListener('open', () => {
        if (!this.closed && !slot.closed && slot.dc === dc && slot.active === null) {
          this.makeSlotAvailable(slot);
        }
      });
    }
  }

  sendControlFrame(type: number, payload: Uint8Array = new Uint8Array(0)): void {
    if (this.closed || !this.ctrlDC || this.ctrlDC.readyState !== 'open') {
      throw new Error('control channel not open');
    }
    if (!isControlFrameType(type)) {
      throw new Error(`invalid control frame type: 0x${type.toString(16)}`);
    }
    const maxCtrl = Math.min(this.chunkSize, MAX_CONTROL_JSON_SIZE);
    if (payload.byteLength > maxCtrl) {
      throw new RangeError(`control frame payload exceeds limit (${payload.byteLength} > ${maxCtrl})`);
    }
    this.ctrlDC.send(encodeFrame(0, type, FrameFlag.NONE, payload));
  }

  sendGoAway(code: number = ErrorCode.GOING_AWAY): void {
    if (this.goAwaySent || this.closed || !this.ctrlDC || this.ctrlDC.readyState !== 'open') return;
    this.goAwaySent = true;
    try {
      this.sendControlFrame(FrameType.GOAWAY, encodeGoAwayPayload(code));
    } catch {
      // ignore
    }
  }

  /**
   * Sends an in-band 8-byte PING frame on `p2pctrl` and resolves true if the peer echoes
   * the matching PONG before `timeoutMs`, or false on timeout/close/error.
   */
  ping(timeoutMs = 1_500): Promise<boolean> {
    if (this.closed || !this.ctrlDC || this.ctrlDC.readyState !== 'open') {
      return Promise.resolve(false);
    }
    const seqHi = (Date.now() >>> 0);
    this.pingSeq = (this.pingSeq + 1) >>> 0;
    const seqLo = this.pingSeq;
    const key = `${seqHi}:${seqLo}`;
    const payload = new Uint8Array(8);
    const view = new DataView(payload.buffer);
    view.setUint32(0, seqHi, false);
    view.setUint32(4, seqLo, false);

    return new Promise<boolean>((resolve) => {
      const timer = setTimeout(() => {
        this.pingWaiters.delete(key);
        resolve(false);
      }, Math.max(1, timeoutMs));
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

  async openHTTPStream(
    headersTlv: Uint8Array,
    fin = false,
    manualFlowControl = false,
    signal?: AbortSignal
  ): Promise<PoolStream> {
    if (this.closed) throw new Error('mux closed');
    if (headersTlv.byteLength > MAX_TOTAL_HEADERS_SIZE) {
      throw new RangeError('HEADERS exceeds 64 KiB');
    }
    const slot = await this.acquirePoolSlot(signal);
    if (this.closed || slot.closed || !slot.dc || slot.dc.readyState !== 'open') {
      throw new Error('mux or pool slot closed');
    }
    slot.localGen = (slot.localGen + 1) >>> 0;
    if (slot.localGen === 0) slot.localGen = 1;
    if (slot.localGen >= GOAWAY_GEN_THRESHOLD) {
      this.sendGoAway(ErrorCode.GOING_AWAY);
    }
    const stream = new PoolStream(this, slot, slot.localGen, manualFlowControl);
    slot.active = stream;
    this.activeHTTPCount++;
    try {
      await stream.sendHeaders(headersTlv, fin);
    } catch (err) {
      stream.reset(ErrorCode.INTERNAL_ERROR);
      throw err;
    }
    return stream;
  }

  async openWSStream(wsDC: DataChannelLike, headersTlv: Uint8Array): Promise<WSStream> {
    if (this.closed) {
      try {
        wsDC.close();
      } catch {
        // ignore
      }
      throw new Error('mux closed');
    }
    if (headersTlv.byteLength > MAX_TOTAL_HEADERS_SIZE) {
      try {
        wsDC.close();
      } catch {
        // ignore
      }
      throw new RangeError('WS HEADERS exceeds 64 KiB');
    }
    if (this.wsStreams.size >= this.maxWSConns) {
      try {
        wsDC.close();
      } catch {
        // ignore
      }
      throw new Error('max concurrent WebSocket streams reached');
    }
    const stream = new WSStream(this, wsDC);
    this.wsStreams.add(stream);
    try {
      await waitForDataChannelOpen(wsDC);
      if (this.closed || stream.isClosed) {
        throw new Error('WS stream closed before open');
      }
      await stream.sendHeaders(headersTlv);
    } catch (err) {
      stream.reset(ErrorCode.INTERNAL_ERROR);
      throw err;
    }
    return stream;
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    const waiters = this.slotWaiters.splice(0);
    for (const w of waiters) {
      w.cleanup();
      w.reject(new Error('mux closed'));
    }
    for (const [, pw] of this.pingWaiters) {
      clearTimeout(pw.timer);
      pw.resolve(false);
    }
    this.pingWaiters.clear();
    this.idleSlots.length = 0;
    for (const slot of this.poolSlots) {
      slot.closed = true;
      if (slot.active) {
        slot.active._terminateFromMux(ErrorCode.GOING_AWAY);
      }
      try {
        slot.dc?.close();
      } catch {
        // ignore
      }
    }
    for (const ws of Array.from(this.wsStreams)) {
      ws._closeFromMux(ErrorCode.GOING_AWAY);
    }
    try {
      this.ctrlDC?.close();
    } catch {
      // ignore
    }
  }

  /** @internal */
  _tryReserveSessionBytes(n: number): boolean {
    if (this.sessionInboundBytes + n > MAX_SESSION_INBOUND_BYTES) return false;
    this.sessionInboundBytes += n;
    return true;
  }

  /** @internal */
  _releaseSessionBytes(n: number): void {
    this.sessionInboundBytes = Math.max(0, this.sessionInboundBytes - n);
  }

  /** @internal */
  _releasePoolSlot(slot: PoolSlot, stream: PoolStream): void {
    if (slot.active === stream) {
      slot.active = null;
      this.activeHTTPCount = Math.max(0, this.activeHTTPCount - 1);
      if (!this.closed && !slot.closed && slot.dc?.readyState === 'open') {
        this.makeSlotAvailable(slot);
      } else {
        this.rejectWaitersIfAllSlotsDead();
      }
    }
  }

  /** @internal */
  _unregisterWSStream(ws: WSStream): void {
    this.wsStreams.delete(ws);
  }

  private hasAnyUsablePoolSlot(): boolean {
    for (const sl of this.poolSlots) {
      if (!sl.closed && sl.dc && (sl.dc.readyState === 'open' || sl.dc.readyState === 'connecting')) {
        return true;
      }
    }
    return false;
  }

  private rejectWaitersIfAllSlotsDead(): void {
    if (this.slotWaiters.length === 0) return;
    if (!this.hasAnyUsablePoolSlot()) {
      const list = this.slotWaiters.splice(0);
      for (const w of list) {
        w.cleanup();
        w.reject(new Error('all HTTP pool DataChannels closed'));
      }
    }
  }

  private makeSlotAvailable(slot: PoolSlot): void {
    while (this.slotWaiters.length > 0) {
      const waiter = this.slotWaiters.shift()!;
      waiter.cleanup();
      waiter.resolve(slot);
      return;
    }
    if (!this.idleSlots.includes(slot)) {
      this.idleSlots.push(slot);
    }
  }

  private async acquirePoolSlot(signal?: AbortSignal): Promise<PoolSlot> {
    if (signal?.aborted) {
      throw new Error('aborted while acquiring HTTP pool slot');
    }
    while (this.idleSlots.length > 0) {
      const slot = this.idleSlots.shift()!;
      if (!slot.closed && slot.dc?.readyState === 'open' && slot.active === null) {
        return slot;
      }
    }
    if (this.closed) {
      throw new Error('mux closed');
    }
    if (!this.hasAnyUsablePoolSlot()) {
      throw new Error('all HTTP pool DataChannels closed');
    }
    return await new Promise<PoolSlot>((resolve, reject) => {
      let settled = false;
      const removeWaiter = () => {
        const idx = this.slotWaiters.indexOf(waiter);
        if (idx >= 0) this.slotWaiters.splice(idx, 1);
      };
      const onAbort = () => {
        if (settled) return;
        settled = true;
        cleanup();
        removeWaiter();
        reject(new Error('aborted while waiting for HTTP pool slot'));
      };
      const timer = setTimeout(() => {
        if (settled) return;
        settled = true;
        cleanup();
        removeWaiter();
        reject(new Error('timed out waiting for available HTTP pool slot'));
      }, POOL_ACQUIRE_TIMEOUT_MS);
      const cleanup = () => {
        clearTimeout(timer);
        if (signal) signal.removeEventListener('abort', onAbort);
      };
      const waiter: SlotWaiter = {
        resolve: (sl) => {
          if (settled) return;
          settled = true;
          resolve(sl);
        },
        reject: (err) => {
          if (settled) return;
          settled = true;
          reject(err);
        },
        cleanup,
      };
      if (signal) {
        signal.addEventListener('abort', onAbort, { once: true });
      }
      this.slotWaiters.push(waiter);
    });
  }

  private handleControlBytes(raw: Uint8Array): void {
    let f: Frame;
    try {
      f = decodeFrame(raw);
    } catch {
      return;
    }
    if (!isControlFrameType(f.type)) return;
    if (f.type === FrameType.PING) {
      try {
        this.sendControlFrame(FrameType.PONG, f.payload);
      } catch {
        // ignore
      }
      return;
    }
    if (f.type === FrameType.PONG) {
      if (f.payload.byteLength === 8) {
        const view = new DataView(f.payload.buffer, f.payload.byteOffset, 8);
        const key = `${view.getUint32(0, false)}:${view.getUint32(4, false)}`;
        const pw = this.pingWaiters.get(key);
        if (pw) {
          this.pingWaiters.delete(key);
          clearTimeout(pw.timer);
          pw.resolve(true);
        }
      }
      return;
    }
    if (f.type === FrameType.GOAWAY) {
      const code = decodeGoAwayPayload(f.payload);
      this.callbacks.onGoAway?.(code);
      return;
    }
    this.callbacks.onControlFrame?.(f);
  }

  private handlePoolBytes(slot: PoolSlot, raw: Uint8Array): void {
    let f: Frame;
    try {
      f = decodeFrame(raw);
    } catch {
      if (slot.active) {
        slot.active.reset(ErrorCode.PROTOCOL_ERROR);
      }
      return;
    }
    if (!isPoolDataFrameType(f.type)) return;
    const active = slot.active;
    if (!active || active.gen !== f.gen) {
      return; // Stale frame from prior generation: O(1) drop.
    }
    active._handleFrame(f);
  }
}
