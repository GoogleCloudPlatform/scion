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
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { paginateAll, PaginationError } from '../../client/paginate-all.js';
import { resourceStyles } from './resource-styles.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';

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

/**
 * Label shown on a schedule whose cron expression carries a zone prefix.
 * Schedules are evaluated in UTC only; the hub rejects such expressions and
 * pauses existing rows that use one.
 */
export const ZONE_PREFIX_BADGE_LABEL = 'Zone prefix not supported — edit to UTC';

/**
 * Reports whether a cron expression begins with a CRON_TZ= or TZ= zone prefix.
 * Mirrors the hub's check: case-sensitive, on the untrimmed expression.
 */
export function hasCronZonePrefix(expr: string): boolean {
  return expr.startsWith('CRON_TZ=') || expr.startsWith('TZ=');
}

/** The edit dialog's fields. */
export interface ScheduleEditFields {
  name: string;
  cronExpr: string;
  /** Resume a paused schedule in the same request. */
  resume: boolean;
}

/**
 * Builds the PATCH body for the edit dialog: only the fields that changed,
 * so an unchanged schedule sends nothing. Returns an error instead for input
 * the hub would reject (empty fields, a zone-prefixed expression).
 */
export function buildScheduleEdit(
  sched: Pick<Schedule, 'name' | 'cronExpr' | 'status'>,
  fields: ScheduleEditFields
): { patch: Record<string, unknown> } | { error: string } {
  const name = fields.name.trim();
  const cronExpr = fields.cronExpr.trim();
  if (!name) return { error: 'Name is required.' };
  if (!cronExpr) return { error: 'Cron expression is required.' };
  if (hasCronZonePrefix(cronExpr)) {
    return {
      error:
        'Zone prefixes (CRON_TZ=, TZ=) are not supported. Remove the prefix and give the time in UTC.',
    };
  }
  const patch: Record<string, unknown> = {};
  if (name !== sched.name) patch.name = name;
  if (cronExpr !== sched.cronExpr) patch.cronExpr = cronExpr;
  if (fields.resume && sched.status === 'paused') patch.status = 'active';
  return { patch };
}

interface ListResponse {
  schedules?: Schedule[];
  nextCursor?: string;
  totalCount?: number;
  serverTime?: string;
}

/** Page size requested when loading the schedule list; every page is followed. */
export const SCHEDULE_PAGE_SIZE = 100;

@customElement('scion-schedule-list')
export class ScionScheduleList extends LitElement {
  /** Re-renders next-run instants when the display timezone changes. */
  readonly _zone = new DisplayZoneController(this);

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

  // Edit dialog
  @state() private editSchedule: Schedule | null = null;
  @state() private editName = '';
  @state() private editCron = '';
  @state() private editResume = false;
  @state() private editLoading = false;
  @state() private editError: string | null = null;

  // Action state
  @state() private actionId: string | null = null;

  // Detail dialog
  @state() private detailSchedule: Schedule | null = null;
  @state() private detailOpen = false;

