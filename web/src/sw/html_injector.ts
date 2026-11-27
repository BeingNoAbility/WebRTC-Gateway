/**
 * V7 Streaming Byte-Level HTML5 Head Tokenizer & Safe Injector
 * - Single-tag absolute URL `<script src="${sandboxOrigin}${PATCH_ASSET_URL}" data-p2p-boot="..."></script>`
 *   with expired cookie filtering & 16 KiB boot payload cap.
 * - Exact 1024-byte `<meta charset>` prescan protection when HTTP `Content-Type` omits `charset=`:
 *   injects right after `<meta charset>` / `<meta http-equiv="content-type">`, or before the first
 *   `<script>` / `</head>` / `<body>` / byte 1024 so `<meta charset>` is never pushed past Chrome's
 *   1024-byte prescan window!
 * - Implements all 9 HTML5 tokenizer edge-case fixes:
 *   1) `TAG_OPEN` re-consumes `<` (`<<head>`)
 *   2) WHATWG HTML5 tag-name delimiter (whitespace / `/` / `>`)
 *   3) Rawtext end-tag attribute parsing (`</style a=">">`)
 *   4) `</ ` / `</1` enters `BOGUS_COMMENT` (HTML5 §13.2.5.7)
 *   5) `<template>`, `<svg>`, `<math>` nesting depth + `<svg/>`/`<math/>` self-closing
 *   6) `<script>` escaped & double-escaped states (`<!-- <script></script> -->`)
 *   7) Headless documents inject BEFORE the first `<script>` tag
 *   8) UTF-16 BOM pass-through + UTF-8 BOM preservation at byte 0
 *   9) ES Module Worker + Classic Worker bootstrap support
 */

import { base64UrlEncode } from '../shared/crypto';
import type { CookieDelta } from '../shared/ipc';

export interface PatchInjectConfig {
  nodeId: string;
  portalOrigin: string;
  backendOrigin: string;
  pinnedPk?: string;
  initCookies: CookieDelta[];
}

export interface HtmlInjectorOptions {
  /** True when the HTTP `Content-Type` header already carries an explicit `charset=` parameter. */
  hasHttpCharset?: boolean;
}

const textEncoder = new TextEncoder();
const MAX_BOOT_JSON_BYTES = 16 * 1024;
const CHARSET_PRESCAN_LIMIT_BYTES = 1024;
const MAX_SCAN_BUFFER_BYTES = 256 * 1024;

export function resolvePatchAssetUrl(): string {
  const injected = (globalThis as { __P2P_GW_PATCH_URL__?: unknown }).__P2P_GW_PATCH_URL__;
  if (typeof injected === 'string' && injected.startsWith('/__p2p_gw__/')) {
    return injected;
  }
  return '/__p2p_gw__/patch.js';
}

function pruneAndBoundBootConfig(cfg: PatchInjectConfig, now = Date.now()): PatchInjectConfig {
  const freshCookies = (cfg.initCookies || []).filter(
    (c) => !(c.expires && c.expires > 0 && c.expires <= now)
  );
  const out: PatchInjectConfig = {
    nodeId: cfg.nodeId,
    portalOrigin: cfg.portalOrigin,
    backendOrigin: cfg.backendOrigin,
    pinnedPk: cfg.pinnedPk,
    initCookies: freshCookies,
  };
  let encoded = textEncoder.encode(JSON.stringify(out));
  while (encoded.byteLength > MAX_BOOT_JSON_BYTES && out.initCookies.length > 0) {
    out.initCookies.pop();
    encoded = textEncoder.encode(JSON.stringify(out));
  }
  return out;
}

export function buildPatchInjectionSnippet(sandboxOrigin: string, cfg: PatchInjectConfig): string {
  const safeOrigin = sandboxOrigin.replace(/\/+$/, '');
  const bounded = pruneAndBoundBootConfig(cfg);
  const bootB64 = base64UrlEncode(textEncoder.encode(JSON.stringify(bounded)));
  return `<script src="${safeOrigin}${resolvePatchAssetUrl()}" data-p2p-boot="${bootB64}"></script>`;
}

