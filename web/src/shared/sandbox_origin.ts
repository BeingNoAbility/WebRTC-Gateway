/**
 * Cross-Site Sandbox Origin Derivation
 * - Tier 1 (Full Isolation): cross-site wildcard (`sandboxMode === 'wildcard'`, `!sameSiteMode`)
 *   or loopback LocalDev (`*.localhost`). Each owner node gets a per-device/per-node 96-bit HMAC
 *   label (`sbx-<24hex>`); capability share links get a 128-bit ephemeral label (`sbx-<32hex>`).
 * - Tier 2 (Restricted Isolation):
 *   a) Fixed single sandbox host (`sandboxMode === 'fixed'`): cross-site vs Portal, shared across tabs.
 *   b) Same-site wildcard (`sameSiteMode === true`): adds Origin-Agent-Cluster: ?1.
 */

import { hmacSha256Bytes, randomHex } from './crypto';

const NODE_ID_RE = /^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$/;
const DEVICE_SALT_STORAGE_KEY = 'p2p_gw_device_salt';
const NODE_SALT_PREFIX = 'p2p_gw_node_salt:';

const memorySalts = new Map<string, string>();
const textEncoder = new TextEncoder();

export interface PortalDomainConfig {
  portalOrigin?: string;
  portalDomain: string;
  sandboxOrigin?: string;
  sandboxMode?: 'local' | 'fixed' | 'wildcard';
  sandboxDomain: string;
  sandboxHost?: string;
  restrictedSandbox?: boolean;
  sameSiteMode: boolean;
  localDevMode: boolean;
  localPort?: string;
}

export function isRestrictedSandbox(cfg: PortalDomainConfig): boolean {
  return Boolean(cfg.restrictedSandbox || cfg.sameSiteMode || cfg.sandboxMode === 'fixed');
}

export function normalizeNodeId(raw: string): string | null {
  const id = String(raw ?? '').trim().toLowerCase();
  if (!NODE_ID_RE.test(id)) return null;
  if (id.startsWith('sbx-') || id.includes('..')) return null;
  return id;
}

interface StorageLike {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

function getGlobalStorage(): StorageLike | null {
  try {
    const s = (globalThis as { localStorage?: StorageLike }).localStorage;
    return s && typeof s.getItem === 'function' && typeof s.setItem === 'function' ? s : null;
  } catch {
    return null;
  }
}

function safeLocalStorageGet(key: string): string | null {
  try {
    const s = getGlobalStorage();
    if (s) return s.getItem(key);
  } catch {
    // ignore restricted storage
  }
  return memorySalts.get(key) ?? null;
}

function safeLocalStorageSet(key: string, val: string): void {
  memorySalts.set(key, val);
  try {
    const s = getGlobalStorage();
    if (s) s.setItem(key, val);
  } catch {
    // ignore restricted storage
  }
}

export function getOrCreateSandboxSalt(nodeId: string): string {
  const nodeKey = `${NODE_SALT_PREFIX}${nodeId}`;
  const existingNodeSalt = safeLocalStorageGet(nodeKey);
  if (existingNodeSalt && /^[0-9a-f]{32}$/.test(existingNodeSalt)) {
    return existingNodeSalt;
  }
  let deviceSalt = safeLocalStorageGet(DEVICE_SALT_STORAGE_KEY);
  if (!deviceSalt || !/^[0-9a-f]{32}$/.test(deviceSalt)) {
    deviceSalt = randomHex(16);
    safeLocalStorageSet(DEVICE_SALT_STORAGE_KEY, deviceSalt);
  }
  return deviceSalt;
}

/**
 * Rotates the per-node 128-bit sandbox salt so a compromised/corrupted app sandbox
 * immediately moves to a fresh isolated origin without affecting other nodes.
 */
export function resetNodeSandboxSalt(nodeId: string): string {
  const fresh = randomHex(16);
  safeLocalStorageSet(`${NODE_SALT_PREFIX}${nodeId}`, fresh);
  return fresh;
}

async function hmacSha256Hex(keyHex: string, message: string): Promise<string> {
  const sig = await hmacSha256Bytes(textEncoder.encode(keyHex), textEncoder.encode(message));
  return Array.from(sig, (b) => b.toString(16).padStart(2, '0')).join('');
}

/**
 * Computes the isolated sandbox origin for this session.
 * Pass optional `explicitSalt` in tests for deterministic verification.
 */
export async function computeSandboxOrigin(
  cfg: PortalDomainConfig,
  protocol: string,
  nodeId: string,
  targetOrigin: string,
  isCapabilitySession: boolean,
  explicitSalt?: string
): Promise<string> {
  const proto = protocol === 'https:' ? 'https:' : 'http:';
  if (cfg.sandboxMode === 'fixed' && (cfg.sandboxOrigin || cfg.sandboxHost || cfg.sandboxDomain)) {
    if (cfg.sandboxOrigin && !cfg.sandboxOrigin.includes('*.')) {
      return cfg.sandboxOrigin;
    }
    const fixedHost = cfg.sandboxHost || cfg.sandboxDomain;
    return `${proto}//${fixedHost}`;
  }

  let label: string;
  if (isCapabilitySession) {
    // 128-bit ephemeral random label (`sbx-<32hex>`)
    label = `sbx-${randomHex(16)}`;
  } else {
    const salt = explicitSalt || getOrCreateSandboxSalt(nodeId);
    const macHex = await hmacSha256Hex(salt, `${nodeId}\x00${targetOrigin}`);
    label = `sbx-${macHex.slice(0, 24)}`;
  }

  if (cfg.localDevMode) {
    const portSuffix = cfg.localPort ? `:${cfg.localPort}` : '';
    return `${proto}//${label}.localhost${portSuffix}`;
  }
  const base = cfg.sandboxDomain || cfg.portalDomain;
  return `${proto}//${label}.${base}`;
}
