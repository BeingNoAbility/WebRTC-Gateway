import {
  HeaderSize,
  RecommendedChunkSize,
  FrameType,
  FrameFlags,
  encodeFrame,
  decodeFrame,
  DecodedFrame,
} from './frame';

export class P2PStream {
  public readonly id: number;
  private mux: P2PMux;
  private readableController?: ReadableStreamDefaultController<Uint8Array>;
  private _readableStream: ReadableStream<Uint8Array>;
  private headersCallbacks: ((headers: any) => void)[] = [];
  private dataCallbacks: ((chunk: Uint8Array, fin: boolean) => void)[] = [];
  public isClosed: boolean = false;
  private readClosed: boolean = false;
  private writeClosed: boolean = false;
  private headersReceived: boolean = false;

  constructor(id: number, mux: P2PMux) {
    this.id = id;
    this.mux = mux;
    this.headersReceived = !mux.getIsClient();

    this._readableStream = new ReadableStream<Uint8Array>({
      start: (controller) => {
        this.readableController = controller;
      },
      cancel: () => {
        this.reset();
      },
    });
  }

  public get readableStream(): ReadableStream<Uint8Array> {
    return this._readableStream;
  }

  public onHeaders(cb: (headers: any) => void) {
    this.headersCallbacks.push(cb);
  }

  public onData(cb: (chunk: Uint8Array, fin: boolean) => void) {
    this.dataCallbacks.push(cb);
  }

  public sendHeaders(headers: any) {
    const jsonStr = typeof headers === 'string' ? headers : JSON.stringify(headers);
    const payload = new TextEncoder().encode(jsonStr);
    const frame = encodeFrame(this.id, FrameType.HEADERS, FrameFlags.NONE, payload);
    this.mux.sendRaw(frame, false);
  }

  public async write(data: Uint8Array): Promise<void> {
    if (this.isClosed) {
      throw new Error(`Stream ${this.id} is closed`);
    }

    let offset = 0;
    while (offset < data.byteLength) {
      const chunkSize = Math.min(RecommendedChunkSize, data.byteLength - offset);
      const chunk = data.subarray(offset, offset + chunkSize);
      const frame = encodeFrame(this.id, FrameType.DATA, FrameFlags.NONE, chunk);
      await this.mux.sendRawAsync(frame, true);
      offset += chunkSize;
    }
  }

  public close() {
    if (this.writeClosed || this.isClosed) return;
    this.writeClosed = true;
    const frame = encodeFrame(this.id, FrameType.DATA, FrameFlags.FIN, null);
    this.mux.sendRaw(frame, true);
    this.checkFullClose();
  }

  public reset() {
    if (this.isClosed) return;
    this.isClosed = true;
    this.readClosed = true;
    this.writeClosed = true;
    const frame = encodeFrame(this.id, FrameType.RST_STREAM, FrameFlags.NONE, null);
    this.mux.sendRaw(frame, false);
    if (this.readableController) {
      try {
        this.readableController.error(new Error('Stream reset by client'));
      } catch {}
    }
    this.mux.removeStream(this.id);
  }

  private checkFullClose() {
    if (this.readClosed && this.writeClosed && this.headersReceived) {
      this.isClosed = true;
      this.mux.removeStream(this.id);
    }
  }

  // Internal dispatchers called by P2PMux
  public _pushHeaders(payload: Uint8Array) {
    this.headersReceived = true;
    let parsed: any = null;
    try {
      const text = new TextDecoder().decode(payload);
      parsed = JSON.parse(text);
    } catch {
      parsed = payload;
    }
    for (const cb of this.headersCallbacks) {
      cb(parsed);
    }
    this.checkFullClose();
  }

  public _pushData(payload: Uint8Array, fin: boolean) {
    if (payload.byteLength > 0 && this.readableController) {
      try {
        this.readableController.enqueue(payload);
      } catch {}
    }
    for (const cb of this.dataCallbacks) {
      cb(payload, fin);
    }

    if (fin) {
      this.readClosed = true;
      if (this.readableController) {
        try {
          this.readableController.close();
        } catch {}
      }
      this.checkFullClose();
    }
  }

  public _pushReset() {
    this.headersReceived = true;
    this.isClosed = true;
    this.readClosed = true;
    this.writeClosed = true;
    if (this.readableController) {
      try {
        this.readableController.error(new Error('Stream reset by peer'));
      } catch {}
    }
    this.mux.removeStream(this.id);
  }
}

export class P2PMux {
  private controlDc: RTCDataChannel;
  private dataDc: RTCDataChannel;
  private isClient: boolean;

  public getIsClient(): boolean {
    return this.isClient;
  }
  private nextStreamId: number;
  private streams: Map<number, P2PStream> = new Map();
  private streamListeners: ((stream: P2PStream) => void)[] = [];
  private pongResolvers: ((rtt: number) => void)[] = [];
  private pingTime: number = 0;

  // Orphan data buffer (DATA / WS_DATA arriving on p2pdata before HEADERS on p2pcontrol)
  private pendingOrphans: Map<number, { frame: DecodedFrame; arrivedAt: number }[]> = new Map();
  private orphanBytesTotal: number = 0;
  private orphanSweepInterval: any = null;

