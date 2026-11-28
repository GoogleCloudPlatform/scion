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
 * Ordered action-row list editor for Triggers (Stage 2 §9.1, P9 actions[] readiness).
 *
 * Designed as an ordered list of action rows so it works with one action today
 * and scales to 1..8 actions once P9 (ptone/scion#9065) lands on the Hub.
 *
 * P9 HOOK: Set `multiActionEnabled = true` (and send `actions` instead of scalar
 * `action` in `trigger-list.ts`) once the Hub accepts `actions[]`.
 *
 * Exported so source form-authoring panes can render or preview actions.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import {
  formatRedactedExecSummary,
  type ProjectAgentOption,
  type TriggerActionSpec,
} from './types.js';

const MAX_P9_ACTIONS = 8;

export function createDefaultMessageAction(): TriggerActionSpec {
  return {
    type: 'message',
    agentId: '',
    message: '',
    wake: false,
    interrupt: false,
    plain: false,
    offline: 'skip',
    queueTtlSeconds: 86400,
  };
}

export function createDefaultDispatchAction(): TriggerActionSpec {
  return {
    type: 'dispatch_agent',
    template: '',
    agentName: '',
    task: '',
    branch: '',
    labels: {},
    ephemeral: false,
    nameConflict: 'suffix',
    reuseMessage: '',
  };
}

/**
 * Shared read-only display of hub-derived untrusted event variable paths for
 * both message and dispatch_agent actions (ptone/scion#9776, untrusted_optin v1).
 */
@customElement('scion-trigger-untrusted-vars')
export class ScionTriggerUntrustedVars extends LitElement {
  @property({ type: Array })
  vars: string[] = [];

  @property({ type: Number })
  actionIndex = 0;

  static override styles = css`
    :host {
      display: block;
    }

    .untrusted-readonly {
      padding: 0.5rem 0.625rem;
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-sm);
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
    }

    .untrusted-readonly-label {
      font-size: 0.75rem;
      color: var(--scion-text-muted);
    }

    .untrusted-readonly-list {
      display: flex;
      flex-wrap: wrap;
      gap: 0.375rem;
    }

    .untrusted-var-chip {
      font-family: var(--scion-font-mono);
      font-size: 0.75rem;
      padding: 0.125rem 0.375rem;
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-sm);
      color: var(--scion-text);
    }
  `;

  override render() {
    if (!this.vars || this.vars.length === 0) {
      return nothing;
    }
    return html`
      <div class="untrusted-readonly" data-untrusted-vars-display=${this.actionIndex}>
        <div class="untrusted-readonly-label">
          Event values inserted as data, not instructions (derived by hub):
        </div>
        <div class="untrusted-readonly-list">
          ${this.vars.map((v) => html`<code class="untrusted-var-chip">${v}</code>`)}
        </div>
      </div>
    `;
  }
}

@customElement('scion-trigger-action-editor')
export class ScionTriggerActionEditor extends LitElement {
  @property({ type: Array })
  actions: TriggerActionSpec[] = [createDefaultMessageAction()];

  @property({ type: Array })
  agents: ProjectAgentOption[] = [];

  @property({ type: String })
  subjectMode = 'new';

  @property({ type: Boolean })
  readonly = false;

  /**
   * Multi-action support (P9, ptone/scion#9065 / ptone/scion#9785): enables
   * adding/removing/reordering up to 8 ordered actions per trigger.
   */
  @property({ type: Boolean })
  multiActionEnabled = true;

  static override styles = css`
    :host {
      display: block;
    }

    .actions-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 0.5rem;
    }

    .actions-label {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text);
    }

    .action-list {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
    }

    .action-card {
      padding: 0.75rem;
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
    }

    .action-card-top {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .order-badge {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      min-width: 1.5rem;
      height: 1.5rem;
      padding: 0 0.375rem;
      font-size: 0.75rem;
      font-weight: 600;
      font-family: var(--scion-font-mono);
      color: var(--scion-text-muted);
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-sm);
    }

    .row-actions {
      display: flex;
      align-items: center;
      gap: 0.25rem;
      margin-left: auto;
    }

    .field-grid {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 0.75rem;
    }

    .toggles-row {
      display: flex;
      flex-wrap: wrap;
      gap: 1rem;
      align-items: center;
    }

    .help-text {
      font-size: 0.75rem;
      color: var(--scion-text-muted);
    }

    .labels-editor {
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
    }

    .labels-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-text);
    }

    .label-pair {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .label-pair sl-input {
      flex: 1;
    }

    .redacted-box {
      padding: 0.625rem 0.75rem;
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-sm);
      font-family: var(--scion-font-mono);
      font-size: 0.8125rem;
      color: var(--scion-text-muted);
    }
  `;

  private emitActions(next: TriggerActionSpec[]): void {
    this.actions = next;
    this.dispatchEvent(
      new CustomEvent<{ actions: TriggerActionSpec[] }>('actions-changed', {
        detail: { actions: next },
        bubbles: true,
        composed: true,
      })
    );
  }

  private updateAction(index: number, patch: Partial<TriggerActionSpec>): void {
    if (this.readonly) return;
    const next = this.actions.map((a, i) => (i === index ? { ...a, ...patch } : a));
    this.emitActions(next);
  }

  private switchActionType(index: number, nextType: string): void {
    if (this.readonly) return;
    const replacement =
      nextType === 'dispatch_agent' ? createDefaultDispatchAction() : createDefaultMessageAction();
    const next = this.actions.map((a, i) => (i === index ? replacement : a));
    this.emitActions(next);
  }

  private addAction(): void {
    if (this.readonly || !this.multiActionEnabled || this.actions.length >= MAX_P9_ACTIONS) {
      return;
    }
    this.emitActions([...this.actions, createDefaultMessageAction()]);
  }

  private removeAction(index: number): void {
    if (this.readonly || !this.multiActionEnabled || this.actions.length <= 1) return;
    this.emitActions(this.actions.filter((_, i) => i !== index));
  }

  private moveAction(index: number, delta: -1 | 1): void {
    if (this.readonly || !this.multiActionEnabled) return;
    const target = index + delta;
    if (target < 0 || target >= this.actions.length) return;
    const next = [...this.actions];
    const [item] = next.splice(index, 1);
    next.splice(target, 0, item);
    this.emitActions(next);
  }

  private renderMessageFields(action: TriggerActionSpec, index: number) {
    return html`
      <div class="field-grid">
        ${this.agents.length > 0
          ? html`
              <sl-select
                label="Target agent"
                size="small"
                hoist
                clearable
                ?disabled=${this.readonly}
                .value=${action.agentId || ''}
                data-action-agent=${index}
                @sl-change=${(e: Event) => {
                  const val = (e.target as HTMLSelectElement).value;
                  const chosen = this.agents.find((a) => a.id === val);
                  this.updateAction(index, {
                    agentId: val,
                    agentName: chosen?.name ?? '',
                  });
                }}
              >
                ${this.agents.map((ag) => html`<sl-option value=${ag.id}>${ag.name}</sl-option>`)}
              </sl-select>
            `
          : html`
              <sl-input
                label="Target agent ID or name"
                size="small"
                placeholder="agent-id or name"
                ?readonly=${this.readonly}
                .value=${action.agentId || action.agentName || ''}
                data-action-agent=${index}
                @sl-input=${(e: Event) => {
                  const val = (e.target as HTMLInputElement).value.trim();
                  this.updateAction(index, { agentId: val, agentName: '' });
                }}
              ></sl-input>
            `}
        <sl-select
          label="Offline behavior"
          size="small"
          hoist
          ?disabled=${this.readonly}
          .value=${action.offline || 'skip'}
          data-action-offline=${index}
          @sl-change=${(e: Event) => {
            const val = (e.target as HTMLSelectElement).value;
            if (val === 'queue') {
              this.updateAction(index, {
                offline: val,
                queueTtlSeconds: action.queueTtlSeconds || 86400,
              });
            } else {
              const next = this.actions.map((a, i) => {
                if (i !== index) return a;
                const copy = { ...a, offline: val };
                delete copy.queueTtlSeconds;
                return copy;
              });
              this.emitActions(next);
            }
          }}
        >
          <sl-option value="skip">Skip if offline</sl-option>
          <sl-option value="queue">Queue while offline</sl-option>
        </sl-select>
      </div>

      ${action.offline === 'queue'
        ? html`
            <sl-input
              label="Queue TTL (seconds)"
              type="number"
              size="small"
              min="60"
              max="172800"
              help-text="60 to 172800 seconds (default 86400 = 24h, max 48h)"
              ?readonly=${this.readonly}
              .value=${String(action.queueTtlSeconds ?? 86400)}
              data-action-queue-ttl=${index}
              @sl-input=${(e: Event) => {
                const n = parseInt((e.target as HTMLInputElement).value, 10);
                this.updateAction(index, {
                  queueTtlSeconds: Number.isFinite(n) ? n : 86400,
                });
              }}
            ></sl-input>
          `
        : nothing}

      <sl-textarea
        label="Message text"
        size="small"
        rows="3"
        resize="auto"
        placeholder="Webhook received: \${event.type}"
        help-text="Supports \${event.*} placeholders (use \${path?} for optional values). Untrusted placeholders are derived by the hub on save and inserted as data, not instructions."
        ?readonly=${this.readonly}
        .value=${action.message || ''}
        data-action-message=${index}
        @sl-input=${(e: Event) => {
          this.updateAction(index, { message: (e.target as HTMLTextAreaElement).value });
        }}
      ></sl-textarea>

      <scion-trigger-untrusted-vars
        .vars=${action.untrustedVars ?? []}
        .actionIndex=${index}
      ></scion-trigger-untrusted-vars>

      <div class="toggles-row">
        <sl-checkbox
          ?checked=${Boolean(action.wake)}
          ?disabled=${this.readonly}
          data-action-wake=${index}
          @sl-change=${(e: Event) => {
            this.updateAction(index, {
              wake: (e.target as HTMLInputElement).checked,
            });
          }}
          >Wake stopped agent</sl-checkbox
        >
        <sl-checkbox
          ?checked=${Boolean(action.interrupt)}
          ?disabled=${this.readonly}
          data-action-interrupt=${index}
          @sl-change=${(e: Event) => {
            this.updateAction(index, {
              interrupt: (e.target as HTMLInputElement).checked,
            });
          }}
          >Interrupt active turn</sl-checkbox
        >
        <sl-checkbox
          ?checked=${Boolean(action.plain)}
          ?disabled=${this.readonly}
          data-action-plain=${index}
          @sl-change=${(e: Event) => {
            this.updateAction(index, {
              plain: (e.target as HTMLInputElement).checked,
            });
          }}
          >Plain text (no framing)</sl-checkbox
        >
      </div>
    `;
  }

  private renderDispatchFields(action: TriggerActionSpec, index: number) {
    const labelEntries = Object.entries(action.labels ?? {});

    const updateLabelEntry = (oldKey: string, newKey: string, newVal: string) => {
      const next: Record<string, string> = {};
      for (const [k, v] of labelEntries) {
        if (k === oldKey) {
          if (newKey) next[newKey] = newVal;
        } else {
          next[k] = v;
        }
      }
      this.updateAction(index, { labels: next });
    };

    const addLabel = () => {
      const next = { ...(action.labels ?? {}) };
      let candidate = 'label';
      let n = 1;
      while (Object.prototype.hasOwnProperty.call(next, candidate)) {
        candidate = `label_${n++}`;
      }
      next[candidate] = '';
      this.updateAction(index, { labels: next });
    };

    const removeLabel = (key: string) => {
      const next = { ...(action.labels ?? {}) };
      delete next[key];
      this.updateAction(index, { labels: next });
    };

    return html`
      <div class="field-grid">
        <sl-input
          label="Template"
          size="small"
          placeholder="developer"
          required
          ?readonly=${this.readonly}
          .value=${action.template || ''}
          data-action-template=${index}
          @sl-input=${(e: Event) => {
            this.updateAction(index, { template: (e.target as HTMLInputElement).value });
          }}
        ></sl-input>
        <sl-input
          label="Agent name template"
          size="small"
          placeholder="fix-\${event.attributes.number}"
          ?readonly=${this.readonly}
          .value=${action.agentName || ''}
          data-action-agent-name=${index}
          @sl-input=${(e: Event) => {
            this.updateAction(index, { agentName: (e.target as HTMLInputElement).value });
          }}
        ></sl-input>
      </div>

      <sl-textarea
        label="Task prompt"
        size="small"
        rows="3"
        resize="auto"
        placeholder="Investigate event \${event.id}"
        help-text="Untrusted placeholders are derived by the hub on save and inserted as data, not instructions."
        ?readonly=${this.readonly}
        .value=${action.task || ''}
        data-action-task=${index}
        @sl-input=${(e: Event) => {
          this.updateAction(index, { task: (e.target as HTMLTextAreaElement).value });
        }}
      ></sl-textarea>

      <div class="field-grid">
        <sl-input
          label="Branch template"
          size="small"
          placeholder="main"
          ?readonly=${this.readonly}
          .value=${action.branch || ''}
          data-action-branch=${index}
          @sl-input=${(e: Event) => {
            this.updateAction(index, { branch: (e.target as HTMLInputElement).value });
          }}
        ></sl-input>
        <sl-select
          label="Name conflict"
          size="small"
          hoist
          ?disabled=${this.readonly}
          .value=${action.nameConflict || 'suffix'}
          data-action-name-conflict=${index}
          @sl-change=${(e: Event) => {
            this.updateAction(index, {
              nameConflict: (e.target as HTMLSelectElement).value,
            });
          }}
        >
          <sl-option value="suffix">Append random suffix</sl-option>
          <sl-option value="skip">Skip fire on conflict</sl-option>
        </sl-select>
      </div>

      ${this.subjectMode === 'reuse'
        ? html`
            <sl-textarea
              label="Reuse message"
              size="small"
              rows="2"
              placeholder="Follow-up event received for \${event.subject}"
              ?readonly=${this.readonly}
              .value=${action.reuseMessage || ''}
              data-action-reuse-message=${index}
              @sl-input=${(e: Event) => {
                this.updateAction(index, {
                  reuseMessage: (e.target as HTMLTextAreaElement).value,
                });
              }}
            ></sl-textarea>
          `
        : nothing}

      <scion-trigger-untrusted-vars
        .vars=${action.untrustedVars ?? []}
        .actionIndex=${index}
      ></scion-trigger-untrusted-vars>

      <div class="labels-editor">
        <div class="labels-header">
          <span>Labels</span>
          ${this.readonly
            ? nothing
            : html`
                <sl-button size="small" variant="text" @click=${() => addLabel()}>
                  <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                  Add label
                </sl-button>
              `}
        </div>
        ${labelEntries.map(
          ([k, v]) => html`
            <div class="label-pair">
              <sl-input
                size="small"
                placeholder="key"
                ?readonly=${this.readonly}
                .value=${k}
                @sl-change=${(e: Event) =>
                  updateLabelEntry(k, (e.target as HTMLInputElement).value.trim(), v)}
              ></sl-input>
              <sl-input
                size="small"
                placeholder="value"
                ?readonly=${this.readonly}
                .value=${v}
                @sl-change=${(e: Event) =>
                  updateLabelEntry(k, k, (e.target as HTMLInputElement).value)}
              ></sl-input>
              ${this.readonly
                ? nothing
                : html`
                    <sl-icon-button
                      name="trash"
                      label="Remove label"
                      @click=${() => removeLabel(k)}
                    ></sl-icon-button>
                  `}
            </div>
          `
        )}
      </div>

      <div class="toggles-row">
        <sl-checkbox
          ?checked=${Boolean(action.ephemeral)}
          ?disabled=${this.readonly}
          data-action-ephemeral=${index}
          @sl-change=${(e: Event) => {
            this.updateAction(index, {
              ephemeral: (e.target as HTMLInputElement).checked,
            });
          }}
          >Ephemeral agent (when supported by hub)</sl-checkbox
        >
      </div>
    `;
  }

  private renderExecReadOnly(action: TriggerActionSpec, index: number) {
    // Never render unredacted command argv in the web UI (Stage 2 P8).
    return html`
      <div class="redacted-box" data-action-exec-redacted=${index}>
        ${formatRedactedExecSummary(action)}
        ${action.target || action.agentName || action.agentId
          ? html` — target: ${action.agentName || action.agentId || action.target}`
          : nothing}
        ${action.timeoutSeconds ? html` — timeout: ${action.timeoutSeconds}s` : nothing}
      </div>
      <div class="help-text">
        Exec command arguments are redacted in the web UI and must be managed via CLI.
      </div>
    `;
  }

  private renderReadOnlyOtherAction(action: TriggerActionSpec, index: number) {
    return html`
      <div
        class="redacted-box"
        data-action-readonly=${index}
        data-action-readonly-type=${action.type}
      >
        <strong>${action.type}</strong>
        ${action.target ? html` — target: ${action.target}` : nothing}
        ${action.replyInThread ? html` (reply in thread)` : nothing}
        ${action.body ? html`<div style="margin-top: 0.375rem;">${action.body}</div>` : nothing}
      </div>
      <div class="help-text">
        This action type is displayed read-only here and preserved unchanged on edit.
      </div>
    `;
  }

  override render() {
    const rows = this.actions.length > 0 ? this.actions : [createDefaultMessageAction()];
    return html`
      <div class="actions-header">
        <span class="actions-label">${this.multiActionEnabled ? 'Actions' : 'Action'}</span>
        ${!this.readonly && this.multiActionEnabled && rows.length < MAX_P9_ACTIONS
          ? html`
              <sl-button
                size="small"
                variant="default"
                data-add-action-row
                @click=${() => this.addAction()}
              >
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                Add action
              </sl-button>
            `
          : nothing}
      </div>
      <div class="action-list">
        ${rows.map((action, index) => {
          const isEditableType = action.type === 'message' || action.type === 'dispatch_agent';
          return html`
            <div class="action-card" data-action-row=${index}>
              <div class="action-card-top">
                ${this.multiActionEnabled || rows.length > 1
                  ? html`<span class="order-badge">${index + 1}</span>`
                  : nothing}
                ${!isEditableType
                  ? html`<sl-badge variant="neutral">${action.type}</sl-badge>`
                  : html`
                      <sl-radio-group
                        label="Action type"
                        size="small"
                        .value=${action.type || 'message'}
                        data-action-type=${index}
                        @sl-change=${(e: Event) => {
                          const val = (e.target as HTMLInputElement).value;
                          this.switchActionType(index, val);
                        }}
                      >
                        <sl-radio-button value="message" ?disabled=${this.readonly}
                          >Message</sl-radio-button
                        >
                        <sl-radio-button value="dispatch_agent" ?disabled=${this.readonly}
                          >Dispatch agent</sl-radio-button
                        >
                      </sl-radio-group>
                    `}
                ${!this.readonly && this.multiActionEnabled && rows.length > 1
                  ? html`
                      <div class="row-actions">
                        <sl-icon-button
                          name="arrow-up"
                          label="Move action up"
                          data-move-action-up=${index}
                          ?disabled=${index === 0}
                          @click=${() => this.moveAction(index, -1)}
                        ></sl-icon-button>
                        <sl-icon-button
                          name="arrow-down"
                          label="Move action down"
                          data-move-action-down=${index}
                          ?disabled=${index === rows.length - 1}
                          @click=${() => this.moveAction(index, 1)}
                        ></sl-icon-button>
                        <sl-icon-button
                          name="trash"
                          label="Remove action"
                          data-remove-action=${index}
                          @click=${() => this.removeAction(index)}
                        ></sl-icon-button>
                      </div>
                    `
                  : nothing}
              </div>

              ${action.type === 'dispatch_agent'
                ? this.renderDispatchFields(action, index)
                : action.type === 'exec'
                  ? this.renderExecReadOnly(action, index)
                  : action.type === 'message'
                    ? this.renderMessageFields(action, index)
                    : this.renderReadOnlyOtherAction(action, index)}
            </div>
          `;
        })}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-trigger-action-editor': ScionTriggerActionEditor;
  }
}
