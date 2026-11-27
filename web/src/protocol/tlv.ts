/**
 * P2PMux V7 Compact Binary TLV Header Codec
 * Byte-for-byte mirror of Go pkg/protocol/tlv.go.
 */

import { MAX_TOTAL_HEADERS_SIZE } from './frame';

export const StreamKind = {
  HTTP: 1,
  WS: 2,
} as const;

export type StreamKindValue = (typeof StreamKind)[keyof typeof StreamKind];

export const MAX_HEADER_COUNT = 256;
export const MAX_METHOD_LEN = 16;
export const MAX_REQUEST_COOKIE_MUTATIONS = 32;
export const MAX_RESPONSE_COOKIE_DELTAS = 64;
export const MAX_COOKIE_NAME_LEN = 255;

export const FetchCtxFlag = {
  NONE: 0x00,
  SAME_ORIGIN_CLIENT: 1 << 0,
  TOP_LEVEL_NAV: 1 << 1,
  CLIENT_DECOMPRESS_GZIP: 1 << 2,
  CREDENTIALS_OMIT: 1 << 3,
} as const;

export const FETCH_CTX_VALID_MASK =
  FetchCtxFlag.SAME_ORIGIN_CLIENT |
  FetchCtxFlag.TOP_LEVEL_NAV |
  FetchCtxFlag.CLIENT_DECOMPRESS_GZIP |
  FetchCtxFlag.CREDENTIALS_OMIT;

export const RespFlag = {
  NONE: 0x00,
  DECOMPRESS_GZIP: 1 << 0,
} as const;

export const RESP_FLAGS_VALID_MASK = RespFlag.DECOMPRESS_GZIP;

export const CookieOp = {
  SET: 1,
  DEL: 2,
} as const;

export type CookieOpValue = (typeof CookieOp)[keyof typeof CookieOp];

export interface HeaderField {
  name: string;
  value: string;
}

export interface CookieTLVEntry {
  op: CookieOpValue;
  name: string;
  value: string;
  path: string;
  expiresMs: number;
}

export interface RequestHeaders {
  kind: StreamKindValue;
  fetchCtx?: number;
  method: string;
  target: string;
  headers: HeaderField[];
  cookieMutations?: CookieTLVEntry[];
}

export interface ResponseHeaders {
  status: number;
  respFlags?: number;
  headers: HeaderField[];
  cookieDeltas?: CookieTLVEntry[];
}

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder('utf-8', { fatal: true });

// RFC 9110 §5.6.2 tchar lookup table.
const TCHAR_TABLE = new Uint8Array(256);
for (let c = 0x30; c <= 0x39; c++) TCHAR_TABLE[c] = 1; // 0-9
for (let c = 0x41; c <= 0x5a; c++) TCHAR_TABLE[c] = 1; // A-Z
for (let c = 0x61; c <= 0x7a; c++) TCHAR_TABLE[c] = 1; // a-z
for (const ch of "!#$%&'*+-.^_`|~") TCHAR_TABLE[ch.charCodeAt(0)] = 1;

function isValidRFC9110TokenBytes(bytes: Uint8Array): boolean {
  if (bytes.byteLength === 0) return false;
  for (let i = 0; i < bytes.byteLength; i++) {
    if (TCHAR_TABLE[bytes[i]] === 0) return false;
  }
  return true;
}

function asciiLowerBytes(bytes: Uint8Array): Uint8Array {
  const out = new Uint8Array(bytes.byteLength);
  for (let i = 0; i < bytes.byteLength; i++) {
    const c = bytes[i];
    out[i] = c >= 0x41 && c <= 0x5a ? c + 32 : c;
  }
  return out;
}

function asciiUpperBytes(bytes: Uint8Array): Uint8Array {
  const out = new Uint8Array(bytes.byteLength);
  for (let i = 0; i < bytes.byteLength; i++) {
    const c = bytes[i];
    out[i] = c >= 0x61 && c <= 0x7a ? c - 32 : c;
  }
  return out;
}

function isValidHeaderValueBytes(bytes: Uint8Array): boolean {
  for (let i = 0; i < bytes.byteLength; i++) {
    const c = bytes[i];
    if ((c < 0x20 && c !== 0x09) || c === 0x7f) return false;
  }
  return true;
}

