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
 * Admin Scheduler page component
 *
 * Read-only view of the Hub scheduler: recurring handlers, event handlers,
 * and recent scheduled events across all projects.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';

interface RecurringHandlerInfo {
  name: string;
  intervalMinutes: number;
}

interface SchedulerInfo {
  tickCount: number;
  tickInterval: string;
  recurringHandlers: RecurringHandlerInfo[];
  eventHandlers: string[];
  activeTimers: number;
}

interface ScheduledEvent {
  id: string;
  projectId: string;
  eventType: string;
  fireAt: string;
  status: string;
  createdAt: string;
  createdBy: string;
  firedAt?: string;
  error?: string;
}

interface RecurringSchedule {
  id: string;
  projectId: string;
  name: string;
  cronExpr: string;
  eventType: string;
  status: string;
  nextRunAt?: string;
  lastRunAt?: string;
  lastRunStatus?: string;
  lastRunError?: string;
  runCount: number;
  errorCount: number;
}

interface SchedulerResponse {
  status?: string;
  scheduler?: SchedulerInfo;
  scheduledEvents?: ScheduledEvent[] | null;
  recurringSchedules?: RecurringSchedule[] | null;
  serverTime?: string;
}

@customElement('scion-page-admin-scheduler')
export class ScionPageAdminScheduler extends LitElement {
  @state()
  private loading = true;

  @state()
  private data: SchedulerResponse | null = null;

  @state()
  private error: string | null = null;

  @state()
  private cancellingId: string | null = null;

  @state()
  private allSchedules: RecurringSchedule[] = [];

