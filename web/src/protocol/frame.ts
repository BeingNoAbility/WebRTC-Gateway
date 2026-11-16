export const HeaderSize = 10;
export const RecommendedChunkSize = 32 * 1024;
export const MaxPayloadSize = 64 * 1024;

export const FrameType = {
  DATA: 0x01,
  HEADERS: 0x02,
  RST_STREAM: 0x03,
  WINDOW_UPDATE: 0x04,
  PING: 0x05,
  PONG: 0x06,
  WS_CONNECT: 0x10,
  WS_DATA: 0x11,
  WS_CLOSE: 0x12,
} as const;

export const FrameFlags = {
  NONE: 0x00,
  FIN: 0x01,
  ACK: 0x02,
} as const;

export interface DecodedFrame {
  streamId: number;
  type: number;
  flags: number;
  length: number;
  payload: Uint8Array;
}

export function encodeFrame(
  streamId: number,
  type: number,
  flags: number,
  payload?: Uint8Array | null
): Uint8Array {
  const payloadLen = payload ? payload.byteLength : 0;
  const totalLen = HeaderSize + payloadLen;
  const buffer = new Uint8Array(totalLen);
  const view = new DataView(buffer.buffer, buffer.byteOffset, buffer.byteLength);

  // BigEndian 32-bit Stream ID
  view.setUint32(0, streamId, false);
  // Type (uint8)
  view.setUint8(4, type);
  // Flags (uint8)
  view.setUint8(5, flags);
  // BigEndian 32-bit Payload Length
  view.setUint32(6, payloadLen, false);

  if (payload && payloadLen > 0) {
    buffer.set(payload, HeaderSize);
  }

  return buffer;
}

export function decodeFrame(data: Uint8Array): DecodedFrame {
  if (data.byteLength < HeaderSize) {
    throw new Error(`Buffer too short for frame header: ${data.byteLength} < ${HeaderSize}`);
  }

  const view = new DataView(data.buffer, data.byteOffset, data.byteLength);
  const streamId = view.getUint32(0, false);
  const type = view.getUint8(4);
  const flags = view.getUint8(5);
  const length = view.getUint32(6, false);

  if (data.byteLength !== HeaderSize + length) {
    throw new Error(`Buffer size mismatch: expected ${HeaderSize + length}, got ${data.byteLength}`);
  }

  // Use .slice() to create an independent Uint8Array with its own ArrayBuffer starting at offset 0
  const payload = data.slice(HeaderSize, HeaderSize + length);

  return {
    streamId,
    type,
    flags,
    length,
    payload,
  };
}
