/**
 * V7 Portal Application Entrypoint (/__p2p_gw__/portal.js)
 * - 128-bit CSPRNG `tabId` and one-time 128-bit `enterNonce` registered over `bridgePort` before mounting `appFrame`.
 * - Enforces strict iframe `sandbox` (NO `allow-top-navigation*`) & explicit `allow` policy on `appFrame`.
 * - 3-Tier sandbox derivation with per-device/per-node 128-bit salt + one-click "Reset Sandbox" button,
 *   plus per-tab capability origin reuse across silent reconnects.
 * - Continuously merges latest `CookieDelta` updates into `activeMeta.initialCookies` so SW wakeups
 *   never roll back non-HttpOnly cookies.
 */

import { randomHex } from '../shared/crypto';
import { resetTofuKey } from '../shared/identity';
import { isRecord, type BackendMeta, type CookieDelta } from '../shared/ipc';
import {
  computeSandboxOrigin,
  normalizeNodeId,
  resetNodeSandboxSalt,
} from '../shared/sandbox_origin';
import { TunnelBridge } from './core/bridge';
import {
  loadIcePreferences,
  parseRouteHash,
  readPortalMetaConfig,
  sanitizeHashForLog,
  scrubSensitiveHashParams,
  type IcePreferences,
} from './core/config';
import { PeerSession, type AuthSuccessInfo } from './core/peer_session';
import { StatsCollector } from './core/stats_collector';
import { subscribeLang, t } from './i18n';
import { PortalModals } from './ui/modals';
import { SmartPill } from './ui/smart_pill';

const APP_IFRAME_SANDBOX =
  'allow-scripts allow-same-origin allow-forms allow-modals allow-downloads allow-popups allow-popups-to-escape-sandbox allow-pointer-lock';
const APP_IFRAME_ALLOW = 'clipboard-read; clipboard-write; fullscreen';

const metaCfg = readPortalMetaConfig();
let icePrefs: IcePreferences = loadIcePreferences();

const landingEl = document.getElementById('landingView') as HTMLElement;
const landingSubEl = document.getElementById('landingSubtitle') as HTMLElement;
const nodeInputEl = document.getElementById('nodeIdInput') as HTMLInputElement;
const connectBtnEl = document.getElementById('connectBtn') as HTMLButtonElement;
const landingErrEl = document.getElementById('landingError') as HTMLElement;
const landingFormEl = document.getElementById('landingForm') as HTMLFormElement;
const versionBadgeEl = document.getElementById('versionBadge') as HTMLElement;
const statusBannerEl = document.getElementById('statusBanner') as HTMLElement;
const sameSiteBannerEl = document.getElementById('sameSiteBanner') as HTMLElement;
const stageEl = document.getElementById('appStage') as HTMLElement;
const modalHostEl = document.getElementById('modalHost') as HTMLElement;
const hudHostEl = document.getElementById('hudHost') as HTMLElement;

// 128-bit CSPRNG tabId (unguessable across tabs/origins).
const tabId = `tab_${randomHex(16)}`;

const modals = new PortalModals(modalHostEl);
const statsCollector = new StatsCollector();
let transport: PeerSession | null = null;
let activeBridge: TunnelBridge | null = null;
let bridgeFrame: HTMLIFrameElement | null = null;
let appFrame: HTMLIFrameElement | null = null;
let activeSandboxOrigin = '';
let tabCapSandboxOrigin = '';
let activeMeta: BackendMeta | null = null;
let activeAuthInfo: AuthSuccessInfo | null = null;
let activeIsCap = false;
let activeNodeId = '';
let currentAppPath = '/';
let bridgeReadyResolver: (() => void) | null = null;
let mountGeneration = 0;

// Portal-side live cookie mirror snapshot so SW sleep/wake never rolls back cookies.
const liveCookieSnapshot = new Map<string, CookieDelta>();

