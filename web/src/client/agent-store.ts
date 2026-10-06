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
 * Shared agent-list store.
 *
 * Owns fetching, pagination, coalescing, completeness and lifetime of agent
 * lists, keyed by query. Every surface that needs "the agents of the hub" (or
 * of one project) asks the store instead of walking the list endpoint itself,
 * so a list is fetched once and then kept current from SSE.
 *
 * Live updates come from a store-owned feed: a dedicated {@link StateManager}
 * on the `agent-feed` scope, which subscribes to `project.*.agent.>`. Its
 * scope never changes, so page navigation (which re-scopes the global
 * `stateManager`) cannot wipe it. All delta merging, unknown-delta buffering,
 * tombstones, seed epochs and the completeness flag are the feed's own.
 *
 * Contract highlights:
 * - `ensure` on a ready, fresh entry answers from memory with no request,
 *   also while a walk the probe started runs in the background.
 *   Concurrent `ensure` calls for one key share one walk.
 * - A caller's abort signal detaches only that caller. A walk is aborted
 *   only when it has neither waiters nor retainers.
 * - `complete` is true only after a full walk succeeded; a walk cut off by
 *   the page bound is `ready` but not complete. Loading snapshots are never
 *   complete. A walk the probe starts leaves the snapshot as it is until it
 *   succeeds; if it fails, the snapshot stays as it was and the probe goes
 *   on.
 * - Every walk is a seeded walk: deltas that arrive while it runs are
 *   reapplied on top of its REST rows, and tombstoned agents never return.
 * - While the feed is down, or an entry is stale, `ensure` revalidates
 *   rather than answering from memory.
 * - A retained hub or unfiltered project list is probed every 30s (±3s)
 *   while the page is visible, for changes SSE does not carry: one compact
 *   page of the 50 most recently active rows (by last activity time, else
 *   `updated`), applied as a seeded merge. It walks once when it cannot
 *   catch up in five pages or when the server's count differs from the rows
 *   held. A probe never sets the completeness flag. A probe due five minutes
 *   or more after the list's last walk began walks instead.
 */

import type { Agent, Capabilities } from '../shared/types.js';
import { apiFetch } from './api.js';
import type { AccessDeniedDetail, ApiFetchOptions } from './api.js';
import { dropTombstoned, mergeChanged } from './agent-merge.js';
import { paginateAll, PaginationTruncatedError } from './paginate-all.js';
import { StateManager, stateManager } from './state.js';
import type { AgentsChangedDetail } from './state.js';
import { ACCOUNT_TEARDOWN_EVENT } from '../utils/auth.js';
import { MEMBERSHIP_CHANGED_EVENT } from '../utils/membership-events.js';

/** An agent list the store can hold. Only filters the server applies are part of a query. */
export type AgentQuery =
  | { scope: 'hub'; ownership?: 'mine' | 'shared'; label?: string }
  | { scope: 'project'; projectId: string; label?: string };

export interface AgentListSnapshot {
  /** Canonical key for the query. */
  readonly key: string;
  /** Identity-stable: a new array only when membership or a row changes. */
  readonly agents: readonly Agent[];
  readonly status: 'idle' | 'loading' | 'ready' | 'error';
  /**
   * True only once a full walk has succeeded and no revalidation is running,
   * other than a walk the probe started in the background.
   */
  readonly complete: boolean;
  readonly error?: Error;
  readonly fetchedAt?: number;
  /** Bumps on every published change, for memoised selectors. */
  readonly version: number;
}

export type InvalidateReason = 'membership' | 'permission' | 'resync' | 'agents-created' | 'manual';

export type AgentListListener = (snapshot: AgentListSnapshot) => void;

export interface EnsureOptions {
  signal?: AbortSignal;
  onProgress?: AgentListListener;
}

export interface AgentStoreOptions {
  /** Issues list requests. Defaults to `apiFetch` with the access-denied toast suppressed. */
  fetch?: (path: string, options: ApiFetchOptions) => Promise<Response>;
  /** Creates the feed. Defaults to a new `StateManager`. */
  feedFactory?: () => StateManager;
  /** Clock for `fetchedAt` and eviction order. */
  now?: () => number;
  /**
   * The signed-in user's id, read on every `ensure` and `retain`. A change
   * from one known user to another resets the store. Defaults to the
   * global `stateManager`'s current user.
   */
  currentUserId?: () => string;
  /**
   * Where the window-level triggers (membership change, access denied,
   * account teardown, page unload) are heard. Defaults to `window`; `null`
   * listens to nothing.
   */
  events?: EventTarget | null;
  /** Row projection requested from the server. Defaults to `full`. */
  view?: 'full' | 'compact';
  pageSize?: number;
  maxPages?: number;
  pageTimeoutMs?: number;
  /** How long a walk waits for the feed to connect before walking anyway. */
  connectTimeoutMs?: number;
  /** How long an unused entry is kept for an instant reopen. */
  evictionGraceMs?: number;
  /** Most unused entries kept at once; the least recently used go first. */
  maxUnretained?: number;
  /** How long the feed stays connected after the last entry is released. */
  feedIdleMs?: number;
  /**
   * Whether the page is visible, and where its `visibilitychange` is heard.
   * Probes run only while it is visible. Defaults to `document`; `null`
   * counts as always visible.
   */
  visibility?: VisibilitySource | null;
  /** Random source for probe jitter, in [0, 1). */
  random?: () => number;
  /**
   * How long after a probed list's last walk began a due probe walks in
   * full instead. Defaults to {@link AGENT_PROBE_FULL_WALK_MS}.
   */
  probeFullWalkMs?: number;
}

/** The part of `document` the probe schedule reads. */
export type VisibilitySource = EventTarget & { readonly visibilityState: string };

/** Page size for every agent-list walk. Server maximum is 500. */
export const AGENT_STORE_PAGE_SIZE = 200;
/** Page bound: 100 pages of 200 rows. Beyond that a list is not complete. */
export const AGENT_STORE_MAX_PAGES = 100;
const DEFAULT_CONNECT_TIMEOUT_MS = 5_000;
const DEFAULT_EVICTION_GRACE_MS = 5 * 60_000;
const DEFAULT_MAX_UNRETAINED = 8;
const DEFAULT_FEED_IDLE_MS = 60_000;
/** Single-agent reads in flight at once for agents created over the feed. */
export const AGENT_READ_CONCURRENCY = 4;
/**
 * More created agents than this waiting for their read is a burst: the store
 * revalidates once instead of reading each (every read costs the hub a scan
 * of the agent's project).
 */
export const AGENT_READ_BURST_LIMIT = 8;
/** A single-agent read is abandoned after this long. */
export const AGENT_READ_TIMEOUT_MS = 30_000;
/**
 * A retained hub or project list is probed this often for changes SSE does
 * not carry: one request for the most recently active rows, which shows
 * agents added without an event and changes to recently active ones, plus
 * the server's count, which shows agents removed. Renames and label edits
 * leave the activity time alone and wait for the periodic full walk.
 */
export const AGENT_PROBE_INTERVAL_MS = 30_000;
/** Each probe is scheduled up to this much earlier or later. */
export const AGENT_PROBE_JITTER_MS = 3_000;
/** Rows per probe page. */
export const AGENT_PROBE_LIMIT = 50;
/** Pages a probe follows past its first before falling back to a full walk. */
export const AGENT_PROBE_MAX_EXTRA_PAGES = 4;
/**
 * After a probe that does not catch up walks, the next such walk waits at
 * least this long after the last walk of any kind, doubling while probes
 * keep overflowing; meanwhile probes read one page. The periodic full walk
 * bounds the wait.
 */
