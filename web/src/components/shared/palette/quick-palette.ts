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
 * Surface-neutral quick command palette component (Cmd/Ctrl-K and friends).
 *
 * Renders a labeled sl-dialog with a combobox query input and up to four
 * grouped result regions (Agents, Threads, People, Documents) — only the
 * groups a caller actually supplies via `groups` are rendered, so a host
 * that only ever populates `agents` gets a single-group palette for free —
 * one global keyboard model across every rendered group, and typed
 * selection/dismiss/retry events. It makes no API calls and does not
 * navigate itself — the host supplies `groups` (and, for its own copy,
 * `label`/`placeholder`) and reacts to its events.
 *
 * Shared by every host that needs a quick-jump palette (chat's full
 * Agents/Threads/People/Documents switcher today; an agents-only "Jump to
 * agent" palette in the terminal view) — do not fork or copy this
 * component for a new surface; configure it instead.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, state, query } from 'lit/decorators.js';

import {
  PALETTE_GROUP_ORDER,
  type GroupState,
  type PaletteCandidate,
  type PaletteDismissReason,
  type PaletteGroup,
  type PaletteTarget,
} from '../../../client/chat-palette-types.js';
import {
  rankCandidates,
  type HighlightRange,
  type RankedCandidate,
} from '../../../utils/chat-palette-match.js';
import { TouchPrimaryController } from '../../../utils/input-modality.js';
import { PaletteTypeahead } from './palette-typeahead.js';

/** Rows shown per group before "show more". */
const PALETTE_GROUP_VISIBLE_LIMIT = 10;

/** Display heading for each group, in the same reading order as {@link PALETTE_GROUP_ORDER}. */
const PALETTE_GROUP_LABELS: Record<PaletteGroup, string> = {
  agents: 'Agents',
  threads: 'Threads',
  people: 'People',
  documents: 'Documents',
};

/** Singular/plural noun for the status region's match-count text (e.g. "1 matching agent" / "2 matching agents"). */
const PALETTE_GROUP_NOUN: Record<PaletteGroup, { singular: string; plural: string }> = {
  agents: { singular: 'agent', plural: 'agents' },
  threads: { singular: 'thread', plural: 'threads' },
  people: { singular: 'person', plural: 'people' },
  documents: { singular: 'document', plural: 'documents' },
};

@customElement('scion-quick-palette')
export class ScionQuickPalette extends LitElement {
  /** Whether the palette dialog is open. */
  @property({ type: Boolean }) open = false;

  /**
   * The dialog's own accessible label (Shoelace `sl-dialog`'s `label`),
   * read by assistive tech when the palette opens. Each host passes copy
   * that names what its palette finds.
   */
  @property({ type: String }) label = 'Quick switcher';

  /** Placeholder text for the query input. Each host passes copy that names what it searches. */
  @property({ type: String }) placeholder = 'Search…';

  /** Per-group load state, keyed by {@link PaletteGroup}. */
  @property({ attribute: false })
  groups: Partial<Record<PaletteGroup, GroupState>> = {
    agents: { status: 'loading', candidates: [] },
  };

  /**
   * The host's type-ahead, when the host starts capturing keys at its open
   * request (before this element exists, on a first open). The host starts
   * and stops it; the palette only stops it when removed from the page.
   * Without one the palette uses its own, started when `open` turns true and
   * stopped when it turns false. Either way the captured text becomes the
   * query once the input has focus.
   */
  @property({ attribute: false }) typeahead: PaletteTypeahead | null = null;

  /** The palette's own type-ahead, for a host that passes none. */
  private readonly ownTypeahead = new PaletteTypeahead();

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
  /**
   * Groups currently expanded past {@link PALETTE_GROUP_VISIBLE_LIMIT} rows —
   * by explicit "show more", or automatically to keep the active option
   * mounted after Up/Down/Tab navigation or a background refresh selects a
   * candidate beyond the visible cap. Reset to empty on open and on every
   * query edit, alongside `manualSelection`.
   */
  @state() private expandedGroups: ReadonlySet<PaletteGroup> = new Set();

  @query('#palette-query-input')
  private paletteInputEl?: HTMLInputElement;

