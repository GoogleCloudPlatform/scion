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
import { browserTimeZone, formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { FEATURE_FLAGS_CHANGED_EVENT, execSchedulesEnabled } from '../../utils/feature-flags.js';
import {
  EXEC_HISTORY_LIMIT,
  type ExecAgentOption,
  type ExecHistoryEvent,
  argvEqual,
  loadProjectExecAgents,
  parseExecPayload,
  parseExecTimeoutValue,
  renderExecAgentPicker,
  renderExecArgvEditor,
  renderExecDetailPayload,
  renderExecHistorySection,
  renderExecTimeoutInput,
  scheduleExecStyles,
  validateExecArgv,
  validateExecTarget,
} from './schedule-exec.js';

interface Schedule {
  id: string;
  projectId: string;
  name: string;
  cronExpr: string;
  /** IANA zone the cron is evaluated in; absent or empty means UTC. */
  timeZone?: string;
  eventType: string;
  payload: string;
  status: string;
  /** System reason a paused schedule was paused; absent when none (active, or paused by a user). */
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

interface HistoryListResponse {
  events?: ExecHistoryEvent[];
}

/**
 * Label shown on a schedule whose cron expression carries a zone prefix.
 * The hub rejects such expressions and pauses existing rows that use one;
 * the zone belongs in the schedule's time zone field.
 */
export const ZONE_PREFIX_BADGE_LABEL = 'Zone prefix not supported — use the time zone field';

/**
 * Text shown for each system pause reason the hub stores in
 * `statusReason`. An unknown reason is shown as stored.
 */
const STATUS_REASON_LABELS: Record<string, string> = {
  creator_deleted: 'Creator deleted: resume to run it under your authority',
  cron_zone_unsupported:
    'Zone prefix not supported: remove it, set the time zone field, then resume',
  target_deleted: 'Target agent deleted: edit to choose a live target and command, then resume',
};

/** Returns the text shown for a schedule's status reason, or '' when it has none. */
export function scheduleStatusReasonLabel(reason: string | undefined): string {
  if (!reason) return '';
  return STATUS_REASON_LABELS[reason] ?? reason;
}

/**
 * Reports whether a cron expression begins with a CRON_TZ= or TZ= zone prefix.
 * Mirrors the hub's check: case-sensitive, on the untrimmed expression.
 */
export function hasCronZonePrefix(expr: string): boolean {
  return expr.startsWith('CRON_TZ=') || expr.startsWith('TZ=');
}

/** The zone shown for a schedule's cron expression: its IANA name, or UTC. */
export function scheduleZoneLabel(timeZone: string | undefined): string {
  return timeZone ? timeZone : 'UTC';
}

/**
 * Normalizes a time zone typed into a form: trimmed, with "UTC" stored as
 * empty, the way the hub stores it. The hub validates the name.
 */
export function normalizeTimeZoneInput(value: string): string {
  const zone = value.trim();
  return zone === 'UTC' ? '' : zone;
}

export interface ScheduleEditExecFields {
  agentId: string;
  agentPickedById?: boolean | undefined;
  command: readonly string[];
  timeoutInput: string;
}

/** The edit dialog's fields. */
export interface ScheduleEditFields {
  name: string;
  cronExpr: string;
  /** The time zone field; omitted leaves the zone alone. */
  timeZone?: string | undefined;
  /** Resume a paused schedule in the same request. */
  resume: boolean;
  /** Exec dispatch fields when editing a revealed exec schedule or replacing a redacted one. */
  exec?: ScheduleEditExecFields | undefined;
}

/**
 * Builds the PATCH body for the edit dialog: only the fields that changed,
 * so an unchanged schedule sends nothing. Never emits `eventType` (the hub
 * forbids changing `eventType` to or from `exec`). Returns an error instead
 * for input the hub would reject.
 */
export function buildScheduleEdit(
  sched: Pick<Schedule, 'name' | 'cronExpr' | 'status' | 'timeZone'> & {
    eventType?: string | undefined;
    payload?: string | undefined;
  },
  fields: ScheduleEditFields
): { patch: Record<string, unknown> } | { error: string } {
  const name = fields.name.trim();
  const cronExpr = fields.cronExpr.trim();
  if (!name) return { error: 'Name is required.' };
  if (!cronExpr) return { error: 'Cron expression is required.' };
  if (hasCronZonePrefix(cronExpr)) {
    return {
      error:
        'Zone prefixes (CRON_TZ=, TZ=) are not supported. Remove the prefix and set the time zone field instead.',
    };
  }
  const patch: Record<string, unknown> = {};
  if (name !== sched.name) patch.name = name;
  if (cronExpr !== sched.cronExpr) patch.cronExpr = cronExpr;
  if (fields.timeZone !== undefined) {
    const timeZone = normalizeTimeZoneInput(fields.timeZone);
    if (timeZone !== (sched.timeZone ?? '')) patch.timeZone = timeZone;
  }
  if (fields.resume && sched.status === 'paused') patch.status = 'active';

  if (fields.exec !== undefined) {
    const targetErr = validateExecTarget(fields.exec.agentId);
    if (targetErr) return { error: targetErr };
    const argvErr = validateExecArgv(fields.exec.command);
    if (argvErr) return { error: argvErr };
    const timeoutRes = parseExecTimeoutValue(fields.exec.timeoutInput);
    if ('error' in timeoutRes) return { error: timeoutRes.error };

    const nextTarget = fields.exec.agentId.trim();
    const pickedById = fields.exec.agentPickedById !== false;
    const orig = parseExecPayload(sched.payload ?? '');

    if (orig.commandRedacted) {
      if (pickedById) {
        patch.agentId = nextTarget;
      } else {
        patch.agentName = nextTarget;
      }
      patch.command = [...fields.exec.command];
      if (timeoutRes.value > 0) {
        patch.timeoutSeconds = timeoutRes.value;
      }
    } else {
      if (pickedById) {
        if (nextTarget !== orig.agentId) patch.agentId = nextTarget;
      } else if (nextTarget !== orig.agentName && nextTarget !== orig.agentId) {
        patch.agentName = nextTarget;
      }
      if (!argvEqual(fields.exec.command, orig.command)) {
        patch.command = [...fields.exec.command];
      }
      const origTimeout = orig.timeoutSeconds > 0 ? orig.timeoutSeconds : 0;
      if (timeoutRes.value !== origTimeout) {
        patch.timeoutSeconds = timeoutRes.value;
      }
    }
  }

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
  @state() private dialogTimeZone = '';
  @state() private dialogEventType = 'message';
  @state() private dialogAgent = '';
  @state() private dialogMessage = '';
  @state() private dialogInterrupt = false;
  @state() private dialogWake = false;
  @state() private dialogTemplate = '';
  @state() private dialogTask = '';
  @state() private dialogBranch = '';
  @state() private dialogExecAgentId = '';
  @state() private dialogExecCommand: string[] = [''];
  @state() private dialogExecTimeout = '';
  @state() private dialogLoading = false;
  @state() private dialogError: string | null = null;

  // Edit dialog
  @state() private editSchedule: Schedule | null = null;
  @state() private editName = '';
  @state() private editCron = '';
  @state() private editTimeZone = '';
  @state() private editResume = false;
  @state() private editExecAgentId = '';
  @state() private editExecCommand: string[] = [''];
  @state() private editExecTimeout = '';
  @state() private editReplaceExec = false;
  @state() private editLoading = false;
  @state() private editError: string | null = null;

  // Project agents for exec picker (loaded lazily when exec is authored/edited)
  @state() private execAgents: ExecAgentOption[] = [];
  @state() private execAgentsLoading = false;
  @state() private execAgentsError: string | null = null;
  private execAgentsLoadedForProject = '';
  private execAgentsGeneration = 0;

  // Action state
  @state() private actionId: string | null = null;

  // Detail dialog + exec run history
  @state() private detailSchedule: Schedule | null = null;
  @state() private detailOpen = false;
  @state() private detailHistory: ExecHistoryEvent[] = [];
  @state() private detailHistoryLoading = false;
  @state() private detailHistoryError: string | null = null;
  private detailHistoryGeneration = 0;

  private readonly boundOnFlagsChanged = (): void => {
    if (!execSchedulesEnabled() && this.dialogEventType === 'exec') {
      this.dialogEventType = 'message';
    }
    this.requestUpdate();
  };

  static override styles = [
    resourceStyles,
    scheduleExecStyles,
    css`
      .badge.zone-prefix {
        background: var(--sl-color-warning-100, #fef3c7);
        color: var(--sl-color-warning-700, #b45309);
        margin-left: 0.375rem;
        white-space: nowrap;
      }

      .status-reason {
        font-size: 0.75rem;
        margin-top: 0.125rem;
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
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFlagsChanged);
    void this.loadSchedules();
  }

  override disconnectedCallback(): void {
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFlagsChanged);
    super.disconnectedCallback();
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

  private async ensureExecAgentsLoaded(): Promise<void> {
    if (!this.projectId) return;
    if (
      this.execAgentsLoadedForProject === this.projectId &&
      !this.execAgentsError &&
      this.execAgents.length > 0
    ) {
      return;
    }
    const gen = ++this.execAgentsGeneration;
    this.execAgentsLoading = true;
    this.execAgentsError = null;
    try {
      const agents = await loadProjectExecAgents(
        this.projectId,
        () => gen === this.execAgentsGeneration
      );
      if (gen !== this.execAgentsGeneration) return;
      this.execAgents = agents;
      this.execAgentsLoadedForProject = this.projectId;
    } catch (err) {
      if (gen !== this.execAgentsGeneration) return;
      this.execAgentsError =
        err instanceof PaginationError && err.hubMessage
          ? err.hubMessage
          : err instanceof Error
            ? err.message
            : 'Failed to load agents';
    } finally {
      if (gen === this.execAgentsGeneration) this.execAgentsLoading = false;
    }
  }

  private async loadScheduleHistory(scheduleId: string): Promise<void> {
    if (!this.projectId) return;
    const gen = ++this.detailHistoryGeneration;
    this.detailHistory = [];
    this.detailHistoryLoading = true;
    this.detailHistoryError = null;

    try {
      const response = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/schedules/${encodeURIComponent(scheduleId)}/history?limit=${EXEC_HISTORY_LIMIT}`
      );
      if (gen !== this.detailHistoryGeneration) return;
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      const data = (await response.json()) as HistoryListResponse;
      if (gen !== this.detailHistoryGeneration) return;
      this.detailHistory = data.events ?? [];
    } catch (err) {
      if (gen !== this.detailHistoryGeneration) return;
      this.detailHistoryError = err instanceof Error ? err.message : 'Failed to load run history';
    } finally {
      if (gen === this.detailHistoryGeneration) this.detailHistoryLoading = false;
    }
  }

  private openCreateDialog(): void {
    this.dialogName = '';
    this.dialogCron = '';
    this.dialogTimeZone = '';
    this.dialogEventType = 'message';
    this.dialogAgent = '';
    this.dialogMessage = '';
    this.dialogInterrupt = false;
    this.dialogWake = false;
    this.dialogTemplate = '';
    this.dialogTask = '';
    this.dialogBranch = '';
    this.dialogExecAgentId = '';
    this.dialogExecCommand = [''];
    this.dialogExecTimeout = '';
    this.dialogError = null;
    this.dialogOpen = true;
  }

  private closeDialog(): void {
    this.dialogOpen = false;
    this.dialogError = null;
  }

  private handleCreateEventTypeChange(nextType: string): void {
    this.dialogEventType = nextType;
    this.dialogError = null;
    if (nextType === 'exec') {
      void this.ensureExecAgentsLoaded();
    }
  }

  private async handleCreate(e: Event): Promise<void> {
    e.preventDefault();
    this.dialogLoading = true;
    this.dialogError = null;

    try {
      const isExec = execSchedulesEnabled() && this.dialogEventType === 'exec';
      const body: Record<string, unknown> = {
        name: this.dialogName,
        cronExpr: this.dialogCron,
        eventType: isExec ? 'exec' : this.dialogEventType,
      };
      const timeZone = normalizeTimeZoneInput(this.dialogTimeZone);
      if (timeZone) body.timeZone = timeZone;

      if (isExec) {
        const targetErr = validateExecTarget(this.dialogExecAgentId);
        if (targetErr) {
          this.dialogError = targetErr;
          return;
        }
        const argvErr = validateExecArgv(this.dialogExecCommand);
        if (argvErr) {
          this.dialogError = argvErr;
          return;
        }
        const timeoutRes = parseExecTimeoutValue(this.dialogExecTimeout);
        if ('error' in timeoutRes) {
          this.dialogError = timeoutRes.error;
          return;
        }
        const target = this.dialogExecAgentId.trim();
        const pickedById = this.execAgents.some((a) => a.id === target);
        if (pickedById || !this.execAgentsError) {
          body.agentId = target;
        } else {
          body.agentName = target;
        }
        body.command = [...this.dialogExecCommand];
        if (timeoutRes.value > 0) {
          body.timeoutSeconds = timeoutRes.value;
        }
      } else {
        body.agentName = this.dialogAgent;
        if (this.dialogEventType === 'message') {
          body.message = this.dialogMessage;
          body.interrupt = this.dialogInterrupt;
          body.wake = this.dialogWake;
        } else if (this.dialogEventType === 'dispatch_agent') {
          if (this.dialogTemplate) body.template = this.dialogTemplate;
          if (this.dialogTask) body.task = this.dialogTask;
          if (this.dialogBranch) body.branch = this.dialogBranch;
        }
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
    this.editTimeZone = sched.timeZone ?? '';
    this.editResume = false;
    this.editError = null;

    if (sched.eventType === 'exec') {
      const parsed = parseExecPayload(sched.payload);
      this.editReplaceExec = false;
      if (parsed.commandRedacted) {
        this.editExecAgentId = '';
        this.editExecCommand = [''];
        this.editExecTimeout = '';
      } else {
        this.editExecAgentId = parsed.agentId || parsed.agentName;
        this.editExecCommand = parsed.command.length > 0 ? [...parsed.command] : [''];
        this.editExecTimeout = parsed.timeoutSeconds > 0 ? String(parsed.timeoutSeconds) : '';
        void this.ensureExecAgentsLoaded();
      }
    } else {
      this.editReplaceExec = false;
      this.editExecAgentId = '';
      this.editExecCommand = [''];
      this.editExecTimeout = '';
    }
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

    let execFields: ScheduleEditExecFields | undefined;
    if (sched.eventType === 'exec') {
      const parsed = parseExecPayload(sched.payload);
      if (!parsed.commandRedacted || this.editReplaceExec) {
        const target = this.editExecAgentId.trim();
        const pickedById =
          target === parsed.agentId ||
          this.execAgents.some((a) => a.id === target) ||
          !this.execAgentsError;
        execFields = {
          agentId: this.editExecAgentId,
          agentPickedById: pickedById,
          command: this.editExecCommand,
          timeoutInput: this.editExecTimeout,
        };
      }
    }

    const body = buildScheduleEdit(sched, {
      name: this.editName,
      cronExpr: this.editCron,
      timeZone: this.editTimeZone,
      resume: this.editResume,
      exec: execFields,
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
    if (sched.eventType === 'exec') {
      void this.loadScheduleHistory(sched.id);
    } else {
      this.detailHistoryGeneration++;
      this.detailHistory = [];
      this.detailHistoryLoading = false;
      this.detailHistoryError = null;
    }
  }

  private closeDetail(): void {
    this.detailHistoryGeneration++;
    this.detailOpen = false;
    this.detailSchedule = null;
    this.detailHistory = [];
    this.detailHistoryLoading = false;
    this.detailHistoryError = null;
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
      return (p.agentName as string) || (p.agentId as string) || '-';
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

  private renderStatusReason(sched: Schedule, inline = false) {
    const label = sched.status === 'paused' ? scheduleStatusReasonLabel(sched.statusReason) : '';
    if (!label) return nothing;
    const reason = sched.statusReason ?? '';
    const cls = 'meta-text status-reason';
    return inline
      ? html`<span class=${cls} data-reason=${reason}>${label}</span>`
      : html`<div class=${cls} data-reason=${reason}>${label}</div>`;
  }

  private renderZonePrefixBadge(sched: Schedule) {
    if (!hasCronZonePrefix(sched.cronExpr)) return nothing;
    return html`<span class="badge zone-prefix" title="Set the zone in the time zone field"
      >${ZONE_PREFIX_BADGE_LABEL}</span
    >`;
  }

  /**
   * The time zone field of the create and edit dialogs. Empty means UTC;
   * the help text offers the browser's zone. The hub validates the name.
   */
  private renderTimeZoneInput(
    cls: string,
    value: string,
    onInput: (value: string) => void
  ): TemplateResult {
    const local = browserTimeZone();
    const hint =
      'IANA time zone, e.g. America/Los_Angeles. Empty means UTC.' +
      (local && local !== 'UTC' ? ` Your browser's zone is ${local}.` : '') +
      ' Daylight saving changes keep the local time.';
    return html`<sl-input
      label="Time zone"
      class=${cls}
      placeholder="UTC"
      help-text=${hint}
      .value=${value}
      @sl-input=${(e: Event): void => onInput((e.target as HTMLInputElement).value)}
    ></sl-input>`;
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
    // The cron expression is evaluated in the schedule's zone (shown under
    // it); the next run is shown in the display zone, labelled, so the two
    // are never confused.
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
          <div class="meta-text schedule-zone">${scheduleZoneLabel(sched.timeZone)}</div>
        </td>
        <td>
          <span class="meta-text">${nextRun}</span>
          ${nextRunAbsolute
            ? html`<div class="meta-text next-run-absolute">${nextRunAbsolute}</div>`
            : nothing}
        </td>
        <td>
          <span class="badge ${this.statusBadgeClass(sched.status)}">${sched.status}</span>
          ${this.renderStatusReason(sched)}
        </td>
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
    const execFlagOn = execSchedulesEnabled();
    const isExec = execFlagOn && this.dialogEventType === 'exec';

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
            help-text="Standard 5-field cron: minute hour day month weekday, in the time zone below"
            .value=${this.dialogCron}
            @sl-input=${(e: Event) => (this.dialogCron = (e.target as HTMLInputElement).value)}
            required
          ></sl-input>

          ${this.renderTimeZoneInput('create-time-zone', this.dialogTimeZone, (v) => {
            this.dialogTimeZone = v;
          })}

          <sl-select
            label="Event Type"
            .value=${isExec ? 'exec' : this.dialogEventType}
            @sl-change=${(e: Event) =>
              this.handleCreateEventTypeChange((e.target as HTMLSelectElement).value)}
          >
            <sl-option value="message">Message</sl-option>
            <sl-option value="dispatch_agent">Dispatch Agent</sl-option>
            ${execFlagOn ? html`<sl-option value="exec">Exec Command</sl-option>` : nothing}
          </sl-select>

          ${isExec
            ? html`
                ${renderExecAgentPicker({
                  agents: this.execAgents,
                  loading: this.execAgentsLoading,
                  loadError: this.execAgentsError,
                  value: this.dialogExecAgentId,
                  onChange: (v) => {
                    this.dialogExecAgentId = v;
                  },
                })}
                ${renderExecArgvEditor(this.dialogExecCommand, (updater) => {
                  this.dialogExecCommand = updater(this.dialogExecCommand);
                })}
                ${renderExecTimeoutInput(this.dialogExecTimeout, (v) => {
                  this.dialogExecTimeout = v;
                })}
              `
            : html`
                <sl-input
                  label=${this.dialogEventType === 'dispatch_agent'
                    ? 'Agent Name (to create)'
                    : 'Target Agent'}
                  placeholder=${this.dialogEventType === 'dispatch_agent'
                    ? 'worker-1'
                    : 'agent-name or all'}
                  .value=${this.dialogAgent}
                  @sl-input=${(e: Event) =>
                    (this.dialogAgent = (e.target as HTMLInputElement).value)}
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

                      <label class="checkbox-label">
                        <input
                          type="checkbox"
                          .checked=${this.dialogWake}
                          @change=${(e: Event) =>
                            (this.dialogWake = (e.target as HTMLInputElement).checked)}
                        />
                        <span class="checkbox-text">
                          <span>Wake agent</span>
                          <span class="checkbox-description"
                            >Resume the agent if it is suspended when the message fires. Requires
                            permission to start the agent.</span
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

  private renderEditExecSection(sched: Schedule): TemplateResult | typeof nothing {
    if (sched.eventType !== 'exec') return nothing;
    const parsed = parseExecPayload(sched.payload);

    if (parsed.commandRedacted) {
      const targetDeleted = sched.statusReason === 'target_deleted';
      return html`
        <div class="exec-redacted-notice">
          <div class="exec-redacted-header">
            <sl-icon name="lock"></sl-icon>
            <span>
              ${targetDeleted
                ? 'Target agent was deleted; stored command is hidden.'
                : 'Stored command is hidden (requires attach permission on target agent).'}
            </span>
          </div>
          <div>
            ${targetDeleted
              ? 'Check below to pick a live target agent and enter a replacement command, or save a new name only.'
              : 'Renaming is allowed. Changing timing, target, command, or resuming requires attach permission on the target agent.'}
          </div>
        </div>

        <label class="checkbox-label">
          <input
            type="checkbox"
            class="edit-replace-exec"
            .checked=${this.editReplaceExec}
            @change=${(e: Event): void => {
              this.editReplaceExec = (e.target as HTMLInputElement).checked;
              if (this.editReplaceExec) {
                void this.ensureExecAgentsLoaded();
              }
            }}
          />
          <span class="checkbox-text">
            <span>Replace target and command</span>
            <span class="checkbox-description"
              >Supply a live target agent and replacement argv command.</span
            >
          </span>
        </label>

        ${this.editReplaceExec
          ? html`
              ${renderExecAgentPicker({
                agents: this.execAgents,
                loading: this.execAgentsLoading,
                loadError: this.execAgentsError,
                value: this.editExecAgentId,
                onChange: (v) => {
                  this.editExecAgentId = v;
                },
              })}
              ${renderExecArgvEditor(this.editExecCommand, (updater) => {
                this.editExecCommand = updater(this.editExecCommand);
              })}
              ${renderExecTimeoutInput(this.editExecTimeout, (v) => {
                this.editExecTimeout = v;
              })}
            `
          : nothing}
      `;
    }

    return html`
      ${renderExecAgentPicker({
        agents: this.execAgents,
        loading: this.execAgentsLoading,
        loadError: this.execAgentsError,
        value: this.editExecAgentId,
        currentMissingAgent: parsed.agentId
          ? { id: parsed.agentId, name: parsed.agentName }
          : undefined,
        onChange: (v) => {
          this.editExecAgentId = v;
        },
      })}
      ${renderExecArgvEditor(this.editExecCommand, (updater) => {
        this.editExecCommand = updater(this.editExecCommand);
      })}
      ${renderExecTimeoutInput(this.editExecTimeout, (v) => {
        this.editExecTimeout = v;
      })}
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
              ? 'Zone prefixes (CRON_TZ=, TZ=) are not supported: remove the prefix and set the time zone field.'
              : 'Standard 5-field cron: minute hour day month weekday, in the time zone below'}
            .value=${this.editCron}
            @sl-input=${(e: Event): void => {
              this.editCron = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>

          ${this.renderTimeZoneInput('edit-time-zone', this.editTimeZone, (v) => {
            this.editTimeZone = v;
          })}
          ${this.renderEditExecSection(sched)}
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

    const isExec = sched.eventType === 'exec';
    const execPayload = isExec ? parseExecPayload(sched.payload) : null;
    const agent = execPayload
      ? execPayload.agentName || execPayload.agentId || '-'
      : this.getPayloadAgent(sched.payload);

    let payloadDetails: Record<string, unknown> = {};
    if (!isExec) {
      try {
        const parsed: unknown = JSON.parse(sched.payload);
        if (parsed && typeof parsed === 'object') {
          payloadDetails = parsed as Record<string, unknown>;
        }
      } catch {
        // ignore
      }
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
            ${this.renderStatusReason(sched, true)}
          </div>
          <div class="detail-row">
            <strong>Cron:</strong> ${sched.cronExpr} ${this.renderZonePrefixBadge(sched)}
          </div>
          <div class="detail-row">
            <strong>Time zone:</strong>
            <span class="schedule-zone">${scheduleZoneLabel(sched.timeZone)}</span>
          </div>
          <div class="detail-row"><strong>Event Type:</strong> ${sched.eventType}</div>
          <div class="detail-row"><strong>Target Agent:</strong> ${agent}</div>
          ${isExec && execPayload ? renderExecDetailPayload(execPayload) : nothing}
          ${sched.eventType === 'message' && payloadDetails.message
            ? html`<div class="detail-row">
                <strong>Message:</strong> ${payloadDetails.message}
              </div>`
            : nothing}
          ${sched.eventType === 'message'
            ? html`
                <div class="detail-row">
                  <strong>Wake:</strong> ${payloadDetails.wake === true ? 'Yes' : 'No'}
                </div>
                <div class="detail-row">
                  <strong>Interrupt:</strong> ${payloadDetails.interrupt === true ? 'Yes' : 'No'}
                </div>
              `
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
          ${isExec
            ? renderExecHistorySection({
                loading: this.detailHistoryLoading,
                error: this.detailHistoryError,
                events: this.detailHistory,
              })
            : nothing}
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
