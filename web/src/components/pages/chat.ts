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
import type { TemplateResult } from 'lit';
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
 * Normalise a Hub timestamp: Go marshals a zero time as year 0001 rather than
 * omitting it, and rendering that as a date would be worse than showing
 * nothing. Returns '' for absent or zero timestamps.
 */
function realTimestamp(value?: string): string {
  return value && !value.startsWith('0001') ? value : '';
}

/**
 * Breakpoint below which the three chat panels become swipeable screens.
 * Mirrors the `max-width: 768px` media query in the component styles.
 */
const MOBILE_BREAKPOINT_PX = 768;

/** A horizontal swipe must cover this many px to count as a flick. */
const SWIPE_FLICK_PX = 50;

/** A flick only counts if it completes within this many ms. */
const SWIPE_FLICK_MS = 300;

/** A slow drag counts as a swipe once it covers this many px. */
const SWIPE_DRAG_PX = 100;

/** A gesture is horizontal once it moves this many px sideways. */
const SWIPE_AXIS_LOCK_PX = 10;

/**
 * Fold a mention slug onto the form the composer inserts: lowercased, with
 * runs of whitespace as dashes. Lets `@my-agent` match a display name of
 * "My Agent" as well as the agent's own slug.
 */
function normalizeMentionSlug(value: string): string {
  return value.trim().toLowerCase().replace(/\s+/g, '-');
}

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
    ...(m.detailMessage ? { detail: { message: m.detailMessage } } : {}),
    lastActivityEvent: m.lastActivityEvent || '',
  };
}

/**
 * The status detail an agent last reported. SSE deltas carry it nested under
 * `detail`; the REST list endpoints carry it flattened as `message`.
 */
function agentDetailMessage(a: { detail?: { message?: string }; message?: string }): string {
  return a.detail?.message || a.message || '';
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
  /**
   * Muted state of this conversation for the current user. Resolved from the
   * DM list; a DM that does not exist yet is unmuted.
   */
  muted?: boolean;
}

/** The parts of a thread that a URL does not carry and have to be resolved. */
interface ThreadMeta {
  threadName: string;
  defaultAgent: string;
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
  /**
   * Which of the three panels is on screen. Only meaningful under the mobile
   * breakpoint — on desktop all three are visible and this is inert.
   *
   * Defaults to the rail: with no conversation open the centre panel is just
   * an empty state, and the rail is the only way to pick a conversation.
   * Anything that opens a conversation switches this to 'center'.
   */
  @state() private mobilePanel: 'left' | 'center' | 'right' = 'left';
  private _touchStartX = 0;
  private _touchStartY = 0;
  private _touchStartTime = 0;
  private _isSwiping = false;

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

