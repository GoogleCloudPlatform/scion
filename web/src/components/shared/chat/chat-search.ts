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
 * Chat Search Component (W8)
 *
 * Provides a search bar and results panel for searching chat messages.
 * Features:
 * - Debounced search input (300ms)
 * - Minimum 2 character query
 * - Results show conversation name, sender, highlighted snippet, timestamp
 * - Click result navigates to conversation
 * - Toggle between "current conversation" and "all" search scope
 * - Keyset pagination via cursor
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { unsafeHTML } from 'lit/directives/unsafe-html.js';
import { apiFetch } from '../../../client/api.js';
import { formatInstant, formatInstantWithZone, formatRelative } from '../../../utils/time.js';
import { DisplayZoneController } from '../../../utils/display-zone-controller.js';
import { focusElement } from '../focus-moved.js';

/** Ages under this are shown relative; older ones as an absolute date. */
const WEEK_MS = 7 * 24 * 60 * 60 * 1000;

/** Shape of a search result from GET /api/v1/chat/search */
interface SearchResult {
  messageId: string;
  conversationKey: string;
  threadName: string;
  senderName: string;
  content: string;
  snippet: string;
  timestamp: string;
  projectId: string;
}

interface SearchResponse {
  results: SearchResult[];
  nextCursor?: string;
}

/** Shown when the hub stopped a search at its time limit (503 SEARCH_TIMEOUT). */
export const SEARCH_TIMEOUT_MESSAGE =
  'Search took too long. Try a longer phrase or search one space.';
/** Shown when another search by the same user is still running (429 SEARCH_BUSY). */
export const SEARCH_BUSY_MESSAGE = 'Another search is still running. Try again in a moment.';
/** Shown for any other failed search. */
export const SEARCH_FAILED_MESSAGE = 'Search failed. Try again.';

/** The hub's longest accepted search term, in characters. */
export const SEARCH_MAX_QUERY_CHARS = 256;

/**
 * The message for a refused search. The error code in the response body
 * decides (SEARCH_TIMEOUT, SEARCH_BUSY); any other code is a plain failure, so
 * a maintenance or proxy 503 does not read as a slow search. The HTTP status
 * is a fallback only when there is no code (0: no response).
 */
export function searchErrorMessage(status: number, code = ''): string {
  if (code === 'SEARCH_TIMEOUT') return SEARCH_TIMEOUT_MESSAGE;
  if (code === 'SEARCH_BUSY') return SEARCH_BUSY_MESSAGE;
  if (code) return SEARCH_FAILED_MESSAGE;
  if (status === 503) return SEARCH_TIMEOUT_MESSAGE;
  if (status === 429) return SEARCH_BUSY_MESSAGE;
  return SEARCH_FAILED_MESSAGE;
}

/** The hub's shortest accepted search term, in characters. */
export const SEARCH_MIN_QUERY_CHARS = 2;

/**
 * The length of a search term in characters as the hub counts them (Unicode
 * code points, not UTF-16 units), so one emoji is one character.
 */
export function searchQueryChars(q: string): number {
  return [...q.trim()].length;
}

/** The error code of an API error body ({ error: { code } }), or '' if none. */
async function errorCode(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { error?: { code?: unknown } };
    const code = body?.error?.code;
    return typeof code === 'string' ? code : '';
  } catch {
    return '';
  }
}

/** Whether err is the rejection of an aborted fetch. */
function isAbortError(err: unknown): boolean {
  return err instanceof DOMException && err.name === 'AbortError';
}

/**
 * The wait a 429 asks for, from its Retry-After header in seconds, held to
 * 1-5 s (1 s when absent or unreadable).
 */
export function retryAfterMs(res: Response): number {
  const n = Number.parseInt(res.headers.get('Retry-After') ?? '', 10);
  const secs = Number.isFinite(n) ? Math.min(Math.max(n, 1), 5) : 1;
  return secs * 1000;
}

/** Wait ms, or reject with an AbortError as soon as signal aborts. */
function sleepAbortable(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(new DOMException('Aborted', 'AbortError'));
      return;
    }
    const onAbort = (): void => {
      clearTimeout(timer);
      reject(new DOMException('Aborted', 'AbortError'));
    };
    const timer = setTimeout(() => {
      signal.removeEventListener('abort', onAbort);
      resolve();
    }, ms);
    signal.addEventListener('abort', onAbort, { once: true });
  });
}

@customElement('scion-chat-search')
export class ScionChatSearch extends LitElement {
  /** Re-renders absolute result dates when the display zone changes. */
  readonly _zone = new DisplayZoneController(this);

  /** Current project ID for scoped search. */
  @property()
  projectId = '';

