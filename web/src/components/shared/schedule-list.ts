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
 * Shared Recurring Schedule List Component
 *
 * Displays recurring schedules for a project with create, pause/resume, and delete actions.
 * Used by the project-schedules page and admin-scheduler page.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { resourceStyles } from './resource-styles.js';

interface Schedule {
  id: string;
  projectId: string;
  name: string;
  cronExpr: string;
  eventType: string;
  payload: string;
  status: string;
  nextRunAt?: string;
  lastRunAt?: string;
  lastRunStatus?: string;
  lastRunError?: string;
  runCount: number;
  errorCount: number;
  createdAt: string;
  createdBy?: string;
}

interface ListResponse {
  schedules: Schedule[];
  totalCount?: number;
  serverTime?: string;
}

@customElement('scion-schedule-list')
export class ScionScheduleList extends LitElement {
  @property() projectId = '';
  @property({ type: Boolean }) compact = false;

  @state() private loading = true;
  @state() private schedules: Schedule[] = [];
  @state() private error: string | null = null;

  // Create dialog
  @state() private dialogOpen = false;
  @state() private dialogName = '';
  @state() private dialogCron = '';
  @state() private dialogEventType = 'message';
  @state() private dialogAgent = '';
  @state() private dialogMessage = '';
  @state() private dialogInterrupt = false;
  @state() private dialogTemplate = '';
  @state() private dialogTask = '';
  @state() private dialogBranch = '';
  @state() private dialogLoading = false;
  @state() private dialogError: string | null = null;

  // Action state
  @state() private actionId: string | null = null;

  // Detail dialog
  @state() private detailSchedule: Schedule | null = null;
  @state() private detailOpen = false;

