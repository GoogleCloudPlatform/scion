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
 * GitHub App post-installation setup page
 *
 * Shown after a user installs the GitHub App. Displays:
 * - A button to create a new git-repository-backed project
 * - A list of existing projects with GitHub remotes and their installation status
 * - Auto-discovers and associates installations with matching projects
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch } from '../../client/api.js';

import type { PageData, Project, GitHubAppProjectStatus } from '../../shared/types.js';

type GitHubProject = Project;

@customElement('scion-page-github-app-setup')
export class ScionPageGitHubAppSetup extends LitElement {
  @property({ type: Object })
  pageData?: PageData;

  @state()
  private loading = true;

  @state()
  private discovering = false;

  @state()
  private projects: GitHubProject[] = [];

  @state()
  private error: string | null = null;

  @state()
  private discoveryResult: { total: number; matched: number } | null = null;

  @state()
  private checkingProjects = new Set<string>();

  override connectedCallback(): void {
    super.connectedCallback();

    this.initPage();
  }

  private async initPage(): Promise<void> {
    this.loading = true;
    try {
      // Run discovery first to sync installations and auto-match projects,
      // then load projects to show the updated state.
      await this.discoverInstallations();
      await this.loadProjects();
    } catch (err) {
      console.error('Failed to initialize setup page:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load page data';
    } finally {
      this.loading = false;
    }
  }

  private async discoverInstallations(): Promise<void> {
    this.discovering = true;
    try {
      const res = await apiFetch('/api/v1/github-app/installations/discover', {
        method: 'POST',
      });
      if (res.ok) {
        const data = (await res.json()) as {
          installations: Array<{ matched_projects?: string[] }>;
          total: number;
        };
        let matched = 0;
        for (const inst of data.installations) {
          if (inst.matched_projects?.length) {
            matched += inst.matched_projects.length;
          }
        }
        this.discoveryResult = { total: data.total, matched };
      }
    } catch (err) {
      console.warn('Installation discovery failed:', err);
      // Non-fatal — continue loading projects
    } finally {
      this.discovering = false;
    }
  }

  private async loadProjects(): Promise<void> {
    const res = await apiFetch('/api/v1/projects?mine=true');
    if (!res.ok) {
      throw new Error(`Failed to fetch projects: HTTP ${res.status}`);
    }
    const data = (await res.json()) as { projects: Project[] };
    // Filter to projects that have a GitHub remote URL
    this.projects = (data.projects || []).filter(
      (p) => p.gitRemote && this.isGitHubUrl(p.gitRemote)
    );
  }

  private isGitHubUrl(url: string): boolean {
    return /github\.com/i.test(url);
  }

  private async checkProjectStatus(project: GitHubProject): Promise<void> {
    if (!project.githubInstallationId) return;

    this.checkingProjects = new Set([...this.checkingProjects, project.id]);
    try {
      const res = await apiFetch(`/api/v1/projects/${project.id}/github-status`, {
        method: 'POST',
      });
      if (res.ok) {
        const data = (await res.json()) as {
          status?: GitHubAppProjectStatus;
        };
        // Update the project in our list
        this.projects = this.projects.map((p) =>
          p.id === project.id
            ? { ...p, githubAppStatus: data.status || p.githubAppStatus }
            : p
        );
      }
    } catch (err) {
      console.error('Failed to check project status:', err);
    } finally {
      const next = new Set(this.checkingProjects);
      next.delete(project.id);
      this.checkingProjects = next;
    }
  }

  private async checkAllProjects(): Promise<void> {
    const projectsWithInstallation = this.projects.filter(
      (p) => p.githubInstallationId
    );
    await Promise.allSettled(
      projectsWithInstallation.map((p) => this.checkProjectStatus(p))
    );
  }

  private navigateTo(path: string): void {
    window.history.pushState({}, '', path);
    window.dispatchEvent(new PopStateEvent('popstate'));
  }

  private renderStatusBadge(project: GitHubProject) {
    if (!project.githubInstallationId) {
      return html`<sl-badge variant="neutral">No Installation</sl-badge>`;
    }

    const status = project.githubAppStatus;
    if (!status) {
      return html`<sl-badge variant="neutral">Unchecked</sl-badge>`;
    }

    switch (status.state) {
      case 'ok':
        return html`<sl-badge variant="success">Connected</sl-badge>`;
      case 'degraded':
        return html`<sl-badge variant="warning">Degraded</sl-badge>`;
      case 'error':
        return html`<sl-badge variant="danger">Error</sl-badge>`;
      default:
        return html`<sl-badge variant="neutral">Unchecked</sl-badge>`;
    }
  }

  private extractRepoName(url: string): string {
    try {
      const cleaned = url
        .replace(/^(https?:\/\/|ssh:\/\/|git:\/\/|git@)/, '')
        .replace(':', '/')
        .replace(/\.git$/, '');
      const parts = cleaned.split('/');
      if (parts.length >= 2) {
        return `${parts[parts.length - 2]}/${parts[parts.length - 1]}`;
      }
      return parts[parts.length - 1] || url;
    } catch {
      return url;
    }
  }

  static override styles = css`
    :host {
      display: block;
    }

    .page-header {
      margin-bottom: 1.5rem;
    }

    .page-header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .page-header h1 sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }

    .page-header p {
      color: var(--scion-text-muted, #64748b);
      margin: 0;
      font-size: 0.875rem;
    }

    .success-banner {
      background: var(--sl-color-success-50, #f0fdf4);
      border: 1px solid var(--sl-color-success-200, #bbf7d0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      margin-bottom: 1.5rem;
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      color: var(--sl-color-success-700, #15803d);
      font-size: 0.875rem;
    }

    .success-banner sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .error-banner {
      background: var(--sl-color-danger-50, #fef2f2);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      margin-bottom: 1.5rem;
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      color: var(--sl-color-danger-700, #b91c1c);
      font-size: 0.875rem;
    }

    .error-banner sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .actions-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .actions-card h2 {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .actions-card p {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0 0 1rem 0;
    }

    .projects-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
    }

    .projects-card h2 {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
      display: flex;
      align-items: center;
      justify-content: space-between;
    }

    .projects-card .subtitle {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0 0 1rem 0;
    }

    .project-list {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
    }

    .project-item {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0.75rem 1rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-bg-subtle, #f8fafc);
    }

    .project-info {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
      min-width: 0;
    }

    .project-name {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      font-size: 0.875rem;
    }

    .project-repo {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.75rem;
      font-family: var(--scion-font-mono, monospace);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .project-status {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .project-actions {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      flex-shrink: 0;
    }

    .empty-state {
      text-align: center;
      padding: 2rem 1rem;
      color: var(--scion-text-muted, #64748b);
    }

    .empty-state sl-icon {
      font-size: 2rem;
      margin-bottom: 0.75rem;
      display: block;
    }

    .empty-state p {
      margin: 0 0 0.5rem 0;
      font-size: 0.875rem;
    }

    .loading-state {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 3rem;
      gap: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }
  `;

  override render() {
    if (this.loading) {
      return html`
        <div class="page-header">
          <h1>
            <sl-icon name="github"></sl-icon>
            GitHub App Setup
          </h1>
        </div>
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <span>${this.discovering ? 'Discovering installations...' : 'Loading...'}</span>
        </div>
      `;
    }

    return html`
      <div class="page-header">
        <h1>
          <sl-icon name="github"></sl-icon>
          GitHub App Setup
        </h1>
        <p>Your GitHub App installation has been recorded. Set up projects for your repositories below.</p>
      </div>

      ${this.error
        ? html`
            <div class="error-banner">
              <sl-icon name="exclamation-triangle"></sl-icon>
              <span>${this.error}</span>
            </div>
          `
        : nothing}

      ${this.discoveryResult && this.discoveryResult.matched > 0
        ? html`
            <div class="success-banner">
              <sl-icon name="check-circle"></sl-icon>
              <span>
                Auto-matched ${this.discoveryResult.matched}
                project${this.discoveryResult.matched !== 1 ? 's' : ''} with GitHub App
                installation${this.discoveryResult.total !== 1 ? 's' : ''}.
              </span>
            </div>
          `
        : nothing}

      <div class="actions-card">
        <h2>Get Started</h2>
        <p>Create a new project linked to a GitHub repository to start running agents.</p>
        <sl-button
          variant="primary"
          @click=${() => this.navigateTo('/projects/new')}
        >
          <sl-icon slot="prefix" name="folder-plus"></sl-icon>
          Create New Project
        </sl-button>
      </div>

      <div class="projects-card">
        <h2>
          <span>GitHub Projects</span>
          ${this.projects.some((p) => p.githubInstallationId)
            ? html`
                <sl-button
                  size="small"
                  variant="default"
                  @click=${() => this.checkAllProjects()}
                  ?disabled=${this.checkingProjects.size > 0}
                >
                  <sl-icon slot="prefix" name="arrow-repeat"></sl-icon>
                  Check All
                </sl-button>
              `
            : nothing}
        </h2>
        <p class="subtitle">
          Projects linked to GitHub repositories.
          ${this.projects.length > 0
            ? 'Verify installations are working or configure them in project settings.'
            : ''}
        </p>

        ${this.projects.length > 0
          ? html`
              <div class="project-list">
                ${this.projects.map((project) => this.renderProjectItem(project))}
              </div>
            `
          : html`
              <div class="empty-state">
                <sl-icon name="folder-x"></sl-icon>
                <p>No projects with GitHub repositories found.</p>
                <p>Create a new project to get started.</p>
              </div>
            `}
      </div>
    `;
  }

  private renderProjectItem(project: GitHubProject) {
    const checking = this.checkingProjects.has(project.id);

    return html`
      <div class="project-item">
        <div class="project-info">
          <span class="project-name">${project.name}</span>
          <span class="project-repo">${this.extractRepoName(project.gitRemote || '')}</span>
        </div>
        <div class="project-actions">
          <div class="project-status">
            ${checking
              ? html`<sl-spinner style="font-size: 1rem;"></sl-spinner>`
              : this.renderStatusBadge(project)}
          </div>
          ${project.githubInstallationId && !checking
            ? html`
                <sl-button
                  size="small"
                  variant="text"
                  @click=${() => this.checkProjectStatus(project)}
                  title="Verify installation"
                >
                  <sl-icon name="arrow-repeat"></sl-icon>
                </sl-button>
              `
            : nothing}
          <sl-button
            size="small"
            variant="default"
            @click=${() => this.navigateTo(`/projects/${project.id}/settings`)}
          >
            <sl-icon slot="prefix" name="gear"></sl-icon>
            Settings
          </sl-button>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-github-app-setup': ScionPageGitHubAppSetup;
  }
}
