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
import { chatNotifications } from '../../../client/chat-notifications.js';
import { chatUnread } from '../../../client/chat-unread.js';
import {
  CHAT_STARTUP_REUSE_MS,
  chatLoadClock,
  chatSpacesLoad,
} from '../../../client/chat-list-cache.js';
import { showConfirm } from '../confirm-dialog.js';
import { showToast } from '../../../utils/toast.js';
import { formatInstantWithZone } from '../../../utils/time.js';
import { touchMenuItemStyles } from '../touch-styles.js';
import { TouchPrimaryController } from '../../../utils/input-modality.js';
import { LongPressController, type LongPressPoint } from './long-press.js';
import {
  placeMenuInViewport,
  renderMenuRows,
  runMenuAction,
  shouldUseMenuSheet,
  type MenuAction,
} from './context-menu.js';
import type { ActionSheetSelectDetail } from './chat-action-sheet.js';
import './chat-action-sheet.js';
import './chat-avatar.js';

/** A space (project) in the rail. */
export interface ChatSpace {
  projectId: string;
  projectName: string;
  projectSlug: string;
  emoji?: string;
  unreadCount: number;
  hasUnreadMention: boolean;
  /**
   * Newest visible-thread activity in the space, when the server sends it
   * (omitted for a space with no threads). Lets activity sort order spaces
   * without loading every space's thread list.
   */
  lastActivityAt?: string;
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

/**
 * A top-level rail entry for a space: either an ungrouped, non-general
 * thread, or a thread group (rendered as one row, with its members nested
 * underneath). Shared between `renderThreadList` and the order-computing
 * helpers (`currentTopLevelOrder` and friends) so a drag/nudge snapshot is
 * built from exactly the same items the user is looking at.
 */
type RailItem =
  | { kind: 'thread'; id: string; thread: ChatSpaceThread }
  | { kind: 'group'; id: string; group: ThreadGroup };

/**
 * User preferences for rail display. `threadSortMode` is only ever the
 * user's alpha/recent choice for threads — a space counts as explicitly
 * ordered when and only when `threadOrder[projectId]` is non-empty; there is
 * no separate "custom" thread mode.
 */
interface RailPrefs {
  spaceSortMode: 'activity' | 'alpha' | 'custom';
  threadSortMode: 'activity' | 'alpha';
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

/**
 * localStorage key for persisted thread-group collapse state (#1698 follow-up).
 *
 * Group IDs (`generateGroupId`) mix `Math.random()` with a timestamp, so they
 * are already unique across every space, not just within one — a flat set of
 * IDs is enough and there is no need to nest the key under a project/space
 * id.
 */
const GROUP_COLLAPSE_STORAGE_KEY = 'scion-chat-group-collapse';

/**
 * Hard cap on remembered collapsed-group entries. Stale entries (for groups
 * that were since deleted) are pruned opportunistically once the rail knows
 * the current set of groups, but this cap is the backstop in case pruning
 * never runs for a given session.
 */
const GROUP_COLLAPSE_STORAGE_LIMIT = 500;

/**
 * The storage key, scoped to a user when one is known. `currentUserId` comes
 * from the page's session data (see `ScionChatSpaceRail.currentUserId`) and
 * is normally available by the time this is first read — but the fallback to
 * the unscoped key (rather than, say, refusing to persist) means a moment
 * without a known user degrades to "shared across whoever's signed in",
 * not to losing the feature.
 */
function collapseStorageKey(userId: string): string {
  return userId ? `${GROUP_COLLAPSE_STORAGE_KEY}:${userId}` : GROUP_COLLAPSE_STORAGE_KEY;
}

/**
 * Read the persisted set of collapsed thread-group IDs. Missing storage,
 * unavailable storage (private browsing), a JSON parse error, or an
 * unexpected shape all fall back to an empty set silently — collapse state
 * is a nicety, not something that should ever block the rail from loading.
 */
function loadCollapsedGroupIds(userId: string): Set<string> {
  try {
    const raw = localStorage.getItem(collapseStorageKey(userId));
    if (!raw) return new Set();
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return new Set();
    return new Set(parsed.filter((id): id is string => typeof id === 'string'));
  } catch {
    return new Set();
  }
}

/**
 * Persist the given set of collapsed group IDs, bounded to the most
 * recently added entries (`Set` preserves insertion order, so re-collapsing
 * an ID that's already in the set doesn't move it — only a fresh add, after
 * an expand or on first collapse, does). Failures (storage unavailable or
 * full) are swallowed for the same reason the read side falls back silently.
 */
function saveCollapsedGroupIds(userId: string, ids: Set<string>): void {
  try {
    let list = [...ids];
    if (list.length > GROUP_COLLAPSE_STORAGE_LIMIT) {
      list = list.slice(list.length - GROUP_COLLAPSE_STORAGE_LIMIT);
    }
    localStorage.setItem(collapseStorageKey(userId), JSON.stringify(list));
  } catch {
    // Storage unavailable (private browsing) or full — collapse state just
    // won't survive a reload this time.
  }
}

/** Parse a user-prefs payload. */
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
  const resolvedSpaceSortMode: RailPrefs['spaceSortMode'] =
    spaceSortMode === 'alpha' || spaceSortMode === 'custom' ? spaceSortMode : 'activity';

  // A stored 'custom' thread mode maps to alpha when spaces sort alpha,
  // otherwise activity; threadOrder is kept. A stored 'activity' mode also
  // maps to alpha when spaces sort alpha. This build always writes the two
  // together, so only data from other clients takes this path.
  let resolvedThreadSortMode: RailPrefs['threadSortMode'];
  if (threadSortMode === 'custom') {
    resolvedThreadSortMode = resolvedSpaceSortMode === 'alpha' ? 'alpha' : 'activity';
  } else {
    resolvedThreadSortMode =
      threadSortMode === 'alpha' || resolvedSpaceSortMode === 'alpha' ? 'alpha' : 'activity';
  }

  return {
    spaceSortMode: resolvedSpaceSortMode,
    threadSortMode: resolvedThreadSortMode,
    spaceOrder,
    threadOrder,
    threadGroups,
  };
}

/** Viewport width at or below which the chat panels are separate screens. */
const MOBILE_BREAKPOINT_PX = 768;

/**
 * Concurrent background thread loads, for the spaces activity sort needs
 * but the user cannot see. Low, so they stay behind the visible spaces and
 * the conversation's own requests.
 */
const BACKGROUND_THREAD_LOADS = 2;

/** Automatic retries of a failed thread load, per space and reload. */
const THREAD_LOAD_AUTO_RETRIES = 1;

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

  /**
   * The signed-in user's ID, passed down by the chat page from its session
   * data. Used only to scope the thread-group collapse localStorage key so
   * switching accounts in the same browser doesn't prune one account's
   * collapse state because it looks stale to the other's.
   */
  @property()
  currentUserId = '';

  /**
   * The space of the open thread, from the route ('' for a DM or nothing
   * open). Its threads load first and it is expanded, before its thread
   * list is known — the rail no longer loads every space's threads to find
   * the selected one.
   */
  @property()
  selectedProjectId = '';

  @state() private spaces: ChatSpace[] = [];
  @state() private threadsBySpace = new Map<string, ChatSpaceThread[]>();
  /** Spaces whose thread list is being fetched (drives the per-space loading state). */
  @state() private loadingThreads = new Set<string>();
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
  /** The open thread or group menu is the mobile bottom sheet, not the popup. */
  @state() private menuAsSheet = false;
  private readonly longPress = new LongPressController(this);
  /** Rows are draggable only for a mouse or trackpad: on touch, a long-press opens the menu. */
  private readonly touchPrimary = new TouchPrimaryController(this);
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
  /**
   * The user's real, persisted collapse preference. This is the only thing
   * `saveCollapsedGroupIds`/`pruneCollapsedGroups` ever write — the deep-link
   * auto-expand below must never add to or remove from it, or the override
   * leaks into storage the next time anything else saves (round-2 review,
   * R2).
   */
  @state() private collapsedGroups = new Set<string>();
  /**
   * The `currentUserId` that `collapsedGroups` was last restored from
   * storage for. `null` before the first restore. Compared against
   * `currentUserId` directly (not against Lit's `changedProperties`) in
   * `willUpdate`, so it's a single check that covers the normal case
   * (already set before connect), a late-arriving ID, and a live switch
   * from one user to another — see `willUpdate` (round-4 review, N7).
   */
  private _collapseLoadedFor: string | null = null;
  /**
   * Transient, render-only override: the one group forced open because it
   * contains the selected/deep-linked thread, even though the user's real
   * preference for it (in `collapsedGroups`) is collapsed. Never persisted.
   * See `maybeAutoExpandGroupForSelectedKey`.
   */
  @state() private autoExpandedGroupId: string | null = null;
  /**
   * The `selectedKey` that `autoExpandedGroupId` was last computed for.
   * `loadData` runs on every SSE-triggered reload with the *same* selected
   * thread, and recomputing the override each time would silently pop the
   * group back open right after the user collapses it — the override must
   * be decided once per distinct thread selection, not once per reload
   * (round-2 review, R3).
   */
  private _autoExpandComputedForKey: string | null = null;
  /**
   * Whether `loadPrefs` has completed successfully at least once. On a cold
   * deep link, `selectedKey` is set as an attribute before `connectedCallback`
   * (chat.ts binds it that way), so the very first `updated()` fires and
   * would otherwise lock in "no override" while `prefs.threadGroups` is
   * simply not loaded yet — not "this user has no groups". Gating on this
   * flag, rather than on `threadGroups` being defined, avoids that false
   * signal (round-3 review, R4).
   */
  private _prefsLoaded = false;
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
    ${touchMenuItemStyles}

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
      overscroll-behavior: contain;
      /* Set by the chat page's mobile panels; see chat.ts. */
      touch-action: var(--chat-touch-action, auto);
      padding: 0.25rem 0;
      /* The last row clears the home indicator (the page uses
         viewport-fit=cover); the inset is 0 elsewhere. */
      padding-bottom: max(0.25rem, env(safe-area-inset-bottom, 0px));
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