function isValidTargetBytes(bytes: Uint8Array): boolean {
  if (bytes.byteLength === 0 || bytes.byteLength > 0xffff || bytes[0] !== 0x2f /* '/' */) {
    return false;
  }
  for (let i = 0; i < bytes.byteLength; i++) {
    const c = bytes[i];
    if (c <= 0x20 || c === 0x7f || c === 0x5c /* '\\' */) return false;
  }
  return true;
}

interface EncodedCookie {
  op: number;
  name: Uint8Array;
  value: Uint8Array;
  path: Uint8Array;
  expiresMs: bigint;
}

function prepareCookies(entries: CookieTLVEntry[] | undefined, maxCount: number): { list: EncodedCookie[]; byteSize: number } {
  const src = entries ?? [];
  if (src.length > maxCount) {
    throw new Error(`too many cookie records (${src.length} > ${maxCount})`);
  }
  const list: EncodedCookie[] = new Array(src.length);
  let byteSize = 2;
  for (let i = 0; i < src.length; i++) {
    const c = src[i];
    if (c.op !== CookieOp.SET && c.op !== CookieOp.DEL) {
      throw new Error(`invalid cookie op: ${c.op}`);
    }
    const name = textEncoder.encode(c.name);
    const value = textEncoder.encode(c.value ?? '');
    const path = textEncoder.encode(c.path ?? '');
    if (
      name.byteLength === 0 ||
      name.byteLength > MAX_COOKIE_NAME_LEN ||
      !isValidHeaderValueBytes(name) ||
      value.byteLength > 0xffff ||
      !isValidHeaderValueBytes(value) ||
      path.byteLength > 0xffff ||
      !isValidHeaderValueBytes(path)
    ) {
      throw new Error('invalid cookie TLV entry');
    }
    if (
      typeof c.expiresMs !== 'number' ||
      !Number.isFinite(c.expiresMs) ||
      !Number.isInteger(c.expiresMs) ||
      c.expiresMs < 0 ||
      c.expiresMs > Number.MAX_SAFE_INTEGER
    ) {
      throw new RangeError(`invalid cookie expiresMs: ${c.expiresMs}`);
    }
    list[i] = { op: c.op, name, value, path, expiresMs: BigInt(c.expiresMs) };
    byteSize += 1 + 1 + name.byteLength + 2 + value.byteLength + 2 + path.byteLength + 8;
  }
  return { list, byteSize };
}

function writeCookies(buf: Uint8Array, view: DataView, startPos: number, list: EncodedCookie[]): number {
  let pos = startPos;
  view.setUint16(pos, list.length, false);
  pos += 2;
  for (const c of list) {
    buf[pos++] = c.op;
    buf[pos++] = c.name.byteLength;
    buf.set(c.name, pos);
    pos += c.name.byteLength;
    view.setUint16(pos, c.value.byteLength, false);
    pos += 2;
    buf.set(c.value, pos);
    pos += c.value.byteLength;
    view.setUint16(pos, c.path.byteLength, false);
    pos += 2;
    buf.set(c.path, pos);
    pos += c.path.byteLength;
    view.setBigInt64(pos, c.expiresMs, false);
    pos += 8;
  }
  return pos;
}