function mergeCookieDeltas(deltas: CookieDelta[] | undefined): void {
  if (!deltas || deltas.length === 0) return;
  const now = Date.now();
  for (const d of deltas) {
    const p = d.path && d.path.startsWith('/') ? d.path : '/';
    const key = `${p}\0${d.name}`;
    if (d.op === 'del' || (d.expires && d.expires > 0 && d.expires <= now)) {
      liveCookieSnapshot.delete(key);
    } else {
      liveCookieSnapshot.set(key, { ...d, path: p });
    }
  }
  if (activeMeta) {
    activeMeta = {
      ...activeMeta,
      initialCookies: exportLiveCookieSnapshot(),
    };
  }
}

function exportLiveCookieSnapshot(): CookieDelta[] {
  const now = Date.now();
  const out: CookieDelta[] = [];
  for (const [k, d] of liveCookieSnapshot) {
    if (d.expires && d.expires > 0 && d.expires <= now) {
      liveCookieSnapshot.delete(k);
      continue;
    }
    out.push(d);
  }
  return out;
}

const pill = new SmartPill(hudHostEl, {
  onShare: () => {
    if (!transport || !activeNodeId) return;
    modals.showShareModal(activeNodeId, location.origin, {
      mint: (opts) => transport!.requestCapabilityOp({ action: 'mint', ...opts }),
      list: () => transport!.requestCapabilityOp({ action: 'list' }),
      revoke: (jti) => transport!.requestCapabilityOp({ action: 'revoke', jti }),
    });
  },
  onSettings: () => {
    modals.showSettingsModal(icePrefs, (next) => {
      icePrefs = next;
      if (transport) {
        transport.updateIcePreferences(next);
        void transport.connect();
      }
    });
  },
  onReconnect: () => {
    if (transport) {
      void transport.connect();
    }
  },
  onResetSandbox: () => {
    if (!activeNodeId || !activeAuthInfo || activeIsCap || metaCfg.sandboxMode === 'fixed') return;
    resetNodeSandboxSalt(activeNodeId);
    activeSandboxOrigin = '';
    pill.appendLog(`[Portal] Rotated 128-bit sandbox salt for node=${activeNodeId}; remounting`);
    void mountSandboxAndApp(activeAuthInfo, false);
  },
});

function refreshStaticText(): void {
  landingSubEl.textContent = t('landing.subtitle');
  nodeInputEl.placeholder = t('landing.nodePlaceholder');
  connectBtnEl.textContent = t('landing.connect');
  sameSiteBannerEl.textContent = t('banner.sameSite');
  versionBadgeEl.textContent = metaCfg.version;
  pill.refreshLabels();
}

subscribeLang(() => refreshStaticText());
refreshStaticText();

if (metaCfg.sameSiteMode) {
  sameSiteBannerEl.style.display = 'block';
}

function setBanner(visible: boolean, text = '', isError = false): void {
  statusBannerEl.style.display = visible ? 'block' : 'none';
  statusBannerEl.textContent = text;
  statusBannerEl.className = isError ? 'p2p-status-banner p2p-status-error' : 'p2p-status-banner';
}

function showLanding(): void {
  teardownSession();
  landingEl.style.display = 'flex';
  stageEl.style.display = 'none';
  setBanner(false);
  pill.setStatus('idle', '');
}

function teardownSession(): void {
  mountGeneration++;
  statsCollector.stop();
  modals.close();
  if (transport) {
    transport.destroy();
    transport = null;
  }
  if (activeBridge) {
    activeBridge.detachWindowListener();
    activeBridge.destroy();
    activeBridge = null;
  }
  if (appFrame) {
    appFrame.remove();
    appFrame = null;
  }
  if (bridgeFrame) {
    bridgeFrame.remove();
    bridgeFrame = null;
  }
  activeSandboxOrigin = '';
  tabCapSandboxOrigin = '';
  activeMeta = null;
  activeAuthInfo = null;
  activeIsCap = false;
  activeNodeId = '';
  liveCookieSnapshot.clear();
  bridgeReadyResolver = null;
}

