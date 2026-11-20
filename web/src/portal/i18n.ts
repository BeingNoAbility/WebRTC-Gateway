/**
 * web/src/portal/i18n.ts
 * Zero-dependency, lightweight, type-safe i18n dictionary and DOM binder for P2P WebRTC Gateway.
 * Adheres strictly to docs/REVIEW_GUIDELINES.md:
 * - Bundle size overhead < 2.5KB
 * - Zero external dependencies
 * - In-place live DOM updates (Zero WebRTC / Service Worker session disruption)
 * - W3C standard navigator.language detection with localStorage persistence
 */

export type Locale = 'zh-CN' | 'en-US';

export interface TranslationParams {
  [key: string]: string | number;
}

const STORAGE_KEY = 'p2p_locale';

const DICT_ZH: Record<string, string> = {
  // Header
  header_title: 'P2P WebRTC Gateway (Zero-Client)',
  header_settings: '⚙ 节点与信令设置',
  badge_disconnected: '未连接',
  badge_connecting: '连接中...',
  badge_connected: '已连接',

  // Cards
  card_peer_title: '目标节点 ID',
  card_peer_tip: '点击修改目标节点 ID',
  card_signal_title: '信令服务端',
  card_signal_tip: '点击修改信令服务端',
  card_edit: '✎ 修改',
  card_rtt_title: '往返网络延迟 (RTT)',
  card_streams_title: '多路复用流',
  card_streams_active: '{count} 个活动',

  // Controls
  panel_mvp_title: '连通性快速测试',
  btn_ping: 'Ping 测试',
  btn_echo: '回显流测试',
  test_output_ready: '准备测试多路复用 P2P 数据流。',

  // Logs
  panel_log_title: '实时事件日志',
  btn_clear_log: '清空',

  // HUD / Floating Info
  hud_transport_mode: '传输连接模式',
  hud_direct: '⚡ P2P Direct 直连',
  hud_relay: '🛡️ TURN Relay 加密中继',
  hud_local_candidate: '本地节点候选',
  hud_remote_candidate: '远端穿透目标',
  hud_protocol: '协议',
  hud_ice_state: 'ICE 状态',
  hud_node: '节点',
  hud_rtt: '往返网络延迟 (RTT)',
  hud_downlink: '瞬时下载速率',
  hud_uplink: '瞬时上传速率',
  hud_total_rx: '累计: {size} MB',
  hud_total_tx: '累计: {size} MB',
  hud_packet_loss: '丢包统计',
  hud_btn_ice_restart: '🔄 测试 ICE Restart (切网自愈)',
  hud_btn_settings: '⚙ 连接设置',
  hud_ice_restarting: '🔄 正在触发 WebRTC 原地 ICE Restart 重协商...',
  hud_ice_success: '✅ ICE Restart 重协商成功，数据通道与会话无感保持！',
  hud_ice_failed: '❌ ICE Restart 失败: {error}',

  // Smart Pill
  pill_title: 'WebRTC 状态指示 (点击展开 / 拖拽移动)',
  pill_collapse_btn_title: '收起为小圆点',
  pill_dot_title: 'WebRTC 状态指示 - 点击展开',

  // Settings Modal
  settings_title: '⚙ 节点与信令设置',
  settings_node_label: '目标节点 ID',
  settings_node_desc: '支持二级/三级子域名（如 bili.p2p.example.com）、URL Hash（#nodeId）或手动指定。',
  settings_node_placeholder: '例如: myhome',
  settings_signal_label: '信令服务端地址',
  settings_signal_desc: '设置 WebRTC 握手撮合信令服务器 WebSocket 地址（默认使用当前同源信令）。',
  settings_signal_placeholder: 'wss://... 或留空默认同源信令',
  settings_secret_label: '访问口令 / 密钥 (可选)',
  settings_secret_desc: '若目标节点开启了 -secret 保护，请输入匹配的口令；留空表示公开访问。',
  settings_secret_placeholder: '可选的访问口令 (Token)',
  settings_turn_label: 'TURN 中继穿透 (可选)',
  settings_turn_desc: '在严格对称型 NAT（企业网/校园网/移动网络）打洞失败时无缝降级走中继。',
  settings_turn_url_placeholder: 'turn:turn.domain.com:3478',
  settings_turn_user_placeholder: '用户名',
  settings_turn_pass_placeholder: '密码',
  settings_share_label: '🔗 限时安全分享链接',
  settings_share_desc: '基于 HMAC-SHA256 纯数学时效签名，无需对方输入密钥，到期自动失效。',
  duration_1h: '有效 1 小时',
  duration_6h: '有效 6 小时',
  duration_24h: '有效 24 小时',
  duration_7d: '有效 7 天',
  duration_30d: '有效 30 天',
  btn_gen_share_link: '生成链接',
  btn_copy: '复制',
  tip_copied: '✓ 已复制到剪贴板！可以直接发送给他人访问。',
  alert_enter_secret: '请在上方“访问口令 / 密钥”输入框中输入口令以生成签名分享链接。',
  alert_gen_failed: '生成签名链接失败: {error}',
  btn_toggle_debug: '切换调试视图',
  btn_save_connect: '保存并连接',
  settings_lang_label: '界面语言',

  // Offline Alerts & Modal
  alert_bar_offline: '目标节点 [{node}] 当前离线（点击查看排障指引或切换节点）',
  btn_dismiss_alert: '关闭提示',
  offline_title: '无法连接到目标节点',
  offline_desc: '目标节点 [{node}] 当前离线、未注册到信令服务器，或 WebRTC P2P 握手超时。',
  btn_dismiss_offline: '查看控制台',
  btn_retry_conn: '重试连接',
  btn_switch_node: '切换节点',
  diagnostics_title: '📋 客户端排障与运行环境',

  // Auth Modal
  auth_title: '访问口令鉴权',
  auth_desc: '目标节点 [{node}] 已开启访问保护，请输入访问密钥：',
  auth_input_placeholder: '请输入访问口令 / Secret Token...',
  auth_remember: '记住此节点的访问口令',
  btn_verify_connect: '验证并连接',
  auth_err_invalid: '访问口令不正确或缺失，请重新输入',
  auth_err_expired: '分享链接已失效或签名不匹配，请输入访问口令',

  // Insecure Context
  insecure_title: '当前处于非安全上下文（Insecure Context），已暂停加载目标网页',
  insecure_desc: '根据 W3C Web 标准与现代浏览器安全规范：在非 HTTPS 且非 localhost 的局域网纯 HTTP IP（如 <code>{origin}</code>）下，<strong>Service Worker 将被强制禁用</strong>。本网关依赖 Service Worker 虚拟化拦截和转发请求，请选择以下任一方式使用：',
  insecure_opt1_title: '方案 1（本机访问，最快）',
  insecure_opt1_desc: '若浏览器与信令服务运行在同一台设备，请直接在地址栏打开 <code>http://localhost:{port}/#{node}</code>（localhost 属于标准安全上下文）。',
  insecure_opt2_title: '方案 2（生产推荐，零配置）',
  insecure_opt2_desc: '部署在支持 HTTPS 的域名或免费部署至 Cloudflare Workers（如 <code>https://p2p.yourdomain.com/#{node}</code>）。',
  insecure_opt3_title: '方案 3（局域网跨设备 HTTP 免证书测试）',
  insecure_opt3_desc: '在 Chrome/Edge 浏览器地址栏打开：<code>chrome://flags/#unsafely-treat-insecure-origin-as-secure</code>，将 <code>http://{host}</code> 填入白名单输入框，并在右侧下拉框选择 <strong>Enabled</strong>，点击 <strong>Relaunch</strong> 重启浏览器即可！',
  btn_stay_debug: '保留在控制台调试视图',

  // File Hub / Vault View
  vault_play_online: '▶ 立即在线播放',
  vault_download: '⚡ 极速 P2P 下载',
  vault_cancel: '⏹ 终止下载',
  vault_open_tab: '📑 在新标签页打开原生预览',
  vault_back_dir: '⬅ 返回文件目录',
  vault_pdf_tip: '💡 <strong>预览提示</strong>：由于浏览器安全沙箱限制，内嵌沙箱环境已禁用 PDF 插件。桌面端请点击<strong>【在新标签页打开原生预览】</strong>；移动端推荐点击<strong>【极速 P2P 下载】</strong>使用系统阅读器浏览。',
  vault_media_tip: '💡 <strong>媒体提示</strong>：点击<strong>【立即在线播放】</strong>使用浏览器原生播放器（支持全屏、倍速与画中画）；点击<strong>【极速 P2P 下载】</strong>将原片高速直存至本地设备。',
  vault_connecting: '⚡ 正在建立 P2P 传输...',
  vault_requesting: '正在通过 WebRTC DataChannel 发起请求...',
  vault_receiving_pct: '📥 正在接收: {pct}% ({speed})',
  vault_transferred_of: '已传输 {received} / {total} · 速度 {speed}',
  vault_receiving_raw: '📥 已接收 {received} ({speed})',
  vault_transferred_raw: '已传输 {received} · 速度 {speed}',
  vault_complete_btn: '✅ 下载完成',
  vault_complete_status: '🎉 传输完毕，共 {size}',
  vault_cancelled_btn: '⚠️ 下载已取消',
  vault_cancelled_status: '用户中断了传输',
  vault_failed_btn: '❌ 下载失败',
  vault_failed_status: '传输失败: {error}',
};