const enum State {
  DATA = 0,
  TAG_OPEN = 1,
  END_TAG_OPEN = 2,
  TAG_NAME = 3,
  BEFORE_ATTR_NAME = 4,
  ATTR_NAME = 5,
  AFTER_ATTR_NAME = 6,
  BEFORE_ATTR_VALUE = 7,
  ATTR_VAL_DQ = 8,
  ATTR_VAL_SQ = 9,
  ATTR_VAL_UNQUOTED = 10,
  SELF_CLOSING_START_TAG = 11,
  BANG = 12,
  COMMENT_START_1 = 13,
  COMMENT_START_2 = 14,
  COMMENT_START_DASH = 15,
  COMMENT = 16,
  COMMENT_END_1 = 17,
  COMMENT_END_2 = 18,
  COMMENT_END_BANG = 19,
  BOGUS_COMMENT = 20,
  DOCTYPE = 21,
  RAWTEXT = 22,
  SCRIPT_DATA = 23,
  SCRIPT_LT = 24,
  SCRIPT_END_TAG = 25,
  SCRIPT_ESC_START = 26,
  SCRIPT_ESC_START_DASH = 27,
  SCRIPT_ESCAPED = 28,
  SCRIPT_ESCAPED_DASH = 29,
  SCRIPT_ESCAPED_DASH_DASH = 30,
  SCRIPT_ESCAPED_LT = 31,
  SCRIPT_ESCAPED_END_TAG = 32,
  SCRIPT_DBL_ESC_START = 33,
  SCRIPT_DBL_ESCAPED = 34,
  SCRIPT_DBL_ESCAPED_DASH = 35,
  SCRIPT_DBL_ESCAPED_DASH_DASH = 36,
  SCRIPT_DBL_ESCAPED_LT = 37,
  SCRIPT_DBL_ESC_END = 38,
}

function asciiLower(b: number): number {
  return b >= 0x41 && b <= 0x5a ? b + 32 : b;
}

function isAsciiAlpha(b: number): boolean {
  return (b >= 0x41 && b <= 0x5a) || (b >= 0x61 && b <= 0x7a);
}

function isWhitespace(b: number): boolean {
  return b === 0x20 || b === 0x09 || b === 0x0a || b === 0x0d || b === 0x0c;
}

const RAWTEXT_TAGS = new Set([
  'style',
  'title',
  'textarea',
  'xmp',
  'noscript',
  'iframe',
  'noembed',
  'noframes',
]);

const NESTED_SKIP_TAGS = new Set(['template', 'svg', 'math']);

export class StreamingHtmlInjector {
  private readonly snippetBytes: Uint8Array;
  private readonly hasHttpCharset: boolean;
  private injected = false;
  private bypassed = false;
  private bomState = 0; // 0=start, 1=saw EF/FF/FE, 2=saw EF BB, 3=done
  private bomFirstByte = 0;

  private state: State = State.DATA;
  private tagName = '';
  private isEndTag = false;
  private selfClosing = false;
  private tagOpenGlobalOffset = 0;
  private bangBuf = '';

  // Rawtext & script state
  private rawTextEndTag = '';
  private rawTextMatchPos = 0;
  private scriptMatchPos = 0;

  // Nested foreign/template skip state (`<template>`, `<svg>`, `<math>`)
  private skipSubtreeTag = '';
  private skipSubtreeDepth = 0;

  // 1024B `<meta charset>` prescan state (only active when `!hasHttpCharset`)
  private inHeadPrescan = false;
  private headCloseEndGlobalOffset = 0;
  private prescanLimitGlobalOffset = CHARSET_PRESCAN_LIMIT_BYTES;
  private curAttrName = '';
  private curAttrVal = '';
  private metaIsCharset = false;

  private pendingChunks: Uint8Array[] = [];
  private pendingBytes = 0;
  private fallbackInsertGlobalOffset = 0;
  private firstScriptGlobalOffset = -1;

  constructor(sandboxOrigin: string, cfg: PatchInjectConfig, opts: HtmlInjectorOptions = {}) {
    this.snippetBytes = textEncoder.encode(buildPatchInjectionSnippet(sandboxOrigin, cfg));
    this.hasHttpCharset = Boolean(opts.hasHttpCharset);
  }