export const AGENT_PROBE_OVERFLOW_BACKOFF_MS = 2 * 60_000;
/** A probe still running after this long is abandoned; the next one is scheduled. */
export const AGENT_PROBE_TIMEOUT_MS = 30_000;
/** After the server refuses a list's sorted view, wait this long before probing it again. */
export const AGENT_PROBE_REFUSED_RETRY_MS = 10 * 60_000;
/**
 * A probed list also walks in full when a probe is due and this long has
 * passed since its last walk began: a rename or label change leaves the
 * activity time alone, so the probe's first page does not show it.
 */
export const AGENT_PROBE_FULL_WALK_MS = 5 * 60_000;

interface Waiter {
  resolve: (snapshot: AgentListSnapshot) => void;
  reject: (err: unknown) => void;
  onProgress?: AgentListListener | undefined;
  cleanup: () => void;
}

interface Walk {
  controller: AbortController;
  /** `connecting` while waiting for the feed, `fetching` once pages are requested. */
  phase: 'connecting' | 'fetching';
  /** Ids an SSE `created` added to the entry while this walk ran. */
  sseAdded: Set<string>;
  /** The feed dropped while this walk was reading pages, even if it reconnected since. */
  feedDropped: boolean;
  /** Started by the probe: the entry keeps its status and rows while it runs. */
  background: boolean;
}

interface Entry {
  key: string;
  query: AgentQuery;
  path: string;
  agents: Agent[];
  status: AgentListSnapshot['status'];
  complete: boolean;
  error?: Error | undefined;
  fetchedAt?: number | undefined;
  version: number;
  snapshot: AgentListSnapshot;
  /** Rows may have missed changes; the next `ensure` (or a retainer) revalidates. */
  stale: boolean;
  /** Something changed during the walk in flight: walk once more after it. */
  followUp: boolean;
  scopeCapabilities?: Capabilities | undefined;
  retainers: Set<AgentListListener>;
  waiters: Set<Waiter>;
  walk: Walk | null;
  lastUsed: number;
  evictTimer: ReturnType<typeof setTimeout> | null;
  probeTimer: ReturnType<typeof setTimeout> | null;
  /** Aborts the probe in flight. */
  probe: AbortController | null;
  /** When the scheduled probe is due (epoch ms); kept while the page is hidden. */
  probeDueAt?: number | undefined;
  /** The newest position in the probe's order seen by the last walk or probe. */
  highWater?: ProbeMark | undefined;
  /** The server refused the sorted view for this list: no probe before this time (epoch ms). */
  probeRetryAt?: number | undefined;
  /** A refusal for this list has been logged. */
  probeRefusalLogged: boolean;
  /** The server count that last made a probe walk, so the same mismatch walks once. */
  countWalkTotal?: number | undefined;
  /** While probes keep overflowing, how long after the last walk began the next overflow walk may start. */
  overflowBackoffMs: number;
  /** When the last walk began (epoch ms). */
  walkedAt?: number | undefined;
}

interface ProbePage {
  agents?: Agent[];
  nextCursor?: string;
  totalCount?: number;
}

function abortError(message: string): DOMException {
  return new DOMException(message, 'AbortError');
}

function isAbortError(err: unknown): boolean {
  return err instanceof DOMException && err.name === 'AbortError';
}

/** Canonical key for a query: scope, then its server filters in a fixed order. */
export function agentQueryKey(q: AgentQuery): string {
  const params: string[] = [];
  const label = q.label?.trim();
  if (label) params.push(`label=${encodeURIComponent(label)}`);
  if (q.scope === 'hub' && q.ownership) params.push(`ownership=${q.ownership}`);
  const base = q.scope === 'hub' ? 'hub' : `project:${q.projectId}`;
  return params.length > 0 ? `${base}?${params.join('&')}` : base;
}

/** Probes cover the lists SSE adds to: the whole hub and unfiltered projects. */
function isProbeable(q: AgentQuery): boolean {
  if (q.label?.trim()) return false;
  return q.scope !== 'hub' || !q.ownership;
}

/** The newest-first sorted page of a probeable list. */
function probePath(q: AgentQuery, cursor?: string): string {
  const params = new URLSearchParams({
    sort: 'updated',
    dir: 'desc',
    limit: String(AGENT_PROBE_LIMIT),
    view: 'compact',
  });
  if (cursor) params.set('cursor', cursor);
  const base =
    q.scope === 'hub'
      ? '/api/v1/agents'
      : `/api/v1/projects/${encodeURIComponent(q.projectId)}/agents`;
  return `${base}?${params.toString()}`;
}

/** A time as epoch ms, or undefined when absent or unparsable. */
function timeMs(value: string | undefined): number | undefined {
  if (!value) return undefined;
  const ms = Date.parse(value);
  return Number.isNaN(ms) ? undefined : ms;
}

/** `updated` as epoch ms, or undefined when absent or unparsable. */
function updatedAt(agent: Agent | undefined): number | undefined {
  return timeMs(agent?.updated);
}

/**
 * A row's position in the server's `sort=updated` order, which lists newest
 * first by the last activity time when set, else `updated`, with ties on
 * `created`, then id, both descending. A heartbeat moves `updated` but not
 * the activity time, so it does not move an active agent up the order.
 * Times compare to the millisecond, coarser than the server stores them: two
 * keys within one millisecond fall through to the tie order here, so a
 * catch-up decision at that boundary can be off by one probe.
 */
interface ProbeMark {
  at: number;
  created: number;
  id: string;
}

function probeMark(agent: Agent | undefined): ProbeMark | undefined {
  if (!agent) return undefined;
  // A Go zero time is how the server writes an unset activity time.
  const activity = agent.lastActivityEvent?.startsWith('0001')
    ? undefined
    : timeMs(agent.lastActivityEvent);
  const at = activity ?? updatedAt(agent);
  if (at === undefined) return undefined;
  return { at, created: timeMs(agent.created) ?? 0, id: agent.id };
}

/** Positive when `a` lists before `b`, negative when after, zero for the same position. */
function compareMarks(a: ProbeMark, b: ProbeMark): number {
  return a.at - b.at || a.created - b.created || (a.id > b.id ? 1 : a.id < b.id ? -1 : 0);
}

function newestMark(rows: readonly Agent[]): ProbeMark | undefined {
  let newest: ProbeMark | undefined;
  for (const row of rows) {
    const mark = probeMark(row);
    if (mark && (!newest || compareMarks(mark, newest) > 0)) newest = mark;
  }
  return newest;
}

/**
 * Probe-row fields that do not make a held row stale: `updated`, which every
 * heartbeat moves; `containerStatus`, the runtime's text ("Up 5 minutes"),
 * which heartbeats rewrite and no list consumer reads; and `creatorName`, the
 * compact view's copy of `appliedConfig.creatorName`, which full rows hold.
 * A row that changes otherwise merges with all of them.
 */
const PROBE_UNCOMPARED_FIELDS: ReadonlySet<string> = new Set([
  'updated',
  'containerStatus',
  'creatorName',
]);

/**
 * Whether merging a probe row into the row held would change a field the
 * probe compares (see {@link PROBE_UNCOMPARED_FIELDS}). A merge never clears
 * a field the probe row omits, so only the probe row's own fields count.
 */
