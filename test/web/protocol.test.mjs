import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { loadTsModule, repoRoot } from './_loader.mjs';

const loadTsBundle = (entryRel) => loadTsModule(entryRel, { platform: 'node' });

function hexToBytes(hex) {
  if (!hex) return new Uint8Array(0);
  return new Uint8Array(Buffer.from(hex, 'hex'));
}

function bytesToHex(bytes) {
  return Buffer.from(bytes.buffer, bytes.byteOffset, bytes.byteLength).toString('hex');
}

function makeMockChannelPair() {
  const make = () => ({
    readyState: 'open',
    bufferedAmount: 0,
    bufferedAmountLowThreshold: 0,
    binaryType: 'arraybuffer',
    peer: null,
    sent: [],
    onmessage: null,
    onopen: null,
    onclose: null,
    onerror: null,
    onbufferedamountlow: null,
    send(data) {
      if (this.readyState !== 'open') throw new Error('DC closed');
      const cp = new Uint8Array(data).slice();
      this.sent.push(cp);
      const peer = this.peer;
      if (peer && peer.readyState === 'open') {
        queueMicrotask(() => {
          if (peer.readyState === 'open') {
            peer.onmessage?.({ data: cp.buffer });
          }
        });
      }
    },
    close() {
      if (this.readyState === 'closed') return;
      this.readyState = 'closed';
      this.onclose?.({});
    },
  });
  const a = make();
  const b = make();
  a.peer = b;
  b.peer = a;
  return [a, b];
}

test('P2PMux V8 Golden Vectors (TS codec parity with Go + H-6 BOM preservation)', async () => {
  const frameMod = await loadTsBundle('web/src/protocol/frame.ts');
  const tlvMod = await loadTsBundle('web/src/protocol/tlv.ts');
  const golden = JSON.parse(
    fs.readFileSync(path.join(repoRoot, 'test/fixtures/protocol_v8_golden_vectors.json'), 'utf8')
  );
  assert.equal(golden.version, 8);

  for (const tc of golden.valid_frames) {
    const payload = hexToBytes(tc.payload_hex);
    const wire = frameMod.encodeFrame(tc.stream_id, tc.type, tc.flags, payload);
    assert.equal(bytesToHex(wire), tc.wire_hex, `encode mismatch: ${tc.name}`);
    const dec = frameMod.decodeFrame(wire);
    assert.equal(dec.streamId, tc.stream_id, tc.name);
    assert.equal(dec.type, tc.type, tc.name);
    assert.equal(dec.flags, tc.flags, tc.name);
    assert.equal(bytesToHex(dec.payload), tc.payload_hex, tc.name);
  }

  for (const tc of golden.invalid_frames) {
    assert.throws(() => frameMod.decodeFrame(hexToBytes(tc.wire_hex)), `expected throw: ${tc.name}`);
  }

  for (const tc of golden.tlv_requests) {
    const cookieMutations = tc.cookie_mutations.map((c) => ({
      op: c.op,
      name: c.name,
      value: c.value,
      path: c.path,
      expiresMs: c.expires_ms,
    }));
    const enc = tlvMod.encodeRequestHeaders({
      kind: tc.kind,
      fetchCtx: tc.fetch_ctx,
      method: tc.method,
      target: tc.target,
      headers: tc.headers,
      cookieMutations,
    });
    assert.equal(bytesToHex(enc), tc.hex, `tlv req encode: ${tc.name}`);
    const dec = tlvMod.decodeRequestHeaders(enc);
    assert.deepEqual(dec, {
      kind: tc.kind,
      fetchCtx: tc.fetch_ctx,
      method: tc.method,
      target: tc.target,
      headers: tc.headers,
      cookieMutations,
    });
  }

  for (const tc of golden.tlv_responses) {
    const cookieDeltas = tc.cookie_deltas.map((c) => ({
      op: c.op,
      name: c.name,
      value: c.value,
      path: c.path,
      expiresMs: c.expires_ms,
    }));
    const enc = tlvMod.encodeResponseHeaders({
      status: tc.status,
      respFlags: tc.resp_flags,
      headers: tc.headers,
      cookieDeltas,
    });
    assert.equal(bytesToHex(enc), tc.hex, `tlv resp encode: ${tc.name}`);
    const dec = tlvMod.decodeResponseHeaders(enc);
    assert.deepEqual(dec, {
      status: tc.status,
      respFlags: tc.resp_flags,
      headers: tc.headers,
      cookieDeltas,
    });
  }

  for (const tc of golden.invalid_tlv_requests || []) {
    assert.throws(() => tlvMod.decodeRequestHeaders(hexToBytes(tc.hex)), `expected invalid TLV req throw: ${tc.name}`);
  }
  for (const tc of golden.invalid_tlv_responses || []) {
    assert.throws(() => tlvMod.decodeResponseHeaders(hexToBytes(tc.hex)), `expected invalid TLV resp throw: ${tc.name}`);
  }

  // H-6b regression: TextDecoder with ignoreBOM: true MUST preserve U+FEFF (never strip to empty cookie name).
  const bomResp = tlvMod.encodeResponseHeaders({
    status: 200,
    respFlags: 0,
    headers: [],
    cookieDeltas: [{ op: tlvMod.CookieOp.SET, name: '\uFEFF', value: 'v', path: '/', expiresMs: 0 }],
  });
  const decBom = tlvMod.decodeResponseHeaders(bomResp);
  assert.equal(decBom.cookieDeltas[0].name, '\uFEFF', 'H-6b regression: BOM was stripped from cookie name');

  // Strict RFC 4648 §3.5 canonical base64url rejects non-zero trailing unused bits.
  const cryptoMod = await loadTsBundle('web/src/shared/crypto.ts');
  assert.throws(() => cryptoMod.base64UrlDecode('AB'), /non-canonical|padding|trailing/i);
  assert.throws(() => cryptoMod.base64UrlDecode('AAB'), /non-canonical|padding|trailing/i);
  assert.deepEqual(Array.from(cryptoMod.base64UrlDecode('AA')), [0]);
});

