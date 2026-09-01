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
 * Access Boundary Schedule Editor (Step 5)
 *
 * Optional activation window with notBefore and expiresAt date/time inputs.
 * Shows viewer time zone label + UTC preview.
 * Validates: notBefore < expiresAt (client-side warning; server validates too).
 * Includes a clear/remove schedule option.
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property, state } from 'lit/decorators.js';

import type { Iso8601 } from '../../shared/access-boundaries.js';

export interface ScheduleChangeDetail {
  notBefore: Iso8601 | undefined;
  expiresAt: Iso8601 | undefined;
}

@customElement('scion-access-boundary-schedule-editor')
export class ScionAccessBoundaryScheduleEditor extends LitElement {
  /** ISO 8601 UTC string for activation start. */
  @property() notBefore: Iso8601 | undefined = undefined;

  /** ISO 8601 UTC string for expiration. */
  @property() expiresAt: Iso8601 | undefined = undefined;

  @state() private hasSchedule = false;
  @state() private notBeforeLocal = '';
  @state() private expiresAtLocal = '';
  @state() private validationError = '';

  private get viewerTimeZone(): string {
    try {
      return Intl.DateTimeFormat().resolvedOptions().timeZone;
    } catch {
      return 'UTC';
    }
  }

  override connectedCallback(): void {
    super.connectedCallback();
    // Initialize local fields from props
    if (this.notBefore || this.expiresAt) {
      this.hasSchedule = true;
      if (this.notBefore) {
        this.notBeforeLocal = this.isoToLocalDatetime(this.notBefore);
      }
      if (this.expiresAt) {
        this.expiresAtLocal = this.isoToLocalDatetime(this.expiresAt);
      }
    }
  }

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .schedule-editor {
        display: flex;
        flex-direction: column;
        gap: 1.5rem;
      }

      .schedule-toggle {
        display: flex;
        align-items: center;
        gap: 0.75rem;
      }

