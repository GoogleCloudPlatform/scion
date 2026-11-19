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
 * Profile Decisions page (`/profile/decisions`, ptone/scion#7996, Design §4.3 R1–R7, D5, D10, D13, D14, §5.1, §5.3, §7).
 *
 * Gated on `web.decisions` (and falls back to `<scion-page-404>` when the Hub
 * returns 404 on `/api/v1/users/me/decisions`).
 *
 * Shares its load/paging controller, format helpers, styles, and row/card/state
 * renderers with `<scion-page-project-decisions>` (`scope="profile"` vs `scope="project"`).
 */

import { LitElement, css, html, nothing, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import { repeat } from 'lit/directives/repeat.js';

import type { PageData } from '../../shared/types.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import type {
  ProjectDecisionActorView,
  ProjectDecisionConversationView,
  ProjectDecisionProjectRef,
  ProjectDecisionPromptView,
} from '../../client/project-decisions.js';
import {
  buildProfileDecisionsUrlSearch,
  dispatchProfileDecisionsCountChanged,
  dispatchProfileDecisionsNotFound,
  fetchProfileMadeDecisions,
  fetchProfilePendingDecisions,
  type ProfileDecisionMadeGroupView,
  type ProfileMadeDecisionsResponse,
  type ProfilePendingDecisionsResponse,
} from '../../client/profile-decisions.js';
import { stateManager } from '../../client/state.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import type {
  DecisionsFilterPatch,
  DecisionsFilterTag,
  DecisionsSearchInputDetail,
} from '../shared/decisions-filter-panel.js';
import '../shared/decisions-filter-panel.js';
import {
  collectWaitingOnOthersItems,
  countActionableInPrompt,
} from '../shared/decisions-format.js';
import { DecisionsPageController } from '../shared/decisions-page-controller.js';
import {
  decisionsPageSharedStyles,
  renderDateRangeError,
  renderDecisionsErrorState,
  renderDecisionsLoadingState,
  renderLoadMoreFooter,
  renderMadeEmptyState,
  renderMadeItem,
  renderMadeTableAndMobileList,
  renderNoMatchState,
  renderPendingEmptyState,
  renderPromptCard,
  renderSearchTimeoutAlert,
  renderWaitingOnOthersSection,
  renderWaitingTabBadge,
} from '../shared/decisions-views.js';
import type { FacetChangeDetail, FacetComboboxOption } from '../shared/facet-combobox.js';
import './not-found.js';

interface PendingProjectGroup {
  project: ProjectDecisionProjectRef;
  prompts: ProjectDecisionPromptView[];
  decisionCount: number;
}

@customElement('scion-page-profile-decisions')
export class ScionPageProfileDecisions extends LitElement {
  readonly _zone = new DisplayZoneController(this);

  @property({ type: Object })
  pageData: PageData | null = null;

  private readonly ctrl = new DecisionsPageController(this, {
    scope: 'profile',
    getPageData: (): PageData | null => this.pageData,
    canOperate: (): boolean => true,
    configureStateScope: (): void => {
      const uid = this.pageData?.user?.id || stateManager.getCurrentUserId();
      stateManager.setScope(uid ? { type: 'profile', userId: uid } : { type: 'profile' });
    },
    onBoot: (): Promise<void> => {
      dispatchPageTitle(this, 'Decisions', 'Profile');
      return Promise.resolve();
    },
    resolvePromptProjectId: (prompt): string => prompt.project?.id || '',
    onCountsUpdated: (counts): void => {
      dispatchProfileDecisionsCountChanged(counts.waitingOnYou);
    },
    onNotFound: (): void => {
      dispatchProfileDecisionsNotFound();
    },
    fetchPending: ({
      sort,
      cursor,
      limit,
      filter,
      signal,
    }): Promise<ProfilePendingDecisionsResponse> =>
      fetchProfilePendingDecisions({
        sort,
        cursor,
        limit,
        filter,
        signal,
      }),
    fetchMade: ({ sort, cursor, limit, filter, signal }): Promise<ProfileMadeDecisionsResponse> =>
      fetchProfileMadeDecisions(filter, {
        sort,
        cursor,
        limit,
        signal,
      }),
  });

