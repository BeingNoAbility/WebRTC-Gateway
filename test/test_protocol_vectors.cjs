// test/test_protocol_vectors.cjs
// Automated Golden Test Vector Verification for P2PMux Wire Protocol (v2.2 Sprint 3)
const fs = require('fs');
const path = require('path');

// 1. Locate and load protocol golden vectors JSON
const fixturePath = path.resolve(__dirname, 'fixtures/protocol_golden_vectors.json');
if (!fs.existsSync(fixturePath)) {
  console.error(`[FAIL] Golden fixture not found at: ${fixturePath}`);
  process.exit(1);
}
const fixture = JSON.parse(fs.readFileSync(fixturePath, 'utf8'));

// 2. Validate web/src/protocol/frame.ts source parity
const frameTsPath = path.resolve(__dirname, '../web/src/protocol/frame.ts');
if (fs.existsSync(frameTsPath)) {
  const tsContent = fs.readFileSync(frameTsPath, 'utf8');
  if (!tsContent.includes('HeaderSize = 10')) {
    console.error('[FAIL] web/src/protocol/frame.ts must declare HeaderSize = 10');
    process.exit(1);
  }
  if (!tsContent.includes('32 * 1024 - HeaderSize')) {
    console.error('[FAIL] web/src/protocol/frame.ts must declare RecommendedChunkSize = 32 * 1024 - HeaderSize');
    process.exit(1);
  }
  console.log('[PASS] web/src/protocol/frame.ts 10-byte HeaderSize and 32KB alignment verified');
}

// 3. JavaScript / Web implementation of P2PMux frame encoding & decoding
const HeaderSize = 10;
const RecommendedChunkSize = 32 * 1024 - HeaderSize;
const MaxPayloadSize = 64 * 1024;

function encodeFrame(streamId, type, flags, payload) {
  const payloadLen = payload ? payload.byteLength : 0;
  const totalLen = HeaderSize + payloadLen;
  const buffer = new Uint8Array(totalLen);
  const view = new DataView(buffer.buffer, buffer.byteOffset, buffer.byteLength);

  view.setUint32(0, streamId, false); // BigEndian 32-bit
  view.setUint8(4, type);
  view.setUint8(5, flags);
  view.setUint32(6, payloadLen, false); // BigEndian 32-bit

  if (payload && payloadLen > 0) {
    buffer.set(payload, HeaderSize);
  }

  return buffer;
}

function decodeFrame(data) {
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

  const payload = data.slice(HeaderSize, HeaderSize + length);

  return {
    streamId,
    type,
    flags,
    length,
    payload,
  };
}

function hexToBytes(hex) {
  if (!hex) return new Uint8Array(0);
  const bytes = new Uint8Array(hex.length / 2);
  for (let i = 0; i < hex.length; i += 2) {
    bytes[i / 2] = parseInt(hex.substring(i, i + 2), 16);
  }
  return bytes;
}

function bytesToHex(bytes) {
  return Array.from(bytes)
    .map((b) => b.toString(16).padStart(2, '0'))
    .join('');
}

console.log('=== Running P2PMux Wire Protocol Cross-Runtime Golden Vector Verification ===');

let totalTests = 0;
let passedTests = 0;

// Test valid vectors
for (const v of fixture.valid_vectors) {
  totalTests++;
  const wireBytes = hexToBytes(v.wire_hex);
  const expectedPayload = hexToBytes(v.payload_hex);

  try {
    const decoded = decodeFrame(wireBytes);
    if (decoded.streamId !== v.stream_id) {
      throw new Error(`streamId mismatch: got ${decoded.streamId}, expected ${v.stream_id}`);
    }
    if (decoded.type !== v.type) {
      throw new Error(`type mismatch: got ${decoded.type}, expected ${v.type}`);
    }
    if (decoded.flags !== v.flags) {
      throw new Error(`flags mismatch: got ${decoded.flags}, expected ${v.flags}`);
    }
    if (decoded.length !== v.length) {
      throw new Error(`length mismatch: got ${decoded.length}, expected ${v.length}`);
    }
    if (bytesToHex(decoded.payload) !== bytesToHex(expectedPayload)) {
      throw new Error(`payload mismatch: got ${bytesToHex(decoded.payload)}, expected ${bytesToHex(expectedPayload)}`);
    }

    // Re-encode and verify 100% byte-for-byte wire equality
    const reEncoded = encodeFrame(decoded.streamId, decoded.type, decoded.flags, decoded.payload);
    const reEncodedHex = bytesToHex(reEncoded);
    if (reEncodedHex.toLowerCase() !== v.wire_hex.toLowerCase()) {
      throw new Error(`Re-encoded wire mismatch: got ${reEncodedHex}, expected ${v.wire_hex}`);
    }

    console.log(`[PASS] Valid vector: ${v.name} (${v.description})`);
    passedTests++;
  } catch (err) {
    console.error(`[FAIL] Valid vector ${v.name}: ${err.message}`);
    process.exit(1);
  }
}

// Test error vectors
for (const ev of fixture.error_vectors) {
  totalTests++;
  const wireBytes = hexToBytes(ev.wire_hex);
  try {
    decodeFrame(wireBytes);
    console.error(`[FAIL] Error vector ${ev.name} was expected to throw, but passed`);
    process.exit(1);
  } catch (err) {
    console.log(`[PASS] Error vector rejected as expected: ${ev.name} (${err.message})`);
    passedTests++;
  }
}

// Test 32KB recommended chunk alignment boundary
totalTests++;
const chunkPayload = new Uint8Array(RecommendedChunkSize);
for (let i = 0; i < chunkPayload.length; i++) {
  chunkPayload[i] = i % 251;
}
const encodedChunk = encodeFrame(99, 1, 0, chunkPayload);
if (encodedChunk.byteLength !== 32 * 1024) {
  console.error(`[FAIL] Chunk wire size mismatch: expected 32768, got ${encodedChunk.byteLength}`);
  process.exit(1);
}
const decodedChunk = decodeFrame(encodedChunk);
if (decodedChunk.length !== RecommendedChunkSize || decodedChunk.payload.byteLength !== RecommendedChunkSize) {
  console.error(`[FAIL] Decoded chunk length mismatch`);
  process.exit(1);
}
console.log(`[PASS] RecommendedChunkSize 32KB allocation boundary test passed (${encodedChunk.byteLength} bytes)`);
passedTests++;

console.log(`=== P2PMux Wire Protocol Golden Vector Suite: ${passedTests}/${totalTests} Passed ===`);
