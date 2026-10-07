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
 * Project detail page component
 *
 * Displays a single project with its agents and settings
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type {
  PageData,
  Project,
  Agent,
  AgentPhase,
  Capabilities,
  ProjectSessionMetricsSummary,
  AgentLifecycleAction,
  DeletionInfo,
} from '../../shared/types.js';
import {
  can,
  canAny,
  canLifecycle,
  isAgentRunning,
  isTerminalAvailable,
  isEmptyPerAgentWorkspace,
  isSharedWorkspace,
  RESUME_BEST_EFFORT_CONFIRM_MESSAGE,
  lifecycleActionRequestInit,
} from '../../shared/types.js';
import { agentStatusBadge } from '../../shared/agent-state-display.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { stateManager } from '../../client/state.js';
import type { AgentsChangedDetail } from '../../client/state.js';
import { fetchHubProjectCapabilities } from '../../client/hub-capabilities.js';
import {
  AgentListWindow,
  incompleteStatNote,
  projectAgentsFitFor,
} from '../../client/agent-list-window.js';
import type {
  AgentListTrigger,
  AgentListView,
  PagedPageParams,
  PagedPageResult,
  SortedRequestTicket,
} from '../../client/agent-list-window.js';
import { AgentDrainRunner } from '../../client/agent-drain.js';
import { AgentSeedEpoch } from '../../client/agent-seed-epoch.js';
import type { SeededDrainResult } from '../../client/agent-drain.js';
import { mergeChanged, dropTombstonedPairs } from '../../client/agent-merge.js';
import type { AgentSortField, SortDir } from '../../shared/agent-sort.js';
import '../shared/git-remote-display.js';
import type { ViewMode } from '../shared/view-toggle.js';
import '../shared/status-badge.js';
import { DeletionLeaseController } from '../shared/deletion-badge.js';
import '../shared/deletion-banner.js';
import { runAgentDelete, lifecycleActionErrorMessage } from '../../client/agent-delete.js';
import type { AgentDeleteRequest } from '../../client/agent-delete.js';
import '../shared/view-toggle.js';
import '../shared/agent-tree-view.js';
import type { ScionAgentTreeView } from '../shared/agent-tree-view.js';
import { GraphPaletteController } from '../shared/palette/graph-palette-controller.js';
import '../shared/agent-message-viewer.js';
import '../shared/agent-pager.js';
import { AGENT_PAGER_PAGE_SIZES } from '../shared/agent-pager.js';
import type { AgentPagerPageSize } from '../shared/agent-pager.js';
import '../shared/file-browser.js';
import '../shared/file-editor.js';
import {
  WorkspaceFileBrowserDataSource,
  SharedDirFileBrowserDataSource,
} from '../shared/file-browser.js';
import type { FileBrowserDataSource } from '../shared/file-browser.js';
import {
  WorkspaceFileEditorDataSource,
  SharedDirFileEditorDataSource,
} from '../shared/file-editor.js';
import type { FileEditorDataSource } from '../shared/file-editor.js';
import { showToast } from '../../utils/toast.js';
import { stopAllNotices, type StopAllResult } from '../../utils/stop-all.js';
import { showConfirm } from '../shared/confirm-dialog.js';
import { terminalHref } from '../../client/open-terminal.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { formatNumber } from '../../utils/format-number.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { navigateTo } from '../../client/navigation.js';

/** A request/refresh trigger; every one funnels into `loadAgentsForView`, which asks the window's planner for the one request it needs. */
type AgentsViewTrigger = AgentListTrigger;

/** The project endpoint's sorted-mode response shape. */
interface SortedAgentsResponse {
  agents: Agent[];
  nextCursor?: string;
  totalCount: number;
  complete?: boolean;
  sort?: string;
  dir?: string;
  stats?: { total: number; running: number; agents?: Array<[string, string]> };
  _capabilities?: Capabilities;
}

/** The window's layout for a page view mode: the `graph` mode is the agent tree. */
function listViewOf(mode: ViewMode): AgentListView {
  return mode === 'graph' ? 'tree' : mode;
}

// User-level (not per-project) sticky preference for the agents section height.
const AGENTS_EXPANDED_STORAGE_KEY = 'scion-project-agents-expanded';
const PAGER_PAGE_SIZE_STORAGE_KEY = 'scion-pagesize-project-agents';

/**
 * The server-prefetched project in `pageData.data`, when it is the project
 * this page shows; otherwise null and the page fetches it. The router only
 * hands over a payload prefetched for this path and the current user, on
 * the first render (see client/ssr-page-data.ts); this checks that the
 * payload is a single project with the requested id.
 */
export function hydratedProjectFor(pageData: PageData | null, projectId: string): Project | null {
  const data = pageData?.data;
  if (!data || typeof data !== 'object' || Array.isArray(data)) return null;
  if (!projectId || data.id !== projectId) return null;
  if (typeof data.name !== 'string') return null;
  return data as unknown as Project;
}

@customElement('scion-page-project-detail')
export class ScionPageProjectDetail extends LitElement {
  /** Re-renders absolute times when the display timezone changes. */
  readonly _zone = new DisplayZoneController(this);

  /**
   * Page data from SSR
   */
  @property({ type: Object })
  pageData: PageData | null = null;

  /**
   * Project ID from URL
   */
  @property({ type: String })
  projectId = '';

  /**
   * Loading state
   */
  @state()
  private loading = true;

  /**
   * Project data
   */
  @state()
  private project: Project | null = null;

  /**
   * Whether the server-prefetched project in `pageData` was already offered
   * to a load. It is offered to the first load only: a retry, or a
   * reconnect of this element, always fetches.
   */
  private hydrationOffered = false;

  /**
   * Whether the page-load agents request (the first one, or a retry of it)
   * has not finished yet. The project header renders while it is pending;
   * the agents section shows a loading row instead of the empty state.
   */
  @state()
  private agentsInitialLoadPending = true;

  /** Error of the last failed page-load agents request; cleared by any adopted agents result. */
  @state()
  private agentsLoadError: string | null = null;

  /** Bumped per page-load agents request, so only the latest one clears `agentsInitialLoadPending`. */
  private agentsPageLoadSeq = 0;

  /**
   * Agents in this project
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
   * Scope-level capabilities from the agents list response
   */
  @state()
  private agentScopeCapabilities: Capabilities | undefined;

  /**
   * Hub-scope project capabilities (`_capabilities` of GET /api/v1/projects).
   * Clone and Create Template make a new project, which needs hub
   * `project.create` (project_clone.go), so they are hidden without it.
   */
  @state()
  private hubProjectCapabilities: Capabilities | undefined;

  /**
   * Active file tab key ('workspace' or shared dir name)
   */
  @state()
  private activeFileTab = 'workspace';

  /**
   * Per-tab file browser data sources keyed by tab name
   */
  private fileBrowserDataSources: Record<string, FileBrowserDataSource> = {};

  /**
   * Whether the (possibly below-the-fold) Files section has become visible
   * or was otherwise explicitly triggered. Until then, only a lightweight
   * placeholder is rendered — no `<scion-file-browser>` is mounted, so no
   * listing request is issued. See observeFilesSection().
   */
  @state()
  private filesSectionVisible = false;

  /**
   * Tab keys ('workspace' or shared-dir name) whose file browser has been
   * mounted at least once. A `<scion-file-browser>` is only instantiated for
   * tabs in this set, so switching to an unvisited tab is what triggers its
   * one initial listing request. Visited tabs stay mounted (not torn down on
   * tab switch) so filter/sort/scroll state and loaded files survive
   * switching between tabs. That state is still lost whenever the file
   * editor is opened and closed, because the tab group itself is unmounted
   * and remounted then (see renderFilesSection()) — unchanged from before
   * this lazy-mounting change.
   */
  @state()
  private visitedFileTabs: Set<string> = new Set();

  /** Observes the Files-section placeholder to lazily reveal it once it nears the viewport. */
  private filesSectionObserver: IntersectionObserver | null = null;

  /** The placeholder element currently registered with filesSectionObserver, if any. */
  private observedFilesPlaceholder: Element | null = null;

  /**
   * Loading state for stop-all action
   */
  @state()
  private stopAllLoading = false;

  /**
   * Current view mode (grid or list)
   */
  @state()
  private viewMode: ViewMode = 'grid';

  /** "Jump to agent" over the graph, offering the agents its tree view shows. */
  readonly graphPalette = new GraphPaletteController(this, {
    treeView: (): ScionAgentTreeView | null =>
      this.renderRoot.querySelector('scion-agent-tree-view'),
  });

  /**
   * Whether the agents section is expanded to full height. Collapsed (the
   * default) caps the grid/table at a fixed height with its own scrollbar.
   * Persisted per user (not per project) in localStorage.
   */
  @state()
  private agentsExpanded = false;

  @state()
  private phaseFilter: AgentPhase | '' = '';

  @state()
  private labelFilter = '';

  @state()
  private sortField: AgentSortField = 'updated';

  @state()
  private sortDir: SortDir = 'desc';

  /**
   * The label filter as of the last commit (`sl-change`/`sl-clear`), as
   * opposed to `labelFilter`, which also tracks every keystroke for local
   * preview. Passed to the window's planner, which decides
   * eligibility for the sorted request path and keys its per-label 422
   * refusal memory, and used for the request parameters.
   */
  private committedLabel = '';

  /** `committedLabel`'s value just before a label commit, so a failed request can restore it instead of leaving every later request re-sending a rejected label. */
  private labelBeforeCommit = '';

  /** Bumped at the start of every page-level agents load; checked after each `await` so an older trigger's response can never overwrite a newer one. */
  private agentsLoadGen = 0;

  /** Bumped on every view-state change, so a load can tell the view changed while it was in flight. */
  private viewEpoch = 0;

  /** Aborts whatever page-level agents request (not the window's own page fetches) was still in flight when a new trigger starts. */
  private agentsAbortController: AbortController | null = null;

  /**
   * Whether a page-level agents load (`loadAgentsForView`: a fit request or a drain)
   * is in flight. Ref-counted, so a single plain boolean cannot be cleared
   * early by an older trigger while a newer one is still in flight:
   * `beginLoadingIndicator`/`endLoadingIndicator` increment/decrement
   * `agentsLoadingCount`, so it stays `true` for as long as *any*
   * page-level load (including a nested 422-or-truncated-legacy fallback)
   * is in flight, regardless of how many overlapping triggers are racing.
   * Used for two things: the "Loading agents…" indicator, and, combined
   * with `agentWindow.loading`, disabling pager navigation and the chip
   * while either kind of request is in flight.
   */
  @state()
  private agentsLoading = false;

  private agentsLoadingCount = 0;

  private beginLoadingIndicator(): void {
    this.agentsLoadingCount++;
    this.agentsLoading = true;
  }

  private endLoadingIndicator(): void {
    this.agentsLoadingCount = Math.max(0, this.agentsLoadingCount - 1);
    this.agentsLoading = this.agentsLoadingCount > 0;
  }

  /**
   * Page size for the list view's window, persisted under
   * `scion-pagesize-project-agents`. Read once in `connectedCallback` —
   * `<scion-agent-pager>` is a controlled component
   * and does not read storage itself, so this is the single source of
   * truth both for the first request's `limit` and for the pager's
   * rendered value.
   */
  @state()
  private pagerPageSize: AgentPagerPageSize = 25;

  /** Forces a re-render when `agentWindow` changes outside of a `@state` setter (pagination, live updates, resync). */
  @state()
  private windowTick = 0;

  /** One lease timer for the agents this page shows (ptone/scion#2483 N4). */
  private readonly deletionLease = new DeletionLeaseController(this, () =>
    this.agentWindow.state === 'paged' ? this.agentWindow.items : this.agents
  );

