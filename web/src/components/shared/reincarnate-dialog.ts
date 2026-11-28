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
 * Reincarnate choice dialog component (ptone/scion#9845).
 *
 * Lets the user choose between "Reincarnate now" (hard stop + reprovision)
 * and "Ask the agent to prepare" (send a message requesting a graceful handoff
 * and self-reincarnation).
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { REINCARNATE_CONFIRM_MESSAGE } from '../../shared/agent-reincarnate.js';

export type ReincarnateChoiceMode = 'now' | 'prepare';

export interface ReincarnateChoiceDetail {
  mode: ReincarnateChoiceMode;
}

@customElement('scion-reincarnate-dialog')
export class ScionReincarnateDialog extends LitElement {
  /** Whether the dialog is open. */
  @property({ type: Boolean, reflect: true }) open = false;

  /** Agent display name for the dialog heading. */
  @property({ type: String }) agentName = '';

  /** Whether "Ask the agent to prepare" is currently available. */
  @property({ type: Boolean }) prepareAvailable = false;

  /** Explanation displayed when "Ask the agent to prepare" is disabled. */
  @property({ type: String }) prepareUnavailableReason = '';

  /** True while the chosen action is being submitted. */
  @property({ type: Boolean }) busy = false;

  @state() private selectedMode: ReincarnateChoiceMode = 'now';

  static override styles = css`
    :host {
      display: contents;
    }

    sl-radio-group::part(form-control-label) {
      position: absolute;
      width: 1px;
      height: 1px;
      padding: 0;
      margin: -1px;
      overflow: hidden;
      clip: rect(0, 0, 0, 0);
      white-space: nowrap;
      border: 0;
    }

    .options {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
    }

    .option-card {
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      padding: 0.875rem 1rem;
      background: var(--scion-surface);
      transition: border-color 150ms ease;
    }

    .option-card.selected {
      border-color: var(--scion-primary);
    }

    .option-card.disabled {
      background: var(--scion-bg-subtle);
    }

    sl-radio {
      width: 100%;
    }

    sl-radio::part(base) {
      align-items: flex-start;
    }

    .option-title {
      font-weight: 600;
      color: var(--scion-text);
      line-height: 1.3;
    }

    .option-desc {
      margin-top: 0.25rem;
      font-size: var(--sl-font-size-small);
      color: var(--scion-text-secondary);
      line-height: 1.45;
    }

    .option-reason {
      margin-top: 0.375rem;
      font-size: var(--sl-font-size-small);
      color: var(--scion-text-muted);
      line-height: 1.4;
    }

    .dialog-footer {
      display: flex;
      justify-content: flex-end;
      gap: var(--sl-spacing-x-small);
    }
  `;

  protected override updated(changed: Map<string, unknown>): void {
    if (changed.has('open') && this.open) {
      this.selectedMode = 'now';
    }
    if (!this.prepareAvailable && this.selectedMode === 'prepare') {
      this.selectedMode = 'now';
    }
  }

  private isOwnDialogEvent(e: Event): boolean {
    const dialog = this.shadowRoot?.querySelector('sl-dialog');
    return dialog !== null && e.composedPath()[0] === dialog;
  }

  private handleRequestClose = (e: Event): void => {
    if (!this.isOwnDialogEvent(e)) return;
    if (this.busy) {
      e.preventDefault();
      return;
    }
    this.emitCancel();
  };

  private handleHide = (e: Event): void => {
    if (!this.isOwnDialogEvent(e)) return;
    if (this.open && !this.busy) {
      this.emitCancel();
    }
  };

  private handleCancel = (): void => {
    if (this.busy) return;
    this.emitCancel();
  };

  private emitCancel(): void {
    this.dispatchEvent(
      new CustomEvent('reincarnate-cancel', {
        bubbles: true,
        composed: true,
      })
    );
  }

  private handleRadioChange(e: Event): void {
    const val = (e.target as HTMLElement & { value?: string }).value;
    if (val === 'now' || (val === 'prepare' && this.prepareAvailable)) {
      this.selectedMode = val;
    }
  }

  private selectMode(mode: ReincarnateChoiceMode): void {
    if (this.busy) return;
    if (mode === 'prepare' && !this.prepareAvailable) return;
    this.selectedMode = mode;
  }

  private handleConfirm = (): void => {
    if (this.busy) return;
    if (this.selectedMode === 'prepare' && !this.prepareAvailable) return;
    this.dispatchEvent(
      new CustomEvent<ReincarnateChoiceDetail>('reincarnate-choice', {
        detail: { mode: this.selectedMode },
        bubbles: true,
        composed: true,
      })
    );
  };

  override render() {
    const label = this.agentName ? `Reincarnate ${this.agentName}` : 'Reincarnate agent';
    const prepareDisabled = !this.prepareAvailable || this.busy;
    const confirmDisabled =
      this.busy || (this.selectedMode === 'prepare' && !this.prepareAvailable);

    return html`
      <sl-dialog
        label=${label}
        ?open=${this.open}
        @sl-request-close=${this.handleRequestClose}
        @sl-hide=${this.handleHide}
      >
        <sl-radio-group
          label="Reincarnation method"
          name="reincarnate-mode"
          .value=${this.selectedMode}
          @sl-change=${this.handleRadioChange}
        >
          <div class="options">
            <div
              class="option-card ${this.selectedMode === 'now' ? 'selected' : ''}"
              @click=${() => this.selectMode('now')}
            >
              <sl-radio value="now" data-testid="reincarnate-now" ?disabled=${this.busy}>
                <div class="option-title">Reincarnate now</div>
                <div class="option-desc">${REINCARNATE_CONFIRM_MESSAGE}</div>
              </sl-radio>
            </div>

            <div
              class="option-card ${this.selectedMode === 'prepare'
                ? 'selected'
                : ''} ${prepareDisabled ? 'disabled' : ''}"
              @click=${() => this.selectMode('prepare')}
            >
              <sl-radio
                value="prepare"
                data-testid="reincarnate-prepare"
                ?disabled=${prepareDisabled}
              >
                <div class="option-title">Ask the agent to prepare</div>
                <div class="option-desc">
                  Send the agent a message asking it to commit its work, write a handoff, and
                  reincarnate itself.
                </div>
                ${!this.prepareAvailable && this.prepareUnavailableReason
                  ? html`<div class="option-reason" data-testid="reincarnate-prepare-reason">
                      ${this.prepareUnavailableReason}
                    </div>`
                  : nothing}
              </sl-radio>
            </div>
          </div>
        </sl-radio-group>

        <div slot="footer" class="dialog-footer">
          <sl-button
            variant="default"
            data-testid="reincarnate-cancel"
            ?disabled=${this.busy}
            @click=${this.handleCancel}
          >
            Cancel
          </sl-button>
          <sl-button
            variant="primary"
            data-testid="reincarnate-confirm"
            ?loading=${this.busy}
            ?disabled=${confirmDisabled}
            @click=${this.handleConfirm}
          >
            Reincarnate
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-reincarnate-dialog': ScionReincarnateDialog;
  }
}
