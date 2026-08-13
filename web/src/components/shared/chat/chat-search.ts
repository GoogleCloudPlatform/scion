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

@customElement('scion-chat-search')
export class ScionChatSearch extends LitElement {
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

  private _debounceTimer: ReturnType<typeof setTimeout> | null = null;

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
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      pointer-events: none;
    }

    .search-input {
      width: 100%;
      padding: 0.375rem 0.5rem 0.375rem 1.75rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 6px;
      font-size: 0.8125rem;
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
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .scope-btn {
      padding: 0.25rem 0.5rem;
      border-radius: 4px;
      cursor: pointer;
      border: 1px solid transparent;
      background: none;
      font-size: 0.75rem;
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
      padding: 0.25rem 0;
    }

    .result-item {
      display: flex;
      flex-direction: column;
      gap: 0.125rem;
      padding: 0.625rem 0.75rem;
      cursor: pointer;
      border-bottom: 1px solid var(--scion-border-subtle, #f1f5f9);
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
      font-size: 0.6875rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .result-time {
      font-size: 0.625rem;
      color: var(--scion-text-muted, #94a3b8);
      white-space: nowrap;
      flex-shrink: 0;
    }

    .result-sender {
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .result-snippet {
      font-size: 0.75rem;
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
      font-size: 0.8125rem;
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
      font-size: 0.75rem;
      cursor: pointer;
    }

    .load-more button:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }
  `;

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this._debounceTimer) {
      clearTimeout(this._debounceTimer);
      this._debounceTimer = null;
    }
  }

  /** Close the search panel and reset state. */
  close(): void {
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
      input?.focus();
    });
  }

  private handleInput(e: Event): void {
    const input = e.target as HTMLInputElement;
    this.query = input.value;

    if (this._debounceTimer) {
      clearTimeout(this._debounceTimer);
    }

    if (this.query.trim().length < 2) {
      this.results = [];
      this.nextCursor = '';
      this.noResults = false;
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
    if (q.length < 2) return;

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

      const res = await apiFetch(`/api/v1/chat/search?${params.toString()}`);
      if (!res.ok) return;

      const data = (await res.json()) as SearchResponse;

      if (append) {
        this.results = [...this.results, ...data.results];
      } else {
        this.results = data.results || [];
      }

      this.nextCursor = data.nextCursor || '';
      this.noResults = this.results.length === 0 && !append;
    } catch {
      // Silently fail
    } finally {
      this.loading = false;
      this.loadingMore = false;
    }
  }

  private toggleScope(all: boolean): void {
    if (this.scopeAll === all) return;
    this.scopeAll = all;
    if (this.query.trim().length >= 2) {
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
    const now = Date.now();
    const diffMs = now - d.getTime();
    const diffMin = Math.floor(diffMs / 60000);

    if (diffMin < 1) return 'now';
    if (diffMin < 60) return `${diffMin}m ago`;
    const diffHrs = Math.floor(diffMin / 60);
    if (diffHrs < 24) return `${diffHrs}h ago`;
    const diffDays = Math.floor(diffHrs / 24);
    if (diffDays < 7) return `${diffDays}d ago`;

    return d.toLocaleDateString('en', { month: 'short', day: 'numeric' });
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
      .replace(/\x01MARK_OPEN\x01/g, '<mark>')
      .replace(/\x01MARK_CLOSE\x01/g, '</mark>');
  }

  override render() {
    if (!this.searchActive) return nothing;

    const hasConversation = !!this.conversationKey;

    return html`
      <div class="search-header">
        <div class="search-input-wrap">
          <sl-icon name="search"></sl-icon>
          <input
            class="search-input"
            type="text"
            placeholder="Search messages..."
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

      ${hasConversation
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
        : nothing}

      <div class="results-list">
        ${this.loading
          ? html`<div class="status-msg"><sl-spinner></sl-spinner></div>`
          : this.noResults
            ? html`<div class="status-msg">No messages found</div>`
            : this.results.length === 0 && this.query.trim().length < 2
              ? html`<div class="status-msg">Type at least 2 characters to search</div>`
              : this.results.map((r) => this.renderResult(r))}
        ${this.nextCursor && !this.loading
          ? html`
              <div class="load-more">
                ${this.loadingMore
                  ? html`<sl-spinner></sl-spinner>`
                  : html`<button @click=${() => void this.performSearch(true)}>
                      Load more
                    </button>`}
              </div>
            `
          : nothing}
      </div>
    `;
  }

  private renderResult(result: SearchResult) {
    return html`
      <div class="result-item" @click=${() => this.handleResultClick(result)}>
        <div class="result-top">
          <span class="result-thread">${result.threadName || result.conversationKey}</span>
          <span class="result-time">${this.formatTime(result.timestamp)}</span>
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
