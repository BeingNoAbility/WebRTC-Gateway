/**
 * Portal Application Entrypoint (/__p2p_gw__/portal.js)
 * - Explicit AbortController-scoped session lifecycle + bfcache pageshow recovery.
 * - 128-bit CSPRNG `tabId` and one-time 128-bit `enterNonce` bound to exact `(tabId, nextPath)`.
 * - Prominent Tier 2 restricted-isolation warning banner for both same-site wildcard AND
 *   fixed single-host sandbox deployments (Tier 1 strictly reserved for cross-site wildcard).
 * - Continuously merges server & client `CookieDelta` updates so SW sleep/wake never rolls
 *   back cookies, while Reset Sandbox starts cleanly without re-injecting stale cookies.
 */

import { CookieMirrorStore } from '../shared/cookie_mirror';
import { randomHex } from '../shared/crypto';
import { resetTofuKey } from '../shared/identity';
import { isRecord, type BackendMeta, type CookieDelta } from '../shared/ipc';
import {
  computeSandboxOrigin,
  isRestrictedSandbox,
  normalizeNodeId,
  resetNodeSandboxSalt,
} from '../shared/sandbox_origin';
import { TunnelBridge } from './core/bridge';
import {
  icePrefsEqual,
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
const APP_IFRAME_ALLOW = 'clipboard-write; fullscreen';
const BRIDGE_IFRAME_SANDBOX = 'allow-scripts allow-same-origin';

const metaCfg = readPortalMetaConfig();
const restrictedSandbox = isRestrictedSandbox(metaCfg);
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
let sessionAbort: AbortController | null = null;
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
let mountGeneration = 0;
let recoveringSw = false;

// Portal-side live cookie mirror snapshot so SW sleep/wake never rolls back cookies.
const liveCookieSnapshot = new CookieMirrorStore();

function mergeCookieDeltas(deltas: CookieDelta[] | undefined): void {
  if (!deltas || deltas.length === 0) return;
  liveCookieSnapshot.applyDeltas(deltas);
  if (activeMeta) {
    activeMeta = {
      ...activeMeta,
      initialCookies: liveCookieSnapshot.snapshot(),
    };
  }
}

function exportLiveCookieSnapshot(): CookieDelta[] {
  return liveCookieSnapshot.snapshot();
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
      const changed = !icePrefsEqual(icePrefs, next);
      icePrefs = next;
      // Do NOT tear down and reconnect the active tunnel when only the UI language changed!
      if (changed && transport) {
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
    liveCookieSnapshot.clear();
    if (appFrame) {
      appFrame.remove();
      appFrame = null;
    }
    if (activeBridge) {
      activeBridge.detachWindowListener();
      activeBridge.destroy();
      activeBridge = null;
    }
    pill.appendLog(`[Portal] Rotated 128-bit sandbox salt for node=${activeNodeId}; reconnecting with fresh CookieJar`);
    if (transport) {
      void transport.resetCookieSessionAndReconnect();
    } else {
      void mountSandboxAndApp(activeAuthInfo, false, true);
    }
  },
});

function refreshStaticText(): void {
  landingSubEl.textContent = t('landing.subtitle');
  nodeInputEl.placeholder = t('landing.nodePlaceholder');
  connectBtnEl.textContent = t('landing.connect');
  sameSiteBannerEl.textContent = metaCfg.sameSiteMode
    ? t('banner.sameSite')
    : t('banner.fixedSandbox');
  versionBadgeEl.textContent = metaCfg.version;
  pill.refreshLabels();
}

subscribeLang(() => refreshStaticText());
refreshStaticText();

if (restrictedSandbox) {
  sameSiteBannerEl.style.display = 'block';
  sameSiteBannerEl.setAttribute('data-sandbox-tier', 'tier2');
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
  if (sessionAbort) {
    sessionAbort.abort();
    sessionAbort = null;
  }
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
  recoveringSw = false;
}

async function attachEphemeralBridge(
  sandboxOrigin: string,
  meta: BackendMeta,
  myMountGen: number,
  signal: AbortSignal
): Promise<void> {
  if (!activeBridge) throw new Error('TunnelBridge not initialized');
  if (appFrame) {
    appFrame.remove();
    appFrame = null;
    activeBridge.setAppFrame(null);
  }
  if (bridgeFrame) {
    bridgeFrame.remove();
    bridgeFrame = null;
  }

  const bFrame = document.createElement('iframe');
  bFrame.className = 'p2p-hidden-bridge';
  bFrame.setAttribute('aria-hidden', 'true');
  bFrame.setAttribute('sandbox', BRIDGE_IFRAME_SANDBOX);
  bFrame.setAttribute('allow', '');
  bFrame.src = `${sandboxOrigin}/__p2p_gw__/bridge.html?tab=${encodeURIComponent(tabId)}`;
  bridgeFrame = bFrame;

  const readyPromise = new Promise<void>((resolve, reject) => {
    if (signal.aborted) {
      reject(new Error('Session aborted'));
      return;
    }
    const onAbort = () => {
      cleanup();
      reject(new Error('Session aborted'));
    };
    const onMessage = (ev: MessageEvent) => {
      if (ev.isTrusted === false) return;
      if (ev.origin !== sandboxOrigin || ev.source !== bFrame.contentWindow) return;
      const data = ev.data;
      if (!isRecord(data) || data.type !== 'P2P_BRIDGE_READY' || data.tabId !== tabId) return;
      cleanup();
      resolve();
    };
    const timer = setTimeout(() => {
      cleanup();
      reject(new Error('Timed out waiting for sandbox Service Worker bridge to initialize'));
    }, 15_000);
    const cleanup = () => {
      clearTimeout(timer);
      window.removeEventListener('message', onMessage);
      signal.removeEventListener('abort', onAbort);
    };
    window.addEventListener('message', onMessage);
    signal.addEventListener('abort', onAbort, { once: true });

    bFrame.addEventListener(
      'load',
      () => {
        if (signal.aborted || myMountGen !== mountGeneration || bridgeFrame !== bFrame || !bFrame.contentWindow || !activeBridge) {
          return;
        }
        const ch = new MessageChannel();
        activeBridge.attachBridgePort(ch.port1);
        const freshMeta: BackendMeta = {
          ...meta,
          initialCookies: exportLiveCookieSnapshot(),
        };
        bFrame.contentWindow.postMessage(
          {
            type: 'P2P_PORTAL_INIT',
            tabId,
            meta: freshMeta,
          },
          sandboxOrigin,
          [ch.port2]
        );
      },
      { once: true }
    );
  });

  stageEl.appendChild(bFrame);
  try {
    await readyPromise;
  } finally {
    bFrame.remove();
    if (bridgeFrame === bFrame) {
      bridgeFrame = null;
    }
  }
}

