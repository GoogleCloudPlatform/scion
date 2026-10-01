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
 * `setSmall()` only flips the state machine; it never resets `pageIndex`,
 * because an unrelated small-state render (a view-state change, or a fresh
 * `setSmall()` call from a later trigger while already small) must not throw
 * the user back to page 0 (that only happens through `setViewState`, a
 * deliberate filter/sort change, exactly as a paged page-0 reset does).
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
  private _pageIndex = 0;
  private _totalCount = 0;
  private _hasNext = false;
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
    return this._pageIndex > 0;
  }

  get hasNext(): boolean {
    if (this._state === 'paged') return this._hasNext;
    return (this._pageIndex + 1) * this.viewState.pageSize < this.display.length;
  }

  /**
   * Render from the host's current `this.agents` (small state): a fit
   * `complete: true` response, or a legacy load, truncated or not (design
   * §4.3; round 1 review B1/B2). `pageIndex` is left untouched — only
   * `setViewState` resets it. The caller is responsible for having already
   * assigned the array `getHeldAgents()` will return.
   */
  setSmall(): void {
    this.generation++;
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
    this._pageIndex = 0;
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

  /** Unsliced, filtered + sorted local view (small state only). Grid and tree views render this directly (design §6.3). */
  get display(): Agent[] {
    if (this._state !== 'small') return this.pageItems;
    return this.filteredAndSorted(this.getHeldAgents());
  }

  private filteredAndSorted(list: Agent[]): Agent[] {
    let out = list;
    if (this.viewState.phaseFilter) {
      out = out.filter((a) => a.phase === this.viewState.phaseFilter);
    }
    const label = this.viewState.label.trim();
    if (label) {
      const parts = label.split('=');
      const key = parts[0];
      const value = parts.slice(1).join('=');
      out = out.filter((a) => {
        if (!a.labels) return false;
        return value ? a.labels[key] === value : key in a.labels;
      });
    }
    return sortAgents(out, this.viewState.sortField, this.viewState.sortDir);
  }

  /** The page slice to render: a local slice of `display` (small), or the current server page (paged). */
  get items(): Agent[] {
    if (this._state === 'paged') return this.pageItems;
    const display = this.display;
    const start = this._pageIndex * this.viewState.pageSize;
    return display.slice(start, start + this.viewState.pageSize);
  }

  get total(): number {
    return this._state === 'paged' ? this._totalCount : this.display.length;
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
   * A sort, phase, page-size or label change. Always resets `pageIndex` to 0
   * (design §6.3). Purely local — never issues a request, in either state
   * (round 1 review B3): the project page's `syncAgentsForViewState` is the
   * single place that decides whether a view-state change needs a fresh
   * paged request (design §4.3's "one request per trigger", and the §11
   * P1c interim-cost transitions).
   */
  setViewState(partial: Partial<AgentListViewState>): void {
    this.viewState = { ...this.viewState, ...partial };
    this._pageIndex = 0;
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
    if (!this._hasNext) return;
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
    if (this._pageIndex === 0) return;
    await this.fetchPageAt(this._pageIndex - 1);
  }

  /** Re-fetch the current page (the paged-state chip click, design §6.2). A no-op in the small state. */
  async refresh(): Promise<void> {
    if (this._state !== 'paged') return;
    await this.fetchPageAt(this._pageIndex);
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
      this._hasNext = !!result.nextCursor;
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

  /** Whether key `k` would land on the *current* page, per design §6.2 ("on page 0: K >= first for desc, K <= first for asc"). */
  private withinPageKRange(k: string, range: { first: string; last: string } | null): boolean {
    if (!range) return this._pageIndex === 0; // an empty page 0 accepts anything.
    const { first, last } = range;
    if (this.viewState.sortDir === 'desc') {
      return this._pageIndex === 0 ? k >= first : k <= first && k >= last;
    }
    return this._pageIndex === 0 ? k <= first : k >= first && k <= last;
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
          // Chip iff the new key would move it off this page (design §6.2).
          const newK = updatedKey(agent);
          if (!this.withinPageKRange(newK, rangeBefore)) chip = true;
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
    const prevPhase = this.memberIndex.getPhase(id);
    const prevPassed =
      wasMember && (!this.viewState.phaseFilter || prevPhase === this.viewState.phaseFilter);
    const nowPasses = this.passesPhase(agent);

    if (wasMember) {
      // An existing off-page member's phase changes unconditionally (round 1
      // review B6: only *adding* a new member is gated by the add rule).
      this.memberIndex.set(id, agent.phase);
    } else if (this.passesCommittedLabel(agent)) {
      // A genuinely new member under today's add rule (design §6.2; round 1
      // review B6) — e.g. a `created` event, which `AgentsChangedDetail`
      // cannot distinguish from any other upsert, so the add rule itself is
      // the gate against inflating stats for an out-of-label agent.
      this.memberIndex.set(id, agent.phase);
    } else {
      // Not a member and does not pass the add rule: never added, and the
      // off-page member-index update below never applies to it. Still
      // raises the chip, like a delta for a project-scope agent generally.
      return true;
    }

    const newlyPasses = nowPasses && !prevPassed;
    const enteredRange = this.withinPageKRange(updatedKey(agent), rangeBefore);
    // "Off-page member change affecting counts only" shows no chip (design §6.2).
    return newlyPasses || enteredRange;
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
