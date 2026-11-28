#!/usr/bin/env node
/**
 * CHANGELOG drift check (warning only, always exits 0).
 *
 * Warns when code under cmd/, pkg/ or web/src changed since the last commit that touched
 * CHANGELOG.md while the `## [Unreleased]` section is still empty.
 *
 * Usage: node scripts/check-changelog-drift.mjs
 */

import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const rootDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const git = (...args) => execFileSync('git', args, { cwd: rootDir, encoding: 'utf8' }).trim();

try {
  const changelog = fs.readFileSync(path.join(rootDir, 'CHANGELOG.md'), 'utf8');
  const m = changelog.match(/^## \[Unreleased\][^\n]*\n([\s\S]*?)(?=^## \[|$(?![\s\S]))/m);
  const unreleasedBody = m ? m[1].trim() : '';
  if (unreleasedBody !== '') {
    console.log('PASS: CHANGELOG.md [Unreleased] section is not empty');
    process.exit(0);
  }

  const lastTouch = git('log', '-1', '--format=%H', '--', 'CHANGELOG.md');
  if (lastTouch === '') {
    process.exit(0);
  }
  const changed = git('diff', '--name-only', lastTouch, '--', 'cmd', 'pkg', 'web/src')
    .split('\n')
    .filter(Boolean);
  if (changed.length > 0) {
    console.warn(
      `WARN: ${changed.length} code file(s) changed since the last CHANGELOG.md update but [Unreleased] is empty. ` +
        'Add user-visible changes to CHANGELOG.md if applicable.'
    );
  } else {
    console.log('PASS: no code changes since the last CHANGELOG.md update');
  }
} catch (err) {
  console.warn(`WARN: CHANGELOG drift check skipped (${err.message})`);
}
