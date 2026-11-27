/**
 * Structured RFC 8841 DataChannel-Only SessionDescriptor Extraction, Deterministic SDP Synthesis,
 * and V7 Length-Prefixed Pure Ed25519 Transcript Verification.
 */

import { MIN_NEGOTIATED_MAX_MSG_SIZE } from '../protocol/frame';
import { appendLenPrefixed, base64UrlDecode, sha256Hex, verifyEd25519 } from './crypto';

export interface SessionDescriptor {
  fp: string; // 64 lowercase hex chars (SHA-256 DTLS fingerprint)
  ice_ufrag: string; // RFC 5245 ice-ufrag
  ice_pwd: string; // RFC 5245 ice-pwd
  setup: 'actpass' | 'active' | 'passive';
  mid: string; // single m=application media ID
  sctp_port: number; // 1..65535
  max_msg_size: number; // 4096..1073741824
  sdp_ver: number; // monotonic RFC 4566 o= session version (>= 2)
}

const DEFAULT_SESSION_ID = '4611731400430051336';
const FP_HEX_RE = /^[0-9a-f]{64}$/;
const ICE_UFRAG_RE = /^[A-Za-z0-9+/]{4,256}$/;
const ICE_PWD_RE = /^[A-Za-z0-9+/]{22,256}$/;
const MID_RE = /^[A-Za-z0-9_-]{1,16}$/;
const STRICT_UINT_RE = /^\d+$/;
const MAX_SAFE_SDP_VER = 2 ** 50;

export function validateSessionDescriptor(desc: unknown, isOffer: boolean): desc is SessionDescriptor {
  if (!desc || typeof desc !== 'object') return false;
  const d = desc as Record<string, unknown>;
  if (typeof d.fp !== 'string' || !FP_HEX_RE.test(d.fp)) return false;
  if (typeof d.ice_ufrag !== 'string' || !ICE_UFRAG_RE.test(d.ice_ufrag)) return false;
  if (typeof d.ice_pwd !== 'string' || !ICE_PWD_RE.test(d.ice_pwd)) return false;
  if (isOffer) {
    if (d.setup !== 'actpass' && d.setup !== 'active' && d.setup !== 'passive') return false;
  } else {
    if (d.setup !== 'active' && d.setup !== 'passive') return false;
  }
  if (typeof d.mid !== 'string' || !MID_RE.test(d.mid)) return false;
  if (typeof d.sctp_port !== 'number' || !Number.isInteger(d.sctp_port) || d.sctp_port < 1 || d.sctp_port > 65535) {
    return false;
  }
  if (
    typeof d.max_msg_size !== 'number' ||
    !Number.isInteger(d.max_msg_size) ||
    d.max_msg_size < MIN_NEGOTIATED_MAX_MSG_SIZE ||
    d.max_msg_size > 1073741824
  ) {
    return false;
  }
  if (
    typeof d.sdp_ver !== 'number' ||
    !Number.isInteger(d.sdp_ver) ||
    d.sdp_ver < 2 ||
    d.sdp_ver > MAX_SAFE_SDP_VER
  ) {
    return false;
  }
  return true;
}

// Returns hex for sha-256, null for other algorithms (e.g. sha-512), or throws on malformed line/sha-256.
function parseSHA256FingerprintHex(raw: string): string | null {
  const parts = raw.trim().toLowerCase().split(/\s+/);
  if (parts.length !== 2) {
    throw new Error(`Malformed DTLS fingerprint line: ${raw}`);
  }
  if (parts[0] !== 'sha-256') {
    return null;
  }
  const hex = parts[1].replace(/:/g, '');
  if (!FP_HEX_RE.test(hex)) {
    throw new Error('Invalid SHA-256 DTLS fingerprint');
  }
  return hex;
}

export function formatColonFingerprint(fpHex: string): string {
  const up = fpHex.toUpperCase();
  const parts: string[] = [];
  for (let i = 0; i < up.length; i += 2) {
    parts.push(up.slice(i, i + 2));
  }
  return parts.join(':');
}

/**
 * Strictly extracts a DataChannel-only SessionDescriptor from locally created WebRTC SDP.
 * Supports multi-algorithm fingerprint lines while rejecting multi-section SDPs or conflicting sha-256 fingerprints.
 */
