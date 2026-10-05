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
 * Agents list page component
 *
 * Displays all agents across all projects with their status
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type {
  PageData,
  Agent,
  AgentPhase,
  Capabilities,
  AgentLifecycleAction,
} from '../../shared/types.js';
import {
  can,
  canLifecycle,
  canMessageAgent,
  isTerminalAvailable,
  getAgentDisplayStatus,
  isAgentRunning,
  RESUME_BEST_EFFORT_CONFIRM_MESSAGE,
  lifecycleActionRequestInit,
} from '../../shared/types.js';

import type { AgentSortField, SortDir } from '../../shared/agent-sort.js';
import type { StatusType } from '../shared/status-badge.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { stateManager } from '../../client/state.js';
import type { AgentsChangedDetail } from '../../client/state.js';
import { mergeChanged, dropTombstonedPairs } from '../../client/agent-merge.js';
import { AgentListWindow } from '../../client/agent-list-window.js';
import type {
  AgentListTrigger,
  AgentListView,
  AgentListViewState,
  PagedPageParams,
  PagedPageResult,
} from '../../client/agent-list-window.js';
import {
  AgentDrainRunner,
  DRAIN_PAGE_LIMIT,
  type DrainFirstPage,
} from '../../client/agent-drain.js';
import { AgentSeedEpoch } from '../../client/agent-seed-epoch.js';
import { AGENT_PAGER_PAGE_SIZES } from '../shared/agent-pager.js';
import type { AgentPagerPageSize } from '../shared/agent-pager.js';
import '../shared/agent-pager.js';
import { formatNumber } from '../../utils/format-number.js';
import { listPageStyles } from '../shared/resource-styles.js';
import type { ViewMode } from '../shared/view-toggle.js';
import '../shared/status-badge.js';
import { DeletionLeaseController } from '../shared/deletion-badge.js';
import { readAcceptedDeletion } from '../../shared/agent-deletion.js';
import '../shared/message-mode-badge.js';
import '../shared/messageability-indicator.js';
import '../shared/view-toggle.js';
import '../shared/agent-tree-view.js';
import type { ScionAgentTreeView } from '../shared/agent-tree-view.js';
import { GraphPaletteController } from '../shared/palette/graph-palette-controller.js';
import '../shared/quick-message-dialog.js';
import {
  getDenialMessage,
  MESSAGE_MODE_DISPLAY,
  getMessageModeDisplay,
} from '../../shared/message-mode.js';
import type { MessageMode } from '../../shared/types.js';
import { showToast } from '../../utils/toast.js';
import { stopAllNotices, type StopAllResult } from '../../utils/stop-all.js';
import { showConfirm } from '../shared/confirm-dialog.js';
import { terminalHref } from '../../client/open-terminal.js';
import { formatRelative } from '../../utils/time.js';

/** The fit value of the global page's sorted first request: the largest set loaded in one request. */
const GLOBAL_AGENTS_FIT = 500;

const PAGER_PAGE_SIZE_STORAGE_KEY = 'scion-pagesize-agents';

/** The global endpoint's response: legacy fields, plus the sorted-mode fields when `sort` was sent. */
interface GlobalAgentsResponse {
  agents?: Agent[];
  nextCursor?: string;
  totalCount?: number;
  /** Only when `fit` was sent: whether the whole set fit in this response. */
  complete?: boolean;
  /** Only with `stats=1`. `agents` is omitted when `total` is above 2,000 (count-only). */
  stats?: { total: number; running: number; agents?: Array<[string, string]> };
  _capabilities?: Capabilities;
}

/** The window's layout for a page view mode: the `graph` mode is the agent tree. */
function listViewOf(mode: ViewMode): AgentListView {
  return mode === 'graph' ? 'tree' : mode;
}

/** Whether `agent` carries the committed `k=v` label; any agent passes an empty or bare-key label (it is not sent). */
function matchesCommittedLabel(agent: Agent, label: string): boolean {
  const eq = label.indexOf('=');
  if (eq < 0) return true;
  return agent.labels?.[label.slice(0, eq)] === label.slice(eq + 1);
}

@customElement('scion-page-agents')
export class ScionPageAgents extends LitElement {
  /**
   * Page data from SSR
   */
  @property({ type: Object })
  pageData: PageData | null = null;

  /**
   * Loading state
   */
  @state()
  private loading = true;

  /**
   * Agents list
   */
  @state()
  private agents: Agent[] = [];

  /**
   * Error message if loading failed
   */
  @state()
  private error: string | null = null;

  /**
   * Loading state for actions
   */
  @state()
  private actionLoading: Record<string, boolean> = {};

  /**
   * Loading state for stop-all action
   */
  @state()
  private stopAllLoading = false;

  /**
   * Scope-level capabilities from the agents list response
   */
  @state()
  private scopeCapabilities: Capabilities | undefined;

  /**
   * Current view mode (grid or list)
   */
  @state()
  private viewMode: ViewMode = 'grid';

  /**
   * One lease timer for this page's deleting agents (ptone/scion#2483 N4).
   * While paged, `this.agents` is empty and the rows are the window's page.
   */
  private readonly deletionLease = new DeletionLeaseController(this, () =>
    this.agentWindow.state === 'paged' ? this.agentWindow.items : this.agents
  );

  /** "Jump to agent" over the graph, offering the agents its tree view shows. */
  readonly graphPalette = new GraphPaletteController(this, {
    treeView: (): ScionAgentTreeView | null =>
      this.renderRoot.querySelector('scion-agent-tree-view'),
  });

  /**
   * Filter scope: 'all' (no filter), 'mine' (created by me), 'shared' (in shared projects)
   */
  @state()
  private agentScope: 'all' | 'mine' | 'shared' = 'all';

  /**
   * The scope `this.agents` was actually fetched for — set alongside
   * `this.agents` itself (never alongside `agentScope`, which changes
   * synchronously on click while the new list is still in flight). The graph
   * view's filterKey reads this, not `agentScope`, so a scope switch isn't
   * mistaken for a delete before the new list lands.
   */
  @state()
  private loadedScope: 'all' | 'mine' | 'shared' = 'all';

  @state()
  private phaseFilter: AgentPhase | '' = '';

  /** The label input's live value: filters what is loaded while typing, with no request. */
  @state()
  private labelFilter = '';

  /**
   * The label requests are sent with: set from the input only on
   * `sl-change` or `sl-clear`, which is a refresh. Sent only when it
   * contains `=`.
   */
  private committedLabel = '';

  @state()
  private modeFilter = '';

  @state()
  private sortField: AgentSortField = 'updated';

  @state()
  private sortDir: SortDir = 'desc';

  @state()
  private quickMessageAgentId = '';

  @state()
  private quickMessageAgentName = '';

  @state()
  private quickMessageOpen = false;

  /**
   * Whether an agents load (a fit request or a drain) is in flight.
   * Ref-counted, so an older load finishing cannot clear it while a newer
   * one is still running. Disables pager navigation and the chip while
   * paged, and shows "Loading agents…" in place of rows that need the
   * complete set.
   */
  @state()
  private agentsLoading = false;

  private agentsLoadingCount = 0;

  /** Page size of the grid and list pager, persisted under `scion-pagesize-agents`. */
  @state()
  private pagerPageSize: AgentPagerPageSize = 25;

  /** Forces a re-render when the window changes outside a `@state` setter. */
  @state()
  private windowTick = 0;

  /**
   * The agent window behind the grid, list and graph views: small, paged,
   * held or capped. The local states read `this.agents` live through
   * `getHeldAgents`. While paged, an agent joins the member index only
   * when the page was loaded for scope `all` (today's add rule).
   */
  private agentWindow = new AgentListWindow({
    viewState: this.windowViewState(),
    getProjectId: (): string => '',
    isAddable: (): boolean => this.loadedScope === 'all',
    fetchPage: (params: PagedPageParams): Promise<PagedPageResult> => this.fetchAgentsPage(params),
    getAgent: (id: string): Agent | undefined => stateManager.getAgent(id),
    getHeldAgents: (): Agent[] => this.agents,
  });

  /** Runs the complete-set drains (complete-needing view states, the held or capped chip). */
  private drainRunner = new AgentDrainRunner();

  private agentsAbortController: AbortController | null = null;
  private agentsLoadGen = 0;
  /** Bumped on every view-state change, so a load can tell the view changed while it was in flight. */
  private viewEpoch = 0;
  /** Only the latest `loadAgents` call clears `loading` or sets `error`. */
  private loadAgentsSeq = 0;

