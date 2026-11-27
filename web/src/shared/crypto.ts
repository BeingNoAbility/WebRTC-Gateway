/**
 * V7 WebCrypto Primitives: SHA-256, HMAC-SHA256, Ed25519 Verify, Base64url,
 * and Length-Prefixed Transcript Framing (mirroring Go pkg/signaling/dto.go).
 */

const textEncoder = new TextEncoder();

export function base64UrlEncode(bytes: Uint8Array): string {
  let bin = '';
  for (let i = 0; i < bytes.byteLength; i++) {
    bin += String.fromCharCode(bytes[i]);
  }
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/g, '');
}

export function base64UrlDecode(str: string): Uint8Array {
  if (!str || str.length % 4 === 1 || !/^[A-Za-z0-9_-]+$/.test(str)) {
    throw new Error('invalid base64url string');
  }
  const pad = str.length % 4 === 0 ? '' : '='.repeat(4 - (str.length % 4));
  const b64 = str.replace(/-/g, '+').replace(/_/g, '/') + pad;
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) {
    out[i] = bin.charCodeAt(i);
  }
  // Enforce canonical zero trailing bits (matches Go base64.RawURLEncoding.Strict()).
  if (base64UrlEncode(out) !== str) {
    throw new Error('non-canonical base64url string');
  }
  return out;
}

export function randomBase64Url(numBytes = 32): string {
  const bytes = new Uint8Array(numBytes);
  crypto.getRandomValues(bytes);
  return base64UrlEncode(bytes);
}

export function randomHex(numBytes = 12): string {
  const bytes = new Uint8Array(numBytes);
  crypto.getRandomValues(bytes);
  let hex = '';
  for (let i = 0; i < bytes.byteLength; i++) {
    hex += bytes[i].toString(16).padStart(2, '0');
  }
  return hex;
}

/**
 * Appends uint16-BE length-prefixed UTF-8 strings after prefix (matches Go signaling.AppendLenPrefixed).
 */
export function appendLenPrefixed(prefix: string, fields: string[]): Uint8Array {
  const prefixBytes = textEncoder.encode(prefix);
  const encodedFields = fields.map((f) => textEncoder.encode(f));
  let total = prefixBytes.length;
  for (const b of encodedFields) {
    if (b.length > 0xffff) throw new Error('Transcript field exceeds uint16 length');
    total += 2 + b.length;
  }
  const out = new Uint8Array(total);
  out.set(prefixBytes, 0);
  let off = prefixBytes.length;
  for (const b of encodedFields) {
    out[off] = (b.length >>> 8) & 0xff;
    out[off + 1] = b.length & 0xff;
    off += 2;
    out.set(b, off);
    off += b.length;
  }
  return out;
}

export async function sha256Bytes(data: Uint8Array): Promise<Uint8Array> {
  const digest = await crypto.subtle.digest('SHA-256', data as BufferSource);
  return new Uint8Array(digest);
}

export async function sha256Hex(input: string | Uint8Array): Promise<string> {
  const raw = typeof input === 'string' ? textEncoder.encode(input) : input;
  const bytes = await sha256Bytes(raw);
  let hex = '';
  for (let i = 0; i < bytes.byteLength; i++) {
    hex += bytes[i].toString(16).padStart(2, '0');
  }
  return hex;
}

export async function hmacSha256Bytes(keyBytes: Uint8Array, msgBytes: Uint8Array): Promise<Uint8Array> {
  if (keyBytes.byteLength === 0) {
    throw new Error('HMAC key must not be empty');
  }
  const key = await crypto.subtle.importKey(
    'raw',
    keyBytes as BufferSource,
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign']
  );
  const sig = await crypto.subtle.sign('HMAC', key, msgBytes as BufferSource);
  return new Uint8Array(sig);
}

export async function verifyEd25519(
  rawPubKey32: Uint8Array,
  message: Uint8Array,
  rawSig64: Uint8Array
): Promise<boolean> {
  if (rawPubKey32.byteLength !== 32 || rawSig64.byteLength !== 64) {
    return false;
  }
  try {
    const key = await crypto.subtle.importKey(
      'raw',
      rawPubKey32 as BufferSource,
      { name: 'Ed25519' },
      false,
      ['verify']
    );
    return await crypto.subtle.verify(
      { name: 'Ed25519' },
      key,
      rawSig64 as BufferSource,
      message as BufferSource
    );
  } catch {
    return false;
  }
}
