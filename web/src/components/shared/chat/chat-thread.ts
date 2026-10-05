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
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { guard } from 'lit/directives/guard.js';
import { repeat } from 'lit/directives/repeat.js';
import { apiFetch, extractApiError } from '../../../client/api.js';
import type { Agent, Message } from '../../../shared/types.js';
import type { ChatSendDetail } from './chat-composer.js';
import { navigateTo, stateManager } from '../../../client/main.js';
import { openTerminal, agentGraphHref } from '../../../client/open-terminal.js';
import { showToast } from '../../../utils/toast.js';
import { playChimeThrottled } from '../../../utils/audio.js';
import type { ChatAgentMember } from './chat-members.js';
import './chat-message.js';
import './chat-system-line.js';
import './chat-composer.js';
import './chat-interagent-marker.js';
import { formatChatDate, renderDateDivider, chatDateDividerStyles } from './chat-date-divider.js';
import { DisplayZoneController } from '../../../utils/display-zone-controller.js';
import { effectiveTimeZone, formatInstantWithZone, toWallClockInput } from '../../../utils/time.js';
import '../code-editor.js';
import '../markdown-preview.js';
import './chat-file-preview.js';
import type { PreviewTarget } from './chat-file-preview.js';
import './chat-action-sheet.js';
import type { ActionSheetSelectDetail } from './chat-action-sheet.js';
import {
  placeMenuInViewport,
  renderMenuRows,
  runMenuAction,
  shouldUseMenuSheet,
  type MenuAction,
} from './context-menu.js';
import {
  parseContainerPath,
  buildFileApiUrl,
  resolveMessageProjectId,
  type PathLinkTarget,
} from '../../../utils/chat-file-links.js';
import { chatRecentFiles } from '../../../client/chat-recent-files.js';
import {
  findTopVisibleRow,
  scrollTopForAnchor,
  type ChatScrollAnchor,
} from './chat-scroll-anchor.js';
import { ComposerRoomController, type RoomComposer } from './composer-room.js';
import { PinOnResizeController } from './pin-on-resize.js';
import { focusElement } from '../focus-moved.js';

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

const EMPTY_ATTACHMENTS: NonNullable<Message['attachments']> = [];
const EMPTY_ATTACHMENT_REFS: import('./chat-message.js').AttachmentRefInfo[] = [];

/** Threshold in pixels from top to trigger upward scroll loading. */
const SCROLL_TOP_THRESHOLD = 100;

/** Threshold in pixels from bottom to consider "pinned to bottom". */
const SCROLL_BOTTOM_THRESHOLD = 80;

/** How long a restored scroll position is held against late layout shifts. */
const RESTORE_SETTLE_MS = 500;

/**
 * How far `scrollTop` may move from the value last written before the
 * restore watch treats it as a user scroll. Fractional device pixel ratios
 * can shift it by a sub-pixel amount with no scroll at all.
 */
const RESTORE_SCROLL_TOLERANCE_PX = 1;

/** Small margin kept above the unread divider when it is anchored to the top. */
const UNREAD_ANCHOR_MARGIN_PX = 16;

/**
 * How long to keep correcting the unread-divider anchor against late layout
 * shifts (images, markdown, attachments finishing their own load) before
 * giving up. Short-lived on purpose: a live SSE message arriving inside this
 * window changes the message count, not just element sizes, and is detected
 * separately so it never re-anchors — this timer only guards against the
 * anchor drifting while the *same* unread content is still settling.
 */
const UNREAD_ANCHOR_WINDOW_MS = 2000;

/**
 * Jump-to-message re-check (scrollend): pixel tolerance when deciding whether
 * a jump target is still "in view" after the scroll settles. A late layout
 * shift (image load, attachment height, late markdown) can move the target
 * out from under a smooth `scrollIntoView`, since Chromium's smooth scroll
 * targets the position computed when the scroll started.
 */
const JUMP_SCROLL_VIEW_TOLERANCE_PX = 24;

/** Jump-to-message re-check: cap on corrective re-scrolls to avoid a loop. */
const JUMP_SCROLL_MAX_RECHECKS = 2;

/** The hub rejects agent names longer than this many runes. */
const MAX_AGENT_NAME_LENGTH = 63;

/** Length of the random suffix in a default /spawn name. */
const SPAWN_SUFFIX_LENGTH = 4;

/**
 * Default /spawn name: `<template>-<suffix>`. The template part is
 * truncated so the whole name fits the hub's length limit, and trailing
 * hyphens are dropped so the result stays a valid slug. The suffix is a
 * random integer below 36^length in base36, left-padded with zeros, so
 * it is always exactly SPAWN_SUFFIX_LENGTH characters of [0-9a-z].
 */
function defaultSpawnName(template: string): string {
  const suffix = Math.floor(Math.random() * 36 ** SPAWN_SUFFIX_LENGTH)
    .toString(36)
    .padStart(SPAWN_SUFFIX_LENGTH, '0');
  const maxBase = MAX_AGENT_NAME_LENGTH - SPAWN_SUFFIX_LENGTH - 1;
  const base = Array.from(template).slice(0, maxBase).join('').replace(/-+$/, '');
  return `${base}-${suffix}`;
}

/**
 * /status: safety bound on agent-list pages followed via `nextCursor`. At the
 * server's 500-per-page cap this covers 10,000 agents; past it the listing
 * is shown with a truncation note rather than looping indefinitely.
 */
const MAX_STATUS_AGENT_PAGES = 20;

/**
 * Jump-to-message re-check: how long the fallback poll (older Safari, no
 * `scrollend`) must see a stable `scrollTop` before treating the scroll as
 * settled. This is a stability *window*, not the sampling interval — see
 * `JUMP_SCROLL_SETTLE_POLL_INTERVAL_MS` for that.
 */
const JUMP_SCROLL_SETTLE_STABLE_MS = 150;

/** Jump-to-message re-check: fallback poll's sampling interval. */
const JUMP_SCROLL_SETTLE_POLL_INTERVAL_MS = 50;

/**
 * Jump-to-message re-check: how long the watch waits, with no `scroll` event
 * and no `scrollend`, before concluding there's nothing left to watch for —
 * either the scroll has genuinely gone idle after settling, or (screened by
 * the pre-scroll check in `scrollToMessageById` for the common cases, but
 * not every one — e.g. content still streaming in mid-jump) it never moved
 * at all. Restarted on every `scroll` event, so a long smooth scroll, which
 * fires `scroll` every frame, stays watched for however long it actually
 * takes. A *fixed* deadline from arm time doesn't: Chromium's smooth-scroll
 * duration grows with distance and exceeded a 1500ms fixed deadline on jumps
 * beyond ~8000px, tearing the watcher down before `scrollend` and silently
 * dropping the re-check on exactly the long jumps most exposed to the bug
 * (deep search results, permalinks) (review R4). `scrollend` normally fires
 * within a frame of the last `scroll`, well inside this window, so it
 * doesn't delay the normal settle path.
 */
const JUMP_SCROLL_IDLE_TIMEOUT_MS = 300;

/**
 * Jump-to-message re-check: hard cap on the watch's total lifetime,
 * regardless of `scroll`/`scrollend` activity. The idle timeout above is
 * what actually bounds the normal cases; this is only a backstop against a
 * scroller that never goes idle (e.g. a runaway continuous scroll), so it
 * can be generous.
 */
const JUMP_SCROLL_HARD_CAP_MS = 5000;

/**
 * Jump-to-message re-check: keys that scroll the page even when focus is
 * outside the scroll container — e.g. `document.activeElement` is `<body>`
 * after clicking a message, yet Chromium still scrolls the last-clicked
 * scroller on PageUp/PageDown. Filtered so unrelated typing (in a reply box,
 * say) doesn't cancel the watch.
 */
const JUMP_SCROLL_CANCEL_KEYS = new Set([
  'PageUp',
  'PageDown',
  'ArrowUp',
  'ArrowDown',
  'ArrowLeft',
  'ArrowRight',
  'Home',
  'End',
  ' ',
]);

/**
 * Whether `targetRect` counts as "in view" within `containerRect`, allowing
 * `JUMP_SCROLL_VIEW_TOLERANCE_PX` of slack. Full containment is sufficient
 * but not necessary: a target taller than the viewport (a long agent reply)
 * can never be fully contained, and `block: 'center'` deliberately puts its
 * top above the viewport, so it also counts as in view once it overlaps the
 * container's vertical midpoint — the point `center` alignment aims for.
 */
function isJumpTargetInView(containerRect: DOMRect, targetRect: DOMRect): boolean {
  const contained =
    targetRect.top >= containerRect.top - JUMP_SCROLL_VIEW_TOLERANCE_PX &&
    targetRect.bottom <= containerRect.bottom + JUMP_SCROLL_VIEW_TOLERANCE_PX;
  if (contained) return true;
  const mid = (containerRect.top + containerRect.bottom) / 2;
  return (
    targetRect.top <= mid + JUMP_SCROLL_VIEW_TOLERANCE_PX &&
    targetRect.bottom >= mid - JUMP_SCROLL_VIEW_TOLERANCE_PX
  );
}

/**
 * Whether scrolling `scrollEl` toward a target outside `containerRect` is
 * clamped — i.e. cannot move further in the needed direction because the
 * container is already scrolled to that end. `scrollIntoView` clamps to the
 * scrollable range: a reply-jump to a message near the bottom while already
 * pinned to the bottom, or a jump near the top while already at the top, has
 * nowhere further to go, so it produces no scroll and no `scrollend`.
 */
function isJumpScrollClamped(
  scrollEl: HTMLElement,
  containerRect: DOMRect,
  targetRect: DOMRect
): boolean {
  const targetAbove = targetRect.top < containerRect.top;
  const targetBelow = targetRect.bottom > containerRect.bottom;
  const atTop = scrollEl.scrollTop <= 0;
  const atBottom =
    scrollEl.scrollTop >=
    scrollEl.scrollHeight - scrollEl.clientHeight - JUMP_SCROLL_VIEW_TOLERANCE_PX;
  return (targetAbove && atTop) || (targetBelow && atBottom);
}

/**
 * Whether `node` is (or is inside) an editable element — an input, textarea,
 * select, or contenteditable — for the purpose of ignoring scroll-shaped
 * keydowns (Space, arrows, ...) typed there as ordinary text input rather
 * than a scroll gesture. Checked against `composedPath()[0]`, the real
 * innermost element the event originated on, rather than `e.target`: at a
 * document-level listener, `e.target` is retargeted to the nearest
 * non-shadow ancestor, and Shoelace's `<sl-textarea>` (the composer) wraps
 * its native `<textarea>` in shadow DOM, so `e.target` there is the
 * `<sl-textarea>` host, not the editable element itself (review N3).
 */
function isEditableKeydownTarget(node: EventTarget | null): boolean {
  if (!(node instanceof Element)) return false;
  if (node.tagName === 'INPUT' || node.tagName === 'TEXTAREA' || node.tagName === 'SELECT') {
    return true;
  }
  return node instanceof HTMLElement && node.isContentEditable;
}

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

