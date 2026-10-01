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
 * Agent list window state machine (design §4.3, §6.1, §6.2).
 *
 * P1c (design §11) implements only the **small** and **paged** states. The
 * **held** and **capped** states (reached only by a complete-set drain) land
 * in P5 with `agent-drain.ts`; see the interim-costs note in project-detail.ts
 * for what happens above 500 candidates until then.
 *
 * The small state never copies the held agent array (round 1 review, B1):
 * it reads it fresh, by reference, from `getHeldAgents()` on every access,
 * so it always reflects whatever the host's live-update path
 * (`onAgentsUpdated`) last assigned — including an SSE delta that arrived
 * after the last trigger, with no re-adoption step and no `pageIndex` reset.
 * `setSmall()` never resets `pageIndex` on a small -> small call, because an
 * unrelated small-state render (a view-state change, or a fresh `setSmall()`
 * call from a later trigger while already small) must not throw the user
 * back to page 0 (that only happens through `setViewState`, a deliberate
 * filter/sort change, exactly as a paged page-0 reset does); it resets on a
 * paged -> small call instead, since that always swaps in a different data
 * set (round 3 review B1'').
 */

import type { Agent, AgentPhase } from '../shared/types.js';
import { isAgentRunning } from '../shared/types.js';
import type { AgentSortField, SortDir } from '../shared/agent-sort.js';
import { sortAgents, serverOrderCompare, updatedKey } from '../shared/agent-sort.js';
import type { AgentsChangedDetail, UnknownAgentDelta } from './state.js';
import { AgentMemberIndex } from './agent-member-index.js';

export type WindowState = 'small' | 'paged';

export interface AgentListViewState {
  phaseFilter: AgentPhase | '';
  /** The live-typed label filter preview (small-state local filtering only — never the request label; see `AgentListWindow`'s own `committedLabel`). */
  label: string;
  sortField: AgentSortField;
  sortDir: SortDir;
  pageSize: number;
}

export interface PagedPageParams {
  cursor?: string | undefined;
  limit: number;
  /** `stats=1`: only on page 0, a view-state change, or a refresh (design §6.1 N5). */
  wantStats: boolean;
}

export interface PagedPageResult {
  agents: Agent[];
  nextCursor?: string | undefined;
  totalCount: number;
  stats?: { total: number; running: number; agents?: Array<[string, string]> } | undefined;
}

export type PagedPageFetcher = (params: PagedPageParams) => Promise<PagedPageResult>;

export interface AgentListWindowOptions {
  viewState: AgentListViewState;
  /** The project this window belongs to, used by the paged-state off-page add rule (design §6.2, round 1 review B6). Read lazily — `project-detail.ts` constructs the window before `this.projectId` is finalized from the URL in `connectedCallback`. */
  getProjectId: () => string;
  fetchPage: PagedPageFetcher;
  /** Full `Agent` lookup for an upserted ID (design §6.2 on-page replace). Typically `stateManager.getAgent`. */
  getAgent: (id: string) => Agent | undefined;
  /**
   * Returns the host's current legacy/held agent array (`this.agents`) on
   * every call. The window never copies it (round 1 review B1) — reading it
   * fresh is what lets an unrelated SSE-driven reassignment of `this.agents`
   * show up in the small-state list with no re-adoption step.
   */
  getHeldAgents: () => Agent[];
}

export class AgentListWindow extends EventTarget {
  readonly memberIndex = new AgentMemberIndex();

  private _state: WindowState = 'small';
  private pageItems: Agent[] = [];
  private cursors: Array<string | undefined> = [undefined];
  /** `pageOffsets[i]` = rows before page `i` (round 2 review N2'). Pages can be short (E2, §5.3 step 5a's race-dropped rows), so this is tracked as pages are actually fetched, not assumed to be `pageIndex * pageSize`. */
  private pageOffsets: number[] = [0];
  private _pageIndex = 0;
  private _totalCount = 0;
  private _hasNext = false;
  /** Cleared by `invalidateCursors()` (round 4 review N1'''); restored by the next `setPaged()`. */
  private _cursorsValid = true;
  private _loading = false;
  private _error: string | null = null;
  private _updatesAvailable = false;
  private generation = 0;

