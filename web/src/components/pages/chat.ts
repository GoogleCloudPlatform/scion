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
 * Chat page component — top-level chat mode.
 *
 * Wave-2 Architecture (default, web.native_chat_v2 ON):
 *
 * This is the primary entry point for Native Chat. Wave-2 adds shared spaces
 * (one per project), multi-participant threads, DMs (agent and human),
 * a members sidebar with presence indicators, typing indicators,
 * notifications, file attachments, and message search.
 *
 * Key design decisions:
 * - **Dual-dialect store**: webchat_topic, webchat_read_state, webchat_dm,
 *   webchat_user_prefs tables (SQLite + Postgres) for chat-specific state.
 *   Messages live in the existing messages table with ThreadID as the routing key.
 * - **One persistence path**: messages are persisted once via the hub's
 *   inprocess spoke; the web channel bus only updates watermarks.
 * - **SSE via stateManager**: a single multiplexed SSE connection per client
 *   replaces per-thread EventSource streams. Events are project-scoped.
 * - **Feature flag**: `web.native_chat_v2` (default ON as of W9). Setting
 *   it OFF reverts to the wave-1 agent-per-thread UI for rollback safety.
 *
 * Renders inside `<scion-chat-shell>` and supports two modes:
 *
 * **V1 (web.native_chat_v2 OFF):**
 * - Thread rail listing agents with last-message preview and unread dot
 * - `/chat` shows the rail with no thread selected
 * - `/chat/:agentId` opens the thread for that agent
 *
 * **V2 (web.native_chat_v2 ON):**
 * - Space rail (chat-space-rail) with project grouping, threads, DMs
 * - Conversation view keyed by conversationKey (topic UUID or DM key)
 * - Routes: `/chat`, `/chat/space/{projectId}`, `/chat/space/{projectId}/thread/{topicId}`, `/chat/dm/{key}`
 * - Members sidebar with presence, typing indicators, search panel
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Capabilities, Agent } from '../../shared/types.js';
import { can } from '../../shared/types.js';
import { apiFetch } from '../../client/api.js';
import { navigateTo, stateManager } from '../../client/main.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { isFeatureEnabled, NATIVE_CHAT_V2_FLAG } from '../../utils/feature-flags.js';
import { hashColor, getInitials } from '../shared/chat/chat-avatar.js';
import '../shared/chat/chat-thread.js';

// Lazy-load the space rail only when v2 is active
const loadSpaceRail = () => import('../shared/chat/chat-space-rail.js');
// Lazy-load the members sidebar only when v2 is active
const loadChatMembers = () => import('../shared/chat/chat-members.js');
// Lazy-load the search component only when v2 is active
const loadChatSearch = () => import('../shared/chat/chat-search.js');

/**
 * Slow fallback poll for the members sidebar.
 *
 * Agent membership and status are SSE-driven (`project.{id}.agent.>`), so this
 * is not the update path for them — it only covers what SSE does not carry
 * (unread DM state, human space membership) and re-syncs after a missed event.
 */
const FALLBACK_POLL_INTERVAL_MS = 60_000;

/**
 * Project a sidebar agent member onto the shared Agent shape so it can seed the
 * state manager's agent map. Only the fields SSE status deltas merge onto
 * matter — the map exists here purely to give those deltas a baseline.
 */
function agentMemberToAgent(
  m: import('../shared/chat/chat-members.js').ChatAgentMember
): Agent {
  return {
    id: m.id,
    name: m.displayName,
    projectId: m.projectId || '',
    template: '',
    phase: (m.phase || '') as Agent['phase'],
    activity: (m.activity || '') as NonNullable<Agent['activity']>,
    slug: m.slug || '',
    lastSeen: m.lastSeen || '',
  };
}

// ---- V1 types ----
// DEPRECATED(wave-1): Remove after v2 is stable and flag is permanently ON.

/** Shape of a thread entry from GET /api/v1/chat/threads */
interface ChatThread {
  agentId: string;
  agentSlug: string;
  agentName: string;
  phase: string;
  activity: string;
  lastMessage?: {
    msg: string;
    sender: string;
    createdAt: string;
    type: string;
  };
  hasUnread: boolean;
}

// ---- V2 types ----

interface V2ConversationState {
  conversationKey: string;
  projectId: string;
  projectSlug: string;
  threadName: string;
  defaultAgent: string;
  isDM: boolean;
  peerName: string;
  peerId: string;
  peerKind: 'user' | 'agent';
}

interface SpaceMember {
  id: string;
  name: string;
  email: string;
  avatarUrl?: string;
  kind: 'user' | 'agent';
}

