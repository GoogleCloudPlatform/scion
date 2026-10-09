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
 * Health Dashboard page component
 *
 * Owns fetching, polling and state for GET /api/v1/admin/health/summary,
 * and lays out the section modules (design 5.1, 5.7):
 * - Header: overall status pill, "as of" time, the serving hub instance
 * - Needs attention (health-attention.ts), full width and first
 * - Hub (health-hub-card.ts, database and the service account check
 *   diagnostic folded in) | Dispatch (health-dispatch-card.ts)
 * - Runtime brokers (compact table, health-broker-table.ts)
 * - Integrations (chat plugins, health-integrations.ts)
 * - Agents (phase counts and problem groups, health-agents-card.ts)
 *
 * Auto-refreshes every 30 seconds via polling.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { formatInstant, formatInstantWithZone } from '../../utils/time.js';
import type { HealthAttentionItem } from './health-attention.js';
import './health-attention.js';
import type {
  HealthSummaryHub,
  HealthSummaryDatabase,
  HealthSummaryServiceAccountCheck,
} from './health-hub-card.js';
import './health-hub-card.js';
import type { HealthSummaryBrokerList } from './health-broker-table.js';
import './health-broker-table.js';
import type { HealthSummaryAgents } from './health-agents-card.js';
import './health-agents-card.js';
import type {
  HealthSummaryIntegration,
  HealthSummaryIntegrationCounts,
} from './health-integrations.js';
import { integrationsSectionVisible } from './health-integrations.js';
import type { HealthSummaryDispatch } from './health-dispatch-card.js';
import './health-dispatch-card.js';
import { healthPillStyles, healthTone } from './health-status.js';

export { formatHeartbeatAge } from './health-broker-table.js';
export type { HealthAttentionItem } from './health-attention.js';
export type { HealthSummaryIntegrationCounts } from './health-integrations.js';
export type { HealthSummaryServiceAccountCheck } from './health-hub-card.js';

export interface HealthSummary {
  status: string;
  /** When the serving hub instance built the summary (RFC 3339). */
  generated_at: string;
  /** Ranked attention items; see deriveHealthSummaryStatus on the server. */
  attention: HealthAttentionItem[];
  hub: HealthSummaryHub;
  database: HealthSummaryDatabase;
  runtime_brokers: HealthSummaryBrokerList;
  /**
   * Chat and messaging plugins; empty when none are configured, or when the
   * caller lacks hub.integrations.read (integrations_detail false).
   */
  integrations: HealthSummaryIntegration[];
  /** True when integrations and integration attention items carry identity. */
  integrations_detail: boolean;
  integration_counts: HealthSummaryIntegrationCounts;
  /** Null when the hub could not aggregate agents (not reported). */
  agents: HealthSummaryAgents | null;
  /** Null when the hub could not count dispatch health (not reported). */
  dispatch: HealthSummaryDispatch | null;
  /**
   * Present only while the service account assignment check cannot run
   * because the hub's identity lacks the access it needs.
   */
  service_account_check?: HealthSummaryServiceAccountCheck;
}

@customElement('scion-page-health-dashboard')
export class ScionPageHealthDashboard extends LitElement {
  @state()
  private loading = true;

  @state()
  private error: string | null = null;

  @state()
  private data: HealthSummary | null = null;

  @state()
  private autoRefresh = true;