    /* Section heading, styled like the dashboard nav's section titles. */
    .rail-header {
      display: flex;
      align-items: center;
      padding: 0.75rem 1rem 0.5rem;
      font-size: 0.6875rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
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

    .thread-content,
    .v2-content {
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

    /*
     * The three panels live in one track so the mobile breakpoint can slide
     * between them. On desktop the track is just a flex row filling the page.
     */
    .v2-panels {
      flex: 1;
      min-width: 0;
      display: flex;
      overflow: hidden;
    }

    .v2-rail {
      width: 260px;
      min-width: 200px;
      max-width: 320px;
      border-right: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
      display: flex;
      flex-direction: column;
      overflow: hidden;
    }

    .v2-rail scion-chat-space-rail {
      flex: 1;
      min-height: 0;
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
      gap: 0.25rem;
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

    /*
     * Mobile-only header controls. On a narrow viewport the three panels are
     * a swipe track, so the header needs explicit affordances for moving
     * between them; the desktop members toggle is meaningless there because
     * the tray is always mounted in the track.
     */
    .mobile-back,
    .mobile-members,
    .empty-state .subtitle.mobile-only {
      display: none;
    }

    @media (max-width: 768px) {
      .mobile-back,
      .mobile-members {
        display: inline-flex;
      }

      .desktop-members {
        display: none;
      }

      .empty-state .subtitle.desktop-only {
        display: none;
      }

      .empty-state .subtitle.mobile-only {
        display: inline;
      }

      /* V1: one screen at a time, driven by the thread-open host class. */
      .thread-rail {
        width: 100%;
        max-width: none;
      }

      :host(.thread-open) .thread-rail {
        display: none;
      }

      :host(:not(.thread-open)) .thread-content {
        display: none;
      }

      /*
       * V2: each panel is absolutely positioned at one viewport wide and
       * translated into or out of view based on data-panel. A 300%-wide
       * sliding track does not survive the nested flex ancestors
       * (:host inside chat-shell inside app-shell), which force the width
       * back down and reveal all three panels at once.
       */
      .v2-panels {
        position: relative;
        /* 'overflow: hidden' comes from the base rule and clips the
           off-screen panels. */
      }

      .v2-panels .v2-rail,
      .v2-panels .v2-content,
      .v2-panels .v2-members {
        position: absolute;
        top: 0;
        left: 0;
        width: 100%;
        height: 100%;
        min-width: 0;
        max-width: none;
        flex: none;
        transition: transform 0.3s ease;
        overflow-y: auto;
        /* The global '* { box-sizing: border-box }' does not cross the shadow
           boundary, so these panels default to content-box: the desktop
           border-right/border-left would add 1px on top of the full-viewport
           width and push content off the left edge on iOS Safari. */
        box-sizing: border-box;
        border: 0;
      }

      /* ---- Left panel active ---- */
      .v2-panels[data-panel='left'] .v2-rail {
        transform: translateX(0);
      }

      .v2-panels[data-panel='left'] .v2-content {
        transform: translateX(100%);
      }

      .v2-panels[data-panel='left'] .v2-members {
        transform: translateX(200%);
      }

      /* ---- Center panel active ---- */
      .v2-panels[data-panel='center'] .v2-rail {
        transform: translateX(-100%);
      }

      .v2-panels[data-panel='center'] .v2-content {
        transform: translateX(0);
      }

      .v2-panels[data-panel='center'] .v2-members {
        transform: translateX(100%);
      }

      /* ---- Right panel active ---- */
      .v2-panels[data-panel='right'] .v2-rail {
        transform: translateX(-200%);
      }

      .v2-panels[data-panel='right'] .v2-content {
        transform: translateX(-100%);
      }

      .v2-panels[data-panel='right'] .v2-members {
        transform: translateX(0);
      }

      /* The members panel is a swipe target on mobile, so it stays in the
         track even when the desktop toggle has collapsed it. */
      .v2-panels .v2-members.collapsed {
        display: flex;
      }
    }

    @media (prefers-reduced-motion: reduce) {
      .v2-panels .v2-rail,
      .v2-panels .v2-content,
      .v2-panels .v2-members {
        transition: none;
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
      // A DM has no membership of its own, so re-syncing here would silently
      // swap the member list the user was just looking at. Leave the sidebar on
      // whatever the previous view loaded.
      if (this.v2Conversation?.isDM) return;
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
      const known = this.knownThreadMeta(topicId);
      this.v2Conversation = {
        conversationKey: topicId,
        projectId,
        projectSlug: '',
        threadName: known.threadName,
        defaultAgent: known.defaultAgent,
        isDM: false,
        peerName: '',
        peerId: '',
        peerKind: 'user',
      };
      this.classList.add('thread-open');
      this.mobilePanel = 'center';
      void this.loadV2Members(projectId);
      this.applyThreadMeta(topicId, known);
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
      // A bare peer ID is a legitimate DM URL (resolveDMByPeerId turns it into a
      // key), so it must pass the guard alongside full DM keys.
      const dmKeyRegex = /^dm:(user|agent):[0-9a-f-]{36}:(user|agent):[0-9a-f-]{36}$/;
      const peerIdRegex = /^[0-9a-f-]{36}$/i;
      if (
        this.v2Conversation?.isDM &&
        dmKeyRegex.test(this.v2Conversation.conversationKey) &&
        !dmKeyRegex.test(segment) &&
        !peerIdRegex.test(segment)
      ) {
        return;
      }

      this.classList.add('thread-open');
      this.mobilePanel = 'center';
      dispatchPageTitle(this, 'DM', 'Chat');

      if (segment.startsWith('dm:')) {
        // Legacy DM key format (e.g. dm:agent:UUID:user:UUID) — use directly
        this.v2Conversation = {
          conversationKey: segment,
          projectId: this.inheritedProjectId(),
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
            projectId: this.inheritedProjectId(),
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
        const known = this.knownThreadMeta(threadId);
        this.v2Conversation = {
          conversationKey: threadId,
          projectId,
          projectSlug: segment1,
          threadName: known.threadName,
          defaultAgent: known.defaultAgent,
          isDM: false,
          peerName: '',
          peerId: '',
          peerKind: 'user',
        };
        this.classList.add('thread-open');
        this.mobilePanel = 'center';
        void this.loadV2Members(projectId);
        this.applyThreadMeta(threadId, known);
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
    // No conversation to show — put the mobile view back on the rail.
    this.mobilePanel = 'left';
    void this.loadHubMembers();
  }

  /**
   * Resolve a project slug to a project ID via the API, then open the
   * specified thread. Used for deep-link cold loads before the rail populates.
   */
  private async resolveSlugAndOpenThread(slug: string, threadId: string): Promise<void> {
    const projectId = await this.resolveProjectBySlug(slug);
    if (!projectId) return;

    const known = this.knownThreadMeta(threadId);
    this.v2Conversation = {
      conversationKey: threadId,
      projectId,
      projectSlug: slug,
      threadName: known.threadName,
      defaultAgent: known.defaultAgent,
      isDM: false,
      peerName: '',
      peerId: '',
      peerKind: 'user',
    };
    this.classList.add('thread-open');
    this.mobilePanel = 'center';
    void this.loadV2Members(projectId);
    this.applyThreadMeta(threadId, known);
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
   * Select a space by slug: wait for the rail to be available, then open
   * #general for the given project — or, on mobile, just expand the space.
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

    // On mobile the rail is a screen of its own, so opening a space expands it
    // in place. Dropping the user into #general would slide the rail — and the
    // thread list they came to choose from — off-screen.
    if (this.isMobileViewport()) {
      rail.expandSpace(projectId);
      this.mobilePanel = 'left';
      void this.loadV2Members(projectId);
      dispatchPageTitle(this, slug, 'Chat');
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
      this.mobilePanel = 'center';
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
        detailMessage: agentDetailMessage(agent) || existing?.detailMessage || '',
        lastActivityEvent:
          realTimestamp(agent.lastActivityEvent) ||
          realTimestamp(agent.updated) ||
          existing?.lastActivityEvent ||
          '',
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

  private handleChatMessage(e: Event): void {
    // The sender is done typing once their message arrives — clear the avatar
    // overlay immediately instead of letting the 6s expiry run out.
    const detail = (e as CustomEvent).detail as { data?: { senderId?: string } } | undefined;
    const eventData = (detail?.data ?? detail) as Record<string, unknown> | undefined;
    this.clearTypingForUser(eventData?.senderId as string | undefined);

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
    this.mobilePanel = 'center';

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

  /** Reset to the global /chat view (no conversation selected). */
  private handleResetView(): void {
    this.v2Conversation = null;
    this.v2MembersExpanded = true; // Always show tray in base view
    this.classList.remove('thread-open');
    // No conversation to show — put the mobile view back on the rail.
    this.mobilePanel = 'left';
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
   * Thread metadata already resolved for this conversation. A URL only
   * carries the topic ID, so parseV2Route rebuilds the conversation from
   * scratch on every re-parse (the rail loading triggers one) — without
   * carrying the name and default agent forward they would be dropped and
   * re-fetched each time.
   */
  private knownThreadMeta(conversationKey: string): ThreadMeta {
    const conv = this.v2Conversation;
    if (!conv || conv.isDM || conv.conversationKey !== conversationKey) {
      return { threadName: '', defaultAgent: '' };
    }
    return { threadName: conv.threadName, defaultAgent: conv.defaultAgent };
  }

  /** Title the page for a thread and fetch whatever the route did not carry. */
  private applyThreadMeta(conversationKey: string, known: ThreadMeta): void {
    dispatchPageTitle(this, known.threadName ? `#${known.threadName}` : 'Thread', 'Chat');
    if (!known.threadName || !known.defaultAgent) {
      void this.fetchThreadDetails(conversationKey);
    }
  }

  /**
   * Fetch a thread's name and default agent from the topic detail endpoint.
   * Deep links, reloads and Forward arrive with only the topic ID in the URL,
   * so this is the only source of the name the header renders.
   */
  private async fetchThreadDetails(conversationKey: string): Promise<void> {
    try {
      const res = await apiFetch(
        `/api/v1/chat/topics/${encodeURIComponent(conversationKey)}`
      );
      if (!res.ok) return;
      const data = (await res.json()) as { name?: string; defaultAgent?: string };
      const conv = this.v2Conversation;
      // The user may have moved on while the request was in flight.
      if (!conv || conv.conversationKey !== conversationKey) return;
      this.v2Conversation = {
        ...conv,
        threadName: data.name || conv.threadName,
        defaultAgent: data.defaultAgent || conv.defaultAgent,
      };
      if (data.name) {
        dispatchPageTitle(this, `#${data.name}`, 'Chat');
      }
    } catch {
      // Non-critical — the thread still works without its metadata
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
          muted?: boolean;
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
          muted: dm.muted === true,
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
            muted?: boolean;
          }>;
        };
        const dm = data.dms?.find((d) => d.peerId === peerId);
        if (dm) {
          const peerName = dm.peerName || dm.peerSlug || dm.peerEmail || displayName || dm.peerId;
          this.v2Conversation = {
            conversationKey: dm.conversationKey,
            projectId: this.inheritedProjectId(),
            projectSlug: '',
            threadName: '',
            defaultAgent: '',
            isDM: true,
            peerName,
            peerId: dm.peerId,
            peerKind: dm.peerKind,
            muted: dm.muted === true,
          };
          this.classList.add('thread-open');
          this.mobilePanel = 'center';
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
        projectId: this.inheritedProjectId(),
        projectSlug: '',
        threadName: '',
        defaultAgent: '',
        isDM: true,
        peerName: displayName,
        peerId,
        peerKind,
      };
      this.classList.add('thread-open');
      this.mobilePanel = 'center';
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
            message?: string;
            detail?: { message?: string };
            lastSeen?: string;
            lastActivityEvent?: string;
            updated?: string;
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
          detailMessage: agentDetailMessage(a),
          lastActivityEvent: realTimestamp(a.lastActivityEvent) || realTimestamp(a.updated),
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
          muted?: boolean;
        }>;
      };
      // A muted DM raises no dot: muting is the user saying "stop telling me
      // about this", and the avatar dot is the telling (#1029).
      const unreadIds = (data.dms || [])
        .filter((dm) => dm.hasUnread && !dm.muted)
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
            message?: string;
            lastSeen?: string;
            lastActivityEvent?: string;
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
          detailMessage: agentDetailMessage(a),
          lastActivityEvent: realTimestamp(a.lastActivityEvent),
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

  /** Drop a user's typing overlay (and its expiry timer), if one is active. */
  private clearTypingForUser(userId: string | undefined): void {
    if (!userId) return;
    const timer = this._typingTimers.get(userId);
    if (timer) {
      clearTimeout(timer);
      this._typingTimers.delete(userId);
    }
    if (this.v2TypingUserIds.includes(userId)) {
      this.v2TypingUserIds = this.v2TypingUserIds.filter((id) => id !== userId);
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

    this.openDM(detail.memberId, detail.memberKind, detail.displayName);
  }

  // ---- Mobile swipe navigation ----

  private handleTouchStart(e: TouchEvent): void {
    const touch = e.touches[0];
    if (!touch) return;
    this._touchStartX = touch.clientX;
    this._touchStartY = touch.clientY;
    this._touchStartTime = Date.now();
    this._isSwiping = false;
  }

  private handleTouchMove(e: TouchEvent): void {
    if (!this._touchStartTime) return;
    const touch = e.touches[0];
    if (!touch) return;

    const dx = touch.clientX - this._touchStartX;
    const dy = touch.clientY - this._touchStartY;

    // Horizontal only — a vertical drag is the message list scrolling.
    if (Math.abs(dx) > Math.abs(dy) && Math.abs(dx) > SWIPE_AXIS_LOCK_PX) {
      this._isSwiping = true;
    }
  }

  private handleTouchEnd(e: TouchEvent): void {
    const wasSwiping = this._isSwiping;
    const startX = this._touchStartX;
    const elapsed = Date.now() - this._touchStartTime;
    this._touchStartTime = 0;
    this._isSwiping = false;

    if (!wasSwiping || !this.isMobileViewport()) return;

    const touch = e.changedTouches[0];
    if (!touch) return;

    const dx = touch.clientX - startX;
    const isSwipe =
      (Math.abs(dx) > SWIPE_FLICK_PX && elapsed < SWIPE_FLICK_MS) || Math.abs(dx) > SWIPE_DRAG_PX;
    if (!isSwipe) return;

    if (dx > 0) {
      this.handleSwipeRight();
    } else {
      this.handleSwipeLeft();
    }
  }

  /**
   * Drop focus so iOS retracts the software keyboard. The composer input lives
   * several shadow roots down, and `document.activeElement` only reports the
   * outermost host, so descend to the real focused element before blurring.
   */
  private dismissKeyboard(): void {
    let el = document.activeElement as HTMLElement | null;
    while (el?.shadowRoot?.activeElement) {
      el = el.shadowRoot.activeElement as HTMLElement;
    }
    el?.blur?.();
  }

  /** Swiping right reveals the panel to the left of the current one. */
  private handleSwipeRight(): void {
    if (this.mobilePanel === 'center') {
      // Leaving the composer behind: the keyboard would otherwise stay up and
      // cover the panel being swiped in.
      this.dismissKeyboard();
      this.mobilePanel = 'left';
    } else if (this.mobilePanel === 'right') {
      this.mobilePanel = 'center';
    }
  }

  /** Swiping left reveals the panel to the right of the current one. */
  private handleSwipeLeft(): void {
    if (this.mobilePanel === 'center') {
      this.dismissKeyboard();
      this.mobilePanel = 'right';
    } else if (this.mobilePanel === 'left') {
      this.mobilePanel = 'center';
    }
  }

  /** Are we under the breakpoint where panels behave as separate screens? */
  private isMobileViewport(): boolean {
    return window.innerWidth <= MOBILE_BREAKPOINT_PX;
  }

  /** Open the DM conversation with a member in the centre panel. */
  private openDM(memberId: string, memberKind: 'user' | 'agent', displayName: string): void {
    const dmKey = this.buildDMKey(memberId, memberKind);
    if (dmKey) {
      this.v2Conversation = {
        conversationKey: dmKey,
        projectId: this.inheritedProjectId(),
        projectSlug: '',
        threadName: '',
        defaultAgent: '',
        isDM: true,
        peerName: displayName,
        peerId: memberId,
        peerKind: memberKind,
      };
      this.classList.add('thread-open');
      this.mobilePanel = 'center';

      // Update the URL with the full DM key so parseV2Route can use it directly.
      const dmPath = `/chat/dm/${encodeURIComponent(dmKey)}`;
      const base = import.meta.env.BASE_URL;
      const browserPath = base && base !== '/' ? base.replace(/\/$/, '') + dmPath : dmPath;
      window.history.pushState({}, '', browserPath);

      dispatchPageTitle(this, displayName, 'Chat');
      return;
    }

    // User ID not available — resolve via API
    void this.resolveDMByPeerId(memberId, memberKind, displayName);
  }

  /**
   * Clicking an @mention in a message opens the DM with that entity. The slug
   * is what the composer inserted: an agent slug, or a display name lowercased
   * with spaces turned into dashes (see mention-autocomplete).
   */
  private handleMentionClick(e: CustomEvent): void {
    const slug = (e.detail as { slug?: string } | null)?.slug;
    if (!slug) return;

    const wanted = normalizeMentionSlug(slug);

    const agent = this.v2AgentMembers.find(
      (a) =>
        normalizeMentionSlug(a.slug || '') === wanted ||
        normalizeMentionSlug(a.displayName) === wanted
    );
    if (agent) {
      this.openDM(agent.id, 'agent', agent.displayName);
      return;
    }

    const human = this.v2HumanMembers.find(
      (h) =>
        normalizeMentionSlug(h.displayName) === wanted ||
        normalizeMentionSlug(h.email?.split('@')[0] || '') === wanted
    );
    if (human) {
      this.openDM(human.id, 'user', human.displayName);
    }
    // Unknown mention (e.g. a stale name): leave the view as it is.
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

  /**
   * The project a DM inherits: the one the user was already looking at when
   * they opened it. A DM belongs to no space, but its attachments still have
   * to be stored somewhere, and a project-scoped upload is what puts the file
   * where an agent in that space can read it. Empty on a cold load straight
   * into a DM, which the upload endpoint accepts.
   */
  private inheritedProjectId(): string {
    return this.v2Conversation?.projectId || '';
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
        <div class="rail-header"><span>Conversations</span></div>
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
      <div
        class="v2-panels"
        data-panel=${this.mobilePanel}
        @touchstart=${this.handleTouchStart}
        @touchmove=${this.handleTouchMove}
        @touchend=${this.handleTouchEnd}
        @mention-click=${this.handleMentionClick}
      >
        <div class="v2-rail">
          ${this.v2SpaceRailLoaded
            ? html`
                <scion-chat-space-rail
                  selectedKey=${this.v2Conversation?.conversationKey || ''}
                  @thread-select=${this.handleThreadSelect}
                  @reset-view=${this.handleResetView}
                ></scion-chat-space-rail>
              `
            : html`<div class="loading-rail"><sl-spinner></sl-spinner></div>`}
        </div>

        <div class="v2-content">
          ${this.v2Conversation
            ? this.renderV2Conversation()
            : html`
                <div class="empty-state">
                  <sl-icon name="chat-dots"></sl-icon>
                  <span class="title">Select a conversation</span>
                  <span class="subtitle desktop-only"
                    >Choose a thread from the left, or click a member to start a DM</span
                  >
                  <span class="subtitle mobile-only">Choose a thread to start chatting</span>
                </div>
              `}
        </div>

        <div class="v2-members ${this.v2MembersExpanded ? '' : 'collapsed'}">
          <div class="v2-members-header">
            ${this.renderMobileBackButton('center')}
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

  /**
   * Back chevron shown only on mobile, where the neighbouring panels are
   * off-screen and otherwise reachable only by an undiscoverable swipe.
   * The conversation header steps back to the rail; the members header
   * steps back to the conversation.
   */
  private renderMobileBackButton(target: 'left' | 'center' = 'left') {
    return html`
      <sl-icon-button
        class="mobile-back"
        name="chevron-left"
        label="Back"
        @click=${() => {
          this.dismissKeyboard();
          this.mobilePanel = target;
        }}
      ></sl-icon-button>
    `;
  }

  /**
   * Members control for the conversation header. On desktop it collapses the
   * members tray; on mobile the tray is a swipe panel, so the button slides
   * the track to it instead.
   */
  private renderMembersButtons() {
    return html`
      <sl-tooltip class="desktop-members" content="Show/Hide members">
        <sl-icon-button
          name="people"
          label="Show/Hide members"
          @click=${() => {
            this.v2MembersExpanded = !this.v2MembersExpanded;
          }}
        ></sl-icon-button>
      </sl-tooltip>
      <sl-icon-button
        class="mobile-members"
        name="people"
        label="Members"
        @click=${() => {
          this.dismissKeyboard();
          this.mobilePanel = 'right';
        }}
      ></sl-icon-button>
    `;
  }

  /**
   * Mute toggle for a DM. Threads carry theirs in the rail's context menu;
   * a DM has no rail row, so the conversation header is the only place a
   * user can silence one.
   */
  private renderDMMuteButton(conv: V2ConversationState): TemplateResult {
    const muted = conv.muted === true;
    return html`
      <sl-tooltip content=${muted ? 'Unmute conversation' : 'Mute conversation'}>
        <sl-icon-button
          class="dm-mute"
          name=${muted ? 'bell-slash' : 'bell'}
          label=${muted ? 'Unmute conversation' : 'Mute conversation'}
          @click=${(): void => void this.toggleDMMute()}
        ></sl-icon-button>
      </sl-tooltip>
    `;
  }

  /**
   * Flip the open DM's muted state. Applied locally first and rolled back if
   * the server refuses, so the bell never claims a state the server does not
   * have.
   */
  private async toggleDMMute(): Promise<void> {
    const conv = this.v2Conversation;
    if (!conv) return;
    const previous = conv.muted === true;
    const next = !previous;
    this.v2Conversation = { ...conv, muted: next };
    try {
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(conv.conversationKey)}/mute`,
        {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ muted: next }),
        }
      );
      if (!res.ok) throw new Error('mute failed');
      const data = (await res.json().catch(() => ({}))) as { muted?: boolean };
      if (typeof data.muted === 'boolean' && data.muted !== next) {
        // The user may have switched conversations while the request was in
        // flight; only reconcile if this DM is still the one on screen.
        if (this.v2Conversation?.conversationKey === conv.conversationKey) {
          this.v2Conversation = { ...this.v2Conversation, muted: data.muted };
        }
      }
    } catch {
      if (this.v2Conversation?.conversationKey === conv.conversationKey) {
        this.v2Conversation = { ...this.v2Conversation, muted: previous };
      }
    }
  }

  private renderV2Conversation() {
    if (!this.v2Conversation) return nothing;
    const conv = this.v2Conversation;

    // Look up the project slug for agent DMs
    const agentProjectSlug = conv.isDM && conv.peerKind === 'agent' && conv.peerId
      ? this.getAgentProjectSlug(conv.peerId)
      : '';

    // The header renders for every open conversation, not only once the name
    // has resolved: on a deep link or reload the name arrives from the topic
    // endpoint a moment later, and until then the back / search / members
    // controls are the only way out of the conversation on mobile.
    return html`
      <div class="v2-thread-header">
        ${this.renderMobileBackButton()}
        ${conv.isDM
          ? html`
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
            `
          : html`
              ${conv.threadName
                ? html`<span class="hash">#</span><span>${conv.threadName}</span>`
                : nothing}
              ${conv.defaultAgent
                ? html`
                    <sl-tooltip content="Default agent: ${conv.defaultAgent}">
                      <span>🤖</span>
                    </sl-tooltip>
                  `
                : nothing}
            `}
        <div class="header-actions" style="display: flex; align-items: center; gap: 0.25rem; margin-left: auto;">
          ${conv.isDM ? this.renderDMMuteButton(conv) : nothing}
          <sl-tooltip content="Search messages">
            <sl-icon-button
              name="search"
              label="Search messages"
              @click=${() => void this.openSearch()}
            ></sl-icon-button>
          </sl-tooltip>
          ${this.renderMembersButtons()}
        </div>
      </div>
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
