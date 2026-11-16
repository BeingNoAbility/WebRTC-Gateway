const http = require('http');
const { spawn } = require('child_process');
const path = require('path');
const os = require('os');
const fs = require('fs');

const rootDir = path.resolve(__dirname, '..');
const signalBin = path.join(rootDir, 'bin', 'signal.exe');
const agentBin = path.join(rootDir, 'bin', 'agent.exe');
const mockBin = path.join(rootDir, 'bin', 'mock_service.exe');
const edgePath = 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';

const SIGNAL_PORT = 22360;
const MOCK_PORT = 18093;
const CDP_PORT = 9227;

const procs = [];
function cleanup() {
  for (const p of procs) {
    try { p.kill(); } catch (e) {}
  }
}
process.on('exit', cleanup);

async function sleep(ms) { return new Promise(r => setTimeout(r, ms)); }
async function waitForHttp(url, timeoutMs = 10000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    try {
      await new Promise((res, rej) => {
        const req = http.get(url, r => res());
        req.on('error', rej);
        req.setTimeout(1000, () => { req.destroy(); rej(new Error('timeout')); });
      });
      return true;
    } catch (e) {
      await sleep(200);
    }
  }
  throw new Error('Timeout waiting for ' + url);
}

class CDPClient {
  constructor(wsUrl) {
    this.ws = new WebSocket(wsUrl);
    this.id = 1;
    this.pending = new Map();
    this.ws.onmessage = (msg) => {
      try {
        const raw = typeof msg.data === 'string' ? msg.data : msg.data.toString();
        const data = JSON.parse(raw);
        if (data.id && this.pending.has(data.id)) {
          this.pending.get(data.id)(data);
          this.pending.delete(data.id);
        }
      } catch (err) {}
    };
  }
  async connect() {
    return new Promise((res, rej) => {
      if (this.ws.readyState === WebSocket.OPEN) return res();
      this.ws.onopen = res;
      this.ws.onerror = rej;
    });
  }
  send(method, params = {}, timeoutMs = 10000) {
    return new Promise((resolve) => {
      const msgId = this.id++;
      const timer = setTimeout(() => {
        if (this.pending.has(msgId)) {
          this.pending.delete(msgId);
          resolve({ error: { message: 'timeout' } });
        }
      }, timeoutMs);
      this.pending.set(msgId, (res) => {
        clearTimeout(timer);
        resolve(res);
      });
      this.ws.send(JSON.stringify({ id: msgId, method, params }));
    });
  }
  async evaluate(expression, timeoutMs = 10000) {
    const res = await this.send('Runtime.evaluate', {
      expression,
      returnByValue: true,
      awaitPromise: true
    }, timeoutMs);
    return res.result?.result?.value ?? res.result?.value ?? null;
  }
  close() { try { this.ws.close(); } catch(e){} }
}