  static override styles = [resourceStyles, css`
    .detail-row {
      padding: 0.375rem 0;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      line-height: 1.5;
    }
    .detail-row strong {
      display: inline-block;
      min-width: 100px;
      color: var(--scion-text-muted, #64748b);
      font-weight: 600;
      font-size: 0.8125rem;
    }
  `];

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadSchedules();
  }

  private async loadSchedules(): Promise<void> {
    if (!this.projectId) return;
    this.loading = true;
    this.error = null;

    try {
      const response = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/schedules`
      );

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      const data = (await response.json()) as ListResponse;
      this.schedules = data.schedules || [];
    } catch (err) {
      console.error('Failed to load schedules:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load schedules';
    } finally {
      this.loading = false;
    }
  }

  private openCreateDialog(): void {
    this.dialogName = '';
    this.dialogCron = '';
    this.dialogEventType = 'message';
    this.dialogAgent = '';
    this.dialogMessage = '';
    this.dialogInterrupt = false;
    this.dialogTemplate = '';
    this.dialogTask = '';
    this.dialogBranch = '';
    this.dialogError = null;
    this.dialogOpen = true;
  }

  private closeDialog(): void {
    this.dialogOpen = false;
    this.dialogError = null;
  }

  private async handleCreate(e: Event): Promise<void> {
    e.preventDefault();
    this.dialogLoading = true;
    this.dialogError = null;

    try {
      const body: Record<string, unknown> = {
        name: this.dialogName,
        cronExpr: this.dialogCron,
        eventType: this.dialogEventType,
        agentName: this.dialogAgent,
      };

      if (this.dialogEventType === 'message') {
        body.message = this.dialogMessage;
        body.interrupt = this.dialogInterrupt;
      } else if (this.dialogEventType === 'dispatch_agent') {
        if (this.dialogTemplate) body.template = this.dialogTemplate;
        if (this.dialogTask) body.task = this.dialogTask;
        if (this.dialogBranch) body.branch = this.dialogBranch;
      }

      const response = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/schedules`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        }
      );

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }

      this.closeDialog();
      await this.loadSchedules();
    } catch (err) {
      this.dialogError = err instanceof Error ? err.message : 'Failed to create schedule';
    } finally {
      this.dialogLoading = false;
    }
  }

  private async handlePause(scheduleId: string): Promise<void> {
    this.actionId = scheduleId;
    try {
      const response = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/schedules/${encodeURIComponent(scheduleId)}/pause`,
        { method: 'POST' }
      );
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      await this.loadSchedules();
    } catch (err) {
      console.error('Failed to pause schedule:', err);
      this.error = err instanceof Error ? err.message : 'Failed to pause schedule';
    } finally {
      this.actionId = null;
    }
  }

  private async handleResume(scheduleId: string): Promise<void> {
    this.actionId = scheduleId;
    try {
      const response = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/schedules/${encodeURIComponent(scheduleId)}/resume`,
        { method: 'POST' }
      );
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      await this.loadSchedules();
    } catch (err) {
      console.error('Failed to resume schedule:', err);
      this.error = err instanceof Error ? err.message : 'Failed to resume schedule';
    } finally {
      this.actionId = null;
    }
  }

  private async handleDelete(scheduleId: string): Promise<void> {
    this.actionId = scheduleId;
    try {
      const response = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/schedules/${encodeURIComponent(scheduleId)}`,
        { method: 'DELETE' }
      );
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      await this.loadSchedules();
    } catch (err) {
      console.error('Failed to delete schedule:', err);
      this.error = err instanceof Error ? err.message : 'Failed to delete schedule';
    } finally {
      this.actionId = null;
    }
  }

  private showDetail(sched: Schedule): void {
    this.detailSchedule = sched;
    this.detailOpen = true;
  }

  private closeDetail(): void {
    this.detailOpen = false;
    this.detailSchedule = null;
  }

  private formatRelativeTime(dateString: string | undefined): string {
    if (!dateString) return '-';
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return dateString;
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

  private formatFutureTime(dateString: string | undefined): string {
    if (!dateString) return '-';
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
      return dateString ?? '-';
    }
  }

  private getPayloadAgent(payload: string): string {
    try {
      const p = JSON.parse(payload) as Record<string, unknown>;
      return (p.agentName as string) || '-';
    } catch {
      return '-';
    }
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

  override render() {
    if (this.compact) {
      return this.renderCompact();
    }
    return this.renderFull();
  }

  private renderCompact() {
    return html`
      <div class="section compact">
        <div class="section-header">
          <div class="section-header-info">
            <h2>Recurring Schedules</h2>
            <p>Automated recurring tasks for this project.</p>
          </div>
          <sl-button size="small" variant="default" @click=${this.openCreateDialog}>
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            New Schedule
          </sl-button>
        </div>

        ${this.loading
          ? html`<div class="section-loading"><sl-spinner></sl-spinner> Loading schedules...</div>`
          : this.error
            ? html`
                <div class="section-error">
                  ${this.error}
                  <sl-button size="small" @click=${() => this.loadSchedules()}>Retry</sl-button>
                </div>
              `
            : this.schedules.length === 0
              ? html`
                  <div class="empty-state">
                    <sl-icon name="arrow-repeat"></sl-icon>
                    <h3>No Recurring Schedules</h3>
                    <p>Create a recurring schedule to automate tasks on a cron cadence.</p>
                    <sl-button variant="primary" size="small" @click=${this.openCreateDialog}>
                      <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                      Create Schedule
                    </sl-button>
                  </div>
                `
              : this.renderTable()}

        ${this.renderCreateDialog()}
        ${this.renderDetailDialog()}
      </div>
    `;
  }

  private renderFull() {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading recurring schedules...</p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load Schedules</h2>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${() => this.loadSchedules()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    return html`
      <div class="list-header">
        <sl-button size="small" variant="primary" @click=${this.openCreateDialog}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          New Schedule
        </sl-button>
      </div>

      ${this.schedules.length === 0
        ? html`
            <div class="empty-state">
              <sl-icon name="arrow-repeat"></sl-icon>
              <h3>No Recurring Schedules</h3>
              <p>Create a recurring schedule to automate tasks on a cron cadence.</p>
            </div>
          `
        : this.renderTable()}

      ${this.renderCreateDialog()}
      ${this.renderDetailDialog()}
    `;
  }

  private renderTable() {
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Type</th>
              <th>Cron</th>
              <th>Next Run</th>
              <th>Status</th>
              <th class="hide-mobile">Runs</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            ${this.schedules.map((sched) => this.renderScheduleRow(sched))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderScheduleRow(sched: Schedule) {
    const isActive = sched.status === 'active';
    const isPaused = sched.status === 'paused';
    const isActing = this.actionId === sched.id;
    const nextRun = isActive ? this.formatFutureTime(sched.nextRunAt) : '-';

    return html`
      <tr @click=${() => this.showDetail(sched)} style="cursor: pointer;">
        <td><strong>${sched.name}</strong></td>
        <td><span class="type-badge environment">${sched.eventType}</span></td>
        <td><span class="meta-text" style="font-family: var(--scion-font-mono, monospace); font-size: 0.8125rem;">${sched.cronExpr}</span></td>
        <td><span class="meta-text">${nextRun}</span></td>
        <td><span class="badge ${this.statusBadgeClass(sched.status)}">${sched.status}</span></td>
        <td class="hide-mobile"><span class="meta-text">${sched.runCount}${sched.errorCount > 0 ? ` (${sched.errorCount} err)` : ''}</span></td>
        <td class="actions-cell" @click=${(e: Event) => e.stopPropagation()}>
          ${isActive
            ? html`
                <sl-icon-button
                  name="pause-circle"
                  label="Pause"
                  ?disabled=${isActing}
                  @click=${() => this.handlePause(sched.id)}
                ></sl-icon-button>
              `
            : nothing}
          ${isPaused
            ? html`
                <sl-icon-button
                  name="play-circle"
                  label="Resume"
                  ?disabled=${isActing}
                  @click=${() => this.handleResume(sched.id)}
                ></sl-icon-button>
              `
            : nothing}
          <sl-icon-button
            name="trash"
            label="Delete"
            ?disabled=${isActing}
            @click=${() => this.handleDelete(sched.id)}
          ></sl-icon-button>
        </td>
      </tr>
    `;
  }

  private renderCreateDialog() {
    return html`
      <sl-dialog
        label="Create Recurring Schedule"
        ?open=${this.dialogOpen}
        @sl-request-close=${this.closeDialog}
      >
        <form class="dialog-form" @submit=${this.handleCreate}>
          ${this.dialogError
            ? html`<div class="dialog-error">${this.dialogError}</div>`
            : nothing}

          <sl-input
            label="Name"
            placeholder="daily-standup"
            .value=${this.dialogName}
            @sl-input=${(e: Event) => (this.dialogName = (e.target as HTMLInputElement).value)}
            required
          ></sl-input>

          <sl-input
            label="Cron Expression"
            placeholder="0 9 * * 1-5"
            help-text="Standard 5-field cron: minute hour day month weekday (UTC)"
            .value=${this.dialogCron}
            @sl-input=${(e: Event) => (this.dialogCron = (e.target as HTMLInputElement).value)}
            required
          ></sl-input>

          <sl-select
            label="Event Type"
            .value=${this.dialogEventType}
            @sl-change=${(e: Event) => (this.dialogEventType = (e.target as HTMLSelectElement).value)}
          >
            <sl-option value="message">Message</sl-option>
            <sl-option value="dispatch_agent">Dispatch Agent</sl-option>
          </sl-select>

          <sl-input
            label=${this.dialogEventType === 'dispatch_agent' ? 'Agent Name (to create)' : 'Target Agent'}
            placeholder=${this.dialogEventType === 'dispatch_agent' ? 'worker-1' : 'agent-name or all'}
            .value=${this.dialogAgent}
            @sl-input=${(e: Event) => (this.dialogAgent = (e.target as HTMLInputElement).value)}
            required
          ></sl-input>

          ${this.dialogEventType === 'message'
            ? html`
                <sl-textarea
                  label="Message"
                  placeholder="Message to send"
                  .value=${this.dialogMessage}
                  @sl-input=${(e: Event) => (this.dialogMessage = (e.target as HTMLTextAreaElement).value)}
                  required
                ></sl-textarea>

                <label class="checkbox-label">
                  <input
                    type="checkbox"
                    .checked=${this.dialogInterrupt}
                    @change=${(e: Event) =>
                      (this.dialogInterrupt = (e.target as HTMLInputElement).checked)}
                  />
                  <span class="checkbox-text">
                    <span>Interrupt agent</span>
                    <span class="checkbox-description">Interrupt the agent's current task before delivering the message.</span>
                  </span>
                </label>
              `
            : html`
                <sl-input
                  label="Template"
                  placeholder="template-name (optional)"
                  .value=${this.dialogTemplate}
                  @sl-input=${(e: Event) => (this.dialogTemplate = (e.target as HTMLInputElement).value)}
                ></sl-input>

                <sl-textarea
                  label="Task / Prompt"
                  placeholder="Task for the agent (optional)"
                  .value=${this.dialogTask}
                  @sl-input=${(e: Event) => (this.dialogTask = (e.target as HTMLTextAreaElement).value)}
                ></sl-textarea>

                <sl-input
                  label="Branch"
                  placeholder="feature-branch (optional)"
                  .value=${this.dialogBranch}
                  @sl-input=${(e: Event) => (this.dialogBranch = (e.target as HTMLInputElement).value)}
                ></sl-input>
              `}
        </form>

        <sl-button slot="footer" variant="default" @click=${this.closeDialog}>Cancel</sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.dialogLoading}
          @click=${this.handleCreate}
          >Create</sl-button
        >
      </sl-dialog>
    `;
  }

  private renderDetailDialog() {
    const sched = this.detailSchedule;
    if (!sched) return nothing;

    const agent = this.getPayloadAgent(sched.payload);
    let payloadDetails: Record<string, unknown> = {};
    try {
      payloadDetails = JSON.parse(sched.payload) as Record<string, unknown>;
    } catch {
      // ignore
    }

    return html`
      <sl-dialog
        label="Schedule: ${sched.name}"
        ?open=${this.detailOpen}
        @sl-request-close=${this.closeDetail}
      >
        <div class="dialog-form">
          <div class="detail-row"><strong>ID:</strong> <span style="font-family: var(--scion-font-mono, monospace); font-size: 0.8125rem;">${sched.id}</span></div>
          <div class="detail-row"><strong>Status:</strong> <span class="badge ${this.statusBadgeClass(sched.status)}">${sched.status}</span></div>
          <div class="detail-row"><strong>Cron:</strong> ${sched.cronExpr}</div>
          <div class="detail-row"><strong>Event Type:</strong> ${sched.eventType}</div>
          <div class="detail-row"><strong>Target Agent:</strong> ${agent}</div>
          ${sched.eventType === 'message' && payloadDetails.message
            ? html`<div class="detail-row"><strong>Message:</strong> ${payloadDetails.message}</div>`
            : nothing}
          ${sched.eventType === 'dispatch_agent' && payloadDetails.template
            ? html`<div class="detail-row"><strong>Template:</strong> ${payloadDetails.template}</div>`
            : nothing}
          ${sched.eventType === 'dispatch_agent' && payloadDetails.task
            ? html`<div class="detail-row"><strong>Task:</strong> ${payloadDetails.task}</div>`
            : nothing}
          ${sched.nextRunAt
            ? html`<div class="detail-row"><strong>Next Run:</strong> ${this.formatFutureTime(sched.nextRunAt)} (${new Date(sched.nextRunAt).toLocaleString()})</div>`
            : nothing}
          ${sched.lastRunAt
            ? html`<div class="detail-row"><strong>Last Run:</strong> ${this.formatRelativeTime(sched.lastRunAt)} (${sched.lastRunStatus || 'unknown'})</div>`
            : nothing}
          <div class="detail-row"><strong>Run Count:</strong> ${sched.runCount} total${sched.errorCount > 0 ? `, ${sched.errorCount} errors` : ''}</div>
          ${sched.lastRunError
            ? html`<div class="detail-row"><strong>Last Error:</strong> <span style="color: var(--sl-color-danger-600, #dc2626);">${sched.lastRunError}</span></div>`
            : nothing}
          <div class="detail-row"><strong>Created:</strong> ${this.formatRelativeTime(sched.createdAt)}${sched.createdBy ? ` by ${sched.createdBy}` : ''}</div>
        </div>

        <sl-button slot="footer" variant="default" @click=${this.closeDetail}>Close</sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-schedule-list': ScionScheduleList;
  }
}
