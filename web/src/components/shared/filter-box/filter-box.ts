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

import { LitElement, css, html, nothing } from 'lit';
import { customElement, property, query, state } from 'lit/decorators.js';
import {
  getSuggestions,
  getViewSchema,
  knownKeysFor,
  parse,
  serverSubset,
  validate,
  type FilterError,
  type FilterWarning,
  type Query,
  type Suggestion,
  type ValueSuggestionProvider,
  type ViewSchema,
} from '../../../shared/filter-query/index.js';
import { renderFilterHelp } from './filter-help.js';

export interface FilterInputDetail {
  value: string;
  query: Query;
  valid: boolean;
}

export interface FilterCommitDetail {
  value: string;
  query: Query;
  serverQ: string;
}

const COMMIT_DEBOUNCE_MS = 300;
const EMPTY_QUERY: Query = { mode: 'text', text: '' };

@customElement('scion-filter-box')
export class ScionFilterBox extends LitElement {
  @property({ type: String })
  view = 'agents';

  @property({ type: String })
  value = '';

  @property({ type: String })
  placeholder = 'Filter\u2026 (try phase:running)';

  @property({ attribute: false })
  valueSuggestions?: ValueSuggestionProvider;

  @state()
  private suggestions: Suggestion[] = [];

  @state()
  private open = false;

  @state()
  private activeIndex = -1;

  @state()
  private helpOpen = false;

  @state()
  private error: FilterError | null = null;

  @state()
  private warning: FilterWarning | null = null;

  @state()
  private suppressExpectedValueError = false;

  @query('input')
  private inputEl?: HTMLInputElement;

  private lastValidQuery: Query = EMPTY_QUERY;
  private lastEmittedValue = '';
  private commitTimer: ReturnType<typeof setTimeout> | null = null;
  private readonly listboxId = `filter-listbox-${Math.random().toString(36).slice(2, 8)}`;

