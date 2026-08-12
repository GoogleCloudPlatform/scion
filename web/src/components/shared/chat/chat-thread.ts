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
 * Chat thread orchestrator component.
 *
 * `<scion-chat-thread>` — the main chat view component.
 *
 * Responsibilities:
 * - Owns the message map (keyed by message ID for deduplication)
 * - Manages EventSource (SSE stream) for real-time messages
 * - Backfill-on-reconnect logic
 * - Scroll anchoring (anchor to bottom, "jump to latest" pill when scrolled up)
 * - Reverse-infinite-scroll upward using cursor pagination
 * - Renders chat-message and chat-system-line children
 * - 500-message buffer cap (MAX_BUFFER)
 *
 * Stream/backfill invariant (load-bearing):
 *   on mount:     GET history (limit 50) -> seed map -> open EventSource
 *   on 'message': parse UserMessageEvent -> upsert by id -> re-sort -> autoscroll if pinned
 *   on 'timeout': close stream -> GET history since lastKnownTimestamp -> merge -> reopen
 *   on error:     EventSource auto-reconnects; on 'open' after error, run same backfill
 *   on scroll-top: GET history with cursor -> prepend -> preserve scroll offset
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch, extractApiError } from '../../../client/api.js';
import type { Message } from '../../../shared/types.js';
import type { ChatSendDetail } from './chat-composer.js';
import './chat-message.js';
import './chat-system-line.js';
import './chat-composer.js';

/** Maximum messages kept in the buffer. */
const MAX_BUFFER = 500;

/** Number of messages to fetch per history request. */
const HISTORY_PAGE_SIZE = 50;

/** Threshold in pixels from top to trigger upward scroll loading. */
const SCROLL_TOP_THRESHOLD = 100;

/** Threshold in pixels from bottom to consider "pinned to bottom". */
const SCROLL_BOTTOM_THRESHOLD = 80;

/** Grouping window: consecutive messages from same sender within 5 min. */
const GROUP_WINDOW_MS = 5 * 60 * 1000;

/** System/state-change message types. */
const SYSTEM_MESSAGE_TYPES = new Set(['state-change', 'system']);

@customElement('scion-chat-thread')
export class ScionChatThread extends LitElement {
  @property()
  agentId = '';

  @property()
  agentName = '';

  @property({ type: Boolean })
  canSend = false;

  @property()
  visibilityMode: 'conversation' | 'verbose' | 'full' = 'conversation';

  @state() private messages: Message[] = [];
  @state() private messageMap = new Map<string, Message>();
  @state() private loading = false;
  @state() private error: string | null = null;
  @state() private streaming = false;
  @state() private sending = false;
  @state() private sendError: string | null = null;
  @state() private pinnedToBottom = true;
  @state() private loadingOlder = false;
  @state() private hasOlderMessages = true;
  @state() private loaded = false;

  private eventSource: EventSource | null = null;
  private nextCursor: string | null = null;
  private lastKnownTimestamp: string | null = null;
  private hadError = false;

