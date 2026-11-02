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
 * The Chat retention panel of Admin > Server Config: the hub-wide chat
 * message retention settings (GET/PUT /api/v1/admin/chat-retention).
 *
 * Rendered only while the hub.chat_retention web flag is on; the flag only
 * controls presentation, and the hub answers 404 on the API while the
 * experiment is off. A save names the revision it was made against
 * (expected_revision), like the cross-project messaging switch; a 409
 * reloads the current values.
 *
 * The panel also shows the recent retention runs and the last preview
 * (the chat-retention-preview maintenance operation, which it can start).
 * A save that enables the sweep or shortens the window first shows the
 * last preview and asks for confirmation; it never counts messages itself.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';
import { apiFetch, apiErrorMessageFromBody } from '../../client/api.js';
import {
  CHAT_RETENTION_FLAG,
  FEATURE_FLAGS_CHANGED_EVENT,
  isFeatureEnabled,
} from '../../utils/feature-flags.js';
import { formatInstantWithZone } from '../../utils/time.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';

const API_PATH = '/api/v1/admin/chat-retention';
const PREVIEW_OP = 'chat-retention-preview';
const PREVIEW_RUN_PATH = `/api/v1/admin/maintenance/operations/${PREVIEW_OP}/run`;
/** How often a started preview is checked, and for how long at most. */
const PREVIEW_POLL_MS = 2000;
const PREVIEW_POLL_MAX = 900;

/** Sweep tuning, as pkg/hub/admin_chat_retention.go sends it. */
export interface ChatRetentionSweep {
  chunk_size: number;
  pause_ms: number;
  tick_budget_seconds: number;
  tick_interval_minutes: number;
}

/** GET/PUT response of /api/v1/admin/chat-retention. */
export interface ChatRetentionSettings {
  enabled: boolean;
  /** null = no window: messages are kept forever. */
  messages_retention_days: number | null;
  /** Days after a conversation or topic is deleted until its messages are purged. */
  deleted_conversation_grace_days: number;
  sweep: ChatRetentionSweep;
  last_applied_cutoff: string | null;
  revision: number;
  /** The most recent retention runs, newest first. */
  runs?: ChatRetentionRun[] | undefined;
  /** The most recent preview, or null when none was run. */
  last_preview?: ChatRetentionRun | null | undefined;
  /** Messages the sweep and manual runs deleted since the last preview started. */
  purged_since_preview?: number | undefined;
}

/** One chat_retention_runs row, as pkg/hub/admin_chat_retention.go sends it. */
export interface ChatRetentionRun {
  id: string;
  /** "sweep", "manual" or "preview"; any other value is shown as sent. */
  trigger: string;
  dry_run: boolean;
  started_at: string;
  finished_at: string | null;
  /** "running", "ok", "partial" or "failed"; any other value is shown as sent. */
  status: string;
  actor?: string;
  error?: string;
  /** Messages deleted (a preview: that would be deleted). */
  messages: number;
  /** Projects with at least one such message. */
  projects: number;
  messages_retention_days: number;
  deleted_conversation_grace_days: number;
}

/** Bounds of the settings, as pkg/config/opsettings/chat_retention.go sets them. */
export const RETENTION_MIN_DAYS = 7;
export const RETENTION_MAX_DAYS = 36500;
export const RETENTION_MIN_GRACE_DAYS = 1;
export const RETENTION_DEFAULT_GRACE_DAYS = 30;

type SweepKey = keyof ChatRetentionSweep;

const SWEEP_FIELDS: ReadonlyArray<{
  key: SweepKey;
  label: string;
  hint: string;
  min: number;
  max: number;
}> = [
  {
    key: 'chunk_size',
    label: 'Chunk size',
    hint: 'Messages deleted per transaction (25 to 1000; default 200).',
    min: 25,
    max: 1000,
  },
  {
    key: 'pause_ms',
    label: 'Pause between chunks (ms)',
    hint: 'Pause after each chunk (0 to 10000; default 500).',
    min: 0,
    max: 10000,
  },
  {
    key: 'tick_budget_seconds',
    label: 'Tick budget (seconds)',
    hint: 'Longest a sweep tick runs (1 to 40; default 30).',
    min: 1,
    max: 40,
  },
  {
    key: 'tick_interval_minutes',
    label: 'Tick interval (minutes)',
    hint: 'Time between sweep ticks (1 to 1440; default 10).',
    min: 1,
    max: 1440,
  },
];

