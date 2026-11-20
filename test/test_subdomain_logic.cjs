function extractNodeIdFromHostname(hostname) {
  if (!hostname) return null;
  const host = hostname.toLowerCase().split(':')[0];
  if (/^(\d{1,3}\.){3}\d{1,3}$/.test(host) || host === 'localhost') return null;

  const paasSuffixes = [
    '.pages.dev', '.workers.dev', '.eu.org', '.github.io',
    '.vercel.app', '.netlify.app', '.onrender.com', '.fly.dev'
  ];
  for (const suffix of paasSuffixes) {
    if (host.endsWith(suffix)) {
      const prefix = host.slice(0, -suffix.length);
      if (!prefix.includes('.')) {
        return null;
      }
    }
  }

  const reserved = new Set([
    'p2p', 'www', 'webrtc', 'webrtc-p2p-signal', 'signal', 'signaling', 'gateway',
    'portal', 'api', 'admin', 'proxy', 'vpn', 'tunnel', 'webrtc-gateway',
    'p2p-gateway', 'app', 'web', 'client', 'relay', 'service', 'hub', 'test',
    'testing', 'stage', 'staging', 'dev', 'demo', 'node', 'nodes', 'server',
    'direct', 'static', 'assets', 'cdn'
  ]);

  // Pattern 1: Node prefix before ".p2p." (e.g. "bili.p2p.example.com", "bili.testing.p2p.example.com")
  const p2pIdx = host.indexOf('.p2p.');
  if (p2pIdx > 0) {
    const parts = host.substring(0, p2pIdx).split('.');
    for (let i = parts.length - 1; i >= 0; i--) {
      const cand = parts[i];
      if (cand && !reserved.has(cand)) return cand;
    }
    return null;
  }

  // Pattern 2: Multi-level subdomain (e.g. "bili.example.com", "nas.home.net", "bili.localhost")
  const parts = host.split('.');
  if (parts.length >= 2) {
    const firstLabel = parts[0];
    if (firstLabel && !reserved.has(firstLabel)) {
      if (parts[parts.length - 1] === 'localhost' || parts.length >= 3) {
        return firstLabel;
      }
    }
  }

  return null;
}

const tests = [
  { host: 'bili.p2p.example.com', expected: 'bili' },
  { host: 'speedtest.p2p.example.com', expected: 'speedtest' },
  { host: 'vaultwarden.p2p.mydomain.com', expected: 'vaultwarden' },
  { host: 'testing.p2p.example.com', expected: null },
  { host: 'bili.testing.p2p.example.com', expected: 'bili' },
  { host: 'stage.p2p.example.com', expected: null },
  { host: 'nas.stage.p2p.example.com', expected: 'nas' },
  { host: 'immich.example.com', expected: 'immich' },
  { host: 'jellyfin.myhome.net', expected: 'jellyfin' },
  { host: 'bili.localhost:8888', expected: 'bili' },
  { host: 'p2p.example.com', expected: null },
  { host: 'webrtc-p2p-signal.example.com', expected: null },
  { host: 'www.example.com', expected: null },
  { host: 'my-portal.workers.dev', expected: null },
  { host: 'my-portal.pages.dev', expected: null },
  { host: 'bili.my-portal.workers.dev', expected: 'bili' },
  { host: '192.168.1.100', expected: null },
  { host: 'localhost:8888', expected: null }
];

let allPass = true;
for (const t of tests) {
  const result = extractNodeIdFromHostname(t.host);
  const pass = result === t.expected;
  if (!pass) allPass = false;
  console.log(`[${pass ? 'PASS' : 'FAIL'}] host: ${t.host.padEnd(38)} -> ${result} (expected: ${t.expected})`);
}
console.log('All tests passed:', allPass);
if (!allPass) process.exit(1);