function differsBeyondHeartbeat(row: Agent, held: Agent): boolean {
  const before = held as unknown as Record<string, unknown>;
  for (const [key, value] of Object.entries(row)) {
    if (PROBE_UNCOMPARED_FIELDS.has(key)) continue;
    const other = before[key];
    if (value === other) continue;
    if (typeof value !== 'object' || typeof other !== 'object' || !value || !other) return true;
    if (JSON.stringify(value) !== JSON.stringify(other)) return true;
  }
  return false;
}

function queryPath(q: AgentQuery, view: 'full' | 'compact'): string {
  const params = new URLSearchParams();
  if (q.scope === 'hub' && q.ownership) params.set('scope', q.ownership);
  const label = q.label?.trim();
  if (label) params.set('label', label);
  if (view === 'compact') params.set('view', 'compact');
  const base =
    q.scope === 'hub'
      ? '/api/v1/agents'
      : `/api/v1/projects/${encodeURIComponent(q.projectId)}/agents`;
  const qs = params.toString();
  return qs ? `${base}?${qs}` : base;
}

/**
 * Whether an agent the entry does not hold yet may be added from SSE. A
 * server-filtered list (ownership, label) never adds: the client cannot
 * evaluate the filter.
 */
function shouldAddFor(q: AgentQuery): (agent: Agent) => boolean {
  const never = (): boolean => false;
  if (q.label?.trim()) return never;
  if (q.scope === 'hub') {
    return q.ownership ? never : (): boolean => true;
  }
  const projectId = q.projectId;
  return (agent) => agent.projectId === projectId;
}

const VISIBILITY_RESOURCES = new Set(['agent', 'project']);
const VISIBILITY_ACTIONS = new Set(['read', 'list']);

/**
 * Whether a listing row (or the feed's row after a status event) shows the
 * agent present with no active delete: its deletion view is an explicit null
 * (never deleted, restored, or no delete active) or a failed delete. Such a
 * row read on a later feed means the agent is live again, whatever an earlier
 * feed saw. A row still deleting, one with a state this client does not know,
 * or one without the deletion key says nothing about that.
 */
export function listedWithoutActiveDelete(row: Agent): boolean {
  const deletion = row.deletion;
  if (deletion === null) return true;
  if (deletion === undefined) return false;
  return deletion.state === 'failed';
}

function defaultFetch(path: string, options: ApiFetchOptions): Promise<Response> {
  // The store reports its own failures to its callers. Letting a store
  // request raise the global access-denied event would also make the store
  // invalidate itself and walk again.
  return apiFetch(path, { ...options, suppressAccessDeniedToast: true });
}

export class AgentStore {
  private readonly entries = new Map<string, Entry>();
  private feed: StateManager | null = null;
  private detachFeed: (() => void) | null = null;
  private feedIdleTimer: ReturnType<typeof setTimeout> | null = null;
  /** A walk already waited out the connect timeout on this feed, and it has not connected since. */
  private feedUnavailable = false;
  /** The current feed was marked as holding the complete hub set. */
  private feedHoldsHubSet = false;
  /**
   * Agents deleted on earlier feeds: a later feed's walks, probes and live
   * events must not bring them back. A restore the feed reports clears one.
   * A restore while no feed is open goes unseen; a later walk or probe row,
   * or a status event, that shows the agent with no delete running clears
   * it (see releaseRestored and withoutCarried).
   */
  private readonly carriedTombstones = new Set<string>();
  /** Agent ids whose single-agent read is in flight on the current feed. */
  private readonly hydrating = new Set<string>();
  /** Agent ids waiting for a read slot on the current feed. */
  private hydrateQueue: string[] = [];
  /** Aborted when the current feed closes, cancelling its reads. */
  private hydrateAbort: AbortController | null = null;
  private userId = '';
  private readonly detachEvents: () => void;

  private readonly fetchPage: (path: string, options: ApiFetchOptions) => Promise<Response>;
  private readonly feedFactory: () => StateManager;
  private readonly now: () => number;
  private readonly currentUserId: () => string;
  private readonly view: 'full' | 'compact';
  private readonly pageSize: number;
  private readonly maxPages: number;
  private readonly pageTimeoutMs: number | undefined;
  private readonly connectTimeoutMs: number;
  private readonly evictionGraceMs: number;
  private readonly maxUnretained: number;
  private readonly feedIdleMs: number;
  private readonly visibility: VisibilitySource | null;
  private readonly random: () => number;
  private readonly probeFullWalkMs: number;

  constructor(options: AgentStoreOptions = {}) {
    this.fetchPage = options.fetch ?? defaultFetch;
    this.feedFactory = options.feedFactory ?? ((): StateManager => new StateManager());
    this.now = options.now ?? ((): number => Date.now());
    this.currentUserId = options.currentUserId ?? ((): string => stateManager.getCurrentUserId());
    this.view = options.view ?? 'full';
    this.pageSize = options.pageSize ?? AGENT_STORE_PAGE_SIZE;
    this.maxPages = options.maxPages ?? AGENT_STORE_MAX_PAGES;
    this.pageTimeoutMs = options.pageTimeoutMs;
    this.connectTimeoutMs = options.connectTimeoutMs ?? DEFAULT_CONNECT_TIMEOUT_MS;
    this.evictionGraceMs = options.evictionGraceMs ?? DEFAULT_EVICTION_GRACE_MS;
    this.maxUnretained = options.maxUnretained ?? DEFAULT_MAX_UNRETAINED;
    this.feedIdleMs = options.feedIdleMs ?? DEFAULT_FEED_IDLE_MS;
    this.visibility =
      options.visibility === undefined
        ? typeof document !== 'undefined'
          ? document
          : null
        : options.visibility;
    this.random = options.random ?? Math.random;
    this.probeFullWalkMs = options.probeFullWalkMs ?? AGENT_PROBE_FULL_WALK_MS;

    const events =
      options.events === undefined
        ? typeof window !== 'undefined'
          ? window
          : null
        : options.events;
    const detachTriggers = events ? this.listenForTriggers(events) : (): void => {};
    const detachVisibility = this.listenForVisibility();
    this.detachEvents = (): void => {
      detachTriggers();
      detachVisibility();
    };
  }

  // --- Public API ---

  /**
   * Load the list for `q`. Resolves with a ready snapshot, from memory when
   * the entry is fresh and the feed is live, otherwise after a walk (joining
   * one already in flight). Rejects with the walk's error, or with an
   * `AbortError` when `signal` aborts or the store is reset. Aborting
   * detaches this caller only. A caller that joins a background walk (one
   * waiting for the feed, say) is rejected with that walk's error even when
   * the walk fails quietly and the snapshot stays ready.
   */
  ensure(q: AgentQuery, opts: EnsureOptions = {}): Promise<AgentListSnapshot> {
    const { signal, onProgress } = opts;
    if (signal?.aborted) return Promise.reject(abortError('agent list load aborted'));

    this.checkUser();
    const entry = this.entryFor(q);
    this.touch(entry);
    const feed = this.ensureFeed();

    if (
      entry.status === 'ready' &&
      !entry.stale &&
      (!entry.walk || entry.walk.background) &&
      !entry.followUp &&
      feed.isConnected
    ) {
      this.scheduleIdleWork(entry);
      return Promise.resolve(entry.snapshot);
    }

    return new Promise<AgentListSnapshot>((resolve, reject) => {
      const onAbort = (): void => {
        if (!entry.waiters.delete(waiter)) return;
        waiter.cleanup();
        reject(abortError('agent list load aborted'));
        this.maybeAbortWalk(entry);
      };
      const waiter: Waiter = {
        resolve,
        reject,
        onProgress,
        cleanup: () => signal?.removeEventListener('abort', onAbort),
      };
      signal?.addEventListener('abort', onAbort);
      entry.waiters.add(waiter);

      if (entry.walk) {
        // A walk already fetching while the feed is down may miss changes
        // that land before the feed is back: walk once more after it.
        if (entry.walk.phase === 'fetching' && !feed.isConnected) entry.followUp = true;
        return;
      }
      this.startWalk(entry);
    });
  }

