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
 * Renders inside `<scion-chat-shell>` and supports two modes:
 *
 * **V1 (web.native_chat_v2 OFF):**
 * - Thread rail listing agents with last-message preview and unread dot
 * - `/chat` shows the rail with no thread selected
 * - `/chat/:agentId` opens the thread for that agent
 *
 * **V2 (web.native_chat_v2 ON):**
 * - Space rail (chat-space-rail) replacing agent-based thread rail
 * - Conversation view refactored to key-based
 * - Routes: `/chat`, `/chat/space/{projectId}`, `/chat/space/{projectId}/thread/{topicId}`, `/chat/dm/{key}`
 * - Members sidebar toggle (placeholder for W5)
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Capabilities } from '../../shared/types.js';
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

// ---- V1 types ----

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
  @state() private v2MembersExpanded = false;
  @state() private v2SpaceRailLoaded = false;
  /** Human members for the members sidebar (from the members endpoint). */
  @state() private v2HumanMembers: import('../shared/chat/chat-members.js').ChatHumanMember[] = [];
  /** Agent members for the members sidebar. */
  @state() private v2AgentMembers: import('../shared/chat/chat-members.js').ChatAgentMember[] = [];
  private _onChatMessage = this.handleChatMessage.bind(this);
  private _onChatTopic = this.handleChatTopic.bind(this);
  private _onPresenceUpdated = this.handlePresenceUpdated.bind(this);
  private _onRailLoaded = this.handleRailLoaded.bind(this);
  /** Whether the search panel is visible. */
  @state() private v2SearchActive = false;
  /** Whether the search component has been lazy-loaded. */
  @state() private v2SearchLoaded = false;
  /** Presence heartbeat interval timer. */
  private _presenceInterval: ReturnType<typeof setInterval> | null = null;
  /** Tracked project IDs for presence heartbeat. */
  private _presenceProjectIds: string[] = [];

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

    .v2-thread-header .members-btn {
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
      this.removeEventListener('rail-loaded', this._onRailLoaded);
      this.stopPresenceHeartbeat();
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
  // V1 Methods (preserved for flag-off compat)
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

    // Subscribe to SSE events
    stateManager.addEventListener('chat-message-received', this._onChatMessage);
    stateManager.addEventListener('chat-topic-updated', this._onChatTopic);
    stateManager.addEventListener('chat-presence-updated', this._onPresenceUpdated);

    // Listen for rail-loaded to set up the SSE scope with space IDs
    this.addEventListener('rail-loaded', this._onRailLoaded);
  }

  /** Called when the space rail finishes loading its data. Sets up the SSE scope. */
  private handleRailLoaded(e: Event): void {
    const detail = (e as CustomEvent).detail as { spaceIds: string[] };
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
    }
  }

  private parseV2Route(): void {
    const path = this.pageData?.path || window.location.pathname;

    // Match /chat/space/{projectId}/thread/{topicId}
    const threadMatch = path.match(/\/chat\/space\/([^/]+)\/thread\/([^/]+)/);
    if (threadMatch) {
      const projectId = decodeURIComponent(threadMatch[1]);
      const topicId = decodeURIComponent(threadMatch[2]);
      this.v2Conversation = {
        conversationKey: topicId,
        projectId,
        threadName: '', // Will be resolved from rail data
        defaultAgent: '',
        isDM: false,
        peerName: '',
        peerId: '',
        peerKind: 'user',
      };
      this.classList.add('thread-open');
      void this.loadV2Members(projectId);
      dispatchPageTitle(this, 'Thread', 'Chat');
      return;
    }

    // Match /chat/space/{projectId} — open #general
    const spaceMatch = path.match(/\/chat\/space\/([^/]+)$/);
    if (spaceMatch) {
      // Space selected but no specific thread — will be resolved when rail loads
      this.classList.add('thread-open');
      return;
    }

    // Match /chat/dm/{key}
    const dmMatch = path.match(/\/chat\/dm\/(.+)$/);
    if (dmMatch) {
      const key = decodeURIComponent(dmMatch[1]);
      this.v2Conversation = {
        conversationKey: key,
        projectId: '',
        threadName: '',
        defaultAgent: '',
        isDM: true,
        peerName: '',
        peerId: '',
        peerKind: 'user',
      };
      this.classList.add('thread-open');
      dispatchPageTitle(this, 'DM', 'Chat');
      // Resolve peer metadata from the DM list (handles page refresh).
      void this.resolveDMPeerInfo(key);
      return;
    }

    // /chat — no conversation selected
    this.v2Conversation = null;
    this.classList.remove('thread-open');
  }

  private handleChatMessage(): void {
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

  private handleChatTopic(): void {
    // Reload the rail when topics change
    const rail = this.shadowRoot?.querySelector('scion-chat-space-rail') as
      | import('../shared/chat/chat-space-rail.js').ScionChatSpaceRail
      | null;
    if (rail) void rail.reload();
  }

  private handleThreadSelect(e: CustomEvent): void {
    const detail = e.detail as {
      conversationKey: string;
      projectId: string;
      threadName: string;
      defaultAgent?: string;
    };
    navigateTo(
      `/chat/space/${encodeURIComponent(detail.projectId)}/thread/${encodeURIComponent(detail.conversationKey)}`
    );
    this.v2Conversation = {
      conversationKey: detail.conversationKey,
      projectId: detail.projectId,
      threadName: detail.threadName,
      defaultAgent: detail.defaultAgent || '',
      isDM: false,
      peerName: '',
      peerId: '',
      peerKind: 'user',
    };
    this.classList.add('thread-open');
    dispatchPageTitle(this, `#${detail.threadName}`, 'Chat');
    void this.loadV2Members(detail.projectId);
  }

  private handleDMSelect(e: CustomEvent): void {
    const detail = e.detail as {
      conversationKey: string;
      peerName: string;
      peerId: string;
      peerKind: 'user' | 'agent';
    };
    navigateTo(`/chat/dm/${encodeURIComponent(detail.conversationKey)}`);
    this.v2Conversation = {
      conversationKey: detail.conversationKey,
      projectId: '',
      threadName: '',
      defaultAgent: '',
      isDM: true,
      peerName: detail.peerName,
      peerId: detail.peerId,
      peerKind: detail.peerKind,
    };
    this.classList.add('thread-open');
    dispatchPageTitle(this, detail.peerName, 'Chat');
  }

  private handleNavigateApp(): void {
    navigateTo('/');
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
        }));
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

    const userId = this.pageData?.user?.id || '';
    let dmKey: string;

    if (detail.memberKind === 'agent') {
      dmKey = `dm:agent:${detail.memberId}:user:${userId}`;
    } else {
      // Sort UUIDs lexically for consistent DM keys
      const ids = [detail.memberId, userId].sort();
      dmKey = `dm:user:${ids[0]}:user:${ids[1]}`;
    }

    // Navigate to the DM
    navigateTo(`/chat/dm/${encodeURIComponent(dmKey)}`);
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

  // ---- V1 Render ----

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
    const avatarColor = hashColor(thread.agentSlug || thread.agentId);
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
                @dm-select=${this.handleDMSelect}
                @navigate-app=${this.handleNavigateApp}
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
                <span class="subtitle">Choose a thread or DM from the left to start chatting</span>
              </div>
            `}
      </div>

      <div class="v2-members ${this.v2MembersExpanded ? '' : 'collapsed'}">
        <div class="v2-members-header">
          <span>Members</span>
          <sl-icon-button
            name="x-lg"
            label="Close"
            @click=${() => {
              this.v2MembersExpanded = false;
            }}
          ></sl-icon-button>
        </div>
        <scion-chat-members
          .humans=${this.v2HumanMembers}
          .agents=${this.v2AgentMembers}
          current-user-id="${this.pageData?.user?.id || ''}"
          @member-click=${this.handleMemberClick}
        ></scion-chat-members>
      </div>
    `;
  }

  private renderV2Conversation() {
    if (!this.v2Conversation) return nothing;
    const conv = this.v2Conversation;

    return html`
      ${conv.isDM && conv.peerName
        ? html`
            <div class="v2-thread-header">
              <sl-icon
                name=${conv.peerKind === 'agent' ? 'cpu' : 'person'}
                style="font-size: 0.875rem; color: var(--scion-text-muted)"
              ></sl-icon>
              <span>${conv.peerName}</span>
              <sl-icon-button
                class="members-btn"
                name="search"
                label="Search messages"
                @click=${() => void this.openSearch()}
              ></sl-icon-button>
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
                        <sl-icon
                          name="cpu"
                          style="font-size: 0.75rem; color: var(--scion-text-muted)"
                        ></sl-icon>
                      </sl-tooltip>
                    `
                  : nothing}
                <sl-icon-button
                  class="members-btn"
                  name="search"
                  label="Search messages"
                  @click=${() => void this.openSearch()}
                ></sl-icon-button>
                <sl-icon-button
                  class="members-btn"
                  name="people"
                  label="Toggle members"
                  @click=${() => {
                    this.v2MembersExpanded = !this.v2MembersExpanded;
                  }}
                ></sl-icon-button>
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
              defaultAgent=${conv.defaultAgent}
              ?isDM=${conv.isDM}
              peerName=${conv.peerName}
              ?canSend=${true}
              .members=${this.v2Members}
              .agents=${this.getAgentsFromMembers()}
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
        navigateTo(
          `/chat/space/${encodeURIComponent(detail.projectId)}/thread/${encodeURIComponent(detail.conversationKey)}`
        );
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
