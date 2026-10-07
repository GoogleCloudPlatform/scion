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
 * Displays a centralized health view of the Scion system including:
 * - Hub status and version
 * - Database pool health
 * - Runtime brokers (compact table, see health-broker-table.ts)
 * - Agents (phase counts and problem groups, see health-agents-card.ts)
 * - Dispatch pipeline status
 *
 * Auto-refreshes every 30 seconds via polling.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import type { HealthSummaryBrokerList } from './health-broker-table.js';
import './health-broker-table.js';
import type { HealthSummaryAgents } from './health-agents-card.js';
import './health-agents-card.js';

export { formatHeartbeatAge } from './health-broker-table.js';

interface HealthSummary {
  status: string;
  hub: {
    status: string;
    version: string;
    uptime: string;
    connected_brokers: number;
    active_agents: number;
    projects: number;
    /** The hub's /healthz check map. */
    checks?: Record<string, string>;
    /** Non-healthy checks as "key: value" — the cause of a degraded/unhealthy hub. */
    unhealthy_checks?: string[];
  };
  database: {
    status: string;
    pool_active: number;
    pool_max: number;
    pool_wait_count_total: number;
    pool_idle: number;
  };
  runtime_brokers: HealthSummaryBrokerList;
  /** Null when the hub could not aggregate agents (not reported). */
  agents: HealthSummaryAgents | null;
  dispatch: {
    stuck_messages: number;
    failed_1h: number;
  } | null;
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
      this.data = await res.json();
      this.error = null;
    } catch (e) {
      this.error = e instanceof Error ? e.message : 'Network error';
    } finally {
      this.loading = false;
    }
  }

  private statusIcon(status: string): string {
    switch (status) {
      case 'healthy':
      case 'online':
      case 'pass':
        return '●'; // filled circle
      case 'degraded':
      case 'warn':
        return '●';
      case 'unhealthy':
      case 'offline':
      case 'fail':
      case 'error':
        return '●';
      default:
        return '○'; // empty circle
    }
  }

  private statusColor(status: string): string {
    switch (status) {
      case 'healthy':
      case 'online':
      case 'pass':
        return 'var(--scion-success, #22c55e)';
      case 'degraded':
      case 'warn':
        return 'var(--scion-warning, #f59e0b)';
      case 'unhealthy':
      case 'offline':
      case 'fail':
      case 'error':
        return 'var(--scion-error, #ef4444)';
      default:
        return 'var(--scion-text-muted, #94a3b8)';
    }
  }

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 2rem;
    }

    .header-left {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .header-right {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .refresh-btn {
      background: none;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      padding: 0.25rem 0.5rem;
      font-size: 0.75rem;
      cursor: pointer;
      color: var(--scion-text-muted, #64748b);
    }

    .refresh-btn:hover {
      background: var(--scion-surface-hover, #f1f5f9);
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

    .card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.25rem;
    }

    .card-title {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
      margin: 0 0 0.75rem 0;
    }

    .status-line {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 1rem;
      font-weight: 600;
      margin-bottom: 0.5rem;
    }

    .stat-row {
      display: flex;
      justify-content: space-between;
      font-size: 0.875rem;
      padding: 0.25rem 0;
      color: var(--scion-text, #1e293b);
    }

    .stat-row .label {
      color: var(--scion-text-muted, #64748b);
    }

    .check-problem {
      font-size: 0.8125rem;
      padding: 0.125rem 0 0.25rem;
      word-break: break-word;
    }

    .loading,
    .error-msg {
      text-align: center;
      padding: 3rem 1rem;
      color: var(--scion-text-muted, #64748b);
    }

    .error-msg {
      color: var(--scion-error, #ef4444);
    }

    .overall-status {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.25rem 0.75rem;
      border-radius: 9999px;
      font-size: 0.8125rem;
      font-weight: 600;
    }

    .overall-status.healthy {
      background: #dcfce7;
      color: #166534;
    }

    .overall-status.degraded {
      background: #fef3c7;
      color: #92400e;
    }

    .overall-status.unhealthy {
      background: #fecaca;
      color: #991b1b;
    }

    @media (max-width: 768px) {
      .grid-2 {
        grid-template-columns: 1fr;
      }
    }
  `;

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
      <div class="header">
        <div class="header-left">
          <h1>Health Dashboard</h1>
          <span class="overall-status ${d.status}">
            <span style="color: ${this.statusColor(d.status)}">${this.statusIcon(d.status)}</span>
            ${d.status}
          </span>
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

      ${this.error
        ? html`<div
            class="error-msg"
            style="margin-bottom:1rem;text-align:left;padding:0.75rem;background:#fef2f2;border-radius:0.375rem;font-size:0.875rem"
          >
            ${this.error}
          </div>`
        : nothing}

      <!-- Hub & Database -->
      <div class="grid-2">${this.renderHubCard(d)} ${this.renderDatabaseCard(d)}</div>

      <!-- Brokers -->
      ${this.renderBrokersCard(d)}

      <!-- Agents -->
      ${this.renderAgentsCard(d)}

      <!-- Dispatch -->
      <div class="grid-full">${this.renderDispatchCard(d)}</div>
    `;
  }

  private renderHubCard(d: HealthSummary) {
    return html`
      <div class="card">
        <div class="card-title">Hub Status</div>
        <div class="status-line">
          <span style="color: ${this.statusColor(d.hub.status)}"
            >${this.statusIcon(d.hub.status)}</span
          >
          ${d.hub.status}
        </div>
        ${(d.hub.unhealthy_checks ?? []).map(
          (c) =>
            html`<div class="check-problem" style="color: ${this.statusColor(d.hub.status)}">
              ${c}
            </div>`
        )}
        <div class="stat-row"><span class="label">Uptime</span><span>${d.hub.uptime}</span></div>
        <div class="stat-row"><span class="label">Version</span><span>${d.hub.version}</span></div>
        <div class="stat-row">
          <span class="label">Connected Brokers</span><span>${d.hub.connected_brokers}</span>
        </div>
        <div class="stat-row">
          <span class="label">Active Agents</span><span>${d.hub.active_agents}</span>
        </div>
        <div class="stat-row">
          <span class="label">Projects</span><span>${d.hub.projects}</span>
        </div>
      </div>
    `;
  }

  private renderDatabaseCard(d: HealthSummary) {
    const poolUtil =
      d.database.pool_max > 0
        ? Math.round((d.database.pool_active / d.database.pool_max) * 100)
        : 0;
    return html`
      <div class="card">
        <div class="card-title">Database</div>
        <div class="status-line">
          <span style="color: ${this.statusColor(d.database.status)}"
            >${this.statusIcon(d.database.status)}</span
          >
          ${d.database.status}
        </div>
        <div class="stat-row">
          <span class="label">Pool</span
          ><span>${d.database.pool_active}/${d.database.pool_max} active (${poolUtil}%)</span>
        </div>
        <div class="stat-row">
          <span class="label">Idle</span><span>${d.database.pool_idle}</span>
        </div>
        <div class="stat-row">
          <span class="label">Wait Count (Total)</span
          ><span>${d.database.pool_wait_count_total}</span>
        </div>
      </div>
    `;
  }

  private renderBrokersCard(d: HealthSummary) {
    return html`
      <div class="grid-full">
        <scion-health-broker-table .brokers=${d.runtime_brokers}></scion-health-broker-table>
      </div>
    `;
  }

  private renderAgentsCard(d: HealthSummary) {
    return html`
      <div class="grid-full">
        <scion-health-agents-card .agents=${d.agents ?? null}></scion-health-agents-card>
      </div>
    `;
  }

  private renderDispatchCard(d: HealthSummary) {
    if (!d.dispatch) {
      return html`
        <div class="card">
          <div class="card-title">Dispatch Pipeline</div>
          <div style="font-size:0.875rem;color:var(--scion-text-muted,#64748b)">
            Dispatch metrics not yet available. A future update will expose dispatch pipeline stats
            via the health summary API.
          </div>
        </div>
      `;
    }
    return html`
      <div class="card">
        <div class="card-title">Dispatch Pipeline</div>
        <div class="stat-row">
          <span class="label">Stuck Messages</span>
          <span
            style="color: ${d.dispatch.stuck_messages > 0
              ? 'var(--scion-error,#ef4444)'
              : 'inherit'}; font-weight: ${d.dispatch.stuck_messages > 0 ? '600' : 'normal'}"
          >
            ${d.dispatch.stuck_messages}
          </span>
        </div>
        <div class="stat-row">
          <span class="label">Failed (1h)</span><span>${d.dispatch.failed_1h}</span>
        </div>
      </div>
    `;
  }
}