const DICT_EN: Record<string, string> = {
  // Header
  header_title: 'P2P WebRTC Gateway (Zero-Client)',
  header_settings: '⚙ Settings',
  badge_disconnected: 'DISCONNECTED',
  badge_connecting: 'CONNECTING...',
  badge_connected: 'CONNECTED',

  // Cards
  card_peer_title: 'Target Node ID',
  card_peer_tip: 'Click to configure Target Node ID',
  card_signal_title: 'Signaling Server',
  card_signal_tip: 'Click to configure Signaling Server',
  card_edit: '✎ Edit',
  card_rtt_title: 'P2P Latency (RTT)',
  card_streams_title: 'Mux Streams',
  card_streams_active: '{count} Active',

  // Controls
  panel_mvp_title: 'MVP Connectivity Controls',
  btn_ping: 'Ping Test',
  btn_echo: 'Run Echo Stream',
  test_output_ready: 'Ready to test multiplexed P2P data flow.',

  // Logs
  panel_log_title: 'Realtime Event Log',
  btn_clear_log: 'Clear',

  // HUD / Floating Info
  hud_transport_mode: 'Transport Mode',
  hud_direct: '⚡ P2P Direct',
  hud_relay: '🛡️ TURN Relay',
  hud_local_candidate: 'Local Candidate',
  hud_remote_candidate: 'Remote Candidate',
  hud_protocol: 'Protocol',
  hud_ice_state: 'ICE State',
  hud_node: 'Node',
  hud_rtt: 'Round Trip Time (RTT)',
  hud_downlink: 'Downlink Rate',
  hud_uplink: 'Uplink Rate',
  hud_total_rx: 'Total: {size} MB',
  hud_total_tx: 'Total: {size} MB',
  hud_packet_loss: 'Packet Loss',
  hud_btn_ice_restart: '🔄 Test ICE Restart (Roaming)',
  hud_btn_settings: '⚙ Settings',
  hud_ice_restarting: '🔄 Triggering in-place WebRTC ICE Restart...',
  hud_ice_success: '✅ ICE Restart succeeded! Data channels and session preserved.',
  hud_ice_failed: '❌ ICE Restart failed: {error}',

  // Smart Pill
  pill_title: 'WebRTC Status (Click to expand / Drag to move)',
  pill_collapse_btn_title: 'Collapse to Mini Dot',
  pill_dot_title: 'WebRTC Status - Click to expand',

  // Settings Modal
  settings_title: '⚙ P2P Gateway Settings',
  settings_node_label: 'Target Node ID',
  settings_node_desc: 'Supports subdomains (e.g. bili.p2p.example.com), URL Hash (#nodeId), or manual entry.',
  settings_node_placeholder: 'e.g. myhome',
  settings_signal_label: 'Signaling Server URL',
  settings_signal_desc: 'WebSocket URL of signaling server (defaults to same-origin signaling).',
  settings_signal_placeholder: 'wss://... or leave blank for same-origin',
  settings_secret_label: 'Secret Token (Optional)',
  settings_secret_desc: 'Enter token if target node is protected with -secret; leave blank for public access.',
  settings_secret_placeholder: 'Optional Secret Token',
  settings_turn_label: 'TURN Relay (Optional)',
  settings_turn_desc: 'Fallback relay when direct hole punching fails on strict symmetric NATs.',
  settings_turn_url_placeholder: 'turn:turn.domain.com:3478',
  settings_turn_user_placeholder: 'Username',
  settings_turn_pass_placeholder: 'Password',
  settings_share_label: '🔗 Temporary Signed Share Link',
  settings_share_desc: 'Stateless HMAC-SHA256 time-bounded token, no password required, expires automatically.',
  duration_1h: 'Valid for 1 Hour',
  duration_6h: 'Valid for 6 Hours',
  duration_24h: 'Valid for 24 Hours',
  duration_7d: 'Valid for 7 Days',
  duration_30d: 'Valid for 30 Days',
  btn_gen_share_link: 'Generate Link',
  btn_copy: 'Copy',
  tip_copied: '✓ Copied to clipboard! Share it with visitors.',
  alert_enter_secret: 'Please enter a secret token above to generate a signed share link.',
  alert_gen_failed: 'Failed to generate signed link: {error}',
  btn_toggle_debug: 'Toggle Debug View',
  btn_save_connect: 'Save & Connect',
  settings_lang_label: 'Display Language',

  // Offline Alerts & Modal
  alert_bar_offline: 'Target node [{node}] is currently offline (Click for diagnostics or switch node)',
  btn_dismiss_alert: 'Dismiss',
  offline_title: 'Unable to Connect to Target Node',
  offline_desc: 'Target node [{node}] is currently offline, unregistered on signaling server, or WebRTC handshake timed out.',
  btn_dismiss_offline: 'Dismiss to Console',
  btn_retry_conn: 'Retry Connection',
  btn_switch_node: 'Switch Node',
  diagnostics_title: '📋 Client Diagnostics & Environment',

  // Auth Modal
  auth_title: 'Authentication Required',
  auth_desc: 'Target node [{node}] is protected. Please enter access secret:',
  auth_input_placeholder: 'Enter secret token...',
  auth_remember: 'Remember secret token for this node',
  btn_verify_connect: 'Verify & Connect',
  auth_err_invalid: 'Access secret is invalid or missing, please try again.',
  auth_err_expired: 'Share link expired or signature mismatch, please enter access secret.',

  // Insecure Context
  insecure_title: 'Insecure Context: Target Web Page Suspended',
  insecure_desc: 'According to W3C Web standards & browser security policies: on non-HTTPS and non-localhost plain HTTP IPs (such as <code>{origin}</code>), <strong>Service Worker is forcefully disabled</strong>. The gateway requires Service Worker to intercept and proxy requests. Please choose one of the options below:',
  insecure_opt1_title: 'Solution 1 (Localhost, Fastest)',
  insecure_opt1_desc: 'If browser and signaling server run on the same device, open <code>http://localhost:{port}/#{node}</code> directly (localhost is a secure context).',
  insecure_opt2_title: 'Solution 2 (Production Recommended, Zero-Config)',
  insecure_opt2_desc: 'Deploy on a domain with HTTPS or free Cloudflare Workers (e.g. <code>https://p2p.yourdomain.com/#{node}</code>).',
  insecure_opt3_title: 'Solution 3 (LAN Cross-Device HTTP Test)',
  insecure_opt3_desc: 'In Chrome/Edge address bar open: <code>chrome://flags/#unsafely-treat-insecure-origin-as-secure</code>, add <code>http://{host}</code> to whitelist, select <strong>Enabled</strong>, and click <strong>Relaunch</strong>.',
  btn_stay_debug: 'Stay in Console Debug View',

  // File Hub / Vault View
  vault_play_online: '▶ Play Online',
  vault_download: '⚡ Fast P2P Download',
  vault_cancel: '⏹ Cancel Download',
  vault_open_tab: '📑 Open Native Preview in New Tab',
  vault_back_dir: '⬅ Back to Directory',
  vault_pdf_tip: '💡 <strong>Preview Notice</strong>: Due to browser security sandbox policies, PDF plugins are disabled in embedded frame. For desktop click <strong>[Open Native Preview in New Tab]</strong>; on mobile click <strong>[Fast P2P Download]</strong> to view with native document reader.',
  vault_media_tip: '💡 <strong>Media Notice</strong>: Click <strong>[Play Online]</strong> to stream with browser native player (supports fullscreen, playback speed, and PiP); click <strong>[Fast P2P Download]</strong> to save original file directly to local storage.',
  vault_connecting: '⚡ Establishing P2P stream...',
  vault_requesting: 'Initiating request over WebRTC DataChannel...',
  vault_receiving_pct: '📥 Receiving: {pct}% ({speed})',
  vault_transferred_of: 'Transferred {received} / {total} · Speed {speed}',
  vault_receiving_raw: '📥 Received {received} ({speed})',
  vault_transferred_raw: 'Transferred {received} · Speed {speed}',
  vault_complete_btn: '✅ Download Complete',
  vault_complete_status: '🎉 Transfer finished, total {size}',
  vault_cancelled_btn: '⚠️ Download Cancelled',
  vault_cancelled_status: 'Transfer aborted by user',
  vault_failed_btn: '❌ Download Failed',
  vault_failed_status: 'Transfer failed: {error}',
};

