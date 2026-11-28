import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const rootDir = path.resolve(__dirname, '..', '..');
const gateScript = path.join(rootDir, 'scripts', 'verify-release-hygiene.mjs');

function createIsolatedFixtureRoot() {
  const tmpRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'webrtc-hygiene-'));
  const relFiles = [
    'VERSION',
    'web/package.json',
    'docs/package.json',
    'web/package-lock.json',
    'docs/package-lock.json',
    'docs/RELEASE_NOTES.md',
    'CHANGELOG.md',
    'docker-compose.yml',
  ];
  for (const rel of relFiles) {
    const src = path.join(rootDir, rel);
    const dst = path.join(tmpRoot, rel);
    if (fs.existsSync(src)) {
      fs.mkdirSync(path.dirname(dst), { recursive: true });
      fs.copyFileSync(src, dst);
    }
  }
  return tmpRoot;
}

function runGate(args = [], envExtra = {}) {
  return execFileSync('node', [gateScript, ...args], {
    cwd: rootDir,
    env: { ...process.env, ...envExtra },
    encoding: 'utf8',
    stdio: 'pipe',
  });
}

test('Release hygiene gate: live workspace passes version parity and --check-tag', () => {
  const ver = fs.readFileSync(path.join(rootDir, 'VERSION'), 'utf8').trim();
  const output = runGate(['--check-tag', `v${ver}`]);
  assert.ok(output.includes('All release hygiene and version parity checks PASSED'));
});

test('Release hygiene gate: rejects mismatched --check-tag', () => {
  const tmpRoot = createIsolatedFixtureRoot();
  try {
    assert.throws(
      () => runGate(['--check-tag', 'v9.9.9'], { HYGIENE_ROOT_DIR: tmpRoot }),
      (err) => ((err.stdout || '') + (err.stderr || '')).includes('Requested tag (v9.9.9) matches VERSION')
    );
  } finally {
    fs.rmSync(tmpRoot, { recursive: true, force: true });
  }
});

test('Release hygiene gate: flags docs/package.json version tampering', () => {
  const tmpRoot = createIsolatedFixtureRoot();
  try {
    const docsPkgPath = path.join(tmpRoot, 'docs', 'package.json');
    const tampered = JSON.parse(fs.readFileSync(docsPkgPath, 'utf8'));
    tampered.version = '9.9.9-tampered';
    fs.writeFileSync(docsPkgPath, JSON.stringify(tampered, null, 2));

    assert.throws(
      () => runGate([], { HYGIENE_ROOT_DIR: tmpRoot }),
      (err) =>
        ((err.stdout || '') + (err.stderr || '')).includes(
          'FAIL: docs/package.json version (9.9.9-tampered) matches VERSION'
        )
    );
  } finally {
    fs.rmSync(tmpRoot, { recursive: true, force: true });
  }
});

test('Release hygiene gate: flags docker-compose.yml image tag drift', () => {
  const tmpRoot = createIsolatedFixtureRoot();
  try {
    const composePath = path.join(tmpRoot, 'docker-compose.yml');
    const drifted = fs.readFileSync(composePath, 'utf8').replace(/webrtc-p2p-agent:v[^\s]+/, 'webrtc-p2p-agent:v0.0.1');
    fs.writeFileSync(composePath, drifted);
    assert.throws(
      () => runGate([], { HYGIENE_ROOT_DIR: tmpRoot }),
      (err) => ((err.stdout || '') + (err.stderr || '')).includes('FAIL: docker-compose.yml image tags match')
    );
  } finally {
    fs.rmSync(tmpRoot, { recursive: true, force: true });
  }
});

test('Release hygiene gate: rejects forbidden .env file', () => {
  const tmpRoot = createIsolatedFixtureRoot();
  try {
    fs.writeFileSync(path.join(tmpRoot, '.env'), 'ACCESS_SECRET=leaked-demo-secret\n');
    assert.throws(
      () => runGate([], { HYGIENE_ROOT_DIR: tmpRoot }),
      (err) => ((err.stdout || '') + (err.stderr || '')).includes('Forbidden .env file')
    );
  } finally {
    fs.rmSync(tmpRoot, { recursive: true, force: true });
  }
});

