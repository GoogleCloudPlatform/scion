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
 * Agent Schedules Tab Component
 *
 * Displays recurring schedules related to a single agent on the agent detail
 * page, grouped into schedules created by this agent and schedules targeting
 * this agent. Each row links to the schedule's detail card on the project
 * schedules page.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { paginateAll, PaginationError } from '../../client/paginate-all.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { resourceStyles } from './resource-styles.js';
import {
  SCHEDULE_PAGE_SIZE,
  parseScheduleWake,
  scheduleStatusReasonLabel,
  scheduleZoneLabel,
} from './schedule-list.js';

export interface AgentScheduleItem {
  id: string;
  projectId: string;
  name: string;
  cronExpr: string;
  timeZone?: string;
  eventType: string;
  payload: string;
  status: string;
  statusReason?: string;
  nextRunAt?: string;
  lastRunAt?: string;
  lastRunStatus?: string;
  lastRunError?: string;
  runCount: number;
  errorCount: number;
  createdAt: string;
  createdBy?: string;
}

interface ListSchedulesResponse {
  schedules?: AgentScheduleItem[];
  nextCursor?: string;
  totalCount?: number;
}

/** Build the deep-link URL to a schedule's detail card on the project schedules page. */
export function scheduleDetailHref(projectId: string, scheduleId: string): string {
  return `/projects/${encodeURIComponent(projectId)}/schedules?schedule=${encodeURIComponent(scheduleId)}`;
}

@customElement('scion-agent-schedules-tab')
export class ScionAgentSchedulesTab extends LitElement {
  /** Re-renders next-run instants when the display timezone changes. */
  readonly _zone = new DisplayZoneController(this);

  @property() projectId = '';
  @property() agentId = '';

  @state() private loading = false;
  @state() private loaded = false;
  @state() private error: string | null = null;
  @state() private createdSchedules: AgentScheduleItem[] = [];
  @state() private targetingSchedules: AgentScheduleItem[] = [];

  private loadGeneration = 0;
  private loadedForKey = '';

  static override styles = [
    resourceStyles,
    css`
      :host {
        display: block;
      }

      .toolbar {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 0.75rem;
        margin-bottom: 1rem;
      }

      .toolbar-summary {
        font-size: 0.875rem;
        color: var(--scion-text-muted);
      }

      .groups {
        display: flex;
        flex-direction: column;
        gap: 1.5rem;
      }

      .group-section {
        background: var(--scion-surface);
        border: 1px solid var(--scion-border);
        border-radius: var(--scion-radius-lg);
        overflow: hidden;
      }

      .group-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 0.5rem;
        padding: 0.875rem 1rem;
        border-bottom: 1px solid var(--scion-border);
        background: var(--scion-bg-subtle);
      }

      .group-header h3 {
        margin: 0;
        font-size: 0.9375rem;
        font-weight: 600;
        color: var(--scion-text);
        display: flex;
        align-items: center;
        gap: 0.5rem;
      }

      .group-empty {
        padding: 1.5rem 1rem;
        text-align: center;
        font-size: 0.875rem;
        color: var(--scion-text-muted);
      }

      .schedule-link {
        color: var(--scion-primary);
        text-decoration: none;
        font-weight: 500;
      }

      .schedule-link:hover {
        text-decoration: underline;
      }

      .type-cell {
        display: inline-flex;
        align-items: center;
        gap: 0.375rem;
        flex-wrap: wrap;
      }

      .cron-expr {
        font-family: var(--scion-font-mono);
        font-size: 0.8125rem;
      }

      .sub-meta {
        font-size: 0.75rem;
        color: var(--scion-text-muted);
        margin-top: 0.125rem;
      }

      .outcome-error {
        color: var(--sl-color-danger-600);
      }

      .outcome-success {
        color: var(--sl-color-success-700);
      }

      .outcome-skipped {
        color: var(--sl-color-warning-700);
      }

      .status-reason {
        font-size: 0.75rem;
        color: var(--scion-text-muted);
        margin-top: 0.125rem;
      }
    `,
  ];

  /** Load schedules related to this agent. Skips repeat fetches unless `force` is true. */
  async loadSchedules(force = false): Promise<void> {
    if (!this.projectId || !this.agentId) return;
    const key = `${this.projectId}:${this.agentId}`;
    if (!force && this.loadedForKey === key && this.loaded) {
      return;
    }

    const generation = ++this.loadGeneration;
    this.loading = true;
    this.error = null;

    const basePath = `/api/v1/projects/${encodeURIComponent(this.projectId)}/schedules`;
    const createdPath = `${basePath}?createdBy=${encodeURIComponent(this.agentId)}`;
    const targetingPath = `${basePath}?targetAgentId=${encodeURIComponent(this.agentId)}`;

    const parsePage = (body: unknown) => {
      const data = body as ListSchedulesResponse;
      return { items: data.schedules ?? [], nextCursor: data.nextCursor ?? '' };
    };

    try {
      const [created, targeting] = await Promise.all([
        paginateAll<AgentScheduleItem>({
          path: createdPath,
          pageSize: SCHEDULE_PAGE_SIZE,
          label: 'agent created schedules',
          parsePage,
          shouldContinue: () => generation === this.loadGeneration,
        }),
        paginateAll<AgentScheduleItem>({
          path: targetingPath,
          pageSize: SCHEDULE_PAGE_SIZE,
          label: 'agent targeting schedules',
          parsePage,
          shouldContinue: () => generation === this.loadGeneration,
        }),
      ]);
      if (generation !== this.loadGeneration) return;
      this.createdSchedules = created;
      this.targetingSchedules = targeting;
      this.loaded = true;
      this.loadedForKey = key;
    } catch (err) {
      if (generation !== this.loadGeneration) return;
      this.error =
        err instanceof PaginationError && err.hubMessage
          ? err.hubMessage
          : err instanceof Error
            ? err.message
            : 'Failed to load schedules';
    } finally {
      if (generation === this.loadGeneration) {
        this.loading = false;
      }
    }
  }

