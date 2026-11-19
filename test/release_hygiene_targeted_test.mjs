import { execSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const rootDir = path.resolve(__dirname, '..');

console.log('🧪 Starting Release Hygiene Gate Targeted & Adversarial Tests...');

// 1. Positive Test: Clean workspace must pass with exit code 0
try {
  const output = execSync('node scripts/verify-release-hygiene.mjs', {
    cwd: rootDir,
    encoding: 'utf8',
    stdio: 'pipe',
  });
  if (!output.includes('All release hygiene and version parity checks PASSED')) {
    throw new Error('Expected pass banner in output');
  }
  console.log('✅ PASS: Baseline workspace passes release hygiene gate');
} catch (err) {
  console.error('❌ FAIL: Clean workspace failed release hygiene gate:', err.message);
  process.exit(1);
}

// 2. Negative Test: Simulating version mismatch in docs/package.json
const docsPkgPath = path.join(rootDir, 'docs', 'package.json');
const originalDocsPkg = fs.readFileSync(docsPkgPath, 'utf8');

try {
  const tampered = JSON.parse(originalDocsPkg);
  tampered.version = '9.9.9-tampered';
  fs.writeFileSync(docsPkgPath, JSON.stringify(tampered, null, 2));

  let failedAsExpected = false;
  try {
    execSync('node scripts/verify-release-hygiene.mjs', {
      cwd: rootDir,
      encoding: 'utf8',
      stdio: 'pipe',
    });
  } catch (err) {
    failedAsExpected = true;
    const stderr = err.stderr ? err.stderr.toString() : '';
    const stdout = err.stdout ? err.stdout.toString() : '';
    if (!stdout.includes('FAIL: docs/package.json version (9.9.9-tampered) matches') && !stderr.includes('FAIL')) {
      throw new Error(`Expected failure message not found in output: ${stdout}\n${stderr}`);
    }
  }

  if (!failedAsExpected) {
    throw new Error('Gate did NOT fail on version tampering in docs/package.json!');
  }
  console.log('✅ PASS: Gate correctly flagged version tampering in docs/package.json with exit code 1');
} finally {
  // Restore original content
  fs.writeFileSync(docsPkgPath, originalDocsPkg);
}

// 3. Negative Test: Simulating multiple release notes headers
const notesPath = path.join(rootDir, 'docs', 'RELEASE_NOTES.md');
const originalNotes = fs.readFileSync(notesPath, 'utf8');

try {
  const tamperedNotes = `${originalNotes}\n\n## [v1.0.0] - Ancient Release\n- Legacy notes\n`;
  fs.writeFileSync(notesPath, tamperedNotes);

  let failedAsExpected = false;
  try {
    execSync('node scripts/verify-release-hygiene.mjs', {
      cwd: rootDir,
      encoding: 'utf8',
      stdio: 'pipe',
    });
  } catch (err) {
    failedAsExpected = true;
  }

  if (!failedAsExpected) {
    throw new Error('Gate did NOT fail on multiple release headers in docs/RELEASE_NOTES.md!');
  }
  console.log('✅ PASS: Gate correctly rejected historical release headers in docs/RELEASE_NOTES.md');
} finally {
  // Restore original content
  fs.writeFileSync(notesPath, originalNotes);
}

console.log('🎉 All release hygiene gate targeted tests PASSED!');