  static override styles = [
    decisionsPageSharedStyles,
    css`
      .sort-bar {
        display: flex;
        align-items: center;
        gap: 0.6rem;
        flex-wrap: wrap;
        margin: 0 0 0.25rem;
      }

      .sort-bar sl-radio-group::part(form-control-label) {
        position: absolute;
        width: 1px;
        height: 1px;
        padding: 0;
        margin: -1px;
        overflow: hidden;
        clip: rect(0 0 0 0);
        clip-path: inset(50%);
        white-space: nowrap;
        border: 0;
      }

      .sort-bar sl-radio-button::part(button) {
        background: var(--scion-surface, #ffffff);
        color: var(--scion-text, #1e293b);
        border-color: var(--scion-border, #e2e8f0);
      }

      .sort-bar sl-radio-button[checked]::part(button) {
        background: var(--scion-primary, #3b82f6);
        color: #ffffff;
        border-color: var(--scion-primary, #3b82f6);
      }

      scion-decisions-filter-panel > scion-facet-combobox[slot='leading-controls'] {
        flex: 1 1 11rem;
        min-width: 10rem;
      }

      sl-alert[data-truncated-projects] {
        display: block;
        margin: 0.75rem 0;
      }

      sl-badge[variant='neutral']::part(base) {
        background: var(--scion-badge-neutral-bg, #f1f5f9);
        color: var(--scion-badge-neutral-text, #334155);
        border-color: var(--scion-border, #e2e8f0);
      }

      .groupsec {
        margin: 1rem 0 0.35rem;
        display: flex;
        align-items: center;
        gap: 0.5rem;
        flex-wrap: wrap;
      }

      .groupsec h3 {
        margin: 0;
        font-size: 0.95rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
      }

      .groupsec .right {
        margin-left: auto;
        font-size: 0.8125rem;
      }

      @media (max-width: 760px) {
        scion-decisions-filter-panel > scion-facet-combobox[slot='leading-controls'] {
          flex: 1 1 auto;
          width: 100%;
          min-width: 0;
        }
      }
    `,
  ];

  private readonly resolveProjectName = (ref: ProjectDecisionProjectRef | undefined): string =>
    this.ctrl.projectDisplayName(ref);

  private get projectFacetOptions(): FacetComboboxOption[] {
    return this.ctrl.facetsProjects.map((p) => ({
      value: p.id,
      label: p.name || p.slug || p.id,
    }));
  }

  private get projectExtraTags(): DecisionsFilterTag[] {
    if (!this.ctrl.filterProject) return [];
    const name = this.ctrl.projectDisplayName(undefined, this.ctrl.filterProject);
    return [{ key: 'project', label: `Project: ${name}` }];
  }

  /** D5: Conversation facet options show their project as the muted combobox suffix. */
  private get profileConversationFacets(): ProjectDecisionConversationView[] {
    return this.ctrl.facetsConversations.map((c) => {
      const projName = c.projectId ? this.ctrl.projectDisplayName(undefined, c.projectId) : '';
      if (!projName) return c;
      const hint = c.hint
        ? c.hint.includes(projName)
          ? c.hint
          : `${projName} · ${c.hint}`
        : projName;
      return { ...c, hint };
    });
  }

  /** D5: Agent facet options show their project from `agents[].projectId` when present, falling back gracefully when absent. */
  private get profileAgentFacets(): ProjectDecisionActorView[] {
    return this.ctrl.facetsAgents.map((a) => {
      const projName = a.projectId ? this.ctrl.projectDisplayName(undefined, a.projectId) : '';
      if (!projName) return a;
      const hint = a.hint
        ? a.hint.includes(projName)
          ? a.hint
          : `${projName} · ${a.hint}`
        : projName;
      return { ...a, hint };
    });
  }

  /** Groups `waitingPrompts` by project preserving the Hub's `sort=project` rank order (R4). */
  private groupWaitingPromptsByProject(
    waitingPrompts: readonly ProjectDecisionPromptView[]
  ): PendingProjectGroup[] {
    const groups: PendingProjectGroup[] = [];
    const byProject = new Map<string, PendingProjectGroup>();
    for (const p of waitingPrompts) {
      const pid = p.project?.id || 'unknown';
      let group = byProject.get(pid);
      if (!group) {
        group = {
          project: p.project ?? { id: pid, name: pid, slug: pid },
          prompts: [],
          decisionCount: 0,
        };
        byProject.set(pid, group);
        groups.push(group);
      }
      group.prompts.push(p);
      group.decisionCount += countActionableInPrompt(p, this.ctrl.currentUserId);
    }
    return groups;
  }

  private onSortChange(e: Event): void {
    const val = (e.currentTarget as HTMLElement & { value?: string })?.value;
    if (val === 'project' || val === 'recent') {
      this.ctrl.setSort(val);
    }
  }

