/**
 * Unified Real Chrome / Edge CDP E2E Harness (V7)
 * - Ephemeral :0 ports everywhere (zero port collisions in parallel/CI runs).
 * - Supports BROWSER_BIN / EDGE_BIN / system google-chrome / chromium / microsoft-edge.
 * - Single-build signal (-tags embedweb, tested WITHOUT -static!) and agent binaries.
 * - Real CDP cross-origin iframe / OOPIF evaluation (NEVER fakes PASS on SecurityError!).
 */

import { execFileSync, spawn } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
export const REPO_ROOT = path.resolve(__dirname, '../..');

export function findBrowserBinary() {
  const candidates = [
    process.env.BROWSER_BIN,
    process.env.EDGE_BIN,
    '/usr/bin/google-chrome',
    '/usr/bin/google-chrome-stable',
    '/usr/bin/chromium',
    '/usr/bin/chromium-browser',
    '/usr/bin/microsoft-edge',
  ].filter(Boolean);

  for (const bin of candidates) {
    if (fs.existsSync(bin)) return bin;
  }
  throw new Error(`No Chrome/Chromium/Edge binary found (tried: ${candidates.join(', ')})`);
}

export function getFreePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.unref();
    srv.on('error', reject);
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address();
      const port = typeof addr === 'object' && addr ? addr.port : 0;
      srv.close(() => resolve(port));
    });
  });
}

export async function waitFor(fn, timeoutMs = 15_000, label = 'condition', intervalMs = 100) {
  const deadline = Date.now() + timeoutMs;
  let lastErr = null;
  while (Date.now() < deadline) {
    try {
      const val = await fn();
      if (val) return val;
    } catch (err) {
      lastErr = err;
    }
    await new Promise((r) => setTimeout(r, intervalMs));
  }
  const suffix = lastErr ? ` (last error: ${lastErr instanceof Error ? lastErr.message : String(lastErr)})` : '';
  throw new Error(`Timed out after ${timeoutMs}ms waiting for ${label}${suffix}`);
}

export function buildV7Binaries(outDir) {
  if (
    process.env.SIGNAL_BIN &&
    process.env.AGENT_BIN &&
    fs.existsSync(process.env.SIGNAL_BIN) &&
    fs.existsSync(process.env.AGENT_BIN)
  ) {
    return { signalBin: process.env.SIGNAL_BIN, agentBin: process.env.AGENT_BIN };
  }
  if (!fs.existsSync(path.join(REPO_ROOT, 'web/dist/__p2p_gw__/manifest.json'))) {
    execFileSync('npm', ['--prefix', path.join(REPO_ROOT, 'web'), 'run', 'build'], {
      stdio: 'pipe',
    });
  }
  const signalBin = path.join(outDir, 'signal');
  const agentBin = path.join(outDir, 'agent');
  execFileSync('go', ['build', '-tags', 'embedweb', '-o', signalBin, './cmd/signal'], {
    cwd: REPO_ROOT,
    stdio: 'pipe',
  });
  execFileSync('go', ['build', '-o', agentBin, './cmd/agent'], {
    cwd: REPO_ROOT,
    stdio: 'pipe',
  });
  return { signalBin, agentBin };
}

/**
 * Generates an ephemeral ECDSA P-256 self-signed TLS cert with custom SANs plus its base64 SPKI SHA-256
 * pin (accepted by Chrome `--ignore-certificate-errors-spki-list=...` even for Service Workers!).
 */
export function generateTestCert(outDir, dnsNames = []) {
  const certPath = path.join(outDir, 'test-cert.pem');
  const keyPath = path.join(outDir, 'test-key.pem');
  const sanEntries = ['IP:127.0.0.1', 'DNS:localhost', ...dnsNames.map((d) => `DNS:${d}`)].join(',');
  execFileSync(
    'openssl',
    [
      'req',
      '-x509',
      '-newkey',
      'ec',
      '-pkeyopt',
      'ec_paramgen_curve:prime256v1',
      '-nodes',
      '-days',
      '1',
      '-subj',
      '/CN=portal.example.com',
      '-addext',
      'basicConstraints=critical,CA:TRUE',
      '-addext',
      `subjectAltName=${sanEntries}`,
      '-keyout',
      keyPath,
      '-out',
      certPath,
    ],
    { stdio: 'pipe' }
  );
  const certPem = fs.readFileSync(certPath, 'utf8');
  const spkiDer = crypto.createPublicKey(certPem).export({ type: 'spki', format: 'der' });
  const spkiHash = crypto.createHash('sha256').update(spkiDer).digest('base64');
  return { certPath, keyPath, spkiHash, certPem };
}

