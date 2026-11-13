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
 * Project Decisions page (`/projects/:id/decisions`, ptone/scion#7800).
 *
 * Gated on both `web.project_decisions` and `web.chat_choices` (Plan B7):
 * renders `<scion-page-404>` and makes zero `/decisions` requests when either
 * flag is off.
 *
 * Two tabs:
 *  - "Waiting on you" (`pending`): interactive `<scion-chat-choices
 *    surface="decisions_page">` cards per open prompt waiting on the viewer,
 *    even when chat retention purged the original prompt message, plus a
 *    collapsed "Waiting on others" read-only list.
 *  - "Made" (`made`): append-only decision log with combinable Agent,
 *    Answered-by, Provenance, and From/To filters, desktop table / mobile
 *    stacked list, and cursor pagination.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { DecisionView, PageData } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { chatConversationPath } from '../../client/chat-routes.js';
import {
  ANSWERABLE,
  GENERIC_ANSWER_DENIED_REASON,
  actionableReason,
  patchDecisions,
} from '../../client/chat-choices.js';
import type { AskerAnswerability, ChoiceAnsweredDetail } from '../../client/chat-choices.js';
import { navigateTo, replaceSearch } from '../../client/navigation.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import {
  fetchProjectMadeDecisions,
  fetchProjectPendingDecisions,
} from '../../client/project-decisions.js';
import type {
  MadeDecisionsFilter,
  ProjectDecisionActorView,
  ProjectDecisionCounts,
  ProjectDecisionLogEntryView,
  ProjectDecisionPromptView,
} from '../../client/project-decisions.js';
import { stateManager } from '../../client/state.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { FEATURE_FLAGS_CHANGED_EVENT, projectDecisionsEnabled } from '../../utils/feature-flags.js';
import { formatInstant, formatInstantWithZone, formatRelative } from '../../utils/time.js';
import '../shared/chat/chat-choices.js';
import './not-found.js';

interface ProjectSummary {
  id: string;
  name: string;
  slug?: string;
}

interface WaitingOnOthersItem {
  prompt: ProjectDecisionPromptView;
  decision: DecisionView;
  reason: string;
}

const AVATAR_PALETTE = ['#7c3aed', '#059669', '#0284c7', '#d97706', '#e11d48', '#4f46e5'] as const;

function avatarColor(seed: string): string {
  let hash = 0;
  for (let i = 0; i < seed.length; i++) {
    hash = (hash * 31 + seed.charCodeAt(i)) | 0;
  }
  const idx = Math.abs(hash) % AVATAR_PALETTE.length;
  return AVATAR_PALETTE[idx] ?? AVATAR_PALETTE[0];
}

function avatarInitial(name: string): string {
  const ch = (name || '?').trim().charAt(0).toUpperCase();
  return ch || '?';
}

function surfaceDisplay(surface: string): { icon: string; label: string } {
  switch (surface) {
    case 'decisions_page':
      return { icon: 'window', label: 'Decisions page' };
    case 'native':
    case 'web':
      return { icon: 'chat-dots', label: 'Chat' };
    case 'slack':
      return { icon: 'slack', label: 'Slack' };
    case 'google_chat':
      return { icon: 'chat-dots', label: 'Google Chat' };
    case 'discord':
      return { icon: 'discord', label: 'Discord' };
    case 'telegram':
      return { icon: 'telegram', label: 'Telegram' };
    case 'teams':
      return { icon: 'microsoft-teams', label: 'Teams' };
    default:
      return { icon: '', label: surface || 'Other' };
  }
}

@customElement('scion-page-project-decisions')
export class ScionPageProjectDecisions extends LitElement {
  readonly _zone = new DisplayZoneController(this);

  @property({ type: Object })
  pageData: PageData | null = null;

  @property({ type: String })
  projectId = '';

  @state() private enabled = false;
  @state() private currentUserId = '';
  @state() private project: ProjectSummary | null = null;
  @state() private activeTab: 'pending' | 'made' = 'pending';

  @state() private initialLoading = true;
  @state() private error: string | null = null;

  @state() private counts: ProjectDecisionCounts = { waitingOnYou: 0, waitingOnOthers: 0 };
  @state() private pendingPrompts: ProjectDecisionPromptView[] = [];
  @state() private pendingNextCursor = '';
  @state() private pendingLoadingMore = false;

  @state() private madeLoaded = false;
  @state() private madeLoading = false;
  @state() private madeLoadingMore = false;
  @state() private madeError: string | null = null;
  @state() private madeEntries: ProjectDecisionLogEntryView[] = [];
  @state() private madeNextCursor = '';

  @state() private facetsAgents: ProjectDecisionActorView[] = [];
  @state() private facetsAnswerers: ProjectDecisionActorView[] = [];

  @state() private filterAgent = '';
  @state() private filterUser = '';
  @state() private filterProvenance = '';
  @state() private filterFrom = '';
  @state() private filterTo = '';
  @state() private mobileFiltersOpen = false;

  private pendingGen = 0;
  private madeGen = 0;
  private pendingAbort: AbortController | null = null;
  private madeAbort: AbortController | null = null;
  private liveRefreshTimer: ReturnType<typeof setTimeout> | null = null;
  private booted = false;