    /* Context menu. It renders hidden and is shown once placed in the viewport. */
    .context-menu {
      visibility: hidden;
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

    /* Text for assistive technology only. */
    .sr-only {
      position: absolute;
      width: 1px;
      height: 1px;
      padding: 0;
      margin: -1px;
      overflow: hidden;
      clip: rect(0, 0, 0, 0);
      white-space: nowrap;
      border: 0;
    }

    /* One space's thread list, still loading. */
    .threads-loading {
      display: flex;
      justify-content: center;
      padding: 0.5rem;
      font-size: var(--chat-fs-md, 14px);
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

    /* Stop iOS/Android focus-zoom on the rail's Shoelace inputs, whose
       local ::part(base) font-size overrides bypass the app-wide
       --sl-input-font-size-* variable, independent of layout density (an
       iPad is coarse-pointer but wider than the mobile breakpoint, so it
       still needs this). */
    @media (pointer: coarse) {
      .group-name-input sl-input::part(base),
      .create-thread sl-input::part(base),
      .rename-input::part(base) {
        font-size: max(16px, var(--chat-fs-md));
      }
    }

    /* Long-press opens the row menu on touch: keep iOS's callout and text
       selection from taking the press first. */
    @media (hover: none) {
      .thread-item,
      .thread-group-header {
        -webkit-touch-callout: none;
        -webkit-user-select: none;
        user-select: none;
      }
    }

    @media (max-width: 768px) {
      /* Beyond comfy: real-device feedback asked for rail text bigger than
         the comfy token set gives, not just comfy-forced-on-mobile. */
      .rail-header {
        font-size: 18px;
      }

      .thread-item {
        font-size: 17px;
        min-height: 48px;
      }

      .thread-item .unread-dot,
      .thread-item .mention-dot {
        width: 8px;
        height: 8px;
      }

      .space-header {
        font-size: 14px;
        min-height: 44px;
      }

      .space-header .unread-badge,
      .space-header .mention-badge {
        font-size: 12px;
        min-width: 1.25rem;
        padding: 0.125rem 0.375rem;
      }

      /* A real 44px-tall button, not a ::before-expanded hit area: the
         rounded segmented border on .filter-toggle needs overflow: hidden,
         which clips any pseudo-element that tries to extend past the
         container's own edge — an invisible hit area here would never
         actually be reachable. */
      .filter-toggle button {
        font-size: 15px;
        min-height: 44px;
      }

      .sort-btn::part(base) {
        width: 44px;
        height: 44px;
      }

      .space-actions sl-icon-button::part(base) {
        width: 44px;
        height: 44px;
      }

      .space-actions sl-menu-item::part(base) {
        font-size: 16px;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    // Restore persisted filter/sort from localStorage
    const savedFilter = localStorage.getItem('scion-chat-space-filter');
    if (savedFilter === 'unread') this.spaceFilter = 'unread';
    // Collapsed thread-groups are restored in willUpdate, not here — see its
    // doc comment (round-4 review, N7).
    void this.loadData();
    // Close context menu on outside click
    this._outsideClickHandler = this.handleOutsideClick.bind(this);
    document.addEventListener('click', this._outsideClickHandler);
  }

  override willUpdate(_changedProperties: Map<string, unknown>): void {
    // Single home for restoring collapsedGroups, run before every render so
    // there is no expand-then-collapse flash. Comparing `currentUserId`
    // against `_collapseLoadedFor` directly — rather than inspecting Lit's
    // changedProperties old/new pair — covers every way the ID can arrive
    // in one check: already set before connect (the normal chat.ts case,
    // where the very first willUpdate sees `currentUserId !== null` and
    // restores), a late-arriving ID after connect, and even a live switch
    // from one signed-in user to another. An old version of this gated on
    // `changedProperties.get('currentUserId') === ''`, which only caught
    // the "was never set, now is" case and missed a same-tick post-append
    // set (the old value reads as `undefined`, not `''`) and any u1-to-u2
    // switch (round-4 review, N7).
    if (this.currentUserId !== this._collapseLoadedFor) {
      this._collapseLoadedFor = this.currentUserId;
      this.collapsedGroups = loadCollapsedGroupIds(this.currentUserId);
    }
    // Expanding a space is asking for its threads: one whose last load
    // failed (and is out of automatic retries) tries again.
    const wasCollapsed = _changedProperties.get('collapsedSpaces') as Set<string> | undefined;
    if (wasCollapsed) {
      for (const id of wasCollapsed) {
        // A load already in flight (the header click's own) is not a retry.
        if (this.collapsedSpaces.has(id) || this.threadsBySpace.has(id)) continue;
        if (this.loadingThreads.has(id)) continue;
        if (this._threadLoadFailures.has(id)) {
          this._threadLoadFailures.delete(id);
          this._threadLoadEpoch.delete(id);
        }
      }
    }
    // Start whatever thread loads the state now calls for: a space expanded
    // or selected since the last render, or the next background load. Run
    // here, before render, so the loading state lands in the same render.
    this.syncThreadLoads();
  }

  override updated(changedProperties: Map<string, unknown>): void {
    if ((this.contextMenuTarget || this.groupContextMenuTarget) && !this.menuAsSheet) {
      placeMenuInViewport(
        this.renderRoot.querySelector<HTMLElement>('.context-menu'),
        this.contextMenuPos
      );
    }
    if (changedProperties.has('selectedProjectId') && this.selectedProjectId) {
      this.expandSpaceForSelectedKey();
    }
    if (changedProperties.has('selectedKey')) {
      if (this.selectedKey) {
        // Auto-expand the space and group containing the selected thread
        // (deep-link support).
        this.expandSpaceForSelectedKey();
        this.maybeAutoExpandGroupForSelectedKey();
      } else {
        // Nothing selected — no group should be forced open on its behalf.
        this.autoExpandedGroupId = null;
        this._autoExpandComputedForKey = null;
      }
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
    // The route names the space: expand it without waiting for its threads.
    // Only once the space list has it, so a reload's "collapse brand-new
    // spaces" pass cannot fold it straight back up.
    const routed = this.selectedProjectId;
    if (routed && this.spaces.some((s) => s.projectId === routed)) {
      if (this.collapsedSpaces.has(routed)) {
        const next = new Set(this.collapsedSpaces);
        next.delete(routed);
        this.collapsedSpaces = next;
      }
      return;
    }
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

  /**
   * Decide, once per distinct `selectedKey`, whether the group containing
   * the selected thread needs a transient auto-expand override (deep-link
   * support): before persisted collapse existed, a group could never hide
   * the active thread — it always started expanded. Now a reload can land
   * on a thread inside a group the user left collapsed, so it is forced
   * open for this view via `autoExpandedGroupId` — never by touching
   * `collapsedGroups`, which stays exactly what the user last set it to.
   *
   * Idempotent per key: `loadData` calls this on every reload (SSE messages,
   * topic changes, etc.), and re-deciding it each time would force the group
   * back open right after the user collapses it, seconds later, with no way
   * to keep it shut while the thread stays open (round-2 review, R3).
   *
   * Does not record a decision until `_prefsLoaded` is true — see that
   * field's doc comment for why a cold deep link would otherwise lock in
   * "no override" before the groups are even known (round-3 review, R4).
   */
  private maybeAutoExpandGroupForSelectedKey(): void {
    if (!this._prefsLoaded) return;
    if (this._autoExpandComputedForKey === this.selectedKey) return;
    this._autoExpandComputedForKey = this.selectedKey;
    this.autoExpandedGroupId = null;
    if (!this.selectedKey || this.collapsedGroups.size === 0) return;
    const allGroups = Object.values(this.prefs.threadGroups ?? {}).flat();
    for (const group of allGroups) {
      if (group.threadIds.includes(this.selectedKey) && this.collapsedGroups.has(group.id)) {
        this.autoExpandedGroupId = group.id;
        return;
      }
    }
  }

  /**
   * Clear the auto-expand override if the group it names has since stopped
   * containing the selected thread — e.g. the thread was moved to a
   * different group, or the group itself was deleted, server-side. Called
   * only after a successful `loadPrefs`, so "no longer contains" reflects
   * the server's current state rather than a stale or failed fetch.
   *
   * Deliberately does *not* pick a new group for the thread's new location:
   * `maybeAutoExpandGroupForSelectedKey` already declined to reconsider
   * once `selectedKey` is set (that's the whole R3 fix), and re-picking
   * here would reopen a group the user may have collapsed in the meantime.
   * The stale group just stops being forced open — the user's real
   * preference for every group, old and new, is left exactly as it was
   * (round-3 review, N5).
   */
  private clearStaleAutoExpand(): void {
    if (!this.autoExpandedGroupId) return;
    const allGroups = Object.values(this.prefs.threadGroups ?? {}).flat();
    const group = allGroups.find((g) => g.id === this.autoExpandedGroupId);
    if (!group || !group.threadIds.includes(this.selectedKey)) {
      this.autoExpandedGroupId = null;
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
    // Nothing can show these lists now; stop the requests rather than let
    // them run to completion for a detached rail.
    for (const controller of this._threadAborts.values()) controller.abort();
    this._threadAborts.clear();
    this._threadLoads.clear();
    this._threadLoadEpoch.clear();
    this._pendingReadState.clear();
    if (this.loadingThreads.size > 0) this.loadingThreads = new Set();
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

  /**
   * Reload all data (called externally when SSE events indicate changes).
   * `startedAfter` — when the triggering event was delivered, on the
   * shared loads' clock — lets the spaces load share a request another
   * owner made after that event; by default only a request started after
   * the call will do.
   */
  async reload(options: { startedAfter?: number } = {}): Promise<void> {
    await this.loadData(options.startedAfter);
  }

  /**
   * A space's thread list, loading it first if the rail has not. Lets the
   * page pick a space's #general on a deep link without a second request
   * for the list the rail is about to show.
   */
  async threadsFor(projectId: string): Promise<ChatSpaceThread[]> {
    if (!this.threadsBySpace.has(projectId)) await this.ensureThreads(projectId);
    return this.threadsBySpace.get(projectId) ?? [];
  }

  /** Returns the list of space project IDs for SSE subscription. */
  getSpaceIds(): string[] {
    return this.spaces.map((s) => s.projectId);
  }

  /**
   * Load the spaces and preferences, coalescing overlapping calls: a call
   * made while a load is running does not start a second, concurrent one —
   * it queues exactly one trailing load (the running one may have read the
   * server before whatever prompted the call) and shares its promise.
   */
  private loadData(startedAfter: number = chatLoadClock()): Promise<void> {
    if (this._loadInFlight) {
      this._reloadQueued = true;
      // The trailing pass must answer every call it absorbs.
      this._queuedStartedAfter = Math.max(this._queuedStartedAfter, startedAfter);
      return this._loadInFlight;
    }
    const run = async (): Promise<void> => {
      let after = startedAfter;
      try {
        do {
          this._reloadQueued = false;
          this._queuedStartedAfter = -Infinity;
          await this.loadDataOnce(after);
          after = this._queuedStartedAfter;
        } while (this._reloadQueued && this.isConnected);
      } finally {
        this._loadInFlight = null;
      }
    };
    this._loadInFlight = run();
    return this._loadInFlight;
  }

  /** The running {@link loadData} pass, if any. */
  private _loadInFlight: Promise<void> | null = null;
  /** A {@link loadData} call arrived during the running pass; run once more after it. */
  private _reloadQueued = false;
  /** The newest `startedAfter` among the calls the trailing pass answers. */
  private _queuedStartedAfter = -Infinity;

  private async loadDataOnce(startedAfter: number): Promise<void> {
    // Only show the full-page spinner on the very first load. Subsequent
    // reloads (e.g. SSE-triggered) update data in-place without a flash.
    const initial = !this._initialLoadDone;
    if (initial) {
      this.loading = true;
    }
    try {
      const [spacesOk, prefsOk] = await Promise.all([
        this.loadSpaces(initial, startedAfter),
        this.loadPrefs(),
      ]);
      // Pruning reads both this.spaces and this.prefs.threadGroups to decide
      // what's stale, so it must not run unless both loaded cleanly in this
      // pass — see pruneCollapsedGroups' doc comment for what goes wrong
      // otherwise.
      if (spacesOk && prefsOk) {
        this.pruneCollapsedGroups();
      }
      if (prefsOk) {
        this.clearStaleAutoExpand();
      }
      if (this.selectedKey) {
        this.maybeAutoExpandGroupForSelectedKey();
      }
    } finally {
      // Names and rollup badges render now; thread lists follow per space
      // (see syncThreadLoads), so a slow thread list no longer holds the
      // whole rail behind a spinner.
      this._listsReady = true;
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

  /**
   * Loads spaces; returns whether the load succeeded (used to gate pruning).
   * The first load shares a startup request another owner already made; a
   * reload follows a change and only shares a request that started
   * after `startedAfter` (see {@link reload}).
   */
  private async loadSpaces(initial: boolean, startedAfter: number): Promise<boolean> {
    const loading = chatSpacesLoad.load(
      initial ? { maxAgeMs: CHAT_STARTUP_REUSE_MS } : { startedAfter }
    );
    // When the request this answer comes from was sent (it may be shared).
    const requestedAt = chatSpacesLoad.startedAt() ?? chatLoadClock();
    const data = await loading;
    if (!data) return false;
    this._spacesStartedAt = requestedAt;
    this.spaces = (data.spaces ?? []) as ChatSpace[];
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
    // On a reload, every thread list loaded so far predates whatever
    // prompted it: the ones on screen refetch on the next render, the rest
    // when next expanded (see syncThreadLoads). Not on the first load — a
    // list fetched before it (a space deep link asking `threadsFor` early)
    // is as fresh as this response and is kept.
    if (!initial) {
      this._threadsEpoch++;
      this._threadLoadFailures.clear();
    }
    // Expand the space holding the selected thread — the deep-link case on
    // the first load, and on every reload too (as before lazy loading), so a
    // reload keeps the open thread's space open.
    if (this.selectedKey || this.selectedProjectId) {
      this.expandSpaceForSelectedKey();
    }
    return true;
  }

  /** When the request behind the current spaces list was sent (`chatLoadClock`). */
  private _spacesStartedAt = -Infinity;
  /** Set once the first spaces/prefs pass has finished; thread loads wait for it. */
  private _listsReady = false;
  /** Bumped by every spaces load: a thread list fetched in an older epoch is stale. */
  private _threadsEpoch = 0;
  /** The {@link _threadsEpoch} in which each space's latest thread load started. */
  private _threadLoadEpoch = new Map<string, number>();
  /** In-flight thread loads, aborted when superseded or when the rail detaches. */
  private _threadAborts = new Map<string, AbortController>();
  /** The latest thread load per space (settled ones stay until replaced). */
  private _threadLoads = new Map<string, Promise<void>>();

  /**
   * Spaces whose thread list is needed now: expanded ones, plus the one
   * holding the open thread (it may be collapsed by the user and still
   * need its threads for the unread filter and selection).
   */
  private visibleThreadSpaceIds(): Set<string> {
    const ids = new Set<string>();
    for (const space of this.spaces) {
      if (!this.collapsedSpaces.has(space.projectId)) ids.add(space.projectId);
    }
    if (this.selectedProjectId) ids.add(this.selectedProjectId);
    return ids;
  }

  /**
   * Whether space order needs every space's thread list: activity sort,
   * with a server that does not report per-space activity. Then the
   * remaining spaces load in the background, behind the visible ones.
   */
  private needsBackgroundThreadLoads(): boolean {
    const mode = this.prefs.spaceSortMode;
    if (mode === 'alpha' || mode === 'custom') return false;
    return !this.spaces.some((s) => s.lastActivityAt !== undefined);
  }

  /**
   * Whether the first round of background thread loads has finished (each
   * listed space attempted, none still loading). Latches: later reloads
   * refetch in the background too, and the rail keeps its activity order
   * through them instead of falling back to the server's order each time.
   */
  private activityOrderSettled(): boolean {
    if (this._activityOrderSettled) return true;
    const pending =
      this.loadingThreads.size > 0 ||
      this.spaces.some(
        (s) =>
          this._knownSpaceIds.has(s.projectId) &&
          (this._threadLoadEpoch.get(s.projectId) ?? -1) < this._threadsEpoch
      );
    if (!pending) this._activityOrderSettled = true;
    return this._activityOrderSettled;
  }

  /** See {@link activityOrderSettled}. */
  private _activityOrderSettled = false;

  /**
   * Start the thread loads the current state calls for. Visible spaces go
   * first and all at once; with {@link needsBackgroundThreadLoads}, the rest
   * follow at most {@link BACKGROUND_THREAD_LOADS} at a time — each finished
   * load re-renders, which calls this again for the next one.
   */
  private syncThreadLoads(): void {
    if (!this._listsReady || !this.isConnected) return;
    // A space with a load in flight is left alone even if a reload made it
    // stale: aborting it would let a busy hub (a reload every message
    // burst, slower list responses) restart it forever, so it never lands.
    // It lands, re-renders, and this runs again to fetch it once more.
    const needsLoad = (id: string): boolean =>
      !this.loadingThreads.has(id) && (this._threadLoadEpoch.get(id) ?? -1) < this._threadsEpoch;
    // Only spaces the last spaces load returned: a thread list is fetched
    // for a space the server listed, never for one that has since gone.
    const known = this._knownSpaceIds;
    const visible = this.visibleThreadSpaceIds();
    for (const id of visible) {
      if (known.has(id) && needsLoad(id)) void this.loadThreads(id);
    }
    if (!this.needsBackgroundThreadLoads()) return;
    let running = [...this.loadingThreads].filter((id) => !visible.has(id)).length;
    for (const space of this.spaces) {
      if (running >= BACKGROUND_THREAD_LOADS) break;
      const id = space.projectId;
      if (visible.has(id) || !known.has(id) || !needsLoad(id)) continue;
      void this.loadThreads(id);
      running++;
    }
  }

  /**
   * Load one space's thread list. Supersedes (aborts) an earlier load of
   * the same space — only explicit callers do that (creating a thread in an
   * unloaded space); {@link syncThreadLoads} never starts a load for a space
   * that has one in flight. Resolves when this load settles.
   */
  private loadThreads(projectId: string): Promise<void> {
    this._threadAborts.get(projectId)?.abort();
    const controller = new AbortController();
    this._threadAborts.set(projectId, controller);
    this._threadLoadEpoch.set(projectId, this._threadsEpoch);
    const startedAt = chatLoadClock();
    if (!this.loadingThreads.has(projectId)) {
      this.loadingThreads = new Set(this.loadingThreads).add(projectId);
    }
    const done = (): void => {
      if (this._threadAborts.get(projectId) !== controller) return;
      this._threadAborts.delete(projectId);
      const next = new Set(this.loadingThreads);
      next.delete(projectId);
      this.loadingThreads = next;
      if (next.size === 0) this.settlePendingReadState();
    };
    const fail = (): void => {
      // Retry automatically once per epoch (the next render's sync picks it
      // up); after that, only an explicit expand or header click, or the
      // next reload, tries again — a hub that keeps failing is not polled.
      const failures = (this._threadLoadFailures.get(projectId) ?? 0) + 1;
      this._threadLoadFailures.set(projectId, failures);
      if (failures <= THREAD_LOAD_AUTO_RETRIES) this._threadLoadEpoch.delete(projectId);
    };
    const load = (async (): Promise<void> => {
      try {
        const res = await apiFetch(`/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`, {
          signal: controller.signal,
        });
        if (!res.ok) {
          if (!controller.signal.aborted) fail();
          return;
        }
        const data = (await res.json()) as { threads?: ChatSpaceThread[] };
        if (controller.signal.aborted) return;
        this._threadLoadFailures.delete(projectId);
        const threads = data.threads || [];
        const newMap = new Map(this.threadsBySpace);
        newMap.set(projectId, threads);
        this.threadsBySpace = newMap;
        this.applyPendingReadState(projectId, threads, startedAt);
        // A thread deep link without a routed space resolves here.
        if (this.selectedKey && !this.selectedProjectId) this.expandSpaceForSelectedKey();
      } catch {
        // A network failure, or an abort (by detach or an explicit newer
        // load), which is not a failure of this space.
        if (!controller.signal.aborted) fail();
      } finally {
        done();
      }
    })();
    this._threadLoads.set(projectId, load);
    return load;
  }

  /** Consecutive failed thread loads per space, reset by a success or a reload. */
  private _threadLoadFailures = new Map<string, number>();

  /**
   * Make sure a space's threads are loaded, and wait for that. Used where
   * an action needs the list — opening #general from a collapsed space, a
   * space deep link. A load already in flight is joined rather than
   * restarted; with none and no list (never loaded, or the last attempt
   * failed), this is an explicit request and loads regardless of retries.
   */
  private async ensureThreads(projectId: string): Promise<void> {
    if (!this.loadingThreads.has(projectId)) {
      const stale = (this._threadLoadEpoch.get(projectId) ?? -1) < this._threadsEpoch;
      if (stale || !this.threadsBySpace.has(projectId)) void this.loadThreads(projectId);
    }
    // A load can be superseded while awaited; wait for whichever is current.
    let pending = this._threadLoads.get(projectId);
    while (pending) {
      await pending;
      const next = this._threadLoads.get(projectId);
      if (next === pending) break;
      pending = next;
    }
  }

  /**
   * Read-state changes (thread id → change and when it happened) for
   * threads no loaded list held while some list was still loading — the
   * usual case being the thread just opened, whose space list is still on
   * its way when the thread view advances its read watermark.
   */
  private _pendingReadState = new Map<string, { kind: 'read' | 'unread'; at: number }>();

  /**
   * Apply pending read-state changes for threads in a list that just
   * landed. A list requested before the change still shows the old state,
   * so the change is applied to it the normal way (row and badge). A list
   * requested after it already shows the new state; then only the space
   * badge can be behind, if the spaces list predates the change too.
   *
   * `at` is when the read-state event arrived, after the read POST returned,
   * so a spaces request sent while the POST was in flight counts as older
   * even if the server already rolled the read in; the badge can then drop
   * one too many (floored at zero). The window is narrow and main has the
   * same race class.
   */
  private applyPendingReadState(
    projectId: string,
    threads: ChatSpaceThread[],
    listStartedAt: number
  ): void {
    if (this._pendingReadState.size === 0) return;
    let rollupStale = false;
    for (const thread of threads) {
      const pending = this._pendingReadState.get(thread.id);
      if (!pending) continue;
      this._pendingReadState.delete(thread.id);
      const listIsOlder = listStartedAt < pending.at;
      const rollupIsOlder = this._spacesStartedAt < pending.at;
      if (listIsOlder && rollupIsOlder) {
        // Both predate the change: apply it the normal way, row and badge.
        if (pending.kind === 'read') this.markThreadRead(thread.id);
        else this.markThreadUnread(thread.id);
      } else if (listIsOlder) {
        // The badge already reflects it; only the row is behind.
        this.updateThread(
          projectId,
          thread.id,
          pending.kind === 'read'
            ? { hasUnread: false, hasUnreadMention: false }
            : { hasUnread: true }
        );
      } else if (rollupIsOlder) {
        rollupStale = true;
      }
    }
    if (rollupStale) void this.reload();
  }

  /**
   * Once nothing is loading, any pending change whose thread turned up in
   * no list (its space failed to load, say) falls back to refreshing the
   * rollup from the server.
   */
  private settlePendingReadState(): void {
    if (this._pendingReadState.size === 0) return;
    this._pendingReadState.clear();
    if (this.spaces.some((s) => !this.threadsBySpace.has(s.projectId))) void this.reload();
  }

  /** Loads prefs; returns whether the load succeeded (used to gate pruning). */
  private async loadPrefs(): Promise<boolean> {
    try {
      const res = await apiFetch('/api/v1/chat/user-prefs');
      if (res.ok) {
        this.prefs = parseRailPrefs(await res.json());
        this._prefsLoaded = true;
        return true;
      }
      return false;
    } catch {
      // Use defaults
      return false;
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
        // Without per-space activity from the server, the order is derived
        // from thread lists that load in the background. Keep the server's
        // order until they have all landed once, rather than reshuffling
        // the rail as each one arrives.
        if (this.needsBackgroundThreadLoads() && !this.activityOrderSettled()) break;
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

  /**
   * A space is explicitly ordered iff it has a non-empty `threadOrder`
   * snapshot — independent of `threadSortMode`, which only ever holds the
   * alpha/recent choice.
   */
  private hasExplicitOrder(projectId: string): boolean {
    return !!this.prefs.threadOrder?.[projectId]?.length;
  }

  /**
   * Case-insensitive, locale-aware name comparison with an id tiebreak —
   * shared by every alpha-sort call site (threads, groups, and the mixed
   * top-level item list) so "Alphabetical" collates identically everywhere.
   */
  private compareByName(aName: string, bName: string, aId: string, bId: string): number {
    const byName = aName.localeCompare(bName, undefined, { sensitivity: 'base' });
    return byName !== 0 ? byName : aId.localeCompare(bId);
  }

  /**
   * Newest-first activity comparison with an id tiebreak — shared by every
   * activity-sort call site.
   */
  private compareByActivity(aTime: number, bTime: number, aId: string, bId: string): number {
    return aTime !== bTime ? bTime - aTime : aId.localeCompare(bId);
  }

  /**
   * Comparator for an explicit per-space snapshot: items present in `order`
   * sort by position; items missing from it sort after, keeping their
   * relative order. Shared by `sortTopLevelItems` and `getSortedThreads`.
   */
  private compareByExplicitOrder(
    order: string[]
  ): (a: { id: string }, b: { id: string }) => number {
    const orderMap = new Map(order.map((id, i) => [id, i]));
    return (a, b) => {
      const ai = orderMap.get(a.id);
      const bi = orderMap.get(b.id);
      if (ai === undefined && bi === undefined) return 0;
      if (ai === undefined) return 1;
      if (bi === undefined) return -1;
      return ai - bi;
    };
  }

  /**
   * A group's members resolved against known threads, sorted under the
   * active mode unless this space has an explicit snapshot (see
   * `hasExplicitOrder`). Shared by `currentGroupThreadOrder` and
   * `renderThreadList`; `threadMap` is scoped to whichever thread set the
   * caller needs (full space, or unread-filtered for display).
   */
  private orderGroupMembers(
    projectId: string,
    group: ThreadGroup,
    threadMap: Map<string, ChatSpaceThread>
  ): ChatSpaceThread[] {
    const members = group.threadIds
      .map((id) => threadMap.get(id))
      .filter((t): t is ChatSpaceThread => t !== undefined);
    if (!this.hasExplicitOrder(projectId)) {
      const mode = this.prefs.threadSortMode;
      members.sort((a, b) => this.compareThreadsForSort(a, b, mode));
    }
    return members;
  }

  /**
   * A group's member ids in display order: resolved known members first
   * (see `orderGroupMembers`), then any raw ids that didn't resolve to a
   * known thread, appended unchanged rather than pruned. Shared by
   * `currentGroupThreadOrder` and `freezeOtherGroupOrders`.
   */
  private displayedGroupIds(
    projectId: string,
    group: ThreadGroup,
    threadMap: Map<string, ChatSpaceThread>
  ): string[] {
    const known = this.orderGroupMembers(projectId, group, threadMap).map((t) => t.id);
    const knownSet = new Set(known);
    const unknown = group.threadIds.filter((id) => !knownSet.has(id));
    return [...known, ...unknown];
  }

  /** A group's members in the order currently on screen, as ids — see `displayedGroupIds`. */
  private currentGroupThreadOrder(projectId: string, groupId: string): string[] {
    const group = this.getGroups(projectId).find((g) => g.id === groupId);
    if (!group) return [];
    const threadMap = new Map((this.threadsBySpace.get(projectId) ?? []).map((t) => [t.id, t]));
    return this.displayedGroupIds(projectId, group, threadMap);
  }

  /**
   * Every group in a space snapshotted to its displayed order, except
   * `exceptIds` (the group(s) the caller is about to write its own order
   * for). Called whenever a drag/nudge gives a space its first explicit
   * top-level snapshot, so every other group's raw `threadIds` matches what
   * it was displaying rather than resurfacing later.
   */
  private freezeOtherGroupOrders(
    projectId: string,
    groups: ThreadGroup[],
    exceptIds: ReadonlySet<string>
  ): ThreadGroup[] {
    const threadMap = new Map((this.threadsBySpace.get(projectId) ?? []).map((t) => [t.id, t]));
    return groups.map((g) =>
      exceptIds.has(g.id) ? g : { ...g, threadIds: this.displayedGroupIds(projectId, g, threadMap) }
    );
  }

  /** The unordered top-level entries for a space: non-general, ungrouped
   * threads, plus each group as a single entry. */
  private topLevelRailItems(projectId: string, threads: ChatSpaceThread[]): RailItem[] {
    const groups = this.getGroups(projectId);
    const groupedThreadIds = new Set(groups.flatMap((g) => g.threadIds));
    const items: RailItem[] = [];
    for (const t of threads) {
      if (t.isGeneral || groupedThreadIds.has(t.id)) continue;
      items.push({ kind: 'thread', id: t.id, thread: t });
    }
    for (const g of groups) {
      items.push({ kind: 'group', id: g.id, group: g });
    }
    return items;
  }

  /** The name to sort a top-level rail item by, under alpha. */
  private railItemName(item: RailItem): string {
    return item.kind === 'thread' ? item.thread.name : item.group.name;
  }

  /**
   * Sort a space's top-level items: by the explicit snapshot when present
   * (see `hasExplicitOrder`), otherwise pinned threads first, then the
   * active alpha/activity mode. A group has no activity of its own, so under
   * Recent it sorts by its most-recently-active member.
   */
  private sortTopLevelItems(
    projectId: string,
    items: RailItem[],
    threadMap: Map<string, ChatSpaceThread>
  ): RailItem[] {
    const sorted = [...items];
    if (this.hasExplicitOrder(projectId)) {
      const order = this.prefs.threadOrder?.[projectId] ?? [];
      sorted.sort(this.compareByExplicitOrder(order));
      return sorted;
    }

    const mode = this.prefs.threadSortMode;
    sorted.sort((a, b) => {
      const aPinned = a.kind === 'thread' && a.thread.pinned ? 0 : 1;
      const bPinned = b.kind === 'thread' && b.thread.pinned ? 0 : 1;
      if (aPinned !== bPinned) return aPinned - bPinned;

      if (mode === 'alpha') {
        return this.compareByName(this.railItemName(a), this.railItemName(b), a.id, b.id);
      }
      const aTime = this.getItemLastActivity(a, threadMap);
      const bTime = this.getItemLastActivity(b, threadMap);
      return this.compareByActivity(aTime, bTime, a.id, b.id);
    });
    return sorted;
  }

  /** A space's top-level display order, as ids (ungrouped threads plus groups, each counted once). */
  private currentTopLevelOrder(projectId: string): string[] {
    const threads = this.threadsBySpace.get(projectId) ?? [];
    const threadMap = new Map(threads.map((t) => [t.id, t]));
    const items = this.topLevelRailItems(projectId, threads);
    return this.sortTopLevelItems(projectId, items, threadMap).map((i) => i.id);
  }

  /** `order` with `id` moved to sit immediately before `anchorId`, or at the end if absent. */
  private insertAdjacent(order: string[], id: string, anchorId: string): string[] {
    const result = order.filter((x) => x !== id);
    const idx = result.indexOf(anchorId);
    result.splice(idx === -1 ? result.length : idx, 0, id);
    return result;
  }

  /**
   * Writes a space's top-level order and every group's member order in one
   * save, always freezing groups (see `freezeOtherGroupOrders`) so an
   * untouched group's raw order can't resurface later. Leaves
   * `threadSortMode` untouched — only `threadOrder`'s presence makes a space
   * explicit (see `hasExplicitOrder`).
   */
  private async saveThreadOrder(
    projectId: string,
    topLevelOrder?: string[],
    updatedGroups?: ThreadGroup[]
  ): Promise<void> {
    const order = topLevelOrder ?? this.currentTopLevelOrder(projectId);
    const groups =
      updatedGroups ?? this.freezeOtherGroupOrders(projectId, this.getGroups(projectId), new Set());
    await this.savePrefs({
      threadOrder: { ...(this.prefs.threadOrder ?? {}), [projectId]: order },
      threadGroups: { ...(this.prefs.threadGroups ?? {}), [projectId]: groups },
    });
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

    const groups = this.prefs.threadGroups?.[projectId];
    const targetGroup = groups?.find((g) => g.threadIds.includes(targetThreadId));
    const sourceGroup = groups?.find((g) => g.threadIds.includes(sourceId));

    if (groups && (targetGroup || sourceGroup) && targetGroup !== sourceGroup) {
      // Crossing a group boundary: into a group, out of one onto an
      // ungrouped thread, or between two groups. Insertion uses each
      // group's *displayed* order, not the raw `threadIds`, so the thread
      // lands where it was dropped rather than wherever raw-index math
      // happens to put it.
      const touched = new Set(
        [sourceGroup?.id, targetGroup?.id].filter((id): id is string => id !== undefined)
      );
      const updatedGroups = this.freezeOtherGroupOrders(projectId, groups, touched).map((g) => {
        if (g.id === sourceGroup?.id && g.id !== targetGroup?.id) {
          // Rebuild the source group from its displayed order too, same as the target branch below.
          const displayed = this.currentGroupThreadOrder(projectId, g.id);
          return { ...g, threadIds: displayed.filter((id) => id !== sourceId) };
        }
        if (g.id === targetGroup?.id) {
          const displayed = this.currentGroupThreadOrder(projectId, g.id);
          return { ...g, threadIds: this.insertAdjacent(displayed, sourceId, targetThreadId) };
        }
        return g;
      });
      const baseTopLevel = this.currentTopLevelOrder(projectId).filter((id) => id !== sourceId);
      const topLevelOrder = targetGroup
        ? baseTopLevel
        : this.insertAdjacent(baseTopLevel, sourceId, targetThreadId);
      await this.saveThreadOrder(projectId, topLevelOrder, updatedGroups);
      return;
    }

    if (targetGroup) {
      // Same group — reorder within its displayed order.
      const frozen = this.freezeOtherGroupOrders(projectId, groups!, new Set([targetGroup.id]));
      const updatedGroups = frozen.map((g) => {
        if (g.id !== targetGroup.id) return g;
        const displayed = this.currentGroupThreadOrder(projectId, g.id);
        return { ...g, threadIds: this.insertAdjacent(displayed, sourceId, targetThreadId) };
      });
      await this.saveThreadOrder(projectId, undefined, updatedGroups);
      return;
    }

    // Top-level reorder: neither thread is in a group.
    const order = this.insertAdjacent(
      this.currentTopLevelOrder(projectId).filter((id) => id !== sourceId),
      sourceId,
      targetThreadId
    );
    await this.saveThreadOrder(projectId, order);
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
      // Reorder within the group's currently-displayed order (see
      // currentGroupThreadOrder) and save it as this space's explicit
      // snapshot; otherwise the very next render would re-sort the group and
      // silently undo the move.
      const ids = this.currentGroupThreadOrder(projectId, containingGroup.id);
      const idx = ids.indexOf(threadId);
      const swapIdx = idx + delta;
      if (idx < 0 || swapIdx < 0 || swapIdx >= ids.length) return;
      [ids[idx], ids[swapIdx]] = [ids[swapIdx], ids[idx]];

      const frozen = this.freezeOtherGroupOrders(projectId, groups, new Set([containingGroup.id]));
      const updatedGroups = frozen.map((g) =>
        g.id === containingGroup.id ? { ...g, threadIds: ids } : g
      );
      // Snapshot the unchanged top-level order alongside the frozen groups so both are pinned.
      await this.saveThreadOrder(projectId, this.currentTopLevelOrder(projectId), updatedGroups);
    } else {
      // Reorder among top-level items (ungrouped threads, and groups as
      // single units) in their currently-displayed order.
      const order = [...this.currentTopLevelOrder(projectId)];
      const idx = order.indexOf(threadId);
      const swapIdx = idx + delta;
      if (idx < 0 || swapIdx < 0 || swapIdx >= order.length) return;
      [order[idx], order[swapIdx]] = [order[swapIdx], order[idx]];

      await this.saveThreadOrder(projectId, order);
    }
  }

  private isThreadAtEdge(threadId: string, projectId: string, edge: 'first' | 'last'): boolean {
    if (!this.canReorderThreads()) return true;

    const groups = this.prefs.threadGroups?.[projectId] ?? [];
    const containingGroup = groups.find((g) => g.threadIds.includes(threadId));

    if (containingGroup) {
      // Check edges within the group's currently-displayed order.
      const ids = this.currentGroupThreadOrder(projectId, containingGroup.id);
      return edge === 'first' ? ids[0] === threadId : ids[ids.length - 1] === threadId;
    }

    // Check edges among top-level items (ungrouped threads, groups as units).
    const order = this.currentTopLevelOrder(projectId);
    if (order.length === 0) return true;
    const index = order.indexOf(threadId);
    if (index === -1) return true;
    return edge === 'first' ? index === 0 : index === order.length - 1;
  }

  // ---------------------------------------------------------------------------
  // Thread groups
  // ---------------------------------------------------------------------------

  /** Get groups for a space, defaulting to empty array. */
  private getGroups(projectId: string): ThreadGroup[] {
    return this.prefs.threadGroups?.[projectId] ?? [];
  }

  private toggleGroupCollapse(groupId: string): void {
    if (groupId === this.autoExpandedGroupId) {
      // This group is only *visually* expanded via the deep-link override —
      // the user's real preference (in collapsedGroups) already has it
      // collapsed. A click here means "collapse", and clearing the override
      // is the entire action; there's nothing new to save.
      this.autoExpandedGroupId = null;
      return;
    }
    const next = new Set(this.collapsedGroups);
    if (next.has(groupId)) {
      next.delete(groupId);
    } else {
      next.add(groupId);
    }
    this.collapsedGroups = next;
    saveCollapsedGroupIds(this.currentUserId, next);
  }

  /**
   * Drop stored collapse entries for groups that no longer exist (deleted,
   * or belonging to a space the user lost access to).
   *
   * The caller (`loadData`) only invokes this after both `loadSpaces` and
   * `loadPrefs` have succeeded in the same pass — `this.spaces` and
   * `this.prefs.threadGroups` only reflect the *server's* current groups
   * when both loaded cleanly. A failed `loadPrefs` in particular leaves
   * `this.prefs` at its old (possibly still-default, all-undefined) value,
   * which would make every stored ID look stale and wipe it permanently on
   * the very first page load. The `this.spaces.length === 0` check here is
   * an extra guard for the same failure mode, kept as defense in depth.
   */
  private pruneCollapsedGroups(): void {
    if (this.spaces.length === 0 || this.collapsedGroups.size === 0) return;
    const validIds = new Set<string>();
    for (const space of this.spaces) {
      for (const group of this.getGroups(space.projectId)) {
        validIds.add(group.id);
      }
    }
    const next = new Set([...this.collapsedGroups].filter((id) => validIds.has(id)));
    if (next.size === this.collapsedGroups.size) return;
    this.collapsedGroups = next;
    saveCollapsedGroupIds(this.currentUserId, next);
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
    const threadGroups = { ...(this.prefs.threadGroups ?? {}), [projectId]: groups };

    // Only touch threadOrder for a space that already has its own snapshot
    // (see `hasExplicitOrder`) — writing a first, membership-only entry here
    // would itself become the explicit-order signal.
    if (!this.hasExplicitOrder(projectId)) {
      await this.savePrefs({ threadGroups });
      return;
    }

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

    // Without this space's own explicit snapshot (see `hasExplicitOrder`),
    // there is no threadOrder entry to replace, and writing a partial one
    // here would itself become the explicit-order signal.
    if (!this.hasExplicitOrder(projectId)) {
      await this.savePrefs({ threadGroups });
      return;
    }

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
    // The server's per-space figure covers threads not loaded yet; a loaded
    // list can be newer (a local change since the spaces load), so take
    // the later of the two.
    const reported = this.spaces.find((s) => s.projectId === projectId)?.lastActivityAt;
    let maxTime = reported ? new Date(reported).getTime() || 0 : 0;
    for (const t of threads) {
      if (t.lastActivityAt) {
        const time = new Date(t.lastActivityAt).getTime();
        if (time > maxTime) maxTime = time;
      }
    }
    return maxTime;
  }

  /**
   * Compare two threads under the alpha or activity sort mode, via the
   * `compareByName`/`compareByActivity` primitives every alpha/activity sort
   * call site shares, so "Alphabetical"/"Recent" collate identically
   * everywhere.
   */
  private compareThreadsForSort(
    a: ChatSpaceThread,
    b: ChatSpaceThread,
    mode: 'alpha' | 'activity'
  ): number {
    if (mode === 'alpha') {
      return this.compareByName(a.name, b.name, a.id, b.id);
    }
    const aTime = a.lastActivityAt ? new Date(a.lastActivityAt).getTime() : 0;
    const bTime = b.lastActivityAt ? new Date(b.lastActivityAt).getTime() : 0;
    return this.compareByActivity(aTime, bTime, a.id, b.id);
  }

  private getSortedThreads(projectId: string): ChatSpaceThread[] {
    const threads = [...(this.threadsBySpace.get(projectId) || [])];

    // With an explicit snapshot, the user's order takes control. #general is
    // always first regardless.
    if (this.hasExplicitOrder(projectId)) {
      const order = this.prefs.threadOrder?.[projectId] ?? [];
      const general = threads.filter((t) => t.isGeneral);
      const rest = threads.filter((t) => !t.isGeneral);
      rest.sort(this.compareByExplicitOrder(order));
      return [...general, ...rest];
    }

    // Separate #general, pinned, and regular.
    const general = threads.filter((t) => t.isGeneral);
    const pinned = threads.filter((t) => !t.isGeneral && t.pinned);
    const regular = threads.filter((t) => !t.isGeneral && !t.pinned);

    const mode = this.prefs.threadSortMode;
    const sortFn = (a: ChatSpaceThread, b: ChatSpaceThread) =>
      this.compareThreadsForSort(a, b, mode);

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
    void this.openDefaultThread(space.projectId);
  }

  /**
   * Open a space's #general (or first) thread, loading its list first if
   * the space was never expanded. Gives up if the user selected something
   * else or collapsed the space while the list loaded.
   */
  private async openDefaultThread(projectId: string): Promise<void> {
    const keyAtClick = this.selectedKey;
    if (!this.threadsBySpace.has(projectId)) {
      await this.ensureThreads(projectId);
      if (this.selectedKey !== keyAtClick || this.collapsedSpaces.has(projectId)) return;
    }
    const threads = this.threadsBySpace.get(projectId) || [];
    const target = threads.find((t) => t.isGeneral) || threads[0];
    if (target) {
      this.handleThreadClick(target, projectId);
    }
  }

  private handleContextMenu(e: MouseEvent, thread: ChatSpaceThread, projectId: string): void {
    if (this.longPress.contextMenu(e)) return;
    e.preventDefault();
    e.stopPropagation();
    this.openThreadMenu(thread, projectId, { x: e.clientX, y: e.clientY });
  }

  /** Open a thread row's menu: the popup at `at`, or the sheet on mobile. */
  private openThreadMenu(thread: ChatSpaceThread, projectId: string, at: LongPressPoint): void {
    this.groupContextMenuTarget = null;
    this.contextMenuTarget = { type: 'thread', thread, projectId };
    this.contextMenuPos = at;
    this.menuAsSheet = shouldUseMenuSheet();
  }

  /** Close whichever row menu is open, popup or sheet. */
  private closeRowMenus(): void {
    this.contextMenuTarget = null;
    this.groupContextMenuTarget = null;
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
    this.refreshRollupForUnloadedThread(threadId, 'read');
  }

  /**
   * A read-state change for a thread no loaded list holds. Its space's list
   * may simply not have loaded yet (lists load lazily), so the space badge
   * cannot be adjusted locally. While lists are loading, the change waits
   * for the one holding the thread (see `applyPendingReadState`); with none
   * loading, the rollup is refreshed from the server (reloads coalesce).
   * With every list loaded, the thread is in no space the rail shows and
   * there is nothing to fix.
   */
  private refreshRollupForUnloadedThread(threadId: string, kind: 'read' | 'unread'): void {
    for (const threads of this.threadsBySpace.values()) {
      if (threads.some((t) => t.id === threadId)) return;
    }
    if (this.spaces.every((s) => this.threadsBySpace.has(s.projectId))) return;
    if (this.loadingThreads.size > 0) {
      // The list holding it is probably on its way (the thread just opened);
      // apply the change when it lands instead of reloading the whole rail.
      this._pendingReadState.set(threadId, { kind, at: chatLoadClock() });
      return;
    }
    void this.reload();
  }

  /** Nudge a space's unread badge, floored at zero. */
  private adjustSpaceUnread(projectId: string, delta: number): void {
    this.spaces = this.spaces.map((s) =>
      s.projectId === projectId ? { ...s, unreadCount: Math.max(0, s.unreadCount + delta) } : s
    );
  }

  /**
   * Mark a thread unread from the context menu. Hidden/disabled by the
   * render guard for an already-unread or empty thread, but this also
   * no-ops defensively for the same reasons handleMarkRead does.
   */
  private async handleMarkUnread(thread: ChatSpaceThread, _projectId: string): Promise<void> {
    this.contextMenuTarget = null;
    if (thread.hasUnread || !thread.lastMessageId) return;
    try {
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(thread.id)}/unread`,
        { method: 'POST' }
      );
      if (!res.ok) return;
      this.markThreadUnread(thread.id);
      // Same-tab suppression must not wait on the SSE round trip: if this
      // thread is the one currently open, the page needs to know right now,
      // not once its own echo comes back.
      this.dispatchEvent(
        new CustomEvent('conversation-marked-unread', {
          detail: { conversationKey: thread.id },
          bubbles: true,
          composed: true,
        })
      );
    } catch {
      // Non-critical
    }
  }

  /**
   * Set a thread's unread markers locally without talking to the server —
   * the inverse of markThreadRead. Called once the server confirms
   * "Mark unread" here, and by the chat page when another of this user's
   * tabs reports the same change over the read-state SSE event (see
   * chat.ts's _handleOwnReadStateSSE).
   */
  markThreadUnread(threadId: string): void {
    for (const [projectId, threads] of this.threadsBySpace) {
      const target = threads.find((t) => t.id === threadId);
      if (!target || target.hasUnread) continue;
      this.updateThread(projectId, threadId, { hasUnread: true });
      if (!target.muted) this.adjustSpaceUnread(projectId, 1);
      return;
    }
    this.refreshRollupForUnloadedThread(threadId, 'unread');
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
      // Update all threads in this space locally (if its list is loaded —
      // the badge below is the space's own and always updates).
      this.patchThreads(projectId, (threads) =>
        threads.map((t) => ({ ...t, hasUnread: false, hasUnreadMention: false }))
      );
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
      // Muting changes what counts as unread and what may pop up.
      chatNotifications.invalidateConversationInfo();
      chatUnread.scheduleRefresh();
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
    lines.push(`Exported: ${formatInstantWithZone(new Date().toISOString())}`);
    lines.push('');
    lines.push('---');

    for (const msg of messages) {
      const rawSender = msg.sender ?? 'Unknown';
      const sender = rawSender.replace(/^(user|agent):/, '');
      const ts = msg.createdAt ?? '';
      const content = msg.msg ?? '';
      const formattedTs = ts ? formatInstantWithZone(ts) || ts : '';

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
      this.patchThreads(projectId, (threads) => threads.filter((t) => t.id !== thread.id));
    } catch (err) {
      showToast(err instanceof Error ? err.message : 'Failed to delete thread', 'danger');
    }
  }

  /**
   * Open the new-thread name entry for a space. `groupId` is the group the
   * thread is filed into once created; every request sets it, so a target
   * left by an earlier group-menu request cannot carry over.
   */
  private startCreateThread(projectId: string, groupId: string | null = null): void {
    this._createThreadGroupId = groupId;
    // The name-entry row renders inside the space's thread list, so a
    // collapsed space must open for the row to be visible.
    this.expandSpace(projectId);
    // Asking again for the space whose row is already open keeps the typed
    // name; only a fresh entry starts empty.
    if (this.creatingThread !== projectId) {
      this.creatingThread = projectId;
      this.newThreadName = '';
    }
    // Focus on every request, not only when the row first opens, so a repeat
    // New thread brings focus back from the menu that issued it.
    void this.updateComplete.then(() => this.focusCreateThreadInput());
  }

  /**
   * Focus the new-thread name input. Native focus also scrolls the input
   * into view, so no separate scroll is needed.
   */
  private async focusCreateThreadInput(): Promise<void> {
    const input = this.shadowRoot?.querySelector<
      HTMLElement & { updateComplete?: Promise<unknown> }
    >('.create-thread sl-input');
    if (!input) return;
    await input.updateComplete;
    input.focus();
  }

  /** Close the new-thread name entry without creating a thread. */
  private cancelCreateThread(): void {
    this.creatingThread = '';
    this._createThreadGroupId = null;
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
        // Optimistic local state update — no re-fetch needed once the list
        // is loaded. If it is not, appending would publish a one-thread list
        // as the whole space; load it instead (superseding any older load,
        // which may predate the new thread).
        if (this.threadsBySpace.has(projectId)) {
          this.patchThreads(projectId, (threads) => [...threads, newThread]);
        } else {
          void this.loadThreads(projectId);
        }
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
    this.patchThreads(projectId, (threads) =>
      threads.map((t) => (t.id === threadId ? { ...t, ...update } : t))
    );
  }

  /**
   * Rewrite a space's loaded thread list. A space whose list has never
   * loaded is left alone: writing a derived list there would make it look
   * loaded — and empty — so expanding it would skip the fetch and find no
   * #general to open.
   */
  private patchThreads(
    projectId: string,
    update: (threads: ChatSpaceThread[]) => ChatSpaceThread[]
  ): void {
    const threads = this.threadsBySpace.get(projectId);
    if (!threads) return;
    const newMap = new Map(this.threadsBySpace);
    newMap.set(projectId, update(threads));
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
      ${this.contextMenuTarget && !this.menuAsSheet ? this.renderContextMenu() : nothing}
      ${this.groupContextMenuTarget && !this.menuAsSheet ? this.renderGroupContextMenu() : nothing}
      ${this.renderMenuSheet()} ${this.emojiPickerSpaceId ? this.renderEmojiPicker() : nothing}
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
            label="Sort"
          ></sl-icon-button>
          <sl-menu @sl-select=${this.handleSortSelect}>
            <sl-menu-label>Sort</sl-menu-label>
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

  /**
   * Alphabetical/Recent is a single, rail-wide choice: it sets
   * `spaceSortMode` and `threadSortMode` together, and clears every space's
   * `threadOrder` snapshot so the choice actually discards custom thread
   * arrangements (see `hasExplicitOrder`) instead of leaving them to
   * resurface on a later drag. Custom applies to space order only —
   * per-space thread order has its own trigger: dragging or nudging a
   * thread.
   */
  private handleSortSelect(e: Event): void {
    const detail = (e as CustomEvent<{ item?: HTMLElement }>).detail;
    const item = detail?.item;
    const value = item?.getAttribute('value');

    // Space + thread sort modes
    if (value === 'activity' || value === 'alpha') {
      void this.savePrefs({ spaceSortMode: value, threadSortMode: value, threadOrder: {} });
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
      // The space holding the open conversation stays even at zero unread —
      // otherwise reading the last unread thread in a space (including via
      // auto-advance) would make the whole space, open thread and all,
      // vanish out from under the user.
      const openProjectId = this.findProjectIdForSelectedThread();
      return sorted.filter(
        (s) => s.unreadCount > 0 || s.hasUnreadMention || s.projectId === openProjectId
      );
    }
    return sorted;
  }

  /** The space containing the currently selected thread, if any. */
  private findProjectIdForSelectedThread(): string | null {
    if (!this.selectedKey) return null;
    if (this.selectedProjectId) return this.selectedProjectId;
    for (const [projectId, threads] of this.threadsBySpace) {
      if (threads.some((t) => t.id === this.selectedKey)) return projectId;
    }
    return null;
  }

  /**
   * Whether a thread counts as unread for the rail's filter — the same
   * definition the dots and space rollup use: a muted thread never counts,
   * mention or not.
   */
  private isThreadUnreadForFilter(thread: ChatSpaceThread): boolean {
    return !thread.muted && (thread.hasUnread || thread.hasUnreadMention);
  }

  /**
   * Threads to show within a shown space under the current filter. The
   * open conversation is always kept, even once read, so the user reading
   * it (or auto-advance marking it read) does not pull it out from under
   * them; every other thread is held to the unread definition above.
   */
  private getVisibleThreads(threads: ChatSpaceThread[]): ChatSpaceThread[] {
    if (this.spaceFilter !== 'unread') return threads;
    return threads.filter((t) => t.id === this.selectedKey || this.isThreadUnreadForFilter(t));
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
          draggable=${this.touchPrimary.isTouch ? nothing : 'true'}
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
        ${!isCollapsed
          ? html`
              <div
                class="thread-list"
                aria-busy=${this.loadingThreads.has(space.projectId) ? 'true' : 'false'}
              >
                ${this.creatingThread === space.projectId
                  ? this.renderCreateThread(space.projectId)
                  : nothing}
                ${this.loadingThreads.has(space.projectId) &&
                !this.threadsBySpace.has(space.projectId)
                  ? html`<div class="threads-loading" role="status">
                      <sl-spinner aria-hidden="true"></sl-spinner>
                      <span class="sr-only">Loading threads for ${space.projectName}</span>
                    </div>`
                  : this.renderThreadList(threads, space.projectId)}
              </div>
            `
          : nothing}
      </div>
    `;
  }

  /**
   * Render the thread list for a space. Groups and ungrouped threads are
   * co-mingled in a single ordered list — no separate "THREADS" heading.
   */
  private renderThreadList(threads: ChatSpaceThread[], projectId: string) {
    const visibleThreads = this.getVisibleThreads(threads);
    const groups = this.getGroups(projectId);
    if (groups.length === 0) {
      // No groups — render flat list, but still show group-name input if active
      return html`
        ${visibleThreads.map((t) => this.renderThread(t, projectId))}
        ${this.groupNameInput?.projectId === projectId ? this.renderGroupNameInput() : nothing}
      `;
    }

    // Build lookups. threadMap is built from the filtered list, so a group
    // whose threads are all filtered out resolves to zero members below —
    // that is what lets the unread filter hide it without a separate check.
    const threadMap = new Map(visibleThreads.map((t) => [t.id, t]));

    // #general threads always come first
    const generalThreads = visibleThreads.filter((t) => t.isGeneral);

    // Build and sort the unified item list: ungrouped non-general threads,
    // plus each group as a single entry — see `topLevelRailItems` and
    // `sortTopLevelItems`.
    const items = this.sortTopLevelItems(
      projectId,
      this.topLevelRailItems(projectId, visibleThreads),
      threadMap
    );

    return html`
      ${generalThreads.map((t) => this.renderThread(t, projectId))}
      ${items.map((item) => {
        if (item.kind === 'thread') {
          return this.renderThread(item.thread, projectId);
        }
        const group = item.group;
        // Alpha/activity apply within group membership too, unless this
        // space has an explicit snapshot (see `hasExplicitOrder`).
        const groupThreads = this.orderGroupMembers(projectId, group, threadMap);
        // Under the unread filter, a group left with no visible threads
        // (empty, or every member read) is noise — hide it entirely rather
        // than showing a bare "(0)" header.
        if (this.spaceFilter === 'unread' && groupThreads.length === 0) {
          return nothing;
        }
        const collapsed =
          this.collapsedGroups.has(group.id) && group.id !== this.autoExpandedGroupId;
        return html`
          <div
            class="thread-group-header ${this.dragOverGroupId === group.id ? 'drag-over' : ''}"
            @click=${() => this.toggleGroupCollapse(group.id)}
            @dragover=${(e: DragEvent) => this.handleGroupDragOver(e, group.id)}
            @drop=${(e: DragEvent) => void this.handleGroupDrop(e, group.id, projectId)}
            @pointerdown=${(e: PointerEvent): void =>
              this.longPress.pointerDown(e, (at) => this.openGroupMenu(group, projectId, at))}
            @contextmenu=${(e: MouseEvent): void => {
              if (this.longPress.contextMenu(e)) return;
              e.preventDefault();
              e.stopPropagation();
              this.openGroupMenu(group, projectId, { x: e.clientX, y: e.clientY });
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

  private openGroupMenu(group: ThreadGroup, projectId: string, at: LongPressPoint): void {
    this.contextMenuTarget = null;
    this.groupContextMenuTarget = { group, projectId };
    this.contextMenuPos = at;
    this.menuAsSheet = shouldUseMenuSheet();
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

    // Every thread is draggable except #general; dragging gives its space an
    // explicit order regardless of the active sort mode. Not on a touch
    // device, where a press-and-hold opens the thread's menu instead.
    const isDraggable = !thread.isGeneral && !this.touchPrimary.isTouch;
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
        @pointerdown=${(e: PointerEvent): void =>
          this.longPress.pointerDown(e, (at) => this.openThreadMenu(thread, projectId, at))}
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
              this.cancelCreateThread();
            }
          }}
          @sl-blur=${() => {
            if (!this.newThreadName.trim()) this.cancelCreateThread();
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

  /** The actions of a thread row's menu, shared by the popup and the sheet. */
  private threadMenuActions(thread: ChatSpaceThread, projectId: string): MenuAction[] {
    const actions: MenuAction[] = [
      {
        id: 'mark-read',
        label: 'Mark as read',
        icon: 'check-circle',
        run: () => void this.handleMarkRead(thread, projectId),
      },
    ];
    if (!thread.hasUnread && thread.lastMessageId) {
      actions.push({
        id: 'mark-unread',
        label: 'Mark unread',
        icon: 'envelope',
        run: () => void this.handleMarkUnread(thread, projectId),
      });
    }
    actions.push(
      {
        id: 'mark-space-read',
        label: 'Mark space read',
        icon: 'check-lg',
        run: () => void this.handleMarkSpaceRead(projectId),
      },
      {
        id: 'pin',
        // The glyph reports the current state, the label offers the action —
        // the filled star means pinned everywhere else in this rail, and a
        // menu that used it for "will be pinned" would make the row indicator
        // ambiguous.
        label: thread.pinned ? 'Unpin' : 'Pin to top',
        icon: thread.pinned ? 'star-fill' : 'star',
        className: 'pin-toggle',
        run: () => void this.handleTogglePin(thread, projectId),
      },
      {
        id: 'mute',
        label: thread.muted ? 'Unmute' : 'Mute',
        icon: thread.muted ? 'bell-slash' : 'bell',
        className: 'mute-toggle',
        run: () => void this.handleToggleMute(thread, projectId),
      }
    );
    if (!thread.isGeneral) {
      actions.push(
        {
          id: 'move-up',
          label: 'Move up',
          icon: 'arrow-up',
          disabled: this.isThreadAtEdge(thread.id, projectId, 'first'),
          run: () => void this.moveThread(thread.id, projectId, -1),
        },
        {
          id: 'move-down',
          label: 'Move down',
          icon: 'arrow-down',
          disabled: this.isThreadAtEdge(thread.id, projectId, 'last'),
          run: () => void this.moveThread(thread.id, projectId, 1),
        }
      );
      const groups = this.getGroups(projectId);
      for (const group of groups.filter((g) => !g.threadIds.includes(thread.id))) {
        actions.push({
          id: `move-to-group:${group.id}`,
          label: `Move to ${group.name}`,
          icon: 'folder',
          run: () => {
            this.contextMenuTarget = null;
            void this.moveThreadToGroup(thread.id, group.id, projectId);
          },
        });
      }
      if (groups.some((g) => g.threadIds.includes(thread.id))) {
        actions.push({
          id: 'remove-from-group',
          label: 'Remove from group',
          icon: 'folder-minus',
          run: () => {
            this.contextMenuTarget = null;
            void this.removeThreadFromGroup(thread.id, projectId);
          },
        });
      }
    }
    actions.push(
      {
        id: 'copy-markdown',
        label: 'Copy as Markdown',
        icon: 'file-earmark-text',
        run: () => void this.handleExportThread(thread),
      },
      {
        id: 'download-markdown',
        label: 'Download as Markdown',
        icon: 'download',
        run: () => void this.handleDownloadThread(thread),
      },
      {
        id: 'rename',
        label: 'Rename',
        icon: 'pencil',
        run: () => this.startRename(thread),
      },
      {
        id: 'delete',
        label: 'Delete',
        icon: 'trash',
        destructive: true,
        run: () => void this.handleDeleteThread(thread, projectId),
      }
    );
    return actions;
  }

  /** The actions of a group header's menu, shared by the popup and the sheet. */
  private groupMenuActions(group: ThreadGroup, projectId: string): MenuAction[] {
    return [
      {
        id: 'new-thread',
        label: 'New thread',
        icon: 'plus-lg',
        run: () => {
          this.groupContextMenuTarget = null;
          this.startCreateThread(projectId, group.id);
        },
      },
      {
        id: 'rename-group',
        label: 'Rename group',
        icon: 'pencil',
        run: () => {
          this.groupContextMenuTarget = null;
          this.startGroupNameInput(projectId, {
            renamingGroupId: group.id,
            initialValue: group.name,
          });
        },
      },
      {
        id: 'delete-group',
        label: 'Delete group',
        icon: 'trash',
        destructive: true,
        run: () => {
          this.groupContextMenuTarget = null;
          void this.deleteGroup(group.id, projectId);
        },
      },
    ];
  }

  /** The open row menu's heading and actions, or null when none is open. */
  private openMenu(): { heading: string; actions: MenuAction[] } | null {
    if (this.contextMenuTarget) {
      const { thread, projectId } = this.contextMenuTarget;
      return { heading: `#${thread.name}`, actions: this.threadMenuActions(thread, projectId) };
    }
    if (this.groupContextMenuTarget) {
      const { group, projectId } = this.groupContextMenuTarget;
      return { heading: group.name, actions: this.groupMenuActions(group, projectId) };
    }
    return null;
  }

  private renderContextMenu() {
    if (!this.contextMenuTarget) return nothing;
    const { thread, projectId } = this.contextMenuTarget;
    return html`
      <div class="context-menu" @click=${(e: Event) => e.stopPropagation()}>
        ${renderMenuRows(this.threadMenuActions(thread, projectId))}
      </div>
    `;
  }

  /** Render context menu for a group header. */
  private renderGroupContextMenu() {
    if (!this.groupContextMenuTarget) return nothing;
    const { group, projectId } = this.groupContextMenuTarget;
    return html`
      <div class="context-menu" @click=${(e: Event) => e.stopPropagation()}>
        ${renderMenuRows(this.groupMenuActions(group, projectId))}
      </div>
    `;
  }

  /** The mobile presentation of the thread and group menus. */
  private renderMenuSheet() {
    const menu = this.menuAsSheet ? this.openMenu() : null;
    return html`
      <scion-action-sheet
        .items=${menu?.actions ?? []}
        heading=${menu?.heading ?? ''}
        .open=${menu !== null}
        @action-sheet-select=${(e: CustomEvent<ActionSheetSelectDetail>): void => {
          const current = this.openMenu();
          if (current) runMenuAction(current.actions, e.detail.id);
        }}
        @action-sheet-close=${(): void => this.closeRowMenus()}
      ></scion-action-sheet>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-space-rail': ScionChatSpaceRail;
  }
}
