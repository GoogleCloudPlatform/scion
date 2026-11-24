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

import { LitElement, html, nothing, type PropertyValues } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import type { PageData, Agent } from '../../shared/types.js';
import { apiFetch } from '../../client/api.js';
import { current, projectTrail, type Named } from '../../client/crumbs.js';
import { projectLabelNow, resolveProjectLabel } from '../../client/project-label.js';
import '../shared/breadcrumb.js';
import { stateManager } from '../../client/state.js';
import { READINESS_MARKS, markReady, resetReadinessMarks } from '../../client/readiness-marks.js';
import type { AgentsChangedDetail } from '../../client/state.js';
import { AgentSeedEpoch } from '../../client/agent-seed-epoch.js';
import {
  AgentDrainRunner,
  DRAIN_CONNECT_TIMEOUT_MS,
  type SeededDrainResult,
} from '../../client/agent-drain.js';
import { cappedTotalText, failedTotalText } from '../../client/agent-list-window.js';
import type { Orientation } from '../../shared/lineage.js';
import type { ViewMode } from '../shared/view-toggle.js';
import '../shared/view-toggle.js';
import '../shared/agent-tree-view.js';
import type { ScionAgentTreeView } from '../shared/agent-tree-view.js';
import '../shared/filter-box/filter-box.js';
import type { FilterCommitDetail, FilterInputDetail } from '../shared/filter-box/filter-box.js';
import {
  canonical,
  filtersQueryDialectEnabled,
  getViewSchema,
  knownKeysFor,
  parse,
  validate,
  type Query,
  type ViewSchema,
} from '../../shared/filter-query/index.js';
import {
  filterGraphAgents,
  formatProjectFilterTerm,
  makeGraphValueSuggestions,
  migrateProjectParamToQuery,
  resolveProjectLoadTarget,
} from './agent-graph-filter.js';
import { GraphPaletteController } from '../shared/palette/graph-palette-controller.js';
import { navigateTo, replaceSearch } from '../../client/navigation.js';

const GRAPH_FILTER_SCHEMA: ViewSchema = getViewSchema('graph') ?? {
  view: 'graph',
  keys: [],
  textHelp: '',
};

/**
 * The fit probe of a scoped entry: one agent, plus the complete set (in
 * the compact view) when every dashboard agent fits in 500.
 */
export const GRAPH_PROBE_URL = '/api/v1/agents?sort=updated&dir=desc&limit=1&fit=500&view=compact';

const BANNER_STYLE =
  'display: flex; align-items: center; gap: 0.75rem; margin-bottom: 0.75rem; padding: 0.5rem 0.75rem; border-radius: var(--sl-border-radius-medium, 4px); background: var(--sl-color-warning-50); color: var(--sl-color-warning-800);';

/** The legacy agent list the graph drains (the drain adds `limit`, `cursor` and `view`). */
const GRAPH_AGENTS_URL = '/api/v1/agents';

/** The parts of the probe response the graph reads. */
interface ProbeResponse {
  agents?: Agent[];
  complete?: boolean;
}

/** Why the shown graph is not the complete set of its scope. */
interface GraphIncomplete {
  reason: 'capped' | 'failed';
  /** Agents the drain loaded. */
  loaded: number;
  /** The drain covered every project (so a project filter can narrow it). */
  unscoped: boolean;
  /** A re-drain of the same scope failed and the graph shown before it is still shown. */
  keptPrevious: boolean;
  /**
   * The graph still shown is the complete set of its scope (only its
   * re-drain failed), so no ancestor is missing from it.
   */
  shownComplete: boolean;
}

/** A capped unscoped drain, kept live for the page lifetime. */
interface CappedSet {
  members: Map<string, Agent>;
  loaded: number;
  /** Live changes to this set may have been missed (a resync, or a late connect). */
  stale: boolean;
}

