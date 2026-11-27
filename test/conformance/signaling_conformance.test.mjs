/**
 * Single Authoritative Go Signal Server Black-Box Conformance Suite (V7)
 *
 * Tests the real compiled `cmd/signal` binary over raw TCP HTTP & real WebSockets:
 *  - Host allowlist 421, Portal vs Sandbox air-gap, single-pass path canonicalization (blocking /%2577s/ double decode!)
 *  - Strict Portal CSP (`style-src 'self'` with ZERO `'unsafe-inline'`, `connect-src 'self'`)
 *  - Exact `bridge.html` `frame-ancestors` + `<meta name="p2p-portal-origins">` (ZERO `*.portal` or `*.localhost`!)
 *  - Localdev `*.localhost` CSWSH Origin rejection
 *  - Real WebSocket Ed25519 `register-v2` (length-prefixed), Client `joined.agent_pk`, structured `SessionDescriptor` Offer/Answer,
 *    forged `Answer.PK` mismatch rejection, per-node `-node-secrets-file`, and `-bound-keys-file` persistence across restart!
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { spawn, execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(__dirname, '..', '..');

function b64url(buf) {
  return Buffer.from(buf)
    .toString('base64')
    .replace(/\+/g, '-')
    .replace(/\//g, '_')
    .replace(/=+$/g, '');
}

function appendLenPrefixed(prefix, fields) {
  const parts = [Buffer.from(prefix, 'utf8')];
  for (const f of fields) {
    const b = Buffer.from(f, 'utf8');
    const hdr = Buffer.alloc(2);
    hdr.writeUInt16BE(b.length, 0);
    parts.push(hdr, b);
  }
  return Buffer.concat(parts);
}

function genEd25519Keypair() {
  const { publicKey, privateKey } = crypto.generateKeyPairSync('ed25519');
  const spki = publicKey.export({ type: 'spki', format: 'der' });
  const rawPub = spki.subarray(spki.length - 32);
  return {
    pkB64: b64url(rawPub),
    signRegisterV2(nodeId, nonce) {
      const msg = appendLenPrefixed('p2pgw-v7:register-v2:', [nodeId, nonce]);
      return b64url(crypto.sign(null, msg, privateKey));
    },
  };
}

function getFreePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address();
      srv.close(() => resolve(port));
    });
    srv.on('error', reject);
  });
}

function rawHttpRequest({ port, hostHeader, rawPath, method = 'GET', extraHeaders = {} }) {
  return new Promise((resolve, reject) => {
    const sock = net.connect(port, '127.0.0.1', () => {
      let req = `${method} ${rawPath} HTTP/1.1\r\nHost: ${hostHeader}\r\nConnection: close\r\n`;
      for (const [k, v] of Object.entries(extraHeaders)) {
        req += `${k}: ${v}\r\n`;
      }
      req += '\r\n';
      sock.write(req);
    });
    let data = '';
    sock.setEncoding('utf8');
    sock.on('data', (chunk) => (data += chunk));
    sock.on('end', () => {
      const [head, ...rest] = data.split('\r\n\r\n');
      const lines = head.split('\r\n');
      const status = Number(lines[0].split(' ')[1]);
      const headers = {};
      for (const line of lines.slice(1)) {
        const idx = line.indexOf(':');
        if (idx > 0) headers[line.slice(0, idx).trim().toLowerCase()] = line.slice(idx + 1).trim();
      }
      resolve({ status, headers, body: rest.join('\r\n\r\n') });
    });
    sock.on('error', reject);
  });
}

// Minimal RFC 6455 JSON WebSocket test client over raw TCP so we can control Host & Origin headers!
function connectRawWS({ port, hostHeader, reqPath, headers = {} }) {
  return new Promise((resolve, reject) => {
    const sock = net.connect(port, '127.0.0.1', () => {
      const key = crypto.randomBytes(16).toString('base64');
      let req =
        `GET ${reqPath} HTTP/1.1\r\n` +
        `Host: ${hostHeader}\r\n` +
        `Upgrade: websocket\r\n` +
        `Connection: Upgrade\r\n` +
        `Sec-WebSocket-Key: ${key}\r\n` +
        `Sec-WebSocket-Version: 13\r\n`;
      for (const [k, v] of Object.entries(headers)) {
        req += `${k}: ${v}\r\n`;
      }
      req += '\r\n';
      sock.write(req);
    });

    let handshakeDone = false;
    let buf = Buffer.alloc(0);
    const queue = [];
    const waiters = [];

    const pushMsg = (obj) => {
      if (waiters.length > 0) {
        waiters.shift().resolve(obj);
      } else {
        queue.push(obj);
      }
    };

    const parseFrames = () => {
      while (buf.length >= 2) {
        const opcode = buf[0] & 0x0f;
        let len = buf[1] & 0x7f;
        let off = 2;
        if (len === 126) {
          if (buf.length < 4) return;
          len = buf.readUInt16BE(2);
          off = 4;
        } else if (len === 127) {
          if (buf.length < 10) return;
          len = Number(buf.readBigUInt64BE(2));
          off = 10;
        }
        if (buf.length < off + len) return;
        const payload = buf.subarray(off, off + len);
        buf = buf.subarray(off + len);
        if (opcode === 1) {
          pushMsg(JSON.parse(payload.toString('utf8')));
        } else if (opcode === 8) {
          sock.end();
        }
      }
    };

    sock.on('data', (chunk) => {
      buf = Buffer.concat([buf, chunk]);
      if (!handshakeDone) {
        const sep = buf.indexOf('\r\n\r\n');
        if (sep < 0) return;
        const head = buf.subarray(0, sep).toString('utf8');
        buf = buf.subarray(sep + 4);
        const status = Number(head.split('\r\n')[0].split(' ')[1]);
        handshakeDone = true;
        if (status !== 101) {
          sock.destroy();
          resolve({ status, ws: null });
          return;
        }
        const ws = {
          sendJSON(obj) {
            const payload = Buffer.from(JSON.stringify(obj), 'utf8');
            const mask = crypto.randomBytes(4);
            let hdr;
            if (payload.length < 126) {
              hdr = Buffer.from([0x81, 0x80 | payload.length]);
            } else {
              hdr = Buffer.alloc(4);
              hdr[0] = 0x81;
              hdr[1] = 0x80 | 126;
              hdr.writeUInt16BE(payload.length, 2);
            }
            const masked = Buffer.alloc(payload.length);
            for (let i = 0; i < payload.length; i++) masked[i] = payload[i] ^ mask[i % 4];
            sock.write(Buffer.concat([hdr, mask, masked]));
          },
          nextJSON(timeoutMs = 5000) {
            if (queue.length > 0) return Promise.resolve(queue.shift());
            return new Promise((res, rej) => {
              const t = setTimeout(() => rej(new Error('ws nextJSON timeout')), timeoutMs);
              waiters.push({
                resolve: (v) => {
                  clearTimeout(t);
                  res(v);
                },
              });
            });
          },
          close() {
            sock.destroy();
          },
        };
        resolve({ status: 101, ws });
        parseFrames();
      } else {
        parseFrames();
      }
    });
    sock.on('error', reject);
  });
}

async function waitHealthz(port, hostHeader) {
  for (let i = 0; i < 40; i++) {
    try {
      const r = await rawHttpRequest({ port, hostHeader, rawPath: '/healthz' });
      if (r.status === 200) return;
    } catch {
      // retry
    }
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error('signal server failed to become ready');
}

test('Go Signal V7 Conformance: HTTP air-gap, double-decode block, exact bridge CSP, WS register-v2, Answer.PK bind & bound-keys persistence', async (t) => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'p2p-sig-v7-conf-'));
  const staticDir = path.join(tmpDir, 'dist');
  fs.mkdirSync(path.join(staticDir, '__p2p_gw__'), { recursive: true });
  fs.writeFileSync(path.join(staticDir, 'index.html'), '<!doctype html><html><head></head><body>ok</body></html>');
  fs.writeFileSync(path.join(staticDir, '__p2p_gw__', 'portal.js'), 'console.log(1);');
  fs.writeFileSync(path.join(staticDir, '__p2p_gw__', 'portal.css'), 'body{}');
  fs.writeFileSync(path.join(staticDir, '__p2p_gw__', 'sw.js'), 'console.log("sw");');
  fs.writeFileSync(
    path.join(staticDir, '__p2p_gw__', 'bridge.html'),
    '<!doctype html><html><head></head><body><script src="/__p2p_gw__/bridge.js"></script></body></html>'
  );
  fs.writeFileSync(path.join(staticDir, '__p2p_gw__', 'bridge.js'), 'console.log("br");');

  const boundKeysFile = path.join(tmpDir, 'bound-keys.json');
  const nodeSecretsFile = path.join(tmpDir, 'node-secrets.json');
  fs.writeFileSync(nodeSecretsFile, JSON.stringify({ nas: 'per-node-secret-nas' }));

  const binPath = path.join(tmpDir, 'signal');
  execFileSync('go', ['build', '-o', binPath, './cmd/signal'], { cwd: repoRoot, stdio: 'pipe' });

  const port = await getFreePort();
  const portalHost = `portal.example.com:${port}`;
  const sandboxHost = `sbx-abc123.sandbox.example.net:${port}`;

  const spawnSignal = () =>
    spawn(
      binPath,
      [
        '-addr', `127.0.0.1:${port}`,
        '-static', staticDir,
        '-portal-domain', portalHost,
        '-sandbox-domain', `sandbox.example.net:${port}`,
        '-bound-keys-file', boundKeysFile,
        '-node-secrets-file', nodeSecretsFile,
      ],
      {
        env: { ...process.env, SIGNAL_SECRET: 'cluster-fallback-secret' },
        stdio: 'pipe',
      }
    );

  let proc = spawnSignal();
  t.after(() => {
    proc?.kill('SIGTERM');
    fs.rmSync(tmpDir, { recursive: true, force: true });
  });

  await waitHealthz(port, portalHost);

  // 1. Path canonicalization & single-pass percent decode (blocks /%73w.js AND /%2577s/agent/nas!)
  for (const p of ['/%73w.js', '//sw.js', '/__p2p_gw__/%73w.js', '/__p2p_gw__/bridge.html', '/%2577s/agent/nas']) {
    const r = await rawHttpRequest({ port, hostHeader: portalHost, rawPath: p });
    assert.equal(r.status, 404, `Portal path ${p} must return 404`);
  }

  // 2. Portal index.html strict CSP (ZERO 'unsafe-inline', connect-src 'self') & portal.css served
  const home = await rawHttpRequest({ port, hostHeader: portalHost, rawPath: '/' });
  assert.equal(home.status, 200);
  assert.equal(home.headers['x-frame-options'], 'DENY');
  const portalCsp = home.headers['content-security-policy'] || '';
  assert.ok(portalCsp.includes("connect-src 'self'"));
  assert.ok(portalCsp.includes("style-src 'self'"));
  assert.ok(!portalCsp.includes('unsafe-inline'), `Portal CSP must not contain unsafe-inline: ${portalCsp}`);
  assert.ok(home.body.includes('name="p2p-config"'));

  const css = await rawHttpRequest({ port, hostHeader: portalHost, rawPath: '/__p2p_gw__/portal.css' });
  assert.equal(css.status, 200);

  // 3. Unauthorized Host -> 421; Sandbox bridge.html exact frame-ancestors + p2p-portal-origins meta!
  const badHost = await rawHttpRequest({ port, hostHeader: `evil.example.org:${port}`, rawPath: '/' });
  assert.equal(badHost.status, 421);

  const br = await rawHttpRequest({ port, hostHeader: sandboxHost, rawPath: '/__p2p_gw__/bridge.html' });
  assert.equal(br.status, 200);
  const brCsp = br.headers['content-security-policy'] || '';
  assert.ok(brCsp.includes(`frame-ancestors http://${portalHost}`));
  assert.ok(!brCsp.includes('*.portal.example.com'), `bridge CSP must not wildcard Portal: ${brCsp}`);
  assert.ok(br.body.includes('name="p2p-portal-origins"'), 'bridge.html must receive p2p-portal-origins meta');

  // 4. Per-node secret enforcement: global fallback rejected for 'nas', per-node secret accepted!
  const wrongSecret = await connectRawWS({
    port,
    hostHeader: portalHost,
    reqPath: '/ws/agent/nas',
    headers: { Authorization: 'Bearer cluster-fallback-secret' },
  });
  assert.equal(wrongSecret.status, 401, 'per-node secret must override global SIGNAL_SECRET for node nas');

  // 5. Real WS Agent Ed25519 register-v2 + Client joined.agent_pk + Answer.PK mismatch check!
  const agentKP = genEd25519Keypair();
  const agConn = await connectRawWS({
    port,
    hostHeader: portalHost,
    reqPath: '/ws/agent/nas',
    headers: { Authorization: 'Bearer per-node-secret-nas' },
  });
  assert.equal(agConn.status, 101);
  const chal = await agConn.ws.nextJSON();
  assert.equal(chal.type, 'challenge');

  agConn.ws.sendJSON({
    type: 'register',
    pk: agentKP.pkB64,
    sig: agentKP.signRegisterV2('nas', chal.nonce),
    require_visitor_auth: true,
  });
  const regOk = await agConn.ws.nextJSON();
  assert.equal(regOk.type, 'registered');

  // Client connects from exact Portal Origin -> receives joined with agent_pk!
  const clConn = await connectRawWS({
    port,
    hostHeader: portalHost,
    reqPath: '/ws/client/nas',
    headers: { Origin: `http://${portalHost}` },
  });
  assert.equal(clConn.status, 101);
  const joined = await clConn.ws.nextJSON();
  assert.equal(joined.type, 'joined');
  assert.equal(joined.online, true);
  assert.equal(joined.agent_pk, agentKP.pkB64);

  // Client sends valid structured SessionDescriptor Offer.
  const offerDesc = {
    fp: 'a'.repeat(64),
    ice_ufrag: 'ufrag1234',
    ice_pwd: 'password1234567890123456',
    setup: 'actpass',
    mid: '0',
    sctp_port: 5000,
    max_msg_size: 262144,
    sdp_ver: 2,
  };
  clConn.ws.sendJSON({ type: 'offer', desc: offerDesc, nonce: 'visitor-nonce-0123456789' });
  const relayedOffer = await agConn.ws.nextJSON();
  assert.equal(relayedOffer.type, 'offer');
  assert.equal(relayedOffer.desc.fp, offerDesc.fp);

  // Negative test: Agent forging a DIFFERENT Answer.PK MUST be rejected by Signal!
  const fakeKP = genEd25519Keypair();
  agConn.ws.sendJSON({
    type: 'answer',
    session_id: relayedOffer.session_id,
    desc: { ...offerDesc, fp: 'b'.repeat(64), setup: 'active' },
    pk: fakeKP.pkB64,
    sig: b64url(Buffer.alloc(64, 1)),
  });
  const errResp = await agConn.ws.nextJSON();
  assert.equal(errResp.type, 'error');
  assert.equal(errResp.code, 'PK_MISMATCH');

  agConn.ws.close();
  clConn.ws.close();

  // 6. Restart Signal server and verify `-bound-keys-file` persisted 'nas' Ed25519 binding!
  proc.kill('SIGTERM');
  await new Promise((r) => proc.once('exit', r));
  assert.ok(fs.existsSync(boundKeysFile), 'bound-keys-file must be written on registration');

  proc = spawnSignal();
  await waitHealthz(port, portalHost);

  const hijackerKP = genEd25519Keypair();
  const hijackConn = await connectRawWS({
    port,
    hostHeader: portalHost,
    reqPath: '/ws/agent/nas',
    headers: { Authorization: 'Bearer per-node-secret-nas' },
  });
  assert.equal(hijackConn.status, 101);
  const hChal = await hijackConn.ws.nextJSON();
  hijackConn.ws.sendJSON({
    type: 'register',
    pk: hijackerKP.pkB64,
    sig: hijackerKP.signRegisterV2('nas', hChal.nonce),
    require_visitor_auth: true,
  });
  const hijackErr = await hijackConn.ws.nextJSON();
  assert.equal(hijackErr.type, 'error');
  assert.equal(hijackErr.code, 'NODE_OWNERSHIP_CONFLICT', 'persisted bound key must block hijack after Signal restart');
  hijackConn.ws.close();
});

test('Go Signal LocalDev Conformance: rejects *.localhost WebSocket CSWSH Origin', async (t) => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'p2p-sig-local-'));
  const binPath = path.join(tmpDir, 'signal');
  execFileSync('go', ['build', '-o', binPath, './cmd/signal'], { cwd: repoRoot, stdio: 'pipe' });

  const port = await getFreePort();
  const proc = spawn(binPath, ['-addr', `127.0.0.1:${port}`, '-allow-anonymous-agents'], { stdio: 'pipe' });
  t.after(() => {
    proc.kill('SIGTERM');
    fs.rmSync(tmpDir, { recursive: true, force: true });
  });
  await waitHealthz(port, `localhost:${port}`);

  // Negative test: rogue subdomain Origin `http://evil.localhost:<port>` must be rejected (403).
  const evilWs = await connectRawWS({
    port,
    hostHeader: `localhost:${port}`,
    reqPath: '/ws/client/nas',
    headers: { Origin: `http://evil.localhost:${port}` },
  });
  assert.equal(evilWs.status, 403, '*.localhost must never be trusted as a Portal Origin');

  // Exact localhost Origin succeeds!
  const goodWs = await connectRawWS({
    port,
    hostHeader: `localhost:${port}`,
    reqPath: '/ws/client/nas',
    headers: { Origin: `http://localhost:${port}` },
  });
  assert.equal(goodWs.status, 101);
  goodWs.ws.close();
});
