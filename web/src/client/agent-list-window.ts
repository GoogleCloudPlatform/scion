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
 */

import type { Agent, AgentPhase } from '../shared/types.js';
import { isAgentRunning } from '../shared/types.js';
import type { AgentSortField, SortDir } from '../shared/agent-sort.js';
import { sortAgents, serverOrderCompare } from '../shared/agent-sort.js';
import type { AgentsChangedDetail } from './state.js';
import { AgentMemberIndex } from './agent-member-index.js';

export type WindowState = 'small' | 'paged';

export interface AgentListViewState {
  phaseFilter: AgentPhase | '';
  /** The *committed* label filter (`sl-change`/`sl-clear`), or the live-typed one for local filtering in the small state. */
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
  fetchPage: PagedPageFetcher;
  /** Full `Agent` lookup for an upserted ID (design §6.2 on-page replace). Typically `stateManager.getAgent`. */
  getAgent: (id: string) => Agent | undefined;
}

/** Dispatched whenever rendered state changes outside of a direct caller action (a background page fetch, a live update, or a resync). */
export type AgentListWindowChangeEvent = Event;

export class AgentListWindow extends EventTarget {
  readonly memberIndex = new AgentMemberIndex();

  private _state: WindowState = 'small';
  private heldAgents: Agent[] = [];
  private pageItems: Agent[] = [];
  private cursors: Array<string | undefined> = [undefined];
  private _pageIndex = 0;
  private _totalCount = 0;
  private _hasNext = false;
  private _loading = false;
  private _error: string | null = null;
  private _updatesAvailable = false;
  private generation = 0;

  private viewState: AgentListViewState;
  private readonly fetchPage: PagedPageFetcher;
  private readonly getAgent: (id: string) => Agent | undefined;

  constructor(options: AgentListWindowOptions) {
    super();
    this.viewState = options.viewState;
    this.fetchPage = options.fetchPage;
    this.getAgent = options.getAgent;
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

  /** Adopt a complete set: fit `complete: true`, or a legacy page with no `nextCursor` (design §4.3). */
  setSmall(agents: Agent[]): void {
    this.generation++;
    this._state = 'small';
    this.heldAgents = agents;
    this._pageIndex = 0;
    this._updatesAvailable = false;
    this._error = null;
    this._loading = false;
    this.memberIndex.seed(agents.map((a) => [a.id, a.phase] as const));
  }

  /** Adopt the first paged response: fit `complete: false` (design §4.3). */
  setPaged(result: PagedPageResult): void {
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
    return this.filteredAndSorted(this.heldAgents);
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
      return {
        total: this.heldAgents.length,
        running: this.heldAgents.filter((a) => isAgentRunning(a)).length,
      };
    }
    return this.memberIndex.stats;
  }

  /**
   * A sort, phase, page-size or label change. Always resets to page 0
   * (design §6.3).
   *
   * In the small state this is purely local — 0 requests, regardless of
   * field. In the paged state, a sort/phase/page-size change starts a
   * fresh request for the new page 0, since the server pages are specific
   * to the previous sort/phase and there is no complete set to re-slice
   * locally (out of the P1 gate's tested scope; see §11 P1c interim-costs
   * note). A **label** change is never a reason to refetch by itself — it
   * only takes effect through a committed label's own trigger request
   * (design §4.3, §6.3); pass `refetchIfPaged: false` for live label
   * typing so a keystroke never issues a request even while paged (design
   * §6.4 row 8).
   */
  setViewState(
    partial: Partial<AgentListViewState>,
    opts: { refetchIfPaged?: boolean } = {}
  ): void {
    this.viewState = { ...this.viewState, ...partial };
    this._pageIndex = 0;
    const refetchIfPaged = opts.refetchIfPaged ?? true;
    if (this._state === 'paged' && refetchIfPaged) {
      void this.fetchPageAt(0);
    } else {
      this.notifyChange();
    }
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

  /**
   * Apply a coalesced `agents-changed` flush (design §6.2's table). A no-op
   * in the small state, which is driven by `onAgentsUpdated` over
   * `this.agents` instead (design §11 P1c).
   */
  applyChanges(detail: AgentsChangedDetail): void {
    if (this._state !== 'paged') return;

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

    const passesPhase = (a: Agent): boolean =>
      !this.viewState.phaseFilter || a.phase === this.viewState.phaseFilter;

    for (const id of detail.upserted) {
      const agent = this.getAgent(id);
      if (!agent) continue;
      if (onPage.has(id)) {
        if (passesPhase(agent)) {
          // Replace this object, then re-sort the page locally (today's live reorder, Q-D).
          onPage.set(id, agent);
          resort = true;
        } else {
          // On-page agent now fails the phase filter: remove the row (backfill chip).
          onPage.delete(id);
          chip = true;
        }
      } else {
        // Off-page upsert, including of an ID already known from elsewhere
        // (R2-B2): member-index only, chip to signal a possible membership change.
        chip = true;
      }
      this.memberIndex.set(id, agent.phase);
    }

    for (const [id, delta] of detail.unknown) {
      if (onPage.has(id)) continue; // on-page agents are always known already.
      if (this.memberIndex.has(id)) {
        if (delta.phase) this.memberIndex.set(id, delta.phase);
        chip = true;
      }
      // Neither on-page nor in the member index: ignored (design §6.2).
    }

    if (resort || onPage.size !== pageSizeBefore) {
      this.pageItems = Array.from(onPage.values()).sort((a, b) =>
        serverOrderCompare(a, b, this.viewState.sortDir)
      );
    }

    if (chip) this._updatesAvailable = true;
    if (chip || resort) this.notifyChange();
  }

  private notifyChange(): void {
    this.dispatchEvent(new Event('change'));
  }
}