test('PoolMux V8 TS: monotonic multiplexing, C-5 10k x 13B SSE, H-5 WSStream fixes, chunked control & ping', async () => {
  const muxMod = await loadTsBundle('web/src/protocol/mux.ts');
  const tlvMod = await loadTsBundle('web/src/protocol/tlv.ts');
  const frameMod = await loadTsBundle('web/src/protocol/frame.ts');

  const [cCtrl, sCtrl] = makeMockChannelPair();
  const poolSize = 4;
  const cLanes = [];
  const sLanes = [];
  for (let i = 0; i < poolSize; i++) {
    const [a, b] = makeMockChannelPair();
    cLanes.push(a);
    sLanes.push(b);
  }

  const ctrlFrames = [];
  const goAways = [];
  const clientMux = new muxMod.PoolMux(poolSize, {
    onControlFrame: (f) => ctrlFrames.push(f),
    onGoAway: (code) => goAways.push(code),
  });
  clientMux.attachControlDC(cCtrl);
  for (let i = 0; i < poolSize; i++) {
    clientMux.attachLaneDC(i, cLanes[i]);
  }

  // Downward-only MMS clamping & floor at 4096.
  clientMux.setMaxMessageSize(1024);
  assert.equal(clientMux.chunkSize, frameMod.DEFAULT_CHUNK_SIZE);
  clientMux.setMaxMessageSize(12_000);
  assert.equal(clientMux.chunkSize, 12_000 - frameMod.HEADER_SIZE);
  clientMux.setMaxMessageSize(65_536);
  assert.equal(clientMux.chunkSize, 12_000 - frameMod.HEADER_SIZE);

  // Server responder on sLanes handling HTTP echo, 10k x 13B SSE, WS fast M1/M2/CLOSE, WS >16MiB, and WS 2.5MiB echo.
  const wsAsms = new Map();
  for (let i = 0; i < poolSize; i++) {
    const sdc = sLanes[i];
    sdc.onmessage = (ev) => {
      const f = frameMod.decodeFrame(new Uint8Array(ev.data));
      if (f.type === frameMod.FrameType.HEADERS) {
        const req = tlvMod.decodeRequestHeaders(f.payload);
        if (req.target === '/echo') {
          const respTlv = tlvMod.encodeResponseHeaders({ status: 200, headers: [] });
          sdc.send(frameMod.encodeFrame(f.streamId, frameMod.FrameType.HEADERS, frameMod.FrameFlag.NONE, respTlv));
          sdc.send(
            frameMod.encodeFrame(
              f.streamId,
              frameMod.FrameType.DATA,
              frameMod.FrameFlag.FIN,
              new TextEncoder().encode(`ok:${f.streamId}`)
            )
          );
        } else if (req.target === '/sse-tiny') {
          const respTlv = tlvMod.encodeResponseHeaders({ status: 200, headers: [] });
          sdc.send(frameMod.encodeFrame(f.streamId, frameMod.FrameType.HEADERS, frameMod.FrameFlag.NONE, respTlv));
          const token = new TextEncoder().encode('data: token\n\n'); // 13 bytes
          for (let k = 0; k < 10_000; k++) {
            const flags = k === 9_999 ? frameMod.FrameFlag.FIN : frameMod.FrameFlag.NONE;
            sdc.send(frameMod.encodeFrame(f.streamId, frameMod.FrameType.DATA, flags, token));
          }
        } else if (req.target === '/ws-fast-close') {
          const respTlv = tlvMod.encodeResponseHeaders({ status: 101, headers: [] });
          sdc.send(frameMod.encodeFrame(f.streamId, frameMod.FrameType.HEADERS, frameMod.FrameFlag.NONE, respTlv));
          sdc.send(frameMod.encodeFrame(f.streamId, frameMod.FrameType.WS_TEXT, frameMod.FrameFlag.MSG_FIN, new TextEncoder().encode('M1')));
          sdc.send(frameMod.encodeFrame(f.streamId, frameMod.FrameType.WS_TEXT, frameMod.FrameFlag.MSG_FIN, new TextEncoder().encode('M2')));
          sdc.send(frameMod.encodeFrame(f.streamId, frameMod.FrameType.WS_CLOSE, frameMod.FrameFlag.NONE, frameMod.encodeWSClosePayload(1000, 'bye')));
        } else if (req.target === '/ws-echo') {
          const respTlv = tlvMod.encodeResponseHeaders({ status: 101, headers: [] });
          sdc.send(frameMod.encodeFrame(f.streamId, frameMod.FrameType.HEADERS, frameMod.FrameFlag.NONE, respTlv));
        } else if (req.target === '/ws-over16m') {
          const respTlv = tlvMod.encodeResponseHeaders({ status: 101, headers: [] });
          sdc.send(frameMod.encodeFrame(f.streamId, frameMod.FrameType.HEADERS, frameMod.FrameFlag.NONE, respTlv));
          const bigChunk = new Uint8Array(32 * 1024);
          // 513 * 32 KiB > 16 MiB
          for (let k = 0; k < 513; k++) {
            sdc.send(frameMod.encodeFrame(f.streamId, frameMod.FrameType.WS_BINARY, frameMod.FrameFlag.NONE, bigChunk));
          }
        }
      } else if (f.type === frameMod.FrameType.WS_BINARY || f.type === frameMod.FrameType.WS_TEXT) {
        let entry = wsAsms.get(f.streamId);
        if (!entry) {
          entry = { op: f.type, chunks: [], bytes: 0 };
          wsAsms.set(f.streamId, entry);
        }
        entry.chunks.push(f.payload);
        entry.bytes += f.payload.byteLength;
        if (entry.bytes >= 128 * 1024) {
          sdc.send(frameMod.encodeFrame(f.streamId, frameMod.FrameType.CREDIT, frameMod.FrameFlag.NONE, frameMod.encodeCreditPayload(entry.bytes)));
          entry.bytes = 0;
        }
        if ((f.flags & frameMod.FrameFlag.MSG_FIN) !== 0) {
          const full = Buffer.concat(entry.chunks);
          wsAsms.delete(f.streamId);
          for (let off = 0; off < full.byteLength; off += 16384) {
            const end = Math.min(off + 16384, full.byteLength);
            const fin = end === full.byteLength ? frameMod.FrameFlag.MSG_FIN : frameMod.FrameFlag.NONE;
            sdc.send(frameMod.encodeFrame(f.streamId, f.type, fin, new Uint8Array(full.subarray(off, end))));
          }
        }
      }
    };
  }

  // 1. 40 concurrent multiplexed streams over 4 lanes (10 streams/lane concurrently, zero blocking!).
  const echoResults = await Promise.all(
    Array.from({ length: 40 }, async () => {
      const req = tlvMod.encodeRequestHeaders({ kind: tlvMod.StreamKind.HTTP, method: 'GET', target: '/echo', headers: [] });
      const st = await clientMux.openHTTPStream(req, false);
      await st.waitResponseHeaders();
      return await new Promise((resolve, reject) => {
        const chunks = [];
        st.onData((c) => {
          chunks.push(c);
          st.ackConsumed(c.byteLength);
        });
        st.onEnd(() => resolve(new TextDecoder().decode(Buffer.concat(chunks))));
        st.onReset(reject);
      });
    })
  );
  assert.equal(echoResults.length, 40);
  assert.equal(clientMux.activeStreamCount, 0);
  assert.equal(clientMux.sessionMemUsed, 0);

  // 2. C-5 Regression: 10,000 x 13-byte SSE chunks MUST complete cleanly with pure-payload CREDIT!
  const sseReq = tlvMod.encodeRequestHeaders({ kind: tlvMod.StreamKind.HTTP, method: 'GET', target: '/sse-tiny', headers: [] });
  const sseStream = await clientMux.openHTTPStream(sseReq, false);
  await sseStream.waitResponseHeaders();
  let sseBytes = 0;
  await new Promise((resolve, reject) => {
    sseStream.onData((c) => {
      sseBytes += c.byteLength;
      sseStream.ackConsumed(c.byteLength, 1);
    });
    sseStream.onEnd(resolve);
    sseStream.onReset(reject);
  });
  assert.equal(sseBytes, 10_000 * 13, 'C-5 regression: 10,000 x 13B SSE stream lost bytes or reset');
  assert.equal(clientMux.sessionMemUsed, 0);

  // 3. H-5a Regression: WSStream receiving fast M1, M2, WS_CLOSE before onMessage() MUST deliver BOTH M1 and M2!
  const wsFastReq = tlvMod.encodeRequestHeaders({ kind: tlvMod.StreamKind.WS, method: 'GET', target: '/ws-fast-close', headers: [] });
  const wsFast = await clientMux.openWSStream(wsFastReq);
  await wsFast.waitResponseHeaders();
  await new Promise((r) => setTimeout(r, 20)); // Let M1, M2, WS_CLOSE arrive before attaching handlers!
  const gotFastMsgs = [];
  const fastClosed = await new Promise((resolve) => {
    wsFast.onClose((code, reason, wasClean) => resolve({ code, reason, wasClean }));
    wsFast.onMessage((m) => gotFastMsgs.push(m.data));
  });
  assert.deepEqual(gotFastMsgs, ['M1', 'M2'], 'H-5a regression: WSStream dropped M2 after fast WS_CLOSE');
  assert.deepEqual(fastClosed, { code: 1000, reason: 'bye', wasClean: true });
  assert.equal(clientMux.sessionMemUsed, 0);

  // 4. H-5b Regression: WSStream reset() BEFORE onMessage() after fast M1, M2, WS_CLOSE MUST free sessionMemUsed to 0!
  const wsLeak = await clientMux.openWSStream(wsFastReq);
  await wsLeak.waitResponseHeaders();
  await new Promise((r) => setTimeout(r, 20));
  assert.ok(clientMux.sessionMemUsed > 0, 'expected buffered pending WS messages in sessionMemUsed');
  wsLeak.reset(frameMod.ErrorCode.CANCEL);
  assert.equal(clientMux.sessionMemUsed, 0, 'H-5b regression: WSStream.reset() leaked sessionMemUsed');

  // 5. H-5c Regression: >16 MiB WS message cleans up sessionMemUsed even if localClose already sent!
  const wsOverReq = tlvMod.encodeRequestHeaders({ kind: tlvMod.StreamKind.WS, method: 'GET', target: '/ws-over16m', headers: [] });
  const wsOver = await clientMux.openWSStream(wsOverReq);
  await wsOver.waitResponseHeaders();
  wsOver.close(1000, 'local-first');
  await new Promise((r) => setTimeout(r, 30));
  assert.equal(clientMux.sessionMemUsed, 0, 'H-5c regression: >16 MiB WS message leaked sessionMemUsed');

  // 6. 2.5 MiB binary WS echo over multiplexed lane.
  const wsEchoReq = tlvMod.encodeRequestHeaders({ kind: tlvMod.StreamKind.WS, method: 'GET', target: '/ws-echo', headers: [] });
  const wsEcho = await clientMux.openWSStream(wsEchoReq);
  await wsEcho.waitResponseHeaders();
  const bigBin = new Uint8Array(2.5 * 1024 * 1024);
  for (let i = 0; i < bigBin.byteLength; i++) bigBin[i] = (i * 31) & 0xff;
  const echoPromise = new Promise((resolve) => wsEcho.onMessage((m) => resolve(m)));
  await wsEcho.sendMessage(bigBin);
  const echoed = await echoPromise;
  assert.equal(echoed.isBinary, true);
  assert.equal(echoed.data.byteLength, bigBin.byteLength);
  wsEcho.close(1000, 'done');

  // 7. Chunked 90 KiB control JSON reassembly over 12 KiB MMS + PING/PONG + GOAWAY.
  const bigCtrl = new TextEncoder().encode(JSON.stringify({ pad: 'a'.repeat(90 * 1024) }));
  const cs = clientMux.chunkSize;
  for (let off = 0; off < bigCtrl.byteLength; off += cs) {
    const end = Math.min(off + cs, bigCtrl.byteLength);
    const flags = end < bigCtrl.byteLength ? frameMod.FrameFlag.MORE : frameMod.FrameFlag.NONE;
    sCtrl.send(frameMod.encodeFrame(0, frameMod.FrameType.AUTH_RESULT, flags, bigCtrl.subarray(off, end)));
  }
  sCtrl.send(frameMod.encodeFrame(0, frameMod.FrameType.GOAWAY, frameMod.FrameFlag.NONE, frameMod.encodeGoAwayPayload(frameMod.ErrorCode.GOING_AWAY)));
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(ctrlFrames.length, 1);
  assert.equal(ctrlFrames[0].payload.byteLength, bigCtrl.byteLength);
  assert.deepEqual(goAways, [frameMod.ErrorCode.GOING_AWAY]);

  // Exact 8-byte PING/PONG check + immediate false on close.
  let pingMode = 'exact';
  sCtrl.onmessage = (ev) => {
    const f = frameMod.decodeFrame(new Uint8Array(ev.data));
    if (f.type === frameMod.FrameType.PING) {
      if (pingMode === 'exact') {
        sCtrl.send(frameMod.encodeFrame(0, frameMod.FrameType.PONG, frameMod.FrameFlag.NONE, f.payload));
      } else if (pingMode === 'wrong') {
        const bad = new Uint8Array(f.payload);
        bad[7] ^= 0xff;
        sCtrl.send(frameMod.encodeFrame(0, frameMod.FrameType.PONG, frameMod.FrameFlag.NONE, bad));
      }
    }
  };
  assert.equal(await clientMux.ping(300), true);
  pingMode = 'wrong';
  assert.equal(await clientMux.ping(80), false);
  pingMode = 'drop';
  const pendingPing = clientMux.ping(5_000);
  clientMux.close();
  assert.equal(await pendingPing, false);
  sCtrl.close();
  for (const s of sLanes) s.close();
});