  /** The current snapshot for `q` without fetching; undefined if never loaded or evicted. */
  peek(q: AgentQuery): AgentListSnapshot | undefined {
    return this.entries.get(agentQueryKey(q))?.snapshot;
  }

  /**
   * Keep the entry for `q` live and not evictable, and hear every change to
   * it. Does not fetch by itself; a retained entry that becomes stale
   * revalidates in the background. Returns the release function.
   */
  retain(q: AgentQuery, listener: AgentListListener): () => void {
    this.checkUser();
    const entry = this.entryFor(q);
    this.touch(entry);
    entry.retainers.add(listener);
    this.ensureFeed();
    this.syncProbe(entry);
    let released = false;
    return () => {
      if (released) return;
      released = true;
      entry.retainers.delete(listener);
      // A user-change reset moves retainers to a fresh entry for the key.
      const current = this.entries.get(entry.key);
      if (!current || (current !== entry && !current.retainers.delete(listener))) return;
      current.lastUsed = this.now();
      this.maybeAbortWalk(current);
      this.syncProbe(current);
      this.scheduleIdleWork(current);
    };
  }

  /**
   * Mark entries stale. Retained entries that have loaded revalidate in the
   * background (one walk each, rows kept visible); others refetch on their
   * next `ensure`. A walk in flight is followed by exactly one more.
   */
  invalidate(_reason: InvalidateReason, match?: (key: string) => boolean): void {
    for (const entry of this.entries.values()) {
      if (match && !match(entry.key)) continue;
      entry.stale = true;
      if (entry.walk) {
        if (entry.walk.phase === 'fetching') entry.followUp = true;
        continue;
      }
      if (entry.retainers.size > 0 && entry.fetchedAt !== undefined) this.startWalk(entry);
    }
  }

  /**
   * Drop everything: abort walks, reject waiters with an `AbortError`,
   * discard the feed and clear entries. A fresh feed is created on next use,
   * so the feed's completeness flag is never cleared in place.
   *
   * With `keepRetainers`, retainers stay registered on fresh entries for
   * their keys, and those that had loaded walk again.
   */
  reset(reason: string, options: { keepRetainers?: boolean } = {}): void {
    const entries = Array.from(this.entries.values());
    this.entries.clear();
    const kept = options.keepRetainers
      ? entries
          .filter((e) => e.retainers.size > 0)
          .map((e) => ({
            query: e.query,
            retainers: Array.from(e.retainers),
            loaded: e.fetchedAt !== undefined,
          }))
      : [];
    for (const entry of entries) {
      if (entry.evictTimer) clearTimeout(entry.evictTimer);
      entry.evictTimer = null;
      this.stopProbe(entry);
      entry.walk?.controller.abort();
      entry.walk = null;
      const waiters = Array.from(entry.waiters);
      entry.waiters.clear();
      for (const w of waiters) {
        w.cleanup();
        w.reject(abortError(`agent store reset: ${reason}`));
      }
    }
    this.closeFeed();
    this.carriedTombstones.clear();
    if (kept.length === 0) return;
    const reload: Entry[] = [];
    for (const k of kept) {
      const entry = this.entryFor(k.query);
      for (const listener of k.retainers) entry.retainers.add(listener);
      if (k.loaded) reload.push(entry);
    }
    this.ensureFeed();
    for (const entry of reload) this.startWalk(entry);
  }

  /** Reset and stop listening for window-level triggers. */
  destroy(): void {
    this.reset('destroyed');
    this.detachEvents();
  }

  /**
   * Reset when the signed-in user differs from the one the lists were
   * loaded for. Sign-in changes normally reload the page; this is a guard.
   * Retainers stay registered and hear the new user's lists.
   */
  private checkUser(): void {
    const userId = this.currentUserId();
    if (!userId) return;
    const previous = this.userId;
    this.userId = userId;
    if (previous && previous !== userId) this.reset('user changed', { keepRetainers: true });
  }

  // --- Entries ---

  private entryFor(q: AgentQuery): Entry {
    const key = agentQueryKey(q);
    let entry = this.entries.get(key);
    if (!entry) {
      entry = {
        key,
        query: q,
        path: queryPath(q, this.view),
        agents: [],
        status: 'idle',
        complete: false,
        version: 0,
        snapshot: { key, agents: [], status: 'idle', complete: false, version: 0 },
        stale: false,
        followUp: false,
        retainers: new Set(),
        waiters: new Set(),
        walk: null,
        lastUsed: this.now(),
        evictTimer: null,
        probeTimer: null,
        probe: null,
        probeRefusalLogged: false,
        overflowBackoffMs: 0,
      };
      this.entries.set(key, entry);
    }
    return entry;
  }

  private touch(entry: Entry): void {
    entry.lastUsed = this.now();
    if (entry.evictTimer) {
      clearTimeout(entry.evictTimer);
      entry.evictTimer = null;
    }
  }

  private isIdle(entry: Entry): boolean {
    return entry.retainers.size === 0 && entry.waiters.size === 0 && !entry.walk;
  }

  /** Start the eviction grace for an unused entry, enforce the LRU bound, and manage the feed's idle close. */
  private scheduleIdleWork(entry: Entry): void {
    if (this.entries.get(entry.key) === entry && this.isIdle(entry) && !entry.evictTimer) {
      entry.evictTimer = setTimeout(() => {
        entry.evictTimer = null;
        if (this.entries.get(entry.key) === entry && this.isIdle(entry)) this.evict(entry);
      }, this.evictionGraceMs);
    }
    const idle = Array.from(this.entries.values()).filter((e) => this.isIdle(e));
    if (idle.length > this.maxUnretained) {
      idle.sort((a, b) => a.lastUsed - b.lastUsed);
      for (const e of idle.slice(0, idle.length - this.maxUnretained)) this.evict(e);
    }
    this.updateFeedIdle();
  }

  /**
   * Remove an entry. Rows no other entry holds leave the feed's map too;
   * the feed keeps its tombstones, so a later seed cannot bring back an
   * agent deleted earlier.
   */
  private evict(entry: Entry): void {
    if (entry.evictTimer) clearTimeout(entry.evictTimer);
    entry.evictTimer = null;
    this.entries.delete(entry.key);
    const feed = this.feed;
    if (!feed) return;
    // A feed marked complete for the hub must keep every hub row, or the
    // flag would claim rows the feed no longer holds. Every row it holds is
    // a hub row then, whichever entry is evicted. They go with the feed.
    if (this.feedHoldsHubSet) return;
    const heldElsewhere = new Set<string>();
    for (const other of this.entries.values()) {
      for (const a of other.agents) heldElsewhere.add(a.id);
    }
    for (const a of entry.agents) {
      if (!heldElsewhere.has(a.id)) feed.removeAgent(a.id);
    }
  }