function decodeCookies(
  raw: Uint8Array,
  view: DataView,
  startPos: number,
  maxCount: number
): { cookies: CookieTLVEntry[]; nextPos: number } {
  let pos = startPos;
  if (pos + 2 > raw.byteLength) throw new Error('truncated cookie count');
  const count = view.getUint16(pos, false);
  pos += 2;
  if (count > maxCount) throw new Error('cookie count exceeds limit');
  const cookies: CookieTLVEntry[] = new Array(count);
  for (let i = 0; i < count; i++) {
    if (pos + 2 > raw.byteLength) throw new Error('truncated cookie entry');
    const op = raw[pos++];
    if (op !== CookieOp.SET && op !== CookieOp.DEL) throw new Error(`invalid cookie op: ${op}`);
    const nLen = raw[pos++];
    if (nLen === 0 || pos + nLen + 2 > raw.byteLength) throw new Error('truncated cookie name');
    const nBytes = raw.subarray(pos, pos + nLen);
    if (!isValidHeaderValueBytes(nBytes)) throw new Error('invalid cookie name');
    const name = textDecoder.decode(nBytes);
    pos += nLen;

    const vLen = view.getUint16(pos, false);
    pos += 2;
    if (pos + vLen + 2 > raw.byteLength) throw new Error('truncated cookie value');
    const vBytes = raw.subarray(pos, pos + vLen);
    if (!isValidHeaderValueBytes(vBytes)) throw new Error('invalid cookie value');
    const value = textDecoder.decode(vBytes);
    pos += vLen;

    const pLen = view.getUint16(pos, false);
    pos += 2;
    if (pos + pLen + 8 > raw.byteLength) throw new Error('truncated cookie path');
    const pBytes = raw.subarray(pos, pos + pLen);
    if (!isValidHeaderValueBytes(pBytes)) throw new Error('invalid cookie path');
    const path = textDecoder.decode(pBytes);
    pos += pLen;

    const rawExp = view.getBigInt64(pos, false);
    if (rawExp < 0n || rawExp > BigInt(Number.MAX_SAFE_INTEGER)) {
      throw new RangeError(`invalid cookie expiresMs: ${rawExp}`);
    }
    const expiresMs = Number(rawExp);
    pos += 8;
    cookies[i] = { op: op as CookieOpValue, name, value, path, expiresMs };
  }
  return { cookies, nextPos: pos };
}

export function encodeRequestHeaders(req: RequestHeaders): Uint8Array {
  if (req.kind !== StreamKind.HTTP && req.kind !== StreamKind.WS) {
    throw new Error(`invalid stream kind: ${req.kind}`);
  }
  const rawFetchCtx = req.fetchCtx ?? 0;
  if (!Number.isInteger(rawFetchCtx) || rawFetchCtx < 0 || rawFetchCtx > 0xff || (rawFetchCtx & ~FETCH_CTX_VALID_MASK) !== 0) {
    throw new Error(`invalid fetchCtx flags: ${rawFetchCtx}`);
  }
  const fetchCtx = rawFetchCtx;
  // Validate strict ASCII RFC 9110 tchar BEFORE uppercasing.
  const rawMethodBytes = textEncoder.encode(req.method);
  if (rawMethodBytes.byteLength === 0 || rawMethodBytes.byteLength > MAX_METHOD_LEN || !isValidRFC9110TokenBytes(rawMethodBytes)) {
    throw new Error(`invalid HTTP method: ${req.method}`);
  }
  const methodBytes = asciiUpperBytes(rawMethodBytes);
  const targetBytes = textEncoder.encode(req.target);
  if (!isValidTargetBytes(targetBytes)) {
    throw new Error('invalid request target');
  }
  if (req.headers.length > MAX_HEADER_COUNT) {
    throw new Error('too many headers');
  }

  const preparedCookies = prepareCookies(req.cookieMutations, MAX_REQUEST_COOKIE_MUTATIONS);
  let total = 1 + 1 + 1 + methodBytes.byteLength + 2 + targetBytes.byteLength + 2 + preparedCookies.byteSize;
  const encodedPairs: Array<[Uint8Array, Uint8Array]> = new Array(req.headers.length);
  for (let i = 0; i < req.headers.length; i++) {
    const rawK = textEncoder.encode(req.headers[i].name);
    const v = textEncoder.encode(req.headers[i].value);
    if (
      rawK.byteLength === 0 ||
      rawK.byteLength > 0xffff ||
      !isValidRFC9110TokenBytes(rawK) ||
      v.byteLength > 0xffff ||
      !isValidHeaderValueBytes(v)
    ) {
      throw new Error(`invalid header field: ${req.headers[i].name}`);
    }
    const k = asciiLowerBytes(rawK);
    encodedPairs[i] = [k, v];
    total += 4 + k.byteLength + v.byteLength;
    if (total > MAX_TOTAL_HEADERS_SIZE) {
      throw new RangeError('encoded request headers exceed 64 KiB');
    }
  }
  if (total > MAX_TOTAL_HEADERS_SIZE) {
    throw new RangeError('encoded request headers exceed 64 KiB');
  }

  const buf = new Uint8Array(total);
  const view = new DataView(buf.buffer);
  let pos = 0;
  buf[pos++] = req.kind;
  buf[pos++] = fetchCtx;
  buf[pos++] = methodBytes.byteLength;
  buf.set(methodBytes, pos);
  pos += methodBytes.byteLength;
  view.setUint16(pos, targetBytes.byteLength, false);
  pos += 2;
  buf.set(targetBytes, pos);
  pos += targetBytes.byteLength;
  view.setUint16(pos, encodedPairs.length, false);
  pos += 2;

  for (const [k, v] of encodedPairs) {
    view.setUint16(pos, k.byteLength, false);
    pos += 2;
    buf.set(k, pos);
    pos += k.byteLength;
    view.setUint16(pos, v.byteLength, false);
    pos += 2;
    buf.set(v, pos);
    pos += v.byteLength;
  }
  writeCookies(buf, view, pos, preparedCookies.list);
  return buf;
}

