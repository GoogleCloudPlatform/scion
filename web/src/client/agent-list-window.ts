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
 * Agent list window: the state machine behind a paginated agent list
 * (project page grid, list and tree views).
 *
 * States:
 * - `small`: the host holds the complete set H (a fit response with
 *   `complete: true`, or a one-page legacy load). Everything is local.
 * - `paged`: server-sorted pages of a sorted-eligible view state, with a
 *   member index for live counts.
 * - `held`: the complete set H from a multi-page drain. Local, like small,
 *   and kept for the page lifetime until a refresh trigger.
 * - `capped`: an incomplete H from a drain that hit its request cap, or
 *   whose later page failed. Local, like held, but flagged incomplete.
 *
 * The window never issues a first request itself: the host asks
 * {@link AgentListWindow.planRequest} which request a trigger needs, sends
 * it, and reports the outcome with `setSmall`, `setPaged` or `adoptDrain`.
 * Only paged navigation (`next`, `prev`, `refresh`) fetches, through the
 * host's `fetchPage`.
 *
 * The local states never copy H: they read it fresh, by reference, from
 * `getHeldAgents()` on every access, so whatever the host's live-update
 * path last assigned is visible at once, with no re-adoption step and no
 * `pageIndex` reset. `pageIndex` resets to 0 on a `setViewState` call in a
 * local state, and on any move out of `paged` (a different data set).
 */

import type { Agent, AgentPhase } from '../shared/types.js';
import { isAgentRunning } from '../shared/types.js';
import type { AgentSortField, SortDir } from '../shared/agent-sort.js';
import { sortAgents, serverOrderCompare, serverSortKey } from '../shared/agent-sort.js';
import type { AgentsChangedDetail, UnknownAgentDelta } from './state.js';
import { AgentMemberIndex } from './agent-member-index.js';
import { formatNumber } from '../utils/format-number.js';

/** See the module comment for what each state means. */
export type WindowState = 'small' | 'paged' | 'held' | 'capped';

/**
 * A refresh trigger: page load, a label commit, a lifecycle-action or
 * stop-all refresh, a view-state change (sort, phase, page size, view), or
 * a click on the stale chip or banner.
 */
export type AgentListTrigger =
  | 'page-load'
  | 'label-commit'
  | 'lifecycle-refresh'
  | 'view-change'
  | 'chip';

/**
 * The one request a trigger needs:
 * - `fit`: a sorted first request (`sort`, `dir`, `limit`, `fit`, `stats=1`,
 *   filters);
 * - `drain`: a complete-set drain whose first page is the legacy request;
 * - `page`: re-fetch the current paged page (`refresh()`);
 * - `none`: no request at all.
 */
export type AgentListRequestPlan = 'fit' | 'drain' | 'page' | 'none';

/** The layout an agent list renders in. Tree always needs the complete set. */
export type AgentListView = 'grid' | 'list' | 'tree';

/**
 * Text of the capped total: "X loaded (newest 2,000 checked), more exist".
 * X is the number of loaded (readable) agents, which can be far below
 * 2,000 when the server filters candidates by read access.
 */
export function cappedTotalText(loaded: number): string {
  return `${formatNumber(loaded)} loaded ${CAPPED_SUFFIX}`;
}

/** What a capped drain covered: the newest drain-cap rows, with more beyond them. */
const CAPPED_SUFFIX = '(newest 2,000 checked), more exist';

/** Text of a failed drain's banner: "Incomplete: loaded X". */
export function failedTotalText(loaded: number): string {
  return `Incomplete: loaded ${formatNumber(loaded)}`;
}

/**
 * The note a renderer shows after an "Agents" (`total`) or "Running"
 * (`running`) count of an incomplete set, worded like the banner for the
 * same reason: a capped drain checked the newest 2,000 and more exist; a
 * failed drain stopped early, so how much it checked is unknown.
 */
export function incompleteStatNote(reason: 'capped' | 'failed', kind: 'total' | 'running'): string {
  const loaded = kind === 'total' ? 'loaded' : 'among loaded';
  return reason === 'capped' ? `${loaded} ${CAPPED_SUFFIX}` : `${loaded}, incomplete`;
}

/**
 * Candidate count at or below which the project page's first sorted request
 * asks for the complete set (the small state). Above it the window pages
 * from the first request, so a mid-size project no longer pays for a full
 * read and capability pass on every load. The server accepts any fit from 1
 * to 500 that is at least the page size.
 */
export const PROJECT_AGENTS_FIT_THRESHOLD = 50;

/**
 * The fit value for a first sorted request at the given page size: the
 * threshold, raised to the page size when the page is larger, because the
 * server rejects a fit below the limit.
 */
export function projectAgentsFitFor(pageSize: number): number {
  return Math.max(PROJECT_AGENTS_FIT_THRESHOLD, pageSize);
}

/** The client view state a window renders. A change resets local pagination to page 0. */
export interface AgentListViewState {
  phaseFilter: AgentPhase | '';
  /** The live-typed label filter preview (small-state local filtering only — never the request label; see `AgentListWindow`'s own `committedLabel`). */
  label: string;
  sortField: AgentSortField;
  sortDir: SortDir;
  pageSize: number;
  view: AgentListView;
  /**
   * The global page's client-side mode filter: a message mode, or
   * `can_message` / `cannot_message`. Empty or omitted means none. A mode
   * filter is never sent to the server, so a non-empty one makes the view
   * state complete-needing.
   */
  modeFilter?: string;
}

/** Parameters of one paged-navigation request, passed to the host's `fetchPage`. */
export interface PagedPageParams {
  cursor?: string | undefined;
  /**
   * The ids of the requested page, in the walk order frozen from page 0
   * (sent as `ids=`; never together with `cursor`). See
   * {@link AgentListWindow} on the frozen walk.
   */
  ids?: readonly string[] | undefined;
  limit: number;
  /** `stats=1`: only on page 0, a view-state change, or a refresh. */
  wantStats: boolean;
  /**
   * Aborted when the window no longer wants this page: a newer page
   * request, a state change, {@link AgentListWindow.cancelPageFetch} (a
   * superseding first request, a scope switch, a disconnect). The host
   * passes it to `fetch`.
   */
  signal: AbortSignal;
}

