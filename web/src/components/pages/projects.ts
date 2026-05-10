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
 * Projects list page component
 *
 * Displays all projects (project workspaces) with their status and agent counts
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Project, Capabilities } from '../../shared/types.js';
import { can } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { stateManager } from '../../client/state.js';
import { listPageStyles } from '../shared/resource-styles.js';
import '../shared/git-remote-display.js';
import type { ViewMode } from '../shared/view-toggle.js';
import '../shared/view-toggle.js';

@customElement('scion-page-projects')
export class ScionPageProjects extends LitElement {
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
   * Projects list
   */
  @state()
  private projects: Project[] = [];

  /**
   * Error message if loading failed
   */
  @state()
  private error: string | null = null;

  /**
   * Scope-level capabilities from the projects list response
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
  private projectScope: 'all' | 'mine' | 'shared' = 'all';

  static override styles = [
    listPageStyles,
    css`
      .project-header {
        display: flex;
        align-items: flex-start;
        justify-content: space-between;
        margin-bottom: 1rem;
      }

      .project-path {
        font-size: 0.875rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.25rem;
        font-family: var(--scion-font-mono, monospace);
        word-break: break-all;
      }

      .project-stats {
        display: flex;
        gap: 1.5rem;
        margin-top: 1rem;
        padding-top: 1rem;
        border-top: 1px solid var(--scion-border, #e2e8f0);
      }

      .project-stats .stat-value {
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

  private boundOnProjectsUpdated = this.onProjectsUpdated.bind(this);

  override connectedCallback(): void {
    super.connectedCallback();

    // Read persisted view mode
    const stored = localStorage.getItem('scion-view-projects') as ViewMode | null;
    if (stored === 'grid' || stored === 'list') {
      this.viewMode = stored;
    }

    // Read persisted scope filter
    if (this.pageData?.user) {
      const scope = localStorage.getItem('scion-scope-projects');
      if (scope === 'mine' || scope === 'shared') {
        this.projectScope = scope;
      }
    }

    // Set SSE scope to dashboard (project summaries).
    // This must happen before checking hydrated data because setScope clears
    // state maps when the scope changes (e.g. from project-detail to dashboard).
    stateManager.setScope({ type: 'dashboard' });

    // Use hydrated data from SSR if available, avoiding the initial fetch.
    // Only trust it when scope was previously null (initial SSR page load);
    // on client-side navigations the maps were just cleared by setScope above.
    // Skip hydrated data when a scope filter is active — SSR data is unfiltered.
    // Also require scope capabilities — without them the "New Project" button
    // won't render, so we must fetch from the API to get them.
    const hydratedProjects = stateManager.getProjects();
    const hydratedCaps = stateManager.getScopeCapabilities();
    if (hydratedProjects.length > 0 && hydratedCaps && this.projectScope === 'all') {
      this.projects = hydratedProjects;
      this.scopeCapabilities = hydratedCaps;
      this.loading = false;
      stateManager.seedProjects(this.projects);
    } else {
      void this.loadProjects();
    }

    // Listen for real-time project updates
    stateManager.addEventListener('projects-updated', this.boundOnProjectsUpdated as EventListener);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('projects-updated', this.boundOnProjectsUpdated as EventListener);
  }

  private onProjectsUpdated(): void {
    const updatedProjects = stateManager.getProjects();
    const deletedIds = stateManager.getDeletedProjectIds();

    const projectMap = new Map(this.projects.map((p) => [p.id, p]));

    // Remove deleted projects
    for (const id of deletedIds) {
      projectMap.delete(id);
    }

    // Merge updated/created projects
    for (const project of updatedProjects) {
      const existing = projectMap.get(project.id);
      // When a scope filter is active, only update projects already in the
      // filtered list — don't add new projects that weren't in the REST response.
      // The server-side filter is the source of truth for ownership/membership.
      if (!existing && this.projectScope !== 'all') {
        continue;
      }
      const merged = { ...existing, ...project } as Project;
      // Preserve _capabilities from existing state when the delta lacks them.
      if (!project._capabilities && existing?._capabilities) {
        merged._capabilities = existing._capabilities;
      }
      projectMap.set(project.id, merged);
    }

    this.projects = Array.from(projectMap.values());
  }

  private async loadProjects(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const url = this.projectScope !== 'all'
        ? `/api/v1/projects?scope=${this.projectScope}`
        : '/api/v1/projects';
      const response = await apiFetch(url);

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      const data = (await response.json()) as { projects?: Project[]; _capabilities?: Capabilities } | Project[];
      if (Array.isArray(data)) {
        this.projects = data;
        this.scopeCapabilities = undefined;
      } else {
        this.projects = data.projects || [];
        this.scopeCapabilities = data._capabilities;
      }

      // Seed stateManager so SSE delta merging has full baseline data
      // and so other pages sharing the same scope can reuse capabilities.
      stateManager.seedProjects(this.projects);
      if (this.scopeCapabilities) {
        stateManager.seedScopeCapabilities(this.scopeCapabilities);
      }
    } catch (err) {
      console.error('Failed to load projects:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load projects';
    } finally {
      this.loading = false;
    }
  }

  private onViewChange(e: CustomEvent<{ view: ViewMode }>): void {
    this.viewMode = e.detail.view;
  }

  private setScope(scope: 'all' | 'mine' | 'shared'): void {
    if (this.projectScope === scope) return;
    this.projectScope = scope;
    if (scope === 'all') {
      localStorage.removeItem('scion-scope-projects');
    } else {
      localStorage.setItem('scion-scope-projects', scope);
    }
    void this.loadProjects();
  }

  override render() {
    return html`
      <div class="header">
        <h1>Projects</h1>
        <div class="header-actions">
          ${this.pageData?.user ? html`
            <div class="scope-toggle">
              <button
                class=${this.projectScope === 'all' ? 'active' : ''}
                title="All projects"
                @click=${() => this.setScope('all')}
              >All</button>
              <button
                class=${this.projectScope === 'mine' ? 'active' : ''}
                title="Projects I own"
                @click=${() => this.setScope('mine')}
              >
                <sl-icon name="person"></sl-icon>
                Mine
              </button>
              <button
                class=${this.projectScope === 'shared' ? 'active' : ''}
                title="Projects shared with me"
                @click=${() => this.setScope('shared')}
              >
                <sl-icon name="people"></sl-icon>
                Shared
              </button>
            </div>
          ` : nothing}
          <scion-view-toggle
            .view=${this.viewMode}
            storageKey="scion-view-projects"
            @view-change=${this.onViewChange}
          ></scion-view-toggle>
          ${can(this.scopeCapabilities, 'create') ? html`
            <a href="/projects/new" style="text-decoration: none;">
              <sl-button variant="primary" size="small">
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                New Project
              </sl-button>
            </a>
          ` : nothing}
        </div>
      </div>

      ${this.loading ? this.renderLoading() : this.error ? this.renderError() : this.renderProjects()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading projects...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Projects</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadProjects()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderProjects() {
    if (this.projects.length === 0) {
      if (this.projectScope === 'mine') {
        return html`
          <div class="empty-state">
            <sl-icon name="person"></sl-icon>
            <h2>No Projects Found</h2>
            <p>You don't own any projects yet.</p>
          </div>
        `;
      }
      if (this.projectScope === 'shared') {
        return html`
          <div class="empty-state">
            <sl-icon name="people"></sl-icon>
            <h2>No Shared Projects</h2>
            <p>No projects have been shared with you yet.</p>
          </div>
        `;
      }
      return this.renderEmptyState();
    }

    return this.viewMode === 'grid' ? this.renderGrid(this.projects) : this.renderTable(this.projects);
  }

  private renderEmptyState() {
    return html`
      <div class="empty-state">
        <sl-icon name="folder2-open"></sl-icon>
        <h2>No Projects Found</h2>
        <p>
          Projects are project workspaces that contain your agents.${can(this.scopeCapabilities, 'create') ? ' Create your first project to get started, or run' : ' Run'}
          <code>scion init</code> in a project directory.
        </p>
        ${can(this.scopeCapabilities, 'create') ? html`
          <a href="/projects/new" style="text-decoration: none;">
            <sl-button variant="primary">
              <sl-icon slot="prefix" name="plus-lg"></sl-icon>
              Create Project
            </sl-button>
          </a>
        ` : nothing}
      </div>
    `;
  }

  private renderGrid(projects: Project[]) {
    return html`
      <div class="resource-grid">${projects.map((project) => this.renderProjectCard(project))}</div>
    `;
  }

  private renderProjectIcon() {
    return html`<sl-icon name="folder-fill"></sl-icon>`;
  }

  private renderLinkedBadge(project: Project) {
    if (project.projectType !== 'linked') return nothing;
    return html` <sl-tooltip content="Linked project"><sl-icon name="link-45deg" style="font-size: 0.875rem; vertical-align: middle; opacity: 0.7;"></sl-icon></sl-tooltip>`;
  }

  private renderProjectCard(project: Project) {
    return html`
      <a href="/projects/${project.id}" class="resource-card">
        <div class="project-header">
          <div>
            <h3 class="resource-name">
              ${this.renderProjectIcon()}
              ${project.name}${this.renderLinkedBadge(project)}
            </h3>
            <div class="project-path"><scion-git-remote-display .project=${project} stop-propagation></scion-git-remote-display></div>
          </div>
        </div>
        <div class="project-stats">
          <div class="stat">
            <span class="stat-label">Agents</span>
            <span class="stat-value">${project.agentCount}</span>
          </div>
          <div class="stat">
            <span class="stat-label">Owner</span>
            <span class="stat-value" style="font-size: 0.875rem; font-weight: 500;">
              ${project.ownerName || '—'}
            </span>
          </div>
        </div>
      </a>
    `;
  }

  private renderTable(projects: Project[]) {
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
            ${projects.map((project) => this.renderProjectRow(project))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderProjectRow(project: Project) {
    return html`
      <tr class="clickable" @click=${() => {
        window.history.pushState({}, '', `/projects/${project.id}`);
        window.dispatchEvent(new PopStateEvent('popstate'));
      }}>
        <td>
          <span class="name-cell">
            ${this.renderProjectIcon()}
            ${project.name}${this.renderLinkedBadge(project)}
          </span>
        </td>
        <td class="mono-cell"><scion-git-remote-display .project=${project} stop-propagation></scion-git-remote-display></td>
        <td>${project.agentCount}</td>
        <td class="hide-mobile">
          <span class="meta-text">${project.ownerName || '—'}</span>
        </td>
      </tr>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-projects': ScionPageProjects;
  }
}