  static override styles = css`
    :host {
      display: block;
      --chat-fs-sm: 0.875rem;
      --chat-fs-base: 0.9375rem;
      --chat-fs-lg: 1.0625rem;
    }

    .crumbs {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.35rem;
      display: flex;
      align-items: center;
      gap: 0.35rem;
      flex-wrap: wrap;
    }

    .crumbs a {
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
    }

    .crumbs a:hover {
      text-decoration: underline;
    }

    .ph {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      flex-wrap: wrap;
    }

    .ph h1 {
      margin: 0;
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
    }

    .sub {
      color: var(--scion-text-secondary, #475569);
      font-size: 0.875rem;
      margin: 0.25rem 0 1rem;
    }

    sl-tab sl-badge::part(base) {
      font-size: 0.68rem;
      margin-left: 0.35rem;
    }

    sl-tab-panel::part(base) {
      padding: 0.75rem 0 0;
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
      font-size: 2.5rem;
      color: var(--sl-color-danger-500, #ef4444);
      margin-bottom: 0.75rem;
    }

    .error-state h2 {
      font-size: 1.2rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem;
    }

    .error-details {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.85rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.65rem 0.9rem;
      border-radius: var(--scion-radius, 0.5rem);
      color: var(--sl-color-danger-700, #b91c1c);
      margin-bottom: 1rem;
    }

    /* ---- Prompt cards (Mock P3-2) ---- */
    .pcard {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      margin: 0.75rem 0;
    }

    .pcard-head {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.55rem 0.75rem;
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-secondary, #475569);
      flex-wrap: wrap;
    }

    .pcard-head .avatar {
      width: 1.5rem;
      height: 1.5rem;
      border-radius: 50%;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      font-size: 0.72rem;
      font-weight: 700;
      color: #ffffff;
      flex-shrink: 0;
    }

    .pcard-head b {
      color: var(--scion-text, #1e293b);
    }

    .pcard-head .right {
      margin-left: auto;
      display: inline-flex;
      gap: 0.4rem;
      align-items: center;
    }

    .pcard scion-chat-choices {
      margin: 0 0.75rem 0.75rem;
    }

    .linkish {
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
      font-weight: 500;
      cursor: pointer;
      display: inline-flex;
      align-items: center;
      gap: 0.3rem;
    }

    .linkish:hover {
      text-decoration: underline;
    }

    .purged-note {
      display: inline-flex;
      align-items: center;
      gap: 0.3rem;
      color: var(--scion-text-muted, #64748b);
    }

    .muted {
      color: var(--scion-text-muted, #64748b);
    }

    .key {
      font: 600 0.75rem/1 var(--scion-font-mono, monospace);
      background: var(--scion-badge-neutral-bg, #f1f5f9);
      color: var(--scion-badge-neutral-text, #334155);
      padding: 0.2rem 0.4rem;
      border-radius: 0.25rem;
      white-space: nowrap;
    }

    .badge {
      display: inline-flex;
      align-items: center;
      gap: 0.2rem;
      font-size: 0.68rem;
      font-weight: 600;
      line-height: 1;
      padding: 0.2rem 0.45rem;
      border-radius: 999px;
      background: var(--scion-badge-neutral-bg, #f1f5f9);
      color: var(--scion-badge-neutral-text, #334155);
      white-space: nowrap;
    }

    .badge.interp {
      background: var(--scion-badge-warning-bg, #fef3c7);
      color: var(--scion-badge-warning-text, #92400e);
    }

    .badge.conf {
      background: var(--scion-badge-success-bg, #dcfce7);
      color: var(--scion-badge-success-text, #166534);
    }

    .badge.over {
      background: var(--scion-badge-primary-bg, #dbeafe);
      color: var(--scion-badge-primary-text, #1e40af);
    }

    .badge sl-icon {
      font-size: 0.8rem;
    }

    /* ---- Waiting on others (Mock P3-2) ---- */
    sl-details.others {
      margin-top: 1rem;
    }

    sl-details.others::part(base) {
      background: var(--scion-surface, #ffffff);
      border-color: var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    sl-details.others::part(header) {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    /* ---- Stacked list (.mlist: Waiting on others & Mobile Made, Mocks P3-2, P3-5) ---- */
    .mlist .item {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.55rem 0.65rem;
      margin: 0.5rem 0;
      font-size: 0.85rem;
      color: var(--scion-text, #1e293b);
    }

    .mlist .item .top {
      display: flex;
      gap: 0.4rem;
      align-items: center;
      flex-wrap: wrap;
      margin-bottom: 0.15rem;
    }

    .mlist .item .meta {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.78rem;
      margin-top: 0.25rem;
    }

    /* ---- Made toolbar & table (Mocks P3-3, P3-5) ---- */
    .toolbar {
      margin: 0.25rem 0 0.75rem;
    }

    .mobile-filters-row {
      display: none;
      align-items: center;
      gap: 0.5rem;
      margin-bottom: 0.5rem;
    }

    .mobile-filters-row sl-badge::part(base) {
      font-size: 0.65rem;
      margin-left: 0.35rem;
    }

    .filter-controls {
      display: flex;
      gap: 0.5rem;
      flex-wrap: wrap;
      align-items: flex-end;
    }

    .filter-controls sl-select,
    .filter-controls sl-input {
      min-width: 9.5rem;
    }

    table.log {
      width: 100%;
      border-collapse: collapse;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
      font-size: 0.85rem;
      color: var(--scion-text, #1e293b);
    }

    table.log th {
      text-align: left;
      font-weight: 600;
      font-size: 0.75rem;
      color: var(--scion-text-secondary, #475569);
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.5rem 0.6rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      white-space: nowrap;
    }

    table.log td {
      padding: 0.5rem 0.6rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: top;
    }

    table.log tbody tr:last-child td {
      border-bottom: none;
    }

    table.log tr.follow td {
      background: var(--scion-bg, #f8fafc);
    }

    table.log td.q {
      max-width: 18rem;
      font-weight: 400;
    }

    table.log td.q .q-text {
      display: -webkit-box;
      -webkit-line-clamp: 2;
      -webkit-box-orient: vertical;
      overflow: hidden;
      overflow-wrap: anywhere;
    }

    .surf {
      display: inline-flex;
      gap: 0.3rem;
      align-items: center;
      color: var(--scion-text-secondary, #475569);
      font-size: 0.8rem;
      white-space: nowrap;
    }

    .made-mobile-list {
      display: none;
    }

    .more {
      display: flex;
      justify-content: center;
      margin: 0.85rem 0;
    }

    /* ---- Empty states (Mock P3-6) ---- */
    .empty {
      text-align: center;
      padding: 2.5rem 1rem;
      color: var(--scion-text-secondary, #475569);
      background: var(--scion-surface, #ffffff);
      border: 1px dashed var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      margin: 0.5rem 0;
    }

    .empty sl-icon {
      font-size: 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .empty p {
      margin: 0.4rem 0 0;
    }

    .empty p b {
      color: var(--scion-text, #1e293b);
    }

    @media (max-width: 640px) {
      .ph h1 {
        font-size: 1.2rem;
      }

      .mobile-filters-row {
        display: flex;
      }

      .filter-controls {
        display: none;
      }

      .filter-controls.open {
        display: flex;
      }

      .filter-controls sl-select,
      .filter-controls sl-input {
        min-width: calc(50% - 0.25rem);
        flex: 1 1 8.5rem;
      }

      table.log {
        display: none;
      }

      .made-mobile-list {
        display: block;
      }
    }
  `;

