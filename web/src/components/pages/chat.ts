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
 * This is the primary entry point for Native Chat: shared spaces (one per
 * project), multi-participant threads, DMs (agent and human), a members
 * sidebar with presence indicators, typing indicators, notifications, file
 * attachments, and message search.
 *
 * Key design decisions:
 * - **Dual-dialect store**: webchat_topic, webchat_read_state, webchat_dm,
 *   webchat_user_prefs tables (SQLite + Postgres) for chat-specific state.
 *   Messages live in the existing messages table with ThreadID as the routing key.
 * - **One persistence path**: messages are persisted once via the hub's
 *   inprocess spoke; the web channel bus only updates watermarks.
 * - **SSE via stateManager**: a single multiplexed SSE connection per client
 *   replaces per-thread EventSource streams. Events are project-scoped.
 *
 * Renders inside `<scion-chat-shell>`:
 * - Space rail (chat-space-rail) with project grouping, threads, DMs
 * - Conversation view keyed by conversationKey (topic UUID or DM key)
 * - Routes: `/chat`, `/chat/space/{projectId}`, `/chat/space/{projectId}/thread/{topicId}`, `/chat/dm/{key}`
 * - Members sidebar with presence, typing indicators, search panel
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state, query } from 'lit/decorators.js';

import type { PageData, Agent } from '../../shared/types.js';
import { apiFetch, parseApiError } from '../../client/api.js';
import { navigateTo, replaceRoute, stateManager } from '../../client/main.js';
import { agentStore } from '../../client/agent-store.js';
import type { AgentListSnapshot } from '../../client/agent-store.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { chatNotifications } from '../../client/chat-notifications.js';
import { chatUnread } from '../../client/chat-unread.js';
import { TouchPrimaryController } from '../../utils/input-modality.js';
import { CHAT_PALETTE_OPEN_REQUEST_EVENT } from '../../client/chat-palette-events.js';
import type { GroupState, PaletteGroup, PaletteTarget } from '../../client/chat-palette-types.js';
import {
  AGENTS_IDLE_TIMEOUT_MS,
  ChatPaletteDataController,
  PaletteLoadError,
  buildDocumentCandidates,
} from '../../client/chat-palette-data.js';
import { chatRecentFiles } from '../../client/chat-recent-files.js';
import type { RecentFile, RecentFilesSnapshot } from '../../client/chat-recent-files.js';
import { paginateAll, PaginationStoppedError } from '../../client/paginate-all.js';
import { isProjectChimeEnabled, setProjectChimeEnabled } from '../../utils/audio.js';
import {
  horizontalScrollRoom,
  scrollerTakesDrag,
  type HorizontalScrollRoom,
} from '../../utils/horizontal-scroll.js';
import { openTerminal, terminalHref, agentGraphHref } from '../../client/open-terminal.js';
import { hasOpenModalDescendant, isOpenModalElement } from '../shared/open-modal.js';
import { deepActiveElement } from '../shared/deep-active-element.js';
import '../shared/chat/chat-thread.js';
import '../shared/chat/chat-file-preview.js';
import type { PreviewTarget } from '../shared/chat/chat-file-preview.js';
import { touchMenuItemStyles } from '../shared/touch-styles.js';

/**
 * The comfy density token values. Defined once and interpolated into both
 * the comfy-density selector and the mobile override (which forces comfy
 * regardless of the density setting), so the two can never drift apart.
 */
const comfyDensityTokens = css`
  --chat-fs-2xs: 0.6875rem;
  --chat-fs-xs: 0.75rem;
  --chat-fs-sm: 0.875rem;
  --chat-fs-base: 0.9375rem;
  --chat-fs-md: 1rem;
  --chat-fs-lg: 1.0625rem;
  --chat-fs-xl: 1.125rem;
  --chat-fs-2xl: 1.25rem;
  --chat-fs-3xl: 1.375rem;
  --chat-fs-4xl: 1.5rem;
  --chat-fs-5xl: 1.875rem;
  --chat-fs-6xl: 2.5rem;
  --chat-fs-7xl: 3.125rem;
  --chat-lh-tight: 1.5rem;
`;

// Lazy-load the space rail only when v2 is active
const loadSpaceRail = () => import('../shared/chat/chat-space-rail.js');
// Lazy-load the members sidebar only when v2 is active
const loadChatMembers = () => import('../shared/chat/chat-members.js');

/** Page size for the hub members sidebar's full users/agents walk. */
const HUB_MEMBERS_PAGE_SIZE = 100;

/** Members panel width bounds, in px. */
const MEMBERS_WIDTH_DEFAULT = 240;
const MEMBERS_WIDTH_MIN = 180;
const MEMBERS_WIDTH_MAX = 520;
/** localStorage key for the persisted members panel width. */
const MEMBERS_WIDTH_KEY = 'scion.chat.membersWidth';
// Lazy-load the search component only when v2 is active
const loadChatSearch = () => import('../shared/chat/chat-search.js');
// Lazy-load the quick switcher component on first Cmd+K press
const loadQuickPalette = () => import('../shared/palette/quick-palette.js');

/**
 * How long a successfully-loaded People or Threads group stays fresh across
 * a close/reopen before it is refetched. The Agents group is kept current
 * by the agent store instead.
 */
const PALETTE_GROUP_CACHE_MS = 30_000;
/** Debounce window for refreshing dirty palette groups while the palette is open. */
const PALETTE_REFRESH_DEBOUNCE_MS = 500;

/**
 * Convert a recent-files record into the shape `<scion-chat-file-preview>`
 * expects. `RecentFileTarget` (the store's own shape) omits the display
 * name, which the store keeps once on the record itself rather than
 * duplicated into every target variant; the preview component needs it
 * alongside the rest of the target, so this is a pure reshape, not a lookup.
 */
