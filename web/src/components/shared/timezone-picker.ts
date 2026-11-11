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
 * Reusable Timezone Picker Component
 *
 * A searchable combobox over the IANA time zone names `listTimeZones()`
 * returns, modelled on `scion-project-picker`'s local search-autocomplete
 * pattern (filtering a fixed list client-side instead of calling an API).
 *
 * Callers that need a "no explicit zone" entry (for example "Auto", which
 * falls back to the browser zone, or "UTC" for a hub-wide default that is
 * empty by default) set `empty-label`; selecting it emits an empty string.
 * Leave it unset to require an explicit zone.
 *
 * Emits `timezone-change` with `{ timezone }` when a zone is selected from
 * the dropdown or typed and committed (blur, or Enter with no dropdown
 * selection active). The component does not validate typed input — callers
 * validate with `isValidTimeZone` from `../../utils/time.js`.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { PropertyValues } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { isValidTimeZone, listTimeZones } from '../../utils/time.js';

/** Event detail emitted when a zone is selected or typed. */
export interface TimezoneChangeDetail {
  timezone: string;
}

const MAX_VISIBLE_RESULTS = 50;

/**
 * Search aids for a handful of common city/country names that Intl's
 * canonical zone list (`listTimeZones()`) does not contain under that
 * spelling, because ICU's canonical pick for that zone is a different
 * historical name. A plain substring match against the candidate list
 * alone finds nothing for these
 * — e.g. "asia/katmandu".includes("kathmandu") is false — so typing one of
 * these terms also surfaces the zone it names.
 *
 * This is a convenience list, not a correctness boundary: a full, exact,
 * valid zone name or alias the list doesn't name here (e.g. "Asia/Kolkata"
 * itself, typed in full) is still offered — see filteredZones's
 * isValidTimeZone check below.
 */
const SEARCH_ALIAS_HINTS: ReadonlyArray<readonly [term: string, zone: string]> = [
  ['kolkata', 'Asia/Calcutta'],
  ['kyiv', 'Europe/Kiev'],
  ['kathmandu', 'Asia/Katmandu'],
  ['ho chi minh', 'Asia/Saigon'],
  ['ho_chi_minh', 'Asia/Saigon'],
];

@customElement('scion-timezone-picker')
export class ScionTimezonePicker extends LitElement {
  /** Input label. */
  @property() label = 'Timezone';

  /** Placeholder override. */
  @property() placeholder = 'Search for a timezone...';

  /** Disabled state. */
  @property({ type: Boolean }) disabled = false;

  /** Current zone value. Empty string means the empty entry, if any. */
  @property() value = '';

  /**
   * Label for a leading entry meaning "no explicit zone" (emits `''`).
   * Unset (the default, `''`) omits the entry, so every selectable option
   * is a real zone name.
   */
  @property({ attribute: 'empty-label' }) emptyLabel = '';

  @state() private searchQuery = '';
  @state() private searchOpen = false;
  @state() private activeDescendantIndex = -1;

