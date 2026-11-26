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
 * Ordered `where` match-rules list editor for Triggers (Stage 2 §9.1,
 * mixed-rule-arrays §2–§4).
 *
 * Exported so source form-authoring panes can render compiled rules read-only.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import {
  ADMITTED_TRIGGER_DIALECTS,
  type TriggerDialectOption,
  type TriggerMatchRuleSpec,
  type TriggerRuleRowError,
} from './types.js';

@customElement('scion-trigger-where-editor')
export class ScionTriggerWhereEditor extends LitElement {
  @property({ type: Array })
  rules: TriggerMatchRuleSpec[] = [];

  @property({ type: Array })
  admittedDialects: readonly TriggerDialectOption[] = ADMITTED_TRIGGER_DIALECTS;

  /** Per-row (0-based) compile errors parsed from 400 invalid_rule responses. */
  @property({ type: Object })
  rowErrors: Record<number, TriggerRuleRowError> = {};

  @property({ type: Boolean })
  readonly = false;

  static override styles = css`
    :host {
      display: block;
    }

    .where-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 0.5rem;
      gap: 0.5rem;
    }

    .where-label {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text);
    }

    .where-hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted);
      margin-bottom: 0.5rem;
    }

    .empty-note {
      font-size: 0.8125rem;
      color: var(--scion-text-muted);
      padding: 0.75rem;
      background: var(--scion-bg-subtle);
      border: 1px dashed var(--scion-border);
      border-radius: var(--scion-radius);
    }

    .rule-list {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
    }

    .rule-row {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
      padding: 0.75rem;
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
    }

    .rule-row.has-error {
      border-color: var(--sl-color-danger-500);
    }

    .rule-row-top {
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

    .dialect-select {
      width: 9rem;
      flex-shrink: 0;
    }

    .row-actions {
      display: flex;
      align-items: center;
      gap: 0.25rem;
      margin-left: auto;
    }

    .expr-input {
      width: 100%;
    }

    .expr-input::part(textarea) {
      font-family: var(--scion-font-mono);
      font-size: 0.8125rem;
    }

    .row-error {
      display: flex;
      align-items: flex-start;
      gap: 0.375rem;
      font-size: 0.75rem;
      color: var(--sl-color-danger-700);
      background: var(--sl-color-danger-50);
      border: 1px solid var(--sl-color-danger-200);
      border-radius: var(--scion-radius-sm);
      padding: 0.375rem 0.5rem;
      white-space: pre-wrap;
      word-break: break-word;
    }

    .row-error-pos {
      font-family: var(--scion-font-mono);
      font-weight: 600;
      flex-shrink: 0;
    }
  `;

  private emitRules(next: TriggerMatchRuleSpec[]): void {
    this.rules = next;
    this.dispatchEvent(
      new CustomEvent<{ rules: TriggerMatchRuleSpec[] }>('rules-changed', {
        detail: { rules: next },
        bubbles: true,
        composed: true,
      })
    );
  }

  private addRule(): void {
    if (this.readonly) return;
    const defaultDialect = this.admittedDialects[0]?.value ?? 'cel';
    this.emitRules([...this.rules, { dialect: defaultDialect, expr: '' }]);
  }

  private removeRule(index: number): void {
    if (this.readonly) return;
    const next = this.rules.filter((_, i) => i !== index);
    this.emitRules(next);
  }

  private moveRule(index: number, delta: -1 | 1): void {
    if (this.readonly) return;
    const target = index + delta;
    if (target < 0 || target >= this.rules.length) return;
    const next = [...this.rules];
    const [item] = next.splice(index, 1);
    next.splice(target, 0, item);
    this.emitRules(next);
  }

  private updateDialect(index: number, dialect: string): void {
    if (this.readonly) return;
    const next = this.rules.map((r, i) => (i === index ? { ...r, dialect } : r));
    this.emitRules(next);
  }

  private updateExpr(index: number, expr: string): void {
    if (this.readonly) return;
    const next = this.rules.map((r, i) => (i === index ? { ...r, expr } : r));
    this.emitRules(next);
  }

  override render() {
    return html`
      <div class="where-header">
        <span class="where-label">Match rules (where)</span>
        ${this.readonly
          ? nothing
          : html`
              <sl-button
                size="small"
                variant="default"
                data-add-where-rule
                @click=${() => this.addRule()}
              >
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                Add rule
              </sl-button>
            `}
      </div>
      <div class="where-hint">
        Evaluated in order (AND with short-circuit). Leave empty for a Types-only rule.
      </div>
      ${this.rules.length === 0
        ? html`
            <div class="empty-note" data-where-empty>
              No match rules — matches all selected event types (Types-only rule).
            </div>
          `
        : html`
            <div class="rule-list">
              ${this.rules.map((rule, index) => {
                const err = this.rowErrors[index];
                return html`
                  <div class="rule-row ${err ? 'has-error' : ''}" data-where-row=${index}>
                    <div class="rule-row-top">
                      <span class="order-badge" title="Evaluation order">${index + 1}</span>
                      <sl-select
                        class="dialect-select"
                        size="small"
                        label="Dialect"
                        hoist
                        ?disabled=${this.readonly}
                        .value=${rule.dialect || this.admittedDialects[0]?.value || 'cel'}
                        data-where-dialect=${index}
                        @sl-change=${(e: Event) => {
                          const sel = e.target as HTMLSelectElement;
                          this.updateDialect(index, sel.value);
                        }}
                      >
                        ${this.admittedDialects.map(
                          (d) => html`<sl-option value=${d.value}>${d.label}</sl-option>`
                        )}
                      </sl-select>
                      ${this.readonly
                        ? nothing
                        : html`
                            <div class="row-actions">
                              <sl-icon-button
                                name="arrow-up"
                                label="Move rule up"
                                ?disabled=${index === 0}
                                data-where-up=${index}
                                @click=${() => this.moveRule(index, -1)}
                              ></sl-icon-button>
                              <sl-icon-button
                                name="arrow-down"
                                label="Move rule down"
                                ?disabled=${index === this.rules.length - 1}
                                data-where-down=${index}
                                @click=${() => this.moveRule(index, 1)}
                              ></sl-icon-button>
                              <sl-icon-button
                                name="trash"
                                label="Remove rule"
                                data-where-remove=${index}
                                @click=${() => this.removeRule(index)}
                              ></sl-icon-button>
                            </div>
                          `}
                    </div>
                    <sl-textarea
                      class="expr-input"
                      size="small"
                      rows="2"
                      resize="auto"
                      placeholder="event.attributes.env == 'prod'"
                      ?readonly=${this.readonly}
                      .value=${rule.expr}
                      data-where-expr=${index}
                      @sl-input=${(e: Event) => {
                        const ta = e.target as HTMLTextAreaElement;
                        this.updateExpr(index, ta.value);
                      }}
                    ></sl-textarea>
                    ${err
                      ? html`
                          <div class="row-error" role="alert" data-where-error=${index}>
                            ${err.line !== undefined
                              ? html`<span class="row-error-pos"
                                  >${err.line}${err.column !== undefined
                                    ? `:${err.column}`
                                    : ''}</span
                                >`
                              : nothing}
                            <span>${err.category ? `[${err.category}] ` : ''}${err.message}</span>
                          </div>
                        `
                      : nothing}
                  </div>
                `;
              })}
            </div>
          `}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-trigger-where-editor': ScionTriggerWhereEditor;
  }
}