  private refreshTimer: ReturnType<typeof setInterval> | null = null;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.fetchData();
    this.startAutoRefresh();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopAutoRefresh();
  }

  private startAutoRefresh(): void {
    this.stopAutoRefresh();
    if (this.autoRefresh) {
      this.refreshTimer = setInterval(() => void this.fetchData(), 30_000);
    }
  }

  private stopAutoRefresh(): void {
    if (this.refreshTimer) {
      clearInterval(this.refreshTimer);
      this.refreshTimer = null;
    }
  }

  private toggleAutoRefresh(): void {
    this.autoRefresh = !this.autoRefresh;
    if (this.autoRefresh) {
      this.startAutoRefresh();
    } else {
      this.stopAutoRefresh();
    }
  }

  private async fetchData(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/admin/health/summary');
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to fetch health summary');
        return;
      }
      this.data = (await res.json()) as HealthSummary;
      this.error = null;
    } catch (e) {
      this.error = e instanceof Error ? e.message : 'Network error';
    } finally {
      this.loading = false;
    }
  }

  static override styles = [
    healthPillStyles,
    css`
      :host {
        display: block;
      }

      .header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        flex-wrap: wrap;
        gap: 0.75rem;
        margin-bottom: 1.5rem;
      }

      .header-left {
        display: flex;
        align-items: center;
        flex-wrap: wrap;
        gap: 0.75rem;
      }

      .header h1 {
        font-size: 1.5rem;
        font-weight: 700;
        color: var(--scion-text);
        margin: 0;
      }

      .overall-status {
        padding: 0.25rem 0.75rem;
        font-size: 0.8125rem;
      }

      .dot {
        width: 0.5rem;
        height: 0.5rem;
        border-radius: 9999px;
        background: currentColor;
      }

      .meta {
        font-size: 0.75rem;
        color: var(--scion-text-muted);
      }

      .header-right {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        font-size: 0.8125rem;
        color: var(--scion-text-muted);
      }

      .refresh-btn {
        background: none;
        border: 1px solid var(--scion-border);
        border-radius: 0.375rem;
        padding: 0.25rem 0.5rem;
        font-size: 0.75rem;
        cursor: pointer;
        color: var(--scion-text-muted);
      }

      .refresh-btn:hover {
        background: var(--scion-bg-subtle);
      }

      .toggle-label {
        display: flex;
        align-items: center;
        gap: 0.375rem;
        cursor: pointer;
        font-size: 0.8125rem;
      }

      .grid-2 {
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: 1rem;
        margin-bottom: 1rem;
      }

      .grid-full {
        margin-bottom: 1rem;
      }

      .loading,
      .error-msg {
        text-align: center;
        padding: 3rem 1rem;
        color: var(--scion-text-muted);
      }

      .error-msg {
        color: var(--scion-badge-danger-text);
      }

      .error-banner {
        margin-bottom: 1rem;
        padding: 0.75rem;
        border-radius: 0.375rem;
        font-size: 0.875rem;
        background: var(--scion-badge-danger-bg);
        color: var(--scion-badge-danger-text);
      }

      @media (max-width: 768px) {
        .grid-2 {
          grid-template-columns: 1fr;
        }
      }
    `,
  ];

  override render() {
    if (this.loading) {
      return html`<div class="loading">Loading health data...</div>`;
    }

    if (this.error && !this.data) {
      return html`<div class="error-msg">${this.error}</div>`;
    }

    if (!this.data) {
      return html`<div class="loading">No data available</div>`;
    }

    const d = this.data;

    return html`
      ${this.renderHeader(d)}
      ${this.error ? html`<div class="error-banner" role="alert">${this.error}</div>` : nothing}

      <div class="grid-full">
        <scion-health-attention
          .items=${d.attention ?? null}
          .integrationsDetail=${d.integrations_detail === true}
        ></scion-health-attention>
      </div>

      <div class="grid-2" data-role="hub-dispatch">
        <scion-health-hub-card
          .hub=${d.hub ?? null}
          .database=${d.database ?? null}
          .serviceAccountCheck=${d.service_account_check ?? null}
        ></scion-health-hub-card>
        <scion-health-dispatch-card .dispatch=${d.dispatch ?? null}></scion-health-dispatch-card>
      </div>

      <div class="grid-full">
        <scion-health-broker-table .brokers=${d.runtime_brokers}></scion-health-broker-table>
      </div>

      ${integrationsSectionVisible(
        d.integrations_detail === true,
        d.integrations,
        d.integration_counts
      )
        ? html`<div class="grid-full">
            <scion-health-integrations
              .integrations=${d.integrations ?? []}
              .detail=${d.integrations_detail === true}
              .counts=${d.integration_counts ?? null}
            ></scion-health-integrations>
          </div>`
        : nothing}

      <div class="grid-full">
        <scion-health-agents-card .agents=${d.agents ?? null}></scion-health-agents-card>
      </div>
    `;
  }

  private renderHeader(d: HealthSummary) {
    const asOf = d.generated_at ? formatInstant(d.generated_at, 'time-seconds') : '';
    const instance = d.hub?.instance_id ?? '';
    return html`
      <div class="header">
        <div class="header-left">
          <h1>Health Dashboard</h1>
          <span class="pill overall-status tone-${healthTone(d.status)}" data-role="overall-status">
            <span class="dot" aria-hidden="true"></span>${d.status || 'unknown'}
          </span>
          ${asOf
            ? html`<span
                class="meta"
                data-role="as-of"
                title=${formatInstantWithZone(d.generated_at, 'datetime-full')}
                >as of ${asOf}</span
              >`
            : nothing}
          ${instance
            ? html`<span
                class="meta"
                data-role="instance"
                title="The hub instance that served this summary"
                >this instance: ${instance}</span
              >`
            : nothing}
        </div>
        <div class="header-right">
          <label class="toggle-label">
            <input
              type="checkbox"
              .checked=${this.autoRefresh}
              @change=${() => this.toggleAutoRefresh()}
            />
            Auto-refresh
          </label>
          <button class="refresh-btn" @click=${() => void this.fetchData()}>Refresh</button>
        </div>
      </div>
    `;
  }
}
