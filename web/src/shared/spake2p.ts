/**
 * Standard RFC 9383 SPAKE2+ (P-256 / SHA-256 / HKDF-SHA256 / HMAC-SHA256)
 * Zero external dependencies: uses ES2022 BigInt Jacobian P-256 arithmetic + WebCrypto.
 * Byte-for-byte interoperable with Go `pkg/agent/spake2p.go` (V7 KDF & domain tags).
 */

import { appendLenPrefixed, hmacSha256Bytes } from './crypto';

export const SPAKE2P_PBKDF2_ITERS = 300_000;
const PROVER_ID = 'webrtc-gateway-visitor';
const VERIFIER_ID = 'webrtc-gateway-agent';

// NIST P-256 domain parameters
const P = 0xffffffff00000001000000000000000000000000ffffffffffffffffffffffffn;
const N = 0xffffffff00000000ffffffffffffffffbce6faada7179e84f3b9cac2fc632551n;
const B = 0x5ac635d8aa3a93e7b3ebbd55769886bc651d06b0cc53b0f63bce3c3e27d2604bn;
const GX = 0x6b17d1f2e12c4247f8bce6e563a440f277037d812deb33a0f4a13945d898c296n;
const GY = 0x4fe342e2fe1a7f9b8ee7eb4a7c0f9e162bce33576b315ececbb6406837bf51f5n;

// RFC 9383 Section 4 P-256 generator points M and N (uncompressed affine coordinates)
const MX = 0x886e2f97ace46e55ba9dd7242579f2993b64e16ef3dcab95afd497333d8fa12fn;
const MY = 0x5ff355163e43ce224e0b0e65ff02ac8e5c7be09419c785e0ca547d55a12e2d20n;
const NX = 0xd8bbd6c639c62937b04d997f38c3770719c629d7014d49a24b4f98baa1292b49n;
const NY = 0x07d60aa6bfade45008a636337f5168c64d9bd36034808cd564490b1e656edbe7n;

interface JacPoint {
  x: bigint;
  y: bigint;
  z: bigint;
}

const INF: JacPoint = { x: 0n, y: 1n, z: 0n };
const G_PT: JacPoint = { x: GX, y: GY, z: 1n };
const M_PT: JacPoint = { x: MX, y: MY, z: 1n };
const N_PT: JacPoint = { x: NX, y: NY, z: 1n };

function modP(a: bigint): bigint {
  const r = a % P;
  return r >= 0n ? r : r + P;
}

function modN(a: bigint): bigint {
  const r = a % N;
  return r >= 0n ? r : r + N;
}

function modPow(base: bigint, exp: bigint, mod: bigint): bigint {
  let res = 1n;
  let b = ((base % mod) + mod) % mod;
  let e = exp;
  while (e > 0n) {
    if ((e & 1n) === 1n) res = (res * b) % mod;
    b = (b * b) % mod;
    e >>= 1n;
  }
  return res;
}

function modInvP(a: bigint): bigint {
  return modPow(a, P - 2n, P);
}

function pointDouble(p: JacPoint): JacPoint {
  if (p.z === 0n || p.y === 0n) return INF;
  const x1 = p.x;
  const y1 = p.y;
  const z1 = p.z;
  const y1Sq = modP(y1 * y1);
  const s = modP(4n * x1 * y1Sq);
  const z1Sq = modP(z1 * z1);
  const m = modP(3n * modP(x1 - z1Sq) * modP(x1 + z1Sq));
  const x3 = modP(m * m - 2n * s);
  const y3 = modP(m * modP(s - x3) - 8n * modP(y1Sq * y1Sq));
  const z3 = modP(2n * y1 * z1);
  return { x: x3, y: y3, z: z3 };
}

function pointAdd(p: JacPoint, q: JacPoint): JacPoint {
  if (p.z === 0n) return q;
  if (q.z === 0n) return p;
  const z1Sq = modP(p.z * p.z);
  const z2Sq = modP(q.z * q.z);
  const u1 = modP(p.x * z2Sq);
  const u2 = modP(q.x * z1Sq);
  const s1 = modP(p.y * q.z * z2Sq);
  const s2 = modP(q.y * p.z * z1Sq);
  if (u1 === u2) {
    if (s1 !== s2) return INF;
    return pointDouble(p);
  }
  const h = modP(u2 - u1);
  const r = modP(s2 - s1);
  const hSq = modP(h * h);
  const hCu = modP(h * hSq);
  const u1hSq = modP(u1 * hSq);
  const x3 = modP(r * r - hCu - 2n * u1hSq);
  const y3 = modP(r * modP(u1hSq - x3) - s1 * hCu);
  const z3 = modP(h * p.z * q.z);
  return { x: x3, y: y3, z: z3 };
}