async function run() {
  console.log('=== VERIFYING HTTP & WEBSOCKET FEATURE MATRIX VIA REAL BROWSER ===\n');

  const sig = spawn(signalBin, ['-addr', `:${SIGNAL_PORT}`]);
  procs.push(sig);
  await waitForHttp(`http://127.0.0.1:${SIGNAL_PORT}/`);

  const mock = spawn(mockBin, ['-addr', `:${MOCK_PORT}`]);
  procs.push(mock);
  await waitForHttp(`http://127.0.0.1:${MOCK_PORT}/`);

  const agent = spawn(agentBin, [
    '-node-id', 'testnode',
    '-signal', `ws://127.0.0.1:${SIGNAL_PORT}/ws`,
    '-target', `http://127.0.0.1:${MOCK_PORT}`
  ]);
  procs.push(agent);
  await sleep(1000);

  const uDir = path.join(os.tmpdir(), 'edge_feat_diag_' + Date.now());
  const edge = spawn(edgePath, [
    '--headless=new',
    `--remote-debugging-port=${CDP_PORT}`,
    '--disable-gpu',
    `--user-data-dir=${uDir}`,
    'about:blank'
  ]);
  procs.push(edge);

  await waitForHttp(`http://127.0.0.1:${CDP_PORT}/json/version`, 10000);
  const targets = await new Promise(r => http.get(`http://127.0.0.1:${CDP_PORT}/json`, res => {
    let d = ''; res.on('data', c => d+=c); res.on('end', () => r(JSON.parse(d)));
  }));
  const pageTarget = targets.find(t => t.type === 'page');
  const cdp = new CDPClient(pageTarget.webSocketDebuggerUrl);
  await cdp.connect();

  await cdp.send('Page.enable');
  await cdp.send('Runtime.enable');

  await cdp.send('Page.navigate', { url: `http://localhost:${SIGNAL_PORT}/?node=testnode&path=/#testnode` });
  await sleep(3000);

  // Helper inside iframe
  const evalInIframe = async (code) => {
    return await cdp.evaluate(`(async () => {
      const iframe = document.querySelector('iframe');
      if (!iframe || !iframe.contentWindow) return { error: 'NO_IFRAME' };
      const w = iframe.contentWindow;
      ${code}
    })()`);
  };

  const results = [];

  // 1. GET & Query String
  const rGet = await evalInIframe(`
    const res = await w.fetch('/api/rest?q=param_value_123');
    return await res.json();
  `);
  results.push({
    feature: 'GET & Query String',
    pass: rGet.method === 'GET' && rGet.query === 'param_value_123',
    detail: `Method: ${rGet.method}, Query: ${rGet.query}`
  });

  // 2. POST & Request Body & Authorization
  const rPost = await evalInIframe(`
    const res = await w.fetch('/api/rest?q=post_action', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Authorization': 'Bearer sk_test_auth_token_999'
      },
      body: JSON.stringify({ action: 'create', count: 42 })
    });
    return await res.json();
  `);
  results.push({
    feature: 'POST & Request Body & Authorization',
    pass: rPost.method === 'POST' && rPost.auth === 'Bearer sk_test_auth_token_999' && rPost.body.includes('create'),
    detail: `Method: ${rPost.method}, Auth: ${rPost.auth}, Body: ${rPost.body}`
  });

  // 3. PUT & Request Body
  const rPut = await evalInIframe(`
    const res = await w.fetch('/api/rest?q=put_update', {
      method: 'PUT',
      headers: { 'Content-Type': 'text/plain' },
      body: 'RAW_BINARY_OR_TEXT_DATA_PAYLOAD'
    });
    return await res.json();
  `);
  results.push({
    feature: 'PUT Method & Content-Type',
    pass: rPut.method === 'PUT' && rPut.body === 'RAW_BINARY_OR_TEXT_DATA_PAYLOAD',
    detail: `Method: ${rPut.method}, BodyLen: ${rPut.bodyLength}`
  });

  // 4. PATCH Method
  const rPatch = await evalInIframe(`
    const res = await w.fetch('/api/rest?q=patch_partial', {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ field: 'patched_value' })
    });
    return await res.json();
  `);
  results.push({
    feature: 'PATCH Method',
    pass: rPatch.method === 'PATCH' && rPatch.body.includes('patched_value'),
    detail: `Method: ${rPatch.method}, Body: ${rPatch.body}`
  });

  // 5. DELETE Method
  const rDelete = await evalInIframe(`
    const res = await w.fetch('/api/rest?q=delete_item', { method: 'DELETE' });
    return await res.json();
  `);
  results.push({
    feature: 'DELETE Method',
    pass: rDelete.method === 'DELETE',
    detail: `Method: ${rDelete.method}, Query: ${rDelete.query}`
  });

  // 6. Cookie: HttpOnly Login & Session Persistence
  const rLogin = await evalInIframe(`
    const resLogin = await w.fetch('/api/auth/login', { method: 'POST' });
    const resStatus = await w.fetch('/api/auth/status');
    return await resStatus.json();
  `);
  results.push({
    feature: 'Cookie & HttpOnly Session',
    pass: rLogin.authenticated === true && rLogin.sessionId.startsWith('p2p_sess_'),
    detail: `Authenticated: ${rLogin.authenticated}, SessionId: ${rLogin.sessionId}`
  });

  // 7. Redirect: 302 & 307
  const rRedirect = await evalInIframe(`
    const res302 = await w.fetch('/api/redirect/302');
    const res307 = await w.fetch('/api/redirect/307', { method: 'POST', body: 'preserve_me' });
    return { d302: await res302.json(), d307: await res307.json() };
  `);
  results.push({
    feature: 'Redirect (302 & 307 Method Preservation)',
    pass: rRedirect.d302.redirectWorked && rRedirect.d307.method === 'POST',
    detail: `302 Worked: ${rRedirect.d302.redirectWorked}, 307 Method: ${rRedirect.d307.method}`
  });

  // 8. Range: HTTP 206 Partial Content
  const rRange = await evalInIframe(`
    const res = await w.fetch('/api/download/range', { headers: { 'Range': 'bytes=100-299' } });
    const text = await res.text();
    return {
      status: res.status,
      contentRange: res.headers.get('content-range'),
      len: text.length
    };
  `);
  results.push({
    feature: 'Range (HTTP 206 Partial Content)',
    pass: rRange.status === 206 && rRange.len === 200,
    detail: `Status: ${rRange.status}, Content-Range: ${rRange.contentRange}, Bytes: ${rRange.len}`
  });

  // 9. Content-Length & 5MB Streaming Download
  const rDownload = await evalInIframe(`
    const t0 = performance.now();
    const res = await w.fetch('/api/download/chunked');
    const cl = res.headers.get('content-length');
    const ab = await res.arrayBuffer();
    const dur = ((performance.now() - t0) / 1000).toFixed(2);
    return { cl, bytes: ab.byteLength, dur };
  `);
  results.push({
    feature: 'Content-Length & Chunked Streaming',
    pass: rDownload.bytes === 5 * 1024 * 1024,
    detail: `Length: ${rDownload.cl} bytes, Received: ${rDownload.bytes} in ${rDownload.dur}s`
  });

  // 10. Cache-Control & Null-Body (204 / 304)
  const rNullBody = await evalInIframe(`
    const r204 = await w.fetch('/api/status/204');
    const r304 = await w.fetch('/api/status/304');
    return { s204: r204.status, s304: r304.status };
  `);
  results.push({
    feature: 'Cache-Control & Null Body (204/304)',
    pass: rNullBody.s204 === 204 && rNullBody.s304 === 304,
    detail: `Status 204: ${rNullBody.s204}, Status 304: ${rNullBody.s304}`
  });

  // 11. WebSocket Full Duplex, Bursts & 128KB Jumbo
  const rWS = await evalInIframe(`
    return new Promise((resolve) => {
      const ws = new w.WebSocket('ws://' + w.location.host + '/ws/echo');
      let echoed = 0;
      ws.onopen = () => {
        for (let i = 0; i < 10; i++) ws.send('burst_' + i);
      };
      ws.onmessage = (e) => {
        if (typeof e.data === 'string') {
          if (e.data.startsWith('[Echo] burst_')) {
            echoed++;
            if (echoed === 10) {
              const big = 'X'.repeat(128 * 1024);
              ws.send(big);
            }
          } else if (e.data.startsWith('[Echo] X')) {
            ws.close();
            resolve({ echoed: 10, jumboLen: e.data.length - 7 });
          }
        }
      };
      ws.onerror = (e) => resolve({ error: 'WS Error' });
      setTimeout(() => resolve({ error: 'Timeout', echoed }), 6000);
    });
  `);
  results.push({
    feature: 'WebSocket (Bursts & 128KB Jumbo)',
    pass: rWS.echoed === 10 && rWS.jumboLen === 128 * 1024,
    detail: `Echoed: ${rWS.echoed}/10, Jumbo Frame Echo: ${rWS.jumboLen} bytes`
  });

  console.log('\n--- VERIFICATION MATRIX RESULTS ---');
  console.table(results);

  cdp.close();
  cleanup();
}

run().catch(console.error);