  private publish(entry: Entry): void {
    entry.version++;
    const snapshot: AgentListSnapshot = Object.freeze({
      key: entry.key,
      agents: entry.agents,
      status: entry.status,
      complete: entry.complete,
      version: entry.version,
      ...(entry.error ? { error: entry.error } : {}),
      ...(entry.fetchedAt !== undefined ? { fetchedAt: entry.fetchedAt } : {}),
    });
    entry.snapshot = snapshot;
    for (const listener of Array.from(entry.retainers)) {
      try {
        listener(snapshot);
      } catch (err) {
        console.error('[agent-store] listener failed:', err);
      }
    }
    if (snapshot.status === 'loading') {
      for (const w of Array.from(entry.waiters)) {
        try {
          w.onProgress?.(snapshot);
        } catch (err) {
          console.error('[agent-store] progress callback failed:', err);
        }
      }
    }
  }

  // --- Walks ---

  /**
   * Walk the entry's list. A background walk publishes nothing until it
   * finishes, so readers keep the rows they have.
   */
  private startWalk(entry: Entry, options: { background?: boolean } = {}): void {
    const walk: Walk = {
      controller: new AbortController(),
      phase: 'connecting',
      sseAdded: new Set(),
      feedDropped: false,
      background: options.background === true,
    };
    // The walk reads everything a probe would. No caller starts a walk over
    // another; aborting one in flight only guards against that.
    entry.probe?.abort();
    entry.probe = null;
    entry.walk?.controller.abort();
    entry.walk = walk;
    entry.walkedAt = this.now();
    entry.followUp = false;
    this.updateFeedIdle();
    if (!walk.background) {
      entry.status = 'loading';
      entry.complete = false;
      this.publish(entry);
    }
    void this.runWalk(entry, walk);
  }

  /** Abort the entry's walk when nobody is waiting for it and nobody retains the entry. */
  private maybeAbortWalk(entry: Entry): void {
    const walk = entry.walk;
    if (!walk || entry.waiters.size > 0 || entry.retainers.size > 0) return;
    walk.controller.abort();
    entry.walk = null;
    entry.followUp = false;
    entry.stale = true;
    if (entry.fetchedAt === undefined) {
      entry.status = 'idle';
      entry.agents = [];
    } else {
      entry.status = entry.error ? 'error' : 'ready';
    }
    this.publish(entry);
    this.scheduleIdleWork(entry);
  }

