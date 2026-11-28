/**
 * Portal Configuration, Hash Parsing, Secret Scrubbing, and RFC 7065 ICE Validation
 * - Connects strictly to same-origin signaling (`location.origin`).
 * - Scrubs `cap=` from `location.hash` via `history.replaceState` immediately after parsing.
 * - Decodes percent-encoded parameter names before redacting sensitive values in logs.
 * - Respects `relayOnly` by omitting default STUN servers when relay-only mode is enabled.
 */

import { isRecord } from '../../shared/ipc';
import { normalizeNodeId, type PortalDomainConfig } from '../../shared/sandbox_origin';

const DEFAULT_STUN_SERVERS: readonly string[] = [
  'stun:stun.cloudflare.com:3478',
  'stun:stun.l.google.com:19302',
];

export interface PortalMetaConfig extends PortalDomainConfig {
  version: string;
}

export interface ParsedRouteHash {
  nodeId: string;
  pinnedPk: string;
  capToken: string;
  initialPath: string;
}

export interface IcePreferences {
  customStun: string;
  turnUrl: string;
  turnUser: string;
  turnPass: string;
  relayOnly: boolean;
}

const ICE_PREFS_STORAGE_KEY = 'p2p_gw_ice_prefs';
const LANG_STORAGE_KEY = 'p2p_gw_lang';

const RFC7065_ICE_URI_RE =
  /^(stuns?|turns?):(\[[0-9a-fA-F:]+\]|[A-Za-z0-9.-]+)(?::(\d{1,5}))?(?:\?transport=(udp|tcp))?$/i;

export function validateIceServerUrl(raw: string): { ok: boolean; normalized: string; error?: string } {
  const s = raw.trim();
  if (!s) return { ok: false, normalized: '', error: 'empty ICE URI' };
  if (s.includes('://')) {
    return { ok: false, normalized: '', error: 'RFC 7065 STUN/TURN URI must not contain "://"' };
  }
  const m = RFC7065_ICE_URI_RE.exec(s);
  if (!m) {
    return { ok: false, normalized: '', error: `invalid STUN/TURN URI: ${s}` };
  }
  if (m[3]) {
    const port = Number(m[3]);
    if (!Number.isInteger(port) || port < 1 || port > 65535) {
      return { ok: false, normalized: '', error: `invalid ICE port: ${m[3]}` };
    }
  }
  return { ok: true, normalized: s };
}

export function buildIceServers(prefs: IcePreferences): RTCIceServer[] {
  const servers: RTCIceServer[] = [];

  if (!prefs.relayOnly) {
    const stunList: string[] = [];
    if (prefs.customStun.trim()) {
      for (const part of prefs.customStun.split(',')) {
        const check = validateIceServerUrl(part);
        if (check.ok && /^stuns?:/i.test(check.normalized)) {
          stunList.push(check.normalized);
        }
      }
    }
    if (stunList.length === 0) {
      stunList.push(...DEFAULT_STUN_SERVERS);
    }
    servers.push({ urls: stunList });
  }

  if (prefs.turnUrl.trim()) {
    const check = validateIceServerUrl(prefs.turnUrl);
    if (check.ok && /^turns?:/i.test(check.normalized)) {
      const turnServer: RTCIceServer = { urls: check.normalized };
      if (prefs.turnUser.trim()) turnServer.username = prefs.turnUser.trim();
      if (prefs.turnPass) turnServer.credential = prefs.turnPass;
      servers.push(turnServer);
    }
  }
  return servers;
}

export function icePrefsEqual(a: IcePreferences, b: IcePreferences): boolean {
  return (
    a.customStun.trim() === b.customStun.trim() &&
    a.turnUrl.trim() === b.turnUrl.trim() &&
    a.turnUser.trim() === b.turnUser.trim() &&
    a.turnPass === b.turnPass &&
    a.relayOnly === b.relayOnly
  );
}

export function readPortalMetaConfig(doc: Document = document): PortalMetaConfig {
  const host = typeof location !== 'undefined' ? location.hostname : '';
  const isLoopback = host === 'localhost' || host === '127.0.0.1' || host === '::1' || host === '[::1]';
  const fallback: PortalMetaConfig = {
    portalOrigin: '',
    portalDomain: '',
    sandboxOrigin: '',
    sandboxMode: isLoopback ? 'local' : 'wildcard',
    sandboxDomain: '',
    sandboxHost: '',
    restrictedSandbox: false,
    sameSiteMode: false,
    localDevMode: isLoopback,
    localPort: typeof location !== 'undefined' ? location.port : '',
    version: typeof __APP_VERSION__ !== 'undefined' ? __APP_VERSION__ : 'v8-dev',
  };

  const metaEl = doc.querySelector('meta[name="p2p-config"]');
  const raw = metaEl?.getAttribute('content');
  if (!raw) return fallback;
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (!isRecord(parsed)) return fallback;
    const rawMode = parsed.sandboxMode;
    const sandboxMode: 'local' | 'fixed' | 'wildcard' =
      rawMode === 'local' || rawMode === 'fixed' || rawMode === 'wildcard'
        ? rawMode
        : Boolean(parsed.localDevMode)
          ? 'local'
          : 'wildcard';
    const sameSiteMode = Boolean(parsed.sameSiteMode);
    const restrictedSandbox = Boolean(parsed.restrictedSandbox) || sameSiteMode || sandboxMode === 'fixed';
    return {
      portalOrigin: typeof parsed.portalOrigin === 'string' ? parsed.portalOrigin : fallback.portalOrigin,
      portalDomain: typeof parsed.portalDomain === 'string' ? parsed.portalDomain : fallback.portalDomain,
      sandboxOrigin: typeof parsed.sandboxOrigin === 'string' ? parsed.sandboxOrigin : fallback.sandboxOrigin,
      sandboxMode,
      sandboxDomain: typeof parsed.sandboxDomain === 'string' ? parsed.sandboxDomain : fallback.sandboxDomain,
      sandboxHost: typeof parsed.sandboxHost === 'string' ? parsed.sandboxHost : '',
      restrictedSandbox,
      sameSiteMode,
      localDevMode: Boolean(parsed.localDevMode),
      localPort: typeof parsed.localPort === 'string' ? parsed.localPort : fallback.localPort,
      version: typeof parsed.version === 'string' && parsed.version ? parsed.version : fallback.version,
    };
  } catch {
    return fallback;
  }
}

