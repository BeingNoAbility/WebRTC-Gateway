/**
 * P2PMux V7 Binary Frame Codec
 * Byte-for-byte mirror of Go pkg/protocol/frame.go.
 *
 * Fixed 8-byte header (Big-Endian):
 *   [0:4] Gen    (uint32 BE: 0 on p2pctrl, monotonic >=1 per HTTP pool DC, strictly 1 on p2pws)
 *   [4]   Type   (uint8)
 *   [5]   Flags  (uint8)
 *   [6:8] Length (uint16 BE)
 */

export const HEADER_SIZE = 8;
export const MAX_PAYLOAD_SIZE = 65535;
export const DEFAULT_CHUNK_SIZE = 32760;
export const MIN_CHUNK_SIZE = 1024;
export const MIN_NEGOTIATED_MAX_MSG_SIZE = 4096;
export const MAX_TOTAL_HEADERS_SIZE = 64 * 1024;
export const MAX_CONTINUATION_FRAMES = 16;
export const MAX_CONTROL_JSON_SIZE = 32 * 1024;
export const MAX_WS_MESSAGE_BYTES = 16 * 1024 * 1024;
export const MAX_WS_CLOSE_PAYLOAD_BYTES = 125;

export const CONTROL_CHANNEL_ID = 0;
export const CONTROL_CHANNEL_LABEL = 'p2pctrl';
export const DEFAULT_POOL_SIZE = 32;
export const WS_CHANNEL_LABEL = 'p2pws';
export const DEFAULT_MAX_WS_CONNS = 64;

export const FrameType = {
  // Control-plane (p2pctrl, id=0, Gen === 0)
  SETTINGS: 0x01,
  AUTH_CHALLENGE: 0x02,
  AUTH_RESPONSE: 0x03,
  AUTH_RESULT: 0x04,
  CAP_REQ: 0x05,
  CAP_RESP: 0x06,
  // 0x07 reserved
  PING: 0x08,
  PONG: 0x09,
  GOAWAY: 0x0a,

  // Data/stream frames (in-band on p2p_01..32 & p2pws, Gen >= 1)
  HEADERS: 0x10,
  CONTINUATION: 0x11,
  DATA: 0x12,
  CREDIT: 0x13,
  RESET: 0x14,

  // Streaming WebSocket sub-frames (exclusively on "p2pws" DCs, Gen === 1)
  WS_TEXT: 0x21,
  WS_BINARY: 0x22,
  WS_CONT: 0x23,
  WS_CLOSE: 0x24,
} as const;

export const FrameFlag = {
  NONE: 0x00,
  FIN: 0x01,
  MSG_FIN: 0x02,
  MORE_HEADERS: 0x04,
} as const;

export const ErrorCode = {
  NO_ERROR: 0,
  PROTOCOL_ERROR: 1,
  INTERNAL_ERROR: 2,
  FLOW_CONTROL_ERROR: 3,
  REFUSED_STREAM: 4,
  CANCEL: 5,
  FRAME_SIZE_ERROR: 6,
  AUTH_REQUIRED: 7,
  FORBIDDEN: 8,
  GOING_AWAY: 9,
} as const;

export interface Frame {
  gen: number;
  type: number;
  flags: number;
  payload: Uint8Array;
}

export function poolChannelId(slotIdx: number): number {
  return 1 + slotIdx;
}

export function poolChannelLabel(slotIdx: number): string {
  const n = slotIdx + 1;
  return `p2p_${n < 10 ? '0' : ''}${n}`;
}

export function isControlFrameType(type: number): boolean {
  return (
    (type >= FrameType.SETTINGS && type <= FrameType.CAP_RESP) ||
    (type >= FrameType.PING && type <= FrameType.GOAWAY)
  );
}

export function isPoolDataFrameType(type: number): boolean {
  return type >= FrameType.HEADERS && type <= FrameType.RESET;
}

export function isWSDataFrameType(type: number): boolean {
  return (
    (type >= FrameType.HEADERS && type <= FrameType.RESET && type !== FrameType.DATA) ||
    (type >= FrameType.WS_TEXT && type <= FrameType.WS_CLOSE)
  );
}