  private renderSortBar(): TemplateResult {
    return html`
      <div class="sort-bar" data-sort-bar>
        <sl-radio-group
          size="small"
          data-sort-switch
          label="Sort decisions"
          .value=${this.ctrl.sort}
          @sl-change=${(e: Event): void => this.onSortChange(e)}
        >
          <sl-radio-button
            value="project"
            data-sort-option="project"
            ?checked=${this.ctrl.sort === 'project'}
            @click=${(): void => this.ctrl.setSort('project')}
          >
            By project
          </sl-radio-button>
          <sl-radio-button
            value="recent"
            data-sort-option="recent"
            ?checked=${this.ctrl.sort === 'recent'}
            @click=${(): void => this.ctrl.setSort('recent')}
          >
            Most recent
          </sl-radio-button>
        </sl-radio-group>
      </div>
    `;
  }

  private renderFilterPanel(): TemplateResult {
    return html`
      <scion-decisions-filter-panel
        scope="profile"
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
        .extraTags=${this.projectExtraTags}
        .currentUserId=${this.ctrl.currentUserId}
        .conversations=${this.profileConversationFacets}
        .agents=${this.profileAgentFacets}
        .answerers=${this.ctrl.facetsAnswerers}
        @decisions-search-input=${(e: CustomEvent<DecisionsSearchInputDetail>): void =>
          this.ctrl.onSearchInput(e.detail)}
        @decisions-filter-change=${(e: CustomEvent<DecisionsFilterPatch>): void =>
          this.ctrl.applyFilterPatch(e.detail)}
        @decisions-filters-clear=${this.ctrl.clearFilters}
      >
        <scion-facet-combobox
          slot="leading-controls"
          data-filter="project"
          label="Project"
          placeholder="Any"
          .options=${this.projectFacetOptions}
          .value=${this.ctrl.filterProject}
          @facet-change=${(e: CustomEvent<FacetChangeDetail>): void =>
            this.ctrl.applyFilterPatch({ project: e.detail.value })}
        ></scion-facet-combobox>
      </scion-decisions-filter-panel>
    `;
  }

  private renderTruncatedBanner(): TemplateResult | typeof nothing {
    if (!this.ctrl.truncatedProjects || this.ctrl.filterProject) return nothing;
    return html`
      <sl-alert variant="primary" open data-truncated-projects>
        <sl-icon slot="icon" name="info-circle"></sl-icon>
        Showing your 200 most recently active projects; pick a project to see others
      </sl-alert>
    `;
  }