export function decodeRequestHeaders(raw: Uint8Array): RequestHeaders {
  if (raw.byteLength > MAX_TOTAL_HEADERS_SIZE) {
    throw new RangeError('request headers exceed 64 KiB');
  }
  if (raw.byteLength < 3) {
    throw new Error('truncated request headers');
  }
  const view = new DataView(raw.buffer, raw.byteOffset, raw.byteLength);
  let pos = 0;
  const kind = raw[pos++];
  if (kind !== StreamKind.HTTP && kind !== StreamKind.WS) {
    throw new Error(`invalid stream kind: ${kind}`);
  }
  const fetchCtx = raw[pos++];
  if ((fetchCtx & ~FETCH_CTX_VALID_MASK) !== 0) {
    throw new Error(`invalid fetchCtx flags: 0x${fetchCtx.toString(16)}`);
  }
  const mLen = raw[pos++];
  if (mLen === 0 || mLen > MAX_METHOD_LEN || pos + mLen + 6 > raw.byteLength) {
    throw new Error('invalid method length');
  }
  const mBytes = raw.subarray(pos, pos + mLen);
  if (!isValidRFC9110TokenBytes(mBytes)) {
    throw new Error('invalid method characters');
  }
  const method = textDecoder.decode(asciiUpperBytes(mBytes));
  pos += mLen;

  const tLen = view.getUint16(pos, false);
  pos += 2;
  if (tLen === 0 || pos + tLen + 4 > raw.byteLength) {
    throw new Error('truncated target');
  }
  const tBytes = raw.subarray(pos, pos + tLen);
  if (!isValidTargetBytes(tBytes)) {
    throw new Error('invalid target characters');
  }
  const target = textDecoder.decode(tBytes);
  pos += tLen;

  const { headers, nextPos } = decodeHeaderFields(raw, view, pos);
  const { cookies, nextPos: finalPos } = decodeCookies(raw, view, nextPos, MAX_REQUEST_COOKIE_MUTATIONS);
  if (finalPos !== raw.byteLength) {
    throw new Error('unexpected trailing bytes in request headers');
  }
  return {
    kind: kind as StreamKindValue,
    fetchCtx,
    method,
    target,
    headers,
    cookieMutations: cookies,
  };
}