export function validateFrameMeta(gen: number, type: number, flags: number, payloadLen: number): void {
  if (!Number.isInteger(gen) || gen < 0 || gen > 0xffffffff) {
    throw new RangeError(`invalid stream generation: ${gen}`);
  }
  if (!Number.isInteger(payloadLen) || payloadLen < 0 || payloadLen > MAX_PAYLOAD_SIZE) {
    throw new RangeError(`payload exceeds 65535 bytes: ${payloadLen}`);
  }
  switch (type) {
    case FrameType.SETTINGS:
    case FrameType.AUTH_CHALLENGE:
    case FrameType.AUTH_RESPONSE:
    case FrameType.AUTH_RESULT:
    case FrameType.CAP_REQ:
    case FrameType.CAP_RESP:
      if (gen !== 0) throw new Error('control JSON frame requires gen === 0');
      if (flags !== FrameFlag.NONE) throw new Error('control JSON frame forbids flags');
      if (payloadLen > MAX_CONTROL_JSON_SIZE) throw new RangeError('control JSON frame exceeds 32 KiB');
      break;
    case FrameType.PING:
    case FrameType.PONG:
      if (gen !== 0 || flags !== FrameFlag.NONE || payloadLen !== 8) {
        throw new Error('PING/PONG requires gen === 0, flags === 0, len === 8');
      }
      break;
    case FrameType.GOAWAY:
      if (gen !== 0 || flags !== FrameFlag.NONE || payloadLen !== 4) {
        throw new Error('GOAWAY requires gen === 0, flags === 0, len === 4');
      }
      break;
    case FrameType.CREDIT:
    case FrameType.RESET:
      if (gen === 0 || flags !== FrameFlag.NONE || payloadLen !== 4) {
        throw new Error('CREDIT/RESET requires gen > 0, flags === 0, len === 4');
      }
      break;
    case FrameType.HEADERS:
      if (gen === 0) throw new Error('HEADERS requires gen > 0');
      if ((flags & ~(FrameFlag.FIN | FrameFlag.MORE_HEADERS)) !== 0) {
        throw new Error('invalid flags on HEADERS');
      }
      break;
    case FrameType.CONTINUATION:
      if (gen === 0) throw new Error('CONTINUATION requires gen > 0');
      if ((flags & ~FrameFlag.MORE_HEADERS) !== 0) {
        throw new Error('invalid flags on CONTINUATION');
      }
      if (payloadLen === 0 && (flags & FrameFlag.MORE_HEADERS) !== 0) {
        throw new Error('non-final 0-byte CONTINUATION is forbidden');
      }
      break;
    case FrameType.DATA:
      if (gen === 0) throw new Error('DATA requires gen > 0');
      if ((flags & ~FrameFlag.FIN) !== 0) {
        throw new Error('invalid flags on DATA');
      }
      break;
    case FrameType.WS_TEXT:
    case FrameType.WS_BINARY:
      if (gen === 0) throw new Error('WS frame requires gen > 0');
      if ((flags & ~FrameFlag.MSG_FIN) !== 0) {
        throw new Error('invalid flags on WS frame');
      }
      break;
    case FrameType.WS_CONT:
      if (gen === 0) throw new Error('WS_CONT requires gen > 0');
      if ((flags & ~FrameFlag.MSG_FIN) !== 0) {
        throw new Error('invalid flags on WS_CONT');
      }
      if (payloadLen === 0 && (flags & FrameFlag.MSG_FIN) === 0) {
        throw new Error('non-final 0-byte WS_CONT is forbidden');
      }
      break;
    case FrameType.WS_CLOSE:
      if (gen === 0 || flags !== FrameFlag.NONE || payloadLen < 2 || payloadLen > MAX_WS_CLOSE_PAYLOAD_BYTES) {
        throw new Error('WS_CLOSE requires gen > 0, flags === 0, len in [2, 125]');
      }
      break;
    default:
      throw new Error(`unknown frame type: 0x${type.toString(16)}`);
  }
}

