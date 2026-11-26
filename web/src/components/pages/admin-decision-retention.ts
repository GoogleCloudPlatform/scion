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
 * The Decision retention panel of Admin > Server Config: hub-wide and
 * per-project retention settings for settled decisions
 * (GET/PUT /api/v1/admin/decision-retention), plus dry-run preview and manual
 * run via the decision-retention-preview / decision-retention-run maintenance
 * operations.
 *
 * Rendered only while the hub.decision_retention web flag is on; the flag only
 * controls presentation, and the hub answers 404 on the API and maintenance
 * operations while the experiment is off. A save names the revision it was
 * made against (expected_revision); a 409 reloads the current values.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';
import { apiFetch, apiErrorMessageFromBody } from '../../client/api.js';
import {
  DECISION_RETENTION_FLAG,
  FEATURE_FLAGS_CHANGED_EVENT,
  isFeatureEnabled,
} from '../../utils/feature-flags.js';
import { formatInstantWithZone } from '../../utils/time.js';
import { formatNumber } from '../../utils/format-number.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';

const API_PATH = '/api/v1/admin/decision-retention';
const PROJECTS_PATH = '/api/v1/projects';
const PREVIEW_OP = 'decision-retention-preview';
const MANUAL_RUN_OP = 'decision-retention-run';
const PREVIEW_RUN_PATH = `/api/v1/admin/maintenance/operations/${PREVIEW_OP}/run`;
const MANUAL_RUN_PATH = `/api/v1/admin/maintenance/operations/${MANUAL_RUN_OP}/run`;

/** How often a started preview or manual run is checked, and for how long at most. */
const OP_POLL_MS = 2000;
const OP_POLL_MAX = 900;
/** Stop polling with an error after this many consecutive non-2xx/network failures. */
const OP_POLL_MAX_ERRORS = 5;

/** Sweep tuning, as pkg/hub/admin_decision_retention.go sends it. */
export interface DecisionRetentionSweep {
  page_size: number;
  tick_budget_seconds: number;
  tick_interval_minutes: number;
}

/** Per-project override mode (design-8984 §3.1). */
export type DecisionRetentionOverrideMode = 'days' | 'keep_forever';

/** Per-project retention override, as pkg/config/opsettings/decision_retention.go sends it. */
export interface DecisionRetentionOverride {
  mode: DecisionRetentionOverrideMode;
  days?: number | undefined;
}

/** Aggregate decision and answer counts for a retention run. */
export interface DecisionRetentionCounts {
  decisions?: number;
  answers?: number;
}

/** One decision_retention_runs row, as pkg/hub/admin_decision_retention.go sends it. */
export interface DecisionRetentionRun {
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
  /** Projects with at least one deleted (or previewed) decision or answer. */
  projects: number;
  totals: DecisionRetentionCounts;
  counts?: Record<string, DecisionRetentionCounts>;
  retention_days: number;
}

/** GET/PUT response of /api/v1/admin/decision-retention. */
export interface DecisionRetentionSettings {
  enabled: boolean;
  /** null = no hub default window: settled decisions are kept forever. */
  retention_days: number | null;
  /** Map of project UUID -> per-project override. */
  project_overrides?: Record<string, DecisionRetentionOverride> | undefined;
  sweep: DecisionRetentionSweep;
  revision: number;
  /** The most recent retention runs, newest first. */
  runs?: DecisionRetentionRun[] | undefined;
}

/** Minimal project option from GET /api/v1/projects for the overrides picker. */
export interface DecisionRetentionProjectOption {
  id: string;
  name: string;
  slug?: string;
}

/** Bounds of the settings, as pkg/config/opsettings/decision_retention.go sets them. */
export const DECISION_RETENTION_MIN_DAYS = 30;
export const DECISION_RETENTION_MAX_DAYS = 36500;

type SweepKey = keyof DecisionRetentionSweep;

const SWEEP_FIELDS: ReadonlyArray<{
  key: SweepKey;
  label: string;
  hint: string;
  min: number;
  max: number;
}> = [
  {
    key: 'page_size',
    label: 'Page size',
    hint: 'Candidate decision rows scanned per page (50 to 1000; default 200).',
    min: 50,
    max: 1000,
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
    hint: 'Time between sweep ticks (1 to 1440; default 60).',
    min: 1,
    max: 1440,
  },
];

/** The form's values, as typed (strings, so an empty field stays empty). */
interface DecisionRetentionForm {
  enabled: boolean;
  days: string;
  overrides: Record<string, DecisionRetentionOverride>;
  sweep: Record<SweepKey, string>;
}