  /** The label the current paged response was fetched under (design §6.2's add rule; round 1 review B6). Empty or `k=v` only — a bare-key label is never paged (design §4.3). */
  private committedLabel = '';

  private viewState: AgentListViewState;
  private readonly getProjectId: () => string;
  private readonly fetchPage: PagedPageFetcher;
  private readonly getAgent: (id: string) => Agent | undefined;
  private readonly getHeldAgents: () => Agent[];

  constructor(options: AgentListWindowOptions) {
    super();
    this.viewState = options.viewState;
    this.getProjectId = options.getProjectId;
    this.fetchPage = options.fetchPage;
    this.getAgent = options.getAgent;
    this.getHeldAgents = options.getHeldAgents;
  }

  get state(): WindowState {
    return this._state;
  }

  get pageIndex(): number {
    return this._pageIndex;
  }

  get loading(): boolean {
    return this._loading;
  }

  get error(): string | null {
    return this._error;
  }

  /** The paged-state "may have changed - Refresh" chip (design §6.2). */
  get updatesAvailable(): boolean {
    return this._updatesAvailable;
  }

  get hasPrev(): boolean {
    if (this._state === 'paged' && !this._cursorsValid) return false;
    return this._pageIndex > 0;
  }

  get hasNext(): boolean {
    if (this._state === 'paged') return this._cursorsValid && this._hasNext;
    return (this._pageIndex + 1) * this.viewState.pageSize < this.display.length;
  }

  /**
   * Render from the host's current `this.agents` (small state): a fit
   * `complete: true` response, or a legacy load, truncated or not (design
   * §4.3; round 1 review B1/B2). The caller is responsible for having
   * already assigned the array `getHeldAgents()` will return.
   *
   * `pageIndex` is reset to 0 only when the **previous** state was `'paged'`
   * (round 3 review B1''): a paged -> small transition always swaps in a
   * different data set (a label commit whose set now fits, a bare-key
   * label or 422 falling back to the legacy load, a lifecycle refresh that
   * drops the candidate count to the fit threshold), so the old paged
   * `pageIndex` can point past the end of — or into the wrong slice of —
   * the newly-adopted held set. A small -> small call (e.g. a later
   * trigger while already small) leaves `pageIndex` alone, exactly as
   * before; `setViewState` remains the only thing that resets it within an
   * already-small session.
   */
  setSmall(): void {
    this.generation++;
    if (this._state === 'paged') {
      this._pageIndex = 0;
    }
    this._state = 'small';
    this._updatesAvailable = false;
    this._error = null;
    this._loading = false;
  }

  /** Adopt the first paged response: fit `complete: false` (design §4.3). */
  setPaged(result: PagedPageResult, committedLabel: string): void {
    this.generation++;
    this._state = 'paged';
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
    this.seedStats(result.stats);
  }

  private seedStats(stats: PagedPageResult['stats']): void {
    if (stats?.agents) {
      this.memberIndex.seed(stats.agents);
    }
  }

  /** Memoization cache for `display` (round 2 review N4'): recomputed only when the held array's identity or the view state's identity changes. */
  private displayCache: { held: Agent[]; viewState: AgentListViewState; result: Agent[] } | null =
    null;

  /**
   * Unsliced, filtered + sorted local view (small state only). Grid and tree
   * views render this directly (design §6.3). Memoized on `(H identity,
   * view state)` per design §6.1 — `getHeldAgents()` returns the same
   * reference across renders until the host reassigns `this.agents`, and
   * `setViewState` always replaces `this.viewState` with a new object, so
   * both are cheap identity checks.
   */
  get display(): Agent[] {
    if (this._state !== 'small') return this.pageItems;
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

  /** The live-typed label preview filter (design §6.3), shared by the small state's full filter chain and the paged state's items-only preview (round 3 review B3''). */
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
    out = this.filterByLabel(out);
    return sortAgents(out, this.viewState.sortField, this.viewState.sortDir);
  }

