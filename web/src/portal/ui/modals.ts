/**
 * Portal Modals:
 *  1. Owner Access Secret Modal (clears password input immediately — zero DOM secret residue)
 *  2. Ed25519 Key-Mismatch / TOFU Hard Red-Screen Blocker
 *  3. DTLS Capability Share-Link & Revocation Modal (in-session minting without re-entering password)
 *  4. ICE / TURN Settings Modal (strict RFC 7065 URI validation)
 *
 * Uses safe DOM creation (`textContent` everywhere, zero `innerHTML` with dynamic data).
 */

import type { KeyVerifyResult } from '../../shared/identity';
import {
  saveIcePreferences,
  validateIceServerUrl,
  type IcePreferences,
} from '../core/config';
import type { CapMintResult, CapabilityRecordDTO } from '../core/auth_client';
import { getLang, setLang, t, type Lang } from '../i18n';

function createEl<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  className?: string,
  text?: string
): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

export class PortalModals {
  private readonly host: HTMLElement;
  private activeOverlay: HTMLElement | null = null;

  constructor(host: HTMLElement) {
    this.host = host;
  }

  close(): void {
    if (this.activeOverlay) {
      // Clear any password inputs before detaching from DOM.
      const pwInputs = this.activeOverlay.querySelectorAll('input[type="password"]');
      pwInputs.forEach((inp) => {
        (inp as HTMLInputElement).value = '';
      });
      this.activeOverlay.remove();
      this.activeOverlay = null;
    }
  }

  private openOverlay(card: HTMLElement, dismissible: boolean, danger = false): void {
    this.close();
    const overlay = createEl('div', danger ? 'p2p-modal-overlay p2p-modal-danger' : 'p2p-modal-overlay');
    if (dismissible) {
      overlay.addEventListener('click', (ev) => {
        if (ev.target === overlay) this.close();
      });
    }
    overlay.appendChild(card);
    this.host.appendChild(overlay);
    this.activeOverlay = overlay;
  }

  showAuthModal(
    nodeId: string,
    errorMsg: string | null,
    onSubmit: (secret: string) => Promise<void>,
    onCancel: () => void
  ): void {
    const card = createEl('div', 'p2p-modal-card');
    card.appendChild(createEl('h3', 'p2p-modal-title', t('auth.title')));
    card.appendChild(createEl('p', 'p2p-modal-desc', t('auth.desc', { node: nodeId })));

    const errBox = createEl('div', 'p2p-modal-error', errorMsg || '');
    errBox.style.display = errorMsg ? 'block' : 'none';
    card.appendChild(errBox);

    const form = createEl('form', 'p2p-modal-form');
    const input = createEl('input', 'p2p-input');
    input.type = 'password';
    input.autocomplete = 'current-password';
    input.placeholder = t('auth.placeholder');
    input.required = true;
    form.appendChild(input);

    const actions = createEl('div', 'p2p-modal-actions');
    const cancelBtn = createEl('button', 'p2p-btn p2p-btn-secondary', t('auth.cancel'));
    cancelBtn.type = 'button';
    cancelBtn.onclick = () => {
      input.value = '';
      this.close();
      onCancel();
    };

    const submitBtn = createEl('button', 'p2p-btn p2p-btn-primary', t('auth.submit'));
    submitBtn.type = 'submit';
    actions.append(cancelBtn, submitBtn);
    form.appendChild(actions);

    form.onsubmit = async (ev) => {
      ev.preventDefault();
      const secret = input.value;
      // Scrub plaintext secret from DOM input immediately!
      input.value = '';
      if (!secret) return;
      submitBtn.disabled = true;
      try {
        await onSubmit(secret);
      } catch (err) {
        errBox.textContent = err instanceof Error ? err.message : String(err);
        errBox.style.display = 'block';
        submitBtn.disabled = false;
      }
    };

    card.appendChild(form);
    this.openOverlay(card, false);
    setTimeout(() => input.focus(), 10);
  }

