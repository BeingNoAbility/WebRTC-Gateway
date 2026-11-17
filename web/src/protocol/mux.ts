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

  constructor(id: number, mux: P2PMux) {
    this.id = id;
    this.mux = mux;

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
    this.mux.sendRaw(frame);
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
      await this.mux.sendRawAsync(frame);
      offset += chunkSize;
    }
  }

  public close() {
    if (this.writeClosed || this.isClosed) return;
    this.writeClosed = true;
    const frame = encodeFrame(this.id, FrameType.DATA, FrameFlags.FIN, null);
    this.mux.sendRaw(frame);
    this.checkFullClose();
  }

  public reset() {
    if (this.isClosed) return;
    this.isClosed = true;
    this.readClosed = true;
    this.writeClosed = true;
    const frame = encodeFrame(this.id, FrameType.RST_STREAM, FrameFlags.NONE, null);
    this.mux.sendRaw(frame);
    if (this.readableController) {
      try {
        this.readableController.error(new Error('Stream reset by client'));
      } catch {}
    }
    this.mux.removeStream(this.id);
  }

  private checkFullClose() {
    if (this.readClosed && this.writeClosed) {
      this.isClosed = true;
      this.mux.removeStream(this.id);
    }
  }

  // Internal dispatchers called by P2PMux
  public _pushHeaders(payload: Uint8Array) {
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
  private dc: RTCDataChannel;
  private isClient: boolean;
  private nextStreamId: number;
  private streams: Map<number, P2PStream> = new Map();
  private streamListeners: ((stream: P2PStream) => void)[] = [];
  private pongResolvers: ((rtt: number) => void)[] = [];
  private pingTime: number = 0;

  private readonly highWaterMark = 256 * 1024;
  private readonly lowWaterMark = 64 * 1024;
  private sendQueue: Promise<void> = Promise.resolve();

  constructor(dc: RTCDataChannel, isClient: boolean = true) {
    this.dc = dc;
    this.isClient = isClient;
    this.nextStreamId = isClient ? 1 : 2;

    this.dc.binaryType = 'arraybuffer';
    this.dc.bufferedAmountLowThreshold = this.lowWaterMark;

    this.dc.onmessage = (event: MessageEvent) => {
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

      const frame = encodeFrame(0, FrameType.PING, FrameFlags.NONE, null);
      this.sendRaw(frame);
    });
  }

  public sendRaw(data: Uint8Array) {
    if (this.dc.readyState !== 'open') {
      throw new Error(`DataChannel not open (readyState: ${this.dc.readyState})`);
    }
    this.dc.send(data as unknown as ArrayBufferView<ArrayBuffer>);
  }

  public async sendRawAsync(data: Uint8Array): Promise<void> {
    if (this.dc.readyState !== 'open') {
      throw new Error(`DataChannel not open (readyState: ${this.dc.readyState})`);
    }

    const task = async () => {
      if (this.dc.readyState !== 'open') {
        throw new Error(`DataChannel not open (readyState: ${this.dc.readyState})`);
      }

      while (this.dc.bufferedAmount > this.highWaterMark) {
        await new Promise<void>((resolve) => {
          let timer: any = null;
          const onLow = () => {
            if (timer) clearTimeout(timer);
            this.dc.removeEventListener('bufferedamountlow', onLow);
            resolve();
          };
          this.dc.addEventListener('bufferedamountlow', onLow);
          timer = setTimeout(() => {
            this.dc.removeEventListener('bufferedamountlow', onLow);
            resolve();
          }, 50);
        });
      }

      this.dc.send(data as unknown as ArrayBufferView<ArrayBuffer>);
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
        // Drops stray orphan frames of client-initiated odd streams (1, 3, 5...) that were already closed locally.
        const isExpectedPeerId = this.isClient ? frame.streamId % 2 === 0 : frame.streamId % 2 === 1;
        if (!isExpectedPeerId) return;

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

  private handleControlFrame(frame: DecodedFrame) {
    switch (frame.type) {
      case FrameType.PING:
        // Echo pong back
        const pong = encodeFrame(0, FrameType.PONG, FrameFlags.NONE, frame.payload);
        this.sendRaw(pong);
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
}