export function spawnLoggedProcess(bin, args, opts = {}) {
  const logs = [];
  const child = spawn(bin, args, {
    cwd: opts.cwd || REPO_ROOT,
    env: { ...process.env, ...(opts.env || {}) },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const capture = (chunk) => {
    const str = chunk.toString('utf8');
    logs.push(str);
    if (logs.length > 200) logs.shift();
  };
  child.stdout?.on('data', capture);
  child.stderr?.on('data', capture);
  return {
    child,
    getLogs: () => logs.join(''),
    stop: async () => {
      if (child.exitCode !== null) return;
      child.kill('SIGTERM');
      await new Promise((resolve) => {
        const timer = setTimeout(() => {
          try {
            child.kill('SIGKILL');
          } catch {
            // ignore
          }
          resolve();
        }, 3000);
        child.once('exit', () => {
          clearTimeout(timer);
          resolve();
        });
      });
    },
  };
}

/**
 * Lightweight zero-dependency Chrome DevTools Protocol client over Node 24 native WebSocket.
 */
export class CDPClient {
  constructor(ws) {
    this.ws = ws;
    this.nextId = 1;
    this.pending = new Map();
    this.listeners = new Map();
    ws.addEventListener('message', (ev) => {
      const msg = JSON.parse(String(ev.data));
      if (typeof msg.id === 'number') {
        const p = this.pending.get(msg.id);
        if (p) {
          this.pending.delete(msg.id);
          if (msg.error) p.reject(new Error(`CDP ${p.method}: ${msg.error.message}`));
          else p.resolve(msg.result || {});
        }
      } else if (msg.method) {
        const set = this.listeners.get(msg.method);
        if (set) {
          for (const fn of set) fn(msg.params || {}, msg.sessionId);
        }
      }
    });
  }

  send(method, params = {}, sessionId = undefined) {
    const id = this.nextId++;
    const payload = { id, method, params };
    if (sessionId) payload.sessionId = sessionId;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`CDP timeout on ${method}`));
      }, 20_000);
      this.pending.set(id, {
        method,
        resolve: (v) => {
          clearTimeout(timer);
          resolve(v);
        },
        reject: (e) => {
          clearTimeout(timer);
          reject(e);
        },
      });
      this.ws.send(JSON.stringify(payload));
    });
  }

  on(method, fn) {
    if (!this.listeners.has(method)) this.listeners.set(method, new Set());
    this.listeners.get(method).add(fn);
    return () => this.listeners.get(method)?.delete(fn);
  }

  close() {
    try {
      this.ws.close();
    } catch {
      // ignore
    }
  }
}

export async function launchHeadlessChrome(userDataDir, extraArgs = []) {
  const bin = findBrowserBinary();
  const proc = spawn(
    bin,
    [
      '--headless=new',
      '--remote-debugging-port=0',
      `--user-data-dir=${userDataDir}`,
      '--no-sandbox',
      '--disable-setuid-sandbox',
      '--disable-gpu',
      '--disable-dev-shm-usage',
      '--disable-background-networking',
      '--no-first-run',
      '--no-default-browser-check',
      ...extraArgs,
      'about:blank',
    ],
    { stdio: ['ignore', 'pipe', 'pipe'] }
  );

  let stderrBuf = '';
  const wsUrl = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      reject(new Error(`Chrome failed to emit DevTools WS URL. stderr:\n${stderrBuf}`));
    }, 15_000);
    proc.stderr.on('data', (chunk) => {
      stderrBuf += chunk.toString('utf8');
      const m = /DevTools listening on (ws:\/\/[^\s]+)/.exec(stderrBuf);
      if (m) {
        clearTimeout(timer);
        resolve(m[1]);
      }
    });
    proc.once('exit', (code) => {
      clearTimeout(timer);
      reject(new Error(`Chrome exited prematurely (${code}). stderr:\n${stderrBuf}`));
    });
  });

  const ws = new WebSocket(wsUrl);
  await new Promise((resolve, reject) => {
    ws.addEventListener('open', resolve, { once: true });
    ws.addEventListener('error', () => reject(new Error('Failed to open CDP WebSocket')), { once: true });
  });

  const client = new CDPClient(ws);
  return {
    client,
    proc,
    close: async () => {
      client.close();
      if (proc.exitCode === null) {
        proc.kill('SIGTERM');
        await new Promise((r) => {
          const t = setTimeout(() => {
            try {
              proc.kill('SIGKILL');
            } catch {
              // ignore
            }
            r();
          }, 3000);
          proc.once('exit', () => {
            clearTimeout(t);
            r();
          });
        });
      }
    },
  };
}