export function encodeFrame(gen: number, type: number, flags: number, payload: Uint8Array = new Uint8Array(0)): Uint8Array {
  validateFrameMeta(gen, type, flags, payload.byteLength);
  if (type === FrameType.CREDIT) {
    decodeCreditPayload(payload);
  }
  const out = new Uint8Array(HEADER_SIZE + payload.byteLength);
  const view = new DataView(out.buffer);
  view.setUint32(0, gen >>> 0, false);
  out[4] = type & 0xff;
  out[5] = flags & 0xff;
  view.setUint16(6, payload.byteLength, false);
  out.set(payload, HEADER_SIZE);
  return out;
}

export function decodeFrame(raw: Uint8Array): Frame {
  if (raw.byteLength < HEADER_SIZE) {
    throw new Error('frame shorter than 8-byte header');
  }
  const view = new DataView(raw.buffer, raw.byteOffset, raw.byteLength);
  const gen = view.getUint32(0, false);
  const type = raw[4];
  const flags = raw[5];
  const declaredLen = view.getUint16(6, false);
  if (raw.byteLength !== HEADER_SIZE + declaredLen) {
    throw new Error(`frame length mismatch: declared=${declaredLen} actual=${raw.byteLength - HEADER_SIZE}`);
  }
  validateFrameMeta(gen, type, flags, declaredLen);
  const payload = raw.subarray(HEADER_SIZE);
  if (type === FrameType.CREDIT) {
    decodeCreditPayload(payload);
  }
  return { gen, type, flags, payload };
}

export function encodeCreditPayload(delta: number): Uint8Array {
  if (!Number.isInteger(delta) || delta < 1 || delta > 0x7fffffff) {
    throw new RangeError(`invalid credit delta: ${delta}`);
  }
  const out = new Uint8Array(4);
  new DataView(out.buffer).setUint32(0, delta, false);
  return out;
}

export function decodeCreditPayload(payload: Uint8Array): number {
  if (payload.byteLength !== 4) {
    throw new Error('CREDIT payload must be 4 bytes');
  }
  const delta = new DataView(payload.buffer, payload.byteOffset, 4).getUint32(0, false);
  if (delta < 1 || delta > 0x7fffffff) {
    throw new RangeError(`invalid credit delta: ${delta}`);
  }
  return delta;
}

export function encodeResetPayload(code: number): Uint8Array {
  const out = new Uint8Array(4);
  new DataView(out.buffer).setUint32(0, code >>> 0, false);
  return out;
}

export function decodeResetPayload(payload: Uint8Array): number {
  if (payload.byteLength !== 4) throw new Error('RESET payload must be 4 bytes');
  return new DataView(payload.buffer, payload.byteOffset, 4).getUint32(0, false);
}

export function encodeGoAwayPayload(code: number): Uint8Array {
  return encodeResetPayload(code);
}

export function decodeGoAwayPayload(payload: Uint8Array): number {
  if (payload.byteLength !== 4) throw new Error('GOAWAY payload must be 4 bytes');
  return new DataView(payload.buffer, payload.byteOffset, 4).getUint32(0, false);
}

const utf8Enc = new TextEncoder();
const utf8Dec = new TextDecoder('utf-8', { fatal: false });

export function encodeWSClosePayload(code: number, reason = ''): Uint8Array {
  let rb = utf8Enc.encode(reason);
  const maxReason = MAX_WS_CLOSE_PAYLOAD_BYTES - 2;
  if (rb.byteLength > maxReason) {
    let n = maxReason;
    while (n > 0 && (rb[n] & 0xc0) === 0x80) n--;
    rb = rb.subarray(0, n);
  }
  const out = new Uint8Array(2 + rb.byteLength);
  new DataView(out.buffer).setUint16(0, code & 0xffff, false);
  out.set(rb, 2);
  return out;
}

export function decodeWSClosePayload(payload: Uint8Array): { code: number; reason: string } {
  if (payload.byteLength < 2 || payload.byteLength > MAX_WS_CLOSE_PAYLOAD_BYTES) {
    throw new Error('invalid WS_CLOSE payload length');
  }
  const code = new DataView(payload.buffer, payload.byteOffset, 2).getUint16(0, false);
  const reason = utf8Dec.decode(payload.subarray(2));
  return { code, reason };
}
