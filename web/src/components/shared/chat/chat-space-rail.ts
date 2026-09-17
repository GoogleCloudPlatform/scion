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
 *
 * Data sources:
 *   - GET /api/v1/chat/spaces — visible spaces with unread rollup
 *   - GET /api/v1/chat/spaces/{projectId}/threads — threads per space
 *   - GET /api/v1/chat/user-prefs — user preferences (sort mode, custom order)
 *
 * DMs are accessed via member-click in the members sidebar (chat-members).
 *
 * Interactions: thread select, context menu, create thread, sorting, DnD.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch } from '../../../client/api.js';
import { showConfirm } from '../confirm-dialog.js';
import { showToast } from '../../../utils/toast.js';
import './chat-avatar.js';

/** A space (project) in the rail. */
export interface ChatSpace {
  projectId: string;
  projectName: string;
  projectSlug: string;
  emoji?: string;
  unreadCount: number;
  hasUnreadMention: boolean;
}

/** A thread within a space. */
export interface ChatSpaceThread {
  id: string;
  name: string;
  isGeneral: boolean;
  pinned: boolean;
  /** Muted threads raise no notifications and show no unread marker. */
  muted?: boolean;
  defaultAgent?: string;
  lastActivityAt?: string;
  lastMessagePreview?: string;
  /** Newest message in the thread — the watermark a "mark read" must set. */
  lastMessageId?: string;
  hasUnread: boolean;
  hasUnreadMention: boolean;
}

/** A named group of threads within a space. */
interface ThreadGroup {
  id: string;
  name: string;
  threadIds: string[];
}

/** User preferences for rail display. */
interface RailPrefs {
  spaceSortMode: 'activity' | 'alpha' | 'custom';
  threadSortMode: 'activity' | 'alpha' | 'custom';
  spaceOrder: string[] | undefined;
  threadOrder: Record<string, string[]> | undefined;
  threadGroups: Record<string, ThreadGroup[]> | undefined;
}

/**
 * Read a prefs payload from the server. `spaceOrder` arrives either as a real
 * array or as a JSON array string, so both are accepted; a hand-edited or
 * truncated value has to degrade to "no custom order" rather than throwing the
 * rail's whole load away. Non-string entries are dropped either way.
 */
/** Safely parse a JSON string, returning undefined on failure. */
function safeJsonParse(value: unknown): unknown {
  if (typeof value !== 'string' || value === '') return undefined;
  try {
    return JSON.parse(value);
  } catch {
    return undefined;
  }
}

function parseRailPrefs(payload: unknown): RailPrefs {
  const data = (payload ?? {}) as {
    spaceSortMode?: string;
    threadSortMode?: string;
    spaceOrder?: unknown;
    threadOrder?: unknown;
    threadGroups?: unknown;
  };
  let spaceOrder: string[] | undefined;
  if (Array.isArray(data.spaceOrder)) {
    spaceOrder = data.spaceOrder.filter((id): id is string => typeof id === 'string');
  } else if (typeof data.spaceOrder === 'string' && data.spaceOrder !== '') {
    try {
      const parsed: unknown = JSON.parse(data.spaceOrder);
      if (Array.isArray(parsed)) {
        spaceOrder = parsed.filter((id): id is string => typeof id === 'string');
      }
    } catch {
      spaceOrder = undefined;
    }
  }

  // Parse threadOrder: either already an object or a JSON string.
  let threadOrder: Record<string, string[]> | undefined;
  const rawThreadOrder =
    typeof data.threadOrder === 'string' ? safeJsonParse(data.threadOrder) : data.threadOrder;
  if (rawThreadOrder && typeof rawThreadOrder === 'object' && !Array.isArray(rawThreadOrder)) {
    threadOrder = {} as Record<string, string[]>;
    for (const [key, val] of Object.entries(rawThreadOrder as Record<string, unknown>)) {
      if (Array.isArray(val)) {
        threadOrder[key] = val.filter((id): id is string => typeof id === 'string');
      }
    }
  }

  // Parse threadGroups: either already an object or a JSON string.
  let threadGroups: Record<string, ThreadGroup[]> | undefined;
  const rawThreadGroups =
    typeof data.threadGroups === 'string' ? safeJsonParse(data.threadGroups) : data.threadGroups;
  if (rawThreadGroups && typeof rawThreadGroups === 'object' && !Array.isArray(rawThreadGroups)) {
    threadGroups = {} as Record<string, ThreadGroup[]>;
    for (const [key, val] of Object.entries(rawThreadGroups as Record<string, unknown>)) {
      if (Array.isArray(val)) {
        const groups: ThreadGroup[] = [];
        for (const g of val) {
          if (
            g &&
            typeof g === 'object' &&
            typeof (g as ThreadGroup).id === 'string' &&
            typeof (g as ThreadGroup).name === 'string' &&
            Array.isArray((g as ThreadGroup).threadIds)
          ) {
            groups.push({
              id: (g as ThreadGroup).id,
              name: (g as ThreadGroup).name,
              threadIds: (g as ThreadGroup).threadIds.filter(
                (id): id is string => typeof id === 'string'
              ),
            });
          }
        }
        threadGroups[key] = groups;
      }
    }
  }

  const spaceSortMode = data.spaceSortMode;
  const threadSortMode = data.threadSortMode;
  return {
    spaceSortMode:
      spaceSortMode === 'alpha' || spaceSortMode === 'custom' ? spaceSortMode : 'activity',
    threadSortMode:
      threadSortMode === 'alpha' || threadSortMode === 'custom' ? threadSortMode : 'activity',
    spaceOrder,
    threadOrder,
    threadGroups,
  };
}

/** Viewport width at or below which the chat panels are separate screens. */
const MOBILE_BREAKPOINT_PX = 768;

/** Event detail for thread selection. */
export interface ThreadSelectDetail {
  conversationKey: string;
  projectId: string;
  projectSlug: string;
  threadName: string;
  defaultAgent: string;
}

@customElement('scion-chat-space-rail')
export class ScionChatSpaceRail extends LitElement {
  /** Currently selected conversation key. */
  @property()
  selectedKey = '';