/** The form's values, as typed (strings, so an empty field stays empty). */
interface RetentionForm {
  enabled: boolean;
  days: string;
  graceDays: string;
  sweep: Record<SweepKey, string>;
}

/** A saved PUT body, or the problems that keep the form from being saved. */
export type RetentionPutBody =
  | {
      ok: true;
      body: {
        enabled: boolean;
        messages_retention_days: number | null;
        deleted_conversation_grace_days: number;
        sweep: ChatRetentionSweep;
        expected_revision: number;
      };
    }
  | { ok: false; errors: string[] };

function parseWholeNumber(value: string): number | null {
  const v = value.trim();
  if (!/^\d+$/.test(v)) return null;
  return Number(v);
}

/**
 * Validate the form and build the PUT body. An empty window is sent as an
 * explicit null (keep forever); every field is sent with the revision the
 * form was loaded at.
 */
export function buildRetentionPutBody(
  form: { enabled: boolean; days: string; graceDays: string; sweep: Record<SweepKey, string> },
  revision: number
): RetentionPutBody {
  const errors: string[] = [];
  let days: number | null = null;
  if (form.days.trim() !== '') {
    days = parseWholeNumber(form.days);
    if (days === null || days < RETENTION_MIN_DAYS || days > RETENTION_MAX_DAYS) {
      errors.push(
        `Retention window must be a whole number of days from ${RETENTION_MIN_DAYS} to ${RETENTION_MAX_DAYS}, or empty to keep messages forever.`
      );
    }
  }
  const graceDays = parseWholeNumber(form.graceDays);
  if (
    graceDays === null ||
    graceDays < RETENTION_MIN_GRACE_DAYS ||
    graceDays > RETENTION_MAX_DAYS
  ) {
    errors.push(
      `Deleted conversation grace must be a whole number of days from ${RETENTION_MIN_GRACE_DAYS} to ${RETENTION_MAX_DAYS}.`
    );
  }
  const sweep = {} as ChatRetentionSweep;
  for (const f of SWEEP_FIELDS) {
    const n = parseWholeNumber(form.sweep[f.key]);
    if (n === null || n < f.min || n > f.max) {
      errors.push(`${f.label} must be a whole number from ${f.min} to ${f.max}.`);
    } else {
      sweep[f.key] = n;
    }
  }
  if (errors.length > 0) return { ok: false, errors };
  return {
    ok: true,
    body: {
      enabled: form.enabled,
      messages_retention_days: days,
      deleted_conversation_grace_days: graceDays as number,
      sweep,
      expected_revision: revision,
    },
  };
}

/**
 * The messages of a failed save: the per-field `errors` of a
 * validation_failed body, else the body's error message.
 */
export function retentionSaveErrors(body: unknown, fallback: string): string[] {
  if (body && typeof body === 'object') {
    const list = (body as { errors?: unknown }).errors;
    if (Array.isArray(list)) {
      const messages = list
        .map((e) => {
          if (!e || typeof e !== 'object') return '';
          const { Path, Message, path, message } = e as Record<string, unknown>;
          const msg =
            typeof Message === 'string' ? Message : typeof message === 'string' ? message : '';
          const at = typeof Path === 'string' ? Path : typeof path === 'string' ? path : '';
          return msg ? (at ? `${at}: ${msg}` : msg) : '';
        })
        .filter((m) => m);
      if (messages.length > 0) return messages;
    }
  }
  return [apiErrorMessageFromBody(body) ?? fallback];
}

/**
 * The warning shown while the sweep is enabled: what it deletes, with the
 * window (null when none is set, 'invalid' while the field holds a value
 * that is not a valid window, which then goes unmentioned: the save shows
 * the validation error) and the deleted-conversation grace (null while the
 * field is not a valid grace).
 */
export function retentionWarning(
  days: number | null | 'invalid',
  graceDays: number | null
): string {
  const parts: string[] = [];
  if (days === null) {
    parts.push(
      'No retention window is set: only the messages of deleted conversations and topics are purged.'
    );
  } else if (days !== 'invalid') {
    parts.push(
      `While enabled, messages older than ${days} days are permanently deleted from this hub.`
    );
  }
  if (graceDays !== null) {
    const unit = graceDays === 1 ? 'day' : 'days';
    parts.push(
      `Messages of conversations and topics deleted more than ${graceDays} ${unit} ago are permanently deleted.`
    );
  }
  return parts.join(' ');
}