  /** Whether the device's primary pointer is touch — drives the keyboard-affordance omissions in {@link renderPalette} below. */
  private touchPrimary = new TouchPrimaryController(this);

  static override styles = [
    css`
      :host {
        /* Combined height of the dialog's own chrome around the results
           list at narrow widths: the input row, its border, the dialog's
           own top/bottom margins, and -- where shown -- the keyboard-help
           legend below the results. Measured empirically against this
           exact layout: ~111px (6.9375rem) with the legend hidden (touch),
           ~137px (8.5625rem) with it shown (a narrow but non-touch window,
           since the legend is gated on touch, not on width). Rounded up
           from the larger of the two with a small safety margin, as a named
           property rather than a bare number in the max-height rule below,
           so a future chrome change has one place to update. */
        --palette-chrome-height: 9rem;

        /* Type scale: a dense default. A host with its own type scale
           sets these on the element to scale the palette with its page. */
        --palette-fs-xs: 0.625rem;
        --palette-fs-sm: 0.6875rem;
        --palette-fs-base: 0.75rem;
        --palette-fs-xl: 0.9375rem;
      }

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
        font-size: var(--palette-fs-xl);
        background: transparent;
        color: var(--scion-text, #1e293b);
        padding: 0.25rem 0;
      }

      /* Stop iOS/Android focus-zoom: this is a native <input>, so it is not
         covered by the app-wide --sl-input-font-size-* rule, and a coarse
         pointer isn't limited to narrow viewports (an iPad is coarse-pointer
         at any width). */
      @media (pointer: coarse) {
        #palette-query-input {
          font-size: max(16px, var(--chat-fs-xl));
        }
      }

      .palette-results {
        max-height: 50vh;
        overflow-y: auto;
        padding-bottom: 0.5rem;
        /* Two-column grid in reading order (Agents, Threads, People,
         * Documents) — the groups are rendered in that order as siblings, so
         * a plain row-major grid places Agents/Threads on the first row and
         * People/Documents on the second without extra markup. */
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: 0 0.75rem;
      }

      /* A single group (e.g. an agents-only host) gets the full width
       * instead of the left column of an otherwise empty grid. */
      .palette-results.single-group {
        grid-template-columns: 1fr;
      }

      @media (max-width: 768px) {
        .palette-dialog::part(panel) {
          /* Near-full-width and top-anchored (see ::part(base) below): a
             centred panel puts the lower results under the iOS on-screen
             keyboard, which anchoring at the top avoids. Shoelace's own
             dialog part sets max-width to calc(100% - var(--sl-spacing-2x-large))
             (2.25rem, i.e. 36px, by default) on this same part, which
             otherwise clamps width below well before 100vw - 1rem at narrow
             widths (354px, not 374px, at a 390px viewport) -- this rule
             needs its own max-width to actually win that clamp, not just a
             width.
             Specificity is not what decides this either way: per CSS
             Cascade 4's "Context" rule, a ::part() declaration from this
             outer tree always wins over the shadow tree's own styles for a
             given property, regardless of either rule's specificity or
             source order -- which is exactly why plain width above already
             took effect on its own. max-width only needed its own explicit
             declaration here because the clamp comes from that same
             property, not because of any specificity contest. */
          width: calc(100vw - 1rem);
          max-width: calc(100vw - 1rem);
          margin-top: max(env(safe-area-inset-top), 0.5rem);
        }

        .palette-dialog::part(base) {
          align-items: flex-start;
        }

        .palette-results {
          grid-template-columns: 1fr;
          /* Neither vh nor dvh shrinks for the iOS keyboard, so a fixed 50vh
             here would leave the bottom rows hidden under it --
             --palette-vvh (set from window.visualViewport's own height while
             open, see _onVisualViewportResize) does shrink.
             --palette-chrome-height (defined on :host above) is the
             dialog's own header, input row and margins. Falls back to
             100dvh when visualViewport isn't available at all, so this
             never regresses to an unbounded scroller. */
          max-height: calc(var(--palette-vvh, 100dvh) - var(--palette-chrome-height));
          overscroll-behavior: contain;
        }
      }

      /* Tap targets: at least 44x44 for every interactive row/button, per
         Apple/WCAG touch-target guidance — not needed on desktop, where
         pointer precision makes the smaller default sizing fine. */
      @media (hover: none) and (pointer: coarse) {
        .palette-option {
          /* .palette-option's own padding (0.5rem top+bottom = 16px) is
             added on top of min-height under the default content-box
             sizing, inflating every row to 60px instead of the intended
             44px minimum — border-box folds the padding back into that
             44px instead of adding to it. */
          box-sizing: border-box;
          min-height: 44px;
          justify-content: center;
        }

        .palette-group-cell sl-button::part(base) {
          min-height: 44px;
        }
      }

      .palette-group-heading {
        font-size: var(--palette-fs-sm);
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
       * nothing but appearance. Scoped to hover-capable devices: on touch,
       * :hover sticks after a tap until the next tap lands elsewhere, which
       * would otherwise leave a stale highlighted row.
       */
      @media (hover: hover) {
        .palette-option:hover:not(.active) {
          background: var(--scion-bg-subtle, #f8fafc);
        }
      }

      .palette-option mark {
        background: none;
        color: inherit;
        font-weight: 700;
        padding: 0;
      }

      .palette-secondary {
        font-size: var(--palette-fs-base);
        /* Not --scion-text-muted: that token (neutral-500 in light mode)
           fails WCAG AA (4.34:1) against the selected row's
           --scion-bg-subtle background. --scion-text-secondary is a
           theme-aware token already darker in light mode and lighter in
           dark mode than --scion-text-muted, clearing AA against
           --scion-bg-subtle in both themes (a raw --scion-neutral-600
           value would pass in light mode but fail badly in dark mode,
           where that scale point is a dark gray on a dark background). */
        color: var(--scion-text-secondary, #475569);
      }

      .palette-empty,
      .palette-empty-overall,
      .palette-loading,
      .palette-group-error,
      .palette-group-incomplete {
        padding: 0.5rem 1rem 0.75rem;
        color: var(--scion-text-muted, #94a3b8);
        font-size: var(--palette-fs-base);
      }

      .palette-group-error sl-button,
      .palette-group-incomplete sl-button {
        margin-top: 0.25rem;
      }

      .palette-show-more {
        padding: 0.25rem 1rem 0.5rem;
      }

      .palette-help {
        padding: 0.375rem 1rem;
        font-size: var(--palette-fs-sm);
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
        font-size: var(--palette-fs-xs);
        background: var(--scion-bg-subtle, #f1f5f9);
        /* Not the surrounding .palette-help's inherited --scion-text-muted:
           that token fails WCAG AA against this chip's own
           --scion-bg-subtle background in light mode. --scion-text-secondary
           is theme-aware (darker in light mode, lighter in dark mode) and
           clears AA against --scion-bg-subtle in both — see
           .palette-secondary's own comment above for why a raw
           --scion-neutral-600 value specifically would not. */
        color: var(--scion-text-secondary, #475569);
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
  ];

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
      this.setActiveId(ranked[0]?.candidate.id ?? null);
      return;
    }
    if (this.manualSelection && this.activeId !== null) {
      const stillPresent = ranked.some((r) => r.candidate.id === this.activeId);
      if (stillPresent) {
        // A background refresh (new candidates loaded) may have moved this
        // id past the visible cap in its group — keep it mounted *and*
        // actually visible, not just present in the DOM: preserving a manual
        // selection by ID is a promise about what the user sees, not only
        // about what's technically rendered off-screen.
        this.ensureGroupExpandedFor(this.activeId);
        this.scrollActivePaletteOptionIntoView();
        return;
      }
    }
    this.setActiveId(ranked[0]?.candidate.id ?? null);
  }

  override willUpdate(changed: PropertyValues<ScionQuickPalette>): void {
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
      this.expandedGroups = new Set();
    }
    if (changed.has('open')) {
      if (this.open) {
        this._startVisualViewportTracking();
        // Keys typed until the input takes focus belong to the query. A
        // host's type-ahead is the host's to start: it may already have
        // captured text, and may have stopped at its time limit, keeping it.
        if (!this.typeahead) this.ownTypeahead.start();
      } else {
        this._stopVisualViewportTracking();
        // Only the palette's own: the host stops its type-ahead when it
        // closes the palette, and may already have started it again for a
        // reopen by the time this update runs.
        this.ownTypeahead.stop();
      }
    }
    if (changedKeys.has('queryText')) {
      // A query edit resets manual navigation and any show-more expansion —
      // the new match set starts back at each group's first 10 rows.
      this.expandedGroups = new Set();
    }
    if (changedKeys.has('queryText') || changed.has('groups') || changed.has('open')) {
      const ranked = this.rankedPaletteCandidates;
      this.reconcileActiveId(ranked, changedKeys.has('queryText'));
    }
  }

  override connectedCallback(): void {
    super.connectedCallback();
    // A reconnect while `open` is already `true` (its value unchanged
    // across the disconnect/reconnect) never flips `willUpdate`'s own
    // `changed.has('open')` check, so that path alone would never restart
    // tracking here even though `disconnectedCallback` always tears it
    // down. Not reachable today (the switcher is never moved/reparented
    // while open), but a future change that does so should not silently
    // lose vvh tracking.
    if (this.open) this._startVisualViewportTracking();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this._stopVisualViewportTracking();
    this.activeTypeahead.stop();
  }

  /** The type-ahead in use: the host's, else the palette's own. */
  private get activeTypeahead(): PaletteTypeahead {
    return this.typeahead ?? this.ownTypeahead;
  }

  /**
   * Neither `vh` nor `dvh` shrinks when the iOS on-screen keyboard opens, so
   * a `50vh`-based results scroller would keep the lower rows hidden under
   * it. `window.visualViewport` does shrink, so its height is mirrored onto
   * `--palette-vvh` on this host while open, which the narrow-viewport
   * results sizing (see styles) consumes instead of a fixed viewport unit.
   * Desktop never sets this custom property, so its own `50vh` rule is
   * unaffected.
   */
  private readonly _onVisualViewportResize = (): void => {
    const vv = window.visualViewport;
    if (!vv) return;
    this.style.setProperty('--palette-vvh', `${vv.height}px`);
  };

  private _startVisualViewportTracking(): void {
    const vv = window.visualViewport;
    if (!vv) return;
    vv.addEventListener('resize', this._onVisualViewportResize);
    this._onVisualViewportResize();
  }

  private _stopVisualViewportTracking(): void {
    const vv = window.visualViewport;
    if (!vv) return;
    vv.removeEventListener('resize', this._onVisualViewportResize);
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

  /**
   * Set the active candidate, expanding its group's "show more" cap first if
   * needed so the option being activated is always actually mounted in the
   * DOM before anything (e.g. scroll-into-view) tries to find it.
   */
  private setActiveId(id: string | null): void {
    if (id) this.ensureGroupExpandedFor(id);
    this.activeId = id;
  }

  /** Expand `candidateId`'s group past the visible cap if it isn't already fully shown. */
  private ensureGroupExpandedFor(candidateId: string): void {
    const ranked = this.rankedPaletteCandidates;
    const active = ranked.find((r) => r.candidate.id === candidateId);
    if (!active) return;
    const group = active.candidate.group;
    if (this.expandedGroups.has(group)) return;
    const indexInGroup = ranked
      .filter((r) => r.candidate.group === group)
      .findIndex((r) => r.candidate.id === candidateId);
    if (indexInGroup >= PALETTE_GROUP_VISIBLE_LIMIT) {
      this.expandGroup(group);
    }
  }

  private expandGroup(group: PaletteGroup): void {
    if (this.expandedGroups.has(group)) return;
    this.expandedGroups = new Set(this.expandedGroups).add(group);
  }

  /** The non-empty (has at least one ranked match) groups, in reading order. */
  private nonEmptyPaletteGroups(ranked: Array<RankedCandidate<PaletteCandidate>>): PaletteGroup[] {
    const present = new Set(ranked.map((r) => r.candidate.group));
    return PALETTE_GROUP_ORDER.filter((g) => present.has(g));
  }

  /**
   * True once every present group has finished loading and none of them
   * matched anything. A `role=listbox` requires at least one real
   * `role=option`/`role=group` descendant (WAI-ARIA's required-owned-elements
   * rule); each individual group's own "No matches" text is a plain,
   * unroled placeholder, so a query (or an empty index) that matches nothing
   * anywhere would otherwise leave the listbox with none. This is checked
   * only once every present group is done loading, so the placeholder never
   * claims "no results" while a group is still in flight.
   */
  private paletteHasNoResultsAnywhere(): boolean {
    if (this.rankedPaletteCandidates.length > 0) return false;
    return PALETTE_GROUP_ORDER.every((g) => this.groups[g]?.status !== 'loading');
  }

  /**
   * IDs the listbox owns via `aria-owns`, in reading order. The listbox
   * element itself has no DOM children (see `renderPalette`) — each group's
   * `role=group` region lives inside its own grid cell alongside its
   * retry/show-more controls, which `aria-owns` cannot be used to select
   * around, so the region is reattached to the listbox this way instead.
   */
  private paletteListboxOwnedIds(): string {
    const ids: string[] = PALETTE_GROUP_ORDER.filter((g) => this.groups[g]).map((g) =>
      this.paletteGroupRegionId(g)
    );
    if (this.paletteHasNoResultsAnywhere()) ids.push('palette-empty-overall');
    return ids.join(' ');
  }

  /**
   * Up/Down: move to the previous/next row *within the active candidate's
   * group*, wrapping. If nothing is active (or the active id no longer
   * matches any ranked candidate), choose the first/last row of the first
   * nonempty group in reading order.
   */
  private moveActive(delta: 1 | -1): void {
    const ranked = this.rankedPaletteCandidates;
    if (ranked.length === 0) return;
    const active = ranked.find((r) => r.candidate.id === this.activeId);

    let groupRanked: Array<RankedCandidate<PaletteCandidate>>;
    let currentIndex: number;
    if (active) {
      groupRanked = ranked.filter((r) => r.candidate.group === active.candidate.group);
      currentIndex = groupRanked.findIndex((r) => r.candidate.id === this.activeId);
    } else {
      const [firstGroup] = this.nonEmptyPaletteGroups(ranked);
      groupRanked = firstGroup ? ranked.filter((r) => r.candidate.group === firstGroup) : [];
      currentIndex = -1;
    }
    if (groupRanked.length === 0) return;

    const nextIndex =
      currentIndex === -1
        ? delta === 1
          ? 0
          : groupRanked.length - 1
        : (currentIndex + delta + groupRanked.length) % groupRanked.length;
    this.manualSelection = true;
    this.setActiveId(groupRanked[nextIndex].candidate.id);
    this.scrollActivePaletteOptionIntoView();
  }

  /**
   * Tab/Shift+Tab: select the first (best-ranked) match of the next/previous
   * nonempty group in reading order, wrapping — a group that has no matches
   * right now is skipped, but "joins the next traversal" automatically once
   * it does, since this recomputes the nonempty-group list fresh on every
   * press rather than caching it. With no matches anywhere, the event is
   * still consumed (via the caller's preventDefault/stopPropagation) but
   * selection is left empty.
   */
  private moveActiveGroup(delta: 1 | -1): void {
    const ranked = this.rankedPaletteCandidates;
    const groups = this.nonEmptyPaletteGroups(ranked);
    if (groups.length === 0) {
      this.setActiveId(null);
      return;
    }
    const active = ranked.find((r) => r.candidate.id === this.activeId);
    const currentGroupIndex = active ? groups.indexOf(active.candidate.group) : -1;
    const nextGroupIndex =
      currentGroupIndex === -1
        ? delta === 1
          ? 0
          : groups.length - 1
        : (currentGroupIndex + delta + groups.length) % groups.length;
    const nextGroup = groups[nextGroupIndex];
    const firstInGroup = ranked.find((r) => r.candidate.group === nextGroup);
    this.manualSelection = true;
    this.setActiveId(firstInGroup?.candidate.id ?? null);
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
   * alone — and select the first match of the next/previous *nonempty* group
   * in reading order, wrapping and skipping empty groups, without ever
   * escaping the modal: Tab is a group-selection key while the input has
   * focus, not a focus-trap escape.
   */
  private handlePaletteKeydown(e: KeyboardEvent): void {
    switch (e.key) {
      case 'Tab': {
        e.preventDefault();
        e.stopPropagation();
        this.moveActiveGroup(e.shiftKey ? -1 : 1);
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
    void this.updateComplete.then(() => this.focusQueryInput());
  }

  /** Focuses the query input, which applies any type-ahead (see {@link applyTypeahead}). */
  private focusQueryInput(): void {
    const input = this.paletteInputEl;
    if (!this.open || !input) return;
    input.focus();
    // Already focused: no focus event, so apply it here.
    this.applyTypeahead();
  }

  private readonly handleQueryInputFocus = (): void => {
    this.applyTypeahead();
  };

  /**
   * Once the query input has focus, by the open or by a click, the keys
   * typed since the open was requested become the query. The query is empty
   * then: an open clears it, and keys typed before the focus were captured.
   */
  private applyTypeahead(): void {
    // A focus while closed (the closing dialog's input) must leave a capture
    // a host has started for a reopen running.
    if (!this.open) return;
    const typed = this.activeTypeahead.take();
    if (typed) this.queryText = typed;
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

  /** The `role=group` element's own DOM ID for {@link group} — referenced by the listbox's `aria-owns` (see `renderPalette`), since this element is not one of the listbox's own DOM children (see renderPaletteGroup's doc comment below). */
  private paletteGroupRegionId(group: PaletteGroup): string {
    return `palette-group-region-${group}`;
  }

  /**
   * One grid cell: a plain (unroled) wrapper around the group's `role=group`
   * region (heading, loading/empty text, and `role=option` rows only — the
   * content WAI-ARIA's listbox permits) plus, as its *siblings* rather than
   * its descendants, the group's retry/incomplete/show-more controls.
   * `role=listbox`/`role=group` forbid an interactive `role=button`
   * descendant anywhere in their subtree (axe's aria-required-children
   * flags it even nested several levels down through unroled divs), so
   * those controls must sit outside the `role=group` element entirely — see
   * `renderPalette`'s `aria-owns`, which reattaches the (DOM-external)
   * region to the listbox for assistive tech despite this split. The wrapper
   * keeps them all stacked in one visual reading order regardless.
   */
  private renderPaletteGroup(group: PaletteGroup): unknown {
    const state = this.groups[group];
    if (!state) return nothing;
    const label = PALETTE_GROUP_LABELS[group];

    const ranked = this.rankedPaletteCandidates.filter((r) => r.candidate.group === group);
    const expanded = this.expandedGroups.has(group);
    const visible = expanded ? ranked : ranked.slice(0, PALETTE_GROUP_VISIBLE_LIMIT);
    const hiddenCount = ranked.length - visible.length;

    return html`
      <div class="palette-group-cell" data-palette-group=${group}>
        <div
          id=${this.paletteGroupRegionId(group)}
          role="group"
          aria-labelledby="palette-heading-${group}"
        >
          <div id="palette-heading-${group}" class="palette-group-heading">${label}</div>
          ${state.status === 'loading' && state.candidates.length === 0
            ? html`<div class="palette-loading">Loading…</div>`
            : nothing}
          ${state.status === 'loading' && state.candidates.length > 0 && ranked.length === 0
            ? html`<div class="palette-loading">Loading more…</div>`
            : nothing}
          ${state.status !== 'loading' && state.status !== 'error' && ranked.length === 0
            ? html`<div class="palette-empty">No matches</div>`
            : nothing}
          ${visible.map(
            (r) => html`
              <div
                id=${this.domIdFor(r.candidate.id)}
                role="option"
                aria-selected=${r.candidate.id === this.activeId}
                class="palette-option ${r.candidate.id === this.activeId ? 'active' : ''}"
                @click=${() => {
                  this.manualSelection = true;
                  this.setActiveId(r.candidate.id);
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
        ${state.status === 'ready' && state.incomplete
          ? html`
              <div class="palette-group-incomplete">
                Some results couldn't load.
                <sl-button size="small" @click=${() => this.retryPaletteGroup(group)}
                  >Retry</sl-button
                >
              </div>
            `
          : nothing}
        ${hiddenCount > 0
          ? html`
              <div class="palette-show-more">
                <sl-button size="small" @click=${() => this.expandGroup(group)}
                  >Show ${hiddenCount} more</sl-button
                >
              </div>
            `
          : nothing}
      </div>
    `;
  }

  /**
   * The status region's announced text, read through a polite live region on
   * loading/count/active-group changes. The single-group wording below is
   * used whenever exactly one group is present (e.g. a host that only
   * supplies Agents). Multiple populated groups get a combined summary
   * instead of picking one group arbitrarily.
   */
  private paletteStatusText(): string {
    const presentGroups = PALETTE_GROUP_ORDER.filter((g) => this.groups[g]);
    const matchCount = this.rankedPaletteCandidates.length;

    if (presentGroups.length === 1) {
      const group = presentGroups[0];
      const state = this.groups[group]!;
      const noun = PALETTE_GROUP_NOUN[group];
      if (state.status === 'loading') return `Loading ${noun.plural}…`;
      if (state.status === 'error') return `${PALETTE_GROUP_LABELS[group]} failed to load.`;
      return `${matchCount} matching ${matchCount === 1 ? noun.singular : noun.plural}`;
    }

    // Not gated on `candidates.length === 0`: a group can be `loading` with
    // partial (progressively-published) candidates already in — e.g.
    // Agents mid-pagination — and still needs to announce as loading, since
    // its current match count (for a query that only matches a page not in
    // yet) can otherwise misreport as a final "0 matching results" that
    // reads as "this doesn't exist" rather than "still loading".
    const loadingGroups = presentGroups.filter((g) => this.groups[g]!.status === 'loading');
    if (loadingGroups.length === presentGroups.length) {
      return 'Loading…';
    }
    const erroredGroups = presentGroups.filter((g) => this.groups[g]!.status === 'error');
    const parts = [`${matchCount} matching ${matchCount === 1 ? 'result' : 'results'}`];
    if (loadingGroups.length > 0) {
      parts.push(`${loadingGroups.map((g) => PALETTE_GROUP_LABELS[g]).join(', ')} still loading`);
    }
    if (erroredGroups.length > 0) {
      parts.push(`${erroredGroups.map((g) => PALETTE_GROUP_LABELS[g]).join(', ')} failed to load`);
    }
    return parts.join('; ');
  }

  private renderPalette() {
    const activeDomId = this.activeId ? this.domIdFor(this.activeId) : undefined;
    const presentGroupCount = PALETTE_GROUP_ORDER.filter((g) => this.groups[g]).length;
    const singleGroup = presentGroupCount === 1;

    return html`
      <sl-dialog
        class="palette-dialog"
        label=${this.label}
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
            aria-describedby=${this.touchPrimary.isTouch ? nothing : 'palette-keyboard-help'}
            placeholder=${this.placeholder}
            .value=${this.queryText}
            autocomplete="off"
            @input=${this.handlePaletteQueryInput}
            @focus=${this.handleQueryInputFocus}
            @keydown=${this.handlePaletteKeydown}
            @compositionstart=${this.handlePaletteCompositionStart}
            @compositionend=${this.handlePaletteCompositionEnd}
          />
        </div>
        <div
          id="palette-result-list"
          role="listbox"
          aria-label="Results"
          aria-owns=${this.paletteListboxOwnedIds()}
        ></div>
        <div class="palette-results ${singleGroup ? 'single-group' : ''}">
          ${this.renderPaletteGroup('agents')} ${this.renderPaletteGroup('threads')}
          ${this.renderPaletteGroup('people')} ${this.renderPaletteGroup('documents')}
          ${this.paletteHasNoResultsAnywhere()
            ? html`<div
                id="palette-empty-overall"
                class="palette-empty-overall"
                role="option"
                aria-disabled="true"
                aria-selected="false"
              >
                No matching results
              </div>`
            : nothing}
        </div>
        ${this.touchPrimary.isTouch
          ? nothing
          : html`
              <div id="palette-keyboard-help" class="palette-help">
                ${presentGroupCount > 1 ? html`<span><kbd>Tab</kbd> next group</span>` : nothing}
                <span><kbd>↑↓</kbd> navigate</span>
                <span><kbd>↵</kbd> open</span>
                <span><kbd>esc</kbd> close</span>
              </div>
            `}
        <div class="palette-status" role="status" aria-live="polite">
          ${this.paletteStatusText()}
        </div>
      </sl-dialog>
    `;
  }

  override render() {
    return this.renderPalette();
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-quick-palette': ScionQuickPalette;
  }
}
