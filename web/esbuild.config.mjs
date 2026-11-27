import * as esbuild from 'esbuild';
import * as crypto from 'node:crypto';
import * as fs from 'node:fs';
import * as path from 'node:path';

const outdir = path.resolve('dist');
const gwDir = path.join(outdir, '__p2p_gw__');
const versionFile = path.resolve('..', 'VERSION');
const appVersion = fs.existsSync(versionFile)
  ? `v${fs.readFileSync(versionFile, 'utf8').trim()}`
  : 'v0.0.0-dev';

// Always clean web/dist before building to guarantee zero stale assets or stray .map files.
fs.rmSync(outdir, { recursive: true, force: true });
fs.mkdirSync(gwDir, { recursive: true });

function shortHash(buf) {
  return crypto.createHash('sha256').update(buf).digest('hex').slice(0, 10);
}

function writeHashedAsset(srcPath, baseName, ext) {
  const data = fs.readFileSync(srcPath);
  const hash = shortHash(data);
  const hashedName = `${baseName}.${hash}.${ext}`;
  fs.writeFileSync(path.join(gwDir, hashedName), data);
  return `/__p2p_gw__/${hashedName}`;
}

function finalizeHtmlAndManifest(isWatch) {
  if (fs.existsSync('src/favicon.svg')) {
    fs.copyFileSync('src/favicon.svg', path.join(outdir, 'favicon.svg'));
  }

  let portalJS = '/__p2p_gw__/portal.js';
  let portalCSS = '/__p2p_gw__/portal.css';
  let bridgeJS = '/__p2p_gw__/bridge.js';
  let patchJS = '/__p2p_gw__/patch.js';
  const swJS = '/__p2p_gw__/sw.js';

  if (isWatch) {
    if (fs.existsSync('src/portal/portal.css')) {
      fs.copyFileSync('src/portal/portal.css', path.join(gwDir, 'portal.css'));
    }
  } else {
    if (fs.existsSync('src/portal/portal.css')) {
      portalCSS = writeHashedAsset('src/portal/portal.css', 'portal', 'css');
    }
    const renameToHashed = (name) => {
      const rawPath = path.join(gwDir, `${name}.js`);
      const data = fs.readFileSync(rawPath);
      const hashedName = `${name}.${shortHash(data)}.js`;
      fs.renameSync(rawPath, path.join(gwDir, hashedName));
      return `/__p2p_gw__/${hashedName}`;
    };
    portalJS = renameToHashed('portal');
    bridgeJS = renameToHashed('bridge');
    patchJS = renameToHashed('patch');
  }

  const manifest = {
    portal_js: portalJS,
    portal_css: portalCSS,
    bridge_js: bridgeJS,
    patch_js: patchJS,
    sw_js: swJS,
  };
  fs.writeFileSync(path.join(gwDir, 'manifest.json'), JSON.stringify(manifest, null, 2) + '\n');

  if (fs.existsSync('src/index.html')) {
    let html = fs.readFileSync('src/index.html', 'utf8');
    html = html.replace('/__p2p_gw__/portal.css', portalCSS);
    html = html.replace('/__p2p_gw__/portal.js', portalJS);
    fs.writeFileSync(path.join(outdir, 'index.html'), html);
  }
  if (fs.existsSync('src/bridge/bridge.html')) {
    let html = fs.readFileSync('src/bridge/bridge.html', 'utf8');
    html = html.replace('/__p2p_gw__/bridge.js', bridgeJS);
    fs.writeFileSync(path.join(gwDir, 'bridge.html'), html);
  }
}

async function build() {
  const isWatch = process.argv.includes('--watch');

  const commonDefine = {
    __APP_VERSION__: JSON.stringify(appVersion),
  };

  const portalOptions = {
    entryPoints: { portal: 'src/portal/app.ts' },
    bundle: true,
    outdir: gwDir,
    format: 'esm',
    target: ['es2022'],
    sourcemap: isWatch,
    minify: !isWatch,
    define: commonDefine,
    logLevel: 'info',
  };

  const classicOptions = {
    entryPoints: {
      sw: 'src/sw/sw.ts',
      patch: 'src/patch/ws_patch.ts',
      bridge: 'src/bridge/bridge.ts',
    },
    bundle: true,
    outdir: gwDir,
    format: 'iife',
    target: ['es2022'],
    sourcemap: isWatch,
    minify: !isWatch,
    define: commonDefine,
    logLevel: 'info',
  };

  if (isWatch) {
    finalizeHtmlAndManifest(true);
    const [portalCtx, classicCtx] = await Promise.all([
      esbuild.context(portalOptions),
      esbuild.context(classicOptions),
    ]);
    await Promise.all([portalCtx.watch(), classicCtx.watch()]);
    console.log(`Watching web assets (${appVersion})...`);
    return;
  }

  await Promise.all([esbuild.build(portalOptions), esbuild.build(classicOptions)]);
  finalizeHtmlAndManifest(false);
  console.log(`Web build completed successfully (${appVersion}).`);
}

build().catch((err) => {
  console.error(err);
  process.exit(1);
});