async function recoverEvictedSw(): Promise<void> {
  if (recoveringSw || !activeBridge || !activeAuthInfo || !activeSandboxOrigin || !activeMeta) return;
  if (await activeBridge.pingSw(1_500)) {
    return;
  }
  if (recoveringSw || !activeAuthInfo) return;
  recoveringSw = true;
  try {
    pill.appendLog('[Bridge] Service Worker was evicted by browser — re-bootstrapping sandbox cleanly');
    if (appFrame) {
      appFrame.remove();
      appFrame = null;
      activeBridge.setAppFrame(null);
    }
    await mountSandboxAndApp(activeAuthInfo, activeIsCap, false);
  } finally {
    recoveringSw = false;
  }
}

async function mountSandboxAndApp(
  info: AuthSuccessInfo,
  isCap: boolean,
  skipInitialCookiesMerge = false
): Promise<void> {
  const myMountGen = ++mountGeneration;
  const activeTransport = transport;
  const signal = sessionAbort?.signal;
  if (!activeTransport || !signal || signal.aborted) return;
  const mux = activeTransport.getMux();
  if (!mux) return;

  activeAuthInfo = info;
  activeIsCap = isCap;

  const targetOrigin = info.meta.backendOrigin || `http://${info.meta.nodeId}.internal`;
  let nextSandboxOrigin: string;
  if (isCap && tabCapSandboxOrigin) {
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
  if (signal.aborted || myMountGen !== mountGeneration || transport !== activeTransport) return;

  if (!skipInitialCookiesMerge) {
    mergeCookieDeltas(info.meta.initialCookies);
  }
  activeMeta = {
    ...info.meta,
    initialCookies: exportLiveCookieSnapshot(),
  };

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
      onSwEvicted: () => {
        void recoverEvictedSw();
      },
      onLog: (line) => pill.appendLog(line),
    });
    activeBridge.attachWindowListener();
  }

  // Silent WebRTC reconnect fast path: preserve appFrame when SW still holds our live bridgePort.
  if (activeSandboxOrigin === nextSandboxOrigin && appFrame) {
    const swAlive = await activeBridge.pingSw(3_000);
    if (signal.aborted || myMountGen !== mountGeneration || transport !== activeTransport) return;
    if (swAlive && appFrame) {
      activeBridge.setContext(mux, nextSandboxOrigin, activeMeta, appFrame);
      setBanner(false);
      return;
    }
    pill.appendLog('[Bridge] SW inactive during silent reconnect — detaching appFrame for clean re-bootstrap');
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

  activeBridge.setContext(mux, nextSandboxOrigin, activeMeta, null);

  try {
    await attachEphemeralBridge(nextSandboxOrigin, activeMeta, myMountGen, signal);
  } catch (err) {
    if (signal.aborted || myMountGen !== mountGeneration) return;
    const msg = err instanceof Error ? err.message : 'Bridge initialization failed';
    setBanner(true, msg, true);
    pill.appendLog(`[Bridge] Error: ${msg}`);
    return;
  }
  if (signal.aborted || myMountGen !== mountGeneration || transport !== activeTransport) return;

  const safePath = currentAppPath.startsWith('/') ? currentAppPath : '/';
  const enterNonce = randomHex(16);
  try {
    await activeBridge.registerEnterNonce(enterNonce, safePath);
  } catch (err) {
    if (signal.aborted || myMountGen !== mountGeneration) return;
    const msg = err instanceof Error ? err.message : 'Failed to register enter nonce';
    setBanner(true, msg, true);
    pill.appendLog(`[Bridge] Error: ${msg}`);
    return;
  }
  if (signal.aborted || myMountGen !== mountGeneration || transport !== activeTransport) return;

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
  sessionAbort = new AbortController();
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
          restrictedSandbox,
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

// Explicitly wipe RAM-only SPAKE2+ (w0, w1) and session state on tab unload / bfcache transition,
// and cleanly re-bootstrap if restored from bfcache!
window.addEventListener('pagehide', () => {
  teardownSession();
});
window.addEventListener('pageshow', (ev: PageTransitionEvent) => {
  if (ev.persisted) {
    startRouteConnection();
  }
});

// Boot initial route cleanly.
startRouteConnection();
