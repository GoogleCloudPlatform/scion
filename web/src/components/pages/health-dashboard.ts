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
 * - Broker status (per-broker cards)
 * - Agent health summary
 * - Dispatch pipeline status
 * - Stall detection configuration (editable)
 *
 * Auto-refreshes every 30 seconds via polling.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { showToast } from '../../utils/toast.js';

interface HealthSummary {
  status: string;
  hub: {
    status: string;
    version: string;
    uptime: string;
    connected_brokers: number;
    active_agents: number;
    projects: number;
  };
  database: {
    status: string;
    pool_active: number;
    pool_max: number;
    pool_wait_count_total: number;
    pool_idle: number;
  };
  brokers: Array<{
    id: string;
    name: string;
    status: string;
    runtime: string;
    runtime_available: boolean;
    agent_count: number;
    agent_healthy: number;
    last_heartbeat: string;
  }>;
  agents: {
    total: number;
    by_phase: Record<string, number>;
    stalled: string[];
    crashed: string[];
    errored: string[];
  };
  dispatch: {
    stuck_messages: number;
    failed_1h: number;
  } | null;
  stall_config: {
    threshold_seconds: number;
    auto_suspend: boolean;
  };
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

  @state()
  private editingStall = false;

  @state()
  private stallAutoSuspend = false;

  @state()
  private savingStall = false;

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
      // Sync stall config editor state
      if (this.data && !this.editingStall) {
        this.stallAutoSuspend = this.data.stall_config.auto_suspend;
      }
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

