/**
 * Strictly Typed IPC Schemas (zero `any`)
 * Shared across Portal, Hidden Bridge iframe, Service Worker, and compatibility patch.
 * Uses native Transferable ReadableStreams over MessagePort for HTTP bodies (zero manual IPC credit loops).
 */

import type { HeaderField } from '../protocol/tlv';

export interface CookieDelta {
  op: 'set' | 'del';
  name: string;
  value?: string;
  path: string;
  expires?: number; // unix ms (0 or omitted = session cookie)
}

export interface ClientCookieMutation {
  op: 'set' | 'del';
  name: string;
  value?: string;
  path?: string;
  expires?: number; // unix ms (0 or omitted = session cookie)
}

export interface BackendMeta {
  nodeId: string;
  backendOrigin: string;
  isStaticMode: boolean;
  sameSiteMode: boolean;
  pinnedPk?: string;
  initialCookies?: CookieDelta[];
}

// SW -> Portal over bridgePort (with per-request MessagePort in ports[0], plus optional transferred bodyStream)
export interface TunnelFetchRequestMsg {
  type: 'TUNNEL_FETCH_REQUEST';
  reqId: string;
  tabId: string;
  method: string;
  target: string;
  headers: HeaderField[];
  hasBody: boolean;
  bodyStream?: ReadableStream<Uint8Array>;
  fetchCtx?: number;
  cookieMutations?: ClientCookieMutation[];
}

// SW -> Portal over bridgePort (with per-WS MessagePort in ports[0])
export interface TunnelWsOpenMsg {
  type: 'TUNNEL_WS_OPEN';
  wsId: string;
  tabId: string;
  target: string;
  protocols: string[];
  fetchCtx?: number;
  cookieMutations?: ClientCookieMutation[];
}

// Portal -> SW over per-request MessagePort
export type RequestPortDownMsg =
  | { type: 'REQ_ACK'; reqId: string }
  | {
      type: 'RESP_START';
      status: number;
      respFlags?: number;
      headers: HeaderField[];
      cookieDeltas?: CookieDelta[];
      hasBody: boolean;
      bodyStream?: ReadableStream<Uint8Array>;
    }
  | { type: 'RESP_DONE' }
  | { type: 'RESP_ERROR'; status: number; message: string };

// SW -> Portal over per-request MessagePort
export type RequestPortUpMsg = { type: 'RESP_CANCEL' };

// WebSocket MessagePort messages (App/Worker <-> Portal)
export type WsPortToPortalMsg =
  | { type: 'WS_SEND'; opcode: 1 | 2; data: ArrayBuffer | string }
  | { type: 'WS_CLOSE'; code: number; reason: string };

export type WsPortFromPortalMsg =
  | { type: 'WS_OPEN'; protocol: string; cookieDeltas?: CookieDelta[] }
  | { type: 'WS_MSG'; opcode: 1 | 2; data: ArrayBuffer | string }
  | { type: 'WS_ACK'; bytes: number }
  | { type: 'WS_CLOSE'; code: number; reason: string }
  | { type: 'WS_ERROR'; message: string };

export function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

export function isCookieDelta(v: unknown): v is CookieDelta {
  return (
    isRecord(v) &&
    (v.op === 'set' || v.op === 'del') &&
    typeof v.name === 'string' &&
    v.name.length > 0 &&
    typeof v.path === 'string' &&
    (v.value === undefined || typeof v.value === 'string') &&
    (v.expires === undefined || typeof v.expires === 'number')
  );
}

export function isClientCookieMutation(v: unknown): v is ClientCookieMutation {
  return (
    isRecord(v) &&
    (v.op === 'set' || v.op === 'del') &&
    typeof v.name === 'string' &&
    v.name.length > 0 &&
    (v.value === undefined || typeof v.value === 'string') &&
    (v.path === undefined || typeof v.path === 'string') &&
    (v.expires === undefined || typeof v.expires === 'number')
  );
}

export function isBackendMeta(v: unknown): v is BackendMeta {
  return (
    isRecord(v) &&
    typeof v.nodeId === 'string' &&
    typeof v.backendOrigin === 'string' &&
    typeof v.isStaticMode === 'boolean' &&
    typeof v.sameSiteMode === 'boolean'
  );
}