  static override styles = css`
    :host {
      display: flex;
      flex-direction: column;
      height: 100%;
      min-height: 300px;
    }

    .thread-container {
      display: flex;
      flex-direction: column;
      flex: 1;
      overflow: hidden;
    }

    /* Streaming indicator */
    .stream-bar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0.25rem 1rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
    }

    .stream-indicator {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
    }

    .stream-dot {
      width: 6px;
      height: 6px;
      border-radius: 50%;
      background: var(--scion-success-500, #22c55e);
      animation: pulse 1.5s ease-in-out infinite;
    }

    @keyframes pulse {
      0%, 100% { opacity: 1; }
      50% { opacity: 0.3; }
    }

    /* Message scroll area */
    .messages-scroll {
      flex: 1;
      overflow-y: auto;
      overflow-x: hidden;
      padding: 0.5rem 0;
      display: flex;
      flex-direction: column;
    }

    .messages-list {
      display: flex;
      flex-direction: column;
      gap: 0;
      min-height: 100%;
      justify-content: flex-end;
    }

    /* Loading older messages */
    .loading-older {
      display: flex;
      justify-content: center;
      padding: 0.5rem;
    }

    /* Jump to latest pill */
    .jump-to-latest {
      position: sticky;
      bottom: 0.5rem;
      align-self: center;
      z-index: 10;
    }

    .jump-btn {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.375rem 0.75rem;
      background: var(--scion-primary, #3b82f6);
      color: #fff;
      border: none;
      border-radius: 1rem;
      font-size: 0.75rem;
      font-weight: 500;
      cursor: pointer;
      box-shadow: 0 2px 8px rgba(0, 0, 0, 0.15);
      transition: background 0.15s;
    }

    .jump-btn:hover {
      background: var(--scion-primary-600, #2563eb);
    }

    .jump-btn sl-icon {
      font-size: 0.875rem;
    }

    /* Date divider */
    .date-divider {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.75rem 1rem 0.25rem;
    }

    .date-divider::before,
    .date-divider::after {
      content: '';
      flex: 1;
      height: 1px;
      background: var(--scion-border, #e2e8f0);
    }

    .date-label {
      font-size: 0.6875rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
      white-space: nowrap;
    }

    /* Empty / Loading / Error states */
    .state-msg {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 3rem 2rem;
      color: var(--scion-text-muted, #64748b);
      gap: 0.75rem;
      flex: 1;
    }

    .state-msg sl-spinner {
      font-size: 1.5rem;
    }

    .state-msg sl-icon {
      font-size: 2rem;
      opacity: 0.4;
    }

    /* Send error toast */
    .send-error {
      padding: 0.375rem 1rem;
      font-size: 0.75rem;
      color: var(--scion-danger-600, #dc2626);
      background: var(--scion-danger-50, #fef2f2);
      border-top: 1px solid var(--scion-danger-200, #fecaca);
    }
  `;

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopStream();
  }

  /** Called by the parent when the chat view is first shown. */
  loadHistory(): void {
    if (this.loaded) return;
    this.loaded = true;
    void this.initialLoad();
  }

  /** Stop the SSE stream. Called on tab hide / disconnect. */
  stopStream(): void {
    if (this.eventSource) {
      this.eventSource.close();
      this.eventSource = null;
    }
    this.streaming = false;
  }

  // ---------------------------------------------------------------------------
  // Data loading
  // ---------------------------------------------------------------------------

  private async initialLoad(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      await this.fetchHistory();
      this.startStream();
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to load messages';
    } finally {
      this.loading = false;
      // On initial load, scroll to bottom
      this.scrollToBottom();
    }
  }

  private async fetchHistory(cursor?: string): Promise<void> {
    const params = new URLSearchParams({ limit: String(HISTORY_PAGE_SIZE) });
    if (cursor) {
      params.set('cursor', cursor);
    }

    const res = await apiFetch(
      `/api/v1/agents/${this.agentId}/messages?${params.toString()}`
    );

    if (!res.ok) {
      throw new Error(await extractApiError(res, 'Failed to fetch messages'));
    }

    const data = (await res.json()) as {
      items?: Message[];
      nextCursor?: string;
    };

    const items = data?.items ?? [];

    if (items.length < HISTORY_PAGE_SIZE) {
      this.hasOlderMessages = false;
    }

    if (data?.nextCursor) {
      this.nextCursor = data.nextCursor;
    }

    this.mergeMessages(items);
  }

  private async backfillSince(): Promise<void> {
    // Guard: skip backfill if we have no messages yet (initial load handles that).
    // Note: lastKnownTimestamp is not sent in the request — the API does not
    // support an `after`/`since` parameter (§5.1). We fetch the latest page and
    // rely on mergeMessages() to deduplicate by ID. If >50 messages arrive
    // during a single timeout gap, intermediate messages may be missed.
    if (!this.lastKnownTimestamp) return;

    const params = new URLSearchParams({
      limit: String(HISTORY_PAGE_SIZE),
      before: new Date().toISOString(),
    });

    const res = await apiFetch(
      `/api/v1/agents/${this.agentId}/messages?${params.toString()}`
    );

    if (!res.ok) return;

    const data = (await res.json()) as { items?: Message[] };
    const items = data?.items ?? [];
    this.mergeMessages(items);
  }

  private mergeMessages(newMessages: Message[]): void {
    for (const msg of newMessages) {
      if (!this.messageMap.has(msg.id)) {
        this.messageMap.set(msg.id, msg);
      }
    }

    // Sort ascending by createdAt (oldest first for chat display)
    const sorted = Array.from(this.messageMap.values()).sort(
      (a, b) => new Date(a.createdAt).getTime() - new Date(b.createdAt).getTime()
    );

    // Enforce buffer cap — remove oldest
    if (sorted.length > MAX_BUFFER) {
      const removed = sorted.splice(0, sorted.length - MAX_BUFFER);
      for (const msg of removed) {
        this.messageMap.delete(msg.id);
      }
    }

    this.messages = sorted;

    // Track last known timestamp for backfill
    if (sorted.length > 0) {
      this.lastKnownTimestamp = sorted[sorted.length - 1].createdAt;
    }
  }

  // ---------------------------------------------------------------------------
  // SSE Streaming
  // ---------------------------------------------------------------------------

  private startStream(): void {
    if (!this.isConnected || this.eventSource || !this.agentId) return;

    const url = `/api/v1/agents/${this.agentId}/messages/stream`;
    this.eventSource = new EventSource(url);
    this.streaming = true;

    this.eventSource.addEventListener('message', (event: Event) => {
      try {
        const msg = JSON.parse((event as MessageEvent).data) as Message;
        const wasPinned = this.pinnedToBottom;
        this.mergeMessages([msg]);
        if (wasPinned) {
          this.updateComplete.then(() => this.scrollToBottom());
        }
      } catch {
        // Skip unparseable entries
      }
    });

    this.eventSource.addEventListener('timeout', () => {
      this.stopStream();
      void this.backfillSince().then(() => this.startStream());
    });

    this.eventSource.addEventListener('open', () => {
      // If reconnecting after an error, backfill
      if (this.hadError) {
        this.hadError = false;
        void this.backfillSince();
      }
    });

    this.eventSource.onerror = () => {
      this.hadError = true;
      // EventSource will auto-reconnect
    };
  }

  // ---------------------------------------------------------------------------
  // Scroll handling
  // ---------------------------------------------------------------------------

  private handleScroll(e: Event): void {
    const el = e.target as HTMLElement;
    const distFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;
    this.pinnedToBottom = distFromBottom < SCROLL_BOTTOM_THRESHOLD;

    // Load older messages when scrolled near top
    if (
      el.scrollTop < SCROLL_TOP_THRESHOLD &&
      !this.loadingOlder &&
      this.hasOlderMessages &&
      this.nextCursor
    ) {
      void this.loadOlderMessages(el);
    }
  }

  private async loadOlderMessages(scrollEl: HTMLElement): Promise<void> {
    this.loadingOlder = true;
    const prevScrollHeight = scrollEl.scrollHeight;

    try {
      await this.fetchHistory(this.nextCursor || undefined);
    } catch {
      // Silently fail for older messages
    } finally {
      this.loadingOlder = false;
      // Preserve scroll position after prepending
      await this.updateComplete;
      const newScrollHeight = scrollEl.scrollHeight;
      scrollEl.scrollTop += newScrollHeight - prevScrollHeight;
    }
  }

  private scrollToBottom(): void {
    const scrollEl = this.shadowRoot?.querySelector('.messages-scroll') as HTMLElement | null;
    if (scrollEl) {
      scrollEl.scrollTop = scrollEl.scrollHeight;
    }
  }

  private handleJumpToLatest(): void {
    this.pinnedToBottom = true;
    this.scrollToBottom();
  }

  // ---------------------------------------------------------------------------
  // Send message
  // ---------------------------------------------------------------------------

  private async handleChatSend(e: CustomEvent<ChatSendDetail>): Promise<void> {
    const { text, plain, interrupt, onSuccess } = e.detail;
    if (!text || this.sending) return;

    this.sending = true;
    this.sendError = null;

    try {
      const res = await apiFetch(`/api/v1/agents/${this.agentId}/message`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          structured_message: { msg: text, plain },
          interrupt,
        }),
      });

      if (!res.ok) {
        this.sendError = await extractApiError(res, 'Failed to send message');
      } else {
        onSuccess();
      }
    } catch (err) {
      this.sendError = err instanceof Error ? err.message : 'Failed to send message';
    } finally {
      this.sending = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    return html`
      <div class="thread-container">
        ${this.renderStreamBar()}
        ${this.renderContent()}
        ${this.sendError
          ? html`<div class="send-error">${this.sendError}</div>`
          : nothing}
        ${this.canSend
          ? html`
              <scion-chat-composer
                ?disabled=${this.sending}
                @chat-send=${this.handleChatSend}
              ></scion-chat-composer>
            `
          : nothing}
      </div>
    `;
  }

  private renderStreamBar() {
    if (!this.streaming) return nothing;
    return html`
      <div class="stream-bar">
        <span class="stream-indicator">
          <span class="stream-dot"></span>
          Live
        </span>
      </div>
    `;
  }

  private renderContent() {
    if (this.loading && this.messages.length === 0) {
      return html`
        <div class="state-msg">
          <sl-spinner></sl-spinner>
          <span>Loading messages...</span>
        </div>
      `;
    }

    if (this.error && this.messages.length === 0) {
      return html`
        <div class="state-msg">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <span>${this.error}</span>
          <sl-button size="small" @click=${() => { this.loaded = false; this.loadHistory(); }}>
            Retry
          </sl-button>
        </div>
      `;
    }

    if (this.messages.length === 0) {
      return html`
        <div class="state-msg">
          <sl-icon name="chat-dots"></sl-icon>
          <span>No messages yet. Start a conversation!</span>
        </div>
      `;
    }

    return html`
      <div class="messages-scroll" @scroll=${this.handleScroll}>
        <div class="messages-list">
          ${this.loadingOlder
            ? html`<div class="loading-older"><sl-spinner></sl-spinner></div>`
            : nothing}
          ${this.renderMessages()}
        </div>
        ${!this.pinnedToBottom
          ? html`
              <div class="jump-to-latest">
                <button class="jump-btn" @click=${this.handleJumpToLatest}>
                  <sl-icon name="arrow-down"></sl-icon>
                  Jump to latest
                </button>
              </div>
            `
          : nothing}
      </div>
    `;
  }

  private renderMessages() {
    const rows: unknown[] = [];
    let lastDate = '';
    let prevSender = '';
    let prevTimestamp = 0;

    for (const msg of this.messages) {
      const d = new Date(msg.createdAt);
      const dateStr = d.toLocaleDateString('en', {
        year: 'numeric',
        month: 'short',
        day: 'numeric',
      });

      // Date divider
      if (dateStr !== lastDate) {
        lastDate = dateStr;
        prevSender = '';
        prevTimestamp = 0;
        rows.push(html`
          <div class="date-divider">
            <span class="date-label">${dateStr}</span>
          </div>
        `);
      }

      // System/state-change messages
      if (SYSTEM_MESSAGE_TYPES.has(msg.type)) {
        prevSender = '';
        prevTimestamp = 0;
        rows.push(html`
          <scion-chat-system-line
            message=${msg.msg}
            timestamp=${msg.createdAt}
            category=${(msg.metadata?.['system_category'] as string) || ''}
          ></scion-chat-system-line>
        `);
        continue;
      }

      // Grouping: consecutive messages from same sender within GROUP_WINDOW_MS
      const msgTime = d.getTime();
      const sameSender = msg.sender === prevSender;
      const withinWindow = msgTime - prevTimestamp < GROUP_WINDOW_MS;
      const showHeader = !sameSender || !withinWindow;

      const isFromAgent = msg.senderId === this.agentId;

      rows.push(html`
        <scion-chat-message
          body=${msg.msg}
          sender=${msg.sender}
          ?fromAgent=${isFromAgent}
          ?plain=${msg.plain ?? false}
          agentSlug=${isFromAgent ? (this.agentName || '') : ''}
          timestamp=${msg.createdAt}
          ?showHeader=${showHeader}
          ?urgent=${msg.urgent ?? false}
          ?broadcasted=${msg.broadcasted ?? false}
          channel=${msg.channel || ''}
          .attachments=${msg.attachments || []}
        ></scion-chat-message>
      `);

      prevSender = msg.sender;
      prevTimestamp = msgTime;
    }

    return rows;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-thread': ScionChatThread;
  }
}
