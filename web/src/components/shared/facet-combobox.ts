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
 * Shared Facet Combobox Component (`<scion-facet-combobox>`, Design D14, ptone/scion#7993).
 *
 * Provides a compact accessible ARIA `combobox` + `listbox` over a local facet
 * list (`{ value, label, hint? }`), using the quick palette's fuzzy matcher and
 * highlight-range helpers (`web/src/utils/palette-match.ts`).
 *
 * - Opens on focus, click, chevron click, or `ArrowDown`/`ArrowUp` to allow picking without typing.
 * - Typing narrows and ranks options by `classifyMatch` tiers and highlights matched slices with `<mark>`.
 * - Selecting an option (click or `Enter`) commits the filter and emits `facet-change`.
 * - Pressing `Escape` (when no uncommitted draft is being edited) or clicking Clear removes the filter.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import {
  classifyMatch,
  highlightRangesFor,
  normalizeQuery,
  type HighlightRange,
  type MatchTier,
} from '../../utils/palette-match.js';

/** One selectable facet option in `<scion-facet-combobox>`. */
export interface FacetComboboxOption {
  value: string;
  label: string;
  hint?: string;
}

/** Detail payload of the `facet-change` event. */
export interface FacetChangeDetail {
  value: string;
  label: string;
}

/** Ranked option produced by {@link rankFacetOptions}. */
export interface RankedFacetOption {
  option: FacetComboboxOption;
  tier: MatchTier;
  labelRanges: HighlightRange[];
  hintRanges: HighlightRange[];
}

let nextComboboxInstanceId = 0;

/**
 * Filters and ranks `options` against `query` using the quick palette's
 * `classifyMatch` tiers and `highlightRangesFor` ranges. An empty query returns
 * all options in input order with no highlight ranges.
 */
export function rankFacetOptions(
  query: string,
  options: readonly FacetComboboxOption[]
): RankedFacetOption[] {
  const q = normalizeQuery(query);
  if (!q) {
    return options.map((option) => ({
      option,
      tier: 0,
      labelRanges: [],
      hintRanges: [],
    }));
  }

  interface ScoredEntry extends RankedFacetOption {
    matchedOnLabel: boolean;
    index: number;
  }

  const scored: ScoredEntry[] = [];
  for (let i = 0; i < options.length; i++) {
    const option = options[i];
    const labelTier = classifyMatch(q, option.label);
    const hintTier = option.hint ? classifyMatch(q, option.hint) : null;
    if (labelTier === null && hintTier === null) continue;

    const matchedOnLabel = labelTier !== null && (hintTier === null || labelTier <= hintTier);
    const tier = matchedOnLabel ? labelTier : (hintTier ?? 3);
    const labelRanges = labelTier !== null ? highlightRangesFor(q, option.label) : [];
    const hintRanges =
      !matchedOnLabel && hintTier !== null && option.hint ? highlightRangesFor(q, option.hint) : [];

    scored.push({
      option,
      tier,
      labelRanges,
      hintRanges,
      matchedOnLabel,
      index: i,
    });
  }

  scored.sort((a, b) => {
    if (a.tier !== b.tier) return a.tier - b.tier;
    if (a.matchedOnLabel !== b.matchedOnLabel) return a.matchedOnLabel ? -1 : 1;
    return a.index - b.index;
  });

  return scored.map(({ option, tier, labelRanges, hintRanges }) => ({
    option,
    tier,
    labelRanges,
    hintRanges,
  }));
}

function renderHighlightedText(
  text: string,
  ranges: readonly HighlightRange[]
): string | TemplateResult[] {
  if (ranges.length === 0) return text;
  const parts: TemplateResult[] = [];
  let cursor = 0;
  for (const range of ranges) {
    if (range.start > cursor) {
      parts.push(html`${text.slice(cursor, range.start)}`);
    }
    parts.push(html`<mark>${text.slice(range.start, range.end)}</mark>`);
    cursor = range.end;
  }
  if (cursor < text.length) {
    parts.push(html`${text.slice(cursor)}`);
  }
  return parts;
}

@customElement('scion-facet-combobox')
export class ScionFacetCombobox extends LitElement {
  /** Visible field label. */
  @property({ type: String }) label = '';

  /** Input placeholder when no option is selected. */
  @property({ type: String }) placeholder = 'Any';

  /** Facet options in scope. */
  @property({ attribute: false }) options: FacetComboboxOption[] = [];

  /** Committed option value (`''` when unfiltered). */
  @property({ type: String }) value = '';

  /** Whether the control is disabled. */
  @property({ type: Boolean }) disabled = false;

  /** Optional muted footer line shown at the bottom of the open listbox. */
  @property({ type: String, attribute: 'footer-text' }) footerText = '';

  @state() private open = false;
  @state() private focused = false;
  @state() private editing = false;
  @state() private draftQuery = '';
  @state() private activeIndex = -1;

  private readonly instanceId = `facet-cbx-${++nextComboboxInstanceId}`;
  private blurTimer: ReturnType<typeof setTimeout> | null = null;

  static override styles = css`
    :host {
      display: block;
      min-width: 10rem;
    }

    .cbx {
      position: relative;
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
      font-size: 0.875rem;
    }

    label {
      font-size: var(--sl-input-label-font-size-small, 0.875rem);
      font-weight: var(--sl-input-label-font-weight, 400);
      color: var(--scion-text, #1e293b);
      line-height: 1.25;
    }

    .inp {
      display: flex;
      align-items: center;
      gap: 0.25rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--sl-input-border-radius-small, 0.25rem);
      height: var(--sl-input-height-small, 1.875rem);
      padding: 0 0.45rem 0 0.6rem;
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text, #1e293b);
      transition:
        border-color 0.12s ease,
        box-shadow 0.12s ease;
    }

    .inp.focus {
      border-color: var(--scion-primary, #2563eb);
      box-shadow: 0 0 0 3px color-mix(in srgb, var(--scion-primary, #2563eb) 22%, transparent);
    }

    .inp.disabled {
      opacity: 0.6;
      cursor: not-allowed;
    }

    input {
      flex: 1;
      min-width: 0;
      border: none;
      outline: none;
      background: transparent;
      color: inherit;
      font: inherit;
      font-size: var(--sl-input-font-size-small, 0.875rem);
      padding: 0;
      height: 100%;
    }

    input::placeholder {
      color: var(--scion-text-muted, #64748b);
    }

    @media (pointer: coarse) {
      input {
        font-size: max(16px, 0.875rem);
      }
    }

    .icon-btn {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      border: none;
      background: transparent;
      padding: 0.1rem;
      margin: 0;
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      font-size: 0.85rem;
      line-height: 1;
      border-radius: 0.2rem;
      flex-shrink: 0;
    }

    .icon-btn:hover:not(:disabled) {
      color: var(--scion-text, #1e293b);
    }

    .icon-btn:disabled {
      cursor: not-allowed;
    }

    .list {
      position: absolute;
      top: calc(100% + 0.2rem);
      left: 0;
      min-width: 100%;
      width: max-content;
      max-width: min(22rem, 92vw);
      max-height: 15rem;
      overflow-y: auto;
      z-index: 20;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      box-shadow: 0 6px 18px rgba(0, 0, 0, 0.14);
      padding: 0.25rem 0;
    }

    .opt {
      display: flex;
      gap: 0.5rem;
      align-items: baseline;
      padding: 0.35rem 0.65rem;
      cursor: pointer;
      color: var(--scion-text, #1e293b);
      font-size: 0.85rem;
      line-height: 1.35;
    }

    .opt:hover,
    .opt.act {
      background: var(
        --scion-primary-50,
        color-mix(in srgb, var(--scion-primary, #2563eb) 12%, transparent)
      );
    }

    .opt-label {
      overflow-wrap: anywhere;
    }

    .opt-hint {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.75rem;
      margin-left: auto;
      white-space: nowrap;
    }

    mark {
      background: none;
      color: var(--scion-primary, #2563eb);
      font-weight: 700;
    }

    .empty {
      padding: 0.45rem 0.65rem;
      font-size: 0.8rem;
      color: var(--scion-text-muted, #64748b);
    }

    .foot {
      font-size: 0.72rem;
      color: var(--scion-text-muted, #64748b);
      padding: 0.25rem 0.65rem 0.15rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      margin-top: 0.2rem;
    }

    @media (forced-colors: active) {
      .list {
        border: 2px solid ButtonText;
      }

      .opt:hover,
      .opt.act {
        outline: 2px solid Highlight;
      }
    }
  `;

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.blurTimer) {
      clearTimeout(this.blurTimer);
      this.blurTimer = null;
    }
  }

  override willUpdate(changed: PropertyValues<this>): void {
    // When parent updates or clears `value` externally, exit draft-editing mode
    // so the committed option label (or empty placeholder) renders cleanly.
    if (changed.has('value') && !this.focused) {
      this.editing = false;
      this.draftQuery = '';
      this.activeIndex = -1;
    } else if (changed.has('value') && !this.value && this.editing) {
      this.editing = false;
      this.draftQuery = '';
      this.activeIndex = -1;
    }
  }

  private get selectedOption(): FacetComboboxOption | undefined {
    if (!this.value) return undefined;
    return this.options.find((opt) => opt.value === this.value);
  }

  private get inputDisplayValue(): string {
    if (this.editing) return this.draftQuery;
    if (!this.value) return '';
    return this.selectedOption?.label ?? this.value;
  }

  private get rankedOptions(): RankedFacetOption[] {
    const query = this.editing ? this.draftQuery : '';
    return rankFacetOptions(query, this.options);
  }

  private openDropdown(): void {
    if (this.disabled) return;
    if (!this.open) {
      this.open = true;
      const ranked = this.rankedOptions;
      const selectedIdx = this.value
        ? ranked.findIndex((item) => item.option.value === this.value)
        : -1;
      this.activeIndex = selectedIdx;
    }
  }

  private toggleDropdown(): void {
    if (this.disabled) return;
    const input = this.shadowRoot?.querySelector<HTMLInputElement>('input');
    if (this.open) {
      this.open = false;
      this.editing = false;
      this.draftQuery = '';
      this.activeIndex = -1;
    } else {
      this.openDropdown();
      input?.focus();
    }
  }

  private handleFocus(e: FocusEvent): void {
    if (this.blurTimer) {
      clearTimeout(this.blurTimer);
      this.blurTimer = null;
    }
    this.focused = true;
    this.openDropdown();
    const input = e.target as HTMLInputElement | null;
    if (input && this.value && !this.editing) {
      // Select committed text on focus so typing immediately replaces it with a new query.
      input.select();
    }
  }

  private handleBlur(): void {
    if (this.blurTimer) clearTimeout(this.blurTimer);
    this.blurTimer = setTimeout(() => {
      this.blurTimer = null;
      this.focused = false;
      this.open = false;
      this.editing = false;
      this.draftQuery = '';
      this.activeIndex = -1;
    }, 150);
  }

  private handleInput(e: Event): void {
    const next = (e.target as HTMLInputElement).value;
    this.editing = true;
    this.draftQuery = next;
    this.open = true;
    const ranked = rankFacetOptions(next, this.options);
    this.activeIndex = normalizeQuery(next) && ranked.length > 0 ? 0 : -1;
  }

  private handleKeydown(e: KeyboardEvent): void {
    if (this.disabled) return;
    const ranked = this.rankedOptions;

    switch (e.key) {
      case 'ArrowDown': {
        e.preventDefault();
        if (!this.open) {
          this.open = true;
          this.activeIndex = ranked.length > 0 ? 0 : -1;
          return;
        }
        if (ranked.length === 0) return;
        this.activeIndex = this.activeIndex < ranked.length - 1 ? this.activeIndex + 1 : 0;
        this.scrollActiveIntoView();
        break;
      }
      case 'ArrowUp': {
        e.preventDefault();
        if (!this.open) {
          this.open = true;
          this.activeIndex = ranked.length > 0 ? ranked.length - 1 : -1;
          return;
        }
        if (ranked.length === 0) return;
        this.activeIndex = this.activeIndex > 0 ? this.activeIndex - 1 : ranked.length - 1;
        this.scrollActiveIntoView();
        break;
      }
      case 'Enter': {
        if (!this.open) return;
        e.preventDefault();
        if (this.activeIndex >= 0 && this.activeIndex < ranked.length) {
          this.commitOption(ranked[this.activeIndex].option);
        } else if (this.editing && !this.draftQuery.trim() && this.value) {
          this.clearSelection();
        }
        break;
      }
      case 'Escape': {
        if (!this.open && !this.value && !this.editing) return;
        e.preventDefault();
        e.stopPropagation();
        if (this.editing) {
          // Discard uncommitted typed query; if no filter was committed, just close.
          this.editing = false;
          this.draftQuery = '';
          this.open = false;
          this.activeIndex = -1;
          return;
        }
        if (this.value) {
          // D14: Escape removes the committed filter.
          this.clearSelection();
          return;
        }
        this.open = false;
        this.activeIndex = -1;
        break;
      }
    }
  }

  private scrollActiveIntoView(): void {
    void this.updateComplete.then(() => {
      if (this.activeIndex < 0) return;
      const optEl = this.shadowRoot?.querySelector<HTMLElement>(
        `#${this.instanceId}-opt-${this.activeIndex}`
      );
      optEl?.scrollIntoView?.({ block: 'nearest' });
    });
  }

  private commitOption(opt: FacetComboboxOption): void {
    const changed = this.value !== opt.value;
    this.value = opt.value;
    this.editing = false;
    this.draftQuery = '';
    this.open = false;
    this.activeIndex = -1;
    if (changed) {
      this.emitChange(opt.value, opt.label);
    }
  }

  clearSelection(): void {
    const hadValue = Boolean(this.value);
    this.value = '';
    this.editing = false;
    this.draftQuery = '';
    this.open = false;
    this.activeIndex = -1;
    if (hadValue) {
      this.emitChange('', '');
    }
  }

  private emitChange(value: string, label: string): void {
    this.dispatchEvent(
      new CustomEvent<FacetChangeDetail>('facet-change', {
        detail: { value, label },
        bubbles: true,
        composed: true,
      })
    );
  }

  override render(): TemplateResult {
    const inputId = `${this.instanceId}-input`;
    const listboxId = `${this.instanceId}-listbox`;
    const ranked = this.rankedOptions;
    const activeOptionId =
      this.open && this.activeIndex >= 0 && this.activeIndex < ranked.length
        ? `${this.instanceId}-opt-${this.activeIndex}`
        : '';
    const showClear = !this.disabled && (Boolean(this.value) || Boolean(this.draftQuery));
    const footer = this.footerText.trim();

    return html`
      <div class="cbx">
        ${this.label ? html`<label for=${inputId}>${this.label}</label>` : nothing}
        <div class="inp ${this.focused ? 'focus' : ''} ${this.disabled ? 'disabled' : ''}">
          <input
            id=${inputId}
            type="text"
            role="combobox"
            autocomplete="off"
            aria-autocomplete="list"
            aria-expanded=${this.open ? 'true' : 'false'}
            aria-controls=${this.open ? listboxId : nothing}
            aria-activedescendant=${activeOptionId || nothing}
            placeholder=${this.placeholder}
            .value=${this.inputDisplayValue}
            ?disabled=${this.disabled}
            @focus=${(e: FocusEvent): void => this.handleFocus(e)}
            @blur=${(): void => this.handleBlur()}
            @click=${(): void => this.openDropdown()}
            @input=${(e: Event): void => this.handleInput(e)}
            @keydown=${(e: KeyboardEvent): void => this.handleKeydown(e)}
          />
          ${showClear
            ? html`
                <button
                  type="button"
                  class="icon-btn clear-btn"
                  tabindex="-1"
                  data-combobox-clear
                  title="Clear ${this.label || 'filter'} (Esc)"
                  aria-label="Clear ${this.label || 'filter'}"
                  @mousedown=${(e: Event): void => e.preventDefault()}
                  @click=${(): void => this.clearSelection()}
                >
                  <sl-icon name="x-circle-fill"></sl-icon>
                </button>
              `
            : nothing}
          <button
            type="button"
            class="icon-btn toggle-btn"
            tabindex="-1"
            data-combobox-toggle
            ?disabled=${this.disabled}
            aria-label="${this.open ? 'Close' : 'Open'} ${this.label || 'options'}"
            @mousedown=${(e: Event): void => {
              e.preventDefault();
              this.toggleDropdown();
            }}
          >
            <sl-icon name=${this.open ? 'chevron-up' : 'chevron-down'}></sl-icon>
          </button>
        </div>
        ${this.open
          ? html`
              <div
                class="list"
                id=${listboxId}
                role="listbox"
                aria-label=${this.label ? `${this.label} options` : 'Options'}
              >
                ${ranked.length === 0
                  ? html`<div class="empty" role="status" aria-live="polite">No matches</div>`
                  : ranked.map(
                      ({ option, labelRanges, hintRanges }, idx) => html`
                        <div
                          class="opt ${idx === this.activeIndex ? 'act' : ''}"
                          id="${this.instanceId}-opt-${idx}"
                          role="option"
                          data-option-value=${option.value}
                          aria-selected=${option.value === this.value ? 'true' : 'false'}
                          @mouseenter=${(): void => {
                            this.activeIndex = idx;
                          }}
                          @mousedown=${(e: Event): void => {
                            e.preventDefault();
                            this.commitOption(option);
                          }}
                        >
                          <span class="opt-label"
                            >${renderHighlightedText(option.label, labelRanges)}</span
                          >
                          ${option.hint
                            ? html`<span class="opt-hint"
                                >${renderHighlightedText(option.hint, hintRanges)}</span
                              >`
                            : nothing}
                        </div>
                      `
                    )}
                ${footer ? html`<div class="foot">${footer}</div>` : nothing}
              </div>
            `
          : nothing}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-facet-combobox': ScionFacetCombobox;
  }
}
