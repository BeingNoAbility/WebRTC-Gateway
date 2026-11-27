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
  const make = () => {
    const listeners = new Map();
    return {
      readyState: 'open',
      bufferedAmount: 0,
      bufferedAmountLowThreshold: 0,
      binaryType: 'arraybuffer',
      peer: null,
      sent: [],
      addEventListener(type, fn) {
        let set = listeners.get(type);
        if (!set) {
          set = new Set();
          listeners.set(type, set);
        }
        set.add(fn);
      },
      removeEventListener(type, fn) {
        listeners.get(type)?.delete(fn);
      },
      _emit(type, ev) {
        const set = listeners.get(type);
        if (!set) return;
        for (const fn of Array.from(set)) fn(ev);
      },
      send(data) {
        if (this.readyState !== 'open') throw new Error('DC closed');
        const cp = new Uint8Array(data).slice();
        this.sent.push(cp);
        const peer = this.peer;
        if (peer && peer.readyState === 'open') {
          queueMicrotask(() => {
            if (peer.readyState === 'open') {
              peer._emit('message', { data: cp.buffer });
            }
          });
        }
      },
      close() {
        if (this.readyState === 'closed') return;
        this.readyState = 'closed';
        this._emit('close', {});
      },
    };
  };
  const a = make();
  const b = make();
  a.peer = b;
  b.peer = a;
  return [a, b];
}

