/**
 * P2PMux V8 Binary Frame Codec
 * Byte-for-byte mirror of Go pkg/protocol/frame.go.
 *
 * Fixed 8-byte header (Big-Endian):
 *   [0:4] StreamID (uint32 BE: 0 on p2pctrl, strictly monotonic >= 1 per data lane)
 *   [4]   Type     (uint8)
 *   [5]   Flags    (uint8)
 *   [6:8] Length   (uint16 BE)
 */

export const PROTOCOL_VERSION = 8;
export const HEADER_SIZE = 8;
export const MAX_PAYLOAD_SIZE = 65535;
export const DEFAULT_CHUNK_SIZE = 32760;
export const MIN_NEGOTIATED_MAX_MSG_SIZE = 4096;
export const MIN_CHUNK_SIZE = MIN_NEGOTIATED_MAX_MSG_SIZE - HEADER_SIZE;
export const MAX_TOTAL_HEADERS_SIZE = 64 * 1024;
export const MAX_HEADER_CHUNKS = 17;
export const MAX_CONTROL_JSON_SIZE = 128 * 1024;
export const MAX_CONTROL_CHUNKS = 34;
export const MAX_WS_MESSAGE_BYTES = 16 * 1024 * 1024;
export const MAX_WS_MESSAGE_CHUNKS = 16384;
export const MAX_WS_CLOSE_PAYLOAD_BYTES = 125;

export const CONTROL_CHANNEL_ID = 0;
export const CONTROL_CHANNEL_LABEL = 'p2pctrl';
export const DEFAULT_POOL_SIZE = 12;
export const MAX_POOL_SIZE = 16;
export const DEFAULT_MAX_WS_CONNS = 64;

export const FrameType = {
  // Control-plane (p2pctrl, id=0, streamId === 0)
  SETTINGS: 0x01,
  AUTH_CHALLENGE: 0x02,
  AUTH_RESPONSE: 0x03,
  AUTH_RESULT: 0x04,
  CAP_REQ: 0x05,
  CAP_RESP: 0x06,
  PING: 0x08,
  PONG: 0x09,
  GOAWAY: 0x0a,

  // Multiplexed data-lane frames (p2p_01..N, streamId >= 1)
  HEADERS: 0x10,
  DATA: 0x12,
  CREDIT: 0x13,
  RESET: 0x14,
  WS_TEXT: 0x21,
  WS_BINARY: 0x22,
  WS_CLOSE: 0x24,
} as const;

export const FrameFlag = {
  NONE: 0x00,
  FIN: 0x01,
  MSG_FIN: 0x02,
  MORE: 0x04,
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
  streamId: number;
  type: number;
  flags: number;
  payload: Uint8Array;
}

export function poolChannelId(laneIdx: number): number {
  return 1 + laneIdx;
}

export function poolChannelLabel(laneIdx: number): string {
  const n = laneIdx + 1;
  return `p2p_${n < 10 ? '0' : ''}${n}`;
}

export function isControlFrameType(type: number): boolean {
  return (
    (type >= FrameType.SETTINGS && type <= FrameType.CAP_RESP) ||
    (type >= FrameType.PING && type <= FrameType.GOAWAY)
  );
}

export function isControlJsonFrameType(type: number): boolean {
  return type >= FrameType.SETTINGS && type <= FrameType.CAP_RESP;
}

export function isStreamFrameType(type: number): boolean {
  return (
    type === FrameType.HEADERS ||
    type === FrameType.DATA ||
    type === FrameType.CREDIT ||
    type === FrameType.RESET ||
    type === FrameType.WS_TEXT ||
    type === FrameType.WS_BINARY ||
    type === FrameType.WS_CLOSE
  );
}

