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
 * Chat quick-switcher component (Cmd/Ctrl-K).
 *
 * Renders a modal overlay with a search input and a filterable list of
 * conversations. Keyboard navigation (Arrow Up/Down, Enter, Escape) is
 * supported. Conversations are sorted by most recent activity first when
 * the search field is empty.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, state, query } from 'lit/decorators.js';

import type {
  GroupState,
  PaletteCandidate,
  PaletteDismissReason,
  PaletteGroup,
  PaletteTarget,
} from '../../../client/chat-palette-types.js';
import {
  rankCandidates,
  type HighlightRange,
  type RankedCandidate,
} from '../../../utils/chat-palette-match.js';

/** A conversation entry displayable in the switcher. */
export interface SwitcherConversation {
  /** Topic UUID or DM key. */
  conversationKey: string;
  /** Display name: thread name, DM peer name, etc. */
  name: string;
  /** Project/space name for context. */
  spaceName: string;
  /** True for DMs, false for threads. */
  isDM: boolean;
  /** ISO timestamp for sorting by recency. */
  lastActivityAt?: string;
  /** Project ID for thread navigation (not set for DMs). */
  projectId?: string;
}

/** Detail emitted on `switcher-select`. */
export interface SwitcherSelectDetail {
  conversationKey: string;
}

@customElement('scion-chat-switcher')
export class ScionChatSwitcher extends LitElement {
  /** Full list of conversations to search through. */
  @property({ type: Array })
  conversations: SwitcherConversation[] = [];

  @state() private searchTerm = '';
  @state() private selectedIndex = 0;

  @query('#switcher-input')
  private inputEl!: HTMLInputElement;

  // ===========================================================================
  // Grouped palette presentation (native chat quick command palette).
  //
  // Gated by `paletteMode`, default false. The legacy flat-list presentation
  // above (properties, styles, render path) is completely unchanged: with
  // `paletteMode` unset, chat-switcher.test.ts exercises that same code
  // path unconditionally. Phase 1 only ever populates the 'agents' group;
  // `threads`, `people` and `documents` are added to `groups` by later
  // phases.
  // ===========================================================================

  /** Selects the grouped sl-dialog presentation instead of the legacy flat overlay. */
  @property({ type: Boolean, attribute: 'palette-mode' })
  paletteMode = false;

  /** Whether the palette dialog is open. Only meaningful when `paletteMode` is true. */
  @property({ type: Boolean }) open = false;

  /** Per-group load state. Phase 1 only ever sets `agents`. */
  @property({ attribute: false })
  groups: Partial<Record<PaletteGroup, GroupState>> = {
    agents: { status: 'loading', candidates: [] },
  };

  @state() private queryText = '';
  /** The globally-selected candidate ID, or null when nothing matches. */
  @state() private activeId: string | null = null;
  /** True once Up/Down/click has picked a candidate; a query edit clears it back to "auto". */
  private manualSelection = false;
  /** True once Enter has committed a selection this open, so a stray repeat can't double-fire. */
  private committed = false;
  private composing = false;
  private domIdCounter = 0;
  private readonly domIdByCandidateId = new Map<string, string>();

  @query('#palette-query-input')
  private paletteInputEl?: HTMLInputElement;