  /**
   * Hard Red-Screen Blocker on Ed25519 pinned or TOFU public key mismatch.
   */
  showKeyMismatchModal(
    result: KeyVerifyResult,
    isPinned: boolean,
    onResetTofu: () => Promise<void>,
    onAbort: () => void
  ): void {
    const card = createEl('div', 'p2p-modal-card p2p-modal-card-danger');
    card.appendChild(createEl('h3', 'p2p-modal-title p2p-danger-text', t('tofu.title')));
    card.appendChild(
      createEl('p', 'p2p-modal-desc', isPinned ? t('tofu.pinnedDesc') : t('tofu.tofuDesc'))
    );

    const box = createEl('div', 'p2p-key-diff');
    box.appendChild(createEl('div', 'p2p-key-row', `${t('tofu.expected')}: ${result.expectedHash}`));
    box.appendChild(createEl('div', 'p2p-key-row', `${t('tofu.actual')}: ${result.actualHash}`));
    card.appendChild(box);

    const actions = createEl('div', 'p2p-modal-actions');
    const abortBtn = createEl('button', 'p2p-btn p2p-btn-primary', t('tofu.closeBtn'));
    abortBtn.type = 'button';
    abortBtn.onclick = () => {
      this.close();
      onAbort();
    };
    actions.appendChild(abortBtn);

    // Explicit pinned `?pk=` links can NEVER be bypassed; TOFU allows explicit owner reset.
    if (!isPinned) {
      const resetBtn = createEl('button', 'p2p-btn p2p-btn-danger', t('tofu.resetBtn'));
      resetBtn.type = 'button';
      resetBtn.onclick = async () => {
        resetBtn.disabled = true;
        this.close();
        await onResetTofu();
      };
      actions.appendChild(resetBtn);
    }

    card.appendChild(actions);
    this.openOverlay(card, false, true);
  }

