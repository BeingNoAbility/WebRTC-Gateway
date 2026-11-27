/**
 * V7 SmartPill Floating Status HUD & Ring-Buffered Log Drawer
 * - Stable click delegation (zero accidental toggle jitter).
 * - Bounded 200-line ring buffer for #logBox (prevents unbounded DOM/memory growth).
 * - Explicit Ed25519 key verification badge & same-site mode indicator.
 */

import type { TunnelStatsSnapshot } from '../core/stats_collector';
import { t } from '../i18n';

const MAX_LOG_LINES = 200;

export interface SmartPillHandlers {
  onShare: () => void;
  onSettings: () => void;
  onReconnect: () => void;
  onResetSandbox?: () => void;
}

function formatRate(bps: number): string {
  if (bps < 1024) return `${Math.round(bps)} B/s`;
  if (bps < 1024 * 1024) return `${(bps / 1024).toFixed(1)} KB/s`;
  return `${(bps / (1024 * 1024)).toFixed(2)} MB/s`;
}

export class SmartPill {
  private readonly root: HTMLElement;
  private readonly pillBtn: HTMLButtonElement;
  private readonly dotEl: HTMLElement;
  private readonly labelEl: HTMLElement;
  private readonly drawerEl: HTMLElement;
  private readonly metaEl: HTMLElement;
  private readonly statsEl: HTMLElement;
  private readonly shareBtn: HTMLButtonElement;
  private readonly settingsBtn: HTMLButtonElement;
  private readonly reconnectBtn: HTMLButtonElement;
  private readonly resetSandboxBtn: HTMLButtonElement;
  private readonly clearLogBtn: HTMLButtonElement;
  private readonly logBox: HTMLElement;
  private expanded = false;
  private logCount = 0;

  constructor(container: HTMLElement, handlers: SmartPillHandlers) {
    this.root = document.createElement('div');
    this.root.className = 'p2p-pill-wrap';

    this.pillBtn = document.createElement('button');
    this.pillBtn.type = 'button';
    this.pillBtn.className = 'p2p-pill-btn';
    this.dotEl = document.createElement('span');
    this.dotEl.className = 'p2p-dot p2p-dot-idle';
    this.labelEl = document.createElement('span');
    this.labelEl.textContent = t('status.idle');
    this.pillBtn.append(this.dotEl, this.labelEl);

    this.drawerEl = document.createElement('div');
    this.drawerEl.className = 'p2p-pill-drawer';
    this.drawerEl.style.display = 'none';

    this.metaEl = document.createElement('div');
    this.metaEl.className = 'p2p-pill-meta';

    this.statsEl = document.createElement('div');
    this.statsEl.className = 'p2p-pill-stats';

    const btnRow = document.createElement('div');
    btnRow.className = 'p2p-pill-actions';

    this.shareBtn = document.createElement('button');
    this.shareBtn.type = 'button';
    this.shareBtn.className = 'p2p-btn p2p-btn-primary p2p-btn-sm';
    this.shareBtn.style.display = 'none';

    this.settingsBtn = document.createElement('button');
    this.settingsBtn.type = 'button';
    this.settingsBtn.className = 'p2p-btn p2p-btn-secondary p2p-btn-sm';

    this.reconnectBtn = document.createElement('button');
    this.reconnectBtn.type = 'button';
    this.reconnectBtn.className = 'p2p-btn p2p-btn-secondary p2p-btn-sm';

    this.resetSandboxBtn = document.createElement('button');
    this.resetSandboxBtn.type = 'button';
    this.resetSandboxBtn.className = 'p2p-btn p2p-btn-secondary p2p-btn-sm';
    this.resetSandboxBtn.style.display = 'none';

    this.clearLogBtn = document.createElement('button');
    this.clearLogBtn.type = 'button';
    this.clearLogBtn.className = 'p2p-btn p2p-btn-secondary p2p-btn-sm';

    btnRow.append(this.shareBtn, this.settingsBtn, this.reconnectBtn, this.resetSandboxBtn, this.clearLogBtn);

    this.logBox = document.createElement('div');
    this.logBox.className = 'p2p-log-box';
    this.logBox.id = 'logBox';

    this.drawerEl.append(this.metaEl, this.statsEl, btnRow, this.logBox);
    this.root.append(this.pillBtn, this.drawerEl);
    container.appendChild(this.root);

    this.pillBtn.addEventListener('click', (ev) => {
      ev.stopPropagation();
      this.setExpanded(!this.expanded);
    });
    this.shareBtn.addEventListener('click', () => handlers.onShare());
    this.settingsBtn.addEventListener('click', () => handlers.onSettings());
    this.reconnectBtn.addEventListener('click', () => handlers.onReconnect());
    this.resetSandboxBtn.addEventListener('click', () => handlers.onResetSandbox?.());
    this.clearLogBtn.addEventListener('click', () => this.clearLogs());

    this.refreshLabels();
  }

  refreshLabels(): void {
    this.shareBtn.textContent = t('pill.share');
    this.settingsBtn.textContent = t('pill.settings');
    this.reconnectBtn.textContent = t('pill.reconnect');
    this.resetSandboxBtn.textContent = t('pill.resetSandbox');
    this.clearLogBtn.textContent = t('pill.clearLogs');
  }

  setExpanded(open: boolean): void {
    this.expanded = open;
    this.drawerEl.style.display = open ? 'flex' : 'none';
  }

  setStatus(status: string, nodeId: string): void {
    this.dotEl.className = `p2p-dot p2p-dot-${status}`;
    const txt = t(`status.${status}`);
    this.labelEl.textContent = nodeId ? `${nodeId} · ${txt}` : txt;
  }

  setSessionInfo(info: {
    nodeId: string;
    role: string;
    pkHash: string;
    version: string;
    sameSiteMode: boolean;
    canResetSandbox?: boolean;
  }): void {
    const parts = [
      `Node: ${info.nodeId}`,
      `Role: ${info.role}`,
      `${t('pill.keyVerified')}: ${info.pkHash.slice(0, 12)}…`,
      info.version,
    ];
    if (info.sameSiteMode) {
      parts.push('⚠ Same-Site');
    }
    this.metaEl.textContent = parts.join(' | ');
    this.shareBtn.style.display = info.role === 'owner' ? 'inline-flex' : 'none';
    this.resetSandboxBtn.style.display = info.canResetSandbox ? 'inline-flex' : 'none';
  }

  updateStats(snap: TunnelStatsSnapshot): void {
    const rtt = snap.rttMs !== null ? `${snap.rttMs} ms` : '—';
    this.statsEl.textContent = `RTT: ${rtt} · ↑ ${formatRate(snap.txBps)} · ↓ ${formatRate(snap.rxBps)} · Streams: ${snap.activeStreams} · ${snap.poolSize} pool DCs (${snap.candidateType})`;
  }

  appendLog(line: string): void {
    const ts = new Date().toLocaleTimeString();
    const row = document.createElement('div');
    row.className = 'p2p-log-line';
    row.textContent = `[${ts}] ${line}`;
    this.logBox.appendChild(row);
    this.logCount++;
    while (this.logCount > MAX_LOG_LINES && this.logBox.firstChild) {
      this.logBox.removeChild(this.logBox.firstChild);
      this.logCount--;
    }
    this.logBox.scrollTop = this.logBox.scrollHeight;
  }

  clearLogs(): void {
    this.logBox.replaceChildren();
    this.logCount = 0;
  }
}