/**
 * A sorted first request in flight, from {@link AgentListWindow.beginSortedRequest}.
 * `key` is the server-parameter key the request was built from; pass it to
 * `setPaged` so the adopted page is recorded under the parameters it was
 * really fetched with.
 */
export interface SortedRequestTicket {
  readonly trigger: AgentListTrigger;
  readonly key: string;
  readonly label: string;
  readonly eligible: boolean;
}

/** A sorted page response, as the host hands it to the window. */
export interface PagedPageResult {
  agents: Agent[];
  nextCursor?: string | undefined;
  totalCount: number;
  stats?: { total: number; running: number; agents?: Array<[string, string]> } | undefined;
  /**
   * IDs upserted live (creates included) while this request was in flight,
   * already applied to the state store. After adopting the response the
   * window replays them as an `agents-changed` flush would: an on-page row
   * is replaced and re-sorted, and an off-page create under the add rule
   * joins the member index (with the chip if it could land on the page).
   * Without it, a create the response predates would be lost from the
   * freshly seeded member index.
   */
  liveChanged?: readonly string[] | undefined;
  /**
   * The merged live delta of each ID that changed while this request was in
   * flight but was not in the state store (see `AgentSeedEpoch.unknownChanges`).
   * The window replays it as the `unknown` part of an `agents-changed`
   * flush: an off-page member's phase follows it, and an activity time that
   * could land on the page (or any change to a count-only snapshot) shows
   * the chip.
   */
  liveUnknown?: ReadonlyMap<string, UnknownAgentDelta> | undefined;
  /**
   * IDs deleted live while this request was in flight (see
   * `AgentSeedEpoch.deletedChanges`). The window replays them as the
   * `deleted` part of an `agents-changed` flush: each leaves the member
   * index, and a count-only snapshot shows the chip.
   */
  liveDeleted?: readonly string[] | undefined;
  /**
   * How many of the server's rows were left out of `agents` because the
   * agent was deleted live and the store no longer holds it (see
   * `AgentSeedResult.dropped`). The page is short by at least that many
   * rows, so the window shows the backfill chip, as it does for a live
   * on-page delete.
   */
  droppedRows?: number | undefined;
  /**
   * The live connection resynced while this request was in flight (see
   * `AgentSeedEpoch.sawResync`): the response may predate changes the
   * connection missed, so the window shows the chip after adopting it.
   */
  liveResync?: boolean | undefined;
}

/** Fetches one sorted page for paged navigation. Rejecting sets the window's `error`. */
export type PagedPageFetcher = (params: PagedPageParams) => Promise<PagedPageResult>;

/** Construction options for {@link AgentListWindow}. */
export interface AgentListWindowOptions {
  viewState: AgentListViewState;
  /** The project this window belongs to, used by the paged-state off-page add rule. Read lazily — `project-detail.ts` constructs the window before `this.projectId` is finalized from the URL in `connectedCallback`. */
  getProjectId: () => string;
  fetchPage: PagedPageFetcher;
  /** Full `Agent` lookup for an upserted ID (the on-page replace). Typically `stateManager.getAgent`. */
  getAgent: (id: string) => Agent | undefined;
  /**
   * Returns the host's current held agent array H (`this.agents`) on
   * every call. The window never copies it — reading it
   * fresh is what lets an unrelated SSE-driven reassignment of `this.agents`
   * show up in the small-state list with no re-adoption step.
   */
  getHeldAgents: () => Agent[];
  /**
   * The paged state's add rule for an agent that is neither on the page
   * nor a member yet, in place of the project match (`getProjectId`). The
   * committed `k=v` label is still applied after it. The global page passes
   * "the page was loaded for scope `all`".
   */
  isAddable?: (agent: Agent) => boolean;
}

/**
 * The window controller. Dispatches a plain `change` event whenever
 * anything a renderer reads may have changed. Guarantees:
 * - never sends a request from `setViewState`, `applyChanges`,
 *   `applyLocalUpdate`, `markResync` or any state setter;
 * - discards a stale paged response (generation counter);
 * - never reports an incomplete set as complete: `capped` is left only by
 *   a new first request.
 */
export class AgentListWindow extends EventTarget {
  /** Live membership counts for the paged state. */
  readonly memberIndex = new AgentMemberIndex();

  private _state: WindowState = 'small';
  private pageItems: Agent[] = [];
  private cursors: Array<string | undefined> = [undefined];
  /** `pageOffsets[i]` = rows before page `i`. Pages can be short (rows dropped by a race with a live delete), so this is tracked as pages are actually fetched, not assumed to be `pageIndex * pageSize`. */
  private pageOffsets: number[] = [0];
  private _pageIndex = 0;
  private _totalCount = 0;
  private _hasNext = false;
  /**
   * The walk order frozen from page 0's stats population (ptone/scion#3744):
   * the readable ids in sort order at that moment, phase filter applied.
   * Later pages are fetched as slices of it by id, so an agent whose sort
   * key changes mid-walk (a heartbeat, an activity event) is neither
   * skipped nor shown twice, as it could be by a cursor over that key.
   * Agents created after page 0 surface through the updates chip, and an
   * agent deleted or no longer readable simply drops out of its page.
   * `null` when the response carried no population (the global endpoint
   * above 2,000 agents) or its head did not match page 0's rows: the walk
   * then follows server cursors, as before, which can still skip or repeat
   * an agent whose sort key changes mid-walk.
   */
  private frozenOrder: string[] | null = null;
  /**
   * Where each page of the frozen walk starts in `frozenOrder`. Page i+1
   * starts right after the ids page i asked for, so an id that drops out
   * of a page (deleted, no longer readable, out of the phase filter) never
   * shifts a later id onto a page already shown.
   */
  private frozenStarts: number[] = [0];
  /** Ids of the frozen order the server no longer returns (they leave the pager total). */
  private frozenDead = new Set<string>();
  /** Cleared by `invalidateCursors()`; restored by the next `setPaged()`. */
  private _cursorsValid = true;
  private _loading = false;
  private _error: string | null = null;
  private _updatesAvailable = false;
  private _stale = false;
  private incomplete: 'capped' | 'failed' | null = null;
  private generation = 0;
  /** The committed label for which the server refused sorted mode (422), or `null`. */
  private refusedLabel: string | null = null;
  /** The server parameters (sort, dir, phase, page size) the adopted paged response was requested with. */
  private pagedParams = '';
  /** The sorted first request in flight, if any (see `beginSortedRequest`). */
  private inFlight: SortedRequestTicket | null = null;
  /** Aborts the window's own page fetch in flight, if any. */
  private pageController: AbortController | null = null;