test('P2PMux V7 Golden Vectors (TS codec parity with Go)', async () => {
  const frameMod = await loadTsBundle('web/src/protocol/frame.ts');
  const tlvMod = await loadTsBundle('web/src/protocol/tlv.ts');
  const golden = JSON.parse(
    fs.readFileSync(path.join(repoRoot, 'test/fixtures/protocol_v7_golden_vectors.json'), 'utf8')
  );
  assert.equal(golden.version, 7);

  for (const tc of golden.valid_frames) {
    const payload = hexToBytes(tc.payload_hex);
    const wire = frameMod.encodeFrame(tc.gen, tc.type, tc.flags, payload);
    assert.equal(bytesToHex(wire), tc.wire_hex, `encode mismatch: ${tc.name}`);
    const dec = frameMod.decodeFrame(wire);
    assert.equal(dec.gen, tc.gen, tc.name);
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

  // Negative tests: ASCII space / backslash / Unicode Kelvin sign in header / CTL in value MUST fail!
  assert.throws(
    () =>
      tlvMod.encodeRequestHeaders({
        kind: tlvMod.StreamKind.HTTP,
        method: 'GET',
        target: '/bad path',
        headers: [],
      }),
    /target/i
  );
  assert.throws(
    () =>
      tlvMod.encodeRequestHeaders({
        kind: tlvMod.StreamKind.HTTP,
        method: 'GET',
        target: '/bad\\path',
        headers: [],
      }),
    /target/i
  );
  assert.throws(
    () =>
      tlvMod.encodeRequestHeaders({
        kind: tlvMod.StreamKind.HTTP,
        method: 'GET',
        target: '/',
        headers: [{ name: 'x-\u212aey', value: 'ok' }],
      }),
    /header/i
  );
  assert.throws(
    () =>
      tlvMod.encodeRequestHeaders({
        kind: tlvMod.StreamKind.HTTP,
        method: 'GET',
        target: '/',
        headers: [{ name: 'x-ctl', value: 'bad\x01val' }],
      }),
    /header/i
  );

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
});

test('PoolMux V7 TS: 32-DC reuse, anti-credit-forgery, downward-only MMS, short response preservation, 2.5MiB WS', async () => {
  const muxMod = await loadTsBundle('web/src/protocol/mux.ts');
  const tlvMod = await loadTsBundle('web/src/protocol/tlv.ts');
  const frameMod = await loadTsBundle('web/src/protocol/frame.ts');

  const [cCtrl, sCtrl] = makeMockChannelPair();
  const poolSize = 4;
  const cPool = [];
  const sPool = [];
  for (let i = 0; i < poolSize; i++) {
    const [a, b] = makeMockChannelPair();
    cPool.push(a);
    sPool.push(b);
  }

  const clientMux = new muxMod.PoolMux({}, poolSize);
  clientMux.attachControlDC(cCtrl);
  for (let i = 0; i < poolSize; i++) {
    clientMux.attachPoolDC(i, cPool[i]);
  }

  // Negative test: setMaxMessageSize clamps strictly downward, never upward.
  const initialChunk = clientMux.effectiveChunkSize();
  clientMux.setMaxMessageSize(12_000);
  const clampedChunk = clientMux.effectiveChunkSize();
  assert.ok(clampedChunk < initialChunk && clampedChunk === 12_000 - frameMod.HEADER_SIZE);
  clientMux.setMaxMessageSize(65_536);
  assert.equal(clientMux.effectiveChunkSize(), clampedChunk, 'setMaxMessageSize must never raise chunkSize');

  // Wire simple server-side responder directly on sPool slots.
  for (let i = 0; i < poolSize; i++) {
    const sdc = sPool[i];
    sdc.addEventListener('message', (ev) => {
      const f = frameMod.decodeFrame(new Uint8Array(ev.data));
      if (f.type === frameMod.FrameType.HEADERS) {
        const req = tlvMod.decodeRequestHeaders(f.payload);
        if (req.target === '/echo') {
          const respTlv = tlvMod.encodeResponseHeaders({ status: 200, headers: [] });
          sdc.send(frameMod.encodeFrame(f.gen, frameMod.FrameType.HEADERS, frameMod.FrameFlag.NONE, respTlv));
          sdc.send(
            frameMod.encodeFrame(
              f.gen,
              frameMod.FrameType.DATA,
              frameMod.FrameFlag.FIN,
              new TextEncoder().encode(`slot:${i}:gen:${f.gen}`)
            )
          );
        } else if (req.target === '/short-before-ondata') {
          const respTlv = tlvMod.encodeResponseHeaders({ status: 200, headers: [] });
          sdc.send(frameMod.encodeFrame(f.gen, frameMod.FrameType.HEADERS, frameMod.FrameFlag.NONE, respTlv));
          sdc.send(
            frameMod.encodeFrame(
              f.gen,
              frameMod.FrameType.DATA,
              frameMod.FrameFlag.FIN,
              new TextEncoder().encode('must-not-be-purged')
            )
          );
        } else if (req.target === '/forged-credit-test') {
          const respTlv = tlvMod.encodeResponseHeaders({ status: 200, headers: [] });
          sdc.send(frameMod.encodeFrame(f.gen, frameMod.FrameType.HEADERS, frameMod.FrameFlag.NONE, respTlv));
          sdc.send(frameMod.encodeFrame(f.gen, frameMod.FrameType.DATA, frameMod.FrameFlag.NONE, new Uint8Array(100)));
        }
      }
    });
  }

  // 1. Reuse 4 pool slots across 20 sequential/concurrent streams (5 generations per slot).
  const results = await Promise.all(
    Array.from({ length: 20 }, async () => {
      const reqTlv = tlvMod.encodeRequestHeaders({
        kind: tlvMod.StreamKind.HTTP,
        method: 'GET',
        target: '/echo',
        headers: [],
      });
      const st = await clientMux.openHTTPStream(reqTlv, true, false);
      return await new Promise((resolve, reject) => {
        const chunks = [];
        st.onHeaders(() => {});
        st.onData((c) => chunks.push(c));
        st.onEnd(() => resolve(new TextDecoder().decode(Buffer.concat(chunks))));
        st.onReset((code) => reject(new Error(`reset ${code}`)));
      });
    })
  );
  assert.equal(results.length, 20);
  assert.equal(clientMux.getActiveStreamCount(), 0);
  assert.equal(clientMux.getSessionInboundBytes(), 0);

  // 2. V7 Negative test: short response (HEADERS + DATA(FIN)) arriving BEFORE onData() is attached
  //    MUST NOT have its buffered inboundChunks purged by checkFullClose()!
  const shortReq = tlvMod.encodeRequestHeaders({
    kind: tlvMod.StreamKind.HTTP,
    method: 'GET',
    target: '/short-before-ondata',
    headers: [],
  });
  const shortStream = await clientMux.openHTTPStream(shortReq, true, false);
  // Wait for both HEADERS and DATA(FIN) microtasks to complete BEFORE attaching onData/onEnd!
  await new Promise((r) => setTimeout(r, 20));
  const lateBody = await new Promise((resolve) => {
    const chunks = [];
    shortStream.onHeaders(() => {});
    shortStream.onData((c) => chunks.push(c));
    shortStream.onEnd(() => resolve(new TextDecoder().decode(Buffer.concat(chunks))));
  });
  assert.equal(lateBody, 'must-not-be-purged', 'PoolStream must never purge unread inboundChunks on checkFullClose');
  assert.equal(clientMux.getSessionInboundBytes(), 0);

  // 3. Negative test: ackConsumed() with forged 10 MiB must not emit CREDIT.
  const reqForged = tlvMod.encodeRequestHeaders({
    kind: tlvMod.StreamKind.HTTP,
    method: 'GET',
    target: '/forged-credit-test',
    headers: [],
  });
  const forgedStream = await clientMux.openHTTPStream(reqForged, true, true);
  const slotDC = cPool[forgedStream.slotIdx];
  const delivered = await new Promise((resolve) => {
    forgedStream.onHeaders(() => {});
    forgedStream.onData((c) => resolve(c.byteLength));
  });
  assert.equal(delivered, 100);
  const sentBefore = slotDC.sent.length;
  forgedStream.ackConsumed(10 * 1024 * 1024); // Forged massive credit from untrusted sandbox!
  const creditFrames = slotDC.sent
    .slice(sentBefore)
    .map((b) => frameMod.decodeFrame(b))
    .filter((f) => f.type === frameMod.FrameType.CREDIT);
  assert.equal(creditFrames.length, 0, 'forged ackConsumed must be strictly clamped to delivered bytes');
  forgedStream.reset(frameMod.ErrorCode.CANCEL);

  // 4. V7 Negative test: openHTTPStream honors AbortSignal when all slots are busy!
  const ac = new AbortController();
  ac.abort();
  await assert.rejects(() => clientMux.openHTTPStream(shortReq, true, false, ac.signal), /aborted/i);

  // 5. Negative test: WSStream handles 2.5 MiB (> 2 MiB window) with overhead-aware flow control.
  const [cWsDC, sWsDC] = makeMockChannelPair();
  const OVERHEAD = muxMod.PER_CHUNK_MEM_OVERHEAD;
  let sWsBufs = [];
  let sWsConsumed = 0;
  let sWsSendWin = muxMod.DEFAULT_STREAM_WINDOW_SIZE;
  let sWsCreditResolvers = [];
  const flushServerWsPayload = async (isBin, payload) => {
    let off = 0;
    let first = true;
    while (off < payload.byteLength) {
      while (sWsSendWin <= OVERHEAD) {
        await new Promise((r) => sWsCreditResolvers.push(r));
      }
      const n = Math.min(payload.byteLength - off, 16384, sWsSendWin - OVERHEAD);
      sWsSendWin -= n + OVERHEAD;
      const op = first
        ? isBin
          ? frameMod.FrameType.WS_BINARY
          : frameMod.FrameType.WS_TEXT
        : frameMod.FrameType.WS_CONT;
      first = false;
      const flags = off + n === payload.byteLength ? frameMod.FrameFlag.MSG_FIN : frameMod.FrameFlag.NONE;
      sWsDC.send(frameMod.encodeFrame(1, op, flags, payload.subarray(off, off + n)));
      off += n;
    }
  };
  sWsDC.addEventListener('message', (ev) => {
    const f = frameMod.decodeFrame(new Uint8Array(ev.data));
    if (f.type === frameMod.FrameType.HEADERS) {
      const resp = tlvMod.encodeResponseHeaders({ status: 101, headers: [] });
      sWsDC.send(frameMod.encodeFrame(1, frameMod.FrameType.HEADERS, frameMod.FrameFlag.NONE, resp));
    } else if (f.type === frameMod.FrameType.CREDIT) {
      sWsSendWin += frameMod.decodeCreditPayload(f.payload);
      for (const r of sWsCreditResolvers.splice(0)) r();
    } else if (f.type === frameMod.FrameType.WS_BINARY || f.type === frameMod.FrameType.WS_CONT) {
      sWsBufs.push(f.payload);
      sWsConsumed += f.payload.byteLength + OVERHEAD;
      if (sWsConsumed >= muxMod.DEFAULT_STREAM_WINDOW_SIZE / 8) {
        const delta = sWsConsumed;
        sWsConsumed = 0;
        sWsDC.send(
          frameMod.encodeFrame(1, frameMod.FrameType.CREDIT, frameMod.FrameFlag.NONE, frameMod.encodeCreditPayload(delta))
        );
      }
      if ((f.flags & frameMod.FrameFlag.MSG_FIN) !== 0) {
        const full = Buffer.concat(sWsBufs);
        sWsBufs = [];
        void flushServerWsPayload(true, new Uint8Array(full));
      }
    }
  });

  const wsReq = tlvMod.encodeRequestHeaders({
    kind: tlvMod.StreamKind.WS,
    method: 'GET',
    target: '/ws',
    headers: [],
  });
  const wsStream = await clientMux.openWSStream(cWsDC, wsReq);
  const wsHandshake = new Promise((r) => wsStream.onHeaders((h) => r(tlvMod.decodeResponseHeaders(h).status)));
  assert.equal(await wsHandshake, 101);

  const bigPayload = new Uint8Array(2.5 * 1024 * 1024);
  for (let i = 0; i < bigPayload.byteLength; i++) bigPayload[i] = (i * 31) & 0xff;

  const echoedPromise = new Promise((resolve, reject) => {
    wsStream.onMessage((isBin, data) => resolve({ isBin, data }));
    wsStream.onReset((code) => reject(new Error(`ws reset ${code}`)));
  });
  await wsStream.sendWSMessage(true, bigPayload);
  const echoed = await echoedPromise;
  assert.equal(echoed.isBin, true);
  assert.equal(echoed.data.byteLength, bigPayload.byteLength);
  assert.equal(echoed.data[0], bigPayload[0]);
  assert.equal(echoed.data[bigPayload.byteLength - 1], bigPayload[bigPayload.byteLength - 1]);

  wsStream.sendWSClose(1000, 'done');
  clientMux.close();
  sCtrl.close();
  for (const s of sPool) s.close();
});

test('RFC 9383 Appendix C.1 official vector & V7 SPAKE2+ KDF cross-language parity (TS <-> Go)', async () => {
  const spakeMod = await loadTsBundle('web/src/shared/spake2p.ts');
  const golden = JSON.parse(
    fs.readFileSync(path.join(repoRoot, 'test/fixtures/protocol_v7_golden_vectors.json'), 'utf8')
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

  // Verify V7 SPAKE2+ KDF (PBKDF2 300k + HKDF-SHA256) matches Go golden vector byte-for-byte!
  const kdf = golden.v7_spake2p_kdf;
  const creds = await spakeMod.deriveSPAKE2PProverCreds(kdf.password, kdf.node_id, kdf.agent_pk_b64);
  assert.equal(bytesToHex(creds.w0Bytes), kdf.w0_hex, 'V7 KDF w0 mismatch between TS and Go');
  assert.equal(bytesToHex(creds.w1Bytes), kdf.w1_hex, 'V7 KDF w1 mismatch between TS and Go');

  const credsWrong = await spakeMod.deriveSPAKE2PProverCreds('wrong-pass', kdf.node_id, kdf.agent_pk_b64);
  assert.notDeepEqual(creds.w0Bytes, credsWrong.w0Bytes);

  const ctx = spakeMod.buildSPAKE2PContext(
    kdf.node_id,
    'a'.repeat(64),
    'b'.repeat(64),
    kdf.agent_pk_b64,
    'vnonce',
    'anonce'
  );
  assert.ok(ctx.byteLength > 100);
});

test('PoolMux.ping verifies exact 8-byte PONG echo, rejects mismatch, and fails fast on close', async () => {
  const frameMod = await loadTsBundle('web/src/protocol/frame.ts');
  const muxMod = await loadTsBundle('web/src/protocol/mux.ts');

  const [cCtrl, sCtrl] = makeMockChannelPair();
  const [cPool, sPool] = makeMockChannelPair();
  const clientMux = new muxMod.PoolMux({}, 1, 2);
  clientMux.attachControlDC(cCtrl);
  clientMux.attachPoolDC(0, cPool);

  // 1. Peer echoes exact PING payload as PONG -> resolves true!
  let echoMode = 'exact';
  sCtrl.addEventListener('message', (ev) => {
    const f = frameMod.decodeFrame(new Uint8Array(ev.data));
    if (f.type === frameMod.FrameType.PING) {
      if (echoMode === 'exact') {
        sCtrl.send(frameMod.encodeFrame(0, frameMod.FrameType.PONG, frameMod.FrameFlag.NONE, f.payload));
      } else if (echoMode === 'wrong') {
        const bad = new Uint8Array(f.payload);
        bad[7] ^= 0xff;
        sCtrl.send(frameMod.encodeFrame(0, frameMod.FrameType.PONG, frameMod.FrameFlag.NONE, bad));
      }
    }
  });

  assert.equal(await clientMux.ping(300), true, 'matching PONG must resolve ping() to true');

  // 2. Mismatched PONG payload must NOT satisfy ping() -> times out as false!
  echoMode = 'wrong';
  assert.equal(await clientMux.ping(80), false, 'mismatched PONG payload must time out as false');

  // 3. Closing mux while ping() is pending must resolve false immediately without leaking timers!
  echoMode = 'drop';
  const pendingPing = clientMux.ping(5_000);
  clientMux.close();
  assert.equal(await pendingPing, false, 'mux.close() must settle in-flight ping() to false immediately');

  sCtrl.close();
  sPool.close();
});