  /**
   * DTLS-Authenticated Capability Mint & Revocation Modal (Owner role only, zero password re-entry).
   */
  showShareModal(
    nodeId: string,
    portalOrigin: string,
    ops: {
      mint: (opts: {
        ttlSec: number;
        pathPrefix: string;
        readonly: boolean;
        maxConns: number;
      }) => Promise<CapMintResult>;
      list: () => Promise<CapMintResult>;
      revoke: (jti: string) => Promise<CapMintResult>;
    }
  ): void {
    const card = createEl('div', 'p2p-modal-card p2p-modal-wide');
    card.appendChild(createEl('h3', 'p2p-modal-title', t('share.title')));
    card.appendChild(createEl('p', 'p2p-modal-desc', t('share.desc')));

    const errBox = createEl('div', 'p2p-modal-error');
    errBox.style.display = 'none';
    card.appendChild(errBox);

    const form = createEl('form', 'p2p-modal-form');

    const ttlLabel = createEl('label', 'p2p-label', t('share.ttl'));
    const ttlSelect = createEl('select', 'p2p-input');
    for (const [sec, label] of [
      ['900', '15 min'],
      ['3600', '1 hour'],
      ['86400', '24 hours'],
      ['604800', '7 days'],
    ]) {
      const opt = createEl('option', undefined, label);
      opt.value = sec;
      if (sec === '3600') opt.selected = true;
      ttlSelect.appendChild(opt);
    }
    ttlLabel.appendChild(ttlSelect);
    form.appendChild(ttlLabel);

    const pathLabel = createEl('label', 'p2p-label', t('share.pathPrefix'));
    const pathInput = createEl('input', 'p2p-input');
    pathInput.type = 'text';
    pathInput.value = '/';
    pathInput.placeholder = '/';
    pathLabel.appendChild(pathInput);
    form.appendChild(pathLabel);

    const maxLabel = createEl('label', 'p2p-label', t('share.maxConns'));
    const maxInput = createEl('input', 'p2p-input');
    maxInput.type = 'number';
    maxInput.min = '1';
    maxInput.max = '64';
    maxInput.value = '4';
    maxLabel.appendChild(maxInput);
    form.appendChild(maxLabel);

    const roWrap = createEl('label', 'p2p-checkbox-row');
    const roCheck = createEl('input');
    roCheck.type = 'checkbox';
    roWrap.append(roCheck, createEl('span', undefined, t('share.readonly')));
    form.appendChild(roWrap);

    const outWrap = createEl('div', 'p2p-share-result');
    outWrap.style.display = 'none';
    const outInput = createEl('input', 'p2p-input');
    outInput.readOnly = true;
    const copyBtn = createEl('button', 'p2p-btn p2p-btn-secondary', t('share.copyBtn'));
    copyBtn.type = 'button';
    copyBtn.onclick = async () => {
      try {
        await navigator.clipboard.writeText(outInput.value);
        copyBtn.textContent = t('share.copied');
        setTimeout(() => {
          copyBtn.textContent = t('share.copyBtn');
        }, 1500);
      } catch {
        outInput.select();
      }
    };
    outWrap.append(outInput, copyBtn);

    const mintBtn = createEl('button', 'p2p-btn p2p-btn-primary', t('share.mintBtn'));
    mintBtn.type = 'submit';
    form.appendChild(mintBtn);
    card.appendChild(form);
    card.appendChild(outWrap);

    // Active / Revoked tokens list
    card.appendChild(createEl('h4', 'p2p-section-title', t('share.activeList')));
    const listWrap = createEl('div', 'p2p-cap-list');
    card.appendChild(listWrap);

    const renderList = (records: CapabilityRecordDTO[]) => {
      listWrap.replaceChildren();
      if (records.length === 0) {
        listWrap.appendChild(createEl('div', 'p2p-muted', t('share.empty')));
        return;
      }
      for (const r of records) {
        const row = createEl('div', 'p2p-cap-row');
        const info = createEl('div', 'p2p-cap-info');
        const expStr = new Date(r.payload.exp * 1000).toLocaleString();
        const flags = [
          r.payload.path_prefix || '/',
          r.payload.readonly ? 'RO' : 'RW',
          `active=${r.active_conns}/${r.payload.max_conns || '∞'}`,
          `exp=${expStr}`,
        ].join(' · ');
        info.appendChild(createEl('div', 'p2p-cap-id', r.payload.jti.slice(0, 12)));
        info.appendChild(createEl('div', 'p2p-muted', flags));
        row.appendChild(info);

        if (r.revoked) {
          row.appendChild(createEl('span', 'p2p-badge p2p-badge-muted', t('share.revoked')));
        } else {
          const revBtn = createEl('button', 'p2p-btn p2p-btn-danger p2p-btn-sm', t('share.revoke'));
          revBtn.type = 'button';
          revBtn.onclick = async () => {
            errBox.style.display = 'none';
            revBtn.disabled = true;
            try {
              const res = await ops.revoke(r.payload.jti);
              if (!res.ok) {
                errBox.textContent = res.error || t('common.error');
                errBox.style.display = 'block';
                revBtn.disabled = false;
              }
            } catch (err) {
              errBox.textContent = err instanceof Error ? err.message : t('common.error');
              errBox.style.display = 'block';
              revBtn.disabled = false;
            }
            await refreshList();
          };
          row.appendChild(revBtn);
        }
        listWrap.appendChild(row);
      }
    };

    const refreshList = async () => {
      try {
        const res = await ops.list();
        if (res.ok && res.records) renderList(res.records);
      } catch {
        // ignore
      }
    };

    form.onsubmit = async (ev) => {
      ev.preventDefault();
      errBox.style.display = 'none';
      mintBtn.disabled = true;
      try {
        const res = await ops.mint({
          ttlSec: Number(ttlSelect.value) || 3600,
          pathPrefix: pathInput.value.trim() || '/',
          readonly: roCheck.checked,
          maxConns: Math.max(1, Number(maxInput.value) || 4),
        });
        if (!res.ok || !res.token || !res.pkHash) {
          throw new Error(res.error || 'Failed to mint share capability');
        }
        const cleanOrigin = portalOrigin.replace(/\/+$/, '');
        const link = `${cleanOrigin}/#${encodeURIComponent(nodeId)}?pk=${encodeURIComponent(res.pkHash)}&cap=${encodeURIComponent(res.token)}`;
        outInput.value = link;
        outWrap.style.display = 'flex';
        await refreshList();
      } catch (err) {
        errBox.textContent = err instanceof Error ? err.message : String(err);
        errBox.style.display = 'block';
      } finally {
        mintBtn.disabled = false;
      }
    };

    const footer = createEl('div', 'p2p-modal-actions');
    const closeBtn = createEl('button', 'p2p-btn p2p-btn-secondary', t('settings.close'));
    closeBtn.type = 'button';
    closeBtn.onclick = () => this.close();
    footer.appendChild(closeBtn);
    card.appendChild(footer);

    this.openOverlay(card, true);
    void refreshList();
  }

