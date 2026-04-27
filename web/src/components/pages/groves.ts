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
 * Groves list page component
 *
 * Displays all groves (project workspaces) with their status and agent counts
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Grove, Capabilities } from '../../shared/types.js';
import { can } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { stateManager } from '../../client/state.js';
import { listPageStyles } from '../shared/resource-styles.js';
import '../shared/git-remote-display.js';
import type { ViewMode } from '../shared/view-toggle.js';
import '../shared/view-toggle.js';

@customElement('scion-page-groves')
export class ScionPageGroves extends LitElement {
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
   * Groves list
   */
  @state()
  private groves: Grove[] = [];

  /**
   * Error message if loading failed
   */
  @state()
  private error: string | null = null;

  /**
   * Scope-level capabilities from the groves list response
   */
  @state()
  private scopeCapabilities: Capabilities | undefined;

  /**
   * Current view mode (grid or list)
   */
  @state()
  private viewMode: ViewMode = 'grid';

  /**
   * Filter scope: 'all' (no filter), 'mine' (owner), 'shared' (member/admin)
   */
  @state()
  private groveScope: 'all' | 'mine' | 'shared' = 'all';

  static override styles = [
    listPageStyles,
    css`
      .grove-header {
        display: flex;
        align-items: flex-start;
        justify-content: space-between;
        margin-bottom: 1rem;
      }

      .grove-path {
        font-size: 0.875rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.25rem;
        font-family: var(--scion-font-mono, monospace);
        word-break: break-all;
      }

      .grove-stats {
        display: flex;
        gap: 1.5rem;
        margin-top: 1rem;
        padding-top: 1rem;
        border-top: 1px solid var(--scion-border, #e2e8f0);
      }

      .grove-stats .stat-value {
        font-size: 1.25rem;
        font-weight: 600;
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

    `,
  ];

  private boundOnGrovesUpdated = this.onGrovesUpdated.bind(this);

  override connectedCallback(): void {
    super.connectedCallback();

    // Read persisted view mode
    const stored = localStorage.getItem('scion-view-groves') as ViewMode | null;
    if (stored === 'grid' || stored === 'list') {
      this.viewMode = stored;
    }

    // Read persisted scope filter
    if (this.pageData?.user) {
      const scope = localStorage.getItem('scion-scope-groves');
      if (scope === 'mine' || scope === 'shared') {
        this.groveScope = scope;
      }
    }

    // Set SSE scope to dashboard (grove summaries).
    // This must happen before checking hydrated data because setScope clears
    // state maps when the scope changes (e.g. from grove-detail to dashboard).
    stateManager.setScope({ type: 'dashboard' });

    // Use hydrated data from SSR if available, avoiding the initial fetch.
    // Only trust it when scope was previously null (initial SSR page load);
    // on client-side navigations the maps were just cleared by setScope above.
    // Skip hydrated data when a scope filter is active — SSR data is unfiltered.
    // Also require scope capabilities — without them the "New Grove" button
    // won't render, so we must fetch from the API to get them.
    const hydratedGroves = stateManager.getGroves();
    const hydratedCaps = stateManager.getScopeCapabilities();
    if (hydratedGroves.length > 0 && hydratedCaps && this.groveScope === 'all') {
      this.groves = hydratedGroves;
      this.scopeCapabilities = hydratedCaps;
      this.loading = false;
      stateManager.seedGroves(this.groves);
    } else {
      void this.loadGroves();
    }

    // Listen for real-time grove updates
    stateManager.addEventListener('groves-updated', this.boundOnGrovesUpdated as EventListener);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('groves-updated', this.boundOnGrovesUpdated as EventListener);
  }

  private onGrovesUpdated(): void {
    const updatedGroves = stateManager.getGroves();
    const deletedIds = stateManager.getDeletedGroveIds();

    const groveMap = new Map(this.groves.map((g) => [g.id, g]));

    // Remove deleted groves
    for (const id of deletedIds) {
      groveMap.delete(id);
    }

    // Merge updated/created groves
    for (const grove of updatedGroves) {
      const existing = groveMap.get(grove.id);
      // When a scope filter is active, only update groves already in the
      // filtered list — don't add new groves that weren't in the REST response.
      // The server-side filter is the source of truth for ownership/membership.
      if (!existing && this.groveScope !== 'all') {
        continue;
      }
      const merged = { ...existing, ...grove } as Grove;
      // Preserve _capabilities from existing state when the delta lacks them.
      if (!grove._capabilities && existing?._capabilities) {
        merged._capabilities = existing._capabilities;
      }
      groveMap.set(grove.id, merged);
    }

    this.groves = Array.from(groveMap.values());
  }

  private async loadGroves(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const url = this.groveScope !== 'all'
        ? `/api/v1/groves?scope=${this.groveScope}`
        : '/api/v1/groves';
      const response = await apiFetch(url);

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      const data = (await response.json()) as { groves?: Grove[]; _capabilities?: Capabilities } | Grove[];
      if (Array.isArray(data)) {
        this.groves = data;
        this.scopeCapabilities = undefined;
      } else {
        this.groves = data.groves || [];
        this.scopeCapabilities = data._capabilities;
      }

      // Seed stateManager so SSE delta merging has full baseline data
      // and so other pages sharing the same scope can reuse capabilities.
      stateManager.seedGroves(this.groves);
      if (this.scopeCapabilities) {
        stateManager.seedScopeCapabilities(this.scopeCapabilities);
      }
    } catch (err) {
      console.error('Failed to load groves:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load groves';
    } finally {
      this.loading = false;
    }
  }

  private onViewChange(e: CustomEvent<{ view: ViewMode }>): void {
    this.viewMode = e.detail.view;
  }

