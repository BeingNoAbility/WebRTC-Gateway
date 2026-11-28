/**
 * Streaming Byte-Level HTML5 Head Tokenizer & Safe Injector
 * - Emits ONLY two compact synchronous external `<script>` tags (~140B total, zero 22 KiB inline
 *   `data-p2p-boot` payload): `/__p2p_gw__/boot.js` (SW-synthesized per-session config) +
 *   immutable hashed `/__p2p_gw__/assets/patch.<hash>.js`.
 * - Propagates upstream CSP `nonce` onto injected `<script>` tags when present.
 * - Injects strictly BEFORE the first executable `<script>` and respects 1024-byte `<meta charset>`
 *   prescan, `<template>` / `<svg>` / `<math>` skip stacks, `<![CDATA[...]]>`, and UTF-16 BOMs.
 */

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
  /** Optional upstream CSP script nonce (e.g. from `script-src 'nonce-XYZ'`). */
  scriptNonce?: string;
}

const textEncoder = new TextEncoder();
const MAX_BOOT_JSON_BYTES = 16 * 1024;
const CHARSET_PRESCAN_LIMIT_BYTES = 1024;
const MAX_SCAN_BUFFER_BYTES = 256 * 1024;
const SAFE_NONCE_RE = /^[A-Za-z0-9+/=_-]{1,128}$/;

export const GATEWAY_BOOT_SCRIPT_PATH = '/__p2p_gw__/boot.js';

export function resolvePatchAssetUrl(): string {
  const injected = (globalThis as { __P2P_GW_PATCH_URL__?: unknown }).__P2P_GW_PATCH_URL__;
  if (typeof injected === 'string' && injected.startsWith('/__p2p_gw__/')) {
    return injected;
  }
  return '/__p2p_gw__/assets/patch.js';
}

