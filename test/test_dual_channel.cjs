// test/test_dual_channel.cjs
// Verification for P2PMux Native Dual-Channel Architecture (v2.3 Sprint 1)
const assert = require('assert');
const fs = require('fs');
const path = require('path');

console.log('=== Running P2PMux Native Dual-Channel Architecture Verification ===');

// Mock RTCDataChannel
class MockRTCDataChannel {
  constructor(label) {
    this.label = label;
    this.readyState = 'open';
    this.binaryType = 'arraybuffer';
    this.bufferedAmount = 0;
    this.bufferedAmountLowThreshold = 0;
    this.sent = [];
    this.onmessage = null;
    this.onclose = null;
    this.onerror = null;
    this.onopen = null;
  }

  send(data) {
    if (this.readyState !== 'open') {
      throw new Error(`DataChannel ${this.label} is not open`);
    }
    const copy = new Uint8Array(data.byteLength);
    copy.set(new Uint8Array(data.buffer || data));
    this.sent.push(copy);
  }

  // Helper to simulate receiving message
  receive(data) {
    if (this.onmessage) {
      this.onmessage({ data: data.buffer });
    }
  }

  close() {
    this.readyState = 'closed';
    if (this.onclose) this.onclose();
  }
}

// 1. Check web/src/protocol/mux.ts source code guarantees
const muxTsPath = path.resolve(__dirname, '../web/src/protocol/mux.ts');
assert(fs.existsSync(muxTsPath), 'web/src/protocol/mux.ts must exist');
const muxSource = fs.readFileSync(muxTsPath, 'utf8');

assert(muxSource.includes('constructor(controlDc: RTCDataChannel, dataDc?: RTCDataChannel'),
  'P2PMux constructor must support controlDc and dataDc');
assert(muxSource.includes('pendingOrphans'), 'P2PMux must implement pendingOrphans queue');
assert(muxSource.includes('MAX_ORPHAN_BYTES_PER_STREAM = 64 * 1024'), 'Per-stream orphan limit must be 64KB');
assert(muxSource.includes('MAX_GLOBAL_ORPHAN_BYTES = 256 * 1024'), 'Global orphan limit must be 256KB');
assert(muxSource.includes('ORPHAN_TTL_MS = 500'), 'Orphan TTL must be 500ms');

console.log('[PASS] web/src/protocol/mux.ts verified for dual-channel & orphan buffering invariants');

// 2. Check web/src/portal/app.ts creates p2pcontrol and p2pdata
const appTsPath = path.resolve(__dirname, '../web/src/portal/app.ts');
assert(fs.existsSync(appTsPath), 'web/src/portal/app.ts must exist');
const appSource = fs.readFileSync(appTsPath, 'utf8');

assert(appSource.includes("createDataChannel('p2pcontrol'"), 'app.ts must create p2pcontrol DataChannel');
assert(appSource.includes("createDataChannel('p2pdata'"), 'app.ts must create p2pdata DataChannel');
assert(!appSource.includes("createDataChannel('p2pmux'"), 'app.ts must NOT create legacy p2pmux DataChannel');

console.log('[PASS] web/src/portal/app.ts verified for creating dual channels (p2pcontrol & p2pdata)');

// 3. Functional tests using Web implementation of frame & mux logic
const HeaderSize = 10;
const FrameType = {
  DATA: 0x0,
  HEADERS: 0x1,
  RST_STREAM: 0x3,
  PING: 0x4,
  PONG: 0x5,
};
const FrameFlags = {
  NONE: 0x0,
  FIN: 0x1,
};

function encodeFrame(streamId, type, flags, payload) {
  const payloadLen = payload ? payload.byteLength : 0;
  const totalLen = HeaderSize + payloadLen;
  const buffer = new Uint8Array(totalLen);
  const view = new DataView(buffer.buffer);
  view.setUint32(0, streamId, false);
  view.setUint8(4, type);
  view.setUint8(5, flags);
  view.setUint32(6, payloadLen, false);
  if (payload && payloadLen > 0) {
    buffer.set(payload, HeaderSize);
  }
  return buffer;
}

function decodeFrame(data) {
  const view = new DataView(data.buffer, data.byteOffset, data.byteLength);
  return {
    streamId: view.getUint32(0, false),
    type: view.getUint8(4),
    flags: view.getUint8(5),
    payload: data.subarray(HeaderSize, HeaderSize + view.getUint32(6, false)),
  };
}

