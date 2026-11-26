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
 * The Decision retention panel of Admin > Server Config: hub-wide retention
 * settings for settled decisions (GET/PUT /api/v1/admin/decision-retention).
 *
 * Rendered only while the hub.decision_retention web flag is on; the flag only
 * controls presentation, and the hub answers 404 on the API while the
 * experiment is off. A save names the revision it was made against
 * (expected_revision); a 409 reloads the current values.
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

/** Sweep tuning, as pkg/hub/admin_decision_retention.go sends it. */
export interface DecisionRetentionSweep {
  page_size: number;
  tick_budget_seconds: number;
  tick_interval_minutes: number;
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
  /** Projects with at least one deleted decision or answer. */
  projects: number;
  totals: DecisionRetentionCounts;
  retention_days: number;
}

/** GET/PUT response of /api/v1/admin/decision-retention. */
export interface DecisionRetentionSettings {
  enabled: boolean;
  /** null = no hub default window: settled decisions are kept forever. */
  retention_days: number | null;
  sweep: DecisionRetentionSweep;
  revision: number;
  /** The most recent retention runs, newest first. */
  runs?: DecisionRetentionRun[] | undefined;
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
  sweep: Record<SweepKey, string>;
}

/** A validated PUT body, or the problems that keep the form from being saved. */
export type DecisionRetentionPutBody =
  | {
      ok: true;
      body: {
        enabled: boolean;
        retention_days: number | null;
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
 * Validate the form and build the PUT body. An empty hub default window is
 * sent as an explicit null (keep forever); every field is sent with the
 * revision the form was loaded at.
 */
export function buildDecisionRetentionPutBody(
  form: { enabled: boolean; days: string; sweep: Record<SweepKey, string> },
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

/**
 * The warning shown while decision retention is enabled: what it deletes, with
 * the hub default window (null when none is set, 'invalid' while the field
 * holds a value that is not a valid window).
 */
export function decisionRetentionWarning(days: number | null | 'invalid'): string {
  if (days === 'invalid') return '';
  if (days === null) {
    return 'No hub default window is set: settled decisions are kept forever.';
  }
  return `While enabled, settled decisions with no activity for ${days} days are permanently deleted from this hub.`;
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
 * Whether saving `next` deletes more than the saved settings do: it enables
 * retention (with a window), or, while enabled, sets a first window or
 * shortens it. Such a save asks for confirmation first.
 */
export function needsDecisionPurgeConfirmation(
  saved: Pick<DecisionRetentionSettings, 'enabled' | 'retention_days'>,
  next: { enabled: boolean; retention_days: number | null }
): boolean {
  if (!next.enabled) return false;
  if (!saved.enabled) return next.retention_days !== null;
  if (next.retention_days === null) return false;
  return saved.retention_days === null || next.retention_days < saved.retention_days;
}

function formFrom(s: DecisionRetentionSettings): DecisionRetentionForm {
  return {
    enabled: s.enabled,
    days: s.retention_days === null ? '' : String(s.retention_days),
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
  /** The validated PUT body waiting for the purge confirmation. */
  @state() private pendingSave: Extract<DecisionRetentionPutBody, { ok: true }>['body'] | null =
    null;

  private loadRequested = false;
  /** Re-renders the last run timestamp when the display zone changes. */
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
      gap: 0.75rem;
      margin-top: 1rem;
      flex-wrap: wrap;
    }
    .meta {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      margin-right: auto;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFlagsChanged);
    this.onFlagsChanged();
  }

  override disconnectedCallback(): void {
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFlagsChanged);
    super.disconnectedCallback();
  }

  private applySettings(s: DecisionRetentionSettings): void {
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
    const warning = form.enabled
      ? decisionRetentionWarning(!windowSet ? null : windowValid ? days : 'invalid')
      : '';
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
      ${this.renderConfirmDialog()}
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
            ? `Enabling decision retention with a ${body?.retention_days ?? ''}-day window permanently deletes settled decisions older than the window.`
            : `Shortening the hub default window to ${body?.retention_days ?? ''} days permanently deletes settled decisions older than the new window.`}
          Deleted decisions and answers cannot be restored; take a database backup first.
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
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-admin-decision-retention': ScionAdminDecisionRetention;
  }
}