  /**
   * ICE / TURN & Language Settings Modal (strict RFC 7065 URI validation).
   */
  showSettingsModal(
    currentPrefs: IcePreferences,
    onSave: (nextPrefs: IcePreferences) => void
  ): void {
    const card = createEl('div', 'p2p-modal-card p2p-modal-wide');
    card.appendChild(createEl('h3', 'p2p-modal-title', t('settings.title')));

    const errBox = createEl('div', 'p2p-modal-error');
    errBox.style.display = 'none';
    card.appendChild(errBox);

    const form = createEl('form', 'p2p-modal-form');

    const stunLabel = createEl('label', 'p2p-label', t('settings.stun'));
    const stunInput = createEl('input', 'p2p-input');
    stunInput.type = 'text';
    stunInput.value = currentPrefs.customStun;
    stunInput.placeholder = 'stun:stun.cloudflare.com:3478';
    stunLabel.appendChild(stunInput);
    form.appendChild(stunLabel);

    const turnLabel = createEl('label', 'p2p-label', t('settings.turnUrl'));
    const turnInput = createEl('input', 'p2p-input');
    turnInput.type = 'text';
    turnInput.value = currentPrefs.turnUrl;
    turnInput.placeholder = 'turn:turn.example.com:3478?transport=udp';
    turnLabel.appendChild(turnInput);
    form.appendChild(turnLabel);

    const userLabel = createEl('label', 'p2p-label', t('settings.turnUser'));
    const userInput = createEl('input', 'p2p-input');
    userInput.type = 'text';
    userInput.value = currentPrefs.turnUser;
    userLabel.appendChild(userInput);
    form.appendChild(userLabel);

    const passLabel = createEl('label', 'p2p-label', t('settings.turnPass'));
    const passInput = createEl('input', 'p2p-input');
    passInput.type = 'password';
    passInput.value = currentPrefs.turnPass;
    passLabel.appendChild(passInput);
    form.appendChild(passLabel);

    const relayRow = createEl('label', 'p2p-checkbox-row');
    const relayCheck = createEl('input');
    relayCheck.type = 'checkbox';
    relayCheck.checked = currentPrefs.relayOnly;
    relayRow.append(relayCheck, createEl('span', undefined, t('settings.relayOnly')));
    form.appendChild(relayRow);

    const langLabel = createEl('label', 'p2p-label', t('settings.lang'));
    const langSelect = createEl('select', 'p2p-input');
    for (const [val, label] of [
      ['zh', '中文'],
      ['en', 'English'],
    ]) {
      const opt = createEl('option', undefined, label);
      opt.value = val;
      if (getLang() === val) opt.selected = true;
      langSelect.appendChild(opt);
    }
    langLabel.appendChild(langSelect);
    form.appendChild(langLabel);

    const actions = createEl('div', 'p2p-modal-actions');
    const closeBtn = createEl('button', 'p2p-btn p2p-btn-secondary', t('settings.close'));
    closeBtn.type = 'button';
    closeBtn.onclick = () => this.close();
    const saveBtn = createEl('button', 'p2p-btn p2p-btn-primary', t('settings.save'));
    saveBtn.type = 'submit';
    actions.append(closeBtn, saveBtn);
    form.appendChild(actions);

    form.onsubmit = (ev) => {
      ev.preventDefault();
      errBox.style.display = 'none';

      // Validate every configured STUN & TURN URI strictly per RFC 7065!
      const rawStun = stunInput.value.trim();
      if (rawStun) {
        for (const item of rawStun.split(',')) {
          const check = validateIceServerUrl(item);
          if (!check.ok || !/^stuns?:/i.test(check.normalized)) {
            errBox.textContent = check.error || `Invalid STUN URI: ${item}`;
            errBox.style.display = 'block';
            return;
          }
        }
      }
      const rawTurn = turnInput.value.trim();
      if (rawTurn) {
        const check = validateIceServerUrl(rawTurn);
        if (!check.ok || !/^turns?:/i.test(check.normalized)) {
          errBox.textContent = check.error || `Invalid TURN URI: ${rawTurn}`;
          errBox.style.display = 'block';
          return;
        }
      }
      if (relayCheck.checked && !rawTurn) {
        errBox.textContent = t('settings.relayNeedsTurn');
        errBox.style.display = 'block';
        return;
      }

      const nextPrefs: IcePreferences = {
        customStun: rawStun,
        turnUrl: rawTurn,
        turnUser: userInput.value.trim(),
        turnPass: passInput.value,
        relayOnly: relayCheck.checked,
      };
      passInput.value = '';
      saveIcePreferences(nextPrefs);
      setLang(langSelect.value as Lang);
      this.close();
      onSave(nextPrefs);
    };

    card.appendChild(form);
    this.openOverlay(card, true);
  }
}