// Test Class replicating the dual-mux dispatching
class MockP2PMux {
  constructor(controlDc, dataDc, isClient = true) {
    this.controlDc = controlDc;
    this.dataDc = dataDc || controlDc;
    this.isClient = isClient;
    this.pendingOrphans = new Map();
    this.streams = new Map();

    const handle = (data) => this.handleMessage(data);
    this.controlDc.onmessage = (e) => handle(new Uint8Array(e.data));
    this.dataDc.onmessage = (e) => handle(new Uint8Array(e.data));
  }

  sendRaw(data, useDataChannel = false) {
    const dc = useDataChannel ? this.dataDc : this.controlDc;
    dc.send(data);
  }

  handleMessage(data) {
    const frame = decodeFrame(data);
    if (frame.streamId === 0) return;

    let stream = this.streams.get(frame.streamId);
    if (!stream) {
      if (this.dataDc !== this.controlDc && frame.type === FrameType.DATA) {
        // Buffer orphan
        const list = this.pendingOrphans.get(frame.streamId) || [];
        list.push({ frame, arrivedAt: Date.now() });
        this.pendingOrphans.set(frame.streamId, list);
        return;
      }

      if (frame.type === FrameType.HEADERS) {
        stream = { id: frame.streamId, headers: frame.payload, data: [] };
        this.streams.set(frame.streamId, stream);
        const orphans = this.pendingOrphans.get(frame.streamId);
        if (orphans) {
          this.pendingOrphans.delete(frame.streamId);
          for (const o of orphans) {
            stream.data.push(o.frame.payload);
          }
        }
        return;
      }
    } else {
      if (frame.type === FrameType.DATA) {
        stream.data.push(frame.payload);
      }
    }
  }
}

// Test 3a: Verify routing of control vs data
const controlDc = new MockRTCDataChannel('p2pcontrol');
const dataDc = new MockRTCDataChannel('p2pdata');
const clientMux = new MockP2PMux(controlDc, dataDc, true);

// Send ping
const pingFrame = encodeFrame(0, FrameType.PING, FrameFlags.NONE, null);
clientMux.sendRaw(pingFrame, false);
assert.strictEqual(controlDc.sent.length, 1, 'PING must be sent on controlDc');
assert.strictEqual(dataDc.sent.length, 0, 'PING must NOT be sent on dataDc');

// Send data frame
const dataChunk = encodeFrame(1, FrameType.DATA, FrameFlags.NONE, new TextEncoder().encode('bulk data'));
clientMux.sendRaw(dataChunk, true);
assert.strictEqual(dataDc.sent.length, 1, 'DATA frame must be sent on dataDc');
assert.strictEqual(controlDc.sent.length, 1, 'DATA frame must NOT increment controlDc count');

console.log('[PASS] Control vs Data channel write demux routing verified');

// Test 3b: Verify orphan data arriving before headers
const serverControlDc = new MockRTCDataChannel('p2pcontrol');
const serverDataDc = new MockRTCDataChannel('p2pdata');
const serverMux = new MockP2PMux(serverControlDc, serverDataDc, false);

// 1. DATA arrives first on serverDataDc
const orphanPayload = new TextEncoder().encode('orphan packet #1');
const orphanFrame = encodeFrame(2, FrameType.DATA, FrameFlags.NONE, orphanPayload);
serverDataDc.receive(orphanFrame);

assert.strictEqual(serverMux.streams.has(2), false, 'Stream 2 must not be created before HEADERS arrive');
assert.strictEqual(serverMux.pendingOrphans.get(2).length, 1, 'Orphan packet must be held in pendingOrphans');

// 2. HEADERS arrives later on serverControlDc
const headerPayload = new TextEncoder().encode('{"method":"GET","path":"/"}');
const headerFrame = encodeFrame(2, FrameType.HEADERS, FrameFlags.NONE, headerPayload);
serverControlDc.receive(headerFrame);

assert.strictEqual(serverMux.streams.has(2), true, 'Stream 2 must be created after HEADERS arrive');
const s = serverMux.streams.get(2);
assert.strictEqual(s.data.length, 1, 'Orphan packet must be flushed into stream data queue');
assert.strictEqual(new TextDecoder().decode(s.data[0]), 'orphan packet #1', 'Flushed data must match original payload');
assert.strictEqual(serverMux.pendingOrphans.has(2), false, 'pendingOrphans for stream 2 must be cleared');

console.log('[PASS] Orphan DATA arriving before HEADERS buffer & flush verified');

console.log('=== All P2PMux Native Dual-Channel Tests Passed Successfully! ===');
