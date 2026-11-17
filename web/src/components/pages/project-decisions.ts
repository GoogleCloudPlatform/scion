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
 * Project Decisions page (`/projects/:id/decisions`, ptone/scion#7800, R7 shared tree ptone/scion#7996).
 *
 * Gated on `web.decisions`: renders `<scion-page-404>` and makes zero
 * `/decisions` requests when the flag is off.
 *
 * Shares its load/paging controller, format helpers, styles, and row/card/state
 * renderers with `<scion-page-profile-decisions>` (`scope="project"` vs `scope="profile"`).
 */

import { LitElement, css, html, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { repeat } from 'lit/directives/repeat.js';

import type { PageData } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import {
  fetchProjectMadeDecisions,
  fetchProjectPendingDecisions,
  type ProjectMadeDecisionsResponse,
  type ProjectPendingDecisionsResponse,
} from '../../client/project-decisions.js';
import { stateManager } from '../../client/state.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import type {
  DecisionsFilterPatch,
  DecisionsSearchInputDetail,
} from '../shared/decisions-filter-panel.js';
import '../shared/decisions-filter-panel.js';
import { collectWaitingOnOthersItems } from '../shared/decisions-format.js';
import { DecisionsPageController } from '../shared/decisions-page-controller.js';
import {
  decisionsPageSharedStyles,
  renderDateRangeError,
  renderDecisionsErrorState,
  renderDecisionsLoadingState,
  renderLoadMoreFooter,
  renderMadeEmptyState,
  renderMadeTableAndMobileList,
  renderNoMatchState,
  renderPendingEmptyState,
  renderPromptCard,
  renderSearchTimeoutAlert,
  renderWaitingOnOthersSection,
  renderWaitingTabBadge,
} from '../shared/decisions-views.js';
import './not-found.js';

interface ProjectSummary {
  id: string;
  name: string;
  slug?: string;
}

@customElement('scion-page-project-decisions')
export class ScionPageProjectDecisions extends LitElement {
  readonly _zone = new DisplayZoneController(this);

  @property({ type: Object })
  pageData: PageData | null = null;

  @property({ type: String })
  projectId = '';

  @state() private project: ProjectSummary | null = null;

  private readonly ctrl = new DecisionsPageController(this, {
    scope: 'project',
    getPageData: (): PageData | null => this.pageData,
    extractRouteParams: (pathname: string): { decodeError?: string | undefined } | undefined => {
      if (!this.projectId) {
        const match = pathname.match(/\/projects\/([^/]+)\/decisions/);
        if (match?.[1]) {
          try {
            this.projectId = decodeURIComponent(match[1]);
          } catch {
            return { decodeError: 'Invalid project ID in URL' };
          }
        }
      }
      return undefined;
    },
    validateBoot: (): string | null => (!this.projectId ? 'No project ID found in URL' : null),
    canOperate: (): boolean => Boolean(this.projectId),
    configureStateScope: (): void => {
      stateManager.setScope({ type: 'project', projectId: this.projectId });
    },
    onBoot: (): Promise<void> => this.loadProjectSummary(),
    matchesProject: (projectId): boolean => !projectId || projectId === this.projectId,
    resolvePromptProjectId: (): string => this.projectId,
    fetchPending: ({ cursor, limit, filter, signal }): Promise<ProjectPendingDecisionsResponse> =>
      fetchProjectPendingDecisions(this.projectId, {
        cursor,
        limit,
        filter,
        signal,
      }),
    fetchMade: ({ cursor, limit, filter, signal }): Promise<ProjectMadeDecisionsResponse> =>
      fetchProjectMadeDecisions(this.projectId, filter, {
        cursor,
        limit,
        signal,
      }),
  });

  static override styles = [
    decisionsPageSharedStyles,
    css`
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
    `,
  ];

  private async loadProjectSummary(): Promise<void> {
    try {
      const res = await apiFetch(`/api/v1/projects/${encodeURIComponent(this.projectId)}`);
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      if (!this.isConnected) return;
      const project = (await res.json()) as ProjectSummary | null;
      this.project = project;
      dispatchPageTitle(this, 'Decisions', project?.name || this.projectId);
    } catch (err) {
      if (!this.isConnected) return;
      this.ctrl.setError(err instanceof Error ? err.message : 'Failed to load project');
    }
  }

  private renderFilterPanel(): TemplateResult {
    return html`
      <scion-decisions-filter-panel
        scope="project"
        .supportsSearch=${this.ctrl.supportsSearch}
        .tab=${this.ctrl.activeTab}
        .q=${this.ctrl.filterQ}
        .searchText=${this.ctrl.searchDraft}
        .conversation=${this.ctrl.filterConversation}
        .agent=${this.ctrl.filterAgent}
        .user=${this.ctrl.filterUser}
        .provenance=${this.ctrl.filterProvenance}
        .from=${this.ctrl.filterFrom}
        .to=${this.ctrl.filterTo}
        .currentUserId=${this.ctrl.currentUserId}
        .conversations=${this.ctrl.facetsConversations}
        .agents=${this.ctrl.facetsAgents}
        .answerers=${this.ctrl.facetsAnswerers}
        @decisions-search-input=${(e: CustomEvent<DecisionsSearchInputDetail>): void =>
          this.ctrl.onSearchInput(e.detail)}
        @decisions-filter-change=${(e: CustomEvent<DecisionsFilterPatch>): void =>
          this.ctrl.applyFilterPatch(e.detail)}
        @decisions-filters-clear=${this.ctrl.clearFilters}
      ></scion-decisions-filter-panel>
    `;
  }

  override render(): TemplateResult {
    if (!this.ctrl.enabled) {
      return html`<scion-page-404 .pageData=${this.pageData}></scion-page-404>`;
    }

    if (this.ctrl.initialLoading) {
      return renderDecisionsLoadingState();
    }

    if (this.ctrl.error) {
      return renderDecisionsErrorState({
        heading: 'Failed to Load Decisions',
        message: this.ctrl.error,
        retryAttr: 'data-retry-pending',
        showPrefixIcon: true,
        onRetry: this.ctrl.retryAll,
      });
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
      <p class="sub">Decisions agents asked in this project's conversations you can read.</p>

      <sl-tab-group @sl-tab-show=${(e: Event): void => this.ctrl.onTabShow(e)}>
        <sl-tab
          slot="nav"
          panel="pending"
          data-tab="pending"
          ?active=${this.ctrl.activeTab === 'pending'}
          @click=${(): void => this.ctrl.switchTab('pending')}
        >
          Waiting on you
          ${renderWaitingTabBadge({
            activeTab: this.ctrl.activeTab,
            isPendingFiltered: this.ctrl.isPendingFiltered,
            counts: this.ctrl.counts,
            filteredCounts: this.ctrl.filteredCounts,
          })}
        </sl-tab>
        <sl-tab
          slot="nav"
          panel="made"
          data-tab="made"
          ?active=${this.ctrl.activeTab === 'made'}
          @click=${(): void => this.ctrl.switchTab('made')}
        >
          Made
        </sl-tab>

        <sl-tab-panel name="pending" ?active=${this.ctrl.activeTab === 'pending'}>
          ${this.ctrl.activeTab === 'pending' ? this.renderFilterPanel() : nothing}
          ${this.renderPendingPanel()}
        </sl-tab-panel>
        <sl-tab-panel name="made" ?active=${this.ctrl.activeTab === 'made'}>
          ${this.ctrl.activeTab === 'made' ? this.renderFilterPanel() : nothing}
          ${this.renderMadePanel()}
        </sl-tab-panel>
      </sl-tab-group>
    `;
  }

  private renderPendingPanel(): TemplateResult {
    if (this.ctrl.pendingSearchTimeout) {
      return renderSearchTimeoutAlert('project');
    }

    const waitingPrompts = this.ctrl.pendingPrompts.filter((p) => p.waitingOnYou);
    const othersItems = collectWaitingOnOthersItems(this.ctrl.pendingPrompts);
    const isFiltered = this.ctrl.isPendingFiltered;

    return html`
      ${waitingPrompts.length === 0
        ? isFiltered
          ? renderNoMatchState('pending', this.ctrl.clearFilters)
          : renderPendingEmptyState({
              scope: 'project',
              seeMadeHref: '?tab=made',
              onSeeMade: () => this.ctrl.switchTab('made'),
            })
        : repeat(
            waitingPrompts,
            (p) => p.promptMessageId,
            (p) =>
              renderPromptCard(p, {
                currentUserId: this.ctrl.currentUserId,
                defaultProjectId: this.projectId,
                onAnswered: this.ctrl.onPromptDecisionAnswered,
                onJump: this.ctrl.onPromptMessageJump,
              })
          )}
      ${renderLoadMoreFooter({
        tab: 'pending',
        nextCursor: this.ctrl.pendingNextCursor,
        loadingMore: this.ctrl.pendingLoadingMore,
        onLoadMore: () => void this.ctrl.loadPendingPage({ mode: 'append' }),
      })}
      ${renderWaitingOnOthersSection({
        items: othersItems,
        counts: this.ctrl.counts,
        filteredCounts: this.ctrl.filteredCounts,
        isFiltered,
        hasWaitingPrompts: waitingPrompts.length > 0,
        defaultProjectId: this.projectId,
      })}
    `;
  }

  private renderMadePanel(): TemplateResult {
    const isFiltered = this.ctrl.isMadeFiltered;

    return html`
      ${this.ctrl.hasDateRangeError ? renderDateRangeError() : nothing}
      ${this.ctrl.madeLoading
        ? renderDecisionsLoadingState('Loading decision log...')
        : this.ctrl.madeSearchTimeout
          ? renderSearchTimeoutAlert('project')
          : this.ctrl.madeError
            ? renderDecisionsErrorState({
                heading: 'Failed to Load Decision Log',
                message: this.ctrl.madeError,
                retryAttr: 'data-retry-made',
                onRetry: () => void this.ctrl.loadMadePage({ mode: 'reset' }),
              })
            : this.ctrl.madeEntries.length === 0
              ? isFiltered
                ? renderNoMatchState('made', this.ctrl.clearFilters)
                : renderMadeEmptyState('project')
              : html`
                  ${renderMadeTableAndMobileList(this.ctrl.madeEntries, {
                    currentUserId: this.ctrl.currentUserId,
                    showProjectColumn: false,
                    defaultProjectId: this.projectId,
                  })}
                  ${renderLoadMoreFooter({
                    tab: 'made',
                    nextCursor: this.ctrl.madeNextCursor,
                    loadingMore: this.ctrl.madeLoadingMore,
                    showEndFilteredNotice: isFiltered,
                    onLoadMore: () => void this.ctrl.loadMadePage({ mode: 'append' }),
                  })}
                `}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-project-decisions': ScionPageProjectDecisions;
  }
}
