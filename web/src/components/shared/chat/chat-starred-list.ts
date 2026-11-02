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
 * The conversation header's starred-message button
 * (web.chat_starred_messages): an always-visible star that opens this
 * conversation's stars, most recently starred first, with Load more.
 *
 * - Desktop: an anchored, nonmodal dialog holding a labelled list of links.
 *   It is kept inside the viewport (shifted sideways, its height capped to
 *   the room below the button); Escape closes it and returns focus to the
 *   button; a pointer down or focus moving outside closes it.
 * - Narrow screens (the chat's menu-sheet breakpoint): the same list in a
 *   modal bottom sheet (a native `<dialog>` in the top layer, so no panel
 *   clips it), with a heading, a Close button, 44px targets, safe-area
 *   padding and a scroll region that keeps touch gestures to itself.
 *
 * Each entry is a link with two compact lines: the local saved date and
 * the author, then the start of the excerpt. Choosing one closes the list
 * and fires `starred-message-open` (detail `{ messageId }`); the page
 * resolves the current target, jumps, and moves focus to the message.
 *
 * The list is fetched when it opens and reconciled while it shows: on the
 * user's star invalidation (SSE), on a local star change in this
 * conversation, on reconnect, on returning to the page, and at most once a
 * minute while visible. A reconcile reloads as many rows as are loaded, so
 * Load more pages are not lost, and keeps the rows on screen meanwhile.
 * Nothing is persisted.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { repeat } from 'lit/directives/repeat.js';

import { stateManager } from '../../../client/state.js';
import {
  chatStarsLocalChanges,
  fetchStarPage,
  StarsError,
  VisibleReconciler,
  type StarItem,
} from '../../../client/chat-stars.js';
import { formatInstant } from '../../../utils/time.js';
import { shouldUseMenuSheet } from './context-menu.js';
import { withFocusMove } from '../focus-moved.js';

/** Page size of the header list (first page and each Load more). */
export const STARRED_LIST_PAGE = 20;

/** The largest page the hub serves; a reconcile reloads in pages of this. */
export const STARRED_LIST_MAX_PAGE = 100;

/** The most deleted message IDs the list remembers. */
const DELETED_MAX = 500;

/** Room kept between the desktop panel and the viewport edges, px. */
const PANEL_MARGIN_PX = 8;

/** The desktop panel's height cap: min(24rem, 60vh). */
const PANEL_MAX_REM = 24;

/** The smallest height the desktop panel is squeezed to, px. */
const PANEL_MIN_PX = 120;

/** `detail` of the `starred-message-open` event. */
export interface StarredMessageOpenDetail {
  messageId: string;
}

let panelSeq = 0;

@customElement('scion-chat-starred-list')
export class ScionChatStarredList extends LitElement {
  /** The conversation whose stars are listed. */
  @property() conversationKey = '';

  @state() private open = false;
  /** Shown as the bottom sheet (decided when it opens). */
  @state() private sheet = false;
  @state() private items: StarItem[] = [];
  @state() private nextCursor: string | undefined;
  /** A first page or a reconcile is running. */
  @state() private loading = false;
  @state() private loadingMore = false;
  /** The last first-page or reconcile request failed. */
  @state() private error = '';
  /** The last Load more failed. */
  @state() private moreError = '';

  /**
   * Bumped by a conversation switch and by closing: every request of an
   * older generation is dropped (and aborted).
   */
  private generation = 0;
  private abort: AbortController | null = null;
  private moreAbort: AbortController | null = null;
  /** An invalidation arrived while a reconcile ran: run once more after. */
  private reconcileAgain = false;
  /**
   * Messages deleted while this list is mounted. A page whose request was
   * already in flight when the deletion arrived may still carry the row;
   * every merged page is filtered through this set so it cannot come back.
   * Bounded: the oldest entries are dropped past DELETED_MAX.
   */
  private readonly deleted = new Set<string>();
  private readonly panelId = `starred-panel-${++panelSeq}`;
  private readonly headingId = `${this.panelId}-heading`;
  private readonly reconciler = new VisibleReconciler(() => void this.reconcile());
  /** Where focus goes once the next render lands (Load more, a reconcile). */
  private pendingFocus: { messageId?: string; index: number } | null = null;

  static override styles = css`
    :host {
      position: relative;
      display: inline-flex;
    }

    /* A native button styled as Shoelace's icon button, so its ARIA state
       (aria-expanded, aria-controls) is on the focusable control itself and
       resolves to the panel in the same shadow root. */
    .trigger {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      border: none;
      background: none;
      padding: var(--sl-spacing-x-small, 0.5rem);
      margin: 0;
      border-radius: var(--sl-border-radius-medium, 0.25rem);
      font-size: inherit;
      line-height: 1;
      color: var(--sl-color-neutral-600, #475569);
      cursor: pointer;
      transition: var(--sl-transition-x-fast, 100ms) color;
      -webkit-appearance: none;
    }

    .trigger:hover {
      color: var(--sl-color-primary-600, #2563eb);
    }

    .trigger:active {
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .trigger:focus {
      outline: none;
    }

    .trigger:focus-visible {
      outline: var(--sl-focus-ring, solid 3px rgba(59, 130, 246, 0.4));
      outline-offset: var(--sl-focus-ring-offset, 1px);
    }

    @media (prefers-reduced-motion: reduce) {
      .trigger {
        transition: none;
      }
    }

    /* ---- Desktop: anchored nonmodal panel ---- */
    .panel {
      position: absolute;
      top: calc(100% + 0.25rem);
      right: 0;
      z-index: 50;
      display: flex;
      flex-direction: column;
      box-sizing: border-box;
      width: min(24rem, calc(100vw - 1rem));
      max-height: min(24rem, 60vh);
      background: var(--scion-surface, #fff);
      color: var(--scion-text, #1e293b);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      box-shadow: var(--sl-shadow-large, 0 8px 24px rgba(0, 0, 0, 0.15));
      font-weight: 400;
      text-align: left;
    }

    .panel:focus {
      outline: none;
    }

    /* ---- Narrow screens: modal bottom sheet ---- */
    dialog.sheet {
      position: fixed;
      inset: auto 0 0 0;
      margin: 0;
      width: 100%;
      max-width: 100%;
      max-height: 80vh;
      max-height: 80dvh;
      box-sizing: border-box;
      padding: 0 env(safe-area-inset-right, 0px) env(safe-area-inset-bottom, 0px)
        env(safe-area-inset-left, 0px);
      border: none;
      border-radius: 0.875rem 0.875rem 0 0;
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text, #1e293b);
      box-shadow: 0 -4px 24px rgba(0, 0, 0, 0.18);
      overflow: hidden;
      flex-direction: column;
      font-weight: 400;
      text-align: left;
      /* Only vertical pans: a sideways drag on the sheet must not become a
         browser gesture or reach the chat's panel swipe. */
      touch-action: pan-y pinch-zoom;
    }

    dialog.sheet[open] {
      display: flex;
      animation: sheet-in 0.18s ease-out;
    }

    dialog.sheet::backdrop {
      background: rgba(15, 23, 42, 0.45);
    }

    @keyframes sheet-in {
      from {
        transform: translateY(100%);
      }
      to {
        transform: translateY(0);
      }
    }

    @media (prefers-reduced-motion: reduce) {
      dialog.sheet[open] {
        animation: none;
      }
    }

    /* ---- Shared content ---- */
    .panel-header {
      flex: none;
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.5rem 0.375rem 0.75rem;
    }

    .sheet .panel-header {
      padding: 0.5rem 0.25rem 0.5rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .panel-title {
      flex: 1 1 auto;
      min-width: 0;
      margin: 0;
      font-weight: 600;
      font-size: var(--chat-fs-base, 0.875rem);
    }

    .sort-label {
      flex: none;
      font-size: var(--chat-fs-xs, 0.75rem);
      color: var(--scion-text-muted, #64748b);
    }

    .close {
      flex: none;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 44px;
      height: 44px;
      border: none;
      background: none;
      border-radius: 0.375rem;
      color: var(--scion-text-muted, #64748b);
      font-size: 1.125rem;
      cursor: pointer;
    }

    .close:focus-visible {
      outline: 2px solid var(--scion-primary, #3b82f6);
      outline-offset: -2px;
    }

    .scroll {
      flex: 1 1 auto;
      min-height: 0;
      overflow-y: auto;
      overscroll-behavior: contain;
      /* Restated: touch-action does not carry across a scroll container. */
      touch-action: pan-y pinch-zoom;
      padding: 0 0.5rem 0.5rem;
    }

    .sheet .scroll {
      padding: 0.25rem 0.5rem 0.75rem;
    }

    ul {
      list-style: none;
      margin: 0;
      padding: 0;
    }

    a.entry {
      display: block;
      min-height: 44px;
      box-sizing: border-box;
      padding: 0.375rem 0.5rem;
      border-radius: 0.375rem;
      color: inherit;
      text-decoration: none;
    }

    a.entry:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    a.entry:focus {
      outline: none;
    }

    a.entry:focus-visible {
      background: var(--scion-bg-subtle, #f1f5f9);
      outline: 2px solid var(--scion-primary, #3b82f6);
      outline-offset: -2px;
    }

    .entry-meta {
      display: flex;
      gap: 0.25rem;
      min-width: 0;
      font-size: var(--chat-fs-xs, 0.75rem);
      line-height: 1.35;
      color: var(--scion-text-muted, #64748b);
    }

    .entry-date {
      flex: none;
    }

    .entry-sender {
      min-width: 0;
      overflow: hidden;
      white-space: nowrap;
      text-overflow: ellipsis;
    }

    .entry-excerpt {
      display: -webkit-box;
      -webkit-line-clamp: 2;
      -webkit-box-orient: vertical;
      overflow: hidden;
      font-size: var(--chat-fs-sm, 0.8125rem);
      line-height: 1.35;
      overflow-wrap: anywhere;
    }

    .status {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.25rem 0.5rem;
      padding: 0.75rem 0.5rem;
      font-size: var(--chat-fs-sm, 0.8125rem);
      color: var(--scion-text-muted, #64748b);
    }

    .status.inline {
      padding: 0.375rem 0.5rem;
    }

    .sr-only {
      position: absolute;
      width: 1px;
      height: 1px;
      margin: -1px;
      overflow: hidden;
      clip: rect(0 0 0 0);
      white-space: nowrap;
    }

    .more {
      display: flex;
      justify-content: center;
      padding-top: 0.25rem;
    }

    .sheet .more sl-button::part(base),
    .sheet .status sl-button::part(base) {
      min-height: 44px;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    stateManager.addEventListener('chat-stars-updated', this.handleInvalidate);
    stateManager.addEventListener('chat-message-deleted', this.handleMessageDeleted);
    stateManager.addEventListener('connected', this.handleInvalidate);
    chatStarsLocalChanges.addEventListener('changed', this.handleLocalChange);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('chat-stars-updated', this.handleInvalidate);
    stateManager.removeEventListener('chat-message-deleted', this.handleMessageDeleted);
    stateManager.removeEventListener('connected', this.handleInvalidate);
    chatStarsLocalChanges.removeEventListener('changed', this.handleLocalChange);
    this.stopListening();
    this.reconciler.stop();
    this.generation++;
    this.abort?.abort();
    this.moreAbort?.abort();
  }

  protected override willUpdate(changed: PropertyValues<this>): void {
    if (changed.has('conversationKey') && changed.get('conversationKey') !== undefined) {
      this.closePanel(false);
      this.reset();
    }
  }

  protected override updated(changed: PropertyValues): void {
    super.updated(changed);
    if (changed.has('open') || changed.has('sheet')) this.syncSheet();
    if (this.open && !this.sheet) this.fitPanel();
    if (this.pendingFocus) this.applyPendingFocus();
  }

  /**
   * Put focus back on the star button: the page calls this when an entry
   * chosen from the list could not be opened, since the list closed
   * before the jump began.
   */
  focusTrigger(): void {
    const trigger = this.renderRoot.querySelector<HTMLElement>('.trigger');
    if (trigger) withFocusMove(() => trigger.focus());
  }

  /** Whether the list is open (for the page and tests). */
  get isOpen(): boolean {
    return this.open;
  }

  /** Open or close the list (the button). */
  private toggle(): void {
    if (this.open) {
      this.closePanel(true);
      return;
    }
    this.sheet = shouldUseMenuSheet();
    this.open = true;
    if (!this.sheet) document.addEventListener('pointerdown', this.handleOutsidePointer, true);
    window.addEventListener('resize', this.handleResize);
    window.addEventListener('popstate', this.handlePopState);
    this.reconciler.start();
    void this.reconcile();
    if (!this.sheet) {
      void this.updateComplete.then(() => {
        this.renderRoot.querySelector<HTMLElement>('.panel')?.focus();
      });
    }
  }

  private closePanel(restoreFocus: boolean): void {
    if (!this.open) return;
    this.open = false;
    this.stopListening();
    this.reconciler.stop();
    // Requests of the closed list are not needed; the rows stay for the
    // next opening, which reconciles them at once.
    this.generation++;
    this.abort?.abort();
    this.moreAbort?.abort();
    this.abort = null;
    this.moreAbort = null;
    this.loading = false;
    this.loadingMore = false;
    this.reconcileAgain = false;
    this.pendingFocus = null;
    if (restoreFocus) {
      void this.updateComplete.then(() => {
        const trigger = this.renderRoot.querySelector<HTMLElement>('.trigger');
        if (trigger) withFocusMove(() => trigger.focus());
      });
    }
  }

  private stopListening(): void {
    document.removeEventListener('pointerdown', this.handleOutsidePointer, true);
    window.removeEventListener('resize', this.handleResize);
    window.removeEventListener('popstate', this.handlePopState);
  }

  private reset(): void {
    this.generation++;
    this.abort?.abort();
    this.moreAbort?.abort();
    this.abort = null;
    this.moreAbort = null;
    this.items = [];
    this.nextCursor = undefined;
    this.loading = false;
    this.loadingMore = false;
    this.error = '';
    this.moreError = '';
    this.reconcileAgain = false;
    this.pendingFocus = null;
  }

  /** Open or close the native sheet dialog to match `open`. */
  private syncSheet(): void {
    const dialog = this.renderRoot.querySelector<HTMLDialogElement>('dialog.sheet');
    if (!dialog) return;
    if (this.open && this.sheet && !dialog.open) {
      withFocusMove(() => dialog.showModal());
      const scroll = dialog.querySelector('.scroll');
      if (scroll) scroll.scrollTop = 0;
    }
  }

  /**
   * Keep the desktop panel inside the viewport: cap its height to the room
   * below the button (and min(24rem, 60vh)), and shift it sideways when the
   * button sits too close to an edge for the right-aligned panel.
   */
  private fitPanel(): void {
    const panel = this.renderRoot.querySelector<HTMLElement>('.panel');
    if (!panel) return;
    const vw = document.documentElement.clientWidth || window.innerWidth;
    const vh = window.visualViewport?.height ?? window.innerHeight;
    const rem = parseFloat(getComputedStyle(document.documentElement).fontSize) || 16;
    panel.style.transform = '';
    const rect = panel.getBoundingClientRect();
    const room = vh - rect.top - PANEL_MARGIN_PX;
    const cap = Math.min(PANEL_MAX_REM * rem, vh * 0.6);
    panel.style.maxHeight = `${Math.max(Math.min(cap, room), PANEL_MIN_PX)}px`;
    let shift = 0;
    if (rect.left < PANEL_MARGIN_PX) shift = PANEL_MARGIN_PX - rect.left;
    else if (rect.right > vw - PANEL_MARGIN_PX) shift = vw - PANEL_MARGIN_PX - rect.right;
    if (shift !== 0) panel.style.transform = `translateX(${Math.round(shift)}px)`;
  }

  /**
   * Reload the list: the first page when nothing is loaded, otherwise as
   * many rows as are loaded (in pages of up to STARRED_LIST_MAX_PAGE), so
   * Load more pages survive. The rows stay on screen while it runs; it
   * replaces them only when every page arrived. Invalidations that arrive
   * meanwhile coalesce into one more reconcile.
   */
  private async reconcile(): Promise<void> {
    const key = this.conversationKey;
    if (!key || !this.open) return;
    if (this.loading) {
      this.reconcileAgain = true;
      return;
    }
    // A Load more in flight would append to rows this replaces.
    this.moreAbort?.abort();
    this.moreAbort = null;
    this.loadingMore = false;
    const generation = this.generation;
    const abort = new AbortController();
    this.abort = abort;
    this.loading = true;
    this.reconciler.noteRefetched();
    const want = Math.max(this.items.length, STARRED_LIST_PAGE);
    try {
      const rows: StarItem[] = [];
      let cursor: string | undefined;
      let short = false;
      do {
        const limit = Math.min(STARRED_LIST_MAX_PAGE, Math.max(want - rows.length, 1));
        const page = await fetchStarPage(key, limit, cursor, abort.signal);
        if (generation !== this.generation) return;
        rows.push(...page.items);
        cursor = page.nextCursor;
        // A short page with a cursor is the hub's scan budget: the rest of
        // the scan runs only on an explicit Load more.
        short = page.items.length < limit;
      } while (cursor && !short && rows.length < want);
      this.replaceItems(rows, cursor);
      this.error = '';
      this.moreError = '';
    } catch (err) {
      if (generation !== this.generation || abort.signal.aborted) return;
      this.error = errorText(err);
    } finally {
      if (generation === this.generation) {
        this.loading = false;
        this.abort = null;
        if (this.reconcileAgain) {
          this.reconcileAgain = false;
          void this.reconcile();
        }
      }
    }
  }

  /** Swap in reloaded rows, keeping keyboard focus on a surviving entry. */
  private replaceItems(rows: StarItem[], cursor: string | undefined): void {
    const seen = new Set<string>();
    const next = rows.filter((i) => {
      if (seen.has(i.messageId) || this.deleted.has(i.messageId)) return false;
      seen.add(i.messageId);
      return true;
    });
    const focused = this.focusedEntry();
    if (focused && !next.some((i) => i.messageId === focused.messageId)) {
      this.pendingFocus = { index: focused.index };
    }
    this.items = next;
    this.nextCursor = cursor;
  }

  /** Append the next page (explicit Load more). */
  private async loadMore(): Promise<void> {
    const key = this.conversationKey;
    const cursor = this.nextCursor;
    if (!key || !cursor || this.loadingMore || this.loading) return;
    const generation = this.generation;
    const abort = new AbortController();
    this.moreAbort = abort;
    this.loadingMore = true;
    this.moreError = '';
    const firstNew = this.items.length;
    try {
      const page = await fetchStarPage(key, STARRED_LIST_PAGE, cursor, abort.signal);
      if (generation !== this.generation || abort.signal.aborted) return;
      const seen = new Set(this.items.map((i) => i.messageId));
      const added = page.items.filter(
        (i) => !seen.has(i.messageId) && !this.deleted.has(i.messageId)
      );
      this.items = [...this.items, ...added];
      this.nextCursor = page.nextCursor;
      // Keyboard users continue at the first new row; the button they
      // pressed may be gone (the last page) or far below.
      if (added.length > 0 && this.focusIsOnLoadMore()) this.pendingFocus = { index: firstNew };
    } catch (err) {
      if (generation !== this.generation || abort.signal.aborted) return;
      // An invalid (expired) cursor restarts from the loaded size.
      if (err instanceof StarsError && err.status === 400) {
        this.moreAbort = null;
        this.loadingMore = false;
        void this.reconcile();
        return;
      }
      this.moreError = errorText(err);
    } finally {
      if (generation === this.generation && this.moreAbort === abort) {
        this.loadingMore = false;
        this.moreAbort = null;
      }
    }
  }

  /** The entry holding keyboard focus, with its index. */
  private focusedEntry(): { messageId: string; index: number } | null {
    const active = (this.renderRoot as ShadowRoot).activeElement as HTMLElement | null;
    const messageId = active?.closest?.('a.entry')?.getAttribute('data-message-id');
    if (!messageId) return null;
    const index = this.items.findIndex((i) => i.messageId === messageId);
    return index >= 0 ? { messageId, index } : null;
  }

  private focusIsOnLoadMore(): boolean {
    const active = (this.renderRoot as ShadowRoot).activeElement as HTMLElement | null;
    return !!active?.closest?.('.more');
  }

  /**
   * Move focus after a render: to the entry at the remembered index (or
   * the last one), else Load more, else the list container.
   */
  private applyPendingFocus(): void {
    const target = this.pendingFocus;
    this.pendingFocus = null;
    if (!target || !this.open) return;
    const entries = [...this.renderRoot.querySelectorAll<HTMLElement>('a.entry')];
    const el =
      entries[Math.min(target.index, entries.length - 1)] ??
      this.renderRoot.querySelector<HTMLElement>('.more sl-button') ??
      this.renderRoot.querySelector<HTMLElement>(this.sheet ? '.close' : '.panel');
    el?.focus({ preventScroll: false });
  }

  /** The user's stars changed somewhere: reconcile while the list shows. */
  private handleInvalidate = (): void => {
    if (!this.open) return;
    void this.reconcile();
  };

  /**
   * A message was deleted: drop its row (and excerpt) at once, whatever
   * conversation it was in; the hub already removed its stars, so a later
   * refetch cannot bring it back.
   */
  private handleMessageDeleted = (e: Event): void => {
    const detail = (e as CustomEvent<{ data?: { messageId?: string }; messageId?: string }>).detail;
    const messageId = detail?.data?.messageId ?? detail?.messageId;
    if (!messageId) return;
    this.deleted.delete(messageId);
    this.deleted.add(messageId);
    for (const oldest of this.deleted) {
      if (this.deleted.size <= DELETED_MAX) break;
      this.deleted.delete(oldest);
    }
    if (this.items.some((i) => i.messageId === messageId)) {
      const focused = this.focusedEntry();
      if (focused?.messageId === messageId) this.pendingFocus = { index: focused.index };
      this.items = this.items.filter((i) => i.messageId !== messageId);
    }
  };

  private handleLocalChange = (e: Event): void => {
    const detail = (e as CustomEvent<{ conversationKey: string }>).detail;
    if (detail?.conversationKey === this.conversationKey) this.handleInvalidate();
  };

  private handleOutsidePointer = (e: PointerEvent): void => {
    if (!e.composedPath().includes(this)) this.closePanel(false);
  };

  /** Focus left the nonmodal panel and its button: close it there. */
  private handleFocusOut = (e: FocusEvent): void => {
    if (!this.open || this.sheet) return;
    const next = e.relatedTarget as Node | null;
    // No new target: the window lost focus (another app); keep it open.
    if (!next) return;
    if (this.contains(next) || this.renderRoot.contains(next)) return;
    this.closePanel(false);
  };

  private handleResize = (): void => {
    if (!this.open) return;
    // Crossing the sheet breakpoint changes the presentation: close it
    // rather than leave a modal sheet on a desktop layout or the reverse.
    if (shouldUseMenuSheet() !== this.sheet) {
      this.closePanel(false);
      return;
    }
    if (!this.sheet) this.fitPanel();
  };

  /** Back/Forward moves what is behind the list: close it. */
  private handlePopState = (): void => {
    this.closePanel(false);
  };

  /** Escape closes an open list from the trigger or from inside it. */
  private handleKeydown = (e: KeyboardEvent): void => {
    if (e.key === 'Escape' && this.open) {
      e.stopPropagation();
      e.preventDefault();
      this.closePanel(true);
    }
  };

  /** The sheet's Escape (the dialog's cancel): close through our path. */
  private handleSheetCancel = (e: Event): void => {
    e.preventDefault();
    this.closePanel(true);
  };

  /** The sheet dialog closed by itself (Android back): keep `open` in step. */
  private handleSheetClose = (): void => {
    if (this.open && this.sheet) this.closePanel(true);
  };

  /** A click on the sheet's backdrop (outside its box) closes it. */
  private handleSheetClick = (e: MouseEvent): void => {
    e.stopPropagation();
    const dialog = e.currentTarget as HTMLDialogElement;
    if (e.target !== dialog) return;
    const r = dialog.getBoundingClientRect();
    const inside =
      e.clientX >= r.left && e.clientX <= r.right && e.clientY >= r.top && e.clientY <= r.bottom;
    if (!inside) this.closePanel(true);
  };

  /**
   * Keep touches on the list from reaching the chat's panel swipe and
   * pull-to-refresh. Passive: nothing here calls preventDefault.
   */
  private stopTouch = {
    handleEvent: (e: TouchEvent): void => e.stopPropagation(),
    passive: true,
  };

  private openEntry(e: MouseEvent, item: StarItem): void {
    // Plain activation jumps in place; modified clicks keep link behaviour.
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    // Close first; the page owns the jump, so unmounting the list cannot
    // cancel it. Focus moves to the message when it lands, not back here.
    this.closePanel(false);
    this.dispatchEvent(
      new CustomEvent<StarredMessageOpenDetail>('starred-message-open', {
        detail: { messageId: item.messageId },
        bubbles: true,
        composed: true,
      })
    );
  }

  private renderEntry(item: StarItem): TemplateResult {
    const saved = formatInstant(item.starredAt, 'datetime');
    const sent = formatInstant(item.messageCreatedAt, 'datetime-full');
    const title = `Sent ${sent} by ${item.senderName}. Starred ${saved}.`;
    return html`<li>
      <a
        class="entry"
        href=${item.permalink}
        title=${title}
        data-message-id=${item.messageId}
        @click=${(e: MouseEvent): void => this.openEntry(e, item)}
      >
        <span class="entry-meta"
          ><span class="sr-only">Starred </span
          ><time class="entry-date" datetime=${item.starredAt}>${saved}</time
          ><span aria-hidden="true">·</span
          ><span class="entry-sender">${item.senderName}</span></span
        >
        <span class="entry-excerpt">${item.excerpt}</span>
        <span class="sr-only">Sent ${sent}${item.edited ? ', edited' : ''}</span>
      </a>
    </li>`;
  }

  private renderBody(): TemplateResult {
    const hasRows = this.items.length > 0;
    if (!hasRows && this.loading) {
      return html`<div class="status" role="status">Loading starred messages…</div>`;
    }
    if (!hasRows && this.error) {
      return html`<div class="status" role="alert">
        <span>${this.error}</span>
        <sl-button size="small" variant="text" @click=${(): void => void this.reconcile()}
          >Retry</sl-button
        >
      </div>`;
    }
    if (!hasRows && !this.nextCursor) {
      return html`<div class="status" role="status">
        No starred messages in this conversation. Use Star message in a message's menu to add one.
      </div>`;
    }
    return html`
      ${hasRows
        ? html`<ul
            aria-label="Starred messages in this conversation"
            aria-busy=${this.loading ? 'true' : 'false'}
          >
            ${repeat(
              this.items,
              (i) => i.messageId,
              (i) => this.renderEntry(i)
            )}
          </ul>`
        : html`<div class="status" role="status">
            No starred messages found yet. Load more to keep looking.
          </div>`}
      ${hasRows && this.error
        ? html`<div class="status inline" role="alert">
            <span>Could not refresh starred messages.</span>
            <sl-button size="small" variant="text" @click=${(): void => void this.reconcile()}
              >Retry</sl-button
            >
          </div>`
        : nothing}
      ${this.moreError
        ? html`<div class="status inline" role="alert">
            <span>${this.moreError}</span>
          </div>`
        : nothing}
      ${this.nextCursor
        ? html`<div class="more">
            <sl-button
              size="small"
              ?loading=${this.loadingMore}
              @click=${(): void => void this.loadMore()}
              >${this.moreError ? 'Retry loading more' : 'Load more'}</sl-button
            >
          </div>`
        : nothing}
    `;
  }

  private renderHeader(): TemplateResult {
    return html`<div class="panel-header">
      <h2 class="panel-title" id=${this.headingId}>Starred messages</h2>
      <span class="sort-label">Recently starred</span>
      ${this.sheet
        ? html`<button
            type="button"
            class="close"
            aria-label="Close starred messages"
            @click=${(): void => this.closePanel(true)}
          >
            <sl-icon name="x-lg" aria-hidden="true"></sl-icon>
          </button>`
        : nothing}
    </div>`;
  }

  private renderPanel(): TemplateResult | typeof nothing {
    if (!this.open) return nothing;
    if (this.sheet) {
      return html`<dialog
        class="sheet"
        id=${this.panelId}
        aria-labelledby=${this.headingId}
        @cancel=${this.handleSheetCancel}
        @close=${this.handleSheetClose}
        @click=${this.handleSheetClick}
        @touchstart=${this.stopTouch}
        @touchmove=${this.stopTouch}
        @touchend=${this.stopTouch}
        @touchcancel=${this.stopTouch}
      >
        ${this.renderHeader()}
        <div class="scroll">${this.renderBody()}</div>
      </dialog>`;
    }
    return html`<div
      class="panel"
      id=${this.panelId}
      role="dialog"
      aria-modal="false"
      aria-labelledby=${this.headingId}
      tabindex="-1"
      @touchstart=${this.stopTouch}
      @touchmove=${this.stopTouch}
      @touchend=${this.stopTouch}
      @touchcancel=${this.stopTouch}
    >
      ${this.renderHeader()}
      <div class="scroll">${this.renderBody()}</div>
    </div>`;
  }

  override render(): TemplateResult {
    return html`
      <div
        class="host-wrap"
        @focusout=${this.handleFocusOut}
        @keydown=${this.handleKeydown}
        style="display: contents"
      >
        <sl-tooltip content="Starred messages in this conversation" ?disabled=${this.open}>
          <button
            type="button"
            class="trigger"
            aria-label="Starred messages in this conversation"
            aria-haspopup="dialog"
            aria-expanded=${this.open ? 'true' : 'false'}
            aria-controls=${this.open ? this.panelId : nothing}
            @click=${(): void => this.toggle()}
          >
            <sl-icon name="star" aria-hidden="true"></sl-icon>
          </button>
        </sl-tooltip>
        ${this.renderPanel()}
      </div>
    `;
  }
}

/** The message of a failed list request. */
function errorText(err: unknown): string {
  return err instanceof Error && err.message ? err.message : 'Could not load starred messages';
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-starred-list': ScionChatStarredList;
  }
}