  private setScope(scope: 'all' | 'mine' | 'shared'): void {
    if (this.groveScope === scope) return;
    this.groveScope = scope;
    if (scope === 'all') {
      localStorage.removeItem('scion-scope-groves');
    } else {
      localStorage.setItem('scion-scope-groves', scope);
    }
    void this.loadGroves();
  }

  override render() {
    return html`
      <div class="header">
        <h1>Groves</h1>
        <div class="header-actions">
          ${this.pageData?.user ? html`
            <div class="scope-toggle">
              <button
                class=${this.groveScope === 'all' ? 'active' : ''}
                title="All groves"
                @click=${() => this.setScope('all')}
              >All</button>
              <button
                class=${this.groveScope === 'mine' ? 'active' : ''}
                title="Groves I own"
                @click=${() => this.setScope('mine')}
              >
                <sl-icon name="person"></sl-icon>
                Mine
              </button>
              <button
                class=${this.groveScope === 'shared' ? 'active' : ''}
                title="Groves shared with me"
                @click=${() => this.setScope('shared')}
              >
                <sl-icon name="people"></sl-icon>
                Shared
              </button>
            </div>
          ` : nothing}
          <scion-view-toggle
            .view=${this.viewMode}
            storageKey="scion-view-groves"
            @view-change=${this.onViewChange}
          ></scion-view-toggle>
          ${can(this.scopeCapabilities, 'create') ? html`
            <a href="/groves/new" style="text-decoration: none;">
              <sl-button variant="primary" size="small">
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                New Grove
              </sl-button>
            </a>
          ` : nothing}
        </div>
      </div>

      ${this.loading ? this.renderLoading() : this.error ? this.renderError() : this.renderGroves()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading groves...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Groves</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadGroves()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderGroves() {
    if (this.groves.length === 0) {
      if (this.groveScope === 'mine') {
        return html`
          <div class="empty-state">
            <sl-icon name="person"></sl-icon>
            <h2>No Groves Found</h2>
            <p>You don't own any groves yet.</p>
          </div>
        `;
      }
      if (this.groveScope === 'shared') {
        return html`
          <div class="empty-state">
            <sl-icon name="people"></sl-icon>
            <h2>No Shared Groves</h2>
            <p>No groves have been shared with you yet.</p>
          </div>
        `;
      }
      return this.renderEmptyState();
    }

    return this.viewMode === 'grid' ? this.renderGrid(this.groves) : this.renderTable(this.groves);
  }

  private renderEmptyState() {
    return html`
      <div class="empty-state">
        <sl-icon name="folder2-open"></sl-icon>
        <h2>No Groves Found</h2>
        <p>
          Groves are project workspaces that contain your agents.${can(this.scopeCapabilities, 'create') ? ' Create your first grove to get started, or run' : ' Run'}
          <code>scion init</code> in a project directory.
        </p>
        ${can(this.scopeCapabilities, 'create') ? html`
          <a href="/groves/new" style="text-decoration: none;">
            <sl-button variant="primary">
              <sl-icon slot="prefix" name="plus-lg"></sl-icon>
              Create Grove
            </sl-button>
          </a>
        ` : nothing}
      </div>
    `;
  }

  private renderGrid(groves: Grove[]) {
    return html`
      <div class="resource-grid">${groves.map((grove) => this.renderGroveCard(grove))}</div>
    `;
  }

  private renderGroveIcon() {
    return html`<sl-icon name="folder-fill"></sl-icon>`;
  }

  private renderLinkedBadge(grove: Grove) {
    if (grove.groveType !== 'linked') return nothing;
    return html` <sl-tooltip content="Linked grove"><sl-icon name="link-45deg" style="font-size: 0.875rem; vertical-align: middle; opacity: 0.7;"></sl-icon></sl-tooltip>`;
  }

  private renderGroveCard(grove: Grove) {
    return html`
      <a href="/groves/${grove.id}" class="resource-card">
        <div class="grove-header">
          <div>
            <h3 class="resource-name">
              ${this.renderGroveIcon()}
              ${grove.name}${this.renderLinkedBadge(grove)}
            </h3>
            <div class="grove-path"><scion-git-remote-display .grove=${grove} stop-propagation></scion-git-remote-display></div>
          </div>
        </div>
        <div class="grove-stats">
          <div class="stat">
            <span class="stat-label">Agents</span>
            <span class="stat-value">${grove.agentCount}</span>
          </div>
          <div class="stat">
            <span class="stat-label">Owner</span>
            <span class="stat-value" style="font-size: 0.875rem; font-weight: 500;">
              ${grove.ownerName || '—'}
            </span>
          </div>
        </div>
      </a>
    `;
  }

  private renderTable(groves: Grove[]) {
    return html`
      <div class="resource-table-container">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Path / Remote</th>
              <th>Agents</th>
              <th class="hide-mobile">Owner</th>
            </tr>
          </thead>
          <tbody>
            ${groves.map((grove) => this.renderGroveRow(grove))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderGroveRow(grove: Grove) {
    return html`
      <tr class="clickable" @click=${() => {
        window.history.pushState({}, '', `/groves/${grove.id}`);
        window.dispatchEvent(new PopStateEvent('popstate'));
      }}>
        <td>
          <span class="name-cell">
            ${this.renderGroveIcon()}
            ${grove.name}${this.renderLinkedBadge(grove)}
          </span>
        </td>
        <td class="mono-cell"><scion-git-remote-display .grove=${grove} stop-propagation></scion-git-remote-display></td>
        <td>${grove.agentCount}</td>
        <td class="hide-mobile">
          <span class="meta-text">${grove.ownerName || '—'}</span>
        </td>
      </tr>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-groves': ScionPageGroves;
  }
}