/** A validated PUT body, or the problems that keep the form from being saved. */
export type DecisionRetentionPutBody =
  | {
      ok: true;
      body: {
        enabled: boolean;
        retention_days: number | null;
        project_overrides: Record<string, DecisionRetentionOverride>;
        sweep: DecisionRetentionSweep;
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
 * Validate a single per-project override entry.
 */
export function validateDecisionRetentionOverride(
  mode: DecisionRetentionOverrideMode,
  daysInput: string
): { ok: true; override: DecisionRetentionOverride } | { ok: false; error: string } {
  if (mode === 'keep_forever') {
    return { ok: true, override: { mode: 'keep_forever' } };
  }
  const days = parseWholeNumber(daysInput);
  if (days === null || days < DECISION_RETENTION_MIN_DAYS || days > DECISION_RETENTION_MAX_DAYS) {
    return {
      ok: false,
      error: `Override window must be a whole number of days from ${DECISION_RETENTION_MIN_DAYS} to ${DECISION_RETENTION_MAX_DAYS}.`,
    };
  }
  return { ok: true, override: { mode: 'days', days } };
}

/**
 * Validate the form and build the PUT body. An empty hub default window is
 * sent as an explicit null (keep forever); overrides are sent in full so
 * removals persist; every field is sent with the revision the form was loaded at.
 */
export function buildDecisionRetentionPutBody(
  form: {
    enabled: boolean;
    days: string;
    overrides?: Record<string, DecisionRetentionOverride>;
    sweep: Record<SweepKey, string>;
  },
  revision: number
): DecisionRetentionPutBody {
  const errors: string[] = [];
  let days: number | null = null;
  if (form.days.trim() !== '') {
    days = parseWholeNumber(form.days);
    if (days === null || days < DECISION_RETENTION_MIN_DAYS || days > DECISION_RETENTION_MAX_DAYS) {
      errors.push(
        `Hub default window must be a whole number of days from ${DECISION_RETENTION_MIN_DAYS} to ${DECISION_RETENTION_MAX_DAYS}, or empty to keep decisions forever.`
      );
    }
  }
  const projectOverrides: Record<string, DecisionRetentionOverride> = {};
  for (const [pid, ov] of Object.entries(form.overrides ?? {})) {
    if (ov.mode === 'keep_forever') {
      projectOverrides[pid] = { mode: 'keep_forever' };
    } else if (
      ov.mode === 'days' &&
      typeof ov.days === 'number' &&
      Number.isInteger(ov.days) &&
      ov.days >= DECISION_RETENTION_MIN_DAYS &&
      ov.days <= DECISION_RETENTION_MAX_DAYS
    ) {
      projectOverrides[pid] = { mode: 'days', days: ov.days };
    } else {
      errors.push(
        `Project override for ${pid} must be Keep forever or a whole number of days from ${DECISION_RETENTION_MIN_DAYS} to ${DECISION_RETENTION_MAX_DAYS}.`
      );
    }
  }
  const sweep = {} as DecisionRetentionSweep;
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
      retention_days: days,
      project_overrides: projectOverrides,
      sweep,
      expected_revision: revision,
    },
  };
}

/**
 * The messages of a failed save: the per-field `errors` of a
 * validation_failed body, else the body's error message.
 */
