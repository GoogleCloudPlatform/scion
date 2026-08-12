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
 * Chat page component for the top-level chat mode (Phase 5).
 *
 * Renders inside `<scion-chat-shell>` and provides:
 * - Thread rail listing agents with last-message preview and unread dot
 * - Selecting a thread opens the existing `<scion-chat-thread>` component
 * - `/chat` shows the rail with no thread selected
 * - `/chat/:agentId` opens the thread for that agent
 *
 * The thread rail reads from `GET /api/v1/chat/threads` which queries
 * the `webchat_thread` table — no aggregate query (AC19a).
 * Unread is a boolean (hasUnread), not a count — a dot is what the rail renders.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Capabilities } from '../../shared/types.js';
import { can } from '../../shared/types.js';
import { apiFetch } from '../../client/api.js';
import { navigateTo, stateManager } from '../../client/main.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import '../shared/chat/chat-thread.js';

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

@customElement('scion-page-chat')
export class ScionPageChat extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @state() private threads: ChatThread[] = [];
  @state() private loadingThreads = false;
  @state() private selectedAgentId = '';
  @state() private selectedAgentName = '';
  @state() private selectedAgentCanSend = false;

  /** Cached agent capabilities keyed by agentId, fetched when a thread is selected. */
  private agentCapabilities = new Map<string, Capabilities | undefined>();

  /** Bound listener for SSE message events — used to refresh the thread rail. */
  private _onUserMessage = this.handleUserMessage.bind(this);

  /** Debounce timer for rail refresh to avoid rapid-fire reloads. */
  private _refreshTimer: ReturnType<typeof setTimeout> | null = null;

  /** Cached project ID to avoid redundant /api/v1/projects?limit=1 fetches. */
  private _cachedProjectId = '';

  static override styles = css`
    :host {
      display: flex;
      height: 100%;
      overflow: hidden;
    }

    /* Thread rail (left sidebar) */
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

    /* Individual thread item */
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

    /* Agent avatar (color + initials) */
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

    /* Main content area (thread view or empty state) */
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

    /* Loading state */
    .loading-rail {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* Responsive: on mobile, hide the rail when a thread is selected */
    @media (max-width: 768px) {
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
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    this.parseRoute();
    void this.loadThreads();

    // Subscribe to SSE message events to refresh the thread rail (O5).
    stateManager.addEventListener('user-message-created', this._onUserMessage);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('user-message-created', this._onUserMessage);
    if (this._refreshTimer) {
      clearTimeout(this._refreshTimer);
      this._refreshTimer = null;
    }
  }

  override updated(changedProperties: Map<string, unknown>): void {
    if (changedProperties.has('pageData') && this.pageData) {
      this.parseRoute();
    }
  }

  /**
   * Handle SSE user-message events — debounce and reload the thread rail.
   * Uses a 2-second debounce to avoid rapid-fire reloads during bursts.
   */
  private handleUserMessage(): void {
    if (this._refreshTimer) {
      clearTimeout(this._refreshTimer);
    }
    this._refreshTimer = setTimeout(() => {
      this._refreshTimer = null;
      void this.loadThreads();
    }, 2000);
  }

  /** Parse the current path to determine selected agent. */
  private parseRoute(): void {
    const path = this.pageData?.path || window.location.pathname;
    const match = path.match(/\/chat\/([^/]+)/);
    const newAgentId = match ? decodeURIComponent(match[1]) : '';

    if (newAgentId !== this.selectedAgentId) {
      this.selectedAgentId = newAgentId;
      // Update class for responsive layout
      if (newAgentId) {
        this.classList.add('thread-open');
        // Fetch capabilities to gate the compose box (O3).
        void this.fetchAgentCapabilities(newAgentId);
      } else {
        this.classList.remove('thread-open');
        this.selectedAgentCanSend = false;
      }
    }
  }

  /** Load thread list from the API. */
  private async loadThreads(): Promise<void> {
    this.loadingThreads = true;

    try {
      // Use the first project the user has access to.
      // The projectId must be determined from context. For now,
      // look at the SSR page data or fetch the projects list.
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

        // If a thread is selected, try to find its name
        if (this.selectedAgentId) {
          this.resolveSelectedAgentName();
        }
      }
    } catch {
      // Silently fail — rail will show empty
    } finally {
      this.loadingThreads = false;
    }
  }

  /** Resolve the project ID from page context. */
  private async resolveProjectId(): Promise<string> {
    if (this._cachedProjectId) return this._cachedProjectId;

    // Try to get from URL query param
    const url = new URL(window.location.href);
    const qProject = url.searchParams.get('projectId');
    if (qProject) {
      this._cachedProjectId = qProject;
      return qProject;
    }

    // Fall back to fetching the user's projects and using the first one
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

  /** Update selectedAgentName from the thread list or API. */
  private resolveSelectedAgentName(): void {
    const thread = this.threads.find(
      (t) => t.agentId === this.selectedAgentId || t.agentSlug === this.selectedAgentId
    );
    if (thread) {
      this.selectedAgentName = thread.agentName || thread.agentSlug || thread.agentId;
      dispatchPageTitle(this, this.selectedAgentName, 'Chat');
    }
  }

  /** Fetch and cache agent capabilities to determine canSend. */
  private async fetchAgentCapabilities(agentId: string): Promise<void> {
    // Return cached value if available.
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
      // On failure, fail-closed: disable send.
      this.selectedAgentCanSend = false;
    }
  }

  /** Mark a thread as read when opened. */
  private async markThreadRead(agentId: string): Promise<void> {
    const projectId = await this.resolveProjectId();
    if (!projectId) return;

    try {
      await apiFetch(
        `/api/v1/chat/threads/${encodeURIComponent(agentId)}/read?projectId=${encodeURIComponent(projectId)}`,
        { method: 'POST' }
      );

      // Clear the unread indicator locally
      this.threads = this.threads.map((t) =>
        t.agentId === agentId ? { ...t, hasUnread: false } : t
      );
    } catch {
      // Non-critical — unread dot will persist until next load
    }
  }

  /** Handle thread selection from the rail. */
  private selectThread(thread: ChatThread): void {
    const agentRef = thread.agentSlug || thread.agentId;
    navigateTo(`/chat/${encodeURIComponent(agentRef)}`);
    this.selectedAgentId = thread.agentId;
    this.selectedAgentName = thread.agentName || thread.agentSlug || thread.agentId;
    this.classList.add('thread-open');
    dispatchPageTitle(this, this.selectedAgentName, 'Chat');

    // Fetch capabilities to gate the compose box (O3).
    void this.fetchAgentCapabilities(thread.agentId);

    // Mark as read (AC19b — server-side watermark)
    void this.markThreadRead(thread.agentId);
  }

  override render() {
    return html`
      <div class="thread-rail">
        <div class="rail-header">
          <span>Conversations</span>
          <a href="/" @click=${(e: Event) => { e.preventDefault(); navigateTo('/'); }}>
            <sl-icon name="arrow-left"></sl-icon>
            App
          </a>
        </div>
        <div class="thread-list">
          ${this.loadingThreads
            ? html`<div class="loading-rail"><sl-spinner></sl-spinner></div>`
            : this.threads.length === 0
              ? html`<div class="loading-rail" style="font-size: 0.8125rem">No conversations yet</div>`
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
      thread.agentId === this.selectedAgentId ||
      thread.agentSlug === this.selectedAgentId;
    const displayName = thread.agentName || thread.agentSlug || thread.agentId;
    const avatarColor = this.hashColor(thread.agentSlug || thread.agentId);
    const initials = this.getInitials(displayName);
    const timeStr = thread.lastMessage?.createdAt
      ? this.formatRelativeTime(thread.lastMessage.createdAt)
      : '';

    return html`
      <div
        class="thread-item ${isSelected ? 'selected' : ''}"
        @click=${() => this.selectThread(thread)}
      >
        <div class="agent-avatar" style="background: ${avatarColor}">
          ${initials}
        </div>
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

  /** Generate a deterministic color from a string (agent slug). */
  private hashColor(str: string): string {
    let hash = 0;
    for (let i = 0; i < str.length; i++) {
      hash = str.charCodeAt(i) + ((hash << 5) - hash);
    }
    const hue = ((hash % 360) + 360) % 360;
    return `hsl(${hue}, 55%, 48%)`;
  }

  /** Get 1-2 character initials from a name. */
  private getInitials(name: string): string {
    const parts = name.split(/[-_\s]+/).filter(Boolean);
    if (parts.length >= 2) {
      return (parts[0][0] + parts[1][0]).toUpperCase();
    }
    return (name.slice(0, 2) || '?').toUpperCase();
  }

  /** Format a timestamp as a relative time string. */
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
