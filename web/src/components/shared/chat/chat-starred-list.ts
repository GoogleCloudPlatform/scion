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
 * (web.chat_starred_messages): an always-visible star that opens an
 * anchored, nonmodal panel listing this conversation's stars, most
 * recently starred first, with Load more. Each entry is a link; choosing
 * one closes the panel and fires `starred-message-open` (detail
 * `{ messageId }`) so the page resolves the current target and jumps.
 *
 * The list is fetched when the panel opens and refetched on the user's
 * star invalidation (SSE) and on local star changes in this conversation
 * while it is open. Nothing is persisted.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { stateManager } from '../../../client/state.js';
import { chatStarsLocalChanges, fetchStarPage, type StarItem } from '../../../client/chat-stars.js';
import { formatInstant } from '../../../utils/time.js';

/** Page size of the header list. */
export const STARRED_LIST_PAGE = 20;

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
  @state() private items: StarItem[] = [];
  @state() private nextCursor: string | undefined;
  @state() private loading = false;
  @state() private loadingMore = false;
  @state() private error = '';

  /** Drops responses for an older request (reopen, refetch, key switch). */
  private generation = 0;
  private abort: AbortController | null = null;
  /** An invalidation arrived while a first page was loading. */
  private refetchAfterLoad = false;
  private readonly panelId = `starred-panel-${++panelSeq}`;

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

    .panel {
      position: absolute;
      top: calc(100% + 0.25rem);
      right: 0;
      z-index: 50;
      width: min(24rem, calc(100vw - 1rem));
      max-height: min(24rem, 60vh);
      overflow-y: auto;
      overscroll-behavior: contain;
      background: var(--scion-surface, #fff);
      color: var(--scion-text, #1e293b);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      box-shadow: var(--sl-shadow-large, 0 8px 24px rgba(0, 0, 0, 0.15));
      padding: 0.5rem;
      font-weight: 400;
      text-align: left;
    }

    /* Narrow screens: span the viewport under the header instead of
       anchoring to the button, so the panel is never clipped. */
    @media (max-width: 768px) {
      .panel {
        position: fixed;
        top: auto;
        left: 0.5rem;
        right: 0.5rem;
        width: auto;
        padding-bottom: calc(0.5rem + env(safe-area-inset-bottom, 0px));
      }
    }

    .panel-header {
      display: flex;
      align-items: baseline;
      justify-content: space-between;
      gap: 0.5rem;
      padding: 0 0.25rem 0.375rem;
    }

    .panel-title {
      font-weight: 600;
      font-size: var(--chat-fs-base, 0.875rem);
      margin: 0;
    }

    .sort-label {
      font-size: var(--chat-fs-xs, 0.75rem);
      color: var(--scion-text-muted, #64748b);
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

    a.entry:hover,
    a.entry:focus-visible {
      background: var(--scion-bg-subtle, #f1f5f9);
      outline: 2px solid var(--scion-primary, #3b82f6);
      outline-offset: -2px;
    }

    .entry-meta {
      display: block;
      font-size: var(--chat-fs-xs, 0.75rem);
      color: var(--scion-text-muted, #64748b);
    }

    .entry-excerpt {
      display: -webkit-box;
      -webkit-line-clamp: 2;
      -webkit-box-orient: vertical;
      overflow: hidden;
      font-size: var(--chat-fs-sm, 0.8125rem);
      overflow-wrap: anywhere;
    }

    .status {
      padding: 0.75rem 0.5rem;
      font-size: var(--chat-fs-sm, 0.8125rem);
      color: var(--scion-text-muted, #64748b);
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
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    stateManager.addEventListener('chat-stars-updated', this.handleInvalidate);
    stateManager.addEventListener('connected', this.handleInvalidate);
    chatStarsLocalChanges.addEventListener('changed', this.handleLocalChange);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('chat-stars-updated', this.handleInvalidate);
    stateManager.removeEventListener('connected', this.handleInvalidate);
    chatStarsLocalChanges.removeEventListener('changed', this.handleLocalChange);
    document.removeEventListener('pointerdown', this.handleOutsidePointer, true);
    this.abort?.abort();
  }

  protected override willUpdate(changed: PropertyValues<this>): void {
    if (changed.has('conversationKey') && changed.get('conversationKey') !== undefined) {
      this.closePanel(false);
      this.reset();
    }
  }

  /** Open or close the panel (the button). */
  private toggle(): void {
    if (this.open) {
      this.closePanel(true);
    } else {
      this.open = true;
      document.addEventListener('pointerdown', this.handleOutsidePointer, true);
      void this.loadFirst();
      void this.updateComplete.then(() => {
        this.renderRoot.querySelector<HTMLElement>('.panel')?.focus();
      });
    }
  }

  private closePanel(restoreFocus: boolean): void {
    if (!this.open) return;
    this.open = false;
    document.removeEventListener('pointerdown', this.handleOutsidePointer, true);
    if (restoreFocus) {
      void this.updateComplete.then(() => {
        this.renderRoot.querySelector<HTMLElement>('.trigger')?.focus();
      });
    }
  }

  private reset(): void {
    this.generation++;
    this.abort?.abort();
    this.abort = null;
    this.items = [];
    this.nextCursor = undefined;
    this.loading = false;
    this.loadingMore = false;
    this.error = '';
    this.refetchAfterLoad = false;
  }

  /** Fetch the first page, replacing the list. */
  private async loadFirst(): Promise<void> {
    const key = this.conversationKey;
    if (!key) return;
    this.abort?.abort();
    const abort = new AbortController();
    this.abort = abort;
    const generation = ++this.generation;
    this.loading = true;
    this.loadingMore = false;
    this.error = '';
    try {
      const page = await fetchStarPage(key, STARRED_LIST_PAGE, undefined, abort.signal);
      if (generation !== this.generation) return;
      this.items = page.items;
      this.nextCursor = page.nextCursor;
    } catch (err) {
      if (generation !== this.generation || abort.signal.aborted) return;
      this.error = err instanceof Error ? err.message : 'Could not load starred messages';
    } finally {
      if (generation === this.generation) {
        this.loading = false;
        if (this.refetchAfterLoad) {
          this.refetchAfterLoad = false;
          void this.loadFirst();
        }
      }
    }
  }

  /** Append the next page (explicit Load more). */
  private async loadMore(): Promise<void> {
    const key = this.conversationKey;
    const cursor = this.nextCursor;
    if (!key || !cursor || this.loadingMore) return;
    const generation = this.generation;
    this.loadingMore = true;
    try {
      const page = await fetchStarPage(key, STARRED_LIST_PAGE, cursor);
      if (generation !== this.generation) return;
      const seen = new Set(this.items.map((i) => i.messageId));
      this.items = [...this.items, ...page.items.filter((i) => !seen.has(i.messageId))];
      this.nextCursor = page.nextCursor;
    } catch (err) {
      if (generation !== this.generation) return;
      // An invalid (expired) cursor restarts from the first page.
      if (err instanceof Error && 'status' in err && (err as { status: number }).status === 400) {
        void this.loadFirst();
        return;
      }
      this.error = err instanceof Error ? err.message : 'Could not load starred messages';
    } finally {
      if (generation === this.generation) this.loadingMore = false;
    }
  }

  /** The user's stars changed somewhere: refetch while the panel shows. */
  private handleInvalidate = (): void => {
    if (!this.open) return;
    if (this.loading) {
      this.refetchAfterLoad = true;
      return;
    }
    void this.loadFirst();
  };

  private handleLocalChange = (e: Event): void => {
    const detail = (e as CustomEvent<{ conversationKey: string }>).detail;
    if (detail?.conversationKey === this.conversationKey) this.handleInvalidate();
  };

  private handleOutsidePointer = (e: PointerEvent): void => {
    if (!e.composedPath().includes(this)) this.closePanel(false);
  };

  private handleKeydown = (e: KeyboardEvent): void => {
    if (e.key === 'Escape') {
      e.stopPropagation();
      this.closePanel(true);
    }
  };

  private openEntry(e: MouseEvent, item: StarItem): void {
    // Plain activation jumps in place; modified clicks keep link behaviour.
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
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
    const saved = formatLocal(item.starredAt);
    const sent = formatLocal(item.messageCreatedAt);
    const title = `Sent ${sent} by ${item.senderName}. Starred ${saved}.`;
    return html`<li>
      <a
        class="entry"
        href=${item.permalink}
        title=${title}
        @click=${(e: MouseEvent): void => this.openEntry(e, item)}
      >
        <span class="entry-meta">Starred ${saved} · ${item.senderName}</span>
        <span class="entry-excerpt">${item.excerpt}</span>
        <span class="sr-only">Sent ${sent}</span>
      </a>
    </li>`;
  }

  private renderBody(): TemplateResult {
    if (this.loading && this.items.length === 0) {
      return html`<div class="status" role="status">Loading starred messages…</div>`;
    }
    if (this.error && this.items.length === 0) {
      return html`<div class="status" role="alert">
        ${this.error}
        <sl-button size="small" variant="text" @click=${(): void => void this.loadFirst()}
          >Retry</sl-button
        >
      </div>`;
    }
    if (this.items.length === 0) {
      return html`<div class="status">
        No starred messages in this conversation. Use Star message in a message's menu to add one.
      </div>`;
    }
    return html`
      <ul aria-label="Starred messages in this conversation">
        ${this.items.map((item) => this.renderEntry(item))}
      </ul>
      ${this.error ? html`<div class="status" role="alert">${this.error}</div>` : nothing}
      ${this.nextCursor
        ? html`<div class="more">
            <sl-button
              size="small"
              ?loading=${this.loadingMore}
              @click=${(): void => void this.loadMore()}
              >Load more</sl-button
            >
          </div>`
        : nothing}
    `;
  }

  override render(): TemplateResult {
    return html`
      <sl-tooltip content="Starred messages in this conversation">
        <button
          type="button"
          class="trigger"
          aria-label="Starred messages in this conversation"
          aria-haspopup="dialog"
          aria-expanded=${this.open ? 'true' : 'false'}
          aria-controls=${this.panelId}
          @click=${(): void => this.toggle()}
        >
          <sl-icon name="star" aria-hidden="true"></sl-icon>
        </button>
      </sl-tooltip>
      ${this.open
        ? html`<div
            class="panel"
            id=${this.panelId}
            role="dialog"
            aria-label="Starred messages in this conversation"
            tabindex="-1"
            @keydown=${this.handleKeydown}
          >
            <div class="panel-header">
              <h2 class="panel-title">Starred messages</h2>
              <span class="sort-label">Recently starred</span>
            </div>
            ${this.renderBody()}
          </div>`
        : nothing}
    `;
  }
}

/** A localized absolute date and time for an ISO instant. */
function formatLocal(iso: string): string {
  return formatInstant(iso, 'datetime');
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-starred-list': ScionChatStarredList;
  }
}
