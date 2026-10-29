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
  sweep: ChatRetentionSweep;
  last_applied_cutoff: string | null;
  revision: number;
}

/** Bounds of the settings, as pkg/config/opsettings/chat_retention.go sets them. */
export const RETENTION_MIN_DAYS = 7;
export const RETENTION_MAX_DAYS = 36500;

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
  sweep: Record<SweepKey, string>;
}

/** A saved PUT body, or the problems that keep the form from being saved. */
export type RetentionPutBody =
  | {
      ok: true;
      body: {
        enabled: boolean;
        messages_retention_days: number | null;
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
  form: { enabled: boolean; days: string; sweep: Record<SweepKey, string> },
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

function formFrom(s: ChatRetentionSettings): RetentionForm {
  return {
    enabled: s.enabled,
    days: s.messages_retention_days === null ? '' : String(s.messages_retention_days),
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

  private loadRequested = false;
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
    this.saving = true;
    try {
      const res = await apiFetch(API_PATH, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(built.body),
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
          <li>
            Nothing is deleted until the sweep is enabled <strong>and</strong> a retention window is
            set.
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
    const deleting = form.enabled && form.days.trim() !== '' && days !== null;
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
      ${deleting
        ? html`<div class="status-message warning delete-warning">
            While enabled, messages older than ${days} days are permanently deleted from this hub.
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
          ?disabled=${this.saving}
          @click=${() => void this.save()}
          >Save</sl-button
        >
        <span class="meta">Revision: ${this.settings.revision}</span>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-admin-chat-retention': ScionAdminChatRetention;
  }
}