      .schedule-toggle-label {
        font-size: 0.875rem;
        font-weight: 500;
        color: var(--scion-text, #1e293b);
      }

      .schedule-toggle-description {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.25rem;
      }

      .datetime-fields {
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: 1.5rem;
      }

      @media (max-width: 640px) {
        .datetime-fields {
          grid-template-columns: 1fr;
        }
      }

      .datetime-field {
        display: flex;
        flex-direction: column;
        gap: 0.25rem;
      }

      .field-label {
        font-size: 0.8125rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
      }

      .field-help {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }

      .utc-preview {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        font-family: var(--sl-font-mono, monospace);
        margin-top: 0.25rem;
      }

      .timezone-label {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        display: flex;
        align-items: center;
        gap: 0.25rem;
      }

      .clear-section {
        display: flex;
        justify-content: flex-start;
      }

      .validation-warning {
        margin-top: 0;
      }

      .no-schedule-info {
        padding: 1rem;
        text-align: center;
        color: var(--scion-text-muted, #64748b);
        font-size: 0.875rem;
      }

      fieldset {
        border: none;
        margin: 0;
        padding: 0;
      }

      @media (forced-colors: active) {
        .validation-warning {
          border: 2px solid Mark;
        }

        .utc-preview {
          color: ButtonText;
        }
      }
    `,
  ];

  private isoToLocalDatetime(iso: Iso8601): string {
    try {
      const date = new Date(iso);
      if (isNaN(date.getTime())) return '';
      // Format as YYYY-MM-DDTHH:MM for datetime-local input
      const year = date.getFullYear();
      const month = String(date.getMonth() + 1).padStart(2, '0');
      const day = String(date.getDate()).padStart(2, '0');
      const hours = String(date.getHours()).padStart(2, '0');
      const minutes = String(date.getMinutes()).padStart(2, '0');
      return `${year}-${month}-${day}T${hours}:${minutes}`;
    } catch {
      return '';
    }
  }

  private localDatetimeToIso(localValue: string): Iso8601 | undefined {
    if (!localValue) return undefined;
    try {
      const date = new Date(localValue);
      if (isNaN(date.getTime())) return undefined;
      return date.toISOString();
    } catch {
      return undefined;
    }
  }

  private formatUtcPreview(localValue: string): string {
    if (!localValue) return '';
    try {
      const date = new Date(localValue);
      if (isNaN(date.getTime())) return '';
      return date.toISOString().replace('T', ' ').replace('.000Z', ' UTC');
    } catch {
      return '';
    }
  }

  private validate(): void {
    this.validationError = '';

    const notBeforeIso = this.localDatetimeToIso(this.notBeforeLocal);
    const expiresAtIso = this.localDatetimeToIso(this.expiresAtLocal);

    if (notBeforeIso && expiresAtIso) {
      const nb = new Date(notBeforeIso);
      const ea = new Date(expiresAtIso);
      if (nb >= ea) {
        this.validationError = 'Activation start must be before expiration.';
      }
    }
  }

  private handleNotBeforeChange(e: Event): void {
    this.notBeforeLocal = (e.target as HTMLInputElement).value;
    this.validate();
    this.emitChange();
  }

  private handleExpiresAtChange(e: Event): void {
    this.expiresAtLocal = (e.target as HTMLInputElement).value;
    this.validate();
    this.emitChange();
  }

  private handleToggleSchedule(): void {
    this.hasSchedule = !this.hasSchedule;
    if (!this.hasSchedule) {
      this.notBeforeLocal = '';
      this.expiresAtLocal = '';
      this.validationError = '';
    }
    this.emitChange();
  }

  private handleClearSchedule(): void {
    this.notBeforeLocal = '';
    this.expiresAtLocal = '';
    this.validationError = '';
    this.hasSchedule = false;
    this.emitChange();
  }

  private emitChange(): void {
    this.dispatchEvent(
      new CustomEvent<ScheduleChangeDetail>('schedule-change', {
        detail: {
          notBefore: this.hasSchedule ? this.localDatetimeToIso(this.notBeforeLocal) : undefined,
          expiresAt: this.hasSchedule ? this.localDatetimeToIso(this.expiresAtLocal) : undefined,
        },
        bubbles: true,
        composed: true,
      })
    );
  }

  override render() {
    return html`
      <div class="schedule-editor">
        <div class="schedule-toggle">
          <sl-checkbox ?checked=${this.hasSchedule} @sl-change=${() => this.handleToggleSchedule()}>
            <span class="schedule-toggle-label">Set an activation window</span>
          </sl-checkbox>
        </div>

        <div class="schedule-toggle-description">
          An activation window limits when this access boundary is in effect. Without an activation
          window, the boundary is effective immediately and does not expire.
        </div>

        ${this.hasSchedule
          ? html`
              <div class="timezone-label">
                <sl-icon name="clock"></sl-icon>
                Times shown in: ${this.viewerTimeZone}
              </div>

              <fieldset>
                <legend class="sr-only">Activation window date and time</legend>
                <div class="datetime-fields">
                  <div class="datetime-field">
                    <label class="field-label" for="not-before">Activation start (optional)</label>
                    <sl-input
                      id="not-before"
                      type="datetime-local"
                      value=${this.notBeforeLocal}
                      aria-describedby=${this.validationError ? 'schedule-validation-msg' : ''}
                      @sl-input=${(e: Event) => this.handleNotBeforeChange(e)}
                      help-text="Boundary is not in effect before this time"
                    ></sl-input>
                    ${this.notBeforeLocal
                      ? html`<div class="utc-preview" aria-label="UTC equivalent">
                          UTC: ${this.formatUtcPreview(this.notBeforeLocal)}
                        </div>`
                      : nothing}
                  </div>

                  <div class="datetime-field">
                    <label class="field-label" for="expires-at">Expiration (optional)</label>
                    <sl-input
                      id="expires-at"
                      type="datetime-local"
                      value=${this.expiresAtLocal}
                      aria-describedby=${this.validationError ? 'schedule-validation-msg' : ''}
                      @sl-input=${(e: Event) => this.handleExpiresAtChange(e)}
                      help-text="Boundary expires at this time"
                    ></sl-input>
                    ${this.expiresAtLocal
                      ? html`<div class="utc-preview" aria-label="UTC equivalent">
                          UTC: ${this.formatUtcPreview(this.expiresAtLocal)}
                        </div>`
                      : nothing}
                  </div>
                </div>
              </fieldset>

              ${this.validationError
                ? html`
                    <sl-alert
                      variant="warning"
                      open
                      class="validation-warning"
                      id="schedule-validation-msg"
                      role="alert"
                    >
                      <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                      ${this.validationError}
                    </sl-alert>
                  `
                : nothing}

              <div class="clear-section">
                <sl-button variant="text" size="small" @click=${() => this.handleClearSchedule()}>
                  <sl-icon name="x-circle" slot="prefix"></sl-icon>
                  Remove activation window
                </sl-button>
              </div>
            `
          : html`
              <div class="no-schedule-info">
                This access boundary will take effect immediately and will not expire.
              </div>
            `}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-access-boundary-schedule-editor': ScionAccessBoundaryScheduleEditor;
  }
}
