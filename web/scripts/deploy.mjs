#!/usr/bin/env node
/**
 * Smart Cloudflare Edge Worker Deployment Wrapper
 *
 * Automatically detects whether a private gitignored `wrangler.local.toml` exists:
 * - If present (local developer environment): deploys using local custom domain overrides.
 * - If absent (CI / other developers / clean checkout): deploys using standard `wrangler.toml`.
 */

import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const webDir = path.resolve(__dirname, '..');

const targetEnv = process.argv[2] === 'testing' ? 'testing' : 'prod';
const localConfig = path.join(webDir, 'wrangler.local.toml');

const configFile = fs.existsSync(localConfig) ? 'wrangler.local.toml' : 'wrangler.toml';
console.log(`🚀 [Cloudflare Deploy] Target: [${targetEnv}], Using Config: [${configFile}]`);

const args = ['wrangler', 'deploy', '-c', configFile];
if (targetEnv === 'testing') {
  args.push('--env', 'testing');
}

const cmd = process.platform === 'win32' ? 'npx.cmd' : 'npx';
const result = spawnSync(cmd, args, { cwd: webDir, stdio: 'inherit' });
process.exit(result.status ?? 0);