export async function openPageTab(cdp, url) {
  const { targetId } = await cdp.send('Target.createTarget', { url: 'about:blank' });
  const { sessionId } = await cdp.send('Target.attachToTarget', { targetId, flatten: true });
  const attachedSessions = new Set([sessionId]);
  const portalOrigin = new URL(url).origin;
  const gatewayErrors = [];
  const uncaughtExceptions = [];
  const capturedResponses = [];

  const isGatewayAssetUrl = (rawUrl) => {
    try {
      const u = new URL(rawUrl);
      if (u.pathname.startsWith('/__p2p_gw__/')) return true;
      if (u.origin === portalOrigin && (u.pathname === '/' || u.pathname === '/index.html' || u.pathname === '/favicon.svg')) {
        return true;
      }
      return false;
    } catch {
      return false;
    }
  };

  const offAttached = cdp.on('Target.attachedToTarget', (params, parentSid) => {
    if (parentSid && attachedSessions.has(parentSid) && params.sessionId) {
      const childSid = params.sessionId;
      attachedSessions.add(childSid);
      Promise.all([
        cdp.send('Network.enable', {}, childSid).catch(() => {}),
        cdp.send('Runtime.enable', {}, childSid).catch(() => {}),
      ]).finally(() => {
        if (params.waitingForDebugger) {
          cdp.send('Runtime.runIfWaitingForDebugger', {}, childSid).catch(() => {});
        }
      });
    }
  });
  const offResponse = cdp.on('Network.responseReceived', (params, sid) => {
    if (!sid || !attachedSessions.has(sid)) return;
    const resp = params.response;
    if (!resp || typeof resp.status !== 'number') return;
    const lowerHeaders = {};
    if (resp.headers && typeof resp.headers === 'object') {
      for (const [k, v] of Object.entries(resp.headers)) {
        lowerHeaders[k.toLowerCase()] = String(v);
      }
    }
    capturedResponses.push({
      url: resp.url,
      status: resp.status,
      statusText: resp.statusText || '',
      headers: lowerHeaders,
    });
    if (resp.status >= 400 && isGatewayAssetUrl(resp.url)) {
      gatewayErrors.push({ url: resp.url, status: resp.status, statusText: resp.statusText || '' });
    }
  });
  const offException = cdp.on('Runtime.exceptionThrown', (params, sid) => {
    if (!sid || !attachedSessions.has(sid)) return;
    const details = params.exceptionDetails || {};
    uncaughtExceptions.push({
      text: details.exception?.description || details.text || 'unknown exception',
      url: details.url || '',
    });
  });

  await cdp.send('Page.enable', {}, sessionId);
  await cdp.send('Network.enable', {}, sessionId);
  await cdp.send('Runtime.enable', {}, sessionId);
  await cdp.send('Target.setAutoAttach', {
    autoAttach: true,
    waitForDebuggerOnStart: true,
    flatten: true,
  }, sessionId);
  await cdp.send('Page.navigate', { url }, sessionId);

  const evalInSession = async (sid, expression) => {
    const res = await cdp.send(
      'Runtime.evaluate',
      { expression, awaitPromise: true, returnByValue: true },
      sid
    );
    if (res.exceptionDetails) {
      throw new Error(
        `Browser eval exception: ${res.exceptionDetails.exception?.description || res.exceptionDetails.text}`
      );
    }
    return res.result?.value;
  };

  /**
   * Evaluates JS inside a real cross-origin OOPIF iframe matching `urlPredicate` using CDP Target attachment
   * (works for cross-site / cross-origin sandboxes WITHOUT SecurityError!).
   */
  const evalInChildFrame = async (urlPredicate, expression) => {
    const { targetInfos } = await cdp.send('Target.getTargets');
    const frameTarget = targetInfos.find(
      (t) => (t.type === 'iframe' || t.type === 'page') && urlPredicate(t.url)
    );
    if (!frameTarget) {
      throw new Error(
        `No child iframe target matched predicate. Open targets: ${targetInfos.map((t) => `${t.type}:${t.url}`).join(', ')}`
      );
    }
    const { sessionId: frameSid } = await cdp.send('Target.attachToTarget', {
      targetId: frameTarget.targetId,
      flatten: true,
    });
    attachedSessions.add(frameSid);
    await cdp.send('Runtime.enable', {}, frameSid);
    return evalInSession(frameSid, expression);
  };

  return {
    targetId,
    sessionId,
    eval: (expr) => evalInSession(sessionId, expr),
    evalInChildFrame,
    getGatewayErrors: () => [...gatewayErrors],
    getExceptions: () => [...uncaughtExceptions],
    getCapturedResponses: () => [...capturedResponses],
    clearCapturedErrors: () => {
      gatewayErrors.length = 0;
      uncaughtExceptions.length = 0;
    },
    clearCapturedResponses: () => {
      capturedResponses.length = 0;
    },
    navigate: (nextUrl) => cdp.send('Page.navigate', { url: nextUrl }, sessionId),
    reload: () => cdp.send('Page.reload', { ignoreCache: true }, sessionId),
    close: () => {
      offAttached();
      offResponse();
      offException();
      return cdp.send('Target.closeTarget', { targetId }).catch(() => {});
    },
  };
}

export function createTempWorkdir(prefix = 'v7-cdp-e2e-') {
  return fs.mkdtempSync(path.join(os.tmpdir(), prefix));
}
