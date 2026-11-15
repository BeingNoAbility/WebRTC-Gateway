import * as esbuild from 'esbuild';
import * as fs from 'fs';
import * as path from 'path';

const outdir = path.resolve('dist');
if (!fs.existsSync(outdir)) {
  fs.mkdirSync(outdir, { recursive: true });
}

// Copy index.html
if (fs.existsSync('src/index.html')) {
  fs.copyFileSync('src/index.html', path.join(outdir, 'index.html'));
}

async function build() {
  const isWatch = process.argv.includes('--watch');

  // 1. Build portal (ESM)
  await esbuild.build({
    entryPoints: { portal: 'src/portal/app.ts' },
    bundle: true,
    outdir: 'dist',
    format: 'esm',
    target: ['es2022'],
    sourcemap: isWatch,
    minify: !isWatch,
    logLevel: 'info',
  });

  // 2. Build Service Worker & Patch (IIFE for standard Classic Script compatibility)
  await esbuild.build({
    entryPoints: {
      sw: 'src/sw/sw.ts',
      patch: 'src/patch/ws_patch.ts'
    },
    bundle: true,
    outdir: 'dist',
    format: 'iife',
    target: ['es2022'],
    sourcemap: isWatch,
    minify: !isWatch,
    logLevel: 'info',
  });

  console.log('Build completed successfully.');
}

build().catch((err) => {
  console.error(err);
  process.exit(1);
});