test('RFC 9383 Appendix C.1 official vector & V7 SPAKE2+ KDF cross-language parity (TS <-> Go)', async () => {
  const spakeMod = await loadTsBundle('web/src/shared/spake2p.ts');
  const golden = JSON.parse(
    fs.readFileSync(path.join(repoRoot, 'test/fixtures/protocol_v8_golden_vectors.json'), 'utf8')
  );
  const v = golden.rfc9383_appendix_c;

  const w0Bytes = hexToBytes(v.w0_hex);
  const w0 = spakeMod.bytesToBigint(w0Bytes);
  const w1 = spakeMod.bytesToBigint(hexToBytes(v.w1_hex));
  const x = spakeMod.bytesToBigint(hexToBytes(v.x_hex));
  const y = spakeMod.bytesToBigint(hexToBytes(v.y_hex));

  const shareV = spakeMod.spake2pComputeShareVForTest(w0, y);
  assert.equal(bytesToHex(shareV), v.share_v_hex, 'RFC 9383 shareV mismatch in TS');

  const te = new TextEncoder();
  const res = await spakeMod.spake2pProverComputeWithScalar(
    { w0, w1, w0Bytes },
    x,
    te.encode(v.context),
    te.encode(v.id_prover),
    te.encode(v.id_verifier),
    shareV
  );
  assert.equal(bytesToHex(res.shareP), v.share_p_hex, 'RFC 9383 shareP mismatch in TS');
  assert.equal(bytesToHex(res.confirmP), v.confirm_p_hex, 'RFC 9383 confirmP mismatch in TS');
  assert.equal(bytesToHex(res.expectedConfirmV), v.confirm_v_hex, 'RFC 9383 confirmV mismatch in TS');
  assert.equal(bytesToHex(res.sharedKey), v.shared_key_hex, 'RFC 9383 sharedKey mismatch in TS');

  const kdf = golden.v7_spake2p_kdf;
  const creds = await spakeMod.deriveSPAKE2PProverCreds(kdf.password, kdf.node_id, kdf.agent_pk_b64);
  assert.equal(bytesToHex(creds.w0Bytes), kdf.w0_hex, 'KDF w0 mismatch between TS and Go');
  assert.equal(bytesToHex(creds.w1Bytes), kdf.w1_hex, 'KDF w1 mismatch between TS and Go');
});

