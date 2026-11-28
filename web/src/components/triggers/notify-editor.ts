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
 * Notify-on-failure editor for Trigger Rules (Stage 2 §10, ptone/scion#8065).
 *
 * Allows configuring:
 * - user notice toggle (default on),
 * - optional clearable target agent sink (checked against author permissions on save),
 * - outcome checkboxes (failed, refused, expired pre-checked by default; skipped opt-in).
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import { splitAgentRef, type ProjectAgentOption, type TriggerNotifySpec } from './types.js';

export const ALL_NOTIFY_OUTCOMES = ['failed', 'refused', 'expired', 'skipped'] as const;
export const DEFAULT_NOTIFY_OUTCOMES = ['failed', 'refused', 'expired'] as const;

export type NotifyOutcome = (typeof ALL_NOTIFY_OUTCOMES)[number];

export interface NormalizedTriggerNotifySpec {
  onOutcomes: string[];
  userNotice: boolean;
  agentId: string;
  agentName: string;
}

const OUTCOME_LABELS: Record<NotifyOutcome, string> = {
  failed: 'Failed',
  refused: 'Refused',
  expired: 'Expired',
  skipped: 'Skipped',
};

/**
 * Returns the default notify policy matching Hub defaults (Stage 2 §10).
 */
export function createDefaultNotifySpec(): NormalizedTriggerNotifySpec {
  return {
    onOutcomes: [...DEFAULT_NOTIFY_OUTCOMES],
    userNotice: true,
    agentId: '',
    agentName: '',
  };
}

/**
 * Normalises a stored (or absent) notify policy into canonical form for editing
 * and equality comparison against the Hub's defaults.
 */
export function normalizeNotifySpec(spec?: TriggerNotifySpec | null): NormalizedTriggerNotifySpec {
  const rawOutcomes =
    spec?.onOutcomes && spec.onOutcomes.length > 0 ? spec.onOutcomes : [...DEFAULT_NOTIFY_OUTCOMES];
  const seen = new Set(rawOutcomes);
  const onOutcomes = ALL_NOTIFY_OUTCOMES.filter((o) => seen.has(o));
  return {
    onOutcomes: onOutcomes.length > 0 ? onOutcomes : [...DEFAULT_NOTIFY_OUTCOMES],
    userNotice: spec?.userNotice !== undefined ? spec.userNotice : true,
    agentId: (spec?.agentId ?? '').trim(),
    agentName: (spec?.agentName ?? '').trim(),
  };
}

@customElement('scion-trigger-notify-editor')
export class ScionTriggerNotifyEditor extends LitElement {
  @property({ type: Object })
  notify: TriggerNotifySpec = createDefaultNotifySpec();

  @property({ type: Array })
  agents: ProjectAgentOption[] = [];

  @property({ type: Boolean })
  readonly = false;

  static override styles = css`
    :host {
      display: block;
    }

    .notify-card {
      padding: 0.75rem;
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
    }

    .notify-row {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
    }

    .outcomes-fieldset {
      border: 0;
      margin: 0;
      padding: 0;
      min-inline-size: 0;
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
    }

    .outcomes-label {
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-text);
      padding: 0;
      margin-bottom: 0.25rem;
    }

    .outcomes-row {
      display: flex;
      flex-wrap: wrap;
      gap: 0.75rem 1.25rem;
      align-items: center;
    }

    .help-text {
      font-size: 0.75rem;
      color: var(--scion-text-muted);
    }
  `;

  private emitNotify(next: TriggerNotifySpec): void {
    this.notify = next;
    this.dispatchEvent(
      new CustomEvent<{ notify: TriggerNotifySpec }>('notify-changed', {
        detail: { notify: next },
        bubbles: true,
        composed: true,
      })
    );
  }

  private updateNotify(patch: Partial<TriggerNotifySpec>): void {
    if (this.readonly) return;
    this.emitNotify({ ...this.notify, ...patch });
  }

  private toggleOutcome(outcome: NotifyOutcome, checked: boolean): void {
    if (this.readonly) return;
    const current = new Set(this.notify.onOutcomes ?? []);
    if (checked) {
      current.add(outcome);
    } else {
      current.delete(outcome);
    }
    const ordered = ALL_NOTIFY_OUTCOMES.filter((o) => current.has(o));
    this.updateNotify({ onOutcomes: ordered });
  }

  override render() {
    const userNotice = this.notify.userNotice !== false;
    const outcomes = new Set(this.notify.onOutcomes ?? []);
    const selectedAgentId = (this.notify.agentId ?? '').trim();
    const selectedAgentName = (this.notify.agentName ?? '').trim();
    const hasMissingSelectedAgent =
      this.agents.length > 0 &&
      selectedAgentId !== '' &&
      !this.agents.some((ag) => ag.id === selectedAgentId);

    return html`
      <div class="notify-card" data-notify-editor>
        <div class="notify-row">
          <sl-switch
            size="small"
            ?checked=${userNotice}
            ?disabled=${this.readonly}
            data-notify-user-notice
            @sl-change=${(e: Event) => {
              const checked = (e.target as HTMLInputElement).checked;
              this.updateNotify({ userNotice: checked });
            }}
          >
            Notify rule creator (user notice)
          </sl-switch>
          <div class="help-text">
            Sends a notification to the rule author (or delegating user). Rate-limited to one notice
            per hour for each outcome and reason.
          </div>
        </div>

        <div class="notify-row">
          ${this.agents.length > 0
            ? html`
                <sl-select
                  label="Notify agent (optional)"
                  size="small"
                  hoist
                  clearable
                  placeholder="None"
                  ?disabled=${this.readonly}
                  .value=${selectedAgentId}
                  data-notify-agent-select
                  @sl-change=${(e: Event) => {
                    const val = ((e.target as HTMLSelectElement).value ?? '').trim();
                    const chosen = this.agents.find((ag) => ag.id === val);
                    this.updateNotify({
                      agentId: val,
                      agentName: chosen?.name ?? (val === selectedAgentId ? selectedAgentName : ''),
                    });
                  }}
                  @sl-clear=${() => {
                    this.updateNotify({ agentId: '', agentName: '' });
                  }}
                >
                  ${hasMissingSelectedAgent
                    ? html`
                        <sl-option value=${selectedAgentId}>
                          ${selectedAgentName || selectedAgentId}
                        </sl-option>
                      `
                    : nothing}
                  ${this.agents.map((ag) => html`<sl-option value=${ag.id}>${ag.name}</sl-option>`)}
                </sl-select>
              `
            : html`
                <sl-input
                  label="Notify agent ID or name (optional)"
                  size="small"
                  clearable
                  placeholder="agent-id or name"
                  ?readonly=${this.readonly}
                  .value=${selectedAgentId || selectedAgentName}
                  data-notify-agent-input
                  @sl-input=${(e: Event) => {
                    const val = (e.target as HTMLInputElement).value;
                    this.updateNotify(splitAgentRef(val));
                  }}
                  @sl-clear=${() => {
                    this.updateNotify({ agentId: '', agentName: '' });
                  }}
                ></sl-input>
              `}
          <div class="help-text">
            Optionally delivers a failure notice message to an agent you are allowed to message.
          </div>
        </div>

        <fieldset class="outcomes-fieldset" data-notify-outcomes-group>
          <legend class="outcomes-label">Notify on outcomes</legend>
          <div class="outcomes-row">
            ${ALL_NOTIFY_OUTCOMES.map(
              (outcome) => html`
                <sl-checkbox
                  size="small"
                  ?checked=${outcomes.has(outcome)}
                  ?disabled=${this.readonly}
                  data-notify-outcome=${outcome}
                  @sl-change=${(e: Event) => {
                    const checked = (e.target as HTMLInputElement).checked;
                    this.toggleOutcome(outcome, checked);
                  }}
                >
                  ${OUTCOME_LABELS[outcome]}
                </sl-checkbox>
              `
            )}
          </div>
          ${outcomes.size === 0
            ? html`
                <div class="help-text" data-notify-outcomes-default-hint>
                  No outcomes selected — hub defaults (failed, refused, expired) will apply on save.
                </div>
              `
            : nothing}
        </fieldset>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-trigger-notify-editor': ScionTriggerNotifyEditor;
  }
}
