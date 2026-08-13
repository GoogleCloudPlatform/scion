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
 * Space rail component — the left sidebar in Wave 2 chat.
 *
 * Replaces the wave-1 agent-based thread rail with a space-oriented hierarchy:
 *   - Spaces section: one per project the user can access
 *   - Each space is collapsible (chevron toggle)
 *   - Under each space: thread list (#general first, pinned, then sorted)
 *   - DM section below spaces
 *
 * Data sources:
 *   - GET /api/v1/chat/spaces — visible spaces with unread rollup
 *   - GET /api/v1/chat/spaces/{projectId}/threads — threads per space
 *   - GET /api/v1/chat/dms — DM conversations
 *   - GET /api/v1/chat/prefs — user preferences (sort mode, custom order)
 *
 * Interactions: thread select, context menu, create thread, sorting, DnD.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch } from '../../../client/api.js';

/** A space (project) in the rail. */
export interface ChatSpace {
  projectId: string;
  projectName: string;
  unreadCount: number;
  hasUnreadMention: boolean;
}

/** A thread within a space. */
export interface ChatSpaceThread {
  id: string;
  name: string;
  isGeneral: boolean;
  pinned: boolean;
  defaultAgent?: string;
  lastActivityAt?: string;
  lastMessagePreview?: string;
  hasUnread: boolean;
  hasUnreadMention: boolean;
}

/** A DM conversation. */
export interface ChatDM {
  conversationKey: string;
  peerName: string;
  peerId: string;
  peerKind: 'user' | 'agent';
  peerAvatarUrl?: string;
  lastMessagePreview?: string;
  lastActivityAt?: string;
  hasUnread: boolean;
}

/** User preferences for rail display. */
interface RailPrefs {
  spaceSortMode: 'activity' | 'alpha' | 'custom';
  threadSortMode: 'activity' | 'alpha';
  spaceOrder: string[] | undefined;
}

/** Event detail for thread selection. */
export interface ThreadSelectDetail {
  conversationKey: string;
  projectId: string;
  threadName: string;
  defaultAgent: string;
}

/** Event detail for DM selection. */
export interface DMSelectDetail {
  conversationKey: string;
  peerName: string;
  peerId: string;
  peerKind: 'user' | 'agent';
}

@customElement('scion-chat-space-rail')
export class ScionChatSpaceRail extends LitElement {
  /** Currently selected conversation key. */
  @property()
  selectedKey = '';

  @state() private spaces: ChatSpace[] = [];
  @state() private threadsBySpace = new Map<string, ChatSpaceThread[]>();
  @state() private dms: ChatDM[] = [];
  @state() private collapsedSpaces = new Set<string>();
  @state() private loading = true;
  @state() private prefs: RailPrefs = {
    spaceSortMode: 'activity',
    threadSortMode: 'activity',
    spaceOrder: undefined,
  };
  @state() private creatingThread = '';
  @state() private newThreadName = '';
  @state() private contextMenuTarget: {
    type: 'thread';
    thread: ChatSpaceThread;
    projectId: string;
  } | null = null;
  @state() private contextMenuPos = { x: 0, y: 0 };
  @state() private renamingThread: string | null = null;
  @state() private renameValue = '';
  @state() private dmSectionCollapsed = false;

