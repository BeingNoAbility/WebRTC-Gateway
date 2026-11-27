/**
 * V7 Portal Internationalization (ZH / EN)
 * - Uses safe `replaceAll` (immune to RegExp `$&` injection).
 * - Deduplicated listener subscription with clean unsubscribe.
 */

import { loadSavedLang, saveLang } from './core/config';

export type Lang = 'zh' | 'en';

const DICT: Record<Lang, Record<string, string>> = {
  zh: {
    'app.title': 'WebRTC Gateway',
    'landing.subtitle': '零客户端端到端加密内网访问网关',
    'landing.nodePlaceholder': '输入目标节点 ID（如 my-nas）',
    'landing.connect': '连接节点',
    'landing.invalidNode': '节点 ID 无效（仅允许小写字母、数字、点或连字符，且不能以 sbx- 开头）',
    'status.idle': '未连接',
    'status.connecting': '正在连接…',
    'status.waiting': '等待 Agent 上线…',
    'status.authenticating': '正在进行 DTLS 通道认证…',
    'status.auth_required': '需要访问密钥',
    'status.connected': '已加密直连',
    'status.disconnected': '连接已断开',
    'status.error': '连接异常',
    'banner.sameSite': '同站降级模式（仅限完全互信的内网应用，不提供跨节点 Cookie 隔离）',
    'pill.share': '分享链接',
    'pill.settings': '网络设置',
    'pill.reconnect': '重新连接',
    'pill.resetSandbox': '重置节点沙箱',
    'pill.clearLogs': '清空日志',
    'pill.keyVerified': 'Ed25519 公钥已校验',
    'auth.title': '节点访问认证',
    'auth.desc': '请输入节点 {node} 的访问密钥（仅在 DTLS 加密通道内进行挑战-响应校验，不会明文传输）：',
    'auth.placeholder': '输入访问密钥',
    'auth.submit': '验证并连接',
    'auth.cancel': '取消',
    'auth.rateLimited': '验证失败次数过多，请等待 {sec} 秒后重试',
    'tofu.title': '警告：Agent 身份公钥发生变化！',
    'tofu.pinnedDesc': '链接中钉扎的公钥指纹与远端 Agent 不匹配，连接已强制阻断。请勿继续访问，可能存在信令中间人攻击。',
    'tofu.tofuDesc': '此节点上次记录的 Ed25519 公钥指纹与当前收到的指纹不一致！若非你主动重装或轮换了 Agent 密钥，请立即停止访问。',
    'tofu.expected': '预期指纹',
    'tofu.actual': '当前指纹',
    'tofu.resetBtn': '确认是我本人轮换了密钥，重置信任并重连',
    'tofu.closeBtn': '断开并返回',
    'share.title': '签发能力分享链接（Ed25519）',
    'share.desc': '由 Agent 直接在 DTLS 通道内签发短期受限能力令牌，无需重新输入密码，且可随时吊销。',
    'share.ttl': '有效期',
    'share.pathPrefix': '允许路径前缀',
    'share.readonly': '仅只读（仅允许 GET / HEAD）',
    'share.maxConns': '最大并发连接数',
    'share.mintBtn': '生成分享链接',
    'share.copyBtn': '复制链接',
    'share.copied': '已复制到剪贴板',
    'share.activeList': '已签发令牌列表',
    'share.revoke': '吊销',
    'share.revoked': '已吊销',
    'share.empty': '暂无已签发的分享令牌',
    'settings.title': 'WebRTC 网络与 ICE 设置',
    'settings.stun': '自定义 STUN（逗号分隔，留空使用内置精简默认）',
    'settings.turnUrl': 'TURN URI（RFC 7065，如 turn:turn.example.com:3478?transport=udp）',
    'settings.turnUser': 'TURN 用户名',
    'settings.turnPass': 'TURN 密码（仅存内存，刷新即清）',
    'settings.relayOnly': '强制仅使用 TURN 中继（iceTransportPolicy: relay）',
    'settings.relayNeedsTurn': '启用仅 TURN 中继模式前必须填写有效的 TURN URI',
    'settings.lang': '界面语言',
    'settings.save': '保存并应用',
    'settings.close': '关闭',
  },
  en: {
    'app.title': 'WebRTC Gateway',
    'landing.subtitle': 'Zero-Client End-to-End Encrypted Intranet Gateway',
    'landing.nodePlaceholder': 'Enter target Node ID (e.g. my-nas)',
    'landing.connect': 'Connect',
    'landing.invalidNode': 'Invalid Node ID (lowercase letters, digits, dots or hyphens; cannot start with sbx-)',
    'status.idle': 'Idle',
    'status.connecting': 'Connecting...',
    'status.waiting': 'Waiting for Agent...',
    'status.authenticating': 'Authenticating inside DTLS...',
    'status.auth_required': 'Access Secret Required',
    'status.connected': 'Connected (E2E)',
    'status.disconnected': 'Disconnected',
    'status.error': 'Error',
    'banner.sameSite': 'Same-Site Mode (trusted apps only — cross-node cookie isolation is disabled)',
    'pill.share': 'Share Link',
    'pill.settings': 'ICE Settings',
    'pill.reconnect': 'Reconnect',
    'pill.resetSandbox': 'Reset Sandbox',
    'pill.clearLogs': 'Clear',
    'pill.keyVerified': 'Ed25519 Verified',
    'auth.title': 'Node Access Authentication',
    'auth.desc': 'Enter the access secret for node {node} (verified via in-DTLS challenge-response; never sent in plaintext):',
    'auth.placeholder': 'Access secret',
    'auth.submit': 'Verify & Connect',
    'auth.cancel': 'Cancel',
    'auth.rateLimited': 'Too many failed attempts. Please retry in {sec}s.',
    'tofu.title': 'WARNING: Agent Identity Public Key Changed!',
    'tofu.pinnedDesc': 'The pinned public key hash in your link does not match the remote Agent. Connection blocked to prevent MITM.',
    'tofu.tofuDesc': 'The Ed25519 key fingerprint for this node differs from the previously trusted key! Unless you intentionally rotated the Agent key, stop immediately.',
    'tofu.expected': 'Expected Fingerprint',
    'tofu.actual': 'Received Fingerprint',
    'tofu.resetBtn': 'I intentionally rotated the Agent key — Reset TOFU & Reconnect',
    'tofu.closeBtn': 'Disconnect & Return',
    'share.title': 'Mint Scoped Capability Link (Ed25519)',
    'share.desc': 'Minted directly by the Agent over DTLS without re-entering your password, with instant revocation.',
    'share.ttl': 'Expires In',
    'share.pathPrefix': 'Allowed Path Prefix',
    'share.readonly': 'Read-Only (GET / HEAD only)',
    'share.maxConns': 'Max Concurrent Connections',
    'share.mintBtn': 'Generate Share Link',
    'share.copyBtn': 'Copy Link',
    'share.copied': 'Copied to clipboard',
    'share.activeList': 'Issued Capability Tokens',
    'share.revoke': 'Revoke',
    'share.revoked': 'Revoked',
    'share.empty': 'No active capability tokens',
    'settings.title': 'WebRTC & ICE Settings',
    'settings.stun': 'Custom STUN (comma-separated; leave blank for minimal defaults)',
    'settings.turnUrl': 'TURN URI (RFC 7065, e.g. turn:turn.example.com:3478?transport=udp)',
    'settings.turnUser': 'TURN Username',
    'settings.turnPass': 'TURN Password (in-memory only, never persisted)',
    'settings.relayOnly': 'Force TURN Relay Only (iceTransportPolicy: relay)',
    'settings.relayNeedsTurn': 'A valid TURN URI is required when TURN Relay Only mode is enabled',
    'settings.lang': 'Language',
    'settings.save': 'Save & Apply',
    'settings.close': 'Close',
  },
};

let currentLang: Lang = loadSavedLang();
const listeners = new Set<(lang: Lang) => void>();

export function getLang(): Lang {
  return currentLang;
}

export function setLang(lang: Lang): void {
  if (lang === currentLang) return;
  currentLang = lang;
  saveLang(lang);
  for (const fn of listeners) {
    try {
      fn(lang);
    } catch {
      // ignore
    }
  }
}

export function subscribeLang(fn: (lang: Lang) => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

export function t(key: string, params?: Record<string, string | number>): string {
  const table = DICT[currentLang] || DICT.en;
  let str = table[key] ?? DICT.en[key] ?? key;
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      str = str.replaceAll(`{${k}}`, String(v));
    }
  }
  return str;
}
