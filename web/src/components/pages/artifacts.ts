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
 * Artifacts list page (experiment hub.artifacts).
 *
 * Route: /artifacts. Lists the artifacts the user owns, those shared with
 * them, and those published in their projects (GET /api/v1/artifacts?mine=1),
 * newest first, with search, a review-pending filter and "Owned by me" and
 * "Shared with me" filters. Each row says why the user sees it (Owned,
 * Project, Shared with you); a row whose home project was deleted says so
 * and, for its owner or an admin, offers Move…. Rows open the artifact page.
 * Owner and project names are looked up best-effort; the ids are shown when
 * a lookup is not allowed.
 *
 * New artifact (ptone/scion#5477) opens the publish dialog with a project
 * picker. The page checks whether the user may publish anywhere (global
 * project first, stopping at the first publishable one): the button is
 * disabled while that runs, disabled with an explanation when there is
 * nowhere, and enabled otherwise, including when the check failed (the
 * dialog then shows the error). A finished load in the dialog updates the
 * button. After publishing, the user lands on the new artifact's page.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { navigateTo, replaceSearch } from '../../client/navigation.js';
import {
  ARTIFACTS_FLAG,
  artifactListUrl,
  artifactPagePath,
  canPublishIn,
  filterPublishable,
  findGlobalProject,
  listReadableProjects,
  type Artifact,
  type ArtifactResponse,
  type ArtifactAccess,
  type ArtifactListItem,
  type ArtifactListResponse,
} from '../../client/artifacts.js';
import {
  getViewSchema,
  hasIsFlagInQuery,
  knownKeysFor,
  parse as parseFilterQuery,
  serverSubset,
  toggleIsFlagInQueryWithNotice,
  validate as validateFilterQuery,
  type ViewSchema,
} from '../../shared/filter-query/index.js';
import type { FilterCommitDetail, FilterInputDetail } from '../shared/filter-box/filter-box.js';
import '../shared/filter-box/filter-box.js';
import { ARTIFACTS_REVIEW_REQUESTS_FLAG, isFeatureEnabled } from '../../utils/feature-flags.js';
import { formatRelative, formatInstant } from '../../utils/time.js';
import { listPageStyles } from '../shared/resource-styles.js';
import { ownerDeletedBadge, ownerDeletedStyles } from '../shared/artifact-owner-deleted.js';
import { shortId } from '../../client/principal-names.js';
import '../shared/artifact-move-dialog.js';
import '../shared/artifact-publish-dialog.js';
import './not-found.js';

/** How each access value is shown in the Access column. */
const ACCESS_BADGES: Record<ArtifactAccess, { label: string; variant: string; icon?: string }> = {
  owned: { label: 'Owned', variant: 'primary' },
  project: { label: 'Project', variant: 'neutral' },
  shared: { label: 'Shared with you', variant: 'success', icon: 'people' },
};

/** The reference page for artifacts on the documentation site. */
export const ARTIFACTS_DOCS_URL =
  'https://googlecloudplatform.github.io/scion/reference/artifacts/';

/** Why New artifact is disabled when the user may publish nowhere. */
export const NOWHERE_TO_PUBLISH =
  "You can't publish in any of your projects. Ask a project owner for access.";

/**
 * Whether the user may publish somewhere: still checking, yes, no, or
 * unknown (the check failed; the dialog shows why).
 */
type PublishAccess = 'checking' | 'yes' | 'no' | 'unknown';

const ARTIFACTS_FILTER_SCHEMA: ViewSchema = getViewSchema('artifacts') ?? {
  view: 'artifacts',
  keys: [],
  textHelp: 'Matches artifact title or key',
};

@customElement('scion-page-artifacts')
export class ScionPageArtifacts extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @state() private enabled = false;
  @state() private loading = true;
  @state() private loadingMore = false;
  @state() private error: string | null = null;
  @state() private items: ArtifactListItem[] = [];
  @state() private nextCursor = '';
  @state() private filterQueryText = '';
  @state() private filterNotice = '';
  /** The row whose Move dialog is open. */
  @state() private moving: ArtifactListItem | null = null;
  /** Display names by "kind:id" (owners) or "project:id"; '' while unknown. */
  @state() private names = new Map<string, string>();
  /** Project slugs by project id, for filter-box `project:` value suggestions. */
  private projectSlugs = new Map<string, string>();
  private committedServerQ = '';
  @state() private publishAccess: PublishAccess = 'checking';
  @state() private publishOpen = false;

  /** Bumped on every reload, so a slow response for old filters is dropped. */
  private generation = 0;
  /** Bumped per publish check, so a stale result is dropped. */
  private publishGen = 0;
  /** Cancels the running publish check. */
  private publishAbort: AbortController | null = null;
  private requested = new Set<string>();

  static override styles = [
    listPageStyles,
    ownerDeletedStyles,
    css`
      .subtitle {
        margin: -1rem 0 1.25rem;
        color: var(--scion-text-muted, #64748b);
        font-size: 0.875rem;
      }
      .key {
        display: block;
        margin-left: 1.625rem;
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }
      .agent-tag {
        color: var(--scion-text-muted, #64748b);
        font-size: 0.75rem;
      }
      .muted {
        color: var(--scion-text-muted, #64748b);
      }
      .deleted {
        font-style: italic;
        color: var(--scion-text-muted, #64748b);
      }
      .move {
        margin-left: 0.375rem;
        font-size: 0.8125rem;
      }
      sl-badge sl-icon {
        vertical-align: -0.125em;
      }
      .count {
        margin-left: auto;
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
      }
      .load-more {
        display: flex;
        justify-content: center;
        margin-top: 1rem;
      }
      .nowhere {
        font-size: 0.875rem;
      }
      .empty-actions {
        display: flex;
        flex-wrap: wrap;
        gap: 0.75rem;
        justify-content: center;
        margin-top: 1rem;
      }
      .filter-chips {
        display: inline-flex;
        flex-wrap: wrap;
        align-items: center;
        gap: 0.375rem;
      }
      .filter-chip {
        appearance: none;
        border: 1px solid var(--scion-border);
        background: var(--scion-surface);
        color: var(--scion-text-secondary);
        border-radius: var(--scion-radius);
        padding: 0.3125rem 0.625rem;
        font: inherit;
        font-size: 0.8125rem;
        line-height: 1.25;
        cursor: pointer;
        transition:
          background-color 120ms ease,
          border-color 120ms ease,
          color 120ms ease;
      }
      .filter-chip:hover {
        background: var(--scion-bg-subtle);
        color: var(--scion-text);
      }
      .filter-chip.active {
        background: var(--scion-primary-tint);
        border-color: var(--scion-primary);
        color: var(--scion-primary);
        font-weight: 500;
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    this.enabled = isFeatureEnabled(ARTIFACTS_FLAG);
    if (!this.enabled) {
      this.loading = false;
      return;
    }
    const params = new URLSearchParams(window.location.search);
    let urlQ = params.get('q') ?? '';
    let migratedLegacy = false;
    if (!urlQ) {
      const migrated: string[] = [];
      const reviewParam = params.get('review_pending');
      if (reviewParam === '1' || reviewParam === 'true') migrated.push('is:review');
      const requestedParam = params.get('requested');
      if (this.reviewRequestsEnabled && (requestedParam === '1' || requestedParam === 'true')) {
        migrated.push('is:requested');
      }
      if (params.get('owner') === 'me') migrated.push('is:mine');
      if (migrated.length > 0) {
        urlQ = migrated.join(' ');
        migratedLegacy = true;
      }
    }
    if (urlQ) {
      this.filterQueryText = urlQ;
      const parsed = parseFilterQuery(urlQ, knownKeysFor(ARTIFACTS_FILTER_SCHEMA));
      if (!parsed.error && parsed.query) {
        const v = validateFilterQuery(parsed.query, ARTIFACTS_FILTER_SCHEMA, urlQ);
        if (!v.error) {
          this.committedServerQ = serverSubset(parsed.query, ARTIFACTS_FILTER_SCHEMA);
        }
      }
      if (migratedLegacy) {
        this.syncQueryToUrl(urlQ);
      }
    }
    void this.load();
    void this.checkPublishAccess();
  }

  /** Artifact review requests gate (experiment hub.artifacts_review_requests). */
  private get reviewRequestsEnabled(): boolean {
    return isFeatureEnabled(ARTIFACTS_REVIEW_REQUESTS_FLAG);
  }
  /** Stops the running publish check, if any; its result is dropped. */
  private cancelPublishCheck(): void {
    this.publishGen++;
    this.publishAbort?.abort();
    this.publishAbort = null;
  }

  /**
   * Works out whether the user may publish in any project: the global
   * project first, looked up on its own; only when it is missing or not
   * publishable are the other projects listed and checked, stopping at the
   * first publishable one.
   */
  private async checkPublishAccess(): Promise<void> {
    this.cancelPublishCheck();
    const gen = this.publishGen;
    const abort = new AbortController();
    this.publishAbort = abort;
    const signal = abort.signal;
    const current = (): boolean => gen === this.publishGen && this.isConnected;
    this.publishAccess = 'checking';
    try {
      const global = await findGlobalProject(signal);
      if (!current()) return;
      if (global && (await canPublishIn(global.id, signal))) {
        if (current()) this.publishAccess = 'yes';
        return;
      }
      if (!current()) return;
      const { projects } = await listReadableProjects({ skip: global?.id, signal });
      if (!current()) return;
      const found = await filterPublishable(projects, { firstOnly: true, signal });
      if (!current()) return;
      this.publishAccess = found.length > 0 ? 'yes' : 'no';
    } catch {
      if (!current()) return;
      this.publishAccess = 'unknown';
    } finally {
      if (this.publishAbort === abort) this.publishAbort = null;
    }
  }

  /** The dialog finished loading the choices: that settles the button. */
  private onChoices(e: CustomEvent<{ count: number }>): void {
    // Stop a page check still running: the dialog's answer is complete.
    this.cancelPublishCheck();
    this.publishAccess = e.detail.count > 0 ? 'yes' : 'no';
  }

  private onPublished(e: CustomEvent<ArtifactResponse>): void {
    this.publishOpen = false;
    navigateTo(artifactPagePath(e.detail.artifact, 0, 'artifacts'));
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.cancelPublishCheck();
  }

  private get filtered(): boolean {
    return this.committedServerQ.trim() !== '';
  }

  /**
   * Loads the first page for the current filters. The previous filters'
   * cursor is dropped at once (a cursor only works for the filters it was
   * issued for), and so is any load-more still in flight. The old rows stay
   * on screen while the new page loads, but not after a failure: then the
   * full error state, with Retry, replaces them.
   */
  private async load(): Promise<void> {
    const gen = ++this.generation;
    this.loading = true;
    this.loadingMore = false;
    this.nextCursor = '';
    this.error = null;
    try {
      const page = await this.fetchPage();
      if (gen !== this.generation || !this.isConnected) return;
      this.items = page.artifacts ?? [];
      this.nextCursor = page.nextCursor ?? '';
      this.resolveNames(this.items);
    } catch (err) {
      if (gen !== this.generation || !this.isConnected) return;
      this.items = [];
      this.error = err instanceof Error ? err.message : 'Failed to load artifacts';
    } finally {
      if (gen === this.generation) this.loading = false;
    }
  }

  /** Appends the next page. */
  private async loadMore(): Promise<void> {
    if (!this.nextCursor || this.loadingMore || this.loading) return;
    const gen = this.generation;
    this.loadingMore = true;
    this.error = null;
    try {
      const page = await this.fetchPage(this.nextCursor);
      if (gen !== this.generation || !this.isConnected) return;
      const more = page.artifacts ?? [];
      this.items = [...this.items, ...more];
      this.nextCursor = page.nextCursor ?? '';
      this.resolveNames(more);
    } catch (err) {
      if (gen !== this.generation || !this.isConnected) return;
      this.error = err instanceof Error ? err.message : 'Failed to load more artifacts';
    } finally {
      if (gen === this.generation) this.loadingMore = false;
    }
  }

  private async fetchPage(cursor?: string): Promise<ArtifactListResponse> {
    const url = artifactListUrl({ q: this.committedServerQ }, cursor);
    const res = await apiFetch(url);
    if (!res.ok) {
      throw new Error(await extractApiError(res, `HTTP ${res.status}`));
    }
    return (await res.json()) as ArtifactListResponse;
  }

  /**
   * Looks up the display names of owners and home projects not yet known.
   * A lookup the caller may not make is not an error: the id stays.
   */
  private resolveNames(items: ArtifactListItem[]): void {
    const me = this.pageData?.user?.id;
    for (const a of items) {
      const ownerKey = `${a.ownerKind}:${a.ownerRef}`;
      const isMe = a.ownerKind === 'user' && a.ownerRef === me;
      if (!isMe && (a.ownerKind === 'agent' || a.ownerKind === 'user')) {
        this.lookup(ownerKey, a.ownerKind === 'agent' ? 'agents' : 'users', a.ownerRef);
      }
      if (a.scopeKind === 'project' && a.scopeRef && !a.scopeDeleted) {
        this.lookup(`project:${a.scopeRef}`, 'projects', a.scopeRef);
      }
    }
  }

  private lookup(key: string, collection: 'agents' | 'users' | 'projects', id: string): void {
    // An empty id would address the collection itself.
    if (!id || this.requested.has(key)) return;
    this.requested.add(key);
    void (async (): Promise<void> => {
      try {
        const res = await apiFetch(`/api/v1/${collection}/${encodeURIComponent(id)}`, {
          suppressAccessDeniedToast: true,
        });
        if (!res.ok) return;
        const body = (await res.json()) as {
          name?: string;
          slug?: string;
          displayName?: string;
        };
        const name = body.displayName || body.name || body.slug || '';
        if (collection === 'projects' && body.slug && this.isConnected) {
          this.projectSlugs.set(id, body.slug);
        }
        if (name && this.isConnected) {
          const next = new Map(this.names);
          next.set(key, name);
          this.names = next;
        }
      } catch {
        // The id is shown instead.
      }
    })();
  }

  private syncQueryToUrl(raw: string): void {
    const params = new URLSearchParams(window.location.search);
    const trimmed = raw.trim();
    if (trimmed) {
      params.set('q', trimmed);
    } else {
      params.delete('q');
    }
    params.delete('review_pending');
    params.delete('requested');
    if (params.get('owner') === 'me') {
      params.delete('owner');
    }
    replaceSearch(params);
  }

  private onFilterInput = (e: CustomEvent<FilterInputDetail>): void => {
    this.filterNotice = '';
    this.filterQueryText = e.detail.value;
  };

  private onFilterCommit = (e: CustomEvent<FilterCommitDetail>): void => {
    this.filterNotice = '';
    this.filterQueryText = e.detail.value;
    this.syncQueryToUrl(e.detail.value);
    if (e.detail.serverQ === this.committedServerQ) return;
    this.committedServerQ = e.detail.serverQ;
    void this.load();
  };

  private onFlagChipClick(flag: 'review' | 'requested' | 'mine' | 'shared'): void {
    const exclusive = flag === 'mine' ? ['shared'] : flag === 'shared' ? ['mine'] : [];
    const edit = toggleIsFlagInQueryWithNotice(
      this.filterQueryText,
      flag,
      exclusive,
      ARTIFACTS_FILTER_SCHEMA
    );
    const next = edit.value;
    this.filterNotice = edit.notice ?? '';
    if (next === this.filterQueryText) return;
    this.filterQueryText = next;
    this.syncQueryToUrl(next);
    const parsed = parseFilterQuery(next, knownKeysFor(ARTIFACTS_FILTER_SCHEMA));
    if (!parsed.error && parsed.query) {
      const v = validateFilterQuery(parsed.query, ARTIFACTS_FILTER_SCHEMA, next);
      if (!v.error) {
        const nextQ = serverSubset(parsed.query, ARTIFACTS_FILTER_SCHEMA);
        if (nextQ !== this.committedServerQ) {
          this.committedServerQ = nextQ;
          void this.load();
        }
      }
    }
  }

  private suggestValues = (key: string): string[] => {
    if (key.toLowerCase() !== 'project') return [];
    const seen = new Set<string>();
    const out: string[] = [];
    for (const a of this.items) {
      if (a.scopeKind !== 'project' || !a.scopeRef || a.scopeDeleted) continue;
      const val = this.projectSlugs.get(a.scopeRef) || a.scopeRef;
      if (!seen.has(val)) {
        seen.add(val);
        out.push(val);
      }
    }
    return out;
  };

  private onMoved(e: CustomEvent<Artifact>): void {
    const moved = e.detail;
    this.moving = null;
    this.items = this.items.map((a) =>
      a.id === moved.id ? { ...a, ...moved, scopeDeleted: false, canManage: false } : a
    );
    this.resolveNames(this.items.filter((a) => a.id === moved.id));
  }

  override render(): TemplateResult {
    if (!this.enabled) {
      return html`<scion-page-404></scion-page-404>`;
    }
    return html`
      <div class="header">
        <h1>Artifacts</h1>
        <div class="header-actions">${this.renderNewButton()}</div>
      </div>
      <p class="subtitle">Artifacts you own, shared with you, or published in your projects.</p>
      ${this.renderFilterBar()} ${this.renderBody()}
      <scion-artifact-publish-dialog
        chooseProject
        ?open=${this.publishOpen}
        @artifact-published=${(e: CustomEvent<ArtifactResponse>): void => this.onPublished(e)}
        @artifact-publish-choices=${(e: CustomEvent<{ count: number }>): void => this.onChoices(e)}
        @artifact-publish-closed=${(): void => {
          this.publishOpen = false;
        }}
      ></scion-artifact-publish-dialog>
    `;
  }

  private renderNewButton(): TemplateResult {
    const disabled = this.publishAccess === 'checking' || this.publishAccess === 'no';
    const button = html`<sl-button
      class="new-artifact"
      size="small"
      variant="primary"
      ?disabled=${disabled}
      @click=${(): void => {
        this.publishOpen = true;
      }}
    >
      <sl-icon slot="prefix" name="plus-lg"></sl-icon>
      New artifact
    </sl-button>`;
    if (this.publishAccess !== 'no') return button;
    // A disabled button gets no pointer or focus events: the wrapper takes
    // them, so hovering or tabbing to it shows why.
    return html`<sl-tooltip content=${NOWHERE_TO_PUBLISH}>
      <span
        class="nowhere-wrap"
        role="button"
        tabindex="0"
        aria-disabled="true"
        aria-label=${`New artifact: ${NOWHERE_TO_PUBLISH}`}
        >${button}</span
      >
    </sl-tooltip>`;
  }

  private renderFilterBar(): TemplateResult {
    const reviewActive = hasIsFlagInQuery(this.filterQueryText, 'review', ARTIFACTS_FILTER_SCHEMA);
    const requestedActive = hasIsFlagInQuery(
      this.filterQueryText,
      'requested',
      ARTIFACTS_FILTER_SCHEMA
    );
    const mineActive = hasIsFlagInQuery(this.filterQueryText, 'mine', ARTIFACTS_FILTER_SCHEMA);
    const sharedActive = hasIsFlagInQuery(this.filterQueryText, 'shared', ARTIFACTS_FILTER_SCHEMA);
    return html`
      <div class="filter-bar">
        <scion-filter-box
          view="artifacts"
          .value=${this.filterQueryText}
          .notice=${this.filterNotice}
          .valueSuggestions=${this.suggestValues}
          placeholder=${this.reviewRequestsEnabled
            ? 'Filter artifacts… (try is:requested project:my-project)'
            : 'Filter artifacts… (try is:review project:my-project)'}
          @filter-input=${this.onFilterInput}
          @filter-commit=${this.onFilterCommit}
        ></scion-filter-box>
        <div class="filter-chips" role="group" aria-label="Quick filters">
          <button
            type="button"
            class="filter-chip ${reviewActive ? 'active' : ''}"
            aria-pressed=${reviewActive ? 'true' : 'false'}
            @click=${(): void => this.onFlagChipClick('review')}
          >
            Review pending
          </button>
          ${this.reviewRequestsEnabled
            ? html`<button
                type="button"
                class="filter-chip ${requestedActive ? 'active' : ''}"
                aria-pressed=${requestedActive ? 'true' : 'false'}
                @click=${(): void => this.onFlagChipClick('requested')}
              >
                Requested of me
              </button>`
            : nothing}
          <button
            type="button"
            class="filter-chip ${mineActive ? 'active' : ''}"
            aria-pressed=${mineActive ? 'true' : 'false'}
            @click=${(): void => this.onFlagChipClick('mine')}
          >
            Owned by me
          </button>
          <button
            type="button"
            class="filter-chip ${sharedActive ? 'active' : ''}"
            aria-pressed=${sharedActive ? 'true' : 'false'}
            @click=${(): void => this.onFlagChipClick('shared')}
          >
            Shared with me
          </button>
        </div>
        ${this.loading && this.items.length > 0
          ? html`<sl-spinner class="inline-loading" aria-label="Loading artifacts"></sl-spinner>`
          : nothing}
        ${this.items.length > 0
          ? html`<span class="count">Showing ${this.items.length}</span>`
          : nothing}
      </div>
    `;
  }

  private renderBody(): TemplateResult {
    if (this.loading && this.items.length === 0) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading artifacts...</p>
        </div>
      `;
    }
    if (this.error && this.items.length === 0) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load Artifacts</h2>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${(): void => void this.load()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }
    if (this.items.length === 0 && !this.nextCursor) {
      return this.filtered ? this.renderNoMatch() : this.renderEmpty();
    }
    if (this.items.length === 0) {
      // A page can end early (the hub examines a bounded number of rows per
      // request) and still have more after it: keep the walk going.
      return html`
        <div class="empty-state">
          <p>Nothing on this page. There may be more artifacts further on.</p>
        </div>
        ${this.renderLoadMore()}
      `;
    }
    return html`
      ${this.renderTable()}
      ${this.error ? html`<p class="muted" role="alert">${this.error}</p>` : nothing}
      ${this.renderLoadMore()}
    `;
  }

  private renderLoadMore(): TemplateResult | typeof nothing {
    if (!this.nextCursor) return nothing;
    return html`
      <div class="load-more">
        <sl-button
          size="small"
          ?loading=${this.loadingMore}
          ?disabled=${this.loading}
          @click=${(): void => void this.loadMore()}
        >
          Load more
        </sl-button>
      </div>
    `;
  }

  private renderEmpty(): TemplateResult {
    return html`
      <div class="empty-state">
        <sl-icon name="file-earmark-richtext"></sl-icon>
        <h2>No Artifacts Found</h2>
        <p>
          Artifacts are documents and reports that you and your agents publish. Ones you own, ones
          shared with you, and ones published in your projects appear here.
        </p>
        ${this.publishAccess === 'no'
          ? html`<p class="nowhere muted">${NOWHERE_TO_PUBLISH}</p>`
          : nothing}
        <div class="empty-actions">
          <sl-button variant="primary" href="/projects">
            <sl-icon slot="prefix" name="folder"></sl-icon>
            Browse projects
          </sl-button>
          <sl-button href=${ARTIFACTS_DOCS_URL} target="_blank" rel="noopener noreferrer">
            <sl-icon slot="prefix" name="box-arrow-up-right"></sl-icon>
            Learn about artifacts
          </sl-button>
        </div>
      </div>
    `;
  }

  private renderNoMatch(): TemplateResult {
    return html`
      <div class="empty-state">
        <sl-icon name="funnel"></sl-icon>
        <h2>No Matching Artifacts</h2>
        <p>No artifacts match the current filters.</p>
      </div>
    `;
  }

  private renderTable(): TemplateResult {
    return html`
      <div class="resource-table-container">
        <table>
          <thead>
            <tr>
              <th>Title</th>
              <th>Access</th>
              <th>Owner</th>
              <th class="hide-mobile">Home project</th>
              <th>Updated</th>
              <th>Status</th>
            </tr>
          </thead>
          <tbody>
            ${this.items.map((a) => this.renderRow(a))}
          </tbody>
        </table>
      </div>
      <scion-artifact-move-dialog
        .artifact=${this.moving}
        ?open=${!!this.moving}
        homeDeleted
        @artifact-moved=${(e: CustomEvent<Artifact>): void => this.onMoved(e)}
        @artifact-move-closed=${(): void => {
          this.moving = null;
        }}
      ></scion-artifact-move-dialog>
    `;
  }

  private renderRow(a: ArtifactListItem): TemplateResult {
    const href = artifactPagePath(a, 0, 'artifacts');
    return html`
      <tr class="clickable" @click=${(): void => navigateTo(href)}>
        <td>
          <span class="name-cell">
            <sl-icon name="file-earmark-richtext"></sl-icon>
            <a href=${href} @click=${(e: Event): void => e.stopPropagation()}>${a.title}</a>
          </span>
          ${a.key ? html`<span class="key">${a.key}</span>` : nothing}
        </td>
        <td>${this.renderAccess(a)}</td>
        <td>${this.renderOwner(a)}</td>
        <td class="hide-mobile">${this.renderProject(a)}</td>
        <td title=${formatInstant(a.updatedAt)}>${formatRelative(a.updatedAt)}</td>
        <td>
          ${a.ownerDeleted ? ownerDeletedBadge(a.ownerKind) : nothing}
          ${a.reviewPending
            ? html`<sl-badge variant="warning" pill>Review pending</sl-badge>`
            : nothing}
          ${a.reviewRequested
            ? html`<sl-badge variant="primary" pill>Review requested</sl-badge>`
            : nothing}
          ${!a.ownerDeleted && !a.reviewPending && !a.reviewRequested
            ? html`<span class="muted">—</span>`
            : nothing}
        </td>
      </tr>
    `;
  }

  private renderAccess(a: ArtifactListItem): TemplateResult | typeof nothing {
    const badge = a.access ? ACCESS_BADGES[a.access] : undefined;
    if (!badge) return nothing;
    return html`<sl-badge variant=${badge.variant} pill>
      ${badge.icon ? html`<sl-icon name=${badge.icon}></sl-icon>` : nothing} ${badge.label}
    </sl-badge>`;
  }

  private renderProject(a: ArtifactListItem): TemplateResult {
    if (a.scopeDeleted) {
      return html`<span class="deleted">Deleted project</span> ${a.canManage
          ? html`<a
              href="#"
              class="move"
              @click=${(e: Event): void => {
                e.preventDefault();
                e.stopPropagation();
                this.moving = a;
              }}
              >Move…</a
            >`
          : nothing}`;
    }
    return html`<a
      href=${`/projects/${encodeURIComponent(a.scopeRef)}`}
      @click=${(e: Event): void => e.stopPropagation()}
      >${this.names.get(`project:${a.scopeRef}`) || a.scopeRef}</a
    >`;
  }

  private renderOwner(a: ArtifactListItem): TemplateResult {
    if (a.ownerKind === 'user' && a.ownerRef === this.pageData?.user?.id) {
      return html`<strong>You</strong>`;
    }
    // A deleted owner's name may no longer be found: show its short id
    // then, struck through like the name (mock S1).
    const known = this.names.get(`${a.ownerKind}:${a.ownerRef}`);
    const name = known || (a.ownerDeleted ? shortId(a.ownerRef) : a.ownerRef);
    const shown = a.ownerDeleted ? html`<span class="owner-gone">${name}</span>` : html`${name}`;
    if (a.ownerKind === 'agent') {
      return html`${shown} <span class="agent-tag">(agent)</span>`;
    }
    return shown;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-artifacts': ScionPageArtifacts;
  }
}