  static override styles = [
    listPageStyles,
    css`
      .agent-header {
        display: flex;
        align-items: flex-start;
        justify-content: space-between;
        margin-bottom: 0.75rem;
        gap: 0.5rem;
        /* Cards are minmax(320px, 1fr), so the header has ~270px to work with
           and the badges claim most of it. Wrapping lets them drop to their own
           line instead of squeezing the name into a few characters. */
        flex-wrap: wrap;
      }

      /* The name column must be allowed to shrink for the wrapping in
         .resource-name to take effect — min-width:auto on this flex item
         would otherwise hold the header open at the full name width. It is
         the only div in the header; the siblings are badge elements.

         flex:1 matters as much as min-width:0. With only min-width:0 the
         column sizes to its content and, because the badges never shrink,
         absorbs the entire overflow — collapsing to a few characters and
         wrapping the meta lines into a narrow ribbon. Growing into the space
         the badges leave keeps names on as few lines as possible. */
      .agent-header > div {
        /* Full-width basis, so the badges always wrap to their own row rather
           than sometimes fitting beside the name and sometimes not. A per-card
           decision left the grid looking ragged — one card with its status
           badge on the title row, the next with it underneath.

           This also removes the crushing problem at its root: the name column
           is never asked to share the row, so it cannot be squeezed down to a
           few characters. Long names still wrap inside .resource-name, which
           keeps its own min-width:0. */
        flex: 1 1 100%;
        min-width: 0;
      }

      /* The badges keep their intrinsic size so the name absorbs the
         shrinking rather than squeezing the status indicators. */
      .agent-header > scion-status-badge,
      .agent-header > scion-message-mode-badge,
      .agent-header > scion-messageability-indicator {
        flex-shrink: 0;
      }

      .agent-meta {
        font-size: 0.813rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.25rem;
        display: flex;
        flex-direction: column;
        gap: 0.125rem;
      }

      .agent-meta sl-icon {
        font-size: 0.875rem;
        vertical-align: -0.125em;
        opacity: 0.7;
      }

      .agent-meta .broker-link {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        color: var(--scion-text-muted, #64748b);
        text-decoration: none;
      }

      .agent-meta .broker-link:hover {
        color: var(--scion-primary, #3b82f6);
      }

      .agent-meta a {
        color: inherit;
        text-decoration: none;
      }

      .agent-meta a:hover {
        text-decoration: underline;
      }

      .agent-task {
        font-size: 0.875rem;
        color: var(--scion-text, #1e293b);
        margin-top: 0.75rem;
        padding: 0.75rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        border-radius: var(--scion-radius, 0.5rem);
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }

      .agent-actions {
        display: flex;
        gap: 0.5rem;
        margin-top: 1rem;
        padding-top: 1rem;
        border-top: 1px solid var(--scion-border, #e2e8f0);
      }

      /* Card-specific: no hover transform for agent cards (they have action buttons) */
      .agent-card {
        background: var(--scion-surface, #ffffff);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-lg, 0.75rem);
        padding: 1.5rem;
        transition: all var(--scion-transition-fast, 150ms ease);
      }

      .agent-card:hover {
        border-color: var(--scion-primary, #3b82f6);
        box-shadow: var(--scion-shadow-md, 0 4px 6px -1px rgba(0, 0, 0, 0.1));
      }

      /* Table-specific: inline action buttons */
      .table-actions {
        display: flex;
        gap: 0.375rem;
        justify-content: flex-end;
      }

      /* Color-coded hover effects for action buttons (skip disabled) */
      .action-btn-danger:not([disabled])::part(base):hover {
        background: var(--scion-action-hover-danger-bg, rgba(239, 68, 68, 0.1));
        border-color: var(--scion-danger-400, #f87171);
        color: var(--scion-danger-600, #dc2626);
      }

      .action-btn-warning:not([disabled])::part(base):hover {
        background: var(--scion-action-hover-warning-bg, rgba(245, 158, 11, 0.1));
        border-color: var(--scion-warning-400, #fbbf24);
        color: var(--scion-warning-600, #d97706);
      }

      .action-btn-success:not([disabled])::part(base):hover {
        background: var(--scion-action-hover-success-bg, rgba(34, 197, 94, 0.1));
        border-color: var(--scion-success-400, #4ade80);
        color: var(--scion-success-600, #16a34a);
      }

      .action-btn-primary:not([disabled])::part(base):hover {
        background: var(--scion-action-hover-primary-bg, rgba(59, 130, 246, 0.1));
        border-color: var(--scion-primary-400, #60a5fa);
        color: var(--scion-primary-600, #2563eb);
      }

      .scope-toggle {
        display: inline-flex;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        overflow: hidden;
      }

      .scope-toggle button {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        height: 2rem;
        border: none;
        background: var(--scion-surface, #ffffff);
        color: var(--scion-text-muted, #64748b);
        cursor: pointer;
        padding: 0 0.625rem;
        font-size: 0.8125rem;
        font-family: inherit;
        transition: all 150ms ease;
        white-space: nowrap;
      }

      .scope-toggle button:not(:last-child) {
        border-right: 1px solid var(--scion-border, #e2e8f0);
      }

      .scope-toggle button:hover:not(.active) {
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .scope-toggle button.active {
        background: var(--scion-primary, #3b82f6);
        color: white;
      }

      .scope-toggle button sl-icon {
        font-size: 0.875rem;
      }

      .project-link {
        color: inherit;
        text-decoration: none;
      }

      .project-link:hover {
        text-decoration: underline;
      }

      .filter-bar {
        display: flex;
        align-items: center;
        gap: 0.75rem;
        margin-bottom: 1rem;
        flex-wrap: wrap;
      }

      .filter-bar .label {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        font-weight: 500;
      }

      th.sortable {
        cursor: pointer;
        user-select: none;
      }

      th.sortable:hover {
        color: var(--scion-text, #1e293b);
      }

      .sort-indicator {
        display: inline-block;
        margin-left: 0.25rem;
        font-size: 0.625rem;
        vertical-align: middle;
        opacity: 0.4;
      }

      th.sorted .sort-indicator {
        opacity: 1;
      }

      .agent-counts {
        color: var(--scion-text-muted, #64748b);
        font-size: 0.875rem;
        margin-bottom: 0.5rem;
      }

      .agent-window-banner {
        display: flex;
        align-items: center;
        gap: 0.75rem;
        padding: 0.5rem 0;
        color: var(--scion-text-muted, #64748b);
        font-size: 0.875rem;
      }

      .agent-window-banner sl-tag {
        cursor: pointer;
      }
    `,
  ];

  private boundOnAgentsChanged = this.onAgentsChanged.bind(this);

  private boundOnAgentsResync = (): void => {
    this.agentWindow.markResync();
  };

  /**
   * A live create while loaded for scope `mine` or `shared` is outside
   * today's add rule: it is not added. A paged window shows the chip, and
   * a held or capped set is marked as possibly stale.
   */
  private boundOnAgentCreated = (): void => {
    if (this.loadedScope !== 'all') this.agentWindow.markMembershipChanged();
  };

  private boundOnWindowChange = (): void => {
    this.windowTick++;
  };