  private formatNextRun(dateString: string | undefined): string {
    if (!dateString) return '-';
    const ms = new Date(dateString).getTime();
    if (!Number.isNaN(ms) && ms <= Date.now()) return 'now';
    return formatRelative(dateString);
  }

  private statusBadgeClass(status: string): string {
    switch (status) {
      case 'active':
        return 'variable';
      case 'paused':
        return 'inject-as-needed';
      default:
        return '';
    }
  }

  private renderTypeCell(sched: AgentScheduleItem) {
    const wakes = parseScheduleWake(sched) === true;
    return html`
      <span class="type-cell">
        <span class="badge">${sched.eventType}</span>
        ${wakes ? html`<span class="badge variable">wake</span>` : nothing}
      </span>
    `;
  }

  private renderScheduleTiming(sched: AgentScheduleItem) {
    const zone = scheduleZoneLabel(sched.timeZone);
    return html`
      <div class="cron-expr">${sched.cronExpr}</div>
      <div class="sub-meta">
        ${zone}${sched.nextRunAt
          ? html` · Next: ${this.formatNextRun(sched.nextRunAt)}
            (${formatInstantWithZone(sched.nextRunAt)})`
          : ''}
      </div>
    `;
  }

  private renderStatusCell(sched: AgentScheduleItem) {
    const reason = sched.status === 'paused' ? scheduleStatusReasonLabel(sched.statusReason) : '';
    return html`
      <span class="badge ${this.statusBadgeClass(sched.status)}">${sched.status}</span>
      ${reason ? html`<div class="status-reason">${reason}</div>` : nothing}
    `;
  }

  private renderOutcomeCell(sched: AgentScheduleItem) {
    if (!sched.lastRunAt && !sched.lastRunStatus) {
      return html`<span class="meta-text">Never run</span>`;
    }
    const status = sched.lastRunStatus || 'unknown';
    const cls =
      status === 'error'
        ? 'outcome-error'
        : status === 'success'
          ? 'outcome-success'
          : status === 'skipped'
            ? 'outcome-skipped'
            : '';
    return html`
      <div class=${cls} title=${sched.lastRunError || ''}>${status}</div>
      ${sched.lastRunAt
        ? html`<div class="sub-meta">${formatRelative(sched.lastRunAt)}</div>`
        : nothing}
      ${sched.lastRunError
        ? html`<div class="sub-meta outcome-error">${sched.lastRunError}</div>`
        : nothing}
    `;
  }

  private renderGroup(
    title: string,
    emptyText: string,
    testId: string,
    items: AgentScheduleItem[]
  ) {
    return html`
      <section class="group-section" data-testid=${testId}>
        <div class="group-header">
          <h3>
            ${title}
            <sl-badge variant="neutral" pill>${items.length}</sl-badge>
          </h3>
        </div>
        ${items.length === 0
          ? html`<div class="group-empty">${emptyText}</div>`
          : html`
              <div class="table-container">
                <table>
                  <thead>
                    <tr>
                      <th>Name</th>
                      <th>Type</th>
                      <th>Schedule / Next Run</th>
                      <th>Status</th>
                      <th>Last Outcome</th>
                    </tr>
                  </thead>
                  <tbody>
                    ${items.map(
                      (sched) => html`
                        <tr>
                          <td>
                            <a
                              class="schedule-link"
                              href=${scheduleDetailHref(this.projectId, sched.id)}
                              >${sched.name}</a
                            >
                          </td>
                          <td>${this.renderTypeCell(sched)}</td>
                          <td>${this.renderScheduleTiming(sched)}</td>
                          <td>${this.renderStatusCell(sched)}</td>
                          <td>${this.renderOutcomeCell(sched)}</td>
                        </tr>
                      `
                    )}
                  </tbody>
                </table>
              </div>
            `}
      </section>
    `;
  }

  override render() {
    if (this.loading && !this.loaded) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading schedules...</p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load Schedules</h2>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${() => void this.loadSchedules(true)}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    return html`
      <div class="toolbar">
        <span class="toolbar-summary">
          Recurring schedules in this project created by or targeting this agent.
        </span>
        <sl-button
          size="small"
          variant="default"
          ?loading=${this.loading}
          @click=${() => void this.loadSchedules(true)}
        >
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Refresh
        </sl-button>
      </div>

      <div class="groups">
        ${this.renderGroup(
          'Created by this agent',
          'No schedules created by this agent.',
          'agent-schedules-created',
          this.createdSchedules
        )}
        ${this.renderGroup(
          'Targeting this agent',
          'No schedules targeting this agent.',
          'agent-schedules-targeting',
          this.targetingSchedules
        )}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-agent-schedules-tab': ScionAgentSchedulesTab;
  }
}
