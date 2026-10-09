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
 * <scion-agent-config-form>: the shared agent configuration form
 * (ptone/scion#3952).
 *
 * One field table, AGENT_CONFIG_FIELDS, drives both rendering and emission,
 * so adding a field is one row. The form:
 *
 * - emits only the fields the user touched (collectConfigPatch). Touched
 *   state is kept per field, never inferred from "value differs from the
 *   loaded one", because Shoelace fires no change event when a user re-picks
 *   the value already shown.
 * - treats Clear as "inherit": a cleared field is sent as an explicit null,
 *   and the hub reverts it to the inherited value. Limits also offer
 *   Unlimited, sent as 0 (or "0" for a duration); clearing never means
 *   unlimited.
 * - shows, per field, whether it is editable now, held, needs a
 *   reincarnation, or is locked (with the reason), from the hub's
 *   editability for the agent and the caller.
 * - shows an unset field's inherited value as a placeholder labelled with
 *   its source, never as the control's value.
 *
 * It currently renders Model, Max turns and Max duration, grouped by
 * function (the create form's tabs), with a summary strip counting the
 * fields by what the agent's phase does with an edit.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type {
  AgentEditability,
  AgentEditDisposition,
  AgentEditTier,
  AgentFieldEditState,
  AgentInlineConfig,
  CapabilityField,
  HarnessAdvancedCapabilities,
} from '../../shared/types.js';

/** The function groups of the form: the create form's Additional Options tabs. */
export type AgentConfigTab = 'general' | 'auth' | 'prompts' | 'limits' | 'environment';

export const AGENT_CONFIG_TABS: ReadonlyArray<{ id: AgentConfigTab; label: string }> = [
  { id: 'general', label: 'General' },
  { id: 'auth', label: 'Auth & Security' },
  { id: 'prompts', label: 'Prompts' },
  { id: 'limits', label: 'Limits & Resources' },
  { id: 'environment', label: 'Environment & Labels' },
];

/** How a field is edited and emitted. */
export type AgentConfigControl = AgentConfigFieldDef['control'];

/** Keys of AgentInlineConfig whose value is a number. */
type NumberConfigKey = {
  [K in keyof AgentInlineConfig]-?: NonNullable<AgentInlineConfig[K]> extends number ? K : never;
}[keyof AgentInlineConfig];

/** Keys of AgentInlineConfig whose value is a string. */
type StringConfigKey = {
  [K in keyof AgentInlineConfig]-?: NonNullable<AgentInlineConfig[K]> extends string ? K : never;
}[keyof AgentInlineConfig];

/**
 * The config object the form emits: a subset of AgentInlineConfig in which
 * null means "clear, inherit". Typed so the compiler checks that each field
 * emits its key's JSON type (a duration is a string, so Unlimited is "0").
 */
export type AgentConfigPatch = {
  [K in keyof AgentInlineConfig]?: NonNullable<AgentInlineConfig[K]> | null;
};

interface AgentConfigFieldBase {
  /** Wire key, as in the hub's editability: "config.<json key>". */
  key: string;
  tab: AgentConfigTab;
  label: string;
  /** The field's tier; used when the hub sends no editability (create mode). */
  tier: AgentEditTier;
  help: string;
  /** The harness capability that gates the field, if any. */
  capability?: (caps: HarnessAdvancedCapabilities) => CapabilityField | undefined;
}

/** One row of the field table. control decides how it is edited and emitted. */
export type AgentConfigFieldDef = AgentConfigFieldBase &
  (
    | { control: 'limit-count'; configKey: NumberConfigKey }
    | { control: 'text' | 'limit-duration'; configKey: StringConfigKey }
  );

export const AGENT_CONFIG_FIELDS: ReadonlyArray<AgentConfigFieldDef> = [
  {
    key: 'config.model',
    configKey: 'model',
    tab: 'general',
    label: 'Model',
    control: 'text',
    tier: 'T1',
    help: 'Model ID or alias the harness runs.',
  },
  {
    key: 'config.max_turns',
    configKey: 'max_turns',
    tab: 'limits',
    label: 'Max turns',
    control: 'limit-count',
    tier: 'T1',
    help: 'Stop the agent after this many turns.',
    capability: (c) => c.limits?.max_turns,
  },
  {
    key: 'config.max_duration',
    configKey: 'max_duration',
    tab: 'limits',
    label: 'Max duration',
    control: 'limit-duration',
    tier: 'T1',
    help: 'Stop the agent after this long, e.g. 30m or 2h.',
    capability: (c) => c.limits?.max_duration,
  },
];

/** An inherited value shown as a placeholder, with where it comes from. */
export interface AgentConfigPlaceholder {
  /** The inherited value, when known. */
  value?: string;
  /** Where the value comes from, e.g. "applied config", "template or hub default". */
  source: string;
}

/** Per-field edit state. touched is derived from it, see isTouched. */
interface FieldDraft {
  /** The user typed into the control. */
  typed: boolean;
  text: string;
  /** The user pressed Clear: send null, inherit. */
  cleared: boolean;
  /** The user chose Unlimited: send 0. */
  unlimited: boolean;
}

function isTouched(d: FieldDraft | undefined): d is FieldDraft {
  return !!d && (d.typed || d.cleared || d.unlimited);
}

/** A Go duration, e.g. "90s", "1h30m". */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;

/** The disposition of a field the hub gave no editability for (create mode). */
const CREATE_MODE_STATE: AgentFieldEditState = { tier: 'T1', disposition: 'now' };

/** Event detail of `agent-config-change`, fired on every edit. */
export interface AgentConfigChangeDetail {
  /** Wire keys of the touched fields. */
  touched: string[];
}

@customElement('scion-agent-config-form')
export class ScionAgentConfigForm extends LitElement {
  /** 'create' renders every field as editable now; 'edit' follows editability. */
  @property() mode: 'create' | 'edit' = 'edit';

  /** The stored values (the agent's inline config). */
  @property({ attribute: false }) values: AgentInlineConfig = {};

  /** Inherited values for unset fields, by wire key. */
  @property({ attribute: false }) placeholders: Record<string, AgentConfigPlaceholder> = {};

  /** The hub's per-field editability for the agent and the caller. */
  @property({ attribute: false }) editability: AgentEditability | null = null;

  @property({ attribute: false }) harnessCapabilities: HarnessAdvancedCapabilities | null = null;

  /** Disables every control, e.g. while a save is in flight. */
  @property({ type: Boolean }) disabled = false;

  @state() private drafts: Record<string, FieldDraft> = {};

  /** The rendered fields. */
  get fields(): ReadonlyArray<AgentConfigFieldDef> {
    return AGENT_CONFIG_FIELDS;
  }

  /** Wire keys of the fields the user touched. */
  get touchedKeys(): string[] {
    return this.fields.filter((f) => isTouched(this.drafts[f.key])).map((f) => f.key);
  }

  /**
   * The config object for a PATCH or create request: only touched fields,
   * an explicit null for a cleared field, 0 for Unlimited.
   */
  collectConfigPatch(): AgentConfigPatch {
    const out: AgentConfigPatch = {};
    for (const f of this.fields) {
      const d = this.drafts[f.key];
      if (!isTouched(d) || !editableNow(this.fieldState(f))) continue;
      if (f.control === 'limit-count') {
        out[f.configKey] = emitCount(d);
      } else {
        out[f.configKey] = emitString(f.control, d);
      }
    }
    return out;
  }

  /** Validation errors of the touched fields; empty when the form can be sent. */
  validate(): string[] {
    const errors: string[] = [];
    for (const f of this.fields) {
      const d = this.drafts[f.key];
      if (!isTouched(d) || d.cleared || d.unlimited) continue;
      const text = d.text.trim();
      if (text === '') continue;
      if (f.control === 'limit-count' && !/^[1-9]\d*$/.test(text)) {
        errors.push(`${f.label} must be a whole number greater than 0, or Unlimited.`);
      }
      if (f.control === 'limit-duration' && !DURATION_RE.test(text)) {
        errors.push(`${f.label} must be a duration such as 30m or 2h, or Unlimited.`);
      }
    }
    return errors;
  }

  /** Forgets every edit, e.g. after a successful save. */
  reset(): void {
    this.drafts = {};
    this.emitChange();
  }

  /** The hub's state for f, narrowed by the harness capability. */
  fieldState(f: AgentConfigFieldDef): AgentFieldEditState {
    let st: AgentFieldEditState =
      this.mode === 'create'
        ? { ...CREATE_MODE_STATE, tier: f.tier }
        : (this.editability?.fields[f.key] ?? {
            tier: f.tier,
            disposition: 'locked',
            reason: 'The hub did not report whether this field can be edited.',
          });
    const cap =
      f.capability && this.harnessCapabilities ? f.capability(this.harnessCapabilities) : undefined;
    if (cap?.support === 'no' && st.disposition !== 'locked') {
      st = {
        ...st,
        disposition: 'locked',
        reason: cap.reason || "This agent's harness does not support this field.",
      };
    }
    return st;
  }

  private storedText(f: AgentConfigFieldDef): string {
    const v = this.values?.[f.configKey];
    if (v === undefined || v === null || v === '' || v === 0) return '';
    return String(v);
  }

  private editField(key: string, patch: Partial<FieldDraft>, f: AgentConfigFieldDef): void {
    const prev = this.drafts[key] ?? {
      typed: false,
      text: this.storedText(f),
      cleared: false,
      unlimited: false,
    };
    this.drafts = { ...this.drafts, [key]: { ...prev, ...patch } };
    this.emitChange();
  }

  private emitChange(): void {
    this.dispatchEvent(
      new CustomEvent<AgentConfigChangeDetail>('agent-config-change', {
        detail: { touched: this.touchedKeys },
        bubbles: true,
        composed: true,
      })
    );
  }

  private applyText(): string {
    const phase = this.editability?.phase;
    if (this.mode === 'create' || phase === 'created') return 'Applies at first start';
    if (phase === 'suspended') return 'Applies at resume';
    return 'Applies at next start';
  }

  private dispositionLabel(d: AgentEditDisposition): string {
    switch (d) {
      case 'now':
        return this.editability?.phase === 'suspended' ? 'apply at resume' : 'apply at next start';
      case 'held':
        return 'held until next start';
      case 'reincarnate':
        return 'need reincarnation';
      case 'immediate':
        return 'apply immediately';
      case 'locked':
        return 'locked';
    }
  }

  private renderSummary(): TemplateResult {
    const counts = new Map<AgentEditDisposition, number>();
    for (const f of this.fields) {
      const d = this.fieldState(f).disposition;
      counts.set(d, (counts.get(d) ?? 0) + 1);
    }
    const order: AgentEditDisposition[] = ['now', 'immediate', 'held', 'reincarnate', 'locked'];
    return html`
      <div class="summary" data-testid="tier-summary">
        ${order
          .filter((d) => (counts.get(d) ?? 0) > 0)
          .map(
            (d) =>
              html`<span class="chip chip-${d}" data-disposition=${d}
                >${counts.get(d)} ${this.dispositionLabel(d)}</span
              >`
          )}
      </div>
    `;
  }

  private renderStatus(f: AgentConfigFieldDef, st: AgentFieldEditState, d: FieldDraft | undefined) {
    switch (st.disposition) {
      case 'locked':
        return html`<div class="status locked" data-testid="locked-reason">
          <sl-icon name="lock"></sl-icon><span>${st.reason || 'Not editable.'}</span>
        </div>`;
      case 'held':
        return html`<div class="status">
          <sl-badge variant="warning" pill
            ><sl-icon name="hourglass-split"></sl-icon> Held — applies at next start</sl-badge
          >
        </div>`;
      case 'reincarnate':
        return html`<div class="status">
          <sl-badge variant="neutral" pill
            ><sl-icon name="arrow-repeat"></sl-icon> Needs reincarnation</sl-badge
          >
        </div>`;
      default: {
        const session = st.note === 'session' ? ' (the conversation continues)' : '';
        const clearNote =
          st.clearNeedsReincarnate && d && (d.cleared || d.unlimited)
            ? html`<span class="clear-note" data-testid="clear-note">
                ${d.cleared ? 'Clearing' : 'Unlimited'} takes effect at the next reincarnation; a
                plain start keeps the previous value.</span
              >`
            : nothing;
        return html`<div class="status help">
          <span>${f.help} ${this.applyText()}${session}.</span>${clearNote}
        </div>`;
      }
    }
  }

  private placeholderText(f: AgentConfigFieldDef): string {
    const p = this.placeholders?.[f.key];
    if (!p) return 'Not set';
    return p.value ? `${p.value} (${p.source})` : `Inherited from ${p.source}`;
  }

  private renderField(f: AgentConfigFieldDef): TemplateResult {
    const st = this.fieldState(f);
    const d = this.drafts[f.key];
    // Held and reincarnate-only edits are shown but not yet editable here:
    // saving them needs held edits on the hub, which this form does not
    // send yet.
    const locked =
      st.disposition === 'locked' || st.disposition === 'held' || st.disposition === 'reincarnate';
    const off = this.disabled || locked;
    const text = d ? d.text : this.storedText(f);
    const isLimit = f.control === 'limit-count' || f.control === 'limit-duration';
    const placeholder = d?.cleared
      ? `Cleared — ${this.placeholderText(f)}`
      : this.placeholderText(f);
    return html`
      <div class="field" data-key=${f.key} data-disposition=${st.disposition} data-tier=${st.tier}>
        <label class="label" for="input-${f.configKey}">
          ${st.disposition === 'locked'
            ? html`<sl-icon name="lock" class="label-lock"></sl-icon>`
            : nothing}
          ${f.label}
          ${isTouched(d) ? html`<span class="edited" data-testid="edited">edited</span>` : nothing}
        </label>
        <div class="row">
          <sl-input
            id="input-${f.configKey}"
            size="small"
            .value=${d?.unlimited ? '' : text}
            placeholder=${d?.unlimited ? 'Unlimited' : placeholder}
            inputmode=${f.control === 'limit-count' ? 'numeric' : 'text'}
            ?disabled=${off || !!d?.unlimited}
            @sl-input=${(e: Event) =>
              this.editField(
                f.key,
                { typed: true, cleared: false, text: (e.target as HTMLInputElement).value },
                f
              )}
          ></sl-input>
          ${isLimit
            ? html`<sl-checkbox
                size="small"
                class="unlimited"
                ?checked=${!!d?.unlimited}
                ?disabled=${off}
                @sl-change=${(e: Event) =>
                  this.editField(
                    f.key,
                    { unlimited: (e.target as HTMLInputElement).checked, cleared: false },
                    f
                  )}
                >Unlimited</sl-checkbox
              >`
            : nothing}
          <sl-button
            size="small"
            class="clear"
            ?disabled=${off || !!d?.cleared}
            @click=${() =>
              this.editField(f.key, { cleared: true, typed: false, unlimited: false, text: '' }, f)}
            >Clear</sl-button
          >
        </div>
        ${this.renderStatus(f, st, d)}
      </div>
    `;
  }

  override render() {
    const groups = AGENT_CONFIG_TABS.map((t) => ({
      ...t,
      fields: this.fields.filter((f) => f.tab === t.id),
    })).filter((g) => g.fields.length > 0);
    return html`
      ${this.mode === 'edit' ? this.renderSummary() : nothing}
      ${groups.map(
        (g) => html`
          <section class="group" data-tab=${g.id}>
            <h3>${g.label}</h3>
            ${g.fields.map((f) => this.renderField(f))}
          </section>
        `
      )}
    `;
  }

  static override styles = css`
    :host {
      display: block;
    }
    .summary {
      display: flex;
      flex-wrap: wrap;
      gap: 0.5rem;
      margin-bottom: 1rem;
    }
    .chip {
      font-size: var(--sl-font-size-small);
      padding: 0.125rem 0.625rem;
      border-radius: var(--sl-border-radius-pill);
      border: 1px solid var(--sl-color-neutral-300);
      background: var(--sl-color-neutral-50);
    }
    .chip-now,
    .chip-immediate {
      border-color: var(--sl-color-success-300);
      background: var(--sl-color-success-50);
    }
    .chip-held,
    .chip-reincarnate {
      border-color: var(--sl-color-warning-300);
      background: var(--sl-color-warning-50);
    }
    .group {
      margin-bottom: 1.25rem;
    }
    .group h3 {
      font-size: var(--sl-font-size-medium);
      margin: 0 0 0.75rem;
    }
    .field {
      margin-bottom: 1rem;
    }
    .label {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      font-weight: var(--sl-font-weight-semibold);
      margin-bottom: 0.25rem;
    }
    .edited {
      font-size: var(--sl-font-size-x-small);
      font-weight: normal;
      color: var(--sl-color-primary-600);
    }
    .row {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }
    .row sl-input {
      flex: 1 1 auto;
      max-width: 28rem;
    }
    .status {
      margin-top: 0.25rem;
      font-size: var(--sl-font-size-small);
      color: var(--sl-color-neutral-600);
      display: flex;
      flex-direction: column;
      gap: 0.125rem;
    }
    .status.locked {
      flex-direction: row;
      align-items: center;
      gap: 0.375rem;
    }
    .clear-note {
      color: var(--sl-color-warning-700);
    }
  `;
}

/** Whether an edit of the field is sent now (not held, not locked). */
function editableNow(st: AgentFieldEditState): boolean {
  return st.disposition === 'now' || st.disposition === 'immediate';
}

/** The PATCH value of a touched count limit: null clears, 0 is Unlimited. */
function emitCount(d: FieldDraft): number | null {
  if (d.cleared) return null;
  if (d.unlimited) return 0;
  const text = d.text.trim();
  return text === '' ? null : Number.parseInt(text, 10);
}

/**
 * The PATCH value of a touched string field: null clears. Unlimited on a
 * duration is "0", which the hub parses as no limit (a JSON number would
 * not decode into the string field).
 */
function emitString(control: 'text' | 'limit-duration', d: FieldDraft): string | null {
  if (d.cleared) return null;
  if (d.unlimited && control === 'limit-duration') return '0';
  const text = d.text.trim();
  return text === '' ? null : text;
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-agent-config-form': ScionAgentConfigForm;
  }
}
