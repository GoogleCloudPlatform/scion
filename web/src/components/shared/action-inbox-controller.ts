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
 * Generic Lit `ReactiveController` for message-action inbox pages
 * (design §5.5, AC-12).
 *
 * Owns tab, filters, sort, cursor paging (including fragment merging by card ID),
 * URL query sync via `replaceSearch`, debounced live refresh on
 * `message-action-updated` filtered by `kind`, and loading/error/404 states.
 */

import type { ReactiveController, ReactiveControllerHost, TemplateResult } from 'lit';
import {
  ActionInboxApiError,
  buildActionInboxUrlSearch,
  fetchActionInbox,
  parseActionInboxUrlState,
  type ActionInboxCounts,
  type ActionInboxFacets,
  type ActionInboxSort,
  type ActionInboxTab,
} from '../../client/action-inbox.js';
import { replaceSearch } from '../../client/navigation.js';
import { stateManager } from '../../client/state.js';
import { FEATURE_FLAGS_CHANGED_EVENT } from '../../utils/feature-flags.js';
import type { InboxTabId } from './inbox-tabs.js';

export const ACTION_INBOX_LIVE_DEBOUNCE_MS = 750;
export const ACTION_INBOX_SEARCH_DEBOUNCE_MS = 250;

export interface ActionInboxKindConfig<TCard> {
  /** Message-action kind name used to filter `message-action-updated` SSE events. */
  kind: string;
  /** Entry in `INBOX_TABS` highlighted in the Inbox tab strip. */
  tabId: InboxTabId;
  /** Hub endpoint path (e.g. `'/api/v1/users/me/<kind-path>'`). */
  apiPath: string;
  /** Page heading and document title. */
  title: string;
  /** Italic descriptor shown under the page heading. */
  descriptor: string;
  /** Optional placeholder text for the search input (defaults to a neutral prompt). */
  searchPlaceholder?: string;
  /** Returns whether the kind's feature gate is enabled. */
  isEnabled: () => boolean;
  /** Stable identifier for a card (used for keyed rendering and fragment merging). */
  getCardId: (card: TCard) => string;
  /**
   * Optional fragment merger invoked when a paginated load returns a card whose
   * ID is already present (e.g. multi-row cards split across Resolved pages).
   */
  mergeCard?: (existing: TCard, incoming: TCard) => TCard;
  /** Renders one card; `ctx.onResolved()` triggers a refresh after a user action. */
  renderCard: (card: TCard, ctx: { onResolved: () => void }) => TemplateResult;
}

export class ActionInboxPageController<TCard> implements ReactiveController {
  private readonly host: ReactiveControllerHost;
  private config: ActionInboxKindConfig<TCard> | null = null;
  private userId = '';

  tab: ActionInboxTab = 'open';
  project = '';
  agent = '';
  q = '';
  sort: ActionInboxSort = 'recent';

  cards: TCard[] = [];
  counts: ActionInboxCounts = { answerable: 0, open: 0 };
  filteredCounts: ActionInboxCounts | null = null;
  facets: ActionInboxFacets = { agents: [], projects: [] };
  truncatedProjects = false;
  capped = false;
  nextCursor = '';

  loading = false;
  loadingMore = false;
  hasNewActivity = false;
  error: string | null = null;
  errorStatus = 0;
  unavailable404 = false;

  private connected = false;
  private abortController: AbortController | null = null;
  private requestSeq = 0;
  private pagesLoaded = 0;
  private liveRefreshTimer: ReturnType<typeof setTimeout> | null = null;
  private searchTimer: ReturnType<typeof setTimeout> | null = null;

  private readonly onMessageActionUpdated = (e: Event): void => {
    if (!this.config || !this.config.isEnabled() || this.unavailable404) return;
    const raw = (e as CustomEvent).detail as
      | ({ data?: { kind?: unknown } } & { kind?: unknown })
      | undefined;
    const data = raw?.data ?? raw;
    if (data?.kind !== this.config.kind) return;
    if (this.pagesLoaded > 1) {
      if (!this.hasNewActivity) {
        this.hasNewActivity = true;
        this.host.requestUpdate();
      }
      return;
    }
    this.scheduleLiveRefresh();
  };

  private readonly onFeatureFlagsChanged = (): void => {
    this.host.requestUpdate();
    if (!this.config) return;
    if (
      this.config.isEnabled() &&
      !this.loading &&
      this.cards.length === 0 &&
      !this.unavailable404
    ) {
      this.ensureProfileScope();
      void this.load();
    }
  };

  constructor(host: ReactiveControllerHost, config: ActionInboxKindConfig<TCard> | null = null) {
    this.host = host;
    this.config = config;
    host.addController(this);
  }

