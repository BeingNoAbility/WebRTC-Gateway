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

export class UnsupportedCryptoError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'UnsupportedCryptoError';
  }
}

// ---- Zero-Dependency Pure-JS BigInt RFC 8032 Ed25519 Verify Fallback ----
// Automatically activated ONLY when WebCrypto `Ed25519` throws `NotSupportedError`
// (unlocking Chromium 87–136, legacy Electron, and Android WebView while keeping 0 runtime deps).
const ED_P = (1n << 255n) - 19n;
const ED_L = (1n << 252n) + 27742317777372353535851937790883648493n;
const ED_D = 37095705934669439343138083508754565189542113879843219016388785533085940283555n;
const ED_2D = (2n * ED_D) % ED_P;
const ED_SQRT_M1 = 19681161376707505956807079304988542015446066515923890162744021073123829784752n;
const ED_EXP_P5_8 = (ED_P - 5n) >> 3n;
const ED_BX = 15112221349535400772501151409588531511454012693041857206046113283949847762202n;
const ED_BY = 46316835694926478169428394003475163141307993866256225615783033603165251855960n;

type EdExtPoint = [bigint, bigint, bigint, bigint]; // [X, Y, Z, T]
const ED_NEUTRAL: EdExtPoint = [0n, 1n, 1n, 0n];
const ED_BASE: EdExtPoint = [ED_BX, ED_BY, 1n, (ED_BX * ED_BY) % ED_P];

function edMod(a: bigint): bigint {
  const r = a % ED_P;
  return r >= 0n ? r : r + ED_P;
}

function edPowMod(base: bigint, exp: bigint): bigint {
  let res = 1n;
  let b = edMod(base);
  let e = exp;
  while (e > 0n) {
    if ((e & 1n) === 1n) res = (res * b) % ED_P;
    b = (b * b) % ED_P;
    e >>= 1n;
  }
  return res;
}

function edLeToBigInt(bytes: Uint8Array): bigint {
  let out = 0n;
  for (let i = bytes.byteLength - 1; i >= 0; i--) {
    out = (out << 8n) | BigInt(bytes[i]);
  }
  return out;
}

function edPointAdd(p1: EdExtPoint, p2: EdExtPoint): EdExtPoint {
  const [x1, y1, z1, t1] = p1;
  const [x2, y2, z2, t2] = p2;
  const a = edMod((y1 - x1) * (y2 - x2));
  const b = edMod((y1 + x1) * (y2 + x2));
  const c = edMod(t1 * ED_2D * t2);
  const d = edMod(2n * z1 * z2);
  const e = edMod(b - a);
  const f = edMod(d - c);
  const g = edMod(d + c);
  const h = edMod(b + a);
  return [edMod(e * f), edMod(g * h), edMod(f * g), edMod(e * h)];
}

function edScalarMult(point: EdExtPoint, scalar: bigint): EdExtPoint {
  let acc: EdExtPoint = ED_NEUTRAL;
  let cur: EdExtPoint = point;
  let k = scalar;
  while (k > 0n) {
    if ((k & 1n) === 1n) acc = edPointAdd(acc, cur);
    cur = edPointAdd(cur, cur);
    k >>= 1n;
  }
  return acc;
}

function edDecodePoint(bytes32: Uint8Array): EdExtPoint | null {
  const xSign = (bytes32[31] >>> 7) & 1;
  const y = edLeToBigInt(bytes32) & ((1n << 255n) - 1n);
  if (y >= ED_P) return null;
  const y2 = (y * y) % ED_P;
  const u = edMod(y2 - 1n);
  const v = edMod(ED_D * y2 + 1n);
  const v3 = (v * v % ED_P) * v % ED_P;
  const v7 = (v3 * v3 % ED_P) * v % ED_P;
  let x = (u * v3 % ED_P) * edPowMod(u * v7, ED_EXP_P5_8) % ED_P;
  const vx2 = (v * ((x * x) % ED_P)) % ED_P;
  if (vx2 !== u) {
    if (vx2 === edMod(-u)) {
      x = (x * ED_SQRT_M1) % ED_P;
    } else {
      return null;
    }
  }
  if (x === 0n && xSign === 1) return null;
  if ((x & 1n) !== BigInt(xSign)) x = ED_P - x;
  return [x, y, 1n, (x * y) % ED_P];
}

export async function verifyEd25519PureJS(
  rawPubKey32: Uint8Array,
  message: Uint8Array,
  rawSig64: Uint8Array
): Promise<boolean> {
  if (rawPubKey32.byteLength !== 32 || rawSig64.byteLength !== 64) {
    return false;
  }
  const rBytes = rawSig64.subarray(0, 32);
  const sBytes = rawSig64.subarray(32, 64);
  const s = edLeToBigInt(sBytes);
  if (s >= ED_L) return false;

  const A = edDecodePoint(rawPubKey32);
  const R = edDecodePoint(rBytes);
  if (!A || !R) return false;

  const hInput = new Uint8Array(64 + message.byteLength);
  hInput.set(rBytes, 0);
  hInput.set(rawPubKey32, 32);
  hInput.set(message, 64);
  const hDigest = new Uint8Array(await crypto.subtle.digest('SHA-512', hInput as BufferSource));
  const k = edLeToBigInt(hDigest) % ED_L;

  const lhs = edScalarMult(ED_BASE, s);
  const rhs = edPointAdd(R, edScalarMult(A, k));
  return edMod(lhs[0] * rhs[2]) === edMod(rhs[0] * lhs[2]) && edMod(lhs[1] * rhs[2]) === edMod(rhs[1] * lhs[2]);
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
  } catch (err) {
    if (err instanceof Error && err.name === 'NotSupportedError') {
      try {
        return await verifyEd25519PureJS(rawPubKey32, message, rawSig64);
      } catch {
        throw new UnsupportedCryptoError(
          'Browser lacks WebCrypto SHA-512/Ed25519 support (requires a modern Secure Context)'
        );
      }
    }
    return false;
  }
}