  static override styles = [
    resourceStyles,
    css`
      .badge.zone-prefix {
        background: var(--sl-color-warning-100, #fef3c7);
        color: var(--sl-color-warning-700, #b45309);
        margin-left: 0.375rem;
        white-space: nowrap;
      }

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
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadSchedules();
  }

  /** Bumped on every load, so a slower, older walk never overwrites a newer result. */
  private loadGeneration = 0;

  private async loadSchedules(): Promise<void> {
    if (!this.projectId) return;
    const generation = ++this.loadGeneration;
    this.loading = true;
    this.error = null;

    try {
      // Follow nextCursor to the end: the hub pages the list, and a single
      // request showed only the first page (ptone/scion#2643).
      const schedules = await paginateAll<Schedule>({
        path: `/api/v1/projects/${encodeURIComponent(this.projectId)}/schedules`,
        pageSize: SCHEDULE_PAGE_SIZE,
        label: 'schedules list',
        parsePage: (body) => {
          const data = body as ListResponse;
          return { items: data.schedules ?? [], nextCursor: data.nextCursor ?? '' };
        },
        shouldContinue: () => generation === this.loadGeneration,
      });
      if (generation !== this.loadGeneration) return;
      this.schedules = schedules;
    } catch (err) {
      if (generation !== this.loadGeneration) return;
      console.error('Failed to load schedules:', err);
      // Prefer the hub's own message for a failed page over the generic
      // "request failed: <status>" text.
      this.error =
        err instanceof PaginationError && err.hubMessage
          ? err.hubMessage
          : err instanceof Error
            ? err.message
            : 'Failed to load schedules';
    } finally {
      if (generation === this.loadGeneration) this.loading = false;
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

  private openEditDialog(sched: Schedule): void {
    if (this.editLoading) return;
    this.detailOpen = false;
    this.editSchedule = sched;
    this.editName = sched.name;
    this.editCron = sched.cronExpr;
    this.editResume = false;
    this.editError = null;
  }

  private closeEditDialog(): void {
    // A save in flight owns the dialog until it settles: closing now would
    // let its result land on whatever dialog is open next.
    if (this.editLoading) return;
    this.editSchedule = null;
    this.editError = null;
  }

  private async handleEdit(e: Event): Promise<void> {
    e.preventDefault();
    const sched = this.editSchedule;
    if (!sched || this.editLoading) return;
    const body = buildScheduleEdit(sched, {
      name: this.editName,
      cronExpr: this.editCron,
      resume: this.editResume,
    });
    if ('error' in body) {
      this.editError = body.error;
      return;
    }
    if (Object.keys(body.patch).length === 0) {
      this.closeEditDialog();
      return;
    }
    this.editLoading = true;
    this.editError = null;
    // Writes below are guarded on the dialog still showing this schedule, in
    // case it was replaced while the PATCH was in flight.
    const current = (): boolean => this.editSchedule === sched;
    let saved = false;
    try {
      const response = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/schedules/${encodeURIComponent(sched.id)}`,
        {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body.patch),
        }
      );
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      saved = true;
    } catch (err) {
      if (current()) {
        this.editError = err instanceof Error ? err.message : 'Failed to update schedule';
      }
    } finally {
      this.editLoading = false;
    }
    // Close and reload only after the in-flight flag is cleared, so the
    // reload never holds it and a later save's flag is never cleared by
    // this one's finally.
    if (saved) {
      if (current()) this.closeEditDialog();
      await this.loadSchedules();
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
    return formatRelative(dateString);
  }

  /** Relative next-run text; an overdue instant reads "now", as before. */
  private formatFutureTime(dateString: string | undefined): string {
    if (!dateString) return '-';
    const ms = new Date(dateString).getTime();
    if (!Number.isNaN(ms) && ms <= Date.now()) return 'now';
    return formatRelative(dateString);
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

  private renderZonePrefixBadge(sched: Schedule) {
    if (!hasCronZonePrefix(sched.cronExpr)) return nothing;
    return html`<span class="badge zone-prefix" title="Cron expressions are evaluated in UTC"
      >${ZONE_PREFIX_BADGE_LABEL}</span
    >`;
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
        ${this.renderCreateDialog()} ${this.renderDetailDialog()} ${this.renderEditDialog()}
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
      ${this.renderCreateDialog()} ${this.renderDetailDialog()} ${this.renderEditDialog()}
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
              <th>Cron (UTC)</th>
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
    // Cron expressions are UTC; the next run is shown in the display zone,
    // labelled, so the two are never confused.
    const nextRunAbsolute =
      isActive && sched.nextRunAt ? formatInstantWithZone(sched.nextRunAt) : '';

    return html`
      <tr @click=${() => this.showDetail(sched)} style="cursor: pointer;">
        <td><strong>${sched.name}</strong></td>
        <td><span class="type-badge environment">${sched.eventType}</span></td>
        <td>
          <span
            class="meta-text"
            style="font-family: var(--scion-font-mono, monospace); font-size: 0.8125rem;"
            >${sched.cronExpr}</span
          >
          ${this.renderZonePrefixBadge(sched)}
        </td>
        <td>
          <span class="meta-text">${nextRun}</span>
          ${nextRunAbsolute
            ? html`<div class="meta-text next-run-absolute">${nextRunAbsolute}</div>`
            : nothing}
        </td>
        <td><span class="badge ${this.statusBadgeClass(sched.status)}">${sched.status}</span></td>
        <td class="hide-mobile">
          <span class="meta-text"
            >${sched.runCount}${sched.errorCount > 0 ? ` (${sched.errorCount} err)` : ''}</span
          >
        </td>
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
            name="pencil"
            label="Edit"
            ?disabled=${isActing}
            @click=${(): void => this.openEditDialog(sched)}
          ></sl-icon-button>
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
          ${this.dialogError ? html`<div class="dialog-error">${this.dialogError}</div>` : nothing}

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
            @sl-change=${(e: Event) =>
              (this.dialogEventType = (e.target as HTMLSelectElement).value)}
          >
            <sl-option value="message">Message</sl-option>
            <sl-option value="dispatch_agent">Dispatch Agent</sl-option>
          </sl-select>

          <sl-input
            label=${this.dialogEventType === 'dispatch_agent'
              ? 'Agent Name (to create)'
              : 'Target Agent'}
            placeholder=${this.dialogEventType === 'dispatch_agent'
              ? 'worker-1'
              : 'agent-name or all'}
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
                  @sl-input=${(e: Event) =>
                    (this.dialogMessage = (e.target as HTMLTextAreaElement).value)}
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
                    <span class="checkbox-description"
                      >Interrupt the agent's current task before delivering the message.</span
                    >
                  </span>
                </label>
              `
            : html`
                <sl-input
                  label="Template"
                  placeholder="template-name (optional)"
                  .value=${this.dialogTemplate}
                  @sl-input=${(e: Event) =>
                    (this.dialogTemplate = (e.target as HTMLInputElement).value)}
                ></sl-input>

                <sl-textarea
                  label="Task / Prompt"
                  placeholder="Task for the agent (optional)"
                  .value=${this.dialogTask}
                  @sl-input=${(e: Event) =>
                    (this.dialogTask = (e.target as HTMLTextAreaElement).value)}
                ></sl-textarea>

                <sl-input
                  label="Branch"
                  placeholder="feature-branch (optional)"
                  .value=${this.dialogBranch}
                  @sl-input=${(e: Event) =>
                    (this.dialogBranch = (e.target as HTMLInputElement).value)}
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

  private renderEditDialog(): TemplateResult | typeof nothing {
    const sched = this.editSchedule;
    if (!sched) return nothing;
    const prefixed = hasCronZonePrefix(this.editCron.trim());
    return html`
      <sl-dialog
        label="Edit Schedule: ${sched.name}"
        open
        @sl-request-close=${(e: Event): void => {
          if (this.editLoading) {
            e.preventDefault();
            return;
          }
          this.closeEditDialog();
        }}
      >
        <form class="dialog-form edit-form" @submit=${(e: Event): void => void this.handleEdit(e)}>
          ${this.editError
            ? html`<div class="dialog-error" role="alert">${this.editError}</div>`
            : nothing}

          <sl-input
            label="Name"
            .value=${this.editName}
            @sl-input=${(e: Event): void => {
              this.editName = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>

          <sl-input
            label="Cron Expression"
            class="edit-cron"
            help-text=${prefixed
              ? 'Zone prefixes (CRON_TZ=, TZ=) are not supported: remove the prefix and give the time in UTC.'
              : 'Standard 5-field cron: minute hour day month weekday (UTC)'}
            .value=${this.editCron}
            @sl-input=${(e: Event): void => {
              this.editCron = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>

          ${sched.status === 'paused'
            ? html`
                <label class="checkbox-label">
                  <input
                    type="checkbox"
                    class="edit-resume"
                    .checked=${this.editResume}
                    @change=${(e: Event): void => {
                      this.editResume = (e.target as HTMLInputElement).checked;
                    }}
                  />
                  <span class="checkbox-text">
                    <span>Resume after saving</span>
                    <span class="checkbox-description"
                      >This schedule is paused. Resume it with the new settings.</span
                    >
                  </span>
                </label>
              `
            : nothing}
        </form>

        <sl-button
          slot="footer"
          variant="default"
          class="edit-cancel"
          ?disabled=${this.editLoading}
          @click=${(): void => this.closeEditDialog()}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          class="edit-save"
          ?loading=${this.editLoading}
          @click=${(e: Event): void => void this.handleEdit(e)}
          >Save</sl-button
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
          <div class="detail-row">
            <strong>ID:</strong>
            <span style="font-family: var(--scion-font-mono, monospace); font-size: 0.8125rem;"
              >${sched.id}</span
            >
          </div>
          <div class="detail-row">
            <strong>Status:</strong>
            <span class="badge ${this.statusBadgeClass(sched.status)}">${sched.status}</span>
          </div>
          <div class="detail-row">
            <strong>Cron (UTC):</strong> ${sched.cronExpr} ${this.renderZonePrefixBadge(sched)}
          </div>
          <div class="detail-row"><strong>Event Type:</strong> ${sched.eventType}</div>
          <div class="detail-row"><strong>Target Agent:</strong> ${agent}</div>
          ${sched.eventType === 'message' && payloadDetails.message
            ? html`<div class="detail-row">
                <strong>Message:</strong> ${payloadDetails.message}
              </div>`
            : nothing}
          ${sched.eventType === 'dispatch_agent' && payloadDetails.template
            ? html`<div class="detail-row">
                <strong>Template:</strong> ${payloadDetails.template}
              </div>`
            : nothing}
          ${sched.eventType === 'dispatch_agent' && payloadDetails.task
            ? html`<div class="detail-row"><strong>Task:</strong> ${payloadDetails.task}</div>`
            : nothing}
          ${sched.nextRunAt
            ? html`<div class="detail-row">
                <strong>Next Run:</strong> ${this.formatFutureTime(sched.nextRunAt)} ·
                <span class="next-run-absolute">${formatInstantWithZone(sched.nextRunAt)}</span>
              </div>`
            : nothing}
          ${sched.lastRunAt
            ? html`<div class="detail-row">
                <strong>Last Run:</strong> ${this.formatRelativeTime(sched.lastRunAt)}
                (${sched.lastRunStatus || 'unknown'})
              </div>`
            : nothing}
          <div class="detail-row">
            <strong>Run Count:</strong> ${sched.runCount}
            total${sched.errorCount > 0 ? `, ${sched.errorCount} errors` : ''}
          </div>
          ${sched.lastRunError
            ? html`<div class="detail-row">
                <strong>Last Error:</strong>
                <span style="color: var(--sl-color-danger-600, #dc2626);"
                  >${sched.lastRunError}</span
                >
              </div>`
            : nothing}
          <div class="detail-row">
            <strong>Created:</strong> ${this.formatRelativeTime(sched.createdAt)}${sched.createdBy
              ? ` by ${sched.createdBy}`
              : ''}
          </div>
        </div>

        <sl-button slot="footer" variant="default" @click=${(): void => this.openEditDialog(sched)}
          >Edit</sl-button
        >
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