  static override styles = [
    css`
      .palette-dialog::part(panel) {
        width: min(560px, 92vw);
      }

      .palette-dialog::part(body) {
        padding: 0;
      }

      .palette-input-row {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.25rem 0.75rem 0.75rem;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
      }

      #palette-query-input {
        flex: 1;
        border: none;
        outline: none;
        font-size: var(--chat-fs-xl);
        background: transparent;
        color: var(--scion-text, #1e293b);
        padding: 0.25rem 0;
      }

      .palette-results {
        max-height: 50vh;
        overflow-y: auto;
        padding-bottom: 0.5rem;
      }

      .palette-group-heading {
        font-size: var(--chat-fs-sm);
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.04em;
        color: var(--scion-text-muted, #64748b);
        padding: 0.5rem 1rem 0.25rem;
      }

      .palette-option {
        display: flex;
        flex-direction: column;
        padding: 0.5rem 1rem;
        cursor: pointer;
        gap: 0.125rem;
        border-left: 3px solid transparent;
      }

      .palette-option.active {
        background: var(--scion-bg-subtle, #f1f5f9);
        border-left-color: var(--scion-primary, #3b82f6);
      }

      /*
       * Pointer affordance only — hover must never change which row Enter
       * would commit. A CSS-only :hover state (rather than a @mouseenter
       * handler updating activeId) guarantees that: hovering repaints
       * nothing but appearance.
       */
      .palette-option:hover:not(.active) {
        background: var(--scion-bg-subtle, #f8fafc);
      }

      .palette-option mark {
        background: none;
        color: inherit;
        font-weight: 700;
        padding: 0;
      }

      .palette-secondary {
        font-size: var(--chat-fs-base);
        color: var(--scion-text-muted, #94a3b8);
      }

      .palette-empty,
      .palette-loading,
      .palette-group-error {
        padding: 0.5rem 1rem 0.75rem;
        color: var(--scion-text-muted, #94a3b8);
        font-size: var(--chat-fs-base);
      }

      .palette-group-error sl-button {
        margin-top: 0.25rem;
      }

      .palette-show-more {
        padding: 0.25rem 1rem 0.5rem;
      }

      .palette-help {
        padding: 0.375rem 1rem;
        font-size: var(--chat-fs-sm);
        color: var(--scion-text-muted, #94a3b8);
        border-top: 1px solid var(--scion-border, #e2e8f0);
        display: flex;
        gap: 1rem;
      }

      .palette-help kbd {
        display: inline-block;
        padding: 0 0.25rem;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: 0.125rem;
        font-family: inherit;
        font-size: var(--chat-fs-xs);
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .palette-status {
        position: absolute;
        width: 1px;
        height: 1px;
        overflow: hidden;
        clip: rect(0 0 0 0);
        white-space: nowrap;
      }
    `,
    css`
      :host {
        display: block;
      }

      .overlay {
        position: fixed;
        inset: 0;
        z-index: 9999;
        display: flex;
        align-items: flex-start;
        justify-content: center;
        padding-top: 15vh;
        background: rgba(0, 0, 0, 0.4);
      }

      .panel {
        width: min(520px, 90vw);
        max-height: 60vh;
        display: flex;
        flex-direction: column;
        background: var(--scion-surface, #fff);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: 0.5rem;
        box-shadow: 0 8px 32px rgba(0, 0, 0, 0.2);
        overflow: hidden;
      }

      .search-row {
        display: flex;
        align-items: center;
        padding: 0.75rem 1rem;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
        gap: 0.5rem;
      }

      .search-row sl-icon {
        color: var(--scion-text-muted, #64748b);
        font-size: var(--chat-fs-2xl);
      }

      .search-row input {
        flex: 1;
        border: none;
        outline: none;
        font-size: var(--chat-fs-xl);
        background: transparent;
        color: var(--scion-text, #1e293b);
      }

      .search-row input::placeholder {
        color: var(--scion-text-muted, #94a3b8);
      }

      .results {
        overflow-y: auto;
        max-height: calc(60vh - 3.5rem);
      }

      .empty {
        padding: 2rem 1rem;
        text-align: center;
        color: var(--scion-text-muted, #94a3b8);
        font-size: var(--chat-fs-lg);
      }

      .item {
        display: flex;
        flex-direction: column;
        padding: 0.5rem 1rem;
        cursor: pointer;
        gap: 0.125rem;
        border-left: 3px solid transparent;
      }

      .item:hover,
      .item.selected {
        background: var(--scion-bg-subtle, #f1f5f9);
        border-left-color: var(--scion-primary, #3b82f6);
      }

      .item-name {
        font-size: var(--chat-fs-lg);
        font-weight: 500;
        color: var(--scion-text, #1e293b);
      }

      .item-context {
        font-size: var(--chat-fs-base);
        color: var(--scion-text-muted, #94a3b8);
      }

      .dm-badge {
        display: inline-block;
        font-size: var(--chat-fs-sm);
        padding: 0 0.25rem;
        border-radius: 0.125rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
        margin-left: 0.25rem;
      }

      .shortcut-hint {
        padding: 0.375rem 1rem;
        font-size: var(--chat-fs-sm);
        color: var(--scion-text-muted, #94a3b8);
        border-top: 1px solid var(--scion-border, #e2e8f0);
        display: flex;
        gap: 1rem;
      }

      kbd {
        display: inline-block;
        padding: 0 0.25rem;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: 0.125rem;
        font-family: inherit;
        font-size: var(--chat-fs-xs);
        background: var(--scion-bg-subtle, #f1f5f9);
      }
    `,
  ];