  /** The label the current paged response was fetched under (the paged add rule). Empty or `k=v` only: a bare-key label is never paged. */
  private committedLabel = '';

  private viewState: AgentListViewState;
  private readonly getProjectId: () => string;
  private readonly fetchPage: PagedPageFetcher;
  private readonly getAgent: (id: string) => Agent | undefined;
  private readonly getHeldAgents: () => Agent[];
  private readonly isAddable: ((agent: Agent) => boolean) | undefined;

  constructor(options: AgentListWindowOptions) {
    super();
    this.viewState = options.viewState;
    this.getProjectId = options.getProjectId;
    this.fetchPage = options.fetchPage;
    this.getAgent = options.getAgent;
    this.getHeldAgents = options.getHeldAgents;
    this.isAddable = options.isAddable;
  }

  /** The current state (see the module comment). */
  get state(): WindowState {
    return this._state;
  }

  /** True in `small`, `held` and `capped`: rows come from H, and every view-state change is local. */
  get isLocal(): boolean {
    return this._state !== 'paged';
  }

  /**
   * Whether a view state may use sorted (server-paged) requests: grid or
   * list, `updated` or `created` sort, no mode filter, and a committed
   * label that is empty or contains `=`. Everything else needs the
   * complete set.
   */
  isSortedEligible(committedLabel: string): boolean {
    const { view, sortField, modeFilter } = this.viewState;
    if (view !== 'grid' && view !== 'list') return false;
    if (modeFilter) return false;
    if (sortField !== 'updated' && sortField !== 'created') return false;
    const label = committedLabel.trim();
    return label === '' || label.includes('=');
  }

  /** Whether the server refused sorted mode (422) for this committed label. */
  isSortedRefused(committedLabel: string): boolean {
    return this.refusedLabel !== null && this.refusedLabel === committedLabel.trim();
  }

  /**
   * Remember a 422 refusal of sorted mode for this committed label. No
   * sorted request is planned again until a different label is committed;
   * that label gets one sorted attempt of its own.
   */
  recordRefusal(committedLabel: string): void {
    this.refusedLabel = committedLabel.trim();
  }

  /**
   * The request a trigger needs, given the current state, view state and
   * committed label. Table (eligible = sorted-eligible and not refused for
   * this label):
   *
   * | trigger              | small  | paged            | held   | capped           |
   * |----------------------|--------|------------------|--------|------------------|
   * | page-load, label     | eligible ? fit : drain (every state)                  |
   * | lifecycle-refresh    | eligible ? fit : drain   | none   | none             |
   * | view-change          | none   | eligible ? fit* : drain | none | eligible ? fit : none |
   * | chip                 | eligible ? fit : drain   | page   | drain  | drain            |
   *
   * (*) While paged, an eligible view change sends a fit request only when
   * a server parameter (sort, dir, phase or page size) differs from the
   * adopted page's; a grid and list switch alone sends nothing, since both
   * render the same server page.
   *
   * A lifecycle refresh in held or capped sends nothing: the phase changes
   * arrive live and merge into H. A view change in capped back to a
   * sorted-eligible state returns to paged, unless sorted mode was refused
   * for this label, in which case H stays capped and is sorted locally.
   */
  planRequest(trigger: AgentListTrigger, committedLabel: string): AgentListRequestPlan {
    const eligible = this.isSortedEligible(committedLabel) && !this.isSortedRefused(committedLabel);
    const first: AgentListRequestPlan = eligible ? 'fit' : 'drain';
    const state = this._state;
    switch (trigger) {
      case 'page-load':
      case 'label-commit':
        return first;
      case 'lifecycle-refresh':
        return state === 'held' || state === 'capped' ? 'none' : first;
      case 'view-change':
        if (state === 'paged') {
          if (!eligible) return 'drain';
          return this.serverParamsKey() === this.pagedParams ? 'none' : 'fit';
        }
        if (state === 'capped') return eligible ? 'fit' : 'none';
        return 'none';
      case 'chip':
        if (state === 'paged') return 'page';
        if (state === 'held' || state === 'capped') return 'drain';
        return first;
    }
  }

  /** The view-state fields a sorted request depends on. The view (grid or list) is not one of them. */
  private serverParamsKey(): string {
    const { sortField, sortDir, phaseFilter, pageSize } = this.viewState;
    return `${sortField}|${sortDir}|${phaseFilter}|${pageSize}`;
  }

  /**
   * Record that the host is sending a sorted first request (`fit`) for the
   * current view state and committed label. Cancels the window's own page
   * fetch, whose result the new response replaces. Returns the ticket to
   * pass to `setPaged` (its `key`) and `endSortedRequest`.
   */
  beginSortedRequest(trigger: AgentListTrigger, committedLabel: string): SortedRequestTicket {
    this.cancelPageFetch();
    const ticket: SortedRequestTicket = {
      trigger,
      key: this.serverParamsKey(),
      label: committedLabel.trim(),
      eligible: this.isSortedEligible(committedLabel),
    };
    this.inFlight = ticket;
    return ticket;
  }

  /** The sorted request of `ticket` has finished (adopted, failed or superseded). Pass no ticket to forget any. */
  endSortedRequest(ticket?: SortedRequestTicket): void {
    if (!ticket || this.inFlight === ticket) this.inFlight = null;
  }

  /**
   * After a view-state change: the trigger of the sorted request in flight
   * when its response no longer fits the current view state (a different
   * sort, dir, phase or page size, a different committed label, or a view
   * state that now needs the complete set), else `null`. The host then
   * supersedes that request: it aborts it, calls `endSortedRequest()`, and
   * plans again (re-sending a page-load or label-commit request, or
   * planning the view change). A grid and list switch alone never
   * supersedes, since both render the same response.
   */
  supersededRequest(committedLabel: string): AgentListTrigger | null {
    const t = this.inFlight;
    if (!t) return null;
    const same =
      t.key === this.serverParamsKey() &&
      t.label === committedLabel.trim() &&
      t.eligible === this.isSortedEligible(committedLabel);
    return same ? null : t.trigger;
  }