/**
 * Standalone agent graph page — loads its own data and owns a cross-project
 * filter dropdown. Delegates all graph rendering to <scion-agent-tree-view>.
 *
 * This page remains useful for deep-linkable, cross-project graph views
 * (e.g. /agents/graph?project=<id>&focus=<agent-id>).  When the user picks
 * grid or list from the toggle the page navigates back to the appropriate
 * agents page.
 *
 * The live (SSE) scope is always the dashboard scope; the project filter is
 * applied on the client. Loading, by what the state store already holds:
 * - the store holds the complete dashboard set (`isAgentSetComplete
 *   ('compact')`): the graph renders it, with no request;
 * - unscoped: a compact drain of every agent; a complete, uncapped drain
 *   marks the store's set complete;
 * - scoped to a project: one fit probe; a complete answer is the whole set
 *   (and marks it complete), otherwise only that project is drained.
 *
 * Membership is the loaded set plus agents created live in the loaded scope
 * (every project, or the scoped project), minus agents deleted live. A
 * capped or failed drain, and a live connection that dropped or came up
 * late, are shown in a banner with a Retry or Refresh that drains again.
 */
@customElement('scion-page-agent-graph')
export class AgentGraphPage extends LitElement {
  @property({ type: Object }) pageData?: PageData;

  /** The loaded membership (every project, or one project; see `memberScope`). */
  @state() private agents: Agent[] = [];
  @state() private loading = true;
  /** A Retry or Refresh is draining while the current graph stays shown. */
  @state() private reloading = false;
  @state() private error: string | null = null;
  @state() private projectFilter = '';
  @state() private filterQueryText = '';
  @state() private committedFilterKey = '';
  @state() private activeFilterQuery: Query | null = null;
  @state() private projectNamed: Named | null = null;
  private resolvedProjectId = '';
  /** Graph flow direction, persisted in the URL as ?dir=horizontal (absent = vertical). */
  @state() private orientation: Orientation = 'vertical';
  /** Set when the shown graph is not the complete set of its scope. */
  @state() private incomplete: GraphIncomplete | null = null;
  /** Live changes may have been missed (a resync, or a late first connect). */
  @state() private stale = false;
  /** True when the page was entered with a ?project= param already set in the URL. */
  private initiallyProjectScoped = false;
  /** Agent ID to center on load (?focus=<agent-id> deep link) */
  private focusId = '';

  /** What `agents` covers: `''` every project, a project ID that project, `null` nothing yet. */
  private memberScope: string | null = null;
  /** The normalised project scope currently loaded or in flight (`''` all, project ID/slug, or `null`). */
  private loadTarget: string | null = null;
  /** The fit probe was sent; it is never repeated in the page lifetime. */
  private probed = false;
  /** A load already showed more than 500 agents, so a probe cannot be complete. */
  private knownLarge = false;
  /** The capped unscoped drain, reused for "all projects" with no request. */
  private cappedAll: CappedSet | null = null;
  private drainRunner = new AgentDrainRunner();
  private probeController: AbortController | null = null;
  /** Bumped by every load and picker change; a load whose number is stale is discarded. */
  private loadSeq = 0;
  /** IDs created live since the last `agents-changed`. */
  private pendingCreated = new Set<string>();
  private connectTimer: ReturnType<typeof setTimeout> | undefined;
  /** Counts attachments, so a previous attachment's connect wait is ignored. */
  private attachment = 0;
  /** The first live connection of this attachment is up. */
  private firstConnected = false;
  /** The first live connection was not up within the drain's connect timeout. */
  private firstConnectLate = false;

  private readonly valueSuggestions = makeGraphValueSuggestions(() => [
    ...this.agents,
    ...stateManager.getAgents(),
  ]);

  /**
   * "Jump to agent" over the graph. With `filters.query_dialect` on, keeps
   * searching all loaded agents even when a filter hides some of them; picking
   * a hidden agent clears the filter so it can be revealed.
   */
  readonly graphPalette = new GraphPaletteController(this, {
    treeView: (): ScionAgentTreeView | null =>
      this.renderRoot.querySelector('scion-agent-tree-view'),
    candidates: (): readonly Agent[] | undefined =>
      filtersQueryDialectEnabled() ? this.agents : undefined,
    onSelectHidden: (agentId: string): boolean => {
      if (!filtersQueryDialectEnabled()) return false;
      const target = this.agents.find((a) => a.id === agentId);
      if (target && this.memberScope && this.memberScope === target.projectId) {
        // Keep the active project scope so we don't trigger a full re-drain
        // when the hidden agent is already in the loaded project set.
        const projectTerm = formatProjectFilterTerm(target.projectSlug || target.projectId);
        const parsed = this.parseValidQuery(projectTerm);
        this.filterQueryText = projectTerm;
        this.activeFilterQuery = parsed;
        this.committedFilterKey = parsed ? canonical(parsed) : '';
        const params = new URLSearchParams(window.location.search);
        params.set('q', projectTerm);
        params.delete('project');
        replaceSearch(params);
      } else {
        this.onClearFilter();
      }
      void this.updateComplete.then(() => {
        this.renderRoot.querySelector('scion-agent-tree-view')?.revealAgent(agentId);
      });
      return true;
    },
  });