  private static readonly MAX_ORPHAN_BYTES_PER_STREAM = 64 * 1024;
  private static readonly MAX_GLOBAL_ORPHAN_BYTES = 256 * 1024;
  private static readonly ORPHAN_TTL_MS = 500;

  private readonly highWaterMark = 256 * 1024;
  private readonly lowWaterMark = 64 * 1024;
  private sendQueue: Promise<void> = Promise.resolve();

  constructor(controlDc: RTCDataChannel, dataDc?: RTCDataChannel, isClient: boolean = true) {
    this.controlDc = controlDc;
    this.dataDc = dataDc || controlDc;
    this.isClient = isClient;
    this.nextStreamId = isClient ? 1 : 2;

    this.setupChannel(this.controlDc);
    if (this.dataDc !== this.controlDc) {
      this.setupChannel(this.dataDc);
    }

    this.orphanSweepInterval = setInterval(() => {
      this.reapExpiredOrphans();
    }, 100);
  }

  // Getter for legacy/single-channel compatibility
  public get dc(): RTCDataChannel {
    return this.controlDc;
  }

  private setupChannel(dc: RTCDataChannel) {
    dc.binaryType = 'arraybuffer';
    dc.bufferedAmountLowThreshold = this.lowWaterMark;

    dc.onmessage = (event: MessageEvent) => {
      if (event.data instanceof ArrayBuffer) {
        this.handleMessage(new Uint8Array(event.data));
      } else if (event.data instanceof Blob) {
        event.data.arrayBuffer().then((buf) => {
          this.handleMessage(new Uint8Array(buf));
        });
      }
    };
  }

  public openStream(): P2PStream {
    let id = this.nextStreamId;
    this.nextStreamId += 2;
    if (id >= 0xFFFF0000) {
      // Guard against 32-bit stream ID overflow / wrap-around (F-10)
      const base = this.isClient ? 1 : 2;
      id = base;
      while (this.streams.has(id)) {
        id += 2;
        if (id >= 0xFFFF0000) {
          throw new Error('P2PMux: stream ID space exhausted');
        }
      }
      this.nextStreamId = id + 2;
    }
    const stream = new P2PStream(id, this);
    this.streams.set(id, stream);
    return stream;
  }

  public onStream(cb: (stream: P2PStream) => void) {
    this.streamListeners.push(cb);
  }

  public removeStream(id: number) {
    this.streams.delete(id);
  }

  public ping(): Promise<number> {
    return new Promise((resolve, reject) => {
      this.pingTime = performance.now();
      const timer = setTimeout(() => {
        reject(new Error('Ping timeout'));
      }, 5000);

      this.pongResolvers.push((rtt) => {
        clearTimeout(timer);
        resolve(rtt);
      });

      // 8-byte uint64 sequence ID with PING frame
      const payload = new Uint8Array(8);
      const view = new DataView(payload.buffer);
      view.setBigUint64(0, BigInt(Math.floor(performance.now() * 1000)));
      const frame = encodeFrame(0, FrameType.PING, FrameFlags.NONE, payload);
      this.sendRaw(frame, false);
    });
  }

  public sendRaw(data: Uint8Array, useDataChannel: boolean = false) {
    const dc = useDataChannel ? this.dataDc : this.controlDc;
    if (dc.readyState !== 'open') {
      throw new Error(`DataChannel not open (readyState: ${dc.readyState})`);
    }
    dc.send(data as unknown as ArrayBufferView<ArrayBuffer>);
  }

  public async sendRawAsync(data: Uint8Array, useDataChannel: boolean = true): Promise<void> {
    const dc = useDataChannel ? this.dataDc : this.controlDc;
    if (dc.readyState !== 'open') {
      throw new Error(`DataChannel not open (readyState: ${dc.readyState})`);
    }

    const task = async () => {
      if (dc.readyState !== 'open') {
        throw new Error(`DataChannel not open (readyState: ${dc.readyState})`);
      }

      while (dc.bufferedAmount > this.highWaterMark) {
        await new Promise<void>((resolve) => {
          let timer: any = null;
          const onLow = () => {
            if (timer) clearTimeout(timer);
            dc.removeEventListener('bufferedamountlow', onLow);
            resolve();
          };
          dc.addEventListener('bufferedamountlow', onLow);
          timer = setTimeout(() => {
            dc.removeEventListener('bufferedamountlow', onLow);
            resolve();
          }, 50);
        });
      }

      dc.send(data as unknown as ArrayBufferView<ArrayBuffer>);
    };

    this.sendQueue = this.sendQueue.then(task, task);
    await this.sendQueue;
  }