function pointNegate(p: JacPoint): JacPoint {
  if (p.z === 0n) return INF;
  return { x: p.x, y: modP(-p.y), z: p.z };
}

function scalarMult(k: bigint, pt: JacPoint): JacPoint {
  const s = modN(k);
  if (s === 0n || pt.z === 0n) return INF;
  let r0 = INF;
  let r1 = pt;
  for (let bit = 255; bit >= 0; bit--) {
    if (((s >> BigInt(bit)) & 1n) === 0n) {
      r1 = pointAdd(r0, r1);
      r0 = pointDouble(r0);
    } else {
      r0 = pointAdd(r0, r1);
      r1 = pointDouble(r1);
    }
  }
  return r0;
}

export function bigintTo32Bytes(v: bigint): Uint8Array {
  const out = new Uint8Array(32);
  let x = v;
  for (let i = 31; i >= 0; i--) {
    out[i] = Number(x & 0xffn);
    x >>= 8n;
  }
  return out;
}

export function bytesToBigint(b: Uint8Array): bigint {
  let out = 0n;
  for (let i = 0; i < b.length; i++) {
    out = (out << 8n) | BigInt(b[i]);
  }
  return out;
}

function encodePoint(pt: JacPoint): Uint8Array {
  if (pt.z === 0n) {
    throw new Error('spake2p: degenerate infinity point');
  }
  const zInv = modInvP(pt.z);
  const zInv2 = modP(zInv * zInv);
  const zInv3 = modP(zInv2 * zInv);
  const ax = modP(pt.x * zInv2);
  const ay = modP(pt.y * zInv3);
  const out = new Uint8Array(65);
  out[0] = 0x04;
  out.set(bigintTo32Bytes(ax), 1);
  out.set(bigintTo32Bytes(ay), 33);
  return out;
}

function decodePoint(b: Uint8Array): JacPoint {
  if (b.length !== 65 || b[0] !== 0x04) {
    throw new Error('spake2p: expected 65-byte uncompressed SEC1 P-256 point');
  }
  const x = bytesToBigint(b.subarray(1, 33));
  const y = bytesToBigint(b.subarray(33, 65));
  if (x >= P || y >= P) {
    throw new Error('spake2p: coordinate out of field');
  }
  const lhs = modP(y * y);
  const rhs = modP(modP(x * x * x) - modP(3n * x) + B);
  if (lhs !== rhs) {
    throw new Error('spake2p: point is not on P-256 curve');
  }
  return { x, y, z: 1n };
}

const M_BYTES = encodePoint(M_PT);
const N_BYTES = encodePoint(N_PT);
const encoder = new TextEncoder();

export interface SPAKE2PProverCreds {
  w0: bigint;
  w1: bigint;
  w0Bytes: Uint8Array;
  w1Bytes: Uint8Array;
}

/**
 * V7 KDF: 1-block PBKDF2-HMAC-SHA256 (300,000 iters, 32 bytes) salted by (nodeId, agentPKB64),
 * followed by HKDF-SHA256("p2pgw-v7:spake2p-w0w1") to 80 bytes -> (w0, w1) mod n.
 */
export async function deriveSPAKE2PProverCreds(
  password: string,
  nodeId: string,
  agentPKB64: string
): Promise<SPAKE2PProverCreds> {
  if (!nodeId || !agentPKB64) {
    throw new Error('spake2p: nodeId and agentPKB64 are required');
  }
  const saltInput = appendLenPrefixed('p2pgw-v7:spake2p-salt:', [nodeId, agentPKB64]);
  const salt = new Uint8Array(await crypto.subtle.digest('SHA-256', saltInput as BufferSource));
  const keyMaterial = await crypto.subtle.importKey(
    'raw',
    encoder.encode(password) as BufferSource,
    { name: 'PBKDF2' },
    false,
    ['deriveBits']
  );
  const seedBuf = await crypto.subtle.deriveBits(
    { name: 'PBKDF2', salt: salt as BufferSource, iterations: SPAKE2P_PBKDF2_ITERS, hash: 'SHA-256' },
    keyMaterial,
    32 * 8
  );
  const okm = await hkdfExpandSha256(new Uint8Array(seedBuf), 'p2pgw-v7:spake2p-w0w1', 80);
  const w0 = modN(bytesToBigint(okm.subarray(0, 40)));
  const w1 = modN(bytesToBigint(okm.subarray(40, 80)));
  return { w0, w1, w0Bytes: bigintTo32Bytes(w0), w1Bytes: bigintTo32Bytes(w1) };
}

