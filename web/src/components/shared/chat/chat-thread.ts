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
import './chat-interagent-marker.js';
import './send-to-agent-picker.js';
import type { AgentSelectedDetail } from './send-to-agent-picker.js';

/** Result from server-side mention fan-out. */
interface MentionResult {
  slug: string;
  status: string;
  error?: string;
}

/** Unused — replaced by flat interagentMessages array grouped inline. */

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

/** Typing indicator expiry in ms. */
const TYPING_EXPIRY_MS = 6000;

/**
 * How long the "Seen" indicator stays on screen after the peer read the
 * message. Past this the delivery state is dropped entirely — a permanent
 * receipt on every conversation is noise, not information.
 */
const SEEN_VISIBLE_MS = 5 * 60 * 1000;

/** Typing send throttle in ms. */
const TYPING_SEND_THROTTLE_MS = 4000;

@customElement('scion-chat-thread')
export class ScionChatThread extends LitElement {
  // DEPRECATED(wave-1): agentId-based mode — remove after v2 is stable and flag is permanently ON.
  @property()
  agentId = '';

  // DEPRECATED(wave-1): agentId-based mode — remove after v2 is stable and flag is permanently ON.
  @property()
  agentName = '';

  @property({ type: Boolean })
  canSend = false;

  // DEPRECATED(wave-1): per-agent visibility mode — remove after v2 is stable and flag is permanently ON.
  @property()
  visibilityMode: VisibilityMode = 'conversation';

  /** Whether the visibility toggle is shown in the header. */
  // DEPRECATED(wave-1): visibility toggle — remove after v2 is stable and flag is permanently ON.
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

  /** Current user ID for own-message detection (v2 mode). */
  @property()
  currentUserId = '';

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
  @state() private sending = false;
  @state() private sendError: string | null = null;
  @state() private pinnedToBottom = true;
  @state() private loadingOlder = false;
  @state() private hasOlderMessages = true;
  @state() private loaded = false;
  /** Mention results keyed by message ID (for "also notified" footer per message). */
  @state() private mentionResultsByMessageId = new Map<string, MentionResult[]>();

  /** Raw inter-agent messages to render as inline markers in agent DMs. */
  @state() private interagentMessages: Message[] = [];

  /** Global expand/collapse state for all inter-agent markers. */
  @state() private interagentExpandAll = false;

  /** Whether inter-agent markers are visible (eye toggle). */
  @state() private interagentVisible = true;

  /** W7: Attachment refs keyed by message ID (from history endpoint + send response). */
  private v2AttachmentMap = new Map<string, import('./chat-message.js').AttachmentRefInfo[]>();

  // ---- Phase-3 state ----

  /** Current user's last-read message ID (for unread divider). */
  @state() private lastReadMessageId = '';

  /** Whether the "New messages" divider is currently visible. */
  @state() private showUnreadDivider = false;

  /** Reply-to context for the composer. */
  @state() private composerReplyTo: {
    messageId: string;
    senderName: string;
    content: string;
  } | null = null;

  /** Edit mode context for the composer. */
  @state() private composerEditMessage: {
    messageId: string;
    content: string;
  } | null = null;

  // ---- Phase-5: Context menu + Send-to-agent state ----

  /** The message targeted by the right-click context menu. */
  @state() private contextMenuMessage: Message | null = null;

  /** Position of the right-click context menu. */
  @state() private contextMenuPosition: { x: number; y: number } = { x: 0, y: 0 };

  /** Whether the agent picker is visible. */
  @state() private showAgentPicker = false;

  /** Temporarily stored message for send-to-agent flow. */
  private _pendingSendToAgentMessage: Message | null = null;

  /** Message extensions keyed by message ID. */
  private v2MessageExtMap = new Map<
    string,
    { replyToId?: string; editedAt?: string; deletedAt?: string }
  >();

  /** Reply previews keyed by reply-to message ID. */
  private v2ReplyPreviewMap = new Map<
    string,
    { messageId: string; senderName: string; content: string }
  >();

  /** Bound listener for v2 SSE message-edited events. */
  private _v2EditHandler = this.handleV2MessageEdited.bind(this);

  /** Bound listener for v2 SSE message-deleted events. */
  private _v2DeleteHandler = this.handleV2MessageDeleted.bind(this);

  private eventSource: EventSource | null = null;
  private nextCursor: string | null = null;
  private lastKnownTimestamp: string | null = null;
  private hadError = false;
  private fetchId = 0;

  /** Bound listener for v2 SSE chat-message events via stateManager. */
  private _v2MessageHandler = this.handleV2ChatMessage.bind(this);

  /** Bound listener for v2 SSE typing events via stateManager. */
  private _v2TypingHandler = this.handleV2TypingEvent.bind(this);

  /** Bound listener for v2 SSE read-state events (DM "seen" receipts). */
  private _v2ReadStateHandler = this.handleV2ReadStateEvent.bind(this);

  // ---- DM read receipt ("seen") state ----

  /** The peer's read watermark in this DM: the last message they have read. */
  @state() private peerReadMessageId = '';

  /** When the peer's watermark last advanced, epoch ms. 0 = unknown. */
  @state() private peerReadAt = 0;

  /** Fires when the "Seen" indicator ages out, to drop it from the render. */
  private _seenExpiryTimer: ReturnType<typeof setTimeout> | null = null;

  /** Last message ID POSTed to /read — suppresses redundant watermark writes. */
  private _lastAdvancedMessageId = '';

  // ---- Typing indicator state ----

  /** Map of userId -> { displayName, timer } for active typing indicators. */
  @state() private typingUsers = new Map<
    string,
    { displayName: string; timer: ReturnType<typeof setTimeout> }
  >();

  /** Last time we sent a typing event (for client-side throttle). */
  private _lastTypingSent = 0;

  /** Current user ID, cached from the stateManager scope once it exists. */
  private _currentUserId = '';

  /** Read tracking: debounce timer for advancing watermark. */
  private _readDebounceTimer: ReturnType<typeof setTimeout> | null = null;

  /** Read tracking: whether the tab is focused. */
  private _tabFocused = true;

  /** Backfill single-flight guard: a backfill request is currently running. */
  private _backfillInFlight = false;

  /** Backfill single-flight guard: another backfill was requested while one was running. */
  private _backfillPending = false;

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
      /*
       * flex: 0 0 auto is load-bearing. As a flex item of .messages-scroll the
       * list would otherwise shrink to the scroll container's height (the
       * explicit min-height replaces the automatic minimum), and because the
       * content is bottom-anchored with justify-content: flex-end the overflow
       * lands past the block-START edge — which is unreachable, so the thread
       * cannot be scrolled at all. Keeping the list at its content height makes
       * the overflow land at the bottom, where the scrollbar can reach it.
       */
      flex: 0 0 auto;
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

    /* Unread divider */
    .unread-divider {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.5rem 1rem;
    }