  override render(): TemplateResult {
    if (!this.ctrl.enabled || this.ctrl.hubNotFound) {
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

    return html`
      <div class="ph">
        <h1>Decisions</h1>
      </div>
      <p class="sub">Decisions agents asked in conversations you can read, across your projects.</p>

      ${this.renderTruncatedBanner()}

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
          ${this.ctrl.activeTab === 'pending'
            ? html`${this.renderSortBar()}${this.renderFilterPanel()}`
            : nothing}
          ${this.renderPendingPanel()}
        </sl-tab-panel>
        <sl-tab-panel name="made" ?active=${this.ctrl.activeTab === 'made'}>
          ${this.ctrl.activeTab === 'made'
            ? html`${this.renderSortBar()}${this.renderFilterPanel()}`
            : nothing}
          ${this.renderMadePanel()}
        </sl-tab-panel>
      </sl-tab-group>
    `;
  }

  private renderPendingPanel(): TemplateResult {
    if (this.ctrl.pendingLoading) {
      return renderDecisionsLoadingState();
    }

    if (this.ctrl.pendingSearchTimeout) {
      return renderSearchTimeoutAlert('profile');
    }

    const waitingPrompts = this.ctrl.pendingPrompts.filter((p) => p.waitingOnYou);
    const othersItems = collectWaitingOnOthersItems(this.ctrl.pendingPrompts);
    const isFiltered = this.ctrl.isPendingFiltered;

    return html`
      ${waitingPrompts.length === 0
        ? isFiltered
          ? renderNoMatchState('pending', this.ctrl.clearFilters)
          : renderPendingEmptyState({
              scope: 'profile',
              seeMadeHref: '?tab=made&sort=recent',
              onSeeMade: () => this.ctrl.switchTab('made'),
            })
        : this.ctrl.sort === 'project'
          ? repeat(
              this.groupWaitingPromptsByProject(waitingPrompts),
              (g) => g.project.id,
              (g) => this.renderPendingProjectGroup(g)
            )
          : repeat(
              waitingPrompts,
              (p) => p.promptMessageId,
              (p) =>
                renderPromptCard(p, {
                  currentUserId: this.ctrl.currentUserId,
                  projectBadgeName: this.ctrl.projectDisplayName(p.project),
                  askerGone: this.ctrl.isPromptAskerGone(p),
                  onDismissTooRecent: this.ctrl.onPromptDismissTooRecent,
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
        resolveProjectName: this.resolveProjectName,
      })}
    `;
  }

  private renderPendingProjectGroup(group: PendingProjectGroup): TemplateResult {
    const projectName = this.ctrl.projectDisplayName(group.project);
    const projectHref = group.project.id
      ? `/projects/${encodeURIComponent(group.project.id)}/decisions`
      : null;

    return html`
      <section class="pending-project-group" data-pending-group=${group.project.id}>
        <div class="groupsec">
          <h3>${projectName}</h3>
          <sl-badge variant="neutral" pill>${group.decisionCount}</sl-badge>
          ${projectHref
            ? html`<a class="linkish right" data-open-project-decisions href=${projectHref}
                >Open project Decisions</a
              >`
            : nothing}
        </div>
        ${repeat(
          group.prompts,
          (p) => p.promptMessageId,
          (p) =>
            renderPromptCard(p, {
              currentUserId: this.ctrl.currentUserId,
              askerGone: this.ctrl.isPromptAskerGone(p),
              onDismissTooRecent: this.ctrl.onPromptDismissTooRecent,
              onAnswered: this.ctrl.onPromptDecisionAnswered,
              onJump: this.ctrl.onPromptMessageJump,
            })
        )}
      </section>
    `;
  }

  private renderMadePanel(): TemplateResult {
    const isFiltered = this.ctrl.isMadeFiltered;
    const isByProject = this.ctrl.sort === 'project';
    const isEmpty = isByProject
      ? this.ctrl.madeGroups.length === 0
      : this.ctrl.madeEntries.length === 0;

    return html`
      ${this.ctrl.hasDateRangeError ? renderDateRangeError() : nothing}
      ${this.ctrl.madeLoading
        ? renderDecisionsLoadingState('Loading decision log...')
        : this.ctrl.madeSearchTimeout
          ? renderSearchTimeoutAlert('profile')
          : this.ctrl.madeError
            ? renderDecisionsErrorState({
                heading: 'Failed to Load Decision Log',
                message: this.ctrl.madeError,
                retryAttr: 'data-retry-made',
                onRetry: () => void this.ctrl.loadMadePage({ mode: 'reset' }),
              })
            : isEmpty
              ? isFiltered
                ? renderNoMatchState('made', this.ctrl.clearFilters)
                : renderMadeEmptyState('profile')
              : isByProject
                ? repeat(
                    this.ctrl.madeGroups,
                    (g) => g.project.id,
                    (g) => this.renderMadeProjectGroup(g)
                  )
                : html`
                    ${renderMadeTableAndMobileList(this.ctrl.madeEntries, {
                      currentUserId: this.ctrl.currentUserId,
                      showProjectColumn: true,
                      resolveProjectName: this.resolveProjectName,
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

  private renderMadeProjectGroup(g: ProfileDecisionMadeGroupView): TemplateResult {
    const projectName = this.ctrl.projectDisplayName(g.project);
    const drillSearch = buildProfileDecisionsUrlSearch({
      tab: 'made',
      sort: 'recent',
      project: g.project.id,
      q: this.ctrl.filterQ,
      conversation: this.ctrl.filterConversation,
      agent: this.ctrl.filterAgent,
      user: this.ctrl.filterUser,
      provenance: this.ctrl.filterProvenance,
      from: this.ctrl.filterFrom,
      to: this.ctrl.filterTo,
    });

    return html`
      <section class="made-project-group" data-made-group=${g.project.id}>
        <div class="groupsec">
          <h3>${projectName}</h3>
          ${g.project.id
            ? html`<a
                class="linkish right"
                href=${`?${drillSearch}`}
                data-see-all-project=${g.project.id}
                data-has-more=${g.more ? 'true' : 'false'}
                @click=${(e: Event): void => {
                  e.preventDefault();
                  this.ctrl.drillDownMadeProject(g.project.id);
                }}
                >See all in ${projectName}</a
              >`
            : nothing}
        </div>
        <div class="mlist">
          ${repeat(
            g.entries,
            (e) => e.answerId,
            (e) =>
              renderMadeItem(e, {
                currentUserId: this.ctrl.currentUserId,
                includeProjectInMeta: false,
                fallbackProject: g.project,
                resolveProjectName: this.resolveProjectName,
              })
          )}
        </div>
      </section>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-profile-decisions': ScionPageProfileDecisions;
  }
}