export function buildSPAKE2PContext(
  nodeId: string,
  visitorFP: string,
  agentFP: string,
  agentPKB64: string,
  visitorNonce: string,
  agentNonce: string
): Uint8Array {
  return appendLenPrefixed('p2pgw-v7:spake2p-ctx:', [
    nodeId,
    visitorFP,
    agentFP,
    agentPKB64,
    visitorNonce,
    agentNonce,
  ]);
}

function concatLE64Prefixed(parts: Uint8Array[]): Uint8Array {
  let total = 0;
  for (const p of parts) total += 8 + p.length;
  const out = new Uint8Array(total);
  const dv = new DataView(out.buffer);
  let off = 0;
  for (const p of parts) {
    dv.setUint32(off, p.length >>> 0, true);
    dv.setUint32(off + 4, Math.floor(p.length / 0x100000000), true);
    off += 8;
    out.set(p, off);
    off += p.length;
  }
  return out;
}

async function hkdfExpandSha256(ikm: Uint8Array, info: string, lengthBytes: number): Promise<Uint8Array> {
  const baseKey = await crypto.subtle.importKey('raw', ikm as BufferSource, { name: 'HKDF' }, false, ['deriveBits']);
  const bits = await crypto.subtle.deriveBits(
    {
      name: 'HKDF',
      hash: 'SHA-256',
      salt: new Uint8Array(0) as BufferSource,
      info: encoder.encode(info) as BufferSource,
    },
    baseKey,
    lengthBytes * 8
  );
  return new Uint8Array(bits);
}

function randomScalar(): bigint {
  const buf = new Uint8Array(40);
  for (;;) {
    crypto.getRandomValues(buf);
    const s = modN(bytesToBigint(buf));
    if (s !== 0n) return s;
  }
}

export interface SPAKE2PProverResult {
  shareP: Uint8Array;
  confirmP: Uint8Array;
  expectedConfirmV: Uint8Array;
  sharedKey: Uint8Array;
}

export function spake2pComputeShareVForTest(w0: bigint, y: bigint): Uint8Array {
  return encodePoint(pointAdd(scalarMult(y, G_PT), scalarMult(w0, N_PT)));
}

export async function spake2pProverComputeWithScalar(
  creds: Pick<SPAKE2PProverCreds, 'w0' | 'w1' | 'w0Bytes'>,
  x: bigint,
  context: Uint8Array,
  idProver: Uint8Array,
  idVerifier: Uint8Array,
  shareV: Uint8Array
): Promise<SPAKE2PProverResult> {
  const vPt = decodePoint(shareV);
  const shareP = encodePoint(pointAdd(scalarMult(x, G_PT), scalarMult(creds.w0, M_PT)));

  const diff = pointAdd(vPt, pointNegate(scalarMult(creds.w0, N_PT)));
  const zBytes = encodePoint(scalarMult(x, diff));
  const vBytes = encodePoint(scalarMult(creds.w1, diff));

  const tt = concatLE64Prefixed([
    context,
    idProver,
    idVerifier,
    M_BYTES,
    N_BYTES,
    shareP,
    shareV,
    zBytes,
    vBytes,
    creds.w0Bytes,
  ]);
  const kMain = new Uint8Array(await crypto.subtle.digest('SHA-256', tt as BufferSource));
  const confKeys = await hkdfExpandSha256(kMain, 'ConfirmationKeys', 64);
  const kcP = confKeys.subarray(0, 32);
  const kcV = confKeys.subarray(32, 64);
  const sharedKey = await hkdfExpandSha256(kMain, 'SharedKey', 32);

  const confirmP = await hmacSha256Bytes(kcP, shareV);
  const expectedConfirmV = await hmacSha256Bytes(kcV, shareP);
  return { shareP, confirmP, expectedConfirmV, sharedKey };
}

export async function spake2pProverCompute(
  creds: SPAKE2PProverCreds,
  context: Uint8Array,
  shareV: Uint8Array
): Promise<SPAKE2PProverResult> {
  return spake2pProverComputeWithScalar(
    creds,
    randomScalar(),
    context,
    encoder.encode(PROVER_ID),
    encoder.encode(VERIFIER_ID),
    shareV
  );
}

export function constantTimeEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) {
    diff |= a[i] ^ b[i];
  }
  return diff === 0;
}