  /**
   * Abort the window's own page fetch in flight (next, prev or refresh), if
   * any, and drop its result. The host calls it on a scope switch and on
   * disconnect; `beginSortedRequest` and every state change call it too.
   */
  cancelPageFetch(): void {
    if (!this.pageController) return;
    this.pageController.abort();
    this.pageController = null;
    this.generation++;
    if (this._loading) {
      this._loading = false;
      this.notifyChange();
    }
  }

  /** 0-based index of the shown page. */
  get pageIndex(): number {
    return this._pageIndex;
  }

  /** A paged navigation request is in flight. */
  get loading(): boolean {
    return this._loading;
  }

  /** The last paged navigation error, cleared by the next request or state change. */
  get error(): string | null {
    return this._error;
  }

  /** The paged-state "may have changed - Refresh" chip. Always false in the local states. */
  get updatesAvailable(): boolean {
    return this._updatesAvailable;
  }

  /**
   * The local states' "may be stale - Refresh" signal: set by a reconnect
   * (`markResync`) or by adopting a drain that may have missed live
   * changes; cleared by the next adopted first response.
   */
  get stale(): boolean {
    return this._stale;
  }

  /**
   * Why H is incomplete in the `capped` state: `capped` (the drain reached
   * its request cap) or `failed` (a later page failed after retries).
   * `null` in every other state.
   */
  get incompleteReason(): 'capped' | 'failed' | null {
    return this._state === 'capped' ? this.incomplete : null;
  }

  /**
   * The banner a renderer shows above the rows, or `null`:
   * - capped: "X loaded (newest 2,000 checked), more exist"
   * - failed: "Incomplete: loaded X"
   * - stale (local states): "may be stale"
   * Clicking it is the `chip` trigger.
   */
  get banner(): { kind: 'capped' | 'failed' | 'stale'; text: string } | null {
    const reason = this.incompleteReason;
    if (reason === 'capped') {
      return { kind: 'capped', text: cappedTotalText(this.getHeldAgents().length) };
    }
    if (reason === 'failed') {
      return { kind: 'failed', text: failedTotalText(this.getHeldAgents().length) };
    }
    if (this._stale && this.isLocal) return { kind: 'stale', text: 'may be stale' };
    return null;
  }

  /** Whether Prev may be used (false while a stale cursor stack is invalidated). */
  get hasPrev(): boolean {
    if (this._state === 'paged' && !this._cursorsValid) return false;
    return this._pageIndex > 0;
  }

  /** Whether Next may be used. */
  get hasNext(): boolean {
    if (this._state === 'paged') return this._cursorsValid && this._hasNext;
    return (this._pageIndex + 1) * this.viewState.pageSize < this.display.length;
  }

  /**
   * Adopt the host's current H as the complete set (`small`): a fit
   * response with `complete: true`, or a legacy page with no `nextCursor`.
   * The host must already have assigned the array `getHeldAgents()`
   * returns. Resets `pageIndex` to 0 when leaving `paged`; clears the
   * incomplete flag, and sets the stale flag to `stale` (a live-connection
   * resync while the response was in flight).
   */
  setSmall(stale = false): void {
    this.enterLocal('small', null, stale);
  }

  /**
   * Adopt a drain outcome. The host must already have assigned the drained
   * membership as H. Moves to:
   * - `small` for a complete drain of one request (the legacy page had no
   *   `nextCursor`);
   * - `held` for a complete drain of more requests;
   * - `capped` (reason `capped`) when the request cap was reached;
   * - `capped` (reason `failed`) when a page failed after retries.
   * `stale` raises the stale banner. Never marks an incomplete drain as
   * complete.
   */
  adoptDrain(outcome: {
    complete: boolean;
    capped: boolean;
    error: unknown;
    requests: number;
    stale?: boolean;
  }): void {
    let next: WindowState;
    let reason: 'capped' | 'failed' | null = null;
    if (outcome.complete && !outcome.error) {
      next = outcome.requests <= 1 ? 'small' : 'held';
    } else {
      next = 'capped';
      reason = outcome.capped && !outcome.error ? 'capped' : 'failed';
    }
    this.enterLocal(next, reason, outcome.stale ?? false);
  }

  private enterLocal(
    next: 'small' | 'held' | 'capped',
    reason: 'capped' | 'failed' | null,
    stale: boolean
  ): void {
    this.cancelPageFetch();
    this.generation++;
    if (this._state === 'paged') {
      this._pageIndex = 0;
    }
    this._state = next;
    this.incomplete = reason;
    this._stale = stale;
    this._updatesAvailable = false;
    this._error = null;
    this._loading = false;
    this.notifyChange();
  }

  /**
   * Adopt the first paged response (a fit response with `complete: false`)
   * fetched under `committedLabel`. Resets to page 0 with no cursor.
   * `fetchedKey` is the ticket key of the request (see
   * `beginSortedRequest`); the page is recorded under it, not under the
   * current view state, so a view state that changed while the request was
   * in flight still plans a request (`planRequest('view-change')`).
   * Defaults to the current view state.
   */
  setPaged(result: PagedPageResult, committedLabel: string, fetchedKey?: string): void {
    this.cancelPageFetch();
    this.generation++;
    this._state = 'paged';
    this.incomplete = null;
    this._stale = false;
    this.pageItems = result.agents;
    this._totalCount = result.totalCount;
    this._hasNext = !!result.nextCursor;
    this.cursors = [undefined, result.nextCursor];
    this.pageOffsets = [0, result.agents.length];
    this._pageIndex = 0;
    this._cursorsValid = true;
    this._updatesAvailable = false;
    this._error = null;
    this._loading = false;
    this.committedLabel = committedLabel;
    this.pagedParams = fetchedKey ?? this.serverParamsKey();
    this.freezeOrder(result);
    this.seedStats(result.stats);
    this.replayLiveChanges(result);
    this.notifyChange();
  }