  @state() private spaces: ChatSpace[] = [];
  @state() private threadsBySpace = new Map<string, ChatSpaceThread[]>();
  @state() private collapsedSpaces = new Set<string>();
  @state() private loading = true;
  @state() private prefs: RailPrefs = {
    spaceSortMode: 'activity',
    threadSortMode: 'activity',
    spaceOrder: undefined,
    threadOrder: undefined,
    threadGroups: undefined,
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
  /** Space filter: 'all' shows everything, 'unread' shows only spaces with unread. */
  @state() private spaceFilter: 'all' | 'unread' = 'all';
  /** Project id of the space header currently being dragged, if any. */
  @state() private draggingSpaceId: string | null = null;
  /** Project id of the space header the drag is hovering over. */
  @state() private dragOverSpaceId: string | null = null;
  /** Project id for which the emoji picker is open, or null if closed. */
  @state() private emojiPickerSpaceId: string | null = null;

  // --- Thread DnD state ---
  /** Thread id currently being dragged, if any. */
  @state() private draggingThreadId: string | null = null;
  /** Thread id the drag is hovering over. */
  @state() private dragOverThreadId: string | null = null;

  // --- Thread groups state ---
  /** Set of collapsed group IDs. */
  @state() private collapsedGroups = new Set<string>();
  /** Group header id the drag is hovering over. */
  @state() private dragOverGroupId: string | null = null;
  /** State for the group name prompt (inline input). */
  @state() private groupNameInput: {
    projectId: string;
    threadId?: string;
    renamingGroupId?: string;
    value: string;
  } | null = null;
  /** When creating a thread, the target group to add it to (if any). */
  private _createThreadGroupId: string | null = null;

  static override styles = css`
    :host {
      display: flex;
      flex-direction: column;
      height: 100%;
      overflow: hidden;
      background: var(--scion-surface, #ffffff);
    }

    /*
     * Section heading, styled like the dashboard nav's section titles
     * ("OVERVIEW", "MANAGEMENT") so chat reads as a peer of the dashboard.
     */
    .rail-header {
      display: flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      font-size: var(--chat-fs-md);
      font-weight: 600;
      color: var(--scion-text, #1e293b);
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
      font-size: var(--chat-fs-sm);
      font-weight: 700;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      user-select: none;
    }

    .space-header:hover {
      color: var(--scion-text, #1e293b);
    }

    /* Reorder affordances: the dragged header fades, the hovered one grows a
       line marking where the drop lands. */
    .space-header.dragging {
      opacity: 0.4;
    }

    .space-header.drag-over {
      box-shadow: inset 0 2px 0 0 var(--scion-primary, #3b82f6);
    }

    .space-header .chevron {
      transition: transform 0.15s;
      font-size: var(--chat-fs-base);
    }

    .space-header .chevron.collapsed {
      transform: rotate(-90deg);
    }

    .space-header .space-emoji {
      font-size: 1rem;
      text-transform: none;
      line-height: 1;
      flex-shrink: 0;
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
      font-size: var(--chat-fs-2xs);
      font-weight: 700;
      padding: 0.0625rem 0.3125rem;
      border-radius: 0.5rem;
      min-width: 1rem;
      text-align: center;
    }

    .space-header .mention-badge {
      background: var(--scion-danger-500, #ef4444);
      color: #fff;
      font-size: var(--chat-fs-2xs);
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
      font-size: var(--chat-fs-base);
      padding: 0.125rem;
    }

    .space-actions sl-menu {
      min-width: 120px;
      padding: 0.125rem 0;
    }

    .space-actions sl-menu-item::part(base) {
      font-size: var(--chat-fs-base);
      padding: 0.25rem 0.5rem;
    }

    .space-actions sl-menu-item::part(label) {
      font-size: var(--chat-fs-base);
    }

    .space-actions sl-menu-item sl-icon {
      font-size: var(--chat-fs-base);
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
      font-size: var(--chat-fs-md);
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
      font-size: var(--chat-fs-base);
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
      font-size: var(--chat-fs-xs);
      color: var(--scion-text-muted, #64748b);
      flex-shrink: 0;
    }

    .thread-item .mute-icon {
      font-size: var(--chat-fs-xs);
      color: var(--scion-text-muted, #64748b);
      flex-shrink: 0;
    }

    /* Thread DnD feedback */
    .thread-item.dragging {
      opacity: 0.4;
    }

    .thread-item.drag-over {
      box-shadow: inset 0 2px 0 0 var(--scion-primary, #3b82f6);
    }

    /* Thread group header */
    .thread-group-header {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.375rem 0.75rem 0.375rem 1.25rem;
      cursor: pointer;
      font-size: 0.6875rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.04em;
      color: var(--scion-text-muted, #64748b);
      user-select: none;
    }

    .thread-group-header:hover {
      color: var(--scion-text, #1e293b);
    }

    .thread-group-header.drag-over {
      box-shadow: inset 0 2px 0 0 var(--scion-primary, #3b82f6);
    }

    .thread-group-header .chevron {
      transition: transform 0.15s;
      font-size: 0.625rem;
    }

    .thread-group-header .chevron.collapsed {
      transform: rotate(-90deg);
    }

    .thread-group-header .group-name {
      flex: 1;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .thread-group-header .group-count {
      font-size: 0.625rem;
      color: var(--scion-text-muted, #64748b);
      font-weight: 400;
    }

    /* Threads inside a group get a slight indent */
    .thread-group .thread-item {
      padding-left: 2.25rem;
    }

    /* Group name inline input */
    .group-name-input {
      padding: 0.25rem 0.75rem 0.25rem 1.25rem;
    }

    .group-name-input sl-input::part(base) {
      font-size: 0.8125rem;
      min-height: 1.5rem;
      background: var(--scion-surface-raised, #ffffff);
      border-color: var(--scion-border, #e2e8f0);
    }

    .group-name-input sl-input::part(input) {
      color: var(--scion-text, #1e293b);
    }

    /* Create thread inline input */
    .create-thread {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.25rem 0.75rem 0.25rem 1.75rem;
    }

    .create-thread sl-input::part(base) {
      font-size: var(--chat-fs-md);
      min-height: 1.75rem;
      background: var(--scion-surface-raised, #ffffff);
      border-color: var(--scion-border, #e2e8f0);
    }

    .create-thread sl-input::part(input) {
      color: var(--scion-text, #1e293b);
    }

    .rename-input::part(input) {
      color: var(--scion-text, #1e293b);
    }

    .rename-input::part(base) {
      background: var(--scion-surface-raised, #ffffff);
      border-color: var(--scion-border, #e2e8f0);
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
      font-size: var(--chat-fs-md);
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
      font-size: var(--chat-fs-lg);
    }

    /* Loading / empty */
    .loading-state {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* Filter + sort toolbar */
    .rail-toolbar {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.375rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .filter-toggle {
      display: inline-flex;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      overflow: hidden;
      flex: 1;
    }

    .filter-toggle button {
      display: inline-flex;
      align-items: center;
      gap: 0.125rem;
      height: 1.5rem;
      border: none;
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      padding: 0 0.5rem;
      font-size: var(--chat-fs-sm);
      font-family: inherit;
      font-weight: 500;
      transition: all 150ms ease;
      white-space: nowrap;
      flex: 1;
      justify-content: center;
    }

    .filter-toggle button:not(:last-child) {
      border-right: 1px solid var(--scion-border, #e2e8f0);
    }

    .filter-toggle button:hover:not(.active) {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .filter-toggle button.active {
      background: var(--scion-primary, #3b82f6);
      color: white;
    }

    .filter-toggle button sl-icon {
      font-size: var(--chat-fs-sm);
    }

    .sort-btn {
      flex-shrink: 0;
    }

    .sort-btn::part(base) {
      font-size: var(--chat-fs-base);
      padding: 0.125rem;
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
      font-size: var(--chat-fs-md);
      min-height: 1.5rem;
    }

    /* Emoji picker dialog */
    .emoji-grid {
      display: grid;
      grid-template-columns: repeat(8, 1fr);
      gap: 0.25rem;
      padding: 0.5rem;
    }

    .emoji-grid button {
      background: none;
      border: 1px solid transparent;
      border-radius: 0.25rem;
      font-size: 1.25rem;
      line-height: 1;
      padding: 0.375rem;
      cursor: pointer;
      text-align: center;
    }

    .emoji-grid button:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
      border-color: var(--scion-border, #e2e8f0);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    // Restore persisted filter/sort from localStorage
    const savedFilter = localStorage.getItem('scion-chat-space-filter');
    if (savedFilter === 'unread') this.spaceFilter = 'unread';
    void this.loadData();
    // Close context menu on outside click
    this._outsideClickHandler = this.handleOutsideClick.bind(this);
    document.addEventListener('click', this._outsideClickHandler);
  }

  override updated(changedProperties: Map<string, unknown>): void {
    // Auto-expand the space containing the selected thread (deep-link support)
    if (changedProperties.has('selectedKey') && this.selectedKey) {
      this.expandSpaceForSelectedKey();
    }
  }

  /**
   * Expand a space without selecting a thread in it. Mobile space navigation
   * stops here: the point is to show the thread list, not to open a thread.
   */
  expandSpace(projectId: string): void {
    if (!this.collapsedSpaces.has(projectId)) return;
    const next = new Set(this.collapsedSpaces);
    next.delete(projectId);
    this.collapsedSpaces = next;
  }

  /** Expand the space that contains the currently selected thread. */
  private expandSpaceForSelectedKey(): void {
    for (const space of this.spaces) {
      const threads = this.threadsBySpace.get(space.projectId) || [];
      const hasThread = threads.some((t) => t.id === this.selectedKey);
      if (hasThread && this.collapsedSpaces.has(space.projectId)) {
        const newSet = new Set(this.collapsedSpaces);
        newSet.delete(space.projectId);
        this.collapsedSpaces = newSet;
        break;
      }
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this._outsideClickHandler) {
      document.removeEventListener('click', this._outsideClickHandler);
    }
    for (const timer of this._recentlyCreatedTimeouts.values()) {
      clearTimeout(timer);
    }
    this._recentlyCreatedTimeouts.clear();
    this._recentlyCreatedTopicIds.clear();
  }

  private _outsideClickHandler: ((e: Event) => void) | null = null;

  private handleOutsideClick(): void {
    if (this.contextMenuTarget) {
      this.contextMenuTarget = null;
    }
    if (this.groupContextMenuTarget) {
      this.groupContextMenuTarget = null;
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
    // Only show the full-page spinner on the very first load. Subsequent
    // reloads (e.g. SSE-triggered) update data in-place without a flash.
    if (!this._initialLoadDone) {
      this.loading = true;
    }
    try {
      await Promise.all([this.loadSpaces(), this.loadPrefs()]);
    } finally {
      this.loading = false;
      // Notify parent that rail data is ready (for SSE scope setup)
      this.dispatchEvent(
        new CustomEvent('rail-loaded', {
          detail: {
            spaceIds: this.getSpaceIds(),
            spaces: this.spaces.map((s) => ({
              projectId: s.projectId,
              projectSlug: s.projectSlug,
              projectName: s.projectName,
              // Carried so the tab-title badge can reuse this load instead of
              // asking the server for the same rollup a second time.
              unreadCount: s.unreadCount,
            })),
          },
          bubbles: true,
          composed: true,
        })
      );
    }
  }

  /** Track whether spaces have been loaded at least once. */
  private _initialLoadDone = false;

  /** Track known space IDs so we can collapse only truly new spaces on reload. */
  private _knownSpaceIds = new Set<string>();

  private async loadSpaces(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/chat/spaces');
      if (res.ok) {
        const data = (await res.json()) as { spaces?: ChatSpace[] };
        this.spaces = data.spaces || [];
        const newSpaceIds = new Set(this.spaces.map((s) => s.projectId));
        if (!this._initialLoadDone) {
          // Collapse all spaces by default on first load — user expands explicitly
          this.collapsedSpaces = new Set(newSpaceIds);
          this._initialLoadDone = true;
        } else {
          // Preserve existing collapsed/expanded state on reload.
          // Remove stale entries for spaces that no longer exist.
          const updated = new Set([...this.collapsedSpaces].filter((id) => newSpaceIds.has(id)));
          // Collapse any brand-new spaces the user hasn't seen yet.
          for (const id of newSpaceIds) {
            if (!this._knownSpaceIds.has(id)) {
              updated.add(id);
            }
          }
          this.collapsedSpaces = updated;
        }
        this._knownSpaceIds = newSpaceIds;
        // Load threads for each space
        await Promise.all(this.spaces.map((s) => this.loadThreads(s.projectId)));
        // Auto-expand the space containing the selected thread (deep-link on first load)
        if (this.selectedKey) {
          this.expandSpaceForSelectedKey();
        }
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

  private async loadPrefs(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/chat/user-prefs');
      if (res.ok) {
        this.prefs = parseRailPrefs(await res.json());
      }
    } catch {
      // Use defaults
    }
  }

  /**
   * Save user preferences. Applied locally first so the rail responds to the
   * click, then reconciled with what the server actually stored — the server
   * defaults blank modes, so its answer can differ from the request.
   */
  async savePrefs(update: Partial<RailPrefs>): Promise<void> {
    const previous = this.prefs;
    const newPrefs = { ...this.prefs, ...update };
    this.prefs = newPrefs;
    try {
      const res = await apiFetch('/api/v1/chat/user-prefs', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          spaceSortMode: newPrefs.spaceSortMode,
          threadSortMode: newPrefs.threadSortMode,
          spaceOrder: JSON.stringify(newPrefs.spaceOrder ?? []),
          threadOrder: JSON.stringify(newPrefs.threadOrder ?? {}),
          threadGroups: JSON.stringify(newPrefs.threadGroups ?? {}),
        }),
      });
      if (!res.ok) {
        this.prefs = previous;
        return;
      }
      this.prefs = parseRailPrefs(await res.json());
    } catch {
      this.prefs = previous;
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
          const orderMap = new Map(this.prefs.spaceOrder.map((id, i) => [id, i]));
          spaces.sort((a, b) => {
            const ai = orderMap.get(a.projectId);
            const bi = orderMap.get(b.projectId);
            if (ai === undefined && bi === undefined) return 0;
            if (ai === undefined) return 1;
            if (bi === undefined) return -1;
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

  // ---------------------------------------------------------------------------
  // Custom ordering
  // ---------------------------------------------------------------------------

  /**
   * Persist a new space order. Dragging or nudging a space is an unambiguous
   * statement about where it belongs, so it switches the rail to custom sort
   * rather than being refused in activity or alpha mode — the check moving to
   * "Custom" in the sort menu is what tells the user the mode changed. The
   * alternative (disabling the drag outside custom mode) would make the
   * feature undiscoverable: you would have to know to pick Custom first, in a
   * menu that gives no hint of what a custom order even is.
   */
  private async applySpaceOrder(order: string[]): Promise<void> {
    await this.savePrefs({ spaceSortMode: 'custom', spaceOrder: order });
  }

  /** The order the user is currently looking at, as project ids. */
  private currentSpaceOrder(): string[] {
    return this.getSortedSpaces().map((s) => s.projectId);
  }

  /** Move a space one slot up (delta = -1) or down (delta = 1). */
  async moveSpace(spaceId: string, delta: -1 | 1): Promise<void> {
    if (this.spaceFilter !== 'all') return;
    const order = this.currentSpaceOrder();
    const idx = order.indexOf(spaceId);
    if (idx === -1) return;
    const swapIdx = idx + delta;
    if (swapIdx < 0 || swapIdx >= order.length) return;
    const next = [...order];
    [next[idx], next[swapIdx]] = [next[swapIdx], next[idx]];
    await this.applySpaceOrder(next);
  }

  /** Whether a space sits at the first or last edge of the visible list. */
  isSpaceAtEdge(spaceId: string, edge: 'first' | 'last'): boolean {
    if (this.spaceFilter !== 'all') return true;
    const order = this.currentSpaceOrder();
    const idx = order.indexOf(spaceId);
    if (idx === -1) return true;
    return edge === 'first' ? idx === 0 : idx === order.length - 1;
  }

  private handleSpaceDragStart(e: DragEvent, projectId: string): void {
    this.draggingSpaceId = projectId;
    if (e.dataTransfer) {
      e.dataTransfer.effectAllowed = 'move';
      // Firefox ignores a drag that carries no data.
      e.dataTransfer.setData('text/plain', projectId);
    }
  }

  private handleSpaceDragOver(e: DragEvent, projectId: string): void {
    if (!this.draggingSpaceId) return;
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
    if (this.dragOverSpaceId !== projectId) this.dragOverSpaceId = projectId;
  }

  private async handleSpaceDrop(e: DragEvent, targetProjectId: string): Promise<void> {
    e.preventDefault();
    const sourceId = this.draggingSpaceId;
    this.draggingSpaceId = null;
    this.dragOverSpaceId = null;
    if (!sourceId || sourceId === targetProjectId) return;

    const order = this.currentSpaceOrder();
    const from = order.indexOf(sourceId);
    const to = order.indexOf(targetProjectId);
    if (from === -1 || to === -1) return;
    const next = [...order];
    const [moved] = next.splice(from, 1);
    next.splice(to, 0, moved);
    await this.applySpaceOrder(next);
  }

  private handleSpaceDragEnd(): void {
    this.draggingSpaceId = null;
    this.dragOverSpaceId = null;
  }

  // ---------------------------------------------------------------------------
  // Thread DnD reorder
  // ---------------------------------------------------------------------------

  /** Whether thread reordering is allowed (same logic as space reorder). */
  private canReorderThreads(): boolean {
    return this.spaceFilter === 'all';
  }

  /** The current thread display order (excluding #general) for a space. */
  private currentThreadOrder(projectId: string): string[] {
    return this.getSortedThreads(projectId)
      .filter((t) => !t.isGeneral)
      .map((t) => t.id);
  }

  private async applyThreadOrder(projectId: string, order: string[]): Promise<void> {
    const threadOrder = { ...(this.prefs.threadOrder ?? {}), [projectId]: order };
    await this.savePrefs({ threadSortMode: 'custom', threadOrder });
  }

  private handleThreadDragStart(e: DragEvent, threadId: string): void {
    if (!this.canReorderThreads()) return;
    this.draggingThreadId = threadId;
    if (e.dataTransfer) {
      e.dataTransfer.effectAllowed = 'move';
      e.dataTransfer.setData('text/plain', threadId);
    }
  }

  private handleThreadDragOver(e: DragEvent, threadId: string): void {
    if (!this.draggingThreadId) return;
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
    if (this.dragOverThreadId !== threadId) this.dragOverThreadId = threadId;
    // Clear group highlight when over a thread
    this.dragOverGroupId = null;
  }

  private async handleThreadDrop(
    e: DragEvent,
    targetThreadId: string,
    projectId: string
  ): Promise<void> {
    e.preventDefault();
    const sourceId = this.draggingThreadId;
    this.draggingThreadId = null;
    this.dragOverThreadId = null;
    this.dragOverGroupId = null;
    if (!sourceId || sourceId === targetThreadId) return;

    const order = this.currentThreadOrder(projectId);
    const from = order.indexOf(sourceId);
    const to = order.indexOf(targetThreadId);
    if (from === -1 || to === -1) return;
    const next = [...order];
    const [moved] = next.splice(from, 1);
    next.splice(to, 0, moved);

    // If dropping on a thread inside a group, move the dragged thread into that group
    const groups = this.prefs.threadGroups?.[projectId];
    if (groups) {
      const targetGroup = groups.find((g) => g.threadIds.includes(targetThreadId));
      const sourceGroup = groups.find((g) => g.threadIds.includes(sourceId));
      if (targetGroup !== sourceGroup) {
        const updatedGroups = groups.map((g) => {
          // Remove from source group
          const filtered = g.threadIds.filter((id) => id !== sourceId);
          if (g === targetGroup) {
            // Add to target group at the right position relative to the target
            const targetIdx = filtered.indexOf(targetThreadId);
            const inserted = [...filtered];
            inserted.splice(targetIdx, 0, sourceId);
            return { ...g, threadIds: inserted };
          }
          return { ...g, threadIds: filtered };
        });
        const threadGroups = { ...(this.prefs.threadGroups ?? {}), [projectId]: updatedGroups };
        await this.savePrefs({
          threadSortMode: 'custom',
          threadOrder: { ...(this.prefs.threadOrder ?? {}), [projectId]: next },
          threadGroups,
        });
        return;
      } else if (targetGroup) {
        // Same group — reorder within it
        const updatedGroups = groups.map((g) => {
          if (g !== targetGroup) return g;
          const ids = g.threadIds.filter((id) => id !== sourceId);
          const idx = ids.indexOf(targetThreadId);
          ids.splice(idx, 0, sourceId);
          return { ...g, threadIds: ids };
        });
        const threadGroups = { ...(this.prefs.threadGroups ?? {}), [projectId]: updatedGroups };
        await this.savePrefs({
          threadSortMode: 'custom',
          threadOrder: { ...(this.prefs.threadOrder ?? {}), [projectId]: next },
          threadGroups,
        });
        return;
      }
    }

    await this.applyThreadOrder(projectId, next);
  }

  private handleThreadDragEnd(): void {
    this.draggingThreadId = null;
    this.dragOverThreadId = null;
    this.dragOverGroupId = null;
  }

  /** Move a thread one slot up or down in its space. */
  private async moveThread(threadId: string, projectId: string, delta: -1 | 1): Promise<void> {
    if (!this.canReorderThreads()) return;

    const groups = this.prefs.threadGroups?.[projectId] ?? [];
    const containingGroup = groups.find((g) => g.threadIds.includes(threadId));

    if (containingGroup) {
      // Reorder within the group's threadIds
      const ids = [...containingGroup.threadIds];
      const idx = ids.indexOf(threadId);
      const swapIdx = idx + delta;
      if (swapIdx < 0 || swapIdx >= ids.length) return;
      [ids[idx], ids[swapIdx]] = [ids[swapIdx], ids[idx]];

      const updatedGroups = groups.map((g) =>
        g.id === containingGroup.id ? { ...g, threadIds: ids } : g
      );
      await this.savePrefs({
        threadGroups: { ...(this.prefs.threadGroups ?? {}), [projectId]: updatedGroups },
      });
    } else {
      // Reorder among ungrouped threads only (exclude grouped thread IDs)
      const groupedIds = new Set(groups.flatMap((g) => g.threadIds));
      const currentOrder = [...this.currentThreadOrder(projectId)];
      const ungroupedOrder = currentOrder.filter((id) => !groupedIds.has(id));

      const idx = ungroupedOrder.indexOf(threadId);
      const swapIdx = idx + delta;
      if (idx < 0 || swapIdx < 0 || swapIdx >= ungroupedOrder.length) return;

      // Identify swap target in the ungrouped list
      const swapTarget = ungroupedOrder[swapIdx];

      // Apply swap in the FULL order (find actual positions)
      const fullIdx = currentOrder.indexOf(threadId);
      const fullSwapIdx = currentOrder.indexOf(swapTarget);
      if (fullIdx < 0 || fullSwapIdx < 0) return;
      [currentOrder[fullIdx], currentOrder[fullSwapIdx]] = [
        currentOrder[fullSwapIdx],
        currentOrder[fullIdx],
      ];

      await this.applyThreadOrder(projectId, currentOrder);
    }
  }

  private isThreadAtEdge(threadId: string, projectId: string, edge: 'first' | 'last'): boolean {
    if (!this.canReorderThreads()) return true;

    const groups = this.prefs.threadGroups?.[projectId] ?? [];
    const containingGroup = groups.find((g) => g.threadIds.includes(threadId));

    if (containingGroup) {
      // Check edges within the group
      const ids = containingGroup.threadIds;
      return edge === 'first' ? ids[0] === threadId : ids[ids.length - 1] === threadId;
    }

    // Check edges among ungrouped threads only (exclude grouped thread IDs)
    const groupedIds = new Set(groups.flatMap((g) => g.threadIds));
    const ungroupedOrder = this.currentThreadOrder(projectId).filter((id) => !groupedIds.has(id));
    if (ungroupedOrder.length === 0) return true;
    const index = ungroupedOrder.indexOf(threadId);
    if (index === -1) return true;
    return edge === 'first' ? index === 0 : index === ungroupedOrder.length - 1;
  }

  // ---------------------------------------------------------------------------
  // Thread groups
  // ---------------------------------------------------------------------------

  /** Get groups for a space, defaulting to empty array. */
  private getGroups(projectId: string): ThreadGroup[] {
    return this.prefs.threadGroups?.[projectId] ?? [];
  }

  private toggleGroupCollapse(groupId: string): void {
    const next = new Set(this.collapsedGroups);
    if (next.has(groupId)) {
      next.delete(groupId);
    } else {
      next.add(groupId);
    }
    this.collapsedGroups = next;
  }

  /** Generate a simple unique id for a new group. */
  private generateGroupId(): string {
    return 'g-' + Math.random().toString(36).slice(2, 10) + Date.now().toString(36);
  }

  /** Create a new thread group and optionally move a thread into it. */
  private async createGroup(projectId: string, name: string, threadId?: string): Promise<void> {
    const currentGroups = this.getGroups(projectId);
    if (currentGroups.length >= 20) {
      showToast('Maximum 20 groups per space', 'warning');
      return;
    }
    const newGroup: ThreadGroup = {
      id: this.generateGroupId(),
      name,
      threadIds: threadId ? [threadId] : [],
    };
    // Remove thread from any existing group (immutably) and append the new group
    const groups = threadId
      ? [
          ...currentGroups.map((g) => ({
            ...g,
            threadIds: g.threadIds.filter((id) => id !== threadId),
          })),
          newGroup,
        ]
      : [...currentGroups, newGroup];

    // Insert the group ID into threadOrder so it co-mingles with threads.
    const currentOrder = [...(this.prefs.threadOrder?.[projectId] ?? [])];
    if (threadId) {
      // Replace the thread's position with the group ID
      const idx = currentOrder.indexOf(threadId);
      if (idx !== -1) {
        currentOrder[idx] = newGroup.id;
      } else {
        currentOrder.push(newGroup.id);
      }
    } else {
      currentOrder.push(newGroup.id);
    }

    const threadGroups = { ...(this.prefs.threadGroups ?? {}), [projectId]: groups };
    const threadOrder = { ...(this.prefs.threadOrder ?? {}), [projectId]: currentOrder };
    await this.savePrefs({ threadGroups, threadOrder });
  }

  /** Move a thread into an existing group. */
  private async moveThreadToGroup(
    threadId: string,
    groupId: string,
    projectId: string
  ): Promise<void> {
    const groups = this.getGroups(projectId).map((g) => {
      const filtered = g.threadIds.filter((id) => id !== threadId);
      return g.id === groupId
        ? { ...g, threadIds: [...filtered, threadId] }
        : { ...g, threadIds: filtered };
    });
    const threadGroups = { ...(this.prefs.threadGroups ?? {}), [projectId]: groups };
    await this.savePrefs({ threadGroups });
  }

  /** Remove a thread from its group (move to ungrouped). */
  private async removeThreadFromGroup(threadId: string, projectId: string): Promise<void> {
    const groups = this.getGroups(projectId).map((g) => ({
      ...g,
      threadIds: g.threadIds.filter((id) => id !== threadId),
    }));
    const threadGroups = { ...(this.prefs.threadGroups ?? {}), [projectId]: groups };
    await this.savePrefs({ threadGroups });
  }

  /** Rename a thread group. */
  private async renameGroup(groupId: string, name: string, projectId: string): Promise<void> {
    const groups = this.getGroups(projectId).map((g) => (g.id === groupId ? { ...g, name } : g));
    const threadGroups = { ...(this.prefs.threadGroups ?? {}), [projectId]: groups };
    await this.savePrefs({ threadGroups });
  }

  /** Delete a thread group, moving its threads to ungrouped. */
  private async deleteGroup(groupId: string, projectId: string): Promise<void> {
    // Retrieve the group's thread IDs before deleting, so they can be
    // re-inserted into the threadOrder at the position the group occupied.
    const deletedGroup = this.getGroups(projectId).find((g) => g.id === groupId);
    const groups = this.getGroups(projectId).filter((g) => g.id !== groupId);
    const threadGroups = { ...(this.prefs.threadGroups ?? {}), [projectId]: groups };

    // Replace the group ID in threadOrder with its former thread IDs,
    // filtering out any that already exist elsewhere to avoid duplicates.
    const currentOrder = [...(this.prefs.threadOrder?.[projectId] ?? [])];
    const idx = currentOrder.indexOf(groupId);
    if (idx !== -1) {
      const deletedThreadIds = deletedGroup?.threadIds ?? [];
      const existingIds = new Set(currentOrder);
      existingIds.delete(groupId); // the group entry itself is being replaced
      const newIds = deletedThreadIds.filter((id) => !existingIds.has(id));
      currentOrder.splice(idx, 1, ...newIds);
    }
    const threadOrder = { ...(this.prefs.threadOrder ?? {}), [projectId]: currentOrder };
    await this.savePrefs({ threadGroups, threadOrder });
  }

  /** Handle dropping a thread onto a group header. */
  private handleGroupDragOver(e: DragEvent, groupId: string): void {
    if (!this.draggingThreadId) return;
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
    this.dragOverGroupId = groupId;
    this.dragOverThreadId = null;
  }

  private async handleGroupDrop(e: DragEvent, groupId: string, projectId: string): Promise<void> {
    e.preventDefault();
    const threadId = this.draggingThreadId;
    this.draggingThreadId = null;
    this.dragOverThreadId = null;
    this.dragOverGroupId = null;
    if (!threadId) return;

    if (groupId === '__ungrouped__') {
      await this.removeThreadFromGroup(threadId, projectId);
    } else {
      await this.moveThreadToGroup(threadId, groupId, projectId);
    }
  }

  /** Start inline input for creating/renaming a group. */
  private startGroupNameInput(
    projectId: string,
    opts: { threadId?: string; renamingGroupId?: string; initialValue?: string }
  ): void {
    this.contextMenuTarget = null;
    const input: {
      projectId: string;
      threadId?: string;
      renamingGroupId?: string;
      value: string;
    } = {
      projectId,
      value: opts.initialValue ?? '',
    };
    if (opts.threadId !== undefined) input.threadId = opts.threadId;
    if (opts.renamingGroupId !== undefined) input.renamingGroupId = opts.renamingGroupId;
    this.groupNameInput = input;
  }

  private async submitGroupNameInput(): Promise<void> {
    const input = this.groupNameInput;
    if (!input || !input.value.trim()) {
      this.groupNameInput = null;
      return;
    }
    // Clear immediately to prevent double-submission (Enter + blur race).
    this.groupNameInput = null;
    if (input.renamingGroupId) {
      await this.renameGroup(input.renamingGroupId, input.value.trim(), input.projectId);
    } else {
      await this.createGroup(input.projectId, input.value.trim(), input.threadId);
    }
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

    // In custom sort mode, the user's explicit order takes control.
    // #general is always first regardless.
    if (this.prefs.threadSortMode === 'custom') {
      const order = this.prefs.threadOrder?.[projectId];
      if (order && order.length > 0) {
        const general = threads.filter((t) => t.isGeneral);
        const rest = threads.filter((t) => !t.isGeneral);
        const orderMap = new Map(order.map((id, i) => [id, i]));
        rest.sort((a, b) => {
          const ai = orderMap.get(a.id);
          const bi = orderMap.get(b.id);
          if (ai === undefined && bi === undefined) return 0;
          if (ai === undefined) return 1;
          if (bi === undefined) return -1;
          return ai - bi;
        });
        return [...general, ...rest];
      }
    }

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

  /** Clicking empty area of the rail body resets to global view. */
  private handleRailBodyClick(e: MouseEvent): void {
    // Only fire when the click target is the rail-body itself (empty space)
    const target = e.target as HTMLElement;
    if (target === e.currentTarget) {
      this.dispatchEvent(new CustomEvent('reset-view', { bubbles: true, composed: true }));
    }
  }

  private handleThreadClick(thread: ChatSpaceThread, projectId: string): void {
    const space = this.spaces.find((s) => s.projectId === projectId);
    this.dispatchEvent(
      new CustomEvent<ThreadSelectDetail>('thread-select', {
        detail: {
          conversationKey: thread.id,
          projectId,
          projectSlug: space?.projectSlug || '',
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

  /** Are we under the breakpoint where the rail is a screen of its own? */
  private isMobileViewport(): boolean {
    return window.innerWidth <= MOBILE_BREAKPOINT_PX;
  }

  private handleCollapsedSpaceClick(space: ChatSpace): void {
    // On desktop the rail sits beside the conversation, so opening #general
    // costs the user nothing. On mobile selecting a thread slides the rail
    // off-screen, which would hide the thread list the tap was asking to
    // see — there the expansion is all this does.
    this.expandSpace(space.projectId);
    if (this.isMobileViewport()) return;

    const threads = this.threadsBySpace.get(space.projectId) || [];
    const target = threads.find((t) => t.isGeneral) || threads[0];
    if (target) {
      this.handleThreadClick(target, space.projectId);
    }
  }

  private handleContextMenu(e: MouseEvent, thread: ChatSpaceThread, projectId: string): void {
    e.preventDefault();
    e.stopPropagation();
    this.contextMenuTarget = { type: 'thread', thread, projectId };
    this.contextMenuPos = { x: e.clientX, y: e.clientY };
  }

  // _projectId is kept for the call site's symmetry with the other context-menu
  // actions; markThreadRead finds the thread's space itself.
  private async handleMarkRead(thread: ChatSpaceThread, _projectId: string): Promise<void> {
    this.contextMenuTarget = null;
    // The server requires the watermark to move to a specific message. Without
    // an ID it rejects the request, and the dot comes back on the next reload.
    if (!thread.lastMessageId) {
      this.markThreadRead(thread.id);
      return;
    }
    try {
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(thread.id)}/read`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ messageId: thread.lastMessageId }),
        }
      );
      if (!res.ok) return;
      // Update locally through the same helper the no-watermark path above
      // uses. The badge arithmetic lives there and nowhere else: doing it
      // inline here is how "Mark as read" on an already-read thread came to
      // decrement the badge on every click (#1029).
      this.markThreadRead(thread.id);
    } catch {
      // Non-critical
    }
  }

  /**
   * Clear a thread's unread markers without talking to the server. Called when
   * the thread view itself advanced the watermark — the rail has no other way
   * to learn that happened — and by "Mark as read" once the server has moved
   * the watermark for it.
   *
   * The space badge is a server-side rollup of unread, unmuted threads, so a
   * thread only leaves it if it was in it: an already-read thread and a muted
   * one both take nothing off, or they would eat another thread's unread.
   */
  markThreadRead(threadId: string): void {
    for (const [projectId, threads] of this.threadsBySpace) {
      const target = threads.find((t) => t.id === threadId);
      if (!target || (!target.hasUnread && !target.hasUnreadMention)) continue;
      this.updateThread(projectId, threadId, { hasUnread: false, hasUnreadMention: false });
      if (!target.muted) this.adjustSpaceUnread(projectId, -1);
      return;
    }
  }

  /** Nudge a space's unread badge, floored at zero. */
  private adjustSpaceUnread(projectId: string, delta: number): void {
    this.spaces = this.spaces.map((s) =>
      s.projectId === projectId ? { ...s, unreadCount: Math.max(0, s.unreadCount + delta) } : s
    );
  }

  /**
   * Apply a mute decision locally, keeping the space badge in step with it.
   * The server's rollup does not count muted threads, so an unread thread
   * leaves the badge when it is muted and rejoins it when it is unmuted —
   * without this the badge only tells the truth again after a reload.
   */
  private setThreadMuted(projectId: string, threadId: string, muted: boolean): void {
    const target = (this.threadsBySpace.get(projectId) || []).find((t) => t.id === threadId);
    if (!target || (target.muted === true) === muted) return;
    this.updateThread(projectId, threadId, { muted });
    if (target.hasUnread) this.adjustSpaceUnread(projectId, muted ? -1 : 1);
  }

  private async handleMarkSpaceRead(projectId: string): Promise<void> {
    this.contextMenuTarget = null;
    try {
      const res = await apiFetch(`/api/v1/chat/spaces/${encodeURIComponent(projectId)}/read`, {
        method: 'POST',
      });
      // A refused request leaves every watermark where it was, so clearing the
      // dots here would show the space as read until the next reload (#1029).
      if (!res.ok) return;
      // Update all threads in this space locally
      const threads = this.threadsBySpace.get(projectId) || [];
      const newMap = new Map(this.threadsBySpace);
      newMap.set(
        projectId,
        threads.map((t) => ({ ...t, hasUnread: false, hasUnreadMention: false }))
      );
      this.threadsBySpace = newMap;
      this.spaces = this.spaces.map((s) =>
        s.projectId === projectId ? { ...s, unreadCount: 0, hasUnreadMention: false } : s
      );
    } catch {
      // Non-critical
    }
  }

  /**
   * Toggle a thread's pinned state. Applied locally first so the rail reorders
   * on the click, and rolled back if the server refuses — a pin the server
   * does not have would silently survive until the next reload otherwise.
   */
  private async handleTogglePin(thread: ChatSpaceThread, projectId: string): Promise<void> {
    this.contextMenuTarget = null;
    const next = !thread.pinned;
    this.updateThread(projectId, thread.id, { pinned: next });
    try {
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(thread.id)}/pin`,
        {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ pinned: next }),
        }
      );
      if (!res.ok) {
        this.updateThread(projectId, thread.id, { pinned: thread.pinned });
        return;
      }
      const data = (await res.json().catch(() => ({}))) as { pinned?: boolean };
      if (typeof data.pinned === 'boolean' && data.pinned !== next) {
        this.updateThread(projectId, thread.id, { pinned: data.pinned });
      }
    } catch {
      this.updateThread(projectId, thread.id, { pinned: thread.pinned });
    }
  }

  /** Toggle a thread's muted state, with the same optimistic-then-reconcile shape as pin. */
  private async handleToggleMute(thread: ChatSpaceThread, projectId: string): Promise<void> {
    this.contextMenuTarget = null;
    const next = !thread.muted;
    this.setThreadMuted(projectId, thread.id, next);
    try {
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(thread.id)}/mute`,
        {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ muted: next }),
        }
      );
      if (!res.ok) {
        this.setThreadMuted(projectId, thread.id, thread.muted === true);
        return;
      }
      const data = (await res.json().catch(() => ({}))) as { muted?: boolean };
      if (typeof data.muted === 'boolean' && data.muted !== next) {
        this.setThreadMuted(projectId, thread.id, data.muted);
      }
    } catch {
      this.setThreadMuted(projectId, thread.id, thread.muted === true);
    }
  }

  // ---------------------------------------------------------------------------
  // Thread export (#1064)
  // ---------------------------------------------------------------------------

  /** Message shape returned by the conversations messages endpoint. */
  private async fetchThreadMessages(threadId: string): Promise<
    Array<{
      sender?: string;
      msg?: string;
      createdAt?: string;
      attachments?: string[];
    }>
  > {
    try {
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(threadId)}/messages`
      );
      if (!res.ok) return [];
      const data = (await res.json()) as { messages?: unknown[] };
      return (data.messages ?? []) as Array<{
        sender?: string;
        msg?: string;
        createdAt?: string;
        attachments?: string[];
      }>;
    } catch {
      return [];
    }
  }

  /** Format thread messages into a markdown document. */
  private formatThreadAsMarkdown(
    thread: ChatSpaceThread,
    messages: Array<{
      sender?: string;
      msg?: string;
      createdAt?: string;
      attachments?: string[];
    }>
  ): string {
    const lines: string[] = [];
    lines.push(`# Thread: ${thread.name}`);
    lines.push(`Exported: ${new Date().toLocaleString()}`);
    lines.push('');
    lines.push('---');

    for (const msg of messages) {
      const rawSender = msg.sender ?? 'Unknown';
      const sender = rawSender.replace(/^(user|agent):/, '');
      const ts = msg.createdAt ?? '';
      const content = msg.msg ?? '';
      const formattedTs = ts ? new Date(ts).toLocaleString() : '';

      lines.push('');
      lines.push(`**${sender}** (${formattedTs}):`);
      lines.push(content);

      // Include attachments as markdown links
      if (msg.attachments && msg.attachments.length > 0) {
        lines.push('');
        for (const att of msg.attachments) {
          const basename = att.split('/').pop() ?? att;
          lines.push(`- [${basename}](${att})`);
        }
      }

      lines.push('');
      lines.push('---');
    }

    return lines.join('\n');
  }

  /** Copy thread content as markdown to clipboard. */
  private async handleExportThread(thread: ChatSpaceThread): Promise<void> {
    this.contextMenuTarget = null;
    const messages = await this.fetchThreadMessages(thread.id);
    const markdown = this.formatThreadAsMarkdown(thread, messages);

    try {
      await navigator.clipboard.writeText(markdown);
      this.showExportToast('Copied to clipboard');
    } catch {
      // Fallback: if clipboard fails, still offer the download
      this.showExportToast('Clipboard unavailable — use Download instead');
    }
  }

  /** Download thread content as a markdown file. */
  private async handleDownloadThread(thread: ChatSpaceThread): Promise<void> {
    this.contextMenuTarget = null;
    const messages = await this.fetchThreadMessages(thread.id);
    const markdown = this.formatThreadAsMarkdown(thread, messages);

    const blob = new Blob([markdown], { type: 'text/markdown;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement('a');
    anchor.href = url;
    anchor.download = `${thread.name.replace(/[^a-zA-Z0-9_-]/g, '-')}.md`;
    document.body.appendChild(anchor);
    anchor.click();
    document.body.removeChild(anchor);
    URL.revokeObjectURL(url);
  }

  /** Show a brief toast notification for export actions. */
  private showExportToast(message: string): void {
    this.dispatchEvent(
      new CustomEvent('show-toast', {
        bubbles: true,
        composed: true,
        detail: { message, variant: 'primary', duration: 3000 },
      })
    );
  }

  private startRename(thread: ChatSpaceThread): void {
    this.contextMenuTarget = null;
    this.renamingThread = thread.id;
    this.renameValue = thread.name;
  }

  private async submitRename(projectId: string): Promise<void> {
    if (!this.renamingThread || !this.renameValue.trim()) {
      this.renamingThread = null;
      return;
    }
    try {
      await apiFetch(`/api/v1/chat/topics/${encodeURIComponent(this.renamingThread)}`, {
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
    const confirmed = await showConfirm(`Delete #${thread.name}? This cannot be undone.`, {
      title: 'Delete Thread',
      confirmText: 'Delete',
      variant: 'danger',
    });
    if (!confirmed) return;
    try {
      const res = await apiFetch(`/api/v1/chat/topics/${encodeURIComponent(thread.id)}`, {
        method: 'DELETE',
      });
      if (!res.ok) {
        const data = (await res.json().catch(() => ({}))) as { error?: string };
        showToast(data.error || 'Failed to delete thread', 'danger');
        return;
      }
      // Remove locally
      const threads = this.threadsBySpace.get(projectId) || [];
      const newMap = new Map(this.threadsBySpace);
      newMap.set(
        projectId,
        threads.filter((t) => t.id !== thread.id)
      );
      this.threadsBySpace = newMap;
    } catch (err) {
      showToast(err instanceof Error ? err.message : 'Failed to delete thread', 'danger');
    }
  }

  private startCreateThread(projectId: string): void {
    this.creatingThread = projectId;
    this.newThreadName = '';
  }

  /** IDs of topics created by this client — suppresses SSE-triggered reloads. */
  _recentlyCreatedTopicIds = new Set<string>();

  /** Timers for clearing _recentlyCreatedTopicIds entries — cleared on disconnect. */
  private _recentlyCreatedTimeouts = new Map<string, ReturnType<typeof setTimeout>>();

  private async submitCreateThread(projectId: string): Promise<void> {
    const threadName = this.newThreadName.trim();
    if (!threadName) {
      this.creatingThread = '';
      this._createThreadGroupId = null;
      return;
    }
    const targetGroupId = this._createThreadGroupId;
    try {
      const res = await apiFetch(`/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: threadName }),
      });
      if (res.ok) {
        const data = (await res.json()) as Record<string, unknown>;
        const id = data.id as string;
        const name = data.name as string;
        if (!id || typeof id !== 'string' || !name || typeof name !== 'string') {
          // Fallback to full reload if response is malformed
          await this.loadThreads(projectId);
          this.creatingThread = '';
          return;
        }
        const newThread: ChatSpaceThread = {
          id,
          name,
          isGeneral: false,
          pinned: false,
          hasUnread: false,
          hasUnreadMention: false,
          defaultAgent: (data.defaultAgent as string) || '',
          lastActivityAt: (data.lastActivityAt as string) || new Date().toISOString(),
        };
        // Optimistic local state update — no re-fetch needed
        const threads = this.threadsBySpace.get(projectId) || [];
        const newMap = new Map(this.threadsBySpace);
        newMap.set(projectId, [...threads, newThread]);
        this.threadsBySpace = newMap;
        // Track this topic so the SSE reload is suppressed
        this._recentlyCreatedTopicIds.add(newThread.id);
        const timer = setTimeout(() => {
          this._recentlyCreatedTopicIds.delete(newThread.id);
          this._recentlyCreatedTimeouts.delete(newThread.id);
        }, 5000);
        this._recentlyCreatedTimeouts.set(newThread.id, timer);
        // If creating in a group, move the thread into it.
        if (targetGroupId && newThread.id) {
          await this.moveThreadToGroup(newThread.id, targetGroupId, projectId);
        }
        // Auto-select the new thread
        this.creatingThread = '';
        this._createThreadGroupId = null;
        this.handleThreadClick(newThread, projectId);
        return;
      }
    } catch {
      // Non-critical
    }
    this.creatingThread = '';
    this._createThreadGroupId = null;
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
  // Emoji picker
  // ---------------------------------------------------------------------------

  private openEmojiPicker(projectId: string): void {
    this.emojiPickerSpaceId = projectId;
  }

  private closeEmojiPicker(): void {
    this.emojiPickerSpaceId = null;
  }

  private async selectEmoji(emoji: string): Promise<void> {
    const projectId = this.emojiPickerSpaceId;
    if (!projectId) return;

    // Optimistic update.
    this.spaces = this.spaces.map((s) => {
      if (s.projectId !== projectId) return s;
      const updated = { ...s };
      if (emoji) {
        updated.emoji = emoji;
      } else {
        delete updated.emoji;
      }
      return updated;
    });
    this.emojiPickerSpaceId = null;

    try {
      const res = await apiFetch(`/api/v1/chat/spaces/${encodeURIComponent(projectId)}/emoji`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ emoji }),
      });
      if (!res.ok) {
        showToast('Failed to update emoji', 'warning');
        await this.reload();
      }
    } catch {
      showToast('Failed to update emoji', 'warning');
      await this.reload();
    }
  }

  /** Compute last-activity time for a unified rail item (thread or group). */
  private getItemLastActivity(
    item: { kind: 'thread'; thread: ChatSpaceThread } | { kind: 'group'; group: ThreadGroup },
    threadMap: Map<string, ChatSpaceThread>
  ): number {
    if (item.kind === 'thread') {
      return item.thread.lastActivityAt ? new Date(item.thread.lastActivityAt).getTime() : 0;
    }
    let maxTime = 0;
    for (const id of item.group.threadIds) {
      const t = threadMap.get(id);
      if (t?.lastActivityAt) {
        const time = new Date(t.lastActivityAt).getTime();
        if (time > maxTime) maxTime = time;
      }
    }
    return maxTime;
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    return html`
      <div class="rail-header"><span>Projects</span></div>

      ${
        this.loading
          ? html`<div class="loading-state"><sl-spinner></sl-spinner></div>`
          : html`
              ${this.renderToolbar()}
              <div class="rail-body" @click=${this.handleRailBodyClick}>${this.renderSpaces()}</div>
            `
      }
      ${this.contextMenuTarget ? this.renderContextMenu() : nothing}
      ${this.groupContextMenuTarget ? this.renderGroupContextMenu() : nothing}
      ${this.emojiPickerSpaceId ? this.renderEmojiPicker() : nothing}
    `;
  }

  /** Render the filter + sort toolbar below the rail header. */
  private renderToolbar() {
    return html`
      <div class="rail-toolbar">
        <div class="filter-toggle">
          <button
            class=${this.spaceFilter === 'all' ? 'active' : ''}
            @click=${() => this.setSpaceFilter('all')}
          >
            All
          </button>
          <button
            class=${this.spaceFilter === 'unread' ? 'active' : ''}
            @click=${() => this.setSpaceFilter('unread')}
          >
            <sl-icon name="envelope"></sl-icon>
            Unread
          </button>
        </div>
        <sl-dropdown>
          <sl-icon-button
            slot="trigger"
            name="sort-down"
            class="sort-btn"
            label="Sort spaces"
          ></sl-icon-button>
          <sl-menu @sl-select=${this.handleSortSelect}>
            <sl-menu-label>Sort spaces</sl-menu-label>
            <sl-menu-item
              type="checkbox"
              value="activity"
              ?checked=${this.prefs.spaceSortMode === 'activity'}
            >
              Recent activity
            </sl-menu-item>
            <sl-menu-item
              type="checkbox"
              value="alpha"
              ?checked=${this.prefs.spaceSortMode === 'alpha'}
            >
              Alphabetical
            </sl-menu-item>
            <sl-menu-item
              type="checkbox"
              value="custom"
              ?checked=${this.prefs.spaceSortMode === 'custom'}
            >
              Custom
            </sl-menu-item>
          </sl-menu>
        </sl-dropdown>
      </div>
    `;
  }

  /** Set space filter and persist to localStorage. */
  private setSpaceFilter(filter: 'all' | 'unread'): void {
    if (this.spaceFilter === filter) return;
    this.spaceFilter = filter;
    if (filter === 'all') {
      localStorage.removeItem('scion-chat-space-filter');
    } else {
      localStorage.setItem('scion-chat-space-filter', filter);
    }
  }

  /** Handle sort mode selection from the dropdown. */
  private handleSortSelect(e: Event): void {
    const detail = (e as CustomEvent<{ item?: HTMLElement }>).detail;
    const item = detail?.item;
    const value = item?.getAttribute('value');

    // Space sort modes
    if (value === 'activity' || value === 'alpha') {
      void this.savePrefs({ spaceSortMode: value });
      return;
    }
    if (value === 'custom') {
      void this.savePrefs({
        spaceSortMode: 'custom',
        spaceOrder: this.prefs.spaceOrder?.length
          ? this.prefs.spaceOrder
          : this.getSortedSpaces().map((s) => s.projectId),
      });
    }
  }

  /** Get filtered spaces based on current filter. */
  private getFilteredSpaces(): ChatSpace[] {
    const sorted = this.getSortedSpaces();
    if (this.spaceFilter === 'unread') {
      return sorted.filter((s) => s.unreadCount > 0 || s.hasUnreadMention);
    }
    return sorted;
  }

  private renderSpaces() {
    const filtered = this.getFilteredSpaces();
    if (filtered.length === 0) {
      if (this.spaceFilter === 'unread') {
        return html`<div class="loading-state" style="font-size: var(--chat-fs-md)">
          All caught up!
        </div>`;
      }
      return html`<div class="loading-state" style="font-size: var(--chat-fs-md)">
        No spaces available
      </div>`;
    }
    return filtered.map((space) => this.renderSpace(space));
  }

  private renderSpace(space: ChatSpace) {
    const isCollapsed = this.collapsedSpaces.has(space.projectId);
    const threads = this.getSortedThreads(space.projectId);

    return html`
      <div class="space-section">
        <div
          class="space-header ${this.draggingSpaceId === space.projectId ? 'dragging' : ''} ${
            this.dragOverSpaceId === space.projectId && this.draggingSpaceId !== space.projectId
              ? 'drag-over'
              : ''
          }"
          draggable="true"
          @dragstart=${(e: DragEvent): void => this.handleSpaceDragStart(e, space.projectId)}
          @dragover=${(e: DragEvent): void => this.handleSpaceDragOver(e, space.projectId)}
          @drop=${(e: DragEvent): void => void this.handleSpaceDrop(e, space.projectId)}
          @dragend=${(): void => this.handleSpaceDragEnd()}
          @click=${() =>
            isCollapsed
              ? this.handleCollapsedSpaceClick(space)
              : this.handleSpaceHeaderClick(space)}
        >
          <sl-icon name="chevron-down" class="chevron ${isCollapsed ? 'collapsed' : ''}"></sl-icon>
          ${space.emoji ? html`<span class="space-emoji">${space.emoji}</span>` : nothing}
          <span class="space-name">${space.projectName}</span>
          <div class="space-actions" @click=${(e: Event) => e.stopPropagation()}>
            ${
              space.hasUnreadMention
                ? html`<span class="mention-badge">@</span>`
                : space.unreadCount > 0
                  ? html`<span class="unread-badge">${space.unreadCount}</span>`
                  : nothing
            }
            <sl-dropdown>
              <sl-icon-button
                slot="trigger"
                name="three-dots-vertical"
                label="Space actions"
              ></sl-icon-button>
              <sl-menu
                @sl-select=${(e: Event) => {
                  const detail = (e as CustomEvent<{ item?: HTMLElement }>).detail;
                  const value = detail?.item?.getAttribute('value');
                  if (value === 'new-thread') {
                    this.startCreateThread(space.projectId);
                  } else if (value === 'new-group') {
                    this.startGroupNameInput(space.projectId, {});
                  } else if (value === 'set-emoji') {
                    this.openEmojiPicker(space.projectId);
                  } else if (value === 'move-up') {
                    void this.moveSpace(space.projectId, -1);
                  } else if (value === 'move-down') {
                    void this.moveSpace(space.projectId, 1);
                  }
                }}
              >
                <sl-menu-item value="new-thread">
                  <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                  New thread
                </sl-menu-item>
                <sl-menu-item value="new-group">
                  <sl-icon slot="prefix" name="folder-plus"></sl-icon>
                  New thread group
                </sl-menu-item>
                <sl-menu-item value="set-emoji">
                  <sl-icon slot="prefix" name="emoji-smile"></sl-icon>
                  Set emoji
                </sl-menu-item>
                <sl-menu-item
                  class="move-up"
                  value="move-up"
                  ?disabled=${this.isSpaceAtEdge(space.projectId, 'first')}
                >
                  <sl-icon slot="prefix" name="arrow-up"></sl-icon>
                  Move up
                </sl-menu-item>
                <sl-menu-item
                  class="move-down"
                  value="move-down"
                  ?disabled=${this.isSpaceAtEdge(space.projectId, 'last')}
                >
                  <sl-icon slot="prefix" name="arrow-down"></sl-icon>
                  Move down
                </sl-menu-item>
              </sl-menu>
            </sl-dropdown>
          </div>
        </div>
        ${
          !isCollapsed
            ? html`
                <div class="thread-list">
                  ${this.renderThreadList(threads, space.projectId)}
                  ${
                    this.creatingThread === space.projectId
                      ? this.renderCreateThread(space.projectId)
                      : nothing
                  }
                </div>
              `
            : nothing
        }
      </div>
    `;
  }

  /**
   * Render the thread list for a space. Groups and ungrouped threads are
   * co-mingled in a single ordered list — no separate "THREADS" heading.
   */
  private renderThreadList(threads: ChatSpaceThread[], projectId: string) {
    const groups = this.getGroups(projectId);
    if (groups.length === 0) {
      // No groups — render flat list, but still show group-name input if active
      return html`
        ${threads.map((t) => this.renderThread(t, projectId))}
        ${this.groupNameInput?.projectId === projectId ? this.renderGroupNameInput() : nothing}
      `;
    }

    // Build lookups
    const threadMap = new Map(threads.map((t) => [t.id, t]));
    const groupedThreadIds = new Set(groups.flatMap((g) => g.threadIds));

    // #general threads always come first
    const generalThreads = threads.filter((t) => t.isGeneral);

    // Build unified item list: ungrouped non-general threads + groups
    type RailItem =
      | { kind: 'thread'; id: string; thread: ChatSpaceThread }
      | { kind: 'group'; id: string; group: ThreadGroup };

    const items: RailItem[] = [];
    for (const t of threads) {
      if (t.isGeneral || groupedThreadIds.has(t.id)) continue;
      items.push({ kind: 'thread', id: t.id, thread: t });
    }
    for (const g of groups) {
      items.push({ kind: 'group', id: g.id, group: g });
    }

    // Sort items based on current mode
    if (this.prefs.threadSortMode === 'custom') {
      const order = this.prefs.threadOrder?.[projectId] ?? [];
      const orderMap = new Map(order.map((id, i) => [id, i]));
      items.sort((a, b) => {
        const ai = orderMap.get(a.id);
        const bi = orderMap.get(b.id);
        if (ai === undefined && bi === undefined) return 0;
        if (ai === undefined) return 1;
        if (bi === undefined) return -1;
        return ai - bi;
      });
    } else {
      // Activity or alpha — pinned ungrouped threads surface first.
      items.sort((a, b) => {
        const aPinned = a.kind === 'thread' && a.thread.pinned ? 0 : 1;
        const bPinned = b.kind === 'thread' && b.thread.pinned ? 0 : 1;
        if (aPinned !== bPinned) return aPinned - bPinned;

        if (this.prefs.threadSortMode === 'alpha') {
          const aName = a.kind === 'thread' ? a.thread.name : a.group.name;
          const bName = b.kind === 'thread' ? b.thread.name : b.group.name;
          return aName.localeCompare(bName);
        }
        // activity (default)
        const aTime = this.getItemLastActivity(a, threadMap);
        const bTime = this.getItemLastActivity(b, threadMap);
        return bTime - aTime;
      });
    }

    return html`
      ${generalThreads.map((t) => this.renderThread(t, projectId))}
      ${items.map((item) => {
        if (item.kind === 'thread') {
          return this.renderThread(item.thread, projectId);
        }
        const group = item.group;
        const groupThreads = group.threadIds
          .map((id) => threadMap.get(id))
          .filter((t): t is ChatSpaceThread => t !== undefined);
        const collapsed = this.collapsedGroups.has(group.id);
        return html`
          <div
            class="thread-group-header ${this.dragOverGroupId === group.id ? 'drag-over' : ''}"
            @click=${() => this.toggleGroupCollapse(group.id)}
            @dragover=${(e: DragEvent) => this.handleGroupDragOver(e, group.id)}
            @drop=${(e: DragEvent) => void this.handleGroupDrop(e, group.id, projectId)}
            @contextmenu=${(e: MouseEvent) => {
              e.preventDefault();
              e.stopPropagation();
              this.contextMenuTarget = null;
              this.showGroupContextMenu(e, group, projectId);
            }}
          >
            <sl-icon name="chevron-down" class="chevron ${collapsed ? 'collapsed' : ''}"></sl-icon>
            <span class="group-name">${group.name}</span>
            <span class="group-count">(${groupThreads.length})</span>
          </div>
          ${
            !collapsed
              ? html`<div class="thread-group">
                  ${groupThreads.map((t) => this.renderThread(t, projectId))}
                </div>`
              : nothing
          }
        `;
      })}
      ${this.groupNameInput?.projectId === projectId ? this.renderGroupNameInput() : nothing}
    `;
  }

  /** Render inline input for creating or renaming a group. */
  private renderGroupNameInput() {
    if (!this.groupNameInput) return nothing;
    return html`
      <div class="group-name-input">
        <sl-input
          size="small"
          placeholder="Group name"
          .value=${this.groupNameInput.value}
          @sl-input=${(e: Event) => {
            if (this.groupNameInput) {
              this.groupNameInput = {
                ...this.groupNameInput,
                value: (e.target as HTMLInputElement).value,
              };
            }
          }}
          @keydown=${(e: KeyboardEvent) => {
            if (e.key === 'Enter') {
              e.preventDefault();
              void this.submitGroupNameInput();
            }
            if (e.key === 'Escape') {
              this.groupNameInput = null;
            }
          }}
          @sl-blur=${() => void this.submitGroupNameInput()}
        ></sl-input>
      </div>
    `;
  }

  /** Show a context menu for a group header. Reuses the same context-menu position mechanism. */
  @state() private groupContextMenuTarget: {
    group: ThreadGroup;
    projectId: string;
  } | null = null;

  private showGroupContextMenu(e: MouseEvent, group: ThreadGroup, projectId: string): void {
    this.groupContextMenuTarget = { group, projectId };
    this.contextMenuPos = { x: e.clientX, y: e.clientY };
  }

  /**
   * Render one thread row. The trailing badge is a single choice, not two: a
   * muted thread shows the bell and deliberately does not advertise its unread
   * state with a dot, so mute is the first branch of one chain.
   */
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

    // Thread is draggable when in custom sort mode (or any mode, since dragging
    // auto-switches to custom), but not for the #general thread.
    const isDraggable = !thread.isGeneral;
    const isDragging = this.draggingThreadId === thread.id;
    const isDragOver = this.dragOverThreadId === thread.id && this.draggingThreadId !== thread.id;

    return html`
      <div
        class="thread-item ${isSelected ? 'selected' : ''} ${
          isDragging ? 'dragging' : ''
        } ${isDragOver ? 'drag-over' : ''}"
        draggable=${isDraggable ? 'true' : nothing}
        @dragstart=${
          isDraggable ? (e: DragEvent) => this.handleThreadDragStart(e, thread.id) : nothing
        }
        @dragover=${
          isDraggable ? (e: DragEvent) => this.handleThreadDragOver(e, thread.id) : nothing
        }
        @drop=${
          isDraggable
            ? (e: DragEvent) => void this.handleThreadDrop(e, thread.id, projectId)
            : nothing
        }
        @dragend=${isDraggable ? () => this.handleThreadDragEnd() : nothing}
        @click=${() => this.handleThreadClick(thread, projectId)}
        @contextmenu=${(e: MouseEvent) => this.handleContextMenu(e, thread, projectId)}
      >
        <span class="hash">#</span>
        <span class="thread-name ${!thread.muted && thread.hasUnread ? 'unread' : ''}"
          >${thread.name}</span
        >
        ${thread.pinned ? html`<sl-icon name="star-fill" class="pin-icon"></sl-icon>` : nothing}
        ${
          thread.muted
            ? html`<sl-icon name="bell-slash" class="mute-icon" title="Muted"></sl-icon>`
            : thread.hasUnreadMention
              ? html`<span class="mention-dot"></span>`
              : thread.hasUnread
                ? html`<span class="unread-dot"></span>`
                : nothing
        }
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

  /** Curated emoji set for the space emoji picker. */
  private static readonly EMOJI_LIST = [
    '🚀',
    '⚡',
    '🔥',
    '⭐',
    '💡',
    '🎯',
    '🔧',
    '⚙️',
    '🌐',
    '🛡️',
    '📦',
    '🧪',
    '🔬',
    '📊',
    '📈',
    '🏗️',
    '🎨',
    '✨',
    '💎',
    '🔑',
    '🏠',
    '📚',
    '🗂️',
    '💬',
    '🤖',
    '🧠',
    '🎮',
    '🌱',
    '🌊',
    '☁️',
    '🔒',
    '📡',
    '🎵',
    '❤️',
    '🐛',
    '🦊',
    '🐍',
    '🦀',
    '🐳',
    '🦅',
  ];

  private renderEmojiPicker() {
    const space = this.spaces.find((s) => s.projectId === this.emojiPickerSpaceId);
    if (!space) return nothing;

    return html`
      <sl-dialog
        label="Set emoji for ${space.projectName}"
        open
        @sl-after-hide=${() => this.closeEmojiPicker()}
      >
        <div class="emoji-grid">
          ${ScionChatSpaceRail.EMOJI_LIST.map(
            (emoji) => html`
              <button @click=${() => void this.selectEmoji(emoji)} title=${emoji}>${emoji}</button>
            `
          )}
        </div>
        <sl-button
          slot="footer"
          variant="text"
          size="small"
          @click=${() => void this.selectEmoji('')}
          >Remove emoji</sl-button
        >
        <sl-button
          slot="footer"
          variant="default"
          size="small"
          @click=${() => this.closeEmojiPicker()}
          >Cancel</sl-button
        >
      </sl-dialog>
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
        <div
          class="context-menu-item pin-toggle"
          @click=${(): void => void this.handleTogglePin(thread, projectId)}
        >
          <!-- The glyph reports the current state, the label offers the
               action — the filled star means pinned everywhere else in this
               rail, and a menu that used it for "will be pinned" would make
               the row indicator ambiguous. -->
          <sl-icon name=${thread.pinned ? 'star-fill' : 'star'}></sl-icon>
          ${thread.pinned ? 'Unpin' : 'Pin to top'}
        </div>
        <div
          class="context-menu-item mute-toggle"
          @click=${(): void => void this.handleToggleMute(thread, projectId)}
        >
          <sl-icon name=${thread.muted ? 'bell-slash' : 'bell'}></sl-icon>
          ${thread.muted ? 'Unmute' : 'Mute'}
        </div>
        ${
          !thread.isGeneral
            ? html`
                <div
                  class="context-menu-item"
                  @click=${() => void this.moveThread(thread.id, projectId, -1)}
                  style="${
                    this.isThreadAtEdge(thread.id, projectId, 'first')
                      ? 'opacity: 0.4; pointer-events: none;'
                      : ''
                  }"
                >
                  <sl-icon name="arrow-up"></sl-icon>
                  Move up
                </div>
                <div
                  class="context-menu-item"
                  @click=${() => void this.moveThread(thread.id, projectId, 1)}
                  style="${
                    this.isThreadAtEdge(thread.id, projectId, 'last')
                      ? 'opacity: 0.4; pointer-events: none;'
                      : ''
                  }"
                >
                  <sl-icon name="arrow-down"></sl-icon>
                  Move down
                </div>
              `
            : nothing
        }
        ${
          !thread.isGeneral
            ? html`
                ${this.getGroups(projectId)
                  .filter((g) => !g.threadIds.includes(thread.id))
                  .map(
                    (group) => html`
                      <div
                        class="context-menu-item"
                        @click=${() => {
                          this.contextMenuTarget = null;
                          void this.moveThreadToGroup(thread.id, group.id, projectId);
                        }}
                      >
                        <sl-icon name="folder"></sl-icon>
                        Move to ${group.name}
                      </div>
                    `
                  )}
                ${
                  this.getGroups(projectId).some((g) => g.threadIds.includes(thread.id))
                    ? html`
                        <div
                          class="context-menu-item"
                          @click=${() => {
                            this.contextMenuTarget = null;
                            void this.removeThreadFromGroup(thread.id, projectId);
                          }}
                        >
                          <sl-icon name="folder-minus"></sl-icon>
                          Remove from group
                        </div>
                      `
                    : nothing
                }
              `
            : nothing
        }
        <div class="context-menu-item" @click=${() => this.handleExportThread(thread)}>
          <sl-icon name="file-earmark-text"></sl-icon>
          Copy as Markdown
        </div>
        <div class="context-menu-item" @click=${() => this.handleDownloadThread(thread)}>
          <sl-icon name="download"></sl-icon>
          Download as Markdown
        </div>
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
      </div>
    `;
  }

  /** Render context menu for a group header. */
  private renderGroupContextMenu() {
    if (!this.groupContextMenuTarget) return nothing;
    const { group, projectId } = this.groupContextMenuTarget;

    return html`
      <div
        class="context-menu"
        style="left: ${this.contextMenuPos.x}px; top: ${this.contextMenuPos.y}px"
        @click=${(e: Event) => e.stopPropagation()}
      >
        <div
          class="context-menu-item"
          @click=${() => {
            this.groupContextMenuTarget = null;
            this._createThreadGroupId = group.id;
            this.startCreateThread(projectId);
          }}
        >
          <sl-icon name="plus-lg"></sl-icon>
          New thread
        </div>
        <div
          class="context-menu-item"
          @click=${() => {
            this.groupContextMenuTarget = null;
            this.startGroupNameInput(projectId, {
              renamingGroupId: group.id,
              initialValue: group.name,
            });
          }}
        >
          <sl-icon name="pencil"></sl-icon>
          Rename group
        </div>
        <div
          class="context-menu-item danger"
          @click=${() => {
            this.groupContextMenuTarget = null;
            void this.deleteGroup(group.id, projectId);
          }}
        >
          <sl-icon name="trash"></sl-icon>
          Delete group
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-space-rail': ScionChatSpaceRail;
  }
}
