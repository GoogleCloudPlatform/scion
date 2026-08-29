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
 * Cascade Mode Dialog Component
 *
 * A dialog for applying a message mode change to an agent and all its
 * descendants. Shows a dry-run preview of affected agents before applying.
 * Follows the `<scion-quick-message-dialog>` declarative pattern: rendered
 * inline in the parent template, controlled via `?open=` binding.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { MESSAGE_MODE_DISPLAY, getMessageModeDisplay } from '../../shared/message-mode.js';
import { showToast } from '../../utils/toast.js';
import type { MessageMode, CascadePreview, CascadeAgentDetail } from '../../shared/types.js';
import '../shared/message-mode-badge.js';

@customElement('scion-cascade-mode-dialog')
export class ScionCascadeModeDialog extends LitElement {
  /** The root agent ID for the cascade. */
  @property() agentId = '';

  /** The root agent name, used in labels. */
  @property() agentName = '';

  /** Whether the dialog is open. */
  @property({ type: Boolean, reflect: true }) open = false;

  /** The root agent's current message mode. */
  @property() currentMode = 'project';

  /** Selected target mode in the dialog's selector. */
  @state() private selectedMode: MessageMode = 'project';

  /** Dry-run preview response from the API. */
  @state() private preview: CascadePreview | null = null;

  /** Whether a dry-run preview is loading. */
  @state() private loading = false;

  /** Whether the cascade apply is in progress. */
  @state() private applying = false;

  /** Error message to show inline. */
  @state() private error: string | null = null;

  static override styles = css`
    .dialog-body {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .mode-selector {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .mode-selector label {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
    }

    .affected-list {
      max-height: 300px;
      overflow-y: auto;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .affected-item {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0.5rem 0.75rem;
      font-size: 0.8125rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .affected-item:last-child {
      border-bottom: none;
    }

    .affected-name {
      font-weight: 500;
      color: var(--scion-text, #1e293b);
    }

    .affected-transition {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .affected-flag {
      font-size: 0.6875rem;
      font-weight: 600;
      padding: 0.0625rem 0.375rem;
      border-radius: 9999px;
    }

    .flag-unseal {
      background: var(--sl-color-warning-100);
      color: var(--sl-color-warning-700);
    }

    .flag-seal {
      background: var(--sl-color-danger-100);
      color: var(--sl-color-danger-700);
    }

    .flag-no-change {
      color: var(--scion-text-muted, #64748b);
      font-style: italic;
    }

    .impact-summary {
      font-size: 0.8125rem;
      padding: 0.75rem;
      border-radius: var(--scion-radius, 0.5rem);
      line-height: 1.4;
    }

    .impact-summary.normal {
      background: var(--sl-color-neutral-100);
      color: var(--scion-text, #1e293b);
    }

    .impact-summary.danger {
      background: var(--sl-color-danger-100);
      color: var(--sl-color-danger-700);
    }

    .dialog-error {
      color: var(--sl-color-danger-600);
      font-size: var(--sl-font-size-small);
    }

    .loading-state {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 2rem;
      color: var(--scion-text-muted, #64748b);
      gap: 0.5rem;
    }

    .empty-preview {
      text-align: center;
      padding: 1.5rem;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }
  `;

  protected override updated(changed: Map<string, unknown>): void {
    if (changed.has('open') && this.open) {
      this.selectedMode = (this.currentMode as MessageMode) || 'project';
      this.preview = null;
      this.error = null;
      this.applying = false;
      this.loading = false;
      void this.fetchPreview();
    }
  }