  /**
   * Replays what landed while a response was in flight over the adopted
   * page and member index: its {@link PagedPageResult.liveDeleted},
   * {@link PagedPageResult.liveChanged} and
   * {@link PagedPageResult.liveUnknown} as one `agents-changed` flush,
   * then the chip for rows dropped as deleted
   * ({@link PagedPageResult.droppedRows}) or a resync
   * ({@link PagedPageResult.liveResync}).
   */
  private replayLiveChanges(result: PagedPageResult): void {
    const upserted = result.liveChanged ?? [];
    const deleted = result.liveDeleted ?? [];
    const unknown = result.liveUnknown ?? new Map<string, UnknownAgentDelta>();
    if (upserted.length > 0 || deleted.length > 0 || unknown.size > 0) {
      this.applyChanges({
        upserted: [...upserted],
        deleted: [...deleted],
        unknown: new Map(unknown),
        generation: 0,
      });
    }
    if ((result.droppedRows ?? 0) > 0 || result.liveResync) this._updatesAvailable = true;
  }

  /**
   * Freeze the walk order from a page-0 response (see `frozenOrder`): its
   * stats population with the phase filter applied, kept only if it agrees
   * with the response itself (its head is page 0's rows and its length is
   * the server's total), so the frozen walk never disagrees with what the
   * server would page.
   */
  private freezeOrder(result: PagedPageResult): void {
    this.frozenOrder = null;
    const population = result.stats?.agents;
    if (!population) return;
    const phase = this.viewState.phaseFilter;
    const order = population.filter(([, p]) => !phase || p === phase).map(([id]) => id);
    if (order.length !== result.totalCount) return;
    for (let i = 0; i < result.agents.length; i++) {
      if (order[i] !== result.agents[i]?.id) return;
    }
    this.frozenOrder = order;
    this.frozenStarts = [0, result.agents.length];
    this.frozenDead = new Set();
  }

  /**
   * Seed the member index from a response's stats: the IDs when present,
   * or a count-only snapshot when the server omitted them (the global
   * endpoint above 2,000 agents).
   */
  private seedStats(stats: PagedPageResult['stats']): void {
    if (!stats) return;
    if (stats.agents) {
      this.memberIndex.seed(stats.agents);
    } else {
      this.memberIndex.seedCounts(stats.total, stats.running);
    }
  }

  /** Memoization cache for `display`: recomputed only when the held array's identity or the view state's identity changes. */
  private displayCache: { held: Agent[]; viewState: AgentListViewState; result: Agent[] } | null =
    null;

  /**
   * Unsliced, filtered and sorted rows of H in the local states (the
   * current server page while paged). The tree view renders this
   * unsliced. Memoized on `(H identity,
   * view state)` — `getHeldAgents()` returns the same
   * reference across renders until the host reassigns `this.agents`, and
   * `setViewState` always replaces `this.viewState` with a new object, so
   * both are cheap identity checks.
   */
  get display(): Agent[] {
    if (!this.isLocal) return this.pageItems;
    const held = this.getHeldAgents();
    if (
      this.displayCache &&
      this.displayCache.held === held &&
      this.displayCache.viewState === this.viewState
    ) {
      return this.displayCache.result;
    }
    const result = this.filteredAndSorted(held);
    this.displayCache = { held, viewState: this.viewState, result };
    return result;
  }

  /** The live-typed label preview filter, shared by the small state's full filter chain and the paged state's items-only preview. */
  private filterByLabel(list: Agent[]): Agent[] {
    const label = this.viewState.label.trim();
    if (!label) return list;
    const parts = label.split('=');
    const key = parts[0];
    const value = parts.slice(1).join('=');
    return list.filter((a) => {
      if (!a.labels) return false;
      return value ? a.labels[key] === value : key in a.labels;
    });
  }

  private filteredAndSorted(list: Agent[]): Agent[] {
    let out = list;
    if (this.viewState.phaseFilter) {
      out = out.filter((a) => a.phase === this.viewState.phaseFilter);
    }
    out = filterByMode(out, this.viewState.modeFilter ?? '');
    out = this.filterByLabel(out);
    return sortAgents(out, this.viewState.sortField, this.viewState.sortDir);
  }

  /**
   * The page slice to render: a local slice of `display` (local states), or the
   * current server page (paged). While paged, the live-typed label is still
   * applied as a local preview with no request and no `pageIndex` change:
   * while typing, the display applies the client label filter to what is
   * loaded. `total`/`rangeStart` deliberately keep reporting the server's
   * unfiltered count during this preview, so the "of N" figure may not
   * match the filtered row count until the label is committed.
   */
  get items(): Agent[] {
    if (this._state === 'paged') return this.filterByLabel(this.pageItems);
    const display = this.display;
    const start = this._pageIndex * this.viewState.pageSize;
    return display.slice(start, start + this.viewState.pageSize);
  }

  /**
   * The pager total: the server's `totalCount` (paged), `display.length`
   * (small, held, and a failed drain), or `{loaded: H.length, capped: true}`
   * for a capped drain (rendered with {@link cappedTotalText}).
   */
  get total(): number | { loaded: number; capped: true } {
    if (this._state === 'paged') return this._totalCount;
    if (this.incompleteReason === 'capped') {
      return { loaded: this.getHeldAgents().length, capped: true };
    }
    return this.display.length;
  }

  /**
   * Rows before the current page (the "a" in "a-b of N" is
   * `rangeStart + 1`). In the local states this is exact
   * (`pageIndex * pageSize`, a pure local slice). In the paged state a page
   * can be short (a row dropped by a race with a live delete), so this
   * is the actually-tracked running offset, not an assumption that every
   * prior page was full.
   */
  get rangeStart(): number {
    if (this._state === 'paged') {
      return this.pageOffsets[this._pageIndex] ?? this._pageIndex * this.viewState.pageSize;
    }
    return this._pageIndex * this.viewState.pageSize;
  }

  /**
   * "Agents" and "Running" counts: over H in the local states (today's
   * counts), from the member index while paged. `incomplete` is true in
   * `capped`, where H is not the whole set.
   */
  get stats(): { total: number; running: number; incomplete: boolean } {
    if (this.isLocal) {
      const held = this.getHeldAgents();
      return {
        total: held.length,
        running: held.filter((a) => isAgentRunning(a)).length,
        incomplete: this._state === 'capped',
      };
    }
    return { ...this.memberIndex.stats, incomplete: false };
  }