function sendFreshBridgePort(sandboxOrigin: string, meta: BackendMeta): void {
  if (!bridgeFrame?.contentWindow || !activeBridge) return;
  const ch = new MessageChannel();
  activeBridge.attachBridgePort(ch.port1);
  const freshMeta: BackendMeta = {
    ...meta,
    initialCookies: exportLiveCookieSnapshot(),
  };
  bridgeFrame.contentWindow.postMessage(
    {
      type: 'P2P_PORTAL_INIT',
      tabId,
      meta: freshMeta,
    },
    sandboxOrigin,
    [ch.port2]
  );
}

// Listen for Bridge iframe lifecycle messages (`P2P_BRIDGE_READY` & `P2P_BRIDGE_NEED_PORT`).
window.addEventListener('message', (ev: MessageEvent) => {
  if (!activeSandboxOrigin || ev.origin !== activeSandboxOrigin) return;
  if (!bridgeFrame || ev.source !== bridgeFrame.contentWindow) return;
  const data = ev.data;
  if (!isRecord(data) || typeof data.type !== 'string') return;
  if (data.tabId !== tabId) return;

  if (data.type === 'P2P_BRIDGE_READY') {
    const res = bridgeReadyResolver;
    bridgeReadyResolver = null;
    res?.();
  } else if (data.type === 'P2P_BRIDGE_NEED_PORT' && activeMeta) {
    pill.appendLog('[Bridge] Service Worker requested fresh bridgePort — reattaching');
    sendFreshBridgePort(activeSandboxOrigin, activeMeta);
  }
});

