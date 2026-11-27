/**
 * Unified Real Chrome CDP End-to-End Test Matrix (V7)
 * Tests REAL compiled `signal` (-tags embedweb, NO -static!) + `agent` + real local HTTP/WS backend
 * + real headless Chrome with OOPIF cross-origin iframe inspection across 5 full suites:
 *
 *  Suite 1: Embedded DistFS + 3-Tier Sandbox Deployment (Local / Tier 1 Fixed / Tier 3 Wildcard)
 *  Suite 2: SPAKE2+, 40x Early 204 POST No-Deadlock, WS send+close, Close Codes 1008/1011 & 2.5 MiB Streaming
 *  Suite 3: Split-Pool CookieMirror Injection/Eviction Defense, Chinese/No-Charset <meta charset> & Attachment Non-Injection
 *  Suite 4: Adversarial CSRF Matrix, Worker/Back/Reload, top.location Sandbox Block, PWA Self-Destruct Defense & Silent Reconnect
 *  Suite 5: Capability Live Revocation, Agent Restart Persistence, StaticFS Hardening & Ed25519 TOFU Red-Screen
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import net from 'node:net';
import path from 'node:path';
import {
  buildV7Binaries,
  createTempWorkdir,
  generateTestCert,
  getFreePort,
  launchHeadlessChrome,
  openPageTab,
  spawnLoggedProcess,
  waitFor,
} from './cdp_harness.mjs';

const workDir = createTempWorkdir();
const { signalBin, agentBin } = buildV7Binaries(workDir);
const tlsCert = generateTestCert(workDir, [
  'portal.example.com',
  'app.example.net',
  'sandbox.example.net',
  '*.sandbox.example.net',
  'evil.example.org',
]);

test.after(() => {
  fs.rmSync(workDir, { recursive: true, force: true });
});

function encodeServerWsFrame(opcode, payload) {
  let hdr;
  if (payload.length < 126) {
    hdr = Buffer.from([0x80 | opcode, payload.length]);
  } else if (payload.length < 65536) {
    hdr = Buffer.alloc(4);
    hdr[0] = 0x80 | opcode;
    hdr[1] = 126;
    hdr.writeUInt16BE(payload.length, 2);
  } else {
    hdr = Buffer.alloc(10);
    hdr[0] = 0x80 | opcode;
    hdr[1] = 127;
    hdr.writeBigUInt64BE(BigInt(payload.length), 2);
  }
  return Buffer.concat([hdr, payload]);
}

function encodeServerWsClose(code, reason) {
  const reasonBuf = Buffer.from(reason, 'utf8');
  const payload = Buffer.alloc(2 + reasonBuf.length);
  payload.writeUInt16BE(code, 0);
  reasonBuf.copy(payload, 2);
  return encodeServerWsFrame(8, payload);
}

function httpsGet(urlStr, caPem, hostHeader) {
  return new Promise((resolve, reject) => {
    const u = new URL(urlStr);
    const req = https.request(
      {
        hostname: '127.0.0.1',
        port: u.port,
        path: `${u.pathname}${u.search}`,
        method: 'GET',
        ca: caPem,
        servername: hostHeader || u.hostname,
        headers: { Host: hostHeader ? `${hostHeader}:${u.port}` : u.host },
      },
      (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => {
          resolve({
            status: res.statusCode || 0,
            headers: res.headers,
            text: Buffer.concat(chunks).toString('utf8'),
          });
        });
      }
    );
    req.on('error', reject);
    req.end();
  });
}

function startMockAppBackend() {
  return new Promise((resolve, reject) => {
    const wsReceivedTexts = [];
    const whoamiRequests = [];
    const postEchoRequests = [];

    const server = http.createServer((req, res) => {
      const url = new URL(req.url || '/', 'http://127.0.0.1');
      let decodedPath = url.pathname;
      try {
        decodedPath = decodeURIComponent(url.pathname);
      } catch {
        // keep raw
      }

      if (url.pathname === '/' || url.pathname === '/index.html') {
        res.writeHead(200, {
          'Content-Type': 'text/html; charset=utf-8',
          'Set-Cookie': [
            'secret_session=httponly-v7-val; Path=/; HttpOnly; SameSite=Strict',
            'theme=dark; Path=/',
          ],
        });
        res.end(`<!doctype html>
<html>
<head><meta charset="utf-8"><title>V7 E2E App</title></head>
<body>
  <h1 id="app-status">READY</h1>
  <script>window.__APP_LOADED__ = true;</script>
</body>
</html>`);
        return;
      }

      if (url.pathname === '/page2') {
        res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
        res.end(`<!doctype html>
<html>
<head><meta charset="utf-8"><title>Page 2</title></head>
<body><h1 id="page2-h1">PAGE2</h1></body>
</html>`);
        return;
      }

      if (url.pathname === '/worker.js') {
        res.writeHead(200, { 'Content-Type': 'application/javascript; charset=utf-8' });
        res.end(`self.onmessage = async () => {
  try {
    const r = await fetch('/whoami');
    const j = await r.json();
    self.postMessage({ ok: r.ok, cookie: j.cookie || '' });
  } catch (err) {
    self.postMessage({ ok: false, error: String(err) });
  }
};`);
        return;
      }

      if (decodedPath === '/目录/中文页') {
        // Intentionally OMIT `charset=` in HTTP Content-Type to verify 1024B `<meta charset="utf-8">` prescan!
        const html = Buffer.from(
          '<!doctype html><html><head><meta charset="utf-8"><title>中文标题</title></head>' +
            '<body><h1 id="cn-h1">你好世界-UTF8</h1></body></html>',
          'utf8'
        );
        res.writeHead(200, {
          'Content-Type': 'text/html',
          'Content-Length': String(html.length),
        });
        res.end(html);
        return;
      }

      if (url.pathname === '/download-html') {
        const body = Buffer.from(
          '<!doctype html><html><head><title>Export</title></head><body>RAW-ATTACHMENT-HTML</body></html>',
          'utf8'
        );
        const rawHttp = Buffer.concat([
          Buffer.from(
            'HTTP/1.1 200 OK\r\n' +
              'Content-Type: text/html; charset=utf-8\r\n' +
              'Content-Disposition: attachment; filename="导出.html"\r\n' +
              `Content-Length: ${body.length}\r\n` +
              'Connection: close\r\n\r\n',
            'utf8'
          ),
          body,
        ]);
        req.socket.end(rawHttp);
        return;
      }

      if (url.pathname === '/whoami') {
        whoamiRequests.push({
          url: req.url || '/',
          cookie: req.headers.cookie || '',
          origin: req.headers.origin || '',
        });
        const payload = Buffer.from(
          JSON.stringify({
            cookie: req.headers.cookie || '',
            xff: req.headers['x-forwarded-for'] || '',
            origin: req.headers.origin || '',
          }),
          'utf8'
        );
        const rawHttp = Buffer.concat([
          Buffer.from(
            'HTTP/1.1 200 OK\r\n' +
              'Content-Type: application/json\r\n' +
              'X-Emoji-Test: ok🚀\r\n' +
              `Content-Length: ${payload.length}\r\n` +
              'Connection: close\r\n\r\n',
            'utf8'
          ),
          payload,
        ]);
        req.socket.end(rawHttp);
        return;
      }

      if (url.pathname === '/early-204') {
        // Respond 204 immediately on POST to stress early-response pool slot cleanup.
        res.writeHead(204);
        res.end();
        return;
      }

      if (url.pathname === '/echo-post') {
        const chunks = [];
        req.on('data', (c) => chunks.push(c));
        req.on('end', () => {
          const body = Buffer.concat(chunks);
          postEchoRequests.push({ url: req.url || '/', len: body.length });
          res.writeHead(200, { 'Content-Type': 'application/json' });
          res.end(body);
        });
        return;
      }

      if (url.pathname === '/download-cn') {
        const body = Buffer.from('chinese-download-ok', 'utf8');
        const rawHttp = Buffer.concat([
          Buffer.from(
            'HTTP/1.1 200 OK\r\n' +
              'Content-Type: text/plain; charset=utf-8\r\n' +
              'Content-Disposition: attachment; filename="测试报告 2026.txt"\r\n' +
              `Content-Length: ${body.length}\r\n` +
              'Connection: close\r\n\r\n',
            'utf8'
          ),
          body,
        ]);
        req.socket.end(rawHttp);
        return;
      }

      if (url.pathname === '/public/ok') {
        res.writeHead(200, { 'Content-Type': 'text/plain' });
        res.end(`public:${req.method}`);
        return;
      }

      if (url.pathname === '/secret') {
        res.writeHead(200, { 'Content-Type': 'text/plain' });
        res.end('top-secret-area');
        return;
      }

      res.writeHead(404);
      res.end('not found');
    });

    server.on('upgrade', (req, socket) => {
      if (req.url !== '/ws') {
        socket.destroy();
        return;
      }
      const key = req.headers['sec-websocket-key'];
      const accept = crypto
        .createHash('sha1')
        .update(`${key}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`)
        .digest('base64');
      socket.write(
        'HTTP/1.1 101 Switching Protocols\r\n' +
          'Upgrade: websocket\r\n' +
          'Connection: Upgrade\r\n' +
          `Sec-WebSocket-Accept: ${accept}\r\n\r\n`
      );

      let buf = Buffer.alloc(0);
      let msgOpcode = 0;
      let msgParts = [];
      let closed = false;
      const safeWrite = (data) => {
        if (!closed && !socket.destroyed && !socket.writableEnded) {
          socket.write(data);
        }
      };
      socket.on('error', () => {
        closed = true;
      });
      socket.on('close', () => {
        closed = true;
      });
      socket.on('data', (chunk) => {
        if (closed) return;
        buf = Buffer.concat([buf, chunk]);
        while (buf.length >= 2) {
          const fin = (buf[0] & 0x80) !== 0;
          const opcode = buf[0] & 0x0f;
          const masked = (buf[1] & 0x80) !== 0;
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
          const maskLen = masked ? 4 : 0;
          if (buf.length < off + maskLen + len) return;
          const mask = masked ? buf.subarray(off, off + 4) : null;
          const raw = Buffer.from(buf.subarray(off + maskLen, off + maskLen + len));
          if (mask) {
            for (let i = 0; i < raw.length; i++) raw[i] ^= mask[i % 4];
          }
          buf = buf.subarray(off + maskLen + len);
          if (opcode === 8) {
            safeWrite(encodeServerWsFrame(8, raw.subarray(0, Math.min(raw.length, 125))));
            closed = true;
            if (!socket.destroyed) socket.end();
            return;
          }
          if (opcode === 1 || opcode === 2) {
            msgOpcode = opcode;
            msgParts = [raw];
          } else if (opcode === 0) {
            msgParts.push(raw);
          }
          if (fin && (msgOpcode === 1 || msgOpcode === 2)) {
            const full = Buffer.concat(msgParts);
            const op = msgOpcode;
            msgOpcode = 0;
            msgParts = [];
            if (op === 1) {
              const txt = full.toString('utf8');
              wsReceivedTexts.push(txt);
              if (txt === 'close:1008') {
                safeWrite(encodeServerWsClose(1008, 'policy-violated'));
                closed = true;
                if (!socket.destroyed) socket.end();
                return;
              }
              if (txt === 'close:1011') {
                safeWrite(encodeServerWsClose(1011, 'internal-err'));
                closed = true;
                if (!socket.destroyed) socket.end();
                return;
              }
              const reply = Buffer.from(`echo:${txt}`, 'utf8');
              safeWrite(encodeServerWsFrame(1, reply));
            } else if (op === 2) {
              safeWrite(encodeServerWsFrame(2, full));
            }
          }
        }
      });
    });

    const sockets = new Set();
    server.on('connection', (sock) => {
      sockets.add(sock);
      sock.once('close', () => sockets.delete(sock));
    });
    server.listen(0, '127.0.0.1', () => {
      const addr = server.address();
      resolve({
        server,
        port: addr.port,
        wsReceivedTexts,
        whoamiRequests,
        postEchoRequests,
        resetLogs: () => {
          wsReceivedTexts.length = 0;
          whoamiRequests.length = 0;
          postEchoRequests.length = 0;
        },
        close: async () => {
          for (const s of sockets) s.destroy();
          server.closeAllConnections?.();
          await new Promise((r) => server.close(r));
        },
      });
    });
    server.on('error', reject);
  });
}

function startLoopbackConnectProxy() {
  return new Promise((resolve, reject) => {
    const server = http.createServer((_req, res) => {
      res.writeHead(405);
      res.end();
    });
    const sockets = new Set();
    server.on('connect', (req, clientSock, head) => {
      const idx = (req.url || '').lastIndexOf(':');
      const port = idx > 0 ? Number.parseInt(req.url.slice(idx + 1), 10) : 443;
      const upstream = net.connect(port, '127.0.0.1', () => {
        clientSock.write('HTTP/1.1 200 Connection Established\r\n\r\n');
        if (head && head.length > 0) upstream.write(head);
        upstream.pipe(clientSock);
        clientSock.pipe(upstream);
      });
      sockets.add(clientSock);
      sockets.add(upstream);
      const cleanup = () => {
        sockets.delete(clientSock);
        sockets.delete(upstream);
        clientSock.destroy();
        upstream.destroy();
      };
      clientSock.on('error', cleanup);
      upstream.on('error', cleanup);
      clientSock.on('close', cleanup);
      upstream.on('close', cleanup);
    });
    server.listen(0, '127.0.0.1', () => {
      resolve({
        port: server.address().port,
        close: async () => {
          for (const s of sockets) s.destroy();
          server.closeAllConnections?.();
          await new Promise((r) => server.close(r));
        },
      });
    });
    server.on('error', reject);
  });
}

function startEvilHttpsOrigin(certPath, keyPath) {
  return new Promise((resolve, reject) => {
    const server = https.createServer(
      { cert: fs.readFileSync(certPath), key: fs.readFileSync(keyPath) },
      (_req, res) => {
        res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
        res.end('<!doctype html><html><body><h1>EVIL SITE</h1></body></html>');
      }
    );
    const sockets = new Set();
    server.on('connection', (s) => {
      sockets.add(s);
      s.once('close', () => sockets.delete(s));
    });
    server.listen(0, '127.0.0.1', () => {
      resolve({
        port: server.address().port,
        close: async () => {
          for (const s of sockets) s.destroy();
          server.closeAllConnections?.();
          await new Promise((r) => server.close(r));
        },
      });
    });
    server.on('error', reject);
  });
}

function chromeHttpsArgs() {
  return [
    '--host-resolver-rules=MAP *.example.com 127.0.0.1, MAP *.example.net 127.0.0.1, MAP *.example.org 127.0.0.1',
    `--ignore-certificate-errors-spki-list=${tlsCert.spkiHash}`,
  ];
}

async function submitOwnerPassword(tab, password) {
  await waitFor(
    () => tab.eval(`Boolean(document.querySelector('.p2p-modal-form input[type="password"]'))`),
    15_000,
    'Owner Auth modal'
  );
  await tab.eval(`(() => {
    const inp = document.querySelector('.p2p-modal-form input[type="password"]');
    inp.value = ${JSON.stringify(password)};
    document.querySelector('.p2p-modal-form').requestSubmit();
  })()`);
}

// ============================================================================
// Suite 1: Embedded DistFS + 3-Tier Deployment (Local / Tier 1 Fixed / Tier 3 Wildcard)
// ============================================================================
test('V7 Suite 1: Embedded DistFS & 3-Tier Sandbox Deployment (Local / Tier 1 Fixed / Tier 3 Wildcard)', async () => {
  const backend = await startMockAppBackend();
  const connectProxy = await startLoopbackConnectProxy();
  const localPort = await getFreePort();
  const tier1Port = await getFreePort();
  const tier3Port = await getFreePort();
  const chromeProfile = path.join(workDir, 'suite1-profile');
  fs.mkdirSync(chromeProfile, { recursive: true });

  const localSig = spawnLoggedProcess(signalBin, [
    '-addr', `127.0.0.1:${localPort}`,
    '-signal-secret', 's1-secret',
  ]);
  const tier1Sig = spawnLoggedProcess(signalBin, [
    '-addr', `127.0.0.1:${tier1Port}`,
    '-portal-domain', `portal.example.com:${tier1Port}`,
    '-sandbox-host', `app.example.net:${tier1Port}`,
    '-tls-cert', tlsCert.certPath,
    '-tls-key', tlsCert.keyPath,
    '-signal-secret', 's1-secret',
  ]);
  const tier3Sig = spawnLoggedProcess(signalBin, [
    '-addr', `127.0.0.1:${tier3Port}`,
    '-portal-domain', `portal.example.com:${tier3Port}`,
    '-sandbox-domain', `sandbox.example.net:${tier3Port}`,
    '-tls-cert', tlsCert.certPath,
    '-tls-key', tlsCert.keyPath,
    '-signal-secret', 's1-secret',
  ]);

  let localAgent = null;
  let tier1Agent = null;
  let tier3Agent = null;
  let chrome = null;

  try {
    await waitFor(async () => {
      const r = await fetch(`http://localhost:${localPort}/healthz`);
      await r.body?.cancel();
      return r.ok;
    }, 10_000, 'local signal /healthz');
    await waitFor(async () => (await httpsGet(`https://portal.example.com:${tier1Port}/healthz`, tlsCert.certPem, 'portal.example.com')).status === 200, 10_000, 'tier1 signal /healthz');
    await waitFor(async () => (await httpsGet(`https://portal.example.com:${tier3Port}/healthz`, tlsCert.certPem, 'portal.example.com')).status === 200, 10_000, 'tier3 signal /healthz');

    // Verify embedded DistFS air-gap, content-hashed assets & strict CSP (zero unsafe-inline):
    const localHome = await fetch(`http://localhost:${localPort}/`);
    assert.equal(localHome.status, 200);
    const localCsp = localHome.headers.get('content-security-policy') || '';
    assert.ok(!localCsp.includes('unsafe-inline'), `Portal CSP must forbid unsafe-inline: ${localCsp}`);
    assert.ok(localCsp.includes("style-src 'self'"), `Portal CSP must pin style-src 'self': ${localCsp}`);
    const localHtml = await localHome.text();
    assert.ok(localHtml.includes('name="p2p-config"'));
    assert.match(localHtml, /\/__p2p_gw__\/portal\.[0-9a-f]{10}\.js/, 'Portal HTML must reference content-hashed portal.js');
    assert.match(localHtml, /\/__p2p_gw__\/portal\.[0-9a-f]{10}\.css/, 'Portal HTML must reference content-hashed portal.css');
    const portalSw = await fetch(`http://localhost:${localPort}/__p2p_gw__/sw.js`);
    assert.equal(portalSw.status, 404, 'Portal origin MUST NOT serve sw.js');
    await portalSw.body?.cancel();
    const sbxSw = await fetch(`http://sbx-check.localhost:${localPort}/__p2p_gw__/sw.js`);
    assert.equal(sbxSw.status, 200, 'Sandbox origin must serve embedded sw.js');
    assert.equal(sbxSw.headers.get('service-worker-allowed'), '/');
    await sbxSw.body?.cancel();

    // Start 3 agents (local ws://, Tier 1 wss:// via CONNECT proxy -> portal.example.com, Tier 3 wss://)
    localAgent = spawnLoggedProcess(agentBin, [
      '-id', 'localnode',
      '-signal', `ws://127.0.0.1:${localPort}/ws`,
      '-signal-secret', 's1-secret',
      '-allow-public',
      '-identity-key', path.join(workDir, 's1-local.key'),
      '-target', `http://127.0.0.1:${backend.port}`,
      '-ice-mdns=false',
    ]);
    const proxyEnv = {
      SSL_CERT_FILE: tlsCert.certPath,
      HTTPS_PROXY: `http://127.0.0.1:${connectProxy.port}`,
      https_proxy: `http://127.0.0.1:${connectProxy.port}`,
      NO_PROXY: '',
      no_proxy: '',
    };
    tier1Agent = spawnLoggedProcess(agentBin, [
      '-id', 'tier1node',
      '-signal', `wss://portal.example.com:${tier1Port}/ws`,
      '-signal-secret', 's1-secret',
      '-allow-public',
      '-identity-key', path.join(workDir, 's1-tier1.key'),
      '-target', `http://127.0.0.1:${backend.port}`,
      '-ice-mdns=false',
    ], { env: proxyEnv });
    tier3Agent = spawnLoggedProcess(agentBin, [
      '-id', 'tier3node',
      '-signal', `wss://portal.example.com:${tier3Port}/ws`,
      '-signal-secret', 's1-secret',
      '-allow-public',
      '-identity-key', path.join(workDir, 's1-tier3.key'),
      '-target', `http://127.0.0.1:${backend.port}`,
      '-ice-mdns=false',
    ], { env: proxyEnv });

    await waitFor(() => localAgent.getLogs().includes('Registered node=localnode'), 10_000, 'localnode reg');
    await waitFor(() => tier1Agent.getLogs().includes('Registered node=tier1node'), 10_000, 'tier1node reg');
    await waitFor(() => tier3Agent.getLogs().includes('Registered node=tier3node'), 10_000, 'tier3node reg');

    chrome = await launchHeadlessChrome(chromeProfile, chromeHttpsArgs());

    // 1A. Local Dev (*.localhost)
    const localTab = await openPageTab(chrome.client, `http://localhost:${localPort}/#localnode`);
    const localSbxSrc = await waitFor(
      () => localTab.eval(`document.querySelector('iframe.p2p-app-frame')?.src || null`),
      15_000,
      'local appFrame'
    );
    assert.match(localSbxSrc, /^http:\/\/sbx-[0-9a-f]{24}\.localhost:/);
    const localOrigin = new URL(localSbxSrc).origin;
    const localReady = await waitFor(
      () => localTab.evalInChildFrame((u) => u.startsWith(localOrigin) && !u.includes('/__p2p_gw__/'), `document.getElementById('app-status')?.textContent === 'READY'`),
      15_000,
      'local READY'
    );
    assert.equal(localReady, true);
    assert.deepEqual(localTab.getGatewayErrors(), []);
    assert.deepEqual(localTab.getExceptions(), []);
    await localTab.close();

    // 1B. Tier 1 Fixed Dual-Domain (https://portal.example.com -> https://app.example.net)
    const tier1Tab = await openPageTab(chrome.client, `https://portal.example.com:${tier1Port}/#tier1node`);
    const tier1SbxSrc = await waitFor(
      () => tier1Tab.eval(`document.querySelector('iframe.p2p-app-frame')?.src || null`),
      15_000,
      'tier1 fixed appFrame'
    );
    const tier1Origin = new URL(tier1SbxSrc).origin;
    assert.equal(tier1Origin, `https://app.example.net:${tier1Port}`, 'Tier 1 MUST mount exact fixed sandboxHost');
    const tier1Ready = await waitFor(
      () => tier1Tab.evalInChildFrame((u) => u.startsWith(tier1Origin) && !u.includes('/__p2p_gw__/'), `document.getElementById('app-status')?.textContent === 'READY'`),
      15_000,
      'tier1 READY'
    );
    assert.equal(tier1Ready, true);
    assert.deepEqual(tier1Tab.getGatewayErrors(), []);
    assert.deepEqual(tier1Tab.getExceptions(), []);
    await tier1Tab.close();

    // 1C. Tier 3 Wildcard Dual-Domain (https://portal.example.com -> https://sbx-<24hex>.sandbox.example.net)
    const tier3Tab = await openPageTab(chrome.client, `https://portal.example.com:${tier3Port}/#tier3node`);
    const tier3SbxSrc = await waitFor(
      () => tier3Tab.eval(`document.querySelector('iframe.p2p-app-frame')?.src || null`),
      15_000,
      'tier3 wildcard appFrame'
    );
    assert.match(tier3SbxSrc, /^https:\/\/sbx-[0-9a-f]{24}\.sandbox\.example\.net:/, 'Tier 3 MUST derive 96-bit HMAC sandbox origin');
    const tier3Origin = new URL(tier3SbxSrc).origin;
    const tier3Ready = await waitFor(
      () => tier3Tab.evalInChildFrame((u) => u.startsWith(tier3Origin) && !u.includes('/__p2p_gw__/'), `document.getElementById('app-status')?.textContent === 'READY'`),
      15_000,
      'tier3 READY'
    );
    assert.equal(tier3Ready, true);
    assert.deepEqual(tier3Tab.getGatewayErrors(), []);
    assert.deepEqual(tier3Tab.getExceptions(), []);
    await tier3Tab.close();
  } finally {
    if (chrome) await chrome.close();
    if (localAgent) await localAgent.stop();
    if (tier1Agent) await tier1Agent.stop();
    if (tier3Agent) await tier3Agent.stop();
    await localSig.stop();
    await tier1Sig.stop();
    await tier3Sig.stop();
    await connectProxy.close();
    await backend.close();
  }
});

// ============================================================================
// Suite 2: SPAKE2+, 40x Early 204 POST, WS send+close, Close Codes 1008/1011 & 2.5MiB Streaming
// Suite 3: Split-Pool CookieMirror Injection/Eviction, Chinese/No-Charset HTML & Attachment Non-Injection
// ============================================================================
test('V7 Suites 2 & 3: SPAKE2+, 40x 204 POST, WS send+close/1008/1011/2.5MiB, Split CookieMirror, Chinese <meta charset> & Attachment', async () => {
  const sigPort = await getFreePort();
  const backend = await startMockAppBackend();
  const chromeProfile = path.join(workDir, 'suite23-profile');
  fs.mkdirSync(chromeProfile, { recursive: true });

  const signalProc = spawnLoggedProcess(signalBin, [
    '-addr', `127.0.0.1:${sigPort}`,
    '-signal-secret', 's23-secret',
  ]);
  let agentProc = null;
  let chrome = null;
  const portalOrigin = `http://localhost:${sigPort}`;

  try {
    await waitFor(async () => {
      const r = await fetch(`${portalOrigin}/healthz`);
      await r.body?.cancel();
      return r.ok;
    }, 10_000, 'signal /healthz');

    agentProc = spawnLoggedProcess(agentBin, [
      '-id', 'demo',
      '-signal', `ws://127.0.0.1:${sigPort}/ws`,
      '-signal-secret', 's23-secret',
      '-access-secret', 'cdp-owner-pass',
      '-identity-key', path.join(workDir, 's23-demo.key'),
      '-target', `http://127.0.0.1:${backend.port}`,
      '-ice-mdns=false',
    ]);
    await waitFor(() => agentProc.getLogs().includes('Registered node=demo'), 10_000, 'agent demo reg');

    chrome = await launchHeadlessChrome(chromeProfile);
    const tab = await openPageTab(chrome.client, `${portalOrigin}/#demo`);
    await submitOwnerPassword(tab, 'cdp-owner-pass');

    const sandboxSrc = await waitFor(
      () => tab.eval(`document.querySelector('iframe.p2p-app-frame')?.src || null`),
      20_000,
      'appFrame'
    );
    const sbxOrigin = new URL(sandboxSrc).origin;
    const isDemoFrame = (u) => u.startsWith(sbxOrigin) && !u.includes('/__p2p_gw__/');
    await waitFor(
      () => tab.evalInChildFrame(isDemoFrame, `document.getElementById('app-status')?.textContent === 'READY'`),
      15_000,
      'app READY'
    );

    // --- Suite 2 checks: 40x early 204 POST, fast JSON POST, WS send+close, 1008/1011 close codes, 2.5 MiB binary ---
    const suite2Res = await tab.evalInChildFrame(isDemoFrame, `(async () => {
      // 1. 40 sequential POSTs returning early 204 No Content (> 32 pool slots!)
      const statuses204 = [];
      for (let i = 0; i < 40; i++) {
        const r = await fetch('/early-204', {
          method: 'POST',
          headers: { 'Content-Type': 'text/plain' },
          body: 'payload-' + i + '-' + 'x'.repeat(1024),
        });
        statuses204.push(r.status);
      }

      // 2. Subsequent normal POST verifies pool slots remain healthy and intact
      const postEcho = await fetch('/echo-post', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ hello: 'v7-post', ok: true }),
      }).then(r => r.json());

      // 3. WS send + immediate close(1000, 'bye') MUST flush message to backend!
      const sendCloseRes = await new Promise((resolve, reject) => {
        const ws = new WebSocket('ws://' + location.host + '/ws');
        const t = setTimeout(() => reject(new Error('send+close timeout')), 10000);
        ws.onopen = () => {
          ws.send('final-msg-before-close');
          ws.close(1000, 'bye');
        };
        ws.onclose = (ev) => {
          clearTimeout(t);
          resolve({ code: ev.code, reason: ev.reason, wasClean: ev.wasClean });
        };
        ws.onerror = () => { clearTimeout(t); reject(new Error('ws send+close error')); };
      });

      // 4. WS RFC 6455 close code preservation (1008 & 1011) with wasClean === true!
      const testCloseCode = (cmd) => new Promise((resolve, reject) => {
        const ws = new WebSocket('ws://' + location.host + '/ws');
        const t = setTimeout(() => reject(new Error(cmd + ' timeout')), 10000);
        ws.onopen = () => ws.send(cmd);
        ws.onclose = (ev) => {
          clearTimeout(t);
          resolve({ code: ev.code, reason: ev.reason, wasClean: ev.wasClean });
        };
        ws.onerror = () => { clearTimeout(t); reject(new Error(cmd + ' error')); };
      });
      const close1008 = await testCloseCode('close:1008');
      const close1011 = await testCloseCode('close:1011');

      // 5. Text + 2.5 MiB binary WS echo (> 2 MiB initial window, 16 KiB streaming!)
      const wsBig = await new Promise((resolve, reject) => {
        const ws = new WebSocket('ws://' + location.host + '/ws');
        ws.binaryType = 'arraybuffer';
        const t = setTimeout(() => reject(new Error('ws 2.5MiB timeout')), 15000);
        let textReply = '';
        ws.onopen = () => ws.send('hello-v7');
        ws.onmessage = (ev) => {
          if (typeof ev.data === 'string') {
            textReply = ev.data;
            const big = new Uint8Array(2.5 * 1024 * 1024);
            big[0] = 0x42;
            big[big.length - 1] = 0x99;
            ws.send(big);
          } else {
            clearTimeout(t);
            const arr = new Uint8Array(ev.data);
            ws.close(1000, 'done');
            resolve({ textReply, bigLen: arr.byteLength, first: arr[0], last: arr[arr.byteLength - 1] });
          }
        };
        ws.onerror = () => { clearTimeout(t); reject(new Error('ws big error')); };
      });

      return { statuses204, postEcho, sendCloseRes, close1008, close1011, wsBig };
    })()`);

    assert.equal(suite2Res.statuses204.length, 40);
    assert.ok(suite2Res.statuses204.every((s) => s === 204), 'all 40 early-204 POSTs must complete without pool deadlock');
    assert.deepEqual(suite2Res.postEcho, { hello: 'v7-post', ok: true });
    assert.equal(suite2Res.sendCloseRes.code, 1000);
    assert.equal(suite2Res.sendCloseRes.wasClean, true);
    await waitFor(() => backend.wsReceivedTexts.includes('final-msg-before-close'), 5_000, 'backend received final-msg-before-close');
    assert.equal(suite2Res.close1008.code, 1008);
    assert.equal(suite2Res.close1008.reason, 'policy-violated');
    assert.equal(suite2Res.close1008.wasClean, true);
    assert.equal(suite2Res.close1011.code, 1011);
    assert.equal(suite2Res.close1011.reason, 'internal-err');
    assert.equal(suite2Res.close1011.wasClean, true);
    assert.equal(suite2Res.wsBig.textReply, 'echo:hello-v7');
    assert.equal(suite2Res.wsBig.bigLen, 2.5 * 1024 * 1024);
    assert.equal(suite2Res.wsBig.first, 0x42);
    assert.equal(suite2Res.wsBig.last, 0x99);

    // --- Suite 3 checks: Split-Pool CookieMirror injection/eviction, RFC 5987, HTML Attachment non-injection ---
    const suite3Res = await tab.evalInChildFrame(isDemoFrame, `(async () => {
      let parentBlocked = false;
      try { void window.parent.document.title; } catch { parentBlocked = true; }
      const initialDocCookie = document.cookie;

      // Normal JS set & delete
      document.cookie = 'js_tok=v7ok; Path=/';
      const whoSet = await fetch('/whoami', { headers: { 'X-Forwarded-For': '203.0.113.99' } }).then(r => r.json());
      document.cookie = 'js_tok=; Max-Age=0; Path=/';
      const whoDel = await fetch('/whoami').then(r => r.json());

      // Adversarial JS cookie attacks: semicolon injection, HttpOnly overwrite, and 40-cookie flood.
      document.cookie = 'evil=1; secret_session=hacked; Path=/';
      document.cookie = 'secret_session=forged; Path=/';
      for (let i = 0; i < 40; i++) {
        document.cookie = 'flood_' + i + '=val_' + i + '; Path=/';
      }
      const whoAfterAttack = await fetch('/whoami').then(r => r.json());

      // Non-ASCII RFC 5987 text download
      const dlResp = await fetch('/download-cn');
      const dlText = await dlResp.text();
      const dlCD = dlResp.headers.get('content-disposition') || '';

      // HTML Attachment download MUST NOT inject patch.js or leak data-p2p-boot!
      const htmlAttResp = await fetch('/download-html');
      const htmlAttText = await htmlAttResp.text();
      const htmlAttCD = htmlAttResp.headers.get('content-disposition') || '';

      return { parentBlocked, initialDocCookie, whoSet, whoDel, whoAfterAttack, dlStatus: dlResp.status, dlText, dlCD, htmlAttStatus: htmlAttResp.status, htmlAttText, htmlAttCD };
    })()`);

    assert.equal(suite3Res.parentBlocked, true);
    assert.ok(!suite3Res.initialDocCookie.includes('secret_session'));
    assert.ok(suite3Res.initialDocCookie.includes('theme=dark'));
    assert.ok(suite3Res.whoSet.cookie.includes('secret_session=httponly-v7-val'));
    assert.ok(suite3Res.whoSet.cookie.includes('js_tok=v7ok'));
    assert.notEqual(suite3Res.whoSet.xff, '203.0.113.99');
    assert.ok(!suite3Res.whoDel.cookie.includes('js_tok='));
    assert.ok(suite3Res.whoDel.cookie.includes('secret_session=httponly-v7-val'));
    // Assertions: HttpOnly server cookie is never evicted, overwritten, or duplicated.
    const pairs = suite3Res.whoAfterAttack.cookie.split('; ').filter(Boolean);
    const secretPairs = pairs.filter((p) => p.startsWith('secret_session='));
    assert.deepEqual(secretPairs, ['secret_session=httponly-v7-val'], `secret_session must survive injection & 40-cookie flood: ${suite3Res.whoAfterAttack.cookie}`);
    assert.equal(suite3Res.dlStatus, 200);
    assert.equal(suite3Res.dlText, 'chinese-download-ok');
    assert.ok(suite3Res.dlCD.includes("filename*=UTF-8''"));
    assert.equal(suite3Res.htmlAttStatus, 200);
    assert.ok(suite3Res.htmlAttCD.includes("filename*=UTF-8''"));
    assert.ok(suite3Res.htmlAttText.includes('RAW-ATTACHMENT-HTML'));
    assert.ok(!suite3Res.htmlAttText.includes('patch.js') && !suite3Res.htmlAttText.includes('data-p2p-boot'), 'HTML attachment MUST NOT receive patch.js or data-p2p-boot');

    // Suite 3 Chinese path + no HTTP charset=<meta charset="utf-8"> prescan verification:
    const cnTab = await openPageTab(
      chrome.client,
      `${portalOrigin}/#demo?path=${encodeURIComponent('/目录/中文页')}`
    );
    await submitOwnerPassword(cnTab, 'cdp-owner-pass');
    const cnH1 = await waitFor(
      () => cnTab.evalInChildFrame(
        (u) => u.startsWith(sbxOrigin) && !u.includes('/__p2p_gw__/') && decodeURIComponent(new URL(u).pathname) === '/目录/中文页',
        `document.getElementById('cn-h1')?.textContent || ''`
      ),
      20_000,
      'Chinese no-HTTP-charset page rendered'
    );
    assert.equal(cnH1, '你好世界-UTF8', '1024B <meta charset="utf-8"> prescan must prevent UTF-8 mojibake');
    assert.deepEqual(cnTab.getGatewayErrors(), []);
    assert.deepEqual(cnTab.getExceptions(), []);
    await cnTab.close();

    assert.deepEqual(tab.getGatewayErrors(), []);
    assert.deepEqual(tab.getExceptions(), []);
    await tab.close();
  } finally {
    if (chrome) await chrome.close();
    if (agentProc) await agentProc.stop();
    await signalProc.stop();
    await backend.close();
  }
});

// ============================================================================
// Suite 4: Adversarial CSRF Matrix, Worker/Back/Reload, top.location Sandbox Block,
//          PWA Self-Destruct Defense & Unified Silent Reconnect
// ============================================================================
test('V7 Suite 4: Adversarial CSRF Matrix, Worker/Back/Reload, top.location Block, PWA Defense & Silent Reconnect', async () => {
  const sigPort = await getFreePort();
  const backend = await startMockAppBackend();
  const connectProxy = await startLoopbackConnectProxy();
  const evilServer = await startEvilHttpsOrigin(tlsCert.certPath, tlsCert.keyPath);
  const chromeProfile = path.join(workDir, 'suite4-profile');
  fs.mkdirSync(chromeProfile, { recursive: true });

  // Run Tier 1 Fixed Dual-Domain so the evil attacker knows the EXACT fixed sandbox origin (`https://app.example.net`)!
  const signalProc = spawnLoggedProcess(signalBin, [
    '-addr', `127.0.0.1:${sigPort}`,
    '-portal-domain', `portal.example.com:${sigPort}`,
    '-sandbox-host', `app.example.net:${sigPort}`,
    '-tls-cert', tlsCert.certPath,
    '-tls-key', tlsCert.keyPath,
    '-signal-secret', 's4-secret',
  ]);
  const keyPath = path.join(workDir, 's4-demo.key');
  const startDemoAgent = () =>
    spawnLoggedProcess(
      agentBin,
      [
        '-id', 'demo',
        '-signal', `wss://portal.example.com:${sigPort}/ws`,
        '-signal-secret', 's4-secret',
        '-access-secret', 's4-owner-pass',
        '-identity-key', keyPath,
        '-target', `http://127.0.0.1:${backend.port}`,
        '-ice-mdns=false',
      ],
      {
        env: {
          SSL_CERT_FILE: tlsCert.certPath,
          HTTPS_PROXY: `http://127.0.0.1:${connectProxy.port}`,
          https_proxy: `http://127.0.0.1:${connectProxy.port}`,
          NO_PROXY: '',
          no_proxy: '',
        },
      }
    );

  let agentProc = null;
  let chrome = null;
  const portalOrigin = `https://portal.example.com:${sigPort}`;
  const sandboxOrigin = `https://app.example.net:${sigPort}`;
  const evilOrigin = `https://evil.example.org:${evilServer.port}`;

  try {
    await waitFor(
      async () => (await httpsGet(`${portalOrigin}/healthz`, tlsCert.certPem, 'portal.example.com')).status === 200,
      10_000,
      'tier1 signal /healthz'
    );
    agentProc = startDemoAgent();
    await waitFor(() => agentProc.getLogs().includes('Registered node=demo'), 10_000, 's4 agent reg');

    chrome = await launchHeadlessChrome(chromeProfile, chromeHttpsArgs());
    const tab = await openPageTab(chrome.client, `${portalOrigin}/#demo`);
    await submitOwnerPassword(tab, 's4-owner-pass');

    await waitFor(
      () => tab.eval(`document.querySelector('iframe.p2p-app-frame')?.src?.startsWith(${JSON.stringify(sandboxOrigin)}) || false`),
      20_000,
      's4 appFrame mounted'
    );
    const isDemoFrame = (u) => u.startsWith(sandboxOrigin) && !u.includes('/__p2p_gw__/');
    await waitFor(
      () => tab.evalInChildFrame(isDemoFrame, `document.getElementById('app-status')?.textContent === 'READY'`),
      15_000,
      's4 app READY'
    );

    // 4A. Positive Legitimate Flows inside appFrame:
    //     - Same-origin Web Worker fetch('/whoami')
    //     - Navigation /page2 -> history.back() -> location.reload()
    //     - top.location block via iframe sandbox attribute
    //     - PWA navigator.serviceWorker self-destruct defense
    const workerRes = await tab.evalInChildFrame(isDemoFrame, `new Promise((resolve, reject) => {
      const w = new Worker('/worker.js');
      const t = setTimeout(() => reject(new Error('worker timeout')), 10000);
      w.onmessage = (ev) => { clearTimeout(t); w.terminate(); resolve(ev.data); };
      w.onerror = (err) => { clearTimeout(t); reject(new Error('worker error: ' + err.message)); };
      w.postMessage('go');
    })`);
    assert.equal(workerRes.ok, true, `same-origin Web Worker fetch must succeed: ${JSON.stringify(workerRes)}`);
    assert.ok(workerRes.cookie.includes('secret_session=httponly-v7-val'), 'same-origin Worker must inherit SameOriginClient cookie context');

    // Navigate -> /page2 -> history.back() -> location.reload()
    await tab.evalInChildFrame(isDemoFrame, `location.href = '/page2';`);
    await waitFor(
      () => tab.evalInChildFrame(isDemoFrame, `document.getElementById('page2-h1')?.textContent === 'PAGE2'`),
      10_000,
      'appFrame navigate /page2'
    );
    await tab.evalInChildFrame(isDemoFrame, `history.back();`);
    await waitFor(
      () => tab.evalInChildFrame(isDemoFrame, `document.getElementById('app-status')?.textContent === 'READY'`),
      10_000,
      'appFrame history.back()'
    );
    await tab.evalInChildFrame(isDemoFrame, `location.reload();`);
    await waitFor(
      () => tab.evalInChildFrame(isDemoFrame, `document.getElementById('app-status')?.textContent === 'READY'`),
      10_000,
      'appFrame location.reload()'
    );

    // top.location sandbox defense + PWA navigator.serviceWorker defense:
    const sandboxAndPwa = await tab.evalInChildFrame(isDemoFrame, `(async () => {
      let topBlocked = false;
      try {
        window.top.location.href = 'https://evil.example.org/';
      } catch {
        topBlocked = true;
      }
      const regs = await navigator.serviceWorker.getRegistrations();
      let regRejected = false;
      try {
        await navigator.serviceWorker.register('/evil-sw.js');
      } catch {
        regRejected = true;
      }
      const postPwaStatus = await fetch('/whoami').then(r => r.status);
      return { topBlocked, regCount: regs.length, regRejected, postPwaStatus };
    })()`);
    assert.equal(sandboxAndPwa.topBlocked, true, 'appFrame sandbox must block top.location navigation');
    assert.equal(await tab.eval(`location.origin`), portalOrigin, 'Portal origin must remain unchanged');
    assert.equal(sandboxAndPwa.regCount, 0, 'PWA getRegistrations() must hide gateway SW');
    assert.equal(sandboxAndPwa.regRejected, true, 'PWA register() must reject cleanly');
    assert.equal(sandboxAndPwa.postPwaStatus, 200, 'Tunnel must remain alive after PWA SW probes');

    // 4B. Adversarial Cross-Site CSRF Attacks:
    //     Layer 1 (External Site `https://evil.example.org`): blocked by Chrome Storage Partitioning + Signal air-gap (404/403), zero backend hits!
    //     Layer 2 (Same-Partition Unbound Probes from a separate `portal.example.com` page, simulating non-partitioned browsers):
    //             MUST hit SW Anti-CSRF Gate and enforce exact 403 / SameSite=Strict isolation / multi-tab 403!
    backend.resetLogs();
    const evilTab = await openPageTab(chrome.client, `${evilOrigin}/`);
    await waitFor(() => evilTab.eval(`document.body?.textContent?.includes('EVIL SITE')`), 5_000, 'evil site loaded');

    // Layer 1a: External cross-site fetch -> blocked (403 or 404) & never reaches backend!
    evilTab.clearCapturedResponses();
    await evilTab.eval(`fetch(${JSON.stringify(`${sandboxOrigin}/whoami?atk=evil-fetch`)}, { mode: 'no-cors' }).catch(() => {})`);
    const evilFetchResp = await waitFor(
      () => evilTab.getCapturedResponses().find((r) => r.url === `${sandboxOrigin}/whoami?atk=evil-fetch`),
      8_000,
      'evil cross-site fetch blocked'
    );
    assert.ok(evilFetchResp.status === 403 || evilFetchResp.status === 404, `external cross-site fetch must be blocked (403/404), got ${evilFetchResp.status}`);
    assert.equal(backend.whoamiRequests.some((r) => r.url.includes('atk=evil-fetch')), false);

    // Layer 1b: External cross-site <form method="POST"> -> blocked (403/404) & never reaches backend!
    evilTab.clearCapturedResponses();
    await evilTab.eval(`(() => {
      const f = document.createElement('iframe');
      f.name = 'evil-post-sink';
      document.body.appendChild(f);
      const form = document.createElement('form');
      form.method = 'POST';
      form.action = ${JSON.stringify(`${sandboxOrigin}/echo-post?atk=evil-formpost`)};
      form.target = 'evil-post-sink';
      document.body.appendChild(form);
      form.submit();
    })()`);
    const evilFormResp = await waitFor(
      () => evilTab.getCapturedResponses().find((r) => r.url.includes('atk=evil-formpost')),
      8_000,
      'evil cross-site form POST blocked'
    );
    assert.ok([403, 404, 405].includes(evilFormResp.status), `external form POST must be blocked (403/404/405), got ${evilFormResp.status}`);
    assert.equal(backend.postEchoRequests.length, 0);

    // Layer 1c: Direct top-level document navigation (`window.open` / `destination: 'document'`) -> blocked (403/404)!
    const directDocTab = await openPageTab(chrome.client, `${sandboxOrigin}/whoami?atk=doc`);
    const atkDocResp = await waitFor(
      () => directDocTab.getCapturedResponses().find((r) => r.url === `${sandboxOrigin}/whoami?atk=doc`),
      8_000,
      'direct document nav blocked'
    );
    assert.ok(atkDocResp.status === 403 || atkDocResp.status === 404, `unbound document nav must be blocked (403/404), got ${atkDocResp.status}`);
    assert.equal(backend.whoamiRequests.some((r) => r.url.includes('atk=doc')), false);
    await directDocTab.close();
    await evilTab.close();

    // Layer 2: Same-Partition SW Anti-CSRF Gate verification (exercises SW FetchEvent directly without Storage Partitioning masking it!)
    const swProbeTab = await openPageTab(chrome.client, `${portalOrigin}/`);
    await waitFor(() => swProbeTab.eval(`document.readyState === 'complete'`), 5_000, 'swProbeTab ready');

    // 2a. Unbound subresource fetch from unbound `bridge.html` inside the exact SW partition -> SW MUST return exact 403!
    const bridgeFetchStatus = await tab.evalInChildFrame(
      (u) => u.startsWith(sandboxOrigin) && u.includes('/__p2p_gw__/bridge.html'),
      `fetch('/whoami?atk=sw-fetch').then(r => r.status)`
    );
    assert.equal(bridgeFetchStatus, 403, 'SW Anti-CSRF Gate must return exact 403 for unbound subresource fetch');
    assert.equal(backend.whoamiRequests.some((r) => r.url.includes('atk=sw-fetch')), false);

    // 2b. Unbound navigate <form method="POST"> from a disposable unbound iframe in the same SW partition -> SW MUST return exact 403!
    await swProbeTab.eval(`(() => {
      const f = document.createElement('iframe');
      f.src = ${JSON.stringify(`${sandboxOrigin}/__p2p_gw__/bridge.html?tab=disposable-post`)};
      document.body.appendChild(f);
    })()`);
    const isDisposableBridge = (u) => u === `${sandboxOrigin}/__p2p_gw__/bridge.html?tab=disposable-post`;
    await waitFor(
      () => swProbeTab.evalInChildFrame(isDisposableBridge, `document.readyState === 'complete'`),
      8_000,
      'disposable bridge iframe ready'
    );
    swProbeTab.clearCapturedResponses();
    await swProbeTab.evalInChildFrame(isDisposableBridge, `(() => {
      const form = document.createElement('form');
      form.method = 'POST';
      form.action = '/echo-post?atk=sw-formpost';
      document.body.appendChild(form);
      form.submit();
    })()`);
    const swFormResp = await waitFor(
      () => swProbeTab.getCapturedResponses().find((r) => r.url.includes('atk=sw-formpost')),
      8_000,
      'SW unbound form POST 403'
    );
    assert.equal(swFormResp.status, 403, 'SW Anti-CSRF Gate must return exact 403 for unbound form POST');
    assert.equal(backend.postEchoRequests.length, 0);

    // 2c. Unbound <iframe referrerpolicy="no-referrer"> while sessions.size === 1:
    //     MUST NOT leak SameSite=Strict `secret_session` AND response MUST carry CSP `frame-ancestors`!
    swProbeTab.clearCapturedResponses();
    await swProbeTab.eval(`(() => {
      const f = document.createElement('iframe');
      f.referrerPolicy = 'no-referrer';
      f.src = ${JSON.stringify(`${sandboxOrigin}/whoami?atk=iframe-single`)};
      document.body.appendChild(f);
    })()`);
    const singleIframeResp = await waitFor(
      () => swProbeTab.getCapturedResponses().find((r) => r.url === `${sandboxOrigin}/whoami?atk=iframe-single`),
      8_000,
      'SW single-tab no-referrer iframe response'
    );
    assert.match(
      singleIframeResp.headers['content-security-policy'] || '',
      new RegExp(`frame-ancestors 'self' ${portalOrigin.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}`),
      'iframe response MUST include CSP frame-ancestors pinning Portal'
    );
    const singleHit = backend.whoamiRequests.find((r) => r.url.includes('atk=iframe-single'));
    assert.ok(singleHit, 'single-tab empty-referrer iframe should reach backend without SameSite=Strict');
    assert.ok(
      !singleHit.cookie.includes('secret_session='),
      `SameSite=Strict cookie MUST NOT leak on empty-referrer iframe: ${singleHit.cookie}`
    );

    // 2d. Open 2nd Portal tab (sessions.size === 2 on Tier 1 fixed origin!) ->
    //     Unbound <iframe referrerpolicy="no-referrer"> MUST get immediate exact 403 without reaching backend!
    const tab2 = await openPageTab(chrome.client, `${portalOrigin}/#demo`);
    await submitOwnerPassword(tab2, 's4-owner-pass');
    await waitFor(
      () => tab2.eval(`document.querySelector('iframe.p2p-app-frame')?.src?.startsWith(${JSON.stringify(sandboxOrigin)}) || false`),
      20_000,
      'second Portal tab connected'
    );

    swProbeTab.clearCapturedResponses();
    await swProbeTab.eval(`(() => {
      const f = document.createElement('iframe');
      f.referrerPolicy = 'no-referrer';
      f.src = ${JSON.stringify(`${sandboxOrigin}/whoami?atk=iframe-multi`)};
      document.body.appendChild(f);
    })()`);
    const multiIframeResp = await waitFor(
      () => swProbeTab.getCapturedResponses().find((r) => r.url === `${sandboxOrigin}/whoami?atk=iframe-multi`),
      8_000,
      'SW multi-tab no-referrer iframe 403'
    );
    assert.equal(multiIframeResp.status, 403, 'Tier 1 multi-tab (sessions.size > 1) MUST reject unbound iframe with exact 403');
    assert.equal(backend.whoamiRequests.some((r) => r.url.includes('atk=iframe-multi')), false, 'multi-tab probe must never reach backend');
    await tab2.close();
    await swProbeTab.close();
    await waitFor(
      async () => {
        const { targetInfos } = await chrome.client.send('Target.getTargets');
        const matches = targetInfos.filter((t) => (t.type === 'iframe' || t.type === 'page') && isDemoFrame(t.url));
        return matches.length === 1;
      },
      5_000,
      'tab2 appFrame target cleanup'
    );

    // 4C. Unified Silent Reconnect: restart Agent -> appFrame stays alive & zero password re-prompt!
    await tab.evalInChildFrame(isDemoFrame, `window.__FRAME_SURVIVED_RECONNECT__ = 'yes';`);
    await agentProc.stop();
    agentProc = startDemoAgent();
    await waitFor(() => agentProc.getLogs().includes('Registered node=demo'), 10_000, 'restarted s4 agent reg');

    let lastReconnectStatus = null;
    let lastReconnectErr = null;
    let reconnectedOk = false;
    try {
      reconnectedOk = await waitFor(
        async () => {
          try {
            const st = await tab.evalInChildFrame(isDemoFrame, `fetch('/whoami?after=reconnect').then(r => r.status)`);
            lastReconnectStatus = st;
            lastReconnectErr = null;
            return st === 200;
          } catch (err) {
            lastReconnectErr = err instanceof Error ? err.message : String(err);
            return false;
          }
        },
        25_000,
        'silent reconnect recovery'
      );
    } catch (err) {
      const portalLogs = await tab
        .eval(`Array.from(document.querySelectorAll('.p2p-log-line')).map(e => e.textContent).join('\\n')`)
        .catch((e) => `<unavailable: ${e.message}>`);
      throw new Error(
        `${err instanceof Error ? err.message : String(err)}\n` +
          `lastStatus=${JSON.stringify(lastReconnectStatus)} lastErr=${JSON.stringify(lastReconnectErr)}\n` +
          `--- PORTAL LOGS ---\n${portalLogs}\n` +
          `--- AGENT2 LOGS ---\n${agentProc.getLogs()}\n` +
          `--- SIGNAL LOGS ---\n${signalProc.getLogs()}`
      );
    }
    assert.equal(reconnectedOk, true);
    const survivedMarker = await tab.evalInChildFrame(isDemoFrame, `window.__FRAME_SURVIVED_RECONNECT__`);
    assert.equal(survivedMarker, 'yes', 'Silent reconnect MUST keep appFrame mounted without reload');
    const authModalReopened = await tab.eval(`Boolean(document.querySelector('.p2p-modal-form input[type="password"]'))`);
    assert.equal(authModalReopened, false, 'Silent reconnect MUST reuse in-memory SPAKE2+ verifier without re-prompting password');

    assert.deepEqual(tab.getGatewayErrors(), []);
    assert.deepEqual(tab.getExceptions(), []);
    await tab.close();
  } finally {
    if (chrome) await chrome.close();
    if (agentProc) await agentProc.stop();
    await signalProc.stop();
    await connectProxy.close();
    await evilServer.close();
    await backend.close();
  }
});

// ============================================================================
// Suite 5: Capability Live Revocation, Agent Restart Persistence,
//          StaticFS Hardening & Ed25519 TOFU Key Rotation Red-Screen
// ============================================================================
test('V7 Suite 5: Capability Live Revocation, Restart Persistence, StaticFS Hardening & TOFU Red-Screen', async () => {
  const sigPort = await getFreePort();
  const backend = await startMockAppBackend();
  const chromeProfile = path.join(workDir, 'suite5-profile');
  fs.mkdirSync(chromeProfile, { recursive: true });

  const staticDir = path.join(workDir, 'suite5-static-root');
  fs.mkdirSync(staticDir, { recursive: true });
  fs.writeFileSync(path.join(staticDir, '100%_report.txt'), 'percent-file-ok');
  fs.writeFileSync(path.join(staticDir, '.env'), 'SECRET=NEVER_EXPOSE');
  fs.symlinkSync('.env', path.join(staticDir, 'readme'));
  fs.symlinkSync('.env', path.join(staticDir, 'hop2'));
  fs.symlinkSync('hop2', path.join(staticDir, 'hop1'));
  fs.writeFileSync(path.join(staticDir, 'payload'), '<!doctype html><html><body><script>alert(1)</script></body></html>');
  fs.writeFileSync(
    path.join(staticDir, 'index.html'),
    '<!doctype html><html><body><h1 id="static-h1">STATIC-OK</h1><script>window.__INLINE_RAN__=true;</script></body></html>'
  );

  const signalProc = spawnLoggedProcess(signalBin, [
    '-addr', `127.0.0.1:${sigPort}`,
    '-signal-secret', 's5-secret',
    '-allow-force-rekey',
  ]);
  const keyPath1 = path.join(workDir, 's5-key-1.ed25519.key');
  const keyPath2 = path.join(workDir, 's5-key-2.ed25519.key');
  const statePath = path.join(workDir, 's5-demo-state.json');
  const staticKeyPath = path.join(workDir, 's5-static.ed25519.key');

  const startDemoAgent = (keyPath, extraFlags = []) =>
    spawnLoggedProcess(agentBin, [
      '-id', 'demo',
      '-signal', `ws://127.0.0.1:${sigPort}/ws`,
      '-signal-secret', 's5-secret',
      '-access-secret', 's5-owner-pass',
      '-identity-key', keyPath,
      '-state-file', statePath,
      '-target', `http://127.0.0.1:${backend.port}`,
      '-ice-mdns=false',
      ...extraFlags,
    ]);

  let agentProc = null;
  let staticAgentProc = null;
  let chrome = null;
  const portalOrigin = `http://localhost:${sigPort}`;

  try {
    await waitFor(async () => {
      const r = await fetch(`${portalOrigin}/healthz`);
      await r.body?.cancel();
      return r.ok;
    }, 10_000, 's5 signal /healthz');

    agentProc = startDemoAgent(keyPath1);
    staticAgentProc = spawnLoggedProcess(agentBin, [
      '-id', 'staticnode',
      '-signal', `ws://127.0.0.1:${sigPort}/ws`,
      '-signal-secret', 's5-secret',
      '-allow-public',
      '-identity-key', staticKeyPath,
      '-dir', staticDir,
      '-ice-mdns=false',
    ]);
    await waitFor(() => agentProc.getLogs().includes('Registered node=demo'), 10_000, 's5 demo reg');
    await waitFor(() => staticAgentProc.getLogs().includes('Registered node=staticnode'), 10_000, 's5 static reg');

    chrome = await launchHeadlessChrome(chromeProfile);
    const ownerTab = await openPageTab(chrome.client, `${portalOrigin}/#demo`);
    await submitOwnerPassword(ownerTab, 's5-owner-pass');

    const ownerSbxSrc = await waitFor(
      () => ownerTab.eval(`document.querySelector('iframe.p2p-app-frame')?.src || null`),
      20_000,
      'owner appFrame'
    );
    const ownerOrigin = new URL(ownerSbxSrc).origin;

    // 5A. Mint scoped read-only capability `/public` via Share Modal
    await ownerTab.eval(`(() => {
      document.querySelector('.p2p-pill-btn').click();
      document.querySelector('.p2p-pill-actions .p2p-btn-primary').click();
    })()`);
    await waitFor(() => ownerTab.eval(`Boolean(document.querySelector('.p2p-modal-form input[type="text"]'))`), 5_000, 'Share modal');
    await ownerTab.eval(`(() => {
      const form = document.querySelector('.p2p-modal-form');
      form.querySelector('input[type="text"]').value = '/public';
      form.querySelector('input[type="checkbox"]').checked = true;
      form.requestSubmit();
    })()`);
    const shareLink = await waitFor(
      () => ownerTab.eval(`(() => {
        const el = document.querySelector('.p2p-share-result input');
        return el && el.value.includes('&cap=') ? el.value : null;
      })()`),
      8_000,
      'minted capability link'
    );
    assert.ok(shareLink.includes('#demo?pk=') && shareLink.includes('&cap='));

    const capTab = await openPageTab(chrome.client, `${shareLink}&path=%2Fpublic%2Fok`);
    const capSbxSrc = await waitFor(
      () => capTab.eval(`document.querySelector('iframe.p2p-app-frame')?.src || null`),
      15_000,
      'capability appFrame'
    );
    const capOrigin = new URL(capSbxSrc).origin;
    assert.notEqual(capOrigin, ownerOrigin, 'Capability visitor must get isolated ephemeral sandbox origin');
    assert.ok(!(await capTab.eval(`location.hash`)).includes('cap='), 'cap= MUST be scrubbed from location.hash');

    const isCapFrame = (u) => u.startsWith(capOrigin) && !u.includes('/__p2p_gw__/');
    const capResults = await waitFor(
      () => capTab.evalInChildFrame(isCapFrame, `(async () => {
        const getOk = await fetch('/public/ok').then(async r => ({ s: r.status, t: await r.text() }));
        const postBlocked = await fetch('/public/ok', { method: 'POST' }).then(r => r.status);
        const secretBlocked = await fetch('/secret').then(r => r.status);
        return { getOk, postBlocked, secretBlocked };
      })()`),
      15_000,
      'capability scope checks'
    );
    assert.equal(capResults.getOk.s, 200);
    assert.equal(capResults.getOk.t, 'public:GET');
    assert.equal(capResults.postBlocked, 403);
    assert.equal(capResults.secretBlocked, 403);

    // 5B. Live Capability Revocation -> active visitor session MUST fail immediately!
    await waitFor(() => ownerTab.eval(`Boolean(document.querySelector('.p2p-cap-row .p2p-btn-danger'))`), 5_000, 'Revoke button');
    await ownerTab.eval(`document.querySelector('.p2p-cap-row .p2p-btn-danger').click()`);
    await waitFor(() => ownerTab.eval(`Boolean(document.querySelector('.p2p-cap-row .p2p-badge-muted'))`), 8_000, 'Revoked badge');

    const revokedImmediateFail = await waitFor(
      async () => {
        try {
          const st = await capTab.evalInChildFrame(isCapFrame, `fetch('/public/ok?after=revoke').then(r => r.status).catch(() => -1)`);
          return st !== 200;
        } catch {
          return true;
        }
      },
      10_000,
      'live capability revocation enforcement'
    );
    assert.equal(revokedImmediateFail, true, 'Revoked capability MUST immediately fail on live session');
    await capTab.close();

    // 5C. Agent Restart Persistence: restart Agent with same -state-file -> revoked token stays permanently rejected.
    await agentProc.stop();
    agentProc = startDemoAgent(keyPath1);
    await waitFor(() => agentProc.getLogs().includes('Registered node=demo'), 10_000, 'restarted s5 demo reg');

    const capTabAfterRestart = await openPageTab(chrome.client, `${shareLink}&path=%2Fpublic%2Fok`);
    const restartErrorShown = await waitFor(
      () => capTabAfterRestart.eval(`(() => {
        const b = document.querySelector('#statusBanner, .p2p-status-banner');
        const txt = (b && b.style.display !== 'none') ? (b.textContent || '') : '';
        return /revok|denied|auth|failed|reject|403|error/i.test(txt) ? txt : null;
      })()`),
      15_000,
      'revoked token rejection banner after Agent restart'
    );
    assert.ok(restartErrorShown, 'Revoked capability must stay rejected after Agent restart');
    assert.equal(
      await capTabAfterRestart.eval(`Boolean(document.querySelector('iframe.p2p-app-frame'))`),
      false,
      'Revoked capability MUST NOT mount appFrame after Agent restart'
    );
    await capTabAfterRestart.close();

    // 5D. Ed25519 TOFU Key Rotation Red-Screen Hard Block
    await agentProc.stop();
    agentProc = startDemoAgent(keyPath2, ['-force-rekey']);
    await waitFor(() => agentProc.getLogs().includes('Registered node=demo'), 10_000, 'rekeyed demo reg');

    await ownerTab.reload();
    const tofuBlocked = await waitFor(
      () => ownerTab.eval(`Boolean(document.querySelector('.p2p-modal-card-danger'))`),
      15_000,
      'TOFU key mismatch red-screen modal'
    );
    assert.equal(tofuBlocked, true);
    assert.equal(await ownerTab.eval(`Boolean(document.querySelector('iframe.p2p-app-frame'))`), false);
    assert.deepEqual(ownerTab.getGatewayErrors(), []);
    assert.deepEqual(ownerTab.getExceptions(), []);
    await ownerTab.close();

    // 5E. StaticFS os.Root Hardening: %, .env 404, 1-hop & 2-hop symlink 404, extensionless HTML CSP: sandbox
    const staticTab = await openPageTab(chrome.client, `${portalOrigin}/#staticnode`);
    const staticSbxSrc = await waitFor(
      () => staticTab.eval(`document.querySelector('iframe.p2p-app-frame')?.src || null`),
      15_000,
      'staticnode appFrame'
    );
    const staticOrigin = new URL(staticSbxSrc).origin;
    const isStaticApp = (u) => u.startsWith(staticOrigin) && !u.includes('/__p2p_gw__/');

    const idxResp = await waitFor(
      () => staticTab.getCapturedResponses().find((r) => r.url === `${staticOrigin}/`),
      15_000,
      'staticnode / response'
    );
    assert.equal(idxResp.status, 200);
    assert.match(idxResp.headers['content-security-policy'] || '', /\bsandbox\b/i);

    const staticDom = await waitFor(
      () => staticTab.evalInChildFrame(isStaticApp, `({ h1: document.getElementById('static-h1')?.textContent || '', inlineRan: Boolean(window.__INLINE_RAN__) })`),
      15_000,
      'staticnode index.html DOM'
    );
    assert.equal(staticDom.h1, 'STATIC-OK');
    assert.equal(staticDom.inlineRan, false, 'CSP: sandbox MUST block inline script');

    const navStatic = async (reqPath) => {
      const targetUrl = `${staticOrigin}${reqPath}`;
      staticTab.clearCapturedResponses();
      await staticTab.eval(`document.querySelector('iframe.p2p-app-frame').src = ${JSON.stringify(targetUrl)};`);
      return waitFor(
        () => staticTab.getCapturedResponses().find((r) => r.url === targetUrl),
        10_000,
        `staticnode ${reqPath}`
      );
    };

    const pctResp = await navStatic('/100%25_report.txt');
    assert.equal(pctResp.status, 200);
    const pctText = await waitFor(
      () => staticTab.evalInChildFrame((u) => u === `${staticOrigin}/100%25_report.txt`, `(document.body?.textContent || '').trim()`),
      10_000,
      'staticnode % file body'
    );
    assert.equal(pctText, 'percent-file-ok');
    assert.equal((await navStatic('/.env')).status, 404);
    assert.equal((await navStatic('/readme')).status, 404);
    assert.equal((await navStatic('/hop1')).status, 404);
    const payloadResp = await navStatic('/payload');
    assert.equal(payloadResp.status, 200);
    assert.match(payloadResp.headers['content-security-policy'] || '', /\bsandbox\b/i);

    assert.deepEqual(staticTab.getGatewayErrors(), []);
    assert.deepEqual(staticTab.getExceptions(), []);
    await staticTab.close();
  } finally {
    if (chrome) await chrome.close();
    if (agentProc) await agentProc.stop();
    if (staticAgentProc) await staticAgentProc.stop();
    await signalProc.stop();
    await backend.close();
  }
});
