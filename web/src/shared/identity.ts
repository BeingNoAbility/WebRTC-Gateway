/**
 * Agent Ed25519 Identity Key Pinning & Fail-Closed SSH-style IndexedDB TOFU Store
 * - Fail-Closed on IndexedDB I/O or blocked errors (never downgrades to tofu_new on DB failure).
 * - Always closes IDBDatabase handles cleanly after each transaction.
 * - Prevents `?pk=<evil>` from overriding an already-stored mismatched TOFU key.
 * - Separates pre-handshake verification (`commitNew: false`) from post-auth TOFU commit so an
 *   unverified signaling server can never poison TOFU before proving Ed25519 key possession.
 */

import { base64UrlEncode, sha256Bytes } from './crypto';

const PK_HASH_CHARS = 22;
const DB_NAME = 'p2p_gw_identity';
const STORE_NAME = 'tofu_keys';

export async function pkHashFromRawPublicKey(rawPubKey32: Uint8Array): Promise<string> {
  const digest = await sha256Bytes(rawPubKey32);
  return base64UrlEncode(digest).slice(0, PK_HASH_CHARS);
}

export interface KeyVerifyResult {
  ok: boolean;
  status: 'pinned_ok' | 'pinned_mismatch' | 'tofu_new' | 'tofu_ok' | 'tofu_mismatch';
  actualHash: string;
  expectedHash: string;
}

// In-memory fallback used ONLY when `indexedDB` is completely absent (Node.js unit tests).
const memFallback = new Map<string, string>();

function openIDB(): Promise<IDBDatabase | null> {
  if (typeof indexedDB === 'undefined') {
    return Promise.resolve(null);
  }
  return new Promise<IDBDatabase>((resolve, reject) => {
    let settled = false;
    try {
      const req = indexedDB.open(DB_NAME, 1);
      req.onupgradeneeded = () => {
        const db = req.result;
        if (!db.objectStoreNames.contains(STORE_NAME)) {
          db.createObjectStore(STORE_NAME);
        }
      };
      req.onsuccess = () => {
        if (!settled) {
          settled = true;
          resolve(req.result);
        }
      };
      req.onerror = () => {
        if (!settled) {
          settled = true;
          reject(req.error || new Error('IndexedDB open failed'));
        }
      };
      req.onblocked = () => {
        if (!settled) {
          settled = true;
          reject(new Error('IndexedDB TOFU store is blocked by another tab'));
        }
      };
    } catch (err) {
      if (!settled) {
        settled = true;
        reject(err instanceof Error ? err : new Error(String(err)));
      }
    }
  });
}

export async function getTofuKey(scopeKey: string): Promise<string | null> {
  const db = await openIDB();
  if (!db) return memFallback.get(scopeKey) ?? null;
  try {
    return await new Promise<string | null>((resolve, reject) => {
      const tx = db.transaction(STORE_NAME, 'readonly');
      const req = tx.objectStore(STORE_NAME).get(scopeKey);
      req.onsuccess = () => resolve(typeof req.result === 'string' ? req.result : null);
      req.onerror = () => reject(req.error || new Error('IndexedDB get failed'));
      tx.onabort = () => reject(tx.error || new Error('IndexedDB transaction aborted'));
    });
  } finally {
    db.close();
  }
}

export async function setTofuKey(scopeKey: string, pkHash: string): Promise<void> {
  const db = await openIDB();
  if (!db) {
    memFallback.set(scopeKey, pkHash);
    return;
  }
  try {
    await new Promise<void>((resolve, reject) => {
      const tx = db.transaction(STORE_NAME, 'readwrite');
      tx.objectStore(STORE_NAME).put(pkHash, scopeKey);
      tx.oncomplete = () => resolve();
      tx.onerror = () => reject(tx.error || new Error('IndexedDB put failed'));
      tx.onabort = () => reject(tx.error || new Error('IndexedDB put aborted'));
    });
  } finally {
    db.close();
  }
}

export async function resetTofuKey(scopeKey: string): Promise<void> {
  memFallback.delete(scopeKey);
  const db = await openIDB();
  if (!db) return;
  try {
    await new Promise<void>((resolve, reject) => {
      const tx = db.transaction(STORE_NAME, 'readwrite');
      tx.objectStore(STORE_NAME).delete(scopeKey);
      tx.oncomplete = () => resolve();
      tx.onerror = () => reject(tx.error || new Error('IndexedDB delete failed'));
      tx.onabort = () => reject(tx.error || new Error('IndexedDB delete aborted'));
    });
  } finally {
    db.close();
  }
}