async function mountSandboxAndApp(info: AuthSuccessInfo, isCap: boolean): Promise<void> {
  const myMountGen = ++mountGeneration;
  const activeTransport = transport;
  if (!activeTransport) return;
  const mux = activeTransport.getMux();
  if (!mux) return;

  activeAuthInfo = info;
  activeIsCap = isCap;

  const targetOrigin = info.meta.backendOrigin || `http://${info.meta.nodeId}.internal`;
  let nextSandboxOrigin: string;
  if (isCap && tabCapSandboxOrigin) {
    // Reuse the per-tab 128-bit capability origin across silent reconnects so `appFrame` stays alive!
    nextSandboxOrigin = tabCapSandboxOrigin;
  } else {
    nextSandboxOrigin = await computeSandboxOrigin(
      metaCfg,
      location.protocol,
      info.meta.nodeId,
      targetOrigin,
      isCap
    );
    if (isCap) {
      tabCapSandboxOrigin = nextSandboxOrigin;
    }
  }
  if (myMountGen !== mountGeneration || transport !== activeTransport) return;

  mergeCookieDeltas(info.meta.initialCookies);
  activeMeta = {
    ...info.meta,
    initialCookies: exportLiveCookieSnapshot(),
  };
  const openWSDC = () => activeTransport.openWSDC();

  if (!activeBridge) {
    activeBridge = new TunnelBridge({
      onRouteSync: (path) => {
        currentAppPath = path;
      },
      onTitleSync: (title) => {
        document.title = `${title} · ${t('app.title')}`;
      },
      onCookieDeltas: (deltas) => {
        mergeCookieDeltas(deltas);
      },
      onLog: (line) => pill.appendLog(line),
    });
    activeBridge.attachWindowListener();
  }

  // If sandbox origin matches an already-mounted bridge (silent reconnect),
  // reuse the live app iframe & re-bind the fresh PoolMux + bridgePort so SPA route stays intact!
  if (activeSandboxOrigin === nextSandboxOrigin && bridgeFrame && appFrame) {
    activeBridge.setContext(mux, openWSDC, nextSandboxOrigin, activeMeta, appFrame);
    sendFreshBridgePort(nextSandboxOrigin, activeMeta);
    setBanner(false);
    return;
  }

  activeSandboxOrigin = nextSandboxOrigin;
  if (appFrame) {
    appFrame.remove();
    appFrame = null;
  }
  if (bridgeFrame) {
    bridgeFrame.remove();
    bridgeFrame = null;
  }

  activeBridge.setContext(mux, openWSDC, nextSandboxOrigin, activeMeta, null);

  // 1. Mount hidden non-navigable bridge iframe (`/__p2p_gw__/bridge.html?tab=...`).
  const bFrame = document.createElement('iframe');
  bFrame.className = 'p2p-hidden-bridge';
  bFrame.setAttribute('aria-hidden', 'true');
  bFrame.setAttribute('allow', '');
  bFrame.src = `${nextSandboxOrigin}/__p2p_gw__/bridge.html?tab=${encodeURIComponent(tabId)}`;
  bridgeFrame = bFrame;

  const readyPromise = new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => {
      bridgeReadyResolver = null;
      reject(new Error('Timed out waiting for sandbox Service Worker bridge to initialize'));
    }, 15_000);
    bridgeReadyResolver = () => {
      clearTimeout(timer);
      resolve();
    };
  });

  bFrame.addEventListener('load', () => {
    if (myMountGen === mountGeneration && bridgeFrame === bFrame && activeMeta) {
      sendFreshBridgePort(nextSandboxOrigin, activeMeta);
    }
  });
  stageEl.appendChild(bFrame);

  try {
    await readyPromise;
  } catch (err) {
    if (myMountGen !== mountGeneration) return;
    const msg = err instanceof Error ? err.message : 'Bridge initialization failed';
    setBanner(true, msg, true);
    pill.appendLog(`[Bridge] Error: ${msg}`);
    return;
  }
  if (myMountGen !== mountGeneration || transport !== activeTransport) return;

  // 2. Register one-time 128-bit `enterNonce` over `bridgePort` BEFORE mounting `appFrame`!
  const safePath = currentAppPath.startsWith('/') ? currentAppPath : '/';
  const enterNonce = randomHex(16);
  try {
    await activeBridge.registerEnterNonce(enterNonce, safePath);
  } catch (err) {
    if (myMountGen !== mountGeneration) return;
    const msg = err instanceof Error ? err.message : 'Failed to register enter nonce';
    setBanner(true, msg, true);
    pill.appendLog(`[Bridge] Error: ${msg}`);
    return;
  }
  if (myMountGen !== mountGeneration || transport !== activeTransport) return;

  // 3. Mount the isolated sandboxed app iframe (NO allow-top-navigation*).
  const aFrame = document.createElement('iframe');
  aFrame.className = 'p2p-app-frame';
  aFrame.setAttribute('sandbox', APP_IFRAME_SANDBOX);
  aFrame.setAttribute('allow', APP_IFRAME_ALLOW);
  aFrame.src =
    `${nextSandboxOrigin}/__p2p_gw__/enter` +
    `?tab=${encodeURIComponent(tabId)}` +
    `&nonce=${encodeURIComponent(enterNonce)}` +
    `&next=${encodeURIComponent(safePath)}`;
  appFrame = aFrame;
  activeBridge.setAppFrame(aFrame);
  stageEl.appendChild(aFrame);
  stageEl.style.display = 'block';
  landingEl.style.display = 'none';
  setBanner(false);
}