  push(chunk: Uint8Array): Uint8Array[] {
    if (chunk.byteLength === 0) return [];
    if (this.injected || this.bypassed) return [chunk];

    let data = chunk;
    if (this.bomState < 3) {
      let i = 0;
      while (i < data.byteLength && this.bomState < 3) {
        const b = data[i];
        if (this.bomState === 0) {
          if (b === 0xef || b === 0xff || b === 0xfe) {
            this.bomState = 1;
            this.bomFirstByte = b;
            i++;
          } else {
            this.bomState = 3;
            break;
          }
        } else if (this.bomState === 1) {
          if (this.bomFirstByte === 0xef && b === 0xbb) {
            this.bomState = 2;
            i++;
          } else if (
            (this.bomFirstByte === 0xff && b === 0xfe) ||
            (this.bomFirstByte === 0xfe && b === 0xff)
          ) {
            // UTF-16 BOM detected -> pass through untouched without UTF-8 corruption!
            this.bomState = 3;
            this.bypassed = true;
            const prefix = new Uint8Array([this.bomFirstByte, b]);
            const rest = data.subarray(i + 1);
            return rest.byteLength > 0 ? [prefix, rest] : [prefix];
          } else {
            const prefix = new Uint8Array([this.bomFirstByte]);
            this.bomState = 3;
            const combined = new Uint8Array(1 + (data.byteLength - i));
            combined.set(prefix, 0);
            combined.set(data.subarray(i), 1);
            data = combined;
            i = 0;
            break;
          }
        } else if (this.bomState === 2) {
          if (b === 0xbf) {
            this.bomState = 3;
            i++;
            this.pendingChunks.push(new Uint8Array([0xef, 0xbb, 0xbf]));
            this.pendingBytes += 3;
            this.fallbackInsertGlobalOffset = 3;
            this.prescanLimitGlobalOffset = 3 + CHARSET_PRESCAN_LIMIT_BYTES;
          } else {
            const prefix = new Uint8Array([0xef, 0xbb]);
            this.bomState = 3;
            const combined = new Uint8Array(2 + (data.byteLength - i));
            combined.set(prefix, 0);
            combined.set(data.subarray(i), 2);
            data = combined;
            i = 0;
            break;
          }
        }
      }
      if (i > 0) {
        data = data.subarray(i);
      }
      if (data.byteLength === 0) return [];
    }

    const baseOffset = this.pendingBytes;
    const insertGlobalOffset = this.scanChunkForInsertGlobalOffset(data, baseOffset);
    this.pendingChunks.push(data);
    this.pendingBytes += data.byteLength;

    if (insertGlobalOffset >= 0) {
      return this.flushAtGlobalOffset(insertGlobalOffset);
    }
    if (this.pendingBytes >= MAX_SCAN_BUFFER_BYTES) {
      const target = this.inHeadPrescan
        ? this.headCloseEndGlobalOffset
        : this.firstScriptGlobalOffset >= 0
          ? this.firstScriptGlobalOffset
          : this.fallbackInsertGlobalOffset;
      return this.flushAtGlobalOffset(target);
    }
    return [];
  }

  finish(): Uint8Array[] {
    if (this.injected || this.bypassed) return [];
    if (this.bomState === 1) {
      this.pendingChunks.push(new Uint8Array([this.bomFirstByte]));
      this.pendingBytes += 1;
      this.bomState = 3;
    } else if (this.bomState === 2) {
      this.pendingChunks.push(new Uint8Array([0xef, 0xbb]));
      this.pendingBytes += 2;
      this.bomState = 3;
    }
    const target = this.inHeadPrescan
      ? this.headCloseEndGlobalOffset
      : this.firstScriptGlobalOffset >= 0
        ? this.firstScriptGlobalOffset
        : this.fallbackInsertGlobalOffset;
    return this.flushAtGlobalOffset(target);
  }

  private flushAtGlobalOffset(rawTarget: number): Uint8Array[] {
    this.injected = true;
    const chunks = this.pendingChunks.splice(0);
    const total = this.pendingBytes;
    this.pendingBytes = 0;

    const insertAt = Math.max(0, Math.min(rawTarget, total));
    if (insertAt === 0) {
      return [this.snippetBytes, ...chunks];
    }

    const out: Uint8Array[] = [];
    let pos = 0;
    let placed = false;
    for (const c of chunks) {
      if (!placed && pos + c.byteLength >= insertAt) {
        const split = insertAt - pos;
        if (split > 0) out.push(c.subarray(0, split));
        out.push(this.snippetBytes);
        if (split < c.byteLength) out.push(c.subarray(split));
        placed = true;
      } else {
        out.push(c);
      }
      pos += c.byteLength;
    }
    if (!placed) {
      out.push(this.snippetBytes);
    }
    return out;
  }

  private commitAttr(): void {
    if (this.inHeadPrescan && !this.isEndTag && this.tagName === 'meta') {
      const name = this.curAttrName.toLowerCase();
      if (name === 'charset') {
        this.metaIsCharset = true;
      } else if (name === 'http-equiv' && this.curAttrVal.trim().toLowerCase() === 'content-type') {
        this.metaIsCharset = true;
      }
    }
    this.curAttrName = '';
    this.curAttrVal = '';
  }

  private resetTagState(openGlobalOffset: number): void {
    this.tagName = '';
    this.isEndTag = false;
    this.selfClosing = false;
    this.tagOpenGlobalOffset = openGlobalOffset;
    this.bangBuf = '';
    this.curAttrName = '';
    this.curAttrVal = '';
    this.metaIsCharset = false;
  }