function plural(n: number, one: string, many: string): string {
  return `${n.toLocaleString()} ${n === 1 ? one : many}`;
}

function formatWhen(iso: string | null): string {
  if (!iso) return '';
  return formatInstantWithZone(iso) || iso;
}

/**
 * The one-line summary of a preview: "Last preview: N messages in M
 * projects, <time>", with the window it previewed.
 */
export function lastPreviewSummary(run: ChatRetentionRun | null | undefined): string {
  if (!run) return 'No preview has been run yet.';
  if (run.status === 'running')
    return `A preview is running (started ${formatWhen(run.started_at)}).`;
  if (run.status === 'failed') {
    return `The last preview failed (${formatWhen(run.started_at)})${run.error ? `: ${run.error}` : '.'}`;
  }
  const window =
    run.messages_retention_days > 0
      ? `a ${run.messages_retention_days}-day window`
      : 'no retention window';
  return (
    `Last preview: ${plural(run.messages, 'message', 'messages')} in ` +
    `${plural(run.projects, 'project', 'projects')}, ` +
    `${formatWhen(run.finished_at ?? run.started_at)} (${window}).`
  );
}

/**
 * The "backlog draining" estimate: the last preview's messages minus the
 * messages purged since it started, never below zero. null without a
 * finished preview.
 */
export function backlogEstimate(
  preview: ChatRetentionRun | null | undefined,
  purgedSince: number | undefined
): number | null {
  if (!preview || preview.status !== 'ok') return null;
  return Math.max(0, preview.messages - (purgedSince ?? 0));
}

/**
 * Whether saving body deletes more than the saved settings do: it enables
 * the sweep, or, while enabled, sets a window where none was or shortens
 * it. Such a save asks for confirmation first.
 */
export function needsPurgeConfirmation(
  saved: Pick<ChatRetentionSettings, 'enabled' | 'messages_retention_days'>,
  next: { enabled: boolean; messages_retention_days: number | null }
): boolean {
  if (!next.enabled) return false;
  if (!saved.enabled) return true;
  if (next.messages_retention_days === null) return false;
  return (
    saved.messages_retention_days === null ||
    next.messages_retention_days < saved.messages_retention_days
  );
}

const TRIGGER_LABELS: Record<string, string> = {
  sweep: 'Sweep',
  manual: 'Manual run',
  preview: 'Preview',
};

const STATUS_LABELS: Record<string, string> = {
  running: 'Running',
  ok: 'Complete',
  partial: 'In progress',
  failed: 'Failed',
};

function formFrom(s: ChatRetentionSettings): RetentionForm {
  return {
    enabled: s.enabled,
    days: s.messages_retention_days === null ? '' : String(s.messages_retention_days),
    graceDays: String(s.deleted_conversation_grace_days ?? RETENTION_DEFAULT_GRACE_DAYS),
    sweep: {
      chunk_size: String(s.sweep.chunk_size),
      pause_ms: String(s.sweep.pause_ms),
      tick_budget_seconds: String(s.sweep.tick_budget_seconds),
      tick_interval_minutes: String(s.sweep.tick_interval_minutes),
    },
  };
}

@customElement('scion-admin-chat-retention')
export class ScionAdminChatRetention extends LitElement {
  @state() private flagOn = isFeatureEnabled(CHAT_RETENTION_FLAG);
  @state() private loading = false;
  @state() private saving = false;
  /** The hub answered 404: the experiment is off on the hub. */
  @state() private unavailable = false;
  @state() private loadError: string | null = null;
  @state() private saveErrors: string[] = [];
  @state() private success: string | null = null;
  @state() private settings: ChatRetentionSettings | null = null;
  @state() private form: RetentionForm | null = null;
  /** The validated PUT body waiting for the purge confirmation. */
  @state() private pendingSave: Extract<RetentionPutBody, { ok: true }>['body'] | null = null;
  @state() private previewStarting = false;
  /** A preview this panel started is still running. */
  @state() private previewRunning = false;
  @state() private previewError: string | null = null;

  private loadRequested = false;
  private previewPoll: ReturnType<typeof setTimeout> | null = null;
  /** Re-renders the last applied cutoff when the display zone changes. */
  readonly _zone = new DisplayZoneController(this);

  private readonly onFlagsChanged = (): void => {
    this.flagOn = isFeatureEnabled(CHAT_RETENTION_FLAG);
    if (this.flagOn && !this.loadRequested) void this.load();
  };