export function pruneAndBoundBootConfig(cfg: PatchInjectConfig, now = Date.now()): PatchInjectConfig {
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

/**
 * Generates the Service-Worker-served `/__p2p_gw__/boot.js` script body containing
 * bounded per-session bootstrap config (zero inline HTML attributes).
 */
export function buildBootScriptPayload(cfg: PatchInjectConfig): string {
  const bounded = pruneAndBoundBootConfig(cfg);
  const safeJson = JSON.stringify(bounded)
    .replace(/</g, '\\u003c')
    .replace(/\u2028/g, '\\u2028')
    .replace(/\u2029/g, '\\u2029');
  return `globalThis.__P2P_GW_BOOT__=${safeJson};\n`;
}

export function buildPatchInjectionSnippet(
  sandboxOrigin: string,
  opts: { scriptNonce?: string } = {}
): string {
  const safeOrigin = sandboxOrigin.replace(/\/+$/, '');
  const nonceAttr =
    opts.scriptNonce && SAFE_NONCE_RE.test(opts.scriptNonce)
      ? ` nonce="${opts.scriptNonce}"`
      : '';
  return (
    `<script src="${safeOrigin}${GATEWAY_BOOT_SCRIPT_PATH}"${nonceAttr}></script>` +
    `<script src="${safeOrigin}${resolvePatchAssetUrl()}"${nonceAttr}></script>`
  );
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
  CDATA = 22,
  CDATA_BRACKET_1 = 23,
  CDATA_BRACKET_2 = 24,
  RAWTEXT = 25,
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

  private rawTextEndTag = '';
  private rawTextMatchPos = 0;

  private readonly skipStack: string[] = [];

  private inHeadPrescan = false;
  private headCloseEndGlobalOffset = 0;
  private prescanLimitGlobalOffset = CHARSET_PRESCAN_LIMIT_BYTES;
  private curAttrName = '';
  private curAttrVal = '';
  private metaIsCharset = false;

  private pendingChunks: Uint8Array[] = [];
  private pendingBytes = 0;
  private fallbackInsertGlobalOffset = 0;

  constructor(sandboxOrigin: string, _cfgOrOpts?: PatchInjectConfig | HtmlInjectorOptions, opts: HtmlInjectorOptions = {}) {
    const resolvedOpts: HtmlInjectorOptions =
      _cfgOrOpts && !('nodeId' in _cfgOrOpts) ? _cfgOrOpts : opts;
    this.snippetBytes = textEncoder.encode(
      buildPatchInjectionSnippet(sandboxOrigin, { scriptNonce: resolvedOpts.scriptNonce })
    );
    this.hasHttpCharset = Boolean(resolvedOpts.hasHttpCharset);
  }

  private inForeignContent(): boolean {
    if (this.skipStack.length === 0) return false;
    const top = this.skipStack[this.skipStack.length - 1];
    return top === 'svg' || top === 'math';
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
    if (
      this.inHeadPrescan &&
      this.skipStack.length === 0 &&
      !this.isEndTag &&
      this.tagName === 'meta'
    ) {
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

      if (this.inHeadPrescan && absPos >= this.prescanLimitGlobalOffset) {
        return this.headCloseEndGlobalOffset;
      }

      switch (this.state) {
        case State.DATA:
          if (b === 0x3c) {
            this.resetTagState(absPos);
            this.state = State.TAG_OPEN;
          }
          break;

        case State.TAG_OPEN:
          if (b === 0x21) {
            this.state = State.BANG;
            this.bangBuf = '';
          } else if (b === 0x3f) {
            this.state = State.BOGUS_COMMENT;
          } else if (b === 0x2f) {
            this.state = State.END_TAG_OPEN;
          } else if (isAsciiAlpha(b)) {
            this.tagName = String.fromCharCode(asciiLower(b));
            this.state = State.TAG_NAME;
          } else if (b === 0x3c) {
            this.resetTagState(absPos);
            this.state = State.TAG_OPEN;
          } else {
            this.state = State.DATA;
          }
          break;

        case State.END_TAG_OPEN:
          if (isAsciiAlpha(b)) {
            this.isEndTag = true;
            this.tagName = String.fromCharCode(asciiLower(b));
            this.state = State.TAG_NAME;
          } else if (b === 0x3e) {
            this.state = State.DATA;
          } else {
            this.state = State.BOGUS_COMMENT;
          }
          break;

        case State.BANG:
          if (b === 0x2d && this.bangBuf === '') {
            this.state = State.COMMENT_START_1;
          } else if (b === 0x3e) {
            if (this.bangBuf.toLowerCase().startsWith('doctype')) {
              this.fallbackInsertGlobalOffset = absPos + 1;
            }
            this.state = State.DATA;
          } else {
            if (this.bangBuf.length < 8) {
              this.bangBuf += String.fromCharCode(asciiLower(b));
              if (this.bangBuf === 'doctype') {
                this.state = State.DOCTYPE;
              } else if (this.bangBuf === '[cdata[' && this.inForeignContent()) {
                this.state = State.CDATA;
              }
            } else {
              this.state = State.BOGUS_COMMENT;
            }
          }
          break;

        case State.DOCTYPE:
          if (b === 0x3e) {
            this.fallbackInsertGlobalOffset = absPos + 1;
            this.state = State.DATA;
          }
          break;

        case State.CDATA:
          if (b === 0x5d) this.state = State.CDATA_BRACKET_1;
          break;

        case State.CDATA_BRACKET_1:
          if (b === 0x5d) this.state = State.CDATA_BRACKET_2;
          else this.state = State.CDATA;
          break;

        case State.CDATA_BRACKET_2:
          if (b === 0x3e) this.state = State.DATA;
          else if (b === 0x5d) this.state = State.CDATA_BRACKET_2;
          else this.state = State.CDATA;
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

        case State.TAG_NAME:
          if (isWhitespace(b)) {
            this.state = State.BEFORE_ATTR_NAME;
          } else if (b === 0x2f) {
            this.state = State.SELF_CLOSING_START_TAG;
          } else if (b === 0x3e) {
            const found = this.onTagClosed(absPos);
            if (found >= 0) return found;
          } else if (this.tagName.length < 32) {
            this.tagName += String.fromCharCode(asciiLower(b));
          }
          break;

        case State.SELF_CLOSING_START_TAG:
          if (b === 0x3e) {
            this.selfClosing = true;
            const found = this.onTagClosed(absPos);
            if (found >= 0) return found;
          } else {
            this.state = State.BEFORE_ATTR_NAME;
            i--;
          }
          break;

        case State.BEFORE_ATTR_NAME:
          if (isWhitespace(b)) {
            // skip
          } else if (b === 0x2f) {
            this.state = State.SELF_CLOSING_START_TAG;
          } else if (b === 0x3e) {
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
          } else if (b === 0x2f) {
            this.commitAttr();
            this.state = State.SELF_CLOSING_START_TAG;
          } else if (b === 0x3d) {
            this.state = State.BEFORE_ATTR_VALUE;
          } else if (b === 0x3e) {
            this.commitAttr();
            const found = this.onTagClosed(absPos);
            if (found >= 0) return found;
          } else if (this.curAttrName.length < 32) {
            this.curAttrName += String.fromCharCode(asciiLower(b));
          }
          break;

        case State.AFTER_ATTR_NAME:
          if (isWhitespace(b)) {
            // skip
          } else if (b === 0x2f) {
            this.commitAttr();
            this.state = State.SELF_CLOSING_START_TAG;
          } else if (b === 0x3d) {
            this.state = State.BEFORE_ATTR_VALUE;
          } else if (b === 0x3e) {
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
          } else if (b === 0x22) {
            this.state = State.ATTR_VAL_DQ;
          } else if (b === 0x27) {
            this.state = State.ATTR_VAL_SQ;
          } else if (b === 0x3e) {
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
          if (b === 0x22) {
            this.commitAttr();
            this.state = State.BEFORE_ATTR_NAME;
          } else if (this.curAttrVal.length < 64) {
            this.curAttrVal += String.fromCharCode(b);
          }
          break;

        case State.ATTR_VAL_SQ:
          if (b === 0x27) {
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
          } else if (b === 0x3e) {
            this.commitAttr();
            const found = this.onTagClosed(absPos);
            if (found >= 0) return found;
          } else if (this.curAttrVal.length < 64) {
            this.curAttrVal += String.fromCharCode(b);
          }
          break;

        case State.RAWTEXT: {
          const target = this.rawTextEndTag;
          if (this.rawTextMatchPos < target.length) {
            if (asciiLower(b) === target.charCodeAt(this.rawTextMatchPos)) {
              this.rawTextMatchPos++;
            } else {
              this.rawTextMatchPos = b === 0x3c ? 1 : 0;
            }
          } else {
            if (b === 0x3e) {
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
      }
    }
    return -1;
  }

  private enterRawtext(tag: string): void {
    this.rawTextEndTag = `</${tag}`;
    this.rawTextMatchPos = 0;
    this.state = State.RAWTEXT;
  }

  private onTagClosed(closeBracketGlobalOffset: number): number {
    this.state = State.DATA;
    const tag = this.tagName;
    if (!tag) return -1;

    if (this.skipStack.length > 0) {
      if (this.isEndTag) {
        if (this.skipStack[this.skipStack.length - 1] === tag) {
          this.skipStack.pop();
        }
        return -1;
      }
      if (NESTED_SKIP_TAGS.has(tag)) {
        if (!(this.selfClosing && (tag === 'svg' || tag === 'math'))) {
          this.skipStack.push(tag);
        }
        return -1;
      }
      if (!this.inForeignContent() && (tag === 'script' || RAWTEXT_TAGS.has(tag))) {
        this.enterRawtext(tag);
      }
      return -1;
    }

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
      if (!this.isEndTag && NESTED_SKIP_TAGS.has(tag)) {
        if (!(this.selfClosing && (tag === 'svg' || tag === 'math'))) {
          this.skipStack.push(tag);
        }
        return -1;
      }
      if (!this.isEndTag && RAWTEXT_TAGS.has(tag)) {
        this.enterRawtext(tag);
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
      if (!(this.selfClosing && (tag === 'svg' || tag === 'math'))) {
        this.skipStack.push(tag);
      }
      return -1;
    }

    if (tag === 'script') {
      return this.tagOpenGlobalOffset;
    }

    if (RAWTEXT_TAGS.has(tag)) {
      this.enterRawtext(tag);
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
  opts: HtmlInjectorOptions = {}
): TransformStream<Uint8Array, Uint8Array> {
  const injector = new StreamingHtmlInjector(sandboxOrigin, opts);
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
 * and UTF-8 BOM. Supports BOTH Classic Workers (`importScripts`) AND ES Module Workers.
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