  /**
   * The agent window behind the grid, list and tree views: small, paged,
   * held or capped. The local states read `this.agents` live through
   * `getHeldAgents` rather than a copy, so an SSE update applied by
   * `mergeAgentsChanged` is visible immediately with no re-adoption step.
   */
  private agentWindow = new AgentListWindow({
    viewState: {
      phaseFilter: this.phaseFilter,
      label: this.labelFilter,
      sortField: this.sortField,
      sortDir: this.sortDir,
      pageSize: this.pagerPageSize,
      view: listViewOf(this.viewMode),
    },
    getProjectId: () => this.projectId,
    fetchPage: (params: PagedPageParams) => this.fetchAgentsPage(params),
    getAgent: (id: string) => stateManager.getAgent(id),
    getHeldAgents: () => this.agents,
  });

  /** Runs the complete-set drains (complete-needing view states, a 422, the held or capped chip). */
  private drainRunner = new AgentDrainRunner();

  private boundOnWindowChange = () => {
    this.windowTick++;
  };

  private boundOnAgentsChanged = (e: Event) => {
    // `notifyWithData` wraps the payload as `{state, data}` (state.ts); the
    // `AgentsChangedDetail` itself is `detail.data`. The paged state merges
    // through the window; the small/held state merges
    // through `mergeChanged` instead of a per-event full rebuild (this
    // replaces the old `onAgentsUpdated`).
    const detail = (e as CustomEvent<{ data: AgentsChangedDetail }>).detail.data;
    this.agentWindow.applyChanges(detail);
    if (this.agentWindow.state !== 'paged') {
      this.mergeAgentsChanged(detail);
    }
  };

  private boundOnAgentsResync = () => {
    this.agentWindow.markResync();
  };

  /**
   * "Agents"/"Running" stats and Stop-all visibility: the member index
   * while paged, or H (`this.agents`) in the local states.
   */
  private get agentStats(): { total: number; running: number } {
    return this.agentWindow.stats;
  }

  /**
   * The "Agents" or "Running" stat. An incomplete set counts only the
   * loaded agents, so the figure says so, worded like the window banner
   * for the same reason (capped or failed).
   */
  private renderAgentStat(kind: 'total' | 'running'): string | TemplateResult {
    const stats = this.agentWindow.stats;
    const value = formatNumber(stats[kind]);
    const reason = this.agentWindow.incompleteReason;
    if (!stats.incomplete || !reason) return value;
    const note = incompleteStatNote(reason, kind);
    return html`${value}<span class="stat-incomplete">${note}</span>`;
  }

  /**
   * Whether a git pull is in progress
   */
  @state()
  private pullLoading = false;

  /**
   * Result of the last git pull operation
   */
  @state()
  private pullResult: {
    status: string;
    updated?: boolean;
    commits?: { hash: string; subject: string }[];
    error?: string;
  } | null = null;

  /**
   * Metrics summary for the project (null = not loaded or unavailable)
   */
  @state()
  private metricsSummary: {
    sessionsCount24h: number;
    apiCalls24h: number;
    tokenUsage24h: number;
    activeAgents24h: number;
    periodLabel: string;
  } | null = null;

  /**
   * DB-backed session metrics summary for the project.
   */
  @state()
  private sessionMetricsSummary: ProjectSessionMetricsSummary | null = null;

  /**
   * Whether the messages section is expanded (lazy-load trigger)
   */
  @state()
  private messagesExpanded = false;

  /**
   * Path of the file currently open in the editor (null = editor closed, '' = new file)
   */
  @state()
  private editingFilePath: string | null = null;

  /**
   * Whether to open the editor initially in preview mode (for .md eye icon)
   */
  @state()
  private editorInitialPreview = false;

  /**
   * Per-tab editor data sources keyed by tab name
   */
  private editorDataSources: Record<string, FileEditorDataSource> = {};

  /**
   * Whether the clone dialog is open
   */
  @state()
  private cloneDialogOpen = false;

  /**
   * Name for the cloned project
   */
  @state()
  private cloneName = '';

  /**
   * Whether the clone operation is in progress
   */
  @state()
  private cloneLoading = false;

  /**
   * Error message from clone operation (shown inline in dialog)
   */
  @state()
  private cloneError = '';

  /** Whether the create-template dialog is open */
  @state()
  private templateDialogOpen = false;

  /** Name for the new template */
  @state()
  private templateName = '';

  /** Whether the create-template operation is in progress */
  @state()
  private templateLoading = false;