  /**
   * A sort, phase, page-size, view or label change. Purely local: it never
   * issues a request in any state. The host asks
   * `planRequest('view-change', label)` whether the change needs one.
   *
   * Resets `pageIndex` to 0 in the local states (a change resets to page
   * 0). While paged, `pageIndex` and the current page and cursors are left
   * alone: a paged change that needs a different page is followed by a
   * host request whose `setPaged` resets to page 0 together with the rows
   * it fetched, and a label keystroke alone (which only takes effect on
   * commit) must not desync `pageIndex` from the rows still on screen.
   */
  setViewState(partial: Partial<AgentListViewState>): void {
    this.viewState = { ...this.viewState, ...partial };
    if (this.isLocal) {
      this._pageIndex = 0;
    }
    this.notifyChange();
  }

  /** Next page: local in the local states, one request while paged. */
  async next(): Promise<void> {
    if (this.isLocal) {
      if (this.hasNext) {
        this._pageIndex++;
        this.notifyChange();
      }
      return;
    }
    // Uses the public `hasNext` getter, not `_hasNext` directly, so an
    // invalidated cursor stack blocks this the same way it blocks the
    // pager's own UI guard.
    if (!this.hasNext) return;
    await this.fetchPageAt(this._pageIndex + 1);
  }

  /** Previous page: local in the local states, one request while paged. */
  async prev(): Promise<void> {
    if (this.isLocal) {
      if (this._pageIndex > 0) {
        this._pageIndex--;
        this.notifyChange();
      }
      return;
    }
    // Uses the public `hasPrev` getter (see `next()`'s comment above).
    if (!this.hasPrev) return;
    await this.fetchPageAt(this._pageIndex - 1);
  }

  /**
   * Re-fetch the current page (the paged-state chip click),
   * always with `stats=1` so the counts are refreshed too. A no-op in the
   * local states. If the cursor stack was invalidated, the
   * current page's cursor is still stale, so this
   * refetches page 0 instead — its cursor is always `undefined`, so it
   * cannot mismatch, and it gives the user a way off a stranded page rather
   * than leaving them on an un-refreshable one until they change a filter.
   */
  async refresh(): Promise<void> {
    if (this._state !== 'paged') return;
    // A frozen walk has no cursor for the current page; refreshing it
    // means a fresh page 0 and a newly frozen order.
    const index = this._cursorsValid && this.frozenOrder === null ? this._pageIndex : 0;
    await this.fetchPageAt(index, true);
  }

  private async fetchPageAt(index: number, wantStats = index === 0): Promise<void> {
    this.pageController?.abort();
    const controller = new AbortController();
    this.pageController = controller;
    const gen = ++this.generation;
    this._loading = true;
    this._error = null;
    this.notifyChange();
    const pageSize = this.viewState.pageSize;
    const frozen = index > 0 && !wantStats ? this.frozenOrder : null;
    const frozenStart = frozen ? this.frozenStarts[index] : undefined;
    const frozenIds =
      frozen && frozenStart !== undefined
        ? frozen.slice(frozenStart, frozenStart + pageSize)
        : null;
    try {
      const fetched = await this.fetchPage(
        frozenIds
          ? { ids: frozenIds, limit: pageSize, wantStats: false, signal: controller.signal }
          : {
              cursor: this.cursors[index],
              limit: pageSize,
              wantStats,
              signal: controller.signal,
            }
      );
      if (gen !== this.generation) return;
      if (this.pageController === controller) this.pageController = null;
      let result = fetched;
      if (frozen && frozenIds && frozenStart !== undefined) {
        // Show the page in the frozen order, keeping only the ids asked
        // for. An id the server did not return (deleted, no longer
        // readable, or out of the phase filter now) just leaves this page;
        // the next page still starts after every id asked for here.
        const byId = new Map(fetched.agents.map((a) => [a.id, a]));
        const agents = frozenIds.map((id) => byId.get(id)).filter((a): a is Agent => !!a);
        // An id that comes back (it re-entered the phase filter, or became
        // readable again) counts towards the total again.
        for (const id of frozenIds) {
          if (byId.has(id)) this.frozenDead.delete(id);
          else this.frozenDead.add(id);
        }
        const nextStart = frozenStart + frozenIds.length;
        const total = frozen.length - this.frozenDead.size;
        if (agents.length === 0) {
          if (nextStart < frozen.length) {
            // The whole slice was gone: this page takes the next slice.
            this.frozenStarts[index] = nextStart;
            await this.fetchPageAt(index, false);
          } else {
            // Nothing is left past the previous page: stay on it, with no
            // next page.
            this._hasNext = false;
            this._totalCount = total;
          }
          return;
        }
        this.frozenStarts[index + 1] = nextStart;
        result = { ...fetched, agents, totalCount: total, nextCursor: undefined };
        this._hasNext = nextStart < frozen.length;
      }
      if (result.agents.length === 0 && index > 0) {
        // An emptied last page: step back one page.
        await this.fetchPageAt(index - 1, wantStats);
        return;
      }
      this.pageItems = result.agents;
      this._totalCount = result.totalCount;
      this._pageIndex = index;
      this.cursors[index + 1] = result.nextCursor;
      this.pageOffsets[index + 1] =
        (this.pageOffsets[index] ?? index * this.viewState.pageSize) + result.agents.length;
      if (!frozenIds) this._hasNext = !!result.nextCursor;
      if (index === 0) {
        // Page 0's request is always cursor-free, so it cannot mismatch; a
        // successful fetch mints `cursors[1]` fresh under the current
        // params, which makes the whole stack valid again.
        this._cursorsValid = true;
        if (wantStats) this.freezeOrder(result);
      }
      this._updatesAvailable = false;
      this.seedStats(result.stats);
      this.replayLiveChanges(result);
    } catch (err) {
      if (gen !== this.generation) return;
      if (this.pageController === controller) this.pageController = null;
      this._error = err instanceof Error ? err.message : 'Failed to load agents';
    } finally {
      if (gen === this.generation) {
        this._loading = false;
        this.notifyChange();
      }
    }
  }