  static override styles = css`
    :host {
      display: block;
    }
    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }
    .section-title {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 1rem 0;
      padding-bottom: 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }
    .intro {
      margin: 0 0 1rem 0;
      padding-left: 1.25rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
    }
    .form-grid {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 1rem;
    }
    @media (max-width: 768px) {
      .form-grid {
        grid-template-columns: 1fr;
      }
    }
    .form-field {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
    }
    .form-field.full-width {
      grid-column: 1 / -1;
    }
    .form-field label {
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
    }
    .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }
    details.advanced {
      margin-top: 1rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      padding-top: 0.75rem;
    }
    details.advanced summary {
      cursor: pointer;
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.75rem;
    }
    .status-message {
      font-size: 0.875rem;
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      margin-top: 1rem;
    }
    .status-message ul {
      margin: 0;
      padding-left: 1.25rem;
    }
    .status-message.success {
      background: var(--scion-success-bg, #dcfce7);
      color: var(--scion-success-text, #166534);
      border: 1px solid var(--scion-success-border, #86efac);
    }
    .status-message.error {
      background: var(--scion-danger-bg, #fee2e2);
      color: var(--scion-danger-text, #991b1b);
      border: 1px solid var(--scion-danger-border, #fca5a5);
    }
    .status-message.warning {
      background: var(--sl-color-warning-50, #fffbeb);
      color: var(--sl-color-warning-800, #92400e);
      border: 1px solid var(--sl-color-warning-200, #fde68a);
    }
    .actions {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-top: 1rem;
    }
    .meta {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
    }
    .history {
      margin-top: 1.5rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      padding-top: 1rem;
    }
    .subsection-title {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }
    .last-preview,
    .backlog {
      font-size: 0.8125rem;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }
    table.runs {
      width: 100%;
      border-collapse: collapse;
      margin-top: 1rem;
      font-size: 0.8125rem;
    }
    table.runs th {
      text-align: left;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      padding: 0.375rem 0.5rem;
    }
    table.runs td {
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      padding: 0.375rem 0.5rem;
      color: var(--scion-text, #1e293b);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFlagsChanged);
    this.onFlagsChanged();
  }

  override disconnectedCallback(): void {
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFlagsChanged);
    this.stopPreviewPoll();
    super.disconnectedCallback();
  }

  private stopPreviewPoll(): void {
    if (this.previewPoll !== null) {
      clearTimeout(this.previewPoll);
      this.previewPoll = null;
    }
  }

  private applySettings(s: ChatRetentionSettings): void {
    this.settings = s;
    this.form = formFrom(s);
  }

  private async load(): Promise<void> {
    this.loadRequested = true;
    this.loading = true;
    this.loadError = null;
    try {
      const res = await apiFetch(API_PATH);
      if (res.status === 404) {
        this.unavailable = true;
        return;
      }
      if (!res.ok) {
        this.loadError = retentionSaveErrors(
          await res.json().catch(() => null),
          'Failed to load chat retention settings'
        ).join(' ');
        return;
      }
      this.unavailable = false;
      this.applySettings((await res.json()) as ChatRetentionSettings);
    } catch {
      this.loadError = 'Failed to load chat retention settings';
    } finally {
      this.loading = false;
    }
  }

  private async save(): Promise<void> {
    if (!this.form || !this.settings) return;
    this.saveErrors = [];
    this.success = null;
    const built = buildRetentionPutBody(this.form, this.settings.revision);
    if (!built.ok) {
      this.saveErrors = built.errors;
      return;
    }
    if (needsPurgeConfirmation(this.settings, built.body)) {
      this.pendingSave = built.body;
      return;
    }
    await this.put(built.body);
  }

  private async confirmSave(): Promise<void> {
    const body = this.pendingSave;
    this.pendingSave = null;
    if (body) await this.put(body);
  }

  private async put(body: Extract<RetentionPutBody, { ok: true }>['body']): Promise<void> {
    this.saving = true;
    try {
      const res = await apiFetch(API_PATH, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (res.status === 409) {
        this.saveErrors = [
          'Settings were changed by another administrator. Reloaded the current values; review them and save again.',
        ];
        await this.load();
        return;
      }
      if (!res.ok) {
        this.saveErrors = retentionSaveErrors(
          await res.json().catch(() => null),
          'Failed to save chat retention settings'
        );
        return;
      }
      this.applySettings((await res.json()) as ChatRetentionSettings);
      this.success = 'Chat retention settings saved.';
    } catch {
      this.saveErrors = ['Failed to save chat retention settings'];
    } finally {
      this.saving = false;
    }
  }

  /** Reload the run history and the last preview, keeping the form. */
  private async loadHistory(): Promise<void> {
    try {
      const res = await apiFetch(API_PATH);
      if (!res.ok || !this.settings) return;
      const s = (await res.json()) as ChatRetentionSettings;
      this.settings = {
        ...this.settings,
        last_applied_cutoff: s.last_applied_cutoff,
        runs: s.runs,
        last_preview: s.last_preview,
        purged_since_preview: s.purged_since_preview,
      };
    } catch {
      // The history stays as it was; the next load refreshes it.
    }
  }

  /**
   * Start a preview: of the window in the form when it holds a valid one,
   * else of the saved settings. The hub runs it in the background; the
   * panel checks it until it ends, then reloads the history.
   */
  private async startPreview(): Promise<void> {
    if (!this.form) return;
    this.previewError = null;
    const days = parseWholeNumber(this.form.days);
    const params: Record<string, string> = {};
    if (days !== null && days >= RETENTION_MIN_DAYS && days <= RETENTION_MAX_DAYS) {
      params.messages_retention_days = String(days);
    }
    this.previewStarting = true;
    try {
      const res = await apiFetch(PREVIEW_RUN_PATH, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ params }),
      });
      const body = (await res.json().catch(() => null)) as { runId?: string } | null;
      if (!res.ok || !body?.runId) {
        this.previewError = retentionSaveErrors(body, 'Failed to start the preview').join(' ');
        return;
      }
      this.previewRunning = true;
      await this.loadHistory();
      this.pollPreview(body.runId, 0);
    } catch {
      this.previewError = 'Failed to start the preview';
    } finally {
      this.previewStarting = false;
    }
  }

  private pollPreview(runId: string, attempt: number): void {
    this.stopPreviewPoll();
    this.previewPoll = setTimeout(() => {
      this.previewPoll = null;
      void (async () => {
        let running = attempt + 1 < PREVIEW_POLL_MAX;
        try {
          const res = await apiFetch(
            `/api/v1/admin/maintenance/operations/${PREVIEW_OP}/runs/${encodeURIComponent(runId)}`
          );
          if (res.ok) {
            const run = (await res.json()) as { status?: string };
            running = running && run.status === 'running';
          }
        } catch {
          // Try again on the next check.
        }
        if (!this.isConnected) return;
        if (running) {
          this.pollPreview(runId, attempt + 1);
          return;
        }
        this.previewRunning = false;
        await this.loadHistory();
      })();
    }, PREVIEW_POLL_MS);
  }

  private updateForm(patch: Partial<RetentionForm>): void {
    if (!this.form) return;
    this.form = { ...this.form, ...patch };
    this.success = null;
  }

  private updateSweep(key: SweepKey, value: string): void {
    if (!this.form) return;
    this.updateForm({ sweep: { ...this.form.sweep, [key]: value } });
  }

  override render() {
    if (!this.flagOn) return nothing;
    return html`
      <div class="section chat-retention">
        <h3 class="section-title">Chat retention</h3>
        <ul class="intro">
          <li>Nothing is deleted while the sweep is disabled.</li>
          <li class="grace-explanation">
            With the sweep enabled and no retention window, only the messages of deleted
            conversations and topics are purged, once the grace period after their deletion has
            passed.
          </li>
          <li>Deletes are permanent: removed messages cannot be restored.</li>
          <li>Take a database backup before enabling.</li>
          <li>The window applies to all chat messages on this hub, in every project.</li>
        </ul>
        ${this.renderBody()}
      </div>
    `;
  }

  private renderBody() {
    if (this.unavailable) {
      return html`<div class="hint unavailable">
        Chat retention is not enabled on this hub (experiment hub.chat_retention is off).
      </div>`;
    }
    if (this.loadError) {
      return html`<div class="status-message error load-error">${this.loadError}</div>`;
    }
    if (!this.form || !this.settings) {
      return html`<div class="hint">${this.loading ? 'Loading…' : ''}</div>`;
    }
    const form = this.form;
    const days = parseWholeNumber(form.days);
    const windowSet = form.days.trim() !== '';
    const windowValid = days !== null && days >= RETENTION_MIN_DAYS && days <= RETENTION_MAX_DAYS;
    const grace = parseWholeNumber(form.graceDays);
    const graceDays =
      grace !== null && grace >= RETENTION_MIN_GRACE_DAYS && grace <= RETENTION_MAX_DAYS
        ? grace
        : null;
    const warning = form.enabled
      ? retentionWarning(!windowSet ? null : windowValid ? days : 'invalid', graceDays)
      : '';
    // With the sweep enabled and neither field valid there is nothing to
    // warn about: say plainly that the form cannot be saved.
    const unsaveable = form.enabled && windowSet && !windowValid && graceDays === null;
    const cutoff = this.settings.last_applied_cutoff;
    return html`
      <div class="form-grid">
        <div class="form-field full-width">
          <sl-switch
            class="retention-enabled"
            ?checked=${form.enabled}
            ?disabled=${this.saving}
            @sl-change=${(e: Event) =>
              this.updateForm({ enabled: (e.target as HTMLInputElement).checked })}
            >Enable the retention sweep</sl-switch
          >
          <span class="hint"
            >Off acts as a legal hold: nothing is deleted while the sweep is disabled.</span
          >
        </div>
        <div class="form-field">
          <label for="retention-days">Retention window (days)</label>
          <sl-input
            id="retention-days"
            class="retention-days"
            type="number"
            min=${RETENTION_MIN_DAYS}
            max=${RETENTION_MAX_DAYS}
            placeholder="Keep forever"
            .value=${form.days}
            ?disabled=${this.saving}
            @sl-input=${(e: Event) =>
              this.updateForm({ days: (e.target as HTMLInputElement).value })}
          ></sl-input>
          <span class="hint"
            >Messages older than this are deleted. Empty keeps messages forever; at least
            ${RETENTION_MIN_DAYS} days.</span
          >
        </div>
        <div class="form-field">
          <label for="retention-grace-days">Deleted conversation grace (days)</label>
          <sl-input
            id="retention-grace-days"
            class="retention-grace-days"
            type="number"
            min=${RETENTION_MIN_GRACE_DAYS}
            max=${RETENTION_MAX_DAYS}
            .value=${form.graceDays}
            ?disabled=${this.saving}
            @sl-input=${(e: Event) =>
              this.updateForm({ graceDays: (e.target as HTMLInputElement).value })}
          ></sl-input>
          <span class="hint"
            >The messages of a deleted conversation or topic are purged this many days after the
            deletion, even when the retention window is longer or empty (default
            ${RETENTION_DEFAULT_GRACE_DAYS}).</span
          >
        </div>
        <div class="form-field">
          <label>Last applied cutoff</label>
          <span class="last-applied-cutoff"
            >${cutoff ? formatInstantWithZone(cutoff) || cutoff : 'None yet'}</span
          >
          <span class="hint">Every eligible message older than this has been deleted.</span>
        </div>
      </div>
      <details class="advanced">
        <summary>Advanced: sweep tuning</summary>
        <div class="form-grid">
          ${SWEEP_FIELDS.map(
            (f) => html`
              <div class="form-field">
                <label for="sweep-${f.key}">${f.label}</label>
                <sl-input
                  id="sweep-${f.key}"
                  class="sweep-input"
                  data-key=${f.key}
                  type="number"
                  min=${f.min}
                  max=${f.max}
                  .value=${form.sweep[f.key]}
                  ?disabled=${this.saving}
                  @sl-input=${(e: Event) =>
                    this.updateSweep(f.key, (e.target as HTMLInputElement).value)}
                ></sl-input>
                <span class="hint">${f.hint}</span>
              </div>
            `
          )}
        </div>
      </details>
      ${warning
        ? html`<div class="status-message warning delete-warning">${warning}</div>`
        : nothing}
      ${unsaveable
        ? html`<div class="status-message error invalid-form" role="alert">
            Neither the retention window nor the deleted conversation grace is valid. Fix both
            before saving; nothing is saved until then.
          </div>`
        : nothing}
      ${this.saveErrors.length > 0
        ? html`<div class="status-message error save-errors" role="alert">
            <ul>
              ${this.saveErrors.map((m) => html`<li>${m}</li>`)}
            </ul>
          </div>`
        : nothing}
      ${this.success
        ? html`<div class="status-message success" role="status">${this.success}</div>`
        : nothing}
      <div class="actions">
        <sl-button
          class="retention-save"
          variant="primary"
          size="small"
          ?loading=${this.saving}
          ?disabled=${this.saving || unsaveable}
          @click=${() => void this.save()}
          >Save</sl-button
        >
        <span class="meta">Revision: ${this.settings.revision}</span>
      </div>
      ${this.renderHistory(windowValid ? days : null)} ${this.renderConfirmDialog()}
    `;
  }

  private renderHistory(formDays: number | null) {
    const s = this.settings;
    if (!s) return nothing;
    const runs = s.runs ?? [];
    const backlog = s.enabled ? backlogEstimate(s.last_preview, s.purged_since_preview) : null;
    const busy = this.previewStarting || this.previewRunning;
    return html`
      <div class="history">
        <h4 class="subsection-title">Preview and run history</h4>
        <p class="last-preview">${lastPreviewSummary(s.last_preview)}</p>
        ${backlog !== null
          ? html`<p class="backlog">
              Backlog draining: about ${plural(backlog, 'message', 'messages')} left to delete (the
              last preview minus ${plural(s.purged_since_preview ?? 0, 'message', 'messages')}
              deleted since).
            </p>`
          : nothing}
        <div class="actions">
          <sl-button
            class="run-preview"
            size="small"
            ?loading=${busy}
            ?disabled=${busy}
            @click=${() => void this.startPreview()}
            >Run preview</sl-button
          >
          <span class="hint"
            >${formDays !== null
              ? `Counts what a ${formDays}-day window would delete, without deleting anything.`
              : 'Counts what the saved settings would delete, without deleting anything.'}</span
          >
        </div>
        ${this.previewError
          ? html`<div class="status-message error preview-error" role="alert">
              ${this.previewError}
            </div>`
          : nothing}
        ${runs.length === 0
          ? html`<p class="hint no-runs">No retention runs yet.</p>`
          : html`<table class="runs">
              <thead>
                <tr>
                  <th>Started</th>
                  <th>Run</th>
                  <th>Status</th>
                  <th>Finished</th>
                  <th>Messages</th>
                </tr>
              </thead>
              <tbody>
                ${runs.map(
                  (r) => html`
                    <tr class="run" data-trigger=${r.trigger}>
                      <td>${formatWhen(r.started_at)}</td>
                      <td>
                        ${TRIGGER_LABELS[r.trigger] ?? r.trigger}${r.dry_run ? ' (dry run)' : ''}
                      </td>
                      <td class="run-status" title=${r.error ?? ''}>
                        ${STATUS_LABELS[r.status] ?? r.status}
                      </td>
                      <td>${r.finished_at ? formatWhen(r.finished_at) : '—'}</td>
                      <td class="run-messages">
                        ${r.dry_run ? 'would delete ' : ''}${plural(
                          r.messages,
                          'message',
                          'messages'
                        )}
                        in ${plural(r.projects, 'project', 'projects')}
                      </td>
                    </tr>
                  `
                )}
              </tbody>
            </table>`}
      </div>
    `;
  }

  private renderConfirmDialog() {
    const body = this.pendingSave;
    const s = this.settings;
    const enabling = body !== null && s !== null && !s.enabled;
    return html`
      <sl-dialog
        class="confirm-dialog"
        label="Delete messages permanently?"
        ?open=${body !== null}
        @sl-request-close=${() => {
          this.pendingSave = null;
        }}
      >
        <p class="confirm-change">
          ${enabling
            ? 'Enabling the retention sweep permanently deletes the messages it selects.'
            : `Shortening the retention window to ${body?.messages_retention_days ?? ''} days permanently deletes the older messages.`}
          Deleted messages cannot be restored; take a database backup first.
        </p>
        <p class="confirm-preview">${lastPreviewSummary(s?.last_preview)}</p>
        ${s?.last_preview &&
        s.last_preview.status === 'ok' &&
        body &&
        s.last_preview.messages_retention_days !== (body.messages_retention_days ?? 0)
          ? html`<p class="hint confirm-mismatch">
              The last preview was for a different window; run a preview of this window to see what
              it deletes.
            </p>`
          : nothing}
        <sl-button
          slot="footer"
          class="confirm-cancel"
          @click=${() => {
            this.pendingSave = null;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          class="confirm-save"
          variant="danger"
          @click=${() => void this.confirmSave()}
          >Save and delete</sl-button
        >
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-admin-chat-retention': ScionAdminChatRetention;
  }
}
