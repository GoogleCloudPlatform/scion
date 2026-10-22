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
 * Health tab of the Brokers area detail page (broker UI Option A, slice
 * A1). Shown only to users who may read the admin health summary (Q3: the
 * tab is admin-only, which maps onto the summary's hub.health.read rule).
 * It shows this broker's row of the summary and never claims "healthy"
 * when the row is missing.
 */

import { LitElement, html } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import type { HealthSummaryBroker } from './health-broker-table.js';
import { failingChecks } from '../../shared/broker-area-filters.js';
import { brokerAreaStyles } from '../shared/broker-area-styles.js';
import { formatRelative } from '../../utils/time.js';

@customElement('scion-broker-area-health')
export class ScionBrokerAreaHealth extends LitElement {
  /** This broker's summary row; null when the summary does not list it. */
  @property({ type: Object })
  row: HealthSummaryBroker | null = null;

  /** Whether the summary was truncated (the row may be past the cap). */
  @property({ type: Boolean })
  truncated = false;

  static override styles = brokerAreaStyles;

  override render() {
    const row = this.row;
    if (!row) {
      return html`
        <p class="tab-note not-in-summary">
          Not in the health summary${this.truncated ? ' (the summary is truncated)' : ''}; see
          <a href="/health">Health</a>.
        </p>
      `;
    }
    const checks = Object.entries(row.health?.checks ?? {}).sort(([a], [b]) => a.localeCompare(b));
    const failing = failingChecks(row.health);
    return html`
      <div class="stats-row">
        <div class="stat">
          <span class="stat-label">Self-reported health</span>
          <span class="stat-value-sm health-status">${row.health?.status || 'not reported'}</span>
        </div>
        <div class="stat">
          <span class="stat-label">Last heartbeat</span>
          <span class="stat-value-sm"
            >${row.last_heartbeat ? formatRelative(row.last_heartbeat) : 'never'}</span
          >
        </div>
        <div class="stat">
          <span class="stat-label">Workspace storage</span>
          <span class="stat-value-sm">${row.workspace_storage?.backend || '—'}</span>
        </div>
        <div class="stat">
          <span class="stat-label">Agents running / needing attention</span>
          <span class="stat-value-sm"
            >${row.agents?.running ?? 0} / ${row.agents?.attention ?? 0}</span
          >
        </div>
      </div>
      <div class="section">
        <div class="section-header">
          <h2>Checks</h2>
        </div>
        ${checks.length === 0
          ? html`<p class="tab-note">No checks reported.</p>`
          : html`
              <dl class="runtime-target-rows health-checks">
                ${checks.map(
                  ([name, result]) => html`
                    <dt>${name}</dt>
                    <dd class=${failing.includes(name) ? 'check-failing' : ''}>${result}</dd>
                  `
                )}
              </dl>
            `}
        <p class="tab-note">See <a href="/health">Health</a> for every broker.</p>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-broker-area-health': ScionBrokerAreaHealth;
  }
}
