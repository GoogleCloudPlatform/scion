/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * Shared confirmation dialog and display helpers for both structured command
 * request cards (`chat-command-request.ts`) and Markdown Execute shell blocks
 * (`chat-message.ts`, design §4.7, §4.14, audit M1.2).
 *
 * Security invariants:
 * - Every non-ASCII code point and non-printable ASCII control is rendered as
 *   a visible `\u{XXXX}` badge; tabs and trailing spaces are visible tokens;
 *   lines wrap (`overflow-wrap: anywhere`) so nothing scrolls off-screen.
 * - Before POSTing, `verifyCommandRequestHashBeforeSubmit` recomputes the
 *   canonical SHA-256 via WebCrypto and refuses on mismatch.
 */

import { LitElement, css, html, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import {
  DEFAULT_COMMAND_REQUEST_TIMEOUT_S,
  classifyCommandWorkdirKind,
  computeCommandRequestCanonicalHash,
  formatCommandInlineForDisplay,
  formatCommandScriptForDisplay,
  formatCommandTimeout,
  type CommandRequestExecContainerView,
  type CommandWorkdirKind,
  type FormattedCommandScript,
} from '../../../client/command-requests.js';

/** Shared CSS for script blocks, workdir badges, container line, and confirm dialog. */
export const commandConfirmDialogStyles = css`
  .banner {
    display: flex;
    align-items: flex-start;
    gap: 0.5rem;
    padding: 0.55rem 0.75rem;
    border-radius: var(--scion-radius);
    font-size: var(--chat-fs-sm, 0.875rem);
    line-height: 1.4;
  }

  .banner sl-icon {
    flex-shrink: 0;
    margin-top: 0.15rem;
    font-size: 1rem;
  }

  .banner.warning {
    background: var(--scion-warning-bg);
    border: 1px solid var(--scion-warning-border);
    color: var(--scion-warning-text);
  }

  .banner.error {
    background: var(--scion-error-bg);
    border: 1px solid var(--scion-border);
    color: var(--scion-error-text);
  }

  .banner.info {
    background: var(--scion-bg-subtle);
    border: 1px solid var(--scion-border);
    color: var(--scion-text-secondary);
  }

  .script-wrap {
    border: 1px solid var(--scion-border);
    border-radius: var(--scion-radius);
    background: var(--scion-bg);
    overflow: hidden;
  }

  .script-nonascii-note {
    display: flex;
    align-items: center;
    gap: 0.45rem;
    padding: 0.4rem 0.65rem;
    background: var(--scion-warning-bg);
    border-bottom: 1px solid var(--scion-warning-border);
    color: var(--scion-warning-text);
    font-size: 0.8125rem;
  }

  .script-nonascii-note sl-icon {
    flex-shrink: 0;
  }

  .script-scroll {
    overflow-x: hidden;
    padding: 0.5rem 0;
    font-family: var(--scion-font-mono);
    font-size: 0.8125rem;
    line-height: 1.5;
    color: var(--scion-text);
  }

  .script-line {
    display: flex;
    align-items: baseline;
    padding: 0 0.65rem 0 0;
  }

  .script-lineno {
    user-select: none;
    min-width: 2.5rem;
    padding: 0 0.6rem 0 0.5rem;
    text-align: right;
    color: var(--scion-text-muted);
    font-variant-numeric: tabular-nums;
    flex-shrink: 0;
  }

  .script-code {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    min-width: 0;
  }

  .ws-token {
    display: inline-block;
    padding: 0 0.2rem;
    margin: 0 0.05rem;
    border-radius: var(--scion-radius-sm);
    background: var(--scion-badge-neutral-bg);
    color: var(--scion-text-muted);
    font-size: 0.75rem;
  }

  .escape-token {
    display: inline-block;
    padding: 0 0.25rem;
    margin: 0 0.05rem;
    border-radius: var(--scion-radius-sm);
    background: var(--scion-warning-bg);
    border: 1px solid var(--scion-warning-border);
    color: var(--scion-warning-text);
    font-weight: 600;
    font-size: 0.75rem;
  }

  .script-foot {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.5rem;
    padding: 0.3rem 0.65rem;
    border-top: 1px solid var(--scion-border);
    background: var(--scion-bg-subtle);
    color: var(--scion-text-muted);
    font-size: 0.75rem;
    font-variant-numeric: tabular-nums;
  }

  .meta-grid {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.45rem 0.9rem;
    font-size: var(--chat-fs-sm, 0.875rem);
    color: var(--scion-text-secondary);
  }

  .meta-item {
    display: inline-flex;
    align-items: center;
    gap: 0.35rem;
    min-width: 0;
  }

  .meta-item sl-icon {
    flex-shrink: 0;
    font-size: 0.9rem;
    color: var(--scion-text-muted);
  }

  .workdir-badge {
    font: 500 0.8125rem/1.2 var(--scion-font-mono);
    background: var(--scion-bg-subtle);
    border: 1px solid var(--scion-border);
    color: var(--scion-text);
    padding: 0.1rem 0.4rem;
    border-radius: var(--scion-radius-sm);
    overflow-wrap: anywhere;
  }

  .workdir-badge.warn {
    background: var(--scion-warning-bg);
    border-color: var(--scion-warning-border);
    color: var(--scion-warning-text);
  }

  .exec-container-line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.4rem 0.6rem;
    padding: 0.5rem 0.7rem;
    border: 1px solid var(--scion-border);
    border-radius: var(--scion-radius);
    background: var(--scion-bg);
    font-size: var(--chat-fs-sm, 0.875rem);
    color: var(--scion-text-secondary);
  }

  .exec-container-line sl-icon {
    flex-shrink: 0;
    color: var(--scion-text-muted);
  }

  .exec-container-name {
    font-weight: 600;
    color: var(--scion-text);
  }

  .exec-state-chip {
    display: inline-flex;
    align-items: center;
    gap: 0.25rem;
    padding: 0.1rem 0.45rem;
    border-radius: var(--scion-radius-sm);
    font-size: 0.75rem;
    font-weight: 600;
    background: var(--scion-badge-neutral-bg);
    color: var(--scion-badge-neutral-text);
  }

  .exec-state-chip.ready {
    color: var(--scion-status-success);
  }

  .exec-state-chip.stopped,
  .exec-state-chip.unavailable {
    background: var(--scion-warning-bg);
    color: var(--scion-warning-text);
  }

  .exec-container-note {
    color: var(--scion-text-muted);
    font-size: 0.8125rem;
  }

  .exec-container-link {
    color: var(--scion-primary);
    font-weight: 600;
    text-decoration: none;
  }

  .exec-container-link:hover {
    text-decoration: underline;
  }

  .share-output-row {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-size: var(--chat-fs-sm, 0.875rem);
    color: var(--scion-text-secondary);
  }

  sl-dialog.confirm-dialog {
    --sl-panel-background-color: var(--scion-surface);
    --sl-panel-border-color: var(--scion-border);
    --sl-color-neutral-700: var(--scion-text);
  }

  sl-dialog.confirm-dialog::part(panel) {
    width: min(42rem, calc(100vw - 1.5rem));
  }

  .confirm-dialog-body {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    font-size: var(--chat-fs-base, 0.9375rem);
    color: var(--scion-text);
  }

  .confirm-sha {
    font-family: var(--scion-font-mono);
    font-size: 0.75rem;
    color: var(--scion-text-muted);
    overflow-wrap: anywhere;
  }

  .confirm-dialog-footer {
    display: flex;
    justify-content: flex-end;
    gap: 0.5rem;
    width: 100%;
  }
`;

export function renderEscapedInlineValue(value: string): TemplateResult {
  const formatted = formatCommandInlineForDisplay(value);
  return html`${formatted.tokens.map((tok) => {
    if (tok.kind === 'ascii') return tok.text;
    return html`<span
      class="escape-token"
      data-testid="escape-token"
      title=${`U+${(tok.codePoint ?? 0).toString(16).toUpperCase().padStart(4, '0')}`}
      >${tok.text}</span
    >`;
  })}`;
}

export function renderCommandScriptBlock(formatted: FormattedCommandScript): TemplateResult {
  return html`
    <div class="script-wrap" data-testid="command-script-block">
      ${formatted.nonAsciiCount > 0
        ? html`
            <div class="script-nonascii-note" role="status" data-testid="non-ascii-warning">
              <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
              <span>
                Script contains ${formatted.nonAsciiCount} non-ASCII
                ${formatted.nonAsciiCount === 1 ? 'character' : 'characters'}, shown as
                <code>\\u{XXXX}</code>.
              </span>
            </div>
          `
        : nothing}
      <div class="script-scroll">
        ${formatted.lines.map(
          (line) => html`
            <div class="script-line">
              <span class="script-lineno" aria-hidden="true">${line.lineNumber}</span>
              <span class="script-code"
                >${line.tokens.map((tok) => {
                  if (tok.kind === 'ascii') return tok.text;
                  if (tok.kind === 'tab') {
                    return html`<span class="ws-token tab" title="Tab (U+0009)">${tok.text}</span>`;
                  }
                  if (tok.kind === 'trailing-space') {
                    return html`<span
                      class="ws-token trailing-space"
                      title="Trailing space (U+0020)"
                      >${tok.text}</span
                    >`;
                  }
                  return html`<span
                    class="escape-token"
                    data-testid="escape-token"
                    title=${`U+${(tok.codePoint ?? 0).toString(16).toUpperCase().padStart(4, '0')}`}
                    >${tok.text}</span
                  >`;
                })}</span
              >
            </div>
          `
        )}
      </div>
      <div class="script-foot">
        <span
          >${formatted.lineCount} ${formatted.lineCount === 1 ? 'line' : 'lines'} ·
          ${formatted.byteLength} ${formatted.byteLength === 1 ? 'byte' : 'bytes'}</span
        >
        <span>/bin/bash --login</span>
      </div>
    </div>
  `;
}

export function renderCommandWorkdirWarning(
  workdirKind: CommandWorkdirKind,
  formatted: FormattedCommandScript
): TemplateResult | typeof nothing {
  if (workdirKind === 'workspace') {
    return html`
      <div class="banner warning" role="status" data-testid="workdir-warning">
        <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
        <span>Working directory is under <code>/workspace</code> (agent-writable).</span>
      </div>
    `;
  }
  if (workdirKind === 'shared_dir') {
    return html`
      <div class="banner warning" role="status" data-testid="workdir-warning">
        <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
        <span>Working directory is a shared directory (agent-writable).</span>
      </div>
    `;
  }
  if (formatted.mentionsAgentWritablePaths) {
    return html`
      <div class="banner warning" role="status" data-testid="script-path-warning">
        <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
        <span
          >Script references <code>/workspace</code> or a shared directory (agent-writable).</span
        >
      </div>
    `;
  }
  return nothing;
}

export function renderExecContainerLine(ec: CommandRequestExecContainerView): TemplateResult {
  if (ec.state === 'unconfigured') {
    return html`
      <div class="exec-container-line" data-testid="exec-container-line">
        <sl-icon name="terminal" aria-hidden="true"></sl-icon>
        <span>No execution container configured.</span>
        <a class="exec-container-link" href="/profile/settings" data-testid="exec-setup-link">
          Set up an execution container
        </a>
      </div>
    `;
  }

  const name = ec.agentName || ec.agentId || 'Execution container';
  const projectSuffix = ec.projectName ? ` (${ec.projectName})` : '';
  const stateClass =
    ec.state === 'ready'
      ? 'ready'
      : ec.state === 'stopped'
        ? 'stopped'
        : ec.state === 'unavailable'
          ? 'unavailable'
          : '';

  return html`
    <div class="exec-container-line" data-testid="exec-container-line">
      <sl-icon name="terminal" aria-hidden="true"></sl-icon>
      <span class="exec-container-name">${name}${projectSuffix}</span>
      <span class="exec-state-chip ${stateClass}" data-testid="exec-container-state"
        >${ec.state}</span
      >
      ${ec.state === 'suspended' && ec.canWake
        ? html`<span class="exec-container-note" data-testid="exec-suspended-wake-note"
            >Will be resumed when you approve.</span
          >`
        : nothing}
      ${ec.state === 'suspended' && !ec.canWake
        ? html`<span class="exec-container-note" data-testid="exec-suspended-no-wake-note"
            >Resume your execution container to approve.</span
          >`
        : nothing}
      ${ec.state === 'stopped'
        ? html`<span class="exec-container-note" data-testid="exec-stopped-note">
            Start your execution container to approve.
          </span>`
        : nothing}
      ${ec.state === 'unavailable'
        ? html`<span class="exec-container-note">
            Execution container is currently unavailable.
          </span>`
        : nothing}
      ${ec.homePersistent === false
        ? html`<span class="exec-container-note" data-testid="exec-home-nonpersistent">
            Home directory is not persistent.
          </span>`
        : nothing}
    </div>
  `;
}

export function execContainerAllowsRun(ec: CommandRequestExecContainerView | null): boolean {
  if (!ec) return true;
  if (ec.state === 'ready' || ec.state === 'starting') return true;
  if (ec.state === 'suspended') return Boolean(ec.canWake);
  return false;
}

/**
 * Recomputes the canonical SHA-256 hash in the browser via WebCrypto before
 * POSTing, refusing if it disagrees with `expectedSha256` (carry-forward a).
 */
export async function verifyCommandRequestHashBeforeSubmit(
  script: string,
  workdir: string,
  timeoutS: number,
  expectedSha256?: string
): Promise<{ ok: true; sha256: string } | { ok: false; error: string }> {
  let computed: string;
  try {
    computed = await computeCommandRequestCanonicalHash(script, workdir, timeoutS);
  } catch {
    return {
      ok: false,
      error: 'Could not verify the command SHA-256 digest in this browser.',
    };
  }
  const normalizedExpected = (expectedSha256 ?? '').trim().toLowerCase();
  if (normalizedExpected && computed !== normalizedExpected) {
    return {
      ok: false,
      error:
        'Command hash verification failed: the displayed command does not match the expected SHA-256 digest. Reload and review before approving.',
    };
  }
  return { ok: true, sha256: computed };
}

export interface RenderCommandConfirmDialogOptions {
  open: boolean;
  label?: string;
  confirmLabel?: string;
  formatted: FormattedCommandScript;
  workdir: string;
  workdirKind: CommandWorkdirKind;
  timeoutS: number;
  sha256: string;
  execContainer: CommandRequestExecContainerView | null;
  execContainerAllowed: boolean;
  shareOutputChecked: boolean;
  submitting: boolean;
  errorMessage: string;
  onClose: () => void;
  onShareOutputChange: (e: Event) => void;
  onConfirm: () => void;
}

export function renderCommandConfirmDialog(
  opts: RenderCommandConfirmDialogOptions
): TemplateResult {
  const workdirLabel = opts.workdir || '$HOME';

  return html`
    <sl-dialog
      class="confirm-dialog"
      label=${opts.label ?? 'Approve & run command'}
      ?open=${opts.open}
      @sl-request-close=${(e: CustomEvent): void => {
        if (e.target !== e.currentTarget) return;
        if (opts.submitting) {
          e.preventDefault();
          return;
        }
        opts.onClose();
      }}
      @sl-after-hide=${(e: Event): void => {
        if (e.target !== e.currentTarget) return;
        opts.onClose();
      }}
    >
      <div class="confirm-dialog-body" data-testid="confirm-dialog-body">
        ${renderCommandScriptBlock(opts.formatted)}

        <div class="meta-grid">
          <span class="meta-item">
            <sl-icon name="folder" aria-hidden="true"></sl-icon>
            <span>Workdir:</span>
            <span
              class="workdir-badge${opts.workdirKind === 'workspace' ||
              opts.workdirKind === 'shared_dir'
                ? ' warn'
                : ''}"
              data-testid="confirm-workdir"
              >${renderEscapedInlineValue(workdirLabel)}</span
            >
          </span>
          <span class="meta-item">
            <sl-icon name="clock" aria-hidden="true"></sl-icon>
            <span>Timeout: ${formatCommandTimeout(opts.timeoutS)}</span>
          </span>
        </div>

        ${renderCommandWorkdirWarning(opts.workdirKind, opts.formatted)}
        ${opts.execContainer ? renderExecContainerLine(opts.execContainer) : nothing}

        <div class="share-output-row">
          <sl-checkbox
            data-testid="confirm-share-output-checkbox"
            ?checked=${opts.shareOutputChecked}
            ?disabled=${opts.submitting}
            @sl-change=${(e: Event): void => opts.onShareOutputChange(e)}
          >
            Share redacted output tail with agent (up to 4 KiB)
          </sl-checkbox>
        </div>

        <div class="confirm-sha" data-testid="confirm-sha256">SHA-256: ${opts.sha256}</div>

        ${opts.errorMessage
          ? html`
              <div class="banner error" role="alert" data-testid="confirm-error-banner">
                <sl-icon name="exclamation-circle" aria-hidden="true"></sl-icon>
                <span>${opts.errorMessage}</span>
              </div>
            `
          : nothing}
      </div>

      <div slot="footer" class="confirm-dialog-footer">
        <sl-button variant="default" ?disabled=${opts.submitting} @click=${opts.onClose}>
          Cancel
        </sl-button>
        <sl-button
          variant="primary"
          class="confirm-approve-btn"
          data-testid="confirm-approve-btn"
          ?loading=${opts.submitting}
          ?disabled=${opts.submitting || !opts.execContainerAllowed}
          @click=${opts.onConfirm}
        >
          ${opts.confirmLabel ?? 'Approve & run'}
        </sl-button>
      </div>
    </sl-dialog>
  `;
}

export interface CommandConfirmSubmitDetail {
  commandSha256: string;
  shareOutput: boolean;
}

/**
 * Standalone reusable confirm dialog element for Markdown Execute shell blocks
 * in `chat-message.ts`. Renders the exact same `renderCommandConfirmDialog`
 * template as `<scion-chat-command-request>` and recomputes SHA-256 via
 * WebCrypto before dispatching `command-confirm-submit`.
 */
@customElement('scion-command-confirm-dialog')
export class ScionCommandConfirmDialog extends LitElement {
  @property({ type: Boolean })
  open = false;

  @property({ type: String })
  script = '';

  @property({ type: String })
  workdir = '';

  @property({ type: Number })
  timeoutS = DEFAULT_COMMAND_REQUEST_TIMEOUT_S;

  @property({ type: String })
  expectedSha256 = '';

  @property({ type: Object })
  execContainer: CommandRequestExecContainerView | null = null;

  @property({ type: Boolean })
  shareOutputDefault = false;

  @property({ type: Boolean })
  submitting = false;

  @property({ type: String })
  errorMessage = '';

  @state()
  private shareOutputChecked = false;

  @state()
  private computedSha256 = '';

  @state()
  private hashError = '';

  private hashSeq = 0;

  static override styles = css`
    ${commandConfirmDialogStyles}
  `;

  protected override willUpdate(changed: PropertyValues): void {
    if (changed.has('open') && this.open) {
      this.shareOutputChecked = this.shareOutputDefault;
      this.hashError = '';
      void this.refreshHash();
    } else if (
      this.open &&
      (changed.has('script') || changed.has('workdir') || changed.has('timeoutS'))
    ) {
      this.hashError = '';
      void this.refreshHash();
    }
  }

  private async refreshHash(): Promise<void> {
    const seq = ++this.hashSeq;
    try {
      const digest = await computeCommandRequestCanonicalHash(
        this.script,
        this.workdir,
        this.timeoutS
      );
      if (seq === this.hashSeq) {
        this.computedSha256 = digest;
      }
    } catch {
      if (seq === this.hashSeq) {
        this.computedSha256 = '';
      }
    }
  }

  private handleClose(): void {
    if (this.submitting) return;
    this.dispatchEvent(new CustomEvent('command-confirm-close', { bubbles: true, composed: true }));
  }

  private handleShareChange(e: Event): void {
    this.shareOutputChecked = Boolean(
      (e.currentTarget as HTMLElement & { checked?: boolean })?.checked
    );
  }

  private async handleConfirm(): Promise<void> {
    if (this.submitting || !execContainerAllowsRun(this.execContainer)) return;
    this.hashError = '';
    const verified = await verifyCommandRequestHashBeforeSubmit(
      this.script,
      this.workdir,
      this.timeoutS,
      this.expectedSha256 || this.computedSha256
    );
    if (!verified.ok) {
      this.hashError = verified.error;
      return;
    }
    this.computedSha256 = verified.sha256;
    this.dispatchEvent(
      new CustomEvent<CommandConfirmSubmitDetail>('command-confirm-submit', {
        detail: {
          commandSha256: verified.sha256,
          shareOutput: this.shareOutputChecked,
        },
        bubbles: true,
        composed: true,
      })
    );
  }

  override render(): TemplateResult {
    const formatted = formatCommandScriptForDisplay(this.script);
    const workdirKind = classifyCommandWorkdirKind(this.workdir);
    return renderCommandConfirmDialog({
      open: this.open,
      label: 'Approve & run command',
      confirmLabel: 'Approve & run',
      formatted,
      workdir: this.workdir,
      workdirKind,
      timeoutS: this.timeoutS,
      sha256: this.computedSha256 || this.expectedSha256 || 'computing…',
      execContainer: this.execContainer,
      execContainerAllowed: execContainerAllowsRun(this.execContainer),
      shareOutputChecked: this.shareOutputChecked,
      submitting: this.submitting,
      errorMessage: this.hashError || this.errorMessage,
      onClose: () => this.handleClose(),
      onShareOutputChange: (e) => this.handleShareChange(e),
      onConfirm: () => void this.handleConfirm(),
    });
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-command-confirm-dialog': ScionCommandConfirmDialog;
  }
}