export function decisionRetentionSaveErrors(body: unknown, fallback: string): string[] {
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

/** Whether a policy has any active day window (hub default or any days override). */
export function policyHasAnyWindow(policy: {
  retention_days: number | null;
  project_overrides?: Record<string, DecisionRetentionOverride> | undefined;
}): boolean {
  if (policy.retention_days !== null && policy.retention_days > 0) return true;
  for (const ov of Object.values(policy.project_overrides ?? {})) {
    if (ov.mode === 'days' && typeof ov.days === 'number' && ov.days > 0) return true;
  }
  return false;
}

/**
 * The warning shown while decision retention is enabled: what it deletes, with
 * the hub default window (null when none is set, 'invalid' while the field
 * holds a value that is not a valid window) and whether any project override
 * sets a day window.
 */
export function decisionRetentionWarning(
  days: number | null | 'invalid',
  hasDayOverride = false
): string {
  if (days === 'invalid') return '';
  if (days === null) {
    if (hasDayOverride) {
      return 'No hub default window is set: settled decisions are deleted only in projects with a day-window override.';
    }
    return 'No hub default window is set: settled decisions are kept forever.';
  }
  return `While enabled, settled decisions with no activity for ${days} days are permanently deleted from this hub (unless a project overrides the policy).`;
}

function plural(n: number, one: string, many: string): string {
  return `${formatNumber(n)} ${n === 1 ? one : many}`;
}

function formatWhen(iso: string | null): string {
  if (!iso) return '';
  return formatInstantWithZone(iso) || iso;
}

/**
 * The one-line summary of the most recent non-dry-run retention run.
 */
export function lastDecisionRunSummary(runs: DecisionRetentionRun[] | null | undefined): string {
  const run = runs?.find((r) => !r.dry_run);
  if (!run) return 'None yet';
  if (run.status === 'running') {
    return `Running (started ${formatWhen(run.started_at)})`;
  }
  if (run.status === 'failed') {
    return `Failed (${formatWhen(run.started_at)})${run.error ? `: ${run.error}` : ''}`;
  }
  const decisions = run.totals?.decisions ?? 0;
  const answers = run.totals?.answers ?? 0;
  const when = formatWhen(run.finished_at ?? run.started_at);
  return `${when} · ${plural(decisions, 'decision', 'decisions')}, ${plural(
    answers,
    'answer',
    'answers'
  )} deleted in ${plural(run.projects, 'project', 'projects')}`;
}

/**
 * The one-line estimate from the most recent dry-run preview, stamped with its
 * start time and saved hub default window. Worded as an estimate because rows
 * can age past the cutoff during a subsequent manual run.
 */
export function lastDecisionPreviewSummary(
  runs: DecisionRetentionRun[] | null | undefined,
  running = false
): string {
  if (running) return 'Preview running…';
  const preview = runs?.find((r) => r.dry_run);
  if (!preview) return '';
  if (preview.status === 'running') return 'Preview running…';
  if (preview.status === 'failed') {
    return `Last preview failed${preview.error ? `: ${preview.error}` : '.'}`;
  }
  const decisions = preview.totals?.decisions ?? 0;
  const when = formatWhen(preview.started_at);
  const policyLabel =
    preview.retention_days > 0 ? `${preview.retention_days}-day default` : 'no hub default';
  const stamp = when ? `${when}, ${policyLabel}` : policyLabel;
  return `Preview estimate (${stamp}): ${plural(decisions, 'decision', 'decisions')} would be deleted now.`;
}

function effectiveDaysForProject(
  policy: {
    retention_days: number | null;
    project_overrides?: Record<string, DecisionRetentionOverride> | undefined;
  },
  projectId: string
): number | null {
  const ov = policy.project_overrides?.[projectId];
  if (ov) {
    return ov.mode === 'days' && typeof ov.days === 'number' ? ov.days : null;
  }
  return policy.retention_days;
}

/**
 * Whether saving `next` deletes more than the saved settings do: it enables
 * retention with at least one active window, or, while enabled, adds/shortens
 * the hub default window or gives any project a shorter effective window than
 * before (including lifting a keep_forever hold while a hub default is set).
 */
export function needsDecisionPurgeConfirmation(
  saved: Pick<DecisionRetentionSettings, 'enabled' | 'retention_days' | 'project_overrides'>,
  next: {
    enabled: boolean;
    retention_days: number | null;
    project_overrides?: Record<string, DecisionRetentionOverride> | undefined;
  }
): boolean {
  if (!next.enabled) return false;
  if (!saved.enabled) return policyHasAnyWindow(next);

  // Hub default added or shortened.
  if (
    next.retention_days !== null &&
    (saved.retention_days === null || next.retention_days < saved.retention_days)
  ) {
    return true;
  }

  // Any overridden project now has a newly active or shorter effective window.
  const projectIds = new Set([
    ...Object.keys(saved.project_overrides ?? {}),
    ...Object.keys(next.project_overrides ?? {}),
  ]);
  for (const pid of projectIds) {
    const prevDays = effectiveDaysForProject(saved, pid);
    const nextDays = effectiveDaysForProject(next, pid);
    if (nextDays !== null && (prevDays === null || nextDays < prevDays)) {
      return true;
    }
  }
  return false;
}

function cloneOverrides(
  overrides: Record<string, DecisionRetentionOverride> | undefined
): Record<string, DecisionRetentionOverride> {
  const out: Record<string, DecisionRetentionOverride> = {};
  for (const [k, v] of Object.entries(overrides ?? {})) {
    out[k] =
      v.mode === 'days' && v.days !== undefined
        ? { mode: 'days', days: v.days }
        : { mode: 'keep_forever' };
  }
  return out;
}

function formFrom(s: DecisionRetentionSettings): DecisionRetentionForm {
  return {
    enabled: s.enabled,
    days: s.retention_days === null ? '' : String(s.retention_days),
    overrides: cloneOverrides(s.project_overrides),
    sweep: {
      page_size: String(s.sweep.page_size),
      tick_budget_seconds: String(s.sweep.tick_budget_seconds),
      tick_interval_minutes: String(s.sweep.tick_interval_minutes),
    },
  };
}

@customElement('scion-admin-decision-retention')
export class ScionAdminDecisionRetention extends LitElement {
  @state() private flagOn = isFeatureEnabled(DECISION_RETENTION_FLAG);
  @state() private loading = false;
  @state() private saving = false;
  /** The hub answered 404: the experiment is off on the hub. */
  @state() private unavailable = false;
  @state() private loadError: string | null = null;
  @state() private saveErrors: string[] = [];
  @state() private success: string | null = null;
  @state() private settings: DecisionRetentionSettings | null = null;
  @state() private form: DecisionRetentionForm | null = null;
  @state() private projects: DecisionRetentionProjectOption[] = [];

  /** Add-override draft controls. */
  @state() private draftProjectId = '';
  @state() private draftMode: DecisionRetentionOverrideMode = 'days';
  @state() private draftDays = '';
  @state() private overrideError: string | null = null;

  /** The validated PUT body waiting for the purge confirmation. */
  @state() private pendingSave: Extract<DecisionRetentionPutBody, { ok: true }>['body'] | null =
    null;

  /** Maintenance op state (preview + manual run). */
  @state() private previewStarting = false;
  @state() private previewRunning = false;
  @state() private manualRunStarting = false;
  @state() private manualRunRunning = false;
  @state() private confirmManualRunOpen = false;
  @state() private opError: string | null = null;

  private loadRequested = false;
  private previewPoll: ReturnType<typeof setTimeout> | null = null;
  private manualRunPoll: ReturnType<typeof setTimeout> | null = null;
  /** Re-renders timestamps when the display zone changes. */
  readonly _zone = new DisplayZoneController(this);

  private readonly onFlagsChanged = (): void => {
    this.flagOn = isFeatureEnabled(DECISION_RETENTION_FLAG);
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
      color: var(--scion-text-secondary, #64748b);
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
    }
    .form-grid {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 1rem 1.25rem;
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
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }
    .last-run {
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      line-height: 1.4;
    }
    .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }
    .subsection-title {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 1.25rem 0 0.5rem 0;
    }
    table.overrides-table {
      width: 100%;
      border-collapse: collapse;
      font-size: 0.8125rem;
    }
    table.overrides-table th {
      text-align: left;
      font-weight: 600;
      color: var(--scion-text-secondary, #64748b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      padding: 0.375rem 0.5rem;
    }
    table.overrides-table td {
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      padding: 0.375rem 0.5rem;
      vertical-align: middle;
      color: var(--scion-text, #1e293b);
    }
    table.overrides-table td.muted {
      color: var(--scion-text-muted, #64748b);
    }
    table.overrides-table td.action-col {
      text-align: right;
      width: 2.5rem;
    }
    @media (max-width: 640px) {
      .col-hide {
        display: none;
      }
    }
    .tag {
      display: inline-block;
      padding: 0.05rem 0.45rem;
      border-radius: 999px;
      font-size: 0.75rem;
      font-weight: 600;
      background: var(--scion-badge-neutral-bg, #f1f5f9);
      color: var(--scion-badge-neutral-text, #334155);
    }
    .tag.hold {
      background: var(--scion-badge-primary-bg, #dbeafe);
      color: var(--scion-badge-primary-text, #1d4ed8);
    }
    .row-actions {
      display: flex;
      gap: 0.5rem;
      align-items: center;
      margin-top: 0.65rem;
      flex-wrap: wrap;
    }
    .row-actions .override-project {
      min-width: 12rem;
      flex: 1 1 12rem;
      max-width: 18rem;
    }
    .row-actions .override-mode {
      width: 9rem;
    }
    .row-actions .override-days {
      width: 6rem;
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
      background: var(--scion-badge-danger-bg, #fee2e2);
      color: var(--scion-badge-danger-text, #991b1b);
      border: 1px solid var(--scion-error-border, #fca5a5);
    }
    .status-message.warning {
      background: var(--sl-color-warning-50, #fffbeb);
      color: var(--sl-color-warning-800, #92400e);
      border: 1px solid var(--sl-color-warning-200, #fde68a);
    }
    .actions {
      display: flex;
      align-items: center;
      justify-content: flex-end;
      gap: 0.5rem;
      margin-top: 1rem;
      flex-wrap: wrap;
    }
    .meta {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      margin-right: auto;
    }
    .preview {
      font-size: 0.8rem;
      color: var(--scion-text-secondary, #475569);
    }
    .preview-saved-hint {
      text-align: right;
      margin-top: 0.25rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFlagsChanged);
    this.onFlagsChanged();
  }

  override disconnectedCallback(): void {
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFlagsChanged);
    this.stopPolls();
    super.disconnectedCallback();
  }

  private stopPolls(): void {
    if (this.previewPoll !== null) {
      clearTimeout(this.previewPoll);
      this.previewPoll = null;
    }
    if (this.manualRunPoll !== null) {
      clearTimeout(this.manualRunPoll);
      this.manualRunPoll = null;
    }
  }

  private applySettings(s: DecisionRetentionSettings): void {
    this.settings = s;
    this.form = formFrom(s);
  }

  private async loadAllProjects(): Promise<DecisionRetentionProjectOption[]> {
    const all: DecisionRetentionProjectOption[] = [];
    const seenIds = new Set<string>();
    const seenCursors = new Set<string>();
    let cursor = '';
    for (let page = 0; page < 50; page++) {
      const url = cursor ? `${PROJECTS_PATH}?cursor=${encodeURIComponent(cursor)}` : PROJECTS_PATH;
      const res = await apiFetch(url).catch(() => null);
      if (!res || !res.ok) break;
      const pdata = (await res.json().catch(() => null)) as {
        projects?: DecisionRetentionProjectOption[];
        nextCursor?: string;
      } | null;
      if (Array.isArray(pdata?.projects)) {
        for (const p of pdata.projects) {
          if (p?.id && !seenIds.has(p.id)) {
            seenIds.add(p.id);
            all.push(p);
          }
        }
      }
      const next = typeof pdata?.nextCursor === 'string' ? pdata.nextCursor.trim() : '';
      if (!next || seenCursors.has(next)) break;
      seenCursors.add(next);
      cursor = next;
    }
    return all;
  }

  private async load(): Promise<void> {
    this.loadRequested = true;
    this.loading = true;
    this.loadError = null;
    try {
      const [res, projects] = await Promise.all([apiFetch(API_PATH), this.loadAllProjects()]);
      if (projects.length > 0) {
        this.projects = projects;
      }
      if (res.status === 404) {
        this.unavailable = true;
        return;
      }
      if (!res.ok) {
        this.loadError = decisionRetentionSaveErrors(
          await res.json().catch(() => null),
          'Failed to load decision retention settings'
        ).join(' ');
        return;
      }
      this.unavailable = false;
      this.applySettings((await res.json()) as DecisionRetentionSettings);
    } catch {
      this.loadError = 'Failed to load decision retention settings';
    } finally {
      this.loading = false;
    }
  }

  /** Reload runs after a preview or manual run completes, preserving unsaved form edits. */
  private async reloadRuns(): Promise<void> {
    try {
      const res = await apiFetch(API_PATH);
      if (!res.ok || !this.settings) return;
      const s = (await res.json()) as DecisionRetentionSettings;
      this.settings = {
        ...this.settings,
        runs: s.runs,
      };
    } catch {
      // Keep existing run history on transient errors.
    }
  }

  private async save(): Promise<void> {
    if (!this.form || !this.settings) return;
    this.saveErrors = [];
    this.success = null;
    const built = buildDecisionRetentionPutBody(this.form, this.settings.revision);
    if (!built.ok) {
      this.saveErrors = built.errors;
      return;
    }
    if (needsDecisionPurgeConfirmation(this.settings, built.body)) {
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

  private async put(body: Extract<DecisionRetentionPutBody, { ok: true }>['body']): Promise<void> {
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
        this.saveErrors = decisionRetentionSaveErrors(
          await res.json().catch(() => null),
          'Failed to save decision retention settings'
        );
        return;
      }
      this.applySettings((await res.json()) as DecisionRetentionSettings);
      this.success = 'Decision retention settings saved.';
    } catch {
      this.saveErrors = ['Failed to save decision retention settings'];
    } finally {
      this.saving = false;
    }
  }

  private async startPreview(): Promise<void> {
    this.opError = null;
    this.previewStarting = true;
    try {
      const res = await apiFetch(PREVIEW_RUN_PATH, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ params: {} }),
      });
      const body = (await res.json().catch(() => null)) as { runId?: string } | null;
      if (!res.ok || !body?.runId) {
        this.opError = decisionRetentionSaveErrors(body, 'Failed to start preview').join(' ');
        return;
      }
      this.previewRunning = true;
      await this.reloadRuns();
      this.pollOperation(PREVIEW_OP, body.runId, 0);
    } catch {
      this.opError = 'Failed to start preview';
    } finally {
      this.previewStarting = false;
    }
  }

  private async startManualRun(): Promise<void> {
    this.confirmManualRunOpen = false;
    this.opError = null;
    this.manualRunStarting = true;
    try {
      const res = await apiFetch(MANUAL_RUN_PATH, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ params: {} }),
      });
      const body = (await res.json().catch(() => null)) as { runId?: string } | null;
      if (!res.ok || !body?.runId) {
        this.opError = decisionRetentionSaveErrors(
          body,
          'Failed to start decision retention run'
        ).join(' ');
        return;
      }
      this.manualRunRunning = true;
      await this.reloadRuns();
      this.pollOperation(MANUAL_RUN_OP, body.runId, 0);
    } catch {
      this.opError = 'Failed to start decision retention run';
    } finally {
      this.manualRunStarting = false;
    }
  }

  private pollOperation(op: string, runId: string, attempt: number, consecutiveErrors = 0): void {
    const isPreview = op === PREVIEW_OP;
    if (isPreview && this.previewPoll !== null) {
      clearTimeout(this.previewPoll);
      this.previewPoll = null;
    }
    if (!isPreview && this.manualRunPoll !== null) {
      clearTimeout(this.manualRunPoll);
      this.manualRunPoll = null;
    }
    const timer = setTimeout(() => {
      if (isPreview) this.previewPoll = null;
      else this.manualRunPoll = null;
      void (async () => {
        let running = attempt + 1 < OP_POLL_MAX;
        let nextErrors = consecutiveErrors;
        try {
          const res = await apiFetch(
            `/api/v1/admin/maintenance/operations/${op}/runs/${encodeURIComponent(runId)}`
          );
          if (res.ok) {
            nextErrors = 0;
            const run = (await res.json().catch(() => null)) as {
              status?: string;
              error?: string;
              log?: string;
            } | null;
            if (run?.status === 'failed') {
              const logTail = run.log
                ?.split('\n')
                .map((l) => l.trim())
                .filter(Boolean)
                .pop();
              this.opError =
                run.error?.trim() ||
                logTail ||
                (isPreview ? 'Preview failed.' : 'Decision retention run failed.');
              running = false;
            } else {
              running = running && run?.status === 'running';
            }
          } else {
            nextErrors++;
            if (nextErrors >= OP_POLL_MAX_ERRORS) {
              this.opError = isPreview
                ? `Failed to check preview status (HTTP ${res.status}).`
                : `Failed to check decision retention run status (HTTP ${res.status}).`;
              running = false;
            }
          }
        } catch {
          nextErrors++;
          if (nextErrors >= OP_POLL_MAX_ERRORS) {
            this.opError = isPreview
              ? 'Lost contact while checking preview status.'
              : 'Lost contact while checking decision retention run status.';
            running = false;
          }
        }
        if (!this.isConnected) return;
        if (running) {
          this.pollOperation(op, runId, attempt + 1, nextErrors);
          return;
        }
        if (isPreview) this.previewRunning = false;
        else this.manualRunRunning = false;
        await this.reloadRuns();
      })();
    }, OP_POLL_MS);
    if (isPreview) this.previewPoll = timer;
    else this.manualRunPoll = timer;
  }

  private addOverride(): void {
    if (!this.form) return;
    this.overrideError = null;
    const pid = this.draftProjectId.trim();
    if (!pid) {
      this.overrideError = 'Choose a project to add an override.';
      return;
    }
    const validated = validateDecisionRetentionOverride(this.draftMode, this.draftDays);
    if (!validated.ok) {
      this.overrideError = validated.error;
      return;
    }
    this.updateForm({
      overrides: {
        ...this.form.overrides,
        [pid]: validated.override,
      },
    });
    this.draftProjectId = '';
    this.draftDays = '';
    this.draftMode = 'days';
  }

  private removeOverride(projectId: string): void {
    if (!this.form) return;
    const next = { ...this.form.overrides };
    delete next[projectId];
    this.overrideError = null;
    this.updateForm({ overrides: next });
  }

  private projectLabel(projectId: string): string {
    const found = this.projects.find((p) => p.id === projectId);
    return found?.name || found?.slug || projectId;
  }

  private updateForm(patch: Partial<DecisionRetentionForm>): void {
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
      <div class="section decision-retention">
        <h3 class="section-title">Decision retention</h3>
        <ul class="intro">
          <li>Nothing is deleted while retention is disabled.</li>
          <li>
            Only settled decisions go: answered, dismissed, withdrawn or replaced. Open decisions
            are never deleted.
          </li>
          <li>
            A prompt is deleted, with its answers, once every decision on it has had no activity for
            the window.
          </li>
          <li>Deletes are permanent. Take a database backup before enabling.</li>
        </ul>
        ${this.renderBody()}
      </div>
    `;
  }

  private renderBody() {
    if (this.unavailable) {
      return html`<div class="hint unavailable">
        Decision retention is not enabled on this hub (experiment hub.decision_retention is off).
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
    const windowValid =
      days !== null && days >= DECISION_RETENTION_MIN_DAYS && days <= DECISION_RETENTION_MAX_DAYS;
    const hasDayOverride = Object.values(form.overrides).some(
      (ov) => ov.mode === 'days' && typeof ov.days === 'number' && ov.days > 0
    );
    const warning = form.enabled
      ? decisionRetentionWarning(!windowSet ? null : windowValid ? days : 'invalid', hasDayOverride)
      : '';
    const previewBusy = this.previewStarting || this.previewRunning;
    const manualRunBusy = this.manualRunStarting || this.manualRunRunning;
    const canManualRun = this.settings.enabled && policyHasAnyWindow(this.settings);
    const previewSummary = lastDecisionPreviewSummary(this.settings.runs, this.previewRunning);

    return html`
      <div class="form-grid">
        <div class="form-field full-width">
          <sl-switch
            class="retention-enabled"
            ?checked=${form.enabled}
            ?disabled=${this.saving}
            @sl-change=${(e: Event) =>
              this.updateForm({ enabled: (e.target as HTMLInputElement).checked })}
            >Enable decision retention</sl-switch
          >
          <span class="hint">Off acts as a legal hold for every project.</span>
        </div>
        <div class="form-field">
          <label for="decision-retention-days">Hub default window (days)</label>
          <sl-input
            id="decision-retention-days"
            class="retention-days"
            type="number"
            size="small"
            min=${DECISION_RETENTION_MIN_DAYS}
            max=${DECISION_RETENTION_MAX_DAYS}
            placeholder="Keep forever"
            .value=${form.days}
            ?disabled=${this.saving}
            @sl-input=${(e: Event) =>
              this.updateForm({ days: (e.target as HTMLInputElement).value })}
          ></sl-input>
          <span class="hint"
            >Applies to every project without an override. Empty keeps decisions forever; at least
            ${DECISION_RETENTION_MIN_DAYS} days.</span
          >
        </div>
        <div class="form-field">
          <label>Last run</label>
          <span class="last-run">${lastDecisionRunSummary(this.settings.runs)}</span>
          <span class="hint">Runs keep counts per project only, never content.</span>
        </div>
      </div>

      ${this.renderOverridesSection(form)}

      <details class="advanced">
        <summary>Advanced: sweep tuning</summary>
        <div class="form-grid">
          ${SWEEP_FIELDS.map(
            (f) => html`
              <div class="form-field">
                <label for="decision-sweep-${f.key}">${f.label}</label>
                <sl-input
                  id="decision-sweep-${f.key}"
                  class="sweep-input"
                  data-key=${f.key}
                  type="number"
                  size="small"
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
      ${this.opError
        ? html`<div class="status-message error op-error" role="alert">${this.opError}</div>`
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
        <span class="meta">Revision: ${this.settings.revision}</span>
        ${previewSummary ? html`<span class="preview">${previewSummary}</span>` : nothing}
        <sl-button
          class="run-preview"
          size="small"
          ?loading=${previewBusy}
          ?disabled=${previewBusy || manualRunBusy}
          @click=${() => void this.startPreview()}
          >Preview</sl-button
        >
        <sl-button
          class="run-manual"
          size="small"
          ?loading=${manualRunBusy}
          ?disabled=${!canManualRun || manualRunBusy || previewBusy}
          @click=${() => {
            this.confirmManualRunOpen = true;
          }}
          >Run now</sl-button
        >
        <sl-button
          class="retention-save"
          variant="primary"
          size="small"
          ?loading=${this.saving}
          ?disabled=${this.saving}
          @click=${() => void this.save()}
          >Save</sl-button
        >
      </div>
      <div class="hint preview-saved-hint">Preview evaluates the saved settings.</div>
      ${this.renderConfirmDialog()} ${this.renderManualRunDialog()}
    `;
  }

  private renderOverridesSection(form: DecisionRetentionForm) {
    const entries = Object.entries(form.overrides).sort(([idA], [idB]) =>
      this.projectLabel(idA).localeCompare(this.projectLabel(idB))
    );
    const availableProjects = this.projects
      .filter((p) => !Object.prototype.hasOwnProperty.call(form.overrides, p.id))
      .sort((a, b) => (a.name || a.slug || a.id).localeCompare(b.name || b.slug || b.id));

    return html`
      <h4 class="subsection-title">Project overrides</h4>
      ${entries.length === 0
        ? html`<p class="hint no-overrides">No project overrides configured.</p>`
        : html`<table class="overrides-table">
            <thead>
              <tr>
                <th>Project</th>
                <th>Policy</th>
                <th class="col-hide">Effective</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              ${entries.map(
                ([pid, ov]) => html`
                  <tr class="override-row" data-project-id=${pid}>
                    <td>${this.projectLabel(pid)}</td>
                    <td>
                      ${ov.mode === 'keep_forever'
                        ? html`<span class="tag hold">Keep forever</span>`
                        : html`<span class="tag">${ov.days} days</span>`}
                    </td>
                    <td class="col-hide muted">
                      ${ov.mode === 'keep_forever' ? 'never deleted' : `after ${ov.days} days`}
                    </td>
                    <td class="action-col">
                      <sl-icon-button
                        class="remove-override"
                        name="trash"
                        label="Remove override"
                        ?disabled=${this.saving}
                        @click=${() => this.removeOverride(pid)}
                      ></sl-icon-button>
                    </td>
                  </tr>
                `
              )}
            </tbody>
          </table>`}
      <div class="row-actions">
        <sl-select
          class="override-project"
          placeholder="Choose a project"
          size="small"
          .value=${this.draftProjectId}
          ?disabled=${this.saving || availableProjects.length === 0}
          @sl-change=${(e: Event) => {
            this.draftProjectId = (e.target as HTMLInputElement).value;
            this.overrideError = null;
          }}
        >
          ${availableProjects.map(
            (p) => html`<sl-option value=${p.id}>${p.name || p.slug || p.id}</sl-option>`
          )}
        </sl-select>
        <sl-select
          class="override-mode"
          size="small"
          .value=${this.draftMode}
          ?disabled=${this.saving}
          @sl-change=${(e: Event) => {
            const val = (e.target as HTMLInputElement).value;
            this.draftMode = val === 'keep_forever' ? 'keep_forever' : 'days';
            this.overrideError = null;
          }}
        >
          <sl-option value="days">Days</sl-option>
          <sl-option value="keep_forever">Keep forever</sl-option>
        </sl-select>
        ${this.draftMode === 'days'
          ? html`<sl-input
              class="override-days"
              type="number"
              size="small"
              min=${DECISION_RETENTION_MIN_DAYS}
              max=${DECISION_RETENTION_MAX_DAYS}
              placeholder="Days"
              .value=${this.draftDays}
              ?disabled=${this.saving}
              @sl-input=${(e: Event) => {
                this.draftDays = (e.target as HTMLInputElement).value;
                this.overrideError = null;
              }}
            ></sl-input>`
          : nothing}
        <sl-button
          class="add-override"
          size="small"
          ?disabled=${this.saving}
          @click=${() => this.addOverride()}
        >
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Add override
        </sl-button>
      </div>
      ${this.overrideError
        ? html`<div class="status-message error override-error" role="alert">
            ${this.overrideError}
          </div>`
        : nothing}
    `;
  }

  private renderConfirmDialog() {
    const body = this.pendingSave;
    const s = this.settings;
    const enabling = body !== null && s !== null && !s.enabled;
    return html`
      <sl-dialog
        class="confirm-dialog"
        label="Delete settled decisions permanently?"
        ?open=${body !== null}
        @sl-request-close=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.pendingSave = null;
        }}
      >
        <p class="confirm-change">
          ${enabling
            ? 'Enabling decision retention permanently deletes settled decisions older than the configured windows.'
            : 'This change shortens or activates a decision retention window and permanently deletes older settled decisions.'}
          Deleted decisions and answers cannot be restored; take a database backup first.
        </p>
        <p class="hint confirm-preview-note">
          Preview counts reflect the saved settings, not this change.
        </p>
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

  private renderManualRunDialog() {
    const previewSummary = lastDecisionPreviewSummary(this.settings?.runs);
    return html`
      <sl-dialog
        class="manual-run-dialog"
        label="Run decision retention now?"
        ?open=${this.confirmManualRunOpen}
        @sl-request-close=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.confirmManualRunOpen = false;
        }}
      >
        <p class="manual-run-change">
          This runs the saved decision retention policy now and permanently deletes eligible settled
          decisions and their answers.
        </p>
        ${previewSummary
          ? html`<p class="manual-run-preview">
              ${previewSummary} Actual deletions may be slightly higher if additional decisions age
              past the cutoff while the run executes.
            </p>`
          : nothing}
        <sl-button
          slot="footer"
          class="manual-run-cancel"
          @click=${() => {
            this.confirmManualRunOpen = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          class="manual-run-confirm"
          variant="danger"
          @click=${() => void this.startManualRun()}
          >Run now</sl-button
        >
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-admin-decision-retention': ScionAdminDecisionRetention;
  }
}