  private scanChunkForInsertGlobalOffset(data: Uint8Array, baseOffset: number): number {
    for (let i = 0; i < data.byteLength; i++) {
      const b = data[i];
      const absPos = baseOffset + i;

      // If in <head> charset prescan and we reached the 1024-byte prescan ceiling, flush at <head> end!
      if (this.inHeadPrescan && absPos >= this.prescanLimitGlobalOffset) {
        return this.headCloseEndGlobalOffset;
      }

      switch (this.state) {
        case State.DATA:
          if (b === 0x3c /* '<' */) {
            this.resetTagState(absPos);
            this.state = State.TAG_OPEN;
          }
          break;

        case State.TAG_OPEN:
          if (b === 0x21 /* '!' */) {
            this.state = State.BANG;
            this.bangBuf = '';
          } else if (b === 0x3f /* '?' */) {
            this.state = State.BOGUS_COMMENT;
          } else if (b === 0x2f /* '/' */) {
            this.state = State.END_TAG_OPEN;
          } else if (isAsciiAlpha(b)) {
            this.tagName = String.fromCharCode(asciiLower(b));
            this.state = State.TAG_NAME;
          } else if (b === 0x3c /* '<' */) {
            // HTML5 §13.2.5.6: re-consume `<` in TAG_OPEN (`<<head>`!)
            this.resetTagState(absPos);
            this.state = State.TAG_OPEN;
          } else {
            this.state = State.DATA;
          }
          break;

        // HTML5 §13.2.5.7: `</ ` or `</1` enters BOGUS_COMMENT; `</>` returns to DATA.
        case State.END_TAG_OPEN:
          if (isAsciiAlpha(b)) {
            this.isEndTag = true;
            this.tagName = String.fromCharCode(asciiLower(b));
            this.state = State.TAG_NAME;
          } else if (b === 0x3e /* '>' */) {
            this.state = State.DATA;
          } else {
            this.state = State.BOGUS_COMMENT;
          }
          break;

        case State.BANG:
          if (b === 0x2d /* '-' */ && this.bangBuf === '') {
            this.state = State.COMMENT_START_1;
          } else if (b === 0x3e /* '>' */) {
            if (this.bangBuf.toLowerCase().startsWith('doctype')) {
              this.fallbackInsertGlobalOffset = absPos + 1;
            }
            this.state = State.DATA;
          } else {
            if (this.bangBuf.length < 8) {
              this.bangBuf += String.fromCharCode(asciiLower(b));
              if (this.bangBuf === 'doctype') {
                this.state = State.DOCTYPE;
              }
            } else {
              this.state = State.BOGUS_COMMENT;
            }
          }
          break;

        case State.DOCTYPE:
          if (b === 0x3e /* '>' */) {
            this.fallbackInsertGlobalOffset = absPos + 1;
            this.state = State.DATA;
          }
          break;

        case State.COMMENT_START_1:
          if (b === 0x2d) this.state = State.COMMENT_START_2;
          else if (b === 0x3e) this.state = State.DATA;
          else this.state = State.BOGUS_COMMENT;
          break;

        case State.COMMENT_START_2:
          if (b === 0x2d) this.state = State.COMMENT_START_DASH;
          else if (b === 0x3e) this.state = State.DATA;
          else this.state = State.COMMENT;
          break;

        case State.COMMENT_START_DASH:
          if (b === 0x2d) this.state = State.COMMENT_END_2;
          else if (b === 0x3e) this.state = State.DATA;
          else this.state = State.COMMENT;
          break;

        case State.COMMENT:
          if (b === 0x2d) this.state = State.COMMENT_END_1;
          break;

        case State.COMMENT_END_1:
          if (b === 0x2d) this.state = State.COMMENT_END_2;
          else this.state = State.COMMENT;
          break;

        case State.COMMENT_END_2:
          if (b === 0x3e) this.state = State.DATA;
          else if (b === 0x21) this.state = State.COMMENT_END_BANG;
          else if (b === 0x2d) this.state = State.COMMENT_END_2;
          else this.state = State.COMMENT;
          break;

        case State.COMMENT_END_BANG:
          if (b === 0x3e) this.state = State.DATA;
          else if (b === 0x2d) this.state = State.COMMENT_END_1;
          else this.state = State.COMMENT;
          break;

        case State.BOGUS_COMMENT:
          if (b === 0x3e) this.state = State.DATA;
          break;

        // HTML5 §13.2.5.8: tag name terminates ONLY on whitespace, '/', or '>'.
        case State.TAG_NAME:
          if (isWhitespace(b)) {
            this.state = State.BEFORE_ATTR_NAME;
          } else if (b === 0x2f /* '/' */) {
            this.state = State.SELF_CLOSING_START_TAG;
          } else if (b === 0x3e /* '>' */) {
            const found = this.onTagClosed(absPos);
            if (found >= 0) return found;
          } else {
            if (this.tagName.length < 32) {
              this.tagName += String.fromCharCode(asciiLower(b));
            }
          }
          break;

        case State.SELF_CLOSING_START_TAG:
          if (b === 0x3e /* '>' */) {
            this.selfClosing = true;
            const found = this.onTagClosed(absPos);
            if (found >= 0) return found;
          } else {
            // Re-consume in BEFORE_ATTR_NAME
            this.state = State.BEFORE_ATTR_NAME;
            i--;
          }
          break;

        case State.BEFORE_ATTR_NAME:
          if (isWhitespace(b)) {
            // skip
          } else if (b === 0x2f /* '/' */) {
            this.state = State.SELF_CLOSING_START_TAG;
          } else if (b === 0x3e /* '>' */) {
            const found = this.onTagClosed(absPos);
            if (found >= 0) return found;
          } else {
            this.curAttrName = String.fromCharCode(asciiLower(b));
            this.curAttrVal = '';
            this.state = State.ATTR_NAME;
          }
          break;

        case State.ATTR_NAME:
          if (isWhitespace(b)) {
            this.state = State.AFTER_ATTR_NAME;
          } else if (b === 0x2f /* '/' */) {
            this.commitAttr();
            this.state = State.SELF_CLOSING_START_TAG;
          } else if (b === 0x3d /* '=' */) {
            this.state = State.BEFORE_ATTR_VALUE;
          } else if (b === 0x3e /* '>' */) {
            this.commitAttr();
            const found = this.onTagClosed(absPos);
            if (found >= 0) return found;
          } else {
            if (this.curAttrName.length < 32) {
              this.curAttrName += String.fromCharCode(asciiLower(b));
            }
          }
          break;

        case State.AFTER_ATTR_NAME:
          if (isWhitespace(b)) {
            // skip
          } else if (b === 0x2f /* '/' */) {
            this.commitAttr();
            this.state = State.SELF_CLOSING_START_TAG;
          } else if (b === 0x3d /* '=' */) {
            this.state = State.BEFORE_ATTR_VALUE;
          } else if (b === 0x3e /* '>' */) {
            this.commitAttr();
            const found = this.onTagClosed(absPos);
            if (found >= 0) return found;
          } else {
            this.commitAttr();
            this.curAttrName = String.fromCharCode(asciiLower(b));
            this.curAttrVal = '';
            this.state = State.ATTR_NAME;
          }
          break;

        case State.BEFORE_ATTR_VALUE:
          if (isWhitespace(b)) {
            // skip
          } else if (b === 0x22 /* '"' */) {
            this.state = State.ATTR_VAL_DQ;
          } else if (b === 0x27 /* "'" */) {
            this.state = State.ATTR_VAL_SQ;
          } else if (b === 0x3e /* '>' */) {
            this.commitAttr();
            const found = this.onTagClosed(absPos);
            if (found >= 0) return found;
          } else {
            if (this.curAttrVal.length < 64) {
              this.curAttrVal += String.fromCharCode(b);
            }
            this.state = State.ATTR_VAL_UNQUOTED;
          }
          break;

        case State.ATTR_VAL_DQ:
          if (b === 0x22 /* '"' */) {
            this.commitAttr();
            this.state = State.BEFORE_ATTR_NAME;
          } else if (this.curAttrVal.length < 64) {
            this.curAttrVal += String.fromCharCode(b);
          }
          break;

        case State.ATTR_VAL_SQ:
          if (b === 0x27 /* "'" */) {
            this.commitAttr();
            this.state = State.BEFORE_ATTR_NAME;
          } else if (this.curAttrVal.length < 64) {
            this.curAttrVal += String.fromCharCode(b);
          }
          break;

        case State.ATTR_VAL_UNQUOTED:
          if (isWhitespace(b)) {
            this.commitAttr();
            this.state = State.BEFORE_ATTR_NAME;
          } else if (b === 0x3e /* '>' */) {
            this.commitAttr();
            const found = this.onTagClosed(absPos);
            if (found >= 0) return found;
          } else if (this.curAttrVal.length < 64) {
            this.curAttrVal += String.fromCharCode(b);
          }
          break;

        // Rawtext end-tag parser (supports quoted attributes inside end tags like `</style a=">">`!)
        case State.RAWTEXT: {
          const target = this.rawTextEndTag;
          if (this.rawTextMatchPos < target.length) {
            if (asciiLower(b) === target.charCodeAt(this.rawTextMatchPos)) {
              this.rawTextMatchPos++;
            } else {
              this.rawTextMatchPos = b === 0x3c ? 1 : 0;
            }
          } else {
            if (b === 0x3e /* '>' */) {
              this.state = State.DATA;
              this.rawTextMatchPos = 0;
            } else if (isWhitespace(b) || b === 0x2f) {
              this.resetTagState(absPos);
              this.isEndTag = true;
              this.state = b === 0x2f ? State.SELF_CLOSING_START_TAG : State.BEFORE_ATTR_NAME;
              this.rawTextMatchPos = 0;
            } else {
              this.rawTextMatchPos = b === 0x3c ? 1 : 0;
            }
          }
          break;
        }

        // HTML5 §13.2.5.17-29: `<script>` data, escaped (`<!--`), and double-escaped (`<!-- <script>`) states!
        case State.SCRIPT_DATA:
          if (b === 0x3c /* '<' */) this.state = State.SCRIPT_LT;
          break;

        case State.SCRIPT_LT:
          if (b === 0x2f /* '/' */) {
            this.scriptMatchPos = 0;
            this.state = State.SCRIPT_END_TAG;
          } else if (b === 0x21 /* '!' */) {
            this.state = State.SCRIPT_ESC_START;
          } else if (b === 0x3c) {
            this.state = State.SCRIPT_LT;
          } else {
            this.state = State.SCRIPT_DATA;
          }
          break;

        case State.SCRIPT_END_TAG: {
          const word = 'script';
          if (this.scriptMatchPos < word.length) {
            if (asciiLower(b) === word.charCodeAt(this.scriptMatchPos)) {
              this.scriptMatchPos++;
            } else {
              this.state = b === 0x3c ? State.SCRIPT_LT : State.SCRIPT_DATA;
            }
          } else {
            if (b === 0x3e) {
              this.state = State.DATA;
            } else if (isWhitespace(b) || b === 0x2f) {
              this.resetTagState(absPos);
              this.isEndTag = true;
              this.state = b === 0x2f ? State.SELF_CLOSING_START_TAG : State.BEFORE_ATTR_NAME;
            } else {
              this.state = b === 0x3c ? State.SCRIPT_LT : State.SCRIPT_DATA;
            }
          }
          break;
        }

        case State.SCRIPT_ESC_START:
          if (b === 0x2d /* '-' */) this.state = State.SCRIPT_ESC_START_DASH;
          else this.state = b === 0x3c ? State.SCRIPT_LT : State.SCRIPT_DATA;
          break;

        case State.SCRIPT_ESC_START_DASH:
          if (b === 0x2d /* '-' */) this.state = State.SCRIPT_ESCAPED_DASH_DASH;
          else this.state = b === 0x3c ? State.SCRIPT_LT : State.SCRIPT_DATA;
          break;

        case State.SCRIPT_ESCAPED:
          if (b === 0x2d) this.state = State.SCRIPT_ESCAPED_DASH;
          else if (b === 0x3c) this.state = State.SCRIPT_ESCAPED_LT;
          break;

        case State.SCRIPT_ESCAPED_DASH:
          if (b === 0x2d) this.state = State.SCRIPT_ESCAPED_DASH_DASH;
          else if (b === 0x3c) this.state = State.SCRIPT_ESCAPED_LT;
          else this.state = State.SCRIPT_ESCAPED;
          break;

        case State.SCRIPT_ESCAPED_DASH_DASH:
          if (b === 0x2d) this.state = State.SCRIPT_ESCAPED_DASH_DASH;
          else if (b === 0x3e /* '>' */) this.state = State.SCRIPT_DATA;
          else if (b === 0x3c) this.state = State.SCRIPT_ESCAPED_LT;
          else this.state = State.SCRIPT_ESCAPED;
          break;

        case State.SCRIPT_ESCAPED_LT:
          if (b === 0x2f /* '/' */) {
            this.scriptMatchPos = 0;
            this.state = State.SCRIPT_ESCAPED_END_TAG;
          } else if (asciiLower(b) === 0x73 /* 's' */) {
            this.scriptMatchPos = 1;
            this.state = State.SCRIPT_DBL_ESC_START;
          } else if (b === 0x2d) {
            this.state = State.SCRIPT_ESCAPED_DASH;
          } else if (b === 0x3c) {
            this.state = State.SCRIPT_ESCAPED_LT;
          } else {
            this.state = State.SCRIPT_ESCAPED;
          }
          break;

        case State.SCRIPT_ESCAPED_END_TAG: {
          const word = 'script';
          if (this.scriptMatchPos < word.length) {
            if (asciiLower(b) === word.charCodeAt(this.scriptMatchPos)) {
              this.scriptMatchPos++;
            } else {
              this.state = b === 0x3c ? State.SCRIPT_ESCAPED_LT : b === 0x2d ? State.SCRIPT_ESCAPED_DASH : State.SCRIPT_ESCAPED;
            }
          } else {
            if (b === 0x3e) {
              this.state = State.DATA;
            } else if (isWhitespace(b) || b === 0x2f) {
              this.resetTagState(absPos);
              this.isEndTag = true;
              this.state = b === 0x2f ? State.SELF_CLOSING_START_TAG : State.BEFORE_ATTR_NAME;
            } else {
              this.state = b === 0x3c ? State.SCRIPT_ESCAPED_LT : State.SCRIPT_ESCAPED;
            }
          }
          break;
        }

        case State.SCRIPT_DBL_ESC_START: {
          const word = 'script';
          if (this.scriptMatchPos < word.length) {
            if (asciiLower(b) === word.charCodeAt(this.scriptMatchPos)) {
              this.scriptMatchPos++;
            } else {
              this.state = b === 0x3c ? State.SCRIPT_ESCAPED_LT : b === 0x2d ? State.SCRIPT_ESCAPED_DASH : State.SCRIPT_ESCAPED;
            }
          } else {
            if (isWhitespace(b) || b === 0x2f || b === 0x3e) {
              this.state = State.SCRIPT_DBL_ESCAPED;
            } else {
              this.state = b === 0x3c ? State.SCRIPT_ESCAPED_LT : State.SCRIPT_ESCAPED;
            }
          }
          break;
        }

        case State.SCRIPT_DBL_ESCAPED:
          if (b === 0x2d) this.state = State.SCRIPT_DBL_ESCAPED_DASH;
          else if (b === 0x3c) this.state = State.SCRIPT_DBL_ESCAPED_LT;
          break;

        case State.SCRIPT_DBL_ESCAPED_DASH:
          if (b === 0x2d) this.state = State.SCRIPT_DBL_ESCAPED_DASH_DASH;
          else if (b === 0x3c) this.state = State.SCRIPT_DBL_ESCAPED_LT;
          else this.state = State.SCRIPT_DBL_ESCAPED;
          break;

        case State.SCRIPT_DBL_ESCAPED_DASH_DASH:
          if (b === 0x2d) this.state = State.SCRIPT_DBL_ESCAPED_DASH_DASH;
          else if (b === 0x3e /* '>' */) this.state = State.SCRIPT_DATA;
          else if (b === 0x3c) this.state = State.SCRIPT_DBL_ESCAPED_LT;
          else this.state = State.SCRIPT_DBL_ESCAPED;
          break;

        case State.SCRIPT_DBL_ESCAPED_LT:
          if (b === 0x2f /* '/' */) {
            this.scriptMatchPos = 0;
            this.state = State.SCRIPT_DBL_ESC_END;
          } else if (b === 0x2d) {
            this.state = State.SCRIPT_DBL_ESCAPED_DASH;
          } else if (b === 0x3c) {
            this.state = State.SCRIPT_DBL_ESCAPED_LT;
          } else {
            this.state = State.SCRIPT_DBL_ESCAPED;
          }
          break;

        case State.SCRIPT_DBL_ESC_END: {
          const word = 'script';
          if (this.scriptMatchPos < word.length) {
            if (asciiLower(b) === word.charCodeAt(this.scriptMatchPos)) {
              this.scriptMatchPos++;
            } else {
              this.state = b === 0x3c ? State.SCRIPT_DBL_ESCAPED_LT : b === 0x2d ? State.SCRIPT_DBL_ESCAPED_DASH : State.SCRIPT_DBL_ESCAPED;
            }
          } else {
            if (isWhitespace(b) || b === 0x2f || b === 0x3e) {
              // Exits double-escaped back to SCRIPT_ESCAPED (outer <script> stays open!).
              this.state = State.SCRIPT_ESCAPED;
            } else {
              this.state = b === 0x3c ? State.SCRIPT_DBL_ESCAPED_LT : State.SCRIPT_DBL_ESCAPED;
            }
          }
          break;
        }
      }
    }
    return -1;
  }

