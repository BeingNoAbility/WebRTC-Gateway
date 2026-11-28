/**
 * Shared Bounded In-Memory Non-HttpOnly Cookie Store
 * Reused by BOTH Service Worker (`CookieMirror`) and page runtime (`ClientCookieMirror`)
 * so path matching, expiration pruning, eviction caps, and serialization share one single source of truth.
 */

import { isCookieDelta, type CookieDelta } from './ipc';

export const MAX_MIRROR_COOKIES = 256;

export interface MirrorCookieEntry {
  name: string;
  value: string;
  path: string;
  expires: number; // unix ms, 0 = session cookie
}

export function normalizeCookiePath(rawPath?: string): string {
  if (!rawPath || !rawPath.startsWith('/')) return '/';
  return rawPath;
}

export function cookieEntryKey(path: string, name: string): string {
  return `${path}\0${name}`;
}

export function cookiePathMatches(reqPath: string, cookiePath: string): boolean {
  const p = reqPath || '/';
  if (p === cookiePath) return true;
  if (!p.startsWith(cookiePath)) return false;
  if (cookiePath.endsWith('/')) return true;
  return p.charCodeAt(cookiePath.length) === 0x2f; // '/'
}

export class CookieMirrorStore {
  protected readonly entries = new Map<string, MirrorCookieEntry>();

  clear(): void {
    this.entries.clear();
  }

  setOrDelete(
    op: 'set' | 'del',
    name: string,
    value: string | undefined,
    rawPath: string | undefined,
    expires: number | undefined,
    now = Date.now()
  ): { key: string; path: string; deleted: boolean; exp: number; val: string } {
    const path = normalizeCookiePath(rawPath);
    const key = cookieEntryKey(path, name);
    const exp = expires && expires > 0 ? expires : 0;
    const deleted = op === 'del' || (exp > 0 && exp <= now);
    if (deleted) {
      this.entries.delete(key);
      return { key, path, deleted: true, exp: 0, val: '' };
    }
    if (!this.entries.has(key) && this.entries.size >= MAX_MIRROR_COOKIES) {
      const oldestKey = this.entries.keys().next().value;
      if (oldestKey) this.entries.delete(oldestKey);
    }
    const val = value ?? '';
    this.entries.set(key, { name, value: val, path, expires: exp });
    return { key, path, deleted: false, exp, val };
  }

  applyDeltas(deltas: CookieDelta[], now = Date.now()): void {
    for (const d of deltas) {
      if (!isCookieDelta(d)) continue;
      this.setOrDelete(d.op, d.name, d.value, d.path, d.expires, now);
    }
  }

  snapshot(now = Date.now()): CookieDelta[] {
    const out: CookieDelta[] = [];
    for (const [k, e] of this.entries) {
      if (e.expires > 0 && e.expires <= now) {
        this.entries.delete(k);
        continue;
      }
      out.push({
        op: 'set',
        name: e.name,
        value: e.value,
        path: e.path,
        expires: e.expires || undefined,
      });
    }
    return out;
  }

  serializeForPath(reqPath: string, now = Date.now()): string {
    const matched: MirrorCookieEntry[] = [];
    for (const [k, e] of this.entries) {
      if (e.expires > 0 && e.expires <= now) {
        this.entries.delete(k);
        continue;
      }
      if (cookiePathMatches(reqPath, e.path)) {
        matched.push(e);
      }
    }
    matched.sort((a, b) => b.path.length - a.path.length);
    return matched.map((c) => `${c.name}=${c.value}`).join('; ');
  }
}