export function encodeResponseHeaders(resp: ResponseHeaders): Uint8Array {
  if (!Number.isInteger(resp.status) || resp.status < 100 || resp.status > 599) {
    throw new RangeError(`invalid HTTP status: ${resp.status}`);
  }
  const rawRespFlags = resp.respFlags ?? 0;
  if (!Number.isInteger(rawRespFlags) || rawRespFlags < 0 || rawRespFlags > 0xff || (rawRespFlags & ~RESP_FLAGS_VALID_MASK) !== 0) {
    throw new Error(`invalid respFlags: ${rawRespFlags}`);
  }
  const respFlags = rawRespFlags;
  if (resp.headers.length > MAX_HEADER_COUNT) {
    throw new Error('too many headers');
  }

  const preparedCookies = prepareCookies(resp.cookieDeltas, MAX_RESPONSE_COOKIE_DELTAS);
  let total = 2 + 1 + 2 + preparedCookies.byteSize;
  const encodedPairs: Array<[Uint8Array, Uint8Array]> = new Array(resp.headers.length);
  for (let i = 0; i < resp.headers.length; i++) {
    const rawK = textEncoder.encode(resp.headers[i].name);
    const v = textEncoder.encode(resp.headers[i].value);
    if (
      rawK.byteLength === 0 ||
      rawK.byteLength > 0xffff ||
      !isValidRFC9110TokenBytes(rawK) ||
      v.byteLength > 0xffff ||
      !isValidHeaderValueBytes(v)
    ) {
      throw new Error(`invalid header field: ${resp.headers[i].name}`);
    }
    const k = asciiLowerBytes(rawK);
    encodedPairs[i] = [k, v];
    total += 4 + k.byteLength + v.byteLength;
    if (total > MAX_TOTAL_HEADERS_SIZE) {
      throw new RangeError('encoded response headers exceed 64 KiB');
    }
  }
  if (total > MAX_TOTAL_HEADERS_SIZE) {
    throw new RangeError('encoded response headers exceed 64 KiB');
  }

  const buf = new Uint8Array(total);
  const view = new DataView(buf.buffer);
  let pos = 0;
  view.setUint16(pos, resp.status, false);
  pos += 2;
  buf[pos++] = respFlags;
  view.setUint16(pos, encodedPairs.length, false);
  pos += 2;

  for (const [k, v] of encodedPairs) {
    view.setUint16(pos, k.byteLength, false);
    pos += 2;
    buf.set(k, pos);
    pos += k.byteLength;
    view.setUint16(pos, v.byteLength, false);
    pos += 2;
    buf.set(v, pos);
    pos += v.byteLength;
  }
  writeCookies(buf, view, pos, preparedCookies.list);
  return buf;
}

export function decodeResponseHeaders(raw: Uint8Array): ResponseHeaders {
  if (raw.byteLength > MAX_TOTAL_HEADERS_SIZE) {
    throw new RangeError('response headers exceed 64 KiB');
  }
  if (raw.byteLength < 7) {
    throw new Error('truncated response headers');
  }
  const view = new DataView(raw.buffer, raw.byteOffset, raw.byteLength);
  const status = view.getUint16(0, false);
  if (status < 100 || status > 599) {
    throw new RangeError(`invalid HTTP status: ${status}`);
  }
  const respFlags = raw[2];
  if ((respFlags & ~RESP_FLAGS_VALID_MASK) !== 0) {
    throw new Error(`invalid respFlags: 0x${respFlags.toString(16)}`);
  }
  const { headers, nextPos } = decodeHeaderFields(raw, view, 3);
  const { cookies, nextPos: finalPos } = decodeCookies(raw, view, nextPos, MAX_RESPONSE_COOKIE_DELTAS);
  if (finalPos !== raw.byteLength) {
    throw new Error('unexpected trailing bytes in response headers');
  }
  return { status, respFlags, headers, cookieDeltas: cookies };
}

function decodeHeaderFields(
  raw: Uint8Array,
  view: DataView,
  startPos: number
): { headers: HeaderField[]; nextPos: number } {
  let pos = startPos;
  if (pos + 2 > raw.byteLength) {
    throw new Error('truncated header count');
  }
  const count = view.getUint16(pos, false);
  pos += 2;
  if (count > MAX_HEADER_COUNT) {
    throw new Error('header count exceeds limit');
  }
  const headers: HeaderField[] = new Array(count);
  for (let i = 0; i < count; i++) {
    if (pos + 2 > raw.byteLength) throw new Error('truncated header name length');
    const kLen = view.getUint16(pos, false);
    pos += 2;
    if (kLen === 0 || pos + kLen + 2 > raw.byteLength) throw new Error('truncated header name');
    const kBytes = raw.subarray(pos, pos + kLen);
    if (!isValidRFC9110TokenBytes(kBytes)) throw new Error('invalid header name');
    const name = textDecoder.decode(asciiLowerBytes(kBytes));
    pos += kLen;

    const vLen = view.getUint16(pos, false);
    pos += 2;
    if (pos + vLen > raw.byteLength) throw new Error('truncated header value');
    const vBytes = raw.subarray(pos, pos + vLen);
    if (!isValidHeaderValueBytes(vBytes)) throw new Error('invalid header value');
    const value = textDecoder.decode(vBytes);
    pos += vLen;
    headers[i] = { name, value };
  }
  return { headers, nextPos: pos };
}