export function parseRouteHash(rawHash: string): ParsedRouteHash {
  const stripped = rawHash.replace(/^#\/?/, '');
  if (!stripped) {
    return { nodeId: '', pinnedPk: '', capToken: '', initialPath: '/' };
  }

  const qIdx = stripped.indexOf('?');
  const nodePart = qIdx >= 0 ? stripped.slice(0, qIdx) : stripped;
  const queryPart = qIdx >= 0 ? stripped.slice(qIdx + 1) : '';

  let decodedNode = nodePart;
  try {
    decodedNode = decodeURIComponent(nodePart);
  } catch {
    // keep raw
  }
  const nodeId = normalizeNodeId(decodedNode) ?? '';

  let pinnedPk = '';
  let capToken = '';
  let initialPath = '/';

  if (queryPart) {
    for (const segment of queryPart.split('&')) {
      if (!segment) continue;
      const eq = segment.indexOf('=');
      const rawKey = eq >= 0 ? segment.slice(0, eq) : segment;
      const rawVal = eq >= 0 ? segment.slice(eq + 1) : '';
      let key = rawKey;
      let val = rawVal;
      try {
        key = decodeURIComponent(rawKey);
        val = decodeURIComponent(rawVal);
      } catch {
        // ignore malformed percent encoding
      }
      switch (key) {
        case 'pk':
          pinnedPk = val.trim();
          break;
        case 'cap':
          capToken = val.trim();
          break;
        case 'path':
          if (val.startsWith('/')) initialPath = val;
          break;
      }
    }
  }

  return { nodeId, pinnedPk, capToken, initialPath };
}

export function scrubSensitiveHashParams(rawHash: string): string {
  const qIdx = rawHash.indexOf('?');
  if (qIdx < 0) return rawHash;
  const prefix = rawHash.slice(0, qIdx);
  const query = rawHash.slice(qIdx + 1);
  const kept: string[] = [];
  for (const seg of query.split('&')) {
    if (!seg) continue;
    const eq = seg.indexOf('=');
    const rawKey = eq >= 0 ? seg.slice(0, eq) : seg;
    let key = rawKey;
    try {
      key = decodeURIComponent(rawKey);
    } catch {
      // keep raw
    }
    if (/^(?:cap|token|secret|password)$/i.test(key)) continue;
    kept.push(seg);
  }
  return kept.length > 0 ? `${prefix}?${kept.join('&')}` : prefix;
}

export function sanitizeHashForLog(rawHash: string): string {
  const qIdx = rawHash.indexOf('?');
  if (qIdx < 0) return rawHash;
  const prefix = rawHash.slice(0, qIdx);
  const query = rawHash.slice(qIdx + 1);
  const out = query.split('&').map((seg) => {
    const eq = seg.indexOf('=');
    if (eq < 0) return seg;
    const rawKey = seg.slice(0, eq);
    let key = rawKey;
    try {
      key = decodeURIComponent(rawKey);
    } catch {
      // keep raw
    }
    if (/^(?:cap|token|secret|password)$/i.test(key)) {
      return `${rawKey}=[REDACTED]`;
    }
    return seg;
  });
  return `${prefix}?${out.join('&')}`;
}

export function loadIcePreferences(): IcePreferences {
  const def: IcePreferences = {
    customStun: '',
    turnUrl: '',
    turnUser: '',
    turnPass: '',
    relayOnly: false,
  };
  try {
    const raw = localStorage.getItem(ICE_PREFS_STORAGE_KEY);
    if (!raw) return def;
    const parsed = JSON.parse(raw) as unknown;
    if (!isRecord(parsed)) return def;
    return {
      customStun: typeof parsed.customStun === 'string' ? parsed.customStun : '',
      turnUrl: typeof parsed.turnUrl === 'string' ? parsed.turnUrl : '',
      turnUser: typeof parsed.turnUser === 'string' ? parsed.turnUser : '',
      turnPass: '',
      relayOnly: Boolean(parsed.relayOnly),
    };
  } catch {
    return def;
  }
}

export function saveIcePreferences(prefs: IcePreferences): void {
  try {
    localStorage.setItem(
      ICE_PREFS_STORAGE_KEY,
      JSON.stringify({
        customStun: prefs.customStun.trim(),
        turnUrl: prefs.turnUrl.trim(),
        turnUser: prefs.turnUser.trim(),
        relayOnly: Boolean(prefs.relayOnly),
      })
    );
  } catch {
    // ignore storage quota errors
  }
}

export function loadSavedLang(): 'zh' | 'en' {
  try {
    const saved = localStorage.getItem(LANG_STORAGE_KEY);
    if (saved === 'zh' || saved === 'en') return saved;
  } catch {
    // ignore
  }
  const nav = typeof navigator !== 'undefined' ? navigator.language.toLowerCase() : 'en';
  return nav.startsWith('zh') ? 'zh' : 'en';
}

export function saveLang(lang: 'zh' | 'en'): void {
  try {
    localStorage.setItem(LANG_STORAGE_KEY, lang);
  } catch {
    // ignore
  }
}
