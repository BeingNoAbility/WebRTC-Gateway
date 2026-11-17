const { execSync } = require('child_process');
const fs = require('fs');
const path = require('path');
const assert = require('assert');

console.log('[Test] Cloudflare Worker Syntax & Scope Guardrail');

const workerPath = path.resolve(__dirname, '../edge/cloudflare-worker/worker.js');
assert.ok(fs.existsSync(workerPath), 'worker.js must exist');

// 1. Native Node.js V8 Syntax Check
execSync(`node --check "${workerPath}"`, { stdio: 'inherit' });
console.log('  ✓ V8 syntax check passed (no parse errors)');

// 2. TDZ Guard: verify requireVisitorAuth declaration precedes serialization
const code = fs.readFileSync(workerPath, 'utf8');
const declIdx = code.indexOf('const requireVisitorAuth =');
const useIdx = code.indexOf('att.requireVisitorAuth = requireVisitorAuth');

assert.ok(declIdx !== -1, 'requireVisitorAuth declaration must exist');
assert.ok(useIdx !== -1, 'att.requireVisitorAuth assignment must exist');
assert.ok(declIdx < useIdx, `TDZ Bug: requireVisitorAuth must be declared (offset ${declIdx}) before usage (offset ${useIdx})`);

console.log('  ✓ TDZ guardrail passed: requireVisitorAuth declared before serialization');
console.log('All Worker checks passed successfully.');