  /** Current conversation key for scoped search. */
  @property()
  conversationKey = '';

  /** Current conversation name for display. */
  @property()
  conversationName = '';

  @state() private query = '';
  @state() private results: SearchResult[] = [];
  @state() private loading = false;
  @state() private searchActive = false;
  @state() private scopeAll = false;
  @state() private nextCursor = '';
  @state() private loadingMore = false;
  @state() private noResults = false;
  /** Message shown when the hub refused the search (timeout or busy). */
  @state() private errorMsg = '';
  /** Message for a failed "Load more"; the results already shown stay. */
  @state() private loadMoreError = '';

  private _debounceTimer: ReturnType<typeof setTimeout> | null = null;
  /** Aborts the in-flight search request; one per request. */
  private _abort: AbortController | null = null;

  static override styles = css`
    :host {
      display: flex;
      flex-direction: column;
      height: 100%;
      background: var(--scion-bg, #f8fafc);
    }

    .search-header {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
    }

    .search-input-wrap {
      flex: 1;
      position: relative;
      display: flex;
      align-items: center;
    }

    .search-input-wrap sl-icon {
      position: absolute;
      left: 0.5rem;
      font-size: var(--chat-fs-lg);
      color: var(--scion-text-muted, #64748b);
      pointer-events: none;
    }

    .search-input {
      width: 100%;
      padding: 0.375rem 0.5rem 0.375rem 1.75rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 6px;
      font-size: var(--chat-fs-md);
      outline: none;
      background: var(--scion-bg, #f8fafc);
      color: var(--scion-text, #1e293b);
    }

    .search-input:focus {
      border-color: var(--scion-primary, #3b82f6);
      box-shadow: 0 0 0 2px rgba(59, 130, 246, 0.15);
    }

    .search-input::placeholder {
      color: var(--scion-text-muted, #94a3b8);
    }

    /* Stop iOS/Android focus-zoom: this is a native <input>, not a
       Shoelace component, so it is not covered by the app-wide
       --sl-input-font-size-* rule. */
    @media (pointer: coarse) {
      .search-input {
        font-size: max(16px, var(--chat-fs-md));
      }
    }

    .close-btn {
      flex-shrink: 0;
    }

    .scope-toggle {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.375rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
      font-size: var(--chat-fs-base);
      color: var(--scion-text-muted, #64748b);
    }

    .scope-btn {
      padding: 0.25rem 0.5rem;
      border-radius: 4px;
      cursor: pointer;
      border: 1px solid transparent;
      background: none;
      font-size: var(--chat-fs-base);
      color: var(--scion-text-muted, #64748b);
    }

    .scope-btn:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .scope-btn.active {
      background: var(--scion-primary-50, #eff6ff);
      color: var(--scion-primary, #3b82f6);
      border-color: var(--scion-primary, #3b82f6);
    }

    .results-list {
      flex: 1;
      overflow-y: auto;
      overscroll-behavior: contain;
      /* Set by the chat page's mobile panels; see chat.ts. */
      touch-action: var(--chat-touch-action, auto);
      padding: 0.25rem 0;
    }

    .result-item {
      display: flex;
      flex-direction: column;
      gap: 0.125rem;
      padding: 0.625rem 0.75rem;
      cursor: pointer;
      border-bottom: 1px solid var(--scion-bg-subtle, #f1f5f9);
      transition: background 0.1s;
    }

    .result-item:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .result-top {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
    }

    .result-thread {
      font-size: var(--chat-fs-sm);
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .result-time {
      font-size: var(--chat-fs-xs);
      color: var(--scion-text-muted, #94a3b8);
      white-space: nowrap;
      flex-shrink: 0;
    }

    .result-sender {
      font-size: var(--chat-fs-base);
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .result-snippet {
      font-size: var(--chat-fs-base);
      color: var(--scion-text, #334155);
      line-height: 1.4;
      word-break: break-word;
    }

    .result-snippet mark {
      background: #fef08a;
      color: inherit;
      border-radius: 2px;
      padding: 0 1px;
    }

    .status-msg {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 2rem;
      color: var(--scion-text-muted, #64748b);
      font-size: var(--chat-fs-md);
      text-align: center;
    }

    .load-more {
      display: flex;
      justify-content: center;
      padding: 0.75rem;
    }

    .load-more button {
      padding: 0.375rem 1rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 6px;
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text, #1e293b);
      font-size: var(--chat-fs-base);
      cursor: pointer;
    }

    .load-more button:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    /* Clear a landscape phone's notch and rounded corners (the page uses
       viewport-fit=cover) on whichever sides this column meets the screen
       edge. Each inset is a transparent border, so the row's background still
       paints to the screen edge and only its content moves in. The chat page
       sets --chat-inset-left and --chat-inset-right for the edges the
       conversation touches; both are 0 everywhere else. */
    .search-header,
    .scope-toggle,
    .results-list {
      border-left: var(--chat-inset-left, 0px) solid transparent;
      border-right: var(--chat-inset-right, 0px) solid transparent;
    }
  `;

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this._debounceTimer) {
      clearTimeout(this._debounceTimer);
      this._debounceTimer = null;
    }
    this.abortSearch();
  }

  /**
   * Abort the in-flight search request, if any. The hub stops the
   * database statement when the request goes away, so a superseded
   * search does not keep running.
   */
  private abortSearch(): void {
    if (this._abort) {
      this._abort.abort();
      this._abort = null;
    }
  }

  /** Close the search panel and reset state. */
  close(): void {
    this.abortSearch();
    this.loading = false;
    this.loadingMore = false;
    this.errorMsg = '';
    this.loadMoreError = '';
    this.searchActive = false;
    this.query = '';
    this.results = [];
    this.nextCursor = '';
    this.noResults = false;
    this.dispatchEvent(new CustomEvent('search-close', { bubbles: true, composed: true }));
  }

  /** Open the search panel and focus the input. */
  open(): void {
    this.searchActive = true;
    this.requestUpdate();
    // Focus the input after render.
    requestAnimationFrame(() => {
      const input = this.shadowRoot?.querySelector('.search-input') as HTMLInputElement;
      focusElement(input);
    });
  }

  private handleInput(e: Event): void {
    const input = e.target as HTMLInputElement;
    this.query = input.value;

    if (this._debounceTimer) {
      clearTimeout(this._debounceTimer);
    }
    // New input supersedes the running search.
    this.abortSearch();
    this.errorMsg = '';
    this.loadMoreError = '';

    if (searchQueryChars(this.query) < SEARCH_MIN_QUERY_CHARS) {
      this.results = [];
      this.nextCursor = '';
      this.noResults = false;
      this.loading = false;
      return;
    }

    this._debounceTimer = setTimeout(() => {
      void this.performSearch();
    }, 300);
  }

  private handleKeydown(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      this.close();
    }
  }

  private async performSearch(append = false): Promise<void> {
    const q = this.query.trim();
    if (searchQueryChars(q) < SEARCH_MIN_QUERY_CHARS) return;

    this.abortSearch();
    const controller = new AbortController();
    this._abort = controller;
    this.errorMsg = '';
    this.loadMoreError = '';

    if (append) {
      this.loadingMore = true;
    } else {
      this.loading = true;
      this.results = [];
      this.nextCursor = '';
    }

    try {
      const params = new URLSearchParams({ q });

      if (!this.scopeAll && this.conversationKey) {
        params.set('key', this.conversationKey);
      } else if (!this.scopeAll && this.projectId) {
        params.set('projectId', this.projectId);
      }

      if (append && this.nextCursor) {
        params.set('cursor', this.nextCursor);
      }

      params.set('limit', '20');

      const url = `/api/v1/chat/search?${params.toString()}`;
      let res = await apiFetch(url, { signal: controller.signal });
      if (res.status === 429) {
        // The hub frees this user's slot only once it has seen the
        // superseded request go away, so a fast typist's next search can
        // meet their own previous one. Retry once after Retry-After, under
        // the same controller (a newer search still aborts it).
        await sleepAbortable(retryAfterMs(res), controller.signal);
        res = await apiFetch(url, { signal: controller.signal });
      }
      if (!res.ok) {
        const code = await errorCode(res);
        // Reading the body can end with the request superseded; a superseded
        // request writes no error (errorCode swallows its AbortError).
        if (controller.signal.aborted) return;
        const msg = searchErrorMessage(res.status, code);
        if (append) {
          // Keep the results already on screen; show the error below them.
          this.loadMoreError = msg;
        } else {
          this.noResults = false;
          this.errorMsg = msg;
        }
        return;
      }

      const data = (await res.json()) as SearchResponse;
      if (controller.signal.aborted) return;

      if (append) {
        this.results = [...this.results, ...data.results];
      } else {
        this.results = data.results || [];
      }

      this.nextCursor = data.nextCursor || '';
      this.noResults = this.results.length === 0 && !append;
    } catch (err) {
      // An aborted request was superseded or the panel closed: not an error.
      if (isAbortError(err)) return;
      if (append) {
        this.loadMoreError = searchErrorMessage(0);
      } else {
        this.errorMsg = searchErrorMessage(0);
      }
    } finally {
      // A newer request owns the loading state once this one is superseded.
      if (this._abort === controller) {
        this._abort = null;
        this.loading = false;
        this.loadingMore = false;
      }
    }
  }

  private toggleScope(all: boolean): void {
    if (this.scopeAll === all) return;
    this.scopeAll = all;
    if (searchQueryChars(this.query) >= SEARCH_MIN_QUERY_CHARS) {
      void this.performSearch();
    }
  }

  private handleResultClick(result: SearchResult): void {
    this.dispatchEvent(
      new CustomEvent('search-navigate', {
        bubbles: true,
        composed: true,
        detail: {
          conversationKey: result.conversationKey,
          messageId: result.messageId,
          projectId: result.projectId,
        },
      })
    );
  }

  private formatTime(iso: string): string {
    const d = new Date(iso);
    if (isNaN(d.getTime())) return '';
    const ageMs = Date.now() - d.getTime();
    // A future instant is clock skew between hub and browser.
    if (ageMs < 0) return 'now';
    // Under a week: a compact relative age.
    if (ageMs < WEEK_MS) return formatRelative(iso, { style: 'narrow' });

    // Older than a week: a compact absolute date in the display zone; the
    // zone is named in the element's title (the slot does not shrink).
    return formatInstant(iso, 'date');
  }

  /** Sanitize snippet HTML to only allow <mark> tags. */
  private sanitizeSnippet(snippet: string): string {
    // Replace <mark> and </mark> with placeholders, escape everything else,
    // then restore the placeholders.
    return snippet
      .replace(/<mark>/g, '\x01MARK_OPEN\x01')
      .replace(/<\/mark>/g, '\x01MARK_CLOSE\x01')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replaceAll('\x01MARK_OPEN\x01', '<mark>')
      .replaceAll('\x01MARK_CLOSE\x01', '</mark>');
  }

  override render() {
    if (!this.searchActive) return nothing;

    const hasConversation = !!this.conversationKey;

    // prettier-ignore
    return html`
      <div class="search-header">
        <div class="search-input-wrap">
          <sl-icon name="search"></sl-icon>
          <input
            class="search-input"
            type="text"
            placeholder="Search messages..."
            maxlength=${SEARCH_MAX_QUERY_CHARS}
            .value=${this.query}
            @input=${this.handleInput}
            @keydown=${this.handleKeydown}
          />
        </div>
        <sl-icon-button
          class="close-btn"
          name="x-lg"
          label="Close search"
          @click=${() => this.close()}
        ></sl-icon-button>
      </div>

      ${
        hasConversation
          ? html`
              <div class="scope-toggle">
                <button
                  class="scope-btn ${!this.scopeAll ? 'active' : ''}"
                  @click=${() => this.toggleScope(false)}
                >
                  In ${this.conversationName || 'this conversation'}
                </button>
                <button
                  class="scope-btn ${this.scopeAll ? 'active' : ''}"
                  @click=${() => this.toggleScope(true)}
                >
                  All conversations
                </button>
              </div>
            `
          : nothing
      }

      <div class="results-list">
        ${
          this.loading
            ? html`<div class="status-msg"><sl-spinner></sl-spinner></div>`
            : this.errorMsg
              ? html`<div class="status-msg search-error" role="alert">${this.errorMsg}</div>`
              : this.noResults
                ? html`<div class="status-msg">No messages found</div>`
                : this.results.length === 0 && searchQueryChars(this.query) < SEARCH_MIN_QUERY_CHARS
                  ? html`<div class="status-msg">Type at least 2 characters to search</div>`
                  : this.results.map((r) => this.renderResult(r))
        }
        ${
          this.nextCursor && !this.loading
            ? html`
                <div class="load-more">
                  ${
                    this.loadMoreError
                      ? html`<div class="status-msg search-error" role="alert">
                          ${this.loadMoreError}
                        </div>`
                      : nothing
                  }
                  ${
                    this.loadingMore
                      ? html`<sl-spinner></sl-spinner>`
                      : html`<button @click=${() => void this.performSearch(true)}>
                          Load more
                        </button>`
                  }
                </div>
              `
            : nothing
        }
      </div>
    `;
  }

  private renderResult(result: SearchResult) {
    return html`
      <div class="result-item" @click=${() => this.handleResultClick(result)}>
        <div class="result-top">
          <span class="result-thread">${result.threadName || result.conversationKey}</span>
          <span
            class="result-time"
            title=${formatInstantWithZone(result.timestamp, 'datetime-full')}
            >${this.formatTime(result.timestamp)}</span
          >
        </div>
        <span class="result-sender">${result.senderName}</span>
        <span class="result-snippet">${unsafeHTML(this.sanitizeSnippet(result.snippet))}</span>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-search': ScionChatSearch;
  }
}