@customElement('scion-page-chat')
export class ScionPageChat extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  // ---- Shared state ----
  private isV2 = isFeatureEnabled(NATIVE_CHAT_V2_FLAG);

  // ---- V1 state ----
  @state() private threads: ChatThread[] = [];
  @state() private loadingThreads = false;
  @state() private selectedAgentId = '';
  @state() private selectedAgentName = '';
  @state() private selectedAgentCanSend = false;
  private agentCapabilities = new Map<string, Capabilities | undefined>();
  private _onUserMessage = this.handleUserMessage.bind(this);
  private _refreshTimer: ReturnType<typeof setTimeout> | null = null;
  private _cachedProjectId = '';

  // ---- V2 state ----
  @state() private v2Conversation: V2ConversationState | null = null;
  @state() private v2Members: SpaceMember[] = [];
  @state() private v2MembersExpanded = true;
  @state() private v2SpaceRailLoaded = false;
  /** Human members for the members sidebar (from the members endpoint). */
  @state() private v2HumanMembers: import('../shared/chat/chat-members.js').ChatHumanMember[] = [];
  /** Agent members for the members sidebar. */
  @state() private v2AgentMembers: import('../shared/chat/chat-members.js').ChatAgentMember[] = [];
  private _onChatMessage = this.handleChatMessage.bind(this);
  private _onChatTopic = this.handleChatTopic.bind(this);
  private _onPresenceUpdated = this.handlePresenceUpdated.bind(this);
  private _onChatTyping = this.handleChatTyping.bind(this);
  private _onRailLoaded = this.handleRailLoaded.bind(this);
  private _onAgentsUpdated = this._handleAgentsUpdated.bind(this);
  private _onScopeChanged = this._handleScopeChanged.bind(this);
  private _onReadStateUpdated = this._handleReadStateUpdated.bind(this);
  /** Map from project slug → project ID for deep-link resolution. */
  private _slugToProjectId = new Map<string, string>();
  /** Map from project ID → project slug for URL generation. */
  private _projectIdToSlug = new Map<string, string>();
  /** IDs of users currently typing (for the members sidebar overlay). */
  @state() private v2TypingUserIds: string[] = [];
  /** Map of userId → expiry timer for typing indicators at page level. */
  private _typingTimers = new Map<string, ReturnType<typeof setTimeout>>();
  /** IDs of members with unread DM messages (for the unread dot on avatars). */
  @state() private v2UnreadFromIds: string[] = [];
  /** Whether the search panel is visible. */
  @state() private v2SearchActive = false;
  /** Whether the search component has been lazy-loaded. */
  @state() private v2SearchLoaded = false;
  /** Presence heartbeat interval timer. */
  private _presenceInterval: ReturnType<typeof setInterval> | null = null;
  /** Tracked project IDs for presence heartbeat. */
  private _presenceProjectIds: string[] = [];
  /** Slow fallback poll for what SSE does not cover (see FALLBACK_POLL_INTERVAL_MS). */
  private _fallbackPollInterval: ReturnType<typeof setInterval> | null = null;

  static override styles = css`
    :host {
      display: flex;
      height: 100%;
      overflow: hidden;
    }

    /* ---- V1 Layout ---- */

    .thread-rail {
      width: 300px;
      min-width: 240px;
      max-width: 360px;
      border-right: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
      display: flex;
      flex-direction: column;
      overflow: hidden;
    }

    .rail-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0.75rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      font-weight: 600;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .rail-header a {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.75rem;
      font-weight: 500;
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
      cursor: pointer;
    }

    .rail-header a:hover {
      text-decoration: underline;
    }

    .thread-list {
      flex: 1;
      overflow-y: auto;
      padding: 0.25rem 0;
    }

    .thread-item {
      display: flex;
      align-items: flex-start;
      gap: 0.625rem;
      padding: 0.625rem 1rem;
      cursor: pointer;
      transition: background 0.1s;
      border-left: 3px solid transparent;
      position: relative;
    }

    .thread-item:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .thread-item.selected {
      background: var(--scion-primary-50, #eff6ff);
      border-left-color: var(--scion-primary, #3b82f6);
    }

    .agent-avatar {
      width: 36px;
      height: 36px;
      border-radius: 50%;
      display: flex;
      align-items: center;
      justify-content: center;
      font-size: 0.75rem;
      font-weight: 600;
      color: #fff;
      flex-shrink: 0;
      text-transform: uppercase;
    }

    .thread-info {
      flex: 1;
      min-width: 0;
    }

    .thread-name {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .thread-name .unread-dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      background: var(--scion-primary, #3b82f6);
      flex-shrink: 0;
    }

    .thread-preview {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
      margin-top: 0.125rem;
    }

    .thread-time {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
      flex-shrink: 0;
    }

    /* ---- Shared layout ---- */

    .thread-content {
      flex: 1;
      display: flex;
      flex-direction: column;
      min-width: 0;
      overflow: hidden;
    }

    .empty-state {
      flex: 1;
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      color: var(--scion-text-muted, #64748b);
      gap: 0.75rem;
      padding: 2rem;
    }

    .empty-state sl-icon {
      font-size: 2.5rem;
      opacity: 0.3;
    }

    .empty-state .title {
      font-size: 1rem;
      font-weight: 500;
    }

    .empty-state .subtitle {
      font-size: 0.875rem;
    }

    .loading-rail {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* ---- V2 Layout ---- */

    .v2-rail {
      width: 260px;
      min-width: 200px;
      max-width: 320px;
      border-right: 1px solid var(--scion-border, #e2e8f0);
      overflow: hidden;
    }

    .v2-members {
      width: 240px;
      border-left: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
      display: flex;
      flex-direction: column;
    }

    .v2-members.collapsed {
      display: none;
    }

    .v2-members-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .v2-members-body {
      flex: 1;
      overflow-y: auto;
      padding: 0.5rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      display: flex;
      align-items: center;
      justify-content: center;
    }

    .v2-thread-header {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      background: var(--scion-surface, #ffffff);
    }

    .v2-thread-header .hash {
      color: var(--scion-text-muted, #64748b);
    }

    .v2-thread-header .header-actions {
      margin-left: auto;
    }

    @media (max-width: 768px) {
      .thread-rail,
      .v2-rail {
        width: 100%;
        max-width: none;
      }

      :host(.thread-open) .thread-rail,
      :host(.thread-open) .v2-rail {
        display: none;
      }

      :host(:not(.thread-open)) .thread-content {
        display: none;
      }

      .v2-members {
        display: none;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();

    if (this.isV2) {
      void this.initV2();
    } else {
      // Guard: redirect v2 routes to /chat when v2 flag is OFF (O3)
      const path = window.location.pathname;
      if (path.startsWith('/chat/space/') || path.startsWith('/chat/dm/')) {
        navigateTo('/chat');
        return;
      }
      this.parseRoute();
      void this.loadThreads();
      stateManager.addEventListener('user-message-created', this._onUserMessage);
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.isV2) {
      stateManager.removeEventListener('chat-message-received', this._onChatMessage);
      stateManager.removeEventListener('chat-topic-updated', this._onChatTopic);
      stateManager.removeEventListener('chat-presence-updated', this._onPresenceUpdated);
      stateManager.removeEventListener('chat-typing-received', this._onChatTyping);
      stateManager.removeEventListener('agents-updated', this._onAgentsUpdated);
      stateManager.removeEventListener('scope-changed', this._onScopeChanged);
      this.removeEventListener('rail-loaded', this._onRailLoaded);
      this.removeEventListener('read-state-updated', this._onReadStateUpdated);
      this.stopPresenceHeartbeat();
      // Clean up the fallback poll
      if (this._fallbackPollInterval) {
        clearInterval(this._fallbackPollInterval);
        this._fallbackPollInterval = null;
      }
      // Clean up typing timers
      for (const timer of this._typingTimers.values()) {
        clearTimeout(timer);
      }
      this._typingTimers.clear();
    } else {
      stateManager.removeEventListener('user-message-created', this._onUserMessage);
    }
    if (this._refreshTimer) {
      clearTimeout(this._refreshTimer);
      this._refreshTimer = null;
    }
  }

  override updated(changedProperties: Map<string, unknown>): void {
    if (changedProperties.has('pageData') && this.pageData) {
      if (this.isV2) {
        this.parseV2Route();
      } else {
        this.parseRoute();
      }
    }
  }

  // =========================================================================
  // DEPRECATED(wave-1): Remove after v2 is stable and flag is permanently ON.
  // V1 Methods — preserved for rollback when web.native_chat_v2 is OFF.
  // =========================================================================

  private handleUserMessage(): void {
    if (this._refreshTimer) {
      clearTimeout(this._refreshTimer);
    }
    this._refreshTimer = setTimeout(() => {
      this._refreshTimer = null;
      void this.loadThreads();
    }, 2000);
  }

  private parseRoute(): void {
    const path = this.pageData?.path || window.location.pathname;
    const match = path.match(/\/chat\/([^/]+)/);
    const newAgentId = match ? decodeURIComponent(match[1]) : '';

    if (newAgentId !== this.selectedAgentId) {
      this.selectedAgentId = newAgentId;
      if (newAgentId) {
        this.classList.add('thread-open');
        void this.fetchAgentCapabilities(newAgentId);
      } else {
        this.classList.remove('thread-open');
        this.selectedAgentCanSend = false;
      }
    }
  }

  private async loadThreads(): Promise<void> {
    this.loadingThreads = true;

    try {
      const projectId = await this.resolveProjectId();
      if (!projectId) {
        this.loadingThreads = false;
        return;
      }

      const res = await apiFetch(
        `/api/v1/chat/threads?projectId=${encodeURIComponent(projectId)}&limit=50`
      );

      if (res.ok) {
        const data = (await res.json()) as { threads: ChatThread[] };
        this.threads = data.threads || [];

        if (this.selectedAgentId) {
          this.resolveSelectedAgentName();
        }
      }
    } catch {
      // Silently fail
    } finally {
      this.loadingThreads = false;
    }
  }

  private async resolveProjectId(): Promise<string> {
    if (this._cachedProjectId) return this._cachedProjectId;

    const url = new URL(window.location.href);
    const qProject = url.searchParams.get('projectId');
    if (qProject) {
      this._cachedProjectId = qProject;
      return qProject;
    }

    try {
      const res = await apiFetch('/api/v1/projects?limit=1');
      if (res.ok) {
        const data = (await res.json()) as { items?: { id: string }[] };
        if (data.items && data.items.length > 0) {
          this._cachedProjectId = data.items[0].id;
          return this._cachedProjectId;
        }
      }
    } catch {
      // ignore
    }

    return '';
  }

  private resolveSelectedAgentName(): void {
    const thread = this.threads.find(
      (t) => t.agentId === this.selectedAgentId || t.agentSlug === this.selectedAgentId
    );
    if (thread) {
      this.selectedAgentName = thread.agentName || thread.agentSlug || thread.agentId;
      dispatchPageTitle(this, this.selectedAgentName, 'Chat');
    }
  }

  private async fetchAgentCapabilities(agentId: string): Promise<void> {
    if (this.agentCapabilities.has(agentId)) {
      this.selectedAgentCanSend = can(this.agentCapabilities.get(agentId), 'message');
      return;
    }

    try {
      const res = await apiFetch(`/api/v1/agents/${encodeURIComponent(agentId)}`);
      if (res.ok) {
        const agent = (await res.json()) as { _capabilities?: Capabilities };
        this.agentCapabilities.set(agentId, agent._capabilities);
        this.selectedAgentCanSend = can(agent._capabilities, 'message');
      }
    } catch {
      this.selectedAgentCanSend = false;
    }
  }

  private async markThreadRead(agentId: string): Promise<void> {
    const projectId = await this.resolveProjectId();
    if (!projectId) return;

    try {
      await apiFetch(
        `/api/v1/chat/threads/${encodeURIComponent(agentId)}/read?projectId=${encodeURIComponent(projectId)}`,
        { method: 'POST' }
      );
      this.threads = this.threads.map((t) =>
        t.agentId === agentId ? { ...t, hasUnread: false } : t
      );
    } catch {
      // Non-critical
    }
  }

  private selectThread(thread: ChatThread): void {
    const agentRef = thread.agentSlug || thread.agentId;
    navigateTo(`/chat/${encodeURIComponent(agentRef)}`);
    this.selectedAgentId = thread.agentId;
    this.selectedAgentName = thread.agentName || thread.agentSlug || thread.agentId;
    this.classList.add('thread-open');
    dispatchPageTitle(this, this.selectedAgentName, 'Chat');

    void this.fetchAgentCapabilities(thread.agentId);
    void this.markThreadRead(thread.agentId);
  }

  // =========================================================================
  // V2 Methods
  // =========================================================================

  private async initV2(): Promise<void> {
    // Lazy-load the space rail and members components
    await Promise.all([loadSpaceRail(), loadChatMembers()]);
    this.v2SpaceRailLoaded = true;

    // Parse initial route
    this.parseV2Route();

    // If no conversation selected, load hub-level members for the sidebar
    if (!this.v2Conversation) {
      void this.loadHubMembers();
    }

    // Load unread DM peer IDs for the blue unread dot on member avatars
    void this.loadUnreadDMPeers();

    // Subscribe to SSE events
    stateManager.addEventListener('chat-message-received', this._onChatMessage);
    stateManager.addEventListener('chat-topic-updated', this._onChatTopic);
    stateManager.addEventListener('chat-presence-updated', this._onPresenceUpdated);
    stateManager.addEventListener('chat-typing-received', this._onChatTyping);
    stateManager.addEventListener('agents-updated', this._onAgentsUpdated);
    stateManager.addEventListener('scope-changed', this._onScopeChanged);

    // Listen for rail-loaded to set up the SSE scope with space IDs
    this.addEventListener('rail-loaded', this._onRailLoaded);

    // The open thread advances its own read watermark; the rail and the DM
    // unread dots are separate components with no way to observe that.
    this.addEventListener('read-state-updated', this._onReadStateUpdated);

    // Agent membership and status badges are SSE-driven: the chat scope
    // subscribes to `project.{spaceId}.agent.>`, which carries both lifecycle
    // (created/deleted) and status (phase/activity) events. See
    // _handleAgentsUpdated.
    //
    // Two things have no SSE event of their own — unread DM state, and human
    // membership of a space — so a slow fallback poll covers them and
    // re-syncs the member list if an SSE event was ever missed. Unread state
    // is additionally refreshed on every inbound chat message.
    this._fallbackPollInterval = setInterval(() => {
      void this.loadUnreadDMPeers();
      if (this.v2Conversation?.projectId) {
        void this.loadV2Members(this.v2Conversation.projectId);
      } else {
        void this.loadHubMembers();
      }
    }, FALLBACK_POLL_INTERVAL_MS);
  }

  /** Called when the space rail finishes loading its data. Sets up the SSE scope. */
  private handleRailLoaded(e: Event): void {
    const detail = (e as CustomEvent).detail as {
      spaceIds: string[];
      spaces?: Array<{ projectId: string; projectSlug: string; projectName: string }>;
    };

    // Populate slug ↔ projectId maps for deep-link resolution
    if (detail.spaces) {
      for (const s of detail.spaces) {
        if (s.projectSlug) {
          this._slugToProjectId.set(s.projectSlug, s.projectId);
          this._projectIdToSlug.set(s.projectId, s.projectSlug);
        }
      }
      // Re-resolve the route now that slug data is available (handles deep-link on first load)
      this.parseV2Route();
    }

    const userId = this.pageData?.user?.id || '';
    if (detail.spaceIds.length > 0 && userId) {
      stateManager.setScope({
        type: 'chat',
        spaceIds: detail.spaceIds,
        userId,
      });
      // Start presence heartbeat
      this._presenceProjectIds = detail.spaceIds;
      this.startPresenceHeartbeat();

      // When in the global /chat view (no conversation selected), the hub
      // members were loaded from /api/v1/users which doesn't include presence
      // state. Now that we have space IDs, fetch presence data from the first
      // space's members endpoint and merge it into the existing list.
      if (!this.v2Conversation) {
        void this.refreshHubMemberPresence(detail.spaceIds[0]);
      }
    }
  }

  private parseV2Route(): void {
    // Always use the browser URL as the source of truth. pushState
    // navigations (handleThreadSelect, handleMemberClick) update the
    // browser URL without updating pageData, so pageData.path can be
    // stale when this is called from handleRailLoaded after a reload.
    const path = window.location.pathname;

    // Match legacy /chat/space/{projectId}/thread/{topicId} (backward compat)
    const legacyThreadMatch = path.match(/\/chat\/space\/([^/]+)\/thread\/([^/]+)/);
    if (legacyThreadMatch) {
      const projectId = decodeURIComponent(legacyThreadMatch[1]);
      const topicId = decodeURIComponent(legacyThreadMatch[2]);
      // Redirect to the readable URL if we know the slug
      const slug = this._projectIdToSlug.get(projectId);
      if (slug) {
        navigateTo(`/chat/${encodeURIComponent(slug)}/${encodeURIComponent(topicId)}`);
        return;
      }
      const existingDefault = this.v2Conversation?.conversationKey === topicId
        ? this.v2Conversation.defaultAgent
        : '';
      this.v2Conversation = {
        conversationKey: topicId,
        projectId,
        projectSlug: '',
        threadName: '',
        defaultAgent: existingDefault,
        isDM: false,
        peerName: '',
        peerId: '',
        peerKind: 'user',
      };
      this.classList.add('thread-open');
      void this.loadV2Members(projectId);
      if (!existingDefault) {
        void this.fetchThreadDefaultAgent(topicId);
      }
      dispatchPageTitle(this, 'Thread', 'Chat');
      return;
    }

    // Match legacy /chat/space/{projectId} (backward compat)
    const legacySpaceMatch = path.match(/\/chat\/space\/([^/]+)$/);
    if (legacySpaceMatch) {
      const projectId = decodeURIComponent(legacySpaceMatch[1]);
      const slug = this._projectIdToSlug.get(projectId);
      if (slug) {
        navigateTo(`/chat/${encodeURIComponent(slug)}`);
        return;
      }
      this.classList.add('thread-open');
      return;
    }

    // Match /chat/dm/{keyOrPeerId}
    const dmMatch = path.match(/\/chat\/dm\/(.+)$/);
    if (dmMatch) {
      const segment = decodeURIComponent(dmMatch[1]);

      // Guard: already viewing this exact DM — skip to avoid overwriting
      // peer metadata that was populated by handleMemberClick or resolveDMByPeerId.
      if (this.v2Conversation?.isDM && this.v2Conversation.conversationKey === segment) {
        return;
      }

      // Guard: don't overwrite a valid DM key with a malformed one.
      const dmKeyRegex = /^dm:(user|agent):[0-9a-f-]{36}:(user|agent):[0-9a-f-]{36}$/;
      if (
        this.v2Conversation?.isDM &&
        dmKeyRegex.test(this.v2Conversation.conversationKey) &&
        !dmKeyRegex.test(segment)
      ) {
        return;
      }

      this.classList.add('thread-open');
      dispatchPageTitle(this, 'DM', 'Chat');

      if (segment.startsWith('dm:')) {
        // Legacy DM key format (e.g. dm:agent:UUID:user:UUID) — use directly
        this.v2Conversation = {
          conversationKey: segment,
          projectId: '',
          projectSlug: '',
          threadName: '',
          defaultAgent: '',
          isDM: true,
          peerName: '',
          peerId: '',
          peerKind: 'user',
        };
        void this.resolveDMPeerInfo(segment);
      } else {
        // Peer ID format (/chat/dm/<peerId>) — reconstruct the full
        // composite DM key using buildDMKey (returns null if user ID
        // is not yet available, preventing broken keys).
        const isAgent = this.v2AgentMembers.some((a) => a.id === segment);
        const peerKind: 'user' | 'agent' = isAgent ? 'agent' : 'user';
        const dmKey = this.buildDMKey(segment, peerKind);

        if (dmKey) {
          // If we already have the correct DM conversation open, skip.
          if (this.v2Conversation?.conversationKey === dmKey) {
            return;
          }

          let peerName = '';
          if (isAgent) {
            const agent = this.v2AgentMembers.find((a) => a.id === segment);
            peerName = agent?.displayName || '';
          } else {
            const human = this.v2HumanMembers.find((h) => h.id === segment);
            peerName = human?.displayName || '';
          }

          this.v2Conversation = {
            conversationKey: dmKey,
            projectId: '',
            projectSlug: '',
            threadName: '',
            defaultAgent: '',
            isDM: true,
            peerName,
            peerId: segment,
            peerKind,
          };
          if (peerName) {
            dispatchPageTitle(this, peerName, 'Chat');
          }
        } else {
          // User ID not available — resolve via API
          void this.resolveDMByPeerId(segment, peerKind);
        }
      }
      return;
    }

    // Match readable /chat/<slug>/<thread-id>
    const readableThreadMatch = path.match(/\/chat\/([^/]+)\/([^/]+)$/);
    if (readableThreadMatch) {
      const segment1 = decodeURIComponent(readableThreadMatch[1]);
      const threadId = decodeURIComponent(readableThreadMatch[2]);

      // Resolve slug → projectId (may need async API call on cold load)
      const projectId = this._slugToProjectId.get(segment1);
      if (projectId) {
        const existingDefault = this.v2Conversation?.conversationKey === threadId
          ? this.v2Conversation.defaultAgent
          : '';
        this.v2Conversation = {
          conversationKey: threadId,
          projectId,
          projectSlug: segment1,
          threadName: '',
          defaultAgent: existingDefault,
          isDM: false,
          peerName: '',
          peerId: '',
          peerKind: 'user',
        };
        this.classList.add('thread-open');
        void this.loadV2Members(projectId);
        if (!existingDefault) {
          void this.fetchThreadDefaultAgent(threadId);
        }
        dispatchPageTitle(this, 'Thread', 'Chat');
      } else {
        // Slug not yet in cache — resolve via API (deep-link cold load)
        void this.resolveSlugAndOpenThread(segment1, threadId);
      }
      return;
    }

    // Match readable /chat/<slug-or-agent> (single segment)
    const singleMatch = path.match(/\/chat\/([^/]+)$/);
    if (singleMatch) {
      const segment = decodeURIComponent(singleMatch[1]);

      // Check if it's a known project slug
      const projectId = this._slugToProjectId.get(segment);
      if (projectId) {
        // It's a space — select it (the rail will open #general)
        this.classList.add('thread-open');
        void this.selectSpaceBySlug(segment, projectId);
        return;
      }

      // If slug map isn't populated yet (cold load), try resolving via API.
      // If it turns out not to be a project slug, resolveSlugAndOpenSpace is a no-op
      // and the URL stays as-is for V1 agent compat.
      if (this._slugToProjectId.size === 0) {
        void this.resolveSlugAndOpenSpace(segment);
        return;
      }

      // Not a project slug — fall through to clear conversation state.
      // (V1 agent slugs are not handled in V2 mode.)
    }

    // /chat — no conversation selected, show hub-level members
    this.v2Conversation = null;
    this.v2MembersExpanded = true; // Always show tray in base view (no header toggle available)
    this.classList.remove('thread-open');
    void this.loadHubMembers();
  }

  /**
   * Resolve a project slug to a project ID via the API, then open the
   * specified thread. Used for deep-link cold loads before the rail populates.
   */
  private async resolveSlugAndOpenThread(slug: string, threadId: string): Promise<void> {
    const projectId = await this.resolveProjectBySlug(slug);
    if (!projectId) return;

    const existingDefault = this.v2Conversation?.conversationKey === threadId
      ? this.v2Conversation.defaultAgent
      : '';
    this.v2Conversation = {
      conversationKey: threadId,
      projectId,
      projectSlug: slug,
      threadName: '',
      defaultAgent: existingDefault,
      isDM: false,
      peerName: '',
      peerId: '',
      peerKind: 'user',
    };
    this.classList.add('thread-open');
    void this.loadV2Members(projectId);
    if (!existingDefault) {
      void this.fetchThreadDefaultAgent(threadId);
    }
    dispatchPageTitle(this, 'Thread', 'Chat');
  }

  /**
   * Resolve a project slug to a project ID via the API, then open the space
   * (selecting #general). Used for deep-link cold loads before the rail populates.
   */
  private async resolveSlugAndOpenSpace(slug: string): Promise<void> {
    const projectId = await this.resolveProjectBySlug(slug);
    if (projectId) {
      this.classList.add('thread-open');
      void this.selectSpaceBySlug(slug, projectId);
    }
    // If resolution fails, leave the URL in place — the V1 parseRoute() may
    // handle it as an agent slug when v2 flag is off (or it's just a 404 space).
  }

  /**
   * Look up a project by slug via the projects API.
   * Populates the slug cache on success.
   */
  private async resolveProjectBySlug(slug: string): Promise<string> {
    // Check cache first
    const cached = this._slugToProjectId.get(slug);
    if (cached) return cached;

    try {
      const res = await apiFetch(`/api/v1/projects?slug=${encodeURIComponent(slug)}&limit=1`);
      if (res.ok) {
        const data = (await res.json()) as {
          items?: Array<{ id: string; slug: string; name: string }>;
        };
        if (data.items && data.items.length > 0) {
          const project = data.items[0];
          this._slugToProjectId.set(project.slug, project.id);
          this._projectIdToSlug.set(project.id, project.slug);
          return project.id;
        }
      }
    } catch {
      // Resolution failed — non-critical
    }
    return '';
  }

  /**
   * Select a space by slug: wait for the rail to be available, then
   * ask it to open #general for the given project.
   */
  private async selectSpaceBySlug(slug: string, projectId: string): Promise<void> {
    // The rail may not have loaded yet; wait for it
    const rail = this.shadowRoot?.querySelector('scion-chat-space-rail') as
      | import('../shared/chat/chat-space-rail.js').ScionChatSpaceRail
      | null;

    if (!rail) {
      // Rail not mounted yet — the rail-loaded handler will re-parse the route
      return;
    }

    // Find #general thread for this space from the rail
    const threads = await this.loadSpaceThreads(projectId);
    const general = threads.find((t: { isGeneral: boolean }) => t.isGeneral);
    if (general) {
      this.v2Conversation = {
        conversationKey: general.id,
        projectId,
        projectSlug: slug,
        threadName: general.name,
        defaultAgent: general.defaultAgent || '',
        isDM: false,
        peerName: '',
        peerId: '',
        peerKind: 'user',
      };
      void this.loadV2Members(projectId);
      dispatchPageTitle(this, `#${general.name}`, 'Chat');
      // Update URL to include the thread
      navigateTo(`/chat/${encodeURIComponent(slug)}/${encodeURIComponent(general.id)}`);
    }
  }

  /**
   * Fetch threads for a space from the API.
   */
  private async loadSpaceThreads(
    projectId: string
  ): Promise<Array<{ id: string; name: string; isGeneral: boolean; defaultAgent?: string }>> {
    try {
      const res = await apiFetch(
        `/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`
      );
      if (res.ok) {
        const data = (await res.json()) as {
          threads?: Array<{
            id: string;
            name: string;
            isGeneral: boolean;
            defaultAgent?: string;
          }>;
        };
        return data.threads || [];
      }
    } catch {
      // Non-critical
    }
    return [];
  }

  /**
   * Update the members sidebar from SSE agent events.
   *
   * The chat scope subscribes to `project.{spaceId}.agent.>`, which the state
   * manager routes through handleAgentEvent for every agent subject —
   * `created` and `deleted` (membership) as well as `status` (phase/activity).
   * All three land here as `agents-updated`, so this rebuilds the agent member
   * list from the shared agent map rather than re-fetching over REST.
   *
   * The REST loaders seed that map (stateManager.seedAgents) so status deltas
   * have a baseline to merge onto — without a baseline the state manager
   * buffers the delta and never notifies.
   */
  /**
   * setScope clears the shared agent map, and the chat scope is only set once
   * the rail reports its space IDs — which can land after the members have
   * already loaded and seeded. Re-seed so SSE status deltas keep a baseline.
   */
  private _handleScopeChanged(): void {
    if (this.v2AgentMembers.length > 0) {
      stateManager.seedAgents(this.v2AgentMembers.map(agentMemberToAgent));
    }
  }

  private _handleAgentsUpdated(): void {
    // Only adopt agents belonging to the current view: the open conversation's
    // project, or every space the user can see in the base view.
    const scopeProjectId = this.v2Conversation?.projectId || '';
    const inScope = (projectId: string): boolean =>
      scopeProjectId ? projectId === scopeProjectId : true;

    const byId = new Map(this.v2AgentMembers.map((a) => [a.id, a]));

    for (const agent of stateManager.getAgents()) {
      const existing = byId.get(agent.id);
      if (!existing && !inScope(agent.projectId || '')) continue;
      byId.set(agent.id, {
        id: agent.id,
        kind: 'agent' as const,
        displayName: agent.name || agent.slug || agent.id,
        slug: agent.slug || existing?.slug || '',
        phase: agent.phase || '',
        activity: agent.activity || '',
        lastSeen: agent.lastSeen || existing?.lastSeen || '',
        projectId: agent.projectId || existing?.projectId || scopeProjectId,
      });
    }

    // Drop agents removed via SSE `deleted` events.
    const deletedRefs = new Set<string>();
    for (const id of stateManager.getDeletedAgentIds()) {
      const removed = byId.get(id);
      if (removed?.slug) deletedRefs.add(removed.slug);
      deletedRefs.add(id);
      byId.delete(id);
    }

    this.v2AgentMembers = Array.from(byId.values());

    // A deleted agent cannot remain the thread default. The server clears the
    // binding and emits topic-updated; this covers the open view even if that
    // event is missed. defaultAgent holds a slug or an ID, so both are checked.
    const conv = this.v2Conversation;
    if (conv?.defaultAgent && deletedRefs.has(conv.defaultAgent)) {
      this.v2Conversation = { ...conv, defaultAgent: '' };
    }
  }

  /**
   * A conversation's read watermark moved (dispatched by chat-thread after a
   * successful POST). Clear the matching unread markers without a round trip,
   * then re-sync from the server so a rejected write cannot leave the UI lying.
   */
  private _handleReadStateUpdated(e: Event): void {
    const detail = (e as CustomEvent).detail as { conversationKey?: string } | undefined;
    const key = detail?.conversationKey || '';
    if (!key) return;

    if (key.startsWith('dm:')) {
      const peerId = this.v2Conversation?.peerId || '';
      if (peerId && this.v2UnreadFromIds.includes(peerId)) {
        this.v2UnreadFromIds = this.v2UnreadFromIds.filter((id) => id !== peerId);
      }
      void this.loadUnreadDMPeers();
      return;
    }

    const rail = this.shadowRoot?.querySelector('scion-chat-space-rail') as
      | import('../shared/chat/chat-space-rail.js').ScionChatSpaceRail
      | null;
    rail?.markThreadRead(key);
  }

  private handleChatMessage(): void {
    // A new message may create an unread DM or clear one — refresh the dots.
    void this.loadUnreadDMPeers();

    // Debounce: reload the rail + backfill conversation
    if (this._refreshTimer) clearTimeout(this._refreshTimer);
    this._refreshTimer = setTimeout(() => {
      this._refreshTimer = null;
      const rail = this.shadowRoot?.querySelector('scion-chat-space-rail') as
        | import('../shared/chat/chat-space-rail.js').ScionChatSpaceRail
        | null;
      if (rail) void rail.reload();
    }, 2000);
  }

  private handleChatTopic(e: Event): void {
    const eventDetail = (e as CustomEvent).detail as Record<string, unknown> | undefined;
    // Unwrap the notifyWithData envelope: { state, data: { action, topic: {...} } }
    const eventData = (eventDetail?.data ?? eventDetail) as Record<string, unknown> | undefined;
    const topic = eventData?.topic as Record<string, unknown> | undefined;
    const topicId = (topic?.id as string) || '';
    const newDefault = (topic?.defaultAgent as string) ?? '';

    // If this is the currently-viewed conversation, update defaultAgent directly
    // and skip the rail reload to avoid the parseV2Route race that overwrites
    // defaultAgent on subsequent changes, and to prevent sidebar flash.
    if (topicId && this.v2Conversation?.conversationKey === topicId) {
      if (this.v2Conversation.defaultAgent !== newDefault) {
        this.v2Conversation = {
          ...this.v2Conversation,
          defaultAgent: newDefault,
        };
      }
      return;
    }

    // For other topic changes (rename, delete, etc.), reload rail
    const rail = this.shadowRoot?.querySelector('scion-chat-space-rail') as
      | import('../shared/chat/chat-space-rail.js').ScionChatSpaceRail
      | null;
    if (rail) void rail.reload();
  }

  private handleThreadSelect(e: CustomEvent): void {
    const detail = e.detail as {
      conversationKey: string;
      projectId: string;
      projectSlug?: string;
      threadName: string;
      defaultAgent?: string;
    };

    // Determine the slug for the readable URL
    const slug =
      detail.projectSlug ||
      this._projectIdToSlug.get(detail.projectId) ||
      '';

    // Cache the mapping if we received a slug
    if (slug && detail.projectId) {
      this._slugToProjectId.set(slug, detail.projectId);
      this._projectIdToSlug.set(detail.projectId, slug);
    }

    // Set up conversation state directly (avoid page recreation from navigateTo
    // which destroys and recreates the page element, causing visible flicker).
    this.v2Conversation = {
      conversationKey: detail.conversationKey,
      projectId: detail.projectId,
      projectSlug: slug,
      threadName: detail.threadName,
      defaultAgent: detail.defaultAgent || '',
      isDM: false,
      peerName: '',
      peerId: '',
      peerKind: 'user',
    };
    this.classList.add('thread-open');

    // Update the URL with pushState to avoid page recreation flicker
    const base = import.meta.env.BASE_URL;
    let threadPath: string;
    if (slug) {
      threadPath = `/chat/${encodeURIComponent(slug)}/${encodeURIComponent(detail.conversationKey)}`;
    } else {
      threadPath = `/chat/space/${encodeURIComponent(detail.projectId)}/thread/${encodeURIComponent(detail.conversationKey)}`;
    }
    const browserPath = base && base !== '/' ? base.replace(/\/$/, '') + threadPath : threadPath;
    window.history.pushState({}, '', browserPath);

    dispatchPageTitle(this, `#${detail.threadName}`, 'Chat');
    void this.loadV2Members(detail.projectId);
  }

  private handleNavigateApp(): void {
    navigateTo('/');
  }

  /** Reset to the global /chat view (no conversation selected). */
  private handleResetView(): void {
    this.v2Conversation = null;
    this.v2MembersExpanded = true; // Always show tray in base view
    this.classList.remove('thread-open');
    // Navigate to bare /chat
    const base = import.meta.env.BASE_URL;
    const chatPath = '/chat';
    const browserPath = base && base !== '/' ? base.replace(/\/$/, '') + chatPath : chatPath;
    window.history.pushState({}, '', browserPath);
    dispatchPageTitle(this, '', 'Chat');
    // Reload hub-level members for the sidebar
    void this.loadHubMembers();
  }

  /**
   * Fetch the defaultAgent for a thread from the topic detail endpoint.
   * Called on first load of a thread (not on re-parse of the same thread).
   */
  private async fetchThreadDefaultAgent(conversationKey: string): Promise<void> {
    try {
      const res = await apiFetch(
        `/api/v1/chat/topics/${encodeURIComponent(conversationKey)}`
      );
      if (res.ok) {
        const data = (await res.json()) as { defaultAgent?: string };
        if (
          this.v2Conversation?.conversationKey === conversationKey &&
          data.defaultAgent
        ) {
          this.v2Conversation = {
            ...this.v2Conversation,
            defaultAgent: data.defaultAgent,
          };
        }
      }
    } catch {
      // Non-critical — thread will work without a default agent
    }
  }

  /** Handle default-agent-changed from the thread component. Updates local state in place to avoid re-render. */
  private handleDefaultAgentChanged(e: CustomEvent): void {
    const detail = e.detail as { defaultAgent: string };
    if (this.v2Conversation) {
      this.v2Conversation = {
        ...this.v2Conversation,
        defaultAgent: detail.defaultAgent || '',
      };
    }
  }

  /**
   * Resolve DM peer info from the DM list endpoint. This handles the case
   * where the user navigates directly to /chat/dm/{key} (e.g., page refresh)
   * and the peer metadata is not populated from the rail click event.
   */
  private async resolveDMPeerInfo(key: string): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/chat/dms');
      if (!res.ok) return;
      const data = (await res.json()) as {
        dms?: Array<{
          conversationKey: string;
          peerName?: string;
          peerEmail?: string;
          peerId: string;
          peerKind: 'user' | 'agent';
          peerSlug?: string;
        }>;
      };
      const dm = data.dms?.find((d) => d.conversationKey === key);
      if (dm && this.v2Conversation?.conversationKey === key) {
        const peerName = dm.peerName || dm.peerSlug || dm.peerEmail || dm.peerId;
        this.v2Conversation = {
          ...this.v2Conversation,
          peerName,
          peerId: dm.peerId,
          peerKind: dm.peerKind,
        };
        dispatchPageTitle(this, peerName, 'Chat');
      }
    } catch {
      // Non-critical — the DM will still work, just without a resolved peer name.
    }
  }

  /**
   * Resolve a DM by peer ID. Used when the URL is /chat/dm/<peerId>
   * (the clean peer-ID format) on page refresh or back-button navigation.
   * Fetches the DM list to find a matching DM, or determines the peer kind
   * from the agents/users API to construct the DM key.
   */
  private async resolveDMByPeerId(
    peerId: string,
    peerKind: 'user' | 'agent' = 'user',
    displayName = ''
  ): Promise<void> {
    // 1. Try to find an existing DM via the DM list API (no user ID needed).
    try {
      const res = await apiFetch('/api/v1/chat/dms');
      if (res.ok) {
        const data = (await res.json()) as {
          dms?: Array<{
            conversationKey: string;
            peerName?: string;
            peerEmail?: string;
            peerId: string;
            peerKind: 'user' | 'agent';
            peerSlug?: string;
          }>;
        };
        const dm = data.dms?.find((d) => d.peerId === peerId);
        if (dm) {
          const peerName = dm.peerName || dm.peerSlug || dm.peerEmail || displayName || dm.peerId;
          this.v2Conversation = {
            conversationKey: dm.conversationKey,
            projectId: '',
            projectSlug: '',
            threadName: '',
            defaultAgent: '',
            isDM: true,
            peerName,
            peerId: dm.peerId,
            peerKind: dm.peerKind,
          };
          this.classList.add('thread-open');
          dispatchPageTitle(this, peerName, 'Chat');
          return;
        }
      }
    } catch {
      // continue to fallback
    }

    // 2. Try fetching user ID from /api/v1/auth/me (handles token-based auth).
    if (!this.pageData?.user?.id) {
      try {
        const authRes = await apiFetch('/api/v1/auth/me');
        if (authRes.ok) {
          const authData = (await authRes.json()) as { id?: string };
          if (authData.id && this.pageData) {
            if (this.pageData.user) {
              this.pageData.user.id = authData.id;
            } else {
              this.pageData = {
                ...this.pageData,
                user: { id: authData.id, email: '', name: '' },
              };
            }
          }
        }
      } catch {
        // continue
      }
    }

    // 3. Retry key construction with the potentially-refreshed user ID.
    const key = this.buildDMKey(peerId, peerKind);
    if (key) {
      this.v2Conversation = {
        conversationKey: key,
        projectId: '',
        projectSlug: '',
        threadName: '',
        defaultAgent: '',
        isDM: true,
        peerName: displayName,
        peerId,
        peerKind,
      };
      this.classList.add('thread-open');
      dispatchPageTitle(this, displayName || 'DM', 'Chat');
      return;
    }

    // 4. Unable to resolve — log error.
    console.error('Unable to open DM — user identity not available. Please refresh the page.');
  }

  /**
   * Load hub-level members (all users and agents in the hub) for the
   * members sidebar when no specific space/project is selected.
   */
  private async loadHubMembers(): Promise<void> {
    try {
      // Fetch users and agents in parallel
      const [usersRes, agentsRes] = await Promise.all([
        apiFetch('/api/v1/users?limit=100'),
        apiFetch('/api/v1/agents?limit=100'),
      ]);

      if (usersRes.ok) {
        const userData = (await usersRes.json()) as {
          users?: Array<{
            id: string;
            displayName: string;
            email?: string;
            avatarUrl?: string;
            role?: string;
            status?: string;
          }>;
        };
        // /api/v1/users carries no presence state. Preserve whatever
        // refreshHubMemberPresence() (or an SSE presence event) already
        // merged in, otherwise the periodic poll would blank out every
        // presence indicator in the base chat view.
        const currentPresence = new Map<string, 'active' | 'idle'>();
        for (const h of this.v2HumanMembers) {
          if (h.presenceState) currentPresence.set(h.id, h.presenceState);
        }
        this.v2HumanMembers = (userData.users || [])
          .filter((u) => u.status !== 'disabled')
          .map((u) => ({
            id: u.id,
            kind: 'user' as const,
            displayName: u.displayName || u.email || u.id,
            email: u.email || '',
            avatarUrl: u.avatarUrl || '',
            role: u.role || '',
            presenceState: currentPresence.get(u.id) || ('' as const),
          }));
      }

      if (agentsRes.ok) {
        const agentData = (await agentsRes.json()) as {
          agents?: Array<{
            id: string;
            name: string;
            slug?: string;
            phase?: string;
            status?: string;
            activity?: string;
            lastSeen?: string;
            projectId?: string;
          }>;
        };
        this.v2AgentMembers = (agentData.agents || []).map((a) => ({
          id: a.id,
          kind: 'agent' as const,
          displayName: a.name || a.slug || a.id,
          slug: a.slug || '',
          phase: a.phase || '',
          activity: a.activity || '',
          lastSeen: a.lastSeen || '',
          projectId: a.projectId || '',
        }));
        // Seed the shared agent map so SSE status deltas have a baseline to
        // merge onto — otherwise they are buffered and never notify.
        stateManager.seedAgents(this.v2AgentMembers.map(agentMemberToAgent));
      }

      // Also populate legacy v2Members for thread @-mention support
      this.v2Members = [
        ...this.v2HumanMembers.map((h) => ({
          id: h.id,
          name: h.displayName,
          email: h.email || '',
          avatarUrl: h.avatarUrl || '',
          kind: 'user' as const,
        })),
        ...this.v2AgentMembers.map((a) => ({
          id: a.id,
          name: a.displayName,
          email: '',
          kind: 'agent' as const,
        })),
      ];
    } catch {
      // Non-critical — sidebar will show empty state
    }
  }

  /**
   * Fetch presence data from a space's members endpoint and merge it into the
   * hub-level human members list.  Called from handleRailLoaded when the user
   * is in the global /chat view so that presence indicators render on first load.
   */
  private async refreshHubMemberPresence(projectId: string): Promise<void> {
    if (!projectId) return;
    try {
      const res = await apiFetch(`/api/v1/chat/spaces/${encodeURIComponent(projectId)}/members`);
      if (!res.ok) return;
      const data = (await res.json()) as {
        humans?: Array<{ id: string; presenceState?: 'active' | 'idle' | '' }>;
      };
      if (!data.humans?.length) return;

      // Build a lookup of userId → presenceState. Members present in the
      // response are authoritative — including those with no presence, so a
      // user who went offline loses their indicator instead of keeping a
      // stale one.
      const presenceMap = new Map<string, 'active' | 'idle' | ''>();
      for (const h of data.humans) {
        presenceMap.set(h.id, h.presenceState || '');
      }

      // Merge presence into existing hub members
      const updated = this.v2HumanMembers.map((m) => {
        const ps = presenceMap.get(m.id);
        return ps !== undefined && ps !== m.presenceState ? { ...m, presenceState: ps } : m;
      });

      if (updated.some((m, i) => m.presenceState !== this.v2HumanMembers[i].presenceState)) {
        this.v2HumanMembers = updated;
      }
    } catch {
      // Non-critical — presence will still update via SSE events
    }
  }

  /**
   * Fetch DM conversations and extract peer IDs with unread messages
   * for the blue unread dot on member avatars.
   */
  private async loadUnreadDMPeers(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/chat/dms');
      if (!res.ok) return;
      const data = (await res.json()) as {
        dms?: Array<{
          peerId: string;
          hasUnread: boolean;
        }>;
      };
      const unreadIds = (data.dms || [])
        .filter((dm) => dm.hasUnread)
        .map((dm) => dm.peerId);
      // Only update if changed to avoid unnecessary re-renders
      if (
        unreadIds.length !== this.v2UnreadFromIds.length ||
        unreadIds.some((id, i) => id !== this.v2UnreadFromIds[i])
      ) {
        this.v2UnreadFromIds = unreadIds;
      }
    } catch {
      // Non-critical — unread dots just won't show
    }
  }

  private async loadV2Members(projectId: string): Promise<void> {
    if (!projectId) return;
    try {
      const res = await apiFetch(`/api/v1/chat/spaces/${encodeURIComponent(projectId)}/members`);
      if (res.ok) {
        const data = (await res.json()) as {
          humans?: Array<{
            id: string;
            kind: 'user';
            displayName: string;
            email?: string;
            avatarUrl?: string;
            role?: string;
            presenceState?: 'active' | 'idle' | '';
          }>;
          agents?: Array<{
            id: string;
            kind: 'agent';
            displayName: string;
            slug?: string;
            phase?: string;
            activity?: string;
            lastSeen?: string;
            projectId?: string;
          }>;
          members?: SpaceMember[];
        };
        // Populate the sidebar member arrays
        this.v2HumanMembers = (data.humans || []).map((h) => ({
          id: h.id,
          kind: 'user' as const,
          displayName: h.displayName,
          email: h.email || '',
          avatarUrl: h.avatarUrl || '',
          role: h.role || '',
          presenceState: h.presenceState || '',
        }));
        this.v2AgentMembers = (data.agents || []).map((a) => ({
          id: a.id,
          kind: 'agent' as const,
          displayName: a.displayName,
          slug: a.slug || '',
          phase: a.phase || '',
          activity: a.activity || '',
          lastSeen: a.lastSeen || '',
          projectId: a.projectId || projectId,
        }));
        // Seed the shared agent map so SSE status deltas have a baseline to
        // merge onto — otherwise they are buffered and never notify.
        stateManager.seedAgents(this.v2AgentMembers.map(agentMemberToAgent));
        // Also populate the legacy v2Members for the thread component
        this.v2Members = [
          ...(data.humans || []).map((h) => ({
            id: h.id,
            name: h.displayName,
            email: h.email || '',
            avatarUrl: h.avatarUrl || '',
            kind: 'user' as const,
          })),
          ...(data.agents || []).map((a) => ({
            id: a.id,
            name: a.displayName,
            email: '',
            slug: a.slug || '',
            kind: 'agent' as const,
          })),
          ...(data.members || []),
        ];
      }
    } catch {
      // Non-critical
    }
  }

  /** Handle presence SSE events to update member presence in real-time. */
  private handlePresenceUpdated(e: Event): void {
    const detail = (e as CustomEvent).detail as {
      data?: { userId?: string; state?: string; displayName?: string };
      userId?: string;
      state?: string;
    };
    const eventData = detail?.data || detail;
    const userId = (eventData as Record<string, unknown>).userId as string | undefined;
    const state = (eventData as Record<string, unknown>).state as string | undefined;

    if (!userId || !state) return;

    // Update the human member's presence state
    const updatedHumans = this.v2HumanMembers.map((h) => {
      if (h.id === userId) {
        return { ...h, presenceState: state as 'active' | 'idle' };
      }
      return h;
    });

    // Only trigger re-render if something actually changed
    const changed = updatedHumans.some(
      (h, i) => h.presenceState !== this.v2HumanMembers[i].presenceState
    );
    if (changed) {
      this.v2HumanMembers = updatedHumans;
    }
  }

  /** Handle typing SSE events to show typing overlay on member avatars. */
  private handleChatTyping(e: Event): void {
    const detail = (e as CustomEvent).detail as {
      data?: { userId?: string; displayName?: string };
      userId?: string;
    };
    const eventData = detail?.data || detail;
    const userId = (eventData as Record<string, unknown>).userId as string | undefined;

    if (!userId) return;

    // Skip self
    const currentUserId = this.pageData?.user?.id || '';
    if (userId === currentUserId) return;

    // Clear existing timer for this user
    const existing = this._typingTimers.get(userId);
    if (existing) {
      clearTimeout(existing);
    }

    // Set a new timer to expire the typing indicator after 6s
    const timer = setTimeout(() => {
      this._typingTimers.delete(userId);
      this.v2TypingUserIds = this.v2TypingUserIds.filter((id) => id !== userId);
    }, 6000);
    this._typingTimers.set(userId, timer);

    // Add to typing list if not already present
    if (!this.v2TypingUserIds.includes(userId)) {
      this.v2TypingUserIds = [...this.v2TypingUserIds, userId];
    }
  }

  /** Start sending presence heartbeats every 60s while the tab is focused. */
  private startPresenceHeartbeat(): void {
    // Stop any existing heartbeat
    this.stopPresenceHeartbeat();

    // Send initial heartbeat
    this.sendPresenceHeartbeat();

    // Send heartbeat every 60 seconds
    this._presenceInterval = setInterval(() => {
      if (document.hasFocus()) {
        this.sendPresenceHeartbeat();
      }
    }, 60000);

    // Send heartbeat on focus regain
    window.addEventListener('focus', this._onFocusPresence);
  }

  /** Stop the presence heartbeat interval. */
  private stopPresenceHeartbeat(): void {
    if (this._presenceInterval) {
      clearInterval(this._presenceInterval);
      this._presenceInterval = null;
    }
    window.removeEventListener('focus', this._onFocusPresence);
  }

  /** Focus handler for presence heartbeat. */
  private _onFocusPresence = (): void => {
    this.sendPresenceHeartbeat();
  };

  /** Send a presence heartbeat to the server. */
  private sendPresenceHeartbeat(): void {
    void apiFetch('/api/v1/chat/presence', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ projectIds: this._presenceProjectIds }),
    });
  }

  /** Handle member click from the members sidebar to open a DM. */
  private handleMemberClick(e: CustomEvent): void {
    const detail = e.detail as {
      memberId: string;
      memberKind: 'user' | 'agent';
      displayName: string;
    };
    if (!detail) return;

    const dmKey = this.buildDMKey(detail.memberId, detail.memberKind);
    if (dmKey) {
      this.v2Conversation = {
        conversationKey: dmKey,
        projectId: '',
        projectSlug: '',
        threadName: '',
        defaultAgent: '',
        isDM: true,
        peerName: detail.displayName,
        peerId: detail.memberId,
        peerKind: detail.memberKind,
      };
      this.classList.add('thread-open');

      // Update the URL with the full DM key so parseV2Route can use it directly.
      const dmPath = `/chat/dm/${encodeURIComponent(dmKey)}`;
      const base = import.meta.env.BASE_URL;
      const browserPath = base && base !== '/' ? base.replace(/\/$/, '') + dmPath : dmPath;
      window.history.pushState({}, '', browserPath);

      dispatchPageTitle(this, detail.displayName, 'Chat');
      return;
    }

    // User ID not available — resolve via API
    void this.resolveDMByPeerId(detail.memberId, detail.memberKind, detail.displayName);
  }

  /**
   * Safely construct a DM conversation key. Returns null if the current user
   * ID is not available, preventing broken keys with empty segments.
   */
  private buildDMKey(peerId: string, peerKind: 'user' | 'agent'): string | null {
    const userId = this.pageData?.user?.id;
    if (!userId) return null;

    if (peerKind === 'agent') {
      return `dm:agent:${peerId}:user:${userId}`;
    }
    const ids = [peerId, userId].sort();
    return `dm:user:${ids[0]}:user:${ids[1]}`;
  }

  // =========================================================================
  // Render
  // =========================================================================

  override render() {
    if (this.isV2) {
      return this.renderV2();
    }
    return this.renderV1();
  }

  // ---- DEPRECATED(wave-1): Remove after v2 is stable and flag is permanently ON. ----

  private renderV1() {
    return html`
      <div class="thread-rail">
        <div class="rail-header">
          <span>Conversations</span>
          <a
            href="/"
            @click=${(e: Event) => {
              e.preventDefault();
              navigateTo('/');
            }}
          >
            <sl-icon name="arrow-left"></sl-icon>
            App
          </a>
        </div>
        <div class="thread-list">
          ${this.loadingThreads
            ? html`<div class="loading-rail"><sl-spinner></sl-spinner></div>`
            : this.threads.length === 0
              ? html`<div class="loading-rail" style="font-size: 0.8125rem">
                  No conversations yet
                </div>`
              : this.threads.map((t) => this.renderThreadItem(t))}
        </div>
      </div>

      <div class="thread-content">
        ${this.selectedAgentId
          ? this.renderSelectedThread()
          : html`
              <div class="empty-state">
                <sl-icon name="chat-dots"></sl-icon>
                <span class="title">Select a conversation</span>
                <span class="subtitle">Choose an agent from the left to start chatting</span>
              </div>
            `}
      </div>
    `;
  }

  private renderThreadItem(thread: ChatThread) {
    const isSelected =
      thread.agentId === this.selectedAgentId || thread.agentSlug === this.selectedAgentId;
    const displayName = thread.agentName || thread.agentSlug || thread.agentId;
    const avatarColor = hashColor(thread.agentId);
    const initials = getInitials(displayName);
    const timeStr = thread.lastMessage?.createdAt
      ? this.formatRelativeTime(thread.lastMessage.createdAt)
      : '';

    return html`
      <div
        class="thread-item ${isSelected ? 'selected' : ''}"
        @click=${() => this.selectThread(thread)}
      >
        <div class="agent-avatar" style="background: ${avatarColor}">${initials}</div>
        <div class="thread-info">
          <div class="thread-name">
            <span>${displayName}</span>
            ${thread.hasUnread ? html`<span class="unread-dot"></span>` : nothing}
          </div>
          ${thread.lastMessage
            ? html`<div class="thread-preview">${thread.lastMessage.msg}</div>`
            : nothing}
        </div>
        ${timeStr ? html`<span class="thread-time">${timeStr}</span>` : nothing}
      </div>
    `;
  }

  private renderSelectedThread() {
    return html`
      <scion-chat-thread
        agentId=${this.selectedAgentId}
        agentName=${this.selectedAgentName}
        ?canSend=${this.selectedAgentCanSend}
      ></scion-chat-thread>
    `;
  }

  // ---- V2 Render ----

  private renderV2() {
    return html`
      <div class="v2-rail">
        ${this.v2SpaceRailLoaded
          ? html`
              <scion-chat-space-rail
                selectedKey=${this.v2Conversation?.conversationKey || ''}
                @thread-select=${this.handleThreadSelect}
                @navigate-app=${this.handleNavigateApp}
                @reset-view=${this.handleResetView}
              ></scion-chat-space-rail>
            `
          : html`<div class="loading-rail"><sl-spinner></sl-spinner></div>`}
      </div>

      <div class="thread-content">
        ${this.v2Conversation
          ? this.renderV2Conversation()
          : html`
              <div class="empty-state">
                <sl-icon name="chat-dots"></sl-icon>
                <span class="title">Select a conversation</span>
                <span class="subtitle">Choose a thread from the left, or click a member to start a DM</span>
              </div>
            `}
      </div>

      <div class="v2-members ${this.v2MembersExpanded ? '' : 'collapsed'}">
        <div class="v2-members-header">
          <span>Members</span>
        </div>
        <scion-chat-members
          .humans=${this.v2HumanMembers}
          .agents=${this.v2AgentMembers}
          .typingUserIds=${this.v2TypingUserIds}
          .unreadFromIds=${this.v2UnreadFromIds}
          current-user-id="${this.pageData?.user?.id || ''}"
          dm-peer-id="${this.v2Conversation?.isDM ? this.v2Conversation.peerId : ''}"
          @member-click=${this.handleMemberClick}
          @reset-view=${this.handleResetView}
        ></scion-chat-members>
      </div>
    `;
  }

  /** Look up the project slug for an agent DM peer. */
  private getAgentProjectSlug(peerId: string): string {
    // First, check if the agent member has a projectId and resolve its slug
    const agent = this.v2AgentMembers.find((a) => a.id === peerId);
    if (agent?.projectId) {
      const slug = this._projectIdToSlug.get(agent.projectId);
      if (slug) return slug;
    }

    // Fallback: derive from the current conversation
    if (this.v2Conversation?.projectId) {
      return this._projectIdToSlug.get(this.v2Conversation.projectId) || '';
    }
    // Fallback: check if we have a single project
    if (this._projectIdToSlug.size === 1) {
      return Array.from(this._projectIdToSlug.values())[0];
    }
    return '';
  }

  private renderV2Conversation() {
    if (!this.v2Conversation) return nothing;
    const conv = this.v2Conversation;

    // Look up the project slug for agent DMs
    const agentProjectSlug = conv.isDM && conv.peerKind === 'agent' && conv.peerId
      ? this.getAgentProjectSlug(conv.peerId)
      : '';

    return html`
      ${conv.isDM && conv.peerName
        ? html`
            <div class="v2-thread-header">
              ${conv.peerKind === 'agent' && agentProjectSlug
                ? html`<sl-icon name="folder" style="font-size: 0.75rem; color: var(--scion-text-muted, #64748b)"></sl-icon>
                        <span style="font-size: 0.8125rem; color: var(--scion-text-muted, #64748b)">${agentProjectSlug}</span>`
                : nothing}
              ${conv.peerKind === 'agent'
                ? html`<span style="font-size: 0.875rem">🤖</span>`
                : html`<sl-icon
                    name="person"
                    style="font-size: 0.875rem; color: var(--scion-text-muted)"
                  ></sl-icon>`}
              <span>${conv.peerName}</span>
              <div class="header-actions" style="display: flex; align-items: center; gap: 0.25rem; margin-left: auto;">
                <sl-tooltip content="Search messages">
                  <sl-icon-button
                    name="search"
                    label="Search messages"
                    @click=${() => void this.openSearch()}
                  ></sl-icon-button>
                </sl-tooltip>
                <sl-tooltip content="Show/Hide members">
                  <sl-icon-button
                    name="people"
                    label="Show/Hide members"
                    @click=${() => {
                      this.v2MembersExpanded = !this.v2MembersExpanded;
                    }}
                  ></sl-icon-button>
                </sl-tooltip>
              </div>
            </div>
          `
        : conv.threadName
          ? html`
              <div class="v2-thread-header">
                <span class="hash">#</span>
                <span>${conv.threadName}</span>
                ${conv.defaultAgent
                  ? html`
                      <sl-tooltip content="Default agent: ${conv.defaultAgent}">
                        <span>🤖</span>
                      </sl-tooltip>
                    `
                  : nothing}
                <div class="header-actions" style="display: flex; align-items: center; gap: 0.25rem; margin-left: auto;">
                  <sl-tooltip content="Search messages">
                    <sl-icon-button
                      name="search"
                      label="Search messages"
                      @click=${() => void this.openSearch()}
                    ></sl-icon-button>
                  </sl-tooltip>
                  <sl-tooltip content="Show/Hide members">
                    <sl-icon-button
                      name="people"
                      label="Show/Hide members"
                      @click=${() => {
                        this.v2MembersExpanded = !this.v2MembersExpanded;
                      }}
                    ></sl-icon-button>
                  </sl-tooltip>
                </div>
              </div>
            `
          : nothing}
      ${this.v2SearchActive && this.v2SearchLoaded
        ? html`
            <scion-chat-search
              projectId=${conv.projectId}
              conversationKey=${conv.conversationKey}
              conversationName=${conv.isDM ? conv.peerName : conv.threadName ? '#' + conv.threadName : ''}
              @search-close=${this.handleSearchClose}
              @search-navigate=${this.handleSearchNavigate}
            ></scion-chat-search>
          `
        : html`
            <scion-chat-thread
              conversationKey=${conv.conversationKey}
              projectId=${conv.projectId}
              threadName=${conv.threadName}
              .defaultAgent=${conv.defaultAgent}
              ?isDM=${conv.isDM}
              peerName=${conv.peerName}
              currentUserId=${this.pageData?.user?.id || ''}
              ?canSend=${true}
              .members=${this.v2Members}
              .agents=${this.getAgentsFromMembers()}
              @default-agent-changed=${this.handleDefaultAgentChanged}
            ></scion-chat-thread>
          `}
    `;
  }

  /** Open the search panel, lazy-loading the component if needed. */
  private async openSearch(): Promise<void> {
    if (!this.v2SearchLoaded) {
      await loadChatSearch();
      this.v2SearchLoaded = true;
    }
    this.v2SearchActive = true;
    // Focus the search input after render.
    requestAnimationFrame(() => {
      const search = this.shadowRoot?.querySelector('scion-chat-search') as
        | import('../shared/chat/chat-search.js').ScionChatSearch
        | null;
      search?.open();
    });
  }

  /** Handle search panel close. */
  private handleSearchClose(): void {
    this.v2SearchActive = false;
  }

  /** Handle navigation from a search result click. */
  private handleSearchNavigate(e: CustomEvent): void {
    const detail = e.detail as {
      conversationKey: string;
      messageId: string;
      projectId: string;
    };
    if (!detail) return;

    this.v2SearchActive = false;

    // If the result is in a different conversation, navigate to it.
    if (detail.conversationKey !== this.v2Conversation?.conversationKey) {
      const isDM = detail.conversationKey.startsWith('dm:');
      if (isDM) {
        navigateTo(`/chat/dm/${encodeURIComponent(detail.conversationKey)}`);
      } else if (detail.projectId) {
        const slug = this._projectIdToSlug.get(detail.projectId);
        if (slug) {
          navigateTo(
            `/chat/${encodeURIComponent(slug)}/${encodeURIComponent(detail.conversationKey)}`
          );
        } else {
          navigateTo(
            `/chat/space/${encodeURIComponent(detail.projectId)}/thread/${encodeURIComponent(detail.conversationKey)}`
          );
        }
      }
    }
    // TODO: scroll to the specific messageId within the conversation
  }

  /** Extract agent members as Agent-like objects for the mention autocomplete. */
  private getAgentsFromMembers(): import('../../shared/types.js').Agent[] {
    return this.v2Members
      .filter((m) => m.kind === 'agent')
      .map((m) => ({
        id: m.id,
        name: m.name,
        slug: m.name,
        projectId: this.v2Conversation?.projectId || '',
        template: '',
        phase: 'running' as const,
        status: 'active' as const,
      }));
  }

  // ---- Shared utilities ----

  private formatRelativeTime(iso: string): string {
    const d = new Date(iso);
    if (isNaN(d.getTime())) return '';
    const now = Date.now();
    const diffMs = now - d.getTime();
    const diffMin = Math.floor(diffMs / 60000);

    if (diffMin < 1) return 'now';
    if (diffMin < 60) return `${diffMin}m`;
    const diffHrs = Math.floor(diffMin / 60);
    if (diffHrs < 24) return `${diffHrs}h`;
    const diffDays = Math.floor(diffHrs / 24);
    if (diffDays < 7) return `${diffDays}d`;

    return d.toLocaleDateString('en', { month: 'short', day: 'numeric' });
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-chat': ScionPageChat;
  }
}