test('PoolMux V8 TS Audit Regressions (P1-7, P2-7): WSStream concurrent sendMessage serialization & MAX_WS_MESSAGE_CHUNKS cap', async () => {
  const muxMod = await loadTsBundle('web/src/protocol/mux.ts');
  const frameMod = await loadTsBundle('web/src/protocol/frame.ts');
  const tlvMod = await loadTsBundle('web/src/protocol/tlv.ts');

  const sentFrames = [];
  let lowCb = null;
  const dc = {
    readyState: 'open',
    bufferedAmount: 0,
    bufferedAmountLowThreshold: 0,
    binaryType: 'arraybuffer',
    onmessage: null,
    onopen: null,
    onclose: null,
    onerror: null,
    set onbufferedamountlow(fn) { lowCb = fn; },
    get onbufferedamountlow() { return lowCb; },
    send(data) {
      const f = frameMod.decodeFrame(new Uint8Array(data));
      sentFrames.push({ type: f.type, flags: f.flags, len: f.payload.byteLength });
      // Trigger SCTP backpressure right after 1st WS chunk so concurrent callers race across await!
      if (sentFrames.length === 2) {
        this.bufferedAmount = 2 * 1024 * 1024;
        setTimeout(() => {
          this.bufferedAmount = 0;
          lowCb?.();
        }, 10);
      }
    },
    close() { this.readyState = 'closed'; },
  };
  const ctrl = { ...dc, send() {}, close() {} };

  const mux = new muxMod.PoolMux(1, {});
  mux.attachControlDC(ctrl);
  mux.attachLaneDC(0, dc);
  mux.setMaxMessageSize(4096); // chunkSize = 4088

  const wsReq = tlvMod.encodeRequestHeaders({ kind: tlvMod.StreamKind.WS, method: 'GET', target: '/ws', headers: [] });
  const ws = await mux.openWSStream(wsReq);

  // 1. P1-7: Concurrent multi-chunk TEXT + BINARY sends MUST NOT interleave opcodes!
  await Promise.all([
    ws.sendMessage('A'.repeat(9000)),
    ws.sendMessage(new Uint8Array(9000).fill(0x42)),
  ]);
  const wsFrames = sentFrames.filter(
    (f) => f.type === frameMod.FrameType.WS_TEXT || f.type === frameMod.FrameType.WS_BINARY
  );
  assert.ok(wsFrames.length >= 6);
  let activeOp = 0;
  for (const f of wsFrames) {
    if (activeOp !== 0) {
      assert.equal(f.type, activeOp, `P1-7 regression: interleaved WS opcodes ${JSON.stringify(wsFrames)}`);
    }
    activeOp = f.flags & frameMod.FrameFlag.MSG_FIN ? 0 : f.type;
  }

  // 2. P2-7: Inbound WS message exceeding MAX_WS_MESSAGE_CHUNKS (16384) must RESET with FRAME_SIZE_ERROR!
  const resp101 = tlvMod.encodeResponseHeaders({ status: 101, headers: [] });
  dc.onmessage({ data: frameMod.encodeFrame(ws.streamId, frameMod.FrameType.HEADERS, frameMod.FrameFlag.NONE, resp101).buffer });
  const oneByte = new Uint8Array([0x01]);
  const chunkRaw = frameMod.encodeFrame(ws.streamId, frameMod.FrameType.WS_BINARY, frameMod.FrameFlag.NONE, oneByte).buffer;
  for (let i = 0; i <= frameMod.MAX_WS_MESSAGE_CHUNKS; i++) {
    if (ws.isClosed) break;
    dc.onmessage({ data: chunkRaw });
  }
  assert.equal(ws.isClosed, true, 'P2-7: exceeding MAX_WS_MESSAGE_CHUNKS must reset WSStream');
  assert.equal(mux.sessionMemUsed, 0, 'P2-7: reset WSStream must release all session memory');
  mux.close();
});
