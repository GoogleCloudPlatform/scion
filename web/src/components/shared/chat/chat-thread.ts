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
import type { Agent, Message } from '../../../shared/types.js';
import type { ChatSendDetail } from './chat-composer.js';
import type { VisibilityMode, VisibilityChangeDetail } from './chat-visibility-toggle.js';
import { stateManager } from '../../../client/main.js';
import './chat-message.js';
import './chat-system-line.js';
import './chat-composer.js';
import './chat-visibility-toggle.js';

/** Result from server-side mention fan-out. */
interface MentionResult {
  slug: string;
  status: string;
  error?: string;
}

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
  visibilityMode: VisibilityMode = 'conversation';

  /** Whether the visibility toggle is shown in the header. */
  @property({ type: Boolean })
  showVisibilityToggle = false;

  /** Agents available for @-mention in the composer. */
  @property({ type: Array })
  agents: Agent[] = [];

  // ---- Wave-2 v2 properties ----

  /**
   * Conversation key for v2 mode (topic UUID or DM key).
   * When set, the component uses v2 conversation endpoints and SSE.
   */
  @property()
  conversationKey = '';

  /** The project ID this conversation belongs to (for v2 mode). */
  @property()
  projectId = '';

  /** Thread name for display (v2 mode). */
  @property()
  threadName = '';

  /** Default agent slug for this thread (v2 mode). */
  @property()
  defaultAgent = '';

  /** Whether this is a DM conversation (v2 mode). */
  @property({ type: Boolean })
  isDM = false;

  /** DM peer name (v2 mode). */
  @property()
  peerName = '';

  /** Members available for @-mention in v2 mode. */
  @property({ type: Array })
  members: Array<{
    id: string;
    name: string;
    email: string;
    avatarUrl?: string;
    kind: 'user' | 'agent';
  }> = [];

  /** Whether v2 mode is active. Derived from conversationKey presence. */
  private get isV2(): boolean {
    return this.conversationKey.length > 0;
  }

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
  /** Mention results keyed by message ID (for "also notified" footer per message). */
  @state() private mentionResultsByMessageId = new Map<string, MentionResult[]>();

  private eventSource: EventSource | null = null;
  private nextCursor: string | null = null;
  private lastKnownTimestamp: string | null = null;
  private hadError = false;
  private fetchId = 0;

  /** Bound listener for v2 SSE chat-message events via stateManager. */
  private _v2MessageHandler = this.handleV2ChatMessage.bind(this);

  /** Read tracking: debounce timer for advancing watermark. */
  private _readDebounceTimer: ReturnType<typeof setTimeout> | null = null;

  /** Read tracking: whether the tab is focused. */
  private _tabFocused = true;

  /** Focus/blur handlers for read tracking. */
  private _focusHandler = () => {
    this._tabFocused = true;
    this.maybeAdvanceReadWatermark();
  };
  private _blurHandler = () => {
    this._tabFocused = false;
  };

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
      0%,
      100% {
        opacity: 1;
      }
      50% {
        opacity: 0.3;
      }
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

    /* Mention results footer */
    .mention-results {
      padding: 0.25rem 1rem;
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .mention-results .mention-slug {
      font-weight: 600;
    }
  `;

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopStream();
    // Clean up v2 SSE listener
    stateManager.removeEventListener('chat-message-received', this._v2MessageHandler);
    // Clean up read tracking
    window.removeEventListener('focus', this._focusHandler);
    window.removeEventListener('blur', this._blurHandler);
    if (this._readDebounceTimer) {
      clearTimeout(this._readDebounceTimer);
      this._readDebounceTimer = null;
    }
  }

  /** Called by the parent when the chat view is first shown. */
  loadHistory(): void {
    if (this.loaded) return;
    this.loaded = true;
    if (this.isV2) {
      void this.initialLoadV2();
    } else {
      void this.loadPrefsAndHistory();
    }
  }

  /** Load saved preferences first, then fetch history. */
  private async loadPrefsAndHistory(): Promise<void> {
    await this.loadPrefs();
    await this.initialLoad();
  }

  /** Load the saved visibility mode pref from the server. */
  private async loadPrefs(): Promise<void> {
    if (!this.agentId) return;
    try {
      const res = await apiFetch(`/api/v1/chat/prefs?agentId=${encodeURIComponent(this.agentId)}`);
      if (res.ok) {
        const data = (await res.json()) as { visibility_mode?: string };
        if (
          data.visibility_mode &&
          ['conversation', 'verbose', 'full'].includes(data.visibility_mode)
        ) {
          this.visibilityMode = data.visibility_mode as VisibilityMode;
        }
      }
    } catch (err) {
      console.warn('Failed to load chat prefs, using defaults', err);
    }
  }

  /** Save the visibility mode pref to the server. */
  private async savePrefs(mode: VisibilityMode): Promise<void> {
    if (!this.agentId) return;
    try {
      const res = await apiFetch(`/api/v1/chat/prefs?agentId=${encodeURIComponent(this.agentId)}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ visibility_mode: mode }),
      });
      if (!res.ok) {
        console.warn('Failed to save chat prefs:', res.status, res.statusText);
      }
    } catch (err) {
      console.warn('Failed to save chat prefs', err);
    }
  }

  /** Handle visibility mode change from the toggle. */
  private handleVisibilityChange(e: CustomEvent<VisibilityChangeDetail>): void {
    const newMode = e.detail.mode;
    if (newMode === this.visibilityMode) return;
    this.visibilityMode = newMode;
    void this.savePrefs(newMode);
    // Clear and re-fetch with the new filter.
    void this.refetchWithNewFilter();
  }

  /** Clear messages and re-fetch history with the current visibility filter. */
  private async refetchWithNewFilter(): Promise<void> {
    const currentId = ++this.fetchId;
    this.messageMap.clear();
    this.messages = [];
    this.nextCursor = null;
    this.lastKnownTimestamp = null;
    this.hasOlderMessages = true;

    // Stop the stream, re-fetch, and restart.
    this.stopStream();
    this.loading = true;
    this.error = null;
    try {
      await this.fetchHistory();
      if (currentId !== this.fetchId) return;
      this.startStream();
    } catch (err) {
      if (currentId !== this.fetchId) return;
      this.error = err instanceof Error ? err.message : 'Failed to load messages';
    } finally {
      if (currentId === this.fetchId) {
        this.loading = false;
        this.scrollToBottom();
      }
    }
  }

  /** Check if a message sender is an agent (v2 multi-sender). */
  private isSenderAgent(msg: Message): boolean {
    // Agent messages have sender like "agent:slug" or recipient patterns
    if (msg.sender.startsWith('agent:')) return true;
    // Check against known members
    const member = this.members.find((m) => m.id === msg.senderId || m.email === msg.sender);
    if (member) return member.kind === 'agent';
    // If sender is not in the current user's perspective, check the type
    return msg.type === 'assistant-reply' || msg.type === 'mention-reply';
  }

  /** Get display name for a message sender (v2 multi-sender). */
  private getSenderDisplayName(msg: Message): string {
    const member = this.members.find((m) => m.id === msg.senderId || m.email === msg.sender);
    if (member) return member.name;
    // Fall back to parsing the sender string
    if (msg.sender.startsWith('agent:')) return msg.sender.slice(6);
    if (msg.sender.startsWith('user:')) return msg.sender.slice(5);
    return msg.sender;
  }

  /** Stop the SSE stream. Called on tab hide / disconnect. */
  stopStream(): void {
    if (this.eventSource) {
      this.eventSource.close();
      this.eventSource = null;
    }
    // Also clean up v2 SSE listener
    if (this.isV2) {
      stateManager.removeEventListener('chat-message-received', this._v2MessageHandler);
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
      this.scrollToBottom();
    }
  }

  /** Check whether a message should be shown given the current visibility mode. */
  private shouldShowMessage(msg: Message): boolean {
    const vis = msg.visibility || 'normal';
    switch (this.visibilityMode) {
      case 'conversation':
        return vis === 'normal';
      case 'verbose':
        return vis === 'normal' || vis === 'verbose';
      case 'full':
        return true;
    }
  }

  /** Build the visibility query params based on the current mode. */
  private appendVisibilityParams(params: URLSearchParams): void {
    switch (this.visibilityMode) {
      case 'conversation':
        params.append('visibility', 'normal');
        break;
      case 'verbose':
        params.append('visibility', 'normal');
        params.append('visibility', 'verbose');
        break;
      case 'full':
        // No filter — show everything.
        break;
    }
  }

  private async fetchHistory(cursor?: string): Promise<void> {
    const currentId = this.fetchId;
    const params = new URLSearchParams({ limit: String(HISTORY_PAGE_SIZE) });
    if (cursor) {
      params.set('cursor', cursor);
    }
    this.appendVisibilityParams(params);

    const res = await apiFetch(
      `/api/v1/agents/${encodeURIComponent(this.agentId)}/messages?${params.toString()}`
    );

    if (currentId !== this.fetchId) return;

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

    const currentId = this.fetchId;
    const params = new URLSearchParams({
      limit: String(HISTORY_PAGE_SIZE),
      before: new Date().toISOString(),
    });
    this.appendVisibilityParams(params);

    const res = await apiFetch(
      `/api/v1/agents/${encodeURIComponent(this.agentId)}/messages?${params.toString()}`
    );

    if (currentId !== this.fetchId) return;
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
        // Prune mention results for evicted messages (R1 fix).
        this.mentionResultsByMessageId.delete(msg.id);
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

    const url = `/api/v1/agents/${encodeURIComponent(this.agentId)}/messages/stream`;
    this.eventSource = new EventSource(url);
    this.streaming = true;

    this.eventSource.addEventListener('message', (event: Event) => {
      try {
        const msg = JSON.parse((event as MessageEvent).data as string) as Message;
        const wasPinned = this.pinnedToBottom;
        this.mergeMessages([msg]);
        if (wasPinned) {
          void this.updateComplete.then(() => this.scrollToBottom());
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
  // V2 mode: conversation-key-based loading + stateManager SSE
  // ---------------------------------------------------------------------------

  private async initialLoadV2(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      await this.fetchHistoryV2();
      this.startStreamV2();
      // Set up read tracking
      window.addEventListener('focus', this._focusHandler);
      window.addEventListener('blur', this._blurHandler);
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to load messages';
    } finally {
      this.loading = false;
      this.scrollToBottom();
    }
  }

  private async fetchHistoryV2(cursor?: string): Promise<void> {
    const currentId = this.fetchId;
    const params = new URLSearchParams({ limit: String(HISTORY_PAGE_SIZE) });
    if (cursor) {
      params.set('cursor', cursor);
    }

    const res = await apiFetch(
      `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/messages?${params.toString()}`
    );

    if (currentId !== this.fetchId) return;

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

  /** Start listening for v2 messages via stateManager instead of per-thread EventSource. */
  private startStreamV2(): void {
    stateManager.addEventListener('chat-message-received', this._v2MessageHandler);
    this.streaming = true;
  }

  /** Handle v2 SSE chat message events. Backfill from REST to get the actual message. */
  private handleV2ChatMessage(): void {
    // The stateManager event doesn't include the full message payload for a specific
    // conversation, so we backfill from REST to pick up new messages.
    void this.backfillV2();
  }

  private async backfillV2(): Promise<void> {
    if (!this.conversationKey) return;
    const currentId = this.fetchId;
    const params = new URLSearchParams({
      limit: String(HISTORY_PAGE_SIZE),
    });

    const res = await apiFetch(
      `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/messages?${params.toString()}`
    );

    if (currentId !== this.fetchId) return;
    if (!res.ok) return;

    const data = (await res.json()) as { items?: Message[] };
    const items = data?.items ?? [];
    const wasPinned = this.pinnedToBottom;
    this.mergeMessages(items);
    if (wasPinned) {
      void this.updateComplete.then(() => this.scrollToBottom());
    }
    // Advance read watermark if applicable
    this.maybeAdvanceReadWatermark();
  }

  /** Send a message in v2 mode. */
  private async handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void> {
    const { text, plain, mentions, onSuccess } = e.detail;
    if (!text || this.sending) return;

    // Check for /default slash command
    if (text.startsWith('/default ')) {
      await this.handleDefaultCommand(text);
      onSuccess();
      return;
    }

    this.sending = true;
    this.sendError = null;

    try {
      const body: Record<string, unknown> = {
        structured_message: { msg: text, plain },
      };
      if (mentions && mentions.length > 0) {
        body.mentions = mentions;
      }

      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/messages`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        }
      );

      if (!res.ok) {
        this.sendError = await extractApiError(res, 'Failed to send message');
      } else {
        onSuccess();
        // Backfill to pick up the message immediately
        void this.backfillV2();
      }
    } catch (err) {
      this.sendError = err instanceof Error ? err.message : 'Failed to send message';
    } finally {
      this.sending = false;
    }
  }

  /** Handle /default slash command. */
  private async handleDefaultCommand(text: string): Promise<void> {
    const arg = text.slice('/default '.length).trim();
    if (!this.conversationKey || this.isDM) return;

    try {
      const body: Record<string, unknown> = {};
      if (arg === 'clear') {
        body.default_agent = null;
      } else {
        body.default_agent = arg;
      }
      const res = await apiFetch(
        `/api/v1/chat/threads/${encodeURIComponent(this.conversationKey)}`,
        {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        }
      );
      if (res.ok) {
        this.defaultAgent = arg === 'clear' ? '' : arg;
        this.dispatchEvent(
          new CustomEvent('default-agent-changed', {
            detail: { defaultAgent: this.defaultAgent },
            bubbles: true,
            composed: true,
          })
        );
      }
    } catch {
      // Non-critical
    }
  }

  /** Advance the read watermark if conditions are met. */
  private maybeAdvanceReadWatermark(): void {
    if (!this.isV2 || !this._tabFocused || !this.pinnedToBottom) return;
    if (this.messages.length === 0) return;

    // Debounce
    if (this._readDebounceTimer) clearTimeout(this._readDebounceTimer);
    this._readDebounceTimer = setTimeout(() => {
      const lastMsg = this.messages[this.messages.length - 1];
      if (lastMsg) {
        void this.advanceReadWatermark(lastMsg.id);
      }
    }, 1000);
  }

  private async advanceReadWatermark(messageId: string): Promise<void> {
    try {
      await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/read`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ last_read_message_id: messageId }),
        }
      );
    } catch {
      // Non-critical
    }
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
      if (this.isV2) {
        void this.loadOlderMessagesV2(el);
      } else {
        void this.loadOlderMessages(el);
      }
    }

    // Advance read watermark in v2 mode
    if (this.pinnedToBottom) {
      this.maybeAdvanceReadWatermark();
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

  private async loadOlderMessagesV2(scrollEl: HTMLElement): Promise<void> {
    this.loadingOlder = true;
    const prevScrollHeight = scrollEl.scrollHeight;

    try {
      await this.fetchHistoryV2(this.nextCursor || undefined);
    } catch {
      // Silently fail for older messages
    } finally {
      this.loadingOlder = false;
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
    const { text, plain, interrupt, mentions, onSuccess } = e.detail;
    if (!text || this.sending) return;

    this.sending = true;
    this.sendError = null;

    try {
      // Build the POST body, including mentions when present.
      const body: Record<string, unknown> = {
        structured_message: { msg: text, plain },
        interrupt,
      };
      if (mentions && mentions.length > 0) {
        body.mentions = mentions;
      }

      const res = await apiFetch(`/api/v1/agents/${encodeURIComponent(this.agentId)}/message`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!res.ok) {
        this.sendError = await extractApiError(res, 'Failed to send message');
      } else {
        // Only parse the JSON body when mentions were sent (O1 fix).
        if (mentions && mentions.length > 0) {
          try {
            const contentType = res.headers.get('content-type');
            if (contentType && contentType.includes('application/json')) {
              const data = (await res.json()) as {
                message_id?: string;
                mention_results?: MentionResult[];
              };
              if (data?.message_id && data?.mention_results && data.mention_results.length > 0) {
                const updated = new Map(this.mentionResultsByMessageId);
                updated.set(data.message_id, data.mention_results);
                this.mentionResultsByMessageId = updated;
              }
            }
          } catch (err) {
            console.error('Failed to parse mention results response:', err);
          }
        }
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
    if (this.isV2) {
      return this.renderV2();
    }
    return html`
      <div class="thread-container">
        ${this.renderStreamBar()} ${this.renderContent()}
        ${this.sendError ? html`<div class="send-error">${this.sendError}</div>` : nothing}
        ${this.canSend
          ? html`
              <scion-chat-composer
                ?disabled=${this.sending}
                .agents=${this.agents}
                @chat-send=${this.handleChatSend}
              ></scion-chat-composer>
            `
          : nothing}
      </div>
    `;
  }

  private renderV2() {
    return html`
      <div class="thread-container">
        ${this.renderStreamBar()} ${this.renderContent()}
        ${this.sendError ? html`<div class="send-error">${this.sendError}</div>` : nothing}
        <scion-chat-composer
          ?disabled=${this.sending}
          .agents=${this.agents}
          .members=${this.members}
          .defaultAgent=${this.defaultAgent}
          .conversationMode=${this.isDM ? 'dm' : 'thread'}
          .peerName=${this.peerName}
          @chat-send=${this.handleChatSendV2}
        ></scion-chat-composer>
      </div>
    `;
  }

  private renderStreamBar() {
    // Show the bar when streaming OR when the toggle is visible (they share the row).
    if (!this.streaming && !this.showVisibilityToggle) return nothing;
    return html`
      <div class="stream-bar">
        <span class="stream-indicator">
          ${this.streaming ? html`<span class="stream-dot"></span> Live` : nothing}
        </span>
        ${this.showVisibilityToggle
          ? html`
              <scion-chat-visibility-toggle
                mode=${this.visibilityMode}
                @visibility-change=${this.handleVisibilityChange}
              ></scion-chat-visibility-toggle>
            `
          : nothing}
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
          <sl-button
            size="small"
            @click=${() => {
              this.loaded = false;
              this.loadHistory();
            }}
          >
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

      // Visibility filter: skip messages that don't match the current mode.
      // The message stays in the map so mode switches show it without re-fetch.
      if (!this.shouldShowMessage(msg)) continue;

      // Grouping: consecutive *visible* messages from same sender within GROUP_WINDOW_MS
      const msgTime = d.getTime();
      const sameSender = msg.sender === prevSender;
      const withinWindow = msgTime - prevTimestamp < GROUP_WINDOW_MS;
      const showHeader = !sameSender || !withinWindow;

      // In v2 mode, determine if from agent by checking sender against known members
      const isFromAgent = this.isV2 ? this.isSenderAgent(msg) : msg.senderId === this.agentId;
      const senderDisplayName = this.isV2
        ? this.getSenderDisplayName(msg)
        : isFromAgent
          ? this.agentName || ''
          : '';

      rows.push(html`
        <scion-chat-message
          body=${msg.msg}
          sender=${msg.sender}
          senderName=${senderDisplayName}
          ?fromAgent=${isFromAgent}
          ?plain=${msg.plain ?? false}
          agentSlug=${isFromAgent ? senderDisplayName : ''}
          timestamp=${msg.createdAt}
          ?showHeader=${showHeader}
          ?urgent=${msg.urgent ?? false}
          ?broadcasted=${msg.broadcasted ?? false}
          channel=${msg.channel || ''}
          visibility=${msg.visibility || 'normal'}
          messageType=${msg.type || ''}
          dispatchState=${msg.dispatchState || ''}
          dispatchFailureReason=${msg.dispatchFailureReason || ''}
          .attachments=${msg.attachments || []}
        ></scion-chat-message>
      `);

      // Render "also notified" footer under the specific message bubble (O3).
      const msgMentionResults = this.mentionResultsByMessageId.get(msg.id);
      if (msgMentionResults) {
        const delivered = msgMentionResults.filter((r) => r.status === 'delivered');
        if (delivered.length > 0) {
          const slugs = delivered.map((r) => html`<span class="mention-slug">@${r.slug}</span>`);
          rows.push(html`
            <div class="mention-results">
              Also notified:
              ${slugs.reduce((acc, s, i) => (i === 0 ? [s] : [...acc, ', ', s]), [] as unknown[])}
            </div>
          `);
        }
      }

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