  private async fetchPreview(): Promise<void> {
    if (!this.agentId) return;
    this.loading = true;
    this.error = null;

    try {
      const response = await apiFetch(
        `/api/v1/agents/${this.agentId}/actions?dryRun=true`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            action: 'set_message_mode',
            mode: this.selectedMode,
            cascade: true,
          }),
        }
      );

      if (!response.ok) {
        throw new Error(
          await extractApiError(response, 'Failed to load cascade preview.')
        );
      }

      this.preview = (await response.json()) as CascadePreview;
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to load preview.';
      this.preview = null;
    } finally {
      this.loading = false;
    }
  }

  private async handleApply(): Promise<void> {
    if (!this.agentId) return;
    this.applying = true;
    this.error = null;

    try {
      const response = await apiFetch(
        `/api/v1/agents/${this.agentId}/actions`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            action: 'set_message_mode',
            mode: this.selectedMode,
            cascade: true,
          }),
        }
      );

      if (!response.ok) {
        throw new Error(
          await extractApiError(response, 'Failed to apply cascade mode change.')
        );
      }

      const count = this.preview?.cascade?.count ?? 0;
      const modeLabel = getMessageModeDisplay(this.selectedMode).label;
      showToast(
        `Message mode set to ${modeLabel} for ${count} agent${count !== 1 ? 's' : ''}.`,
        'success'
      );

      // Dispatch custom event so the parent can refresh
      this.dispatchEvent(
        new CustomEvent('cascade-applied', { bubbles: true, composed: true })
      );
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to apply cascade.';
    } finally {
      this.applying = false;
    }
  }

  private handleModeSelect(e: Event): void {
    this.selectedMode = (e.target as HTMLSelectElement).value as MessageMode;
    void this.fetchPreview();
  }

  private close(): void {
    this.open = false;
    this.dispatchEvent(
      new CustomEvent('sl-request-close', { bubbles: true, composed: true })
    );
  }

  override render() {
    return html`
      <sl-dialog
        label="Apply Mode to Branch"
        ?open=${this.open}
        @sl-request-close=${(e: Event) => {
          e.stopPropagation();
          this.close();
        }}
      >
        <div class="dialog-body">
          <div class="mode-selector">
            <label>Set all agents in this branch to:</label>
            <sl-select
              size="small"
              value=${this.selectedMode}
              @sl-change=${this.handleModeSelect}
              style="min-width: 200px;"
            >
              ${(Object.keys(MESSAGE_MODE_DISPLAY) as MessageMode[]).map(
                (mode) => html`
                  <sl-option value=${mode}>
                    <sl-icon slot="prefix" name=${MESSAGE_MODE_DISPLAY[mode].icon}></sl-icon>
                    ${MESSAGE_MODE_DISPLAY[mode].label}
                  </sl-option>
                `
              )}
            </sl-select>
          </div>

          ${this.loading
            ? html`
                <div class="loading-state">
                  <sl-spinner></sl-spinner>
                  Loading preview...
                </div>
              `
            : this.renderPreview()}

          ${this.error
            ? html`<div class="dialog-error">${this.error}</div>`
            : nothing}
        </div>

        <div slot="footer">
          <sl-button
            variant="default"
            @click=${this.close}
            ?disabled=${this.applying}
          >
            Cancel
          </sl-button>
          <sl-button
            variant=${this.selectedMode === 'none' ? 'danger' : 'primary'}
            @click=${() => void this.handleApply()}
            ?loading=${this.applying}
            ?disabled=${this.applying || this.loading}
            style="margin-inline-start: 0.5rem;"
          >
            ${this.selectedMode === 'none'
              ? `Seal ${this.affectedCount} agent${this.affectedCount !== 1 ? 's' : ''}`
              : `Apply to ${this.affectedCount} agent${this.affectedCount !== 1 ? 's' : ''}`}
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  private get affectedCount(): number {
    return this.preview?.cascade?.count ?? 0;
  }

  private renderPreview() {
    if (!this.preview?.cascade?.details?.length) {
      if (this.error) return nothing;
      return html`<div class="empty-preview">No agents will be affected by this change.</div>`;
    }

    const details = this.preview.cascade.details;
    const sealCount = details.filter(
      (d) => d.new_mode === 'none' && d.current_mode !== 'none'
    ).length;
    const unsealCount = details.filter(
      (d) => d.current_mode === 'none' && d.new_mode !== 'none'
    ).length;

    return html`
      <div>
        <div style="font-size: 0.8125rem; font-weight: 500; margin-bottom: 0.5rem;">
          Affected agents (${details.length}):
        </div>
        <div class="affected-list">
          ${details.map((d) => this.renderAffectedAgent(d))}
        </div>
      </div>

      ${this.renderImpactSummary(details.length, sealCount, unsealCount)}
    `;
  }

  private renderAffectedAgent(detail: CascadeAgentDetail) {
    const currentDisplay = getMessageModeDisplay(detail.current_mode);
    const newDisplay = getMessageModeDisplay(detail.new_mode);
    const noChange = detail.current_mode === detail.new_mode;
    const isUnseal = detail.current_mode === 'none' && detail.new_mode !== 'none';
    const isSeal = detail.new_mode === 'none' && detail.current_mode !== 'none';

    return html`
      <div class="affected-item">
        <span class="affected-name">${detail.agent_name}</span>
        <span class="affected-transition">
          ${noChange
            ? html`<span class="flag-no-change">${currentDisplay.label} (no change)</span>`
            : html`
                ${currentDisplay.label}
                <sl-icon name="arrow-right" style="font-size: 0.625rem;"></sl-icon>
                ${newDisplay.label}
                ${isUnseal
                  ? html`<span class="affected-flag flag-unseal">unseal</span>`
                  : nothing}
                ${isSeal
                  ? html`<span class="affected-flag flag-seal">seal</span>`
                  : nothing}
              `}
        </span>
      </div>
    `;
  }

  private renderImpactSummary(total: number, sealed: number, unsealed: number) {
    const isDanger = this.selectedMode === 'none';
    const parts: string[] = [];
    parts.push(`${total} agent${total !== 1 ? 's' : ''} will change mode.`);
    if (sealed > 0) parts.push(`${sealed} will be sealed.`);
    if (unsealed > 0) parts.push(`${unsealed} will be unsealed.`);

    if (isDanger) {
      return html`
        <div class="impact-summary danger">
          <sl-icon name="exclamation-triangle" style="margin-right: 0.375rem;"></sl-icon>
          All ${total} agent${total !== 1 ? 's' : ''} will be sealed. Only super-admins
          will be able to message them. This takes effect immediately.
        </div>
      `;
    }

    return html`
      <div class="impact-summary normal">
        ${parts.join(' ')} This takes effect immediately for all affected agents.
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-cascade-mode-dialog': ScionCascadeModeDialog;
  }
}