export class I18nManager {
  private currentLocale: Locale;
  private listeners: Array<(locale: Locale) => void> = [];

  constructor() {
    this.currentLocale = this.detectLocale();
  }

  /**
   * Detect locale adhering to W3C standards:
   * 1. localStorage explicit override ('p2p_locale')
   * 2. navigator.language (starts with 'zh' -> 'zh-CN')
   * 3. Fallback -> 'en-US'
   */
  private detectLocale(): Locale {
    try {
      const stored = localStorage.getItem(STORAGE_KEY);
      if (stored === 'zh-CN' || stored === 'zh') return 'zh-CN';
      if (stored === 'en-US' || stored === 'en') return 'en-US';
    } catch {
      // Ignore localStorage sandbox restrictions
    }

    if (typeof navigator !== 'undefined') {
      const navLang = (navigator.languages && navigator.languages[0]) || navigator.language || '';
      if (navLang.toLowerCase().startsWith('zh')) {
        return 'zh-CN';
      }
    }

    return 'en-US';
  }

  public getLocale(): Locale {
    return this.currentLocale;
  }

  public setLocale(locale: Locale): void {
    if (this.currentLocale === locale) return;
    this.currentLocale = locale;
    try {
      localStorage.setItem(STORAGE_KEY, locale);
    } catch {
      // Ignore
    }
    this.notify();
  }