  /**
   * Resolve once the feed is live, or false once the connect timeout passes
   * or the walk is aborted. After one timeout, later walks on the same feed
   * do not wait again until it connects.
   */
  private waitForFeed(feed: StateManager, signal: AbortSignal): Promise<boolean> {
    if (this.feedUnavailable && this.feed === feed) return Promise.resolve(false);
    return new Promise<boolean>((resolve) => {
      let settled = false;
      const finish = (connected: boolean): void => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        signal.removeEventListener('abort', onAbort);
        resolve(connected);
      };
      const onAbort = (): void => finish(false);
      const timer = setTimeout(() => {
        if (this.feed === feed) this.feedUnavailable = true;
        finish(false);
      }, this.connectTimeoutMs);
      signal.addEventListener('abort', onAbort);
      feed.sseConnected(feed.scopeGeneration).then(
        () => finish(true),
        () => finish(false)
      );
    });
  }

  private async runWalk(entry: Entry, walk: Walk): Promise<void> {
    const feed = this.ensureFeed();
    const signal = walk.controller.signal;
    const firstLoad = entry.fetchedAt === undefined;
    try {
      const waited = await this.waitForFeed(feed, signal);
      if (signal.aborted || entry.walk !== walk) return;
      walk.phase = 'fetching';

      let scopeCapabilities: Capabilities | undefined;
      let rows: Agent[];
      let truncated = false;
      const token = feed.beginSeedEpoch();
      try {
        try {
          rows = await paginateAll<Agent>({
            path: entry.path,
            pageSize: this.pageSize,
            maxPages: this.maxPages,
            label: 'agents list',
            signal,
            fetch: this.fetchPage,
            ...(this.pageTimeoutMs !== undefined ? { pageTimeoutMs: this.pageTimeoutMs } : {}),
            parsePage: (body) => {
              const page = body as {
                agents?: Agent[];
                nextCursor?: string;
                _capabilities?: Capabilities;
              };
              if (page._capabilities && !scopeCapabilities) scopeCapabilities = page._capabilities;
              return {
                items: Array.isArray(page.agents) ? page.agents : [],
                ...(page.nextCursor ? { nextCursor: page.nextCursor } : {}),
              };
            },
            onPage: (page, all) => {
              if (entry.walk !== walk || this.feed !== feed) return;
              this.releaseRestored(page);
              if (!firstLoad) return;
              const tombstones = this.tombstonesOf(feed);
              // Changes SSE delivered since the walk began win over its pages.
              const listed = dropTombstoned([...all], tombstones).map((a) =>
                feed.withSeedEpochDeltas(token, a)
              );
              const listedIds = new Set(listed.map((a) => a.id));
              // Keep rows SSE added during the walk that no page has listed yet.
              const added = entry.agents.filter(
                (a) => walk.sseAdded.has(a.id) && !listedIds.has(a.id) && !tombstones.has(a.id)
              );
              entry.agents = added.length > 0 ? [...listed, ...added] : listed;
              this.publish(entry);
            },
          });
        } catch (err) {
          if (!(err instanceof PaginationTruncatedError)) throw err;
          rows = err.items as Agent[];
          truncated = true;
        }
        if (signal.aborted || entry.walk !== walk || this.feed !== feed) return;
        if (this.carriedTombstones.size > 0) {
          rows = rows.filter((row) => !this.carriedTombstones.has(row.id));
        }
        // Only the hub list replaces feed rows. Other lists merge: the
        // project list, for one, omits fields the hub rows carry.
        feed.seedAgents(rows, { token, partial: this.view === 'compact' || entry.key !== 'hub' });
      } finally {
        feed.endSeedEpoch(token);
      }

      // Connected for the whole walk: events missed during a drop leave the
      // rows incomplete even after a reconnect.
      const connected = waited && feed.isConnected && !walk.feedDropped;
      const tombstones = this.tombstonesOf(feed);
      const byId = new Map<string, Agent>();
      for (const row of rows) {
        if (tombstones.has(row.id)) continue;
        byId.set(row.id, feed.getAgent(row.id) ?? row);
      }
      // Agents created over SSE after their page was read are not in the
      // REST rows but are in the feed; keep them.
      for (const id of walk.sseAdded) {
        if (byId.has(id) || tombstones.has(id)) continue;
        const agent = feed.getAgent(id);
        if (agent) byId.set(id, agent);
      }

      if (this.view === 'compact') {
        // Compact rows carry every field the list consumers read; the feed
        // only ever records the compact flag for them.
        if (entry.key === 'hub' && !truncated && connected) {
          feed.markAgentSetComplete('compact');
          this.feedHoldsHubSet = true;
        }
      } else if (entry.key === 'hub' && !truncated && connected) {
        feed.markAgentSetComplete('full');
        this.feedHoldsHubSet = true;
      }

      entry.walk = null;
      entry.agents = Array.from(byId.values());
      entry.highWater = newestMark(rows);
      entry.scopeCapabilities = scopeCapabilities;
      entry.status = 'ready';
      entry.complete = !truncated;
      entry.error = undefined;
      entry.fetchedAt = this.now();
      entry.stale = !connected;
      this.finishWalk(entry);
    } catch (err) {
      if (entry.walk !== walk) return;
      entry.walk = null;
      if (isAbortError(err) && signal.aborted) return;
      const error = err instanceof Error ? err : new Error(String(err));
      // A list marked stale (a resync, including one that lands while the
      // walk waits for the feed, or an earlier walk that ran disconnected or
      // failed) or a feed drop during the walk is behind on events: its
      // failure leaves the list behind, so it fails as any walk does.
      const missed = entry.followUp || entry.stale;
      entry.followUp = false;
      if (walk.background && !missed) {
        // The snapshot is as current as before the walk; the probe goes on.
        console.warn(`[agent-store] ${entry.key}: background walk failed:`, error);
      } else {
        if (firstLoad) entry.agents = [];
        entry.status = 'error';
        entry.error = error;
        entry.complete = false;
        entry.stale = true;
        this.publish(entry);
      }
      const waiters = Array.from(entry.waiters);
      entry.waiters.clear();
      for (const w of waiters) {
        w.cleanup();
        w.reject(error);
      }
      // The probe keeps a loaded list current after a failed walk, and its
      // next periodic walk retries.
      this.syncProbe(entry);
      this.scheduleIdleWork(entry);
    }
  }

  /** Publish a finished walk, then either walk once more or resolve the waiters. */
  private finishWalk(entry: Entry): void {
    if (entry.followUp && (entry.waiters.size > 0 || entry.retainers.size > 0)) {
      entry.stale = true;
      this.startWalk(entry);
      return;
    }
    entry.followUp = false;
    this.publish(entry);
    const waiters = Array.from(entry.waiters);
    entry.waiters.clear();
    for (const w of waiters) {
      w.cleanup();
      w.resolve(entry.snapshot);
    }
    this.syncProbe(entry);
    this.scheduleIdleWork(entry);
  }

  // --- Delta probe ---

  /**
   * Keep the entry's probe timer running exactly while it may probe: the
   * entry is current, retained, loaded and probeable, and the page is
   * visible. Otherwise stop it. A probe the page hid before it was due
   * keeps its due time, so switching tabs does not put it off; one that
   * fell due while hidden is scheduled afresh. After the server refuses the
   * list's sorted view, the next probe waits
   * {@link AGENT_PROBE_REFUSED_RETRY_MS}.
   */
  private syncProbe(entry: Entry): void {
    const probed =
      this.entries.get(entry.key) === entry &&
      entry.retainers.size > 0 &&
      entry.fetchedAt !== undefined &&
      isProbeable(entry.query);
    if (!probed || !this.isVisible()) {
      this.stopProbe(entry);
      return;
    }
    if (entry.probeTimer || entry.probe) return;
    const now = this.now();
    if (entry.probeDueAt === undefined || entry.probeDueAt <= now) {
      const jitter = (this.random() * 2 - 1) * AGENT_PROBE_JITTER_MS;
      entry.probeDueAt = now + AGENT_PROBE_INTERVAL_MS + jitter;
    }
    const refused = (entry.probeRetryAt ?? 0) - now;
    entry.probeTimer = setTimeout(
      () => {
        entry.probeTimer = null;
        // A timer may fire a little before a fractional due time; the next
        // probe is scheduled afresh either way.
        entry.probeDueAt = undefined;
        this.runProbeTick(entry);
      },
      Math.max(entry.probeDueAt - now, refused)
    );
  }

  private stopProbe(entry: Entry): void {
    if (entry.probeTimer) clearTimeout(entry.probeTimer);
    entry.probeTimer = null;
    entry.probe?.abort();
    entry.probe = null;
  }

  private isVisible(): boolean {
    return !this.visibility || this.visibility.visibilityState === 'visible';
  }

  private runProbeTick(entry: Entry): void {
    // A different signed-in user resets the store, which replaces this entry.
    this.checkUser();
    if (this.entries.get(entry.key) !== entry) return;
    const feed = this.feed;
    // A walk in flight reads everything a probe would; try on the next tick.
    if (!feed || entry.walk) {
      this.syncProbe(entry);
      return;
    }
    if (this.now() - (entry.walkedAt ?? 0) >= this.probeFullWalkMs) {
      this.startWalk(entry, { background: true });
      return;
    }
    const controller = new AbortController();
    entry.probe = controller;
    const timer = setTimeout(() => controller.abort(), AGENT_PROBE_TIMEOUT_MS);
    void this.probe(entry, feed, controller).finally(() => {
      clearTimeout(timer);
      if (entry.probe === controller) entry.probe = null;
      this.syncProbe(entry);
    });
  }

  /**
   * One delta probe: read the most recently active rows in the server's
   * order, and merge those the feed does not hold, or holds older and
   * different in a field heartbeats do not move, as a seeded
   * merge (deltas that land meanwhile win, deleted agents stay
   * deleted, the completeness flag is untouched). When the first page all
   * lists before the newest row the last probe saw, follow further pages;
   * when that does not catch up, or the server's count differs from the
   * rows held, walk once. Under sustained overflow, walks back off and
   * probes read one page.
   */
  private async probe(
    entry: Entry,
    feed: StateManager,
    controller: AbortController
  ): Promise<void> {
    const signal = controller.signal;
    const previous = entry.highWater;
    let highWater = previous;
    const backingOff =
      entry.overflowBackoffMs > 0 && this.now() - (entry.walkedAt ?? 0) < entry.overflowBackoffMs;
    const extraPages = backingOff ? 0 : AGENT_PROBE_MAX_EXTRA_PAGES;
    const token = feed.beginSeedEpoch();
    const changed: Agent[] = [];
    const listed = new Set(entry.agents.map((a) => a.id));
    let total: number | undefined;
    let caughtUp = false;
    let fresh: Agent[] = [];
    try {
      let cursor: string | undefined;
      for (let page = 0; page <= extraPages; page++) {
        const response = await this.fetchPage(probePath(entry.query, cursor), { signal });
        if (signal.aborted || entry.probe !== controller) return;
        if (!response.ok) {
          if (response.status === 422) this.probeRefused(entry);
          return;
        }
        const body = (await response.json()) as ProbePage;
        if (signal.aborted || entry.probe !== controller) return;
        const rows = Array.isArray(body.agents) ? body.agents : [];
        // The abort and identity check above already covers a feed swap
        // today (a swap starts a walk, which aborts this probe). The feed
        // check only guards a future path that swaps the feed without
        // aborting the probe.
        if (this.feed === feed) this.releaseRestored(rows);
        if (typeof body.totalCount === 'number') total = body.totalCount;
        for (const row of rows) {
          const held = feed.getAgent(row.id);
          const rowUpdated = updatedAt(row);
          const heldUpdated = updatedAt(held);
          if (
            !held ||
            !listed.has(row.id) ||
            (rowUpdated !== undefined &&
              (heldUpdated === undefined || rowUpdated > heldUpdated) &&
              differsBeyondHeartbeat(row, held))
          ) {
            changed.push(row);
          }
        }
        const newest = newestMark(rows);
        if (newest && (!highWater || compareMarks(newest, highWater) > 0)) highWater = newest;
        const last = probeMark(rows[rows.length - 1]);
        // With no mark yet (no row had a time), there is nothing to catch up
        // with: the count check finds agents the list lacks.
        caughtUp =
          !body.nextCursor ||
          previous === undefined ||
          (last !== undefined && compareMarks(last, previous) <= 0);
        if (caughtUp) break;
        cursor = body.nextCursor;
      }
      if (this.feed !== feed) return;
      fresh = changed.filter((row) => !this.carriedTombstones.has(row.id));
      feed.seedAgents(fresh, { token, partial: true });
      // Only a merged probe that caught up moves the mark. An interrupted
      // one reads the same pages again; one that did not catch up leaves
      // the mark to the walk it hands off to, so if that walk fails, later
      // probes still know they are behind.
      if (caughtUp) entry.highWater = highWater;
    } catch (err) {
      if (!isAbortError(err)) console.warn('[agent-store] agent probe failed:', err);
      return;
    } finally {
      feed.endSeedEpoch(token);
    }

    // Only the rows seeded. The server can still list an agent deleted on an
    // earlier feed while its delete completes, and this feed can hold it
    // from a `created` replayed after the delete.
    const upserted = Array.from(new Set(fresh.map((row) => row.id)));
    if (upserted.length > 0) {
      this.applyChange(
        feed,
        { upserted, deleted: [], unknown: new Map(), generation: feed.scopeGeneration },
        entry
      );
    }
    // A listener may have reset the store during the merge, or started a
    // walk; that walk reads everything this probe would have handed off,
    // and its finish sets the mark and the next probe.
    if (this.entries.get(entry.key) !== entry || entry.walk) return;
    if (!caughtUp) {
      // Churn faster than the probe reads would otherwise walk every time.
      if (backingOff) return;
      entry.overflowBackoffMs = entry.overflowBackoffMs
        ? 2 * entry.overflowBackoffMs
        : AGENT_PROBE_OVERFLOW_BACKOFF_MS;
      this.startWalk(entry, { background: true });
      return;
    }
    entry.overflowBackoffMs = 0;
    if (total === undefined || !entry.complete) return;
    if (total === entry.agents.length) {
      entry.countWalkTotal = undefined;
      return;
    }
    // Agents granted, revoked or deleted without an event. A count that
    // already made a walk does not make another until it changes.
    if (entry.countWalkTotal === total) return;
    entry.countWalkTotal = total;
    this.startWalk(entry, { background: true });
  }

  /**
   * The project list refuses its sorted view while its candidate count,
   * taken before read filtering, is above its ceiling; that can change
   * either way. Rely on resync walks for a while, then try again.
   */
  private probeRefused(entry: Entry): void {
    entry.probeRetryAt = this.now() + AGENT_PROBE_REFUSED_RETRY_MS;
    if (entry.probeRefusalLogged) return;
    entry.probeRefusalLogged = true;
    console.info(`[agent-store] ${entry.key}: sorted agent list unavailable; probing paused`);
  }

  private listenForVisibility(): () => void {
    const target = this.visibility;
    if (!target) return (): void => {};
    const onChange = (): void => {
      for (const entry of this.entries.values()) this.syncProbe(entry);
    };
    target.addEventListener('visibilitychange', onChange);
    return () => target.removeEventListener('visibilitychange', onChange);
  }

  // --- Feed ---

  private ensureFeed(): StateManager {
    if (this.feedIdleTimer) {
      clearTimeout(this.feedIdleTimer);
      this.feedIdleTimer = null;
    }
    if (this.feed) return this.feed;
    const feed = this.feedFactory();
    const onChanged = ((event: CustomEvent<{ data: AgentsChangedDetail }>) => {
      if (this.feed === feed) this.applyChange(feed, event.detail.data);
    }) as EventListener;
    // Agent ids are not reused: a restore is the one way a deleted agent
    // comes back. A listing row still deleting never counts as one (see
    // releaseRestored). The restore mark is subject to the replay limit
    // documented in state.ts.
    const onCreated = ((event: CustomEvent<{ data: { agentId: string; restored?: boolean } }>) => {
      if (this.feed === feed && event.detail.data.restored) {
        this.carriedTombstones.delete(event.detail.data.agentId);
      }
    }) as EventListener;
    const onResync = (): void => {
      if (this.feed === feed) this.invalidate('resync');
    };
    const onConnected = (): void => {
      if (this.feed === feed) this.feedUnavailable = false;
    };
    const onDisconnected = (): void => {
      if (this.feed !== feed) return;
      // A walk reading pages while the feed is down can miss events; walk
      // once more after it. Held entries revalidate on the reconnect's
      // resync, and `ensure` does not answer from memory meanwhile.
      for (const entry of this.entries.values()) {
        if (entry.walk?.phase !== 'fetching') continue;
        entry.followUp = true;
        entry.walk.feedDropped = true;
      }
    };
    feed.addEventListener('agent-created', onCreated);
    feed.addEventListener('agents-changed', onChanged);
    feed.addEventListener('agents-resync', onResync);
    feed.addEventListener('connected', onConnected);
    feed.addEventListener('disconnected', onDisconnected);
    this.detachFeed = (): void => {
      feed.removeEventListener('agent-created', onCreated);
      feed.removeEventListener('agents-changed', onChanged);
      feed.removeEventListener('agents-resync', onResync);
      feed.removeEventListener('connected', onConnected);
      feed.removeEventListener('disconnected', onDisconnected);
    };
    this.feed = feed;
    this.hydrateAbort = new AbortController();
    feed.setScope({ type: 'agent-feed' });
    return feed;
  }

  /**
   * Disconnect and discard the feed; every entry left is stale. Its
   * tombstones carry over to the next feed.
   */
  private closeFeed(): void {
    if (this.feedIdleTimer) {
      clearTimeout(this.feedIdleTimer);
      this.feedIdleTimer = null;
    }
    const feed = this.feed;
    if (!feed) return;
    this.detachFeed?.();
    this.detachFeed = null;
    this.feed = null;
    this.feedUnavailable = false;
    this.feedHoldsHubSet = false;
    this.hydrateAbort?.abort();
    this.hydrateAbort = null;
    this.hydrating.clear();
    this.hydrateQueue = [];
    for (const id of feed.getDeletedAgentIds()) this.carriedTombstones.add(id);
    feed.disconnect();
    for (const entry of this.entries.values()) entry.stale = true;
  }

  /**
   * Replace the feed with a fresh connection, so the server expands the
   * subscription again for the session's current access. Walks in flight
   * restart on the new feed and keep their waiters.
   */
  private replaceFeed(): void {
    if (!this.feed) return;
    this.closeFeed();
    this.ensureFeed();
    for (const entry of this.entries.values()) {
      if (!entry.walk) continue;
      entry.walk.controller.abort();
      entry.walk = null;
      this.startWalk(entry);
    }
    this.updateFeedIdle();
  }

  /** Close the feed once nothing retains an entry and no walk runs; keep it open otherwise. */
  private updateFeedIdle(): void {
    if (!this.feed) return;
    const busy = Array.from(this.entries.values()).some(
      (e) => e.retainers.size > 0 || e.walk !== null || e.waiters.size > 0
    );
    if (busy) {
      if (this.feedIdleTimer) {
        clearTimeout(this.feedIdleTimer);
        this.feedIdleTimer = null;
      }
      return;
    }
    if (this.feedIdleTimer) return;
    this.feedIdleTimer = setTimeout(() => {
      this.feedIdleTimer = null;
      this.closeFeed();
    }, this.feedIdleMs);
  }

  /**
   * Forget the tombstones that earlier feeds carried for agents these listing
   * rows (walk or probe pages) show restored (see
   * {@link listedWithoutActiveDelete}). The restore event went out while no
   * feed was open. Every walk and probe on the current feed starts after those
   * tombstones were carried, so its rows were read after the deletes were
   * seen. A hard delete is published before its row is removed, so a row still
   * deleting keeps the tombstone. The current feed's own tombstones are left
   * alone: a page read before a delete on this feed can still list the agent.
   */
  private releaseRestored(rows: readonly Agent[]): void {
    if (this.carriedTombstones.size === 0) return;
    for (const row of rows) {
      if (this.carriedTombstones.has(row.id) && listedWithoutActiveDelete(row)) {
        this.carriedTombstones.delete(row.id);
      }
    }
  }

  /** Tombstones of the current feed and of the feeds before it. */
  private tombstonesOf(feed: StateManager): Set<string> {
    const own = feed.getDeletedAgentIds();
    if (this.carriedTombstones.size === 0) return own;
    return new Set([...this.carriedTombstones, ...own]);
  }

  /**
   * Hold back upserts for agents an earlier feed saw deleted. The current
   * feed has not seen those deletes, so a live event can still add such an
   * agent to it. An upsert releases the carried tombstone when the feed's
   * row shows the agent with no delete running (see
   * {@link listedWithoutActiveDelete}); only a status event sets that view
   * live, since a created event carries none. Otherwise the upsert is
   * dropped and the tombstone kept. A restore event releases the tombstone
   * before this runs. The feed's own tombstones are not involved: the feed
   * never reports an upsert for an agent it saw deleted.
   */
  private withoutCarried(feed: StateManager, change: AgentsChangedDetail): AgentsChangedDetail {
    if (this.carriedTombstones.size === 0 || change.upserted.length === 0) return change;
    let kept: string[] | null = null;
    change.upserted.forEach((id, i) => {
      if (this.carriedTombstones.has(id)) {
        const agent = feed.getAgent(id);
        if (!agent || !listedWithoutActiveDelete(agent)) {
          kept ??= change.upserted.slice(0, i);
          return;
        }
        this.carriedTombstones.delete(id);
      }
      kept?.push(id);
    });
    return kept ? { ...change, upserted: kept } : change;
  }

  /**
   * Apply one coalesced feed flush to every entry, notifying each changed
   * entry once. With `only`, the rows came from that entry's own read: only
   * it adds rows it lacks, and the others update rows they already hold.
   */
  private applyChange(feed: StateManager, rawChange: AgentsChangedDetail, only?: Entry): void {
    const change = this.withoutCarried(feed, rawChange);
    const added = new Set<string>();
    for (const entry of this.entries.values()) {
      // An entry that never loaded and is not loading holds no rows to keep
      // current; its first walk reads them.
      if (entry.fetchedAt === undefined && !entry.walk) continue;
      const held = entry.agents;
      const next = mergeChanged(held, change, {
        getAgent: (id) => feed.getAgent(id),
        shouldAdd: only && only !== entry ? (): boolean => false : shouldAddFor(entry.query),
        scopeCapabilities: entry.scopeCapabilities,
      });
      if (next === held) continue;
      const heldIds = new Set(held.map((a) => a.id));
      for (const a of next) {
        if (heldIds.has(a.id)) continue;
        added.add(a.id);
        entry.walk?.sseAdded.add(a.id);
      }
      entry.agents = next;
      this.publish(entry);
    }
    const missing = Array.from(added).filter(
      (id) =>
        !feed.getAgent(id)?._capabilities &&
        !this.hydrating.has(id) &&
        !this.hydrateQueue.includes(id)
    );
    if (missing.length > 0) this.queueHydrate(feed, missing);
  }

  /**
   * Queue single-agent reads, at most {@link AGENT_READ_CONCURRENCY} at once.
   * Past {@link AGENT_READ_BURST_LIMIT} waiting, drop the queue and revalidate
   * once instead: the walk reads every new agent with its capabilities.
   */
  private queueHydrate(feed: StateManager, ids: string[]): void {
    this.hydrateQueue.push(...ids);
    if (this.hydrating.size + this.hydrateQueue.length > AGENT_READ_BURST_LIMIT) {
      this.hydrateQueue = [];
      this.invalidate('agents-created');
      return;
    }
    this.pumpHydrate(feed);
  }

  private pumpHydrate(feed: StateManager): void {
    while (this.hydrating.size < AGENT_READ_CONCURRENCY && this.hydrateQueue.length > 0) {
      void this.hydrate(feed, this.hydrateQueue.shift()!);
    }
  }

  /**
   * Read one agent an SSE event added without its per-agent capabilities
   * and messageability (a `created` event carries neither), seed it, and
   * apply it to the entries. Until then the row holds only the list's
   * scope capabilities. A failure leaves the row; the next walk refills it.
   * The read is cancelled when the feed closes and abandoned after
   * {@link AGENT_READ_TIMEOUT_MS}. The single-agent endpoint has no compact
   * form, so in compact view the feed holds this one row in full (a
   * superset of the compact fields).
   */
  private async hydrate(feed: StateManager, id: string): Promise<void> {
    const feedSignal = this.hydrateAbort?.signal;
    if (!feedSignal || feedSignal.aborted) return;
    this.hydrating.add(id);
    const controller = new AbortController();
    const onFeedClosed = (): void => controller.abort();
    feedSignal.addEventListener('abort', onFeedClosed);
    const timer = setTimeout(() => controller.abort(), AGENT_READ_TIMEOUT_MS);
    const token = feed.beginSeedEpoch();
    let seeded = false;
    try {
      const response = await this.fetchPage(`/api/v1/agents/${encodeURIComponent(id)}`, {
        signal: controller.signal,
      });
      if (!response.ok) return;
      const row = (await response.json()) as Agent | null;
      if (this.feed !== feed || row?.id !== id) return;
      // The feed skips an agent deleted while the read was in flight.
      feed.seedAgents([row], { token });
      seeded = true;
    } catch (err) {
      if (!isAbortError(err)) console.warn('[agent-store] agent read failed:', err);
    } finally {
      clearTimeout(timer);
      feedSignal.removeEventListener('abort', onFeedClosed);
      feed.endSeedEpoch(token);
      if (this.feed === feed) {
        this.hydrating.delete(id);
        this.pumpHydrate(feed);
      }
    }
    if (!seeded) return;
    this.applyChange(feed, {
      upserted: [id],
      deleted: [],
      unknown: new Map(),
      generation: feed.scopeGeneration,
    });
  }

  // --- Window triggers ---

  private listenForTriggers(events: EventTarget): () => void {
    const onMembership = (): void => this.accessChanged('membership');
    const onAccessDenied = (event: Event): void => {
      const detail = (event as CustomEvent<AccessDeniedDetail | undefined>).detail;
      // Only a denied read or list of an agent or project says what this
      // session can see has changed. Denied actions and denials that name
      // no resource say nothing about visibility.
      if (!detail?.resource || !VISIBILITY_RESOURCES.has(detail.resource)) return;
      if (!detail.action || !VISIBILITY_ACTIONS.has(detail.action)) return;
      this.accessChanged('permission');
    };
    const onTeardown = (): void => this.reset('account teardown');
    const onPageHide = (event: Event): void => {
      // A page kept in the back/forward cache comes back with its retainers.
      if ((event as PageTransitionEvent).persisted) return;
      this.reset('page unload');
    };
    events.addEventListener(MEMBERSHIP_CHANGED_EVENT, onMembership);
    events.addEventListener('scion:access-denied', onAccessDenied);
    events.addEventListener(ACCOUNT_TEARDOWN_EVENT, onTeardown);
    events.addEventListener('pagehide', onPageHide);
    return () => {
      events.removeEventListener(MEMBERSHIP_CHANGED_EVENT, onMembership);
      events.removeEventListener('scion:access-denied', onAccessDenied);
      events.removeEventListener(ACCOUNT_TEARDOWN_EVENT, onTeardown);
      events.removeEventListener('pagehide', onPageHide);
    };
  }

  /** The session's access may have changed: reconnect the feed and revalidate every entry. */
  private accessChanged(reason: InvalidateReason): void {
    this.replaceFeed();
    this.invalidate(reason);
  }
}

/** The app-wide store. */
export const agentStore = new AgentStore();
