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
 * newest first, with search, a review-pending filter and an "Owned by me"
 * filter. Rows open the artifact page. Owner and project names are looked up
 * best-effort; the ids are shown when a lookup is not allowed.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { navigateTo } from '../../client/navigation.js';
import {
  ARTIFACTS_FLAG,
  artifactListUrl,
  artifactPagePath,
  type ArtifactListItem,
  type ArtifactListResponse,
} from '../../client/artifacts.js';
import { isFeatureEnabled } from '../../utils/feature-flags.js';
import { formatRelative, formatInstant } from '../../utils/time.js';
import { listPageStyles } from '../shared/resource-styles.js';
import './not-found.js';

/** The reference page for artifacts on the documentation site. */
export const ARTIFACTS_DOCS_URL =
  'https://googlecloudplatform.github.io/scion/reference/artifacts/';

/** Delay before a search box change reloads the list. */
const SEARCH_DEBOUNCE_MS = 300;

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
  @state() private search = '';
  @state() private reviewPending = false;
  @state() private ownedOnly = false;
  /** Display names by "kind:id" (owners) or "project:id"; '' while unknown. */
  @state() private names = new Map<string, string>();

  /** Bumped on every reload, so a slow response for old filters is dropped. */
  private generation = 0;
  private searchTimer: ReturnType<typeof setTimeout> | null = null;
  private requested = new Set<string>();

  static override styles = [
    listPageStyles,
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
      .empty-actions {
        display: flex;
        flex-wrap: wrap;
        gap: 0.75rem;
        justify-content: center;
        margin-top: 1rem;
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
    void this.load();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.searchTimer) clearTimeout(this.searchTimer);
  }

  private get filtered(): boolean {
    return this.search.trim() !== '' || this.reviewPending || this.ownedOnly;
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
      if (gen !== this.generation) return;
      this.items = page.artifacts ?? [];
      this.nextCursor = page.nextCursor ?? '';
      this.resolveNames(this.items);
    } catch (err) {
      if (gen !== this.generation) return;
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
      if (gen !== this.generation) return;
      const more = page.artifacts ?? [];
      this.items = [...this.items, ...more];
      this.nextCursor = page.nextCursor ?? '';
      this.resolveNames(more);
    } catch (err) {
      if (gen !== this.generation) return;
      this.error = err instanceof Error ? err.message : 'Failed to load more artifacts';
    } finally {
      if (gen === this.generation) this.loadingMore = false;
    }
  }

  private async fetchPage(cursor?: string): Promise<ArtifactListResponse> {
    const url = artifactListUrl(
      { q: this.search, reviewPending: this.reviewPending, ownedOnly: this.ownedOnly },
      cursor
    );
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
      if (a.scopeKind === 'project' && a.scopeRef) {
        this.lookup(`project:${a.scopeRef}`, 'projects', a.scopeRef);
      }
    }
  }

  private lookup(key: string, collection: 'agents' | 'users' | 'projects', id: string): void {
    if (this.requested.has(key)) return;
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
        if (name) {
          const next = new Map(this.names);
          next.set(key, name);
          this.names = next;
        }
      } catch {
        // The id is shown instead.
      }
    })();
  }

  private onSearchInput(e: Event): void {
    const value = (e.target as HTMLElement & { value: string }).value;
    if (this.searchTimer) clearTimeout(this.searchTimer);
    this.searchTimer = setTimeout(() => {
      this.search = value;
      void this.load();
    }, SEARCH_DEBOUNCE_MS);
  }

  private onStatusChange(e: Event): void {
    this.reviewPending = (e.target as HTMLElement & { value: string }).value === 'review';
    void this.load();
  }

  private onOwnedChange(e: Event): void {
    this.ownedOnly = (e.target as HTMLElement & { checked: boolean }).checked;
    void this.load();
  }

  override render(): TemplateResult {
    if (!this.enabled) {
      return html`<scion-page-404></scion-page-404>`;
    }
    return html`
      <div class="header">
        <h1>Artifacts</h1>
      </div>
      <p class="subtitle">Artifacts you own, shared with you, or published in your projects.</p>
      ${this.renderFilterBar()} ${this.renderBody()}
    `;
  }

  private renderFilterBar(): TemplateResult {
    return html`
      <div class="filter-bar">
        <sl-input
          class="search-input"
          size="small"
          placeholder="Search title or key..."
          aria-label="Search artifacts"
          clearable
          @sl-input=${(e: Event): void => this.onSearchInput(e)}
        >
          <sl-icon slot="prefix" name="search"></sl-icon>
        </sl-input>
        <sl-select
          size="small"
          value=${this.reviewPending ? 'review' : 'all'}
          @sl-change=${(e: Event): void => this.onStatusChange(e)}
          style="min-width: 170px;"
        >
          <sl-option value="all">All artifacts</sl-option>
          <sl-option value="review">Review pending</sl-option>
        </sl-select>
        <sl-checkbox
          size="small"
          ?checked=${this.ownedOnly}
          @sl-change=${(e: Event): void => this.onOwnedChange(e)}
          >Owned by me</sl-checkbox
        >
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
          <sl-icon name="file-earmark-richtext"></sl-icon>
          <h2>No Artifacts Found Yet</h2>
          <p>There may be more artifacts further on.</p>
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
    `;
  }

  private renderRow(a: ArtifactListItem): TemplateResult {
    const href = artifactPagePath(a);
    return html`
      <tr class="clickable" @click=${(): void => navigateTo(href)}>
        <td>
          <span class="name-cell">
            <sl-icon name="file-earmark-richtext"></sl-icon>
            <a href=${href} @click=${(e: Event): void => e.stopPropagation()}>${a.title}</a>
          </span>
          ${a.key ? html`<span class="key">${a.key}</span>` : nothing}
        </td>
        <td>${this.renderOwner(a)}</td>
        <td class="hide-mobile">
          <a
            href=${`/projects/${encodeURIComponent(a.scopeRef)}`}
            @click=${(e: Event): void => e.stopPropagation()}
            >${this.names.get(`project:${a.scopeRef}`) || a.scopeRef}</a
          >
        </td>
        <td title=${formatInstant(a.updatedAt)}>${formatRelative(a.updatedAt)}</td>
        <td>
          ${a.reviewPending
            ? html`<sl-badge variant="warning" pill>Review pending</sl-badge>`
            : html`<span class="muted">—</span>`}
        </td>
      </tr>
    `;
  }

  private renderOwner(a: ArtifactListItem): TemplateResult {
    if (a.ownerKind === 'user' && a.ownerRef === this.pageData?.user?.id) {
      return html`<strong>You</strong>`;
    }
    const name = this.names.get(`${a.ownerKind}:${a.ownerRef}`) || a.ownerRef;
    if (a.ownerKind === 'agent') {
      return html`${name} <span class="agent-tag">(agent)</span>`;
    }
    return html`${name}`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-artifacts': ScionPageArtifacts;
  }
}