  private onTagClosed(closeBracketGlobalOffset: number): number {
    this.state = State.DATA;
    const tag = this.tagName;
    if (!tag) return -1;

    // Handle nested `<template>`, `<svg>`, `<math>` subtrees (preserving depth & foreign self-closing!).
    if (this.skipSubtreeDepth > 0) {
      if (this.isEndTag) {
        if (tag === this.skipSubtreeTag) {
          this.skipSubtreeDepth--;
          if (this.skipSubtreeDepth === 0) this.skipSubtreeTag = '';
        }
      } else {
        if (tag === this.skipSubtreeTag) {
          if (!(this.selfClosing && (tag === 'svg' || tag === 'math'))) {
            this.skipSubtreeDepth++;
          }
        } else if (tag === 'script') {
          this.state = State.SCRIPT_DATA;
        } else if (RAWTEXT_TAGS.has(tag)) {
          this.rawTextEndTag = `</${tag}`;
          this.rawTextMatchPos = 0;
          this.state = State.RAWTEXT;
        }
      }
      return -1;
    }

    // Active `<head>` 1024-byte `<meta charset>` prescan window (only when `!hasHttpCharset`).
    if (this.inHeadPrescan) {
      if (!this.isEndTag && tag === 'meta' && this.metaIsCharset) {
        return closeBracketGlobalOffset + 1;
      }
      if (
        (!this.isEndTag && (tag === 'script' || tag === 'body')) ||
        (this.isEndTag && tag === 'head')
      ) {
        return this.tagOpenGlobalOffset >= this.headCloseEndGlobalOffset
          ? this.tagOpenGlobalOffset
          : this.headCloseEndGlobalOffset;
      }
      if (!this.isEndTag && tag === 'style') {
        this.rawTextEndTag = '</style';
        this.rawTextMatchPos = 0;
        this.state = State.RAWTEXT;
      } else if (!this.isEndTag && RAWTEXT_TAGS.has(tag)) {
        this.rawTextEndTag = `</${tag}`;
        this.rawTextMatchPos = 0;
        this.state = State.RAWTEXT;
      }
      return -1;
    }

    if (this.isEndTag) return -1;

    if (tag === 'head') {
      const afterHead = closeBracketGlobalOffset + 1;
      if (this.hasHttpCharset || afterHead >= this.prescanLimitGlobalOffset) {
        return afterHead;
      }
      this.inHeadPrescan = true;
      this.headCloseEndGlobalOffset = afterHead;
      return -1;
    }

    if (tag === 'html') {
      this.fallbackInsertGlobalOffset = closeBracketGlobalOffset + 1;
      return -1;
    }

    if (NESTED_SKIP_TAGS.has(tag)) {
      // `<svg />` and `<math />` are self-closing in HTML5 foreign content; `<template />` is NOT.
      if (!(this.selfClosing && (tag === 'svg' || tag === 'math'))) {
        this.skipSubtreeTag = tag;
        this.skipSubtreeDepth = 1;
      }
      return -1;
    }

    if (tag === 'script') {
      // Record first `<script>` offset for headless documents (no `<head>`), and enter HTML5 SCRIPT_DATA
      // so `<script>` content (including `<!-- <script></script> -->`) is tokenized accurately!
      if (this.firstScriptGlobalOffset < 0) {
        this.firstScriptGlobalOffset = this.tagOpenGlobalOffset;
      }
      this.state = State.SCRIPT_DATA;
      return -1;
    }

    if (RAWTEXT_TAGS.has(tag)) {
      this.rawTextEndTag = `</${tag}`;
      this.rawTextMatchPos = 0;
      this.state = State.RAWTEXT;
      return -1;
    }

    if (tag === 'body') {
      return closeBracketGlobalOffset + 1;
    }

    return -1;
  }
}