  /**
   * A live-connection resync: raises the chip while paged, or the stale
   * banner in the local states. Issues no request.
   */
  markResync(): void {
    if (this.isLocal) this._stale = true;
    else this._updatesAvailable = true;
    this.notifyChange();
  }

  /**
   * A membership change the add rule cannot decide, for example an agent
   * created while the global page is loaded for scope `mine` or `shared`.
   * Raises the chip while paged, and the stale banner in `held` or
   * `capped` (a drained set that may now miss that agent). Issues no
   * request. A no-op in `small`.
   */
  markMembershipChanged(): void {
    if (this._state === 'paged') {
      this._updatesAvailable = true;
    } else if (this._state === 'held' || this._state === 'capped') {
      this._stale = true;
    } else {
      return;
    }
    this.notifyChange();
  }

  /**
   * Clears paged navigation after a failed view-change request: the stored
   * cursors were minted under the previous phase/label/dir (a cursor is
   * bound to its request parameters) and would 400 if replayed under the new,
   * now-current params. `hasNext`/`hasPrev` report `false` until the next
   * successful `setPaged()` mints a fresh cursor stack. A no-op in the
   * small state, which has no cursors.
   *
   * Also bumps `generation` and drops the loading flag, the same way
   * `setPaged`/`setSmall` do: a window fetch can still be in flight when
   * the triggering view-change fails, and if that fetch is a page-0
   * request it would otherwise land afterward and re-validate the stack
   * using a cursor minted under the params that were just invalidated. The
   * generation bump makes that late response a no-op, so only a page-0
   * fetch issued *after* this call can mark the stack valid again.
   */
  invalidateCursors(): void {
    if (this._state !== 'paged') return;
    this.cancelPageFetch();
    this._cursorsValid = false;
    this.frozenOrder = null;
    this.generation++;
    this._loading = false;
    this.notifyChange();
  }

  /** The committed label's add rule (today's add rule, mirroring the server's stats population): the project match (or `isAddable`) plus the committed `k=v`, if any. */
  private passesCommittedLabel(agent: Agent): boolean {
    if (this.isAddable) {
      if (!this.isAddable(agent)) return false;
    } else if (agent.projectId !== this.getProjectId()) {
      return false;
    }
    const label = this.committedLabel.trim();
    if (!label || !label.includes('=')) return true;
    const eq = label.indexOf('=');
    const key = label.slice(0, eq);
    const value = label.slice(eq + 1);
    return agent.labels?.[key] === value;
  }

  /** The server sort the paged state was fetched with: `created`, or `updated` for everything else. */
  private get serverSort(): 'updated' | 'created' {
    return this.viewState.sortField === 'created' ? 'created' : 'updated';
  }

  private kOf(a: Agent): string {
    return serverSortKey(a, this.serverSort);
  }

  /** The page's current [first, last] sort-key bounds, before this flush's mutations (the K-range chip rule). `null` if the page is empty. */
  private pageKRange(): { first: string; last: string } | null {
    if (this.pageItems.length === 0) return null;
    return {
      first: this.kOf(this.pageItems[0]),
      last: this.kOf(this.pageItems[this.pageItems.length - 1]),
    };
  }

  /**
   * Whether an **off-page** key `k` would land on the *current* page (on
   * page 0: K >= first for desc, K <= first for asc). Used
   * only for off-page members — an on-page row uses
   * the different `onPageChipForNewKey` predicate below.
   */
  private withinPageKRange(k: string, range: { first: string; last: string } | null): boolean {
    if (!range) return this._pageIndex === 0; // an empty page 0 accepts anything.
    const { first, last } = range;
    if (this.viewState.sortDir === 'desc') {
      return this._pageIndex === 0 ? k >= first : k <= first && k >= last;
    }
    return this._pageIndex === 0 ? k <= first : k >= first && k <= last;
  }

  /**
   * Whether an **on-page** row's new key `k` should raise the chip: the new
   * K is outside the page's [first,last] range and the row is not at the
   * top of page 0. This is the
   * opposite direction of `withinPageKRange` — a row already on the page
   * that simply reorders to the current extreme ("the top of page 0") never
   * left the page, so no chip; only falling off the *other* end, or rising
   * past the top on any page but page 0 (nothing above page 0 to shift it
   * into), does.
   */
  private onPageChipForNewKey(k: string, range: { first: string; last: string }): boolean {
    const { first, last } = range;
    const atPageZero = this._pageIndex === 0;
    if (this.viewState.sortDir === 'desc') {
      if (k < last) return true; // fell off the bottom, onto the next page.
      if (k > first) return !atPageZero; // rose past the old top.
      return false; // reordered within range.
    }
    if (k > last) return true;
    if (k < first) return !atPageZero;
    return false;
  }

  private passesPhase(a: Agent): boolean {
    return !this.viewState.phaseFilter || a.phase === this.viewState.phaseFilter;
  }

  /**
   * Apply a coalesced `agents-changed` flush to the current page and member
   * index, one rule per row kind (on-page, off-page member, new agent,
   * unknown delta). A no-op in the local states, whose host merges the
   * change into H itself.
   */
  applyChanges(detail: AgentsChangedDetail): void {
    if (this._state !== 'paged') return;

    const rangeBefore = this.pageKRange();
    const onPage = new Map(this.pageItems.map((a) => [a.id, a]));
    const pageSizeBefore = onPage.size;
    let chip = false;
    let resort = false;

    // Deletes first: idempotent, safe to apply even for an ID already gone
    // (`deleted` is a safe superset, never wrong).
    for (const id of detail.deleted) {
      if (onPage.delete(id)) chip = true; // on-page delete (backfill)
      this.memberIndex.delete(id);
    }

    for (const id of detail.upserted) {
      const agent = this.getAgent(id);
      if (!agent) continue;
      if (onPage.has(id)) {
        if (this.passesPhase(agent)) {
          // Replace this object, then re-sort the page locally (today's live reorder).
          // Chip iff the new key would move it off this page.
          const newK = this.kOf(agent);
          if (rangeBefore && this.onPageChipForNewKey(newK, rangeBefore)) chip = true;
          onPage.set(id, agent);
          resort = true;
        } else {
          // On-page agent now fails the phase filter: remove the row (backfill chip).
          onPage.delete(id);
          chip = true;
        }
        this.memberIndex.set(id, agent.phase);
      } else {
        chip ||= this.applyOffPageUpsert(id, agent, rangeBefore);
      }
    }

    for (const [id, delta] of detail.unknown) {
      if (onPage.has(id)) continue; // on-page agents are always known already.
      chip ||= this.applyOffPageUnknown(id, delta, rangeBefore);
    }

    // Count-only: the snapshot counts cannot be adjusted, so any change may
    // have changed them.
    if (this.memberIndex.countOnly) {
      const any =
        detail.upserted.length > 0 || detail.deleted.length > 0 || detail.unknown.size > 0;
      if (any) chip = true;
    }

    if (resort || onPage.size !== pageSizeBefore) {
      this.pageItems = Array.from(onPage.values()).sort((a, b) =>
        serverOrderCompare(a, b, this.viewState.sortDir, this.serverSort)
      );
    }

    if (chip) this._updatesAvailable = true;
    if (chip || resort) this.notifyChange();
  }