test('Release hygiene gate: rejects GitHub PAT, Ed25519 private key, and file:/// leaks', () => {
  const tmpRoot = createIsolatedFixtureRoot();
  try {
    const fakePat = ['github', 'pat', '11ABCDEF0123456789_0123456789'].join('_');
    fs.writeFileSync(path.join(tmpRoot, 'docs', 'pat_leak.txt'), `token=${fakePat}\n`);
    assert.throws(
      () => runGate([], { HYGIENE_ROOT_DIR: tmpRoot }),
      (err) => ((err.stdout || '') + (err.stderr || '')).includes('GitHub token')
    );
    fs.unlinkSync(path.join(tmpRoot, 'docs', 'pat_leak.txt'));

    const beginKey = ['-----BEGIN ', 'ED25519 PRIVATE ', 'KEY-----'].join('');
    fs.writeFileSync(path.join(tmpRoot, 'docs', 'key_leak.txt'), `${beginKey}\n`);
    assert.throws(
      () => runGate([], { HYGIENE_ROOT_DIR: tmpRoot }),
      (err) => ((err.stdout || '') + (err.stderr || '')).includes('Private cryptographic key block')
    );
    fs.unlinkSync(path.join(tmpRoot, 'docs', 'key_leak.txt'));

    const fileLink = ['file:', '', '', 'c:', 'mock', 'leak.txt'].join('/');
    fs.writeFileSync(path.join(tmpRoot, 'docs', 'link_leak.md'), `Link: ${fileLink}\n`);
    assert.throws(() => runGate([], { HYGIENE_ROOT_DIR: tmpRoot }));
    fs.unlinkSync(path.join(tmpRoot, 'docs', 'link_leak.md'));

    // RFC 1918 private LAN IP in docs/configs must be rejected (use RFC 5737 instead).
    const fakeLanIp = ['192', '168', '10', '42'].join('.');
    fs.writeFileSync(path.join(tmpRoot, 'docs', 'lan_leak.md'), `Target: http://${fakeLanIp}:8080\n`);
    assert.throws(
      () => runGate([], { HYGIENE_ROOT_DIR: tmpRoot }),
      (err) => ((err.stdout || '') + (err.stderr || '')).includes('RFC 1918 private IP')
    );
  } finally {
    fs.rmSync(tmpRoot, { recursive: true, force: true });
  }
});

test('Release hygiene gate: rejects regex-escaped user home paths', () => {
  const tmpRoot = createIsolatedFixtureRoot();
  try {
    const escapedMacPath = ['\\/', 'Users', '\\/', 'alice'].join('');
    fs.writeFileSync(path.join(tmpRoot, 'docs', 'escaped_leak.js'), `const re = /${escapedMacPath}/i;\n`);
    assert.throws(
      () => runGate([], { HYGIENE_ROOT_DIR: tmpRoot }),
      (err) => ((err.stdout || '') + (err.stderr || '')).includes('Local macOS home directory path')
    );
  } finally {
    fs.rmSync(tmpRoot, { recursive: true, force: true });
  }
});

test('Release hygiene gate: enforces gitignored .hygiene-private-patterns.local without echoing secrets', () => {
  const tmpRoot = createIsolatedFixtureRoot();
  try {
    const syntheticPrivate = 'private-sentinel-998811';
    fs.writeFileSync(
      path.join(tmpRoot, '.hygiene-private-patterns.local'),
      `# Local-only private tokens\n\n${syntheticPrivate}\n`
    );

    // Clean workspace passes with denylist loaded (and denylist file does not self-match)
    const okOut = runGate([], { HYGIENE_ROOT_DIR: tmpRoot });
    assert.ok(okOut.includes('+ 1 local denylist entries'));
    assert.ok(!okOut.includes(syntheticPrivate));

    // Leaked token in a workspace file is caught without leaking the token into stdout/stderr
    fs.writeFileSync(path.join(tmpRoot, 'docs', 'note.txt'), `endpoint: ${syntheticPrivate.toUpperCase()}\n`);
    assert.throws(
      () => runGate([], { HYGIENE_ROOT_DIR: tmpRoot }),
      (err) => {
        const combined = (err.stdout || '') + (err.stderr || '');
        return combined.includes('local denylist entry #1 matched') && !combined.includes(syntheticPrivate);
      }
    );
  } finally {
    fs.rmSync(tmpRoot, { recursive: true, force: true });
  }
});
