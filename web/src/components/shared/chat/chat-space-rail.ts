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
import './chat-avatar.js';

/** A space (project) in the rail. */
export interface ChatSpace {
  projectId: string;
  projectName: string;
  projectSlug: string;
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

/** User preferences for rail display. */
interface RailPrefs {
  spaceSortMode: 'activity' | 'alpha' | 'custom';
  threadSortMode: 'activity' | 'alpha';
  spaceOrder: string[] | undefined;
}

/**
 * Read a prefs payload from the server. `spaceOrder` arrives either as a real
 * array or as a JSON array string, so both are accepted; a hand-edited or
 * truncated value has to degrade to "no custom order" rather than throwing the
 * rail's whole load away. Non-string entries are dropped either way.
 */
function parseRailPrefs(payload: unknown): RailPrefs {
  const data = (payload ?? {}) as {
    spaceSortMode?: string;
    threadSortMode?: string;
    spaceOrder?: unknown;
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
  const spaceSortMode = data.spaceSortMode;
  const threadSortMode = data.threadSortMode;
  return {
    spaceSortMode:
      spaceSortMode === 'alpha' || spaceSortMode === 'custom' ? spaceSortMode : 'activity',
    threadSortMode: threadSortMode === 'alpha' ? 'alpha' : 'activity',
    spaceOrder,
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
      padding: 0.75rem 1rem 0.5rem;
      font-size: 0.6875rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
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

    .space-actions sl-menu {
      min-width: 120px;
      padding: 0.125rem 0;
    }

    .space-actions sl-menu-item::part(base) {
      font-size: 0.75rem;
      padding: 0.25rem 0.5rem;
    }

    .space-actions sl-menu-item::part(label) {
      font-size: 0.75rem;
    }

    .space-actions sl-menu-item sl-icon {
      font-size: 0.75rem;
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

    .thread-item .mute-icon {
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
      font-size: 0.6875rem;
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
      font-size: 0.6875rem;
    }

    .sort-btn {
      flex-shrink: 0;
    }

    .sort-btn::part(base) {
      font-size: 0.75rem;
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
      font-size: 0.8125rem;
      min-height: 1.5rem;
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

  /**
   * Move a space one slot up (-1) or down (+1) in the displayed order. This is
   * the keyboard-reachable half of reordering: drag-and-drop cannot be done
   * without a pointer, and a rail only reorderable by mouse is not reorderable
   * for everyone.
   */
  private async moveSpace(projectId: string, delta: -1 | 1): Promise<void> {
    if (!this.canReorderSpaces()) return;
    const order = this.currentSpaceOrder();
    const from = order.indexOf(projectId);
    const to = from + delta;
    if (from === -1 || to < 0 || to >= order.length) return;
    const next = [...order];
    const [moved] = next.splice(from, 1);
    next.splice(to, 0, moved);
    await this.applySpaceOrder(next);
  }

  /**
   * Reordering is only offered on the unfiltered list. The order that gets
   * persisted is the global one, so "Move up" against a filtered view would
   * swap the space with a neighbour the user cannot see — either appearing to
   * do nothing, or quietly writing an arrangement they never chose. Reordering
   * against the visible list instead would be worse: the same click would mean
   * different things depending on a filter elsewhere in the rail.
   */
  private canReorderSpaces(): boolean {
    return this.spaceFilter === 'all';
  }

  /**
   * True when the reorder item should be disabled: the space is already at the
   * given end of the displayed order, or reordering is off altogether.
   */
  private isSpaceAtEdge(projectId: string, edge: 'first' | 'last'): boolean {
    if (!this.canReorderSpaces()) return true;
    const order = this.currentSpaceOrder();
    const index = order.indexOf(projectId);
    if (index === -1) return true;
    return edge === 'first' ? index === 0 : index === order.length - 1;
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

  /** Clicking empty area of the rail body resets to global view. */
  private handleRailBodyClick(e: MouseEvent): void {
    // Only fire when the click target is the rail-body itself (empty space)
    const target = e.target as HTMLElement;
    if (target === e.currentTarget) {
      this.dispatchEvent(
        new CustomEvent('reset-view', { bubbles: true, composed: true })
      );
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
    const general = threads.find((t) => t.isGeneral);
    if (general) {
      this.handleThreadClick(general, space.projectId);
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
  private async fetchThreadMessages(threadId: string): Promise<Array<{
    sender_name?: string;
    sender?: string;
    body?: string;
    content?: string;
    created_at?: string;
    timestamp?: string;
    attachments?: string[];
  }>> {
    try {
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(threadId)}/messages`
      );
      if (!res.ok) return [];
      const data = (await res.json()) as { messages?: unknown[] };
      return (data.messages ?? []) as Array<{
        sender_name?: string;
        sender?: string;
        body?: string;
        content?: string;
        created_at?: string;
        timestamp?: string;
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
      sender_name?: string;
      sender?: string;
      body?: string;
      content?: string;
      created_at?: string;
      timestamp?: string;
      attachments?: string[];
    }>
  ): string {
    const lines: string[] = [];
    lines.push(`# Thread: ${thread.name}`);
    lines.push(`Exported: ${new Date().toLocaleString()}`);
    lines.push('');
    lines.push('---');

    for (const msg of messages) {
      const sender = msg.sender_name ?? msg.sender ?? 'Unknown';
      const ts = msg.created_at ?? msg.timestamp ?? '';
      const content = msg.body ?? msg.content ?? '';
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
    if (thread.isGeneral) return;
    const confirmed = await showConfirm(`Delete #${thread.name}? This cannot be undone.`, {
      title: 'Delete Thread',
      confirmText: 'Delete',
      variant: 'danger',
    });
    if (!confirmed) return;
    try {
      await apiFetch(`/api/v1/chat/topics/${encodeURIComponent(thread.id)}`, {
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

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    return html`
      <div class="rail-header"><span>Project Spaces</span></div>

      ${this.loading
        ? html`<div class="loading-state"><sl-spinner></sl-spinner></div>`
        : html`
            ${this.renderToolbar()}
            <div class="rail-body" @click=${this.handleRailBodyClick}>${this.renderSpaces()}</div>
          `}
      ${this.contextMenuTarget ? this.renderContextMenu() : nothing}
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
    if (value === 'activity' || value === 'alpha') {
      void this.savePrefs({ spaceSortMode: value });
      return;
    }
    if (value === 'custom') {
      // Switching to custom with no order saved yet freezes the order the user
      // is currently looking at, so the list does not jump on the click.
      void this.savePrefs({
        spaceSortMode: 'custom',
        // An empty saved order counts as "no order saved yet", so test length
        // rather than nullishness — `[]` would otherwise freeze nothing.
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
        return html`<div class="loading-state" style="font-size: 0.8125rem">All caught up!</div>`;
      }
      return html`<div class="loading-state" style="font-size: 0.8125rem">
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
          class="space-header ${this.draggingSpaceId === space.projectId ? 'dragging' : ''} ${this
            .dragOverSpaceId === space.projectId && this.draggingSpaceId !== space.projectId
            ? 'drag-over'
            : ''}"
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
          <span class="space-name">${space.projectName}</span>
          <div class="space-actions" @click=${(e: Event) => e.stopPropagation()}>
            ${space.hasUnreadMention
              ? html`<span class="mention-badge">@</span>`
              : space.unreadCount > 0
                ? html`<span class="unread-badge">${space.unreadCount}</span>`
                : nothing}
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
                <sl-divider></sl-divider>
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

    return html`
      <div
        class="thread-item ${isSelected ? 'selected' : ''}"
        @click=${() => this.handleThreadClick(thread, projectId)}
        @contextmenu=${(e: MouseEvent) => this.handleContextMenu(e, thread, projectId)}
      >
        <span class="hash">#</span>
        <span class="thread-name ${!thread.muted && thread.hasUnread ? 'unread' : ''}"
          >${thread.name}</span
        >
        ${thread.pinned ? html`<sl-icon name="star-fill" class="pin-icon"></sl-icon>` : nothing}
        ${thread.muted
          ? html`<sl-icon name="bell-slash" class="mute-icon" title="Muted"></sl-icon>`
          : thread.hasUnreadMention
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
        <div
          class="context-menu-item"
          @click=${() => this.handleExportThread(thread)}
        >
          <sl-icon name="file-earmark-text"></sl-icon>
          Copy as Markdown
        </div>
        <div
          class="context-menu-item"
          @click=${() => this.handleDownloadThread(thread)}
        >
          <sl-icon name="download"></sl-icon>
          Download as Markdown
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