  public toggleLocale(): Locale {
    const next: Locale = this.currentLocale === 'zh-CN' ? 'en-US' : 'zh-CN';
    this.setLocale(next);
    return next;
  }

  public subscribe(cb: (locale: Locale) => void): () => void {
    this.listeners.push(cb);
    return () => {
      this.listeners = this.listeners.filter((fn) => fn !== cb);
    };
  }

  private notify(): void {
    for (const listener of this.listeners) {
      try {
        listener(this.currentLocale);
      } catch (err) {
        console.error('[i18n] listener error:', err);
      }
    }
  }

  /**
   * Format translation key with optional interpolation parameters.
   */
  public t(key: string, params?: TranslationParams): string {
    const dict = this.currentLocale === 'zh-CN' ? DICT_ZH : DICT_EN;
    let template = dict[key] ?? DICT_EN[key] ?? key;

    if (params) {
      for (const [pKey, pVal] of Object.entries(params)) {
        template = template.replace(new RegExp(`\\{${pKey}\\}`, 'g'), String(pVal));
      }
    }

    return template;
  }

  /**
   * Batch scan DOM for declarative i18n attributes:
   * - [data-i18n]: updates textContent
   * - [data-i18n-placeholder]: updates placeholder attribute
   * - [data-i18n-title]: updates title attribute
   */
  public applyI18nToDOM(root: HTMLElement | Document = document): void {
    // 1. Text content
    const textEls = root.querySelectorAll<HTMLElement>('[data-i18n]');
    textEls.forEach((el) => {
      const key = el.getAttribute('data-i18n');
      if (key) {
        el.textContent = this.t(key);
      }
    });

    // 2. Placeholders
    const phEls = root.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>('[data-i18n-placeholder]');
    phEls.forEach((el) => {
      const key = el.getAttribute('data-i18n-placeholder');
      if (key) {
        el.placeholder = this.t(key);
      }
    });

    // 3. Titles / tooltips
    const titleEls = root.querySelectorAll<HTMLElement>('[data-i18n-title]');
    titleEls.forEach((el) => {
      const key = el.getAttribute('data-i18n-title');
      if (key) {
        el.title = this.t(key);
      }
    });
  }
}

export const i18n = new I18nManager();
export const t = (key: string, params?: TranslationParams): string => i18n.t(key, params);