  static override styles = css`
    :host {
      display: flex;
      flex-direction: column;
      height: 100%;
      overflow: hidden;
      background: var(--scion-surface, #ffffff);
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

    .rail-body {
      flex: 1;
      overflow-y: auto;
      padding: 0.25rem 0;
    }

    /* Space section */
    .space-section {
      margin-bottom: 0.25rem;
    }

    .space-header {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.375rem 0.75rem;
      cursor: pointer;
      font-size: 0.6875rem;
      font-weight: 700;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      user-select: none;
    }

    .space-header:hover {
      color: var(--scion-text, #1e293b);
    }

    .space-header .chevron {
      transition: transform 0.15s;
      font-size: 0.75rem;
    }

    .space-header .chevron.collapsed {
      transform: rotate(-90deg);
    }

    .space-header .space-name {
      flex: 1;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .space-header .unread-badge {
      background: var(--scion-primary, #3b82f6);
      color: #fff;
      font-size: 0.5625rem;
      font-weight: 700;
      padding: 0.0625rem 0.3125rem;
      border-radius: 0.5rem;
      min-width: 1rem;
      text-align: center;
    }

    .space-header .mention-badge {
      background: var(--scion-danger-500, #ef4444);
      color: #fff;
      font-size: 0.5625rem;
      font-weight: 700;
      padding: 0.0625rem 0.3125rem;
      border-radius: 0.5rem;
      min-width: 1rem;
      text-align: center;
    }

    .space-actions {
      display: flex;
      align-items: center;
      gap: 0.25rem;
    }

    .space-actions sl-icon-button::part(base) {
      font-size: 0.75rem;
      padding: 0.125rem;
    }

    /* Thread items */
    .thread-list {
      padding-left: 0;
    }

    .thread-item {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.3125rem 0.75rem 0.3125rem 1.75rem;
      cursor: pointer;
      font-size: 0.8125rem;
      color: var(--scion-text, #1e293b);
      transition: background 0.1s;
      position: relative;
    }

    .thread-item:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .thread-item.selected {
      background: var(--scion-primary-50, #eff6ff);
      font-weight: 600;
    }

    .thread-item .hash {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.75rem;
      flex-shrink: 0;
    }

    .thread-item .thread-name {
      flex: 1;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .thread-item .thread-name.unread {
      font-weight: 700;
    }

    .thread-item .unread-dot {
      width: 6px;
      height: 6px;
      border-radius: 50%;
      background: var(--scion-primary, #3b82f6);
      flex-shrink: 0;
    }

    .thread-item .mention-dot {
      width: 6px;
      height: 6px;
      border-radius: 50%;
      background: var(--scion-danger-500, #ef4444);
      flex-shrink: 0;
    }

    .thread-item .pin-icon {
      font-size: 0.625rem;
      color: var(--scion-text-muted, #64748b);
      flex-shrink: 0;
    }

    /* Create thread inline input */
    .create-thread {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.25rem 0.75rem 0.25rem 1.75rem;
    }

    .create-thread sl-input::part(base) {
      font-size: 0.8125rem;
      min-height: 1.75rem;
    }

    /* DM section */
    .dm-section {
      border-top: 1px solid var(--scion-border, #e2e8f0);
      margin-top: 0.25rem;
      padding-top: 0.25rem;
    }

    .dm-item {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.375rem 0.75rem 0.375rem 1rem;
      cursor: pointer;
      font-size: 0.8125rem;
      color: var(--scion-text, #1e293b);
      transition: background 0.1s;
    }

    .dm-item:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .dm-item.selected {
      background: var(--scion-primary-50, #eff6ff);
      font-weight: 600;
    }

    .dm-avatar {
      width: 24px;
      height: 24px;
      border-radius: 50%;
      display: flex;
      align-items: center;
      justify-content: center;
      font-size: 0.625rem;
      font-weight: 600;
      color: #fff;
      flex-shrink: 0;
    }

    .dm-info {
      flex: 1;
      min-width: 0;
    }

    .dm-name {
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .dm-name.unread {
      font-weight: 700;
    }

    /* Context menu */
    .context-menu {
      position: fixed;
      z-index: 1000;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.5rem;
      box-shadow: 0 4px 12px rgba(0, 0, 0, 0.12);
      min-width: 160px;
      padding: 0.25rem 0;
    }

    .context-menu-item {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.375rem 0.75rem;
      font-size: 0.8125rem;
      cursor: pointer;
      color: var(--scion-text, #1e293b);
    }

    .context-menu-item:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .context-menu-item.danger {
      color: var(--scion-danger-600, #dc2626);
    }

    .context-menu-item sl-icon {
      font-size: 0.875rem;
    }

    /* Loading / empty */
    .loading-state {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* Sort dropdown */
    .sort-selector {
      padding: 0.375rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    /* Rename input */
    .rename-input {
      width: 100%;
    }

    .rename-input::part(base) {
      font-size: 0.8125rem;
      min-height: 1.5rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadData();
    // Close context menu on outside click
    this._outsideClickHandler = this.handleOutsideClick.bind(this);
    document.addEventListener('click', this._outsideClickHandler);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this._outsideClickHandler) {
      document.removeEventListener('click', this._outsideClickHandler);
    }
  }

  private _outsideClickHandler: ((e: Event) => void) | null = null;

  private handleOutsideClick(): void {
    if (this.contextMenuTarget) {
      this.contextMenuTarget = null;
    }
  }

  /** Reload all data (called externally when SSE events indicate changes). */
  async reload(): Promise<void> {
    await this.loadData();
  }

  /** Returns the list of space project IDs for SSE subscription. */
  getSpaceIds(): string[] {
    return this.spaces.map((s) => s.projectId);
  }

  private async loadData(): Promise<void> {
    this.loading = true;
    try {
      await Promise.all([this.loadSpaces(), this.loadDMs(), this.loadPrefs()]);
    } finally {
      this.loading = false;
      // Notify parent that rail data is ready (for SSE scope setup)
      this.dispatchEvent(
        new CustomEvent('rail-loaded', {
          detail: { spaceIds: this.getSpaceIds() },
          bubbles: true,
          composed: true,
        })
      );
    }
  }

  private async loadSpaces(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/chat/spaces');
      if (res.ok) {
        const data = (await res.json()) as { spaces?: ChatSpace[] };
        this.spaces = data.spaces || [];
        // Load threads for each space
        await Promise.all(this.spaces.map((s) => this.loadThreads(s.projectId)));
      }
    } catch {
      // Silently fail
    }
  }

  private async loadThreads(projectId: string): Promise<void> {
    try {
      const res = await apiFetch(`/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`);
      if (res.ok) {
        const data = (await res.json()) as { threads?: ChatSpaceThread[] };
        const newMap = new Map(this.threadsBySpace);
        newMap.set(projectId, data.threads || []);
        this.threadsBySpace = newMap;
      }
    } catch {
      // Silently fail
    }
  }

  private async loadDMs(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/chat/dms');
      if (res.ok) {
        const data = (await res.json()) as { dms?: ChatDM[] };
        this.dms = data.dms || [];
      }
    } catch {
      // Silently fail
    }
  }

  private async loadPrefs(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/chat/prefs');
      if (res.ok) {
        const data = (await res.json()) as {
          space_sort_mode?: string;
          thread_sort_mode?: string;
          space_order?: string[];
        };
        this.prefs = {
          spaceSortMode: (data.space_sort_mode as RailPrefs['spaceSortMode']) || 'activity',
          threadSortMode: (data.thread_sort_mode as RailPrefs['threadSortMode']) || 'activity',
          spaceOrder: data.space_order,
        };
      }
    } catch {
      // Use defaults
    }
  }

  /** Save user preferences. Exposed for sort mode changes. */
  async savePrefs(update: Partial<RailPrefs>): Promise<void> {
    const newPrefs = { ...this.prefs, ...update };
    this.prefs = newPrefs;
    try {
      await apiFetch('/api/v1/chat/prefs', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          space_sort_mode: newPrefs.spaceSortMode,
          thread_sort_mode: newPrefs.threadSortMode,
          space_order: newPrefs.spaceOrder,
        }),
      });
    } catch {
      // Non-critical
    }
  }

  // ---------------------------------------------------------------------------
  // Sorting
  // ---------------------------------------------------------------------------

  private getSortedSpaces(): ChatSpace[] {
    const spaces = [...this.spaces];
    switch (this.prefs.spaceSortMode) {
      case 'alpha':
        spaces.sort((a, b) => a.projectName.localeCompare(b.projectName));
        break;
      case 'custom':
        if (this.prefs.spaceOrder) {
          const order = this.prefs.spaceOrder;
          spaces.sort((a, b) => {
            const ai = order.indexOf(a.projectId);
            const bi = order.indexOf(b.projectId);
            if (ai === -1 && bi === -1) return 0;
            if (ai === -1) return 1;
            if (bi === -1) return -1;
            return ai - bi;
          });
        }
        break;
      case 'activity':
      default:
        // activity sort: spaces with more recent activity first
        // We use the threads' lastActivityAt to derive this
        spaces.sort((a, b) => {
          const aTime = this.getSpaceLastActivity(a.projectId);
          const bTime = this.getSpaceLastActivity(b.projectId);
          return bTime - aTime;
        });
        break;
    }
    return spaces;
  }

  private getSpaceLastActivity(projectId: string): number {
    const threads = this.threadsBySpace.get(projectId) || [];
    let maxTime = 0;
    for (const t of threads) {
      if (t.lastActivityAt) {
        const time = new Date(t.lastActivityAt).getTime();
        if (time > maxTime) maxTime = time;
      }
    }
    return maxTime;
  }

  private getSortedThreads(projectId: string): ChatSpaceThread[] {
    const threads = [...(this.threadsBySpace.get(projectId) || [])];

    // Separate #general, pinned, and regular
    const general = threads.filter((t) => t.isGeneral);
    const pinned = threads.filter((t) => !t.isGeneral && t.pinned);
    const regular = threads.filter((t) => !t.isGeneral && !t.pinned);

    // Sort pinned and regular
    const sortFn =
      this.prefs.threadSortMode === 'alpha'
        ? (a: ChatSpaceThread, b: ChatSpaceThread) => a.name.localeCompare(b.name)
        : (a: ChatSpaceThread, b: ChatSpaceThread) => {
            const aTime = a.lastActivityAt ? new Date(a.lastActivityAt).getTime() : 0;
            const bTime = b.lastActivityAt ? new Date(b.lastActivityAt).getTime() : 0;
            return bTime - aTime;
          };

    pinned.sort(sortFn);
    regular.sort(sortFn);

    return [...general, ...pinned, ...regular];
  }

  // ---------------------------------------------------------------------------
  // Actions
  // ---------------------------------------------------------------------------

  private handleThreadClick(thread: ChatSpaceThread, projectId: string): void {
    this.dispatchEvent(
      new CustomEvent<ThreadSelectDetail>('thread-select', {
        detail: {
          conversationKey: thread.id,
          projectId,
          threadName: thread.name,
          defaultAgent: thread.defaultAgent || '',
        },
        bubbles: true,
        composed: true,
      })
    );
  }

  private handleSpaceHeaderClick(space: ChatSpace): void {
    if (this.collapsedSpaces.has(space.projectId)) {
      // Expanding — do nothing special
      const newSet = new Set(this.collapsedSpaces);
      newSet.delete(space.projectId);
      this.collapsedSpaces = newSet;
    } else {
      // Collapsing
      const newSet = new Set(this.collapsedSpaces);
      newSet.add(space.projectId);
      this.collapsedSpaces = newSet;
    }
  }

  private handleCollapsedSpaceClick(space: ChatSpace): void {
    // If collapsed, clicking opens #general
    const threads = this.threadsBySpace.get(space.projectId) || [];
    const general = threads.find((t) => t.isGeneral);
    if (general) {
      // Expand and select #general
      const newSet = new Set(this.collapsedSpaces);
      newSet.delete(space.projectId);
      this.collapsedSpaces = newSet;
      this.handleThreadClick(general, space.projectId);
    }
  }

  private handleDMClick(dm: ChatDM): void {
    this.dispatchEvent(
      new CustomEvent<DMSelectDetail>('dm-select', {
        detail: {
          conversationKey: dm.conversationKey,
          peerName: dm.peerName,
          peerId: dm.peerId,
          peerKind: dm.peerKind,
        },
        bubbles: true,
        composed: true,
      })
    );
  }

  private handleContextMenu(e: MouseEvent, thread: ChatSpaceThread, projectId: string): void {
    e.preventDefault();
    e.stopPropagation();
    this.contextMenuTarget = { type: 'thread', thread, projectId };
    this.contextMenuPos = { x: e.clientX, y: e.clientY };
  }

  private async handleMarkRead(thread: ChatSpaceThread, projectId: string): Promise<void> {
    this.contextMenuTarget = null;
    try {
      await apiFetch(`/api/v1/chat/conversations/${encodeURIComponent(thread.id)}/read`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({}),
      });
      // Update locally
      this.updateThread(projectId, thread.id, { hasUnread: false, hasUnreadMention: false });
    } catch {
      // Non-critical
    }
  }

  private async handleMarkSpaceRead(projectId: string): Promise<void> {
    this.contextMenuTarget = null;
    try {
      await apiFetch(`/api/v1/chat/spaces/${encodeURIComponent(projectId)}/read`, {
        method: 'POST',
      });
      // Update all threads in this space locally
      const threads = this.threadsBySpace.get(projectId) || [];
      const newMap = new Map(this.threadsBySpace);
      newMap.set(
        projectId,
        threads.map((t) => ({ ...t, hasUnread: false, hasUnreadMention: false }))
      );
      this.threadsBySpace = newMap;
    } catch {
      // Non-critical
    }
  }

  private startRename(thread: ChatSpaceThread): void {
    this.contextMenuTarget = null;
    if (thread.isGeneral) return;
    this.renamingThread = thread.id;
    this.renameValue = thread.name;
  }

  private async submitRename(projectId: string): Promise<void> {
    if (!this.renamingThread || !this.renameValue.trim()) {
      this.renamingThread = null;
      return;
    }
    try {
      await apiFetch(`/api/v1/chat/threads/${encodeURIComponent(this.renamingThread)}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: this.renameValue.trim() }),
      });
      this.updateThread(projectId, this.renamingThread, { name: this.renameValue.trim() });
    } catch {
      // Non-critical
    }
    this.renamingThread = null;
  }

  private async handleDeleteThread(thread: ChatSpaceThread, projectId: string): Promise<void> {
    this.contextMenuTarget = null;
    if (thread.isGeneral) return;
    if (!confirm(`Delete #${thread.name}? This cannot be undone.`)) return;
    try {
      await apiFetch(`/api/v1/chat/threads/${encodeURIComponent(thread.id)}`, {
        method: 'DELETE',
      });
      // Remove locally
      const threads = this.threadsBySpace.get(projectId) || [];
      const newMap = new Map(this.threadsBySpace);
      newMap.set(
        projectId,
        threads.filter((t) => t.id !== thread.id)
      );
      this.threadsBySpace = newMap;
    } catch {
      // Non-critical
    }
  }

  private startCreateThread(projectId: string): void {
    this.creatingThread = projectId;
    this.newThreadName = '';
  }

  private async submitCreateThread(projectId: string): Promise<void> {
    if (!this.newThreadName.trim()) {
      this.creatingThread = '';
      return;
    }
    try {
      const res = await apiFetch(`/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: this.newThreadName.trim() }),
      });
      if (res.ok) {
        await this.loadThreads(projectId);
      }
    } catch {
      // Non-critical
    }
    this.creatingThread = '';
  }

  private updateThread(
    projectId: string,
    threadId: string,
    update: Partial<ChatSpaceThread>
  ): void {
    const threads = this.threadsBySpace.get(projectId) || [];
    const newMap = new Map(this.threadsBySpace);
    newMap.set(
      projectId,
      threads.map((t) => (t.id === threadId ? { ...t, ...update } : t))
    );
    this.threadsBySpace = newMap;
  }

  // ---------------------------------------------------------------------------
  // Utility
  // ---------------------------------------------------------------------------

  private hashColor(str: string): string {
    let hash = 0;
    for (let i = 0; i < str.length; i++) {
      hash = str.charCodeAt(i) + ((hash << 5) - hash);
    }
    const hue = ((hash % 360) + 360) % 360;
    return `hsl(${hue}, 55%, 48%)`;
  }

  private getInitials(name: string): string {
    const parts = name.split(/[-_\s]+/).filter(Boolean);
    if (parts.length >= 2) {
      return (parts[0][0] + parts[1][0]).toUpperCase();
    }
    return (name.slice(0, 2) || '?').toUpperCase();
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    return html`
      <div class="rail-header">
        <span>Chat</span>
        <a
          href="/"
          @click=${(e: Event) => {
            e.preventDefault();
            this.dispatchEvent(new CustomEvent('navigate-app', { bubbles: true, composed: true }));
          }}
        >
          <sl-icon name="arrow-left"></sl-icon>
          App
        </a>
      </div>

      ${this.loading
        ? html`<div class="loading-state"><sl-spinner></sl-spinner></div>`
        : html` <div class="rail-body">${this.renderSpaces()} ${this.renderDMs()}</div> `}
      ${this.contextMenuTarget ? this.renderContextMenu() : nothing}
    `;
  }

  private renderSpaces() {
    const sorted = this.getSortedSpaces();
    if (sorted.length === 0) {
      return html`<div class="loading-state" style="font-size: 0.8125rem">
        No spaces available
      </div>`;
    }
    return sorted.map((space) => this.renderSpace(space));
  }

  private renderSpace(space: ChatSpace) {
    const isCollapsed = this.collapsedSpaces.has(space.projectId);
    const threads = this.getSortedThreads(space.projectId);

    return html`
      <div class="space-section">
        <div
          class="space-header"
          @click=${() =>
            isCollapsed
              ? this.handleCollapsedSpaceClick(space)
              : this.handleSpaceHeaderClick(space)}
        >
          <sl-icon name="chevron-down" class="chevron ${isCollapsed ? 'collapsed' : ''}"></sl-icon>
          <span class="space-name">${space.projectName}</span>
          <div class="space-actions">
            ${space.hasUnreadMention
              ? html`<span class="mention-badge">@</span>`
              : space.unreadCount > 0
                ? html`<span class="unread-badge">${space.unreadCount}</span>`
                : nothing}
            <sl-icon-button
              name="plus-lg"
              label="Create thread"
              @click=${(e: Event) => {
                e.stopPropagation();
                this.startCreateThread(space.projectId);
              }}
            ></sl-icon-button>
          </div>
        </div>
        ${!isCollapsed
          ? html`
              <div class="thread-list">
                ${threads.map((t) => this.renderThread(t, space.projectId))}
                ${this.creatingThread === space.projectId
                  ? this.renderCreateThread(space.projectId)
                  : nothing}
              </div>
            `
          : nothing}
      </div>
    `;
  }

  private renderThread(thread: ChatSpaceThread, projectId: string) {
    const isSelected = thread.id === this.selectedKey;

    if (this.renamingThread === thread.id) {
      return html`
        <div class="thread-item">
          <span class="hash">#</span>
          <sl-input
            class="rename-input"
            size="small"
            .value=${this.renameValue}
            @sl-input=${(e: Event) => {
              this.renameValue = (e.target as HTMLInputElement).value;
            }}
            @keydown=${(e: KeyboardEvent) => {
              if (e.key === 'Enter') {
                e.preventDefault();
                void this.submitRename(projectId);
              }
              if (e.key === 'Escape') {
                this.renamingThread = null;
              }
            }}
            @sl-blur=${() => void this.submitRename(projectId)}
          ></sl-input>
        </div>
      `;
    }

    return html`
      <div
        class="thread-item ${isSelected ? 'selected' : ''}"
        @click=${() => this.handleThreadClick(thread, projectId)}
        @contextmenu=${(e: MouseEvent) => this.handleContextMenu(e, thread, projectId)}
      >
        <span class="hash">#</span>
        <span class="thread-name ${thread.hasUnread ? 'unread' : ''}">${thread.name}</span>
        ${thread.pinned ? html`<sl-icon name="star-fill" class="pin-icon"></sl-icon>` : nothing}
        ${thread.hasUnreadMention
          ? html`<span class="mention-dot"></span>`
          : thread.hasUnread
            ? html`<span class="unread-dot"></span>`
            : nothing}
      </div>
    `;
  }

  private renderCreateThread(projectId: string) {
    return html`
      <div class="create-thread">
        <span class="hash" style="color: var(--scion-text-muted)">#</span>
        <sl-input
          size="small"
          placeholder="thread-name"
          .value=${this.newThreadName}
          @sl-input=${(e: Event) => {
            this.newThreadName = (e.target as HTMLInputElement).value;
          }}
          @keydown=${(e: KeyboardEvent) => {
            if (e.key === 'Enter') {
              e.preventDefault();
              void this.submitCreateThread(projectId);
            }
            if (e.key === 'Escape') {
              this.creatingThread = '';
            }
          }}
          @sl-blur=${() => {
            if (!this.newThreadName.trim()) this.creatingThread = '';
          }}
          style="flex: 1"
        ></sl-input>
      </div>
    `;
  }

  private renderDMs() {
    if (this.dms.length === 0) return nothing;

    return html`
      <div class="dm-section">
        <div
          class="space-header"
          @click=${() => {
            this.dmSectionCollapsed = !this.dmSectionCollapsed;
          }}
        >
          <sl-icon
            name="chevron-down"
            class="chevron ${this.dmSectionCollapsed ? 'collapsed' : ''}"
          ></sl-icon>
          <span class="space-name">Direct Messages</span>
        </div>
        ${!this.dmSectionCollapsed ? this.dms.map((dm) => this.renderDM(dm)) : nothing}
      </div>
    `;
  }

  private renderDM(dm: ChatDM) {
    const isSelected = dm.conversationKey === this.selectedKey;
    const avatarColor = this.hashColor(dm.peerId);
    const initials = this.getInitials(dm.peerName);
    const icon = dm.peerKind === 'agent' ? 'cpu' : 'person';

    return html`
      <div class="dm-item ${isSelected ? 'selected' : ''}" @click=${() => this.handleDMClick(dm)}>
        <div class="dm-avatar" style="background: ${avatarColor}">${initials}</div>
        <div class="dm-info">
          <span class="dm-name ${dm.hasUnread ? 'unread' : ''}">
            <sl-icon name="${icon}" style="font-size: 0.6875rem; vertical-align: -1px"></sl-icon>
            ${dm.peerName}
          </span>
        </div>
        ${dm.hasUnread
          ? html`<span
              class="unread-dot"
              style="width: 6px; height: 6px; border-radius: 50%; background: var(--scion-primary, #3b82f6); flex-shrink: 0;"
            ></span>`
          : nothing}
      </div>
    `;
  }

  private renderContextMenu() {
    if (!this.contextMenuTarget) return nothing;
    const { thread, projectId } = this.contextMenuTarget;

    return html`
      <div
        class="context-menu"
        style="left: ${this.contextMenuPos.x}px; top: ${this.contextMenuPos.y}px"
        @click=${(e: Event) => e.stopPropagation()}
      >
        <div class="context-menu-item" @click=${() => this.handleMarkRead(thread, projectId)}>
          <sl-icon name="check-circle"></sl-icon>
          Mark as read
        </div>
        <div class="context-menu-item" @click=${() => this.handleMarkSpaceRead(projectId)}>
          <sl-icon name="check-lg"></sl-icon>
          Mark space read
        </div>
        ${!thread.isGeneral
          ? html`
              <div class="context-menu-item" @click=${() => this.startRename(thread)}>
                <sl-icon name="pencil"></sl-icon>
                Rename
              </div>
              <div
                class="context-menu-item danger"
                @click=${() => this.handleDeleteThread(thread, projectId)}
              >
                <sl-icon name="trash"></sl-icon>
                Delete
              </div>
            `
          : nothing}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-space-rail': ScionChatSpaceRail;
  }
}
