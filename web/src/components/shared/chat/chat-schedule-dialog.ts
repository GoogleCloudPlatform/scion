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
 * The Schedule send dialog: presets and a date-and-time picker. The picker
 * value is a wall-clock time in the user's display zone (profile setting,
 * else the browser's zone), converted to a UTC instant on confirm.
 *
 * Events:
 * - `schedule-confirm` ({ fireAt }: UTC ISO instant)
 * - `schedule-cancel` (closed without scheduling)
 */

import { LitElement, html, css, nothing } from 'lit';
import type { PropertyValues } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { effectiveTimeZone } from '../../../utils/time.js';
import { DisplayZoneController } from '../../../utils/display-zone-controller.js';
import { resolveScheduleTime, schedulePresets } from './schedule-presets.js';

export interface ScheduleConfirmDetail {
  fireAt: string;
}

@customElement('scion-chat-schedule-dialog')
export class ScionChatScheduleDialog extends LitElement {
  @property({ type: Boolean })
  open = false;

  @state() private value = '';
  @state() private error = '';

  readonly _zone = new DisplayZoneController(this);

  static override styles = css`
    .presets {
      display: flex;
      flex-wrap: wrap;
      gap: 0.5rem;
      margin-bottom: 1rem;
    }
    .error {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: var(--sl-font-size-small, 0.875rem);
      margin-top: 0.5rem;
    }
  `;

  protected override willUpdate(changed: PropertyValues<this>): void {
    if (changed.has('open') && this.open) {
      // Start from tomorrow 09:00, the most common choice.
      const presets = schedulePresets(new Date(), effectiveTimeZone());
      this.value = presets.find((p) => p.id === 'tomorrow-9')?.value ?? '';
      this.error = '';
    }
  }

  override render() {
    const zone = effectiveTimeZone();
    const presets = schedulePresets(new Date(), zone);
    return html`
      <sl-dialog label="Schedule send" ?open=${this.open} @sl-request-close=${this.handleCancel}>
        <div class="presets">
          ${presets.map(
            (p) => html`
              <sl-button
                size="small"
                data-preset=${p.id}
                variant=${p.value === this.value ? 'primary' : 'default'}
                @click=${() => {
                  this.value = p.value;
                  this.error = '';
                }}
                >${p.label}</sl-button
              >
            `
          )}
        </div>
        <sl-input
          label="Date & time"
          type="datetime-local"
          help-text="Times in: ${zone}"
          .value=${this.value}
          @sl-input=${(e: Event) => {
            this.value = (e.target as HTMLInputElement).value;
            this.error = '';
          }}
        ></sl-input>
        ${this.error ? html`<div class="error" role="alert">${this.error}</div>` : nothing}
        <sl-button slot="footer" class="cancel-btn" @click=${this.handleCancel}>Cancel</sl-button>
        <sl-button slot="footer" class="confirm-btn" variant="primary" @click=${this.handleConfirm}>
          Schedule
        </sl-button>
      </sl-dialog>
    `;
  }

  private readonly handleConfirm = (): void => {
    const result = resolveScheduleTime(this.value, effectiveTimeZone(), new Date());
    if ('error' in result) {
      this.error = result.error;
      return;
    }
    this.dispatchEvent(
      new CustomEvent<ScheduleConfirmDetail>('schedule-confirm', {
        detail: { fireAt: result.fireAt },
        bubbles: true,
        composed: true,
      })
    );
  };

  private readonly handleCancel = (): void => {
    this.dispatchEvent(new CustomEvent('schedule-cancel', { bubbles: true, composed: true }));
  };
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-schedule-dialog': ScionChatScheduleDialog;
  }
}