  /** Error message from create-template operation */
  @state()
  private templateError = '';

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 1.5rem;
      gap: 1rem;
    }

    .header-info {
      flex: 1;
    }

    .header-title {
      display: flex;
      align-items: flex-start;
      gap: 0.75rem;
      margin-bottom: 0.5rem;
    }

    .header-title > sl-icon {
      flex-shrink: 0;
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
      /* Centre the icon on the first line of the name: (1.95rem h1 line box
         - 1.5rem icon) / 2. */
      margin-top: 0.225rem;
    }
    /* A long name wraps on its own line; the badges then follow on the next
       line instead of floating beside a multi-line name. */
    .header-title-text {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.5rem 0.75rem;
      min-width: 0;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      line-height: 1.3;
      color: var(--scion-text, #1e293b);
      margin: 0;
      min-width: 0;
      overflow-wrap: anywhere;
    }
    /* The linked-project icon inside the title keeps the primary colour. */
    .header h1 sl-icon {
      color: var(--scion-primary, #3b82f6);
    }

    .header-path {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
      word-break: break-all;
    }

    .header-actions {
      display: flex;
      gap: 0.5rem;
      flex-shrink: 0;
    }

    .stats-row {
      display: flex;
      gap: 2rem;
      margin-bottom: 2rem;
      padding: 1.25rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .stat {
      display: flex;
      flex-direction: column;
    }

    .stat-label {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
      margin-bottom: 0.25rem;
    }

    .stat-value {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
    }

    .stat-incomplete {
      display: block;
      font-size: 0.75rem;
      font-weight: 400;
      color: var(--scion-text-muted, #64748b);
    }

    .section-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1rem;
    }

    .section-header h2 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    /* Matches the shared .resource-grid in resource-styles.ts. When expanded
       the section flows at full height; when collapsed (the default) it is
       capped by .agents-collapsed below. */
    .agent-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
      gap: 1.5rem;
    }

    /* Collapsed agents section: fixed max-height with a slim, themed
       scrollbar. The expand toggle in the section header removes the cap. */
    .agent-grid.agents-collapsed,
    .agent-table-container.agents-collapsed {
      max-height: 26rem;
      overflow-y: auto;
      scrollbar-width: thin;
      scrollbar-color: var(--scion-border, #cbd5e1) transparent;
    }

    /* Keep column headers visible while the collapsed table scrolls. With
       border-collapse the th border-bottom does not stick, so draw the
       divider with an inset shadow instead. */
    .agent-table-container.agents-collapsed th {
      position: sticky;
      top: 0;
      z-index: 1;
      box-shadow: inset 0 -1px 0 var(--scion-border, #e2e8f0);
    }

    .agent-grid.agents-collapsed {
      /* Room so card hover shadows/borders are not clipped by the scroller. */
      padding: 2px 0.5rem 2px 2px;
    }

    .agents-collapsed::-webkit-scrollbar {
      width: 8px;
      height: 8px;
    }

    .agents-collapsed::-webkit-scrollbar-track {
      background: transparent;
    }

    .agents-collapsed::-webkit-scrollbar-thumb {
      background: var(--scion-border, #cbd5e1);
      border-radius: 9999px;
    }

    .agents-collapsed::-webkit-scrollbar-thumb:hover {
      background: var(--scion-text-muted, #94a3b8);
    }

    .agents-expand-toggle {
      font-size: 1rem;
      color: var(--scion-text-muted, #64748b);
    }

    .agent-card scion-deletion-banner {
      margin-top: 0.75rem;
    }

    td scion-deletion-banner {
      margin-top: 0.25rem;
    }

    .agent-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      transition: all var(--scion-transition-fast, 150ms ease);
      text-decoration: none;
      color: inherit;
      display: block;
    }

    .agent-card:hover {
      border-color: var(--scion-primary, #3b82f6);
      box-shadow: var(--scion-shadow-md, 0 4px 6px -1px rgba(0, 0, 0, 0.1));
    }

    .agent-header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 0.75rem;
      gap: 0.5rem;
      flex-wrap: wrap;
    }

    /* Let the name column shrink so .agent-name can wrap; without this a
       flex item's min-width:auto holds the header open at the full name. */
    .agent-header > div {
      /* Full-width basis so the badge always wraps to its own row. A wide
         status label like "waiting on parent" would otherwise crush the name
         to a few characters, the same failure the agents grid had — it is the
         badge's width that matters, not how many there are. */
      flex: 1 1 100%;
      min-width: 0;
    }

    .agent-header > scion-status-badge {
      flex-shrink: 0;
    }

    .agent-name {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
      display: flex;
      align-items: center;
      gap: 0.5rem;
      min-width: 0;
    }

    .agent-name sl-icon {
      color: var(--scion-primary, #3b82f6);
      flex-shrink: 0;
    }

    /* Wrapping has to land on the anchor that holds the text; the flex parent
       only bounds it. overflow-wrap:anywhere covers the hard case — a long
       name with no spaces or hyphens, which has nowhere else to break. */
    .agent-name > a {
      min-width: 0;
      overflow-wrap: anywhere;
    }

    .agent-meta {
      font-size: 0.813rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
    }

    .agent-meta sl-icon {
      font-size: 0.875rem;
      vertical-align: -0.125em;
      opacity: 0.7;
    }

    .broker-link {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
    }

    .broker-link:hover {
      color: var(--scion-primary, #3b82f6);
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

    /* overflow-x: auto keeps the rounded corners clipping the table while
       allowing horizontal scrolling on smaller screens. The collapsed height
       cap is shared with the grid via .agents-collapsed. */
    .agent-table-container {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow-x: auto;
    }

    .agent-table-container table {
      width: 100%;
      border-collapse: collapse;
    }

    .agent-table-container th {
      text-align: left;
      padding: 0.75rem 1rem;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .agent-table-container td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }

    .agent-table-container tr:last-child td {
      border-bottom: none;
    }

    .agent-table-container tr:hover td {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .agent-table-container .name-cell {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-weight: 500;
    }

    .agent-table-container .name-cell sl-icon {
      color: var(--scion-primary, #3b82f6);
      flex-shrink: 0;
    }

    .agent-table-container .name-cell a {
      color: inherit;
      text-decoration: none;
    }

    .agent-table-container .name-cell a:hover {
      text-decoration: underline;
    }

    .agent-table-container .status-col {
      min-width: 11rem;
    }

    .agent-table-container .task-cell {
      display: -webkit-box;
      -webkit-line-clamp: 2;
      -webkit-box-orient: vertical;
      overflow: hidden;
      max-width: 250px;
      white-space: normal;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
    }

    .agent-table-container .actions-cell {
      text-align: right;
      white-space: nowrap;
    }

    .table-actions {
      display: flex;
      gap: 0.375rem;
      justify-content: flex-end;
    }

    .empty-state {
      text-align: center;
      padding: 4rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px dashed var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .empty-state > sl-icon {
      font-size: 4rem;
      color: var(--scion-text-muted, #64748b);
      opacity: 0.5;
      margin-bottom: 1rem;
    }

    .empty-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .empty-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1.5rem 0;
    }

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 4rem 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .loading-state sl-spinner {
      font-size: 2rem;
      margin-bottom: 1rem;
    }

    .error-state {
      text-align: center;
      padding: 3rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .error-state sl-icon {
      font-size: 3rem;
      color: var(--sl-color-danger-500, #ef4444);
      margin-bottom: 1rem;
    }

    .error-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .error-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .error-details {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      color: var(--sl-color-danger-700, #b91c1c);
      margin-bottom: 1rem;
    }

    .clone-summary {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 1rem;
      line-height: 1.5;
    }

    .clone-summary strong {
      color: var(--scion-text, #1e293b);
    }

    .clone-slug-preview {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.5rem;
    }

    .clone-error {
      color: var(--sl-color-danger-700, #b91c1c);
      font-size: 0.875rem;
      margin-top: 0.75rem;
    }

    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }

    .back-link:hover {
      color: var(--scion-primary, #3b82f6);
    }

    .header-path a {
      color: inherit;
      text-decoration: none;
    }

    .header-path a:hover {
      color: var(--scion-primary, #3b82f6);
    }

    .workspace-section {
      margin-top: 2rem;
      margin-bottom: 2rem;
    }

    .workspace-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1rem;
    }

    .workspace-header-left {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .workspace-header h2 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .files-tab-group {
      margin-bottom: 0;
    }

    .files-tab-group::part(base) {
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .files-tab-group::part(body) {
      padding: 0;
    }

    .editor-back-row {
      margin-bottom: 0.5rem;
    }

    .tab-label-truncated {
      max-width: 10rem;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      display: inline-block;
      vertical-align: bottom;
    }

    .pull-commits {
      margin-top: 0.375rem;
      max-height: 8rem;
      overflow-y: auto;
      font-family: var(--sl-font-mono, monospace);
      line-height: 1.5;
      color: var(--scion-text, #1e293b);
    }

    .pull-commits .commit-hash {
      color: var(--sl-color-primary-600, #2563eb);
      margin-right: 0.375rem;
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

    .empty-filter-state {
      text-align: center;
      padding: 3rem 2rem;
      color: var(--scion-text-muted, #64748b);
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

    @media (max-width: 768px) {
      .hide-mobile {
        display: none;
      }
    }
  `;

  private boundOnProjectsUpdated = this.onProjectsUpdated.bind(this);

  override connectedCallback(): void {
    super.connectedCallback();
    // SSR property bindings (.projectId=) aren't restored during client-side
    // hydration for top-level page components. Fall back to URL parsing.
    if (!this.projectId && typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/projects\/([^/]+)/);
      if (match) {
        this.projectId = match[1];
      }
    }

    // Read persisted view mode
    const stored = localStorage.getItem('scion-view-project-agents') as ViewMode | null;
    if (stored === 'grid' || stored === 'list' || stored === 'graph') {
      this.viewMode = stored;
    }

    // Read persisted agents section expand/collapse preference (user-level)
    this.agentsExpanded = localStorage.getItem(AGENTS_EXPANDED_STORAGE_KEY) === 'true';

    // Read persisted phase filter
    const storedPhase = localStorage.getItem(`scion-filter-project-agents-phase-${this.projectId}`);
    if (
      storedPhase === 'running' ||
      storedPhase === 'stopping' ||
      storedPhase === 'stopped' ||
      storedPhase === 'suspended' ||
      storedPhase === 'error'
    ) {
      this.phaseFilter = storedPhase;
    }

    // Read persisted sort
    const storedSort = localStorage.getItem(`scion-sort-project-agents-${this.projectId}`);
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

    // Read the persisted page size before the window's
    // view state is synced and the first request is sent, so both the
    // request's `limit` and the pager's rendered size start out correct —
    // `<scion-agent-pager>` is a controlled component and no longer reads
    // storage itself.
    const storedPageSize = Number(localStorage.getItem(PAGER_PAGE_SIZE_STORAGE_KEY));
    if (AGENT_PAGER_PAGE_SIZES.includes(storedPageSize as AgentPagerPageSize)) {
      this.pagerPageSize = storedPageSize as AgentPagerPageSize;
    }

    // Sync the window's view state with the persisted values read above,
    // before the initial load.
    this.agentWindow.setViewState({
      phaseFilter: this.phaseFilter,
      label: this.labelFilter,
      sortField: this.sortField,
      sortDir: this.sortDir,
      pageSize: this.pagerPageSize,
      view: listViewOf(this.viewMode),
    });

    // Set SSE scope to this project (receives all agent events within
    // project) before the first load: a drain belongs to the scope
    // generation it started in, and is discarded if the scope changes
    // underneath it.
    if (this.projectId) {
      stateManager.setScope({ type: 'project', projectId: this.projectId });
    }

    void this.loadData();
    void this.loadHubProjectCapabilities();

    // Listen for real-time updates
    stateManager.addEventListener('projects-updated', this.boundOnProjectsUpdated as EventListener);
    stateManager.addEventListener('agents-changed', this.boundOnAgentsChanged as EventListener);
    stateManager.addEventListener('agents-resync', this.boundOnAgentsResync as EventListener);
    this.agentWindow.addEventListener('change', this.boundOnWindowChange);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener(
      'projects-updated',
      this.boundOnProjectsUpdated as EventListener
    );
    stateManager.removeEventListener('agents-changed', this.boundOnAgentsChanged as EventListener);
    stateManager.removeEventListener('agents-resync', this.boundOnAgentsResync as EventListener);
    this.agentWindow.removeEventListener('change', this.boundOnWindowChange);
    this.cancelAgentsLoad();
    this.filesSectionObserver?.disconnect();
    this.filesSectionObserver = null;
    this.observedFilesPlaceholder = null;
  }

  override willUpdate(changed: Map<string, unknown>): void {
    super.willUpdate(changed);
    // Must run before render: if the tab list changed (a shared dir arrived
    // or disappeared via an SSE project update) and left activeFileTab
    // pointing at a tab that no longer exists, rendering against a stale
    // activeFileTab would mount nothing for it — Shoelace silently falls
    // back to displaying tabs[0] without firing sl-tab-show, so nothing
    // else would notice or mount it. Doing this in willUpdate (before
    // render), rather than updated() (after), sets
    // activeFileTab/visitedFileTabs before Lit renders instead of
    // scheduling a second render to fix up a frame that already rendered
    // wrong.
    //
    // Also re-checked on an editingFilePath change: normalizeActiveFileTab()
    // itself skips entirely while the editor is open (see its doc comment),
    // so a correction that a project update would otherwise have triggered
    // while editing is deferred — this is what catches it up on the very
    // next update after the editor closes, rather than leaving it stale
    // indefinitely.
    if (changed.has('project') || changed.has('editingFilePath')) {
      this.normalizeActiveFileTab();
    }
  }

  override updated(changed: Map<string, unknown>): void {
    super.updated(changed);
    this.observeFilesSection();
  }

  /**
   * Keep activeFileTab pointing at a tab that actually exists, and drop
   * visitedFileTabs entries for tabs that no longer exist.
   *
   * activeFileTab defaults to 'workspace' and is otherwise only assigned in
   * loadData() (initial load) and onFileTabChange() (user click). Neither
   * one runs when the tab list itself changes later — e.g. a live project
   * update adds or removes a shared dir (onProjectsUpdated() merges new
   * project fields, including sharedDirs, straight into `this.project`).
   * Left uncorrected, activeFileTab can name a tab that no longer renders,
   * and Shoelace's tab-group falls back to displaying tabs[0] internally
   * without emitting sl-tab-show — so the panel shown has no
   * scion-file-browser mounted for it and stays empty with no way for the
   * user to recover by clicking (setActiveTab no-ops when asked to
   * activate the tab it already considers active).
   *
   * Skipped entirely while the file editor is open: the editor's data
   * source is derived from activeFileTab (see renderFilesSection()), and
   * retargeting it out from under an open edit would silently redirect a
   * Save to a different shared dir, or — for an existing file — discard
   * unsaved edits by reloading a different tab's content. Leaving
   * activeFileTab stale while editing matches what happens today if its
   * shared dir is removed server-side: the save targets a directory that no
   * longer exists and fails, which is safer than silently retargeting.
   */
  private normalizeActiveFileTab(): void {
    if (this.editingFilePath !== null) return;
    if (!this.project) return;
    const tabs = this.getFileTabs();
    const tabKeys = new Set(tabs.map((t) => t.key));

    if (tabs.length > 0 && !tabKeys.has(this.activeFileTab)) {
      this.activeFileTab = tabs[0].key;
    }
    // If the Files section is already open, the (possibly just-corrected)
    // active tab must actually be mounted — see the note above on why
    // nothing else would notice it needs to be.
    if (tabs.length > 0 && this.filesSectionVisible) {
      this.markFileTabVisited(this.activeFileTab);
    }

    // Drop visited-tab entries for tabs that no longer exist, so a shared
    // dir removed and later re-added under the same name starts fresh
    // (unvisited) instead of mounting hidden and issuing a listing request
    // for a tab the user never actually opened this time around.
    if ([...this.visitedFileTabs].some((key) => !tabKeys.has(key))) {
      this.visitedFileTabs = new Set([...this.visitedFileTabs].filter((key) => tabKeys.has(key)));
    }
  }

  /**
   * Lazily reveal the Files section once its placeholder nears the
   * viewport, so a project page that never scrolls that far mounts zero
   * file browsers and issues zero listing requests. Without
   * IntersectionObserver support (older engines, tests) the section is
   * revealed immediately — equivalent to "explicitly opened".
   *
   * Viewport visibility is the only reveal trigger; there is no separate
   * click-to-open affordance on the placeholder itself. A section already
   * in the initial viewport still reveals promptly, on its first
   * intersection check.
   */
  private observeFilesSection(): void {
    if (this.filesSectionVisible) return;
    const placeholder = this.shadowRoot?.querySelector('.files-section-placeholder');
    if (!placeholder) {
      // shouldShowFilesSection() flipped to false before reveal (e.g. the
      // last shared dir was removed via a live project update), so Lit tore
      // down the placeholder without replacing it. Stop watching the
      // detached element now, instead of holding it until the section
      // reappears (the "replaced" branch below) or the element is disconnected.
      if (this.observedFilesPlaceholder) {
        this.filesSectionObserver?.unobserve(this.observedFilesPlaceholder);
        this.observedFilesPlaceholder = null;
      }
      return;
    }

    if (typeof IntersectionObserver !== 'function') {
      this.revealFilesSection();
      return;
    }

    if (!this.filesSectionObserver) {
      this.filesSectionObserver = new IntersectionObserver(
        (entries) => {
          for (const entry of entries) {
            if (entry.isIntersecting) {
              this.revealFilesSection();
              break;
            }
          }
        },
        { rootMargin: '200px' }
      );
    }
    if (this.observedFilesPlaceholder && this.observedFilesPlaceholder !== placeholder) {
      // The previous placeholder was replaced (e.g. shouldShowFilesSection()
      // flipped false then true before reveal, so Lit tore down and
      // recreated the placeholder element) — stop watching the detached one.
      this.filesSectionObserver.unobserve(this.observedFilesPlaceholder);
    }
    if (this.observedFilesPlaceholder !== placeholder) {
      this.filesSectionObserver.observe(placeholder);
      this.observedFilesPlaceholder = placeholder;
    }
  }

  private revealFilesSection(): void {
    if (this.filesSectionVisible) return;
    this.filesSectionObserver?.disconnect();
    this.observedFilesPlaceholder = null;
    this.filesSectionVisible = true;
    // The active tab (default, or the first shared dir for git-based
    // projects — see loadData()) is the one the user actually sees, so
    // mount its browser now rather than waiting for a tab click.
    this.markFileTabVisited(this.activeFileTab);
  }

  private markFileTabVisited(tab: string): void {
    if (this.visitedFileTabs.has(tab)) return;
    this.visitedFileTabs = new Set(this.visitedFileTabs).add(tab);
  }

  /**
   * Live updates for the small/held state: one
   * `agents-changed` flush merged through `mergeChanged`, replacing the old
   * `onAgentsUpdated` per-event full rebuild over `stateManager.getAgents()`.
   * Never called while the window is paged — `boundOnAgentsChanged` gates
   * the call to this method on `agentWindow.state !== 'paged'`, since
   * `this.agents` is intentionally empty then and the list view's live
   * updates go through `agentWindow.applyChanges` instead,
   * called unconditionally before that gate.
   */
  private mergeAgentsChanged(detail: AgentsChangedDetail): void {
    // Lazily derive scope capabilities from existing agents if not yet set
    // (e.g. a delta arrives before the page's own load has set it).
    if (!this.agentScopeCapabilities) {
      for (const a of this.agents) {
        if (a._capabilities) {
          this.agentScopeCapabilities = a._capabilities;
          break;
        }
      }
    }
    const merged = mergeChanged(this.agents, detail, {
      getAgent: (id) => stateManager.getAgent(id),
      // Today's add rule: any agent in this project, or an ID
      // already held (e.g. one whose projectId changed underneath it keeps
      // getting its updates until an explicit `deleted` removes it).
      shouldAdd: (agent) => agent.projectId === this.projectId,
      scopeCapabilities: this.agentScopeCapabilities,
    });
    if (merged !== this.agents) {
      this.agents = merged;
    }
  }

  private onProjectsUpdated(): void {
    const updatedProject = stateManager.getProject(this.projectId);
    if (updatedProject && this.project) {
      this.project = { ...this.project, ...updatedProject };
    }
  }

  private async loadHubProjectCapabilities(): Promise<void> {
    const caps = await fetchHubProjectCapabilities();
    if (!this.isConnected) return;
    this.hubProjectCapabilities = caps;
  }

  /**
   * Loads the project and, independently, the agents window's first
   * request. The project header renders as soon as the project is known,
   * without waiting for the agents: a cold load with a matching
   * server-prefetched project uses it and sends no project request, while
   * client-side navigation, a retry or a mismatched payload fetches it.
   * Resolves when both have finished.
   */
  private async loadData(): Promise<void> {
    this.error = null;
    const agentsLoad = this.loadPageAgents();

    const offered = this.hydrationOffered
      ? null
      : hydratedProjectFor(this.pageData, this.projectId);
    this.hydrationOffered = true;
    if (offered) {
      this.applyLoadedProject(offered);
      this.loading = false;
      await agentsLoad;
      return;
    }

    this.loading = true;
    try {
      const projectResponse = await apiFetch(`/api/v1/projects/${this.projectId}`);

      if (!projectResponse.ok) {
        throw new Error(
          await extractApiError(
            projectResponse,
            `HTTP ${projectResponse.status}: ${projectResponse.statusText}`
          )
        );
      }

      this.applyLoadedProject((await projectResponse.json()) as Project);
    } catch (err) {
      console.error('Failed to load project:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load project';
    } finally {
      this.loading = false;
    }
    await agentsLoad;
  }

  /**
   * The agents window's page-load request, tracked separately from the
   * project: its loading and failure states belong to the agents section,
   * never to the page as a whole.
   */
  private async loadPageAgents(): Promise<void> {
    const seq = ++this.agentsPageLoadSeq;
    this.agentsInitialLoadPending = true;
    this.agentsLoadError = null;
    try {
      await this.loadAgentsForView('page-load');
    } finally {
      if (seq === this.agentsPageLoadSeq) this.agentsInitialLoadPending = false;
    }
  }

  /** The agents section's Retry after a failed page-load request. */
  private retryAgentsLoad(): void {
    if (this.agentsLoading || this.agentWindow.loading) return;
    void this.loadPageAgents();
  }

  private applyLoadedProject(project: Project): void {
    this.project = project;
    dispatchPageTitle(this, this.project.name || this.projectId, 'Projects');

    stateManager.seedProjects([this.project]);

    // Pre-create data sources for file tabs (the component loads files on connect)
    if (this.hasProjectWorkspace()) {
      this.getTabDataSource('workspace');
    }
    // Without a project workspace (per-agent git, empty per agent), activate
    // the first shared dir
    if (!this.hasProjectWorkspace() && this.project.sharedDirs?.length) {
      this.activeFileTab = this.project.sharedDirs[0].name;
      this.getTabDataSource(this.project.sharedDirs[0].name);
    }

    // Fetch metrics summary (non-blocking, gracefully degrades)
    void this.loadMetricsSummary();
    void this.loadSessionMetricsSummary();

    // Auto-discover GitHub App installation if project has a GitHub remote but no installation
    if (
      this.project.gitRemote &&
      /github\.com[/:]/.test(this.project.gitRemote) &&
      this.project.githubInstallationId == null
    ) {
      void this.autoDiscoverGitHubApp();
    }
  }

  private async loadMetricsSummary(): Promise<void> {
    try {
      const res = await apiFetch(`/api/v1/projects/${this.projectId}/metrics-summary`);
      if (!res.ok) {
        this.metricsSummary = null;
        return;
      }
      const data = await res.json();
      // If metrics service is unavailable, the backend returns {available: false}
      if (data && data.available === false) {
        this.metricsSummary = null;
        return;
      }
      this.metricsSummary = data;
    } catch {
      this.metricsSummary = null;
    }
  }

  private async loadSessionMetricsSummary(): Promise<void> {
    try {
      const res = await apiFetch(`/api/v1/projects/${this.projectId}/metrics/summary`);
      if (res.ok) {
        this.sessionMetricsSummary = (await res.json()) as ProjectSessionMetricsSummary;
      } else {
        this.sessionMetricsSummary = null;
      }
    } catch {
      this.sessionMetricsSummary = null;
    }
  }

  private formatTokenCount(n: number): string {
    if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(1)}B`;
    if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
    if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
    return formatNumber(n);
  }

  private backgroundRefresh(trigger: AgentsViewTrigger = 'lifecycle-refresh'): void {
    this.fetchAndMergeAgents(trigger).catch((err) => {
      console.warn('Background refresh failed:', err);
    });
  }

  /** Label commit, lifecycle/stop-all refresh and the chip all land here, same as `loadData`. */
  private async fetchAndMergeAgents(
    trigger: AgentsViewTrigger = 'lifecycle-refresh'
  ): Promise<void> {
    await this.loadAgentsForView(trigger);
  }

  /**
   * Starts a new page-level agents load: bumps the stale-response guard
   * (two overlapping triggers, e.g. a label commit
   * racing a lifecycle refresh, must not let the older response win) and
   * aborts whatever page-level request or drain was still in flight.
   * Returns the generation to check after every `await` and the signal to
   * pass to `apiFetch`. Does not apply to the window's own page fetches
   * (`fetchAgentsPage`), which already guard themselves with their own
   * generation counter.
   */
  private beginAgentsLoad(): { gen: number; signal: AbortSignal } {
    this.cancelAgentsLoad();
    const controller = new AbortController();
    this.agentsAbortController = controller;
    const gen = ++this.agentsLoadGen;
    return { gen, signal: controller.signal };
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
   * A single place to react to any failed agents-load request, regardless
   * of *how* it failed (a network error or a non-OK response) or which
   * request (fit or drain) hit it. A label commit reverts `committedLabel`
   * to its pre-commit value, so a rejected label is not kept re-sent on
   * the next request. A view-change clears the paged window's navigation,
   * since the stored cursors were minted under the previous phase, dir or
   * label and would 400 if replayed under the new, now-current params.
   * A page-load additionally empties the page: there is no previous data
   * to keep. Every other trigger keeps the previous data, with today's
   * client label filter applied to it.
   */
  private onAgentsLoadFailed(trigger: AgentsViewTrigger): void {
    if (trigger === 'page-load') {
      this.agentsLoadError = 'Could not load agents.';
      this.agents = [];
      this.agentScopeCapabilities = undefined;
      this.agentWindow.setSmall();
    }
    if (trigger === 'label-commit') {
      this.committedLabel = this.labelBeforeCommit;
    }
    if (trigger === 'view-change' && this.agentWindow.state === 'paged') {
      this.agentWindow.invalidateCursors();
    }
  }

  /**
   * The project page's single request-choosing function. Every trigger
   * calls it once; the window's planner picks the one request the trigger
   * needs in the current state: a fit request, a drain, a paged refresh
   * (the paged chip), or nothing. When the view state changed while the
   * request was in flight and the adopted result no longer fits it, plans
   * again for the current view state.
   */
  private async loadAgentsForView(trigger: AgentsViewTrigger): Promise<void> {
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
          ? await this.loadFitAgents(trigger, label)
          : await this.drainProjectAgents(trigger, label);
    } finally {
      this.endLoadingIndicator();
    }
    if (adopted) this.agentsLoadError = null;
    if (
      adopted &&
      viewEpoch !== this.viewEpoch &&
      this.agentWindow.planRequest('view-change', this.committedLabel.trim()) !== 'none'
    ) {
      await this.loadAgentsForView('view-change');
    }
  }

  /**
   * The sorted first request of a sorted-eligible view state: complete
   * (small), paged, or 422 (drain). Resolves `true` when a result was
   * adopted.
   */
  private async loadFitAgents(trigger: AgentsViewTrigger, label: string): Promise<boolean> {
    const { gen, signal } = this.beginAgentsLoad();
    const ticket = this.agentWindow.beginSortedRequest(trigger, label);
    // Opened before the request is sent, so a live create or update that
    // lands while it is in flight survives the (older) response.
    const epoch = new AgentSeedEpoch();
    try {
      return await this.loadFitAgentsInEpoch(trigger, label, gen, signal, epoch, ticket);
    } finally {
      epoch.close();
      this.agentWindow.endSortedRequest(ticket);
    }
  }

  private async loadFitAgentsInEpoch(
    trigger: AgentsViewTrigger,
    label: string,
    gen: number,
    signal: AbortSignal,
    epoch: AgentSeedEpoch,
    ticket: SortedRequestTicket
  ): Promise<boolean> {
    const params = new URLSearchParams();
    params.set('sort', this.serverSortField);
    params.set('dir', this.sortDir);
    params.set('limit', String(this.pagerPageSize));
    params.set('fit', String(projectAgentsFitFor(this.pagerPageSize)));
    params.set('stats', '1');
    if (label) params.set('label', label);
    if (this.phaseFilter) params.set('phase', this.phaseFilter);

    let response: Response;
    try {
      response = await apiFetch(`/api/v1/projects/${this.projectId}/agents?${params.toString()}`, {
        signal,
      });
    } catch (err) {
      if (this.isStaleAgentsLoad(gen) || this.isAbortError(err)) return false; // superseded.
      console.warn('Failed to load agents:', err);
      this.onAgentsLoadFailed(trigger);
      return false;
    }
    if (this.isStaleAgentsLoad(gen)) return false;

    if (response.status === 422) {
      // Candidate ceiling: remember the refusal for this committed label
      // and drain instead.
      this.agentWindow.recordRefusal(label);
      this.agentWindow.endSortedRequest(ticket);
      epoch.close(); // the drain runs its own epoch.
      return await this.drainProjectAgents(trigger, label, gen);
    }

    if (!response.ok) {
      this.onAgentsLoadFailed(trigger);
      return false;
    }

    let data: SortedAgentsResponse;
    try {
      data = (await response.json()) as SortedAgentsResponse;
    } catch (err) {
      if (this.isStaleAgentsLoad(gen)) return false; // stale: nothing to revert/invalidate.
      if (this.isAbortError(err)) return false;
      console.warn('Failed to load agents:', err);
      this.onAgentsLoadFailed(trigger);
      return false;
    }
    if (this.isStaleAgentsLoad(gen)) return false;
    if (data._capabilities) {
      this.agentScopeCapabilities = data._capabilities;
    }

    // A REST response can race an SSE `deleted` already processed: the
    // epoch's seed leaves any such row out of both the store and the
    // page's own copies (`this.agents`, the window).
    if (data.complete) {
      // The complete set: the response plus live creates it predates.
      this.agents = epoch.seed(data.agents || [], {
        partial: false,
        isMember: (agent) => this.isProjectMember(agent, label),
      }).agents;
      if (!this.agentScopeCapabilities) {
        this.agentScopeCapabilities = this.agents.find((a) => a._capabilities)?._capabilities;
      }
      this.agentWindow.setSmall(epoch.sawResync);
    } else {
      // Paged: `this.agents` stays empty; stats and Stop-all read the
      // member index through `agentStats` instead.
      this.agents = [];
      this.agentWindow.setPaged(this.seedPage(epoch, data), label, ticket.key);
    }
    return true;
  }

  /**
   * The complete-set drain of a complete-needing view state, a 422, or the
   * held/capped chip. Its first page is today's legacy request. It ends in
   * small (one page), held, or capped. A first page that fails (for
   * example a label 400) keeps the previous data. Resolves `true` when a
   * result was adopted.
   */
  private async drainProjectAgents(
    trigger: AgentsViewTrigger,
    label: string,
    carriedGen?: number
  ): Promise<boolean> {
    const gen = carriedGen ?? this.beginAgentsLoad().gen;

    const params = new URLSearchParams();
    if (label.includes('=')) params.append('label', label);
    const qs = params.toString();
    const url = qs
      ? `/api/v1/projects/${this.projectId}/agents?${qs}`
      : `/api/v1/projects/${this.projectId}/agents`;

    let result: SeededDrainResult | null;
    try {
      result = await this.drainRunner.run({
        url,
        view: 'full',
        isMember: (agent) => this.isProjectMember(agent, label),
      });
    } catch (err) {
      if (this.isStaleAgentsLoad(gen) || this.isAbortError(err)) return false;
      console.warn('Failed to load agents:', err);
      this.onAgentsLoadFailed(trigger);
      return false;
    }
    if (!result || this.isStaleAgentsLoad(gen)) return false; // superseded.

    if (result.firstPageFailed) {
      console.warn('Failed to load agents:', result.error?.message);
      this.onAgentsLoadFailed(trigger);
      return false;
    }

    this.agents = result.agents;
    this.agentScopeCapabilities =
      result.capabilities ?? this.agents.find((a) => a._capabilities)?._capabilities;
    this.agentWindow.adoptDrain(result);
    return true;
  }

  /** Membership of a live-created agent during a drain: this project, and the committed `k=v` label, if any. */
  private isProjectMember(agent: Agent, label: string): boolean {
    if (agent.projectId !== this.projectId) return false;
    const eq = label.indexOf('=');
    if (eq < 0) return true;
    return agent.labels?.[label.slice(0, eq)] === label.slice(eq + 1);
  }

  /** The server sort of a sorted request: `created`, or `updated` for every other sort field. */
  private get serverSortField(): 'updated' | 'created' {
    return this.sortField === 'created' ? 'created' : 'updated';
  }

  /**
   * A view-state change (view, sort, phase or page size): one request only
   * if the planner says so. A sorted first request still in flight that no
   * longer fits the view state is superseded first: a page load or label
   * commit is sent again for the new view state.
   */
  private onAgentViewStateChanged(): void {
    this.viewEpoch++;
    const superseded = this.agentWindow.supersededRequest(this.committedLabel);
    if (superseded) {
      this.cancelAgentsLoad();
      if (superseded === 'page-load' || superseded === 'label-commit') {
        this.backgroundRefresh(superseded);
        return;
      }
    }
    this.backgroundRefresh('view-change');
  }

  /**
   * Apply an optimistic lifecycle patch in every state: the patched agents
   * merge into H through `mergeChanged` in the local states, and replace
   * their on-page rows while paged. `deletedIds` leave H at once; while
   * paged they leave the page with the follow-up refresh or SSE delete.
   */
  private applyOptimisticAgents(patch: Agent[], deletedIds: string[] = []): void {
    if (this.agentWindow.isLocal) {
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
          scopeCapabilities: this.agentScopeCapabilities,
        }
      );
    } else {
      this.agentWindow.applyLocalUpdate(patch);
    }
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
   * `stats` with any already-tombstoned ID dropped from `stats.agents`
   * because a paged response's member-index seed can race an SSE
   * `deleted` the same way the page's own agent rows can (which the
   * epoch's seed leaves out) — without this, a deleted agent's count
   * would re-enter the member index via `stats.agents` and nothing would
   * ever remove it again, inflating the paged total/running counts and
   * Stop-all visibility. Returns `stats` itself when there is nothing to
   * drop.
   */
  private freshStats(stats: SortedAgentsResponse['stats']): SortedAgentsResponse['stats'] {
    if (!stats?.agents) return stats;
    const agents = dropTombstonedPairs(stats.agents, stateManager.getDeletedAgentIds());
    if (agents === stats.agents) return stats;
    return { ...stats, agents };
  }

  /** `true` iff `err` is the `AbortError` from an intentionally superseded request. */
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
    if (label) qs.set('label', label);
    if (this.phaseFilter) qs.set('phase', this.phaseFilter);

    const epoch = new AgentSeedEpoch();
    try {
      const response = await apiFetch(
        `/api/v1/projects/${this.projectId}/agents?${qs.toString()}`,
        {
          signal: params.signal,
        }
      );
      if (!response.ok) {
        throw new Error(await extractApiError(response, 'Failed to load agents'));
      }
      const data = (await response.json()) as SortedAgentsResponse;
      return this.seedPage(epoch, data);
    } finally {
      epoch.close();
    }
  }

  /**
   * Seed one sorted page under its epoch and build the window's page
   * result: the page rows as the store's current objects (a row of an
   * agent already deleted live is left out; if the store no longer holds
   * the agent it is counted for the backfill chip, as in
   * `AgentSeedResult.dropped`, and the page is short by at least that many
   * rows), and what landed while the request was in flight (upserted,
   * deleted, and deltas for agents not in the store, plus any resync) for
   * the window to replay.
   */
  private seedPage(epoch: AgentSeedEpoch, data: SortedAgentsResponse): PagedPageResult {
    // A full-view page: each object replaces the stored one, so a field
    // the server no longer sends does not linger. Seeding skips a server
    // page row whose SSE `deleted` this client already processed.
    const seeded = epoch.seed(data.agents || [], { partial: false });
    return {
      agents: seeded.agents,
      nextCursor: data.nextCursor,
      totalCount: data.totalCount,
      stats: this.freshStats(data.stats),
      liveChanged: epoch.changedIds,
      liveUnknown: epoch.unknownChanges,
      liveDeleted: epoch.deletedChanges,
      droppedRows: seeded.dropped.length,
      liveResync: epoch.sawResync,
    };
  }

  private async autoDiscoverGitHubApp(): Promise<void> {
    try {
      // Check if the hub has a GitHub App configured
      const configRes = await apiFetch('/api/v1/github-app');
      if (!configRes.ok) return;
      const configData = (await configRes.json()) as { configured: boolean };
      if (!configData.configured) return;

      // Trigger discovery — the hub will match installations to this project's git remote
      const discoverRes = await apiFetch('/api/v1/github-app/installations/discover', {
        method: 'POST',
      });
      if (!discoverRes.ok) return;

      // Reload project data to pick up the newly associated installation
      const projectRes = await apiFetch(`/api/v1/projects/${this.projectId}`);
      if (projectRes.ok) {
        this.project = (await projectRes.json()) as Project;
        stateManager.seedProjects([this.project]);
      }
    } catch {
      // Non-critical — project just won't show GitHub icon until settings page is visited
    }
  }

  private renderProjectIcon() {
    return html`<sl-icon name="folder-fill"></sl-icon>`;
  }

  private renderLinkedBadge() {
    if (!this.project || this.project.projectType !== 'linked') return nothing;
    return html` <sl-tooltip content="Linked project"
      ><sl-icon
        name="link-45deg"
        style="font-size: 0.875rem; vertical-align: middle; opacity: 0.7;"
      ></sl-icon
    ></sl-tooltip>`;
  }

  /** Formats a timestamp for the header stats; "—" when missing or invalid. */
  private formatDate(dateString: string | undefined): string {
    return (dateString && formatInstantWithZone(dateString)) || '—';
  }

  private getTabDataSource(tabName: string): FileBrowserDataSource {
    if (!this.fileBrowserDataSources[tabName]) {
      if (tabName === 'workspace') {
        this.fileBrowserDataSources[tabName] = new WorkspaceFileBrowserDataSource(this.projectId);
      } else {
        this.fileBrowserDataSources[tabName] = new SharedDirFileBrowserDataSource(
          this.projectId,
          tabName
        );
      }
    }
    return this.fileBrowserDataSources[tabName];
  }

  private getEditorDataSource(tabName: string): FileEditorDataSource {
    if (!this.editorDataSources[tabName]) {
      if (tabName === 'workspace') {
        this.editorDataSources[tabName] = new WorkspaceFileEditorDataSource(this.projectId);
      } else {
        this.editorDataSources[tabName] = new SharedDirFileEditorDataSource(
          this.projectId,
          tabName
        );
      }
    }
    return this.editorDataSources[tabName];
  }

  private handleFileEditRequested(e: CustomEvent<{ path: string }>): void {
    this.editingFilePath = e.detail.path;
    this.editorInitialPreview = false;
  }

  private handleFilePreviewRequested(e: CustomEvent<{ path: string }>): void {
    this.editingFilePath = e.detail.path;
    this.editorInitialPreview = true;
  }

  private handleFileCreateRequested(): void {
    this.editingFilePath = '';
    this.editorInitialPreview = false;
  }

  private handleEditorClosed(): void {
    this.editingFilePath = null;
    this.editorInitialPreview = false;
  }

  private handleFileSaved(): void {
    // Refresh the file browser to reflect the saved/new file
    this.refreshActiveFileBrowser();
  }

  /**
   * Delete through the shared helper (ptone/scion#2483 phase 2): the
   * card/row Delete button (`event` for the Alt-key bypass), and the
   * failure banner's Retry (no confirm) and Force. On 204 the card goes
   * now; on 202 the helper has applied "Deleting…" and the SSE `deleted`
   * removes it in place. A 502/503 now offers the same force fallback as
   * the other pages (the helper owns it).
   */
  private async deleteAgent(agentId: string, opts: AgentDeleteRequest = {}): Promise<void> {
    // `this.agents` is empty while paged: `findShownAgent` falls back to
    // the window's current page, then to state (an off-page agent, e.g.
    // acted on right after a chip click elsewhere).
    const outcome = await runAgentDelete({
      agentId,
      agentName: this.findShownAgent(agentId)?.name,
      ...opts,
      onBusy: (busy): void => {
        this.actionLoading = { ...this.actionLoading, [agentId]: busy };
      },
    });
    if (outcome.kind === 'deleted') {
      // Drops the card from the held set now; on a paged page the
      // background refresh drops it (GoogleCloudPlatform/scion#2451).
      this.applyOptimisticAgents([], [agentId]);
      this.backgroundRefresh();
    } else if (outcome.kind === 'failed') {
      showToast(outcome.message);
    }
  }

  /**
   * The badge shows only a live delete; a failed one (abandoned included)
   * shows the failure banner instead.
   */
  private deletingView(agent: Agent): DeletionInfo | null {
    const view = this.deletionLease.view(agent);
    return view?.state === 'deleting' ? view : null;
  }

  /**
   * Failure banner with Retry and Force. Cards get the full form; table
   * rows the compact one, which fits the status cell.
   */
  private renderDeletionBanner(agent: Agent, compact: boolean): TemplateResult | typeof nothing {
    const view = this.deletionLease.view(agent);
    if (view?.state !== 'failed') return nothing;
    return html`<scion-deletion-banner
      class="deletion-banner"
      .deletion=${view}
      agent-name=${agent.name}
      ?can-delete=${can(agent._capabilities, 'delete')}
      ?busy=${this.actionLoading[agent.id] || false}
      ?compact=${compact}
      @deletion-retry=${(): void => void this.deleteAgent(agent.id, { confirm: false })}
      @deletion-force=${(): void => void this.deleteAgent(agent.id, { force: true })}
    ></scion-deletion-banner>`;
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
      await this.deleteAgent(agentId, { event });
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
        throw new Error(await lifecycleActionErrorMessage(response, `Failed to ${action} agent`));
      }

      this.backgroundRefresh();
    } catch (err) {
      console.error(`Failed to ${action} agent:`, err);
      showToast(err instanceof Error ? err.message : `Failed to ${action} agent`);
      this.backgroundRefresh();
    }
  }

  private onViewChange(e: CustomEvent<{ view: ViewMode }>): void {
    this.viewMode = e.detail.view;
    this.agentWindow.setViewState({ view: listViewOf(this.viewMode) });
    this.onAgentViewStateChanged();
  }

  private toggleAgentsExpanded(): void {
    this.agentsExpanded = !this.agentsExpanded;
    localStorage.setItem(AGENTS_EXPANDED_STORAGE_KEY, String(this.agentsExpanded));
  }

  private setPhaseFilter(phase: AgentPhase | ''): void {
    if (this.phaseFilter === phase) return;
    this.phaseFilter = phase;
    if (phase) {
      localStorage.setItem(`scion-filter-project-agents-phase-${this.projectId}`, phase);
    } else {
      localStorage.removeItem(`scion-filter-project-agents-phase-${this.projectId}`);
    }
    this.agentWindow.setViewState({ phaseFilter: phase });
    this.onAgentViewStateChanged();
  }

  private toggleSort(field: AgentSortField): void {
    if (this.sortField === field) {
      this.sortDir = this.sortDir === 'asc' ? 'desc' : 'asc';
    } else {
      this.sortField = field;
      this.sortDir = field === 'name' ? 'asc' : 'desc';
    }
    localStorage.setItem(
      `scion-sort-project-agents-${this.projectId}`,
      JSON.stringify({ field: this.sortField, dir: this.sortDir })
    );
    this.agentWindow.setViewState({ sortField: this.sortField, sortDir: this.sortDir });
    this.onAgentViewStateChanged();
  }

  private sortIndicator(field: AgentSortField): string {
    return this.sortField === field ? (this.sortDir === 'asc' ? '▲' : '▼') : '▲';
  }

  private formatRelativeTime(isoString: string): string {
    const ms = new Date(isoString).getTime();
    if (Number.isNaN(ms)) return '—';
    // A future instant is clock skew between hub and browser.
    if (ms > Date.now()) return 'just now';
    return formatRelative(isoString, { style: 'narrow' });
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
            class=${this.phaseFilter === 'stopping' ? 'active' : ''}
            @click=${(): void => this.setPhaseFilter('stopping')}
          >
            Stopping
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
            // Live preview only — no request per keystroke. `setViewState` never fetches, so
            // this is always free regardless of window state.
            this.agentWindow.setViewState({ label: this.labelFilter });
          }}
          @sl-change=${() => {
            this.labelBeforeCommit = this.committedLabel;
            this.committedLabel = this.labelFilter;
            this.backgroundRefresh('label-commit');
          }}
          @sl-clear=${() => {
            this.labelBeforeCommit = this.committedLabel;
            this.labelFilter = '';
            this.committedLabel = '';
            this.agentWindow.setViewState({ label: '' });
            this.backgroundRefresh('label-commit');
          }}
          style="max-width: 220px;"
        >
          <sl-icon slot="prefix" name="tag"></sl-icon>
        </sl-input>
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

  private hasRunningAgents(): boolean {
    return this.agentStats.running > 0;
  }

  private async handleStopAll(): Promise<void> {
    const isProjectAdmin = can(this.project?._capabilities, 'manage');
    const confirmMsg = isProjectAdmin
      ? 'Are you sure you want to stop all running agents in this project?'
      : 'Are you sure you want to stop all of your running agents in this project?';
    if (!(await showConfirm(confirmMsg))) {
      return;
    }

    // Optimistic: mark running agents as "stopping" (H, or the current page while paged)
    const shownAgents = this.agentWindow.isLocal ? this.agents : this.agentWindow.items;
    this.applyOptimisticAgents(
      shownAgents
        .filter((a) => isAgentRunning(a))
        .map((a) => ({ ...a, phase: 'stopping' as const }))
    );
    this.stopAllLoading = true;

    try {
      const response = await apiFetch(`/api/v1/projects/${this.projectId}/agents/stop-all`, {
        method: 'POST',
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, 'Failed to stop agents'));
      }

      const result = (await response.json()) as StopAllResult;
      for (const notice of stopAllNotices(result)) {
        showToast(notice.message, notice.variant);
      }

      this.backgroundRefresh();
    } catch (err) {
      console.error('Failed to stop agents:', err);
      showToast(err instanceof Error ? err.message : 'Failed to stop agents');
      this.backgroundRefresh();
    } finally {
      this.stopAllLoading = false;
    }
  }

  private async handlePullLatest(): Promise<void> {
    this.pullLoading = true;
    this.pullResult = null;

    try {
      const response = await apiFetch(`/api/v1/projects/${this.projectId}/workspace/pull`, {
        method: 'POST',
      });

      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const result = (await response.json()) as any;

      if (!response.ok) {
        // Extract error message from structured APIError or legacy format
        const apiErr = result?.error;
        let errorMsg =
          (typeof apiErr === 'object' ? apiErr?.message : null) ||
          result?.detail ||
          result?.error ||
          'Pull failed';
        // Append guidance hint if available
        const guidance = apiErr?.details?.guidance;
        if (guidance) {
          errorMsg += ` — ${guidance}`;
        }
        this.pullResult = { status: 'error', error: errorMsg };
        return;
      }

      this.pullResult = { status: 'ok', updated: result.updated, commits: result.commits };
      // Refresh file list after pull
      this.refreshActiveFileBrowser();
    } catch (err) {
      this.pullResult = {
        status: 'error',
        error: err instanceof Error ? err.message : 'Pull failed',
      };
    } finally {
      this.pullLoading = false;
    }
  }

  private slugify(text: string): string {
    return text
      .toLowerCase()
      .trim()
      .replace(/[^a-z0-9]+/g, '-')
      .replace(/^-+|-+$/g, '');
  }

  private openCloneDialog(): void {
    this.cloneName = this.project ? `${this.project.name} copy` : '';
    this.cloneError = '';
    this.cloneLoading = false;
    this.cloneDialogOpen = true;
  }

  private async handleCloneProject(): Promise<void> {
    if (!this.cloneName.trim()) {
      this.cloneError = 'Project name is required.';
      return;
    }

    this.cloneLoading = true;
    this.cloneError = '';

    try {
      const response = await apiFetch(`/api/v1/projects/${this.projectId}/clone`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: this.cloneName.trim() }),
      });

      if (!response.ok) {
        const errorText = await extractApiError(response, 'Failed to clone project');
        throw new Error(errorText);
      }

      const cloned = (await response.json()) as { id: string; name: string; slug: string };
      this.cloneDialogOpen = false;

      // Navigate to the newly cloned project
      navigateTo(`/projects/${cloned.id}`);
    } catch (err) {
      this.cloneError = err instanceof Error ? err.message : 'Failed to clone project';
    } finally {
      this.cloneLoading = false;
    }
  }

  private openTemplateDialog(): void {
    this.templateName = this.project ? `${this.project.name} Template` : '';
    this.templateError = '';
    this.templateLoading = false;
    this.templateDialogOpen = true;
  }

  private async handleCreateTemplate(): Promise<void> {
    if (!this.templateName.trim()) {
      this.templateError = 'Template name is required.';
      return;
    }
    this.templateLoading = true;
    this.templateError = '';
    try {
      const response = await apiFetch(`/api/v1/projects/${this.projectId}/clone`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: this.templateName.trim(), asTemplate: true }),
      });
      if (!response.ok) {
        const errorText = await extractApiError(response, 'Failed to create template');
        throw new Error(errorText);
      }
      this.templateDialogOpen = false;
      // Navigate to hub resources templates tab
      navigateTo('/settings?tab=project-templates');
    } catch (err) {
      this.templateError = err instanceof Error ? err.message : 'Failed to create template';
    } finally {
      this.templateLoading = false;
    }
  }

  private renderTemplateDialog() {
    return html`
      <sl-dialog
        label="Create Template"
        ?open=${this.templateDialogOpen}
        @sl-request-close=${(e: CustomEvent) => {
          if (this.templateLoading) {
            e.preventDefault();
            return;
          }
          this.templateDialogOpen = false;
        }}
      >
        <p>
          Create a project template from
          <strong>${this.project?.name ?? 'this project'}</strong>.
        </p>
        <sl-input
          label="Template Name"
          .value=${this.templateName}
          @sl-input=${(e: Event) => (this.templateName = (e.target as HTMLInputElement).value)}
          ?disabled=${this.templateLoading}
        ></sl-input>
        ${this.templateError ? html`<div class="clone-error">${this.templateError}</div>` : nothing}
        <sl-button
          slot="footer"
          variant="primary"
          @click=${() => this.handleCreateTemplate()}
          ?loading=${this.templateLoading}
          ?disabled=${this.templateLoading}
        >
          Create Template
        </sl-button>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => (this.templateDialogOpen = false)}
          ?disabled=${this.templateLoading}
        >
          Cancel
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderCloneDialog() {
    return html`
      <sl-dialog
        label="Clone Project"
        ?open=${this.cloneDialogOpen}
        @sl-request-close=${(e: CustomEvent) => {
          if (this.cloneLoading) {
            e.preventDefault();
            return;
          }
          this.cloneDialogOpen = false;
        }}
      >
        <sl-input
          label="Name"
          .value=${this.cloneName}
          @sl-input=${(e: Event) => (this.cloneName = (e.target as HTMLInputElement).value)}
          ?disabled=${this.cloneLoading}
        ></sl-input>
        <div class="clone-slug-preview">Slug: ${this.slugify(this.cloneName) || '...'}</div>
        <div class="clone-summary">
          <strong>Copies:</strong> settings, labels, environment variables, injected skills,
          pre-start hook, project harness configs and templates.<br />
          <strong>Does not copy:</strong> secrets, agents, history, or chat integrations.
        </div>
        ${this.cloneError ? html`<div class="clone-error">${this.cloneError}</div>` : nothing}
        <sl-button
          slot="footer"
          variant="primary"
          @click=${() => this.handleCloneProject()}
          ?loading=${this.cloneLoading}
          ?disabled=${this.cloneLoading}
        >
          Clone
        </sl-button>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => (this.cloneDialogOpen = false)}
          ?disabled=${this.cloneLoading}
        >
          Cancel
        </sl-button>
      </sl-dialog>
    `;
  }

  override render() {
    if (this.loading) {
      return this.renderLoading();
    }

    if (this.error) {
      return this.renderError();
    }

    if (!this.project) {
      return this.renderError();
    }

    return html`
      <a href="/projects" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Projects
      </a>

      <div class="header">
        <div class="header-info">
          <div class="header-title">
            ${this.renderProjectIcon()}
            <div class="header-title-text">
              <h1>${this.project.name}${this.renderLinkedBadge()}</h1>
            </div>
          </div>
          <div class="header-path">
            <scion-git-remote-display .project=${this.project}></scion-git-remote-display>
          </div>
        </div>
        <div class="header-actions">
          ${can(this.agentScopeCapabilities, 'create')
            ? html`
                <a href="/agents/new?projectId=${this.projectId}" style="text-decoration: none;">
                  <sl-button variant="primary" size="small">
                    <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                    New Agent
                  </sl-button>
                </a>
              `
            : nothing}
          ${this.project &&
          isSharedWorkspace(this.project) &&
          can(this.project?._capabilities, 'update')
            ? html`
                <sl-button
                  size="small"
                  ?loading=${this.pullLoading}
                  ?disabled=${this.pullLoading}
                  @click=${() => this.handlePullLatest()}
                >
                  <sl-icon slot="prefix" name="arrow-down-circle"></sl-icon>
                  Pull Latest
                </sl-button>
              `
            : nothing}
          ${can(this.project?._capabilities, 'read') && can(this.hubProjectCapabilities, 'create')
            ? this.pageData?.user?.role === 'admin'
              ? html`
                  <sl-dropdown>
                    <sl-button slot="trigger" size="small" caret>
                      <sl-icon slot="prefix" name="copy"></sl-icon>
                      Clone
                    </sl-button>
                    <sl-menu>
                      <sl-menu-item @click=${() => this.openCloneDialog()}>
                        <sl-icon slot="prefix" name="copy"></sl-icon>
                        Clone Project
                      </sl-menu-item>
                      <sl-menu-item @click=${() => this.openTemplateDialog()}>
                        <sl-icon slot="prefix" name="file-earmark-plus"></sl-icon>
                        Create Template
                      </sl-menu-item>
                    </sl-menu>
                  </sl-dropdown>
                `
              : html`
                  <sl-button size="small" @click=${() => this.openCloneDialog()}>
                    <sl-icon slot="prefix" name="copy"></sl-icon>
                    Clone
                  </sl-button>
                `
            : nothing}
          <a href="/projects/${this.projectId}/metrics" style="text-decoration: none;">
            <sl-button size="small">
              <sl-icon slot="prefix" name="graph-up"></sl-icon>
              Metrics
            </sl-button>
          </a>
          ${canAny(this.project?._capabilities, 'update', 'delete', 'manage')
            ? html`
                <a href="/projects/${this.projectId}/settings" style="text-decoration: none;">
                  <sl-button size="small">
                    <sl-icon slot="prefix" name="gear"></sl-icon>
                    Settings
                  </sl-button>
                </a>
              `
            : nothing}
        </div>
      </div>

      <div class="stats-row">
        <div class="stat">
          <span class="stat-label">Agents</span>
          <span class="stat-value">${this.renderAgentStat('total')}</span>
        </div>
        <div class="stat">
          <span class="stat-label">Running</span>
          <span class="stat-value">${this.renderAgentStat('running')}</span>
        </div>
        <div class="stat">
          <span class="stat-label">Created</span>
          <span class="stat-value" style="font-size: 1rem; font-weight: 500;">
            ${this.formatDate(this.project.created || this.project.createdAt)}
          </span>
        </div>
        <div class="stat">
          <span class="stat-label">Updated</span>
          <span class="stat-value" style="font-size: 1rem; font-weight: 500;">
            ${this.formatDate(this.project.updated || this.project.updatedAt)}
          </span>
        </div>
      </div>

      ${this.metricsSummary
        ? html`
            <div class="stats-row" style="margin-top: 0.5rem;">
              <div class="stat">
                <span class="stat-label">Sessions (24h)</span>
                <span class="stat-value">${this.metricsSummary.sessionsCount24h}</span>
              </div>
              <div class="stat">
                <span class="stat-label">API Calls (24h)</span>
                <span class="stat-value">${this.metricsSummary.apiCalls24h}</span>
              </div>
              <div class="stat">
                <span class="stat-label">Tokens (24h)</span>
                <span class="stat-value"
                  >${this.formatTokenCount(this.metricsSummary.tokenUsage24h)}</span
                >
              </div>
              <div class="stat">
                <span class="stat-label">Active Agents (24h)</span>
                <span class="stat-value">${this.metricsSummary.activeAgents24h}</span>
              </div>
              <div style="display: flex; align-items: center; margin-left: auto;">
                <a href="/projects/${this.projectId}/metrics" style="text-decoration: none;">
                  <sl-button size="small" variant="text">
                    <sl-icon slot="prefix" name="graph-up"></sl-icon>
                    View Details
                  </sl-button>
                </a>
              </div>
            </div>
          `
        : nothing}
      ${this.sessionMetricsSummary
        ? html`
            <div class="stats-row" style="margin-top: 0.5rem;">
              <div class="stat">
                <span class="stat-label">Total Sessions</span>
                <span class="stat-value">${this.sessionMetricsSummary.totalSessions}</span>
              </div>
              <div class="stat">
                <span class="stat-label">Total Tokens</span>
                <span class="stat-value"
                  >${this.formatTokenCount(
                    this.sessionMetricsSummary.totalTokensInput +
                      this.sessionMetricsSummary.totalTokensOutput
                  )}</span
                >
              </div>
              <div class="stat">
                <span class="stat-label">Active Agents</span>
                <span class="stat-value">${this.sessionMetricsSummary.activeAgents}</span>
              </div>
            </div>
          `
        : nothing}
      ${this.pullResult
        ? html`
            <sl-alert
              variant=${this.pullResult.status === 'ok' ? 'success' : 'danger'}
              open
              closable
              @sl-after-hide=${() => {
                this.pullResult = null;
              }}
            >
              <sl-icon
                slot="icon"
                name=${this.pullResult.status === 'ok' ? 'check-circle' : 'exclamation-triangle'}
              ></sl-icon>
              ${this.pullResult.status === 'ok'
                ? this.pullResult.updated &&
                  this.pullResult.commits &&
                  this.pullResult.commits.length > 0
                  ? html`
                      Pulled ${this.pullResult.commits.length}
                      commit${this.pullResult.commits.length === 1 ? '' : 's'}
                      <div class="pull-commits">
                        ${this.pullResult.commits.map(
                          (c) =>
                            html`<div><span class="commit-hash">${c.hash}</span>${c.subject}</div>`
                        )}
                      </div>
                    `
                  : 'Already up to date.'
                : this.pullResult.error || 'Pull failed.'}
            </sl-alert>
          `
        : nothing}

      <div class="section-header">
        <h2>Agents</h2>
        <div style="display: flex; align-items: center; gap: 0.75rem;">
          <scion-view-toggle
            .view=${this.viewMode}
            storageKey="scion-view-project-agents"
            @view-change=${this.onViewChange}
          ></scion-view-toggle>
          ${can(this.agentScopeCapabilities, 'stop_all') && this.hasRunningAgents()
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
          ${this.agentStats.total > 0 && this.viewMode !== 'graph'
            ? html`
                <sl-tooltip content=${this.agentsExpanded ? 'Collapse' : 'Expand'}>
                  <sl-icon-button
                    class="agents-expand-toggle"
                    name=${this.agentsExpanded ? 'arrows-angle-contract' : 'arrows-angle-expand'}
                    label=${this.agentsExpanded ? 'Collapse agents' : 'Expand agents'}
                    aria-expanded=${this.agentsExpanded ? 'true' : 'false'}
                    @click=${() => this.toggleAgentsExpanded()}
                  ></sl-icon-button>
                </sl-tooltip>
              `
            : nothing}
        </div>
      </div>

      ${this.agentsLoadError
        ? this.renderAgentsLoadError()
        : this.agentsInitialLoadPending
          ? html`<div class="empty-filter-state agents-initial-loading">Loading agents…</div>`
          : this.agentStats.total === 0
            ? html`${this.renderAgentWindowBanner()}${this.renderEmptyAgents()}`
            : html`
                ${this.renderFilterBar()} ${this.renderAgentWindowBanner()}
                ${this.renderAgentRows()}
              `}
      ${this.project?.cloudLogging ? this.renderMessagesSection() : nothing}
      ${this.shouldShowFilesSection()
        ? this.filesSectionVisible
          ? this.renderFilesSection()
          : this.renderFilesSectionPlaceholder()
        : ''}
      ${this.renderCloneDialog()} ${this.renderTemplateDialog()}
    `;
  }

  private handleMessagesToggle(): void {
    if (this.messagesExpanded) {
      // Collapse: stop streaming and reset loaded state so next expand reloads
      const viewer = this.shadowRoot?.querySelector('scion-agent-message-viewer') as
        | import('../shared/agent-message-viewer.js').ScionAgentMessageViewer
        | null;
      viewer?.stopStream();
      viewer?.resetLoaded();
      this.messagesExpanded = false;
    } else {
      this.messagesExpanded = true;
      this.updateComplete.then(() => {
        const viewer = this.shadowRoot?.querySelector('scion-agent-message-viewer') as
          | import('../shared/agent-message-viewer.js').ScionAgentMessageViewer
          | null;
        viewer?.loadMessages();
      });
    }
  }

  private renderMessagesSection() {
    return html`
      <div class="workspace-section">
        <div class="section-header" style="cursor: pointer;" @click=${this.handleMessagesToggle}>
          <h2>
            <sl-icon
              name=${this.messagesExpanded ? 'chevron-down' : 'chevron-right'}
              style="font-size: 0.875rem; vertical-align: middle; margin-right: 0.25rem;"
            ></sl-icon>
            Messages
          </h2>
        </div>
        ${this.messagesExpanded
          ? html`
              <scion-agent-message-viewer
                logsUrl=${`/api/v1/projects/${this.projectId}/message-logs`}
                streamUrl=${`/api/v1/projects/${this.projectId}/message-logs/stream`}
                broadcastUrl=${`/api/v1/projects/${this.projectId}/broadcast`}
                ?canSend=${true}
              ></scion-agent-message-viewer>
            `
          : nothing}
      </div>
    `;
  }

  /**
   * Whether the project has a project-level workspace directory to browse:
   * hub-native shared workspaces and shared-workspace git projects. Per-agent
   * git projects and empty-per-agent projects (#2703 D5) have none.
   */
  private hasProjectWorkspace(): boolean {
    if (!this.project) return false;
    if (isEmptyPerAgentWorkspace(this.project)) return false;
    return !this.project.gitRemote || isSharedWorkspace(this.project);
  }

  private shouldShowFilesSection(): boolean {
    if (!this.project) return false;
    if (this.hasProjectWorkspace()) return true;
    // Otherwise show files only when shared dirs exist
    return (this.project.sharedDirs?.length ?? 0) > 0;
  }

  private getFileTabs(): Array<{ key: string; label: string }> {
    const tabs: Array<{ key: string; label: string }> = [];
    // Only projects with a project-level workspace get a workspace tab
    if (this.hasProjectWorkspace()) {
      tabs.push({ key: 'workspace', label: 'workspace' });
    }
    // Add one tab per shared dir
    for (const dir of this.project?.sharedDirs ?? []) {
      tabs.push({ key: dir.name, label: dir.name });
    }
    return tabs;
  }

  private truncateTabLabel(label: string): string {
    if (label.length <= 20) return label;
    return '\u2026' + label.slice(label.length - 18);
  }

  private onFileTabChange(e: CustomEvent<{ name: string }>): void {
    const panel = e.detail.name;
    if (!panel) return;
    this.activeFileTab = panel;
    // Mount that tab's file browser now — this is what triggers its one
    // initial listing request. Already-visited tabs stay mounted, so
    // switching back to one does not refetch or lose local state.
    this.markFileTabVisited(panel);
  }

  private renderFilesSectionPlaceholder() {
    return html`
      <div class="workspace-section files-section-placeholder">
        <div class="workspace-header">
          <div class="workspace-header-left">
            <h2>Files</h2>
          </div>
        </div>
        <div class="loading-state">
          <sl-spinner></sl-spinner>
        </div>
      </div>
    `;
  }

  private refreshActiveFileBrowser(): void {
    const browser = this.shadowRoot?.querySelector(
      `scion-file-browser[data-tab="${this.activeFileTab}"]`
    ) as import('../shared/file-browser.js').ScionFileBrowser | null;
    browser?.loadFiles();
  }

  private renderFilesSection() {
    const tabs = this.getFileTabs();
    const isEditable = can(this.project?._capabilities, 'update');
    const isEditorOpen = this.editingFilePath !== null;

    return html`
      <div class="workspace-section">
        <div class="workspace-header">
          <div class="workspace-header-left">
            <h2>Files</h2>
          </div>
        </div>

        ${isEditorOpen
          ? html`
              <div class="editor-back-row">
                <sl-button size="small" variant="text" @click=${this.handleEditorClosed}>
                  <sl-icon slot="prefix" name="arrow-left"></sl-icon>
                  Back to files
                </sl-button>
              </div>
              <scion-file-editor
                .filePath=${this.editingFilePath || ''}
                .dataSource=${this.getEditorDataSource(this.activeFileTab)}
                ?readonly=${!isEditable}
                ?initialPreview=${this.editorInitialPreview}
                @file-saved=${this.handleFileSaved}
                @editor-closed=${this.handleEditorClosed}
              ></scion-file-editor>
            `
          : html`
              <div class="files-tab-header">
                <sl-tab-group class="files-tab-group" @sl-tab-show=${this.onFileTabChange}>
                  ${tabs.map(
                    (tab) => html`
                      <sl-tab slot="nav" panel=${tab.key} ?active=${tab.key === this.activeFileTab}>
                        <span class="tab-label-truncated" title=${tab.label}
                          >${this.truncateTabLabel(tab.label)}</span
                        >
                      </sl-tab>
                    `
                  )}
                  ${tabs.map(
                    (tab) => html`
                      <sl-tab-panel name=${tab.key}>
                        ${this.visitedFileTabs.has(tab.key)
                          ? html`
                              <scion-file-browser
                                data-tab=${tab.key}
                                .dataSource=${this.getTabDataSource(tab.key)}
                                ?editable=${isEditable}
                                ?showArchive=${true}
                                @file-edit-requested=${this.handleFileEditRequested}
                                @file-preview-requested=${this.handleFilePreviewRequested}
                                @file-create-requested=${this.handleFileCreateRequested}
                              ></scion-file-browser>
                            `
                          : nothing}
                      </sl-tab-panel>
                    `
                  )}
                </sl-tab-group>
              </div>
            `}
      </div>
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading project...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <a href="/projects" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Projects
      </a>

      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Project</h2>
        <p>There was a problem loading this project.</p>
        <div class="error-details">${this.error || 'Project not found'}</div>
        <sl-button variant="primary" @click=${() => this.loadData()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  /**
   * The agents section when the page-load agents request failed: the
   * project stays on screen, with a Retry for the agents only.
   */
  private renderAgentsLoadError(): TemplateResult {
    return html`<div class="empty-filter-state agents-load-error">
      ${this.agentsLoadError}
      <sl-button
        size="small"
        ?disabled=${this.agentsLoading || this.agentWindow.loading}
        @click=${(): void => this.retryAgentsLoad()}
      >
        <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
        Retry
      </sl-button>
    </div>`;
  }

  private renderEmptyAgents() {
    return html`
      <div class="empty-state">
        <sl-icon name="cpu"></sl-icon>
        <h2>No Agents</h2>
        <p>
          This project doesn't have any agents
          yet.${can(this.agentScopeCapabilities, 'create')
            ? ' Create your first agent to get started.'
            : ''}
        </p>
        ${can(this.agentScopeCapabilities, 'create')
          ? html`
              <a href="/agents/new?projectId=${this.projectId}" style="text-decoration: none;">
                <sl-button variant="primary">
                  <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                  New Agent
                </sl-button>
              </a>
            `
          : nothing}
      </div>
    `;
  }

  /**
   * The agent rows for the current view, all rendered from the window:
   * the tree from the unsliced `display`, grid and list from the page
   * `items` with the pager. While paged in a complete-needing view state
   * (tree, name or status sort, bare-key label) the drain that view
   * started is in flight, or failed; the server page is not shown in
   * the wrong order meanwhile.
   */
  private renderAgentRows(): TemplateResult | typeof nothing {
    const win = this.agentWindow;
    if (win.state === 'paged' && !win.isSortedEligible(this.committedLabel)) {
      return this.agentsLoading
        ? html`<div class="empty-filter-state">Loading agents…</div>`
        : html`<div class="empty-filter-state">
            Could not load every agent for this view.
            <sl-button size="small" @click=${(): void => this.onAgentViewStateChanged()}
              >Retry</sl-button
            >
          </div>`;
    }
    if (this.viewMode === 'graph') {
      const display = win.display;
      if (display.length === 0) return this.renderNoAgentRows();
      return html`<scion-agent-tree-view
        .agents=${display}
        filterKey=${`${this.phaseFilter}|${this.labelFilter}`}
      ></scion-agent-tree-view>`;
    }
    if (this.viewMode === 'grid') {
      const items = win.items;
      return html`
        ${items.length === 0
          ? this.renderNoAgentRows()
          : html`<div class="agent-grid ${this.agentsExpanded ? '' : 'agents-collapsed'}">
              ${items.map((agent) => this.renderAgentCard(agent))}
            </div>`}
        ${this.renderAgentPager(items.length)}
      `;
    }
    return this.renderAgentWindowList();
  }

  /**
   * The empty-rows message: "Loading agents…" only while a load is in
   * flight with nothing held yet, otherwise the filter-empty message (a
   * phase filter that matches nothing must not flicker to "Loading" on
   * every lifecycle refresh).
   */
  private renderNoAgentRows(): TemplateResult {
    return this.agents.length === 0 && this.agentWindow.isLocal && this.agentsLoading
      ? html`<div class="empty-filter-state">Loading agents…</div>`
      : html`<div class="empty-filter-state">No agents match the current filter.</div>`;
  }

  /**
   * The window's banner: capped, failed or stale, with a Refresh that is
   * the chip trigger. Shown in every view rendered from a capped set (grid,
   * list and tree), so an incomplete set is never presented as complete;
   * the grid and list pagers also carry the capped total.
   */
  private renderAgentWindowBanner(): TemplateResult | typeof nothing {
    const banner = this.agentWindow.banner;
    if (!banner) return nothing;
    return html`<div class="agent-window-banner">
      <span>${banner.text}</span>
      <sl-tag variant="primary" pill @click=${(): void => this.onAgentWindowRefresh()}>
        <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
        Refresh
      </sl-tag>
    </div>`;
  }

  /** The banner's Refresh: the chip trigger, ignored while a load is in flight. */
  private onAgentWindowRefresh(): void {
    if (this.agentsLoading || this.agentWindow.loading) return;
    this.backgroundRefresh('chip');
  }

  private renderAgentTableHead() {
    return html`
      <thead>
        <tr>
          <th
            class="sortable ${this.sortField === 'name' ? 'sorted' : ''}"
            @click=${() => this.toggleSort('name')}
          >
            Name <span class="sort-indicator">${this.sortIndicator('name')}</span>
          </th>
          <th class="hide-mobile">Template</th>
          <th class="hide-mobile">Broker</th>
          <th
            class="status-col sortable ${this.sortField === 'status' ? 'sorted' : ''}"
            @click=${() => this.toggleSort('status')}
          >
            Status <span class="sort-indicator">${this.sortIndicator('status')}</span>
          </th>
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
    `;
  }

  /** The list view: the window's page `items` and the pager, in every state. */
  private renderAgentWindowList() {
    const items = this.agentWindow.items;
    return html`
      <div class="agent-table-container ${this.agentsExpanded ? '' : 'agents-collapsed'}">
        ${items.length === 0
          ? this.renderNoAgentRows()
          : html`
              <table>
                ${this.renderAgentTableHead()}
                <tbody>
                  ${items.map((agent) => this.renderAgentRow(agent))}
                </tbody>
              </table>
            `}
        ${this.renderAgentPager(items.length)}
      </div>
    `;
  }

  /** The pager under the grid and list views. Its chip is the paged-state chip trigger. */
  private renderAgentPager(rowsOnPage: number): TemplateResult {
    return html`<scion-agent-pager
      .storageKey=${PAGER_PAGE_SIZE_STORAGE_KEY}
      .pageIndex=${this.agentWindow.pageIndex}
      .rangeStart=${this.agentWindow.rangeStart}
      .rowsOnPage=${rowsOnPage}
      .total=${this.agentWindow.total}
      .pageSize=${this.pagerPageSize}
      .hasNext=${this.agentWindow.hasNext}
      .hasPrev=${this.agentWindow.hasPrev}
      .loading=${this.agentWindow.loading ||
      (this.agentWindow.state === 'paged' && this.agentsLoading)}
      .error=${this.agentWindow.error}
      .showChip=${this.agentWindow.updatesAvailable}
      @prev=${(): void => this.onPagerNav(() => this.agentWindow.prev())}
      @next=${(): void => this.onPagerNav(() => this.agentWindow.next())}
      @chip-click=${(): void => this.onPagerNav(() => this.loadAgentsForView('chip'))}
      @page-size-change=${(e: CustomEvent<{ pageSize: AgentPagerPageSize }>): void =>
        this.onPagerSizeChange(e.detail.pageSize)}
    ></scion-agent-pager>`;
  }

  /**
   * A second, defense-in-depth guard for Prev/Next/chip-click, on top of
   * the `.loading=` binding that disables the pager's own button/chip. This
   * specifically protects against anything that fires the pager's
   * `prev`/`next`/`chip-click` events without going through its own
   * guarded `onPrev`/`onNext`/`onChipClick` methods (the pager's own guard
   * covers a real click; this one covers an event dispatched directly on
   * the host). This never bumps `agentsLoadGen` — it simply refuses to
   * navigate while either the window's own fetch or, while paged, a
   * page-level load is in flight, so the mismatched-cursor race
   * can never start in the first place. A page-level load alone
   * never gates small-state navigation: small-state Prev/Next is a purely
   * local slice of `display` and sends no request, so a held refresh or
   * lifecycle load has nothing to race.
   */
  private onPagerNav(action: () => Promise<void>): void {
    if (this.agentWindow.loading || (this.agentWindow.state === 'paged' && this.agentsLoading)) {
      return;
    }
    void action();
  }

  private onPagerSizeChange(size: AgentPagerPageSize): void {
    this.pagerPageSize = size;
    this.agentWindow.setViewState({ pageSize: size });
    this.onAgentViewStateChanged();
  }

  private renderAgentRow(agent: Agent) {
    const isLoading = this.actionLoading[agent.id] || false;
    // While the hub is deleting, hide every lifecycle action and Delete.
    const deleting = this.deletionLease.isDeleting(agent);
    const lifecycleOk = canLifecycle(agent._capabilities) && !deleting;

    return html`
      <tr>
        <td>
          <span class="name-cell">
            <sl-icon name="cpu"></sl-icon>
            <a href="/agents/${agent.id}">${agent.name}</a>
          </span>
        </td>
        <td class="hide-mobile">${agent.template}</td>
        <td class="hide-mobile">
          ${agent.runtimeBrokerId
            ? html`<a href="/brokers/${agent.runtimeBrokerId}" class="broker-link">
                <sl-icon name="hdd-rack"></sl-icon>
                ${agent.runtimeBrokerName || agent.runtimeBrokerId}
              </a>`
            : '\u2014'}
        </td>
        <td>
          ${agentStatusBadge(agent, { size: 'small' })}
          <scion-deletion-badge
            .deletion=${this.deletingView(agent)}
            size="small"
          ></scion-deletion-badge>
          ${this.renderDeletionBanner(agent, true)}
        </td>
        <td class="hide-mobile">
          ${(agent.lastActivityEvent && !agent.lastActivityEvent.startsWith('0001')) ||
          agent.updated ||
          agent.updatedAt
            ? this.formatRelativeTime(
                (agent.lastActivityEvent && !agent.lastActivityEvent.startsWith('0001')
                  ? agent.lastActivityEvent
                  : agent.updated || agent.updatedAt)!
              )
            : '\u2014'}
        </td>
        <td class="hide-mobile">
          <span class="task-cell">${agent.taskSummary || '\u2014'}</span>
        </td>
        <td class="actions-cell">
          <span class="table-actions">
            ${can(agent._capabilities, 'attach')
              ? html`
                  <sl-tooltip content="Terminal">
                    <span style="display: inline-flex">
                      <sl-button
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
          </span>
        </td>
      </tr>
    `;
  }

  private renderAgentCard(agent: Agent) {
    const isLoading = this.actionLoading[agent.id] || false;
    // While the hub is deleting, hide every lifecycle action and Delete.
    const deleting = this.deletionLease.isDeleting(agent);
    const lifecycleOk = canLifecycle(agent._capabilities) && !deleting;

    return html`
      <div class="agent-card">
        <div class="agent-header">
          <div>
            <h3 class="agent-name">
              <sl-icon name="cpu"></sl-icon>
              <a href="/agents/${agent.id}" style="color: inherit; text-decoration: none;">
                ${agent.name}
              </a>
            </h3>
            <div class="agent-meta">
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
          ${agentStatusBadge(agent, { size: 'small' })}
          <scion-deletion-badge
            .deletion=${this.deletingView(agent)}
            size="small"
          ></scion-deletion-badge>
        </div>

        ${this.renderDeletionBanner(agent, false)}
        ${agent.taskSummary ? html`<div class="agent-task">${agent.taskSummary}</div>` : ''}

        <div class="agent-actions">
          ${can(agent._capabilities, 'attach')
            ? html`
                <sl-tooltip content="Terminal">
                  <span style="display: inline-flex">
                    <sl-button
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
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-project-detail': ScionPageProjectDetail;
  }
}