  /**
   * Apply an optimistic local patch (for example a phase change right after
   * a stop action) while paged: each given agent replaces its on-page row,
   * the page is re-sorted locally in server order (as a live on-page upsert
   * is), and an existing member's phase is updated in the member index.
   * Never adds or removes a row, never adds a member, never raises the chip
   * and never sends a request. A no-op in the local
   * states, where the host merges the patch into H itself.
   */
  applyLocalUpdate(agents: readonly Agent[]): void {
    if (this._state !== 'paged' || agents.length === 0) return;
    const patch = new Map(agents.map((a) => [a.id, a]));
    let changed = false;
    this.pageItems = this.pageItems.map((row) => {
      const next = patch.get(row.id);
      if (!next) return row;
      changed = true;
      return next;
    });
    if (changed) {
      this.pageItems.sort((a, b) =>
        serverOrderCompare(a, b, this.viewState.sortDir, this.serverSort)
      );
    }
    for (const a of agents) {
      if (this.memberIndex.has(a.id)) this.memberIndex.set(a.id, a.phase);
    }
    if (changed) this.notifyChange();
  }

  /**
   * An off-page upsert. A full `Agent` is
   * available, so phase, membership and K are all known precisely.
   * Returns whether the chip should show.
   */
  private applyOffPageUpsert(
    id: string,
    agent: Agent,
    rangeBefore: { first: string; last: string } | null
  ): boolean {
    const wasMember = this.memberIndex.has(id);

    if (wasMember) {
      // An existing off-page member's phase changes unconditionally: only
      // *adding* a new member is gated by the add rule.
      const prevPhase = this.memberIndex.getPhase(id);
      const prevPassed = !this.viewState.phaseFilter || prevPhase === this.viewState.phaseFilter;
      const nowPasses = this.passesPhase(agent);
      this.memberIndex.set(id, agent.phase);
      const newlyPasses = nowPasses && !prevPassed;
      const enteredRange = this.withinPageKRange(this.kOf(agent), rangeBefore);
      // An off-page member change affecting counts only shows no chip.
      return newlyPasses || enteredRange;
    }

    if (!this.passesCommittedLabel(agent)) {
      // Neither on-page nor a member, and outside the committed label: the
      // same as a delta for an ID the window has never heard of — ignored,
      // with no chip. The project page
      // has no "created outside today's add rule" row; that row is for the
      // global mine/shared pages only.
      return false;
    }

    // A genuinely new member under today's add rule — e.g.
    // a `created` event, which `AgentsChangedDetail`
    // cannot distinguish from any other upsert, so the add rule itself is
    // what keeps an out-of-label agent out of the stats.
    this.memberIndex.set(id, agent.phase);
    // A newly created agent shows only if it could land on this page: for
    // a desc sort only page 0 can receive a brand-new row (inserts sort
    // before the cursor); an asc sort can receive one on any page.
    const couldLandOnThisPage = this.viewState.sortDir === 'asc' || this._pageIndex === 0;
    return this.passesPhase(agent) && couldLandOnThisPage;
  }

  /** An off-page `unknown` delta: only phase (and sometimes `lastActivityEvent`) is known. */
  private applyOffPageUnknown(
    id: string,
    delta: UnknownAgentDelta,
    rangeBefore: { first: string; last: string } | null
  ): boolean {
    if (!this.memberIndex.has(id)) return false; // neither on-page nor a member: ignored.
    const prevPhase = this.memberIndex.getPhase(id);
    const prevPassed = !this.viewState.phaseFilter || prevPhase === this.viewState.phaseFilter;
    if (delta.phase) this.memberIndex.set(id, delta.phase);
    const nowPasses =
      !this.viewState.phaseFilter || (delta.phase ?? prevPhase) === this.viewState.phaseFilter;
    const newlyPasses = nowPasses && !prevPassed;
    // K is not carried by UnknownAgentDelta's phase-only shape unless a
    // lastActivityEvent accompanies it; when absent, assume it does not
    // enter the page (conservative: this class of off-page drift is
    // accepted, and carrying K end-to-end would be additive).
    // An activity time moves only the `updated` key.
    const enteredRange =
      delta.lastActivityEvent && this.serverSort === 'updated'
        ? this.withinPageKRange(delta.lastActivityEvent, rangeBefore)
        : false;
    return newlyPasses || enteredRange;
  }

  private notifyChange(): void {
    this.dispatchEvent(new Event('change'));
  }
}

/**
 * The global page's mode filter over a list: `can_message` and
 * `cannot_message` by messageability, any other value by message mode
 * (`project` when unset). An empty filter returns the list itself.
 */
function filterByMode(list: Agent[], modeFilter: string): Agent[] {
  if (!modeFilter) return list;
  if (modeFilter === 'can_message') {
    return list.filter((a) => a._messageability?.canMessage === true);
  }
  if (modeFilter === 'cannot_message') {
    return list.filter((a) => a._messageability?.canMessage === false);
  }
  return list.filter((a) => (a.messageMode || 'project') === modeFilter);
}