  override firstUpdated(): void {
    if (this.paletteMode) return;
    // Autofocus the input after render.
    requestAnimationFrame(() => {
      this.inputEl?.focus();
    });
  }

  private get filtered(): SwitcherConversation[] {
    const term = this.searchTerm.toLowerCase().trim();
    let list = this.conversations;

    if (term) {
      list = list.filter(
        (c) => c.name.toLowerCase().includes(term) || c.spaceName.toLowerCase().includes(term)
      );
    }

    // Sort by most recent activity first.
    return [...list].sort((a, b) => {
      const ta = a.lastActivityAt ? new Date(a.lastActivityAt).getTime() : 0;
      const tb = b.lastActivityAt ? new Date(b.lastActivityAt).getTime() : 0;
      return tb - ta;
    });
  }

  private handleInput(e: InputEvent): void {
    this.searchTerm = (e.target as HTMLInputElement).value;
    this.selectedIndex = 0;
  }

  private handleKeydown(e: KeyboardEvent): void {
    const items = this.filtered;
    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        this.selectedIndex = Math.min(this.selectedIndex + 1, items.length - 1);
        this.scrollSelectedIntoView();
        break;
      case 'ArrowUp':
        e.preventDefault();
        this.selectedIndex = Math.max(this.selectedIndex - 1, 0);
        this.scrollSelectedIntoView();
        break;
      case 'Enter':
        e.preventDefault();
        if (items.length > 0 && this.selectedIndex < items.length) {
          this.selectConversation(items[this.selectedIndex]);
        }
        break;
      case 'Escape':
        e.preventDefault();
        this.close();
        break;
    }
  }

  private scrollSelectedIntoView(): void {
    requestAnimationFrame(() => {
      const selected = this.shadowRoot?.querySelector('.item.selected');
      selected?.scrollIntoView({ block: 'nearest' });
    });
  }

  private selectConversation(conv: SwitcherConversation): void {
    this.dispatchEvent(
      new CustomEvent<SwitcherSelectDetail>('switcher-select', {
        detail: { conversationKey: conv.conversationKey },
        bubbles: true,
        composed: true,
      })
    );
  }

  private close(): void {
    this.dispatchEvent(new CustomEvent('switcher-close', { bubbles: true, composed: true }));
  }

  private handleOverlayClick(e: MouseEvent): void {
    // Close only when clicking the backdrop, not the panel.
    if ((e.target as HTMLElement)?.classList.contains('overlay')) {
      this.close();
    }
  }

  // ---------------------------------------------------------------------
  // Grouped palette: ranking, DOM option IDs, keyboard, and rendering.
  // ---------------------------------------------------------------------

  /**
   * Memoization cache for {@link rankedPaletteCandidates}:
   * without it, ranking (a filter + classify + highlight-range computation
   * pass over every candidate, then an O(N log N) sort) ran three times per
   * render — once each from `willUpdate`, `renderPalette`'s match count, and
   * `renderPaletteGroup` — plus once per arrow-key press. `groups` is always
   * replaced wholesale, never mutated in place (every call site does
   * `this.groups = {...this.groups, ...}`), so reference equality on it is a
   * correct, cheap invalidation check; same for the primitive `queryText`.
   */
  private _rankedPaletteCache: {
    queryText: string;
    groups: Partial<Record<PaletteGroup, GroupState>>;
    result: Array<RankedCandidate<PaletteCandidate>>;
  } | null = null;

  /** Ranked, filtered candidates for the current query, across every populated group. */
  private get rankedPaletteCandidates(): Array<RankedCandidate<PaletteCandidate>> {
    const cache = this._rankedPaletteCache;
    if (cache && cache.queryText === this.queryText && cache.groups === this.groups) {
      return cache.result;
    }
    const all: PaletteCandidate[] = [];
    for (const group of Object.values(this.groups)) {
      if (group) all.push(...group.candidates);
    }
    const result = rankCandidates(this.queryText, all);
    this._rankedPaletteCache = { queryText: this.queryText, groups: this.groups, result };
    return result;
  }

  /** Stable, DOM-safe option ID for a candidate ID. Never derived from the (user-controlled) label. */
  private domIdFor(candidateId: string): string {
    let id = this.domIdByCandidateId.get(candidateId);
    if (!id) {
      id = `palette-option-${this.domIdCounter++}`;
      this.domIdByCandidateId.set(candidateId, id);
    }
    return id;
  }

  /**
   * Recompute the active (globally-selected) candidate after the ranked list
   * changes (query edit or a group finishing/refreshing load).
   *
   * A query edit always resets to the new global best. A group refresh
   * preserves a manual selection by stable ID when it is still present,
   * otherwise falls back to the new global best.
   */
  private reconcileActiveId(
    ranked: Array<RankedCandidate<PaletteCandidate>>,
    queryChanged: boolean
  ): void {
    if (queryChanged) {
      this.manualSelection = false;
      this.activeId = ranked[0]?.candidate.id ?? null;
      return;
    }
    if (this.manualSelection && ranked.some((r) => r.candidate.id === this.activeId)) {
      return;
    }
    this.activeId = ranked[0]?.candidate.id ?? null;
  }

  override willUpdate(changed: PropertyValues<ScionChatSwitcher>): void {
    if (!this.paletteMode) return;
    // `queryText` is a private @state field: TypeScript's `keyof` on a class
    // omits private/protected member names, so PropertyValues<T>'s generic
    // `has<K extends keyof T>` rejects the literal even though the field is
    // real. Reading it through the plain Map shape it structurally is
    // sidesteps that without losing type-checking for the public keys below.
    const changedKeys = changed as unknown as Map<PropertyKey, unknown>;
    if (changed.has('open') && this.open) {
      // Fresh open: reset transient input/selection state from any prior open.
      this.queryText = '';
      this.manualSelection = false;
      this.committed = false;
    }
    if (changedKeys.has('queryText') || changed.has('groups') || changed.has('open')) {
      const ranked = this.rankedPaletteCandidates;
      this.reconcileActiveId(ranked, changedKeys.has('queryText'));
    }
  }

  private handlePaletteQueryInput(e: InputEvent): void {
    this.queryText = (e.target as HTMLInputElement).value;
  }

  private handlePaletteCompositionStart(): void {
    this.composing = true;
  }

  private handlePaletteCompositionEnd(): void {
    this.composing = false;
  }

  private moveActive(delta: 1 | -1): void {
    const ranked = this.rankedPaletteCandidates;
    if (ranked.length === 0) return;
    const currentIndex = ranked.findIndex((r) => r.candidate.id === this.activeId);
    const nextIndex =
      currentIndex === -1
        ? delta === 1
          ? 0
          : ranked.length - 1
        : (currentIndex + delta + ranked.length) % ranked.length;
    this.manualSelection = true;
    this.activeId = ranked[nextIndex].candidate.id;
    this.scrollActivePaletteOptionIntoView();
  }

  private scrollActivePaletteOptionIntoView(): void {
    requestAnimationFrame(() => {
      const active = this.shadowRoot?.querySelector('.palette-option.active');
      active?.scrollIntoView({ block: 'nearest' });
    });
  }

  private commitActivePaletteCandidate(): void {
    if (this.committed) return;
    const ranked = this.rankedPaletteCandidates;
    const active = ranked.find((r) => r.candidate.id === this.activeId);
    if (!active) return;
    this.committed = true;
    this.dispatchEvent(
      new CustomEvent<{ target: PaletteTarget }>('palette-select', {
        detail: { target: active.candidate.target },
        bubbles: true,
        composed: true,
      })
    );
  }

  private dismissPalette(reason: PaletteDismissReason): void {
    this.dispatchEvent(
      new CustomEvent<{ reason: PaletteDismissReason }>('palette-dismiss', {
        detail: { reason },
        bubbles: true,
        composed: true,
      })
    );
  }

  private retryPaletteGroup(group: PaletteGroup): void {
    this.dispatchEvent(
      new CustomEvent<{ group: PaletteGroup }>('palette-retry', {
        detail: { group },
        bubbles: true,
        composed: true,
      })
    );
  }

  /**
   * Keydown at the query input. Tab/Shift+Tab must both preventDefault() and
   * stopPropagation() — Shoelace's document Tab trap ignores preventDefault
   * alone (verified in Chromium). With a single populated group in
   * Phase 1, Tab/Shift+Tab reselects that group's first match and keeps
   * focus in the input rather than escaping to Shoelace's trap; later phases
   * cycle across the other populated groups here.
   */
  private handlePaletteKeydown(e: KeyboardEvent): void {
    switch (e.key) {
      case 'Tab': {
        e.preventDefault();
        e.stopPropagation();
        const ranked = this.rankedPaletteCandidates;
        if (ranked.length === 0) {
          this.activeId = null;
          return;
        }
        this.manualSelection = true;
        this.activeId = ranked[0].candidate.id;
        return;
      }
      case 'ArrowDown':
        e.preventDefault();
        this.moveActive(1);
        return;
      case 'ArrowUp':
        e.preventDefault();
        this.moveActive(-1);
        return;
      case 'Enter':
        if (this.composing || e.isComposing) return;
        e.preventDefault();
        if (e.repeat) return;
        this.commitActivePaletteCandidate();
        return;
      case 'Escape':
        // sl-dialog handles Escape itself via sl-request-close; nothing to
        // do here beyond letting the keydown continue to bubble to it.
        return;
      default:
        return;
    }
  }

  private handlePaletteInitialFocus(e: Event): void {
    // Bound directly on the palette's own sl-dialog: a nested Shoelace
    // dialog/drawer added inside the palette later could otherwise bubble
    // its own sl-initial-focus here and steal focus back to the query input.
    if (e.target !== e.currentTarget) return;
    e.preventDefault();
    void this.updateComplete.then(() => {
      this.paletteInputEl?.focus();
    });
  }

  private handlePaletteRequestClose(
    e: CustomEvent<{ source: 'close-button' | 'keyboard' | 'overlay' }>
  ): void {
    // Same reasoning as handlePaletteInitialFocus above — only the owned
    // dialog's own sl-request-close should dismiss the palette.
    if (e.target !== e.currentTarget) return;
    const source = e.detail?.source;
    const reason: PaletteDismissReason =
      source === 'keyboard' ? 'escape' : source === 'overlay' ? 'backdrop' : 'close';
    this.dismissPalette(reason);
  }

  private renderHighlighted(text: string, ranges: HighlightRange[]): TemplateResult {
    if (ranges.length === 0) return html`${text}`;
    const parts: unknown[] = [];
    let cursor = 0;
    for (const range of ranges) {
      if (range.start > cursor) parts.push(text.slice(cursor, range.start));
      parts.push(html`<mark>${text.slice(range.start, range.end)}</mark>`);
      cursor = range.end;
    }
    if (cursor < text.length) parts.push(text.slice(cursor));
    return html`${parts}`;
  }

  private renderPaletteGroup(group: PaletteGroup, label: string): unknown {
    const state = this.groups[group];
    if (!state) return nothing;

    const ranked = this.rankedPaletteCandidates.filter((r) => r.candidate.group === group);

    return html`
      <div role="group" aria-labelledby="palette-heading-${group}">
        <div id="palette-heading-${group}" class="palette-group-heading">${label}</div>
        ${state.status === 'loading' && state.candidates.length === 0
          ? html`<div class="palette-loading">Loading…</div>`
          : nothing}
        ${state.status === 'error'
          ? html`
              <div class="palette-group-error">
                ${state.error || 'Failed to load.'}
                <sl-button size="small" @click=${() => this.retryPaletteGroup(group)}
                  >Retry</sl-button
                >
              </div>
            `
          : nothing}
        ${state.status !== 'loading' && state.status !== 'error' && ranked.length === 0
          ? html`<div class="palette-empty">No matches</div>`
          : nothing}
        ${ranked.map(
          (r) => html`
            <div
              id=${this.domIdFor(r.candidate.id)}
              role="option"
              aria-selected=${r.candidate.id === this.activeId}
              class="palette-option ${r.candidate.id === this.activeId ? 'active' : ''}"
              @click=${() => {
                this.manualSelection = true;
                this.activeId = r.candidate.id;
                this.commitActivePaletteCandidate();
              }}
            >
              <div>
                ${r.highlightField === 'label'
                  ? this.renderHighlighted(r.candidate.label, r.highlight)
                  : r.candidate.label}
              </div>
              ${r.candidate.secondaryLabel
                ? html`<div class="palette-secondary">
                    ${r.highlightField === 'secondaryLabel'
                      ? this.renderHighlighted(r.candidate.secondaryLabel, r.highlight)
                      : r.candidate.secondaryLabel}
                  </div>`
                : nothing}
            </div>
          `
        )}
      </div>
    `;
  }

  private renderPalette() {
    const activeDomId = this.activeId ? this.domIdFor(this.activeId) : undefined;
    const agentsState = this.groups.agents;
    const matchCount = this.rankedPaletteCandidates.length;
    const statusText =
      agentsState?.status === 'loading'
        ? 'Loading agents…'
        : agentsState?.status === 'error'
          ? 'Agents failed to load.'
          : `${matchCount} matching ${matchCount === 1 ? 'agent' : 'agents'}`;

    return html`
      <sl-dialog
        class="palette-dialog"
        label="Quick switcher"
        ?open=${this.open}
        @sl-initial-focus=${this.handlePaletteInitialFocus}
        @sl-request-close=${this.handlePaletteRequestClose}
      >
        <div class="palette-input-row">
          <sl-icon name="search"></sl-icon>
          <input
            id="palette-query-input"
            type="text"
            role="combobox"
            aria-autocomplete="list"
            aria-expanded="true"
            aria-controls="palette-result-list"
            aria-activedescendant=${activeDomId ?? nothing}
            aria-describedby="palette-keyboard-help"
            placeholder="Search agents…"
            .value=${this.queryText}
            autocomplete="off"
            @input=${this.handlePaletteQueryInput}
            @keydown=${this.handlePaletteKeydown}
            @compositionstart=${this.handlePaletteCompositionStart}
            @compositionend=${this.handlePaletteCompositionEnd}
          />
        </div>
        <div id="palette-result-list" role="listbox" class="palette-results" aria-label="Results">
          ${this.renderPaletteGroup('agents', 'Agents')}
        </div>
        <div id="palette-keyboard-help" class="palette-help">
          <span><kbd>↑↓</kbd> navigate</span>
          <span><kbd>↵</kbd> open</span>
          <span><kbd>esc</kbd> close</span>
        </div>
        <div class="palette-status" role="status" aria-live="polite">${statusText}</div>
      </sl-dialog>
    `;
  }

  override render() {
    if (this.paletteMode) {
      return this.renderPalette();
    }
    const items = this.filtered;
    return html`
      <div class="overlay" @click=${this.handleOverlayClick} @keydown=${this.handleKeydown}>
        <div class="panel">
          <div class="search-row">
            <sl-icon name="search"></sl-icon>
            <input
              id="switcher-input"
              type="text"
              placeholder="Search conversations..."
              .value=${this.searchTerm}
              @input=${this.handleInput}
              autocomplete="off"
            />
          </div>
          <div class="results">
            ${items.length === 0
              ? html`<div class="empty">
                  ${this.searchTerm ? 'No matching conversations' : 'No conversations'}
                </div>`
              : items.map(
                  (item, i) => html`
                    <div
                      class="item ${i === this.selectedIndex ? 'selected' : ''}"
                      @click=${() => this.selectConversation(item)}
                      @mouseenter=${() => {
                        this.selectedIndex = i;
                      }}
                    >
                      <div class="item-name">
                        ${item.name} ${item.isDM ? html`<span class="dm-badge">DM</span>` : nothing}
                      </div>
                      <div class="item-context">${item.spaceName}</div>
                    </div>
                  `
                )}
          </div>
          <div class="shortcut-hint">
            <span><kbd>↑↓</kbd> navigate</span>
            <span><kbd>↵</kbd> select</span>
            <span><kbd>esc</kbd> close</span>
          </div>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-switcher': ScionChatSwitcher;
  }
}