  @state()
  private projectMap: Map<string, { name: string; slug: string }> = new Map();

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 2rem;
    }

    .header sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    /* ── Sections ───────────────────────────────────────────────────── */

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .section-title {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .section-description {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    /* ── Overview stats ──────────────────────────────────────────────── */

    .stats-row {
      display: flex;
      gap: 1.5rem;
      flex-wrap: wrap;
    }

    .stat-card {
      display: flex;
      flex-direction: column;
      padding: 1rem 1.25rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
      min-width: 140px;
    }

    .stat-label {
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.25rem;
    }

    .stat-value {
      font-size: 1.25rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
    }

    /* ── Table ───────────────────────────────────────────────────────── */

    .table-container {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      overflow: hidden;
    }

    table {
      width: 100%;
      border-collapse: collapse;
    }

    th {
      text-align: left;
      padding: 0.75rem 1rem;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }

    tr:last-child td {
      border-bottom: none;
    }

    .mono {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.8125rem;
    }

    .project-link {
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
      font-weight: 500;
    }

    .project-link:hover {
      text-decoration: underline;
    }

    .meta-text {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* ── Status badges ──────────────────────────────────────────────── */

    .status-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
    }

    .status-badge.pending {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #a16207);
    }

    .status-badge.fired {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .status-badge.cancelled {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    .status-badge.expired {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .status-badge.active {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .status-badge.paused {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #a16207);
    }

    .type-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.6875rem;
      font-weight: 500;
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .error-text {
      font-size: 0.75rem;
      color: var(--sl-color-danger-600, #dc2626);
      max-width: 200px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    /* ── Tag list (event handlers) ──────────────────────────────────── */

    .tag-list {
      display: flex;
      gap: 0.5rem;
      flex-wrap: wrap;
    }

    .tag {
      display: inline-flex;
      align-items: center;
      padding: 0.25rem 0.625rem;
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.8125rem;
      font-weight: 500;
      font-family: var(--scion-font-mono, monospace);
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text, #1e293b);
      border: 1px solid var(--scion-border, #e2e8f0);
    }

    /* ── Empty state ─────────────────────────────────────────────────── */

    .empty-inline {
      padding: 1.5rem;
      text-align: center;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }

    /* ── Loading / Error ─────────────────────────────────────────────── */

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 4rem 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .loading-state sl-spinner {
      font-size: 2rem;
      margin-bottom: 1rem;
    }

    .error-state {
      text-align: center;
      padding: 3rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .error-state sl-icon {
      font-size: 3rem;
      color: var(--sl-color-danger-500, #ef4444);
      margin-bottom: 1rem;
    }

    .error-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .error-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .error-details {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      color: var(--sl-color-danger-700, #b91c1c);
      margin-bottom: 1rem;
    }

    @media (max-width: 768px) {
      .hide-mobile {
        display: none;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadSchedulerStatus();
  }

  private async loadSchedulerStatus(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const [response, projectsResponse] = await Promise.all([
        apiFetch('/api/v1/admin/scheduler'),
        apiFetch('/api/v1/projects'),
      ]);

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      const data = (await response.json()) as SchedulerResponse;
      this.data = data;
      this.allSchedules = data.recurringSchedules ?? [];

      // Build project ID -> name/slug lookup map
      if (projectsResponse.ok) {
        const projectsBody = (await projectsResponse.json()) as { projects: { id: string; name: string; slug?: string }[] };
        const projectsData = projectsBody.projects ?? [];
        const map = new Map<string, { name: string; slug: string }>();
        for (const p of projectsData) {
          map.set(p.id, { name: p.name, slug: p.slug ?? p.id });
        }
        this.projectMap = map;
      }
    } catch (err) {
      console.error('Failed to load scheduler status:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load scheduler status';
    } finally {
      this.loading = false;
    }
  }

  private formatRelativeTime(dateString: string | undefined): string {
    if (!dateString) return 'Never';
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return 'Never';
      const diffMs = Date.now() - date.getTime();
      const diffSeconds = Math.round(diffMs / 1000);
      const diffMinutes = Math.round(diffMs / (1000 * 60));
      const diffHours = Math.round(diffMs / (1000 * 60 * 60));
      const diffDays = Math.round(diffMs / (1000 * 60 * 60 * 24));

      const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

      if (Math.abs(diffSeconds) < 60) {
        return rtf.format(-diffSeconds, 'second');
      } else if (Math.abs(diffMinutes) < 60) {
        return rtf.format(-diffMinutes, 'minute');
      } else if (Math.abs(diffHours) < 24) {
        return rtf.format(-diffHours, 'hour');
      } else {
        return rtf.format(-diffDays, 'day');
      }
    } catch {
      return dateString;
    }
  }

  private formatFutureTime(dateString: string): string {
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return dateString;
      const diffMs = date.getTime() - Date.now();
      if (diffMs <= 0) return 'now';
      const diffSeconds = Math.round(diffMs / 1000);
      const diffMinutes = Math.round(diffMs / (1000 * 60));
      const diffHours = Math.round(diffMs / (1000 * 60 * 60));

      const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

      if (Math.abs(diffSeconds) < 60) {
        return rtf.format(diffSeconds, 'second');
      } else if (Math.abs(diffMinutes) < 60) {
        return rtf.format(diffMinutes, 'minute');
      } else {
        return rtf.format(diffHours, 'hour');
      }
    } catch {
      return dateString;
    }
  }

  private renderProjectCell(projectId: string) {
    const project = this.projectMap.get(projectId);
    if (project) {
      return html`<a class="project-link" href="/projects/${encodeURIComponent(project.slug)}">${project.name}</a>`;
    }
    // Fallback to truncated ID if project not found
    return html`<span class="mono">${projectId.length > 12 ? projectId.slice(0, 12) + '...' : projectId}</span>`;
  }

  private formatInterval(minutes: number): string {
    if (minutes < 60) return `${minutes}m`;
    const h = Math.floor(minutes / 60);
    const m = minutes % 60;
    return m > 0 ? `${h}h ${m}m` : `${h}h`;
  }

  private async handleCancelEvent(evt: ScheduledEvent): Promise<void> {
    this.cancellingId = evt.id;
    try {
      const response = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(evt.projectId)}/scheduled-events/${encodeURIComponent(evt.id)}`,
        { method: 'DELETE' }
      );
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      // Reload data to reflect the cancellation
      await this.loadSchedulerStatus();
    } catch (err) {
      console.error('Failed to cancel event:', err);
      this.error = err instanceof Error ? err.message : 'Failed to cancel event';
    } finally {
      this.cancellingId = null;
    }
  }

  override render() {
    return html`
      <div class="header">
        <sl-icon name="clock"></sl-icon>
        <h1>Scheduler</h1>
      </div>

      ${this.loading
        ? this.renderLoading()
        : this.error
          ? this.renderError()
          : this.renderContent()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading scheduler status...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Scheduler</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadSchedulerStatus()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderContent() {
    if (!this.data) return nothing;

    const { scheduler, scheduledEvents } = this.data;

    if (!scheduler) {
      return html`
        <div class="section">
          <h2 class="section-title">Scheduler Not Available</h2>
          <p class="section-description">
            The scheduler has not been initialized on this server.
          </p>
        </div>
      `;
    }

    const events = scheduledEvents ?? [];

    return html`
      ${this.renderOverview(scheduler)}
      ${this.renderRecurringHandlers(scheduler.recurringHandlers)}
      ${this.renderEventHandlers(scheduler.eventHandlers)}
      ${this.renderRecurringSchedules()}
      ${this.renderScheduledEvents(events)}
    `;
  }

  private renderOverview(scheduler: SchedulerInfo) {
    return html`
      <div class="section">
        <h2 class="section-title">Overview</h2>
        <p class="section-description">Current scheduler state and counters.</p>
        <div class="stats-row">
          <div class="stat-card">
            <span class="stat-label">Tick Count</span>
            <span class="stat-value">${scheduler.tickCount.toLocaleString()}</span>
          </div>
          <div class="stat-card">
            <span class="stat-label">Tick Interval</span>
            <span class="stat-value">${scheduler.tickInterval}</span>
          </div>
          <div class="stat-card">
            <span class="stat-label">Active Timers</span>
            <span class="stat-value">${scheduler.activeTimers}</span>
          </div>
          <div class="stat-card">
            <span class="stat-label">Recurring Tasks</span>
            <span class="stat-value">${scheduler.recurringHandlers.length}</span>
          </div>
        </div>
      </div>
    `;
  }

  private renderRecurringHandlers(handlers: RecurringHandlerInfo[]) {
    return html`
      <div class="section">
        <h2 class="section-title">Recurring Handlers</h2>
        <p class="section-description">
          Periodic tasks driven by the root ticker. All handlers run on startup (tick 0), then at
          their configured interval.
        </p>
        ${handlers.length === 0
          ? html`<div class="empty-inline">No recurring handlers registered.</div>`
          : html`
              <div class="table-container">
                <table>
                  <thead>
                    <tr>
                      <th>Name</th>
                      <th>Interval</th>
                    </tr>
                  </thead>
                  <tbody>
                    ${handlers.map(
                      (h) => html`
                        <tr>
                          <td class="mono">${h.name}</td>
                          <td>
                            <span class="meta-text"
                              >Every ${this.formatInterval(h.intervalMinutes)}</span
                            >
                          </td>
                        </tr>
                      `
                    )}
                  </tbody>
                </table>
              </div>
            `}
      </div>
    `;
  }

  private renderEventHandlers(handlers: string[]) {
    return html`
      <div class="section">
        <h2 class="section-title">Event Handlers</h2>
        <p class="section-description">
          Registered one-shot event type handlers for scheduled events.
        </p>
        ${handlers.length === 0
          ? html`<div class="empty-inline">No event handlers registered.</div>`
          : html`
              <div class="tag-list">
                ${handlers.map((h) => html`<span class="tag">${h}</span>`)}
              </div>
            `}
      </div>
    `;
  }

  private renderScheduledEvents(events: ScheduledEvent[]) {
    return html`
      <div class="section">
        <h2 class="section-title">Scheduled Events</h2>
        <p class="section-description">Recent one-shot scheduled events across all projects.</p>
        ${events.length === 0
          ? html`<div class="empty-inline">No scheduled events.</div>`
          : html`
              <div class="table-container">
                <table>
                  <thead>
                    <tr>
                      <th>Type</th>
                      <th>Status</th>
                      <th>Fire At</th>
                      <th class="hide-mobile">Project</th>
                      <th class="hide-mobile">Created</th>
                      <th class="hide-mobile">Error</th>
                      <th>Actions</th>
                    </tr>
                  </thead>
                  <tbody>
                    ${events.map((evt) => this.renderEventRow(evt))}
                  </tbody>
                </table>
              </div>
            `}
      </div>
    `;
  }

  private renderRecurringSchedules() {
    return html`
      <div class="section">
        <h2 class="section-title">Recurring Schedules</h2>
        <p class="section-description">User-defined recurring schedules across all projects.</p>
        ${this.allSchedules.length === 0
          ? html`<div class="empty-inline">No recurring schedules.</div>`
          : html`
              <div class="table-container">
                <table>
                  <thead>
                    <tr>
                      <th>Name</th>
                      <th>Type</th>
                      <th>Cron</th>
                      <th>Status</th>
                      <th>Next Run</th>
                      <th class="hide-mobile">Project</th>
                      <th class="hide-mobile">Runs</th>
                      <th class="hide-mobile">Last Run</th>
                    </tr>
                  </thead>
                  <tbody>
                    ${this.allSchedules.map(
                      (sched) => html`
                        <tr>
                          <td><strong>${sched.name}</strong></td>
                          <td><span class="type-badge">${sched.eventType}</span></td>
                          <td><span class="mono">${sched.cronExpr}</span></td>
                          <td><span class="status-badge ${sched.status}">${sched.status}</span></td>
                          <td>
                            <span class="meta-text">
                              ${sched.status === 'active' && sched.nextRunAt
                                ? this.formatFutureTime(sched.nextRunAt)
                                : '-'}
                            </span>
                          </td>
                          <td class="hide-mobile">
                            ${this.renderProjectCell(sched.projectId)}
                          </td>
                          <td class="hide-mobile">
                            <span class="meta-text">
                              ${sched.runCount}${sched.errorCount > 0
                                ? ` (${sched.errorCount} err)`
                                : ''}
                            </span>
                          </td>
                          <td class="hide-mobile">
                            ${sched.lastRunAt
                              ? html`
                                  <span class="meta-text">
                                    ${this.formatRelativeTime(sched.lastRunAt)}
                                    ${sched.lastRunStatus === 'error'
                                      ? html`<span class="error-text" title=${sched.lastRunError || ''}> (error)</span>`
                                      : ''}
                                  </span>
                                `
                              : html`<span class="meta-text">-</span>`}
                          </td>
                        </tr>
                      `
                    )}
                  </tbody>
                </table>
              </div>
            `}
      </div>
    `;
  }

  private renderEventRow(evt: ScheduledEvent) {
    const isPending = evt.status === 'pending';
    const isCancelling = this.cancellingId === evt.id;
    const fireTimeDisplay = isPending
      ? this.formatFutureTime(evt.fireAt)
      : this.formatRelativeTime(evt.firedAt ?? evt.fireAt);

    return html`
      <tr>
        <td><span class="type-badge">${evt.eventType}</span></td>
        <td><span class="status-badge ${evt.status}">${evt.status}</span></td>
        <td><span class="meta-text">${fireTimeDisplay}</span></td>
        <td class="hide-mobile">
          ${this.renderProjectCell(evt.projectId)}
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${this.formatRelativeTime(evt.createdAt)}</span>
        </td>
        <td class="hide-mobile">
          ${evt.error
            ? html`<span class="error-text" title=${evt.error}>${evt.error}</span>`
            : html`<span class="meta-text">-</span>`}
        </td>
        <td>
          ${isPending
            ? html`
                <sl-icon-button
                  name="x-circle"
                  label="Cancel event"
                  ?disabled=${isCancelling}
                  @click=${() => this.handleCancelEvent(evt)}
                ></sl-icon-button>
              `
            : nothing}
        </td>
      </tr>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-scheduler': ScionPageAdminScheduler;
  }
}