export function createHtmlInjectorTransform(
  sandboxOrigin: string,
  cfg: PatchInjectConfig,
  opts: HtmlInjectorOptions = {}
): TransformStream<Uint8Array, Uint8Array> {
  const injector = new StreamingHtmlInjector(sandboxOrigin, cfg, opts);
  return new TransformStream<Uint8Array, Uint8Array>({
    transform(chunk, controller) {
      for (const out of injector.push(chunk)) {
        controller.enqueue(out);
      }
    },
    flush(controller) {
      for (const out of injector.finish()) {
        controller.enqueue(out);
      }
    },
  });
}

/**
 * Injects Web Worker bootstrap URL rewrite helper while preserving leading `'use strict';`
 * and UTF-8 BOM. Supports BOTH Classic Workers (`importScripts`) AND ES Module Workers
 * (inline `globalThis.fetch` rewrite when `importScripts` is unavailable).
 */
export function injectWorkerBootstrap(
  source: string,
  sandboxOrigin: string,
  cfg: Pick<PatchInjectConfig, 'backendOrigin'>
): string {
  const safeOrigin = sandboxOrigin.replace(/\/+$/, '');
  const backendOriginJson = JSON.stringify(cfg.backendOrigin || '');
  const safeOriginJson = JSON.stringify(safeOrigin);
  const patchUrlJson = JSON.stringify(`${safeOrigin}${resolvePatchAssetUrl()}`);

  const preamble =
    `globalThis.__P2P_GW_WORKER_CFG__={currentOrigin:${safeOriginJson},backendOrigin:${backendOriginJson}};\n` +
    `if(typeof importScripts==='function'){try{importScripts(${patchUrlJson});}catch(_){}}` +
    `else if(typeof globalThis.fetch==='function'&&!globalThis.__P2P_GW_ESM_PATCH__){` +
    `globalThis.__P2P_GW_ESM_PATCH__=true;` +
    `const __oF=globalThis.fetch.bind(globalThis),__tO=${backendOriginJson};` +
    `globalThis.fetch=function(u,i){try{const r=u instanceof Request?u.url:String(u),p=new URL(r,location.href);` +
    `if(__tO&&p.origin===__tO){const n=p.pathname+p.search+p.hash;return u instanceof Request?__oF(new Request(n,u),i):__oF(n,i);}}catch(_){}return __oF(u,i);};}\n`;

  const strictRe = /^(\uFEFF?(?:\s|\/\/[^\n]*\n|\/\*[\s\S]*?\*\/)*['"]use strict['"]\s*;?\s*)/;
  const match = strictRe.exec(source);
  if (match) {
    return `${match[1]}\n${preamble}${source.slice(match[1].length)}`;
  }
  return `${preamble}${source}`;
}