  /**
   * The page slice to render: a local slice of `display` (small), or the
   * current server page (paged). While paged, the live-typed label is still
   * applied as a local preview with no request and no `pageIndex` change
   * (design §6.3's "while typing, the display applies today's client label
   * filter to what is loaded"; round 3 review B3'' — this previously
   * returned `pageItems` unfiltered). `total`/`rangeStart` deliberately keep
   * reporting the server's unfiltered count during this preview window; the
   * design accepts that the "of N" figure may not match the filtered row
   * count until the label is committed.
   */
  get items(): Agent[] {
    if (this._state === 'paged') return this.filterByLabel(this.pageItems);
    const display = this.display;
    const start = this._pageIndex * this.viewState.pageSize;
    return display.slice(start, start + this.viewState.pageSize);
  }

  get total(): number {
    return this._state === 'paged' ? this._totalCount : this.display.length;
  }

  /**
   * Rows before the current page (design §6.1's "a" in "a-b of N" is
   * `rangeStart + 1`). In the small state this is exact
   * (`pageIndex * pageSize`, a pure local slice). In the paged state a page
   * can be short — a race-dropped row (design §5.3 step 5a, E2) — so this
   * is the actually-tracked running offset, not an assumption that every
   * prior page was full (round 2 review N2').
   */
  get rangeStart(): number {
    if (this._state === 'paged') {
      return this.pageOffsets[this._pageIndex] ?? this._pageIndex * this.viewState.pageSize;
    }
    return this._pageIndex * this.viewState.pageSize;
  }

  get stats(): { total: number; running: number } {
    if (this._state === 'small') {
      const held = this.getHeldAgents();
      return {
        total: held.length,
        running: held.filter((a) => isAgentRunning(a)).length,
      };
    }
    return this.memberIndex.stats;
  }

  /**
   * A sort, phase, page-size or label change. Purely local — never issues a
   * request, in either state (round 1 review B3): the project page's
   * `syncAgentsForViewState` is the single place that decides whether a
   * view-state change needs a fresh paged request (design §4.3's "one
   * request per trigger", and the §11 P1c interim-cost transitions).
   *
   * Resets `pageIndex` to 0 only in the **small** state (design §6.3, "a
   * change resets to page 0"). In the **paged** state, `pageIndex` and the
   * current page/cursors are left alone here (round 2 review B1'): every
   * paged view-state change that actually needs a different page — sort,
   * phase, page-size — is followed by `syncAgentsForViewState` calling
   * `loadAgentsForView`, whose `setPaged` already resets to page 0 together
   * with the items and cursors it fetched. A **label** change alone is
   * never followed by a refetch (it only takes effect on commit), so
   * resetting `pageIndex` here for a paged label keystroke would desync it
   * from the rows still on screen — the pager, Prev/Next and the chip's
   * page-0 rule would all act as if page 0 were showing.
   */
  setViewState(partial: Partial<AgentListViewState>): void {
    this.viewState = { ...this.viewState, ...partial };
    if (this._state === 'small') {
      this._pageIndex = 0;
    }
    this.notifyChange();
  }

  async next(): Promise<void> {
    if (this._state === 'small') {
      if (this.hasNext) {
        this._pageIndex++;
        this.notifyChange();
      }
      return;
    }
    // Uses the public `hasNext` getter, not `_hasNext` directly, so an
    // invalidated cursor stack (round 4 review N1''') blocks this the same
    // way it blocks the pager's own UI guard.
    if (!this.hasNext) return;
    await this.fetchPageAt(this._pageIndex + 1);
  }