export function validateFrameMeta(streamId: number, type: number, flags: number, payloadLen: number): void {
  if (!Number.isInteger(streamId) || streamId < 0 || streamId > 0xffffffff) {
    throw new RangeError(`invalid streamId: ${streamId}`);
  }
  if (!Number.isInteger(flags) || flags < 0 || flags > 0xff) {
    throw new RangeError(`invalid frame flags: ${flags}`);
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
      if (streamId !== 0) throw new Error('control JSON frame requires streamId === 0');
      if ((flags & ~FrameFlag.MORE) !== 0) throw new Error('invalid flags on control JSON frame');
      if (payloadLen === 0 && (flags & FrameFlag.MORE) !== 0) {
        throw new Error('non-final 0-byte control JSON chunk is forbidden');
      }
      break;
    case FrameType.PING:
    case FrameType.PONG:
      if (streamId !== 0 || flags !== FrameFlag.NONE || payloadLen !== 8) {
        throw new Error('PING/PONG requires streamId === 0, flags === 0, len === 8');
      }
      break;
    case FrameType.GOAWAY:
      if (streamId !== 0 || flags !== FrameFlag.NONE || payloadLen !== 4) {
        throw new Error('GOAWAY requires streamId === 0, flags === 0, len === 4');
      }
      break;
    case FrameType.CREDIT:
    case FrameType.RESET:
      if (streamId === 0 || flags !== FrameFlag.NONE || payloadLen !== 4) {
        throw new Error('CREDIT/RESET requires streamId > 0, flags === 0, len === 4');
      }
      break;
    case FrameType.HEADERS:
      if (streamId === 0) throw new Error('HEADERS requires streamId > 0');
      if ((flags & ~(FrameFlag.FIN | FrameFlag.MORE)) !== 0) {
        throw new Error('invalid flags on HEADERS');
      }
      if (payloadLen === 0 && (flags & FrameFlag.MORE) !== 0) {
        throw new Error('non-final 0-byte HEADERS chunk is forbidden');
      }
      break;
    case FrameType.DATA:
      if (streamId === 0) throw new Error('DATA requires streamId > 0');
      if ((flags & ~FrameFlag.FIN) !== 0) {
        throw new Error('invalid flags on DATA');
      }
      if (payloadLen === 0 && (flags & FrameFlag.FIN) === 0) {
        throw new Error('non-final 0-byte DATA is forbidden');
      }
      break;
    case FrameType.WS_TEXT:
    case FrameType.WS_BINARY:
      if (streamId === 0) throw new Error('WS frame requires streamId > 0');
      if ((flags & ~FrameFlag.MSG_FIN) !== 0) {
        throw new Error('invalid flags on WS frame');
      }
      if (payloadLen === 0 && (flags & FrameFlag.MSG_FIN) === 0) {
        throw new Error('non-final 0-byte WS chunk is forbidden');
      }
      break;
    case FrameType.WS_CLOSE:
      if (streamId === 0 || flags !== FrameFlag.NONE || payloadLen < 2 || payloadLen > MAX_WS_CLOSE_PAYLOAD_BYTES) {
        throw new Error('WS_CLOSE requires streamId > 0, flags === 0, len in [2, 125]');
      }
      break;
    default:
      throw new Error(`unknown frame type: 0x${type.toString(16)}`);
  }
}

export function encodeFrame(
  streamId: number,
  type: number,
  flags: number,
  payload: Uint8Array = new Uint8Array(0)
): Uint8Array {
  validateFrameMeta(streamId, type, flags, payload.byteLength);
  if (type === FrameType.CREDIT) {
    decodeCreditPayload(payload);
  }
  const out = new Uint8Array(HEADER_SIZE + payload.byteLength);
  const view = new DataView(out.buffer);
  view.setUint32(0, streamId >>> 0, false);
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
  const streamId = view.getUint32(0, false);
  const type = raw[4];
  const flags = raw[5];
  const declaredLen = view.getUint16(6, false);
  if (raw.byteLength !== HEADER_SIZE + declaredLen) {
    throw new Error(`frame length mismatch: declared=${declaredLen} actual=${raw.byteLength - HEADER_SIZE}`);
  }
  validateFrameMeta(streamId, type, flags, declaredLen);
  const payload = raw.subarray(HEADER_SIZE);
  if (type === FrameType.CREDIT) {
    decodeCreditPayload(payload);
  }
  return { streamId, type, flags, payload };
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
// H-6b: ignoreBOM: true ensures U+FEFF bytes are preserved identically to Go UTF-8 decoding.
const utf8Dec = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true });

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