export function extractSessionDescriptor(rawSdp: string, sdpVer = 2): SessionDescriptor {
  if (!rawSdp || !rawSdp.trim()) {
    throw new Error('Empty SDP');
  }
  const lines = rawSdp.split(/\r?\n/);
  let mediaCount = 0;
  let inMedia = false;
  let sessFP = '';
  let mediaFP = '';
  let ufrag = '';
  let pwd = '';
  let setup = '';
  let mid = '';
  let sctpPort = 5000;
  let maxMsgSize = 262144;

  for (const rawLine of lines) {
    const line = rawLine.trim();
    if (!line) continue;
    if (line.startsWith('m=')) {
      mediaCount++;
      inMedia = true;
      if (!line.startsWith('m=application ')) {
        throw new Error(`Unexpected non-application media section: ${line}`);
      }
      continue;
    }
    if (!line.startsWith('a=')) continue;
    const attr = line.slice(2);
    const colon = attr.indexOf(':');
    if (colon < 0) continue;
    const key = attr.slice(0, colon);
    const val = attr.slice(colon + 1).trim();

    switch (key) {
      case 'fingerprint': {
        const hex = parseSHA256FingerprintHex(val);
        if (hex === null) break;
        if (inMedia) {
          if (mediaFP && mediaFP !== hex) {
            throw new Error('Conflicting media-level DTLS fingerprints');
          }
          mediaFP = hex;
        } else {
          if (sessFP && sessFP !== hex) {
            throw new Error('Conflicting session-level DTLS fingerprints');
          }
          sessFP = hex;
        }
        break;
      }
      case 'ice-ufrag':
        ufrag = val;
        break;
      case 'ice-pwd':
        pwd = val;
        break;
      case 'setup':
        setup = val.toLowerCase();
        break;
      case 'mid':
        if (inMedia) mid = val;
        break;
      case 'sctp-port': {
        if (!STRICT_UINT_RE.test(val)) throw new Error(`Invalid sctp-port: ${val}`);
        const n = Number(val);
        if (!Number.isSafeInteger(n)) throw new Error(`Invalid sctp-port: ${val}`);
        sctpPort = n;
        break;
      }
      case 'max-message-size': {
        if (!STRICT_UINT_RE.test(val)) throw new Error(`Invalid max-message-size: ${val}`);
        const n = Number(val);
        if (!Number.isSafeInteger(n)) throw new Error(`Invalid max-message-size: ${val}`);
        maxMsgSize = n;
        break;
      }
    }
  }

  if (mediaCount !== 1) {
    throw new Error(`Expected exactly 1 media description, got ${mediaCount}`);
  }
  if (sessFP && mediaFP && sessFP !== mediaFP) {
    throw new Error('Conflicting session-level and media-level DTLS fingerprints');
  }
  const fp = mediaFP || sessFP;
  if (!mid) mid = '0';
  // RFC 8841 §6.6: max-message-size:0 denotes unlimited -> normalize to 262144;
  // positive values below MIN_NEGOTIATED_MAX_MSG_SIZE (4096) fail closed.
  let normalizedMMS: number;
  if (maxMsgSize === 0) {
    normalizedMMS = 262144;
  } else if (maxMsgSize < MIN_NEGOTIATED_MAX_MSG_SIZE) {
    throw new Error(`max-message-size ${maxMsgSize} below minimum ${MIN_NEGOTIATED_MAX_MSG_SIZE}`);
  } else {
    normalizedMMS = Math.min(1073741824, maxMsgSize);
  }
  const ver = Math.max(2, Math.floor(sdpVer));

  const desc: SessionDescriptor = {
    fp,
    ice_ufrag: ufrag,
    ice_pwd: pwd,
    setup: setup as SessionDescriptor['setup'],
    mid,
    sctp_port: sctpPort,
    max_msg_size: normalizedMMS,
    sdp_ver: ver,
  };
  if (!validateSessionDescriptor(desc, setup === 'actpass')) {
    throw new Error('Extracted SessionDescriptor failed strict validation');
  }
  return desc;
}