    .unread-divider::before,
    .unread-divider::after {
      content: '';
      flex: 1;
      height: 1px;
      background: var(--scion-primary, #3b82f6);
    }

    .unread-label {
      font-size: 0.6875rem;
      font-weight: 600;
      color: var(--scion-primary, #3b82f6);
      white-space: nowrap;
    }

    /* Permalink highlight animation */
    .permalink-highlight {
      animation: permalink-fade 2s ease-out;
    }

    @keyframes permalink-fade {
      0% { background-color: rgba(59, 130, 246, 0.2); }
      100% { background-color: transparent; }
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

    /* Inter-agent toggle bar */
    .interagent-toggle-bar {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.25rem 1rem;
      border-bottom: 1px solid var(--scion-border, rgba(148, 163, 184, 0.15));
    }

    .interagent-label {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      font-weight: 500;
    }

    .interagent-icons {
      display: flex;
      align-items: center;
      gap: 0.25rem;
    }

    .interagent-icons sl-icon-button::part(base) {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* Typing indicator */
    .typing-indicator {
      display: flex;
      align-items: center;
      gap: 6px;
      padding: 4px 16px;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      min-height: 20px;
    }

    .typing-dots {
      display: inline-flex;
      gap: 2px;
      align-items: center;
    }

    .typing-dots span {
      width: 4px;
      height: 4px;
      border-radius: 50%;
      background: var(--scion-text-muted, #64748b);
      animation: typing-bounce 1.4s ease-in-out infinite;
    }

    .typing-dots span:nth-child(2) {
      animation-delay: 0.2s;
    }

    .typing-dots span:nth-child(3) {
      animation-delay: 0.4s;
    }

    @keyframes typing-bounce {
      0%,
      60%,
      100% {
        transform: translateY(0);
        opacity: 0.4;
      }
      30% {
        transform: translateY(-3px);
        opacity: 1;
      }
    }

    /* Phase-3: Scroll-to-message highlight effect. */
    scion-chat-message.scroll-highlight {
      animation: highlight-flash 2s ease-out;
    }

    @keyframes highlight-flash {
      0%, 20% {
        background: var(--scion-primary-50, #eff6ff);
      }
      100% {
        background: transparent;
      }
    }

    /* Phase-5: Context menu */
    .context-menu-overlay {
      position: fixed;
      inset: 0;
      z-index: 149;
    }

    .context-menu {
      position: fixed;
      z-index: 150;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.5rem;
      box-shadow: 0 4px 16px rgba(0, 0, 0, 0.15);
      min-width: 180px;
      padding: 0.25rem 0;
    }

    .context-menu-item {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      font-size: 0.8125rem;
      cursor: pointer;
      color: var(--scion-text, #1e293b);
      white-space: nowrap;
      transition: background 0.1s;
    }

    .context-menu-item:hover {
      background: var(--scion-primary-50, #eff6ff);
    }

    .context-menu-item sl-icon {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* Phase-5: Slash command system message */
    .system-info-message {
      padding: 0.5rem 1rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: 0.375rem;
      margin: 0.25rem 1rem;
      white-space: pre-wrap;
    }
  `;

  /** Auto-trigger loadHistory when the component first renders in v2 mode. */
  override firstUpdated(): void {
    if (this.isV2) {
      this.loadHistory();
    }
  }

  /**
   * Detect conversationKey changes for v2 mode.
   * When the user switches threads/DMs, the same component instance gets a
   * new conversationKey — we must tear down old state and reload.
   */
  override updated(changedProperties: Map<string, unknown>): void {
    if (
      changedProperties.has('conversationKey') &&
      changedProperties.get('conversationKey') !== undefined
    ) {
      const oldKey = changedProperties.get('conversationKey') as string;
      if (oldKey !== this.conversationKey && this.isV2) {
        this.resetV2State();
        this.loadHistory();
      }
    }
  }

  /** Tear down v2 state so a fresh load can happen. */
  private resetV2State(): void {
    // Stop any active SSE listener
    stateManager.removeEventListener('chat-message-received', this._v2MessageHandler);
    stateManager.removeEventListener('chat-typing-received', this._v2TypingHandler);
    stateManager.removeEventListener('chat-read-state-updated', this._v2ReadStateHandler);
    stateManager.removeEventListener('chat-message-edited', this._v2EditHandler);
    stateManager.removeEventListener('chat-message-deleted', this._v2DeleteHandler);

    // Clear read-receipt state — it belongs to the conversation we just left.
    this.clearSeenState();

    // Clear unread divider state.
    this.lastReadMessageId = '';
    this.showUnreadDivider = false;

    // Clear message state
    this.messageMap.clear();
    this.messages = [];
    this.nextCursor = null;
    this.lastKnownTimestamp = null;
    this.hasOlderMessages = true;
    this.loaded = false;
    this.error = null;
    this.sendError = null;
    this.pinnedToBottom = true;
    this.loadingOlder = false;

    // Clear inter-agent state
    this.interagentMessages = [];
    this.interagentExpandAll = false;
    this.interagentVisible = true;

    // Clear typing state
    for (const entry of this.typingUsers.values()) {
      clearTimeout(entry.timer);
    }
    this.typingUsers = new Map();

    // Clear read tracking timer
    if (this._readDebounceTimer) {
      clearTimeout(this._readDebounceTimer);
      this._readDebounceTimer = null;
    }

    // Increment fetchId to invalidate any in-flight requests
    this.fetchId++;
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopStream();
    // Clean up v2 SSE listeners
    stateManager.removeEventListener('chat-message-received', this._v2MessageHandler);
    stateManager.removeEventListener('chat-typing-received', this._v2TypingHandler);
    stateManager.removeEventListener('chat-read-state-updated', this._v2ReadStateHandler);
    stateManager.removeEventListener('chat-message-edited', this._v2EditHandler);
    stateManager.removeEventListener('chat-message-deleted', this._v2DeleteHandler);
    this.clearSeenState();
    // Clean up typing timers
    for (const entry of this.typingUsers.values()) {
      clearTimeout(entry.timer);
    }
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

  // DEPRECATED(wave-1): agentId-based load path — remove after v2 is stable and flag is permanently ON.
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
        this.scrollToBottomAfterRender();
      }
    }
  }

  /** W7: Get attachment refs for a message (from history or send response). */
  private getMessageAttachmentRefs(
    messageId: string
  ): import('./chat-message.js').AttachmentRefInfo[] {
    return this.v2AttachmentMap.get(messageId) ?? [];
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
    // Also clean up v2 SSE listeners
    if (this.isV2) {
      stateManager.removeEventListener('chat-message-received', this._v2MessageHandler);
      stateManager.removeEventListener('chat-typing-received', this._v2TypingHandler);
      stateManager.removeEventListener('chat-read-state-updated', this._v2ReadStateHandler);
      stateManager.removeEventListener('chat-message-edited', this._v2EditHandler);
      stateManager.removeEventListener('chat-message-deleted', this._v2DeleteHandler);
    }

  }

  // ---------------------------------------------------------------------------
  // Data loading
  // ---------------------------------------------------------------------------

  // DEPRECATED(wave-1): agentId-based load path — remove after v2 is stable and flag is permanently ON.
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
      this.scrollToBottomAfterRender();
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

  // DEPRECATED(wave-1): agentId-based history fetch — remove after v2 is stable and flag is permanently ON.
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
      this.messageMap.set(msg.id, msg);
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

  // DEPRECATED(wave-1): agentId-based SSE stream — remove after v2 is stable and flag is permanently ON.
  private startStream(): void {
    if (!this.isConnected || this.eventSource || !this.agentId) return;

    const url = `/api/v1/agents/${encodeURIComponent(this.agentId)}/messages/stream`;
    this.eventSource = new EventSource(url);

    this.eventSource.addEventListener('message', (event: Event) => {
      try {
        const msg = JSON.parse((event as MessageEvent).data as string) as Message;
        this.mergeMessages([msg]);
        this.scrollToBottomAfterRender();
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
      // Fetch inter-agent exchanges for agent DMs (non-blocking).
      if (this.isAgentDM) {
        void this.fetchInteragentExchanges();
      }
      // Human DMs show a read receipt — seed it so "Seen" survives a reload
      // instead of waiting for the peer's next watermark advance.
      if (this.isHumanDM) {
        void this.fetchPeerReadState();
      }
      // Fetch own read watermark for the unread divider.
      await this.fetchOwnReadState();
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to load messages';
    } finally {
      this.loading = false;
      // Determine scroll target: permalink hash > unread divider > bottom.
      const hashMsgId = this.parseMessageHash();
      if (hashMsgId) {
        this.scrollToMessageById(hashMsgId, true);
      } else if (this.showUnreadDivider) {
        this.scrollToUnreadDivider();
      } else {
        this.scrollToBottomAfterRender();
      }
      // Advance read watermark after a short delay so the user sees the divider.
      if (this.showUnreadDivider && this.messages.length > 0) {
        setTimeout(() => {
          const lastMsg = this.messages[this.messages.length - 1];
          if (lastMsg) {
            void this.advanceReadWatermark(lastMsg.id);
          }
        }, 2000);
      }
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
      messages?: Message[];
      nextCursor?: string;
      messageAttachments?: Record<string, import('./chat-message.js').AttachmentRefInfo[]>;
      messageExtensions?: Record<
        string,
        { messageId: string; replyToId?: string; editedAt?: string; deletedAt?: string }
      >;
      replyPreviews?: Record<
        string,
        { messageId: string; senderName: string; content: string }
      >;
    };

    const items = data?.items ?? data?.messages ?? [];

    // W7: Merge attachment refs from history response.
    if (data?.messageAttachments) {
      for (const [msgId, refs] of Object.entries(data.messageAttachments)) {
        this.v2AttachmentMap.set(msgId, refs);
      }
    }

    // Phase-3: Merge message extensions and reply previews.
    if (data?.messageExtensions) {
      for (const [msgId, ext] of Object.entries(data.messageExtensions)) {
        this.v2MessageExtMap.set(msgId, ext);
      }
    }
    if (data?.replyPreviews) {
      for (const [msgId, preview] of Object.entries(data.replyPreviews)) {
        this.v2ReplyPreviewMap.set(msgId, preview);
      }
    }

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
    stateManager.addEventListener('chat-typing-received', this._v2TypingHandler);
    stateManager.addEventListener('chat-read-state-updated', this._v2ReadStateHandler);
    stateManager.addEventListener('chat-message-edited', this._v2EditHandler);
    stateManager.addEventListener('chat-message-deleted', this._v2DeleteHandler);
    // Seed the typing self-filter. The scope may not exist yet — see selfUserId.
    const scope = stateManager.currentScope;
    if (scope && scope.type === 'chat') {
      this._currentUserId = scope.userId;
      // Also populate currentUserId if not set from the parent.
      if (!this.currentUserId && scope.userId) {
        this.currentUserId = scope.userId;
      }
    }
  }

  /**
   * Who "self" is, for filtering out our own echoed events.
   *
   * The chat scope is only configured once the space rail reports its space
   * IDs, which lands after a thread mounted from a cold load has already
   * subscribed — so resolve it lazily, and fall back to the ID the page passes
   * down. Without this a DM opened directly showed the user their own
   * "X is typing…".
   */
  private selfUserId(): string {
    if (!this._currentUserId) {
      const scope = stateManager.currentScope;
      if (scope && scope.type === 'chat' && scope.userId) {
        this._currentUserId = scope.userId;
      }
    }
    return this._currentUserId || this.currentUserId;
  }

  /** Handle v2 SSE chat message events.
   *
   * If the SSE event carries a full message payload (has `id` and content),
   * merge it directly via `mergeMessages()` instead of doing a full 50-message
   * backfill. Fall back to `backfillV2()` when the event is a lightweight
   * notification (e.g. just a threadId).
   */
  private handleV2ChatMessage(e: Event): void {
    type ChatEventData = {
      threadId?: string;
      conversationKey?: string;
      topicId?: string;
      senderId?: string;
      // Full message fields from UserMessageEvent:
      id?: string;
      msg?: string;
      sender?: string;
      recipient?: string;
      recipientId?: string;
      type?: string;
      projectId?: string;
      agentId?: string;
      createdAt?: string;
      channel?: string;
      visibility?: string;
      groupId?: string;
      dispatchState?: string;
      urgent?: boolean;
      broadcasted?: boolean;
      read?: boolean;
      attachments?: import('./chat-message.js').AttachmentRefInfo[];
    };
    const detail = (e as CustomEvent).detail as
      | ({ data?: ChatEventData } & ChatEventData)
      | undefined;
    // stateManager wraps SSE payloads as { state, data }; tolerate a flat detail too.
    const eventData: ChatEventData | undefined = detail?.data ?? detail;
    if (!eventData) {
      void this.backfillV2();
      return;
    }

    // Filter: only process events for this conversation
    const eventKey = eventData.threadId || eventData.conversationKey || eventData.topicId || '';
    if (eventKey && eventKey !== this.conversationKey) {
      return; // Not for this conversation
    }

    // The sender finished typing the moment their message landed — drop the
    // indicator now rather than waiting out TYPING_EXPIRY_MS.
    this.clearTypingForUser(eventData.senderId);

    // If the event carries a full message payload, merge directly instead of
    // doing a round-trip backfill.
    // SSE events from PublishUserMessage carry the full message payload.
    // mergeMessages() deduplicates by ID (last-write-wins via Map.set),
    // so if both the POST response and the SSE event provide the same
    // message, the later arrival's fields prevail — this is acceptable.
    if (eventData.id && (eventData.msg !== undefined || eventData.type)) {
      const msg: Message = {
        id: eventData.id,
        projectId: eventData.projectId || '',
        sender: eventData.sender || '',
        senderId: eventData.senderId || '',
        recipient: eventData.recipient || '',
        recipientId: eventData.recipientId || '',
        msg: eventData.msg || '',
        type: eventData.type || '',
        agentId: eventData.agentId || '',
        createdAt: eventData.createdAt || new Date().toISOString(),
        ...(eventData.channel != null ? { channel: eventData.channel } : {}),
        ...(eventData.threadId != null ? { threadId: eventData.threadId } : {}),
        ...(eventData.visibility != null ? { visibility: eventData.visibility } : {}),
        ...(eventData.groupId != null ? { groupId: eventData.groupId } : {}),
        ...(eventData.dispatchState != null ? { dispatchState: eventData.dispatchState } : {}),
        ...(eventData.urgent != null ? { urgent: eventData.urgent } : {}),
        ...(eventData.broadcasted != null ? { broadcasted: eventData.broadcasted } : {}),
        ...(eventData.read != null ? { read: eventData.read } : {}),
      };
      // Update attachment map BEFORE mergeMessages so the triggered re-render
      // already sees the refs. v2AttachmentMap is not @state() — writing it
      // after mergeMessages would leave the first render without attachments.
      if (eventData.attachments && eventData.attachments.length > 0) {
        this.v2AttachmentMap.set(msg.id, eventData.attachments);
      }

      this.mergeMessages([msg]);

      this.scrollToBottomAfterRender();
      this.maybeAdvanceReadWatermark();
      return;
    }

    // Lightweight notification (no full message) — fall back to backfill.
    void this.backfillV2();
  }

  /** Drop a user's typing indicator (and its expiry timer), if one is active. */
  private clearTypingForUser(userId: string | undefined): void {
    if (!userId) return;
    const existing = this.typingUsers.get(userId);
    if (!existing) return;
    clearTimeout(existing.timer);
    const updated = new Map(this.typingUsers);
    updated.delete(userId);
    this.typingUsers = updated;
  }

  /** Handle v2 SSE typing events. Only show for this conversation, and skip self. */
  private handleV2TypingEvent(e: Event): void {
    const detail = (e as CustomEvent).detail as {
      data?: { threadId?: string; userId?: string; displayName?: string };
    };
    const eventData = detail?.data || (detail as Record<string, unknown>);
    const threadId = (eventData as Record<string, unknown>).threadId as string | undefined;
    const userId = (eventData as Record<string, unknown>).userId as string | undefined;
    const displayName = (eventData as Record<string, unknown>).displayName as string | undefined;

    if (!threadId || !userId || !displayName) return;

    // Only show for this conversation
    if (threadId !== this.conversationKey) return;

    // Don't show own typing indicator
    if (userId === this.selfUserId()) return;

    // Clear existing timer for this user if any
    const existing = this.typingUsers.get(userId);
    if (existing) {
      clearTimeout(existing.timer);
    }

    // Set a new timer to expire the typing indicator
    const timer = setTimeout(() => {
      const updated = new Map(this.typingUsers);
      updated.delete(userId);
      this.typingUsers = updated;
    }, TYPING_EXPIRY_MS);

    const updated = new Map(this.typingUsers);
    updated.set(userId, { displayName, timer });
    this.typingUsers = updated;
  }

  /** Send a typing event to the server (client-throttled to once per 4s). */
  private sendTypingEvent(): void {
    if (!this.isV2 || !this.conversationKey) return;

    const now = Date.now();
    if (now - this._lastTypingSent < TYPING_SEND_THROTTLE_MS) return;
    this._lastTypingSent = now;

    // Fire and forget — typing is ephemeral, errors are acceptable
    void apiFetch(`/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/typing`, {
      method: 'POST',
    });
  }

  /**
   * Refetch the recent history window. Single-flighted: concurrent callers
   * (a burst of SSE events) collapse into one trailing refetch.
   */
  private async backfillV2(): Promise<void> {
    if (!this.conversationKey) return;
    if (this._backfillInFlight) {
      this._backfillPending = true;
      return;
    }
    this._backfillInFlight = true;
    try {
      await this.runBackfillV2();
    } finally {
      this._backfillInFlight = false;
      if (this._backfillPending) {
        this._backfillPending = false;
        void this.backfillV2();
      }
    }
  }

  private async runBackfillV2(): Promise<void> {
    const currentId = this.fetchId;
    const params = new URLSearchParams({
      limit: String(HISTORY_PAGE_SIZE),
    });

    const res = await apiFetch(
      `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/messages?${params.toString()}`
    );

    if (currentId !== this.fetchId) return;
    if (!res.ok) return;

    const data = (await res.json()) as {
      items?: Message[];
      messages?: Message[];
      messageAttachments?: Record<string, import('./chat-message.js').AttachmentRefInfo[]>;
    };
    const items = data?.items ?? data?.messages ?? [];

    // W7: Merge attachment refs from history response.
    if (data?.messageAttachments) {
      for (const [msgId, refs] of Object.entries(data.messageAttachments)) {
        this.v2AttachmentMap.set(msgId, refs);
      }
    }

    this.mergeMessages(items);
    this.scrollToBottomAfterRender();
    // Advance read watermark if applicable
    this.maybeAdvanceReadWatermark();
  }

  /**
   * Fetch the current user's read watermark for this conversation.
   * Used to position the unread divider on thread open.
   */
  private async fetchOwnReadState(): Promise<void> {
    const currentId = this.fetchId;
    try {
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/read`
      );
      if (!res.ok || currentId !== this.fetchId) return;
      const data = (await res.json()) as {
        lastReadMessageId?: string;
        peerLastReadMessageId?: string;
        peerLastReadAt?: string;
      };
      if (currentId !== this.fetchId) return;
      if (data?.lastReadMessageId) {
        this.lastReadMessageId = data.lastReadMessageId;
        // Show divider if there are messages after the watermark.
        const idx = this.messages.findIndex((m) => m.id === this.lastReadMessageId);
        if (idx >= 0 && idx < this.messages.length - 1) {
          this.showUnreadDivider = true;
        }
      }
    } catch {
      // Non-critical: the divider is a convenience, not essential.
    }
  }

  // ---------------------------------------------------------------------------
  // Inter-agent exchange loading
  // ---------------------------------------------------------------------------

  /** Whether this conversation is an agent DM (eligible for inter-agent markers). */
  private get isAgentDM(): boolean {
    return this.isDM && this.conversationKey.startsWith('dm:agent:');
  }

  /** Whether there are inter-agent markers to render in this conversation. */
  private get hasInteragentMessages(): boolean {
    return this.isAgentDM && this.interagentMessages.length > 0;
  }

  /** Whether this is a human-to-human DM (the only place read receipts apply). */
  private get isHumanDM(): boolean {
    return this.isDM && this.conversationKey.startsWith('dm:user:');
  }

  // ---------------------------------------------------------------------------
  // DM read receipts ("Seen")
  // ---------------------------------------------------------------------------

  /** Load the peer's read watermark for this DM. Best-effort. */
  private async fetchPeerReadState(): Promise<void> {
    const currentId = this.fetchId;
    try {
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/read`
      );
      if (!res.ok || currentId !== this.fetchId) return;
      const data = (await res.json()) as {
        peerLastReadMessageId?: string;
        peerLastReadAt?: string;
      };
      if (currentId !== this.fetchId || !data?.peerLastReadMessageId) return;
      this.applyPeerReadState(data.peerLastReadMessageId, data.peerLastReadAt);
    } catch {
      // Non-critical: the receipt is decoration, not content.
    }
  }

  /** Handle a peer's read-watermark advance arriving over SSE. */
  private handleV2ReadStateEvent(e: Event): void {
    type ReadStateData = { conversationKey?: string; messageId?: string; readAt?: string };
    const detail = (e as CustomEvent).detail as
      | ({ data?: ReadStateData } & ReadStateData)
      | undefined;
    const eventData: ReadStateData | undefined = detail?.data ?? detail;
    if (!eventData?.messageId) return;
    if (eventData.conversationKey !== this.conversationKey) return;
    this.applyPeerReadState(eventData.messageId, eventData.readAt);
  }

  /** Record the peer watermark and arm the auto-hide timer. */
  private applyPeerReadState(messageId: string, readAt?: string): void {
    const parsed = readAt ? new Date(readAt).getTime() : NaN;
    this.peerReadMessageId = messageId;
    this.peerReadAt = Number.isNaN(parsed) ? Date.now() : parsed;

    if (this._seenExpiryTimer) clearTimeout(this._seenExpiryTimer);
    const remaining = this.peerReadAt + SEEN_VISIBLE_MS - Date.now();
    if (remaining > 0) {
      this._seenExpiryTimer = setTimeout(() => {
        this._seenExpiryTimer = null;
        this.requestUpdate();
      }, remaining);
    }
  }

  /** Drop all read-receipt state (conversation switch / teardown). */
  private clearSeenState(): void {
    this.peerReadMessageId = '';
    this.peerReadAt = 0;
    this._lastAdvancedMessageId = '';
    if (this._seenExpiryTimer) {
      clearTimeout(this._seenExpiryTimer);
      this._seenExpiryTimer = null;
    }
  }

  /**
   * Whether the peer's watermark has reached this message.
   *
   * Message IDs are UUIDs, so they cannot be compared for ordering — the
   * watermark message is looked up in the buffer and compared by timestamp.
   * If it is not buffered (scrolled out of the window), we report "not seen"
   * rather than guess.
   */
  /** ID of the newest message sent by the current user, '' if none. */
  private lastOwnMessageId(): string {
    if (!this.currentUserId) return '';
    for (let i = this.messages.length - 1; i >= 0; i--) {
      const msg = this.messages[i];
      if (msg.senderId === this.currentUserId && !SYSTEM_MESSAGE_TYPES.has(msg.type)) {
        return msg.id;
      }
    }
    return '';
  }

  /**
   * Delivery state to render for a message.
   *
   * Only the newest own message shows a receipt, and it disappears once the
   * "Seen" indicator has aged out. Failures are the exception: they stay
   * visible on every message, because a silently dropped message is exactly
   * what the user needs to be told about.
   */
  private deliveryStateFor(msg: Message, lastOwnMessageId: string, seenExpired: boolean): string {
    const dispatchState = msg.dispatchState || '';
    if (!dispatchState || dispatchState === 'failed') return dispatchState;
    if (msg.id !== lastOwnMessageId) return '';
    if (seenExpired && this.isMessageSeen(msg)) return '';
    return dispatchState;
  }

  private isMessageSeen(msg: Message): boolean {
    if (!this.peerReadMessageId) return false;
    const watermark = this.messageMap.get(this.peerReadMessageId);
    if (!watermark) return false;
    return new Date(msg.createdAt).getTime() <= new Date(watermark.createdAt).getTime();
  }

  /** Fetch inter-agent messages for inline markers. Stores the raw flat list. */
  private async fetchInteragentExchanges(): Promise<void> {
    if (!this.isAgentDM) return;

    const params = new URLSearchParams({ limit: '200' });
    const currentId = this.fetchId;

    try {
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/interagent?${params.toString()}`
      );
      if (!res.ok || currentId !== this.fetchId) return;

      const data = (await res.json()) as { messages?: Message[] };
      if (currentId !== this.fetchId) return;
      const msgs = data?.messages ?? [];
      // Store sorted flat list — grouping by DM gaps happens in renderMessages().
      this.interagentMessages = [...msgs].sort(
        (a, b) => new Date(a.createdAt).getTime() - new Date(b.createdAt).getTime()
      );
    } catch {
      // Non-critical
    }
  }

  /** Send a message in v2 mode. */
  private async handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void> {
    const { text, mentions, attachmentIds, replyToId, onSuccess } = e.detail;
    const hasContent = text.length > 0 || (attachmentIds && attachmentIds.length > 0);
    if (!hasContent || this.sending) return;

    // Check for /default slash command
    if (text.startsWith('/default ')) {
      await this.handleDefaultCommand(text);
      onSuccess();
      return;
    }

    this.sending = true;
    this.sendError = null;

    try {
      // Generate an idempotency key so duplicate sends (e.g. network retry)
      // are collapsed server-side.
      const idempotencyKey = crypto.randomUUID();
      const body: Record<string, unknown> = {
        content: text,
        idempotency_key: idempotencyKey,
      };
      if (mentions && mentions.length > 0) {
        body.mentions = mentions;
      }
      // W7: Include attachment IDs.
      if (attachmentIds && attachmentIds.length > 0) {
        body.attachments = attachmentIds;
      }
      // Phase-3: Include reply_to_id.
      if (replyToId) {
        body.reply_to_id = replyToId;
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
        // W7: Parse attachment refs from the send response.
        const resData = (await res.json().catch(() => null)) as {
          id?: string;
          attachments?: import('./chat-message.js').AttachmentRefInfo[];
        } | null;
        if (resData?.id && resData?.attachments && resData.attachments.length > 0) {
          this.v2AttachmentMap.set(resData.id, resData.attachments);
        }
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
        body.defaultAgent = '';
      } else {
        body.defaultAgent = arg;
      }
      const res = await apiFetch(
        `/api/v1/chat/topics/${encodeURIComponent(this.conversationKey)}`,
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

  /** Handle default-agent-change from the composer dropdown. */
  private async handleDefaultAgentChange(e: CustomEvent<{ defaultAgent: string }>): Promise<void> {
    const newDefault = e.detail.defaultAgent;
    if (!this.conversationKey || this.isDM) return;
    const currentId = this.fetchId;

    try {
      const body: Record<string, unknown> = {
        defaultAgent: newDefault,
      };
      const res = await apiFetch(
        `/api/v1/chat/topics/${encodeURIComponent(this.conversationKey)}`,
        {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        }
      );
      // A conversation switch mid-flight makes this response irrelevant: the
      // default agent now belongs to a topic we are no longer showing.
      if (res.ok && currentId === this.fetchId) {
        this.defaultAgent = newDefault;
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

  // ---------------------------------------------------------------------------
  // Phase-3: Message action handlers
  // ---------------------------------------------------------------------------

  /** Handle reply action from a message. Sets the composer reply-to context. */
  private handleMessageReply(
    e: CustomEvent<{ messageId: string; senderName: string; content: string }>
  ): void {
    this.composerEditMessage = null; // Cancel any pending edit
    this.composerReplyTo = {
      messageId: e.detail.messageId,
      senderName: e.detail.senderName,
      content:
        e.detail.content.length > 100
          ? e.detail.content.slice(0, 100) + '...'
          : e.detail.content,
    };
  }

  /** Handle edit action from a message. Sets the composer edit mode. */
  private handleMessageEditRequest(
    e: CustomEvent<{ messageId: string; content: string }>
  ): void {
    this.composerReplyTo = null; // Cancel any pending reply
    this.composerEditMessage = {
      messageId: e.detail.messageId,
      content: e.detail.content,
    };
  }

  /** Handle delete action from a message. Shows confirmation and calls API. */
  private async handleMessageDeleteRequest(
    e: CustomEvent<{ messageId: string }>
  ): Promise<void> {
    const { messageId } = e.detail;
    const confirmed = window.confirm('Delete this message? This cannot be undone.');
    if (!confirmed) return;

    try {
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/messages/${encodeURIComponent(messageId)}`,
        { method: 'DELETE' }
      );
      if (!res.ok) {
        const errMsg = await extractApiError(res, 'Failed to delete message');
        this.sendError = errMsg;
      }
      // SSE event will update the message state.
    } catch (err) {
      this.sendError = err instanceof Error ? err.message : 'Failed to delete message';
    }
  }

  /** Handle chat-edit event from the composer. Calls PUT endpoint. */
  private async handleChatEditV2(
    e: CustomEvent<{ messageId: string; text: string }>
  ): Promise<void> {
    const { messageId, text } = e.detail;
    if (!text.trim()) return;

    try {
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/messages/${encodeURIComponent(messageId)}`,
        {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ content: text }),
        }
      );
      if (!res.ok) {
        const errMsg = await extractApiError(res, 'Failed to edit message');
        this.sendError = errMsg;
      }
      // SSE event will update the message state.
    } catch (err) {
      this.sendError = err instanceof Error ? err.message : 'Failed to edit message';
    }
  }

  /** Handle scroll-to-message from reply preview click. */
  private handleScrollToMessage(e: CustomEvent<{ messageId: string }>): void {
    this.scrollToMessageById(e.detail.messageId);
  }

  /** SSE handler for message-edited events. */
  private handleV2MessageEdited(e: Event): void {
    const detail = (e as CustomEvent).detail as
      | ({ data?: { conversationKey?: string; messageId?: string; content?: string; editedAt?: string } }
        & { conversationKey?: string; messageId?: string; content?: string; editedAt?: string })
      | undefined;
    // stateManager wraps SSE payloads as { state, data }; unwrap like handleV2ChatMessage.
    const eventData = detail?.data ?? detail;
    if (!eventData || eventData.conversationKey !== this.conversationKey) return;

    const messageId = eventData.messageId;
    const content = eventData.content;
    const editedAt = eventData.editedAt;
    if (!messageId) return;

    // Update the message in messageMap.
    const msg = this.messageMap.get(messageId);
    if (msg) {
      msg.msg = content ?? msg.msg;
      // Update the extension map.
      const ext = this.v2MessageExtMap.get(messageId) || {};
      if (editedAt) ext.editedAt = editedAt;
      this.v2MessageExtMap.set(messageId, ext);
      // Force re-render by cloning messages array.
      this.messages = [...this.messages];
    }
  }

  /** SSE handler for message-deleted events. */
  private handleV2MessageDeleted(e: Event): void {
    const detail = (e as CustomEvent).detail as
      | ({ data?: { conversationKey?: string; messageId?: string; deletedAt?: string } }
        & { conversationKey?: string; messageId?: string; deletedAt?: string })
      | undefined;
    // stateManager wraps SSE payloads as { state, data }; unwrap like handleV2ChatMessage.
    const eventData = detail?.data ?? detail;
    if (!eventData || eventData.conversationKey !== this.conversationKey) return;

    const messageId = eventData.messageId;
    const deletedAt = eventData.deletedAt;
    if (!messageId) return;

    // Update the extension map with deletedAt.
    const ext = this.v2MessageExtMap.get(messageId) || {};
    if (deletedAt) ext.deletedAt = deletedAt;
    this.v2MessageExtMap.set(messageId, ext);
    // Force re-render.
    this.messages = [...this.messages];
  }

  /** Check if any agent has sent a message after the given message. */
  private hasAgentReplyAfter(msg: Message): boolean {
    const msgIdx = this.messages.indexOf(msg);
    if (msgIdx < 0) return false;
    for (let i = msgIdx + 1; i < this.messages.length; i++) {
      if (this.messages[i].sender?.startsWith('agent:')) {
        return true;
      }
    }
    return false;
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
    // The watermark only moves forward and the scroll/focus triggers fire far
    // more often than it changes; re-POSTing the same ID would also re-fan the
    // read-state event out to the peer for nothing.
    if (!messageId || messageId === this._lastAdvancedMessageId) return;
    this._lastAdvancedMessageId = messageId;

    // Pin both to the conversation this POST is for: a switch mid-flight makes
    // the response belong to a thread we are no longer showing.
    const currentId = this.fetchId;
    const conversationKey = this.conversationKey;

    try {
      // Field name must match the server contract in handleConversationRead.
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(conversationKey)}/read`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ messageId }),
        }
      );
      if (currentId !== this.fetchId) return;

      if (!res.ok) {
        // Let the next trigger retry: the watermark did not actually move.
        this._lastAdvancedMessageId = '';
        console.warn('Failed to update read state:', res.status);
        return;
      }

      // The rail and the DM list own their own unread badges and have no way
      // to learn the watermark moved — tell them.
      this.dispatchEvent(
        new CustomEvent('read-state-updated', {
          detail: { conversationKey, messageId },
          bubbles: true,
          composed: true,
        })
      );
    } catch {
      // Non-critical
      if (currentId === this.fetchId) {
        this._lastAdvancedMessageId = '';
      }
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

  /**
   * Scroll to the newest message once the pending render has committed.
   *
   * scrollToBottom() reads scrollHeight synchronously, so calling it directly
   * after a load reads the height of the still-empty (or stale) container and
   * leaves the user parked at the top of the real list (#1028). Awaiting
   * updateComplete lets Lit commit the newly loaded messages first.
   *
   * The deferred scroll respects pinnedToBottom: if the user scrolled away
   * while the load was in flight, it is skipped rather than yanking them back.
   *
   * updateComplete rejects when a reactive update throws, so the chain is
   * caught: a failed render should not also surface as an unhandled rejection,
   * and there is nothing to scroll to in that case anyway.
   */
  private scrollToBottomAfterRender(): void {
    void this.updateComplete
      .then(() => {
        if (!this.pinnedToBottom) return;
        this.scrollToBottom();
      })
      .catch(() => {});
  }

  private handleJumpToLatest(): void {
    this.pinnedToBottom = true;
    this.scrollToBottomAfterRender();
  }

  /**
   * Parse `#msg-{id}` from the URL hash.
   * Returns the message ID or empty string if no match.
   */
  private parseMessageHash(): string {
    const hash = window.location.hash;
    const match = hash.match(/^#msg-(.+)$/);
    return match ? decodeURIComponent(match[1]) : '';
  }

  /**
   * Scroll to a specific message by ID, with optional highlight animation.
   * Can be called externally (e.g. from search navigation on same conversation).
   */
  scrollToMessageById(messageId: string, highlight = true): void {
    void this.updateComplete.then(() => {
      const scrollEl = this.shadowRoot?.querySelector('.messages-scroll');
      if (!scrollEl) return;
      const msgEl = scrollEl.querySelector(`#msg-${messageId}`);
      if (!msgEl) return;
      msgEl.scrollIntoView({ behavior: 'smooth', block: 'center' });
      if (highlight) {
        msgEl.classList.add('permalink-highlight');
        setTimeout(() => msgEl.classList.remove('permalink-highlight'), 2000);
      }
    });
  }

  /** Scroll to the unread divider after render. */
  private scrollToUnreadDivider(): void {
    void this.updateComplete.then(() => {
      const divider = this.shadowRoot?.querySelector('.unread-divider');
      if (divider) {
        divider.scrollIntoView({ behavior: 'auto', block: 'center' });
      } else {
        // Fallback: scroll to bottom if divider not found.
        this.scrollToBottom();
      }
    });
  }

  /** Handle copy-link event from a message's action bar. */
  private handleCopyLink(e: CustomEvent<{ messageId: string }>): void {
    const { messageId } = e.detail;
    const url = `${window.location.origin}${window.location.pathname}#msg-${encodeURIComponent(messageId)}`;
    navigator.clipboard.writeText(url).catch(() => {
      // Fallback: ignore clipboard failure silently.
    });
  }

  // ---------------------------------------------------------------------------
  // Phase-5: Context menu + Send-to-agent
  // ---------------------------------------------------------------------------

  /** Render the context menu overlay when a message is right-clicked. */
  private renderContextMenu() {
    if (!this.contextMenuMessage) return nothing;

    return html`
      <div class="context-menu-overlay" @click=${this.closeContextMenu}></div>
      <div
        class="context-menu"
        style="left: ${this.contextMenuPosition.x}px; top: ${this.contextMenuPosition.y}px;"
      >
        <div class="context-menu-item" @click=${this.handleSendToAgent}>
          <sl-icon name="send"></sl-icon>
          Send to Agent...
        </div>
      </div>
    `;
  }

  /** Handle right-click on a message to show context menu. */
  private handleMessageContextMenu(e: MouseEvent, msg: Message): void {
    e.preventDefault();
    this.contextMenuMessage = msg;
    this.contextMenuPosition = { x: e.clientX, y: e.clientY };
    // Close agent picker if open
    this.showAgentPicker = false;
  }

  /** Close the context menu. */
  private closeContextMenu(): void {
    this.contextMenuMessage = null;
  }

  /** Handle "Send to Agent..." click from context menu. */
  private handleSendToAgent(): void {
    // Close context menu and show agent picker
    const pos = { ...this.contextMenuPosition };
    this.contextMenuPosition = pos;
    this.showAgentPicker = true;
    // Keep contextMenuMessage so we have the message content
    // but close the visual context menu
    const msg = this.contextMenuMessage;
    this.contextMenuMessage = null;
    // Re-store message for the picker callback
    this._pendingSendToAgentMessage = msg;
  }

  /** Handle agent selection from the picker. */
  private handleAgentSelected(e: CustomEvent<AgentSelectedDetail>): void {
    const { agentId } = e.detail;
    const msg = this._pendingSendToAgentMessage;
    this._pendingSendToAgentMessage = null;
    this.showAgentPicker = false;

    if (!msg) return;

    // Store context in sessionStorage to avoid URL length overflow (msg.msg
    // can be 16 000 runes → 48 KB+ when percent-encoded).
    const contextKey = `scion-send-ctx-${crypto.randomUUID().slice(0, 8)}`;
    sessionStorage.setItem(contextKey, msg.msg);
    const url = `/chat/dm/agent/${encodeURIComponent(agentId)}?ctx=${contextKey}`;
    const navEvent = new CustomEvent('navigate', {
      detail: { url },
      bubbles: true,
      composed: true,
      cancelable: true,
    });
    this.dispatchEvent(navEvent);
    if (!navEvent.defaultPrevented) {
      window.location.href = url;
    }
  }

  // ---------------------------------------------------------------------------
  // Phase-5: Slash command handling
  // ---------------------------------------------------------------------------

  /** Handle slash commands dispatched from the composer. */
  private async handleSlashCommand(
    e: CustomEvent<{ command: string; args: string }>
  ): Promise<void> {
    const { command, args } = e.detail;

    switch (command) {
      case 'status':
        await this.handleSlashStatus();
        break;
      case 'clear':
        this.handleSlashClear();
        break;
      case 'help':
        this.handleSlashHelp();
        break;
      case 'spawn':
        await this.handleSlashSpawn(args);
        break;
      case 'stop':
        await this.handleSlashStop(args);
        break;
      case 'default':
        await this.handleDefaultCommand(`/default ${args}`);
        break;
      default:
        this.insertLocalSystemMessage(`Unknown command: /${command}`);
        break;
    }
  }

  /** /status — Fetch agent status for the project. */
  private async handleSlashStatus(): Promise<void> {
    if (!this.projectId) {
      this.insertLocalSystemMessage('No project context available.');
      return;
    }

    try {
      const res = await apiFetch(
        `/api/v1/agents?project=${encodeURIComponent(this.projectId)}`
      );
      if (!res.ok) {
        this.insertLocalSystemMessage('Failed to fetch project status.');
        return;
      }
      const data = (await res.json()) as { items?: Agent[] };
      const agents = data?.items ?? [];

      if (agents.length === 0) {
        this.insertLocalSystemMessage('No agents found in this project.');
        return;
      }

      const lines = agents.map((a) => {
        const slug = a.slug || a.name || 'unknown';
        const phase = a.phase || 'unknown';
        return `  ${slug}: ${phase}`;
      });
      this.insertLocalSystemMessage(`Project agents:\n${lines.join('\n')}`);
    } catch {
      this.insertLocalSystemMessage('Failed to fetch project status.');
    }
  }

  /** /clear — Clear messages locally. */
  private handleSlashClear(): void {
    this.messageMap.clear();
    this.messages = [];
    this.interagentMessages = [];
    this.insertLocalSystemMessage('Conversation cleared.');
  }

  /** /help — List available commands. */
  private handleSlashHelp(): void {
    const helpText = [
      'Available commands:',
      '  /status — Show project agent status',
      '  /clear — Clear the conversation view',
      '  /help — Show this help message',
      '  /spawn <template> — Spawn a new agent from a template',
      '  /stop <agent> — Stop a running agent',
      '  /default <agent|clear> — Set or clear the thread default agent',
    ].join('\n');
    this.insertLocalSystemMessage(helpText);
  }

  /** /spawn <template> — Spawn a new agent. */
  private async handleSlashSpawn(args: string): Promise<void> {
    const template = args.trim();
    if (!template) {
      this.insertLocalSystemMessage('Usage: /spawn <template>');
      return;
    }

    try {
      const body: Record<string, unknown> = {
        template,
        project_id: this.projectId,
      };
      const res = await apiFetch('/api/v1/agents', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!res.ok) {
        const errMsg = await extractApiError(res, 'Failed to spawn agent');
        this.insertLocalSystemMessage(`Failed to spawn agent: ${errMsg}`);
        return;
      }

      const data = (await res.json()) as { name?: string; slug?: string };
      const name = data?.slug || data?.name || template;
      this.insertLocalSystemMessage(`Agent "${name}" spawned successfully.`);
    } catch (err) {
      this.insertLocalSystemMessage(
        `Failed to spawn agent: ${err instanceof Error ? err.message : 'unknown error'}`
      );
    }
  }

  /** /stop <agent> — Stop a running agent. */
  private async handleSlashStop(args: string): Promise<void> {
    const agentSlug = args.trim();
    if (!agentSlug) {
      this.insertLocalSystemMessage('Usage: /stop <agent-slug>');
      return;
    }

    try {
      const res = await apiFetch(
        `/api/v1/agents/${encodeURIComponent(agentSlug)}`,
        { method: 'DELETE' }
      );

      if (!res.ok) {
        const errMsg = await extractApiError(res, 'Failed to stop agent');
        this.insertLocalSystemMessage(`Failed to stop agent: ${errMsg}`);
        return;
      }

      this.insertLocalSystemMessage(`Agent "${agentSlug}" stop requested.`);
    } catch (err) {
      this.insertLocalSystemMessage(
        `Failed to stop agent: ${err instanceof Error ? err.message : 'unknown error'}`
      );
    }
  }

  /** Insert a local-only system message into the thread. */
  private insertLocalSystemMessage(text: string): void {
    const localMsg: Message = {
      id: `local-${Date.now()}-${Math.random().toString(36).slice(2)}`,
      projectId: this.projectId,
      sender: 'system',
      senderId: '',
      recipient: '',
      recipientId: '',
      msg: text,
      type: 'system',
      agentId: '',
      createdAt: new Date().toISOString(),
    };
    this.mergeMessages([localMsg]);
    this.scrollToBottomAfterRender();
  }

  /** Focus the composer textarea when clicking the message area background. */
  private handleMessageAreaClick(e: MouseEvent): void {
    const target = e.target as HTMLElement;
    // Don't steal focus from interactive elements or message content
    if (
      target.closest(
        'a, button, input, textarea, sl-menu-item, sl-dropdown, scion-chat-message, scion-chat-system-line, scion-chat-interagent-marker'
      )
    ) {
      return;
    }
    const composer = this.shadowRoot?.querySelector('scion-chat-composer');
    if (composer) {
      const slTextarea = (composer as LitElement).shadowRoot?.querySelector('sl-textarea');
      if (slTextarea) {
        (slTextarea as HTMLElement).focus();
      }
    }
  }

  // ---------------------------------------------------------------------------
  // Send message
  // ---------------------------------------------------------------------------

  // DEPRECATED(wave-1): agentId-based send — remove after v2 is stable and flag is permanently ON.
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
        ${this.renderStreamBar()}
        ${this.renderInteragentToggle()}
        ${this.renderContent()} ${this.renderTypingIndicator()}
        ${this.sendError ? html`<div class="send-error">${this.sendError}</div>` : nothing}
        <scion-chat-composer
          ?disabled=${this.sending}
          .agents=${this.agents}
          .members=${this.members}
          .defaultAgent=${this.defaultAgent}
          .conversationMode=${this.isDM ? 'dm' : 'thread'}
          .peerName=${this.peerName}
          .projectId=${this.projectId}
          .conversationKey=${this.conversationKey}
          .replyTo=${this.composerReplyTo}
          .editMessage=${this.composerEditMessage}
          @chat-send=${this.handleChatSendV2}
          @chat-edit=${this.handleChatEditV2}
          @chat-typing=${() => this.sendTypingEvent()}
          @default-agent-change=${this.handleDefaultAgentChange}
          @chat-slash-command=${this.handleSlashCommand}
        ></scion-chat-composer>
        ${this.renderContextMenu()}
        <scion-send-to-agent-picker
          .agents=${this.agents}
          ?open=${this.showAgentPicker}
          .posX=${this.contextMenuPosition.x}
          .posY=${this.contextMenuPosition.y}
          @agent-selected=${this.handleAgentSelected}
        ></scion-send-to-agent-picker>
      </div>
    `;
  }

  /** Render the toolbar with label + eye (show/hide) + expand/collapse icons. */
  private renderInteragentToggle() {
    if (!this.hasInteragentMessages) return nothing;

    return html`
      <div class="interagent-toggle-bar">
        <span class="interagent-label">Agent-agent messages:</span>
        <div class="interagent-icons">
          <sl-tooltip content=${this.interagentVisible ? 'Hide' : 'Show'}>
            <sl-icon-button
              name=${this.interagentVisible ? 'eye' : 'eye-slash'}
              label=${this.interagentVisible ? 'Hide agent messages' : 'Show agent messages'}
              @click=${this.toggleInteragentVisibility}
            ></sl-icon-button>
          </sl-tooltip>
          <sl-tooltip content=${this.interagentExpandAll ? 'Collapse all' : 'Expand all'}>
            <sl-icon-button
              name=${this.interagentExpandAll ? 'chevron-up' : 'chevron-down'}
              label=${this.interagentExpandAll ? 'Collapse all' : 'Expand all'}
              @click=${this.toggleAllInteragent}
            ></sl-icon-button>
          </sl-tooltip>
        </div>
      </div>
    `;
  }

  /** Toggle visibility of all inter-agent markers. */
  private toggleInteragentVisibility(): void {
    this.interagentVisible = !this.interagentVisible;
  }

  /** Toggle all inter-agent markers expanded/collapsed. */
  private toggleAllInteragent(): void {
    this.interagentExpandAll = !this.interagentExpandAll;
  }

  /** Render the typing indicator below messages, above the composer. */
  private renderTypingIndicator() {
    if (this.typingUsers.size === 0) return nothing;

    const names = Array.from(this.typingUsers.values()).map((v) => v.displayName);
    let text: string;
    if (names.length === 1) {
      text = `${names[0]} is typing...`;
    } else if (names.length === 2) {
      text = `${names[0]} and ${names[1]} are typing...`;
    } else {
      text = `${names[0]} and ${names.length - 1} others are typing...`;
    }

    return html`
      <div class="typing-indicator">
        <span class="typing-dots"> <span></span><span></span><span></span> </span>
        <span class="typing-text">${text}</span>
      </div>
    `;
  }

  private renderStreamBar() {
    // Show the bar only when the visibility toggle is visible.
    if (!this.showVisibilityToggle) return nothing;
    return html`
      <div class="stream-bar">
        <span class="stream-indicator"></span>
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

    // A conversation with no direct messages is not necessarily empty: an agent
    // DM can carry inter-agent exchanges, which renderMessages() emits as
    // markers. Only show the empty state when there is nothing at all to render.
    if (this.messages.length === 0 && !this.hasInteragentMessages) {
      return html`
        <div class="state-msg">
          <sl-icon name="chat-dots"></sl-icon>
          <span>No messages yet. Start a conversation!</span>
        </div>
      `;
    }

    return html`
      <div class="messages-scroll" @scroll=${this.handleScroll} @click=${this.handleMessageAreaClick}>
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

    // Pre-sort inter-agent messages by time for gap-based grouping.
    const iaMessages = [...this.interagentMessages].sort(
      (a, b) => new Date(a.createdAt).getTime() - new Date(b.createdAt).getTime()
    );
    let iaIdx = 0;
    const hasIA = this.hasInteragentMessages;

    // Delivery state is a property of the conversation's tail, not of every
    // bubble: only the newest message this user sent carries it.
    const lastOwnMessageId = this.lastOwnMessageId();
    const seenExpired = this.peerReadAt > 0 && Date.now() - this.peerReadAt > SEEN_VISIBLE_MS;

    // Unread divider: find the position of the last-read message so we can
    // insert the divider after it.
    let unreadDividerInserted = false;
    let lastReadIdx = -1;
    if (this.showUnreadDivider && this.lastReadMessageId) {
      lastReadIdx = this.messages.findIndex((m) => m.id === this.lastReadMessageId);
    }

    for (let mi = 0; mi < this.messages.length; mi++) {
      const msg = this.messages[mi];
      const d = new Date(msg.createdAt);
      const dateStr = d.toLocaleDateString('en', {
        year: 'numeric',
        month: 'short',
        day: 'numeric',
      });

      // Collect all inter-agent messages that fall before this DM message
      // and insert ONE pill for the entire group.
      if (hasIA) {
        const msgTime = d.getTime();
        const pendingIA: Message[] = [];
        while (iaIdx < iaMessages.length && new Date(iaMessages[iaIdx].createdAt).getTime() < msgTime) {
          pendingIA.push(iaMessages[iaIdx]);
          iaIdx++;
        }
        if (pendingIA.length > 0) {
          rows.push(html`
            <scion-chat-interagent-marker
              .messageCount=${pendingIA.length}
              .messages=${pendingIA}
              ?global-expanded=${this.interagentExpandAll}
              ?hidden=${!this.interagentVisible}
            ></scion-chat-interagent-marker>
          `);
          // Reset grouping after a marker so the next message shows its header.
          prevSender = '';
          prevTimestamp = 0;
        }
      }

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

      // Unread divider: insert between the last-read message and the next one.
      if (!unreadDividerInserted && lastReadIdx >= 0 && mi > lastReadIdx) {
        unreadDividerInserted = true;
        rows.push(html`
          <div class="unread-divider">
            <span class="unread-label">New messages</span>
          </div>
        `);
        // Reset grouping so the first unread message shows its header.
        prevSender = '';
        prevTimestamp = 0;
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

      // In v2 mode, use currentUserId to determine own vs. others' messages.
      // Own messages (fromAgent=false): right-aligned, no header/avatar.
      // Others' messages — both users and agents (fromAgent=true): left-aligned with header/avatar.
      const isFromAgent = this.isV2
        ? this.currentUserId
          ? msg.senderId !== this.currentUserId
          : this.isSenderAgent(msg)
        : msg.senderId === this.agentId;
      // Routing is a property of the individual message, not the current UI
      // default-agent state. Use the per-message `recipient` field (set at
      // send time) so historical messages without a default agent don't
      // retroactively show a routing header.
      const isAgentSender = this.isSenderAgent(msg);
      const msgRoutedTo = !isAgentSender && msg.recipient
        ? (msg.recipient.startsWith('agent:') ? msg.recipient.slice(6) : msg.recipient)
        : '';
      const senderDisplayName = this.isV2
        ? this.getSenderDisplayName(msg)
        : isFromAgent
          ? this.agentName || ''
          : '';

      // Phase-3: Get extension data for this message.
      const ext = this.v2MessageExtMap.get(msg.id);
      const replyPreview = ext?.replyToId
        ? this.v2ReplyPreviewMap.get(ext.replyToId) ?? null
        : null;
      const isOwnMessage = msg.senderId === this.currentUserId;
      // Guard: can edit/delete only if no agent has replied after this message.
      const canEditDelete = isOwnMessage && !this.hasAgentReplyAfter(msg);

      rows.push(html`
        <scion-chat-message
          @contextmenu=${(e: MouseEvent) => this.handleMessageContextMenu(e, msg)}
          id="msg-${msg.id}"
          body=${msg.msg}
          sender=${msg.sender}
          senderId=${msg.senderId || ''}
          senderName=${senderDisplayName}
          ?fromAgent=${isFromAgent}
          ?plain=${msg.plain ?? false}
          agentSlug=${isFromAgent ? senderDisplayName : ''}
          timestamp=${msg.createdAt}
          .showHeader=${showHeader}
          ?urgent=${msg.urgent ?? false}
          ?broadcasted=${msg.broadcasted ?? false}
          channel=${msg.channel || ''}
          visibility=${msg.visibility || 'normal'}
          messageType=${msg.type || ''}
          dispatchState=${this.deliveryStateFor(msg, lastOwnMessageId, seenExpired)}
          ?seen=${msg.id === lastOwnMessageId && this.isMessageSeen(msg)}
          dispatchFailureReason=${msg.dispatchFailureReason || ''}
          .attachments=${msg.attachments || []}
          .attachmentRefs=${this.getMessageAttachmentRefs(msg.id)}
          routedTo=${msgRoutedTo}
          messageId=${msg.id}
          ?isOwn=${isOwnMessage}
          ?canEdit=${canEditDelete}
          ?canDelete=${canEditDelete}
          .replyPreview=${replyPreview}
          editedAt=${ext?.editedAt || ''}
          deletedAt=${ext?.deletedAt || ''}
          @message-reply=${this.handleMessageReply}
          @message-edit=${this.handleMessageEditRequest}
          @message-delete=${this.handleMessageDeleteRequest}
          @message-copy-link=${this.handleCopyLink}
          @scroll-to-message=${this.handleScrollToMessage}
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

    // Append any remaining inter-agent messages that come after all DM messages.
    if (hasIA && iaIdx < iaMessages.length) {
      const trailingIA = iaMessages.slice(iaIdx);
      rows.push(html`
        <scion-chat-interagent-marker
          .messageCount=${trailingIA.length}
          .messages=${trailingIA}
          ?global-expanded=${this.interagentExpandAll}
          ?hidden=${!this.interagentVisible}
        ></scion-chat-interagent-marker>
      `);
    }

    return rows;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-thread': ScionChatThread;
  }
}