  private readonly allZones = listTimeZones();
  private blurTimeoutId: ReturnType<typeof setTimeout> | null = null;
  /** True after selectZone(); reset on new typed input. */
  private selectedViaDropdown = false;

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.blurTimeoutId) clearTimeout(this.blurTimeoutId);
  }

  override willUpdate(changed: PropertyValues<this>): void {
    // Reset the input text when `value` is changed externally (not via our
    // own selectZone()/blur commit), so a parent clearing or reloading the
    // value is reflected without the user having to retype. Computed in
    // willUpdate (before render), not updated (after), so this is part of
    // the same update cycle instead of scheduling a second one.
    //
    // Gate on whether `value` actually differs from what the input
    // currently means (`valueFor(searchQuery)`), not on `selectedViaDropdown`:
    // gating on the flag alone would make every resync after a dropdown
    // selection a no-op until the user typed or cleared, including resyncs
    // for completely unrelated *later* external value changes, so an
    // external `.value` set after a selection would be silently ignored.
    // Comparing values keeps the round-trip-the-same-value case a no-op
    // without that collateral damage. `selectedViaDropdown` is still reset
    // here, since a real resync means whatever it was gating is now stale;
    // it otherwise stays as set by selectZone()/handleSearchInput()/
    // sl-clear, which is where it still matters — gating the blur handler's
    // commitTyped() call below, so a dropdown mousedown selection followed
    // by a blur event doesn't re-emit the same value a second time.
    //
    // Also resync unconditionally on the very first update
    // (`!this.hasUpdated`, true throughout willUpdate on that first call —
    // Lit flips it only after update() commits), and whenever `emptyLabel`
    // itself changes: on the first render, `value` defaults to `''` and
    // `searchQuery` also starts `''`, so `valueFor('') === ''` trivially
    // matches `this.value` and the value-comparison guard alone never fires
    // — the empty-label row ("UTC", "Auto", ...) would otherwise never
    // appear.
    if (
      (changed.has('value') || changed.has('emptyLabel')) &&
      (!this.hasUpdated || this.value !== this.valueFor(this.searchQuery.trim()))
    ) {
      this.searchQuery = this.displayValue(this.value);
      this.selectedViaDropdown = false;
    }
  }

  private displayValue(value: string): string {
    return value ? value : this.emptyLabel;
  }

  private get candidates(): string[] {
    // De-duplicate: if emptyLabel collides with a real zone name already in
    // allZones (e.g. the admin default's empty-label="UTC", and
    // listTimeZones() always includes the real "UTC"), drop that zone from
    // the plain list so there is one row, not two.
    const base = this.emptyLabel
      ? this.allZones.filter((z) => z !== this.emptyLabel)
      : this.allZones;

    // Always include the current value, even when it's a valid alias
    // Intl's canonical list omits (e.g. a stored "Asia/Kathmandu" when the
    // canonical list only has "Asia/Katmandu" — see isValidTimeZone's
    // doc comment), so a persisted alias is listed instead of looking
    // "not found" on focus.
    const extras: string[] = [];
    if (this.emptyLabel) extras.push(this.emptyLabel);
    if (this.value && this.value !== this.emptyLabel && !base.includes(this.value)) {
      extras.push(this.value);
    }
    return [...extras, ...base];
  }

  private get filteredZones(): string[] {
    const trimmed = this.searchQuery.trim();
    const query = trimmed.toLowerCase();
    const candidates = this.candidates;
    if (!query) return candidates.slice(0, MAX_VISIBLE_RESULTS);

    let matches = candidates.filter((z) => z.toLowerCase().includes(query));

    // Alias search aid: surface the canonical zone for a handful of
    // common alternate names a plain substring match can't find (see
    // SEARCH_ALIAS_HINTS's doc comment).
    for (const [term, zone] of SEARCH_ALIAS_HINTS) {
      if (term.includes(query) && candidates.includes(zone) && !matches.includes(zone)) {
        matches = [zone, ...matches];
      }
    }

    // If the typed text is itself a full, valid zone name or alias not
    // already offered (e.g. "Asia/Kolkata", a valid Intl alias absent from
    // supportedValuesOf — see isValidTimeZone), surface it directly so it
    // can be selected.
    if (isValidTimeZone(trimmed) && !matches.includes(trimmed)) {
      matches = [trimmed, ...matches];
    }

    return matches.slice(0, MAX_VISIBLE_RESULTS);
  }

  private valueFor(displayText: string): string {
    return displayText === this.emptyLabel ? '' : displayText;
  }

  static override styles = css`
    :host {
      display: block;
    }

    .timezone-search-container {
      position: relative;
    }

    .timezone-search-dropdown {
      position: absolute;
      top: 100%;
      left: 0;
      right: 0;
      z-index: 1000;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      box-shadow: 0 4px 12px rgba(0, 0, 0, 0.1);
      max-height: 240px;
      overflow-y: auto;
      margin-top: 0.25rem;
    }

    .timezone-search-option {
      display: flex;
      align-items: center;
      padding: 0.5rem 0.75rem;
      min-height: 36px;
      cursor: pointer;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .timezone-search-option:last-child {
      border-bottom: none;
    }

    .timezone-search-option:hover,
    .timezone-search-option.active-descendant {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .timezone-search-empty {
      padding: 0.75rem;
      text-align: center;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    @media (forced-colors: active) {
      .timezone-search-dropdown {
        border: 2px solid ButtonText;
      }

      .timezone-search-option:hover,
      .timezone-search-option.active-descendant {
        outline: 2px solid Highlight;
      }
    }
  `;

  private handleSearchInput(e: Event): void {
    this.searchQuery = (e.target as HTMLInputElement).value;
    this.selectedViaDropdown = false;
    this.activeDescendantIndex = -1;
    this.searchOpen = true;
  }

  private handleKeydown(e: KeyboardEvent): void {
    if (!this.searchOpen) return;
    const results = this.filteredZones;

    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        if (this.activeDescendantIndex < results.length - 1) {
          this.activeDescendantIndex++;
        }
        break;
      case 'ArrowUp':
        e.preventDefault();
        if (this.activeDescendantIndex > 0) {
          this.activeDescendantIndex--;
        }
        break;
      case 'Enter':
        e.preventDefault();
        if (this.activeDescendantIndex >= 0 && this.activeDescendantIndex < results.length) {
          this.selectZone(results[this.activeDescendantIndex]);
        } else {
          this.commitTyped();
        }
        break;
      case 'Escape':
        e.preventDefault();
        this.searchOpen = false;
        this.activeDescendantIndex = -1;
        break;
    }
  }

  private selectZone(displayText: string): void {
    this.searchQuery = displayText;
    this.searchOpen = false;
    this.activeDescendantIndex = -1;
    this.selectedViaDropdown = true;
    this.emitChange(this.valueFor(displayText));
  }

  private commitTyped(): void {
    this.emitChange(this.valueFor(this.searchQuery.trim()));
  }

  private emitChange(timezone: string): void {
    this.dispatchEvent(
      new CustomEvent<TimezoneChangeDetail>('timezone-change', {
        detail: { timezone },
        bubbles: true,
        composed: true,
      })
    );
  }

  override render() {
    const results = this.filteredZones;
    return html`
      <div class="timezone-search-container">
        <sl-input
          label=${this.label}
          placeholder=${this.placeholder}
          value=${this.searchQuery}
          type="text"
          autocomplete="off"
          role="combobox"
          aria-expanded=${this.searchOpen ? 'true' : 'false'}
          aria-controls="timezone-picker-listbox"
          aria-activedescendant=${this.activeDescendantIndex >= 0
            ? `timezone-picker-option-${this.activeDescendantIndex}`
            : ''}
          clearable
          ?disabled=${this.disabled}
          @sl-input=${(e: Event) => this.handleSearchInput(e)}
          @sl-clear=${() => {
            this.searchQuery = '';
            this.selectedViaDropdown = false;
            this.emitChange(this.valueFor(''));
          }}
          @keydown=${(e: KeyboardEvent) => this.handleKeydown(e)}
          @sl-focus=${() => {
            this.searchOpen = true;
          }}
          @sl-blur=${() => {
            this.blurTimeoutId = setTimeout(() => {
              this.searchOpen = false;
              this.activeDescendantIndex = -1;
              if (!this.selectedViaDropdown) this.commitTyped();
            }, 200);
          }}
        ></sl-input>
        ${this.searchOpen
          ? html`
              <div
                class="timezone-search-dropdown"
                id="timezone-picker-listbox"
                role="listbox"
                aria-label="Timezone search results"
              >
                ${results.length === 0
                  ? html`<div class="timezone-search-empty" role="status" aria-live="polite">
                      No matching timezones
                    </div>`
                  : results.map(
                      (zone, idx) => html`
                        <div
                          class="timezone-search-option ${idx === this.activeDescendantIndex
                            ? 'active-descendant'
                            : ''}"
                          id="timezone-picker-option-${idx}"
                          role="option"
                          aria-selected=${idx === this.activeDescendantIndex ? 'true' : 'false'}
                          @mousedown=${(e: Event) => {
                            e.preventDefault();
                            this.selectZone(zone);
                          }}
                        >
                          ${zone}
                        </div>
                      `
                    )}
              </div>
            `
          : nothing}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-timezone-picker': ScionTimezonePicker;
  }
}