  setConfig(config: ActionInboxKindConfig<TCard> | null): void {
    const prevKind = this.config?.kind ?? '';
    const nextKind = config?.kind ?? '';
    this.config = config;
    if (this.connected && nextKind && nextKind !== prevKind) {
      this.unavailable404 = false;
      this.cards = [];
      this.nextCursor = '';
      if (config?.isEnabled()) {
        this.ensureProfileScope();
        void this.load();
      }
    }
  }

  setUserId(userId: string): void {
    this.userId = userId;
    if (this.connected && this.config?.isEnabled()) {
      this.ensureProfileScope();
    }
  }

  get hasActiveFilters(): boolean {
    return Boolean(this.project || this.agent || this.q);
  }

  hostConnected(): void {
    this.connected = true;
    this.hydrateFromUrl();

    stateManager.addEventListener('message-action-updated', this.onMessageActionUpdated);
    if (typeof window !== 'undefined') {
      window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFeatureFlagsChanged);
    }

    if (this.config?.isEnabled()) {
      this.ensureProfileScope();
      void this.load();
    }
  }

  hostDisconnected(): void {
    this.connected = false;
    this.abortController?.abort();
    this.abortController = null;
    this.clearTimers();

    stateManager.removeEventListener('message-action-updated', this.onMessageActionUpdated);
    if (typeof window !== 'undefined') {
      window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFeatureFlagsChanged);
    }
  }

  private ensureProfileScope(): void {
    const uid = this.userId || stateManager.getCurrentUserId();
    if (uid) {
      stateManager.setCurrentUserId(uid);
    }
    if (typeof EventSource !== 'undefined') {
      stateManager.setScope(uid ? { type: 'profile', userId: uid } : { type: 'profile' });
    }
  }

  private hydrateFromUrl(): void {
    if (typeof window === 'undefined') return;
    const parsed = parseActionInboxUrlState(window.location.search);
    this.tab = parsed.tab;
    this.project = parsed.project;
    this.agent = parsed.agent;
    this.q = parsed.q;
    this.sort = parsed.sort;
  }

  private syncUrl(): void {
    if (typeof window === 'undefined') return;
    const qs = buildActionInboxUrlSearch({
      tab: this.tab,
      project: this.project,
      agent: this.agent,
      q: this.q,
      sort: this.sort,
    });
    replaceSearch(qs);
  }

  private clearTimers(): void {
    if (this.liveRefreshTimer !== null) {
      clearTimeout(this.liveRefreshTimer);
      this.liveRefreshTimer = null;
    }
    if (this.searchTimer !== null) {
      clearTimeout(this.searchTimer);
      this.searchTimer = null;
    }
  }

  private scheduleLiveRefresh(): void {
    if (this.liveRefreshTimer !== null) {
      clearTimeout(this.liveRefreshTimer);
    }
    this.liveRefreshTimer = setTimeout(() => {
      this.liveRefreshTimer = null;
      if (this.connected && this.config?.isEnabled()) {
        void this.load({ background: true });
      }
    }, ACTION_INBOX_LIVE_DEBOUNCE_MS);
  }

  setTab(tab: ActionInboxTab): void {
    if (this.tab === tab) return;
    this.tab = tab;
    this.nextCursor = '';
    this.syncUrl();
    void this.load();
  }

  setProject(project: string): void {
    if (this.project === project) return;
    this.project = project;
    this.nextCursor = '';
    this.syncUrl();
    void this.load();
  }

  setAgent(agent: string): void {
    if (this.agent === agent) return;
    this.agent = agent;
    this.nextCursor = '';
    this.syncUrl();
    void this.load();
  }

  setSort(sort: ActionInboxSort): void {
    if (this.sort === sort) return;
    this.sort = sort;
    this.nextCursor = '';
    this.syncUrl();
    void this.load();
  }

  setQuery(q: string, opts?: { immediate?: boolean }): void {
    if (this.q === q) return;
    this.q = q;
    this.nextCursor = '';
    this.syncUrl();
    this.host.requestUpdate();

    if (this.searchTimer !== null) {
      clearTimeout(this.searchTimer);
      this.searchTimer = null;
    }
    if (opts?.immediate) {
      void this.load();
      return;
    }
    this.searchTimer = setTimeout(() => {
      this.searchTimer = null;
      if (this.connected) {
        void this.load();
      }
    }, ACTION_INBOX_SEARCH_DEBOUNCE_MS);
  }

  clearFilters(): void {
    if (!this.hasActiveFilters) return;
    this.project = '';
    this.agent = '';
    this.q = '';
    this.nextCursor = '';
    if (this.searchTimer !== null) {
      clearTimeout(this.searchTimer);
      this.searchTimer = null;
    }
    this.syncUrl();
    void this.load();
  }

  refresh(): void {
    if (this.liveRefreshTimer !== null) {
      clearTimeout(this.liveRefreshTimer);
      this.liveRefreshTimer = null;
    }
    void this.load({ background: this.cards.length > 0 });
  }

  /**
   * Called after the user resolves a card in place. On page 1, refreshes the
   * list immediately; when multiple pages are loaded, preserves the mounted
   * pages and surfaces the refresh affordance so in-progress inputs on other
   * cards are not discarded.
   */
  notifyCardResolved(): void {
    if (this.pagesLoaded > 1) {
      if (!this.hasNewActivity) {
        this.hasNewActivity = true;
        this.host.requestUpdate();
      }
      return;
    }
    this.refresh();
  }

  async load(opts?: { background?: boolean }): Promise<void> {
    const config = this.config;
    if (!config || !config.isEnabled()) return;

    this.abortController?.abort();
    const controller = new AbortController();
    this.abortController = controller;
    const seq = ++this.requestSeq;
    // Clear any in-flight Load more state superseded by this full load.
    this.loadingMore = false;
    this.hasNewActivity = false;

    if (!opts?.background) {
      this.loading = true;
      this.error = null;
      this.errorStatus = 0;
      this.host.requestUpdate();
    } else {
      this.host.requestUpdate();
    }

    try {
      const res = await fetchActionInbox<TCard>(
        config.apiPath,
        {
          tab: this.tab,
          project: this.project,
          agent: this.agent,
          q: this.q,
          sort: this.sort,
        },
        { signal: controller.signal, kind: config.kind }
      );
      if (seq !== this.requestSeq || !this.connected) return;

      this.unavailable404 = false;
      this.counts = res.counts;
      this.filteredCounts = res.filteredCounts ?? null;
      this.cards = res.cards;
      this.pagesLoaded = 1;
      this.facets = res.facets;
      this.truncatedProjects = res.truncatedProjects;
      this.capped = res.capped;
      this.nextCursor = res.nextCursor ?? '';
      this.error = null;
      this.errorStatus = 0;
    } catch (err) {
      if (controller.signal.aborted || seq !== this.requestSeq || !this.connected) return;
      if (err instanceof ActionInboxApiError) {
        if (err.status === 404) {
          this.unavailable404 = true;
          this.cards = [];
          this.error = null;
          this.errorStatus = 404;
        } else {
          this.error = err.message;
          this.errorStatus = err.status;
        }
      } else {
        this.error = err instanceof Error ? err.message : 'Failed to load requests.';
        this.errorStatus = 0;
      }
    } finally {
      if (seq === this.requestSeq && this.connected) {
        this.loading = false;
        this.host.requestUpdate();
      }
    }
  }

  async loadMore(): Promise<void> {
    const config = this.config;
    if (!config || !config.isEnabled() || !this.nextCursor || this.loadingMore || this.loading) {
      return;
    }

    const controller = new AbortController();
    this.abortController = controller;
    const seq = ++this.requestSeq;
    this.loadingMore = true;
    this.error = null;
    this.host.requestUpdate();

    try {
      const res = await fetchActionInbox<TCard>(
        config.apiPath,
        {
          tab: this.tab,
          project: this.project,
          agent: this.agent,
          q: this.q,
          sort: this.sort,
          cursor: this.nextCursor,
        },
        { signal: controller.signal, kind: config.kind }
      );
      if (seq !== this.requestSeq || !this.connected) return;

      this.counts = res.counts;
      this.filteredCounts = res.filteredCounts ?? this.filteredCounts;
      this.cards = this.mergeIncomingCards(config, this.cards, res.cards);
      this.pagesLoaded = Math.max(1, this.pagesLoaded) + 1;
      this.truncatedProjects = this.truncatedProjects || res.truncatedProjects;
      this.capped = this.capped || res.capped;
      this.nextCursor = res.nextCursor ?? '';
    } catch (err) {
      if (controller.signal.aborted || seq !== this.requestSeq || !this.connected) return;
      if (err instanceof ActionInboxApiError) {
        this.error = err.message;
        this.errorStatus = err.status;
      } else {
        this.error = err instanceof Error ? err.message : 'Failed to load more requests.';
        this.errorStatus = 0;
      }
    } finally {
      if (seq === this.requestSeq && this.connected) {
        this.loadingMore = false;
        this.host.requestUpdate();
      }
    }
  }

  private mergeIncomingCards(
    config: ActionInboxKindConfig<TCard>,
    existing: readonly TCard[],
    incoming: readonly TCard[]
  ): TCard[] {
    if (incoming.length === 0) return [...existing];
    const next = [...existing];
    const indexById = new Map<string, number>();
    next.forEach((card, idx) => {
      indexById.set(config.getCardId(card), idx);
    });

    for (const card of incoming) {
      const id = config.getCardId(card);
      const existingIdx = indexById.get(id);
      if (existingIdx !== undefined) {
        next[existingIdx] = config.mergeCard ? config.mergeCard(next[existingIdx], card) : card;
      } else {
        indexById.set(id, next.length);
        next.push(card);
      }
    }
    return next;
  }
}