function startRouteConnection(): void {
  const rawHash = location.hash;
  const parsed = parseRouteHash(rawHash);
  if (!parsed.nodeId) {
    showLanding();
    return;
  }

  // Scrub bearer `cap=` from the URL hash immediately so it never lingers in history!
  const cleanedHash = scrubSensitiveHashParams(rawHash);
  if (cleanedHash !== rawHash) {
    try {
      history.replaceState(null, '', cleanedHash);
    } catch {
      // ignore if restricted
    }
  }

  pill.appendLog(`[Portal] Connecting to node=${parsed.nodeId} hash=${sanitizeHashForLog(rawHash)}`);

  teardownSession();
  activeNodeId = parsed.nodeId;
  currentAppPath = parsed.initialPath || '/';
  landingEl.style.display = 'none';
  stageEl.style.display = 'block';
  setBanner(true, t('status.connecting'));
  pill.setStatus('connecting', activeNodeId);

  const isCap = Boolean(parsed.capToken);

  transport = new PeerSession(
    parsed.nodeId,
    location.origin,
    metaCfg.sameSiteMode,
    parsed.pinnedPk,
    parsed.capToken,
    icePrefs,
    {
      onStatus: (status, detail) => {
        pill.setStatus(status, activeNodeId);
        if (status === 'error') {
          setBanner(true, detail || t('status.error'), true);
        } else if (status !== 'connected') {
          setBanner(true, detail || t(`status.${status}`));
        }
      },
      onLog: (line) => pill.appendLog(line),
      onAuthRequired: (nodeId) => {
        modals.showAuthModal(
          nodeId,
          null,
          async (secret) => {
            await transport?.submitOwnerPassword(secret);
          },
          () => {
            location.hash = '';
          }
        );
      },
      onAuthFailed: (code, message, retryAfterSec) => {
        const display =
          code === 'RATE_LIMITED' && retryAfterSec
            ? t('auth.rateLimited', { sec: retryAfterSec })
            : message;
        if (isCap) {
          setBanner(true, display, true);
          pill.setStatus('error', activeNodeId);
        } else {
          modals.showAuthModal(
            activeNodeId,
            display,
            async (secret) => {
              if (!transport) return;
              await transport.submitOwnerPassword(secret);
            },
            () => {
              location.hash = '';
            }
          );
        }
      },
      onKeyMismatch: (result, isPinned) => {
        pill.setStatus('error', activeNodeId);
        setBanner(true, t('tofu.title'), true);
        modals.showKeyMismatchModal(
          result,
          isPinned,
          async () => {
            await resetTofuKey(`${location.origin}::${activeNodeId}`);
            void transport?.connect();
          },
          () => {
            location.hash = '';
          }
        );
      },
      onAuthenticated: (info, _mux) => {
        modals.close();
        pill.setStatus('connected', activeNodeId);
        pill.setSessionInfo({
          nodeId: activeNodeId,
          role: info.role,
          pkHash: info.pkHash,
          version: metaCfg.version,
          sameSiteMode: metaCfg.sameSiteMode,
          canResetSandbox: !isCap && metaCfg.sandboxMode !== 'fixed',
        });
        statsCollector.start(
          () => transport?.getPeerConnection() ?? null,
          () => transport?.getMux() ?? null,
          (snap) => pill.updateStats(snap)
        );
        void mountSandboxAndApp(info, isCap);
      },
      onDisconnected: (reason, willReconnect) => {
        pill.setStatus(willReconnect ? 'connecting' : 'disconnected', activeNodeId);
        if (!willReconnect) {
          setBanner(true, `${t('status.disconnected')}: ${reason}`, true);
        }
      },
    }
  );

  void transport.connect();
}

landingFormEl.addEventListener('submit', (ev) => {
  ev.preventDefault();
  landingErrEl.style.display = 'none';
  const norm = normalizeNodeId(nodeInputEl.value);
  if (!norm) {
    landingErrEl.textContent = t('landing.invalidNode');
    landingErrEl.style.display = 'block';
    return;
  }
  location.hash = `#${encodeURIComponent(norm)}`;
});

window.addEventListener('hashchange', () => {
  startRouteConnection();
});

// Explicitly wipe RAM-only SPAKE2+ (w0, w1) and session state on tab unload / bfcache transition.
window.addEventListener('pagehide', () => {
  teardownSession();
});

// Boot initial route cleanly.
startRouteConnection();