  private readonly boundOnFlagsChanged = (): void => {
    const next = projectDecisionsEnabled();
    if (next === this.enabled) return;
    this.enabled = next;
    if (!next) {
      this.teardownActiveLoads();
      this.booted = false;
      return;
    }
    void this.bootIfEnabled();
  };

  private readonly boundOnChoiceUpdated = (e: Event): void => {
    if (!this.enabled || !this.projectId) return;
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
    if (data?.projectId && data.projectId !== this.projectId) return;
    if (data?.decision) {
      this.applyIncomingDecisionPatch(data.messageId, [data.decision]);
    }
    this.scheduleLiveRefresh();
  };

  private readonly boundOnChatMessageReceived = (e: Event): void => {
    if (!this.enabled || !this.projectId) return;
    const raw = (e as CustomEvent).detail as {
      data?: { projectId?: string; type?: string; decisions?: unknown };
      projectId?: string;
      type?: string;
      decisions?: unknown;
    } | null;
    const data = raw?.data ?? raw;
    if (data?.projectId && data.projectId !== this.projectId) return;
    if (data?.type !== 'choice-prompt' && !Array.isArray(data?.decisions)) return;
    this.scheduleLiveRefresh();
  };

  override connectedCallback(): void {
    super.connectedCallback();
    this.extractRouteState();
    this.enabled = projectDecisionsEnabled();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFlagsChanged);
    stateManager.addEventListener('choice-updated', this.boundOnChoiceUpdated);
    stateManager.addEventListener('chat-message-received', this.boundOnChatMessageReceived);
    void this.bootIfEnabled();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFlagsChanged);
    stateManager.removeEventListener('choice-updated', this.boundOnChoiceUpdated);
    stateManager.removeEventListener('chat-message-received', this.boundOnChatMessageReceived);
    this.teardownActiveLoads();
  }

  private teardownActiveLoads(): void {
    if (this.liveRefreshTimer !== null) {
      clearTimeout(this.liveRefreshTimer);
      this.liveRefreshTimer = null;
    }
    this.pendingAbort?.abort();
    this.pendingAbort = null;
    this.madeAbort?.abort();
    this.madeAbort = null;
  }

  private extractRouteState(): void {
    const rawPath = this.pageData?.path || window.location.pathname + window.location.search;
    const [pathname = '', query = ''] = rawPath.split('?', 2);
    if (!this.projectId) {
      const match = pathname.match(/\/projects\/([^/]+)\/decisions/);
      if (match?.[1]) {
        this.projectId = decodeURIComponent(match[1]);
      }
    }
    if (this.pageData?.user?.id) {
      this.currentUserId = this.pageData.user.id;
    }
    const search = query ? `?${query}` : window.location.search;
    const params = new URLSearchParams(search);
    this.activeTab = params.get('tab') === 'made' ? 'made' : 'pending';
  }

  private async bootIfEnabled(): Promise<void> {
    if (!this.enabled || this.booted) return;
    this.extractRouteState();
    if (!this.projectId) {
      this.error = 'No project ID found in URL';
      this.initialLoading = false;
      return;
    }
    this.booted = true;
    this.initialLoading = true;
    this.error = null;

    if (this.currentUserId) {
      stateManager.setCurrentUserId(this.currentUserId);
    }
    stateManager.setScope({ type: 'project', projectId: this.projectId });

    const userTask = this.currentUserId ? Promise.resolve() : this.loadCurrentUserBestEffort();
    const projectTask = this.loadProjectSummary();
    const pendingTask = this.loadPendingPage({ reset: true, silent: false });
    const madeTask =
      this.activeTab === 'made'
        ? this.loadMadePage({ reset: true, silent: false })
        : Promise.resolve();

    await Promise.all([userTask, projectTask, pendingTask, madeTask]);
    if (this.isConnected) {
      this.initialLoading = false;
    }
  }

  private async loadCurrentUserBestEffort(): Promise<void> {
    try {
      const res = await apiFetch('/auth/me', { authErrors: 'caller' });
      if (!res.ok || !this.isConnected) return;
      const body = (await res.json()) as { id?: string; user?: { id?: string } } | null;
      const uid = body?.id || body?.user?.id || '';
      if (uid && !this.currentUserId) {
        this.currentUserId = uid;
        stateManager.setCurrentUserId(uid);
        if (this.enabled && this.projectId) {
          stateManager.setScope({ type: 'project', projectId: this.projectId });
        }
      }
    } catch {
      // Best-effort fallback when pageData.user was not injected.
    }
  }

  private async loadProjectSummary(): Promise<void> {
    try {
      const res = await apiFetch(`/api/v1/projects/${encodeURIComponent(this.projectId)}`);
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      if (!this.isConnected) return;
      this.project = (await res.json()) as ProjectSummary;
      dispatchPageTitle(this, 'Decisions', this.project.name || this.projectId);
    } catch (err) {
      if (!this.isConnected) return;
      this.error = err instanceof Error ? err.message : 'Failed to load project';
    }
  }

  private mergeFacets(
    agents: ProjectDecisionActorView[],
    answerers: ProjectDecisionActorView[]
  ): void {
    if (agents.length > 0 || this.facetsAgents.length === 0) {
      this.facetsAgents = agents;
    }
    if (answerers.length > 0 || this.facetsAnswerers.length === 0) {
      this.facetsAnswerers = answerers;
    }
  }

  private async loadPendingPage(opts: { reset: boolean; silent: boolean }): Promise<void> {
    if (!this.enabled || !this.projectId) return;
    const gen = ++this.pendingGen;
    this.pendingAbort?.abort();
    const ac = new AbortController();
    this.pendingAbort = ac;

    const cursor = opts.reset ? undefined : this.pendingNextCursor;
    if (!opts.reset) {
      this.pendingLoadingMore = true;
    }

    try {
      const res = await fetchProjectPendingDecisions(this.projectId, {
        cursor,
        signal: ac.signal,
      });
      if (gen !== this.pendingGen || !this.isConnected) return;
      this.counts = res.counts;
      this.pendingPrompts = opts.reset ? res.prompts : [...this.pendingPrompts, ...res.prompts];
      this.pendingNextCursor = res.nextCursor ?? '';
      this.mergeFacets(res.agents, res.answerers);
      if (!opts.silent) {
        this.error = null;
      }
    } catch (err) {
      if (ac.signal.aborted || gen !== this.pendingGen || !this.isConnected) return;
      if (!opts.silent) {
        this.error = err instanceof Error ? err.message : 'Failed to load decisions';
      }
    } finally {
      if (gen === this.pendingGen) {
        this.pendingLoadingMore = false;
        if (this.pendingAbort === ac) this.pendingAbort = null;
      }
    }
  }

  private currentMadeFilter(): MadeDecisionsFilter {
    return {
      agent: this.filterAgent,
      user: this.filterUser,
      provenance: this.filterProvenance,
      from: this.filterFrom,
      to: this.filterTo,
    };
  }

  private get activeFilterCount(): number {
    let n = 0;
    if (this.filterAgent) n++;
    if (this.filterUser) n++;
    if (this.filterProvenance) n++;
    if (this.filterFrom) n++;
    if (this.filterTo) n++;
    return n;
  }

  private async loadMadePage(opts: { reset: boolean; silent: boolean }): Promise<void> {
    if (!this.enabled || !this.projectId) return;
    const gen = ++this.madeGen;
    this.madeAbort?.abort();
    const ac = new AbortController();
    this.madeAbort = ac;

    const cursor = opts.reset ? undefined : this.madeNextCursor;
    if (opts.reset && !opts.silent) {
      this.madeLoading = true;
      this.madeError = null;
    } else if (!opts.reset) {
      this.madeLoadingMore = true;
      this.madeError = null;
    }

    try {
      const res = await fetchProjectMadeDecisions(this.projectId, this.currentMadeFilter(), {
        cursor,
        signal: ac.signal,
      });
      if (gen !== this.madeGen || !this.isConnected) return;
      this.counts = res.counts;
      this.madeEntries = opts.reset ? res.entries : [...this.madeEntries, ...res.entries];
      this.madeNextCursor = res.nextCursor ?? '';
      this.madeLoaded = true;
      this.mergeFacets(res.agents, res.answerers);
      this.madeError = null;
    } catch (err) {
      if (ac.signal.aborted || gen !== this.madeGen || !this.isConnected) return;
      if (!opts.silent) {
        this.madeError = err instanceof Error ? err.message : 'Failed to load decision log';
      }
    } finally {
      if (gen === this.madeGen) {
        this.madeLoading = false;
        this.madeLoadingMore = false;
        if (this.madeAbort === ac) this.madeAbort = null;
      }
    }
  }

  private applyIncomingDecisionPatch(
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
      if (deltaWaitingOnYou !== 0) {
        this.counts = {
          ...this.counts,
          waitingOnYou: Math.max(0, this.counts.waitingOnYou + deltaWaitingOnYou),
        };
      }
    }
  }

  private scheduleLiveRefresh(): void {
    if (this.liveRefreshTimer !== null) {
      clearTimeout(this.liveRefreshTimer);
    }
    this.liveRefreshTimer = setTimeout(() => {
      this.liveRefreshTimer = null;
      if (!this.isConnected || !this.enabled || !this.projectId) return;
      void this.loadPendingPage({ reset: true, silent: true });
      if (this.madeLoaded || this.activeTab === 'made') {
        void this.loadMadePage({ reset: true, silent: true });
      }
    }, 60);
  }

  private switchTab(next: 'pending' | 'made'): void {
    if (next !== 'pending' && next !== 'made') return;
    const changed = this.activeTab !== next;
    this.activeTab = next;

    const params = new URLSearchParams(window.location.search);
    if (next === 'made') {
      params.set('tab', 'made');
    } else {
      params.delete('tab');
    }
    replaceSearch(params);

    const group = this.shadowRoot?.querySelector('sl-tab-group') as
      | (HTMLElement & { show?: (panel: string) => void })
      | null;
    if (changed && typeof group?.show === 'function') {
      group.show(next);
    }

    if (next === 'made' && !this.madeLoaded && !this.madeLoading) {
      void this.loadMadePage({ reset: true, silent: false });
    }
  }

  private onTabShow(e: Event): void {
    if (e.target !== e.currentTarget) return;
    const name = (e as CustomEvent<{ name?: string }>).detail?.name;
    if (name === 'pending' || name === 'made') {
      this.switchTab(name);
    }
  }

  private onPromptChoiceAnswered(
    prompt: ProjectDecisionPromptView,
    e: CustomEvent<ChoiceAnsweredDetail>
  ): void {
    this.applyIncomingDecisionPatch(
      e.detail.messageId || prompt.promptMessageId,
      e.detail.decisions
    );
    this.scheduleLiveRefresh();
  }

  private onPromptMessageJump(
    prompt: ProjectDecisionPromptView,
    e: CustomEvent<{ messageId?: string }>
  ): void {
    if (!prompt.conversationAvailable) return;
    const targetMsgId = e.detail?.messageId?.trim();
    if (!targetMsgId) return;
    const base = chatConversationPath({
      conversationKey: prompt.chatKey,
      projectId: this.projectId,
    });
    if (!base) return;
    navigateTo(`${base}#msg-${encodeURIComponent(targetMsgId)}`);
  }

  private selectValueOf(e: Event): string {
    const target = e.currentTarget as (HTMLElement & { value?: string }) | null;
    const raw = (target?.value ?? '').trim();
    return raw === 'all' ? '' : raw;
  }

  private onFilterChanged(): void {
    this.madeNextCursor = '';
    void this.loadMadePage({ reset: true, silent: false });
  }

  private clearFilters(): void {
    this.filterAgent = '';
    this.filterUser = '';
    this.filterProvenance = '';
    this.filterFrom = '';
    this.filterTo = '';
    this.onFilterChanged();
  }

  private retryAll(): void {
    this.booted = false;
    void this.bootIfEnabled();
  }

  private promptAnswerability(p: ProjectDecisionPromptView): AskerAnswerability {
    if (p.canAnswer) return ANSWERABLE;
    return {
      state: 'blocked',
      reason: p.disabledReason || GENERIC_ANSWER_DENIED_REASON,
    };
  }

  private conversationDisplayLabel(chatKey: string, conversationLabel: string): string {
    if (chatKey.startsWith('dm:')) return 'your DM';
    return conversationLabel || 'chat';
  }

  private conversationHref(chatKey: string, available: boolean): string | null {
    if (!available) return null;
    return chatConversationPath({ conversationKey: chatKey, projectId: this.projectId });
  }

  private formatLogWhen(iso: string): string {
    const today = formatInstant(new Date().toISOString(), 'date');
    const rowDate = formatInstant(iso, 'date');
    if (today && rowDate === today) {
      return formatInstant(iso, 'time');
    }
    return formatInstant(iso, 'datetime');
  }

  private waitingOnOthersItems(): WaitingOnOthersItem[] {
    const items: WaitingOnOthersItem[] = [];
    for (const p of this.pendingPrompts) {
      for (const d of p.decisions) {
        const reason = p.waitingReasons?.[d.rowId];
        if (reason) {
          items.push({ prompt: p, decision: d, reason });
        } else if (
          !p.waitingOnYou &&
          d.status !== 'replaced' &&
          !(d.status === 'decided' && d.decision?.provenance !== 'agent_interpreted')
        ) {
          items.push({ prompt: p, decision: d, reason: 'other_user_confirm' });
        }
      }
    }
    return items;
  }

  override render(): TemplateResult {
    if (!this.enabled) {
      return html`<scion-page-404 .pageData=${this.pageData}></scion-page-404>`;
    }

    if (this.initialLoading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading decisions...</p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load Decisions</h2>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${() => this.retryAll()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    const projectName = this.project?.name || this.project?.slug || this.projectId;

    return html`
      <nav class="crumbs" aria-label="Breadcrumb">
        <a href="/projects">Projects</a>
        <span aria-hidden="true">/</span>
        <a href="/projects/${encodeURIComponent(this.projectId)}">${projectName}</a>
        <span aria-hidden="true">/</span>
        <span>Decisions</span>
      </nav>

      <div class="ph">
        <h1>Decisions</h1>
      </div>
      <p class="sub">Choices agents asked in this project's conversations you can read.</p>

      <sl-tab-group @sl-tab-show=${(e: Event) => this.onTabShow(e)}>
        <sl-tab
          slot="nav"
          panel="pending"
          data-tab="pending"
          ?active=${this.activeTab === 'pending'}
          @click=${() => this.switchTab('pending')}
        >
          Waiting on you
          ${this.counts.waitingOnYou > 0
            ? html`<sl-badge variant="primary" pill data-waiting-badge
                >${this.counts.waitingOnYou}</sl-badge
              >`
            : nothing}
        </sl-tab>
        <sl-tab
          slot="nav"
          panel="made"
          data-tab="made"
          ?active=${this.activeTab === 'made'}
          @click=${() => this.switchTab('made')}
        >
          Made
        </sl-tab>

        <sl-tab-panel name="pending" ?active=${this.activeTab === 'pending'}>
          ${this.renderPendingPanel()}
        </sl-tab-panel>
        <sl-tab-panel name="made" ?active=${this.activeTab === 'made'}>
          ${this.renderMadePanel()}
        </sl-tab-panel>
      </sl-tab-group>
    `;
  }

  private renderPendingPanel(): TemplateResult {
    const waitingPrompts = this.pendingPrompts.filter((p) => p.waitingOnYou);
    const othersItems = this.waitingOnOthersItems();
    const othersCount = Math.max(this.counts.waitingOnOthers, othersItems.length);

    return html`
      ${waitingPrompts.length === 0
        ? html`
            <div class="empty" data-empty="pending">
              <sl-icon name="check2-circle"></sl-icon>
              <p><b>Nothing is waiting on you</b></p>
              <p>
                When an agent asks you to choose, it shows up here.
                <a
                  class="linkish"
                  href="?tab=made"
                  data-see-made
                  @click=${(e: Event) => {
                    e.preventDefault();
                    this.switchTab('made');
                  }}
                  >See decisions made</a
                >
              </p>
            </div>
          `
        : waitingPrompts.map((p) => this.renderPromptCard(p))}
      ${this.pendingNextCursor
        ? html`
            <div class="more">
              <sl-button
                size="small"
                data-load-more="pending"
                ?loading=${this.pendingLoadingMore}
                ?disabled=${this.pendingLoadingMore}
                @click=${() => void this.loadPendingPage({ reset: false, silent: false })}
              >
                Load more
              </sl-button>
            </div>
          `
        : nothing}
      ${othersItems.length > 0
        ? html`
            <sl-details
              class="others"
              data-waiting-others
              summary=${`Waiting on others (${othersCount})`}
            >
              <div class="mlist">
                ${othersItems.map((item) => this.renderWaitingOnOthersItem(item))}
              </div>
            </sl-details>
          `
        : nothing}
    `;
  }

  private renderPromptCard(p: ProjectDecisionPromptView): TemplateResult {
    const whereLabel = this.conversationDisplayLabel(p.chatKey, p.conversationLabel);
    const convHref = this.conversationHref(p.chatKey, p.conversationAvailable);
    const permalink =
      p.promptAvailable && convHref
        ? `${convHref}#msg-${encodeURIComponent(p.promptMessageId)}`
        : null;
    const mode = p.answerMode === 'set' || p.answerMode === 'single' ? p.answerMode : undefined;

    return html`
      <div
        class="pcard"
        data-prompt-card=${p.promptMessageId}
        @choice-answered=${(e: CustomEvent<ChoiceAnsweredDetail>) =>
          this.onPromptChoiceAnswered(p, e)}
        @scroll-to-message=${(e: CustomEvent<{ messageId?: string }>) =>
          this.onPromptMessageJump(p, e)}
        @choice-jump=${(e: CustomEvent<{ messageId?: string }>) => this.onPromptMessageJump(p, e)}
      >
        <div class="pcard-head">
          <span
            class="avatar"
            style="background: ${avatarColor(p.askerAgentId || p.askerAgentName)}"
            >${avatarInitial(p.askerAgentName)}</span
          >
          <b>${p.askerAgentName}</b>
          <span>
            in
            ${convHref
              ? html`<a class="linkish" data-conv-link href=${convHref}>${whereLabel}</a>`
              : html`<span class="muted" data-conv-unavailable>${whereLabel}</span>`}
          </span>
          <span class="muted" title=${formatInstantWithZone(p.createdAt)}
            >· ${formatRelative(p.createdAt, { style: 'short' })}</span
          >
          <span class="right">
            ${permalink
              ? html`<a class="linkish" data-open-in-chat href=${permalink}
                  ><sl-icon name="chat-left-text" aria-hidden="true"></sl-icon> Open in chat</a
                >`
              : !p.promptAvailable
                ? html`<span class="purged-note" data-purged-note
                    ><sl-icon name="archive" aria-hidden="true"></sl-icon>
                    <span>Original message no longer available</span></span
                  >`
                : nothing}
          </span>
        </div>
        <scion-chat-choices
          .messageId=${p.promptMessageId}
          .decisions=${p.decisions}
          .currentUserId=${this.currentUserId}
          .answerMode=${mode}
          .answerability=${this.promptAnswerability(p)}
          surface="decisions_page"
        ></scion-chat-choices>
      </div>
    `;
  }

  private renderWaitingOnOthersItem(item: WaitingOnOthersItem): TemplateResult {
    const { prompt: p, decision: d, reason } = item;
    const whereLabel = this.conversationDisplayLabel(p.chatKey, p.conversationLabel);
    const convHref = this.conversationHref(p.chatKey, p.conversationAvailable);
    const permalink =
      p.promptAvailable && convHref
        ? `${convHref}#msg-${encodeURIComponent(p.promptMessageId)}`
        : null;

    let reasonBadge: TemplateResult;
    if (reason === 'other_user_confirm') {
      const owner = d.decision?.answeredByName || 'Another user';
      reasonBadge = html`<span class="badge interp"
        ><sl-icon name="robot" aria-hidden="true"></sl-icon>${owner} to confirm</span
      >`;
    } else if (reason === 'read_only') {
      reasonBadge = html`<span class="badge">read-only for you</span>`;
    } else if (reason === 'message_denied') {
      reasonBadge = html`<span class="badge">messaging restricted</span>`;
    } else {
      reasonBadge = html`<span class="badge">${reason}</span>`;
    }

    return html`
      <div class="item" data-others-row=${d.rowId}>
        <div class="top">
          ${d.key ? html`<span class="key">${d.key}</span>` : nothing}
          <b>${d.question}</b>
          ${reasonBadge}
        </div>
        <div class="meta">
          ${p.askerAgentName} · ${whereLabel} · ${formatRelative(p.createdAt, { style: 'short' })}
          ${permalink
            ? html` · <a class="linkish" href=${permalink}>Open in chat</a>`
            : !p.promptAvailable
              ? html` · Original message no longer available`
              : nothing}
        </div>
      </div>
    `;
  }

  private renderMadePanel(): TemplateResult {
    const filterCount = this.activeFilterCount;

    return html`
      <div class="toolbar">
        <div class="mobile-filters-row">
          <sl-button
            size="small"
            data-mobile-filters-toggle
            @click=${() => {
              this.mobileFiltersOpen = !this.mobileFiltersOpen;
            }}
          >
            <sl-icon slot="prefix" name="funnel"></sl-icon>
            Filters
            ${filterCount > 0
              ? html`<sl-badge variant="primary" pill>${filterCount}</sl-badge>`
              : nothing}
          </sl-button>
          ${filterCount > 0
            ? html`<sl-button size="small" variant="text" @click=${() => this.clearFilters()}
                >Clear</sl-button
              >`
            : nothing}
        </div>

        <div class="filter-controls ${this.mobileFiltersOpen ? 'open' : ''}">
          <sl-select
            size="small"
            label="Agent"
            data-filter="agent"
            .value=${this.filterAgent || 'all'}
            @sl-change=${(e: Event) => {
              this.filterAgent = this.selectValueOf(e);
              this.onFilterChanged();
            }}
          >
            <sl-option value="all">All agents</sl-option>
            ${this.facetsAgents.map((a) => html`<sl-option value=${a.id}>${a.name}</sl-option>`)}
          </sl-select>

          <sl-select
            size="small"
            label="Answered by"
            data-filter="user"
            .value=${this.filterUser || 'all'}
            @sl-change=${(e: Event) => {
              this.filterUser = this.selectValueOf(e);
              this.onFilterChanged();
            }}
          >
            <sl-option value="all">Anyone</sl-option>
            ${this.currentUserId
              ? html`<sl-option value=${this.currentUserId}>You</sl-option>`
              : nothing}
            ${this.facetsAnswerers
              .filter((u) => u.id && u.id !== this.currentUserId)
              .map((u) => html`<sl-option value=${u.id}>${u.name}</sl-option>`)}
          </sl-select>

          <sl-select
            size="small"
            label="Provenance"
            data-filter="provenance"
            .value=${this.filterProvenance || 'all'}
            @sl-change=${(e: Event) => {
              this.filterProvenance = this.selectValueOf(e);
              this.onFilterChanged();
            }}
          >
            <sl-option value="all">Any</sl-option>
            <sl-option value="direct">Direct</sl-option>
            <sl-option value="interpreted">Interpreted</sl-option>
            <sl-option value="confirmed">Confirmed</sl-option>
            <sl-option value="overridden">Changed</sl-option>
          </sl-select>

          <sl-input
            size="small"
            type="date"
            label="From"
            data-filter="from"
            .value=${this.filterFrom}
            @sl-change=${(e: Event) => {
              this.filterFrom = (
                (e.currentTarget as { value?: string } | null)?.value ?? ''
              ).trim();
              this.onFilterChanged();
            }}
          ></sl-input>

          <sl-input
            size="small"
            type="date"
            label="To"
            data-filter="to"
            .value=${this.filterTo}
            @sl-change=${(e: Event) => {
              this.filterTo = ((e.currentTarget as { value?: string } | null)?.value ?? '').trim();
              this.onFilterChanged();
            }}
          ></sl-input>

          ${filterCount > 0
            ? html`<sl-button
                size="small"
                variant="text"
                data-clear-filters
                @click=${() => this.clearFilters()}
                >Clear filters</sl-button
              >`
            : nothing}
        </div>
      </div>

      ${this.madeLoading
        ? html`
            <div class="loading-state">
              <sl-spinner></sl-spinner>
              <p>Loading decision log...</p>
            </div>
          `
        : this.madeError
          ? html`
              <div class="error-state">
                <sl-icon name="exclamation-triangle"></sl-icon>
                <h2>Failed to Load Decision Log</h2>
                <div class="error-details">${this.madeError}</div>
                <sl-button
                  variant="primary"
                  @click=${() => void this.loadMadePage({ reset: true, silent: false })}
                >
                  Retry
                </sl-button>
              </div>
            `
          : this.madeEntries.length === 0
            ? html`
                <div class="empty" data-empty="made">
                  <sl-icon name="journal-text"></sl-icon>
                  <p>
                    <b
                      >${filterCount > 0
                        ? 'No decisions match the current filters'
                        : 'No decisions recorded yet'}</b
                    >
                  </p>
                  <p>
                    ${filterCount > 0
                      ? 'Try adjusting or clearing the filters above.'
                      : "Answers to agents' choices in this project's conversations appear here."}
                  </p>
                </div>
              `
            : html`
                <table class="log" data-made-table>
                  <thead>
                    <tr>
                      <th>When</th>
                      <th>Decision</th>
                      <th>Question</th>
                      <th>Answer</th>
                      <th>Answered by</th>
                      <th>Provenance</th>
                      <th>Surface</th>
                      <th>Agent · where</th>
                    </tr>
                  </thead>
                  <tbody>
                    ${this.madeEntries.map((e) => this.renderMadeRow(e))}
                  </tbody>
                </table>

                <div class="mlist made-mobile-list" data-made-mobile-list>
                  ${this.madeEntries.map((e) => this.renderMadeMobileItem(e))}
                </div>

                ${this.madeNextCursor
                  ? html`
                      <div class="more">
                        <sl-button
                          size="small"
                          data-load-more="made"
                          ?loading=${this.madeLoadingMore}
                          ?disabled=${this.madeLoadingMore}
                          @click=${() => void this.loadMadePage({ reset: false, silent: false })}
                        >
                          Load more
                        </sl-button>
                      </div>
                    `
                  : nothing}
              `}
    `;
  }

  private renderAnsweredBy(e: ProjectDecisionLogEntryView): TemplateResult | string {
    if (this.currentUserId && e.answeredByUserId === this.currentUserId) {
      return 'you';
    }
    if (!e.answeredByUserId) {
      return html`${e.answeredByName || 'Unknown'} <span class="muted">(deleted user)</span>`;
    }
    return e.answeredByName || 'Unknown';
  }

  private renderProvenanceBadge(e: ProjectDecisionLogEntryView): TemplateResult {
    if (e.provenance === 'agent_interpreted') {
      return html`<span class="badge interp"
        ><sl-icon name="robot" aria-hidden="true"></sl-icon>interpreted</span
      >`;
    }
    if (e.provenance === 'direct' && e.outcome === 'confirm') {
      return html`<span class="badge conf"
        ><sl-icon name="patch-check" aria-hidden="true"></sl-icon>confirmed</span
      >`;
    }
    if (e.provenance === 'direct' && e.outcome === 'override') {
      return html`<span class="badge over"
        ><sl-icon name="arrow-left-right" aria-hidden="true"></sl-icon>changed</span
      >`;
    }
    return html`<span class="badge">direct</span>`;
  }

  private replyHrefForEntry(e: ProjectDecisionLogEntryView): string | null {
    if (!e.sourceMessageId || !e.sourceMessageAvailable || !e.conversationAvailable) {
      return null;
    }
    const convHref = this.conversationHref(e.chatKey, e.conversationAvailable);
    if (!convHref) return null;
    return `${convHref}#msg-${encodeURIComponent(e.sourceMessageId)}`;
  }

  private renderMadeRow(e: ProjectDecisionLogEntryView): TemplateResult {
    const isFollow = e.outcome === 'confirm' || e.outcome === 'override';
    const surf = surfaceDisplay(e.surface);
    const whereLabel = this.conversationDisplayLabel(e.chatKey, e.conversationLabel);
    const convHref = this.conversationHref(e.chatKey, e.conversationAvailable);
    const replyHref = this.replyHrefForEntry(e);

    return html`
      <tr class=${isFollow ? 'follow' : ''} data-answer-row=${e.answerId}>
        <td>
          <time datetime=${e.at} title=${formatInstantWithZone(e.at)}
            >${this.formatLogWhen(e.at)}</time
          >
        </td>
        <td><span class="key">${e.decision.key || e.decision.ref}</span> v${e.decision.version}</td>
        <td class="q">
          <span class="q-text" title=${e.decision.question}>${e.decision.question}</span>
        </td>
        <td><b>${e.label || e.choiceId}</b></td>
        <td data-answered-by>${this.renderAnsweredBy(e)}</td>
        <td data-provenance>
          ${this.renderProvenanceBadge(e)}
          ${e.provenance === 'agent_interpreted' && replyHref
            ? html` <a class="linkish" data-reply-link href=${replyHref}>reply</a>`
            : e.provenance === 'direct' && e.outcome === 'override' && e.previousChoiceLabel
              ? html` <span class="muted">from ${e.previousChoiceLabel}</span>`
              : e.provenance === 'direct' && e.outcome === 'answer' && e.matchedDefault
                ? html` <span class="muted">recommended</span>`
                : nothing}
        </td>
        <td data-surface>
          <span class="surf"
            >${surf.icon
              ? html`<sl-icon name=${surf.icon} aria-hidden="true"></sl-icon>`
              : nothing}${surf.label}</span
          >
        </td>
        <td data-agent-where>
          ${e.decision.askerAgentName} ·
          ${convHref
            ? html`<a class="linkish" data-conv-link href=${convHref}>${whereLabel}</a>`
            : html`<span class="muted" data-conv-unavailable>${whereLabel}</span>`}
        </td>
      </tr>
    `;
  }

  private renderMadeMobileItem(e: ProjectDecisionLogEntryView): TemplateResult {
    const surf = surfaceDisplay(e.surface);
    const whereLabel = this.conversationDisplayLabel(e.chatKey, e.conversationLabel);
    const convHref = this.conversationHref(e.chatKey, e.conversationAvailable);
    const replyHref = this.replyHrefForEntry(e);

    return html`
      <div class="item" data-mobile-answer=${e.answerId}>
        <div class="top">
          <span class="key">${e.decision.key || e.decision.ref}</span>
          <b>${e.label || e.choiceId}</b>
          ${this.renderProvenanceBadge(e)}
        </div>
        <div>${e.decision.question}</div>
        <div class="meta">
          ${this.renderAnsweredBy(e)}
          ${e.outcome === 'override' && e.previousChoiceLabel
            ? html` · from ${e.previousChoiceLabel}`
            : nothing}
          · ${surf.label} · ${e.decision.askerAgentName} ·
          ${convHref
            ? html`<a class="linkish" href=${convHref}>${whereLabel}</a>`
            : html`<span class="muted">${whereLabel}</span>`}
          ·
          <time datetime=${e.at} title=${formatInstantWithZone(e.at)}
            >${this.formatLogWhen(e.at)}</time
          >
          ${replyHref ? html` · <a class="linkish" href=${replyHref}>reply</a>` : nothing}
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-project-decisions': ScionPageProjectDecisions;
  }
}