  static override styles = css`
    :host {
      display: block;
      position: relative;
      min-width: 240px;
      flex: 1 1 280px;
      max-width: 480px;
    }

    .input-shell {
      display: flex;
      align-items: center;
      gap: 0.25rem;
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      padding: 0 0.375rem 0 0.5rem;
      min-height: 2rem;
      transition: border-color 120ms ease;
    }

    .input-shell:focus-within {
      border-color: var(--scion-primary);
    }

    .input-shell.has-error {
      border-color: var(--scion-danger-500);
    }

    .prefix-icon {
      color: var(--scion-text-muted);
      font-size: 0.875rem;
      flex-shrink: 0;
    }

    input {
      flex: 1 1 auto;
      min-width: 0;
      border: none;
      outline: none;
      background: transparent;
      color: var(--scion-text);
      font: inherit;
      font-size: 0.8125rem;
      padding: 0.25rem 0;
    }

    input::placeholder {
      color: var(--scion-text-muted);
    }

    .clear-btn {
      appearance: none;
      border: none;
      background: transparent;
      color: var(--scion-text-muted);
      cursor: pointer;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      padding: 0.125rem;
      border-radius: var(--scion-radius-sm, var(--scion-radius));
    }

    .clear-btn:hover {
      color: var(--scion-text);
    }

    .help-btn::part(base) {
      padding: 0.125rem;
      font-size: 0.875rem;
      color: var(--scion-text-muted);
    }

    .listbox {
      position: absolute;
      top: calc(100% + 2px);
      left: 0;
      right: 0;
      z-index: 50;
      background: var(--scion-surface-raised, var(--scion-surface));
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      box-shadow: var(--scion-shadow-md, 0 4px 12px var(--scion-border));
      max-height: 240px;
      overflow-y: auto;
      padding: 0.25rem;
      margin: 0;
      list-style: none;
    }

    .option {
      display: flex;
      align-items: baseline;
      justify-content: space-between;
      gap: 0.75rem;
      padding: 0.375rem 0.5rem;
      border-radius: var(--scion-radius-sm, var(--scion-radius));
      cursor: pointer;
      font-size: 0.8125rem;
      color: var(--scion-text);
    }

    .option:hover,
    .option.active {
      background: var(--scion-primary-tint);
    }

    .option-label {
      font-family: var(--scion-font-mono);
      font-weight: 500;
    }

    .option-help {
      color: var(--scion-text-muted);
      font-size: 0.75rem;
      text-align: right;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .feedback {
      font-size: 0.75rem;
      margin-top: 0.25rem;
      min-height: 0;
      line-height: 1.3;
      display: flex;
      flex-wrap: wrap;
      align-items: baseline;
      gap: 0.375rem;
    }

    .feedback.error {
      color: var(--scion-danger-600);
    }

    .feedback.warning {
      color: var(--scion-warning-700);
    }

    .feedback-excerpt {
      font-family: var(--scion-font-mono);
      font-size: 0.6875rem;
      color: var(--scion-text-secondary);
      background: var(--scion-bg-subtle);
      padding: 0.0625rem 0.25rem;
      border-radius: var(--scion-radius-sm, var(--scion-radius));
      white-space: pre;
    }

    .error-span {
      background: var(--scion-danger-100);
      color: var(--scion-danger-800);
      border-radius: var(--scion-radius-sm, var(--scion-radius));
      padding: 0 0.125rem;
      text-decoration: underline wavy var(--scion-danger-500);
    }

    .help-popover {
      background: var(--scion-surface-raised, var(--scion-surface));
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      box-shadow: var(--scion-shadow-md, 0 4px 12px var(--scion-border));
      padding: 0.75rem;
      width: 380px;
      max-width: 90vw;
      font-size: 0.75rem;
      color: var(--scion-text);
    }

    .help-table-wrap {
      max-height: 280px;
      overflow-y: auto;
    }

    .help-exec {
      display: inline-block;
      margin-left: 0.25rem;
      font-size: 0.625rem;
      padding: 0 0.25rem;
      border-radius: var(--scion-radius-sm, var(--scion-radius));
      background: var(--scion-bg-subtle);
      color: var(--scion-text-muted);
      vertical-align: middle;
    }

    .help-title {
      font-weight: 600;
      margin-bottom: 0.375rem;
    }

    .help-cheat,
    .help-text {
      color: var(--scion-text-secondary);
      margin-bottom: 0.5rem;
      line-height: 1.4;
    }

    .help-popover code {
      font-family: var(--scion-font-mono);
      background: var(--scion-bg-subtle);
      padding: 0.0625rem 0.25rem;
      border-radius: var(--scion-radius-sm, var(--scion-radius));
    }

    .help-table {
      width: 100%;
      border-collapse: collapse;
    }

    .help-table th,
    .help-table td {
      text-align: left;
      vertical-align: top;
      padding: 0.25rem 0.375rem;
      border-top: 1px solid var(--scion-border);
    }

    .help-values {
      color: var(--scion-text-muted);
      font-family: var(--scion-font-mono);
      font-size: 0.6875rem;
      margin-top: 0.125rem;
    }
  `;

  private get schema(): ViewSchema {
    return (
      getViewSchema(this.view) ?? {
        view: this.view,
        keys: [],
        textHelp: '',
      }
    );
  }