export function clearMemoryTofuStoreForTests(): void {
  memFallback.clear();
}

export interface VerifyKeyOptions {
  /**
   * When true (default), writes a brand-new TOFU key to IndexedDB on first sight.
   * Pass `false` for pre-handshake checks (before Ed25519 signature + DTLS auth completes).
   */
  commitNew?: boolean;
}

/**
 * Verifies an Agent's Ed25519 public key against:
 *  1) Explicit pinned `#node?pk=<hash22>` (hard fail on mismatch, never bypassable)
 *  2) SSH-style IndexedDB TOFU (also rejects `?pk=<evil>` if a DIFFERENT TOFU key is already recorded!)
 */
export async function verifyPinnedOrTofuKey(
  portalOrigin: string,
  nodeId: string,
  rawPubKey32: Uint8Array,
  pinnedPkHash: string,
  opts: VerifyKeyOptions = {}
): Promise<KeyVerifyResult> {
  const actualHash = await pkHashFromRawPublicKey(rawPubKey32);
  const pinned = pinnedPkHash.trim();
  const scopeKey = `${portalOrigin}::${nodeId}`;
  const commitNew = opts.commitNew !== false;

  const stored = await getTofuKey(scopeKey);

  if (pinned) {
    if (actualHash !== pinned) {
      return { ok: false, status: 'pinned_mismatch', actualHash, expectedHash: pinned };
    }
    // Defense against `?pk=<evil>` overriding an already-trusted node identity in TOFU!
    if (stored && stored !== actualHash) {
      return { ok: false, status: 'tofu_mismatch', actualHash, expectedHash: stored };
    }
    return { ok: true, status: 'pinned_ok', actualHash, expectedHash: pinned };
  }

  if (!stored) {
    if (commitNew) {
      await setTofuKey(scopeKey, actualHash);
    }
    return { ok: true, status: 'tofu_new', actualHash, expectedHash: actualHash };
  }
  if (stored === actualHash) {
    return { ok: true, status: 'tofu_ok', actualHash, expectedHash: stored };
  }
  return { ok: false, status: 'tofu_mismatch', actualHash, expectedHash: stored };
}

/**
 * Atomically commits a verified TOFU public key hash after Ed25519 Answer & DTLS auth succeed.
 * Runs in a single readwrite IndexedDB transaction and throws Fail-Closed if a different key was
 * concurrently committed by another tab (prevents multi-tab TOCTOU overwrite/bypass).
 */
export async function commitTofuKey(
  portalOrigin: string,
  nodeId: string,
  pkHash: string
): Promise<void> {
  const scopeKey = `${portalOrigin}::${nodeId}`;
  const db = await openIDB();
  if (!db) {
    const existing = memFallback.get(scopeKey);
    if (existing && existing !== pkHash) {
      throw new Error(`TOFU key conflict for ${nodeId}: expected ${existing}, got ${pkHash}`);
    }
    memFallback.set(scopeKey, pkHash);
    return;
  }
  try {
    await new Promise<void>((resolve, reject) => {
      const tx = db.transaction(STORE_NAME, 'readwrite');
      const store = tx.objectStore(STORE_NAME);
      let conflictErr: Error | null = null;
      const getReq = store.get(scopeKey);
      getReq.onsuccess = () => {
        const existing = typeof getReq.result === 'string' ? getReq.result : null;
        if (existing && existing !== pkHash) {
          conflictErr = new Error(`TOFU key conflict for ${nodeId}: expected ${existing}, got ${pkHash}`);
          try {
            tx.abort();
          } catch {
            // ignore
          }
          return;
        }
        if (!existing) {
          store.put(pkHash, scopeKey);
        }
      };
      getReq.onerror = () => reject(getReq.error || new Error('IndexedDB get failed'));
      tx.oncomplete = () => resolve();
      tx.onerror = () => reject(conflictErr || tx.error || new Error('IndexedDB commit failed'));
      tx.onabort = () => reject(conflictErr || tx.error || new Error('IndexedDB commit aborted'));
    });
  } finally {
    db.close();
  }
}