  override connectedCallback(): void {
    super.connectedCallback();

    // Read persisted view mode
    const stored = localStorage.getItem('scion-view-agents') as ViewMode | null;
    if (stored === 'grid' || stored === 'list' || stored === 'graph') {
      this.viewMode = stored;
    }

    // Read persisted scope filter
    if (this.pageData?.user) {
      const scope = localStorage.getItem('scion-scope-agents');
      if (scope === 'mine' || scope === 'shared') {
        this.agentScope = scope;
      }
    }

    // Read persisted phase filter
    const storedPhase = localStorage.getItem('scion-filter-agents-phase');
    if (
      storedPhase === 'running' ||
      storedPhase === 'stopped' ||
      storedPhase === 'suspended' ||
      storedPhase === 'error'
    ) {
      this.phaseFilter = storedPhase;
    }

    // Read persisted mode filter
    const storedMode = localStorage.getItem('scion-filter-agents-mode');
    if (storedMode) {
      this.modeFilter = storedMode;
    }

    // Read persisted sort
    const storedSort = localStorage.getItem('scion-sort-agents');
    if (storedSort) {
      try {
        const parsed = JSON.parse(storedSort);
        if (
          parsed &&
          (parsed.field === 'name' ||
            parsed.field === 'status' ||
            parsed.field === 'created' ||
            parsed.field === 'updated') &&
          (parsed.dir === 'asc' || parsed.dir === 'desc')
        ) {
          this.sortField = parsed.field;
          this.sortDir = parsed.dir;
        }
      } catch {
        /* ignore invalid stored sort */
      }
    }

    const storedPageSize = Number(localStorage.getItem(PAGER_PAGE_SIZE_STORAGE_KEY));
    if (AGENT_PAGER_PAGE_SIZES.includes(storedPageSize as AgentPagerPageSize)) {
      this.pagerPageSize = storedPageSize as AgentPagerPageSize;
    }
    // Sync the window with the persisted view state before the first load.
    this.agentWindow.setViewState(this.windowViewState());

    // Set SSE scope to dashboard (all project summaries).
    // This must happen before checking hydrated data because setScope clears
    // state maps when the scope changes (e.g. from agent-detail to dashboard).
    stateManager.setScope({ type: 'dashboard' });

    // Use hydrated data from SSR if available, avoiding the initial fetch.
    // Only trust it when scope was previously null (initial SSR page load);
    // on client-side navigations the maps were just cleared by setScope above.
    // Skip hydrated data when a scope filter is active — SSR data is unfiltered.
    // Also require scope capabilities — without them the "New Agent" button
    // won't render, so we must fetch from the API to get them. And require
    // the state store to hold the complete dashboard set with full objects:
    // otherwise state may hold only a label, mine or shared subset, a
    // single page, or compact objects, which must not render as "all".
    const hydratedAgents = stateManager.getAgents();
    const hydratedCaps = stateManager.getScopeCapabilities('agent');
    if (
      hydratedAgents.length > 0 &&
      hydratedCaps &&
      this.agentScope === 'all' &&
      stateManager.isAgentSetComplete('full')
    ) {
      this.agents = hydratedAgents;
      this.loadedScope = 'all';
      this.scopeCapabilities = hydratedCaps;
      this.loading = false;
      stateManager.seedAgents(this.agents);
      this.adoptCompleteSet();
    } else {
      void this.loadAgents('page-load');
    }

    // Listen for real-time agent updates
    stateManager.addEventListener('agents-changed', this.boundOnAgentsChanged as EventListener);
    stateManager.addEventListener('agents-resync', this.boundOnAgentsResync as EventListener);
    stateManager.addEventListener('agent-created', this.boundOnAgentCreated as EventListener);
    this.agentWindow.addEventListener('change', this.boundOnWindowChange);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('agents-changed', this.boundOnAgentsChanged as EventListener);
    stateManager.removeEventListener('agents-resync', this.boundOnAgentsResync as EventListener);
    stateManager.removeEventListener('agent-created', this.boundOnAgentCreated as EventListener);
    this.agentWindow.removeEventListener('change', this.boundOnWindowChange);
    this.cancelAgentsLoad();
  }

  /** The window's view state from the page's persisted fields and the live label. */
  private windowViewState(): AgentListViewState {
    return {
      phaseFilter: this.phaseFilter,
      label: this.labelFilter,
      sortField: this.sortField,
      sortDir: this.sortDir,
      pageSize: this.pagerPageSize,
      view: listViewOf(this.viewMode),
      modeFilter: this.modeFilter,
    };
  }

  /**
   * Adopt the state store's complete set (`this.agents`) with no request:
   * small, or held when it is larger than one legacy page, so a lifecycle
   * refresh then costs nothing (the phase changes arrive live).
   */
  private adoptCompleteSet(): void {
    this.agentWindow.adoptDrain({
      complete: true,
      capped: false,
      error: null,
      requests: Math.max(1, Math.ceil(this.agents.length / DRAIN_PAGE_LIMIT)),
    });
  }

  /**
   * Live updates: one `agents-changed` flush merged
   * through `mergeChanged`, replacing the old `onAgentsUpdated` per-event
   * full rebuild over `stateManager.getAgents()`.
   */
  private onAgentsChanged(e: Event): void {
    // `notifyWithData` wraps the payload as `{state, data}` (state.ts); the
    // `AgentsChangedDetail` itself is `detail.data`.
    const detail = (e as CustomEvent<{ data: AgentsChangedDetail }>).detail.data;
    // Paged: the window applies the change to its page and member index.
    this.agentWindow.applyChanges(detail);
    if (this.agentWindow.state === 'paged') return;
    const merged = mergeChanged(this.agents, detail, {
      getAgent: (id) => stateManager.getAgent(id),
      // Today's add rule: global page, scope `all` only — a
      // scope filter's server-side response is the source of truth for
      // membership, so a brand-new SSE agent is not added under a filter.
      // An ID already held keeps getting its updates regardless.
      shouldAdd: () => this.agentScope === 'all',
      scopeCapabilities: this.scopeCapabilities,
    });
    if (merged !== this.agents) {
      this.agents = merged;
    }
  }