export function synthesizeSDP(desc: SessionDescriptor, isOffer: boolean): string {
  if (!validateSessionDescriptor(desc, isOffer)) {
    throw new Error('Cannot synthesize SDP from invalid SessionDescriptor');
  }
  const lines = [
    'v=0',
    `o=- ${DEFAULT_SESSION_ID} ${desc.sdp_ver} IN IP4 127.0.0.1`,
    's=-',
    't=0 0',
    `a=group:BUNDLE ${desc.mid}`,
    'a=msid-semantic: WMS',
    'm=application 9 UDP/DTLS/SCTP webrtc-datachannel',
    'c=IN IP4 0.0.0.0',
    `a=ice-ufrag:${desc.ice_ufrag}`,
    `a=ice-pwd:${desc.ice_pwd}`,
    'a=ice-options:trickle',
    `a=fingerprint:sha-256 ${formatColonFingerprint(desc.fp)}`,
    `a=setup:${desc.setup}`,
    `a=mid:${desc.mid}`,
    `a=sctp-port:${desc.sctp_port}`,
    `a=max-message-size:${desc.max_msg_size}`,
  ];
  return lines.join('\r\n') + '\r\n';
}

/**
 * Encodes the canonical V7 length-prefixed Pure Ed25519 answer transcript
 * (byte-for-byte identical to Go agent.EncodeAnswerTranscript).
 */
export function encodeAnswerTranscript(
  nodeId: string,
  visitorNonce: string,
  offer: SessionDescriptor,
  answer: SessionDescriptor
): Uint8Array {
  if (!nodeId || !visitorNonce) {
    throw new Error('Empty nodeId or visitorNonce');
  }
  if (!validateSessionDescriptor(offer, true)) {
    throw new Error('Invalid offer descriptor');
  }
  if (!validateSessionDescriptor(answer, false)) {
    throw new Error('Invalid answer descriptor');
  }
  return appendLenPrefixed('p2pgw-v7:answer-v2:', [
    nodeId,
    visitorNonce,
    offer.fp,
    offer.ice_ufrag,
    offer.ice_pwd,
    offer.setup,
    offer.mid,
    String(offer.sctp_port),
    String(offer.max_msg_size),
    String(offer.sdp_ver),
    answer.fp,
    answer.ice_ufrag,
    answer.ice_pwd,
    answer.setup,
    answer.mid,
    String(answer.sctp_port),
    String(answer.max_msg_size),
    String(answer.sdp_ver),
  ]);
}

export async function verifyAnswerTranscript(
  pkB64: string,
  nodeId: string,
  visitorNonce: string,
  offer: SessionDescriptor,
  answer: SessionDescriptor,
  sigB64: string
): Promise<boolean> {
  try {
    const pkBytes = base64UrlDecode(pkB64);
    const sigBytes = base64UrlDecode(sigB64);
    if (pkBytes.length !== 32 || sigBytes.length !== 64) {
      return false;
    }
    const transcript = encodeAnswerTranscript(nodeId, visitorNonce, offer, answer);
    return await verifyEd25519(pkBytes, transcript, sigBytes);
  } catch {
    return false;
  }
}

export interface PeerConnectionDtlsLike {
  sctp?: {
    transport?: {
      getRemoteCertificates?: () => ArrayBuffer[];
    } | null;
  } | null;
}

export async function verifyPeerDtlsCertificate(
  pc: PeerConnectionDtlsLike,
  expectedFpHex: string
): Promise<boolean> {
  const dtlsTransport = pc.sctp?.transport;
  const rtcDtlsCtor = (globalThis as { RTCDtlsTransport?: { prototype?: { getRemoteCertificates?: unknown } } })
    .RTCDtlsTransport;
  const runtimeSupportsDtlsCerts =
    typeof rtcDtlsCtor !== 'undefined' &&
    typeof rtcDtlsCtor.prototype?.getRemoteCertificates === 'function';
  if (!dtlsTransport || typeof dtlsTransport.getRemoteCertificates !== 'function') {
    // Fail-Closed when browser exposes RTCDtlsTransport.getRemoteCertificates (Chromium/Firefox)
    // but pc.sctp or transport is missing; degrade gracefully only on engines lacking the API (e.g. Safari).
    return !runtimeSupportsDtlsCerts;
  }
  const certs = dtlsTransport.getRemoteCertificates();
  if (!certs || certs.length === 0) {
    return false;
  }
  const gotHex = await sha256Hex(new Uint8Array(certs[0]));
  return gotHex === expectedFpHex.toLowerCase();
}