  private readonly onAgentCreated = (e: Event): void => {
    const id = (e as CustomEvent<{ data?: { agentId?: string } }>).detail?.data?.agentId;
    if (id) this.pendingCreated.add(id);
  };

  private readonly onAgentsChanged = (e: Event): void => {
    const detail = (e as CustomEvent<{ data?: Partial<AgentsChangedDetail> }>).detail?.data;
    const created = this.pendingCreated;
    this.pendingCreated = new Set();
    if (!detail) return;
    if (this.cappedAll) this.applyDelta(this.cappedAll.members, '', detail, created);
    if (this.memberScope === null) return;
    const members = new Map(this.agents.map((a) => [a.id, a]));
    if (this.applyDelta(members, this.memberScope, detail, created)) {
      this.agents = Array.from(members.values());
    }
  };

  private readonly onAgentsResync = (): void => {
    if (this.memberScope !== null) this.stale = true;
    if (this.cappedAll) this.cappedAll.stale = true;
  };

  override connectedCallback(): void {
    super.connectedCallback();
    // Always the dashboard scope: a project scope would clear the store's
    // complete dashboard set, which later picker changes and other pages
    // reuse.
    stateManager.setScope({ type: 'dashboard' });

    const params = new URLSearchParams(window.location.search);
    const useDialect = filtersQueryDialectEnabled();
    let initialFallbackProject = '';
    if (useDialect) {
      const { queryText, migrated, initialProject } = migrateProjectParamToQuery(
        params,
        stateManager.getAgents(),
        stateManager.getProjects()
      );
      this.initiallyProjectScoped = migrated;
      this.filterQueryText = queryText;
      this.activeFilterQuery = this.parseValidQuery(queryText);
      this.committedFilterKey = this.activeFilterQuery ? canonical(this.activeFilterQuery) : '';
      initialFallbackProject = initialProject;
      if (migrated) {
        if (queryText) params.set('q', queryText);
        else params.delete('q');
        params.delete('project');
        replaceSearch(params);
      }
    } else {
      this.projectFilter = params.get('project') || '';
      this.initiallyProjectScoped = !!params.get('project');
    }
    this.syncProjectLabel();
    this.focusId = params.get('focus') || '';
    this.orientation = params.get('dir') === 'horizontal' ? 'horizontal' : 'vertical';

    stateManager.addEventListener('agent-created', this.onAgentCreated);
    stateManager.addEventListener('agents-changed', this.onAgentsChanged);
    stateManager.addEventListener('agents-resync', this.onAgentsResync);

    this.watchFirstConnect();
    if (useDialect) {
      this.syncLoadScopeForQuery(this.activeFilterQuery, {
        initial: true,
        fallbackProjectId: initialFallbackProject,
      });
    } else if (stateManager.isAgentSetComplete('compact')) {
      this.adoptHeldSet();
    } else {
      void this.load(this.projectFilter, false);
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('agent-created', this.onAgentCreated);
    stateManager.removeEventListener('agents-changed', this.onAgentsChanged);
    stateManager.removeEventListener('agents-resync', this.onAgentsResync);
    this.loadSeq++;
    this.drainRunner.abort();
    this.probeController?.abort();
    this.probeController = null;
    if (this.connectTimer !== undefined) {
      clearTimeout(this.connectTimer);
      this.connectTimer = undefined;
    }
    this.resetPageState();
  }

  /**
   * Forget everything learned in this attachment: the capped set stops
   * receiving live changes while detached, and the probe and size facts
   * may no longer hold, so a re-attached page loads from scratch.
   */
  private resetPageState(): void {
    this.agents = [];
    this.memberScope = null;
    this.loadTarget = null;
    this.incomplete = null;
    this.stale = false;
    this.error = null;
    this.loading = true;
    this.reloading = false;
    this.probed = false;
    this.knownLarge = false;
    this.cappedAll = null;
    this.pendingCreated.clear();
    this.firstConnected = false;
    this.firstConnectLate = false;
  }

  /**
   * A live connection that is not up within the drain's connect timeout
   * may have missed changes to a set the graph did not drain itself (the
   * store's complete set, or the probe's answer): the stale banner shows.
   * A drain reports its own late connect.
   */
  private watchFirstConnect(): void {
    // A new attachment starts unconnected, whatever an earlier one saw.
    this.firstConnected = false;
    this.firstConnectLate = false;
    if (this.connectTimer !== undefined) clearTimeout(this.connectTimer);
    this.connectTimer = undefined;
    const gen = stateManager.scopeGeneration;
    const attachment = ++this.attachment;
    let settled = false;
    this.connectTimer = setTimeout(() => {
      this.connectTimer = undefined;
      if (settled || !this.isConnected) return;
      this.firstConnectLate = true;
      if (!this.drainRunner.running) this.stale = true;
    }, DRAIN_CONNECT_TIMEOUT_MS);
    stateManager.sseConnected(gen).then(
      () => {
        settled = true;
        // An earlier attachment's connect must not touch this one's state.
        if (attachment !== this.attachment) return;
        this.firstConnected = true;
        if (this.connectTimer !== undefined) clearTimeout(this.connectTimer);
        this.connectTimer = undefined;
      },
      () => {
        // The scope changed: this page is gone.
      }
    );
  }

  /** Render the store's complete dashboard set, filtered on the client. */
  private adoptHeldSet(): void {
    this.agents = stateManager.getAgents();
    this.memberScope = '';
    this.loadTarget = '';
    this.incomplete = null;
    this.error = null;
    this.loading = false;
    this.pendingCreated.clear();
    markReady(READINESS_MARKS.agentsData, 'graph');
  }

  /**
   * Apply one coalesced live change to `members`, the membership of
   * `scope` (`''` every project). Deletes leave; members take the store's
   * current object; an agent created live joins when it belongs to the
   * scope. An agent changed live that is not a member stays out (it may lie
   * beyond a capped drain). Returns whether `members` changed.
   */
  private applyDelta(
    members: Map<string, Agent>,
    scope: string,
    detail: Partial<AgentsChangedDetail>,
    created: Set<string>
  ): boolean {
    const tombstones = stateManager.getDeletedAgentIds();
    let changed = false;
    for (const id of detail.deleted ?? []) {
      if (members.delete(id)) changed = true;
    }
    const ids = new Set<string>([...(detail.upserted ?? []), ...created]);
    for (const id of ids) {
      // Defensive: the store drops a live create for a tombstoned ID.
      if (tombstones.has(id)) continue;
      const agent = stateManager.getAgent(id);
      if (!agent) continue;
      if (members.has(id)) {
        if (members.get(id) !== agent) {
          members.set(id, agent);
          changed = true;
        }
      } else if (created.has(id) && (scope === '' || agent.projectId === scope)) {
        members.set(id, agent);
        changed = true;
      }
    }
    return changed;
  }

  /**
   * Load the graph for `target` (`''` every project). `keepShown` keeps the
   * current graph on screen while it loads (Retry and Refresh); otherwise
   * the spinner shows.
   */
  private async load(target: string, keepShown: boolean): Promise<void> {
    const seq = ++this.loadSeq;
    this.loadTarget = target;
    this.drainRunner.abort();
    this.probeController?.abort();
    if (keepShown) this.reloading = true;
    else this.loading = true;
    this.error = null;
    try {
      if (target === '') {
        await this.drainUnscoped(seq);
      } else if (!this.probed && !this.knownLarge) {
        const probe = await this.probe(seq);
        if (probe === 'not-complete') await this.drainProject(target, seq);
      } else {
        await this.drainProject(target, seq);
      }
    } catch (err) {
      if (seq === this.loadSeq) {
        console.error('Failed to load agents:', err);
        this.showError(err instanceof Error ? err.message : 'Failed to load agents');
      }
    } finally {
      if (seq === this.loadSeq) {
        this.loading = false;
        this.reloading = false;
      }
    }
  }

  /**
   * The fit probe. Complete only when the response says `complete: true`
   * (a server without sorted mode ignores `sort` and `fit` and answers
   * with a legacy page, which never counts as complete); then it is the
   * whole dashboard set, which the store merges and marks complete.
   */
  private async probe(seq: number): Promise<'complete' | 'not-complete' | 'superseded'> {
    this.probed = true;
    // A probe sent before the first connect came up may predate changes the
    // live connection never delivered.
    const connectedAtSend = this.firstConnected;
    const controller = new AbortController();
    this.probeController = controller;
    const epoch = new AgentSeedEpoch();
    try {
      let body: ProbeResponse | Agent[];
      try {
        const resp = await apiFetch(GRAPH_PROBE_URL, { signal: controller.signal });
        if (seq !== this.loadSeq) return 'superseded';
        if (!resp.ok) return 'not-complete';
        body = (await resp.json()) as ProbeResponse | Agent[];
      } catch (err) {
        if (seq !== this.loadSeq || controller.signal.aborted) return 'superseded';
        console.warn('Agent graph probe failed; loading the project instead:', err);
        return 'not-complete';
      }
      if (seq !== this.loadSeq) return 'superseded';
      // A complete: false answer needs no knownLarge: probed already stops a second probe.
      if (Array.isArray(body) || body.complete !== true) return 'not-complete';
      const seeded = epoch.seed(body.agents ?? [], { partial: true, isMember: () => true });
      const lateConnect = !connectedAtSend && this.firstConnectLate;
      this.adopt(seeded.agents, '', null, epoch.sawResync || lateConnect);
      stateManager.markAgentSetComplete('compact');
      return 'complete';
    } finally {
      epoch.close();
      if (this.probeController === controller) this.probeController = null;
    }
  }

  /** Drain every agent (compact). A complete, uncapped drain marks the store's set complete. */
  private async drainUnscoped(seq: number): Promise<void> {
    const result = await this.drainRunner.run({
      url: GRAPH_AGENTS_URL,
      view: 'compact',
      isMember: () => true,
    });
    if (!result || seq !== this.loadSeq) return;
    if (result.capped || result.requests > 1) this.knownLarge = true;
    if (!this.adoptDrain(result, '')) return;
    if (result.capped) {
      this.cappedAll = {
        members: new Map(result.agents.map((a) => [a.id, a])),
        loaded: result.agents.length,
        stale: result.stale,
      };
    } else {
      // The adopted set is what "all projects" shows now.
      this.cappedAll = null;
    }
    if (result.complete && !result.capped && !result.error) {
      stateManager.markAgentSetComplete('compact');
    }
  }

  /** Drain one project (compact). Never marks the store's set complete. */
  private async drainProject(projectIdOrSlug: string, seq: number): Promise<void> {
    const lower = projectIdOrSlug.toLowerCase();
    const result = await this.drainRunner.run({
      url: `${GRAPH_AGENTS_URL}?projectId=${encodeURIComponent(projectIdOrSlug)}`,
      view: 'compact',
      isMember: (agent) =>
        agent.projectId === projectIdOrSlug || agent.projectSlug?.toLowerCase() === lower,
    });
    if (!result || seq !== this.loadSeq) return;
    // Normalise a slug scope to the canonical project ID returned by the hub
    // so live deltas (agent.projectId === memberScope) and subsequent query
    // commits compare on IDs only.
    const resolvedId = result.agents.find((a) => a.projectId)?.projectId || projectIdOrSlug;
    if (this.adoptDrain(result, resolvedId)) {
      this.loadTarget = resolvedId;
      if (this.projectFilter === projectIdOrSlug) {
        this.projectFilter = resolvedId;
      }
    }
  }

  /**
   * Show a drain result for `scope`. A failure keeps the graph already
   * shown for the same scope (complete, capped or partial) with its banner
   * and a "showing the previous graph" note; a first page that failed with
   * no graph of that scope shows the error. Returns whether the result was
   * adopted as the new membership.
   */
  private adoptDrain(result: SeededDrainResult, scope: string): boolean {
    if (result.error && this.memberScope === scope) {
      // A capped or partial graph keeps its own reason and count; a complete
      // one reports what it shows, never fewer rows than are on screen, and
      // stays marked complete.
      this.incomplete = {
        ...(this.incomplete ?? {
          reason: 'failed',
          loaded: this.agents.length,
          unscoped: scope === '',
          shownComplete: true,
        }),
        keptPrevious: true,
      };
      return false;
    }
    if (result.error && result.firstPageFailed) {
      this.showError(result.error.message || 'Failed to load agents');
      return false;
    }
    const reason = result.capped ? 'capped' : result.error ? 'failed' : null;
    this.adopt(
      result.agents,
      scope,
      reason
        ? {
            reason,
            loaded: result.agents.length,
            unscoped: scope === '',
            keptPrevious: false,
            shownComplete: false,
          }
        : null,
      result.stale
    );
    return true;
  }

  /** The error view, with nothing loaded: live changes are not tracked until a load succeeds. */
  private showError(message: string): void {
    this.error = message;
    this.agents = [];
    this.memberScope = null;
    this.incomplete = null;
  }

  private adopt(
    agents: Agent[],
    scope: string,
    incomplete: GraphIncomplete | null,
    stale: boolean
  ): void {
    this.agents = agents;
    this.memberScope = scope;
    this.incomplete = incomplete;
    this.stale = stale;
    this.error = null;
    this.pendingCreated.clear();
    markReady(READINESS_MARKS.agentsData, 'graph');
  }

  /**
   * A project filter change: the in-flight load is aborted; focus and
   * orientation are kept. A held complete set is re-filtered and a kept
   * capped set reused for "all projects", with no request; otherwise the
   * chosen scope is drained (never probed again).
   */
  private applyProjectFilter(target: string): void {
    // The page keeps the dashboard scope, so a filter change is where the
    // readiness marks' load for the chosen scope starts.
    resetReadinessMarks();
    this.loadSeq++;
    this.drainRunner.abort();
    this.probeController?.abort();
    this.probeController = null;
    this.reloading = false;
    if (stateManager.isAgentSetComplete('compact')) {
      this.adoptHeldSet();
      return;
    }
    if (target === '' && this.cappedAll) {
      const kept = this.cappedAll;
      this.loadTarget = '';
      this.adopt(
        Array.from(kept.members.values()),
        '',
        {
          reason: 'capped',
          loaded: kept.loaded,
          unscoped: true,
          keptPrevious: false,
          shownComplete: false,
        },
        kept.stale
      );
      this.loading = false;
      return;
    }
    void this.load(target, false);
  }

  /** Retry or Refresh: drain the scope the graph shows again, keeping it on screen. */
  private onReload(): void {
    if (this.reloading) return;
    if (this.memberScope === null || this.error) {
      void this.load(this.projectFilter, false);
      return;
    }
    void this.load(this.memberScope, true);
  }

  /** Agents after the project filter is applied */
  private get visibleAgents(): Agent[] {
    if (!this.projectFilter) return this.agents;
    return this.agents.filter((a) => a.projectId === this.projectFilter);
  }

  private get projects(): Array<{ id: string; name: string }> {
    // The store holds every agent loaded in this page lifetime (the
    // unscoped set too), so a project-scoped membership still offers every
    // known project.
    const seen = new Map<string, string>();
    for (const agent of [...this.agents, ...stateManager.getAgents()]) {
      if (agent.projectId && !seen.has(agent.projectId)) {
        seen.set(agent.projectId, agent.project || agent.projectId);
      }
    }
    return Array.from(seen, ([id, name]) => ({ id, name })).sort((a, b) =>
      a.name.localeCompare(b.name)
    );
  }

  private onProjectFilterChange(e: Event): void {
    this.setProjectFilter((e.target as HTMLSelectElement & { value: string }).value);
  }

  /** Change the project filter (`''` every project), keeping the URL in sync. */
  private setProjectFilter(value: string): void {
    if (value === this.projectFilter) return;
    this.projectFilter = value;
    this.applyProjectFilter(value);
    const params = new URLSearchParams(window.location.search);
    if (value) {
      params.set('project', value);
    } else {
      params.delete('project');
    }
    replaceSearch(params);
  }

  private parseValidQuery(raw: string): Query | null {
    const trimmed = raw.trim();
    if (!trimmed) return null;
    const parsed = parse(raw, knownKeysFor(GRAPH_FILTER_SCHEMA));
    if (parsed.error || !parsed.query) return null;
    const v = validate(parsed.query, GRAPH_FILTER_SCHEMA, raw);
    if (v.error) return null;
    return parsed.query;
  }

  /**
   * Single helper that decides the load scope from a query and reconciles
   * what the page has loaded (or is loading) against that normalised target.
   * Called from connect, commit, and clear.
   */
  private syncLoadScopeForQuery(
    query: Query | null,
    opts?: { initial?: boolean; fallbackProjectId?: string }
  ): void {
    const target = resolveProjectLoadTarget(
      query,
      [...this.agents, ...stateManager.getAgents()],
      stateManager.getProjects(),
      opts?.fallbackProjectId
    );
    this.projectFilter = target;
    if (opts?.initial) {
      if (stateManager.isAgentSetComplete('compact')) {
        this.adoptHeldSet();
      } else {
        void this.load(target, false);
      }
      return;
    }
    if (target !== this.loadTarget) {
      this.applyProjectFilter(target);
    }
  }

  private onFilterInput(e: CustomEvent<FilterInputDetail>): void {
    this.filterQueryText = e.detail.value;
    this.activeFilterQuery = e.detail.query;
  }

  private onFilterCommit(e: CustomEvent<FilterCommitDetail>): void {
    this.filterQueryText = e.detail.value;
    this.activeFilterQuery = e.detail.query;
    this.committedFilterKey = canonical(e.detail.query);
    const params = new URLSearchParams(window.location.search);
    const trimmed = e.detail.value.trim();
    if (trimmed) {
      params.set('q', trimmed);
    } else {
      params.delete('q');
    }
    params.delete('project');
    replaceSearch(params);
    this.syncLoadScopeForQuery(e.detail.query);
  }

  private onClearFilter(): void {
    this.filterQueryText = '';
    this.committedFilterKey = '';
    this.activeFilterQuery = null;
    const params = new URLSearchParams(window.location.search);
    params.delete('q');
    params.delete('project');
    replaceSearch(params);
    this.syncLoadScopeForQuery(null);
  }

  /** Keeps the URL's ?dir= param in sync with the graph orientation toggle. */
  private onOrientationChange(e: CustomEvent<{ orientation: Orientation }>): void {
    this.orientation = e.detail.orientation;
    const params = new URLSearchParams(window.location.search);
    if (this.orientation === 'horizontal') {
      params.set('dir', 'horizontal');
    } else {
      params.delete('dir');
    }
    replaceSearch(params);
  }

  /** Grid/list picks from the toggle navigate back to the agents list. */
  private onViewChange(e: CustomEvent<{ view: ViewMode }>): void {
    const mode = e.detail.view;
    if (mode === 'graph') return;
    if (this.initiallyProjectScoped && this.projectFilter) {
      localStorage.setItem('scion-view-project-agents', mode);
      navigateTo(`/projects/${this.projectFilter}`);
    } else {
      localStorage.setItem('scion-view-agents', mode);
      navigateTo('/agents');
    }
  }

  /** The banner text of an incomplete graph. */
  private incompleteText(i: GraphIncomplete): string {
    let text =
      i.reason === 'capped'
        ? `Graph incomplete: ${cappedTotalText(i.loaded)}`
        : failedTotalText(i.loaded);
    if (i.reason === 'capped' && i.unscoped) text += ' · narrow with a project filter';
    return i.keptPrevious ? `${text} · showing the previous graph` : text;
  }

  /** The incomplete (Retry) and may-be-stale (Refresh) banners. Neither issues a request until clicked. */
  private renderBanners() {
    const i = this.incomplete;
    return html`
      ${i
        ? html`<div class="graph-banner graph-incomplete" role="status" style=${BANNER_STYLE}>
            <span>${this.incompleteText(i)}</span>
            <sl-button
              size="small"
              ?disabled=${this.reloading}
              ?loading=${this.reloading}
              @click=${() => this.onReload()}
            >
              Retry
            </sl-button>
          </div>`
        : nothing}
      ${this.stale
        ? html`<div class="graph-banner graph-stale" role="status" style=${BANNER_STYLE}>
            <span>Graph may be stale</span>
            <sl-button
              size="small"
              ?disabled=${this.reloading}
              ?loading=${this.reloading}
              @click=${() => this.onReload()}
            >
              Refresh
            </sl-button>
          </div>`
        : nothing}
    `;
  }

  protected override willUpdate(changed: PropertyValues): void {
    super.willUpdate(changed);
    if (changed.has('projectFilter')) {
      this.syncProjectLabel();
    }
  }

  private syncProjectLabel(): void {
    const id = (this.projectFilter ?? '').trim();
    if (!id) {
      this.projectNamed = null;
      this.resolvedProjectId = '';
      return;
    }
    if (id === this.resolvedProjectId) return;
    this.resolvedProjectId = id;
    this.projectNamed = projectLabelNow(id);
    void resolveProjectLabel(id).then((resolved) => {
      if (this.isConnected && (this.projectFilter ?? '').trim() === id) {
        this.projectNamed = resolved;
      }
    });
  }

  private graphCrumbs() {
    const id = this.projectFilter.trim();
    const ancestorsOnly = this.loading || Boolean(this.error);
    if (id) {
      const project = this.projectNamed?.id === id ? this.projectNamed : projectLabelNow(id);
      return ancestorsOnly
        ? projectTrail(project)
        : [...projectTrail(project), current('Agent graph', false)];
    }
    const root = [{ label: 'Agents', href: '/agents' }];
    return ancestorsOnly ? root : [...root, current('Graph', false)];
  }

  override render() {
    const useDialect = filtersQueryDialectEnabled();
    const filtered = useDialect ? filterGraphAgents(this.agents, this.activeFilterQuery) : null;
    const shownAgents = filtered ? filtered.agents : this.visibleAgents;
    const contextIds = filtered ? filtered.contextIds : undefined;
    const filteredEmpty = Boolean(
      filtered && filtered.hasActiveFilter && this.agents.length > 0 && shownAgents.length === 0
    );
    const treeFilterKey = useDialect ? this.committedFilterKey : this.projectFilter;

    return html`
      <div style="padding: var(--sl-spacing-large, 1.25rem);">
        <scion-breadcrumb .crumbs=${this.graphCrumbs()}></scion-breadcrumb>
        <div
          style="display: flex; align-items: center; justify-content: space-between; gap: 1rem; margin-bottom: 1rem; flex-wrap: wrap;"
        >
          <h1 style="margin: 0; font-size: 1.5rem;">Agents</h1>
          <div
            style="display: flex; align-items: center; gap: 0.75rem; flex-wrap: wrap; flex: 1 1 auto; justify-content: flex-end;"
          >
            ${useDialect
              ? html`
                  <scion-filter-box
                    view="graph"
                    .value=${this.filterQueryText}
                    .valueSuggestions=${this.valueSuggestions}
                    placeholder="Filter graph… (try phase:running harness:antigravity)"
                    @filter-input=${this.onFilterInput}
                    @filter-commit=${this.onFilterCommit}
                  ></scion-filter-box>
                `
              : this.initiallyProjectScoped
                ? nothing
                : html`
                    <sl-select
                      size="small"
                      placeholder="All projects"
                      clearable
                      value=${this.projectFilter}
                      @sl-change=${this.onProjectFilterChange}
                      style="min-width: 180px"
                    >
                      ${this.projects.map(
                        (p) => html`<sl-option value=${p.id}>${p.name}</sl-option>`
                      )}
                    </sl-select>
                  `}
            <scion-view-toggle view="graph" @view-change=${this.onViewChange}></scion-view-toggle>
          </div>
        </div>
        ${this.loading
          ? html`
              <div
                style="display: flex; flex-direction: column; align-items: center; gap: 0.75rem; padding: 3rem 1rem; color: var(--sl-color-neutral-600); text-align: center;"
              >
                <sl-spinner></sl-spinner>
                <p>Loading agents...</p>
              </div>
            `
          : this.error
            ? html`
                <div
                  style="display: flex; flex-direction: column; align-items: center; gap: 0.75rem; padding: 3rem 1rem; color: var(--sl-color-danger-600); text-align: center;"
                >
                  <sl-icon name="exclamation-triangle"></sl-icon>
                  <p>${this.error}</p>
                  <sl-button size="small" @click=${() => this.onReload()}>Retry</sl-button>
                </div>
              `
            : html`
                ${this.renderBanners()}
                <scion-agent-tree-view
                  .agents=${shownAgents}
                  .contextIds=${contextIds}
                  ?filteredEmpty=${filteredEmpty}
                  focusId=${this.focusId}
                  orientation=${this.orientation}
                  filterKey=${treeFilterKey}
                  .markMissingAncestors=${this.incomplete !== null &&
                  !this.incomplete.shownComplete}
                  @orientation-change=${this.onOrientationChange}
                  @clear-filter=${this.onClearFilter}
                ></scion-agent-tree-view>
              `}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-agent-graph': AgentGraphPage;
  }
}