  private handleMessage(data: Uint8Array) {
    try {
      const frame = decodeFrame(data);

      // Stream ID 0 is control stream
      if (frame.streamId === 0) {
        this.handleControlFrame(frame);
        return;
      }

      let stream = this.streams.get(frame.streamId);
      if (!stream) {
        // Strict stream ID parity check:
        // Client only accepts even stream IDs (0, 2, 4...) initiated by Agent peer.
        const isExpectedPeerId = this.isClient ? frame.streamId % 2 === 0 : frame.streamId % 2 === 1;
        if (!isExpectedPeerId) return;

        // If orphan DATA frame arrives on dual-channel before HEADERS
        if (this.dataDc !== this.controlDc && (frame.type === FrameType.DATA || frame.type === FrameType.WS_DATA)) {
          this.bufferOrphanFrame(frame);
          return;
        }

        if (frame.type === FrameType.RST_STREAM) {
          this.dropOrphans(frame.streamId);
          return;
        }

        if (
          frame.type === FrameType.HEADERS ||
          frame.type === FrameType.DATA ||
          frame.type === FrameType.WS_CONNECT
        ) {
          // New inbound stream from peer
          stream = new P2PStream(frame.streamId, this);
          this.streams.set(frame.streamId, stream);
          for (const cb of this.streamListeners) {
            cb(stream);
          }

          if (frame.type === FrameType.HEADERS) {
            stream._pushHeaders(frame.payload);
          } else if (frame.type === FrameType.DATA || frame.type === FrameType.WS_CONNECT) {
            const fin = (frame.flags & FrameFlags.FIN) !== 0;
            stream._pushData(frame.payload, fin);
          }

          // Flush any buffered orphan frames
          const orphans = this.takeOrphanFrames(frame.streamId);
          if (orphans) {
            for (const o of orphans) {
              const fin = (o.frame.flags & FrameFlags.FIN) !== 0;
              stream._pushData(o.frame.payload, fin);
            }
          }
          return;
        } else {
          return;
        }
      }

      switch (frame.type) {
        case FrameType.HEADERS:
          stream._pushHeaders(frame.payload);
          break;
        case FrameType.DATA:
        case FrameType.WS_DATA:
          const fin = (frame.flags & FrameFlags.FIN) !== 0;
          stream._pushData(frame.payload, fin);
          break;
        case FrameType.RST_STREAM:
          stream._pushReset();
          break;
      }
    } catch (err) {
      console.error('[P2PMux] Failed to decode incoming frame:', err);
    }
  }

  private bufferOrphanFrame(frame: DecodedFrame) {
    const payloadLen = frame.payload.byteLength;
    if (this.orphanBytesTotal + payloadLen > P2PMux.MAX_GLOBAL_ORPHAN_BYTES) {
      return;
    }
    const streamOrphans = this.pendingOrphans.get(frame.streamId) || [];
    let streamBytes = 0;
    for (const o of streamOrphans) {
      streamBytes += o.frame.payload.byteLength;
    }
    if (streamBytes + payloadLen > P2PMux.MAX_ORPHAN_BYTES_PER_STREAM) {
      return;
    }

    streamOrphans.push({ frame, arrivedAt: Date.now() });
    this.pendingOrphans.set(frame.streamId, streamOrphans);
    this.orphanBytesTotal += payloadLen;
  }

  private takeOrphanFrames(streamId: number): { frame: DecodedFrame; arrivedAt: number }[] | undefined {
    const orphans = this.pendingOrphans.get(streamId);
    if (!orphans) return undefined;
    this.pendingOrphans.delete(streamId);
    for (const o of orphans) {
      this.orphanBytesTotal -= o.frame.payload.byteLength;
    }
    if (this.orphanBytesTotal < 0) this.orphanBytesTotal = 0;
    return orphans;
  }

  private dropOrphans(streamId: number) {
    this.takeOrphanFrames(streamId);
  }

  private reapExpiredOrphans() {
    if (this.pendingOrphans.size === 0) return;
    const now = Date.now();
    const expired: number[] = [];
    for (const [streamId, orphans] of this.pendingOrphans.entries()) {
      if (orphans.length > 0 && now - orphans[0].arrivedAt > P2PMux.ORPHAN_TTL_MS) {
        expired.push(streamId);
      }
    }
    for (const sid of expired) {
      this.dropOrphans(sid);
      try {
        const rst = encodeFrame(sid, FrameType.RST_STREAM, FrameFlags.NONE, null);
        this.sendRaw(rst, false);
      } catch {}
    }
  }

  private handleControlFrame(frame: DecodedFrame) {
    switch (frame.type) {
      case FrameType.PING:
        // Echo pong back on control channel
        const pong = encodeFrame(0, FrameType.PONG, FrameFlags.NONE, frame.payload);
        this.sendRaw(pong, false);
        break;
      case FrameType.PONG:
        const rtt = performance.now() - this.pingTime;
        while (this.pongResolvers.length > 0) {
          const fn = this.pongResolvers.shift();
          if (fn) fn(rtt);
        }
        break;
    }
  }

  public destroy() {
    if (this.orphanSweepInterval) {
      clearInterval(this.orphanSweepInterval);
      this.orphanSweepInterval = null;
    }
    this.pendingOrphans.clear();
    this.orphanBytesTotal = 0;
    for (const s of this.streams.values()) {
      s.reset();
    }
    this.streams.clear();
  }
}