function recentFileToPreviewTarget(file: RecentFile): PreviewTarget {
  return file.target.kind === 'attachment'
    ? {
        kind: 'attachment',
        id: file.target.id,
        name: file.name,
        mime: file.target.mime,
        size: file.target.size,
      }
    : {
        kind: 'path',
        projectId: file.target.projectId,
        containerPath: file.target.containerPath,
        location: file.target.location,
        name: file.name,
      };
}

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
function agentMemberToAgent(m: import('../shared/chat/chat-members.js').ChatAgentMember): Agent {
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

/** The subset of a raw `/api/v1/users` row `loadHubMembers` reads. */
interface RawHubUser {
  id: string;
  displayName: string;
  email?: string;
  avatarUrl?: string;
  role?: string;
  status?: string;
}

/** `paginateAll`'s page extractor for `/api/v1/users`. */
function parseHubUsersPage(body: unknown): { items: RawHubUser[]; nextCursor?: string } {
  const data = body as { users?: RawHubUser[]; nextCursor?: string };
  return { items: data.users ?? [], ...(data.nextCursor ? { nextCursor: data.nextCursor } : {}) };
}

/** The subset of a raw `/api/v1/agents` row `loadHubMembers` reads. */
interface RawHubAgent {
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
  canAttach?: boolean;
}

/** `paginateAll`'s page extractor for `/api/v1/agents`. */
function parseHubAgentsPage(body: unknown): { items: RawHubAgent[]; nextCursor?: string } {
  const data = body as { agents?: RawHubAgent[]; nextCursor?: string };
  return { items: data.agents ?? [], ...(data.nextCursor ? { nextCursor: data.nextCursor } : {}) };
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

  /** Layout density: 'dense' is the compact default, 'comfy' bumps font sizes ~20-25%. */
  @property({ type: String, attribute: 'data-density', reflect: true })
  private density: 'dense' | 'comfy' = 'dense';

  /** Debounce handle shared by rail-refresh triggers (e.g. a new chat message). */
  private _refreshTimer: ReturnType<typeof setTimeout> | null = null;

  // ---- V2 state ----
  @state() private v2Conversation: V2ConversationState | null = null;
  @state() private v2Members: SpaceMember[] = [];
  /** Per-project chat chime preference for the currently open conversation's project. */
  @state() private projectChimeOn = true;
  private _chimeProjectId = '';

  private mentionAgentsSource: SpaceMember[] | null = null;
  private mentionAgentsProjectId = '';
  private mentionAgents: import('../../shared/types.js').Agent[] = [];
  @state() private v2MembersExpanded = true;

  /** Width of the members panel in px. Persisted per browser. */
  @state() private membersWidth = MEMBERS_WIDTH_DEFAULT;
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
  private _onAgentCreated = this._handleAgentCreated.bind(this);
  private _onScopeChanged = this._handleScopeChanged.bind(this);
  private _onReadStateUpdated = this._handleReadStateUpdated.bind(this);
  /**
   * Bound listener for the read-state SSE event (stateManager's
   * 'chat-read-state-updated', sourced from ChatReadStateEvent — distinct
   * from the same-tab DOM event above). Gated on the event's `unread` field,
   * not merely a userId match.
   */
  private _onOwnReadStateSSE = this._handleOwnReadStateSSE.bind(this);
  /** Bound listener for the rail's own-tab "Mark unread" notification. */
  private _onConversationMarkedUnread = this._handleConversationMarkedUnread.bind(this);
  private _unreadDMRequestId = 0;
  /**
   * Bumped whenever the user navigates within the page (opens a thread or a
   * DM, resets the view) and when the page is removed. A lookup the user
   * started captures it and gives up if it has changed, so a late answer
   * never overrides what the user did since.
   */
  private _userNavSeq = 0;
  /**
   * Which view (a project, or the hub) last claimed the members sidebar, for
   * the two loaders that don't go through the hub walk's own guards.
   * Bumped by every call to loadV2Members (which replaces the arrays with
   * one project's members) and by every call to loadHubMembers (which
   * claims the sidebar for the hub view, whether it starts a walk or joins
   * one). loadV2Members and refreshHubMemberPresence capture it before
   * their network await and bail, after their last await, if it has moved
   * on — a newer view has claimed the sidebar and their response is stale.
   * refreshHubMemberPresence only merges fields into the existing arrays
   * rather than replacing them, so it captures this token without bumping
   * it.
   *
   * The hub walk itself doesn't check this token: its own `v2Conversation`
   * and {@link _hubMembersGeneration} checks keep it from publishing over a
   * project view.
   */
  private _membersViewSeq = 0;
  /**
   * Coalescing gate for {@link loadHubMembers}. `loadHubMembers` has several
   * call sites — a route parse, `initV2`'s no-conversation branch, a
   * rail-data re-parse, `handleResetView`, and the fallback poll — that fire
   * at different points during a cold `/chat` mount and afterward, not
   * necessarily in the same synchronous turn. Calls that land before the
   * scheduled walk's microtask runs batch into that one walk, since they're
   * indistinguishable from each other (nothing could have changed between
   * them); calls that land once the walk's requests are already in flight
   * join it instead of starting a second one — see below for the two kinds.
   *
   * `_hubMembersScheduled` is true from the first call that schedules the
   * walk until its `queueMicrotask` callback actually starts it; every call
   * in that window collapses into the single scheduled walk with zero extra
   * requests.
   *
   * `_hubMembersInFlight` is true only once that walk's network requests are
   * actually in flight, and `_hubMembersInFlightGeneration` records which
   * {@link _hubMembersGeneration} owns it. A call arriving while a walk is in
   * flight for the caller's *current* generation is one of two kinds:
   *
   * - A "join" call (the default — route/view re-parses, which re-derive the
   *   same hub-wide view rather than reacting to anything that could have
   *   changed the data) simply waits for the walk already running; it does
   *   not queue anything.
   * - A `{ refresh: true }` call (only the fallback poll, which exists
   *   precisely because something could have changed since the last load)
   *   sets `_hubMembersReloadQueued`, and `_runHubMembersLoad`'s loop performs
   *   exactly one trailing walk once the current one settles. Further calls
   *   during that trailing walk re-set the same flag (or simply join it, if
   *   they're join calls) rather than queuing a second one.
   *
   * A call arriving while a walk is in flight for a *stale* generation — the
   * element disconnected and reconnected while that walk was still running,
   * or a conversation opened and the view returned to the hub-wide /chat
   * view before the walk settled — does not join it: that walk's result is
   * for a view that no longer exists and may never publish anything the
   * caller's view can see, so the caller schedules a fresh walk for its own
   * generation instead of waiting.
   */
  private _hubMembersScheduled = false;
  private _hubMembersInFlight = false;
  /** The {@link _hubMembersGeneration} that owns the current in-flight walk; meaningless while `_hubMembersInFlight` is false. */
  private _hubMembersInFlightGeneration = 0;
  private _hubMembersReloadQueued = false;
  /**
   * Bumped on `disconnectedCallback` (same pattern as `_unreadDMRequestId`
   * below) and whenever `v2Conversation` is assigned a truthy value (see
   * `updated()`'s `v2Conversation` branch) — both retire any hub-members
   * walk started before them. The latter is not only "a conversation opens"
   * in the user-facing sense: it also fires for a mute toggle or a
   * default-agent edit on the conversation already open, since those
   * reassign `v2Conversation` to a new (still truthy) object too, and the
   * bump doesn't need to distinguish those from an actual navigation — any
   * of them retiring a stale hub-wide walk is harmless, since one can only
   * be in flight while no conversation is open in the first place. The bump
   * exists because a walk's own `this.v2Conversation` check only prevents it
   * from publishing *while* a conversation is open; it says nothing once the
   * user has since returned to the hub-wide view and a stale walk settles
   * after that, by which time `this.v2Conversation` reads clear again. A
   * walk captures this generation at the start and compares it before
   * looping again or publishing, so a walk superseded either way can't write
   * stale data into a view it no longer belongs to.
   *
   * This is bumped only when `updated()` observes the *final* value of a
   * batch of `v2Conversation` writes to be truthy — a batch that opens and
   * closes a conversation again before `updated()` runs reads clear there
   * and is not bumped. That gap is covered separately, and robustly against
   * however Lit happens to batch the writes, by the per-attempt stopped
   * result {@link _fetchHubMembersOnce} returns rather than by this
   * generation.
   */
  private _hubMembersGeneration = 0;
  private _onDMPromoted = this.handleDMPromoted.bind(this);
  /** Bound keydown handler for Cmd/Ctrl+K quick switcher. */
  private _onKeydown = this._handleGlobalKeydown.bind(this);
  /** Bound handler for the header palette button's open-request event. */
  private _onPaletteOpenRequest = this._handlePaletteOpenRequest.bind(this);
  /** Whether the device's primary pointer is touch -- see `_selectionHandsFocusToComposer`. */
  private touchPrimary = new TouchPrimaryController(this);
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
  /**
   * Map of DM peer ID → DM info, for members with an existing, non-empty DM.
   * Lets the members sidebar's "Mark unread" item know which conversation to
   * act on and whether it is already unread (hide itself) or muted (a
   * mark-unread there must not push a dot), and hides itself for a member
   * with no DM at all.
   */
  @state() private v2DMInfoByPeerId: Record<
    string,
    { key: string; muted: boolean; hasUnread: boolean }
  > = {};
  /**
   * Whether the quick command palette component has been lazy-loaded.
   * `@state` (not a plain field) so togglePalette's `await
   * this.updateComplete` after setting it actually waits for a real,
   * separate render: mount `<scion-quick-palette>` with open=false first, so
   * the following `v2PaletteOpen = true` is a genuine false->true transition
   * on an existing element rather than both happening in the same render
   * pass (which is indistinguishable from "born open" to Shoelace's dialog —
   * see togglePalette's comment).
   */
  @state() private v2SwitcherLoaded = false;
  /** Whether the grouped palette is open. */
  @state() private v2PaletteOpen = false;
  /**
   * True while an open is in flight but hasn't set `v2PaletteOpen` yet —
   * synchronous (set before the first `await`, unlike `v2PaletteOpen`) so a
   * second Ctrl+K press arriving during the first-open lazy import
   * (`loadQuickPalette()`) can be detected before that import resolves.
   * Without this, that second press would re-enter `togglePalette` while
   * `v2PaletteOpen` is still false and would either open a second time or
   * re-capture the invoker focus.
   */
  private _palettePendingOpen = false;
  /**
   * Bumped synchronously at the start of every `_openPalette` call (fresh or
   * dequeued). `_retargetPaletteInvokerToNewComposer` captures this value
   * before its own await and only assigns `_paletteInvoker` if it still
   * matches once that await resolves — without it, a slow composer mount
   * (this poll can take up to 2s) could still be in flight when the
   * reopened palette is closed and a *later* open captures a fresh invoker
   * of its own, and the stale poll's eventual assignment would silently
   * overwrite that fresh one out from under the next close.
   */
  private _paletteOpenEpoch = 0;
  /** Per-group palette load state for Agents, Threads and People. */
  @state()
  private v2PaletteGroups: Partial<Record<PaletteGroup, GroupState>> = {
    agents: { status: 'loading', candidates: [] },
  };
  /** Agents/DM data controller for the palette: pagination, DM join, cancellation. */
  private _paletteDataController = new ChatPaletteDataController();
  /** Epoch ms a group last finished loading successfully — the basis for the 30s per-group cache. */
  private _paletteGroupCacheAt: Partial<Record<PaletteGroup, number>> = {};
  /** Groups invalidated by an SSE event since their last successful load — forces a refetch even inside the 30s cache window. */
  private _paletteGroupDirty: Partial<Record<PaletteGroup, boolean>> = {};
  /**
   * Bumped by {@link _markPaletteGroupsDirty} for each named group. A loader
   * snapshots this at load start and only clears `_paletteGroupDirty` on
   * success if the epoch is still unchanged when the load finishes. This
   * guarantees that an invalidation arriving *during* an in-flight load is
   * never silently lost: if the epoch changed mid-load, `dirty` and the
   * cache timestamp are left untouched, so the debounced refresh that
   * invalidation already scheduled still reloads the group with current
   * data shortly after, instead of the load in flight publishing/caching a
   * possibly-stale snapshot for a full 30s.
   */
  private _paletteGroupInvalidationEpoch: Partial<Record<PaletteGroup, number>> = {};
  /**
   * The current load's own identity token for a group, keyed by
   * `PaletteGroup` — present while that group's own loader
   * (`_loadPaletteAgents`/`_loadPalettePeople`/`_loadPaletteThreads`) has a
   * fetch in flight, absent otherwise. `_refreshDirtyPaletteGroups` checks
   * presence before reloading a dirty group — see its own doc comment for
   * why.
   *
   * A token, not a boolean: a loader clears its own entry in `finally` only
   * if it is still the current token, so a load superseded by a newer one
   * for the same group (an explicit reopen/retry, or — for People
   * specifically — a newer load already past the `_resolveSelfUserId`
   * identity step, which the data controller's own `cancel()` does not reach,
   * while this one is still in it) cannot clear bookkeeping that belongs to
   * that newer load. For Agents and
   * Threads, the data controller's own generation check already turns every
   * supersede case into an AbortError before the loader's success or catch
   * branch runs at all, so those two loaders only need the token in
   * `finally`. People is the exception: its identity-resolution step runs
   * before the data controller is ever called, so a newer load can already
   * be in that step — superseding nothing the controller tracks — while an
   * older one is still resolving or has just failed. People's loader
   * therefore additionally checks the token on both its success path and its
   * catch path before publishing a result, not just before clearing it.
   */
  private _paletteGroupLoadToken: Partial<Record<PaletteGroup, object>> = {};
  /**
   * True once a `ready` Agents result has actually been published, independent
   * of the group's *current* `status` — a refresh sets `status: 'loading'`
   * and a failed/cancelled refresh can leave it `'loading'` or `'error'`
   * while still holding that same complete, DM-ranked snapshot in
   * `candidates` (see the error branch and `_closePaletteAndCancelLoad`,
   * neither of which clears `candidates`). `_loadPaletteAgents` reads this —
   * not `status` — to decide whether to wire `onProgress`: keying on
   * `status === 'ready'` instead would wire it (and so replace the complete
   * list with page one alone) for every refresh that is cancelled by a
   * close/reopen or that errors, which is most refreshes on a hub busy
   * enough to keep invalidating the group. Never reset back to `false` once
   * set — there is no event in this group's lifecycle that invalidates a
   * previously-published complete snapshot, only ones that might replace it
   * with a newer one.
   */
  private _agentsSnapshotComplete = false;
  /**
   * Set when a chat or DM change marks the Agents group's DM list stale
   * while the palette is open, so the debounced refresh re-reads recency.
   * Cleared when an Agents load starts.
   */
  private _agentDmsRefreshPending = false;
  /**
   * Identifies the current People load across its identity-resolution phase
   * (`_resolveSelfUserId`'s `/auth/me` fetch, bounded by its own idle-timeout
   * `AbortController` but not covered by `_paletteDataController.cancel()`).
   * `_loadPalettePeople` captures this counter's value at its own start and
   * bumps it again on every new call, so a load whose identity resolution is
   * still pending when a *later* People load starts (a reopen, a refresh, or
   * simply calling it twice) is superseded the moment that later call begins
   * — before either load's identity has even resolved yet.
   * `_closePaletteAndCancelLoad` also bumps it, so a load left pending when
   * the palette closes (and is never reopened) is superseded too. This must
   * be keyed to which load is current, not to `v2PaletteOpen`: a reopen sets
   * `v2PaletteOpen` back to `true`, so an open-state check alone cannot tell
   * a stale load's identity resolution apart from a newer one, and a stale
   * resolution completing after the reopen could overwrite a newer,
   * already-`ready` People state.
   */
  private _peopleLoadSeq = 0;
  /**
   * The `AbortController` backing whichever `/api/v1/auth/me` fetch
   * {@link _resolveSelfUserId} currently has in flight, so a close
   * ({@link _closePaletteAndCancelLoad}) or a newer People load (which
   * aborts the previous controller before installing its own, same as
   * {@link ChatPaletteDataController}'s own per-group abort fields) stops the
   * request immediately instead of leaving it running for up to
   * {@link AGENTS_IDLE_TIMEOUT_MS}. Purely a resource-usage fix: `_peopleLoadSeq`
   * already guarantees a stale resolution can never publish, with or without
   * this abort.
   */
  private _selfUserAbortController: AbortController | null = null;
  /** Debounce timer coalescing SSE-driven dirty-group refreshes while the palette is open. */
  private _paletteRefreshDebounce: ReturnType<typeof setTimeout> | null = null;
  /** The deep-active element (and, for a textarea or text input, its selection) captured just before the palette opened. */
  private _paletteInvoker: HTMLElement | null = null;
  private _paletteInvokerSelection: {
    start: number | null;
    end: number | null;
    direction: 'forward' | 'backward' | 'none' | null;
  } | null = null;
  /**
   * True when the palette is closing because of a committed selection rather
   * than escape/backdrop/toggle — the `sl-after-hide` handler uses this to
   * decide whether to focus the new conversation's composer instead of
   * restoring the old invoker (never restore focus into the old composer
   * afterward).
   */
  private _paletteClosedBySelection = false;
  /**
   * True when the palette is being closed programmatically (route change
   * hiding chat, or another modal opening) rather than by the user — focus
   * must not be restored into a hidden page in that case (close it without
   * restoring focus into hidden chat).
   */
  private _paletteSkipFocusRestore = false;
  /**
   * True from the moment a close is requested (`_closePaletteAndCancelLoad`)
   * until that close's `sl-after-hide` actually fires. Shoelace's dialog
   * does not handle `open` flipping back to `true` while its hide animation
   * is still running — the panel stays visually hidden even though `open`
   * (and `v2PaletteOpen`) says otherwise. `togglePalette` checks this flag to
   * queue a reopen instead, rather than ever setting `v2PaletteOpen = true`
   * during this window.
   */
  private _paletteCloseAnimating = false;
  /**
   * Set by `togglePalette` when a Cmd/Ctrl+K arrives while a close is still
   * animating out (see `_paletteCloseAnimating`); `_handlePaletteAfterHide`
   * consumes it once that close's `sl-after-hide` fires, opening the palette
   * fresh instead of running that close's own focus disposition (composer
   * focus or invoker restore) — the user's later request to reopen
   * supersedes it.
   */
  private _palettePendingReopen = false;
  /**
   * A Documents selection's resolved preview target, held from
   * `_handlePaletteSelect` until the palette's own close animation actually
   * finishes — see `_handlePaletteAfterHide`. Never set at the same time as
   * `_paletteClosedBySelection`: a selection is exactly one of a
   * dm/thread navigation or a document preview.
   */
  private _pendingDocumentPreviewTarget: PreviewTarget | null = null;
  /**
   * The page-level file preview's current target, or null to render nothing.
   * Mounted outside the conditional conversation rendering in `renderV2`, so
   * a Documents selection can preview a file with no thread mounted (or from
   * a different project's active conversation) without disturbing the
   * current URL, conversation, draft/selection or mobile panel.
   */
  @state() private _paletteFilePreviewTarget: PreviewTarget | null = null;
  /** Unsubscribe from `chatRecentFiles`, set once connected — see `_handleRecentFilesSnapshot`. */
  private _paletteDocumentsUnsubscribe: (() => void) | null = null;
  /** Releases the agent store's hub entry, retained while the page is connected. */
  private _paletteAgentsRelease: (() => void) | null = null;
  /** Bounded-poll watchdog closing the palette if the route/visibility guards stop passing while it's open (see `_startPaletteVisibilityWatchdog`). */
  private _paletteVisibilityWatchdog: ReturnType<typeof setInterval> | null = null;
  /** Bound handler for `sl-after-hide` bubbling up from the palette's internal sl-dialog. */
  private _onPaletteAfterHide = this._handlePaletteAfterHide.bind(this);
  /** Bound handler: close the open palette if some other dialog/drawer opens while it's open. */
  private _onDocumentModalShow = this._handleDocumentModalShow.bind(this);
  /** Bound handler: close the open palette if a route change navigates away from /chat. */
  private _onPopState = this._handlePopStateForPalette.bind(this);
  /** The mounted switcher/palette element, if any — excluded from the modal guard's live query. */
  @query('scion-quick-palette') private _switcherEl?: Element;
  /** Whether the search panel is visible. */
  @state() private v2SearchActive = false;
  /** Whether the search component has been lazy-loaded. */
  @state() private v2SearchLoaded = false;
  /** Whether the promote-to-thread confirmation dialog is open. */
  @state() private promoteDialogOpen = false;
  /** Thread name entered in the promote dialog. */
  @state() private promoteThreadName = '';
  /** Whether the promote API call is in flight. */
  @state() private promoteLoading = false;
  /** Presence heartbeat interval timer. */
  private _presenceInterval: ReturnType<typeof setInterval> | null = null;
  /** Tracked project IDs for presence heartbeat. */
  private _presenceProjectIds: string[] = [];
  /** Slow fallback poll for what SSE does not cover (see FALLBACK_POLL_INTERVAL_MS). */
  private _fallbackPollInterval: ReturnType<typeof setInterval> | null = null;
  /** Debounced re-fetch of members after a new agent appears via SSE. */
  private _canAttachRefreshTimer: ReturnType<typeof setTimeout> | null = null;
  // IDs an authoritative members fetch has just left out (see loadV2Members
  // reconciliation). Blocks _handleAgentsUpdated from re-adding a stale
  // stateManager entry the server has already rejected, breaking the
  // SSE-merge/refetch feedback loop. Cleared when a legitimate SSE
  // `created` event arrives for that ID.
  private _serverOmittedAgentIds = new Set<string>();
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
  /** Which way the sideways scrollers under the current touch could still scroll. */
  private _touchScrollRoom: HorizontalScrollRoom = { rightward: false, leftward: false };
  /**
   * More than one finger has been down during the current gesture: it is a
   * pinch, not a swipe, until every finger lifts.
   */
  private _touchMulti = false;

  /**
   * Whether the viewport is under the mobile breakpoint. Driven by a
   * matchMedia listener rather than re-measured per call, so the CSS media
   * query (`@media (max-width: 768px)`, MOBILE_BREAKPOINT_PX) and this
   * reactive state always agree on the same breakpoint — `isMobileViewport()`
   * just reads it.
   */
  @state() private isMobileLayout = false;
  private _mobileLayoutQuery: MediaQueryList | null = null;
  private _onMobileLayoutChange = this._handleMobileLayoutChange.bind(this);

  static override styles = css`
    ${touchMenuItemStyles}

    :host {
      display: flex;
      height: 100%;
      overflow: hidden;

      /* Layout density: dense (default) — current sizes */
      --chat-fs-2xs: 0.5625rem;
      --chat-fs-xs: 0.625rem;
      --chat-fs-sm: 0.6875rem;
      --chat-fs-base: 0.75rem;
      --chat-fs-md: 0.8125rem;
      --chat-fs-lg: 0.875rem;
      --chat-fs-xl: 0.9375rem;
      --chat-fs-2xl: 1rem;
      --chat-fs-3xl: 1.125rem;
      --chat-fs-4xl: 1.25rem;
      --chat-fs-5xl: 1.5rem;
      --chat-fs-6xl: 2rem;
      --chat-fs-7xl: 2.5rem;
      --chat-lh-tight: 1.25rem;
    }

    /* The quick palette follows this page's density. */
    scion-quick-palette {
      --palette-fs-xs: var(--chat-fs-xs);
      --palette-fs-sm: var(--chat-fs-sm);
      --palette-fs-base: var(--chat-fs-base);
      --palette-fs-xl: var(--chat-fs-xl);
    }

    :host([data-density='comfy']) {
      ${comfyDensityTokens}
    }

    /* Mobile always gets the comfy token set, regardless of the density
       setting, and the toggle below is hidden there. Shares its values
       with the comfy block above via comfyDensityTokens, so it is
       harmless that the comfy attribute selector otherwise outranks this
       plain :host rule. */
    @media (max-width: 768px) {
      :host {
        ${comfyDensityTokens}
      }
    }

    /* ---- Shared layout ---- */

    .v2-content {
      flex: 1;
      display: flex;
      flex-direction: column;
      min-width: 0;
      overflow: hidden;
    }

    /* The thread fills what the header leaves and may shrink to nothing
       but its composer: its own 300px floor (kept for other hosts) would
       push the composer out of a short frame — a landscape phone, or a
       small one with the keyboard open. */
    .v2-content scion-chat-thread {
      flex: 1 1 0;
      min-height: 0;
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
      font-size: var(--chat-fs-7xl);
      opacity: 0.3;
    }

    .empty-state .title {
      font-size: var(--chat-fs-2xl);
      font-weight: 500;
    }

    .empty-state .subtitle {
      font-size: var(--chat-fs-lg);
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
      /* 'clip' wins over 'hidden' where supported (Safari 16+, Chrome 90+)
         and, unlike 'hidden', is never itself a scroll container — nothing
         (focus, scrollIntoView, native iOS reveal) can set its scrollLeft.
         Desktop has no off-screen content, so this is harmless there. */
      overflow: clip;
    }

    /*
     * Landscape phones wide enough for the side-by-side layout (the page
     * uses viewport-fit=cover): the edge columns carry the notch and
     * rounded-corner insets inside their own surface, so the surface still
     * runs to the screen edge and only the content moves in. The columns
     * are content-box, so the padding adds to their width rather than
     * taking from it. All 0 on devices without side insets; below the
     * mobile breakpoint every panel carries both insets instead.
     */
    .v2-rail {
      padding-left: env(safe-area-inset-left, 0px);
    }

    .v2-members {
      padding-right: env(safe-area-inset-right, 0px);
    }

    /* The conversation column has several rows with their own backgrounds
       (this header, the search and thread bars, and the composer), so
       padding the column would leave a band beside them. Instead the column
       says how far each side must move in, through --chat-inset-left and
       --chat-inset-right, and each row takes that as a transparent border
       that its background paints under. Side by side, only the right edge
       can touch the screen, and only while the members panel is hidden; in
       the mobile layout (below) the column is full width and takes both. */
    .v2-content.edge-right {
      --chat-inset-right: env(safe-area-inset-right, 0px);
    }

    .v2-thread-header {
      border-left: var(--chat-inset-left, 0px) solid transparent;
      border-right: var(--chat-inset-right, 0px) solid transparent;
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
      /* Width is driven by --members-w so the mobile rules below, which set
         width:100%, can still win — an inline width would beat them. */
      position: relative;
      width: var(--members-w, 240px);
      border-left: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
      display: flex;
      flex-direction: column;
      flex-shrink: 0;
    }

    /* Grab strip on the panel's left border.
       The CSS resize property was tried first and is the wrong tool: it only
       offers a corner grabber at the element's bottom-right, so on a
       full-height right-hand panel it lands at the bottom of the viewport and
       can never drag the left edge, which is the border people reach for. */
    .v2-members-resizer {
      position: absolute;
      left: -3px;
      top: 0;
      bottom: 0;
      width: 7px;
      cursor: col-resize;
      z-index: 5;
      background: transparent;
      border: none;
      padding: 0;
      touch-action: none;
    }

    .v2-members-resizer:hover,
    .v2-members-resizer:focus-visible {
      background: var(--scion-primary, #3b82f6);
      opacity: 0.35;
      outline: none;
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
      font-size: var(--chat-fs-md);
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .v2-thread-header {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      font-size: var(--chat-fs-lg);
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

      /* Full 44px visible box: the members header has nothing else
         competing for width, unlike the thread header's back/members
         buttons, which share a crowded row and only get the ::before
         hit-area treatment below. */
      .v2-members-header .mobile-back::part(base) {
        width: 44px;
        height: 44px;
      }

      /* The density toggle has no effect on mobile: tokens are forced to
         comfy above regardless of the density setting. */
      .density-toggle {
        display: none;
      }

      /* Thread-header icon buttons: the hit area grows via an invisible,
         larger ::before box (still part of the button, so it still
         receives the click), without growing the visible button itself —
         at 320px wide, several of these sit in one row (search, members,
         export, and more on a DM), and growing the visible boxes pushes
         that row past the viewport. The horizontal inset is kept smaller
         than the vertical one so neighbouring buttons' hit areas don't
         overlap — the row packs them closer together than their own
         height. */
      .v2-thread-header sl-icon-button::part(base) {
        position: relative;
      }

      .v2-thread-header sl-icon-button::part(base)::before {
        content: '';
        position: absolute;
        inset: -6px -2px;
      }

      .desktop-members {
        display: none;
      }

      /* An agent DM header carries the most actions (terminal, promote,
         mute, export, search, members) and, at 320px, more than fit beside
         the back button and the peer name. Rather than run off the screen,
         taking the members button with it, the actions row shrinks and
         scrolls sideways. The name gives way first, wrapping down to its
         longest word, so the row scrolls only once that is not enough.
         The block padding keeps the buttons' enlarged hit areas (above)
         inside the scroller's clip, and the negative margin gives that
         space back. */
      .v2-thread-header > span {
        flex-shrink: 1000;
      }

      .v2-thread-header .header-actions {
        flex: 0 1 auto;
        min-width: 0;
        overflow-x: auto;
        overflow-y: hidden;
        scrollbar-width: none;
        padding: 6px 2px;
        margin-block: -6px;
      }

      /* Fit the promote dialog to the frame, which the open keyboard
         shrinks below the layout viewport the dialog is positioned in. */
      .promote-dialog::part(base) {
        bottom: auto;
        height: var(--scion-app-height, 100dvh);
      }

      .promote-dialog::part(panel) {
        max-height: calc(100% - 1rem);
      }

      .empty-state .subtitle.desktop-only {
        display: none;
      }

      .empty-state .subtitle.mobile-only {
        display: inline;
      }

      /*
       * Each panel is absolutely positioned at one viewport wide and
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
        /* Each panel delegates scrolling to its own inner scroller
           (.rail-body, .messages-scroll, chat-members' .members-body) —
           see the base .v2-rail/.v2-content rules above. The panel itself
           no longer scrolls, so native focus reveal and scrollIntoView
           can't drift it horizontally. */
        overflow: hidden;
        overflow: clip;
        /* The global '* { box-sizing: border-box }' does not cross the shadow
           boundary, so these panels default to content-box: the desktop
           border-right/border-left would add 1px on top of the full-viewport
           width and push content off the left edge on iOS Safari. */
        box-sizing: border-box;
        border: 0;
        /* A horizontal drag here belongs to the swipe handler above, never
           to the browser: Chromium turns a horizontal touch overscroll that
           nothing consumed into history-back navigation, which rebuilds the
           whole page. overscroll-behavior on the root does not stop it, so
           horizontal panning is taken away from the browser instead. Touch
           events still fire, so the swipe handler is unaffected, and
           pinch-zoom is kept. touch-action does not carry into a scroll
           container (each one starts over), so the panels' own scrollers
           restate it through --chat-touch-action; scrollers that really
           scroll sideways (code blocks, tables) do not, and keep panning. */
        --chat-touch-action: pan-y pinch-zoom;
        touch-action: var(--chat-touch-action);
      }

      /* Landscape: keep each full-width panel's content clear of the notch
         and rounded corners. 0 on devices without side insets. The rail and
         members panels have one surface, so they pad. */
      .v2-panels .v2-rail,
      .v2-panels .v2-members {
        padding-inline: env(safe-area-inset-left, 0px) env(safe-area-inset-right, 0px);
      }

      /* The conversation panel's rows take both insets as borders (see the
         side-by-side .v2-content.edge-right rule). Its right value is the
         same env() as that rule's, so which of the two applies does not
         matter, and the panel itself has no inline padding to add twice. */
      .v2-panels .v2-content {
        --chat-inset-left: env(safe-area-inset-left, 0px);
        --chat-inset-right: env(safe-area-inset-right, 0px);
      }

      /* ---- Left panel active ---- */
      .v2-panels[data-panel='left'] .v2-rail {
        /* 'none' rather than 'translateX(0)': a transform of any kind makes
           this panel the containing block for position:fixed descendants
           (custom context menus), offsetting them by the panel's page
           position instead of the viewport. The transition still
           interpolates to/from the translateX() values below. */
        transform: none;
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
        transform: none;
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
        transform: none;
      }

      /* The members panel is a swipe target on mobile, so it stays in the
         track even when the desktop toggle has collapsed it. */
      .v2-panels .v2-members.collapsed {
        display: flex;
      }

      /* Full-width here, so there is nothing to drag. */
      .v2-members-resizer {
        display: none;
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

  /**
   * Begin dragging the members panel's left edge.
   *
   * Listeners go on window rather than the handle, because the pointer leaves
   * the 7px strip immediately on any real drag.
   */
  private startMembersResize(e: PointerEvent): void {
    e.preventDefault();
    const startX = e.clientX;
    const startWidth = this.membersWidth;

    const onMove = (ev: PointerEvent) => {
      // The panel sits on the right, so dragging left widens it.
      const next = startWidth - (ev.clientX - startX);
      this.membersWidth = Math.min(
        MEMBERS_WIDTH_MAX,
        Math.max(MEMBERS_WIDTH_MIN, Math.round(next))
      );
    };
    const onUp = () => {
      window.removeEventListener('pointermove', onMove);
      window.removeEventListener('pointerup', onUp);
      window.removeEventListener('pointercancel', onUp);
      this.persistMembersWidth();
    };
    window.addEventListener('pointermove', onMove);
    window.addEventListener('pointerup', onUp);
    window.addEventListener('pointercancel', onUp);
  }

  /** Keyboard resizing, so the panel is adjustable without a pointer. */
  private onMembersResizeKey(e: KeyboardEvent): void {
    const step = e.shiftKey ? 40 : 10;
    let next = this.membersWidth;
    if (e.key === 'ArrowLeft') next += step;
    else if (e.key === 'ArrowRight') next -= step;
    else if (e.key === 'Home') next = MEMBERS_WIDTH_DEFAULT;
    else return;
    e.preventDefault();
    this.membersWidth = Math.min(MEMBERS_WIDTH_MAX, Math.max(MEMBERS_WIDTH_MIN, next));
    this.persistMembersWidth();
  }

  private persistMembersWidth(): void {
    try {
      localStorage.setItem(MEMBERS_WIDTH_KEY, String(this.membersWidth));
    } catch {
      // Private mode or blocked storage: the width still applies for this
      // session, it just will not be remembered.
    }
  }

  /** Restore a persisted width, ignoring anything unparseable or out of range. */
  private restoreMembersWidth(): void {
    try {
      const raw = localStorage.getItem(MEMBERS_WIDTH_KEY);
      if (!raw) return;
      const parsed = Number.parseInt(raw, 10);
      if (Number.isFinite(parsed) && parsed >= MEMBERS_WIDTH_MIN && parsed <= MEMBERS_WIDTH_MAX) {
        this.membersWidth = parsed;
      }
    } catch {
      // Blocked storage: keep the default.
    }
  }

  /** Toggle between dense and comfortable layout density. */
  private toggleDensity(): void {
    this.density = this.density === 'dense' ? 'comfy' : 'dense';
    try {
      localStorage.setItem('scion.chat.density', this.density);
    } catch {
      // localStorage unavailable (private browsing, iframe sandbox)
    }
  }

  override connectedCallback(): void {
    super.connectedCallback();
    this._mobileLayoutQuery = window.matchMedia(`(max-width: ${MOBILE_BREAKPOINT_PX}px)`);
    this.isMobileLayout = this._mobileLayoutQuery.matches;
    this._mobileLayoutQuery.addEventListener('change', this._onMobileLayoutChange);
    this.restoreMembersWidth();
    // Restore persisted layout density preference.
    try {
      const savedDensity = localStorage.getItem('scion.chat.density');
      if (savedDensity === 'comfy') this.density = 'comfy';
    } catch {
      // localStorage unavailable (private browsing, iframe sandbox)
    }
    // Global Cmd/Ctrl+K listener for the quick switcher.
    document.addEventListener('keydown', this._onKeydown);
    // Header palette button: a composed custom event rather than a direct
    // reference, since the header renders as a sibling (slotted into
    // scion-chat-shell), not an ancestor.
    document.addEventListener(CHAT_PALETTE_OPEN_REQUEST_EVENT, this._onPaletteOpenRequest);
    // Reactive half of the modal/route interaction: if another modal opens
    // or route hides chat while the palette is open, close it without
    // restoring focus into hidden chat. The guard itself
    // (_isUnrelatedModalActive) queries live DOM state at keydown time, not
    // these events — see its doc comment for why.
    document.addEventListener('sl-show', this._onDocumentModalShow);
    window.addEventListener('popstate', this._onPopState);
    this._handleRecentFilesSnapshot(chatRecentFiles.snapshot());
    this._paletteDocumentsUnsubscribe = chatRecentFiles.subscribe((snapshot) =>
      this._handleRecentFilesSnapshot(snapshot)
    );
    this._paletteAgentsRelease?.();
    this._paletteAgentsRelease = agentStore.retain({ scope: 'hub' }, (snapshot) =>
      this._handlePaletteAgentSnapshot(snapshot)
    );
    void this.initV2();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    ++this._unreadDMRequestId;
    ++this._userNavSeq;
    ++this._hubMembersGeneration;
    this._mobileLayoutQuery?.removeEventListener('change', this._onMobileLayoutChange);
    this._mobileLayoutQuery = null;
    document.removeEventListener('keydown', this._onKeydown);
    document.removeEventListener(CHAT_PALETTE_OPEN_REQUEST_EVENT, this._onPaletteOpenRequest);
    document.removeEventListener('sl-show', this._onDocumentModalShow);
    window.removeEventListener('popstate', this._onPopState);
    this._paletteDocumentsUnsubscribe?.();
    this._paletteDocumentsUnsubscribe = null;
    this._paletteAgentsRelease?.();
    this._paletteAgentsRelease = null;
    this._paletteDataController.cancel();
    // Same reasoning as `_closePaletteAndCancelLoad`'s own close-time
    // handling, both parts: the seq bump is the correctness guard (a
    // disconnect-time abort resolves to '', and without this an in-flight
    // People load would still publish that as a visible identity error on
    // the now-detached page); the abort itself is the resource-usage
    // measure (don't leave the fetch running for up to AGENTS_IDLE_TIMEOUT_MS
    // on a page nothing can use the result of).
    this._peopleLoadSeq++;
    this._selfUserAbortController?.abort();
    this._selfUserAbortController = null;
    this._stopPaletteVisibilityWatchdog();
    this._stopPaletteDebouncedRefresh();
    // Without this, a disconnect landing while a first-open lazy import is
    // still in flight would leave `_palettePendingOpen` stuck true on the
    // detached page — the suspended togglePalette call would resume anyway
    // (nothing observes disconnection), setting v2PaletteOpen and issuing
    // the agents/DM GETs on a page no longer in the document. The
    // visibility watchdog would still close it shortly after
    // (off-route/invisible once detached), so nothing gets permanently
    // stuck, but this work should be aborted on controller disposal. A
    // reopen queued behind a still-animating close would otherwise resume
    // the same way once that close's sl-after-hide eventually fires. Reset
    // `_paletteCloseAnimating` too: if the page detaches mid-hide, that
    // close's own `sl-after-hide` may never arrive, leaving it stuck `true`
    // forever — every later Cmd/Ctrl+K on a reconnected page would then queue
    // behind a close that already isn't happening, instead of opening.
    this._palettePendingOpen = false;
    this._palettePendingReopen = false;
    this._paletteCloseAnimating = false;
    stateManager.removeEventListener('chat-message-received', this._onChatMessage);
    stateManager.removeEventListener('chat-topic-updated', this._onChatTopic);
    stateManager.removeEventListener('chat-presence-updated', this._onPresenceUpdated);
    stateManager.removeEventListener('chat-typing-received', this._onChatTyping);
    stateManager.removeEventListener('agents-updated', this._onAgentsUpdated);
    stateManager.removeEventListener('agent-created', this._onAgentCreated);
    stateManager.removeEventListener('scope-changed', this._onScopeChanged);
    stateManager.removeEventListener('chat-dm-promoted', this._onDMPromoted);
    stateManager.removeEventListener('chat-read-state-updated', this._onOwnReadStateSSE);
    this.removeEventListener('rail-loaded', this._onRailLoaded);
    this.removeEventListener('read-state-updated', this._onReadStateUpdated);
    this.removeEventListener('conversation-marked-unread', this._onConversationMarkedUnread);
    this.stopPresenceHeartbeat();
    // Clean up the fallback poll
    if (this._fallbackPollInterval) {
      clearInterval(this._fallbackPollInterval);
      this._fallbackPollInterval = null;
    }
    // Clean up the canAttach refresh timer
    if (this._canAttachRefreshTimer != null) {
      clearTimeout(this._canAttachRefreshTimer);
      this._canAttachRefreshTimer = null;
    }
    // Clean up typing timers
    for (const timer of this._typingTimers.values()) {
      clearTimeout(timer);
    }
    this._typingTimers.clear();
    if (this._refreshTimer) {
      clearTimeout(this._refreshTimer);
      this._refreshTimer = null;
    }
    // Nothing is on screen any more, so nothing is being actively read.
    chatNotifications.setActiveConversation(null);
  }

  override updated(changedProperties: Map<string, unknown>): void {
    if (changedProperties.has('pageData') && this.pageData) {
      this.parseV2Route();
    }
    // A message arriving in the conversation already on screen should not
    // also pop a desktop notification about it. Reported from updated()
    // rather than from each of the many places v2Conversation is assigned.
    if (changedProperties.has('v2Conversation')) {
      chatNotifications.setActiveConversation(this.v2Conversation?.conversationKey ?? null);

      const projectId = this.v2Conversation?.projectId || '';
      if (projectId !== this._chimeProjectId) {
        this._chimeProjectId = projectId;
        this.projectChimeOn = projectId ? isProjectChimeEnabled(projectId) : true;
      }

      // Any assignment of v2Conversation to a truthy value — not only an
      // actual "open a conversation" navigation, but also e.g. a mute toggle
      // or default-agent edit on the conversation already open — retires any
      // hub-members walk started before it, by generation rather than by the
      // (racy) current value of v2Conversation at publish time — see
      // _hubMembersGeneration's doc comment. Reported from updated(), the
      // same centralized spot as the notification handling above, rather
      // than from each of the many places v2Conversation is assigned.
      if (this.v2Conversation) {
        ++this._hubMembersGeneration;
      }
    }
  }

  /** Handle selections from the rail header's options menu (currently: chime toggle). */
  private handleRailMenuSelect(e: Event): void {
    const detail = (e as CustomEvent<{ item?: HTMLElement }>).detail;
    const value = detail?.item?.getAttribute('value');
    if (value === 'toggle-chime') {
      const projectId = this.v2Conversation?.projectId;
      if (projectId) {
        this.projectChimeOn = !this.projectChimeOn;
        this._chimeProjectId = projectId;
        setProjectChimeEnabled(projectId, this.projectChimeOn);
      }
    }
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
    stateManager.addEventListener('agent-created', this._onAgentCreated);
    stateManager.addEventListener('scope-changed', this._onScopeChanged);
    stateManager.addEventListener('chat-dm-promoted', this._onDMPromoted);
    stateManager.addEventListener('chat-read-state-updated', this._onOwnReadStateSSE);

    // Listen for rail-loaded to set up the SSE scope with space IDs
    this.addEventListener('rail-loaded', this._onRailLoaded);

    // The open thread advances its own read watermark; the rail and the DM
    // unread dots are separate components with no way to observe that.
    this.addEventListener('read-state-updated', this._onReadStateUpdated);

    // The rail's own "Mark unread" click, for same-tab suppression without
    // waiting on the SSE round trip. The members sidebar's equivalent
    // ('member-marked-unread') is handled inline by handleMemberMarkedUnread
    // rather than through this same listener, since it also carries the
    // peerId needed for the dot.
    this.addEventListener('conversation-marked-unread', this._onConversationMarkedUnread);

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
        // The one caller that exists specifically because the hub member
        // list might have changed since the last load — unlike the
        // route/view re-parses elsewhere, which just want whatever walk is
        // already in flight.
        void this.loadHubMembers({ refresh: true });
      }
    }, FALLBACK_POLL_INTERVAL_MS);
  }

  /** Called when the space rail finishes loading its data. Sets up the SSE scope. */
  private handleRailLoaded(e: Event): void {
    const detail = (e as CustomEvent).detail as {
      spaceIds: string[];
      spaces?: Array<{
        projectId: string;
        projectSlug: string;
        projectName: string;
        unreadCount?: number;
      }>;
    };

    // The rail just loaded the space rollup the tab-title badge needs; hand it
    // over rather than fetching /chat/spaces again alongside it.
    if (detail.spaces) {
      chatUnread.setSpaceUnread(detail.spaces);
    }

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

  /**
   * Is a non-DM thread with this project and conversation key already the
   * open conversation? `parseV2Route` calls this before rebuilding
   * `v2Conversation` and resetting `mobilePanel` to 'center' for a thread
   * route, because it runs on every re-parse of the current URL, not only
   * on a real navigation — a swipe to a different mobile panel never
   * changes the URL, so without this guard any later re-parse (the rail's
   * `rail-loaded` event, dispatched again after every SSE-triggered
   * reload) silently snaps the view back to the conversation the URL still
   * names, overriding a manual swipe away from it.
   */
  private isAlreadyViewingThread(projectId: string, threadId: string): boolean {
    return !!this.openThread(projectId, threadId);
  }

  /** The open conversation, if it is this thread; otherwise null. */
  private openThread(projectId: string, threadId: string): V2ConversationState | null {
    const conv = this.v2Conversation;
    return conv && !conv.isDM && conv.projectId === projectId && conv.conversationKey === threadId
      ? conv
      : null;
  }

  /** Does the current URL still name this readable thread route? */
  private routeNamesThread(slug: string, threadId: string): boolean {
    const match = window.location.pathname.match(/\/chat\/([^/]+)\/([^/]+)$/);
    return (
      !!match && decodeURIComponent(match[1]) === slug && decodeURIComponent(match[2]) === threadId
    );
  }

  /** Does the current URL still name this space route, on a mounted page? */
  private routeNamesSpace(slug: string): boolean {
    const match = window.location.pathname.match(/\/chat\/([^/]+)$/);
    return this.isConnected && !!match && decodeURIComponent(match[1]) === slug;
  }

  /** Does the current URL still name this peer-ID DM route? */
  private routeNamesDMPeer(peerId: string): boolean {
    const match = window.location.pathname.match(/\/chat\/dm\/([^/]+)$/);
    return !!match && decodeURIComponent(match[1]) === peerId;
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
      // Rewrite to the readable URL in place once the slug is known, then
      // parse that. navigateTo would rebuild the page (resetting the mobile
      // panel the user may have swiped to since) and push an entry that Back
      // lands on only to redirect forward again.
      const slug = this._projectIdToSlug.get(projectId);
      if (slug) {
        const open = this.openThread(projectId, topicId);
        if (open && open.projectSlug !== slug) {
          this.v2Conversation = { ...open, projectSlug: slug };
        }
        void replaceRoute(`/chat/${encodeURIComponent(slug)}/${encodeURIComponent(topicId)}`).then(
          () => {
            // The shell titles itself from the path it now records; put the
            // open thread's own title back on top.
            const conv = this.openThread(projectId, topicId);
            if (conv) {
              dispatchPageTitle(this, conv.threadName ? `#${conv.threadName}` : 'Thread', 'Chat');
            }
          }
        );
        this.parseV2Route();
        return;
      }
      // See isAlreadyViewingThread's doc comment for why this guard matters.
      if (this.isAlreadyViewingThread(projectId, topicId)) return;
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

      if (segment.startsWith('dm:')) {
        this.mobilePanel = 'center';
        dispatchPageTitle(this, 'DM', 'Chat');
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
          // A /chat/dm/<peerId> URL never equals the open conversation's key,
          // so a re-parse (rail-loaded) lands here for a DM that is already
          // open. If it is the same peer, the key is only being corrected
          // (the agent roster arrived after the first parse): update it in
          // place and leave the mobile panel where the user put it.
          const samePeer = !!this.v2Conversation?.isDM && this.v2Conversation.peerId === segment;
          if (!samePeer) {
            this.mobilePanel = 'center';
            dispatchPageTitle(this, 'DM', 'Chat');
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
          void this.resolveDMByPeerId(segment, peerKind, '', { fromRoute: true });
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
        // See isAlreadyViewingThread's doc comment for why this guard matters.
        if (this.isAlreadyViewingThread(projectId, threadId)) return;
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
        void this.selectSpaceBySlug(segment, projectId);
        return;
      }

      // If slug map isn't populated yet (cold load), try resolving via API.
      // If it turns out not to be a project slug, resolveSlugAndOpenSpace is a
      // no-op and the URL stays as-is.
      if (this._slugToProjectId.size === 0) {
        void this.resolveSlugAndOpenSpace(segment);
        return;
      }

      // Not a project slug — fall through to clear conversation state.
    }

    // /chat — no conversation selected, show hub-level members
    this.v2Conversation = null;
    this.v2MembersExpanded = true; // Always show tray in base view (no header toggle available)
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
    // While the lookup was in flight the rail may have opened this thread
    // (and the user swiped away from it), or the user may have moved on to
    // another conversation. Only open it if the URL still names it and it is
    // not already open.
    if (
      !this.routeNamesThread(slug, threadId) ||
      this.isAlreadyViewingThread(projectId, threadId)
    ) {
      return;
    }

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
    // The rail can load during the await and the user open a thread from it,
    // or the page be rebuilt; only open the space if it is still what the
    // URL names on this page.
    if (projectId && this.routeNamesSpace(slug)) {
      void this.selectSpaceBySlug(slug, projectId);
    }
    // If resolution fails, leave the URL in place — it's just a 404 space.
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

    // Find #general thread (or fall back to first thread) for this space
    const threads = await this.loadSpaceThreads(projectId);
    // Same as after the slug lookup: a thread opened during the await wins.
    if (!this.routeNamesSpace(slug)) return;
    const target = threads.find((t: { isGeneral: boolean }) => t.isGeneral) || threads[0];
    if (target) {
      this.v2Conversation = {
        conversationKey: target.id,
        projectId,
        projectSlug: slug,
        threadName: target.name,
        defaultAgent: target.defaultAgent || '',
        isDM: false,
        peerName: '',
        peerId: '',
        peerKind: 'user',
      };
      this.mobilePanel = 'center';
      void this.loadV2Members(projectId);
      dispatchPageTitle(this, `#${target.name}`, 'Chat');
      // Update URL to include the thread
      navigateTo(`/chat/${encodeURIComponent(slug)}/${encodeURIComponent(target.id)}`);
    }
  }

  /**
   * Fetch threads for a space from the API.
   */
  private async loadSpaceThreads(
    projectId: string
  ): Promise<Array<{ id: string; name: string; isGeneral: boolean; defaultAgent?: string }>> {
    try {
      const res = await apiFetch(`/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`);
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
    let hasNewAgent = false;

    for (const agent of stateManager.getAgents()) {
      const existing = byId.get(agent.id);
      if (!existing && !inScope(agent.projectId || '')) continue;
      // A prior authoritative refetch left this ID out of the members
      // response. Don't re-add it from stateManager's stale cache until a
      // legitimate SSE `created` event confirms it (see _handleAgentCreated) —
      // otherwise every SSE tick re-adds it and re-triggers the refetch that
      // removes it again (the flicker loop).
      if (!existing && this._serverOmittedAgentIds.has(agent.id)) continue;
      if (!existing) hasNewAgent = true;
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
        // SSE deltas carry status, not authorization. Preserve what the
        // members endpoint decided; rebuilding without it restores the
        // terminal control on every status tick.
        canAttach: existing?.canAttach,
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

    // When a brand-new agent appears via SSE, its `canAttach` is unknown
    // (SSE events carry status, not per-viewer authorization). Schedule a
    // debounced re-fetch of the members endpoint so the terminal icon
    // appears without waiting for the 60-second fallback poll.
    if (hasNewAgent && this.v2Conversation && !this.v2Conversation.isDM) {
      const projectId = this.v2Conversation.projectId;
      if (this._canAttachRefreshTimer != null) {
        clearTimeout(this._canAttachRefreshTimer);
      }
      this._canAttachRefreshTimer = setTimeout(() => {
        this._canAttachRefreshTimer = null;
        if (projectId && this.v2Conversation?.projectId === projectId) {
          void this.loadV2Members(projectId);
        }
      }, 1000);
    }

    // A deleted agent cannot remain the thread default. The server clears the
    // binding and emits topic-updated; this covers the open view even if that
    // event is missed. defaultAgent holds a slug or an ID, so both are checked.
    const conv = this.v2Conversation;
    if (conv?.defaultAgent && deletedRefs.has(conv.defaultAgent)) {
      this.v2Conversation = { ...conv, defaultAgent: '' };
    }
  }

  /**
   * A legitimate SSE `created` event arrived for this agent ID. Lift the
   * loop guard set by loadV2Members' reconciliation, if any, so a real
   * re-creation (or ID reuse) isn't permanently suppressed.
   */
  private _handleAgentCreated(e: Event): void {
    const detail = (e as CustomEvent).detail as Record<string, unknown> | undefined;
    const eventData = (detail?.data ?? detail) as Record<string, unknown> | undefined;
    const agentId = eventData?.agentId as string | undefined;
    if (agentId) {
      this._serverOmittedAgentIds.delete(agentId);
    }
  }

  /**
   * A conversation's read watermark moved (dispatched by chat-thread after a
   * successful POST). Clear the matching unread markers without a round trip,
   * then re-sync from the server to account for messages arriving meanwhile.
   */
  private _handleReadStateUpdated(e: Event): void {
    const detail = (e as CustomEvent).detail as { conversationKey?: string } | undefined;
    const key = detail?.conversationKey || '';
    if (!key) return;

    if (key.startsWith('dm:')) {
      // The acknowledgement belongs to its key, even if navigation changed
      // the selected conversation before this event was delivered.
      const parts = key.split(':');
      const userId = this.pageData?.user?.id;
      let peerId = '';
      if (parts.length === 5) {
        if (parts[1] === 'user' && parts[2] === userId) peerId = parts[4];
        else if (parts[3] === 'user' && parts[4] === userId) peerId = parts[2];
      }
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

  /**
   * A read-state change arrived over the SSE 'chat-read-state-updated'
   * event. Only the caller's OWN watermark moving via mark-unread is this
   * handler's concern — a DM peer's "seen" receipt (a different userId) is
   * chat-thread's. The `unread` field, not the userId match, is what
   * identifies a mark-unread event: userId alone would also match a future
   * self-notifying /read, which must NOT re-mark the conversation unread.
   * Mirrors _handleReadStateUpdated's DM-peer-ID resolution, but marks the
   * thread/DM unread rather than read.
   */
  private _handleOwnReadStateSSE(e: Event): void {
    type ReadStateData = { conversationKey?: string; userId?: string; unread?: boolean };
    const detail = (e as CustomEvent).detail as
      | ({ data?: ReadStateData } & ReadStateData)
      | undefined;
    const eventData: ReadStateData | undefined = detail?.data ?? detail;
    const key = eventData?.conversationKey || '';
    const userId = this.pageData?.user?.id;
    if (!key || !userId || eventData?.userId !== userId || eventData?.unread !== true) return;

    if (key.startsWith('dm:')) {
      const parts = key.split(':');
      let peerId = '';
      if (parts.length === 5) {
        if (parts[1] === 'user' && parts[2] === userId) peerId = parts[4];
        else if (parts[3] === 'user' && parts[4] === userId) peerId = parts[2];
      }
      this.applyDMMarkedUnread(peerId);
      this.suppressOpenThreadAutoAdvance(key);
      return;
    }

    const rail = this.shadowRoot?.querySelector('scion-chat-space-rail') as
      | import('../shared/chat/chat-space-rail.js').ScionChatSpaceRail
      | null;
    rail?.markThreadUnread(key);
    this.suppressOpenThreadAutoAdvance(key);
  }

  /**
   * Record that a DM peer's conversation was just marked unread: add it to
   * the unread-dot list unless the DM is muted (muting suppresses the dot
   * regardless of why the watermark moved — #1029 — so a mark-unread on a
   * muted DM rewinds the watermark without ever showing a dot for it), and —
   * regardless of mute — update `v2DMInfoByPeerId[peerId].hasUnread` so the
   * members sidebar's `canMarkUnread` sees the change immediately.
   *
   * Without this second part the map only refreshes on the next
   * `loadUnreadDMPeers` (on connect, an inbound message, a normal /read, or
   * the 60s fallback poll), so "Mark unread" would stay offered after a
   * successful click until that next refresh — up to 60s on a quiet DM,
   * muted or not, since `/chat/dms` reports `hasUnread` independently of
   * mute.
   */
  private applyDMMarkedUnread(peerId: string): void {
    if (!peerId) return;
    const info = this.v2DMInfoByPeerId[peerId];
    if (info && !info.hasUnread) {
      this.v2DMInfoByPeerId = { ...this.v2DMInfoByPeerId, [peerId]: { ...info, hasUnread: true } };
    }
    if (this.v2UnreadFromIds.includes(peerId) || info?.muted) return;
    this.v2UnreadFromIds = [...this.v2UnreadFromIds, peerId];
  }

  /**
   * If `key` is the conversation currently open in the center panel, tell it
   * directly to suppress auto-advance. Used both by the SSE path above (for
   * this tab's own echo, and other tabs) and, more importantly, right after
   * this tab's own "Mark unread" POST succeeds — same-tab suppression must
   * not wait on the SSE round trip.
   */
  private suppressOpenThreadAutoAdvance(key: string): void {
    if (!key || this.v2Conversation?.conversationKey !== key) return;
    const thread = this.shadowRoot?.querySelector('scion-chat-thread') as
      | import('../shared/chat/chat-thread.js').ScionChatThread
      | null;
    thread?.suppressAutoAdvance();
  }

  /**
   * The rail's own "Mark unread" click succeeded — same-tab suppression path.
   * The DM/members equivalent is handled inline in handleMemberMarkedUnread
   * since it also needs the peerId for the dot.
   */
  private _handleConversationMarkedUnread(e: Event): void {
    const detail = (e as CustomEvent).detail as { conversationKey?: string } | undefined;
    if (detail?.conversationKey) this.suppressOpenThreadAutoAdvance(detail.conversationKey);
  }

  private handleChatMessage(e: Event): void {
    // A message can move People or Threads recency — a DM message affects
    // People, a thread message affects Threads — and the event detail
    // doesn't cheaply distinguish which without parsing the full envelope
    // this handler otherwise ignores, so mark both stale, and Agents
    // recency with them.
    this._markPaletteGroupsDirty('people', 'threads');
    this._markAgentDmsStale();

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
    this._markPaletteGroupsDirty('threads');
    const eventDetail = (e as CustomEvent).detail as Record<string, unknown> | undefined;
    // Unwrap the notifyWithData envelope: { state, data: { action, topic: {...} } }
    const eventData = (eventDetail?.data ?? eventDetail) as Record<string, unknown> | undefined;
    const action = eventData?.action as string | undefined;
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

    // Skip reload for topics this client just created — already handled
    // optimistically by the rail's submitCreateThread.
    const rail = this.shadowRoot?.querySelector('scion-chat-space-rail') as
      | import('../shared/chat/chat-space-rail.js').ScionChatSpaceRail
      | null;
    if (action === 'created' && topicId && rail?._recentlyCreatedTopicIds?.has(topicId)) {
      rail._recentlyCreatedTopicIds.delete(topicId);
      return;
    }

    // For other topic changes (rename, delete, etc.), reload rail
    if (rail) void rail.reload();
  }

  /**
   * Handle a `dm.promoted` SSE event. If the currently-viewed DM was promoted
   * (possibly from another tab), navigate to the new thread and remove the DM
   * from the switcher cache.
   */
  private handleDMPromoted(e: Event): void {
    // A promoted DM disappears from People recency and appears as a new
    // Threads row, so mark both groups stale, and Agents recency with them.
    this._markPaletteGroupsDirty('people', 'threads');
    this._markAgentDmsStale();
    const detail = (e as CustomEvent).detail as Record<string, unknown> | undefined;
    const eventData = (detail?.data ?? detail) as Record<string, unknown> | undefined;
    const oldConversationKey = eventData?.oldConversationKey as string | undefined;
    const newTopic = eventData?.newTopic as
      | {
          id: string;
          projectId: string;
          name: string;
          defaultAgent?: string;
        }
      | undefined;
    if (!oldConversationKey || !newTopic) return;

    // If we're currently viewing the promoted DM, navigate to the new thread
    if (this.v2Conversation?.conversationKey === oldConversationKey) {
      this.promoteDialogOpen = false;
      this.navigateToPromotedThread(newTopic);
      this.showPromoteToast(`Conversation promoted to #${newTopic.name}`, 'success');
    }

    // Reload the space rail so the new thread appears
    const rail = this.shadowRoot?.querySelector('scion-chat-space-rail') as
      | import('../shared/chat/chat-space-rail.js').ScionChatSpaceRail
      | null;
    if (rail) void rail.reload();
  }

  private handleThreadSelect(e: CustomEvent): void {
    this.navigateToThread(
      e.detail as {
        conversationKey: string;
        projectId: string;
        projectSlug?: string;
        threadName: string;
        defaultAgent?: string;
      }
    );
  }

  /**
   * In-page thread navigation shared by the space rail's `thread-select`
   * event and the palette's Threads group selection
   * (`_handlePaletteSelect`) — the one routine both callers share, so they
   * route identically. Prefers
   * `detail.projectSlug` (the target's own known slug) over a locally cached
   * one for the same project ID, then falls back to the `_projectIdToSlug`
   * map; when neither exists, routes by project ID rather than guessing
   * another project's slug — `_slugToProjectId`'s own keys are never
   * consulted here as a fallback.
   */
  private navigateToThread(detail: {
    conversationKey: string;
    projectId: string;
    projectSlug?: string;
    threadName: string;
    defaultAgent?: string;
  }): void {
    ++this._userNavSeq;
    // Determine the slug for the readable URL
    const slug = detail.projectSlug || this._projectIdToSlug.get(detail.projectId) || '';

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
    ++this._userNavSeq;
    this.v2Conversation = null;
    this.v2MembersExpanded = true; // Always show tray in base view
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
      const res = await apiFetch(`/api/v1/chat/topics/${encodeURIComponent(conversationKey)}`);
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
    displayName = '',
    opts: { fromRoute?: boolean } = {}
  ): Promise<void> {
    // A lookup can be overtaken while it awaits. A route-driven one opens the
    // DM only if the URL still names this peer and its DM is not already open
    // (a re-parse on rail-loaded starts another); one the user started opens
    // it only if they have not navigated since, including by opening another
    // DM. Either way a late answer never moves the panel the user has since
    // moved. The URL alone cannot tell this for a user-started lookup: an
    // earlier lookup's own push changes it without the user doing anything.
    const startSeq = this._userNavSeq;
    const superseded = (): boolean =>
      opts.fromRoute
        ? !this.routeNamesDMPeer(peerId) ||
          (!!this.v2Conversation?.isDM && this.v2Conversation.peerId === peerId)
        : this._userNavSeq !== startSeq;
    // A DM the user opened gets its own URL, as openDM gives it when the key
    // is known; otherwise the next re-parse of the old route would close it.
    const pushIfOpenedByUser = (dmKey: string): void => {
      if (!opts.fromRoute) this.pushDMPath(dmKey);
    };

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
          if (superseded()) return;
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
          this.mobilePanel = 'center';
          pushIfOpenedByUser(dm.conversationKey);
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

    // 3. Retry key construction with the potentially-refreshed user ID. A
    // lookup overtaken by now stops here, so it neither opens the DM nor
    // reports a missing identity.
    if (superseded()) return;
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
      this.mobilePanel = 'center';
      pushIfOpenedByUser(key);
      dispatchPageTitle(this, displayName || 'DM', 'Chat');
      return;
    }

    // 4. Unable to resolve — log error.
    console.error('Unable to open DM — user identity not available. Please refresh the page.');
  }

  /**
   * Load hub-level members (every user and agent in the hub, fully
   * paginated) for the members sidebar when no specific space/project is
   * selected.
   *
   * Entry point for {@link _hubMembersScheduled}'s coalescing gate — see its
   * doc comment for why this has to batch its many call sites rather than
   * issue one request per call. Synchronous and fire-and-forget so every
   * existing `void this.loadHubMembers();` call site keeps working unchanged.
   *
   * `options.refresh` distinguishes the two kinds of caller: route/view
   * re-parses (the default) just want the current hub-wide view and are
   * content to join a walk already in flight *for the current generation* —
   * a walk still running from before the last conversation opened is for a
   * view that's since moved on, so "join" only ever means the former, never
   * that one. Only the fallback poll — the one caller that exists
   * specifically because something *might* have changed since the last
   * load — passes `{ refresh: true }` to queue a trailing walk when one is
   * already running.
   *
   * The generation is captured here, at schedule time, rather than read
   * fresh by `_runHubMembersLoad` once its `queueMicrotask` callback
   * actually runs — a call that schedules the walk and is then followed by
   * `disconnectedCallback` in the same microtask drain would otherwise have
   * its walk start under the *post-disconnect* generation (read late,
   * inside the callback) instead of the one active when it was requested,
   * defeating the disconnect's own invalidation.
   *
   * A consequence of that capture: a further synchronous call landing after
   * such a disconnect, in the same microtask drain and before the scheduled
   * callback runs, coalesces into the walk already scheduled (captured with
   * the pre-disconnect generation) and is therefore dropped along with it.
   * The reconnect path reaches this method only after `initV2` awaits its
   * lazy imports, so it cannot land in that window as the code stands.
   */
  private loadHubMembers(options?: { refresh?: boolean }): void {
    // Claim the sidebar for the hub view before any early return below, so a
    // project or presence response still in flight from the view the user
    // just left is discarded even when this call only joins a walk.
    ++this._membersViewSeq;
    const inFlightForThisGeneration =
      this._hubMembersInFlight && this._hubMembersInFlightGeneration === this._hubMembersGeneration;
    if (inFlightForThisGeneration) {
      if (options?.refresh) this._hubMembersReloadQueued = true;
      return;
    }
    if (this._hubMembersScheduled) return;
    this._hubMembersScheduled = true;
    const scheduledGeneration = this._hubMembersGeneration;
    queueMicrotask(() => {
      void this._runHubMembersLoad(scheduledGeneration);
    });
  }

  /**
   * Runs the actual walk, then — if a refresh was requested while it was in
   * flight — runs exactly one more before releasing the gate. A refresh
   * requested during that trailing walk re-sets the same flag rather than
   * queuing a second one, so three triggers during one walk still produce
   * only one trailing reload.
   *
   * Stops instead of starting a trailing walk once a specific conversation
   * (a project or DM) is open, or once the element has been disconnected —
   * in both cases, the hub-wide view this walk is for is no longer on
   * screen, so a trailing walk would have nothing valid to publish into.
   *
   * Records itself as the owner of `_hubMembersInFlight` for its generation,
   * and only clears that flag in `finally` if it is still the owner — a walk
   * that outlives a disconnect-then-reconnect must not clear the flag out
   * from under the fresh walk the reconnect started.
   *
   * Also re-runs, the same as a queued refresh, when
   * `_fetchHubMembersOnce` returns true — one of this attempt's legs stopped
   * early rather than completing — that attempt's result is never published
   * (see {@link PaginationStoppedError}), so without a re-run here nothing
   * would publish a usable list for however many callers joined this walk.
   *
   * `generation` is the value {@link loadHubMembers} captured when it
   * scheduled this call, not read fresh from `_hubMembersGeneration` here —
   * see that method's doc comment for why reading it late would let a
   * disconnect landing before this callback runs go unnoticed.
   */
  private async _runHubMembersLoad(generation: number): Promise<void> {
    this._hubMembersScheduled = false;
    this._hubMembersInFlight = true;
    this._hubMembersInFlightGeneration = generation;
    try {
      let stopped: boolean;
      do {
        this._hubMembersReloadQueued = false;
        stopped = await this._fetchHubMembersOnce(generation);
      } while (
        (this._hubMembersReloadQueued || stopped) &&
        generation === this._hubMembersGeneration &&
        !this.v2Conversation
      );
    } finally {
      if (this._hubMembersInFlightGeneration === generation) {
        this._hubMembersInFlight = false;
      }
    }
  }

  /**
   * One full users+agents walk. Publishes each list only once its own walk
   * completes, and only on success — a failed walk (any page) leaves that
   * list exactly as it was, so the sidebar never blanks on error, while
   * the other list (fetched in parallel) still updates on its own success.
   * A blanket try/catch wraps the publish: an
   * unexpected failure anywhere in the assignment below (not just a rejected
   * walk) leaves both lists exactly as they were rather than throwing out of
   * this `queueMicrotask`-scheduled call, where nothing would catch it.
   *
   * Skips publishing entirely if a specific conversation is open, or the
   * element has disconnected, by the time the walk (which can take several
   * page-fetches) finishes — a hub-wide walk that started while the global
   * `/chat` view was showing must not overwrite a project's or DM's member
   * list, or the shared `v2Members` roster, with every user and agent in the
   * hub just because it happened to land after the user navigated away or
   * left the page.
   *
   * Also stops requesting further pages, rather than merely discarding the
   * result, once either of those becomes true mid-walk: `shouldContinue`
   * below is checked by `paginateAll` before every page of both walks, so a
   * stale walk stops hitting the server as soon as the view it was for is
   * gone instead of running to completion for no reason.
   *
   * `generation` is passed in by {@link _runHubMembersLoad} rather than
   * re-read from `_hubMembersGeneration` here, so both always agree on which
   * walk this is.
   *
   * Both `this.v2Conversation` and the generation are checked below, and
   * neither subsumes the other: `this.v2Conversation` is a plain field read,
   * so it reflects a conversation opening the instant it's assigned — it
   * stops a page fetch (via `shouldContinue`) the moment one is open,
   * without waiting on anything. The generation instead catches the case
   * `this.v2Conversation` cannot: a conversation that opened *and closed
   * again* before this walk settled reads clear here even though the walk is
   * for a hub view that's already been superseded once — see
   * `_hubMembersGeneration`'s doc comment for that failure mode. Dropping
   * either check reopens the gap the other one covers.
   *
   * Neither check, though, covers a conversation that opened *and closed
   * again within the same Lit update batch* — `updated()` never sees the
   * open at all in that case, so the generation never bumps, yet `shouldContinue`
   * (a live field read) still stops a page fetch for the moment the
   * conversation was open. A leg stopped that way rejects with
   * {@link PaginationStoppedError} rather than resolving with its partial
   * result, so the `status === 'fulfilled'` checks below already can't
   * publish *that* leg.
   *
   * The guard below also skips publishing the *other* leg when this
   * attempt had a stopped leg. That leg (for example a single-page one whose
   * only `shouldContinue` check passed before the conversation opened) did
   * complete, and its data is for the hub view that is still on screen —
   * but this attempt is going to be re-run regardless, and the re-run
   * publishes both lists. Publishing the completed leg now would only
   * publish that list twice, once from this abandoned attempt and once from
   * the re-run, with the two lists in between coming from different
   * attempts. Skipping it keeps each publish to a single attempt in which
   * both legs ran to their end (completed or failed).
   *
   * Returns whether either leg stopped early via `shouldContinue`, computed
   * from this attempt's own results only. {@link _runHubMembersLoad} uses it
   * to decide whether to re-run. It is a per-attempt value rather than
   * shared state, so a superseded walk stopping at a page boundary cannot
   * affect the decisions of a fresh walk running for the current generation.
   */
  private async _fetchHubMembersOnce(generation: number): Promise<boolean> {
    const shouldContinue = (): boolean =>
      !this.v2Conversation && generation === this._hubMembersGeneration;
    const [usersResult, agentsResult] = await Promise.allSettled([
      paginateAll({
        path: '/api/v1/users',
        pageSize: HUB_MEMBERS_PAGE_SIZE,
        parsePage: parseHubUsersPage,
        label: 'users list',
        shouldContinue,
      }),
      paginateAll({
        path: '/api/v1/agents',
        pageSize: HUB_MEMBERS_PAGE_SIZE,
        parsePage: parseHubAgentsPage,
        label: 'agents list',
        shouldContinue,
      }),
    ]);

    const isStopped = (r: PromiseSettledResult<unknown>): boolean =>
      r.status === 'rejected' && r.reason instanceof PaginationStoppedError;
    const stopped = isStopped(usersResult) || isStopped(agentsResult);

    try {
      // The hub view this walk is for may no longer be on screen by the time
      // it finishes, or this attempt had a leg stop early and will be
      // re-run — see the doc comment above.
      if (this.v2Conversation || generation !== this._hubMembersGeneration || stopped) {
        return stopped;
      }

      if (usersResult.status === 'fulfilled') {
        // /api/v1/users carries no presence state. Preserve whatever
        // refreshHubMemberPresence() (or an SSE presence event) already
        // merged in, otherwise the periodic poll would blank out every
        // presence indicator in the base chat view.
        const currentPresence = new Map<string, 'active' | 'idle'>();
        for (const h of this.v2HumanMembers) {
          if (h.presenceState) currentPresence.set(h.id, h.presenceState);
        }
        // /api/v1/users paginates by creation-time offset, not a stable
        // keyset cursor, so a signup or deletion mid-walk can shift page
        // boundaries and return the same user twice. De-dupe by id (keeping
        // the first occurrence) so that can't produce duplicate-keyed rows.
        const seenUserIds = new Set<string>();
        this.v2HumanMembers = usersResult.value
          .filter((u) => {
            if (seenUserIds.has(u.id)) return false;
            seenUserIds.add(u.id);
            return true;
          })
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
      // A failed users walk leaves this.v2HumanMembers untouched — non-critical,
      // sidebar keeps showing what it already had.

      if (agentsResult.status === 'fulfilled') {
        this.v2AgentMembers = agentsResult.value.map((a) => ({
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
          canAttach: a.canAttach,
        }));
        // Seed the shared agent map so SSE status deltas have a baseline to
        // merge onto — otherwise they are buffered and never notify.
        stateManager.seedAgents(this.v2AgentMembers.map(agentMemberToAgent));
      }
      // A failed agents walk leaves this.v2AgentMembers untouched, same as users above.

      // Also populate legacy v2Members for thread @-mention support — rebuilt
      // from whatever the current v2HumanMembers/v2AgentMembers are, so a
      // partial failure above (existing list kept) is reflected here too.
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
      // Non-critical — sidebar keeps whatever it already had.
    }
    return stopped;
  }

  /**
   * Fetch presence data from a space's members endpoint and merge it into the
   * hub-level human members list.  Called from handleRailLoaded when the user
   * is in the global /chat view so that presence indicators render on first load.
   */
  private async refreshHubMemberPresence(projectId: string): Promise<void> {
    if (!projectId) return;
    // Captured, not bumped: this only merges presence fields into whatever
    // loadHubMembers/loadV2Members last populated, it doesn't replace that
    // view's arrays. If the view has since moved on, bail instead of
    // merging hub presence onto another view's members.
    const seq = this._membersViewSeq;
    try {
      const res = await apiFetch(`/api/v1/chat/spaces/${encodeURIComponent(projectId)}/members`);
      if (!res.ok) return;
      const data = (await res.json()) as {
        humans?: Array<{ id: string; presenceState?: 'active' | 'idle' | '' }>;
      };
      // Check after the last await, immediately before the first write —
      // a newer load could otherwise land in the window between the body
      // resolving and this check.
      if (seq !== this._membersViewSeq) return;
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
    const requestId = ++this._unreadDMRequestId;
    try {
      const res = await apiFetch('/api/v1/chat/dms');
      if (!res.ok) return;
      const data = (await res.json()) as {
        dms?: Array<{
          conversationKey: string;
          peerId: string;
          hasUnread: boolean;
          muted?: boolean;
          lastMessageId?: string;
        }>;
      };
      // A muted DM raises no dot: muting is the user saying "stop telling me
      // about this", and the avatar dot is the telling (#1029).
      if (requestId !== this._unreadDMRequestId) return;
      const unreadIds = (data?.dms || [])
        .filter((dm) => dm.hasUnread && !dm.muted)
        .map((dm) => dm.peerId);
      // Same list the tab-title badge counts — reuse the response.
      chatUnread.setDMUnread(data?.dms || []);
      // Only update if changed to avoid unnecessary re-renders
      if (
        unreadIds.length !== this.v2UnreadFromIds.length ||
        unreadIds.some((id, i) => id !== this.v2UnreadFromIds[i])
      ) {
        this.v2UnreadFromIds = unreadIds;
      }
      // Which members have an existing, non-empty DM, its key, mute state,
      // and real (mute-independent) unread state — the members sidebar's
      // "Mark unread" needs this to know what to act on, to hide itself for
      // a member with no DM (or a DM with no messages yet, which
      // mark-unread has nothing to do to) or one that is already unread
      // regardless of mute, and to know whether marking it unread should
      // push a dot at all.
      this.v2DMInfoByPeerId = Object.fromEntries(
        (data?.dms || [])
          .filter((dm) => !!dm.lastMessageId)
          .map((dm) => [
            dm.peerId,
            { key: dm.conversationKey, muted: dm.muted === true, hasUnread: dm.hasUnread === true },
          ])
      );
    } catch {
      // Non-critical — unread dots just won't show
    }
  }

  private async loadV2Members(projectId: string): Promise<void> {
    if (!projectId) return;
    const seq = ++this._membersViewSeq;
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
            canAttach?: boolean;
          }>;
          members?: SpaceMember[];
        };
        // The view may have moved on while this was in flight — to another
        // project, or to the hub — so a later loadV2Members/loadHubMembers
        // call has already claimed the sidebar. Check after the last await,
        // immediately before the first write, so applying this response
        // can't overwrite that current view with stale data.
        if (seq !== this._membersViewSeq) return;
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
          // Carry the Hub attach decision through. These mappers rebuild the
          // member objects field by field, so anything not named here is
          // dropped before the sidebar sees it.
          canAttach: a.canAttach,
        }));
        // Reconcile stateManager: remove agents the server no longer returns
        // for this project. This handles agents deleted while the client was
        // disconnected (e.g., Safari backgrounded, missed SSE `deleted`
        // event) — stateManager.seedAgents is add-only, so a stale entry
        // would otherwise linger and get re-added by every SSE tick,
        // triggering this same refetch in a loop (see _handleAgentsUpdated).
        const serverAgentIds = new Set((data.agents || []).map((a) => a.id));
        // If an agent the server now returns was previously marked as omitted,
        // lift the suppression — it is active and should not be blocked.
        for (const id of serverAgentIds) {
          this._serverOmittedAgentIds.delete(id);
        }
        const staleIds: string[] = [];
        for (const agent of stateManager.getAgents()) {
          if (agent.projectId === projectId && !serverAgentIds.has(agent.id)) {
            staleIds.push(agent.id);
          }
        }
        for (const id of staleIds) {
          stateManager.removeAgent(id);
          this._serverOmittedAgentIds.add(id);
        }
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

  /**
   * A member's DM was marked unread from the members sidebar's context menu.
   * The sidebar already confirmed the request succeeded — this reflects it
   * in the unread-dot state the page owns (respecting mute, same as
   * loadUnreadDMPeers) and in `v2DMInfoByPeerId`, so the sidebar's own
   * "Mark unread" item hides right away instead of staying offered until the
   * next refresh. If that DM happens to be the conversation currently open,
   * this also suppresses its auto-advance immediately rather than waiting on
   * the SSE round trip.
   */
  private handleMemberMarkedUnread(e: CustomEvent): void {
    const detail = e.detail as { peerId?: string; conversationKey?: string } | undefined;
    const peerId = detail?.peerId || '';
    this.applyDMMarkedUnread(peerId);
    if (detail?.conversationKey) {
      this.suppressOpenThreadAutoAdvance(detail.conversationKey);
    }
  }

  // ---- Mobile swipe navigation ----

  private handleTouchStart(e: TouchEvent): void {
    if (e.touches.length > 1) {
      this.abandonTouchForPinch();
      return;
    }
    const touch = e.touches[0];
    if (!touch) return;
    this._touchMulti = false;
    this._touchStartX = touch.clientX;
    this._touchStartY = touch.clientY;
    this._touchStartTime = Date.now();
    this._isSwiping = false;
    // Measured before the drag moves anything: a code block or wide table
    // under the finger that can still scroll in the drag direction keeps the
    // gesture, and only once it is at that end does the panel swipe apply.
    // Without the mobile panels there is no swipe, so nothing to measure.
    this._touchScrollRoom = this.isMobileViewport()
      ? horizontalScrollRoom(e.composedPath())
      : { rightward: false, leftward: false };
  }

  private handleTouchMove(e: TouchEvent): void {
    if (!this._touchStartTime || this._touchMulti) return;
    if (e.touches.length > 1) {
      this.abandonTouchForPinch();
      return;
    }
    const touch = e.touches[0];
    if (!touch) return;

    const dx = touch.clientX - this._touchStartX;
    const dy = touch.clientY - this._touchStartY;

    // The panels' touch-action keeps sideways drags from the browser, but a
    // sideways scroller (code block, table) starts its own touch-action
    // chain. Dragged past its end, the browser hands the unused pan to its
    // history swipe. When the scroller under the touch has no room in this
    // direction, cancel the move before the browser claims the pan; the
    // panel swipe still reads these events.
    const room = this._touchScrollRoom;
    if (
      this.isMobileViewport() &&
      (room.rightward || room.leftward) &&
      e.cancelable &&
      Math.abs(dx) > Math.abs(dy) &&
      !scrollerTakesDrag(room, dx)
    ) {
      e.preventDefault();
    }

    // Horizontal only — a vertical drag is the message list scrolling.
    if (Math.abs(dx) > Math.abs(dy) && Math.abs(dx) > SWIPE_AXIS_LOCK_PX) {
      this._isSwiping = true;
    }
  }

  private handleTouchEnd(e: TouchEvent): void {
    if (this._touchMulti) {
      // A pinch ends only when its last finger lifts; until then a finger
      // left on the screen must not turn into a swipe.
      if (e.touches.length === 0) this._touchMulti = false;
      this._touchStartTime = 0;
      this._isSwiping = false;
      return;
    }
    const wasSwiping = this._isSwiping;
    const startX = this._touchStartX;
    const elapsed = Date.now() - this._touchStartTime;
    this._touchStartTime = 0;
    this._isSwiping = false;

    if (!wasSwiping || !this.isMobileViewport()) return;

    const touch = e.changedTouches[0];
    if (!touch) return;

    const dx = touch.clientX - startX;
    if (scrollerTakesDrag(this._touchScrollRoom, dx)) return;
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
   * A second finger turned the gesture into a pinch. Leave it to the browser
   * for the rest of the gesture: no swipe, and no cancelled moves (cancelling
   * a touchmove would cancel the pinch zoom).
   */
  private abandonTouchForPinch(): void {
    this._touchMulti = true;
    this._touchStartTime = 0;
    this._isSwiping = false;
    this._touchScrollRoom = { rightward: false, leftward: false };
  }

  /**
   * Drop focus so iOS retracts the software keyboard. The composer input lives
   * several shadow roots down, and `document.activeElement` only reports the
   * outermost host, so descend to the real focused element before blurring.
   */
  private dismissKeyboard(): void {
    const el = deepActiveElement();
    if (el instanceof HTMLElement || el instanceof SVGElement) el.blur();
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
    return this.isMobileLayout;
  }

  private _handleMobileLayoutChange(e: MediaQueryListEvent): void {
    this.isMobileLayout = e.matches;
  }

  /**
   * Fallback for browsers that don't support `overflow: clip` on
   * `.v2-panels`: it is still a scroll container there, so a native focus
   * reveal or `scrollIntoView` can set its scroll offset. Resetting it on
   * every scroll event costs nothing where `clip` is supported, because
   * those engines never let the container become scrollable in the first
   * place.
   */
  private _handleV2PanelsScroll(e: Event): void {
    const el = e.currentTarget as HTMLElement;
    if (el.scrollLeft !== 0 || el.scrollTop !== 0) {
      el.scrollLeft = 0;
      el.scrollTop = 0;
    }
  }

  /** Open the DM conversation with a member in the centre panel. */
  private openDM(memberId: string, memberKind: 'user' | 'agent', displayName: string): void {
    ++this._userNavSeq;
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
      this.mobilePanel = 'center';

      this.pushDMPath(dmKey);

      dispatchPageTitle(this, displayName, 'Chat');
      return;
    }

    // User ID not available — resolve via API
    void this.resolveDMByPeerId(memberId, memberKind, displayName);
  }

  /** Push the DM's full-key URL, which parseV2Route matches directly. */
  private pushDMPath(dmKey: string): void {
    const dmPath = `/chat/dm/${encodeURIComponent(dmKey)}`;
    const base = import.meta.env.BASE_URL;
    const browserPath = base && base !== '/' ? base.replace(/\/$/, '') + dmPath : dmPath;
    window.history.pushState({}, '', browserPath);
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
  // Quick command palette (Cmd/Ctrl-K)  — #1048
  // =========================================================================

  /**
   * Global keydown handler: open the grouped quick command palette on
   * Cmd/Ctrl+K. This is the single shortcut owner — every guard below runs
   * before it does.
   */
  private _handleGlobalKeydown(e: KeyboardEvent): void {
    if (e.defaultPrevented || e.repeat || e.isComposing) return;
    // Escape while a reopen is queued behind a still-animating close — the
    // palette's own, or the document preview's — cancels the queue. The
    // dialog is already hiding, so this press never reaches its
    // sl-request-close and nothing else would clear the queue; without this
    // the palette would reopen even though the last keystroke asked to close
    // it. togglePalette gives Cmd/Ctrl+K the same even-presses-end-closed
    // behaviour by toggling the queue.
    if (
      e.key === 'Escape' &&
      (this._paletteCloseAnimating || this._paletteFilePreviewTarget) &&
      this._palettePendingReopen
    ) {
      this._palettePendingReopen = false;
      return;
    }
    if (e.altKey || e.shiftKey) return;
    // Exactly one of Ctrl/Meta — not both (e.g. some IMEs), not neither.
    if (e.metaKey === e.ctrlKey) return;
    if (e.key.toLowerCase() !== 'k') return;

    // Ctrl+K must keep reaching the terminal's PTY untouched, and a chat page
    // hidden behind the terminal workspace must perform zero palette state
    // changes or fetches even though it stays mounted.
    if (this._eventFromTerminalSurface(e)) return;
    if (!this._paletteOpenGuardsHold()) return;

    e.preventDefault();
    void this.togglePalette();
  }

  /**
   * The header palette button's open-request handler. Shares the same route/
   * visibility/modal guard as the shortcut, but skips
   * `_eventFromTerminalSurface`: that guard exists to let Ctrl+K keep
   * reaching the terminal's PTY, which only makes sense for a keystroke — a
   * click on the header button is never a keypress the terminal could want.
   * Opens with `{ mode: 'open' }` rather than toggling: see `togglePalette`'s
   * doc comment for why the button must never close an already-open palette.
   */
  private _handlePaletteOpenRequest(): void {
    if (!this._paletteOpenGuardsHold()) return;
    void this.togglePalette({ mode: 'open' });
  }

  /**
   * The conditions that must hold for the palette to actually be allowed
   * open: on the chat route, the page actually visible, and no unrelated
   * modal already up. Shared between a fresh Cmd/Ctrl+K
   * (`_handleGlobalKeydown`, above) and the moment a reopen queued behind a
   * still-animating close (`_palettePendingReopen`) is about to actually run
   * (`_handlePaletteAfterHide`) — real time passes between the press that
   * queues a reopen and the later moment it runs, so whatever made that
   * press valid is not guaranteed to still hold once the deferred close
   * finally settles.
   */
  private _paletteOpenGuardsHold(): boolean {
    return this._isOnChatRoute() && this._isPageVisible() && !this._isUnrelatedModalActive();
  }

  /** True when the event's real (composedPath) origin is inside a terminal pane / xterm surface. */
  private _eventFromTerminalSurface(e: KeyboardEvent): boolean {
    return e.composedPath().some((node) => {
      if (!(node instanceof Element)) return false;
      if (node.tagName === 'SCION-TERMINAL-PANE') return true;
      return node.classList?.contains('xterm') ?? false;
    });
  }

  /** Is the current URL (relative to BASE_URL) `/chat` or a route below it? */
  private _isOnChatRoute(): boolean {
    const base = (import.meta.env.BASE_URL || '/').replace(/\/$/, '');
    let path = window.location.pathname;
    if (base && path.startsWith(base)) {
      path = path.slice(base.length) || '/';
    }
    return path === '/chat' || path.startsWith('/chat/');
  }

  /**
   * Is this page actually visible? The chat page stays mounted (and its
   * document keydown listener stays live) while the terminal workspace hides
   * the route outlet via an ancestor `hidden` attribute, not on this element
   * itself — so this walks up through light DOM and shadow-root hosts,
   * preferring the standard `checkVisibility()` where available.
   */
  private _isPageVisible(): boolean {
    if (document.hidden) return false;
    const checkVisibility = (
      this as unknown as { checkVisibility?: (opts?: Record<string, boolean>) => boolean }
    ).checkVisibility;
    if (typeof checkVisibility === 'function') {
      try {
        return checkVisibility.call(this, { checkOpacity: false, checkVisibilityCSS: true });
      } catch {
        // Fall through to the manual walk below (e.g. not implemented in this environment).
      }
    }
    let node: Node | null = this as unknown as Node;
    while (node) {
      const el = node as HTMLElement;
      if (el.hidden) return false;
      if (typeof getComputedStyle === 'function' && node instanceof Element) {
        try {
          const style = getComputedStyle(el);
          if (style.display === 'none' || style.visibility === 'hidden') return false;
        } catch {
          // Ignore — environments without a real layout engine (e.g. some test DOMs).
        }
      }
      const parent: Element | null = (node as Element).parentElement;
      if (parent) {
        node = parent;
        continue;
      }
      const root = node.getRootNode();
      node = root instanceof ShadowRoot ? root.host : null;
    }
    return true;
  }

  /**
   * Is some dialog/drawer other than our own switcher/palette currently
   * open? Queried live at keydown time rather than tracked from `sl-show`/
   * `sl-after-hide` events, for two reasons:
   *
   * 1. Shoelace only fires `sl-show` on an `open` false->true *transition*.
   *    Every real dialog in this app that renders as `<sl-dialog open>`
   *    behind a `when(...)`/ternary (the attachment preview, the interagent
   *    marker overlay, the space rail's emoji picker) is "born open" and
   *    never fires it at all — an event-tracked set would never see them.
   * 2. An event-tracked set only removes an entry on `sl-after-hide`. A
   *    dialog that is *disconnected* while still open (conditional
   *    re-render, conversation switch, back navigation) never fires that
   *    event, so the entry — and the guard it feeds — would leak forever,
   *    permanently killing Ctrl+K for every v2 user.
   *
   * A live query has neither failure mode: it only ever reports what is
   * actually open and connected right now.
   */
  private _isUnrelatedModalActive(): boolean {
    return hasOpenModalDescendant(document, this._switcherEl ?? null);
  }

  /**
   * If another modal opens while the palette is open, close it without
   * restoring focus into hidden chat. `composedPath()[0]` is the
   * real element that emitted `sl-show`, even from inside a shadow root.
   * Only actual modal surfaces close the palette — `sl-show`
   * also fires (bubbling + composed) from non-modal Shoelace elements like
   * `sl-alert` toasts, `sl-tooltip`, `sl-dropdown`, `sl-details` and
   * `sl-select`, none of which should steal focus from an open palette.
   */
  private _handleDocumentModalShow(e: Event): void {
    if (!this.v2PaletteOpen) return;
    const path = e.composedPath();
    if (this._switcherEl && path.includes(this._switcherEl)) return; // our own dialog opening
    const origin = path[0];
    if (!(origin instanceof Element) || !isOpenModalElement(origin)) return;
    this._closePaletteWithoutFocusRestore();
  }

  /**
   * If a route change hides chat while the palette is open, close it
   * without restoring focus into hidden chat. `navigateTo`/`pushState`
   * callers don't fire `popstate` themselves, but the browser does for
   * back/forward, and main.ts's terminal-workspace transition uses
   * `pushState` directly — this covers the case a route change leaves the
   * page non-current while the palette is still open.
   */
  private _handlePopStateForPalette(): void {
    if (!this.v2PaletteOpen) return;
    if (!this._isOnChatRoute() || !this._isPageVisible()) {
      this._closePaletteWithoutFocusRestore();
    }
  }

  private _closePaletteWithoutFocusRestore(): void {
    this._paletteSkipFocusRestore = true;
    this._closePaletteAndCancelLoad();
  }

  // =========================================================================
  // Grouped palette (native chat quick command palette)
  // =========================================================================

  /**
   * Open (or, on a repeat press, close) the grouped palette. Closing here —
   * the "toggle" dismiss path — does not unmount the switcher element; it
   * only flips `open`, so Shoelace's own close animation runs and
   * `sl-after-hide` fires exactly as it does for escape/backdrop, which is
   * what restores deep focus and cursor/selection on every dismissal path.
   *
   * A press that arrives while a previous close is still animating out is
   * queued (`_paletteCloseAnimating`) rather than opened immediately: see
   * `_handlePaletteAfterHide`, which runs the deferred open once that close
   * actually finishes.
   *
   * `mode` defaults to `'toggle'` (the shortcut's behaviour, unchanged byte
   * for byte). `'open'` is the header button's contract: it never closes an
   * already-open palette and never cancels an in-flight open, so a double
   * tap or double click — common on touch, and not reliably distinguishable
   * from two deliberate presses — can only ever leave the palette open,
   * never toggle it shut. It differs from `'toggle'` at exactly three
   * points, every one of them a no-op or an unconditional set in place of a
   * toggle; everything else (`_openPalette`, the epoch, the watchdog, the
   * lazy load, the after-hide queue and the focus handoff) runs through the
   * same code for both modes.
   */
  private async togglePalette(options: { mode?: 'toggle' | 'open' } = {}): Promise<void> {
    const mode = options.mode ?? 'toggle';
    if (this.v2PaletteOpen) {
      // 'open': the palette is already open and visible — nothing to do.
      if (mode === 'open') return;
      this._closePaletteAndCancelLoad();
      return;
    }
    if (this._palettePendingOpen) {
      // 'open': a first-open lazy import is already in flight from an
      // earlier press — let it finish rather than cancelling it (a 'toggle'
      // here would cancel it instead, see below). This is exactly the race
      // a double tap on the button hits during the very first open.
      if (mode === 'open') return;
      // A second Ctrl+K arrived while the first press's lazy import was
      // still in flight — cancel the pending open rather
      // than opening a second time (or doing nothing, which would silently
      // eat the press). The suspended first call below observes this flag
      // once its await resolves and backs out.
      this._palettePendingOpen = false;
      return;
    }
    if (this._paletteFilePreviewTarget) {
      // The document preview is open, or (Escape already flipped its own
      // `sl-dialog`'s `open` to `false`, so `_isUnrelatedModalActive` no
      // longer sees it) still running its own ~250ms hide animation before
      // `chat-file-preview-close` actually arrives. Opening the palette now
      // would re-capture whatever is focused mid-hide inside the dying
      // preview, which the preview's own delayed close then discards when it
      // restores focus — leaving the just-opened palette with no focus at
      // all. Queue it instead, the same way a still-animating palette close
      // is queued; `_closePaletteFilePreview` serves it once that close
      // actually arrives. Toggled, not set, for the same even-presses-end-
      // closed reason as the palette's own queue below — except for 'open',
      // which sets the queue unconditionally: a button press can only ever
      // ask for open, never cancel a previously queued one.
      this._palettePendingReopen = mode === 'open' ? true : !this._palettePendingReopen;
      return;
    }
    if (this._paletteCloseAnimating) {
      // A prior close (selection, escape, backdrop, toggle...) hasn't
      // actually finished yet. Flipping `v2PaletteOpen` back to `true` here
      // would set `open` on a dialog Shoelace is still mid-hide-animation
      // on — the panel would stay visibly hidden regardless, and that
      // close's own stale `sl-after-hide` would still fire afterward and run
      // its focus disposition (e.g. focusing the composer) out from under
      // the "reopened" palette. Queue it instead; the open runs once that
      // `sl-after-hide` actually arrives. Toggled, not just set to `true`:
      // the shortcut is a toggle, so a second press during the same pending
      // close cancels the first press's queued reopen rather than leaving it
      // queued — an even number of presses while closing must still end
      // closed, the same as it would with no close in flight at all. 'open'
      // sets it unconditionally instead, for the same reason as above.
      this._palettePendingReopen = mode === 'open' ? true : !this._palettePendingReopen;
      return;
    }
    await this._openPalette();
  }

  /**
   * The actual open sequence, shared by a fresh `togglePalette` press and a
   * reopen deferred past a pending close. `skipInvokerCapture` is set only
   * by the deferred-reopen path: that call already decided what a later
   * close should restore (see `_handlePaletteAfterHide`'s pending-reopen
   * branch), and capturing again here would overwrite it with whatever is
   * focused mid-animation instead.
   */
  private async _openPalette(options: { skipInvokerCapture?: boolean } = {}): Promise<void> {
    this._palettePendingOpen = true;
    this._paletteOpenEpoch++;
    try {
      if (!options.skipInvokerCapture) this._capturePaletteInvokerFocus();
      if (!this.v2SwitcherLoaded) {
        await loadQuickPalette();
        this.v2SwitcherLoaded = true;
        // Let <scion-quick-palette> mount and render with open=false first.
        // Shoelace's dialog reacts to `open` transitioning false -> true to
        // run its show animation and fire sl-initial-focus/sl-show; created
        // already-open, it skips that lifecycle entirely — a real Shoelace
        // quirk, confirmed against a real sl-dialog.
        await this.updateComplete;
      }
      if (!this._palettePendingOpen) {
        // Cancelled by a second press while we were awaiting above.
        return;
      }
      this.v2PaletteOpen = true;
      this._startPaletteVisibilityWatchdog();
      this._loadPaletteGroupsOnOpen();
    } finally {
      this._palettePendingOpen = false;
    }
  }

  /**
   * Keep the Documents group in sync with `chatRecentFiles`. Unlike the other
   * three groups, this is not a "load" in the fetch/cache/retry sense —
   * `chatRecentFiles` is an already-live, identity-scoped index, so this
   * subscription (registered once in `connectedCallback`, called once
   * immediately for the initial snapshot and again on every subsequent
   * change) is the group's only population path; `_loadPaletteGroupsOnOpen`
   * below never touches it.
   */
  private _handleRecentFilesSnapshot(snapshot: RecentFilesSnapshot): void {
    this.v2PaletteGroups = {
      ...this.v2PaletteGroups,
      documents: { status: 'ready', candidates: buildDocumentCandidates(snapshot.records) },
    };
  }

  /**
   * Keep the open palette's Agents group current with the agent store:
   * every ready hub snapshot (an SSE change, a revalidation) re-derives the
   * candidates against the DM recency of the last load, with no request.
   * Progress snapshots are left to the load that asked for them, and
   * nothing is published while the palette is closed (the next open
   * rebuilds the group), while an Agents load is in flight, or before one
   * succeeded.
   */
  private _handlePaletteAgentSnapshot(snapshot: AgentListSnapshot): void {
    if (!this.v2PaletteOpen) return;
    if (snapshot.status !== 'ready') return;
    if (this._paletteGroupLoadToken.agents !== undefined) return;
    const candidates = this._paletteDataController.deriveAgentCandidates(snapshot);
    if (!candidates) return;
    this.v2PaletteGroups = { ...this.v2PaletteGroups, agents: { status: 'ready', candidates } };
  }

  /**
   * On open, reuse a People or Threads snapshot when it is still inside the
   * 30s cache window and nothing has invalidated it since; a group that has
   * never loaded, is stale, or was marked dirty by an SSE event gets a fresh
   * fetch instead. Agents always loads: its rows come from the agent store
   * (from memory once loaded) and its DM recency is fetched only when the
   * last DM list is stale (see `_loadPaletteAgents`). Documents
   * is not fetched here — see `_handleRecentFilesSnapshot`.
   */
  private _loadPaletteGroupsOnOpen(): void {
    void this._loadPaletteAgents();
    if (!this._shouldUseCachedPaletteGroup('people')) void this._loadPalettePeople();
    if (!this._shouldUseCachedPaletteGroup('threads')) void this._loadPaletteThreads();
  }

  private _shouldUseCachedPaletteGroup(group: PaletteGroup): boolean {
    const state = this.v2PaletteGroups[group];
    if (!state || state.status !== 'ready') return false;
    if (this._paletteGroupDirty[group]) return false;
    const cachedAt = this._paletteGroupCacheAt[group];
    if (!cachedAt) return false;
    return Date.now() - cachedAt < PALETTE_GROUP_CACHE_MS;
  }

  /**
   * Mark one or more palette groups stale: their cached snapshot (if any) is
   * no longer trusted for a future open, and — if the palette is currently
   * open — a 500ms debounced refresh reloads them shortly. Safe to call
   * whether or not the palette is loaded/open.
   */
  private _markPaletteGroupsDirty(...groups: PaletteGroup[]): void {
    for (const group of groups) {
      this._paletteGroupDirty[group] = true;
      this._paletteGroupInvalidationEpoch[group] =
        (this._paletteGroupInvalidationEpoch[group] ?? 0) + 1;
    }
    if (this.v2PaletteOpen) this._schedulePaletteDebouncedRefresh();
  }

  /**
   * Mark the Agents group's DM list stale. While the palette is open, the
   * debounced refresh re-reads it, so recency follows new messages; when
   * closed, the next open does.
   */
  private _markAgentDmsStale(): void {
    this._paletteDataController.markAgentDmsStale();
    if (!this.v2PaletteOpen) return;
    this._agentDmsRefreshPending = true;
    this._schedulePaletteDebouncedRefresh();
  }

  /** Snapshot the invalidation epoch for `group` at load start — pass the result to {@link _finishPaletteGroupLoad}. */
  private _beginPaletteGroupLoad(group: PaletteGroup): number {
    return this._paletteGroupInvalidationEpoch[group] ?? 0;
  }

  /**
   * Mark a successful load's group fresh — but only if nothing invalidated
   * it since {@link _beginPaletteGroupLoad} captured `epochAtStart`. If an
   * invalidation landed mid-load, leave `dirty=true` and the cache timestamp
   * untouched: the debounced refresh that invalidation already scheduled (or
   * the next open, since a dirty group is never cache-eligible) will pick up
   * the real, current data instead of the possibly-stale snapshot this load
   * just fetched.
   */
  private _finishPaletteGroupLoad(group: PaletteGroup, epochAtStart: number): void {
    if ((this._paletteGroupInvalidationEpoch[group] ?? 0) !== epochAtStart) return;
    this._paletteGroupDirty[group] = false;
    this._paletteGroupCacheAt[group] = Date.now();
  }

  private _schedulePaletteDebouncedRefresh(): void {
    if (this._paletteRefreshDebounce) clearTimeout(this._paletteRefreshDebounce);
    this._paletteRefreshDebounce = setTimeout(() => {
      this._paletteRefreshDebounce = null;
      this._refreshDirtyPaletteGroups();
    }, PALETTE_REFRESH_DEBOUNCE_MS);
  }

  private _stopPaletteDebouncedRefresh(): void {
    if (this._paletteRefreshDebounce) {
      clearTimeout(this._paletteRefreshDebounce);
      this._paletteRefreshDebounce = null;
    }
  }

  /**
   * Reload every group an SSE event invalidated while the palette is open. A
   * group not currently dirty is left untouched.
   *
   * A dirty group whose own loader is still mid-flight (see
   * `_paletteGroupLoadToken`'s doc comment) is *not* reloaded here — doing so
   * would call straight into the data controller's own supersede logic and
   * abort that still-running fetch, and on a hub where invalidations arrive
   * more often than one fetch takes to finish, every restart would cancel
   * the last one's progress before it could ever land, so the group would
   * never resolve. Instead this reschedules another debounced check: once
   * the in-flight load finally settles, a later tick here finds it no longer
   * in flight (still dirty, since that load's own epoch check leaves `dirty`
   * set) and reloads it then — one coalesced follow-up fetch per burst of
   * invalidations, just deferred until the fetch already running is done.
   */
  private _refreshDirtyPaletteGroups(): void {
    if (!this.v2PaletteOpen) return;
    let deferred = false;
    // The groups refreshed on invalidation; Agents follows the agent store
    // and `documents` is never dirtied.
    for (const group of ['people', 'threads'] as const) {
      if (!this._paletteGroupDirty[group]) continue;
      if (this._paletteGroupLoadToken[group] !== undefined) {
        deferred = true;
        continue;
      }
      if (group === 'people') void this._loadPalettePeople();
      else void this._loadPaletteThreads();
    }
    // Agents rows follow the agent store; only their DM recency is re-read.
    if (this._agentDmsRefreshPending) {
      if (this._paletteGroupLoadToken.agents !== undefined) deferred = true;
      else void this._loadPaletteAgents({ keepReady: true });
    }
    if (deferred) this._schedulePaletteDebouncedRefresh();
  }

  /**
   * Bounded polling for the "route hides chat" case while the palette
   * is open. `popstate` alone is not enough: the terminal-workspace
   * transition (and other in-page navigations) change the route via
   * `history.pushState` directly, which — by design of the History API —
   * does not fire `popstate` itself; only actual back/forward navigation
   * does. A short interval catches that case (and any other way the page
   * could stop being the current route/visible) without depending on how
   * the transition happens, at the cost of a small close delay instead of
   * an instant one. The popstate listener still gives instant closing for
   * real back/forward.
   */
  private _startPaletteVisibilityWatchdog(): void {
    this._stopPaletteVisibilityWatchdog();
    this._paletteVisibilityWatchdog = setInterval(() => {
      if (!this.v2PaletteOpen) {
        this._stopPaletteVisibilityWatchdog();
        return;
      }
      if (!this._isOnChatRoute() || !this._isPageVisible()) {
        this._closePaletteWithoutFocusRestore();
      }
    }, 250);
  }

  private _stopPaletteVisibilityWatchdog(): void {
    if (this._paletteVisibilityWatchdog) {
      clearInterval(this._paletteVisibilityWatchdog);
      this._paletteVisibilityWatchdog = null;
    }
  }

  /**
   * Close the palette and cancel any in-flight group load — closing the
   * palette cancels in-flight first-open work but retains completed groups.
   * `cancel()` on an already-settled controller is a harmless no-op, so this
   * is safe to call from every close path uniformly. `v2PaletteOpen` flips
   * synchronously here, but the dialog itself is still playing its hide
   * animation — `_paletteCloseAnimating` marks that window until this
   * close's own `sl-after-hide` arrives (see `togglePalette`).
   */
  private _closePaletteAndCancelLoad(): void {
    this._paletteDataController.cancel();
    // Supersede a People load still resolving identity when the palette
    // closes — the data controller's own cancel() above doesn't reach
    // `_resolveSelfUserId`'s `/auth/me` fetch.
    this._peopleLoadSeq++;
    // Stop that fetch immediately rather than leaving it running for up to
    // AGENTS_IDLE_TIMEOUT_MS — `_peopleLoadSeq` above already guarantees its
    // (eventual) result can't publish, so this is a resource-usage fix, not
    // a correctness one.
    this._selfUserAbortController?.abort();
    this._selfUserAbortController = null;
    this._stopPaletteVisibilityWatchdog();
    this._stopPaletteDebouncedRefresh();
    this.v2PaletteOpen = false;
    this._paletteCloseAnimating = true;
  }

  /**
   * Load the Agents group: the agent store's hub list joined with the DM
   * list for recency. Between loads the group follows the store through
   * {@link _handlePaletteAgentSnapshot}.
   *
   * Until a complete snapshot has been published (see
   * {@link _agentsSnapshotComplete}), publishes candidates progressively as
   * each agents page arrives (see
   * {@link ChatPaletteDataController.loadAgentsGroup}'s `onProgress`) —
   * recency not yet known for any of them, since the DM list a page's
   * recency depends on is only fetched once the whole agents list is in —
   * rather than holding every row back until the last page lands: a hub
   * with a long agent list can take a while to finish paginating, and the
   * group should show *something* well before that.
   *
   * A refresh of a group that already holds a complete snapshot (see
   * {@link _agentsSnapshotComplete}) does **not** wire `onProgress`: a
   * progress tick would replace that complete, DM-ranked list with page one
   * alone — recency reset to unknown, a manually-selected row past page one
   * gone — for the whole reload. The previous snapshot is left on screen
   * untouched (see the `loading` state set below, which keeps
   * `previous?.candidates`) until the refreshed result actually displaces
   * it.
   *
   * With `keepReady`, a ready group stays ready, showing its rows, while the
   * load re-reads recency.
   *
   * `token` identifies this call for {@link _paletteGroupLoadToken}'s
   * `finally` check only: a load superseded by a newer one for this group
   * cannot clear bookkeeping that belongs to that newer load. Nothing else
   * here needs to check it — the data controller's own generation check
   * already filters a superseded call's `onProgress` ticks and turns its
   * eventual resolution into an `AbortError`, which the catch branch below
   * handles directly.
   */
  private async _loadPaletteAgents(options: { keepReady?: boolean } = {}): Promise<void> {
    const token = {};
    this._paletteGroupLoadToken.agents = token;
    this._agentDmsRefreshPending = false;
    // With a ready store snapshot and a fresh DM list the group is shown at
    // once; the load below then only confirms (or revalidates) it.
    const instant = this._paletteDataController.peekAgentsGroup();
    const previous = this.v2PaletteGroups.agents;
    if (instant) {
      this.v2PaletteGroups = {
        ...this.v2PaletteGroups,
        agents: { status: 'ready', candidates: instant },
      };
    } else if (!options.keepReady || previous?.status !== 'ready') {
      this.v2PaletteGroups = {
        ...this.v2PaletteGroups,
        agents: { status: 'loading', candidates: previous?.candidates ?? [] },
      };
    }
    try {
      const candidates = await this._paletteDataController.loadAgentsGroup(
        this._agentsSnapshotComplete
          ? undefined
          : (partial) => {
              this.v2PaletteGroups = {
                ...this.v2PaletteGroups,
                agents: { status: 'loading', candidates: partial },
              };
            }
      );
      this.v2PaletteGroups = { ...this.v2PaletteGroups, agents: { status: 'ready', candidates } };
      this._agentsSnapshotComplete = true;
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') return;
      const message = err instanceof PaletteLoadError || err instanceof Error ? err.message : '';
      // Keep whatever candidates are already on screen (partial progress, or
      // an untouched previous-ready snapshot) rather than wiping them: an
      // idle timeout on a hub whose list is simply bigger than expected
      // should read as "partial list, Retry for the rest", not an empty
      // error.
      this.v2PaletteGroups = {
        ...this.v2PaletteGroups,
        agents: {
          status: 'error',
          candidates: this.v2PaletteGroups.agents?.candidates ?? [],
          ...(message ? { error: message } : {}),
        },
      };
    } finally {
      if (this._paletteGroupLoadToken.agents === token) {
        delete this._paletteGroupLoadToken.agents;
      }
    }
  }

  /**
   * Resolve the authenticated self ID for the palette's People group,
   * fetching `/api/v1/auth/me` when `pageData.user.id` isn't known yet (the
   * same fallback `resolveDMByPeerId` already uses for token-based auth) and
   * caching the result onto `pageData.user`. Returns `''` if identity truly
   * cannot be resolved right now — the caller must not publish or cache a
   * `ready` People group in that case: a `''` self ID excludes nothing, so a
   * real self row — and a nonsensical self-DM target — could otherwise leak
   * into the list and get cached for 30s with no way to self-correct once
   * identity does resolve.
   *
   * Does not mark People dirty itself: its only caller,
   * {@link _loadPalettePeople}, uses the resolved ID immediately within the
   * very same load, so a self-triggered dirty mark would only bump the
   * group's own invalidation epoch out from under its own
   * {@link _finishPaletteGroupLoad} check and schedule a pointless second
   * reload 500ms later. A genuinely external resolver (one that is not
   * itself in the middle of a People load) is responsible for marking
   * People dirty if it wants a still-open palette to retry.
   *
   * Bounded by {@link AGENTS_IDLE_TIMEOUT_MS} (shared with the Agents
   * group's own idle bound): a hung `/api/v1/auth/me` would otherwise never
   * let this function return at all, which would in turn hold
   * {@link _loadPalettePeople}'s load token forever — its `finally` can't run
   * until this `await` settles one way or another. A timeout here settles it
   * with the same `''`-means-unresolved outcome as any other failure.
   *
   * Also aborted directly — not only by the idle timeout — on close
   * ({@link _closePaletteAndCancelLoad}) and on a newer People load, which
   * aborts the previous call's still-pending fetch first via
   * {@link _selfUserAbortController}, same abort-the-predecessor pattern as
   * {@link ChatPaletteDataController}'s own per-group loaders, so the old
   * request doesn't keep running for up to {@link AGENTS_IDLE_TIMEOUT_MS}
   * after nothing can use its result. That abort is a resource-usage measure
   * only, not the correctness guard — a stale resolution (abort-triggered or
   * not) can still only return `''`, and {@link _peopleLoadSeq} is what
   * actually keeps a stale result from publishing, with or without it.
   *
   * The predecessor abort runs even when this call resolves synchronously
   * from `known` below, since a previous call's own fetch may still be
   * in flight regardless of whether this one needs to make one of its own.
   */
  private async _resolveSelfUserId(): Promise<string> {
    this._selfUserAbortController?.abort();
    const known = this.pageData?.user?.id;
    if (known) return known;
    const timeoutController = new AbortController();
    this._selfUserAbortController = timeoutController;
    const timeoutId = setTimeout(() => timeoutController.abort(), AGENTS_IDLE_TIMEOUT_MS);
    try {
      const authRes = await apiFetch('/api/v1/auth/me', { signal: timeoutController.signal });
      if (authRes.ok) {
        const authData = (await authRes.json()) as { id?: string };
        if (authData.id && this.pageData) {
          // A plain field write, not a `pageData` reassignment. `updated()`
          // re-parses the current route whenever `changedProperties`
          // contains `pageData` — that path exists so the page parses its
          // route when `main.ts` hands a freshly created element its
          // `pageData` (alongside the parse `connectedCallback` does); it is
          // not a response to an identity change. Resolving identity here,
          // in the background, while a conversation may already be open,
          // must not re-trigger that parse and revert it to whatever the
          // URL happens to read right now. `requestUpdate()` with no
          // property name still schedules the render every pageData-bound
          // binding (e.g. currentUserId) needs, without adding `pageData`
          // to that set.
          if (this.pageData.user) {
            this.pageData.user.id = authData.id;
          } else {
            this.pageData.user = { id: authData.id, email: '', name: '' };
          }
          this.requestUpdate();
          return authData.id;
        }
      }
    } catch {
      // Identity truly unavailable right now — a timeout abort, a
      // close/supersede abort, or a genuine fetch failure are all treated
      // alike: the caller gets '' and treats it as failure.
    } finally {
      clearTimeout(timeoutId);
      // Only clear the field if it's still this call's own controller — a
      // newer call's abort-the-predecessor step above may already have
      // replaced it with its own, and this (now-superseded) call's finally
      // must not null out a newer call's live controller out from under it.
      if (this._selfUserAbortController === timeoutController) {
        this._selfUserAbortController = null;
      }
    }
    return '';
  }

  /**
   * Load the People group. Requires the authenticated self ID (resolved via
   * {@link _resolveSelfUserId}) to exclude the current user from the list and
   * to build the deterministic sorted-user DM key on selection
   * (`openDM`/`buildDMKey`). If identity cannot be resolved, the group is
   * published as a retryable error — never as a self-inclusive `ready` list,
   * and never cached.
   */
  private async _loadPalettePeople(): Promise<void> {
    const token = {};
    this._paletteGroupLoadToken.people = token;
    this.v2PaletteGroups = {
      ...this.v2PaletteGroups,
      people: { status: 'loading', candidates: this.v2PaletteGroups.people?.candidates ?? [] },
    };
    const epochAtStart = this._beginPaletteGroupLoad('people');
    try {
      const mySeq = ++this._peopleLoadSeq;
      const selfId = await this._resolveSelfUserId();
      // The identity fetch above is aborted directly on close/supersede as
      // well as bounded by its own idle-timeout signal, but that abort is a
      // resource-usage measure, not what keeps a stale result from
      // publishing — this manual guard, keyed on this load's own sequence
      // token, is. Not `v2PaletteOpen`: closing and reopening sets
      // `v2PaletteOpen` back to `true`, which would let a stale load's
      // identity resolution overwrite a newer, already-`ready` People state.
      // A newer People load (from a reopen, a refresh, or another call) or a
      // close in the meantime bumps `_peopleLoadSeq`, superseding this one.
      // This return still runs the `finally` below, which is exactly why
      // that `finally` must check `token` itself rather than clearing
      // unconditionally — see `_paletteGroupLoadToken`'s doc comment.
      if (mySeq !== this._peopleLoadSeq) {
        return;
      }
      if (!selfId) {
        this.v2PaletteGroups = {
          ...this.v2PaletteGroups,
          people: {
            status: 'error',
            candidates: [],
            error: 'Could not resolve your identity yet.',
          },
        };
        return;
      }
      // Unlike Agents/Threads (see their own loaders' doc comments), this
      // check is reachable: a newer People load (B) can already be awaiting
      // its own `_resolveSelfUserId` — not covered by the data controller's
      // generation check — while this load (A) is still inside
      // `loadPeopleGroup`, which nothing has superseded from the data
      // controller's point of view. Without
      // this, A resolving after B has taken over would publish a stale
      // `ready` list over B's own in-progress (or already-`ready`) state.
      const candidates = await this._paletteDataController.loadPeopleGroup(selfId);
      if (this._paletteGroupLoadToken.people !== token) return;
      this.v2PaletteGroups = { ...this.v2PaletteGroups, people: { status: 'ready', candidates } };
      this._finishPaletteGroupLoad('people', epochAtStart);
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') return;
      // Reachable for the same reason as the success-path check above: a
      // newer People load (B) can already be awaiting its own identity while
      // this load (A) fails for a real (non-abort) reason inside
      // `loadPeopleGroup` — nothing the data controller tracks has
      // superseded A, since B has not called `loadPeopleGroup` yet, so A's
      // failure is not reclassified as an abort. Without this, A's failure
      // would publish a stale `error` (wiping candidates) over B's own
      // in-progress or already-`ready` state.
      if (this._paletteGroupLoadToken.people !== token) return;
      const message = err instanceof PaletteLoadError || err instanceof Error ? err.message : '';
      this.v2PaletteGroups = {
        ...this.v2PaletteGroups,
        people: { status: 'error', candidates: [], ...(message ? { error: message } : {}) },
      };
    } finally {
      if (this._paletteGroupLoadToken.people === token) {
        delete this._paletteGroupLoadToken.people;
      }
    }
  }

  /**
   * Load (or, with `retryOnly`, re-fetch only the previously-failed spaces
   * of) the Threads group. A partial failure keeps the group `'ready'` with
   * its successful rows selectable and `incomplete: true`, rather than
   * hiding them behind an `'error'` state.
   *
   * `token` identifies this call for {@link _paletteGroupLoadToken}'s
   * `finally` check only — see `_loadPaletteAgents`'s doc comment for why
   * nothing else here needs it.
   */
  private async _loadPaletteThreads(retryOnly = false): Promise<void> {
    const token = {};
    this._paletteGroupLoadToken.threads = token;
    const previous = this.v2PaletteGroups.threads;
    this.v2PaletteGroups = {
      ...this.v2PaletteGroups,
      threads: {
        status: 'loading',
        candidates: previous?.candidates ?? [],
        ...(previous?.incomplete ? { incomplete: true } : {}),
      },
    };
    const epochAtStart = this._beginPaletteGroupLoad('threads');
    try {
      const result = retryOnly
        ? await this._paletteDataController.retryThreadsGroup()
        : await this._paletteDataController.loadThreadsGroup();
      this.v2PaletteGroups = {
        ...this.v2PaletteGroups,
        threads: {
          status: 'ready',
          candidates: result.candidates,
          ...(result.incomplete ? { incomplete: true } : {}),
        },
      };
      this._finishPaletteGroupLoad('threads', epochAtStart);
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') return;
      const message = err instanceof PaletteLoadError || err instanceof Error ? err.message : '';
      this.v2PaletteGroups = {
        ...this.v2PaletteGroups,
        threads: { status: 'error', candidates: [], ...(message ? { error: message } : {}) },
      };
    } finally {
      if (this._paletteGroupLoadToken.threads === token) {
        delete this._paletteGroupLoadToken.threads;
      }
    }
  }

  /**
   * Retry a failed (or partially failed) palette group. Threads retries only
   * its own previously-failed spaces via
   * {@link ChatPaletteDataController.retryThreadsGroup}.
   */
  private _handlePaletteRetry(e: CustomEvent<{ group: PaletteGroup }>): void {
    const group = e.detail?.group;
    if (group === 'agents') void this._loadPaletteAgents();
    else if (group === 'people') void this._loadPalettePeople();
    else if (group === 'threads') void this._loadPaletteThreads(true);
  }

  /**
   * A palette selection is untrusted stale UI state until checked against
   * the freshly loaded group: verify the candidate is still present before
   * navigating. A `dm` target reuses the existing `openDM` — same typed
   * peerKind and deterministic DM key as a member-click or mention-click
   * selection. A `thread` target reuses the rail's own in-page navigation
   * path (`navigateToThread`), which already routes correctly when a
   * project has no known slug. A `document` target does not navigate at all:
   * it records a pending preview target that `_handlePaletteAfterHide` opens
   * once the palette's own close animation actually finishes, so the preview
   * dialog is never fighting the palette dialog for focus at the same time.
   */
  private _handlePaletteSelect(e: CustomEvent<{ target: PaletteTarget }>): void {
    const target = e.detail?.target;
    this._closePaletteAndCancelLoad();
    // Chat builds no `agent` candidates (its agent rows open a DM), so an
    // `agent` target can only be stale or foreign UI state.
    if (!target || target.kind === 'agent') return;
    if (target.kind === 'dm') {
      const group = target.peerKind === 'agent' ? 'agents' : 'people';
      const stillPresent = (this.v2PaletteGroups[group]?.candidates ?? []).some(
        (c) =>
          c.target.kind === 'dm' &&
          c.target.peerKind === target.peerKind &&
          c.target.peerId === target.peerId
      );
      // A rejected stale candidate falls through to the normal (invoker-
      // restoring) dismissal path below — only an actual navigation takes
      // the "focus the new composer" path. The flag is set only once a
      // navigation is confirmed, so sl-after-hide never points at a composer
      // that was never opened.
      if (!stillPresent) return;
      this._paletteClosedBySelection = true;
      this.openDM(target.peerId, target.peerKind, target.displayName);
      return;
    }
    if (target.kind === 'document') {
      const stillPresent = (this.v2PaletteGroups.documents?.candidates ?? []).some(
        (c) => c.target.kind === 'document' && c.target.file.key === target.file.key
      );
      // Same untrusted-stale-UI-state check as the dm/thread branches. A
      // rejected stale candidate falls through to the normal invoker-
      // restoring dismissal below — the pending-preview flag is set only
      // once a preview target is confirmed, so sl-after-hide never opens a
      // preview for a document the refresh already dropped.
      if (!stillPresent) return;
      this._pendingDocumentPreviewTarget = recentFileToPreviewTarget(target.file);
      return;
    }
    const stillPresent = (this.v2PaletteGroups.threads?.candidates ?? []).some(
      (c) =>
        c.target.kind === 'thread' &&
        c.target.projectId === target.projectId &&
        c.target.threadId === target.threadId
    );
    if (!stillPresent) return;
    this._paletteClosedBySelection = true;
    this.navigateToThread({
      conversationKey: target.threadId,
      projectId: target.projectId,
      ...(target.projectSlug ? { projectSlug: target.projectSlug } : {}),
      threadName: target.threadName,
      ...(target.defaultAgent ? { defaultAgent: target.defaultAgent } : {}),
    });
  }

  /**
   * Escape/backdrop/close-button: Shoelace is already animating its own
   * close in parallel (we never preventDefault its sl-request-close), so
   * this just syncs our `open` truth to match — see togglePalette's doc
   * comment for why this doesn't unmount the element. The reason itself
   * doesn't change what happens next — sl-after-hide always restores the
   * invoker for a non-selection dismissal — so it isn't threaded further.
   */
  private _handlePaletteDismiss(): void {
    this._closePaletteAndCancelLoad();
  }

  /**
   * Fires once Shoelace's close animation actually completes, regardless of
   * how the palette closed. A conversation selection focuses the new
   * composer; a document selection opens the page-level preview (the
   * invoker stays captured for the preview to restore later, so exactly one
   * modal owns focus at a time); a reopen queued during this close
   * (`_palettePendingReopen`) takes over instead of running this close's own
   * disposition; every other dismissal restores the captured invoker.
   */
  private _handlePaletteAfterHide(e: Event): void {
    // Focus/close handlers must be filtered to the owned dialog, not nested
    // bubbling events. This listener sits on <scion-quick-palette> itself,
    // one shadow-root boundary away from the actual sl-dialog that emits
    // sl-after-hide — any *other* Shoelace modal a future change nests
    // inside the switcher would otherwise bubble through here and wrongly
    // trigger a focus restore/composer-focus that this dismissal was never
    // about.
    const origin = e.composedPath()[0] as Element | undefined;
    if (!origin || !origin.classList?.contains('palette-dialog')) return;
    this._paletteCloseAnimating = false;
    if (this._palettePendingReopen) {
      // A Cmd/Ctrl+K arrived while this close was still animating out — see
      // togglePalette. This close's own disposition (composer focus, a
      // pending document preview, or an invoker restore) never ran and never
      // will: the user has already asked to reopen, which takes over focus
      // itself — provided the reopen is still actually allowed.
      const wasClosedBySelection = this._paletteClosedBySelection;
      this._palettePendingReopen = false;
      this._paletteClosedBySelection = false;
      this._paletteSkipFocusRestore = false;
      this._pendingDocumentPreviewTarget = null;
      if (this._paletteOpenGuardsHold()) {
        // The reopened palette owns focus right now — it must not re-capture
        // "whatever happens to be focused mid-animation" (likely nothing
        // useful) as what a *later* close on it should restore. The
        // superseded close's own would-be disposition is what that later
        // close should still land on: the composer a selection was about to
        // focus, or — for every other dismissal kind (escape, backdrop,
        // toggle, a discarded document preview) — whatever invoker was
        // already captured when the palette was first opened, left
        // untouched rather than re-captured.
        // Open first: `_openPalette` bumps `_paletteOpenEpoch` synchronously
        // before its own first `await`, so the value read right after this
        // call already reflects *this* open — not a later one that might
        // start (and bump it again) before the retarget's own await below
        // resolves.
        void this._openPalette({ skipInvokerCapture: true });
        if (wasClosedBySelection && this._selectionHandsFocusToComposer()) {
          void this._retargetPaletteInvokerToNewComposer(this._paletteOpenEpoch);
        }
      } else {
        // Real time passed between the press that queued this reopen and
        // this moment: another modal can have opened, or the route/
        // visibility can have changed. Whatever now blocks a fresh
        // Cmd/Ctrl+K blocks this deferred one too — opening anyway would
        // stack the palette over that modal, or open it invisibly on a page
        // that just stopped being current. Whatever now owns the page
        // (the new modal, the new route) owns focus too, so the old invoker
        // is discarded rather than restored into a page it may no longer
        // belong to.
        this._paletteInvoker = null;
        this._paletteInvokerSelection = null;
      }
      return;
    }
    if (this._paletteSkipFocusRestore) {
      // Closed programmatically (route change hid chat, or another modal
      // opened) — the invoker may now be hidden or gone; do not touch focus.
      this._paletteSkipFocusRestore = false;
      this._paletteInvoker = null;
      this._paletteInvokerSelection = null;
      return;
    }
    if (this._pendingDocumentPreviewTarget) {
      const target = this._pendingDocumentPreviewTarget;
      this._pendingDocumentPreviewTarget = null;
      if (!this._paletteOpenGuardsHold()) {
        // Real time passed between the selection and this moment: another
        // modal can have opened, or the route/visibility can have changed.
        // Opening the preview anyway would stack it over that modal, or open
        // it on a page that is no longer current — the same reasoning as the
        // queued-reopen branch above. The old invoker is discarded rather
        // than restored into a page it may no longer belong to.
        this._paletteInvoker = null;
        this._paletteInvokerSelection = null;
        return;
      }
      // The invoker deliberately stays captured: the preview dialog is about
      // to become the sole modal in control of focus, and
      // _closePaletteFilePreview restores the invoker once it closes.
      this._paletteFilePreviewTarget = target;
      void this._focusIntoFilePreview();
      return;
    }
    if (this._paletteClosedBySelection) {
      this._paletteClosedBySelection = false;
      if (this._selectionHandsFocusToComposer()) {
        void this._focusComposerAfterPaletteSelection();
      } else {
        this._focusPaletteFallback();
      }
      return;
    }
    this._restorePaletteInvokerFocus();
  }

  /**
   * The page-level preview's `chat-file-preview-close`: clear the target,
   * then either serve a Cmd/Ctrl+K queued behind this close (see
   * `togglePalette`) or restore the focus captured when the palette
   * originally opened.
   */
  private _closePaletteFilePreview(): void {
    this._paletteFilePreviewTarget = null;
    if (this._palettePendingReopen) {
      this._palettePendingReopen = false;
      if (this._paletteOpenGuardsHold()) {
        // The original invoker (the composer a document selection closed the
        // palette from, with its selection) stays captured for the reopened
        // palette's own later close to restore — the same
        // skipInvokerCapture reasoning as the palette's own queued reopen.
        void this._openPalette({ skipInvokerCapture: true });
        return;
      }
      // Real time passed between the press that queued this reopen and this
      // moment: another modal can have opened, or the route/visibility can
      // have changed. Whatever now blocks a fresh Cmd/Ctrl+K blocks this
      // deferred one too, so the old invoker is discarded rather than
      // restored into a page it may no longer belong to.
      this._paletteInvoker = null;
      this._paletteInvokerSelection = null;
      return;
    }
    this._restorePaletteInvokerFocus();
  }

  /**
   * `<scion-chat-file-preview>` is mounted once, always `<sl-dialog open>` —
   * a target change never flips `open` `false -> true`, so Shoelace's own
   * `sl-initial-focus` (which only fires on that transition) never runs for
   * it, and focus is left on whatever was focused before the palette closed
   * (typically nothing, since the palette's own input was just hidden).
   * Moves focus into the newly-opened preview's own close button — the one
   * control every preview state renders regardless of load status — so a
   * keyboard or screen-reader user lands inside the one dialog now actually
   * in control, the same guarantee the palette itself already gives.
   */
  private async _focusIntoFilePreview(): Promise<void> {
    await this.updateComplete;
    const preview = this.shadowRoot?.querySelector('scion-chat-file-preview');
    if (!(preview instanceof HTMLElement)) return;
    await (preview as unknown as { updateComplete: Promise<boolean> }).updateComplete;
    const dialog = preview.shadowRoot?.querySelector('sl-dialog');
    if (!(dialog instanceof HTMLElement)) return;
    await (dialog as unknown as { updateComplete: Promise<boolean> }).updateComplete;
    const closeButton = dialog.shadowRoot?.querySelector<HTMLElement>('[part~="close-button"]');
    closeButton?.focus();
  }

  private _capturePaletteInvokerFocus(): void {
    const el = deepActiveElement();
    this._paletteInvoker = el instanceof HTMLElement ? el : null;
    this._paletteInvokerSelection =
      el instanceof HTMLTextAreaElement || el instanceof HTMLInputElement
        ? this._readInvokerSelection(el)
        : null;
  }

  /**
   * Some `<input>` types (`number`, `email`, ...) don't support a text
   * selection at all: per the current spec their `selectionStart`/
   * `selectionEnd`/`selectionDirection` getters just return `null`. Older
   * engines (pre-2016 spec) instead threw an `InvalidStateError` from these
   * getters. Either way there's nothing to restore, so treat both the same
   * rather than letting the throwing case crash capture.
   */
  private _readInvokerSelection(el: HTMLTextAreaElement | HTMLInputElement): {
    start: number | null;
    end: number | null;
    direction: 'forward' | 'backward' | 'none' | null;
  } | null {
    try {
      return { start: el.selectionStart, end: el.selectionEnd, direction: el.selectionDirection };
    } catch {
      return null;
    }
  }

  /**
   * `offsetParent` is null both for an actually-hidden element and for a
   * `position: fixed` one, so it can't tell those two cases apart — a
   * visible invoker pinned with `position: fixed` (e.g. inside a docked
   * toolbar) would be wrongly treated as hidden. `checkVisibility()` tests
   * true visibility (display, visibility, and content-visibility) without
   * that false negative, once both `visibilityProperty` and its older alias
   * `checkVisibilityCSS` are passed — without them the CSS `visibility`
   * property is not checked by default, so a `visibility: hidden` invoker
   * would count as visible. `opacity` is deliberately left unchecked: an
   * `opacity: 0` element is still focusable and should get focus back.
   * Where `checkVisibility` isn't implemented, an element with no client
   * rects has no layout box at all, which covers `display: none` and a
   * disconnected element the same way `offsetParent === null` did.
   */
  private _isInvokerVisible(el: HTMLElement): boolean {
    const checkVisibility = (
      el as unknown as { checkVisibility?: (opts?: Record<string, boolean>) => boolean }
    ).checkVisibility;
    if (typeof checkVisibility === 'function') {
      return checkVisibility.call(el, { visibilityProperty: true, checkVisibilityCSS: true });
    }
    return el.getClientRects().length > 0;
  }

  private _restorePaletteInvokerFocus(): void {
    const el = this._paletteInvoker;
    const selection = this._paletteInvokerSelection;
    this._paletteInvoker = null;
    this._paletteInvokerSelection = null;
    if (!el || !el.isConnected || !this._isInvokerVisible(el)) {
      this._focusPaletteFallback();
      return;
    }
    el.focus();
    if (selection && (el instanceof HTMLTextAreaElement || el instanceof HTMLInputElement)) {
      try {
        el.setSelectionRange(selection.start, selection.end, selection.direction ?? undefined);
      } catch {
        // Some input types (number, email, ...) throw on setSelectionRange
        // regardless of arguments — selection restore is best-effort; focus
        // already succeeded above.
      }
    }
  }

  /**
   * The invoker disappeared (e.g. its panel was swiped away) or no composer
   * ever mounted — fall back to the page's own conversation container,
   * which is always present and visible whenever v2 renders at all (unlike
   * a generic "first `[tabindex]` element", which could be an offscreen or
   * `display:none` control elsewhere in the shadow root).
   */
  private _focusPaletteFallback(): void {
    const fallback = this.shadowRoot?.querySelector<HTMLElement>('#palette-focus-fallback');
    if (!fallback) return;
    const checkVisibility = (fallback as unknown as { checkVisibility?: () => boolean })
      .checkVisibility;
    if (typeof checkVisibility === 'function' && !checkVisibility.call(fallback)) return;
    fallback.focus();
  }

  /**
   * Whether a palette selection should hand focus to the newly-selected
   * conversation's composer. False on touch: focusing the composer would
   * raise the on-screen keyboard immediately after the user just dismissed
   * the palette, which is not what tapping a result is for on a phone.
   * Shared by the ordinary selection-close path below and the queued-reopen
   * path that can supersede it (`_handlePaletteAfterHide`'s
   * `_palettePendingReopen` branch) — without sharing it, a reopen queued
   * during a touch selection's close animation would silently re-target the
   * invoker to the composer anyway, bypassing the touch rule the direct path
   * enforces.
   */
  private _selectionHandsFocusToComposer(): boolean {
    return !this.touchPrimary.isTouch;
  }

  /**
   * After a DM selection, `openDM` has already updated `v2Conversation` and
   * pushed the new URL without recreating the page. Wait for this page (and
   * the newly (re)rendered thread/composer beneath it) to settle, then focus
   * the new composer — never the old one.
   */
  private async _focusComposerAfterPaletteSelection(): Promise<void> {
    const slTextarea = await this._pollForNewComposerTextarea();
    if (slTextarea) {
      slTextarea.focus();
      return;
    }
    // The composer never became available (e.g. read-only) — fall back to a
    // focusable heading/container rather than leaving focus lost.
    this._focusPaletteFallback();
  }

  /**
   * When a reopen queued behind a superseded *selection* close is dequeued
   * (see `_handlePaletteAfterHide`), the reopened palette itself owns focus
   * right now — this does not focus the new composer, it only holds it as
   * the invoker a *later* close on the reopened palette should land on,
   * mirroring what `_focusComposerAfterPaletteSelection` would have focused
   * had the reopen not superseded it.
   *
   * `epoch` is the value of `_paletteOpenEpoch` captured by the caller right
   * after starting *this* open — the assignment below only applies if that
   * open is still the current one once the poll resolves, and the palette
   * is still open at all. Without this, the poll below (which can take up
   * to 2s on a slow thread mount) could still be in flight when the
   * reopened palette is closed and a later open captures a fresh invoker of
   * its own — this stale result would then silently overwrite that fresh
   * one out from under the next close.
   */
  private async _retargetPaletteInvokerToNewComposer(epoch: number): Promise<void> {
    const slTextarea = await this._pollForNewComposerTextarea();
    if (slTextarea && epoch === this._paletteOpenEpoch && this.v2PaletteOpen) {
      this._paletteInvoker = slTextarea;
      this._paletteInvokerSelection = null;
    }
  }

  /**
   * Poll briefly for the current conversation's composer textarea, shared by
   * {@link _focusComposerAfterPaletteSelection} and
   * {@link _retargetPaletteInvokerToNewComposer}. `scion-chat-thread`/
   * `scion-chat-composer` mount as a consequence of a just-committed
   * property update, but their own nested render passes are separate async
   * update cycles this page's `updateComplete` does not wait for — a single
   * rAF check can lose that race under load, so this polls for up to 2s
   * instead. Returns `null` if the composer never became available (e.g.
   * read-only).
   */
  private async _pollForNewComposerTextarea(): Promise<HTMLElement | null> {
    await this.updateComplete;
    const deadline = Date.now() + 2000;
    let slTextarea: Element | null = null;
    do {
      const thread = this.shadowRoot?.querySelector('scion-chat-thread');
      const composer = thread?.shadowRoot?.querySelector('scion-chat-composer');
      slTextarea = composer?.shadowRoot?.querySelector('sl-textarea') ?? null;
      if (slTextarea) break;
      await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    } while (Date.now() < deadline);
    return slTextarea instanceof HTMLElement ? slTextarea : null;
  }

  // =========================================================================
  // Render
  // =========================================================================

  override render() {
    return this.renderV2();
  }

  // ---- V2 Render ----

  private renderV2() {
    return html`
      ${this.v2SwitcherLoaded
        ? html`
            <scion-quick-palette
              label="Quick switcher"
              placeholder="Search agents, threads, people, documents…"
              .open=${this.v2PaletteOpen}
              .groups=${this.v2PaletteGroups}
              @palette-select=${this._handlePaletteSelect}
              @palette-retry=${this._handlePaletteRetry}
              @palette-dismiss=${this._handlePaletteDismiss}
              @sl-after-hide=${this._onPaletteAfterHide}
            ></scion-quick-palette>
          `
        : nothing}
      <scion-chat-file-preview
        .target=${this._paletteFilePreviewTarget}
        @chat-file-preview-close=${() => this._closePaletteFilePreview()}
      ></scion-chat-file-preview>
      <div
        id="palette-focus-fallback"
        tabindex="-1"
        class="v2-panels"
        data-panel=${this.mobilePanel}
        @touchstart=${this.handleTouchStart}
        @touchmove=${this.handleTouchMove}
        @touchend=${this.handleTouchEnd}
        @mention-click=${this.handleMentionClick}
        @scroll=${this._handleV2PanelsScroll}
      >
        <div class="v2-rail" ?inert=${this.isMobileLayout && this.mobilePanel !== 'left'}>
          ${this.v2SpaceRailLoaded
            ? html`
                <scion-chat-space-rail
                  selectedKey=${this.v2Conversation?.conversationKey || ''}
                  currentUserId=${this.pageData?.user?.id || ''}
                  @thread-select=${this.handleThreadSelect}
                  @reset-view=${this.handleResetView}
                ></scion-chat-space-rail>
              `
            : html`<div class="loading-rail"><sl-spinner></sl-spinner></div>`}
        </div>

        <div
          class="v2-content ${this.v2MembersExpanded ? '' : 'edge-right'}"
          ?inert=${this.isMobileLayout && this.mobilePanel !== 'center'}
        >
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

        <div
          class="v2-members ${this.v2MembersExpanded ? '' : 'collapsed'}"
          style="--members-w: ${this.membersWidth}px"
          ?inert=${this.isMobileLayout && this.mobilePanel !== 'right'}
        >
          <button
            class="v2-members-resizer"
            role="separator"
            aria-orientation="vertical"
            aria-label="Resize members panel"
            aria-valuenow=${this.membersWidth}
            aria-valuemin=${MEMBERS_WIDTH_MIN}
            aria-valuemax=${MEMBERS_WIDTH_MAX}
            @pointerdown=${this.startMembersResize}
            @keydown=${this.onMembersResizeKey}
          ></button>
          <div class="v2-members-header">
            ${this.renderMobileBackButton('center')}
            <span>Members</span>
          </div>
          <scion-chat-members
            .humans=${this.v2HumanMembers}
            .agents=${this.v2AgentMembers}
            .typingUserIds=${this.v2TypingUserIds}
            .unreadFromIds=${this.v2UnreadFromIds}
            .dmInfoByPeerId=${this.v2DMInfoByPeerId}
            current-user-id="${this.pageData?.user?.id || ''}"
            dm-peer-id="${this.v2Conversation?.isDM ? this.v2Conversation.peerId : ''}"
            default-agent-slug="${this.v2Conversation?.defaultAgent || ''}"
            @member-click=${this.handleMemberClick}
            @member-marked-unread=${this.handleMemberMarkedUnread}
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

  /** Look up the project ID for an agent DM peer. */
  private getAgentProjectId(peerId: string): string {
    const agent = this.v2AgentMembers.find((a) => a.id === peerId);
    if (agent?.projectId) return agent.projectId;
    return this.v2Conversation?.projectId || '';
  }

  /**
   * Resolve a thread's `defaultAgent` (which holds either an agent ID or a
   * slug) to the agent's ID, so it can be used wherever DM code expects
   * `conv.peerId`. Empty string when the agent isn't a known space member.
   */
  private resolveDefaultAgentId(defaultAgent: string): string {
    if (!defaultAgent) return '';
    const byId = this.v2AgentMembers.find((a) => a.id === defaultAgent);
    if (byId) return byId.id;
    const bySlug = this.v2AgentMembers.find((a) => a.slug === defaultAgent);
    return bySlug?.id || '';
  }

  /**
   * Terminal + graph icon buttons for an agent, shared by the DM header and
   * the thread header (when the thread has a default agent). Graph link is
   * omitted when the agent's project can't be resolved.
   */
  private renderAgentToolbarButtons(agentId: string): TemplateResult | typeof nothing {
    if (!agentId) return nothing;
    const projectId = this.getAgentProjectId(agentId);
    return html`
      <sl-tooltip content="Open terminal">
        <sl-icon-button
          name="terminal"
          label="Open terminal"
          href=${terminalHref(agentId)}
          @click=${(e: MouseEvent) => {
            if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
            e.preventDefault();
            openTerminal(agentId);
          }}
        ></sl-icon-button>
      </sl-tooltip>
      ${projectId
        ? html`
            <sl-tooltip content="Open in graph">
              <sl-icon-button
                name="diagram-3"
                label="Open in graph"
                href=${agentGraphHref(projectId, agentId)}
                @click=${(e: MouseEvent) => {
                  if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
                  e.preventDefault();
                  navigateTo(agentGraphHref(projectId, agentId));
                }}
              ></sl-icon-button>
            </sl-tooltip>
          `
        : nothing}
    `;
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
    const agentProjectSlug =
      conv.isDM && conv.peerKind === 'agent' && conv.peerId
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
                ? html`<sl-icon
                      name="folder"
                      style="font-size: var(--chat-fs-base); color: var(--scion-text-muted, #64748b)"
                    ></sl-icon>
                    <span
                      style="font-size: var(--chat-fs-md); color: var(--scion-text-muted, #64748b)"
                      >${agentProjectSlug}</span
                    >`
                : nothing}
              ${conv.peerKind === 'agent'
                ? html`<span style="font-size: var(--chat-fs-lg)">🤖</span>`
                : html`<sl-icon
                    name="person"
                    style="font-size: var(--chat-fs-lg); color: var(--scion-text-muted)"
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
        <div
          class="header-actions"
          style="display: flex; align-items: center; gap: 0.25rem; margin-left: auto;"
        >
          ${conv.isDM && conv.peerKind === 'agent' && conv.peerId
            ? this.renderAgentToolbarButtons(conv.peerId)
            : nothing}
          ${!conv.isDM && conv.defaultAgent
            ? this.renderAgentToolbarButtons(this.resolveDefaultAgentId(conv.defaultAgent))
            : nothing}
          <sl-tooltip
            class="density-toggle"
            content=${this.density === 'dense' ? 'Comfortable view' : 'Dense view'}
          >
            <sl-icon-button
              name=${this.density === 'dense' ? 'arrows-angle-expand' : 'arrows-angle-contract'}
              label="Toggle density"
              @click=${() => this.toggleDensity()}
            ></sl-icon-button>
          </sl-tooltip>
          ${conv.isDM && conv.peerKind === 'agent'
            ? html`
                <sl-tooltip content="Promote to thread">
                  <sl-icon-button
                    name="box-arrow-up-right"
                    label="Promote to thread"
                    @click=${() => void this.openPromoteDialog()}
                  ></sl-icon-button>
                </sl-tooltip>
              `
            : nothing}
          ${conv.isDM ? this.renderDMMuteButton(conv) : nothing}
          ${conv.projectId
            ? html`
                <sl-dropdown>
                  <sl-icon-button
                    slot="trigger"
                    name="three-dots-vertical"
                    label="Options"
                  ></sl-icon-button>
                  <sl-menu @sl-select=${this.handleRailMenuSelect}>
                    <sl-menu-item value="toggle-chime">
                      <sl-icon
                        slot="prefix"
                        name=${this.projectChimeOn ? 'volume-up' : 'volume-mute'}
                      ></sl-icon>
                      ${this.projectChimeOn ? 'Chime on' : 'Chime off'}
                    </sl-menu-item>
                  </sl-menu>
                </sl-dropdown>
              `
            : nothing}
          <sl-dropdown>
            <sl-tooltip content="Export conversation" slot="trigger">
              <sl-icon-button name="download" label="Export conversation"></sl-icon-button>
            </sl-tooltip>
            <sl-menu>
              <sl-menu-item @click=${() => this.exportMarkdown()}>
                <sl-icon slot="prefix" name="filetype-md"></sl-icon>
                Download as Markdown
              </sl-menu-item>
              <sl-menu-item @click=${() => this.exportPrint()}>
                <sl-icon slot="prefix" name="printer"></sl-icon>
                Print / Save as PDF
              </sl-menu-item>
              <sl-menu-item @click=${() => void this.exportClipboard()}>
                <sl-icon slot="prefix" name="clipboard"></sl-icon>
                Copy to clipboard
              </sl-menu-item>
            </sl-menu>
          </sl-dropdown>
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
              conversationName=${conv.isDM
                ? conv.peerName
                : conv.threadName
                  ? '#' + conv.threadName
                  : ''}
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
              .agentMembers=${this.v2AgentMembers}
              .agents=${this.getAgentsFromMembers()}
              @default-agent-changed=${this.handleDefaultAgentChanged}
            ></scion-chat-thread>
          `}
      ${this.renderPromoteDialog()}
    `;
  }

  /**
   * Render the promote-to-thread confirmation dialog. Displayed inline when
   * the user clicks the promote button on an agent DM header.
   */
  private renderPromoteDialog() {
    if (!this.promoteDialogOpen || !this.v2Conversation) return nothing;
    const conv = this.v2Conversation;
    const displaySlug =
      conv.projectSlug || this._projectIdToSlug.get(conv.projectId) || 'this project';
    return html`
      <sl-dialog
        class="promote-dialog"
        label="Promote DM to Thread"
        ?open=${this.promoteDialogOpen}
        @sl-after-hide=${() => {
          this.promoteDialogOpen = false;
        }}
      >
        <p>
          This will move your conversation with
          <strong>${conv.peerName}</strong> into a shared thread visible to all members of
          <strong>${displaySlug}</strong>. This cannot be undone.
        </p>
        <sl-input
          label="Thread name"
          value=${this.promoteThreadName}
          @sl-input=${(e: Event) => {
            this.promoteThreadName = (e.target as HTMLInputElement).value;
          }}
          maxlength="100"
          required
        ></sl-input>
        <div slot="footer">
          <sl-button
            @click=${() => {
              this.promoteDialogOpen = false;
            }}
            >Cancel</sl-button
          >
          <sl-button
            variant="danger"
            ?loading=${this.promoteLoading}
            ?disabled=${!this.promoteThreadName.trim()}
            @click=${() => void this.executePromote()}
            >Promote</sl-button
          >
        </div>
      </sl-dialog>
    `;
  }

  /** Open the promote dialog, pre-filling the thread name with the peer name. */
  private openPromoteDialog(): void {
    const conv = this.v2Conversation;
    if (!conv || !conv.isDM || conv.peerKind !== 'agent') return;
    this.promoteThreadName = conv.peerName || '';
    this.promoteDialogOpen = true;
  }

  /**
   * Execute the DM-to-thread promotion: POST to the promote endpoint, then
   * navigate to the newly created thread on success.
   */
  private async executePromote(): Promise<void> {
    const conv = this.v2Conversation;
    if (!conv) return;
    const conversationKey = conv.conversationKey;
    this.promoteLoading = true;
    try {
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(conv.conversationKey)}/promote`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name: this.promoteThreadName }),
        }
      );
      if (!res.ok) {
        const err = await parseApiError(res, `Promotion failed (${res.status})`);
        if (res.status === 409) {
          const msg =
            err.code === 'IN_FLIGHT_MESSAGES'
              ? 'Agent is still responding. Try again in a few seconds.'
              : err.code === 'NAME_CONFLICT'
                ? 'A thread with that name already exists.'
                : err.message || 'Conflict — please try again.';
          this.showPromoteToast(msg, 'warning');
        } else {
          this.showPromoteToast(err.message, 'danger');
        }
        return;
      }

      const topic = (await res.json()) as {
        id: string;
        projectId: string;
        name: string;
        defaultAgent?: string;
      };

      // Always close the dialog on success, even if the SSE dm.promoted
      // handler already navigated us away before this fetch resolved.
      this.promoteDialogOpen = false;

      // Guard: SSE dm.promoted may have already navigated us away
      if (this.v2Conversation?.conversationKey !== conversationKey) return;

      // Navigate to the new thread
      this.navigateToPromotedThread(topic);
      this.showPromoteToast(`Conversation promoted to #${topic.name}`, 'success');
    } finally {
      this.promoteLoading = false;
    }
  }

  /**
   * Navigate to a newly promoted thread, updating conversation state and URL.
   */
  private navigateToPromotedThread(topic: {
    id: string;
    projectId: string;
    name: string;
    defaultAgent?: string;
  }): void {
    ++this._userNavSeq;
    const slug = this._projectIdToSlug.get(topic.projectId) || '';
    this.v2Conversation = {
      conversationKey: topic.id,
      projectId: topic.projectId,
      projectSlug: slug,
      threadName: topic.name,
      defaultAgent: topic.defaultAgent || '',
      isDM: false,
      peerName: '',
      peerId: '',
      peerKind: 'user',
    };
    this.mobilePanel = 'center';

    // Update URL
    const base = import.meta.env.BASE_URL;
    let threadPath: string;
    if (slug) {
      threadPath = `/chat/${encodeURIComponent(slug)}/${encodeURIComponent(topic.id)}`;
    } else {
      threadPath = `/chat/space/${encodeURIComponent(topic.projectId)}/thread/${encodeURIComponent(topic.id)}`;
    }
    const browserPath = base && base !== '/' ? base.replace(/\/$/, '') + threadPath : threadPath;
    window.history.pushState({}, '', browserPath);

    dispatchPageTitle(this, `#${topic.name}`, 'Chat');
    void this.loadV2Members(topic.projectId);

    // Reload the space rail so the new thread appears
    const rail = this.shadowRoot?.querySelector('scion-chat-space-rail') as
      | import('../shared/chat/chat-space-rail.js').ScionChatSpaceRail
      | null;
    if (rail) void rail.reload();
  }

  /** Show a toast notification for promote results. */
  private showPromoteToast(
    message: string,
    variant: 'success' | 'warning' | 'danger' = 'success'
  ): void {
    // Use the Shoelace alert/toast pattern if available, else console
    const alert = Object.assign(document.createElement('sl-alert'), {
      variant,
      closable: true,
      duration: 4000,
      innerHTML: `<sl-icon name="${variant === 'success' ? 'check-circle' : variant === 'warning' ? 'exclamation-triangle' : 'exclamation-circle'}" slot="icon"></sl-icon>${message}`,
    });
    document.body.appendChild(alert);
    void (alert as unknown as { toast(): Promise<void> }).toast();
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

    const msgFragment = detail.messageId ? `#msg-${encodeURIComponent(detail.messageId)}` : '';

    this.v2SearchActive = false;

    // If the result is in a different conversation, navigate to it.
    if (detail.conversationKey !== this.v2Conversation?.conversationKey) {
      const isDM = detail.conversationKey.startsWith('dm:');
      if (isDM) {
        navigateTo(`/chat/dm/${encodeURIComponent(detail.conversationKey)}${msgFragment}`);
      } else if (detail.projectId) {
        const slug = this._projectIdToSlug.get(detail.projectId);
        if (slug) {
          navigateTo(
            `/chat/${encodeURIComponent(slug)}/${encodeURIComponent(detail.conversationKey)}${msgFragment}`
          );
        } else {
          navigateTo(
            `/chat/space/${encodeURIComponent(detail.projectId)}/thread/${encodeURIComponent(detail.conversationKey)}${msgFragment}`
          );
        }
      }
    } else if (detail.messageId) {
      // Same conversation: scroll directly to the message.
      void this.updateComplete.then(() => {
        const thread = this.shadowRoot?.querySelector('scion-chat-thread') as
          | import('../shared/chat/chat-thread.js').ScionChatThread
          | null;
        void thread?.scrollToMessageById(detail.messageId);
      });
    }
  }

  /** Extract agent members as Agent-like objects for the mention autocomplete. */
  private getAgentsFromMembers(): import('../../shared/types.js').Agent[] {
    const projectId = this.v2Conversation?.projectId || '';
    if (this.mentionAgentsSource === this.v2Members && this.mentionAgentsProjectId === projectId) {
      return this.mentionAgents;
    }
    this.mentionAgentsSource = this.v2Members;
    this.mentionAgentsProjectId = projectId;
    this.mentionAgents = this.v2Members
      .filter((m) => m.kind === 'agent')
      .map((m) => ({
        id: m.id,
        name: m.name,
        slug: m.name,
        projectId,
        template: '',
        phase: 'running' as const,
        status: 'active' as const,
      }));
    return this.mentionAgents;
  }

  // ---------------------------------------------------------------------------
  // Export helpers — delegate to the thread component (#1570)
  // ---------------------------------------------------------------------------

  /** Get a reference to the active scion-chat-thread component. */
  private get chatThread(): import('../shared/chat/chat-thread.js').ScionChatThread | null {
    return this.shadowRoot?.querySelector('scion-chat-thread') as
      | import('../shared/chat/chat-thread.js').ScionChatThread
      | null;
  }

  /** Download the current conversation as Markdown. */
  private exportMarkdown(): void {
    this.chatThread?.exportAsMarkdown();
  }

  /** Open a print-friendly view of the conversation. */
  private exportPrint(): void {
    this.chatThread?.printConversation();
  }

  /** Copy the conversation to the clipboard as formatted text. */
  private async exportClipboard(): Promise<void> {
    await this.chatThread?.copyAsFormattedText();
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-chat': ScionPageChat;
  }
}