  /**
   * A page load, a scope change or a label commit: shows the loading state,
   * and a failed first request sets `error` (today's error path, including
   * a label 400).
   */
  private async loadAgents(trigger: 'page-load' | 'label-commit' = 'page-load'): Promise<void> {
    const seq = ++this.loadAgentsSeq;
    this.loading = true;
    this.error = null;

    try {
      await this.loadAgentsForView(trigger);
    } catch (err) {
      if (seq !== this.loadAgentsSeq) return;
      console.error('Failed to load agents:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load agents';
    } finally {
      if (seq === this.loadAgentsSeq) this.loading = false;
    }
  }

  /** A lifecycle-action or stop-all refresh, or the chip: keeps the current data on failure. */
  private backgroundRefresh(trigger: AgentListTrigger = 'lifecycle-refresh'): void {
    this.loadAgentsForView(trigger).catch((err) => {
      console.warn('Background refresh failed:', err);
    });
  }

  /**
   * A view-state change (view, sort, phase, mode or page size): one request
   * only if the planner says so. A sorted first request still in flight
   * that no longer fits the view state is superseded first: a page load or
   * label commit is sent again for the new view state.
   */
  private onAgentViewStateChanged(): void {
    this.viewEpoch++;
    const superseded = this.agentWindow.supersededRequest(this.committedLabel);
    if (superseded) {
      this.cancelAgentsLoad();
      if (superseded === 'page-load' || superseded === 'label-commit') {
        void this.loadAgents(superseded);
        return;
      }
    }
    this.backgroundRefresh('view-change');
  }

  private beginLoadingIndicator(): void {
    this.agentsLoadingCount++;
    this.agentsLoading = true;
  }

  private endLoadingIndicator(): void {
    this.agentsLoadingCount = Math.max(0, this.agentsLoadingCount - 1);
    this.agentsLoading = this.agentsLoadingCount > 0;
  }

  /**
   * Starts a new agents load: supersedes (and aborts) any load or drain
   * still in flight. Returns the generation to check after every `await`
   * and the signal for `apiFetch`.
   */
  private beginAgentsLoad(): { gen: number; signal: AbortSignal } {
    this.cancelAgentsLoad();
    const controller = new AbortController();
    this.agentsAbortController = controller;
    return { gen: ++this.agentsLoadGen, signal: controller.signal };
  }

  /** Aborts every agents request in flight: the first request, a drain and the window's page fetch. */
  private cancelAgentsLoad(): void {
    this.agentsAbortController?.abort();
    this.agentsAbortController = null;
    this.drainRunner.abort();
    this.agentsLoadGen++;
    this.agentWindow.cancelPageFetch();
    this.agentWindow.endSortedRequest();
  }

  private isStaleAgentsLoad(gen: number): boolean {
    return gen !== this.agentsLoadGen;
  }

  /**
   * The page's single request-choosing function. Every trigger calls it
   * once, and the window's planner picks the one request it needs: a
   * sorted fit request, a drain (complete-needing view states), a paged
   * refresh (the paged chip), or nothing. Rejects when the first request
   * fails; resolves without changes when superseded. When the view state
   * changed while the request was in flight and the adopted result no
   * longer fits it, plans again for the current view state.
   */
  private async loadAgentsForView(trigger: AgentListTrigger): Promise<void> {
    const label = this.committedLabel.trim();
    const plan = this.agentWindow.planRequest(trigger, label);
    if (plan === 'none') return;
    if (plan === 'page') {
      await this.agentWindow.refresh();
      return;
    }
    const viewEpoch = this.viewEpoch;
    let adopted = false;
    this.beginLoadingIndicator();
    try {
      adopted =
        plan === 'fit'
          ? await this.loadFitAgents(label, trigger)
          : await this.drainGlobalAgents(label);
    } catch (err) {
      // The stored cursors were minted under the previous view state.
      if (trigger === 'view-change' && this.agentWindow.state === 'paged') {
        this.agentWindow.invalidateCursors();
      }
      throw err;
    } finally {
      this.endLoadingIndicator();
    }
    if (
      adopted &&
      viewEpoch !== this.viewEpoch &&
      this.agentWindow.planRequest('view-change', this.committedLabel.trim()) !== 'none'
    ) {
      // A view change, so a failure keeps the adopted rows, as a
      // background view-change refresh does, and never becomes the page
      // error of the load that just succeeded.
      try {
        await this.loadAgentsForView('view-change');
      } catch (err) {
        console.warn('View refresh failed:', err);
      }
    }
  }

  /**
   * The sorted first request: complete (small, the response is the whole
   * set) or paged. Sends the scope only when it is not `all` and the label
   * only when it contains `=`, as the legacy request does. Resolves `true`
   * when a result was adopted, `false` when superseded.
   */
  private async loadFitAgents(label: string, trigger: AgentListTrigger): Promise<boolean> {
    const { gen, signal } = this.beginAgentsLoad();
    const ticket = this.agentWindow.beginSortedRequest(trigger, label);
    // Captured now: the scope can change while the request is in flight,
    // and `loadedScope` must be the scope this response was fetched for.
    const requestedScope = this.agentScope;
    const phase = this.phaseFilter;
    const params = new URLSearchParams();
    params.set('sort', this.serverSortField);
    params.set('dir', this.sortDir);
    params.set('limit', String(this.pagerPageSize));
    params.set('fit', String(GLOBAL_AGENTS_FIT));
    params.set('stats', '1');
    if (requestedScope !== 'all') params.set('scope', requestedScope);
    if (label.includes('=')) params.set('label', label);
    if (phase) params.set('phase', phase);

    // Opened before the request is sent, so a live change that lands while
    // it is in flight survives the (older) response.
    const epoch = new AgentSeedEpoch();
    try {
      let response: Response;
      try {
        response = await apiFetch(`/api/v1/agents?${params.toString()}`, { signal });
      } catch (err) {
        if (this.isStaleAgentsLoad(gen) || this.isAbortError(err)) return false;
        throw err;
      }
      if (this.isStaleAgentsLoad(gen)) return false;
      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`)
        );
      }
      const body = (await response.json()) as GlobalAgentsResponse | Agent[];
      if (this.isStaleAgentsLoad(gen)) return false;
      const data: GlobalAgentsResponse = Array.isArray(body) ? { agents: body } : body;

      // A server that ignored sorted mode answers with a legacy page. With
      // no phase sent, it is complete when it has no `nextCursor`, and
      // otherwise the drain continues from it when it is a full drain page
      // (a shorter page, from a server that honoured `limit`, is discarded
      // and the drain starts from the first page). A legacy list honours
      // `phase`, so a page fetched with one is only part of the set: the
      // whole set is drained from the start.
      const legacy = data.complete === undefined;
      // The drain carries no ticket, so each branch ends the sorted request
      // before it drains. The phase branch also closes this epoch (its
      // drain starts its own); the carry branch hands the epoch to the drain,
      // which closes it. The finally below ends the request and closes the
      // epoch again, which does nothing once they are ended and closed.
      if (legacy && phase) {
        // The restarted drain runs its own epoch.
        this.agentWindow.endSortedRequest(ticket);
        epoch.close();
        return await this.drainGlobalAgents(label, gen, requestedScope);
      }
      if (legacy && data.nextCursor) {
        // The drain takes over this epoch and closes it, so live changes
        // since the request was sent are kept, whether or not it uses the
        // page's rows. The rows are passed as the server sent them: the
        // drain decides by the page's length whether to carry it, and
        // seeding drops any agent deleted live.
        this.agentWindow.endSortedRequest(ticket);
        return await this.drainGlobalAgents(label, gen, requestedScope, {
          firstPage: {
            agents: data.agents || [],
            nextCursor: data.nextCursor,
            capabilities: Array.isArray(body) ? undefined : data._capabilities,
          },
          epoch,
        });
      }
      const complete = data.complete === true || legacy;

      this.loadedScope = requestedScope;
      this.adoptScopeCapabilities(Array.isArray(body) ? undefined : data._capabilities);
      // Seeding drops any agent already deleted live: a REST response can
      // race an SSE `deleted`.
      if (complete) {
        this.agents = epoch.seed(data.agents || [], {
          partial: false,
          isMember: (agent) => requestedScope === 'all' && matchesCommittedLabel(agent, label),
        }).agents;
        this.agentWindow.setSmall(epoch.sawResync);
        this.markCompleteSet(requestedScope, label);
      } else {
        // Paged: `this.agents` stays empty; stats and Stop-all read the
        // member index through the window.
        this.agents = [];
        this.agentWindow.setPaged(this.seedPage(epoch, data), label, ticket.key);
      }
      return true;
    } finally {
      epoch.close();
      this.agentWindow.endSortedRequest(ticket);
    }
  }

  /**
   * The complete-set drain of a complete-needing view state, or the held
   * or capped chip. Its first page is today's legacy request, or `carry`'s
   * page when an old server already answered the sorted request with it.
   * Ends in small (one page), held, or capped. Rejects when the first page
   * fails; resolves `true` when a result was adopted.
   */
  private async drainGlobalAgents(
    label: string,
    carriedGen?: number,
    scope: 'all' | 'mine' | 'shared' = this.agentScope,
    carry?: { firstPage: DrainFirstPage; epoch: AgentSeedEpoch }
  ): Promise<boolean> {
    const gen = carriedGen ?? this.beginAgentsLoad().gen;
    const params = new URLSearchParams();
    if (scope !== 'all') params.set('scope', scope);
    if (label.includes('=')) params.set('label', label);
    const qs = params.toString();
    const result = await this.drainRunner.run({
      url: qs ? `/api/v1/agents?${qs}` : '/api/v1/agents',
      view: 'full',
      // Scope `all` adds live creates (today's add rule). For mine and
      // shared, membership cannot be decided on the client: a live create
      // is not added and the set is marked stale.
      ...(scope === 'all'
        ? { isMember: (agent: Agent): boolean => matchesCommittedLabel(agent, label) }
        : {}),
      ...(carry ?? {}),
    });
    if (!result || this.isStaleAgentsLoad(gen)) return false; // superseded.
    if (result.firstPageFailed) {
      throw new Error(result.error?.message ?? 'Failed to load agents');
    }
    this.loadedScope = scope;
    this.agents = result.agents;
    this.adoptScopeCapabilities(result.capabilities);
    this.agentWindow.adoptDrain(result);
    if (result.complete && !result.capped && !result.error) {
      this.markCompleteSet(scope, label);
    }
    return true;
  }

  /** Today's scope capabilities handling for a first response. */
  private adoptScopeCapabilities(caps: Capabilities | undefined): void {
    this.scopeCapabilities = caps;
    if (caps) stateManager.seedScopeCapabilities('agent', caps);
  }

  /**
   * After a complete load: the state store holds the complete dashboard
   * set when it was loaded for scope `all` with no committed label. A mode
   * filter is client-side and does not matter.
   */
  private markCompleteSet(scope: 'all' | 'mine' | 'shared', label: string): void {
    if (scope === 'all' && label === '') stateManager.markAgentSetComplete('full');
  }

  /** The server sort of a sorted request: `created`, or `updated` for every other sort field. */
  private get serverSortField(): 'updated' | 'created' {
    return this.sortField === 'created' ? 'created' : 'updated';
  }

  /** `true` iff `err` is the `AbortError` of an intentionally superseded request. */
  private isAbortError(err: unknown): boolean {
    return err instanceof Error && err.name === 'AbortError';
  }

  /** The window's own paged navigation and paged-chip fetches. */
  private async fetchAgentsPage(params: PagedPageParams): Promise<PagedPageResult> {
    const label = this.committedLabel.trim();
    const qs = new URLSearchParams();
    qs.set('sort', this.serverSortField);
    qs.set('dir', this.sortDir);
    qs.set('limit', String(params.limit));
    if (params.cursor) qs.set('cursor', params.cursor);
    if (params.wantStats) qs.set('stats', '1');
    if (this.loadedScope !== 'all') qs.set('scope', this.loadedScope);
    if (label.includes('=')) qs.set('label', label);
    if (this.phaseFilter) qs.set('phase', this.phaseFilter);

    const epoch = new AgentSeedEpoch();
    try {
      const response = await apiFetch(`/api/v1/agents?${qs.toString()}`, {
        signal: params.signal,
      });
      if (!response.ok) {
        throw new Error(await extractApiError(response, 'Failed to load agents'));
      }
      const data = (await response.json()) as GlobalAgentsResponse;
      return this.seedPage(epoch, data);
    } finally {
      epoch.close();
    }
  }

  /**
   * Seed one sorted page under its epoch and build the window's page
   * result. Rows of agents already deleted live are left out, and their
   * IDs are dropped from `stats.agents` too, so a deleted agent never
   * re-enters the member index. A left-out row whose agent the store no
   * longer holds is counted for the backfill chip (`AgentSeedResult.dropped`):
   * the page is short by at least that many rows. The live
   * changes and any resync since the request was sent go to the window to
   * replay.
   */
  private seedPage(epoch: AgentSeedEpoch, data: GlobalAgentsResponse): PagedPageResult {
    // A full-view page: each object replaces the stored one, so a field
    // the server no longer sends does not linger.
    const seeded = epoch.seed(data.agents || [], { partial: false });
    let stats = data.stats;
    if (stats?.agents) {
      const agents = dropTombstonedPairs(stats.agents, stateManager.getDeletedAgentIds());
      if (agents !== stats.agents) stats = { ...stats, agents };
    }
    return {
      agents: seeded.agents,
      nextCursor: data.nextCursor,
      totalCount: data.totalCount ?? seeded.agents.length,
      stats,
      liveChanged: epoch.changedIds,
      liveUnknown: epoch.unknownChanges,
      liveDeleted: epoch.deletedChanges,
      droppedRows: seeded.dropped.length,
      liveResync: epoch.sawResync,
    };
  }

  /**
   * Apply an optimistic lifecycle patch: merged into H in the local
   * states, replacing on-page rows while paged. `deletedIds` leave H at
   * once; while paged they leave with the follow-up refresh or live delete.
   */
  private applyOptimisticAgents(patch: Agent[], deletedIds: string[] = []): void {
    if (!this.agentWindow.isLocal) {
      this.agentWindow.applyLocalUpdate(patch);
      return;
    }
    const byId = new Map(patch.map((a) => [a.id, a]));
    this.agents = mergeChanged(
      this.agents,
      {
        upserted: patch.map((a) => a.id),
        deleted: deletedIds,
        unknown: new Map(),
        generation: stateManager.scopeGeneration,
      },
      {
        getAgent: (id) => byId.get(id),
        shouldAdd: () => false,
        scopeCapabilities: this.scopeCapabilities,
      }
    );
  }

  /** The agent with `id` as currently shown: H, the current page, then the state store. */
  private findShownAgent(id: string): Agent | undefined {
    return (
      this.agents.find((a) => a.id === id) ??
      this.agentWindow.items.find((a) => a.id === id) ??
      stateManager.getAgent(id)
    );
  }

  /**
   * DELETE returned 202: the hub is still deleting (ptone/scion#2483).
   * Keep the row, show "Deleting…" from the response's `deletion` at once,
   * and let the SSE `deleted` event remove it in place.
   */
  private async keepDeletingAgent(agentId: string, response: Response): Promise<void> {
    stateManager.applyDeleteAccepted(agentId, await readAcceptedDeletion(response));
  }

  private async handleAgentAction(
    agentId: string,
    action: AgentLifecycleAction,
    event?: MouseEvent
  ): Promise<void> {
    if (action === 'force-resume') {
      if (
        !(await showConfirm(RESUME_BEST_EFFORT_CONFIRM_MESSAGE, {
          title: 'Resume (best effort)',
          confirmText: 'Resume',
          variant: 'primary',
        }))
      ) {
        return;
      }
    }

    if (action === 'delete') {
      const agentName = this.findShownAgent(agentId)?.name ?? 'this agent';
      if (
        !event?.altKey &&
        !(await showConfirm(`Are you sure you want to delete agent "${agentName}"?`))
      ) {
        return;
      }
      // Show per-button spinner for delete; don't optimistically remove
      this.actionLoading = { ...this.actionLoading, [agentId]: true };
      this.requestUpdate();

      try {
        const response = await apiFetch(`/api/v1/agents/${agentId}`, {
          method: 'DELETE',
        });

        if (!response.ok) {
          // If the broker is unreachable (502/503), offer a force-delete fallback.
          if (response.status === 502 || response.status === 503) {
            const forceConfirmed = await showConfirm(
              'Delete failed — the broker may be unreachable. Force delete this agent? This will remove the hub record without notifying the broker.',
              { title: 'Force Delete', confirmText: 'Force Delete', variant: 'danger' }
            );
            if (forceConfirmed) {
              const forceResponse = await apiFetch(`/api/v1/agents/${agentId}?force=true`, {
                method: 'DELETE',
              });
              if (!forceResponse.ok) {
                throw new Error(
                  await extractApiError(forceResponse, 'Failed to force delete agent')
                );
              }
              if (forceResponse.status === 202) {
                await this.keepDeletingAgent(agentId, forceResponse);
                return;
              }
              this.applyOptimisticAgents([], [agentId]);
              this.backgroundRefresh();
              return;
            }
          }
          throw new Error(await extractApiError(response, 'Failed to delete agent'));
        }

        if (response.status === 202) {
          await this.keepDeletingAgent(agentId, response);
          return;
        }

        // Server confirmed — remove from local list
        this.applyOptimisticAgents([], [agentId]);
        this.backgroundRefresh();
      } catch (err) {
        console.error('Failed to delete agent:', err);
        showToast(err instanceof Error ? err.message : 'Failed to delete agent');
      } finally {
        this.actionLoading = { ...this.actionLoading, [agentId]: false };
      }
      return;
    }

    // Apply optimistic phase update immediately
    const optimisticPhase: Record<string, string> = {
      start: 'starting',
      stop: 'stopping',
      suspend: 'stopping',
      resume: 'starting',
      'force-resume': 'starting',
    };
    const shown = this.findShownAgent(agentId);
    if (shown) {
      this.applyOptimisticAgents([{ ...shown, phase: optimisticPhase[action] as Agent['phase'] }]);
    }

    const actionUrls: Record<string, string> = {
      start: `/api/v1/agents/${agentId}/start`,
      stop: `/api/v1/agents/${agentId}/stop`,
      suspend: `/api/v1/agents/${agentId}/suspend`,
      resume: `/api/v1/agents/${agentId}/start`,
      'force-resume': `/api/v1/agents/${agentId}/start`,
    };

    try {
      const response = await apiFetch(actionUrls[action], lifecycleActionRequestInit(action));

      if (!response.ok) {
        throw new Error(await extractApiError(response, `Failed to ${action} agent`));
      }

      this.backgroundRefresh();
    } catch (err) {
      console.error(`Failed to ${action} agent:`, err);
      showToast(err instanceof Error ? err.message : `Failed to ${action} agent`);
      // Roll back optimistic update on failure
      this.backgroundRefresh();
    }
  }

  /**
   * Stop-all visibility: a running agent in H in the local states (today's
   * check), the member index while paged, or the snapshot running count in
   * count-only mode.
   */
  private hasRunningAgents(): boolean {
    return this.agentWindow.stats.running > 0;
  }

  private async handleStopAll(): Promise<void> {
    if (!(await showConfirm('Are you sure you want to stop all running agents?'))) {
      return;
    }

    // Optimistic: mark all running agents shown as "stopping"
    const shownAgents = this.agentWindow.isLocal ? this.agents : this.agentWindow.items;
    this.applyOptimisticAgents(
      shownAgents
        .filter((a) => isAgentRunning(a))
        .map((a) => ({ ...a, phase: 'stopping' as const }))
    );
    this.stopAllLoading = true;

    try {
      const response = await apiFetch('/api/v1/agents/stop-all', {
        method: 'POST',
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, 'Failed to stop all agents'));
      }

      const result = (await response.json()) as StopAllResult;
      for (const notice of stopAllNotices(result)) {
        showToast(notice.message, notice.variant);
      }

      this.backgroundRefresh();
    } catch (err) {
      console.error('Failed to stop all agents:', err);
      showToast(err instanceof Error ? err.message : 'Failed to stop all agents');
      this.backgroundRefresh();
    } finally {
      this.stopAllLoading = false;
    }
  }

  private onViewChange(e: CustomEvent<{ view: ViewMode }>): void {
    this.viewMode = e.detail.view;
    this.agentWindow.setViewState({ view: listViewOf(this.viewMode) });
    this.onAgentViewStateChanged();
  }

  private formatRelativeTime(isoString: string): string {
    const ms = new Date(isoString).getTime();
    if (Number.isNaN(ms)) return '—';
    // A future instant is clock skew between hub and browser.
    if (ms > Date.now()) return 'just now';
    return formatRelative(isoString, { style: 'narrow' });
  }

  private setPhaseFilter(phase: AgentPhase | ''): void {
    if (this.phaseFilter === phase) return;
    this.phaseFilter = phase;
    if (phase) {
      localStorage.setItem('scion-filter-agents-phase', phase);
    } else {
      localStorage.removeItem('scion-filter-agents-phase');
    }
    this.agentWindow.setViewState({ phaseFilter: phase });
    this.onAgentViewStateChanged();
  }

  private setModeFilter(mode: string): void {
    if (this.modeFilter === mode) return;
    this.modeFilter = mode;
    if (mode) {
      localStorage.setItem('scion-filter-agents-mode', mode);
    } else {
      localStorage.removeItem('scion-filter-agents-mode');
    }
    this.agentWindow.setViewState({ modeFilter: mode });
    this.onAgentViewStateChanged();
  }

  private getModeFilterLabel(): string {
    if (!this.modeFilter) return 'All Modes';
    if (this.modeFilter === 'can_message') return 'Can message';
    if (this.modeFilter === 'cannot_message') return 'Cannot message';
    return getMessageModeDisplay(this.modeFilter).label;
  }

  private toggleSort(field: AgentSortField): void {
    if (this.sortField === field) {
      this.sortDir = this.sortDir === 'asc' ? 'desc' : 'asc';
    } else {
      this.sortField = field;
      this.sortDir = field === 'name' ? 'asc' : 'desc';
    }
    localStorage.setItem(
      'scion-sort-agents',
      JSON.stringify({ field: this.sortField, dir: this.sortDir })
    );
    this.agentWindow.setViewState({ sortField: this.sortField, sortDir: this.sortDir });
    this.onAgentViewStateChanged();
  }

  private sortIndicator(field: AgentSortField): string {
    return this.sortField === field ? (this.sortDir === 'asc' ? '▲' : '▼') : '▲';
  }

  private setScope(scope: 'all' | 'mine' | 'shared'): void {
    if (this.agentScope === scope) return;
    this.agentScope = scope;
    if (scope === 'all') {
      localStorage.removeItem('scion-scope-agents');
    } else {
      localStorage.setItem('scion-scope-agents', scope);
    }
    // A scope change is a refresh: one first request, as on page load.
    void this.loadAgents('page-load');
  }

  /** `sl-change` or `sl-clear`: commit the label, a refresh. */
  private commitLabel(value: string): void {
    this.labelFilter = value;
    this.committedLabel = value.trim();
    this.agentWindow.setViewState({ label: value });
    void this.loadAgents('label-commit');
  }

  override render() {
    return html`
      <div class="header">
        <h1>Agents</h1>
        <div class="header-actions">
          ${this.pageData?.user
            ? html`
                <div class="scope-toggle">
                  <button
                    class=${this.agentScope === 'all' ? 'active' : ''}
                    title="All agents"
                    @click=${() => this.setScope('all')}
                  >
                    All
                  </button>
                  <button
                    class=${this.agentScope === 'mine' ? 'active' : ''}
                    title="Agents I created"
                    @click=${() => this.setScope('mine')}
                  >
                    <sl-icon name="person"></sl-icon>
                    Mine
                  </button>
                  <button
                    class=${this.agentScope === 'shared' ? 'active' : ''}
                    title="Agents in shared projects"
                    @click=${() => this.setScope('shared')}
                  >
                    <sl-icon name="people"></sl-icon>
                    Shared
                  </button>
                </div>
              `
            : nothing}
          <scion-view-toggle
            .view=${this.viewMode}
            storageKey="scion-view-agents"
            @view-change=${this.onViewChange}
          ></scion-view-toggle>
          ${can(this.scopeCapabilities, 'stop_all') && this.hasRunningAgents()
            ? html`
                <sl-button
                  variant="danger"
                  size="small"
                  outline
                  ?loading=${this.stopAllLoading}
                  ?disabled=${this.stopAllLoading}
                  @click=${() => this.handleStopAll()}
                >
                  <sl-icon slot="prefix" name="stop-circle"></sl-icon>
                  Stop All
                </sl-button>
              `
            : nothing}
          ${can(this.scopeCapabilities, 'create')
            ? html`
                <a href="/agents/new" style="text-decoration: none;">
                  <sl-button variant="primary" size="small">
                    <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                    New Agent
                  </sl-button>
                </a>
              `
            : nothing}
        </div>
      </div>

      ${this.loading
        ? this.renderLoading()
        : this.error
          ? this.renderError()
          : html` ${this.renderFilterBar()} ${this.renderAgents()} `}

      <scion-quick-message-dialog
        agentId=${this.quickMessageAgentId}
        agentName=${this.quickMessageAgentName}
        ?open=${this.quickMessageOpen}
        @sl-request-close=${() => {
          this.quickMessageOpen = false;
        }}
      ></scion-quick-message-dialog>
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading agents...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Agents</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${(): Promise<void> => this.loadAgents('page-load')}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderFilterBar() {
    return html`
      <div class="filter-bar">
        <span class="label">Status:</span>
        <div class="scope-toggle">
          <button
            class=${this.phaseFilter === '' ? 'active' : ''}
            @click=${() => this.setPhaseFilter('')}
          >
            All
          </button>
          <button
            class=${this.phaseFilter === 'running' ? 'active' : ''}
            @click=${() => this.setPhaseFilter('running')}
          >
            Running
          </button>
          <button
            class=${this.phaseFilter === 'stopped' ? 'active' : ''}
            @click=${() => this.setPhaseFilter('stopped')}
          >
            Stopped
          </button>
          <button
            class=${this.phaseFilter === 'suspended' ? 'active' : ''}
            @click=${() => this.setPhaseFilter('suspended')}
          >
            Suspended
          </button>
          <button
            class=${this.phaseFilter === 'error' ? 'active' : ''}
            @click=${() => this.setPhaseFilter('error')}
          >
            Error
          </button>
        </div>
        <sl-input
          size="small"
          placeholder="Filter by label (key=value)"
          clearable
          .value=${this.labelFilter}
          @sl-input=${(e: Event) => {
            this.labelFilter = (e.target as HTMLElement & { value: string }).value;
            this.agentWindow.setViewState({ label: this.labelFilter });
          }}
          @sl-change=${(): void => this.commitLabel(this.labelFilter)}
          @sl-clear=${(): void => this.commitLabel('')}
          style="max-width: 220px;"
        >
          <sl-icon slot="prefix" name="tag"></sl-icon>
        </sl-input>
        <sl-dropdown>
          <sl-button slot="trigger" size="small" outline>
            ${this.modeFilter &&
            this.modeFilter !== 'can_message' &&
            this.modeFilter !== 'cannot_message'
              ? html`<sl-icon
                  slot="prefix"
                  name=${MESSAGE_MODE_DISPLAY[this.modeFilter as MessageMode]?.icon || 'funnel'}
                ></sl-icon>`
              : html`<sl-icon slot="prefix" name="funnel"></sl-icon>`}
            ${this.getModeFilterLabel()}
          </sl-button>
          <sl-menu
            @sl-select=${(e: CustomEvent<{ item: { value: string } }>) =>
              this.setModeFilter(e.detail.item.value)}
          >
            <sl-menu-item value="" ?checked=${this.modeFilter === ''}>All Modes</sl-menu-item>
            <sl-divider></sl-divider>
            <sl-menu-item value="project" ?checked=${this.modeFilter === 'project'}>
              <sl-icon slot="prefix" name="globe2"></sl-icon>
              Project
            </sl-menu-item>
            <sl-menu-item value="branch" ?checked=${this.modeFilter === 'branch'}>
              <sl-icon slot="prefix" name="diagram-3"></sl-icon>
              Branch
            </sl-menu-item>
            <sl-menu-item value="lineage" ?checked=${this.modeFilter === 'lineage'}>
              <sl-icon slot="prefix" name="person-lines-fill"></sl-icon>
              Lineage
            </sl-menu-item>
            <sl-menu-item value="none" ?checked=${this.modeFilter === 'none'}>
              <sl-icon slot="prefix" name="shield-lock"></sl-icon>
              Sealed
            </sl-menu-item>
            <sl-divider></sl-divider>
            <sl-menu-item value="can_message" ?checked=${this.modeFilter === 'can_message'}>
              <sl-icon slot="prefix" name="check-circle"></sl-icon>
              Can message
            </sl-menu-item>
            <sl-menu-item value="cannot_message" ?checked=${this.modeFilter === 'cannot_message'}>
              <sl-icon slot="prefix" name="x-circle"></sl-icon>
              Cannot message
            </sl-menu-item>
          </sl-menu>
        </sl-dropdown>
        ${this.viewMode === 'grid'
          ? html`
              <sl-dropdown>
                <sl-button slot="trigger" size="small" outline>
                  <sl-icon
                    slot="prefix"
                    name=${this.sortDir === 'asc' ? 'sort-alpha-down' : 'sort-alpha-down-alt'}
                  ></sl-icon>
                  Sort: ${this.sortField}
                </sl-button>
                <sl-menu
                  @sl-select=${(e: CustomEvent<{ item: { value: string } }>) =>
                    this.toggleSort(e.detail.item.value as AgentSortField)}
                >
                  <sl-menu-item value="name" ?checked=${this.sortField === 'name'}
                    >Name</sl-menu-item
                  >
                  <sl-menu-item value="status" ?checked=${this.sortField === 'status'}
                    >Status</sl-menu-item
                  >
                  <sl-menu-item value="created" ?checked=${this.sortField === 'created'}
                    >Created</sl-menu-item
                  >
                  <sl-menu-item value="updated" ?checked=${this.sortField === 'updated'}
                    >Updated</sl-menu-item
                  >
                </sl-menu>
              </sl-dropdown>
            `
          : nothing}
      </div>
    `;
  }

  /**
   * The agent views. The empty states read `T`: the member index total
   * while paged (the count-only snapshot above 2,000), otherwise
   * `this.agents.length`, as today.
   */
  private renderAgents() {
    const win = this.agentWindow;
    if (win.stats.total === 0 && win.display.length === 0) {
      // An incomplete or stale set still says so above the empty state.
      return html`${this.renderWindowBanner()}${this.renderNoAgents()}`;
    }

    if (win.state === 'paged' && !win.isSortedEligible(this.committedLabel)) {
      // The drain this view state needs is in flight, or failed; the server
      // page is not shown in the wrong order meanwhile.
      return this.agentsLoading
        ? html`<div class="empty-state"><p>Loading agents…</p></div>`
        : html`<div class="empty-state">
            <p>Could not load every agent for this view.</p>
            <sl-button size="small" @click=${(): void => this.onAgentViewStateChanged()}
              >Retry</sl-button
            >
          </div>`;
    }

    const filtered = win.display;
    if (filtered.length === 0 && this.phaseFilter) {
      // A capped, failed or stale set still says so above the empty state:
      // agents beyond what was loaded may match the filter.
      return html`
        ${this.renderWindowBanner()}
        <div class="empty-state">
          <sl-icon name="funnel"></sl-icon>
          <h2>No Matching Agents</h2>
          <p>No agents match the current filter. Try changing the status filter.</p>
        </div>
      `;
    }

    if (this.viewMode === 'graph') {
      // Everything that can narrow/widen `filtered` independent of a delete.
      const filterKey = `${this.loadedScope}|${this.phaseFilter}|${this.modeFilter}|${this.labelFilter}`;
      return html`${this.renderWindowBanner()}<scion-agent-tree-view
          .agents=${filtered}
          filterKey=${filterKey}
        ></scion-agent-tree-view>`;
    }
    const items = win.items;
    return html`
      ${this.renderWindowBanner()} ${this.renderCountOnlyCounts()}
      ${this.viewMode === 'grid' ? this.renderGrid(items) : this.renderTable(items)}
      ${this.renderAgentPager(items.length)}
    `;
  }

  /**
   * Count-only mode (more than 2,000 agents): the counts are the last
   * refresh's snapshot and are not adjusted live.
   */
  private renderCountOnlyCounts(): TemplateResult | typeof nothing {
    const win = this.agentWindow;
    if (win.state !== 'paged' || !win.memberIndex.countOnly) return nothing;
    const { total, running } = win.stats;
    return html`<div class="agent-counts">
      ${formatNumber(total)} agents · ${formatNumber(running)} running, as of last refresh
    </div>`;
  }

  /** The empty state of the current scope. */
  private renderNoAgents(): TemplateResult {
    if (this.agentScope === 'mine') {
      return html`
        <div class="empty-state">
          <sl-icon name="person"></sl-icon>
          <h2>No Agents Found</h2>
          <p>You haven't created any agents yet.</p>
        </div>
      `;
    }
    if (this.agentScope === 'shared') {
      return html`
        <div class="empty-state">
          <sl-icon name="people"></sl-icon>
          <h2>No Shared Agents</h2>
          <p>No agents have been shared with you yet.</p>
        </div>
      `;
    }
    return this.renderEmptyState();
  }

  /** The window's capped, failed or stale banner, with a Refresh that is the chip trigger. */
  private renderWindowBanner(): TemplateResult | typeof nothing {
    const banner = this.agentWindow.banner;
    if (!banner) return nothing;
    return html`<div class="agent-window-banner">
      <span>${banner.text}</span>
      <sl-tag variant="primary" pill @click=${(): void => this.onChip()}>
        <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
        Refresh
      </sl-tag>
    </div>`;
  }

  /** The chip or banner Refresh, ignored while a load is in flight. */
  private onChip(): void {
    if (this.agentsLoading || this.agentWindow.loading) return;
    this.backgroundRefresh('chip');
  }

  /** The pager under the grid and list views. Its chip is the chip trigger. */
  private renderAgentPager(rowsOnPage: number): TemplateResult {
    const win = this.agentWindow;
    return html`<scion-agent-pager
      .storageKey=${PAGER_PAGE_SIZE_STORAGE_KEY}
      .pageIndex=${win.pageIndex}
      .rangeStart=${win.rangeStart}
      .rowsOnPage=${rowsOnPage}
      .total=${win.total}
      .pageSize=${this.pagerPageSize}
      .hasNext=${win.hasNext}
      .hasPrev=${win.hasPrev}
      .loading=${win.loading || (win.state === 'paged' && this.agentsLoading)}
      .error=${win.error}
      .showChip=${win.updatesAvailable}
      .chipText=${win.memberIndex.countOnly && win.state === 'paged'
        ? 'counts may have changed · Refresh'
        : 'may have changed · Refresh'}
      @prev=${(): void => this.onPagerNav(() => win.prev())}
      @next=${(): void => this.onPagerNav(() => win.next())}
      @chip-click=${(): void => this.onPagerNav(() => this.loadAgentsForView('chip'))}
      @page-size-change=${(e: CustomEvent<{ pageSize: AgentPagerPageSize }>): void =>
        this.onPagerSizeChange(e.detail.pageSize)}
    ></scion-agent-pager>`;
  }

  /**
   * Prev, Next and chip clicks are refused while the window's own fetch
   * or, while paged, a page-level load is in flight, so a cursor minted
   * under an older view state is never replayed.
   */
  private onPagerNav(action: () => Promise<void>): void {
    const win = this.agentWindow;
    if (win.loading || (win.state === 'paged' && this.agentsLoading)) return;
    action().catch((err) => {
      console.warn('Failed to load agents:', err);
    });
  }

  private onPagerSizeChange(size: AgentPagerPageSize): void {
    this.pagerPageSize = size;
    this.agentWindow.setViewState({ pageSize: size });
    this.onAgentViewStateChanged();
  }

  private renderEmptyState() {
    return html`
      <div class="empty-state">
        <sl-icon name="cpu"></sl-icon>
        <h2>No Agents Found</h2>
        <p>
          Agents are AI-powered workers that can help you with coding
          tasks.${can(this.scopeCapabilities, 'create')
            ? ' Create your first agent to get started.'
            : ''}
        </p>
        ${can(this.scopeCapabilities, 'create')
          ? html`
              <a href="/agents/new" style="text-decoration: none;">
                <sl-button variant="primary">
                  <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                  Create Agent
                </sl-button>
              </a>
            `
          : nothing}
      </div>
    `;
  }

  private renderGrid(items: Agent[]): TemplateResult {
    return html`
      <div class="resource-grid">${items.map((agent) => this.renderAgentCard(agent))}</div>
    `;
  }

  private renderActionButtons(agent: Agent) {
    const isLoading = this.actionLoading[agent.id] || false;
    // While the hub is deleting, hide every lifecycle action and Delete.
    const deleting = this.deletionLease.isDeleting(agent);
    const lifecycleOk = canLifecycle(agent._capabilities) && !deleting;

    return html`
      ${agent.messageMode === 'none'
        ? nothing
        : agent._messageability?.canMessage === false
          ? html`
              <sl-tooltip content="${getDenialMessage(agent._messageability.reason, agent.name)}">
                <span style="display: inline-flex">
                  <sl-button
                    class="action-btn-primary"
                    variant="default"
                    size="small"
                    outline
                    disabled
                    aria-label="Message"
                  >
                    <sl-icon slot="prefix" name="chat-dots"></sl-icon>
                  </sl-button>
                </span>
              </sl-tooltip>
            `
          : canMessageAgent(agent._capabilities)
            ? html`
                <sl-tooltip content="Message">
                  <span style="display: inline-flex">
                    <sl-button
                      class="action-btn-primary"
                      variant="default"
                      size="small"
                      outline
                      @click=${() => {
                        this.quickMessageAgentId = agent.id;
                        this.quickMessageAgentName = agent.name;
                        this.quickMessageOpen = true;
                      }}
                      aria-label="Message"
                    >
                      <sl-icon slot="prefix" name="chat-dots"></sl-icon>
                    </sl-button>
                  </span>
                </sl-tooltip>
              `
            : nothing}
      ${can(agent._capabilities, 'attach')
        ? html`
            <sl-tooltip content="Terminal">
              <span style="display: inline-flex">
                <sl-button
                  class="action-btn-primary"
                  variant="primary"
                  size="small"
                  href=${terminalHref(agent.id)}
                  ?disabled=${!isTerminalAvailable(agent)}
                  aria-label="Terminal"
                >
                  <sl-icon slot="prefix" name="terminal"></sl-icon>
                </sl-button>
              </span>
            </sl-tooltip>
          `
        : nothing}
      ${isAgentRunning(agent)
        ? lifecycleOk
          ? html`
              ${agent.harnessCapabilities?.resume?.support !== 'no'
                ? html`
                    <sl-tooltip content="Suspend">
                      <sl-button
                        class="action-btn-warning"
                        variant="warning"
                        size="small"
                        outline
                        ?loading=${isLoading}
                        ?disabled=${isLoading}
                        @click=${() => this.handleAgentAction(agent.id, 'suspend')}
                        aria-label="Suspend"
                      >
                        <sl-icon slot="prefix" name="pause-circle"></sl-icon>
                      </sl-button>
                    </sl-tooltip>
                  `
                : nothing}
              <sl-tooltip content="Stop">
                <sl-button
                  class="action-btn-danger"
                  variant="danger"
                  size="small"
                  outline
                  ?loading=${isLoading}
                  ?disabled=${isLoading}
                  @click=${() => this.handleAgentAction(agent.id, 'stop')}
                  aria-label="Stop"
                >
                  <sl-icon slot="prefix" name="stop-circle"></sl-icon>
                </sl-button>
              </sl-tooltip>
            `
          : nothing
        : agent.phase === 'suspended'
          ? lifecycleOk
            ? html`
                <sl-tooltip content="Resume">
                  <sl-button
                    class="action-btn-success"
                    variant="success"
                    size="small"
                    outline
                    ?loading=${isLoading}
                    ?disabled=${isLoading}
                    @click=${() => this.handleAgentAction(agent.id, 'resume')}
                    aria-label="Resume"
                  >
                    <sl-icon slot="prefix" name="play-circle"></sl-icon>
                  </sl-button>
                </sl-tooltip>
              `
            : nothing
          : lifecycleOk
            ? html`
                ${agent.phase === 'error'
                  ? html`
                      <sl-tooltip content="Resume (best effort)">
                        <sl-button
                          size="small"
                          outline
                          ?loading=${isLoading}
                          ?disabled=${isLoading}
                          @click=${() => this.handleAgentAction(agent.id, 'force-resume')}
                          aria-label="Resume (best effort)"
                        >
                          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                        </sl-button>
                      </sl-tooltip>
                    `
                  : nothing}
                <sl-tooltip content="Start">
                  <sl-button
                    class="action-btn-success"
                    variant="success"
                    size="small"
                    outline
                    ?loading=${isLoading}
                    ?disabled=${isLoading}
                    @click=${() => this.handleAgentAction(agent.id, 'start')}
                    aria-label="Start"
                  >
                    <sl-icon slot="prefix" name="play-circle"></sl-icon>
                  </sl-button>
                </sl-tooltip>
              `
            : nothing}
      ${can(agent._capabilities, 'delete') && !deleting
        ? html`
            <sl-tooltip content="Delete">
              <sl-button
                class="action-btn-danger"
                variant="default"
                size="small"
                outline
                ?loading=${isLoading}
                ?disabled=${isLoading}
                @click=${(e: MouseEvent) => this.handleAgentAction(agent.id, 'delete', e)}
                aria-label="Delete"
              >
                <sl-icon slot="prefix" name="trash"></sl-icon>
              </sl-button>
            </sl-tooltip>
          `
        : nothing}
    `;
  }

  private renderAgentCard(agent: Agent) {
    return html`
      <div class="agent-card">
        <div class="agent-header">
          <div>
            <h3 class="resource-name">
              <sl-icon name="cpu"></sl-icon>
              <a href="/agents/${agent.id}" style="color: inherit; text-decoration: none;">
                ${agent.name}
              </a>
            </h3>
            <div class="agent-meta">
              ${agent.project
                ? html`<div>
                    <sl-icon name="folder"></sl-icon>
                    <a
                      href="/projects/${agent.projectId}"
                      @click=${(e: MouseEvent) => e.stopPropagation()}
                      >${agent.project}</a
                    >
                  </div>`
                : ''}
              <div><sl-icon name="code-square"></sl-icon> ${agent.template}</div>
              ${agent.runtimeBrokerId
                ? html`<div>
                    <a href="/brokers/${agent.runtimeBrokerId}" class="broker-link">
                      <sl-icon name="hdd-rack"></sl-icon>
                      ${agent.runtimeBrokerName || agent.runtimeBrokerId}
                    </a>
                  </div>`
                : ''}
            </div>
          </div>
          <scion-status-badge
            status=${getAgentDisplayStatus(agent) as StatusType}
            label=${getAgentDisplayStatus(agent)}
            size="small"
          >
          </scion-status-badge>
          <scion-deletion-badge
            .deletion=${this.deletionLease.view(agent)}
            size="small"
          ></scion-deletion-badge>
          <scion-message-mode-badge
            mode=${agent.messageMode || 'project'}
            size="small"
            ?showLabel=${false}
          ></scion-message-mode-badge>
          ${agent._messageability
            ? html`
                <scion-messageability-indicator
                  .messageability=${agent._messageability}
                  size="small"
                ></scion-messageability-indicator>
              `
            : nothing}
        </div>

        ${agent.taskSummary ? html` <div class="agent-task">${agent.taskSummary}</div> ` : ''}
        ${agent.labels && Object.keys(agent.labels).length > 0
          ? html`<div class="agent-labels" style="margin-top: 0.5em;">
              ${Object.entries(agent.labels).map(
                ([k, v]) =>
                  html`<sl-tag size="small" variant="neutral" style="margin: 0.15em;"
                    >${k}: ${v}</sl-tag
                  >`
              )}
            </div>`
          : ''}

        <div class="agent-actions">${this.renderActionButtons(agent)}</div>
      </div>
    `;
  }

  private renderTable(items: Agent[]): TemplateResult {
    return html`
      <div class="resource-table-container">
        <table>
          <thead>
            <tr>
              <th
                class="sortable ${this.sortField === 'name' ? 'sorted' : ''}"
                @click=${() => this.toggleSort('name')}
              >
                Name <span class="sort-indicator">${this.sortIndicator('name')}</span>
              </th>
              <th>Project</th>
              <th class="hide-mobile">Template</th>
              <th
                class="status-col sortable ${this.sortField === 'status' ? 'sorted' : ''}"
                @click=${() => this.toggleSort('status')}
              >
                Status <span class="sort-indicator">${this.sortIndicator('status')}</span>
              </th>
              <th class="hide-mobile">Messaging</th>
              <th
                class="hide-mobile sortable ${this.sortField === 'updated' ? 'sorted' : ''}"
                @click=${() => this.toggleSort('updated')}
              >
                Updated <span class="sort-indicator">${this.sortIndicator('updated')}</span>
              </th>
              <th class="hide-mobile">Task</th>
              <th style="text-align: right">Actions</th>
            </tr>
          </thead>
          <tbody>
            ${items.map((agent) => this.renderAgentRow(agent))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderAgentRow(agent: Agent) {
    return html`
      <tr>
        <td>
          <span class="name-cell">
            <sl-icon name="cpu"></sl-icon>
            <a href="/agents/${agent.id}">${agent.name}</a>
          </span>
        </td>
        <td>
          ${agent.project
            ? html`<a href="/projects/${agent.projectId}" class="project-link">${agent.project}</a>`
            : '\u2014'}
        </td>
        <td class="hide-mobile">${agent.template}</td>
        <td>
          <scion-status-badge
            status=${getAgentDisplayStatus(agent) as StatusType}
            label=${getAgentDisplayStatus(agent)}
            size="small"
          ></scion-status-badge>
          <scion-deletion-badge
            .deletion=${this.deletionLease.view(agent)}
            size="small"
          ></scion-deletion-badge>
        </td>
        <td class="hide-mobile">
          <scion-message-mode-badge
            mode=${agent.messageMode || 'project'}
            size="small"
          ></scion-message-mode-badge>
          ${agent._messageability
            ? html`
                <scion-messageability-indicator
                  .messageability=${agent._messageability}
                  size="small"
                ></scion-messageability-indicator>
              `
            : nothing}
        </td>
        <td class="hide-mobile">
          ${(agent.lastActivityEvent && !agent.lastActivityEvent.startsWith('0001')) ||
          agent.updated
            ? this.formatRelativeTime(
                (agent.lastActivityEvent && !agent.lastActivityEvent.startsWith('0001')
                  ? agent.lastActivityEvent
                  : agent.updated)!
              )
            : '\u2014'}
        </td>
        <td class="hide-mobile">
          <span class="task-cell">${agent.taskSummary || '\u2014'}</span>
        </td>
        <td class="actions-cell">
          <span class="table-actions"> ${this.renderActionButtons(agent)} </span>
        </td>
      </tr>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-agents': ScionPageAgents;
  }
}
