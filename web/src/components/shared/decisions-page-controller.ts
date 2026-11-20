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
 * Shared reactive controller owning the tab, filter, cursor-paging, SSE live
 * refresh, timeout facet-hydration, search debounce, and `/` shortcut state
 * machine for `<scion-page-project-decisions>` and
 * `<scion-page-profile-decisions>` (Design §4.3 R7, ptone/scion#7996).
 */

import type { LitElement, ReactiveController, ReactiveControllerHost } from 'lit';

import type { DecisionView, PageData } from '../../shared/types.js';
import { apiFetch } from '../../client/api.js';
import {
  actionableReason,
  dismissAvailableAtMs,
  dismissDecisionPrompt,
  fetchAskerAnswerability,
  newerDecisionView,
  patchDecisions,
  type DecisionAnsweredDetail,
} from '../../client/decision-prompts.js';
import { chatConversationPath } from '../../client/chat-routes.js';
import { navigateTo, replaceSearch } from '../../client/navigation.js';
import {
  DecisionsApiError,
  buildDecisionsUrlSearch,
  hasDecisionsSearchCapability,
  isDecisionsSearchTimeoutError,
  normalizeDecisionsSearchQuery,
  parseDecisionsUrlState,
  type MadeDecisionsProvenanceFilter,
  type ProjectDecisionConversationView,
  type ProjectDecisionCounts,
  type ProjectDecisionLogEntryView,
  type ProjectDecisionProjectRef,
  type ProjectDecisionPromptView,
  type ProjectMadeDecisionsResponse,
  type ProjectPendingDecisionsResponse,
} from '../../client/project-decisions.js';
import {
  buildProfileDecisionsUrlSearch,
  cleanProfileProjectValue,
  defaultProfileDecisionsSort,
  isDecisionPromptMessageEvent,
  parseProfileDecisionsUrlState,
  type ProfileDecisionActorView,
  type ProfileDecisionMadeGroupView,
  type ProfileDecisionsSort,
  type ProfileMadeDecisionsFilter,
  type ProfileMadeDecisionsResponse,
  type ProfilePendingDecisionsResponse,
  type ProfileSharedDecisionsFilter,
} from '../../client/profile-decisions.js';
import { stateManager } from '../../client/state.js';
import { FEATURE_FLAGS_CHANGED_EVENT, decisionsEnabled } from '../../utils/feature-flags.js';
import { DISPLAY_TIMEZONE_CHANGED_EVENT } from '../../utils/time.js';
import { showToast } from '../../utils/toast.js';
import { deepActiveElement } from './deep-active-element.js';
import type {
  DecisionsFilterPatch,
  DecisionsSearchInputDetail,
  ScionDecisionsFilterPanel,
} from './decisions-filter-panel.js';
import {
  DEFAULT_DECISIONS_PAGE_SIZE,
  MAX_BULK_DISMISS_CONCURRENCY,
  MAX_BULK_DISMISS_SELECTION,
  MAX_DECISIONS_PAGE_SIZE,
  SEARCH_DEBOUNCE_MS,
  bulkDismissResultToast,
  clampDismissReason,
  dateBoundToIso,
  isEditableTarget,
  isOnlyTooRecentDismissError,
  promptHasWaitingOnOthers,
  promptWaitingOnViewerRows,
  runWithConcurrencyLimit,
  type BulkDismissCardOutcome,
} from './decisions-format.js';

export type DecisionsPageScope = 'project' | 'profile';
export type DecisionsLoadMode = 'reset' | 'append' | 'live';

export type DecisionsPendingFetchResult =
  | ProjectPendingDecisionsResponse
  | ProfilePendingDecisionsResponse;

export type DecisionsMadeFetchResult = ProjectMadeDecisionsResponse | ProfileMadeDecisionsResponse;

export interface DecisionsPendingFetchArgs {
  sort: ProfileDecisionsSort;
  cursor?: string | undefined;
  limit?: number | undefined;
  filter: ProfileSharedDecisionsFilter;
  signal: AbortSignal;
}

export interface DecisionsMadeFetchArgs {
  sort: ProfileDecisionsSort;
  cursor?: string | undefined;
  limit?: number | undefined;
  filter: ProfileMadeDecisionsFilter;
  signal: AbortSignal;
}

export interface DecisionsPageAdapter {
  readonly scope: DecisionsPageScope;
  getPageData(): PageData | null;
  extractRouteParams?(pathname: string): { decodeError?: string | undefined } | undefined;
  validateBoot?(): string | null;
  canOperate(): boolean;
  configureStateScope(): void;
  onBoot?(): Promise<void>;
  matchesProject?(projectId: string | undefined): boolean;
  resolvePromptProjectId(prompt: ProjectDecisionPromptView): string;
  onCountsUpdated?(counts: ProjectDecisionCounts): void;
  onNotFound?(): void;
  fetchPending(args: DecisionsPendingFetchArgs): Promise<DecisionsPendingFetchResult>;
  fetchMade(args: DecisionsMadeFetchArgs): Promise<DecisionsMadeFetchResult>;
}

type DecisionsHost = ReactiveControllerHost & LitElement;

export class DecisionsPageController implements ReactiveController {
  private readonly host: DecisionsHost;
  private readonly adapter: DecisionsPageAdapter;

  enabled = false;
  hubNotFound = false;
  currentUserId = '';
  activeTab: 'pending' | 'made' = 'pending';
  sort: ProfileDecisionsSort = 'project';

  initialLoading = true;
  error: string | null = null;

  counts: ProjectDecisionCounts = { waitingOnYou: 0, waitingOnOthers: 0 };
  filteredCounts: ProjectDecisionCounts | null = null;
  supportsSearch: boolean;
  truncatedProjects = false;

  pendingPrompts: ProjectDecisionPromptView[] = [];
  pendingNextCursor = '';
  pendingLoading = false;
  pendingLoadingMore = false;
  pendingSearchTimeout = false;

  madeLoaded = false;
  madeLoading = false;
  madeLoadingMore = false;
  madeError: string | null = null;
  madeSearchTimeout = false;
  madeEntries: ProjectDecisionLogEntryView[] = [];
  madeGroups: ProfileDecisionMadeGroupView[] = [];
  madeNextCursor = '';

  facetsAgents: ProfileDecisionActorView[] = [];
  facetsAnswerers: ProfileDecisionActorView[] = [];
  facetsConversations: ProjectDecisionConversationView[] = [];
  facetsProjects: ProjectDecisionProjectRef[] = [];

  filterProject = '';
  filterQ = '';
  searchDraft = '';
  filterConversation = '';
  filterAgent = '';
  filterUser = '';
  filterProvenance: MadeDecisionsProvenanceFilter = '';
  filterFrom = '';
  filterTo = '';

  clockNow: () => number = () => Date.now();

  selectedPromptIds = new Set<string>();
  bulkDismissPrompts: ProjectDecisionPromptView[] = [];
  bulkDismissReasonDraft = '';
  bulkDismissSubmitting = false;
  bulkDismissCompleted = 0;
  bulkDismissGoneAgentIds = new Set<string>();

  private readonly dismissAvailableAtOverrides = new Map<string, number>();
  private dismissUnlockTimer: ReturnType<typeof setTimeout> | null = null;
  private bulkDismissProbeGen = 0;

  private pendingSortOverride: ProfileDecisionsSort | null = null;
  private madeSortOverride: ProfileDecisionsSort | null = null;
  private readonly knownProjectsById = new Map<string, ProjectDecisionProjectRef>();

  private pendingGen = 0;
  private madeGen = 0;
  private pendingAbort: AbortController | null = null;
  private madeAbort: AbortController | null = null;
  private userAbort: AbortController | null = null;
  private liveRefreshTimer: ReturnType<typeof setTimeout> | null = null;
  private searchDebounceTimer: ReturnType<typeof setTimeout> | null = null;
  private pendingResetInFlight = false;
  private pendingRefreshDeferred = false;
  private pendingNeedsReset = false;
  private pendingPagedBeyondFirst = false;
  private madePagedBeyondFirst = false;
  private madeRefreshDeferred = false;
  private madeStale = false;
  private madeNeedsReset = false;
  private facetsHydrated = false;
  private routeDecodeError: string | null = null;
  private booted = false;

  constructor(host: DecisionsHost, adapter: DecisionsPageAdapter) {
    this.host = host;
    this.adapter = adapter;
    this.supportsSearch = adapter.scope === 'profile';
    this.sort = adapter.scope === 'profile' ? 'project' : 'recent';
    host.addController(this);
  }

  private notify(): void {
    this.host.requestUpdate();
  }

  setError(message: string): void {
    this.error = message;
    this.notify();
  }

  private canOperate(): boolean {
    if (this.adapter.scope === 'profile' && this.hubNotFound) return false;
    return this.adapter.canOperate();
  }

  private readonly boundOnFlagsChanged = (): void => {
    const next = decisionsEnabled();
    if (next === this.enabled) return;
    this.enabled = next;
    this.notify();
    if (!next) {
      this.teardownActiveLoads();
      this.booted = false;
      return;
    }
    void this.bootIfEnabled();
  };

  private readonly boundOnDecisionUpdated = (e: Event): void => {
    if (!this.enabled || !this.canOperate()) return;
    const raw = (e as CustomEvent).detail as {
      data?: {
        projectId?: string;
        messageId?: string;
        decision?: DecisionView;
      };
      projectId?: string;
      messageId?: string;
      decision?: DecisionView;
    } | null;
    const data = raw?.data ?? raw;
    if (this.adapter.matchesProject && !this.adapter.matchesProject(data?.projectId)) return;
    if (data?.decision) {
      this.applyIncomingDecisionPatch(data.messageId, [data.decision]);
    }
    this.scheduleLiveRefresh();
  };

  private readonly boundOnChatMessageReceived = (e: Event): void => {
    if (!this.enabled || !this.canOperate()) return;
    if (!isDecisionPromptMessageEvent(e)) return;
    const raw = (e as CustomEvent).detail as {
      data?: { projectId?: string };
      projectId?: string;
    } | null;
    const data = raw?.data ?? raw;
    if (this.adapter.matchesProject && !this.adapter.matchesProject(data?.projectId)) return;
    this.scheduleLiveRefresh();
  };

  private readonly boundOnTimezoneChanged = (): void => {
    if (!this.enabled || !this.madeLoaded || (!this.filterFrom && !this.filterTo)) return;
    if (this.hasDateRangeError) return;
    if (this.activeTab === 'made') {
      void this.loadMadePage({ mode: 'reset' });
    } else {
      this.madeStale = true;
    }
  };

  private readonly boundOnKeydown = (e: KeyboardEvent): void => {
    if (e.key !== '/' || e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey) return;
    if (
      !this.enabled ||
      !this.canOperate() ||
      !this.supportsSearch ||
      this.initialLoading ||
      this.error !== null
    ) {
      return;
    }
    if (isEditableTarget(deepActiveElement())) return;
    for (const target of e.composedPath()) {
      if (isEditableTarget(target)) return;
    }
    const panel = this.host.shadowRoot?.querySelector<ScionDecisionsFilterPanel>(
      'scion-decisions-filter-panel'
    );
    if (!panel) return;
    e.preventDefault();
    void panel.expandAndFocusSearch();
  };

  hostConnected(): void {
    this.extractRouteState();
    this.enabled = decisionsEnabled();
    this.notify();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFlagsChanged);
    window.addEventListener(DISPLAY_TIMEZONE_CHANGED_EVENT, this.boundOnTimezoneChanged);
    window.addEventListener('keydown', this.boundOnKeydown);
    stateManager.addEventListener('decision-updated', this.boundOnDecisionUpdated);
    stateManager.addEventListener('chat-message-received', this.boundOnChatMessageReceived);
    void this.bootIfEnabled();
  }

  hostDisconnected(): void {
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFlagsChanged);
    window.removeEventListener(DISPLAY_TIMEZONE_CHANGED_EVENT, this.boundOnTimezoneChanged);
    window.removeEventListener('keydown', this.boundOnKeydown);
    stateManager.removeEventListener('decision-updated', this.boundOnDecisionUpdated);
    stateManager.removeEventListener('chat-message-received', this.boundOnChatMessageReceived);
    this.teardownActiveLoads();
  }

  private teardownActiveLoads(): void {
    if (this.liveRefreshTimer !== null) {
      clearTimeout(this.liveRefreshTimer);
      this.liveRefreshTimer = null;
    }
    if (this.searchDebounceTimer !== null) {
      clearTimeout(this.searchDebounceTimer);
      this.searchDebounceTimer = null;
    }
    if (this.dismissUnlockTimer !== null) {
      clearTimeout(this.dismissUnlockTimer);
      this.dismissUnlockTimer = null;
    }
    ++this.bulkDismissProbeGen;
    this.selectedPromptIds = new Set();
    this.bulkDismissPrompts = [];
    this.bulkDismissReasonDraft = '';
    this.bulkDismissSubmitting = false;
    this.bulkDismissCompleted = 0;
    this.bulkDismissGoneAgentIds = new Set();
    this.pendingResetInFlight = false;
    this.pendingLoading = false;
    this.pendingLoadingMore = false;
    this.pendingRefreshDeferred = false;
    this.madeLoading = false;
    this.madeLoadingMore = false;
    this.madeRefreshDeferred = false;
    this.pendingAbort?.abort();
    this.pendingAbort = null;
    this.madeAbort?.abort();
    this.madeAbort = null;
    this.userAbort?.abort();
    this.userAbort = null;
    this.notify();
  }

  private extractRouteState(): void {
    this.routeDecodeError = null;
    const pageData = this.adapter.getPageData();
    const rawPath = pageData?.path || window.location.pathname + window.location.search;
    const [pathname = '', query = ''] = rawPath.split('?', 2);

    const routeResult = this.adapter.extractRouteParams?.(pathname);
    if (routeResult?.decodeError) {
      this.routeDecodeError = routeResult.decodeError;
      this.error = routeResult.decodeError;
      this.initialLoading = false;
      this.notify();
      return;
    }

    if (pageData?.user?.id) {
      this.currentUserId = pageData.user.id;
    }

    const search = query ? `?${query}` : window.location.search;
    if (this.adapter.scope === 'profile') {
      const parsed = parseProfileDecisionsUrlState(search);
      this.activeTab = parsed.tab;
      this.sort = parsed.sort;
      if (parsed.tab === 'pending') {
        this.pendingSortOverride =
          parsed.sort !== defaultProfileDecisionsSort('pending') ? parsed.sort : null;
        this.madeSortOverride = null;
      } else {
        this.madeSortOverride =
          parsed.sort !== defaultProfileDecisionsSort('made') ? parsed.sort : null;
        this.pendingSortOverride = null;
      }
      this.filterProject = parsed.project;
      this.filterQ = parsed.q;
      this.searchDraft = parsed.q;
      this.filterConversation = parsed.conversation;
      this.filterAgent = parsed.agent;
      this.filterUser = parsed.user;
      this.filterProvenance = parsed.provenance;
      this.filterFrom = parsed.from;
      this.filterTo = parsed.to;
    } else {
      const parsed = parseDecisionsUrlState(search);
      this.activeTab = parsed.tab;
      this.sort = 'recent';
      this.filterProject = '';
      this.filterQ = parsed.q;
      this.searchDraft = parsed.q;
      this.filterConversation = parsed.conversation;
      this.filterAgent = parsed.agent;
      this.filterUser = parsed.user;
      this.filterProvenance = parsed.provenance;
      this.filterFrom = parsed.from;
      this.filterTo = parsed.to;
    }
    this.notify();
  }

  syncUrlState(): void {
    const nextQs =
      this.adapter.scope === 'profile'
        ? buildProfileDecisionsUrlSearch({
            tab: this.activeTab,
            sort: this.sort,
            project: this.filterProject,
            q: this.filterQ,
            conversation: this.filterConversation,
            agent: this.filterAgent,
            user: this.filterUser,
            provenance: this.filterProvenance,
            from: this.filterFrom,
            to: this.filterTo,
          })
        : buildDecisionsUrlSearch({
            tab: this.activeTab,
            q: this.filterQ,
            conversation: this.filterConversation,
            agent: this.filterAgent,
            user: this.filterUser,
            provenance: this.filterProvenance,
            from: this.filterFrom,
            to: this.filterTo,
          });
    const curQs = window.location.search.replace(/^\?/, '');
    if (nextQs !== curQs) {
      replaceSearch(nextQs);
    }
  }

  async bootIfEnabled(): Promise<void> {
    if (!this.enabled || this.booted) return;
    this.extractRouteState();
    if (this.routeDecodeError) {
      this.error = this.routeDecodeError;
      this.initialLoading = false;
      this.notify();
      return;
    }
    const validationError = this.adapter.validateBoot?.() ?? null;
    if (validationError) {
      this.error = validationError;
      this.initialLoading = false;
      this.notify();
      return;
    }

    this.syncUrlState();
    this.booted = true;
    this.hubNotFound = false;
    this.initialLoading = true;
    this.error = null;
    this.notify();

    if (this.currentUserId) {
      stateManager.setCurrentUserId(this.currentUserId);
    }
    this.adapter.configureStateScope();

    const userTask = this.currentUserId ? Promise.resolve() : this.loadCurrentUserBestEffort();
    const hostBootTask = this.adapter.onBoot ? this.adapter.onBoot() : Promise.resolve();
    const pendingTask = this.loadPendingPage({ mode: 'reset' });
    const madeTask =
      this.activeTab === 'made' ? this.loadMadePage({ mode: 'reset' }) : Promise.resolve();

    await Promise.all([userTask, hostBootTask, pendingTask, madeTask]);
    if (this.host.isConnected) {
      this.initialLoading = false;
      this.notify();
    }
  }

  private async loadCurrentUserBestEffort(): Promise<void> {
    this.userAbort?.abort();
    const ac = new AbortController();
    this.userAbort = ac;
    try {
      const res = await apiFetch('/auth/me', { authErrors: 'caller', signal: ac.signal });
      if (ac.signal.aborted || !res.ok || !this.host.isConnected) return;
      const body = (await res.json()) as { id?: string; user?: { id?: string } } | null;
      if (ac.signal.aborted || !this.host.isConnected) return;
      const uid = body?.id || body?.user?.id || '';
      if (uid && !this.currentUserId) {
        this.currentUserId = uid;
        stateManager.setCurrentUserId(uid);
        if (this.enabled && this.canOperate()) {
          this.adapter.configureStateScope();
        }
        this.notify();
      }
    } catch {
      // Best-effort fallback when pageData.user was not injected.
    } finally {
      if (this.userAbort === ac) {
        this.userAbort = null;
      }
    }
  }

  private recordKnownProjects(projects: readonly ProjectDecisionProjectRef[]): void {
    for (const p of projects) {
      if (p?.id) {
        this.knownProjectsById.set(p.id, p);
      }
    }
  }

  private recordKnownProjectsFromPrompts(prompts: readonly ProjectDecisionPromptView[]): void {
    for (const p of prompts) {
      if (p.project?.id) {
        this.knownProjectsById.set(p.project.id, p.project);
      }
    }
  }

  private recordKnownProjectsFromEntries(
    entries: readonly ProjectDecisionLogEntryView[],
    fallbackProject?: ProjectDecisionProjectRef
  ): void {
    for (const e of entries) {
      const proj = e.project?.id ? e.project : fallbackProject;
      if (proj?.id) {
        this.knownProjectsById.set(proj.id, proj);
      }
    }
  }

  projectDisplayName(ref: ProjectDecisionProjectRef | undefined, idFallback = ''): string {
    if (ref?.name) return ref.name;
    if (ref?.slug) return ref.slug;
    const id = ref?.id || idFallback;
    if (!id) return '';
    const known = this.knownProjectsById.get(id);
    return known?.name || known?.slug || id;
  }

  private updateCounts(next: ProjectDecisionCounts): void {
    this.counts = next;
    this.adapter.onCountsUpdated?.(next);
  }

  private mergeFacets(opts: {
    agents: ProfileDecisionActorView[];
    answerers: ProfileDecisionActorView[];
    conversations?: ProjectDecisionConversationView[] | undefined;
    projects?: ProjectDecisionProjectRef[] | undefined;
  }): void {
    this.facetsHydrated = true;
    if (opts.projects) {
      this.recordKnownProjects(opts.projects);
      if (opts.projects.length > 0 || this.facetsProjects.length === 0) {
        this.facetsProjects = opts.projects;
      }
    }
    if (opts.agents.length > 0 || this.facetsAgents.length === 0) {
      this.facetsAgents = opts.agents;
    }
    if (opts.answerers.length > 0 || this.facetsAnswerers.length === 0) {
      this.facetsAnswerers = opts.answerers;
    }
    if (hasDecisionsSearchCapability({ conversations: opts.conversations }) && opts.conversations) {
      this.supportsSearch = true;
      if (opts.conversations.length > 0 || this.facetsConversations.length === 0) {
        this.facetsConversations = opts.conversations;
      }
    } else if (this.adapter.scope === 'project' && !this.supportsSearch) {
      const stripPendingAgent = this.activeTab === 'pending' && Boolean(this.filterAgent);
      if (this.filterQ || this.filterConversation || stripPendingAgent) {
        this.filterQ = '';
        this.searchDraft = '';
        this.filterConversation = '';
        if (stripPendingAgent) {
          this.filterAgent = '';
        }
        this.syncUrlState();
      }
    }
  }

  currentSharedFilter(): ProfileSharedDecisionsFilter {
    return {
      q: this.filterQ,
      conversation: this.filterConversation,
      agent: this.filterAgent,
      ...(this.adapter.scope === 'profile' ? { project: this.filterProject } : {}),
    };
  }

  currentMadeFilter(): ProfileMadeDecisionsFilter {
    return {
      q: this.filterQ,
      conversation: this.filterConversation,
      agent: this.filterAgent,
      ...(this.adapter.scope === 'profile' ? { project: this.filterProject } : {}),
      user: this.filterUser,
      provenance: this.filterProvenance,
      from: dateBoundToIso(this.filterFrom, false),
      to: dateBoundToIso(this.filterTo, true),
    };
  }

  get isPendingFiltered(): boolean {
    if (this.adapter.scope === 'project' && !this.supportsSearch) return false;
    return Boolean(
      (this.supportsSearch &&
        (normalizeDecisionsSearchQuery(this.filterQ) || this.filterConversation)) ||
      this.filterAgent ||
      this.filterProject
    );
  }

  get isMadeFiltered(): boolean {
    return Boolean(
      (this.supportsSearch &&
        (normalizeDecisionsSearchQuery(this.filterQ) || this.filterConversation)) ||
      this.filterAgent ||
      this.filterProject ||
      this.filterUser ||
      this.filterProvenance ||
      this.filterFrom ||
      this.filterTo
    );
  }

  get hasDateRangeError(): boolean {
    return Boolean(this.filterFrom && this.filterTo && this.filterFrom > this.filterTo);
  }

  pendingEffectiveSort(): ProfileDecisionsSort {
    if (this.adapter.scope !== 'profile') return 'recent';
    return this.activeTab === 'pending'
      ? this.sort
      : (this.pendingSortOverride ?? defaultProfileDecisionsSort('pending'));
  }

  madeEffectiveSort(): ProfileDecisionsSort {
    if (this.adapter.scope !== 'profile') return 'recent';
    return this.activeTab === 'made'
      ? this.sort
      : (this.madeSortOverride ?? defaultProfileDecisionsSort('made'));
  }

  async loadPendingPage(opts: { mode: DecisionsLoadMode }): Promise<void> {
    if (!this.enabled || !this.canOperate()) return;
    if (opts.mode === 'live' && (this.pendingLoadingMore || this.pendingResetInFlight)) {
      this.pendingRefreshDeferred = true;
      return;
    }
    if (opts.mode !== 'live') {
      this.pendingRefreshDeferred = false;
      this.pendingNeedsReset = false;
    }

    const gen = ++this.pendingGen;
    this.pendingAbort?.abort();
    const ac = new AbortController();
    this.pendingAbort = ac;

    const effectiveSort = this.pendingEffectiveSort();
    const cursor = opts.mode === 'append' ? this.pendingNextCursor : undefined;
    const limit =
      opts.mode === 'live'
        ? Math.min(
            MAX_DECISIONS_PAGE_SIZE,
            Math.max(DEFAULT_DECISIONS_PAGE_SIZE, this.pendingPrompts.length)
          )
        : undefined;

    if (opts.mode === 'reset') {
      this.pendingResetInFlight = true;
      this.pendingLoading = !this.initialLoading;
      this.pendingSearchTimeout = false;
      this.notify();
    } else if (opts.mode === 'append') {
      this.pendingLoadingMore = true;
      this.notify();
    }

    try {
      const res = await this.adapter.fetchPending({
        sort: effectiveSort,
        cursor,
        limit,
        filter: this.currentSharedFilter(),
        signal: ac.signal,
      });
      if (gen !== this.pendingGen || !this.host.isConnected) return;
      this.updateCounts(res.counts);
      this.filteredCounts = res.filteredCounts ?? null;
      if ('truncatedProjects' in res) {
        this.truncatedProjects = res.truncatedProjects;
      }
      this.pendingSearchTimeout = false;
      this.recordKnownProjectsFromPrompts(res.prompts);

      if (opts.mode === 'reset') {
        this.pendingPrompts = res.prompts;
        this.pendingNextCursor = res.nextCursor ?? '';
        this.pendingPagedBeyondFirst = false;
      } else if (opts.mode === 'append') {
        const existingIds = new Set(this.pendingPrompts.map((p) => p.promptMessageId));
        const extra = res.prompts.filter((p) => !existingIds.has(p.promptMessageId));
        this.pendingPrompts = [...this.pendingPrompts, ...extra];
        this.pendingNextCursor = res.nextCursor ?? '';
        this.pendingPagedBeyondFirst = true;
      } else {
        const mergedIncoming = this.mergeHeldDecisionRanks(res.prompts);
        // In sort=project mode on profile, P4's cursor tracks finished projects;
        // always reload from page 1 so project group ranks and cursors stay consistent.
        if (effectiveSort === 'recent' && this.pendingPrompts.length > MAX_DECISIONS_PAGE_SIZE) {
          const headIds = new Set(mergedIncoming.map((p) => p.promptMessageId));
          const openTail = this.pendingPrompts
            .slice(MAX_DECISIONS_PAGE_SIZE)
            .filter(
              (p) =>
                !headIds.has(p.promptMessageId) && (p.waitingOnYou || promptHasWaitingOnOthers(p))
            );
          this.pendingPrompts = [...mergedIncoming, ...openTail];
        } else {
          this.pendingPrompts = mergedIncoming;
          this.pendingNextCursor = res.nextCursor ?? '';
          this.pendingPagedBeyondFirst = false;
        }
      }

      this.pruneInvalidSelections();
      this.mergeFacets({
        agents: res.agents,
        answerers: res.answerers,
        conversations: res.conversations,
        projects: 'projects' in res ? res.projects : undefined,
      });
      if (opts.mode !== 'live') {
        this.error = null;
      }
      this.scheduleDismissUnlockTimer();
      this.notify();
    } catch (err) {
      if (ac.signal.aborted || gen !== this.pendingGen || !this.host.isConnected) return;
      if (
        this.adapter.scope === 'profile' &&
        err instanceof DecisionsApiError &&
        err.status === 404
      ) {
        this.hubNotFound = true;
        this.error = null;
        this.adapter.onNotFound?.();
        this.notify();
        return;
      }
      if (opts.mode !== 'live') {
        if (isDecisionsSearchTimeoutError(err)) {
          this.supportsSearch = true;
          this.pendingSearchTimeout = true;
          this.error = null;
          this.notify();
          if (opts.mode === 'reset' && this.activeTab === 'pending' && !this.facetsHydrated) {
            await this.hydrateFacetsAfterTimeout('pending', ac.signal, gen);
          }
        } else {
          this.pendingSearchTimeout = false;
          this.error = err instanceof Error ? err.message : 'Failed to load decisions';
          this.notify();
        }
      }
    } finally {
      if (gen === this.pendingGen) {
        this.pendingResetInFlight = false;
        this.pendingLoading = false;
        this.pendingLoadingMore = false;
        if (this.pendingAbort === ac) this.pendingAbort = null;
        this.notify();
        if (this.pendingRefreshDeferred) {
          this.pendingRefreshDeferred = false;
          if (
            this.host.isConnected &&
            this.enabled &&
            this.canOperate() &&
            !this.error &&
            !this.pendingSearchTimeout &&
            !this.pendingNeedsReset
          ) {
            void this.loadPendingPage({ mode: 'live' });
          }
        }
      }
    }
  }

  private async hydrateFacetsAfterTimeout(
    tab: 'pending' | 'made',
    signal: AbortSignal,
    expectedGen: number
  ): Promise<void> {
    if (this.facetsHydrated || !this.host.isConnected || !this.enabled || !this.canOperate()) {
      return;
    }
    try {
      if (tab === 'pending') {
        const res = await this.adapter.fetchPending({
          sort: this.pendingEffectiveSort(),
          limit: 1,
          filter: {
            conversation: this.filterConversation,
            agent: this.filterAgent,
            ...(this.adapter.scope === 'profile' ? { project: this.filterProject } : {}),
          },
          signal,
        });
        if (signal.aborted || expectedGen !== this.pendingGen || !this.host.isConnected) return;
        this.updateCounts(res.counts);
        if ('truncatedProjects' in res) {
          this.truncatedProjects = res.truncatedProjects;
        }
        this.recordKnownProjectsFromPrompts(res.prompts);
        this.mergeFacets({
          agents: res.agents,
          answerers: res.answerers,
          conversations: res.conversations,
          projects: 'projects' in res ? res.projects : undefined,
        });
        this.notify();
      } else {
        const res = await this.adapter.fetchMade({
          sort: this.madeEffectiveSort(),
          limit: 1,
          filter: { ...this.currentMadeFilter(), q: '' },
          signal,
        });
        if (signal.aborted || expectedGen !== this.madeGen || !this.host.isConnected) return;
        this.updateCounts(res.counts);
        if ('truncatedProjects' in res) {
          this.truncatedProjects = res.truncatedProjects;
        }
        if ('sort' in res && res.sort === 'project') {
          for (const g of res.groups) {
            this.recordKnownProjectsFromEntries(g.entries, g.project);
          }
        } else {
          this.recordKnownProjectsFromEntries(res.entries);
        }
        this.mergeFacets({
          agents: res.agents,
          answerers: res.answerers,
          conversations: res.conversations,
          projects: 'projects' in res ? res.projects : undefined,
        });
        this.notify();
      }
    } catch {
      // Best-effort facet hydration: preserve SEARCH_TIMEOUT warning alert on fallback failure.
    }
  }

  async loadMadePage(opts: { mode: DecisionsLoadMode }): Promise<void> {
    if (!this.enabled || !this.canOperate() || this.hasDateRangeError) return;
    if (opts.mode === 'live' && (this.madeLoadingMore || this.madeLoading)) {
      this.madeRefreshDeferred = true;
      return;
    }
    if (opts.mode !== 'live') {
      this.madeRefreshDeferred = false;
      this.madeNeedsReset = false;
    }
    this.madeStale = false;

    const gen = ++this.madeGen;
    this.madeAbort?.abort();
    const ac = new AbortController();
    this.madeAbort = ac;

    const effectiveSort = this.madeEffectiveSort();
    const cursor =
      opts.mode === 'append' && effectiveSort === 'recent' ? this.madeNextCursor : undefined;
    const limit = opts.mode === 'live' ? DEFAULT_DECISIONS_PAGE_SIZE : undefined;

    if (opts.mode === 'reset') {
      this.madeLoading = true;
      this.madeError = null;
      this.madeSearchTimeout = false;
      this.notify();
    } else if (opts.mode === 'append') {
      this.madeLoadingMore = true;
      this.madeError = null;
      this.notify();
    }

    try {
      const res = await this.adapter.fetchMade({
        sort: effectiveSort,
        cursor,
        limit,
        filter: this.currentMadeFilter(),
        signal: ac.signal,
      });
      if (gen !== this.madeGen || !this.host.isConnected) return;
      this.updateCounts(res.counts);
      if ('truncatedProjects' in res) {
        this.truncatedProjects = res.truncatedProjects;
      }
      this.madeLoaded = true;
      this.madeError = null;
      this.madeSearchTimeout = false;

      if ('sort' in res && res.sort === 'project') {
        for (const g of res.groups) {
          this.recordKnownProjectsFromEntries(g.entries, g.project);
        }
        this.madeGroups = res.groups;
        this.madeEntries = [];
        this.madeNextCursor = '';
        this.madePagedBeyondFirst = false;
      } else {
        this.recordKnownProjectsFromEntries(res.entries);
        this.madeGroups = [];
        if (opts.mode === 'reset') {
          this.madeEntries = res.entries;
          this.madeNextCursor = res.nextCursor ?? '';
          this.madePagedBeyondFirst = false;
        } else if (opts.mode === 'append') {
          const existingIds = new Set(this.madeEntries.map((e) => e.answerId));
          const extra = res.entries.filter((e) => !existingIds.has(e.answerId));
          this.madeEntries = [...this.madeEntries, ...extra];
          this.madeNextCursor = res.nextCursor ?? '';
          this.madePagedBeyondFirst = true;
        } else {
          const incomingById = new Map(res.entries.map((e) => [e.answerId, e]));
          const existingIds = new Set(this.madeEntries.map((e) => e.answerId));
          const prepended = res.entries.filter((e) => !existingIds.has(e.answerId));
          const updatedExisting = this.madeEntries.map((e) => incomingById.get(e.answerId) ?? e);
          this.madeEntries = [...prepended, ...updatedExisting];
          if (!this.madePagedBeyondFirst && !this.madeNextCursor) {
            this.madeNextCursor = res.nextCursor ?? '';
          }
        }
      }

      this.mergeFacets({
        agents: res.agents,
        answerers: res.answerers,
        conversations: res.conversations,
        projects: 'projects' in res ? res.projects : undefined,
      });
      this.notify();
    } catch (err) {
      if (ac.signal.aborted || gen !== this.madeGen || !this.host.isConnected) return;
      if (
        this.adapter.scope === 'profile' &&
        err instanceof DecisionsApiError &&
        err.status === 404
      ) {
        this.hubNotFound = true;
        this.madeError = null;
        this.adapter.onNotFound?.();
        this.notify();
        return;
      }
      if (opts.mode !== 'live') {
        if (isDecisionsSearchTimeoutError(err)) {
          this.supportsSearch = true;
          this.madeSearchTimeout = true;
          this.madeError = null;
          this.notify();
          if (opts.mode === 'reset' && this.activeTab === 'made' && !this.facetsHydrated) {
            await this.hydrateFacetsAfterTimeout('made', ac.signal, gen);
          }
        } else {
          this.madeSearchTimeout = false;
          this.madeError = err instanceof Error ? err.message : 'Failed to load decision log';
          this.notify();
        }
      }
    } finally {
      if (gen === this.madeGen) {
        this.madeLoading = false;
        this.madeLoadingMore = false;
        if (this.madeAbort === ac) this.madeAbort = null;
        this.notify();
        if (this.madeRefreshDeferred) {
          this.madeRefreshDeferred = false;
          if (
            this.host.isConnected &&
            this.enabled &&
            this.canOperate() &&
            !this.hasDateRangeError &&
            !this.madeError &&
            !this.madeSearchTimeout &&
            !this.madeNeedsReset
          ) {
            if (this.activeTab === 'made') {
              void this.loadMadePage({ mode: 'live' });
            } else if (this.madeLoaded) {
              this.madeStale = true;
            }
          }
        }
      }
    }
  }

  applyIncomingDecisionPatch(
    messageId: string | undefined,
    incoming: readonly DecisionView[]
  ): void {
    if (incoming.length === 0 || this.pendingPrompts.length === 0) return;
    const incomingIds = new Set(incoming.map((d) => d.rowId));
    let changed = false;
    let deltaWaitingOnYou = 0;

    const nextPrompts = this.pendingPrompts.map((p) => {
      const matches =
        (messageId && p.promptMessageId === messageId) ||
        p.decisions.some((d) => incomingIds.has(d.rowId));
      if (!matches) return p;

      const patched = patchDecisions(p.decisions, incoming);
      if (patched === p.decisions) return p;
      changed = true;

      const wasActionableCount = p.waitingOnYou
        ? p.decisions.filter(
            (d) => !p.waitingReasons?.[d.rowId] && actionableReason(d, this.currentUserId) === ''
          ).length
        : 0;
      const nowActionableCount = p.waitingOnYou
        ? patched.filter(
            (d) => !p.waitingReasons?.[d.rowId] && actionableReason(d, this.currentUserId) === ''
          ).length
        : 0;
      deltaWaitingOnYou += nowActionableCount - wasActionableCount;

      const stillWaitingOnYou = p.waitingOnYou && nowActionableCount > 0;
      return {
        ...p,
        decisions: patched,
        waitingOnYou: stillWaitingOnYou,
      };
    });

    if (changed) {
      this.pendingPrompts = nextPrompts;
      this.pruneInvalidSelections();
      this.scheduleDismissUnlockTimer();
      if (deltaWaitingOnYou !== 0) {
        this.updateCounts({
          ...this.counts,
          waitingOnYou: Math.max(0, this.counts.waitingOnYou + deltaWaitingOnYou),
        });
        if (this.filteredCounts !== null) {
          this.filteredCounts = {
            ...this.filteredCounts,
            waitingOnYou: Math.max(0, this.filteredCounts.waitingOnYou + deltaWaitingOnYou),
          };
        }
      }
      this.notify();
    }
  }

  scheduleLiveRefresh(): void {
    if (this.liveRefreshTimer !== null) {
      clearTimeout(this.liveRefreshTimer);
    }
    this.liveRefreshTimer = setTimeout(() => {
      this.liveRefreshTimer = null;
      if (!this.host.isConnected || !this.enabled || !this.canOperate()) return;
      if (!this.error && !this.pendingSearchTimeout) {
        if (this.pendingNeedsReset) {
          // Never run an off-tab live pending load when pendingNeedsReset is
          // set: a live error or >200 tail merge would preserve unfiltered rows
          // across switchTab('pending') (review pass 1 finding 2).
          if (this.activeTab === 'pending') {
            this.pendingNextCursor = '';
            this.pendingPagedBeyondFirst = false;
            void this.loadPendingPage({ mode: 'reset' });
          }
        } else if (
          this.adapter.scope === 'profile' &&
          this.pendingEffectiveSort() === 'project' &&
          this.pendingPagedBeyondFirst
        ) {
          if (this.activeTab === 'pending') {
            this.pendingNextCursor = '';
            this.pendingPagedBeyondFirst = false;
            void this.loadPendingPage({ mode: 'reset' });
          } else {
            this.pendingNeedsReset = true;
          }
        } else {
          void this.loadPendingPage({ mode: 'live' });
        }
      }
      if (this.activeTab === 'made') {
        if (
          !this.hasDateRangeError &&
          !this.madeSearchTimeout &&
          !this.madeNeedsReset &&
          (this.madeLoaded || this.madeLoading)
        ) {
          void this.loadMadePage({ mode: 'live' });
        }
      } else if (this.madeLoaded || this.madeLoading) {
        this.madeStale = true;
      }
    }, 60);
  }

  private flushDebouncedSearchIfPending(): boolean {
    if (this.searchDebounceTimer === null) return false;
    clearTimeout(this.searchDebounceTimer);
    this.searchDebounceTimer = null;
    const nextQ = normalizeDecisionsSearchQuery(this.searchDraft);
    if (nextQ !== this.filterQ) {
      this.filterQ = nextQ;
      return true;
    }
    return false;
  }

  invalidateInFlightMadeLoad(): void {
    this.madeNeedsReset = true;
    this.madeSearchTimeout = false;
    this.madeRefreshDeferred = false;
    if (this.madeAbort || this.madeLoading || this.madeLoadingMore) {
      ++this.madeGen;
      this.madeAbort?.abort();
      this.madeAbort = null;
      this.madeLoading = false;
      this.madeLoadingMore = false;
    }
    this.notify();
  }

  invalidateInFlightPendingLoad(): void {
    this.pendingNeedsReset = true;
    this.pendingSearchTimeout = false;
    this.pendingRefreshDeferred = false;
    if (this.pendingAbort || this.pendingResetInFlight || this.pendingLoadingMore) {
      ++this.pendingGen;
      this.pendingAbort?.abort();
      this.pendingAbort = null;
      this.pendingResetInFlight = false;
      this.pendingLoading = false;
      this.pendingLoadingMore = false;
    }
    this.notify();
  }

  switchTab(next: 'pending' | 'made'): void {
    if (next !== 'pending' && next !== 'made') return;
    if (this.activeTab === next) return;

    const flushedSearch = this.flushDebouncedSearchIfPending();
    if (flushedSearch) {
      if (next === 'pending') {
        this.invalidateInFlightMadeLoad();
        this.pendingNeedsReset = true;
      } else {
        this.invalidateInFlightPendingLoad();
        this.madeNeedsReset = true;
      }
    }

    if (next === 'pending') {
      const stripOldHubAgent =
        this.adapter.scope === 'project' && !this.supportsSearch && Boolean(this.filterAgent);
      const hadMadeOnlyFilters = Boolean(
        stripOldHubAgent ||
        this.filterUser ||
        this.filterProvenance ||
        this.filterFrom ||
        this.filterTo
      );
      if (hadMadeOnlyFilters) {
        if (stripOldHubAgent) {
          this.filterAgent = '';
        }
        this.filterUser = '';
        this.filterProvenance = '';
        this.filterFrom = '';
        this.filterTo = '';
        this.invalidateInFlightMadeLoad();
      }
    }

    if (this.adapter.scope === 'profile') {
      this.sort =
        next === 'pending'
          ? (this.pendingSortOverride ?? defaultProfileDecisionsSort('pending'))
          : (this.madeSortOverride ?? defaultProfileDecisionsSort('made'));
    }

    if (!this.bulkDismissSubmitting && this.selectedPromptIds.size > 0) {
      this.selectedPromptIds = new Set();
    }
    this.activeTab = next;
    this.syncUrlState();
    this.notify();

    const group = this.host.shadowRoot?.querySelector('sl-tab-group') as
      | (HTMLElement & { show?: (panel: string) => void })
      | null;
    if (typeof group?.show === 'function') {
      group.show(next);
    }

    if (next === 'pending') {
      if (this.pendingNeedsReset) {
        this.pendingNextCursor = '';
        this.pendingPagedBeyondFirst = false;
        void this.loadPendingPage({ mode: 'reset' });
      }
      return;
    }

    if (!this.hasDateRangeError) {
      if (this.madeNeedsReset) {
        this.madeNextCursor = '';
        this.madePagedBeyondFirst = false;
        void this.loadMadePage({ mode: 'reset' });
      } else if (!this.madeLoading) {
        if (!this.madeLoaded) {
          void this.loadMadePage({ mode: 'reset' });
        } else if (this.madeStale) {
          this.madeStale = false;
          void this.loadMadePage({ mode: 'live' });
        }
      }
    }
  }

  setSort(next: ProfileDecisionsSort): void {
    if (this.adapter.scope !== 'profile') return;
    if (next !== 'project' && next !== 'recent') return;
    if (this.sort === next) return;

    const flushedSearch = this.flushDebouncedSearchIfPending();
    this.sort = next;
    if (this.activeTab === 'pending') {
      this.pendingSortOverride = next !== defaultProfileDecisionsSort('pending') ? next : null;
      if (
        flushedSearch &&
        (this.madeLoaded ||
          this.madeSearchTimeout ||
          this.madeLoading ||
          this.madeLoadingMore ||
          this.madeAbort !== null)
      ) {
        this.invalidateInFlightMadeLoad();
      }
      this.syncUrlState();
      if (!this.bulkDismissSubmitting && this.selectedPromptIds.size > 0) {
        this.selectedPromptIds = new Set();
      }
      this.pendingNextCursor = '';
      this.pendingPagedBeyondFirst = false;
      this.notify();
      void this.loadPendingPage({ mode: 'reset' });
    } else {
      this.madeSortOverride = next !== defaultProfileDecisionsSort('made') ? next : null;
      if (flushedSearch) {
        this.invalidateInFlightPendingLoad();
      }
      this.madeNextCursor = '';
      this.madePagedBeyondFirst = false;
      this.madeRefreshDeferred = false;
      this.syncUrlState();
      this.notify();
      if (this.hasDateRangeError) {
        ++this.madeGen;
        this.madeAbort?.abort();
        this.madeAbort = null;
        this.madeLoading = false;
        this.madeLoadingMore = false;
        this.madeError = null;
        this.madeSearchTimeout = false;
        this.notify();
        return;
      }
      void this.loadMadePage({ mode: 'reset' });
    }
  }

  drillDownMadeProject(projectId: string): void {
    if (this.adapter.scope !== 'profile') return;
    const cleaned = cleanProfileProjectValue(projectId);
    if (!cleaned) return;
    const flushedSearch = this.flushDebouncedSearchIfPending();
    const projectChanged = cleaned !== this.filterProject;
    this.filterProject = cleaned;
    this.sort = 'recent';
    this.madeSortOverride = null;
    if (projectChanged || flushedSearch) {
      this.invalidateInFlightPendingLoad();
    }
    this.madeNextCursor = '';
    this.madePagedBeyondFirst = false;
    this.madeRefreshDeferred = false;
    this.syncUrlState();
    this.notify();
    if (this.hasDateRangeError) {
      ++this.madeGen;
      this.madeAbort?.abort();
      this.madeAbort = null;
      this.madeLoading = false;
      this.madeLoadingMore = false;
      this.madeError = null;
      this.madeSearchTimeout = false;
      this.notify();
      return;
    }
    void this.loadMadePage({ mode: 'reset' });
  }

  onTabShow(e: Event): void {
    if (e.target !== e.currentTarget) return;
    const name = (e as CustomEvent<{ name?: string }>).detail?.name;
    if (name === 'pending' || name === 'made') {
      this.switchTab(name);
    }
  }

  onPromptDecisionAnswered = (
    prompt: ProjectDecisionPromptView,
    e: CustomEvent<DecisionAnsweredDetail>
  ): void => {
    this.applyIncomingDecisionPatch(
      e.detail.messageId || prompt.promptMessageId,
      e.detail.decisions
    );
    this.scheduleLiveRefresh();
  };

  onPromptMessageJump = (
    prompt: ProjectDecisionPromptView,
    e: CustomEvent<{ messageId?: string }>
  ): void => {
    if (!prompt.conversationAvailable) return;
    const targetMsgId = e.detail?.messageId?.trim();
    const projectId = this.adapter.resolvePromptProjectId(prompt);
    if (!targetMsgId || !projectId) return;
    const base = chatConversationPath({
      conversationKey: prompt.chatKey,
      projectId,
    });
    if (!base) return;
    navigateTo(`${base}#msg-${encodeURIComponent(targetMsgId)}`);
  };

  onSearchInput(detail: DecisionsSearchInputDetail): void {
    this.searchDraft = detail.value;
    this.notify();
    if (this.searchDebounceTimer !== null) {
      clearTimeout(this.searchDebounceTimer);
      this.searchDebounceTimer = null;
    }

    const commitSearch = (): void => {
      const nextQ = normalizeDecisionsSearchQuery(this.searchDraft);
      if (nextQ === this.filterQ && !this.pendingSearchTimeout && !this.madeSearchTimeout) {
        return;
      }
      this.filterQ = nextQ;
      this.onFiltersUpdated({ sharedChanged: true });
    };

    if (detail.immediate) {
      commitSearch();
      return;
    }
    this.searchDebounceTimer = setTimeout(() => {
      this.searchDebounceTimer = null;
      commitSearch();
    }, SEARCH_DEBOUNCE_MS);
  }

  applyFilterPatch(patch: DecisionsFilterPatch): void {
    let sharedChanged = false;
    let madeOnlyChanged = false;

    if (patch.q !== undefined) {
      if (this.searchDebounceTimer !== null) {
        clearTimeout(this.searchDebounceTimer);
        this.searchDebounceTimer = null;
      }
      const nextQ = normalizeDecisionsSearchQuery(patch.q);
      this.searchDraft = nextQ;
      if (nextQ !== this.filterQ) {
        this.filterQ = nextQ;
        sharedChanged = true;
      }
    }
    if (this.adapter.scope === 'profile' && patch.project !== undefined) {
      const nextProject = cleanProfileProjectValue(patch.project);
      if (nextProject !== this.filterProject) {
        this.filterProject = nextProject;
        sharedChanged = true;
      }
    }
    if (patch.conversation !== undefined && patch.conversation !== this.filterConversation) {
      this.filterConversation = patch.conversation;
      sharedChanged = true;
    }
    if (patch.agent !== undefined && patch.agent !== this.filterAgent) {
      this.filterAgent = patch.agent;
      sharedChanged = true;
    }
    if (patch.user !== undefined && patch.user !== this.filterUser) {
      this.filterUser = patch.user;
      madeOnlyChanged = true;
    }
    if (patch.provenance !== undefined && patch.provenance !== this.filterProvenance) {
      this.filterProvenance = patch.provenance;
      madeOnlyChanged = true;
    }
    if (patch.from !== undefined && patch.from !== this.filterFrom) {
      this.filterFrom = patch.from;
      madeOnlyChanged = true;
    }
    if (patch.to !== undefined && patch.to !== this.filterTo) {
      this.filterTo = patch.to;
      madeOnlyChanged = true;
    }

    this.notify();
    if (sharedChanged || madeOnlyChanged) {
      this.onFiltersUpdated({ sharedChanged });
    }
  }

  private onFiltersUpdated(opts: { sharedChanged: boolean }): void {
    this.syncUrlState();
    this.notify();

    if (this.activeTab === 'pending') {
      if (opts.sharedChanged) {
        if (!this.bulkDismissSubmitting && this.selectedPromptIds.size > 0) {
          this.selectedPromptIds = new Set();
        }
        this.pendingNextCursor = '';
        this.pendingPagedBeyondFirst = false;
        if (
          this.madeLoaded ||
          this.madeSearchTimeout ||
          this.madeLoading ||
          this.madeLoadingMore ||
          this.madeAbort !== null
        ) {
          this.invalidateInFlightMadeLoad();
        }
        void this.loadPendingPage({ mode: 'reset' });
      }
      return;
    }

    if (opts.sharedChanged) {
      this.invalidateInFlightPendingLoad();
    }
    this.madeNextCursor = '';
    this.madePagedBeyondFirst = false;
    this.madeRefreshDeferred = false;
    if (this.hasDateRangeError) {
      ++this.madeGen;
      this.madeAbort?.abort();
      this.madeAbort = null;
      this.madeLoading = false;
      this.madeLoadingMore = false;
      this.madeError = null;
      this.madeSearchTimeout = false;
      this.notify();
      return;
    }
    void this.loadMadePage({ mode: 'reset' });
  }

  clearFilters = (): void => {
    if (this.searchDebounceTimer !== null) {
      clearTimeout(this.searchDebounceTimer);
      this.searchDebounceTimer = null;
    }
    const hadShared = Boolean(
      this.filterProject ||
      this.filterQ ||
      this.searchDraft ||
      this.filterConversation ||
      this.filterAgent
    );
    const hadMadeOnly = Boolean(
      this.filterUser || this.filterProvenance || this.filterFrom || this.filterTo
    );
    this.filterProject = '';
    this.filterQ = '';
    this.searchDraft = '';
    this.filterConversation = '';
    this.filterAgent = '';
    this.filterUser = '';
    this.filterProvenance = '';
    this.filterFrom = '';
    this.filterTo = '';
    this.notify();
    if (!hadShared && !hadMadeOnly && !this.pendingSearchTimeout && !this.madeSearchTimeout) {
      return;
    }
    this.onFiltersUpdated({
      sharedChanged: hadShared || this.pendingSearchTimeout || this.madeSearchTimeout,
    });
  };

  retryAll = (): void => {
    this.booted = false;
    this.facetsHydrated = false;
    void this.bootIfEnabled();
  };

  promptDismissAvailableAtMs = (prompt: ProjectDecisionPromptView): number => {
    const overrideMs = this.dismissAvailableAtOverrides.get(prompt.promptMessageId) ?? 0;
    return Math.max(dismissAvailableAtMs(prompt.createdAt), overrideMs);
  };

  promptDismissRemainingMinutes = (prompt: ProjectDecisionPromptView): number => {
    const nowMs = this.clockNow();
    const availMs = this.promptDismissAvailableAtMs(prompt);
    if (availMs <= 0 || nowMs >= availMs) return 0;
    return Math.max(1, Math.ceil((availMs - nowMs) / 60000));
  };

  private scheduleDismissUnlockTimer(): void {
    if (this.dismissUnlockTimer !== null) {
      clearTimeout(this.dismissUnlockTimer);
      this.dismissUnlockTimer = null;
    }
    if (!this.host.isConnected || !this.enabled || !this.canOperate()) return;
    const nowMs = this.clockNow();
    let earliestMs = Infinity;
    for (const p of this.pendingPrompts) {
      if (!p.waitingOnYou || !p.canAnswer) continue;
      if (promptWaitingOnViewerRows(p, this.currentUserId).length === 0) continue;
      const overrideMs = this.dismissAvailableAtOverrides.get(p.promptMessageId) ?? 0;
      const availMs = Math.max(dismissAvailableAtMs(p.createdAt), overrideMs);
      if (availMs > nowMs && availMs < earliestMs) {
        earliestMs = availMs;
      }
    }
    if (!Number.isFinite(earliestMs)) return;
    const delayMs = Math.min(2_147_483_647, Math.max(1, earliestMs - nowMs + 25));
    this.dismissUnlockTimer = setTimeout(() => {
      this.dismissUnlockTimer = null;
      if (!this.host.isConnected || !this.enabled) return;
      this.scheduleDismissUnlockTimer();
      this.notify();
    }, delayMs);
  }

  isPromptAskerGone = (prompt: ProjectDecisionPromptView): boolean => {
    const facetActor = this.facetsAgents.find((a) => a.id === prompt.askerAgentId);
    return Boolean(facetActor?.hint?.includes('deleted'));
  };

  onPromptDismissTooRecent = (
    prompt: ProjectDecisionPromptView,
    e: CustomEvent<{ messageId: string; availableAt: string }>
  ): void => {
    const parsed = Date.parse(e.detail?.availableAt ?? '');
    if (!Number.isFinite(parsed)) return;
    this.dismissAvailableAtOverrides.set(prompt.promptMessageId, parsed);
    this.pruneInvalidSelections();
    this.scheduleDismissUnlockTimer();
    this.notify();
  };

  private mergeHeldDecisionRanks(
    incomingPrompts: readonly ProjectDecisionPromptView[]
  ): ProjectDecisionPromptView[] {
    if (this.pendingPrompts.length === 0) return [...incomingPrompts];
    const heldByMsgId = new Map(this.pendingPrompts.map((p) => [p.promptMessageId, p]));
    return incomingPrompts.map((incoming) => {
      const held = heldByMsgId.get(incoming.promptMessageId);
      if (!held) return incoming;
      const heldRows = new Map(held.decisions.map((d) => [d.rowId, d]));
      let changed = false;
      const nextDecisions = incoming.decisions.map((d) => {
        const prior = heldRows.get(d.rowId);
        if (!prior) return d;
        const chosen = newerDecisionView(prior, d);
        if (chosen !== d) changed = true;
        return chosen;
      });
      if (!changed) return incoming;
      const nowActionable = incoming.waitingOnYou
        ? nextDecisions.filter(
            (d) =>
              !incoming.waitingReasons?.[d.rowId] && actionableReason(d, this.currentUserId) === ''
          ).length
        : 0;
      return {
        ...incoming,
        decisions: nextDecisions,
        waitingOnYou: incoming.waitingOnYou && nowActionable > 0,
      };
    });
  }

  orderedWaitingPrompts(): ProjectDecisionPromptView[] {
    const waiting = this.pendingPrompts.filter((p) => p.waitingOnYou);
    if (this.adapter.scope !== 'profile' || this.pendingEffectiveSort() !== 'project') {
      return waiting;
    }
    const groups: ProjectDecisionPromptView[][] = [];
    const byProject = new Map<string, ProjectDecisionPromptView[]>();
    for (const p of waiting) {
      const pid = p.project?.id || 'unknown';
      let bucket = byProject.get(pid);
      if (!bucket) {
        bucket = [];
        byProject.set(pid, bucket);
        groups.push(bucket);
      }
      bucket.push(p);
    }
    return groups.flat();
  }

  promptCanDismiss = (prompt: ProjectDecisionPromptView): boolean =>
    promptWaitingOnViewerRows(prompt, this.currentUserId).length > 0;

  isPromptSelectable = (prompt: ProjectDecisionPromptView): boolean =>
    this.promptCanDismiss(prompt) && this.promptDismissRemainingMinutes(prompt) === 0;

  selectableWaitingPrompts(): ProjectDecisionPromptView[] {
    return this.orderedWaitingPrompts().filter(this.isPromptSelectable);
  }

  isPromptSelected = (promptMessageId: string): boolean =>
    this.selectedPromptIds.has(promptMessageId);

  isPromptSelectionCapReached = (promptMessageId: string): boolean =>
    !this.selectedPromptIds.has(promptMessageId) &&
    this.selectedPromptIds.size >= MAX_BULK_DISMISS_SELECTION;

  private pruneInvalidSelections(): void {
    if (this.selectedPromptIds.size === 0) return;
    const byId = new Map(this.pendingPrompts.map((p) => [p.promptMessageId, p]));
    let removed = false;
    const next = new Set<string>();
    for (const id of this.selectedPromptIds) {
      const p = byId.get(id);
      if (p && this.isPromptSelectable(p) && next.size < MAX_BULK_DISMISS_SELECTION) {
        next.add(id);
      } else {
        removed = true;
      }
    }
    if (removed) {
      this.selectedPromptIds = next;
    }
    if (this.bulkDismissPrompts.length > 0 && !this.bulkDismissSubmitting) {
      const refreshed = this.bulkDismissPrompts
        .map((p) => byId.get(p.promptMessageId))
        .filter((p): p is ProjectDecisionPromptView => Boolean(p && this.isPromptSelectable(p)));
      if (refreshed.length === 0) {
        this.bulkDismissPrompts = [];
        this.bulkDismissReasonDraft = '';
        this.bulkDismissGoneAgentIds = new Set();
      } else {
        this.bulkDismissPrompts = refreshed;
      }
    }
  }

  setPromptSelected = (prompt: ProjectDecisionPromptView, checked: boolean): void => {
    if (this.bulkDismissSubmitting) return;
    if (!checked) {
      if (this.selectedPromptIds.delete(prompt.promptMessageId)) {
        this.selectedPromptIds = new Set(this.selectedPromptIds);
      }
      this.notify();
      return;
    }
    if (!this.isPromptSelectable(prompt)) {
      this.notify();
      return;
    }
    if (
      !this.selectedPromptIds.has(prompt.promptMessageId) &&
      this.selectedPromptIds.size >= MAX_BULK_DISMISS_SELECTION
    ) {
      this.notify();
      return;
    }
    const next = new Set(this.selectedPromptIds);
    next.add(prompt.promptMessageId);
    this.selectedPromptIds = next;
    this.notify();
  };

  toggleSelectAllPrompts = (checked: boolean): void => {
    if (this.bulkDismissSubmitting) return;
    const selectable = this.selectableWaitingPrompts();
    const cap = Math.min(selectable.length, MAX_BULK_DISMISS_SELECTION);
    if (!checked || cap === 0) {
      this.clearPromptSelection();
      return;
    }
    const next = new Set<string>();
    for (const p of selectable) {
      if (this.selectedPromptIds.has(p.promptMessageId) && next.size < MAX_BULK_DISMISS_SELECTION) {
        next.add(p.promptMessageId);
      }
    }
    for (const p of selectable) {
      if (next.size >= MAX_BULK_DISMISS_SELECTION) break;
      next.add(p.promptMessageId);
    }
    this.selectedPromptIds = next;
    this.notify();
  };

  clearPromptSelection = (): void => {
    if (this.bulkDismissSubmitting || this.selectedPromptIds.size === 0) return;
    this.selectedPromptIds = new Set();
    this.notify();
  };

  openBulkDismissDialog = (): void => {
    if (this.bulkDismissSubmitting) return;
    const selected = this.orderedWaitingPrompts()
      .filter((p) => this.selectedPromptIds.has(p.promptMessageId) && this.isPromptSelectable(p))
      .slice(0, MAX_BULK_DISMISS_SELECTION);
    if (selected.length === 0) return;

    this.bulkDismissPrompts = selected;
    this.bulkDismissReasonDraft = '';
    this.bulkDismissSubmitting = false;
    this.bulkDismissCompleted = 0;

    const initialGone = new Set<string>();
    const distinctAgents = new Map<string, string>();
    for (const p of selected) {
      if (!p.askerAgentId) continue;
      distinctAgents.set(p.askerAgentId, p.askerAgentName);
      const actor = this.facetsAgents.find((a) => a.id === p.askerAgentId);
      if (actor?.hint?.includes('deleted')) {
        initialGone.add(p.askerAgentId);
      }
    }
    this.bulkDismissGoneAgentIds = initialGone;
    this.notify();

    if (distinctAgents.size > 0) {
      const gen = ++this.bulkDismissProbeGen;
      for (const [agentId, agentName] of distinctAgents) {
        void fetchAskerAnswerability(agentId, agentName).then((ans) => {
          if (
            gen !== this.bulkDismissProbeGen ||
            !this.host.isConnected ||
            this.bulkDismissPrompts.length === 0
          ) {
            return;
          }
          const next = new Set(this.bulkDismissGoneAgentIds);
          if (ans.state === 'gone') {
            next.add(agentId);
          } else {
            next.delete(agentId);
          }
          if (next.size !== this.bulkDismissGoneAgentIds.size) {
            this.bulkDismissGoneAgentIds = next;
            this.notify();
          }
        });
      }
    }
  };

  closeBulkDismissDialog = (): void => {
    if (this.bulkDismissSubmitting) return;
    ++this.bulkDismissProbeGen;
    this.bulkDismissPrompts = [];
    this.bulkDismissReasonDraft = '';
    this.bulkDismissGoneAgentIds = new Set();
    this.notify();
  };

  setBulkDismissReasonDraft = (value: string): void => {
    this.bulkDismissReasonDraft = clampDismissReason(value);
    this.notify();
  };

  confirmBulkDismiss = async (): Promise<void> => {
    if (this.bulkDismissSubmitting || this.bulkDismissPrompts.length === 0) return;
    const targets = this.bulkDismissPrompts
      .map((prompt) => ({
        prompt,
        decisionIds: promptWaitingOnViewerRows(prompt, this.currentUserId).map((d) => d.rowId),
        idempotencyKey: crypto.randomUUID(),
      }))
      .filter((t) => t.decisionIds.length > 0);
    if (targets.length === 0) {
      this.closeBulkDismissDialog();
      return;
    }

    const reason = this.bulkDismissReasonDraft;
    this.bulkDismissSubmitting = true;
    this.bulkDismissCompleted = 0;
    this.notify();

    try {
      const outcomes = await runWithConcurrencyLimit(
        targets,
        MAX_BULK_DISMISS_CONCURRENCY,
        async (target): Promise<BulkDismissCardOutcome> => {
          const res = await dismissDecisionPrompt(target.prompt.promptMessageId, {
            reason,
            surface: 'decisions_page',
            decisionIds: target.decisionIds,
            idempotencyKey: target.idempotencyKey,
          });
          this.bulkDismissCompleted++;
          if (this.host.isConnected) {
            if (res.ok) {
              const nextSel = new Set(this.selectedPromptIds);
              nextSel.delete(target.prompt.promptMessageId);
              this.selectedPromptIds = nextSel;
              this.applyIncomingDecisionPatch(target.prompt.promptMessageId, [
                ...res.dismissed,
                ...res.alreadyDismissed,
              ]);
            } else if (isOnlyTooRecentDismissError(res)) {
              if (res.availableAt) {
                const parsed = Date.parse(res.availableAt);
                if (Number.isFinite(parsed)) {
                  this.dismissAvailableAtOverrides.set(target.prompt.promptMessageId, parsed);
                }
              }
              const nextSel = new Set(this.selectedPromptIds);
              nextSel.delete(target.prompt.promptMessageId);
              this.selectedPromptIds = nextSel;
              this.scheduleDismissUnlockTimer();
            } else if (res.code === 'dismiss_changed') {
              if (res.decisions) {
                this.applyIncomingDecisionPatch(target.prompt.promptMessageId, res.decisions);
              }
              this.pruneInvalidSelections();
            }
            this.notify();
          }
          return { prompt: target.prompt, result: res };
        }
      );

      if (!this.host.isConnected) return;
      this.bulkDismissPrompts = [];
      this.bulkDismissReasonDraft = '';
      this.bulkDismissGoneAgentIds = new Set();
      this.pruneInvalidSelections();
      const toast = bulkDismissResultToast(outcomes);
      showToast(toast.message, toast.variant);
      this.scheduleLiveRefresh();
    } finally {
      this.bulkDismissSubmitting = false;
      this.bulkDismissCompleted = 0;
      if (this.host.isConnected) {
        this.notify();
      }
    }
  };
}