/** Match the store's ascending (created, UUID) order, not arrival order. */
function compareMessageOrder(a: Message, b: Message): number {
  const milliseconds = Date.parse(a.createdAt) - Date.parse(b.createdAt);
  if (Number.isFinite(milliseconds) && milliseconds !== 0) return milliseconds;
  // Go emits RFC3339 timestamps with up to nine fractional digits; Date
  // truncates to milliseconds. Preserve the remainder before breaking ties
  // by ID, and pad varying precision (.1 and .100000001) to the same width.
  const remainder = (timestamp: string): number => {
    const fraction = /\.(\d+)/.exec(timestamp)?.[1] || '';
    return Number(fraction.padEnd(9, '0').slice(3, 9));
  };
  const subMilliseconds = remainder(a.createdAt) - remainder(b.createdAt);
  if (subMilliseconds !== 0) return subMilliseconds;
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

/**
 * Return a copy of `msg` with `dispatchState` set to `dispatchState` and
 * `dispatchFailureReason`/`dispatchFailureCode` replaced by `reason`/`code`
 * (nc-delivery-unreachable PR #1895 review: build a fresh object instead of
 * mutating and `delete`-ing the stale fields, which deoptimizes the object's
 * V8 hidden class). Passing `undefined` for `reason`/`code` clears a stale
 * value rather than leaving it behind — callers that need a truthy check
 * instead of a nullish one should pre-convert falsy values to `undefined`
 * before calling.
 */
function withDispatchFailure(
  msg: Message,
  dispatchState: string,
  reason: string | undefined,
  code: string | undefined
): Message {
  /* eslint-disable-next-line @typescript-eslint/no-unused-vars -- rest-omit idiom: bind and drop these two keys so `...rest` excludes them */
  const { dispatchFailureReason: _reason, dispatchFailureCode: _code, ...rest } = msg;
  return {
    ...rest,
    dispatchState,
    ...(reason != null ? { dispatchFailureReason: reason } : {}),
    ...(code != null ? { dispatchFailureCode: code } : {}),
  };
}

/** Typing send throttle in ms. */
const TYPING_SEND_THROTTLE_MS = 4000;

/** Error shown when a path-link click cannot resolve any project id. */
const PATH_LINK_NO_PROJECT_ERROR =
  'Cannot open file: could not determine which project this file belongs to';

// Re-exported for existing tests/consumers (#1148); the implementation now
// lives in utils/chat-file-links.ts so the recorder can share it.
export { parseContainerPath, buildFileApiUrl, type PathLinkTarget };

@customElement('scion-chat-thread')
export class ScionChatThread extends LitElement {
  /**
   * Re-renders the thread (date dividers, any inline times) when the
   * effective display zone changes (review R2-1).
   */
  readonly _zone = new DisplayZoneController(this);

  /**
   * Keeps the composer's text field from growing past the visible frame: see
   * composer-room.ts.
   */
  readonly _composerRoom = new ComposerRoomController(this, () => ({
    // The message list, or the column holding the empty / loading / error
    // state (and, on a phone, the typing indicator) in its place.
    messages:
      this.shadowRoot?.querySelector<HTMLElement>('.messages-scroll, .state-area, .state-msg') ??
      null,
    composer: this.shadowRoot?.querySelector<RoomComposer>('scion-chat-composer') ?? null,
  }));

  /**
   * Keeps a list pinned to the bottom there when it gets shorter (the
   * keyboard opening): see pin-on-resize.ts. Not while the open-time unread
   * anchor holds the scroll position.
   */
  readonly _pinOnResize = new PinOnResizeController(
    this,
    () => this.shadowRoot?.querySelector<HTMLElement>('.messages-scroll') ?? null,
    () => this.pinnedToBottom && !this._unreadAnchorActive
  );

  // DEPRECATED(wave-1): agentId-based mode — remove after v2 is stable and flag is permanently ON.
  @property()
  agentId = '';

  // DEPRECATED(wave-1): agentId-based mode — remove after v2 is stable and flag is permanently ON.
  @property()
  agentName = '';

  @property({ type: Boolean })
  canSend = false;

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

  /**
   * Agent members with the richer per-agent fields (`canAttach`, `projectId`)
   * the members sidebar (chat-members.ts) already receives as `.agents`.
   * The context menu's "Open terminal" / "Open in graph" items key off this
   * list rather than `members` because they act on the message's author
   * agent, which may not be the thread's default agent or DM peer — the
   * only agents `getAgentProjectId`/`renderAgentToolbarButtons` in
   * pages/chat.ts otherwise resolve.
   */
  @property({ type: Array })
  agentMembers: ChatAgentMember[] = [];

  /** Whether v2 mode is active. Derived from conversationKey presence. */
  private get isV2(): boolean {
    return this.conversationKey.length > 0;
  }

  /**
   * Scroll position to restore when this conversation first loads, carried
   * over from a previous chat page instance (see chat-scroll-anchor.ts).
   * Ignored unless it names this conversation, and used at most once.
   */
  @property({ attribute: false })
  restoreScrollAnchor: ChatScrollAnchor | null = null;

  /** The restore anchor already applied, so a re-load does not reuse it. */
  private _usedRestoreAnchor: ChatScrollAnchor | null = null;

  /**
   * Bumped by each explicit jump, so a restore or an earlier jump still in
   * flight stands down, and the initial load leaves the view to the jump.
   */
  private _jumpSeq = 0;

  /** `_jumpSeq` when the current conversation was opened. */
  private _jumpSeqAtOpen = 0;

  /** The initial load left the view to a jump that has not landed yet. */
  private _openScrollDeferred = false;

  /** `_jumpSeq` of the last jump that reached its target. */
  private _landedJumpSeq = 0;

  /** Latest scroll position, kept current from scroll events. */
  private _scrollAnchor: ChatScrollAnchor | null = null;

  /** Pending rAF that refreshes `_scrollAnchor` after a scroll. */
  private _scrollAnchorRaf: number | null = null;

  /** Tears down the short watch that keeps a restored position in place. */
  private _restoreSettleCleanup: (() => void) | null = null;

  @state() private messages: Message[] = [];
  @state() private messageMap = new Map<string, Message>();
  @state() private loading = false;
  @state() private error: string | null = null;
  @state() private sending = false;
  @state() private sendError: string | null = null;
  @state() private pinnedToBottom = true;
  /** Whether the user expanded a one-line send error to its full text. */
  @state() private sendErrorExpanded = false;
  /** Whether the one-line send error (phone or tablet) cuts its text. */
  @state() private sendErrorTruncated = false;
  @state() private loadingOlder = false;
  @state() private hasOlderMessages = true;
  @state() private loaded = false;

  private messageRowsVersion = 0;

  override willUpdate(changedProperties: Map<string, unknown>): void {
    // A new (or cleared) send error starts collapsed.
    if (changedProperties.has('sendError')) {
      this.sendErrorExpanded = false;
      this.sendErrorTruncated = false;
    }
    // These updates affect controls around the transcript, not its rows.
    // Invalidate for every other property (including future ones), and for
    // explicit requestUpdate() calls such as read-receipt expiry. Metadata
    // maps are mutated in place alongside messages, so do not cache by map identity.
    if (
      changedProperties.size === 0 ||
      [...changedProperties.keys()].some(
        (key) =>
          key !== 'typingUsers' &&
          key !== 'agents' &&
          key !== 'pinnedToBottom' &&
          key !== 'sendErrorExpanded' &&
          key !== 'sendErrorTruncated'
      )
    ) {
      this.messageRowsVersion++;
    }
  }
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

  // ---- Phase-5: Context menu state ----

  /** The message targeted by the right-click context menu. */
  @state() private contextMenuMessage: Message | null = null;

  /** Position of the right-click context menu. */
  @state() private contextMenuPosition: { x: number; y: number } = { x: 0, y: 0 };

  /** The open message menu is the mobile bottom sheet, not the popup. */
  @state() private contextMenuAsSheet = false;

  // ---- Path-link file preview state (#1148) ----

  /**
   * Current path preview target, or null when closed. Loading/error/download
   * state lives inside the reusable `<scion-chat-file-preview>`; this
   * component only owns which path is currently being previewed.
   */
  @state() private filePreview: PreviewTarget | null = null;

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
  private viewingAroundMessage = false;
  private lastKnownTimestamp: string | null = null;
  private hadError = false;
  private fetchId = 0;

  // ---- Unread-divider open-to-top scroll anchor ----

  /** True while the open-time anchor to the unread divider is still in effect. */
  private _unreadAnchorActive = false;

  /** Message count captured when the anchor was applied, to tell a late layout
   *  shift of existing content (re-apply) apart from a new message arriving
   *  (stop — never re-anchor on live activity). */
  private _unreadAnchorMessageCount = 0;

  /** True for the duration of our own programmatic scrollTop write, so the
   *  resulting 'scroll' event is not mistaken for the user scrolling away. */
  private _applyingUnreadAnchor = false;

  /** Handle of the pending rAF that will clear `_applyingUnreadAnchor`. Kept
   *  so a second `applyUnreadAnchor` call within the same short window can
   *  cancel the earlier one before scheduling its own — otherwise the
   *  earlier rAF (e.g. from a ResizeObserver notification a frame apart from
   *  a second one) could clear the guard while the later write's own
   *  'scroll' event is still pending, and that event would then be
   *  misread as a user scroll. */
  private _applyingUnreadAnchorRaf: number | null = null;

  /** The value `applyUnreadAnchor` last wrote to `scrollTop` (read back after
   *  the browser clamps it), so a genuine user scroll landing in the same
   *  frame as that write — before the guard above clears on the next rAF —
   *  is still recognized as manual instead of being swallowed by the guard. */
  private _unreadAnchorWrittenScrollTop = 0;

  /** Watches `.messages-list` for late layout shifts while the anchor is active. */
  private _unreadAnchorResizeObserver: ResizeObserver | null = null;

  /** Bounds how long the short-lived resize watch stays attached. */
  private _unreadAnchorTimer: ReturnType<typeof setTimeout> | null = null;

  /** Handle of the deferred rAF, scheduled by `scrollToUnreadDivider`, that
   *  applies the initial anchor after `updateComplete`. Canceled on
   *  deactivation so a thread left before that first frame lands can't have
   *  it fire against a superseded or torn-down anchor. */
  private _unreadAnchorInitialRaf: number | null = null;

  /**
   * Cleanup for the in-flight jump-to-message scrollend re-check, if any.
   * Set by `watchJumpScrollSettle()`; calling it tears down whatever
   * listener/timer is pending and is idempotent.
   */
  private _jumpScrollCleanup: (() => void) | null = null;

  /** Bound listener for v2 SSE chat-message events via stateManager. */
  private _v2MessageHandler = this.handleV2ChatMessage.bind(this);

  /** True once an SSE connection has been observed while this thread listens. */
  private _sawSseConnect = false;

  /**
   * The hub keeps no event history, so anything sent while the stream was down
   * was never delivered; a reconnection refetches the latest page to fill the
   * gap. mergeMessages dedupes, so overlap with what is already shown is safe.
   */
  private _sseReconnectHandler = (): void => {
    if (!this._sawSseConnect) {
      this._sawSseConnect = true;
      return;
    }
    if (!this.loaded || this.viewingAroundMessage) return;
    void this.fetchHistoryV2().catch((err) => {
      console.warn('[chat-thread] catch-up fetch after reconnect failed:', err);
    });
  };

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

  /**
   * Set when this conversation was just marked unread (from the rail, the
   * members sidebar, or another of the user's own tabs) while it is open
   * here. Blocks maybeAdvanceReadWatermark so viewing the still-open
   * conversation does not immediately re-mark it read — Slack-like
   * behaviour. Cleared by a conversation switch (navigate away and back) or
   * by sending a message here.
   */
  private _autoAdvanceSuppressed = false;

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

  /** Timer for the initial-load watermark advance (500ms or 2000ms). */
  private _initialWatermarkTimer: ReturnType<typeof setTimeout> | null = null;

  /** Read tracking: debounce timer for advancing watermark. */
  private _readDebounceTimer: ReturnType<typeof setTimeout> | null = null;

  /** Read tracking: whether the tab is focused. */
  private _tabFocused = true;

  /**
   * Cross-project: maps foreign project IDs to resolved slugs.
   * Populated in the `updated()` lifecycle when `messages` changes.
   * Reactive so that resolved slugs trigger a re-render automatically.
   */
  @state() private _projectSlugCache = new Map<string, string>();

  /** Set of project IDs currently being resolved to avoid duplicate fetches. */
  private _projectSlugPending = new Set<string>();

  /** Backfill single-flight guard: a backfill request is currently running. */
  private _backfillInFlight = false;

  /** Backfill single-flight guard: another backfill was requested while one was running. */
  private _backfillPending = false;

  /** Idempotency keys of currently in-flight optimistic messages. */
  private _pendingIdempotencyKeys = new Set<string>();

  /** Focus/blur handlers for read tracking. */
  private _focusHandler = () => {
    this._tabFocused = true;
    this.maybeAdvanceReadWatermark();
  };
  private _blurHandler = () => {
    this._tabFocused = false;
  };

  /**
   * Look up a resolved project slug from the cache. Called during render —
   * no async work happens here. Resolution is handled in `updated()`.
   */
  private resolveProjectSlug(projectId: string): string {
    if (!projectId || projectId === this.projectId) return '';
    return this._projectSlugCache.get(projectId) ?? '';
  }

  /**
   * Scan messages for foreign senderProjectId values and resolve any that
   * are not yet cached. Called from the `updated()` lifecycle so that
   * async fetches never run during render.
   */
  private resolveUnknownProjectSlugs(): void {
    const toResolve = new Set<string>();
    for (const msg of this.messages) {
      const pid = msg.senderProjectId;
      if (
        pid &&
        pid !== this.projectId &&
        !this._projectSlugCache.has(pid) &&
        !this._projectSlugPending.has(pid)
      ) {
        toResolve.add(pid);
      }
    }

    for (const projectId of toResolve) {
      this._projectSlugPending.add(projectId);
      void apiFetch(`/api/v1/projects/${encodeURIComponent(projectId)}`)
        .then(async (res) => {
          const updated = new Map(this._projectSlugCache);
          if (res.ok) {
            const data = (await res.json()) as { slug?: string; name?: string };
            updated.set(projectId, data.slug || data.name || projectId.slice(0, 8));
          } else {
            updated.set(projectId, projectId.slice(0, 8));
          }
          this._projectSlugPending.delete(projectId);
          this._projectSlugCache = updated;
        })
        .catch(() => {
          const updated = new Map(this._projectSlugCache);
          updated.set(projectId, projectId.slice(0, 8));
          this._projectSlugPending.delete(projectId);
          this._projectSlugCache = updated;
        });
    }
  }

  static override styles = [
    chatDateDividerStyles,
    css`
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
        font-size: var(--chat-fs-base);
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
        /* Positioned so descendants' offsetTop (used to anchor the unread
         * divider to the top of the viewport on open) is measured relative to
         * this container instead of bubbling up to an ancestor outside it. */
        position: relative;
        flex: 1;
        overflow-y: auto;
        overflow-x: hidden;
        overscroll-behavior: contain;
        /* Set by the chat page's mobile panels; see chat.ts. Code blocks
         * and tables are scrollers of their own, so they still pan sideways. */
        touch-action: var(--chat-touch-action, auto);
        /* In a tight keyboard frame inside the chat shell (it publishes
         * --scion-chat-tight) the padding goes, so the list can give up all
         * its room to the composer rather than keeping a 1rem minimum. */
        padding: calc(0.5rem * (1 - var(--scion-chat-tight, 0))) 0;
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
        font-size: var(--chat-fs-base);
        font-weight: 500;
        cursor: pointer;
        box-shadow: 0 2px 8px rgba(0, 0, 0, 0.15);
        transition: background 0.15s;
      }

      .jump-btn:hover {
        background: var(--scion-primary-600, #2563eb);
      }

      .jump-btn sl-icon {
        font-size: var(--chat-fs-lg);
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
        font-size: var(--chat-fs-sm);
        font-weight: 600;
        color: var(--scion-primary, #3b82f6);
        white-space: nowrap;
      }

      /* Permalink highlight animation */
      .permalink-highlight {
        animation: permalink-fade 2s ease-out;
      }

      @keyframes permalink-fade {
        0% {
          background-color: rgba(59, 130, 246, 0.2);
        }
        100% {
          background-color: transparent;
        }
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
        font-size: var(--chat-fs-5xl);
      }

      .state-msg sl-icon {
        font-size: var(--chat-fs-6xl);
        opacity: 0.4;
      }

      /* Send error toast */
      .send-error {
        padding: 0.375rem 1rem;
        font-size: var(--chat-fs-base);
        color: var(--scion-danger-600, #dc2626);
        background: var(--scion-danger-50, #fef2f2);
        border-top: 1px solid var(--scion-danger-200, #fecaca);
      }

      /* The expandable form (phone or tablet, text cut): a button that
         looks like the plain row. The native button look is switched off
         explicitly (iOS also rounds buttons); the background, colour and
         padding come from .send-error above, which as an author style
         already beats the button's defaults. */
      button.send-error {
        appearance: none;
        -webkit-appearance: none;
        border-radius: 0;
        display: block;
        width: 100%;
        box-sizing: border-box;
        margin: 0;
        border: none;
        border-top: 1px solid var(--scion-danger-200, #fecaca);
        font: inherit;
        font-size: var(--chat-fs-base);
        text-align: start;
        cursor: pointer;
      }

      /* On a phone or tablet the error is one line, cut with an ellipsis,
         until the user expands it; it can shrink (and clip) rather than
         push the composer's field out of the frame. The composer may shrink
         too (see chat-composer.ts). */
      @media (max-width: 768px), (pointer: coarse) {
        .send-error {
          flex-shrink: 1;
          min-height: 0;
          overflow: hidden;
        }

        .send-error:not([data-expanded]) {
          white-space: nowrap;
          text-overflow: ellipsis;
        }

        scion-chat-composer {
          min-height: 0;
        }

        /* The empty, loading and error states stand where the list does and
           give way like it: no fixed padding, and they can shrink to nothing
           rather than push the composer out of a short frame. */
        .state-msg {
          min-height: 0;
          overflow: hidden;
          padding-top: 0;
          padding-bottom: 0;
        }

        /* The state message and the typing indicator at its foot, as one
           column standing where the list does (see renderContentAndTyping). */
        .state-area {
          display: flex;
          flex-direction: column;
          flex: 1;
          min-height: 0;
          overflow: hidden;
        }

        .state-area > .typing-indicator {
          flex: none;
        }
      }

      /* Mention results footer */
      .mention-results {
        padding: 0.25rem 1rem;
        font-size: var(--chat-fs-sm);
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
        font-size: var(--chat-fs-sm);
        color: var(--scion-text-muted, #64748b);
        font-weight: 500;
      }

      .interagent-icons {
        display: flex;
        align-items: center;
        gap: 0.25rem;
      }

      .interagent-icons sl-icon-button::part(base) {
        font-size: var(--chat-fs-lg);
        color: var(--scion-text-muted, #64748b);
      }

      /* Typing indicator */
      .typing-indicator {
        display: flex;
        align-items: center;
        gap: 6px;
        padding: 4px 16px;
        font-size: var(--chat-fs-base);
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
        0%,
        20% {
          background: var(--scion-primary-50, #eff6ff);
        }
        100% {
          background: transparent;
        }
      }

      /* Phase-5: Context menu. It renders hidden and is shown once placed in
         the viewport. */
      .context-menu-overlay {
        position: fixed;
        inset: 0;
        z-index: 149;
      }

      .context-menu {
        visibility: hidden;
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
        font-size: var(--chat-fs-md);
        cursor: pointer;
        color: var(--scion-text, #1e293b);
        white-space: nowrap;
        transition: background 0.1s;
      }

      .context-menu-item:hover {
        background: var(--scion-primary-50, #eff6ff);
      }

      .context-menu-item.danger {
        color: var(--scion-danger-600, #dc2626);
      }

      .context-menu-item sl-icon {
        font-size: var(--chat-fs-lg);
        color: var(--scion-text-muted, #64748b);
      }

      .context-menu-item.danger sl-icon {
        color: var(--scion-danger-600, #dc2626);
      }

      /* Phase-5: Slash command system message */
      .system-info-message {
        padding: 0.5rem 1rem;
        font-size: var(--chat-fs-base);
        color: var(--scion-text-muted, #64748b);
        background: var(--scion-bg-subtle, #f1f5f9);
        border-radius: 0.375rem;
        margin: 0.25rem 1rem;
        white-space: pre-wrap;
      }

      /* Clear a landscape phone's notch and rounded corners (the page uses
         viewport-fit=cover) on whichever sides this column meets the screen
         edge. Each inset is a transparent border, so the row's background still
         paints to the screen edge and only its content moves in. The chat page
         sets --chat-inset-left and --chat-inset-right for the edges the
         conversation touches; both are 0 everywhere else. */
      .interagent-toggle-bar,
      .state-msg,
      .messages-scroll,
      .typing-indicator,
      .send-error {
        border-left: var(--chat-inset-left, 0px) solid transparent;
        border-right: var(--chat-inset-right, 0px) solid transparent;
      }
    `,
  ];

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
    this.measureSendErrorTruncation();
    this.observeSendError();
    // The typing indicator is in the list on a phone or tablet: when it
    // appears or goes, a list at the bottom stays at the bottom.
    if (
      changedProperties.has('typingUsers') &&
      this._composerRoom.capped &&
      this.pinnedToBottom &&
      !this._unreadAnchorActive
    ) {
      const list = this.shadowRoot?.querySelector<HTMLElement>('.messages-scroll');
      if (list) list.scrollTop = list.scrollHeight;
    }
    if (this.contextMenuMessage && !this.contextMenuAsSheet) {
      placeMenuInViewport(
        this.renderRoot.querySelector<HTMLElement>('.context-menu'),
        this.contextMenuPosition
      );
    }
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

    // Resolve foreign project slugs when messages change — keeps async work
    // out of the render phase (M1 fix).
    if (changedProperties.has('messages')) {
      this.resolveUnknownProjectSlugs();
    }
  }

  /** Tear down v2 state so a fresh load can happen. */
  private resetV2State(): void {
    // Cancel any pending jump-to-message scrollend re-check — it belongs to
    // the thread we're leaving, and a late correction must not fire against
    // the new one.
    this.cancelJumpScrollWatch();

    // Clear initial watermark timer to prevent it from firing against wrong thread
    if (this._initialWatermarkTimer) {
      clearTimeout(this._initialWatermarkTimer);
      this._initialWatermarkTimer = null;
    }

    // A thread switch is a fresh "open" — the old anchor (and its watchers)
    // belong to the conversation we just left.
    this.deactivateUnreadAnchor();
    this.cancelScrollAnchorCapture();
    this.cancelRestoreSettleWatch();
    this._scrollAnchor = null;

    // Stop any active SSE listener
    stateManager.removeEventListener('connected', this._sseReconnectHandler);
    stateManager.removeEventListener('chat-message-received', this._v2MessageHandler);
    stateManager.removeEventListener('chat-typing-received', this._v2TypingHandler);
    stateManager.removeEventListener('chat-read-state-updated', this._v2ReadStateHandler);
    stateManager.removeEventListener('chat-message-edited', this._v2EditHandler);
    stateManager.removeEventListener('chat-message-deleted', this._v2DeleteHandler);

    // Clear read-receipt state — it belongs to the conversation we just left.
    this.clearSeenState();

    // A thread switch is "navigate away" — mark-unread's suppression is
    // scoped to the conversation being open continuously, so leaving it
    // (even to come straight back) lifts it.
    this._autoAdvanceSuppressed = false;

    // Clear unread divider state.
    this.lastReadMessageId = '';
    this.showUnreadDivider = false;

    // Clear message state
    this._pendingIdempotencyKeys.clear();
    this.messageMap.clear();
    this.messages = [];
    this.nextCursor = null;
    this.viewingAroundMessage = false;
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
    // Jumps made before this point belong to the conversation left.
    this._jumpSeqAtOpen = this._jumpSeq;
    this._openScrollDeferred = false;
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this._sendErrorObserver?.disconnect();
    this._sendErrorObserver = null;
    this._observedSendError = null;
    this.stopStream();
    this.deactivateUnreadAnchor();
    // Keep `_scrollAnchor` itself: the page reads it after we detach.
    this.cancelScrollAnchorCapture();
    this.cancelRestoreSettleWatch();
    // Cancel any pending jump-to-message scrollend re-check and its listeners/timers.
    this.cancelJumpScrollWatch();
    // Clean up v2 SSE listeners
    stateManager.removeEventListener('connected', this._sseReconnectHandler);
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
    if (this._initialWatermarkTimer) {
      clearTimeout(this._initialWatermarkTimer);
      this._initialWatermarkTimer = null;
    }
    if (this._readDebounceTimer) {
      clearTimeout(this._readDebounceTimer);
      this._readDebounceTimer = null;
    }
    // Clean up context menu keyboard listener
    document.removeEventListener('keydown', this.handleContextMenuKeydown);
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
  /** Load history (previously loaded prefs too, but visibility prefs have been removed). */
  private async loadPrefsAndHistory(): Promise<void> {
    await this.initialLoad();
  }

  /** W7: Get attachment refs for a message (from history or send response). */
  private getMessageAttachmentRefs(
    messageId: string
  ): import('./chat-message.js').AttachmentRefInfo[] {
    return this.v2AttachmentMap.get(messageId) ?? EMPTY_ATTACHMENT_REFS;
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

  // DEPRECATED(wave-1): agentId-based history fetch — remove after v2 is stable and flag is permanently ON.
  private async fetchHistory(cursor?: string): Promise<void> {
    const currentId = this.fetchId;
    const params = new URLSearchParams({ limit: String(HISTORY_PAGE_SIZE) });
    if (cursor) {
      params.set('cursor', cursor);
    }

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
      const existing = this.messageMap.get(msg.id);
      // Preserve agent recipient from existing message if incoming lacks it
      // (e.g., backfill or SSE may not carry agent routing info).
      if (existing?.recipient?.startsWith('agent:') && !msg.recipient?.startsWith('agent:')) {
        msg.recipient = existing.recipient;
        msg.recipientId = existing.recipientId;
      }
      // nc-delivery-unreachable review FYI 1 / round 4 Optional 1: `failed`
      // is terminal for a given message ID. If the entry already shown is
      // failed and an incoming update (SSE or HTTP, in either order) would
      // move it away from `failed` — including an incoming entry that omits
      // dispatchState entirely, e.g. a backfill/history row that doesn't
      // carry dispatch info — keep the failed state and its reason/code,
      // merging every other field from the incoming message so legitimate
      // edits/updates still apply.
      let toStore = msg;
      if (existing?.dispatchState === 'failed' && msg.dispatchState !== 'failed') {
        // The pinned dispatchFailureReason/Code must come only from
        // `existing`, so strip any value `msg` carries for them before
        // conditionally re-adding `existing`'s.
        toStore = withDispatchFailure(
          msg,
          existing.dispatchState,
          existing.dispatchFailureReason,
          existing.dispatchFailureCode
        );
      }
      this.messageMap.set(msg.id, toStore);
    }

    // Match the server's (created, id) order so the displayed tail is also
    // the unread watermark, including tied/sub-millisecond timestamps.
    const sorted = Array.from(this.messageMap.values()).sort(compareMessageOrder);

    // Enforce buffer cap — remove oldest
    if (sorted.length > MAX_BUFFER) {
      const removed = sorted.splice(0, sorted.length - MAX_BUFFER);
      for (const msg of removed) {
        this.messageMap.delete(msg.id);
        // Prune mention results for evicted messages (R1 fix).
        this.mentionResultsByMessageId.delete(msg.id);
      }
    }

    // Filter out mention fan-out messages from the display array. They are
    // tracking artifacts for agent dispatch, not user-visible chat. They
    // remain in messageMap so ID-based dedup prevents a later backfill from
    // re-inserting them as real messages.
    this.messages = sorted.filter((m) => m.type !== 'mention');

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
        if (msg.senderId && msg.senderId !== this.selfUserId()) {
          playChimeThrottled(this.projectId || msg.projectId || '');
        }
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
    const loadId = this.fetchId;
    this.loading = true;
    this.error = null;

    try {
      // A switch while the history loads hands over to the next
      // conversation's own load; this one stops here.
      // A jump that has already shown the page around its target keeps
      // it: merging the latest page in as well would leave a gap.
      if (!(await this.fetchHistoryV2(undefined, () => !this.viewingAroundMessage))) return;
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
      // The loading flag, the restore anchor, the scroll target and the
      // watermark timer all belong to the conversation now on screen.
      if (loadId === this.fetchId) {
        this.loading = false;
        // Determine scroll target: permalink hash > restored position >
        // unread divider > bottom. A restored position that was following the
        // bottom yields to the unread divider: messages that arrived while the
        // user was away should be met at "New messages", not scrolled past.
        // The anchor is taken (used up) even when the hash wins.
        // An explicit jump made while loading (e.g. a search result in
        // this conversation) outranks all of them, including the hash.
        const jumped = this._jumpSeq !== this._jumpSeqAtOpen;
        const hashMsgId = jumped ? '' : this.parseMessageHash();
        const restore = jumped ? null : this.takeRestoreScrollAnchor();
        if (jumped) {
          // The jump owns the scroll position.
          this._openScrollDeferred = this._landedJumpSeq !== this._jumpSeq;
        } else if (hashMsgId) {
          void this.scrollToMessageById(hashMsgId, true);
        } else if (restore && !(restore.pinnedToBottom && this.showUnreadDivider)) {
          void this.restoreScrollPosition(restore);
        } else if (this.showUnreadDivider) {
          this.scrollToUnreadDivider();
        } else {
          this.scrollToBottomAfterRender();
        }
        // Advance read watermark after a delay so the blue dot clears. When
        // showUnreadDivider is true, use a longer delay so the user can see the
        // "New messages" divider before it is acknowledged. When it is false
        // (first DM open — no prior read state), a shorter settle delay is
        // enough to let the render commit.
        if (this.messages.length > 0) {
          const delay = this.showUnreadDivider ? 2000 : 500;
          if (this._initialWatermarkTimer) clearTimeout(this._initialWatermarkTimer);
          this._initialWatermarkTimer = setTimeout(() => {
            this._initialWatermarkTimer = null;
            // Same "viewing counts as reading" auto-behaviour maybeAdvanceReadWatermark
            // gates — a mark-unread landing during this delay (e.g. another tab,
            // or this one via the rail) must not be undone the instant this
            // timer fires.
            if (this._autoAdvanceSuppressed) return;
            const messageId = this.lastReadableMessageId();
            if (messageId) {
              void this.advanceReadWatermark(messageId);
            }
          }, delay);
        }
      }
    }
  }

  /**
   * Fetch a page of history and merge it. Resolves false, having changed
   * nothing, when the thread switched conversations before the page
   * arrived — including when the request then failed: that failure
   * belongs to the conversation left. Callers then leave the new
   * conversation alone too. When `shouldMerge` turns false while the page
   * loads, the page is dropped but the result is still true.
   */
  private async fetchHistoryV2(
    cursor?: string,
    shouldMerge: () => boolean = () => true
  ): Promise<boolean> {
    const currentId = this.fetchId;
    try {
      // Captured before the request starts: a response landing after the user
      // has logged out (or switched accounts) must not repopulate a store that
      // is no longer this identity's.
      const recentFilesGeneration = chatRecentFiles.scopeGeneration;
      const params = new URLSearchParams({ limit: String(HISTORY_PAGE_SIZE) });
      if (cursor) {
        params.set('cursor', cursor);
      }

      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/messages?${params.toString()}`
      );

      // Early out only: the catch below and the check after the body is
      // read would also drop a stale page, but there is no need to read it.
      if (currentId !== this.fetchId) return false;

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
        replyPreviews?: Record<string, { messageId: string; senderName: string; content: string }>;
      };

      // The body can still be arriving after the headers; a conversation
      // switch in the meantime must not merge this page into the new one.
      if (currentId !== this.fetchId) return false;
      if (!shouldMerge()) return true;

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
      this.recordRecentFilesForHistory(items, recentFilesGeneration);
      return true;
    } catch (err) {
      if (currentId !== this.fetchId) return false;
      throw err;
    }
  }

  /** Start listening for v2 messages via stateManager instead of per-thread EventSource. */
  private startStreamV2(): void {
    this._sawSseConnect = stateManager.isConnected;
    stateManager.addEventListener('connected', this._sseReconnectHandler);
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
      groupId?: string;
      dispatchState?: string;
      dispatchFailureReason?: string;
      dispatchFailureCode?: string;
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
    if (this.viewingAroundMessage) return;

    // The sender finished typing the moment their message landed — drop the
    // indicator now rather than waiting out TYPING_EXPIRY_MS.
    this.clearTypingForUser(eventData.senderId);

    // If the event carries a full message payload, merge directly instead of
    // doing a round-trip backfill.
    // SSE events from PublishUserMessage carry the full message payload.
    // mergeMessages() deduplicates by ID (last-write-wins via Map.set), so if
    // both the POST response and the SSE event provide the same message, the
    // later arrival's fields prevail — except that `failed` is terminal for
    // dispatch fields (dispatchState/dispatchFailureReason/dispatchFailureCode):
    // once a message is failed, a later arrival can't downgrade it back to
    // dispatched/pending, though its other fields still merge normally.
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
        ...(eventData.groupId != null ? { groupId: eventData.groupId } : {}),
        ...(eventData.dispatchState != null ? { dispatchState: eventData.dispatchState } : {}),
        ...(eventData.dispatchFailureReason != null
          ? { dispatchFailureReason: eventData.dispatchFailureReason }
          : {}),
        ...(eventData.dispatchFailureCode != null
          ? { dispatchFailureCode: eventData.dispatchFailureCode }
          : {}),
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

      // If we have a pending optimistic message and this SSE event is from us,
      // remove the temp-keyed optimistic entry to prevent a duplicate flash.
      if (this._pendingIdempotencyKeys.size > 0 && msg.senderId === this.selfUserId()) {
        for (const key of this._pendingIdempotencyKeys) {
          const opt = this.messageMap.get(key);
          if (opt) {
            // Preserve optimistic agent recipient if SSE version lacks it.
            if (opt.recipient?.startsWith('agent:') && !msg.recipient?.startsWith('agent:')) {
              msg.recipient = opt.recipient;
              msg.recipientId = opt.recipientId;
            }
            this.messageMap.delete(key);
            this._pendingIdempotencyKeys.delete(key);
            break;
          }
        }
      }

      this.mergeMessages([msg]);
      // A payload with no real createdAt would otherwise record (or, worse,
      // "correct" a pending provisional record with) a fabricated
      // viewing-time timestamp — the recent-files record must use the
      // message's own send time, never a value invented at capture time.
      // Skip capture here; the next history/backfill merge carries the
      // server's authoritative createdAt for the same message.
      if (eventData.createdAt) {
        this.recordRecentFiles(msg, this.getMessageAttachmentRefs(msg.id));
      }

      // Play a chime for messages from others — never for our own echoed
      // back to this tab.
      if (msg.senderId && msg.senderId !== this.selfUserId()) {
        playChimeThrottled(this.projectId || msg.projectId || '');
      }

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
    if (!this.conversationKey || this.viewingAroundMessage) return;
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
    const recentFilesGeneration = chatRecentFiles.scopeGeneration;
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
      messageExtensions?: Record<
        string,
        { messageId: string; replyToId?: string; editedAt?: string; deletedAt?: string }
      >;
      replyPreviews?: Record<string, { messageId: string; senderName: string; content: string }>;
    };
    // See fetchHistoryV2: re-checked once the body has been read.
    if (currentId !== this.fetchId) return;
    const items = data?.items ?? data?.messages ?? [];

    // W7: Merge attachment refs from history response.
    if (data?.messageAttachments) {
      for (const [msgId, refs] of Object.entries(data.messageAttachments)) {
        this.v2AttachmentMap.set(msgId, refs);
      }
    }

    // Phase-3: Merge message extensions and reply previews from backfill.
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

    this.mergeMessages(items);
    this.recordRecentFilesForHistory(items, recentFilesGeneration);
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

  /**
   * Handle a read-watermark change arriving over SSE. This fires for two
   * different things sharing one event: a DM peer's watermark advancing
   * (render the "Seen" tick), and the caller's OWN watermark moving via
   * mark-unread. The `unread` field is the sole discriminator for the
   * latter — NOT the userId match. userId alone would also be true for any
   * future self-notifying /read, which must not be misread as mark-unread. A
   * self-targeted event without `unread: true` is neither a peer tick nor a
   * mark-unread — it is ignored, not misapplied as either.
   */
  private handleV2ReadStateEvent(e: Event): void {
    type ReadStateData = {
      conversationKey?: string;
      userId?: string;
      messageId?: string;
      readAt?: string;
      unread?: boolean;
    };
    const detail = (e as CustomEvent).detail as
      | ({ data?: ReadStateData } & ReadStateData)
      | undefined;
    const eventData: ReadStateData | undefined = detail?.data ?? detail;
    if (!eventData || eventData.conversationKey !== this.conversationKey) return;
    // selfUserId(), not the public currentUserId field directly: it lazily
    // resolves the ID from the chat scope for threads mounted before the
    // scope is configured, falling back to currentUserId once set.
    if (eventData.userId && eventData.userId === this.selfUserId()) {
      if (eventData.unread === true) {
        this.handleOwnReadStateChanged();
      }
      return;
    }
    if (!eventData.messageId) return;
    this.applyPeerReadState(eventData.messageId, eventData.readAt);
  }

  /**
   * The caller's own watermark moved via mark-unread while this conversation
   * is open (here, or in another of their tabs). Suppress auto-advance so
   * simply having it open does not immediately undo the mark-unread.
   */
  private handleOwnReadStateChanged(): void {
    this.suppressAutoAdvance();
  }

  /**
   * Suppress auto-advance immediately. Called from two places: the SSE path
   * above (other tabs, and this one on the round trip back), and directly by
   * the chat page right after this tab's own "Mark unread" POST succeeds —
   * the same-tab case must not wait on the SSE echo.
   *
   * Also cancels any debounce timer already in flight. In single-threaded
   * JS this clearTimeout always wins over a pending callback — there is no
   * "queued before the clear takes effect" race to close — so this is
   * belt-and-braces with maybeAdvanceReadWatermark's own re-check and never
   * load-bearing on its own: whichever of the two runs first already
   * prevents the stale advance.
   */
  suppressAutoAdvance(): void {
    this._autoAdvanceSuppressed = true;
    if (this._readDebounceTimer) {
      clearTimeout(this._readDebounceTimer);
      this._readDebounceTimer = null;
    }
  }

  /** Record the peer watermark and arm the auto-hide timer. */
  private applyPeerReadState(messageId: string, readAt?: string): void {
    const parsed = readAt ? new Date(readAt).getTime() : NaN;
    this.peerReadMessageId = messageId;
    this.peerReadAt = Number.isNaN(parsed) ? Date.now() : parsed;

    if (this._seenExpiryTimer) clearTimeout(this._seenExpiryTimer);
    this._seenExpiryTimer = null;
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
    // F5 (p2a-r2 review): "deferred" stays visible on every message, like
    // "failed" — the sender must be told their message was saved for
    // catch-up rather than dispatched, on every message it happened to,
    // not just the most recent one.
    if (!dispatchState || dispatchState === 'failed' || dispatchState === 'deferred') {
      return dispatchState;
    }
    if (msg.id !== lastOwnMessageId) return '';
    if (seenExpired && this.isMessageSeen(msg)) return '';
    return dispatchState;
  }

  private isMessageSeen(msg: Message): boolean {
    if (!this.peerReadMessageId) return false;
    const watermark = this.messageMap.get(this.peerReadMessageId);
    if (!watermark) return false;
    return compareMessageOrder(msg, watermark) <= 0;
  }

  /** Fetch inter-agent messages for inline markers. Stores the raw flat list. */
  private async fetchInteragentExchanges(): Promise<void> {
    if (!this.isAgentDM) return;

    const params = new URLSearchParams({ limit: '200' });
    const currentId = this.fetchId;

    try {
      // Viewing inter-agent exchanges requires agent.attach, which members
      // lack on agents they did not create. The markers are optional, so a
      // 403 just hides them rather than raising the access-denied toast.
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/interagent?${params.toString()}`,
        { suppressAccessDeniedToast: true }
      );
      if (!res.ok || currentId !== this.fetchId) return;

      const data = (await res.json()) as { messages?: Message[] };
      if (currentId !== this.fetchId) return;
      const msgs = data?.messages ?? [];
      // Store sorted flat list — grouping by DM gaps happens in renderMessages().
      this.interagentMessages = [...msgs].sort(compareMessageOrder);
    } catch {
      // Non-critical
    }
  }

  /** Send a message in v2 mode. */
  private async handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void> {
    const {
      text,
      interrupt,
      mentions,
      attachmentIds,
      replyToId,
      replyToContent,
      onSuccess,
      onError,
    } = e.detail;
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
    // Sending is the other way mark-unread's suppression lifts (besides
    // navigating away and back): you cannot both have just marked a
    // conversation unread and be sending into it without meaning to read it.
    this._autoAdvanceSuppressed = false;

    // Generate an idempotency key so duplicate sends (e.g. network retry)
    // are collapsed server-side. Also used as the optimistic message temp ID.
    const idempotencyKey = crypto.randomUUID();

    // Save composerReplyTo before clearing it optimistically. On failure we
    // restore it so the reply bar comes back; on success it stays cleared.
    const savedReplyTo = this.composerReplyTo;
    this.composerReplyTo = null;

    // Optimistic insertion: show the message in the history immediately with
    // a "Sending" indicator, before the API call returns.
    const optimisticMsg: Message = {
      id: idempotencyKey,
      projectId: '',
      sender: '',
      senderId: this.selfUserId(),
      recipient: this.defaultAgent ? 'agent:' + this.defaultAgent : '',
      recipientId: this.defaultAgent || '',
      msg: text,
      type: replyToId ? 'reply' : 'chat',
      agentId: '',
      createdAt: new Date().toISOString(),
      dispatchState: 'pending',
    };
    this.messageMap.set(optimisticMsg.id, optimisticMsg);
    this._pendingIdempotencyKeys.add(idempotencyKey);
    this.messages = Array.from(this.messageMap.values())
      .filter((m) => m.type !== 'mention')
      .sort(compareMessageOrder);
    this.scrollToBottomAfterRender();
    const recentFilesGeneration = chatRecentFiles.scopeGeneration;
    // Snapshotted before the POST's `await`s below: a conversation switch
    // while the send is in flight must not attribute this message's files to
    // whatever conversation/project the thread has since moved on to.
    const sendConversationKey = this.conversationKey;
    const sendProjectId = this.resolvePathLinkProjectId(optimisticMsg);
    // A failure that lands after a conversation switch must not put this
    // send's reply bar or error on the conversation now on screen.
    const sendFetchId = this.fetchId;

    try {
      const body: Record<string, unknown> = {
        content: text,
        idempotency_key: idempotencyKey,
      };
      if (mentions && mentions.length > 0) {
        body.mentions = mentions;
      }
      // "Send with interruption": only sent when requested so ordinary sends
      // keep the minimal body.
      if (interrupt) {
        body.interrupt = true;
      }
      // W7: Include attachment IDs.
      if (attachmentIds && attachmentIds.length > 0) {
        body.attachments = attachmentIds;
      }
      // Phase-3: Include reply_to_id.
      if (replyToId) {
        body.reply_to_id = replyToId;
      }
      // nc-reply-recipient: the primary recipient for a reply is resolved
      // server-side from reply_to_id (the replied-to message's actual
      // sender) — not from a client-supplied agent slug, which would be
      // spoofable and inconsistent across clients.
      // Fix: Add RE-to metadata when replying — first 32 codepoints with ellipsis.
      // Use spread to avoid splitting UTF-16 surrogate pairs (e.g. emoji).
      if (replyToId && replyToContent) {
        const codepoints = [...replyToContent];
        const metadata: Record<string, string> = {
          'RE-to':
            codepoints.length > 32 ? codepoints.slice(0, 32).join('') + '...' : replyToContent,
        };
        body.metadata = metadata;
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
        // Remove optimistic message on failure.
        this.messageMap.delete(idempotencyKey);
        this._pendingIdempotencyKeys.delete(idempotencyKey);
        this.messages = Array.from(this.messageMap.values())
          .filter((m) => m.type !== 'mention')
          .sort(compareMessageOrder);
        // Restore reply-to state so the reply bar comes back for retry —
        // before reading the error, so a reply picked meanwhile stands.
        // These guards cover the thread's own state only; the composer's
        // onError restore after a switch is handled separately.
        if (sendFetchId === this.fetchId) this.composerReplyTo = savedReplyTo;
        const error = await extractApiError(res, 'Failed to send message');
        if (sendFetchId === this.fetchId) this.sendError = error;
        onError?.(error ?? 'Failed to send message');
      } else {
        // W7: Parse attachment refs from the send response.
        const resData = (await res.json().catch(() => null)) as {
          id?: string;
          attachments?: import('./chat-message.js').AttachmentRefInfo[];
          dispatchState?: string;
          dispatchFailureReason?: string;
          dispatchFailureCode?: string;
        } | null;
        if (resData?.id && resData?.attachments && resData.attachments.length > 0) {
          this.v2AttachmentMap.set(resData.id, resData.attachments);
        }
        // nc-delivery-unreachable: the backend now reports the real dispatch
        // outcome instead of always being "dispatched" on any HTTP 2xx.
        const dispatchState = resData?.dispatchState ?? 'dispatched';

        // Update the optimistic message in-place with the server-assigned ID
        // instead of deleting it. This avoids a visible flicker (message
        // disappearing then reappearing) between the delete and the backfill
        // delivering the real message.
        const optimistic = this.messageMap.get(idempotencyKey);
        if (optimistic && resData?.id) {
          this.messageMap.delete(idempotencyKey);
          this._pendingIdempotencyKeys.delete(idempotencyKey);
          // If SSE already delivered the real message, keep it as the ground truth
          // to preserve all server-enriched fields (createdAt, metadata, groupId, etc.)
          const sseVersion = this.messageMap.get(resData.id);
          if (sseVersion) {
            // nc-delivery-unreachable review FYI 1: never downgrade a
            // terminal `failed` state — skip applying this HTTP response's
            // dispatch fields if the SSE-delivered version is already failed
            // and this response would move it back to dispatched/pending.
            const wouldDowngrade =
              sseVersion.dispatchState === 'failed' &&
              (dispatchState === 'dispatched' || dispatchState === 'pending');
            let updatedSseVersion = sseVersion;
            if (!wouldDowngrade) {
              // Strip any prior dispatchFailureReason/Code before
              // conditionally re-adding the response's, so a stale value is
              // dropped rather than left behind when the response omits it.
              updatedSseVersion = withDispatchFailure(
                sseVersion,
                dispatchState,
                resData?.dispatchFailureReason || undefined,
                resData?.dispatchFailureCode || undefined
              );
            }
            // Preserve optimistic agent recipient if SSE version lacks it.
            if (
              optimistic.recipient?.startsWith('agent:') &&
              !updatedSseVersion.recipient?.startsWith('agent:')
            ) {
              updatedSseVersion = {
                ...updatedSseVersion,
                recipient: optimistic.recipient,
                recipientId: optimistic.recipientId,
              };
            }
            this.messageMap.set(resData.id, updatedSseVersion);
          } else {
            // Symmetric with the sseVersion branch above: a stale
            // dispatchFailureReason/Code is dropped, not left behind on this
            // reused object, when the response omits it.
            const updatedOptimistic: Message = {
              ...withDispatchFailure(
                optimistic,
                dispatchState,
                resData?.dispatchFailureReason || undefined,
                resData?.dispatchFailureCode || undefined
              ),
              id: resData.id,
            };
            this.messageMap.set(resData.id, updatedOptimistic);
          }
        } else {
          // Fallback: remove if we cannot remap (should not happen).
          this.messageMap.delete(idempotencyKey);
          this._pendingIdempotencyKeys.delete(idempotencyKey);
        }
        this.messages = Array.from(this.messageMap.values())
          .filter((m) => m.type !== 'mention')
          .sort(compareMessageOrder);

        // The send response never carries the server's authoritative
        // createdAt, so this is recorded "provisional": the client's own
        // send time stands in until the SSE echo or backfill (both
        // non-provisional) correct it — even if the corrected time is
        // earlier.
        if (resData?.id) {
          this.recordRecentFiles({ ...optimisticMsg, id: resData.id }, resData.attachments ?? [], {
            provisional: true,
            scopeGeneration: recentFilesGeneration,
            conversationKey: sendConversationKey,
            projectId: sendProjectId,
          });
        }

        onSuccess();
        // Backfill to get the full server-enriched message. The optimistic
        // message (now keyed by the real ID) stays visible until the backfill
        // overwrites it, so there is no gap.
        void this.backfillV2();
      }
    } catch (err) {
      // Remove optimistic message on failure.
      this.messageMap.delete(idempotencyKey);
      this._pendingIdempotencyKeys.delete(idempotencyKey);
      this.messages = Array.from(this.messageMap.values())
        .filter((m) => m.type !== 'mention')
        .sort(compareMessageOrder);
      const error = err instanceof Error ? err.message : 'Failed to send message';
      // Thread state only; the composer's onError restore after a switch
      // is handled separately.
      if (sendFetchId === this.fetchId) {
        // Restore reply-to state so the reply bar comes back for retry.
        this.composerReplyTo = savedReplyTo;
        this.sendError = error;
      }
      onError?.(error);
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

  /**
   * Clear the composer's reply context. The composer cannot do this itself —
   * `replyTo` is a property we own and push down, so a local assignment there
   * is overwritten on our next render.
   */
  private handleComposerCancelReply(): void {
    this.composerReplyTo = null;
  }

  /** Clear the composer's edit context. See handleComposerCancelReply. */
  private handleComposerCancelEdit(): void {
    this.composerEditMessage = null;
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
    void this.scrollToMessageById(e.detail.messageId);
  }

  /** SSE handler for message-edited events. */
  private handleV2MessageEdited(e: Event): void {
    const detail = (e as CustomEvent).detail as
      | ({
          data?: {
            conversationKey?: string;
            messageId?: string;
            content?: string;
            editedAt?: string;
          };
        } & { conversationKey?: string; messageId?: string; content?: string; editedAt?: string })
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
      | ({ data?: { conversationKey?: string; messageId?: string; deletedAt?: string } } & {
          conversationKey?: string;
          messageId?: string;
          deletedAt?: string;
        })
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
      if (this.isSenderAgent(this.messages[i])) {
        return true;
      }
    }
    return false;
  }

  /**
   * The ID of the last message eligible to become the read watermark: skips
   * messages still keyed by their optimistic-send idempotency key (no SSE
   * echo or HTTP ack yet). That temporary ID names no persisted message —
   * POSTing it would advance the watermark to an ID the server (and every
   * other client) cannot resolve, and if the send is slow (e.g.
   * sendAgentRouted waiting on agent dispatch) or the SSE connection is
   * degraded, the 1s/500ms/2s timers below can fire before reconciliation
   * replaces it. Returns '' if every message is still pending.
   */
  private lastReadableMessageId(): string {
    for (let i = this.messages.length - 1; i >= 0; i--) {
      const id = this.messages[i].id;
      if (!this._pendingIdempotencyKeys.has(id)) return id;
    }
    return '';
  }

  /** Advance the read watermark if conditions are met. */
  private maybeAdvanceReadWatermark(): void {
    if (!this.isV2 || !this._tabFocused || !this.pinnedToBottom) return;
    if (this._autoAdvanceSuppressed) return;
    if (this.messages.length === 0) return;

    // Debounce
    if (this._readDebounceTimer) clearTimeout(this._readDebounceTimer);
    this._readDebounceTimer = setTimeout(() => {
      this._readDebounceTimer = null;
      // Re-check: suppression can arrive after this callback is scheduled
      // but before it fires — a message arms this 1s debounce, then
      // mark-unread lands mid-flight. suppressAutoAdvance already clears an
      // in-flight timer synchronously when that is how suppression arrives,
      // so this guard is belt-and-braces for that path (there is no "queued
      // before the clear" race in single-threaded JS) and load-bearing only
      // if suppression is ever set some other way, without going through
      // suppressAutoAdvance.
      if (this._autoAdvanceSuppressed) return;
      const messageId = this.lastReadableMessageId();
      if (messageId) {
        void this.advanceReadWatermark(messageId);
      }
    }, 1000);
  }

  private async advanceReadWatermark(messageId: string): Promise<void> {
    // The watermark only moves forward and the scroll/focus triggers fire far
    // more often than it changes; re-POSTing the same ID would also re-fan the
    // read-state event out to the peer for nothing.
    if (!messageId || messageId === this._lastAdvancedMessageId) return;
    // Defence in depth: callers already filter these out via
    // lastReadableMessageId(), but never let an optimistic temp ID reach the
    // network regardless of caller.
    if (this._pendingIdempotencyKeys.has(messageId)) return;
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

    // Any scroll not caused by our own anchor write is the user taking over —
    // stop re-anchoring to the unread divider immediately. A genuine user
    // scroll landing in the very same frame as our write is still detected:
    // `_applyingUnreadAnchor` is true for that whole frame, but the resulting
    // scrollTop then disagrees with the value we just wrote.
    if (
      this._unreadAnchorActive &&
      (!this._applyingUnreadAnchor || el.scrollTop !== this._unreadAnchorWrittenScrollTop)
    ) {
      this.deactivateUnreadAnchor();
    }

    const distFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;
    this.pinnedToBottom = distFromBottom < SCROLL_BOTTOM_THRESHOLD;
    this.scheduleScrollAnchorCapture();

    // A tap-opened (or right-clicked) context menu is positioned at a fixed
    // viewport point; once the thread scrolls it no longer points at the
    // message it targets, so dismiss it rather than leave it stranded. The
    // mobile sheet is not anchored to the message, so it stays open (a new
    // message arriving scrolls the list underneath it).
    if (this.contextMenuMessage && !this.contextMenuAsSheet) {
      this.closeContextMenu();
    }

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
    const loadId = this.fetchId;
    this.loadingOlder = true;
    const prevScrollHeight = scrollEl.scrollHeight;

    try {
      await this.fetchHistoryV2(this.nextCursor || undefined);
    } catch {
      // Silently fail for older messages
    } finally {
      // After a switch, the spinner and the height delta belong to the
      // conversation the user left.
      if (loadId === this.fetchId) {
        this.loadingOlder = false;
        await this.updateComplete;
        const newScrollHeight = scrollEl.scrollHeight;
        scrollEl.scrollTop += newScrollHeight - prevScrollHeight;
      }
    }
  }

  /** Whether the user is typing (or has a draft) in this thread's composer. */
  get isComposing(): boolean {
    const composer = this.shadowRoot?.querySelector('scion-chat-composer') as
      | import('./chat-composer.js').ScionChatComposer
      | null;
    return composer?.isComposing ?? false;
  }

  /**
   * The current scroll position as an anchor, for a chat page that is about
   * to be destroyed to hand on to its replacement. Null until the thread has
   * been scrolled (programmatically or by the user) at least once.
   */
  get scrollAnchor(): ChatScrollAnchor | null {
    const anchor = this._scrollAnchor;
    return anchor && anchor.conversationKey === this.conversationKey ? { ...anchor } : null;
  }

  private scheduleScrollAnchorCapture(): void {
    if (this._scrollAnchorRaf !== null) return;
    this._scrollAnchorRaf = requestAnimationFrame(() => {
      this._scrollAnchorRaf = null;
      this.captureScrollAnchor();
    });
  }

  private cancelScrollAnchorCapture(): void {
    if (this._scrollAnchorRaf === null) return;
    cancelAnimationFrame(this._scrollAnchorRaf);
    this._scrollAnchorRaf = null;
  }

  /** Record the topmost visible message and its offset from the top edge. */
  private captureScrollAnchor(): void {
    if (!this.isV2) return;
    const scrollEl = this.shadowRoot?.querySelector('.messages-scroll') as HTMLElement | null;
    if (!scrollEl) return;
    const containerRect = scrollEl.getBoundingClientRect();
    // Hidden (e.g. the outlet is hidden behind the terminal view): layout
    // reads are all zero and would overwrite a good anchor with nonsense.
    if (containerRect.height === 0) return;
    const rows = scrollEl.querySelectorAll<HTMLElement>('scion-chat-message[id^="msg-"]');
    const row = findTopVisibleRow(
      rows.length,
      (i) => {
        const el = rows[i];
        const rect = el.getBoundingClientRect();
        return { id: el.id.slice('msg-'.length), top: rect.top, bottom: rect.bottom };
      },
      containerRect.top
    );
    this._scrollAnchor = {
      conversationKey: this.conversationKey,
      pinnedToBottom: this.pinnedToBottom,
      messageId: row?.id ?? '',
      offset: row ? row.top - containerRect.top : 0,
    };
  }

  /**
   * The restore anchor for this conversation, once; null otherwise. Taking
   * it fires `scroll-restore-consumed` so the page stops offering it: a
   * thread element re-created later (closing search re-mounts it) must not
   * restore a position the user has long since moved on from.
   */
  private takeRestoreScrollAnchor(): ChatScrollAnchor | null {
    const anchor = this.restoreScrollAnchor;
    if (!anchor || anchor === this._usedRestoreAnchor) return null;
    if (anchor.conversationKey !== this.conversationKey) return null;
    this._usedRestoreAnchor = anchor;
    this.dispatchEvent(
      new CustomEvent<ChatScrollAnchor>('scroll-restore-consumed', { detail: anchor })
    );
    return anchor;
  }

  /**
   * Put the view back where a previous instance left it: at the bottom when
   * it was following new messages, otherwise with the anchor message at the
   * same offset — loading the history around it first if the latest page
   * does not include it. Falls back to the bottom if the message is gone.
   */
  private async restoreScrollPosition(anchor: ChatScrollAnchor): Promise<void> {
    const jumpSeq = this._jumpSeq;
    // Until a real capture replaces it, the restore target is this thread's
    // position: leaving while the restore is still loading (slow network)
    // must hand it on rather than lose it. Seeded here, not where the anchor
    // is taken, so an anchor used up by a jump is never handed on.
    this._scrollAnchor ??= { ...anchor };
    if (anchor.pinnedToBottom || !anchor.messageId) {
      this.pinnedToBottom = true;
      this.scrollToBottomAfterRender();
      return;
    }
    const fetchId = this.fetchId;
    // Not following the bottom: keep late loads (inter-agent exchanges)
    // from auto-scrolling down while the anchor is being located.
    this.pinnedToBottom = false;
    await this.updateComplete;
    let msgEl = this.shadowRoot?.getElementById(`msg-${anchor.messageId}`) ?? null;
    if (!msgEl) {
      await this.fetchAroundMessage(anchor.messageId, () => jumpSeq === this._jumpSeq);
      await this.updateComplete;
      msgEl = this.shadowRoot?.getElementById(`msg-${anchor.messageId}`) ?? null;
    }
    // Superseded by a thread switch or an explicit jump (e.g. a search
    // result in this conversation) made while the anchor was being located.
    if (fetchId !== this.fetchId || jumpSeq !== this._jumpSeq) return;
    if (!msgEl) {
      this.pinnedToBottom = true;
      this.scrollToBottomAfterRender();
      return;
    }
    const target = msgEl;
    /** Write the anchor's scrollTop; returns it as read back, or null. */
    const apply = (): number | null => {
      const scrollEl = this.shadowRoot?.querySelector('.messages-scroll') as HTMLElement | null;
      if (!scrollEl || !target.isConnected) return null;
      const containerRect = scrollEl.getBoundingClientRect();
      if (containerRect.height === 0) return null; // hidden: no layout to read
      scrollEl.scrollTop = scrollTopForAnchor(
        scrollEl.scrollTop,
        target.getBoundingClientRect().top,
        containerRect.top,
        anchor.offset
      );
      return scrollEl.scrollTop;
    };
    const written = apply();
    if (written !== null) this.watchRestoreSettle(apply, written);
  }

  /**
   * Rows can keep arriving or resizing for a moment after a restore (late
   * markdown, fonts, an agent DM's inter-agent exchanges loading), which
   * would drift the view off the anchor. Re-apply it on every resize of the
   * list for `RESTORE_SETTLE_MS`, stopping early the moment the user scrolls
   * (seen as `scrollTop` moving more than `RESTORE_SCROLL_TOLERANCE_PX`
   * from the value last written).
   */
  private watchRestoreSettle(apply: () => number | null, written: number): void {
    this.cancelRestoreSettleWatch();
    const list = this.shadowRoot?.querySelector('.messages-list');
    if (!list || typeof ResizeObserver === 'undefined') return;
    let last = written;
    const observer = new ResizeObserver(() => {
      const scrollEl = this.shadowRoot?.querySelector('.messages-scroll') as HTMLElement | null;
      if (!scrollEl || Math.abs(scrollEl.scrollTop - last) > RESTORE_SCROLL_TOLERANCE_PX) {
        this.cancelRestoreSettleWatch();
        return;
      }
      const next = apply();
      if (next !== null) last = next;
    });
    observer.observe(list);
    const timer = setTimeout(() => this.cancelRestoreSettleWatch(), RESTORE_SETTLE_MS);
    this._restoreSettleCleanup = (): void => {
      observer.disconnect();
      clearTimeout(timer);
    };
  }

  private cancelRestoreSettleWatch(): void {
    const cleanup = this._restoreSettleCleanup;
    this._restoreSettleCleanup = null;
    cleanup?.();
  }

  private scrollToBottom(): void {
    // A bottom-anchor scroll (autoscroll on new message, "Jump to latest")
    // supersedes any pending jump-to-message re-check — don't let a stale
    // correction yank the view back up once we've moved on.
    this.cancelJumpScrollWatch();
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

  private async handleJumpToLatest(): Promise<void> {
    // "Jump to latest" is explicit programmatic navigation too — same
    // precedence over the open-time unread anchor as scrollToMessageById.
    this.deactivateUnreadAnchor();
    if (this.viewingAroundMessage) {
      this.messageMap.clear();
      this.messages = [];
      this.v2AttachmentMap.clear();
      this.v2MessageExtMap.clear();
      this.v2ReplyPreviewMap.clear();
      this.nextCursor = null;
      this.hasOlderMessages = true;
      this.pinnedToBottom = true;

      try {
        // Switched away meanwhile: the new conversation keeps its own view.
        if (!(await this.fetchHistoryV2())) return;
        this.viewingAroundMessage = false;
      } catch (err) {
        this.error = err instanceof Error ? err.message : 'Failed to load messages';
        this.pinnedToBottom = false;
        return;
      }
    }

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
  async scrollToMessageById(messageId: string, highlight = true): Promise<void> {
    // Explicit programmatic navigation (search-jump, reply-jump, deep link)
    // takes precedence over the open-time unread anchor (rule 5). Without
    // this, a same-thread search-jump made inside the anchor window is
    // overridden the moment content resizes and the ResizeObserver re-anchors
    // to the divider (R1).
    this.deactivateUnreadAnchor();
    // It also outranks a restored position: use the anchor up if the load
    // has not taken it yet, and stop a restore that is still in flight.
    this.takeRestoreScrollAnchor();
    const jumpSeq = ++this._jumpSeq;
    const fetchId = this.fetchId;
    // A newer jump, or leaving the conversation, supersedes this one.
    const isCurrent = () => jumpSeq === this._jumpSeq && fetchId === this.fetchId;
    this.cancelRestoreSettleWatch();
    await this.updateComplete;
    if (!isCurrent()) return;

    // Looked up after the target: while the initial load is still running
    // the thread shows a spinner, not the scroll container.
    let msgEl = this.shadowRoot?.getElementById(`msg-${messageId}`) ?? null;
    if (!msgEl) {
      await this.fetchAroundMessage(messageId, isCurrent);
      await this.updateComplete;
      if (!isCurrent()) return;
      msgEl = this.shadowRoot?.getElementById(`msg-${messageId}`) ?? null;
    }
    const scrollEl = this.shadowRoot?.querySelector('.messages-scroll') as HTMLElement | null;
    if (!msgEl || !scrollEl) {
      this.handOpenScrollBack();
      return;
    }
    this._openScrollDeferred = false;
    this._landedJumpSeq = jumpSeq;

    const align: ScrollLogicalPosition = 'center';
    const containerRect = scrollEl.getBoundingClientRect();
    const targetRect = msgEl.getBoundingClientRect();
    // A jump that can't move the scroll position — the target is already in
    // view, or reaching it is clamped at an end of the thread (the common
    // reply-jump-while-pinned-to-bottom case) — never fires `scrollend`.
    // Arming the watcher anyway would leave it pending until the user's next
    // unrelated scroll, which it would then wrongly "correct" (review R1).
    const willScroll =
      !isJumpTargetInView(containerRect, targetRect) &&
      !isJumpScrollClamped(scrollEl, containerRect, targetRect);

    msgEl.scrollIntoView({ behavior: 'smooth', block: align });
    if (willScroll) {
      // A large layout shift mid-scroll (image load, late markdown,
      // attachment height) can leave the target off screen because the
      // smooth scroll's destination was computed when it started. Re-check
      // once it settles and correct if needed (#1749 review).
      this.watchJumpScrollSettle(scrollEl, messageId, align);
    } else {
      // Nothing to watch, but a previous jump's watcher — now superseded —
      // must still be torn down.
      this.cancelJumpScrollWatch();
    }
    if (highlight) {
      msgEl.classList.add('permalink-highlight');
      setTimeout(() => msgEl?.classList.remove('permalink-highlight'), 2000);
    }
  }

  /**
   * A jump that found no target gives the open-time scroll back: to the
   * initial load if it is still running, else straight to the bottom if the
   * load already left the view to this jump. Either way a later load (a
   * Retry after a failed one) no longer counts this jump as started.
   * Nothing to hand back once an earlier jump has landed since the open:
   * the view stays on it.
   */
  private handOpenScrollBack(): void {
    if (this._landedJumpSeq > this._jumpSeqAtOpen) return;
    this._jumpSeqAtOpen = this._jumpSeq;
    if (!this.loading && this._openScrollDeferred) {
      this._openScrollDeferred = false;
      this.pinnedToBottom = true;
      this.scrollToBottomAfterRender();
    }
  }

  /** Cancel any pending jump-to-message scrollend re-check. Idempotent. */
  private cancelJumpScrollWatch(): void {
    const cleanup = this._jumpScrollCleanup;
    this._jumpScrollCleanup = null;
    cleanup?.();
  }

  /**
   * Whether the `scrollend` event is supported in the current runtime.
   * Feature-detected on `window` (a single browser-wide capability, not a
   * per-element one) rather than on the scroll container: `onscrollend` is a
   * statically-known property of `HTMLElement` in our TS lib target, so
   * branching on `'onscrollend' in scrollEl` and then continuing to use
   * `scrollEl` would let TS narrow the fallback branch to `never`.
   *
   * A method (not an inlined check) so tests can stub the older-Safari path
   * without needing to fight jsdom/happy-dom's global handler properties.
   */
  private supportsScrollEndEvent(): boolean {
    return typeof window !== 'undefined' && 'onscrollend' in window;
  }

  /**
   * After starting a smooth jump-to-message scroll, wait for it to settle and
   * verify the target actually landed in view. If a mid-scroll layout shift
   * moved it, re-issue the scroll and check again, capped at
   * `JUMP_SCROLL_MAX_RECHECKS` so a target that can never settle (e.g. content
   * still streaming in) doesn't loop forever.
   *
   * "Settled" is detected via the `scrollend` event where supported. Older
   * Safari has no `scrollend`, so this falls back to polling `scrollTop` for
   * `JUMP_SCROLL_SETTLE_STABLE_MS` of no movement. Both paths, and the whole
   * watch, are additionally bounded by `JUMP_SCROLL_IDLE_TIMEOUT_MS` of
   * inactivity (no `scroll` event and no `scrollend`), restarted on every
   * `scroll`, plus a `JUMP_SCROLL_HARD_CAP_MS` backstop — a scroll that never
   * settles, or never scrolls at all, must not leave the watcher armed
   * indefinitely (review R1, R4).
   *
   * Cancelled by any subsequent manual scroll (wheel, touch, a scrollbar
   * grab/middle-click via `pointerdown`, or a scroll-relevant `keydown`
   * anywhere in the document — not just on the scroll container, since focus
   * often isn't there, and ignoring the same keys typed into an editable
   * field like the composer, review N3), another jump (scrollToMessageById
   * re-entry, or scrollToBottom for "Jump to latest" / autoscroll), or
   * thread switch/disconnect — never yanks the user back to a target they've
   * since scrolled away from on purpose.
   */
  private watchJumpScrollSettle(
    scrollEl: HTMLElement,
    messageId: string,
    align: ScrollLogicalPosition
  ): void {
    // Only one jump's re-check is ever pending at a time.
    this.cancelJumpScrollWatch();

    let recheckCount = 0;
    let pollTimer: ReturnType<typeof setTimeout> | null = null;
    let scrollEndListener: (() => void) | null = null;
    let idleTimer: ReturnType<typeof setTimeout> | null = null;
    let hardCapTimer: ReturnType<typeof setTimeout> | null = null;

    const onManualScroll = (): void => cleanup();
    const manualScrollEvents: Array<keyof HTMLElementEventMap> = [
      'wheel',
      'touchstart',
      'pointerdown',
    ];
    const onManualKeydown = (e: KeyboardEvent): void => {
      if (!JUMP_SCROLL_CANCEL_KEYS.has(e.key)) return;
      // Space and the arrow keys are ordinary typing in the composer (or any
      // other editable field), not a scroll gesture there — even though
      // they're in JUMP_SCROLL_CANCEL_KEYS for the document-wide case (review
      // N3).
      if (isEditableKeydownTarget(e.composedPath()[0])) return;
      cleanup();
    };
    // Not a cancel signal — Chromium fires `scroll` every frame of the
    // programmatic smooth scroll too — but activity all the same, so it
    // pushes the idle timeout back out (review R4).
    const onScrollActivity = (): void => scheduleIdleTimeout();

    const removeManualScrollListeners = (): void => {
      for (const type of manualScrollEvents) {
        scrollEl.removeEventListener(type, onManualScroll);
      }
      scrollEl.removeEventListener('scroll', onScrollActivity);
      document.removeEventListener('keydown', onManualKeydown, { capture: true });
    };

    const cleanup = (): void => {
      if (this._jumpScrollCleanup !== cleanup) return;
      this._jumpScrollCleanup = null;
      removeManualScrollListeners();
      if (scrollEndListener) {
        scrollEl.removeEventListener('scrollend', scrollEndListener);
        scrollEndListener = null;
      }
      if (pollTimer !== null) {
        clearTimeout(pollTimer);
        pollTimer = null;
      }
      if (idleTimer !== null) {
        clearTimeout(idleTimer);
        idleTimer = null;
      }
      if (hardCapTimer !== null) {
        clearTimeout(hardCapTimer);
        hardCapTimer = null;
      }
    };
    this._jumpScrollCleanup = cleanup;

    // Idle timeout (review R4): restarted on every `scroll` event, so a long
    // smooth scroll (which fires `scroll` every frame) stays watched for
    // however long it actually takes, instead of being torn down by a fixed
    // deadline before Chromium's `scrollend` fires. If no `scroll` and no
    // `scrollend` arrive within the window — the jump produced no scroll to
    // begin with, e.g. missed by the pre-scroll check — it fires and cleans
    // up, faster than the old fixed deadline did.
    const scheduleIdleTimeout = (): void => {
      if (idleTimer !== null) clearTimeout(idleTimer);
      idleTimer = setTimeout(() => cleanup(), JUMP_SCROLL_IDLE_TIMEOUT_MS);
    };

    for (const type of manualScrollEvents) {
      scrollEl.addEventListener(type, onManualScroll, { passive: true });
    }
    scrollEl.addEventListener('scroll', onScrollActivity, { passive: true });
    // Captured on `document`, not `scrollEl`: after clicking a message,
    // focus commonly lands on `<body>`, so a PageUp/PageDown/arrow `keydown`
    // never reaches scrollEl even though Chromium still scrolls it.
    document.addEventListener('keydown', onManualKeydown, { capture: true });

    // Hard cap (review R4): a generous backstop against a scroller that
    // never goes idle, so `scroll` events alone can't keep this watch armed
    // forever. The idle timeout above is what bounds the normal cases.
    hardCapTimer = setTimeout(() => cleanup(), JUMP_SCROLL_HARD_CAP_MS);

    const checkAndMaybeRescroll = (): void => {
      if (this._jumpScrollCleanup !== cleanup) return; // already cancelled
      const targetEl = this.shadowRoot?.getElementById(`msg-${messageId}`) ?? null;
      if (!targetEl) {
        cleanup();
        return;
      }
      const containerRect = scrollEl.getBoundingClientRect();
      const targetRect = targetEl.getBoundingClientRect();
      if (isJumpTargetInView(containerRect, targetRect)) {
        cleanup();
        return;
      }
      recheckCount++;
      if (recheckCount > JUMP_SCROLL_MAX_RECHECKS) {
        cleanup();
        return;
      }
      // Instant for the correction: it's already off screen, and stacking
      // another smooth animation only widens the window for a second shift.
      targetEl.scrollIntoView({ behavior: 'auto', block: align });
      scheduleSettleWait();
    };

    const scheduleSettleWait = (): void => {
      // Restart the idle window for this leg of the wait (the initial scroll,
      // or a re-check's corrective one): a fresh scrollIntoView here may or
      // may not itself produce `scroll` events, so don't rely solely on a
      // stale timer from an earlier leg.
      scheduleIdleTimeout();
      if (this.supportsScrollEndEvent()) {
        // Removed manually (also on cleanup) rather than `{ once: true }`:
        // cleanup() needs to be able to remove a still-pending listener
        // either way, so `once` would just be a second, redundant mechanism.
        // Bound to a local `const` (rather than reading back the mutable
        // `scrollEndListener` field) so TS narrows it to `() => void` here
        // without an `as EventListener` assertion.
        const listener = (): void => {
          scrollEl.removeEventListener('scrollend', listener);
          scrollEndListener = null;
          checkAndMaybeRescroll();
        };
        scrollEndListener = listener;
        scrollEl.addEventListener('scrollend', listener);
        return;
      }
      // Fallback: poll scrollTop until it's stable for
      // JUMP_SCROLL_SETTLE_STABLE_MS; the idle timeout and hard cap above
      // still bound this path too.
      let lastTop = scrollEl.scrollTop;
      let stableSince = Date.now();
      const poll = (): void => {
        const now = Date.now();
        const currentTop = scrollEl.scrollTop;
        if (currentTop !== lastTop) {
          lastTop = currentTop;
          stableSince = now;
        }
        if (now - stableSince >= JUMP_SCROLL_SETTLE_STABLE_MS) {
          pollTimer = null;
          checkAndMaybeRescroll();
          return;
        }
        pollTimer = setTimeout(poll, JUMP_SCROLL_SETTLE_POLL_INTERVAL_MS);
      };
      pollTimer = setTimeout(poll, JUMP_SCROLL_SETTLE_POLL_INTERVAL_MS);
    };

    scheduleSettleWait();
  }

  /**
   * `isCurrent`, when given, is re-checked once the response arrives: a
   * caller superseded in the meantime (a restore overtaken by a jump) must
   * not replace the window that the newer request loaded.
   */
  private async fetchAroundMessage(
    messageId: string,
    isCurrent: () => boolean = () => true
  ): Promise<void> {
    if (!this.conversationKey) return;

    const currentId = this.fetchId;
    const recentFilesGeneration = chatRecentFiles.scopeGeneration;
    const params = new URLSearchParams({
      around: messageId,
      limit: String(HISTORY_PAGE_SIZE),
    });

    try {
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/messages?${params.toString()}`
      );
      if (currentId !== this.fetchId || !res.ok || !isCurrent()) return;

      const data = (await res.json()) as {
        items?: Message[];
        messages?: Message[];
        nextCursor?: string;
        messageAttachments?: Record<string, import('./chat-message.js').AttachmentRefInfo[]>;
        messageExtensions?: Record<
          string,
          { messageId: string; replyToId?: string; editedAt?: string; deletedAt?: string }
        >;
        replyPreviews?: Record<string, { messageId: string; senderName: string; content: string }>;
      };
      if (currentId !== this.fetchId || !isCurrent()) return;

      const items = data.items ?? data.messages ?? [];
      this.messageMap.clear();
      this.v2AttachmentMap.clear();
      this.v2MessageExtMap.clear();
      this.v2ReplyPreviewMap.clear();

      for (const [msgId, refs] of Object.entries(data.messageAttachments ?? {})) {
        this.v2AttachmentMap.set(msgId, refs);
      }
      for (const [msgId, ext] of Object.entries(data.messageExtensions ?? {})) {
        this.v2MessageExtMap.set(msgId, ext);
      }
      for (const [msgId, preview] of Object.entries(data.replyPreviews ?? {})) {
        this.v2ReplyPreviewMap.set(msgId, preview);
      }

      this.nextCursor = data.nextCursor || null;
      this.hasOlderMessages = this.nextCursor !== null;
      this.viewingAroundMessage = true;
      this.pinnedToBottom = false;
      this.mergeMessages(items);
      this.recordRecentFilesForHistory(items, recentFilesGeneration);
    } catch (err) {
      console.error('Failed to fetch around message:', err);
    }
  }

  /**
   * Anchor the scroll position to the unread divider after render, so the
   * user opens the thread reading forward from their first unread message
   * instead of the tail.
   *
   * Only called once, at open time (initial load or a thread/conversationKey
   * switch) — never from the SSE/merge path, so a message arriving while the
   * thread is already open keeps the ordinary stick-to-bottom / "Jump to
   * latest" behavior instead of re-anchoring to the divider.
   */
  private scrollToUnreadDivider(): void {
    this._unreadAnchorActive = true;
    this._unreadAnchorMessageCount = this.messages.length;
    // Captured as a local, not stored back onto `this`: `fetchId` only bumps
    // on a thread switch, so if this open is superseded before this deferred
    // callback runs, a *new* call to scrollToUnreadDivider() would otherwise
    // overwrite a shared instance field with its own token before this one
    // fires, making the two indistinguishable. A local closure variable
    // keeps each open's token fixed to the value it had when scheduled.
    const openToken = this.fetchId;
    void this.updateComplete.then(() => {
      // One more frame past updateComplete: images, attachments and markdown
      // inside already-rendered rows can still change height after Lit has
      // committed the DOM, and reading geometry too early re-introduces the
      // jump-to-bottom-then-up flash this anchor exists to avoid.
      this._unreadAnchorInitialRaf = requestAnimationFrame(() => {
        this._unreadAnchorInitialRaf = null;
        if (!this._unreadAnchorActive || this.fetchId !== openToken) return;
        this.applyUnreadAnchor();
        this.observeUnreadAnchorLayout(openToken);
      });
    });
  }

  /**
   * Compute and apply the anchor scroll position: the unread divider's top
   * edge, minus a small margin, clamped to the container's maximum scroll so
   * unread content shorter than the viewport lands at the bottom instead of
   * over-scrolling.
   */
  private applyUnreadAnchor(): void {
    const scrollEl = this.shadowRoot?.querySelector('.messages-scroll') as HTMLElement | null;
    if (!scrollEl) return;

    const divider = this.shadowRoot?.querySelector('.unread-divider') as HTMLElement | null;
    if (!divider) {
      // The divider isn't in the DOM (e.g. the unread message was since
      // removed) — fall back to the ordinary bottom anchor.
      this.scrollToBottom();
      return;
    }

    const maxScroll = Math.max(0, scrollEl.scrollHeight - scrollEl.clientHeight);
    const target = Math.min(maxScroll, Math.max(0, divider.offsetTop - UNREAD_ANCHOR_MARGIN_PX));

    // Reflect where this leaves the user directly, rather than waiting on the
    // 'scroll' event this write triggers to recompute it: that event is
    // asynchronous, and an SSE message landing in the gap must already see
    // the correct state to decide whether to stick-to-bottom or show "Jump to
    // latest". Only the clamped-to-bottom case (behavior #3) counts as pinned.
    this.pinnedToBottom = target >= maxScroll;

    this._applyingUnreadAnchor = true;
    scrollEl.scrollTop = target;
    // Read back the value the browser actually committed (it may clamp
    // differently than our own `maxScroll` computation) so handleScroll can
    // tell our write apart from a real user scroll landing in the same frame.
    this._unreadAnchorWrittenScrollTop = scrollEl.scrollTop;
    // The 'scroll' event this write triggers lands asynchronously in real
    // browsers; clear the guard on the next frame rather than synchronously
    // so handleScroll still ignores it, and a genuine user scroll shortly
    // after is still detected as manual.
    //
    // Rapid successive calls (e.g. ResizeObserver firing twice in one frame
    // window) must not leave an earlier rAF racing this one: if it fired
    // first, it would clear the guard while this write's own 'scroll' event
    // is still pending, and that event could then be misread as a user
    // scroll. Canceling any previous one keeps exactly one guard-clearing
    // rAF alive at a time, tied to the most recent write.
    if (this._applyingUnreadAnchorRaf !== null) {
      cancelAnimationFrame(this._applyingUnreadAnchorRaf);
    }
    this._applyingUnreadAnchorRaf = requestAnimationFrame(() => {
      this._applyingUnreadAnchorRaf = null;
      this._applyingUnreadAnchor = false;
    });
  }

  /**
   * Re-apply the anchor while content above the divider is still settling,
   * for a short, bounded window. Stops itself — rather than fighting the live
   * thread indefinitely — the moment the message count changes (a real
   * message arrived, not a layout shift) or the window elapses.
   *
   * `openToken` is the `fetchId` this anchor belongs to, captured by the
   * caller — see the comment in `scrollToUnreadDivider`. It guards this
   * closure the same way against firing for a thread already left behind.
   */
  private observeUnreadAnchorLayout(openToken: number): void {
    if (typeof ResizeObserver === 'undefined') return;
    const list = this.shadowRoot?.querySelector('.messages-list');
    if (!list) return;

    this._unreadAnchorResizeObserver?.disconnect();
    this._unreadAnchorResizeObserver = new ResizeObserver(() => {
      if (!this._unreadAnchorActive || this.fetchId !== openToken) return;
      if (this.messages.length !== this._unreadAnchorMessageCount) {
        // New content, not late layout of the unread content itself — leave
        // it to the ordinary stick-to-bottom / "Jump to latest" behavior.
        this.deactivateUnreadAnchor();
        return;
      }
      this.applyUnreadAnchor();
    });
    this._unreadAnchorResizeObserver.observe(list);

    if (this._unreadAnchorTimer) clearTimeout(this._unreadAnchorTimer);
    this._unreadAnchorTimer = setTimeout(() => {
      this.deactivateUnreadAnchor();
    }, UNREAD_ANCHOR_WINDOW_MS);
  }

  /** Stop correcting the unread-divider anchor and tear down its watchers. */
  private deactivateUnreadAnchor(): void {
    this._unreadAnchorActive = false;
    this._applyingUnreadAnchor = false;
    if (this._unreadAnchorResizeObserver) {
      this._unreadAnchorResizeObserver.disconnect();
      this._unreadAnchorResizeObserver = null;
    }
    if (this._unreadAnchorTimer) {
      clearTimeout(this._unreadAnchorTimer);
      this._unreadAnchorTimer = null;
    }
    if (this._unreadAnchorInitialRaf !== null) {
      cancelAnimationFrame(this._unreadAnchorInitialRaf);
      this._unreadAnchorInitialRaf = null;
    }
    if (this._applyingUnreadAnchorRaf !== null) {
      cancelAnimationFrame(this._applyingUnreadAnchorRaf);
      this._applyingUnreadAnchorRaf = null;
    }
  }

  // ---------------------------------------------------------------------------
  // Phase-5: Context menu
  // ---------------------------------------------------------------------------

  /** The actions of a message's menu, shared by the popup and the sheet. */
  private messageMenuActions(msg: Message): MenuAction[] {
    const isOwnMessage = msg.senderId === (this._currentUserId || this.currentUserId);
    const canEditDelete = isOwnMessage && !this.hasAgentReplyAfter(msg);
    const actions: MenuAction[] = [
      { id: 'reply', label: 'Reply', icon: 'reply', run: () => this.handleContextMenuReply() },
    ];
    if (canEditDelete) {
      actions.push(
        { id: 'edit', label: 'Edit', icon: 'pencil', run: () => this.handleContextMenuEdit() },
        {
          id: 'delete',
          label: 'Delete',
          icon: 'trash',
          destructive: true,
          run: () => void this.handleContextMenuDelete(),
        }
      );
    }
    actions.push(
      {
        id: 'copy-text',
        label: 'Copy text',
        icon: 'clipboard',
        run: () => this.handleContextMenuCopyText(),
      },
      {
        id: 'copy-link',
        label: 'Copy link',
        icon: 'link-45deg',
        run: () => this.handleContextMenuCopyLink(),
      }
    );
    if (
      this.isSenderAgent(msg) &&
      !this.isDM &&
      !(msg.sender.startsWith('agent:') && msg.sender.slice(6) === this.defaultAgent)
    ) {
      actions.push({
        id: 'set-default-agent',
        label: 'Make this agent thread default',
        icon: 'robot',
        run: () => void this.handleContextMenuSetDefault(),
      });
    }
    if (this.isSenderAgent(msg)) actions.push(...this.agentMenuActions(msg));
    return actions;
  }

  /** Render the context menu when a message is right-clicked or tapped. */
  private renderContextMenu() {
    if (!this.contextMenuMessage || this.contextMenuAsSheet) return nothing;
    return html`
      <div class="context-menu-overlay" @click=${this.closeContextMenu}></div>
      <div class="context-menu">
        ${renderMenuRows(this.messageMenuActions(this.contextMenuMessage))}
      </div>
    `;
  }

  /** The mobile presentation of the message menu. */
  private renderContextMenuSheet() {
    const msg = this.contextMenuAsSheet ? this.contextMenuMessage : null;
    return html`
      <scion-action-sheet
        .items=${msg ? this.messageMenuActions(msg) : []}
        heading=${msg ? this.getSenderDisplayName(msg) || msg.sender : ''}
        .open=${msg !== null}
        @action-sheet-select=${(e: CustomEvent<ActionSheetSelectDetail>): void => {
          if (this.contextMenuMessage) {
            runMenuAction(this.messageMenuActions(this.contextMenuMessage), e.detail.id);
          }
        }}
        @action-sheet-close=${(): void => this.closeContextMenu()}
      ></scion-action-sheet>
    `;
  }

  /**
   * "Open terminal" / "Open in graph" menu actions for the message's
   * author agent — the same icons, labels and actions as the toolbar's
   * `renderAgentToolbarButtons` (pages/chat.ts) and the members sidebar's
   * `renderAgent` (chat-members.ts), scoped to the author of this message
   * rather than the thread's default agent or DM peer.
   *
   * Terminal is gated on the author being a current roster member with
   * `canAttach === true`, fail-closed exactly like the sidebar: absent,
   * false, or the agent missing from `agentMembers` altogether (e.g. it left
   * the space or was deleted — chat.ts drops deleted agents from that list)
   * all hide the item rather than offering a control the server would
   * refuse.
   *
   * Graph does not require a roster entry: it's gated only on a resolvable
   * project id, which `senderProjectId` (#1706, cross-project messaging)
   * supplies even for a departed author — the graph page can still show that
   * project and the agent's history. An empty `senderId` hides both
   * regardless (see `resolveAgentActionProjectId`): `isSenderAgent` can
   * classify a message as agent-authored by `type` alone, with no id to act
   * on.
   */
  private agentMenuActions(msg: Message): MenuAction[] {
    if (!msg.senderId) return [];
    const member = this.agentMembers.find((m) => m.id === msg.senderId);
    const projectId = this.resolveAgentActionProjectId(msg);
    const actions: MenuAction[] = [];
    if (member?.canAttach === true) {
      actions.push({
        id: 'open-terminal',
        label: 'Open terminal',
        icon: 'terminal',
        run: () => this.handleContextMenuOpenTerminal(),
      });
    }
    if (projectId) {
      actions.push({
        id: 'open-graph',
        label: 'Open in graph',
        icon: 'diagram-3',
        run: () => this.handleContextMenuOpenGraph(),
      });
    }
    return actions;
  }

  /**
   * Project id for the author agent's graph/terminal actions. Prefers the
   * server-derived `senderProjectId` — the only signal that's correct when
   * the author belongs to a different project than this conversation, or
   * has since left the roster entirely — falling back to the roster's
   * per-agent `projectId`. Empty when `senderId` is empty — there is no
   * agent to focus the graph on.
   *
   * `senderProjectId` is not set on every agent-authored row: the
   * agent-to-user outbound path never sets it (same gap
   * `resolvePathLinkProjectId` documents), so this is current behaviour for
   * those messages, not just history. Those rows do carry the message's own
   * `projectId` — the sending agent's project — so it comes next in the
   * chain, ahead of the thread fallback: it stays correct even for a
   * departed, cross-project author.
   *
   * Only once all three are empty does a project-scoped (non-DM) thread's
   * own `projectId` kick in, as a last resort: unlike a DM's `projectId`
   * (see `resolvePathLinkProjectId`, which is only `inheritedProjectId()`
   * and unrelated to the conversation), a group thread's `projectId` is the
   * project the conversation itself belongs to. This keeps "Open in graph"
   * available instead of hiding it outright, at the cost of being a best
   * guess rather than a guarantee for the rare row with no project of its
   * own.
   */
  private resolveAgentActionProjectId(msg: Message): string {
    if (!msg.senderId) return '';
    const member = this.agentMembers.find((m) => m.id === msg.senderId);
    return (
      msg.senderProjectId ||
      member?.projectId ||
      msg.projectId ||
      (!this.isDM ? this.projectId : '')
    );
  }

  /** Handle right-click on a message to show context menu. */
  private handleMessageContextMenu(e: MouseEvent, msg: Message): void {
    e.preventDefault();
    this.contextMenuMessage = msg;
    this.contextMenuPosition = { x: e.clientX, y: e.clientY };
    this.contextMenuAsSheet = shouldUseMenuSheet();
    document.addEventListener('keydown', this.handleContextMenuKeydown);
  }

  /**
   * Touch devices have no `:hover` state to reveal message actions, and a
   * long-press (which would otherwise fire `contextmenu`) is consumed by
   * iOS's native text-selection gesture instead. A plain tap opens the same
   * context menu a desktop right-click would, positioned at the tap point,
   * so touch users have a reachable path to reply/edit/delete/copy.
   */
  private handleMessageTap(e: MouseEvent, msg: Message): void {
    // Desktop already has hover-revealed actions and a working right-click
    // menu — only intervene on devices that cannot hover.
    if (!window.matchMedia('(hover: none)').matches) return;

    // <scion-chat-message> renders into its own shadow root, so `e.target`
    // here is retargeted to the message host itself regardless of what was
    // actually clicked inside it. `composedPath()[0]` is the real innermost
    // element, which is what "was a link/button tapped?" needs to inspect.
    const target = e.composedPath()[0] as HTMLElement;
    if (target.closest?.('a, button, sl-icon-button, .entity-link, .mention, .reply-preview'))
      return;

    this.handleMessageContextMenu(e, msg);
  }

  /** Dismiss context menu on Escape key. */
  private handleContextMenuKeydown = (e: KeyboardEvent): void => {
    if (e.key === 'Escape') {
      this.closeContextMenu();
    }
  };

  /** Close the context menu. */
  private closeContextMenu(): void {
    this.contextMenuMessage = null;
    document.removeEventListener('keydown', this.handleContextMenuKeydown);
  }

  /** Context menu: Reply to the right-clicked message. */
  private handleContextMenuReply(): void {
    const msg = this.contextMenuMessage;
    this.closeContextMenu();
    if (!msg) return;
    this.composerEditMessage = null; // Cancel any pending edit
    this.composerReplyTo = {
      messageId: msg.id,
      senderName: this.getSenderDisplayName(msg) || msg.sender,
      content: msg.msg.length > 100 ? msg.msg.slice(0, 100) + '...' : msg.msg,
    };
  }

  /** Context menu: Edit the right-clicked message. */
  private handleContextMenuEdit(): void {
    const msg = this.contextMenuMessage;
    this.closeContextMenu();
    if (!msg) return;
    this.composerReplyTo = null; // Cancel any pending reply
    this.composerEditMessage = { messageId: msg.id, content: msg.msg };
  }

  /** Context menu: Delete the right-clicked message. */
  private async handleContextMenuDelete(): Promise<void> {
    const msg = this.contextMenuMessage;
    this.closeContextMenu();
    if (!msg) return;

    const confirmed = window.confirm('Delete this message? This cannot be undone.');
    if (!confirmed) return;

    try {
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/messages/${encodeURIComponent(msg.id)}`,
        { method: 'DELETE' }
      );
      if (!res.ok) {
        const errMsg = await extractApiError(res, 'Failed to delete message');
        this.sendError = errMsg;
      }
    } catch (err) {
      this.sendError = err instanceof Error ? err.message : 'Failed to delete message';
    }
  }

  /** Context menu: Copy message text to clipboard. */
  private handleContextMenuCopyText(): void {
    const msg = this.contextMenuMessage;
    this.closeContextMenu();
    if (!msg) return;
    navigator.clipboard.writeText(msg.msg).catch(() => {
      // Fallback: ignore clipboard failure silently.
    });
  }

  /** Context menu: Copy link to message. */
  private handleContextMenuCopyLink(): void {
    const msg = this.contextMenuMessage;
    this.closeContextMenu();
    if (!msg) return;
    const url = `${window.location.origin}${window.location.pathname}#msg-${encodeURIComponent(msg.id)}`;
    navigator.clipboard.writeText(url).catch(() => {
      // Fallback: ignore clipboard failure silently.
    });
  }

  /** Context menu: Set the sender agent as the thread default. */
  private async handleContextMenuSetDefault(): Promise<void> {
    const msg = this.contextMenuMessage;
    this.closeContextMenu();
    if (!msg || !this.conversationKey || this.isDM) return;

    // Extract agent slug from sender (strip "agent:" prefix).
    // Guard: only proceed if the sender uses the "agent:" format.
    if (!msg.sender.startsWith('agent:')) return;
    const agentSlug = msg.sender.slice(6);
    if (!agentSlug) return;

    try {
      const body: Record<string, unknown> = {
        defaultAgent: agentSlug,
      };
      const res = await apiFetch(
        `/api/v1/chat/topics/${encodeURIComponent(this.conversationKey)}`,
        {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        }
      );
      if (res.ok) {
        this.defaultAgent = agentSlug;
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

  /** Context menu: Open a terminal for the message's author agent. */
  private handleContextMenuOpenTerminal(): void {
    const msg = this.contextMenuMessage;
    this.closeContextMenu();
    if (!msg) return;
    openTerminal(msg.senderId);
  }

  /** Context menu: Open the message's author agent in the dependency graph. */
  private handleContextMenuOpenGraph(): void {
    const msg = this.contextMenuMessage;
    this.closeContextMenu();
    if (!msg) return;
    const projectId = this.resolveAgentActionProjectId(msg);
    if (!projectId) return;
    navigateTo(agentGraphHref(projectId, msg.senderId));
  }

  // ---------------------------------------------------------------------------
  // Path-link file preview (#1148)
  // ---------------------------------------------------------------------------

  /**
   * Resolve the best-available project id for a path-link click.
   *
   * For project-scoped threads, the thread's own `projectId` is correct and
   * takes priority. But a DM's thread-level `projectId` is not a project the
   * DM belongs to — it's whatever project the user happened to be viewing
   * before opening the DM (`inheritedProjectId()`), kept around only for
   * attachment uploads. Using it here would resolve links against an
   * unrelated project (silently opening the wrong `/workspace` file, or
   * 404ing on a scratchpad path that project doesn't declare). So in a DM we
   * never fall back to it. Order of preference in a DM:
   *
   *   1. The message's own project — `senderProjectId` (server-derived) or
   *      `projectId` (set on the agent-to-user outbound path, which never
   *      sets `senderProjectId`; also the hub's user-to-agent send path
   *      stamps this from the peer agent's project, so a persisted or
   *      SSE-delivered message in an agent DM always carries it).
   *   2. The DM peer agent's project, read from the shared agent cache
   *      (`stateManager`, for the current view scope — cleared on every
   *      `setScope()` and re-seeded by the chat page's member list — no
   *      extra fetch). This only fills a narrow, real gap: the client's
   *      own optimistic message (`optimisticMsg.projectId = ''` above) has
   *      no project yet because it hasn't round-tripped the server. The
   *      peer agent is not an unrelated project — it is who the DM is with.
   *   3. Nothing — return '' so the caller shows the "could not determine
   *      which project" error instead of guessing at an unrelated project.
   */
  private resolvePathLinkProjectId(msg: Message | undefined): string {
    return resolveMessageProjectId({
      isDM: this.isDM,
      threadProjectId: this.projectId,
      ...(msg?.senderProjectId ? { senderProjectId: msg.senderProjectId } : {}),
      ...(msg?.projectId ? { messageProjectId: msg.projectId } : {}),
      peerAgentProjectId: this.peerAgentProjectId(),
    });
  }

  /**
   * Feed one admitted message's attachments and detected container paths to
   * the recent-files recorder. `refs` are the modern (W7) attachment refs
   * already resolved for this message; historical wave-1 paths live on
   * `msg.attachments`.
   *
   * Never called for drafts, failed sends, or the client's own optimistic
   * placeholder — every call site below only reaches this once a message is
   * either loaded from history or accepted by the server.
   */
  private recordRecentFiles(
    msg: Message,
    refs: import('./chat-message.js').AttachmentRefInfo[],
    opts: {
      provisional?: boolean;
      scopeGeneration?: number;
      /**
       * Conversation key to record under, snapshotted by the caller before
       * an `await` that could let `this.conversationKey` move on to a
       * different conversation. Defaults to the live value for callers with
       * no such gap (e.g. the synchronous SSE hook).
       */
      conversationKey?: string;
      /**
       * Resolved project id to record under, snapshotted the same way (and
       * for the same reason) as `conversationKey`. `''` is a valid, explicit
       * "no project resolved" — distinct from omitting the option, which
       * falls back to resolving fresh from `msg`.
       */
      projectId?: string;
    } = {}
  ): void {
    const conversationKey = opts.conversationKey ?? this.conversationKey;
    const projectId =
      opts.projectId !== undefined ? opts.projectId : this.resolvePathLinkProjectId(msg);
    chatRecentFiles.ingest(
      {
        id: msg.id,
        conversationKey,
        sentAt: msg.createdAt,
        text: msg.msg,
        ...(msg.attachments && msg.attachments.length > 0
          ? { legacyAttachmentPaths: msg.attachments }
          : {}),
      },
      refs,
      {
        ...(projectId ? { projectId } : {}),
        ...(opts.provisional ? { provisional: true } : {}),
      },
      opts.scopeGeneration !== undefined ? { scopeGeneration: opts.scopeGeneration } : {}
    );
  }

  /**
   * Apply {@link recordRecentFiles} to every message in a history/backfill/
   * around-message page. Excludes `type === 'mention'` fan-out copies — the
   * same filter `mergeMessages`/the message getters apply before display —
   * so a recorded file's provenance never points at a message id the thread
   * itself never shows.
   */
  private recordRecentFilesForHistory(items: Message[], scopeGeneration: number): void {
    for (const msg of items) {
      if (msg.type === 'mention') continue;
      this.recordRecentFiles(msg, this.getMessageAttachmentRefs(msg.id), { scopeGeneration });
    }
  }

  /**
   * The DM peer agent's project id, from the shared in-memory agent cache
   * (no network call). Empty when this isn't an agent DM or the peer agent
   * isn't in the cache.
   */
  private peerAgentProjectId(): string {
    if (!this.isAgentDM) return '';
    const peerAgentId = this.conversationKey.split(':')[2] || '';
    return (peerAgentId && stateManager.getAgent(peerAgentId)?.projectId) || '';
  }

  /**
   * Handle path-link-click event from a chat message. Resolution and
   * validation are synchronous now that loading/error state moved into
   * <scion-chat-file-preview>; this only ever sets which path to preview.
   */
  private handlePathLinkClick(e: CustomEvent<{ path: string }>, msg?: Message): void {
    const containerPath = e.detail.path;
    const resolvedProjectId = this.resolvePathLinkProjectId(msg);

    if (!resolvedProjectId) {
      this.sendError = PATH_LINK_NO_PROJECT_ERROR;
      setTimeout(() => {
        if (this.sendError === PATH_LINK_NO_PROJECT_ERROR) {
          this.sendError = null;
        }
      }, 4000);
      return;
    }

    const target = parseContainerPath(containerPath);
    if (!target) {
      this.sendError = 'Cannot open file: unrecognized path format';
      setTimeout(() => {
        if (this.sendError === 'Cannot open file: unrecognized path format') {
          this.sendError = null;
        }
      }, 4000);
      return;
    }

    const fileName = containerPath.split('/').pop() || containerPath;

    // Loading/error/image/binary state lives inside the reusable
    // <scion-chat-file-preview>; this component only owns which path is
    // being previewed.
    this.filePreview = {
      kind: 'path',
      projectId: resolvedProjectId,
      containerPath,
      location: target,
      name: fileName,
    };
  }

  /**
   * Handle gcs-link-click event from a chat message. Unlike a path-link
   * click, there is no project resolution and no further validation here:
   * chat-message already parsed and validated the URI, and the hub derives
   * every authorization decision itself from the message id.
   */
  private handleGcsLinkClick(
    e: CustomEvent<{ bucket: string; object: string; name: string; messageId: string }>
  ): void {
    const { bucket, object, name, messageId } = e.detail;
    this.filePreview = { kind: 'gcs', messageId, bucket, object, name };
  }

  /** Close the file preview dialog. */
  private closeFilePreview(): void {
    this.filePreview = null;
  }

  /** Render the file preview overlay dialog. */
  private renderFilePreview() {
    if (!this.filePreview) return nothing;
    return html`
      <scion-chat-file-preview
        .target=${this.filePreview}
        @chat-file-preview-close=${() => this.closeFilePreview()}
      ></scion-chat-file-preview>
    `;
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

  /**
   * /status — Fetch agent status for the project.
   *
   * Like /stop, a chat-page DM resolves the peer agent's project via
   * `peerAgentProjectId()`, since `this.projectId` there is only the
   * inherited project. Non-DM threads use `this.projectId`.
   */
  private async handleSlashStatus(): Promise<void> {
    const projectId = this.isDM ? this.peerAgentProjectId() : this.projectId;
    if (!projectId) {
      this.insertLocalSystemMessage('No project context available.');
      return;
    }

    // GET /api/v1/agents filters on `projectId` and returns
    // `{ agents, nextCursor }`. Pages are capped server-side, so follow
    // `nextCursor` until it is empty. The cursor is bound to the request's
    // filter, so every page must repeat the same `projectId`.
    const base = `/api/v1/agents?projectId=${encodeURIComponent(projectId)}`;
    const agents: Agent[] = [];
    const seenCursors = new Set<string>();
    let cursor = '';
    let pages = 0;

    try {
      do {
        const url = cursor ? `${base}&cursor=${encodeURIComponent(cursor)}` : base;
        const res = await apiFetch(url);
        if (!res.ok) {
          this.insertLocalSystemMessage('Failed to fetch project status.');
          return;
        }
        const data = (await res.json()) as { agents?: Agent[]; nextCursor?: string } | null;
        if (Array.isArray(data?.agents)) agents.push(...data.agents);
        const next = typeof data?.nextCursor === 'string' ? data.nextCursor : '';
        if (next && seenCursors.has(next)) {
          this.insertLocalSystemMessage('Failed to fetch project status.');
          return;
        }
        if (next) seenCursors.add(next);
        cursor = next;
        pages++;
      } while (cursor && pages < MAX_STATUS_AGENT_PAGES);

      if (agents.length === 0) {
        this.insertLocalSystemMessage('No agents found in this project.');
        return;
      }

      const lines = agents.map((a) => {
        const slug = a.slug || a.name || 'unknown';
        const phase = a.phase || 'unknown';
        return `  ${slug}: ${phase}`;
      });
      if (cursor) lines.push('  … (list truncated)');
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
      '  /spawn <template> [name] — Spawn a new agent from a template',
      '  /stop <agent> — Stop a running agent',
      '  /default <agent|clear> — Set or clear the thread default agent',
    ].join('\n');
    this.insertLocalSystemMessage(helpText);
  }

  /**
   * /spawn <template> [name] — Create and start a new agent.
   *
   * The hub's create handler requires both `name` and `projectId`, so an
   * omitted name defaults to the template plus a short random suffix (the
   * hub rejects a name already taken in the project; see
   * `defaultSpawnName`). The response wraps the created agent as
   * `{ agent }`.
   *
   * Like /stop, a DM resolves the peer agent's project: a DM's
   * `this.projectId` is only the inherited project (whatever the user was
   * viewing before opening the DM), so spawning there would create the
   * agent in an unrelated project.
   */
  private async handleSlashSpawn(args: string): Promise<void> {
    const parts = args.trim().split(/\s+/).filter(Boolean);
    const template = parts[0];
    if (!template || parts.length > 2) {
      this.insertLocalSystemMessage('Usage: /spawn <template> [name]');
      return;
    }
    const name = parts[1] || defaultSpawnName(template);

    const projectId = this.isDM ? this.peerAgentProjectId() : this.projectId;
    if (!projectId) {
      this.insertLocalSystemMessage('No project context available.');
      return;
    }

    try {
      const body = { name, projectId, template };
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

      const data = (await res.json()) as { agent?: { name?: string; slug?: string } };
      const spawned = data?.agent?.slug || data?.agent?.name || name;
      this.insertLocalSystemMessage(`Agent "${spawned}" spawned successfully.`);
    } catch (err) {
      this.insertLocalSystemMessage(
        `Failed to spawn agent: ${err instanceof Error ? err.message : 'unknown error'}`
      );
    }
  }

  /**
   * /stop <agent> — Stop a running agent.
   *
   * In a chat-page DM, `this.projectId` is only the inherited project
   * (whatever the user was viewing before opening the DM), so DMs
   * resolve the agent's project via `peerAgentProjectId()` instead.
   * Non-DM threads use `this.projectId`, which is the thread's real
   * project.
   */
  private async handleSlashStop(args: string): Promise<void> {
    const agentSlug = args.trim();
    if (!agentSlug) {
      this.insertLocalSystemMessage('Usage: /stop <agent-slug>');
      return;
    }

    const projectId = this.isDM ? this.peerAgentProjectId() : this.projectId;
    if (!projectId) {
      this.insertLocalSystemMessage('No project context available.');
      return;
    }

    try {
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(projectId)}/agents/${encodeURIComponent(agentSlug)}/stop`,
        { method: 'POST' }
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
        focusElement(slTextarea as HTMLElement);
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
        ${this.renderContent()} ${this.renderSendError()}
        ${this.canSend
          ? html`
              <scion-chat-composer
                .agents=${this.agents}
                @chat-send=${this.handleChatSend}
              ></scion-chat-composer>
            `
          : nothing}
        ${this.renderFilePreview()}
      </div>
    `;
  }

  /**
   * The send error. On a desktop layout, the full text, as it always was.
   * On a phone or tablet it is one line with an ellipsis, so its height
   * never changes with the frame; when the text is actually cut it is a
   * button that expands to the full text (its title carries it too).
   */
  private renderSendError(): TemplateResult | typeof nothing {
    if (!this.sendError) return nothing;
    const expandable =
      this._composerRoom.capped && (this.sendErrorTruncated || this.sendErrorExpanded);
    if (!expandable) return html`<div class="send-error">${this.sendError}</div>`;
    return html`
      <button
        type="button"
        class="send-error"
        title=${this.sendError}
        aria-expanded=${this.sendErrorExpanded ? 'true' : 'false'}
        ?data-expanded=${this.sendErrorExpanded}
        @click=${(): void => {
          this.sendErrorExpanded = !this.sendErrorExpanded;
        }}
      >
        ${this.sendError}
      </button>
    `;
  }

  /** On a phone or tablet, whether the one-line send error cuts its text. */
  private measureSendErrorTruncation(): void {
    if (!this._composerRoom.capped || !this.sendError || this.sendErrorExpanded) return;
    const el = this.shadowRoot?.querySelector<HTMLElement>('.send-error');
    if (!el) return;
    const truncated = el.scrollWidth > el.clientWidth + 1;
    if (truncated !== this.sendErrorTruncated) this.sendErrorTruncated = truncated;
  }

  /** Watches the send error's width on a phone or tablet (rotation, panels). */
  private _sendErrorObserver: ResizeObserver | null = null;
  private _observedSendError: Element | null = null;

  /**
   * Re-measure the send error's truncation whenever its width changes, not
   * only when the thread renders: a rotation can make a fitting error cut,
   * or a cut one fit.
   */
  private observeSendError(): void {
    const el = this._composerRoom.capped
      ? (this.shadowRoot?.querySelector('.send-error') ?? null)
      : null;
    if (el === this._observedSendError) return;
    this._sendErrorObserver?.disconnect();
    this._observedSendError = el;
    if (!el || typeof ResizeObserver === 'undefined') return;
    this._sendErrorObserver ??= new ResizeObserver(() => this.measureSendErrorTruncation());
    this._sendErrorObserver.observe(el);
  }

  /**
   * The message list (or the state message in its place) and the typing
   * indicator. On a desktop layout the indicator follows the list, as it
   * always did. On a phone or tablet it is the list's last item (see
   * renderContent()); with no list (the empty, loading and error states) it
   * shares a column with the state message, at its foot, and the composer
   * sizes its field from that column, so a typist arriving takes room from
   * the state message, never from the field.
   */
  private renderContentAndTyping(): TemplateResult {
    if (!this._composerRoom.capped) {
      return html`${this.renderContent()} ${this.renderTypingIndicator()}`;
    }
    if (!this.showsStateMessage) return html`${this.renderContent()}`;
    return html`<div class="state-area">
      ${this.renderContent()} ${this.renderTypingIndicator()}
    </div>`;
  }

  private renderV2() {
    return html`
      <div class="thread-container">
        ${this.renderInteragentToggle()} ${this.renderContentAndTyping()} ${this.renderSendError()}
        <scion-chat-composer
          .agents=${this.agents}
          .members=${this.members}
          .defaultAgent=${this.defaultAgent}
          .conversationMode=${this.isDM ? 'dm' : 'thread'}
          .peerName=${this.peerName}
          .projectId=${this.projectId}
          .conversationKey=${this.conversationKey}
          .replyTo=${this.composerReplyTo}
          .editMessage=${this.composerEditMessage}
          @chat-cancel-reply=${this.handleComposerCancelReply}
          @chat-cancel-edit=${this.handleComposerCancelEdit}
          @chat-send=${this.handleChatSendV2}
          @chat-edit=${this.handleChatEditV2}
          @chat-typing=${() => this.sendTypingEvent()}
          @default-agent-change=${this.handleDefaultAgentChange}
          @chat-slash-command=${this.handleSlashCommand}
        ></scion-chat-composer>
        ${this.renderContextMenu()} ${this.renderContextMenuSheet()} ${this.renderFilePreview()}
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

  /**
   * Whether renderContent() shows a state message (empty, loading or load
   * error) in place of the message list.
   */
  private get showsStateMessage(): boolean {
    if (this.messages.length > 0) return false;
    return this.loading || !!this.error || !this.hasInteragentMessages;
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
      <div
        class="messages-scroll"
        @scroll=${this.handleScroll}
        @click=${this.handleMessageAreaClick}
      >
        <div class="messages-list">
          ${this.loadingOlder
            ? html`<div class="loading-older"><sl-spinner></sl-spinner></div>`
            : nothing}
          ${guard([this.messageRowsVersion, this.seenExpired], () => this.renderMessages())}
          ${
            // On a phone or tablet the typing indicator is the last item in
            // the list, so it takes no room from the composer and someone
            // starting to type never moves it; a list at the bottom keeps
            // it in view (see updated()).
            this._composerRoom.capped ? this.renderTypingIndicator() : nothing
          }
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

  private get seenExpired(): boolean {
    return this.peerReadAt > 0 && Date.now() - this.peerReadAt >= SEEN_VISIBLE_MS;
  }

  private renderMessages() {
    // Rendered with lit/directives/repeat.js and stable keys (not index
    // position) so that hiding/showing inter-agent markers — which changes
    // how many divider rows precede later rows — does not shift the
    // identity of unrelated rows. Without stable keys, Lit's default
    // positional array diffing tears down and recreates every row after the
    // point where the row count changed, which loses per-element state such
    // as a marker's expanded/collapsed toggle (R4).
    const rows: Array<{ key: string; tpl: unknown }> = [];
    let lastDate = '';
    let prevSender = '';
    let prevTimestamp = 0;

    // Pre-sort inter-agent messages by time for gap-based grouping.
    const iaMessages = [...this.interagentMessages].sort(compareMessageOrder);
    let iaIdx = 0;
    const hasIA = this.hasInteragentMessages;

    // Delivery state is a property of the conversation's tail, not of every
    // bubble: only the newest message this user sent carries it.
    const lastOwnMessageId = this.lastOwnMessageId();
    const seenExpired = this.seenExpired;

    // Unread divider: find the position of the last-read message so we can
    // insert the divider after it.
    let unreadDividerInserted = false;
    let lastReadIdx = -1;
    if (this.showUnreadDivider && this.lastReadMessageId) {
      lastReadIdx = this.messages.findIndex((m) => m.id === this.lastReadMessageId);
    }

    // Inter-agent messages are dated items like any other row: a run of
    // consecutive messages is split into one marker per calendar day (local
    // time), with the shared date-divider inserted between them exactly as
    // it appears between normal messages. `msgs` is time-sorted, so day
    // boundaries within it are contiguous.
    const pushInteragentGroups = (msgs: Message[]): void => {
      let groupStart = 0;
      // Cache the current group's date string rather than recomputing it
      // from msgs[groupStart] on every iteration of a long run.
      let groupDateStr = msgs.length > 0 ? formatChatDate(msgs[0].createdAt) : '';
      for (let i = 1; i <= msgs.length; i++) {
        const atBoundary = i === msgs.length || formatChatDate(msgs[i].createdAt) !== groupDateStr;
        if (!atBoundary) continue;
        const group = msgs.slice(groupStart, i);
        // Only a visible marker actually occupies a day in the timeline —
        // when inter-agent messages are hidden, don't advance lastDate or
        // emit a divider for them, or the next human message's own divider
        // (or a divider for a day with no visible content at all) would be
        // suppressed or left dangling with nothing under it.
        if (this.interagentVisible) {
          const groupDate = groupDateStr || 'Invalid Date';
          if (groupDate !== lastDate) {
            lastDate = groupDate;
            rows.push({ key: `day:${groupDate}`, tpl: renderDateDivider(groupDate) });
          }
        }
        rows.push({
          key: `ia:${group[0].id}`,
          tpl: html`
            <scion-chat-interagent-marker
              .messageCount=${group.length}
              .messages=${group}
              ?global-expanded=${this.interagentExpandAll}
              ?hidden=${!this.interagentVisible}
              current-project-id=${this.projectId}
            ></scion-chat-interagent-marker>
          `,
        });
        // Reset grouping after a marker so the next message shows its header.
        prevSender = '';
        prevTimestamp = 0;
        groupStart = i;
        if (i < msgs.length) groupDateStr = formatChatDate(msgs[i].createdAt);
      }
    };

    for (let mi = 0; mi < this.messages.length; mi++) {
      const msg = this.messages[mi];
      // Used for both the inter-agent cutoff below and the sender-grouping
      // window further down — not tied to any single one of them.
      const msgTime = new Date(msg.createdAt).getTime();
      const dateStr = formatChatDate(msg.createdAt) || 'Invalid Date';

      // Collect all inter-agent messages that fall before this DM message
      // and split them into one marker per day.
      if (hasIA) {
        const pendingIA: Message[] = [];
        while (
          iaIdx < iaMessages.length &&
          new Date(iaMessages[iaIdx].createdAt).getTime() < msgTime
        ) {
          pendingIA.push(iaMessages[iaIdx]);
          iaIdx++;
        }
        if (pendingIA.length > 0) {
          pushInteragentGroups(pendingIA);
        }
      }

      // Date divider
      if (dateStr !== lastDate) {
        lastDate = dateStr;
        prevSender = '';
        prevTimestamp = 0;
        rows.push({ key: `day:${dateStr}`, tpl: renderDateDivider(dateStr) });
      }

      // Unread divider: insert between the last-read message and the next one.
      if (!unreadDividerInserted && lastReadIdx >= 0 && mi > lastReadIdx) {
        unreadDividerInserted = true;
        rows.push({
          key: 'unread',
          tpl: html`
            <div class="unread-divider">
              <span class="unread-label">New messages</span>
            </div>
          `,
        });
        // Reset grouping so the first unread message shows its header.
        prevSender = '';
        prevTimestamp = 0;
      }

      // System/state-change messages
      if (SYSTEM_MESSAGE_TYPES.has(msg.type)) {
        prevSender = '';
        prevTimestamp = 0;
        rows.push({
          key: `msg:${msg.id}`,
          tpl: html`
            <scion-chat-system-line
              message=${msg.msg}
              timestamp=${msg.createdAt}
              category=${(msg.metadata?.['system_category'] as string) || ''}
            ></scion-chat-system-line>
          `,
        });
        continue;
      }

      // Grouping: consecutive *visible* messages from same sender within GROUP_WINDOW_MS
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
      const msgRoutedTo =
        !isAgentSender && msg.recipient && msg.recipient.startsWith('agent:')
          ? msg.recipient.slice(6)
          : '';
      const senderDisplayName = this.isV2
        ? this.getSenderDisplayName(msg)
        : isFromAgent
          ? this.agentName || ''
          : '';

      // Phase-3: Get extension data for this message.
      const ext = this.v2MessageExtMap.get(msg.id);
      const replyPreview = ext?.replyToId
        ? (this.v2ReplyPreviewMap.get(ext.replyToId) ?? null)
        : null;
      rows.push({
        key: `msg:${msg.id}`,
        tpl: html`
          <scion-chat-message
            @contextmenu=${(e: MouseEvent) => this.handleMessageContextMenu(e, msg)}
            @click=${(e: MouseEvent) => this.handleMessageTap(e, msg)}
            id="msg-${msg.id}"
            messageId=${msg.id}
            body=${msg.msg}
            sender=${msg.sender}
            senderId=${msg.senderId || ''}
            senderName=${senderDisplayName}
            ?fromAgent=${isFromAgent}
            ?senderIsAgent=${isAgentSender}
            ?plain=${msg.plain ?? false}
            agentSlug=${isFromAgent ? senderDisplayName : ''}
            timestamp=${msg.createdAt}
            .showHeader=${showHeader}
            ?urgent=${msg.urgent ?? false}
            ?broadcasted=${msg.broadcasted ?? false}
            channel=${msg.channel || ''}
            messageType=${msg.type || ''}
            dispatchState=${this.deliveryStateFor(msg, lastOwnMessageId, seenExpired)}
            ?seen=${msg.id === lastOwnMessageId && this.isMessageSeen(msg)}
            dispatchFailureReason=${msg.dispatchFailureReason || ''}
            dispatchFailureCode=${msg.dispatchFailureCode || ''}
            .attachments=${msg.attachments || EMPTY_ATTACHMENTS}
            .attachmentRefs=${this.getMessageAttachmentRefs(msg.id)}
            routedTo=${msgRoutedTo}
            .replyPreview=${replyPreview}
            editedAt=${ext?.editedAt || ''}
            deletedAt=${ext?.deletedAt || ''}
            senderProjectSlug=${msg.senderProjectId
              ? this.resolveProjectSlug(msg.senderProjectId)
              : ''}
            @scroll-to-message=${this.handleScrollToMessage}
            @path-link-click=${(e: CustomEvent<{ path: string }>) =>
              this.handlePathLinkClick(e, msg)}
            @gcs-link-click=${(
              e: CustomEvent<{ bucket: string; object: string; name: string; messageId: string }>
            ) => this.handleGcsLinkClick(e)}
          ></scion-chat-message>
        `,
      });

      // Render "also notified" footer under the specific message bubble (O3).
      const msgMentionResults = this.mentionResultsByMessageId.get(msg.id);
      if (msgMentionResults) {
        const delivered = msgMentionResults.filter((r) => r.status === 'delivered');
        if (delivered.length > 0) {
          const slugs = delivered.map((r) => html`<span class="mention-slug">@${r.slug}</span>`);
          rows.push({
            key: `mention:${msg.id}`,
            tpl: html`
              <div class="mention-results">
                Also notified:
                ${slugs.reduce((acc, s, i) => (i === 0 ? [s] : [...acc, ', ', s]), [] as unknown[])}
              </div>
            `,
          });
        }
      }

      prevSender = msg.sender;
      prevTimestamp = msgTime;
    }

    // Append any remaining inter-agent messages that come after all DM messages.
    if (hasIA && iaIdx < iaMessages.length) {
      pushInteragentGroups(iaMessages.slice(iaIdx));
    }

    return repeat(
      rows,
      (row) => row.key,
      (row) => row.tpl
    );
  }

  // ---------------------------------------------------------------------------
  // Export helpers (#1570)
  // ---------------------------------------------------------------------------

  /** Escape HTML special characters to prevent XSS. */
  private escapeHtml(text: string): string {
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
  }

  /**
   * Format an ISO timestamp for export display: 24-hour, in the effective
   * display zone, with the zone named so the exported text stands alone.
   * Falls back to the raw string when it does not parse.
   */
  private formatExportTimestamp(iso: string): string {
    return formatInstantWithZone(iso, 'datetime-full') || iso;
  }

  /** Generate a filename-safe date string (YYYY-MM-DD) in the effective display zone. */
  private filenameDateStamp(): string {
    return toWallClockInput(new Date().toISOString(), effectiveTimeZone()).slice(0, 10);
  }

  /**
   * Download the conversation as a Markdown file.
   *
   * Iterates the current message buffer, formats each message with sender,
   * timestamp, and body, then triggers a browser download.
   */
  public exportAsMarkdown(): void {
    if (this.messages.length === 0) {
      showToast('No messages to export', 'warning');
      return;
    }

    const lines = this.messages.map((m) => {
      const ts = this.formatExportTimestamp(m.createdAt);
      return `### ${m.sender} — ${ts}\n\n${m.msg}\n\n---\n`;
    });

    const content = lines.join('\n');
    const blob = new Blob([content], { type: 'text/markdown;charset=utf-8' });

    const nameBase = this.threadName
      ? this.threadName.replace(/[^a-zA-Z0-9_-]/g, '_')
      : 'conversation';
    const fileName = `${nameBase}-${this.filenameDateStamp()}.md`;

    const anchor = document.createElement('a');
    anchor.href = URL.createObjectURL(blob);
    anchor.download = fileName;
    document.body.appendChild(anchor);
    anchor.click();
    document.body.removeChild(anchor);
    URL.revokeObjectURL(anchor.href);
  }

  /**
   * Open a print-friendly window with the conversation content.
   *
   * Creates a new browser window with clean HTML formatted messages and
   * invokes the browser's print dialog (which allows saving as PDF).
   */
  public printConversation(): void {
    if (this.messages.length === 0) {
      showToast('No messages to export', 'warning');
      return;
    }

    const printWindow = window.open('', '_blank');
    if (!printWindow) {
      showToast('Unable to open print window — check your popup blocker.', 'warning');
      return;
    }

    const title = this.threadName ? this.escapeHtml(this.threadName) : 'Conversation';

    const messagesHtml = this.messages
      .map(
        (m) =>
          `<div style="margin-bottom: 1rem; padding-bottom: 0.5rem; border-bottom: 1px solid #eee;">
        <div style="font-weight: bold; font-size: 0.875rem;">${this.escapeHtml(m.sender)}
          <span style="color: #666; font-weight: normal;">${this.escapeHtml(this.formatExportTimestamp(m.createdAt))}</span>
        </div>
        <div style="margin-top: 0.5rem; white-space: pre-wrap;">${this.escapeHtml(m.msg)}</div>
      </div>`
      )
      .join('');

    printWindow.document.write(
      `<!DOCTYPE html><html><head><meta charset="utf-8"><title>${title}</title>` +
        `<style>body{font-family:system-ui,-apple-system,sans-serif;max-width:800px;margin:2rem auto;padding:0 1rem;color:#1a1a1a;}</style>` +
        `</head><body><h1 style="font-size:1.25rem;margin-bottom:1.5rem;">${title}</h1>${messagesHtml}</body></html>`
    );
    printWindow.document.close();
    printWindow.onafterprint = () => printWindow.close();
    printWindow.print();
    // Fallback for browsers that don't fire afterprint
    setTimeout(() => {
      if (!printWindow.closed) printWindow.close();
    }, 1000);
  }

  /**
   * Copy the conversation as formatted text to the clipboard.
   *
   * Writes both HTML and plain-text representations using the Clipboard API.
   * Falls back to plain text if the ClipboardItem API is unavailable.
   */
  public async copyAsFormattedText(): Promise<void> {
    if (this.messages.length === 0) {
      showToast('No messages to export', 'warning');
      return;
    }

    const htmlContent = this.messages
      .map(
        (m) =>
          `<p><strong>${this.escapeHtml(m.sender)}</strong> (${this.escapeHtml(this.formatExportTimestamp(m.createdAt))})</p>` +
          `<p>${this.escapeHtml(m.msg)}</p><hr>`
      )
      .join('');

    const plainText = this.messages
      .map((m) => `${m.sender} (${this.formatExportTimestamp(m.createdAt)})\n${m.msg}`)
      .join('\n\n---\n\n');

    try {
      await navigator.clipboard.write([
        new ClipboardItem({
          'text/html': new Blob([htmlContent], { type: 'text/html' }),
          'text/plain': new Blob([plainText], { type: 'text/plain' }),
        }),
      ]);
      showToast('Conversation copied to clipboard.', 'success');
    } catch {
      // Fallback: plain text copy.
      try {
        await navigator.clipboard.writeText(plainText);
        showToast('Conversation copied to clipboard (plain text).', 'success');
      } catch {
        showToast('Failed to copy to clipboard.', 'danger');
      }
    }
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-thread': ScionChatThread;
  }
}
