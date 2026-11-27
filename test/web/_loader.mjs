import path from 'node:path';
import { fileURLToPath } from 'node:url';
import * as esbuild from '../../web/node_modules/esbuild/lib/main.js';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
export const repoRoot = path.resolve(__dirname, '..', '..');

/**
 * Bundles a TypeScript module in-memory via esbuild and imports it as a zero-disk ESM data URL.
 */
export async function loadTsModule(entryRel, opts = {}) {
  const res = await esbuild.build({
    entryPoints: [path.join(repoRoot, entryRel)],
    bundle: true,
    write: false,
    format: 'esm',
    platform: opts.platform ?? 'neutral',
    mainFields: ['module', 'main'],
    target: ['es2022'],
    define: {
      __APP_VERSION__: '"v7.0.0-test"',
      ...(opts.define ?? {}),
    },
  });
  const code = res.outputFiles[0].text;
  const dataUrl = `data:text/javascript;base64,${Buffer.from(code).toString('base64')}`;
  return import(dataUrl);
}