  private boundGlobalKeydown = (e: KeyboardEvent): void => {
    if (e.key !== '/' || e.metaKey || e.ctrlKey || e.altKey) return;
    const path = e.composedPath();
    const target = path[0] as HTMLElement | undefined;
    if (
      target &&
      (target.tagName === 'INPUT' ||
        target.tagName === 'TEXTAREA' ||
        target.tagName === 'SELECT' ||
        target.isContentEditable)
    ) {
      return;
    }
    e.preventDefault();
    this.focusInput();
  };

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener('keydown', this.boundGlobalKeydown);
    this.syncExternalValue();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener('keydown', this.boundGlobalKeydown);
    if (this.commitTimer !== null) {
      clearTimeout(this.commitTimer);
      this.commitTimer = null;
    }
  }

  override willUpdate(changed: Map<string, unknown>): void {
    if (changed.has('value') && this.value !== this.lastEmittedValue) {
      if (this.commitTimer !== null) {
        clearTimeout(this.commitTimer);
        this.commitTimer = null;
      }
      this.lastEmittedValue = this.value;
    }
    if (changed.has('value') || changed.has('view')) {
      this.syncExternalValue();
    }
  }

  focusInput(): void {
    this.inputEl?.focus();
    this.refreshSuggestions();
    this.open = this.suggestions.length > 0;
  }

  private syncExternalValue(): void {
    const schema = this.schema;
    const parsed = parse(this.value, knownKeysFor(schema));
    if (parsed.error || !parsed.query) {
      this.error = parsed.error ?? null;
      this.warning = null;
      return;
    }
    const validation = validate(parsed.query, schema, this.value);
    this.error = validation.error ?? null;
    this.warning = validation.warnings[0] ?? null;
    if (!validation.error) {
      this.lastValidQuery = parsed.query;
    }
  }

  private get visibleError(): FilterError | null {
    if (!this.error) return null;
    if (
      this.suppressExpectedValueError &&
      this.open &&
      this.error.code === 'syntax' &&
      this.error.message.startsWith('expected value after ')
    ) {
      return null;
    }
    return this.error;
  }

  private renderErrorSpan(err: FilterError) {
    if (err.length <= 0 || err.offset < 0 || err.offset >= this.value.length) {
      return nothing;
    }
    const end = Math.min(this.value.length, err.offset + err.length);
    const contextRadius = 16;
    const snipStart = Math.max(0, err.offset - contextRadius);
    const snipEnd = Math.min(this.value.length, end + contextRadius);
    const before = this.value.slice(snipStart, err.offset);
    const bad = this.value.slice(err.offset, end);
    const after = this.value.slice(end, snipEnd);
    return html`<code class="feedback-excerpt"
      >${snipStart > 0 ? '\u2026' : ''}${before}<mark class="error-span">${bad}</mark
      >${after}${snipEnd < this.value.length ? '\u2026' : ''}</code
    >`;
  }

  private evaluateAndEmit(raw: string, immediateCommit: boolean): void {
    this.lastEmittedValue = raw;
    const schema = this.schema;
    const parsed = parse(raw, knownKeysFor(schema));

    let valid = false;
    let activeQuery = this.lastValidQuery;

    if (parsed.error || !parsed.query) {
      this.error = parsed.error ?? null;
      this.warning = null;
    } else {
      const validation = validate(parsed.query, schema, raw);
      this.error = validation.error ?? null;
      this.warning = validation.warnings[0] ?? null;
      if (!validation.error) {
        valid = true;
        this.lastValidQuery = parsed.query;
        activeQuery = parsed.query;
      }
    }

    this.dispatchEvent(
      new CustomEvent<FilterInputDetail>('filter-input', {
        detail: { value: raw, query: activeQuery, valid },
        bubbles: true,
        composed: true,
      })
    );

    // An invalid edit within the debounce window must NOT cancel a pending
    // commit of the last valid query.
    if (!valid) {
      return;
    }

    if (this.commitTimer !== null) {
      clearTimeout(this.commitTimer);
      this.commitTimer = null;
    }

    const emitCommit = (): void => {
      this.commitTimer = null;
      this.dispatchEvent(
        new CustomEvent<FilterCommitDetail>('filter-commit', {
          detail: {
            value: raw,
            query: activeQuery,
            serverQ: serverSubset(activeQuery, schema),
          },
          bubbles: true,
          composed: true,
        })
      );
    };

    if (immediateCommit) {
      emitCommit();
    } else {
      this.commitTimer = setTimeout(emitCommit, COMMIT_DEBOUNCE_MS);
    }
  }

  private refreshSuggestions(): void {
    const cursor = this.inputEl?.selectionStart ?? this.value.length;
    this.suggestions = getSuggestions(this.value, cursor, this.schema, this.valueSuggestions);
    this.activeIndex = -1;
  }

  private onInput(e: Event): void {
    const target = e.target as HTMLInputElement;
    this.suppressExpectedValueError = false;
    this.value = target.value;
    this.refreshSuggestions();
    this.open = this.suggestions.length > 0;
    this.evaluateAndEmit(this.value, false);
  }

  private onFocus(): void {
    this.refreshSuggestions();
    this.open = this.suggestions.length > 0;
  }

  private onBlur(): void {
    // Allow mousedown on option to fire before closing.
    setTimeout(() => {
      this.open = false;
      this.suppressExpectedValueError = false;
    }, 120);
  }

  private onKeydown(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      if (this.open) {
        e.preventDefault();
        this.open = false;
        this.suppressExpectedValueError = false;
      } else if (this.helpOpen) {
        e.preventDefault();
        this.helpOpen = false;
      }
      return;
    }

    if (this.open && this.suggestions.length > 0) {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        this.activeIndex =
          this.activeIndex < 0 ? 0 : (this.activeIndex + 1) % this.suggestions.length;
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        this.activeIndex =
          this.activeIndex < 0
            ? this.suggestions.length - 1
            : (this.activeIndex - 1 + this.suggestions.length) % this.suggestions.length;
        return;
      }
      if ((e.key === 'Tab' || e.key === 'Enter') && this.activeIndex >= 0) {
        const chosen = this.suggestions[this.activeIndex];
        if (chosen) {
          e.preventDefault();
          this.acceptSuggestion(chosen);
          return;
        }
      }
      if (e.key === 'Tab') {
        this.open = false;
        this.suppressExpectedValueError = false;
        return;
      }
    }

    if (e.key === 'Enter') {
      e.preventDefault();
      this.open = false;
      this.suppressExpectedValueError = false;
      this.evaluateAndEmit(this.value, true);
    }
  }

  private acceptSuggestion(s: Suggestion): void {
    const nextValue =
      this.value.slice(0, s.replaceStart) + s.insertText + this.value.slice(s.replaceEnd);
    const nextCursor = s.replaceStart + s.insertText.length;
    this.value = nextValue;
    this.suppressExpectedValueError = Boolean(s.continueSuggest);
    if (s.continueSuggest) {
      // Refresh value suggestions synchronously so `this.open` is true before
      // rendering the suppressed state.
      this.suggestions = getSuggestions(nextValue, nextCursor, this.schema, this.valueSuggestions);
      this.activeIndex = -1;
      this.open = this.suggestions.length > 0;
    } else {
      this.open = false;
    }
    this.evaluateAndEmit(nextValue, !s.continueSuggest);

    void this.updateComplete.then(() => {
      if (this.inputEl) {
        this.inputEl.value = nextValue;
        this.inputEl.setSelectionRange(nextCursor, nextCursor);
        this.inputEl.focus();
      }
    });
  }

  private onClear(): void {
    this.suppressExpectedValueError = false;
    this.value = '';
    this.open = false;
    this.evaluateAndEmit('', true);
    this.inputEl?.focus();
  }

  override render() {
    const activeOptionId =
      this.open && this.activeIndex >= 0 ? `${this.listboxId}-opt-${this.activeIndex}` : undefined;
    const shownError = this.visibleError;

    return html`
      <div class="input-shell ${shownError ? 'has-error' : ''}">
        <sl-icon class="prefix-icon" name="funnel"></sl-icon>
        <input
          type="text"
          role="combobox"
          aria-autocomplete="list"
          aria-expanded=${this.open ? 'true' : 'false'}
          aria-controls=${this.listboxId}
          aria-activedescendant=${activeOptionId ?? nothing}
          aria-invalid=${shownError ? 'true' : 'false'}
          aria-label="Filter query"
          .value=${this.value}
          placeholder=${this.placeholder}
          @input=${this.onInput}
          @focus=${this.onFocus}
          @blur=${this.onBlur}
          @keydown=${this.onKeydown}
        />
        ${this.value
          ? html`
              <button
                type="button"
                class="clear-btn"
                aria-label="Clear filter"
                @click=${this.onClear}
              >
                <sl-icon name="x-lg"></sl-icon>
              </button>
            `
          : nothing}
        <sl-popup placement="bottom-end" distance="6" ?active=${this.helpOpen} flip shift>
          <sl-icon-button
            slot="anchor"
            class="help-btn"
            name="question-circle"
            label="Filter syntax help"
            @click=${() => {
              this.helpOpen = !this.helpOpen;
            }}
          ></sl-icon-button>
          ${this.helpOpen ? renderFilterHelp(this.schema) : nothing}
        </sl-popup>
      </div>

      ${this.open && this.suggestions.length > 0
        ? html`
            <ul id=${this.listboxId} class="listbox" role="listbox">
              ${this.suggestions.map(
                (s, idx) => html`
                  <li
                    id="${this.listboxId}-opt-${idx}"
                    class="option ${idx === this.activeIndex ? 'active' : ''}"
                    role="option"
                    aria-selected=${idx === this.activeIndex ? 'true' : 'false'}
                    @mousedown=${(e: MouseEvent) => {
                      e.preventDefault();
                      this.acceptSuggestion(s);
                    }}
                  >
                    <span class="option-label">${s.label}</span>
                    ${s.help ? html`<span class="option-help">${s.help}</span>` : nothing}
                  </li>
                `
              )}
            </ul>
          `
        : nothing}

      <div
        class="feedback ${shownError ? 'error' : this.warning ? 'warning' : ''}"
        aria-live="polite"
      >
        ${shownError
          ? html`<span class="feedback-msg">${shownError.message}</span>${this.renderErrorSpan(
                shownError
              )}`
          : this.warning
            ? this.warning.message
            : nothing}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-filter-box': ScionFilterBox;
  }
}