  async prev(): Promise<void> {
    if (this._state === 'small') {
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
   * Re-fetch the current page (the paged-state chip click, design §6.2). A
   * no-op in the small state. If the cursor stack was invalidated (round 5
   * review N2''''), the current page's cursor is still stale, so this
   * refetches page 0 instead — its cursor is always `undefined`, so it
   * cannot mismatch, and it gives the user a way off a stranded page rather
   * than leaving them on an un-refreshable one until they change a filter.
   */
  async refresh(): Promise<void> {
    if (this._state !== 'paged') return;
    await this.fetchPageAt(this._cursorsValid ? this._pageIndex : 0);
  }

  private async fetchPageAt(index: number): Promise<void> {
    const gen = ++this.generation;
    this._loading = true;
    this._error = null;
    this.notifyChange();
    try {
      const result = await this.fetchPage({
        cursor: this.cursors[index],
        limit: this.viewState.pageSize,
        wantStats: index === 0,
      });
      if (gen !== this.generation) return;
      if (result.agents.length === 0 && index > 0) {
        // An emptied last page (design §6.2, W5 T4): step back one page.
        await this.fetchPageAt(index - 1);
        return;
      }
      this.pageItems = result.agents;
      this._totalCount = result.totalCount;
      this._pageIndex = index;
      this.cursors[index + 1] = result.nextCursor;
      this.pageOffsets[index + 1] =
        (this.pageOffsets[index] ?? index * this.viewState.pageSize) + result.agents.length;
      this._hasNext = !!result.nextCursor;
      if (index === 0) {
        // Page 0's request is always cursor-free, so it cannot mismatch; a
        // successful fetch mints `cursors[1]` fresh under the current
        // params, which makes the whole stack valid again.
        this._cursorsValid = true;
      }
      this._updatesAvailable = false;
      this.seedStats(result.stats);
    } catch (err) {
      if (gen !== this.generation) return;
      this._error = err instanceof Error ? err.message : 'Failed to load agents';
    } finally {
      if (gen === this.generation) {
        this._loading = false;
        this.notifyChange();
      }
    }
  }

  /** `agents-resync` (design §6.2, §7): the zero-cost stale signal. Issues no request. */
  markResync(): void {
    this._updatesAvailable = true;
    this.notifyChange();
  }

  /**
   * Clears paged navigation after a failed view-change request: the stored
   * cursors were minted under the previous phase/label/dir (design §4.4's
   * cursor-binding contract) and would 400 if replayed under the new,
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
    this._cursorsValid = false;
    this.generation++;
    this._loading = false;
    this.notifyChange();
  }

  /** The committed label's add rule (design §6.2's "today's add rule", mirroring the server's stats population): project match plus the committed `k=v`, if any (round 1 review B6). */
  private passesCommittedLabel(agent: Agent): boolean {
    if (agent.projectId !== this.getProjectId()) return false;
    const label = this.committedLabel.trim();
    if (!label || !label.includes('=')) return true;
    const eq = label.indexOf('=');
    const key = label.slice(0, eq);
    const value = label.slice(eq + 1);
    return agent.labels?.[key] === value;
  }

  /** The page's current [first, last] `updatedKey` bounds, before this flush's mutations (design §6.2's K-range chip rule). `null` if the page is empty. */
  private pageKRange(): { first: string; last: string } | null {
    if (this.pageItems.length === 0) return null;
    return {
      first: updatedKey(this.pageItems[0]),
      last: updatedKey(this.pageItems[this.pageItems.length - 1]),
    };
  }

  /**
   * Whether an **off-page** key `k` would land on the *current* page, per
   * design §6.2 ("on page 0: K >= first for desc, K <= first for asc"). Used
   * only for off-page members (round 2 review B2'a — an on-page row uses
   * the different `onPageChipForNewKey` predicate below).
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
   * Whether an **on-page** row's new key `k` should raise the chip (design
   * §6.2: "the new K is outside the page's [first,last] range and the row
   * is not at the top of page 0"). Round 2 review B2'a: this is the
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
   * Apply a coalesced `agents-changed` flush, per the design §6.2 table
   * (round 1 review N1: implemented row-for-row, not approximated). A no-op
   * in the small state, which is driven by `onAgentsUpdated` over
   * `this.agents` instead (design §11 P1c).
   */
  applyChanges(detail: AgentsChangedDetail): void {
    if (this._state !== 'paged') return;

    const rangeBefore = this.pageKRange();
    const onPage = new Map(this.pageItems.map((a) => [a.id, a]));
    const pageSizeBefore = onPage.size;
    let chip = false;
    let resort = false;

    // Deletes first: idempotent, safe to apply even for an ID already gone
    // (P1a FYI — `deleted` is a safe superset, never wrong).
    for (const id of detail.deleted) {
      if (onPage.delete(id)) chip = true; // on-page delete (backfill)
      this.memberIndex.delete(id);
    }

    for (const id of detail.upserted) {
      const agent = this.getAgent(id);
      if (!agent) continue;
      if (onPage.has(id)) {
        if (this.passesPhase(agent)) {
          // Replace this object, then re-sort the page locally (today's live reorder, Q-D).
          // Chip iff the new key would move it off this page (design §6.2; round 2 review B2'a).
          const newK = updatedKey(agent);
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

    if (resort || onPage.size !== pageSizeBefore) {
      this.pageItems = Array.from(onPage.values()).sort((a, b) =>
        serverOrderCompare(a, b, this.viewState.sortDir)
      );
    }

    if (chip) this._updatesAvailable = true;
    if (chip || resort) this.notifyChange();
  }

  /**
   * An off-page upsert (design §6.2's off-page rows). A full `Agent` is
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
      // An existing off-page member's phase changes unconditionally (round 1
      // review B6: only *adding* a new member is gated by the add rule).
      const prevPhase = this.memberIndex.getPhase(id);
      const prevPassed = !this.viewState.phaseFilter || prevPhase === this.viewState.phaseFilter;
      const nowPasses = this.passesPhase(agent);
      this.memberIndex.set(id, agent.phase);
      const newlyPasses = nowPasses && !prevPassed;
      const enteredRange = this.withinPageKRange(updatedKey(agent), rangeBefore);
      // "Off-page member change affecting counts only" shows no chip (design §6.2).
      return newlyPasses || enteredRange;
    }

    if (!this.passesCommittedLabel(agent)) {
      // Neither on-page nor a member, and outside the committed label: the
      // same as a delta for an ID the window has never heard of — ignored,
      // with no chip (design §6.2; round 2 review B2'b). The project page
      // has no "created outside today's add rule" row; that row is for the
      // global mine/shared pages only.
      return false;
    }

    // A genuinely new member under today's add rule (design §6.2; round 1
    // review B6) — e.g. a `created` event, which `AgentsChangedDetail`
    // cannot distinguish from any other upsert, so the add rule itself is
    // the gate against inflating stats for an out-of-label agent.
    this.memberIndex.set(id, agent.phase);
    // "created... show if it could land on this page" (design §6.2; round 2
    // review B2'c): for a desc sort only page 0 can receive a brand-new
    // row (inserts sort before the cursor — design §4.5); an asc sort can
    // receive one on any page.
    const couldLandOnThisPage = this.viewState.sortDir === 'asc' || this._pageIndex === 0;
    return this.passesPhase(agent) && couldLandOnThisPage;
  }

  /** An off-page `unknown` delta (design §6.2): only phase (and sometimes `lastActivityEvent`) is known. */
  private applyOffPageUnknown(
    id: string,
    delta: UnknownAgentDelta,
    rangeBefore: { first: string; last: string } | null
  ): boolean {
    if (!this.memberIndex.has(id)) return false; // neither on-page nor a member: ignored (design §6.2).
    const prevPhase = this.memberIndex.getPhase(id);
    const prevPassed = !this.viewState.phaseFilter || prevPhase === this.viewState.phaseFilter;
    if (delta.phase) this.memberIndex.set(id, delta.phase);
    const nowPasses =
      !this.viewState.phaseFilter || (delta.phase ?? prevPhase) === this.viewState.phaseFilter;
    const newlyPasses = nowPasses && !prevPassed;
    // K is not carried by UnknownAgentDelta's phase-only shape unless a
    // lastActivityEvent accompanies it; when absent, assume it does not
    // enter the page (conservative: §14 R5 already accepts this class of
    // drift for off-page staleness, and carrying K end-to-end is additive).
    const enteredRange = delta.lastActivityEvent
      ? this.withinPageKRange(delta.lastActivityEvent, rangeBefore)
      : false;
    return newlyPasses || enteredRange;
  }

  private notifyChange(): void {
    this.dispatchEvent(new Event('change'));
  }
}