  private async saveStallConfig(): Promise<void> {
    this.savingStall = true;
    try {
      const settings: Record<string, unknown> = {
        'server.hub.auto_suspend_stalled': this.stallAutoSuspend,
      };
      const res = await apiFetch('/api/v1/admin/server-config', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(settings),
      });
      if (!res.ok) {
        const msg = await extractApiError(res, 'Failed to save stall settings');
        showToast(msg, 'danger');
        return;
      }
      showToast('Stall detection settings saved', 'success');
      this.editingStall = false;
      void this.fetchData();
    } catch (e) {
      showToast(e instanceof Error ? e.message : 'Save failed', 'danger');
    } finally {
      this.savingStall = false;
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

    .broker-grid {
      display: flex;
      flex-wrap: wrap;
      gap: 1rem;
    }

    .broker-card {
      background: var(--scion-surface-alt, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.5rem;
      padding: 1rem;
      min-width: 200px;
      flex: 1;
    }

    .broker-name {
      font-weight: 600;
      margin-bottom: 0.5rem;
    }

    .broker-stat {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      padding: 0.125rem 0;
    }

    .agent-summary {
      display: flex;
      gap: 1.5rem;
      flex-wrap: wrap;
      margin-bottom: 0.75rem;
      font-size: 0.9375rem;
    }

    .agent-summary .stat {
      font-weight: 600;
    }

    .agent-alert {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      font-size: 0.875rem;
      padding: 0.25rem 0;
    }

    .alert-warn {
      color: var(--scion-warning, #f59e0b);
    }

    .alert-error {
      color: var(--scion-error, #ef4444);
    }

    .stall-config {
      display: flex;
      align-items: center;
      gap: 1rem;
      flex-wrap: wrap;
    }

    .stall-config .stat-row {
      flex: 1;
      min-width: 200px;
    }

    .edit-btn {
      background: none;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      padding: 0.25rem 0.625rem;
      font-size: 0.75rem;
      cursor: pointer;
      color: var(--scion-text-muted, #64748b);
    }

    .edit-btn:hover {
      background: var(--scion-surface-hover, #f1f5f9);
    }

    .stall-edit-form {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      flex-wrap: wrap;
    }

    .stall-edit-form label {
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .stall-edit-form input[type='number'] {
      width: 60px;
      padding: 0.25rem 0.5rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.25rem;
      font-size: 0.875rem;
    }

    .save-btn {
      background: var(--scion-primary, #3b82f6);
      color: white;
      border: none;
      border-radius: 0.375rem;
      padding: 0.375rem 0.75rem;
      font-size: 0.8125rem;
      cursor: pointer;
    }

    .save-btn:hover {
      opacity: 0.9;
    }

    .save-btn:disabled {
      opacity: 0.5;
      cursor: not-allowed;
    }

    .cancel-btn {
      background: none;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      padding: 0.375rem 0.75rem;
      font-size: 0.8125rem;
      cursor: pointer;
      color: var(--scion-text-muted, #64748b);
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

    .alerts-link {
      font-size: 0.875rem;
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
    }

    .alerts-link:hover {
      text-decoration: underline;
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

      <!-- Dispatch & Stall Config -->
      <div class="grid-2">${this.renderDispatchCard(d)} ${this.renderStallCard(d)}</div>

      <!-- Recent Alerts placeholder -->
      <div class="grid-full">
        <div class="card">
          <div class="card-title">Recent Alerts</div>
          <div style="font-size:0.875rem;color:var(--scion-text-muted,#64748b)">
            View recent alerts in the
            <a
              class="alerts-link"
              href="https://console.cloud.google.com/monitoring/alerting"
              target="_blank"
              rel="noopener"
            >
              GCP Cloud Monitoring Console
            </a>
          </div>
        </div>
      </div>
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
    if (d.brokers.length === 0) {
      return html`
        <div class="grid-full">
          <div class="card">
            <div class="card-title">Brokers</div>
            <div style="font-size:0.875rem;color:var(--scion-text-muted,#64748b)">
              No brokers registered
            </div>
          </div>
        </div>
      `;
    }

    return html`
      <div class="grid-full">
        <div class="card">
          <div class="card-title">Brokers</div>
          <div class="broker-grid">
            ${d.brokers.map(
              (b) => html`
                <div class="broker-card">
                  <div class="broker-name">${b.name || b.id}</div>
                  <div class="status-line" style="font-size:0.875rem">
                    <span style="color: ${this.statusColor(b.status)}"
                      >${this.statusIcon(b.status)}</span
                    >
                    ${b.status}
                  </div>
                  <div class="broker-stat">Agents: ${b.agent_healthy}/${b.agent_count} healthy</div>
                  <div class="broker-stat">
                    Runtime: ${b.runtime} ${b.runtime_available ? '✓' : '✗'}
                  </div>
                  <div class="broker-stat" style="color:var(--scion-text-muted,#64748b)">
                    NFS: not reported
                  </div>
                  ${b.last_heartbeat
                    ? html`<div class="broker-stat">
                        Heartbeat: ${this.timeAgo(b.last_heartbeat)}
                      </div>`
                    : nothing}
                </div>
              `
            )}
          </div>
        </div>
      </div>
    `;
  }

  private renderAgentsCard(d: HealthSummary) {
    return html`
      <div class="grid-full">
        <div class="card">
          <div class="card-title">Agents</div>
          <div class="agent-summary">
            <div><span class="stat">${d.agents.total}</span> Total</div>
            ${Object.entries(d.agents.by_phase).map(
              ([phase, count]) => html`<div><span class="stat">${count}</span> ${phase}</div>`
            )}
          </div>
          ${d.agents.stalled.length > 0
            ? html`
                <div class="agent-alert alert-warn">
                  ⚠ ${d.agents.stalled.length} stalled: ${d.agents.stalled.join(', ')}
                </div>
              `
            : nothing}
          ${d.agents.crashed.length > 0
            ? html`
                <div class="agent-alert alert-error">
                  ✗ ${d.agents.crashed.length} crashed: ${d.agents.crashed.join(', ')}
                </div>
              `
            : nothing}
          ${d.agents.errored.length > 0
            ? html`
                <div class="agent-alert alert-error">
                  ✗ ${d.agents.errored.length} errored: ${d.agents.errored.join(', ')}
                </div>
              `
            : nothing}
          ${d.agents.stalled.length === 0 &&
          d.agents.crashed.length === 0 &&
          d.agents.errored.length === 0
            ? html`<div style="font-size:0.875rem;color:var(--scion-success,#22c55e)">
                All agents healthy
              </div>`
            : nothing}
        </div>
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

  private renderStallCard(d: HealthSummary) {
    if (this.editingStall) {
      return html`
        <div class="card">
          <div class="card-title">Stall Detection Settings</div>
          <!-- Threshold is a startup-time ServerConfig setting and cannot be
               changed at runtime via the operational settings API. Display it
               as read-only. Only auto_suspend_stalled is a runtime setting. -->
          <div class="stat-row" style="margin-bottom:0.75rem">
            <span class="label">Stalled Threshold</span>
            <span
              >${Math.round(d.stall_config.threshold_seconds / 60)} min
              <span style="font-size:0.75rem;color:var(--scion-text-muted,#94a3b8)"
                >(set at startup)</span
              ></span
            >
          </div>
          <div class="stall-edit-form">
            <label style="display:flex;align-items:center;gap:0.375rem">
              <input
                type="checkbox"
                .checked=${this.stallAutoSuspend}
                @change=${() => {
                  this.stallAutoSuspend = !this.stallAutoSuspend;
                }}
              />
              Auto-suspend stalled agents
            </label>
            <button
              class="save-btn"
              ?disabled=${this.savingStall}
              @click=${() => void this.saveStallConfig()}
            >
              ${this.savingStall ? 'Saving...' : 'Save'}
            </button>
            <button
              class="cancel-btn"
              @click=${() => {
                this.editingStall = false;
              }}
            >
              Cancel
            </button>
          </div>
        </div>
      `;
    }

    return html`
      <div class="card">
        <div
          class="card-title"
          style="display:flex;justify-content:space-between;align-items:center"
        >
          Stall Detection Settings
          <button
            class="edit-btn"
            @click=${() => {
              this.editingStall = true;
            }}
          >
            Edit
          </button>
        </div>
        <div class="stat-row">
          <span class="label">Stalled Threshold</span
          ><span>${Math.round(d.stall_config.threshold_seconds / 60)} min</span>
        </div>
        <div class="stat-row">
          <span class="label">Auto-Suspend Stalled</span
          ><span>${d.stall_config.auto_suspend ? 'enabled' : 'disabled'}</span>
        </div>
      </div>
    `;
  }

  private timeAgo(isoDate: string): string {
    if (!isoDate) return 'never';
    try {
      const d = new Date(isoDate);
      if (isNaN(d.getTime())) return 'unknown';
      const now = Date.now();
      const diffMs = now - d.getTime();
      if (diffMs < 0) return 'just now';
      const seconds = Math.floor(diffMs / 1000);
      if (seconds < 60) return `${seconds}s ago`;
      const minutes = Math.floor(seconds / 60);
      if (minutes < 60) return `${minutes}m ago`;
      const hours = Math.floor(minutes / 60);
      if (hours < 24) return `${hours}h ago`;
      const days = Math.floor(hours / 24);
      return `${days}d ago`;
    } catch {
      return 'unknown';
    }
  }
}
