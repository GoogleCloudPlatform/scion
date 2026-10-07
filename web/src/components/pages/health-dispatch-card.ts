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
 * Dispatch card for the health dashboard (ptone/scion#3589).
 *
 * Shows three store-backed counts: stuck pending messages, stuck broker
 * dispatches and broker dispatches failed in the last hour. They come from
 * the hub's database, so every hub instance reports the same numbers. A null
 * section means the hub could not count them ("not reported"); it renders
 * neutral, never as zeros.
 */

import { LitElement, html, css, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';

/** The summary's dispatch block. */
export interface HealthSummaryDispatch {
  /** Agent-addressed messages still pending past the stuck-message threshold. */
  stuck_messages: number;
  /** Broker dispatches in progress with no update past the reaper's stuck age. */
  stuck_broker_dispatch: number;
  /** Broker dispatches that failed in the last hour. */
  failed_broker_dispatch_1h: number;
}

/** Tone of one figure: stuck work is a warning; failures are shown, not flagged. */
export type DispatchTone = 'warn' | 'neutral';

/** One row of the card. */
export interface DispatchRow {
  key: keyof HealthSummaryDispatch;
  label: string;
  title: string;
  value: number;
  tone: DispatchTone;
}

/**
 * The card's rows in display order. Only a non-zero stuck count is a
 * warning: stuck work needs action, while failed dispatches are history
 * that the reaper already finished with. The status policy (design 5.5,
 * P6.1, ptone/scion#3593) will also treat stuck work as degraded; until it
 * lands, the badge can show while the status pill reads healthy.
 */
export function dispatchRows(d: HealthSummaryDispatch): DispatchRow[] {
  return [
    {
      key: 'stuck_messages',
      label: 'Stuck messages',
      title: 'Messages to agents still waiting to be delivered after the stuck threshold',
      value: d.stuck_messages,
      tone: d.stuck_messages > 0 ? 'warn' : 'neutral',
    },
    {
      key: 'stuck_broker_dispatch',
      label: 'Stuck broker dispatches',
      title: 'Broker operations in progress with no update for longer than the stuck age',
      value: d.stuck_broker_dispatch,
      tone: d.stuck_broker_dispatch > 0 ? 'warn' : 'neutral',
    },
    {
      key: 'failed_broker_dispatch_1h',
      label: 'Failed broker dispatches (1h)',
      title: 'Broker operations that failed in the last hour',
      value: d.failed_broker_dispatch_1h,
      tone: 'neutral',
    },
  ];
}

@customElement('scion-health-dispatch-card')
export class ScionHealthDispatchCard extends LitElement {
  @property({ attribute: false })
  dispatch: HealthSummaryDispatch | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .card {
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-lg);
      padding: 1.25rem;
    }

    .card-title {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text-muted);
      text-transform: uppercase;
      letter-spacing: 0.05em;
      margin: 0 0 0.75rem 0;
    }

    ul.rows {
      margin: 0;
      padding: 0;
      list-style: none;
    }

    ul.rows li {
      display: flex;
      justify-content: space-between;
      align-items: center;
      gap: 1rem;
      padding: 0.375rem 0;
      font-size: 0.875rem;
      color: var(--scion-text);
      border-bottom: 1px solid var(--scion-border);
    }

    ul.rows li:last-child {
      border-bottom: none;
    }

    .label {
      color: var(--scion-text-muted);
    }

    .value {
      font-weight: 600;
      font-variant-numeric: tabular-nums;
    }

    .pill {
      display: inline-block;
      padding: 0.0625rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
    }

    .tone-warn {
      background: var(--scion-badge-warning-bg);
      color: var(--scion-badge-warning-text);
    }

    .empty {
      color: var(--scion-text-muted);
      font-size: 0.875rem;
    }
  `;

  override render(): TemplateResult {
    const d = this.dispatch;
    if (!d) {
      return html`
        <div class="card">
          <div class="card-title">Dispatch</div>
          <div class="empty">Dispatch data not available</div>
        </div>
      `;
    }
    return html`
      <div class="card">
        <div class="card-title">Dispatch</div>
        <ul class="rows">
          ${dispatchRows(d).map(
            (r) =>
              html`<li data-key=${r.key} title=${r.title}>
                <span class="label">${r.label}</span>
                <span class="value ${r.tone === 'warn' ? 'pill tone-warn' : ''}">${r.value}</span>
              </li>`
          )}
        </ul>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-health-dispatch-card': ScionHealthDispatchCard;
  }
}
