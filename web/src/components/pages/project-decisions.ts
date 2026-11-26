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

import { LitElement, html, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { repeat } from 'lit/directives/repeat.js';

import type { PageData } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { current, projectTrail, projectsRoot, type Named } from '../../client/crumbs.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { projectDisplayName, projectLabelNow } from '../../client/project-label.js';
import '../shared/breadcrumb.js';
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
import {
  DecisionsPageController,
  isValidDecisionsDialectQuery,
} from '../shared/decisions-page-controller.js';
import {
  decisionsPageSharedStyles,
  renderBulkDismissDialog,
  renderDateRangeError,
  renderDecisionsErrorState,
  renderDecisionsLoadingState,
  renderLoadMoreFooter,
  renderMadeEmptyState,
  renderMadeTableAndMobileList,
  renderNoMatchState,
  renderPendingEmptyState,
  renderPendingSelectionHeader,
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
        validateQuery: isValidDecisionsDialectQuery,
      }),
    fetchMade: ({ cursor, limit, filter, signal }): Promise<ProjectMadeDecisionsResponse> =>
      fetchProjectMadeDecisions(this.projectId, filter, {
        cursor,
        limit,
        signal,
        validateQuery: isValidDecisionsDialectQuery,
      }),
  });

  static override styles = [decisionsPageSharedStyles];

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
        @decisions-query-commit=${(e: CustomEvent<{ q: string }>): void =>
          this.ctrl.applyDialectQueryCommit(e.detail.q)}
        @decisions-filter-change=${(e: CustomEvent<DecisionsFilterPatch>): void =>
          this.ctrl.applyFilterPatch(e.detail)}
        @decisions-filters-clear=${this.ctrl.clearFilters}
      ></scion-decisions-filter-panel>
    `;
  }

  private projectNamed(allowPending: boolean): Named | null {
    if (!this.projectId) return null;
    if (this.project) {
      return {
        id: this.projectId,
        label: projectDisplayName(this.project, this.projectId),
      };
    }
    const cached = projectLabelNow(this.projectId);
    return allowPending ? cached : { id: cached.id, label: cached.label };
  }

  private ancestorCrumbs(allowPending: boolean) {
    const project = this.projectNamed(allowPending);
    return project ? projectTrail(project) : [projectsRoot()];
  }

  override render(): TemplateResult {
    if (!this.ctrl.enabled) {
      return html`<scion-page-404 .pageData=${this.pageData}></scion-page-404>`;
    }

    if (this.ctrl.initialLoading) {
      return html`
        <scion-breadcrumb .crumbs=${this.ancestorCrumbs(true)}></scion-breadcrumb>
        ${renderDecisionsLoadingState()}
      `;
    }

    if (this.ctrl.error) {
      return html`
        <scion-breadcrumb .crumbs=${this.ancestorCrumbs(false)}></scion-breadcrumb>
        ${renderDecisionsErrorState({
          heading: 'Failed to Load Decisions',
          message: this.ctrl.error,
          retryAttr: 'data-retry-pending',
          showPrefixIcon: true,
          onRetry: this.ctrl.retryAll,
        })}
      `;
    }

    const crumbs = [...this.ancestorCrumbs(false), current('Decisions', false)];

    return html`
      <scion-breadcrumb .crumbs=${crumbs}></scion-breadcrumb>

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
      ${renderBulkDismissDialog({
        prompts: this.ctrl.bulkDismissPrompts,
        currentUserId: this.ctrl.currentUserId,
        reasonDraft: this.ctrl.bulkDismissReasonDraft,
        submitting: this.ctrl.bulkDismissSubmitting,
        completedCount: this.ctrl.bulkDismissCompleted,
        goneAgentIds: this.ctrl.bulkDismissGoneAgentIds,
        onReasonInput: this.ctrl.setBulkDismissReasonDraft,
        onCancel: this.ctrl.closeBulkDismissDialog,
        onConfirm: () => void this.ctrl.confirmBulkDismiss(),
      })}
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
        : html`
            ${renderPendingSelectionHeader({
              selectedCount: this.ctrl.selectedPromptIds.size,
              selectableCount: this.ctrl.selectableWaitingPrompts().length,
              submitting: this.ctrl.bulkDismissSubmitting,
              onToggleAll: this.ctrl.toggleSelectAllPrompts,
              onClear: this.ctrl.clearPromptSelection,
              onDismiss: this.ctrl.openBulkDismissDialog,
            })}
            ${repeat(
              waitingPrompts,
              (p) => p.promptMessageId,
              (p) =>
                renderPromptCard(p, {
                  currentUserId: this.ctrl.currentUserId,
                  defaultProjectId: this.projectId,
                  askerGone: this.ctrl.isPromptAskerGone(p),
                  dismissRemainingMin: this.ctrl.promptDismissRemainingMinutes(p),
                  dismissAvailableAtMs: this.ctrl.promptDismissAvailableAtMs(p),
                  selected: this.ctrl.isPromptSelected(p.promptMessageId),
                  canSelect: this.ctrl.promptCanDismiss(p),
                  selectionCapReached: this.ctrl.isPromptSelectionCapReached(p.promptMessageId),
                  onToggleSelect: this.ctrl.setPromptSelected,
                  onDismissTooRecent: this.ctrl.onPromptDismissTooRecent,
                  onAnswered: this.ctrl.onPromptDecisionAnswered,
                  onJump: this.ctrl.onPromptMessageJump,
                })
            )}
          `}
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
                    disclosure: this.ctrl.madeDisclosure,
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
