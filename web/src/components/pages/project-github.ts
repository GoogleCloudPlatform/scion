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
 * Project GitHub page (`/projects/:id/github`, M1.5).
 *
 * Hosts the unified project-scoped GitHub UI:
 * - Shared status header (connected repo, App handle, readiness/routing extension points)
 * - Tabs: Agent access and Connection (plus a clean extension point for M5.1's Automation tab)
 * - Disconnected projects land on Connection with other tabs disabled ("connect first")
 * - Hidden (renders 404) when the remote is not on the App host or the App is not configured
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, ProjectGitHubInfo } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import {
  fetchProjectGitHub,
  isProjectGitHubConnected,
  isProjectGitHubVisible,
} from '../../client/project-github.js';
import { current, projectTrail, projectsRoot, type Named } from '../../client/crumbs.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { projectDisplayName, projectLabelNow } from '../../client/project-label.js';
import '../shared/breadcrumb.js';
import '../shared/project-github-card.js';
import './not-found.js';

interface ProjectSummary {
  id: string;
  name: string;
  slug?: string;
}

export interface ProjectGitHubTabContext {
  projectId: string;
  project: ProjectSummary | null;
  info: ProjectGitHubInfo;
  onInfoChanged: (info: ProjectGitHubInfo) => void;
}

export interface ProjectGitHubTabExtension {
  id: string;
  label: string;
  /** Lower numbers appear first. Built-in tabs use 20 (agent-access) and 30 (connection). */
  order: number;
  /** Whether the tab requires an active GitHub App connection. Defaults to true. */
  requiresConnection?: boolean;
  /** Optional visibility gate (e.g. experiment flag for Automation in M5.1). */
  isEnabled?: () => boolean;
  /** Render the tab panel body. */
  render: (ctx: ProjectGitHubTabContext) => TemplateResult;
}

const registeredTabExtensions: ProjectGitHubTabExtension[] = [];

/**
 * Register an additional tab on `/projects/{id}/github` (extension point for M5.1 Automation).
 */
export function registerProjectGitHubTab(ext: ProjectGitHubTabExtension): void {
  const idx = registeredTabExtensions.findIndex((t) => t.id === ext.id);
  if (idx >= 0) {
    registeredTabExtensions[idx] = ext;
  } else {
    registeredTabExtensions.push(ext);
  }
}

/** Remove all registered tab extensions (for unit tests). */
export function clearProjectGitHubTabsForTests(): void {
  registeredTabExtensions.length = 0;
}

@customElement('scion-page-project-github')
export class ScionPageProjectGithub extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @property({ type: String })
  projectId = '';

  @state() private project: ProjectSummary | null = null;
  @state() private githubInfo: ProjectGitHubInfo | null = null;
  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private notFound = false;
  @state() private activeTab = 'agent-access';

  private requestedTabParam: string | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      flex-wrap: wrap;
      gap: 0.75rem;
      margin-bottom: 0.75rem;
    }

    .header sl-icon {
      font-size: 1.5rem;
      color: var(--scion-text);
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text);
      margin: 0;
    }

    .status-header {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 1rem 1.5rem;
      padding: 0.875rem 1.25rem;
      margin-bottom: 1.25rem;
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-lg);
    }

    .status-header-item {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.875rem;
      color: var(--scion-text);
    }

    .status-header-label {
      font-size: 0.75rem;
      color: var(--scion-text-muted);
    }

    .status-dot {
      display: inline-block;
      width: 10px;
      height: 10px;
      border-radius: var(--scion-radius-full, 9999px);
      flex-shrink: 0;
    }

    .status-dot.ok {
      background: var(--sl-color-success-600);
    }

    .status-dot.degraded {
      background: var(--sl-color-yellow-500);
    }

    .status-dot.error {
      background: var(--sl-color-danger-600);
    }

    .status-dot.disconnected {
      background: var(--scion-text-disabled);
    }

    .header-chip {
      font-size: 0.75rem;
      padding: 0.125rem 0.5rem;
      border-radius: var(--scion-radius-full, 9999px);
      background: var(--scion-bg-subtle);
      color: var(--scion-text-muted);
      border: 1px solid var(--scion-border);
      white-space: nowrap;
    }

    .tab-disabled-hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted);
      margin-left: 0.375rem;
    }

    .tab-panel-body {
      padding-top: 1rem;
    }

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 4rem 2rem;
      color: var(--scion-text-muted);
    }

    .loading-state sl-spinner {
      font-size: 2rem;
      margin-bottom: 1rem;
    }

    .error-state {
      text-align: center;
      padding: 3rem 2rem;
      background: var(--scion-surface);
      border: 1px solid var(--sl-color-danger-200);
      border-radius: var(--scion-radius-lg);
    }

    .error-state sl-icon {
      font-size: 3rem;
      color: var(--sl-color-danger-500);
      margin-bottom: 1rem;
    }

    .error-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text);
      margin: 0 0 0.5rem 0;
    }

    .error-details {
      font-family: var(--scion-font-mono);
      font-size: 0.875rem;
      background: var(--scion-bg-subtle);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius);
      color: var(--sl-color-danger-700);
      margin-bottom: 1rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    this.extractRouteParams();
    void this.loadData();
  }

  private extractRouteParams(): void {
    if (!this.projectId) {
      const path = this.pageData?.path || window.location.pathname;
      const match = path.match(/\/projects\/([^/]+)\/github/);
      if (match?.[1]) {
        try {
          this.projectId = decodeURIComponent(match[1]);
        } catch {
          this.error = 'Invalid project ID in URL';
        }
      }
    }

    const params = new URLSearchParams(window.location.search);
    this.requestedTabParam = params.get('tab');
  }

  private getTabs(): ProjectGitHubTabExtension[] {
    const builtIn: ProjectGitHubTabExtension[] = [
      {
        id: 'agent-access',
        label: 'Agent access',
        order: 20,
        requiresConnection: true,
        render: (ctx) => html`
          <scion-project-github-card
            .projectId=${ctx.projectId}
            .info=${ctx.info}
            section="permissions"
            @github-info-changed=${(e: CustomEvent<{ info: ProjectGitHubInfo }>) => {
              ctx.onInfoChanged(e.detail.info);
            }}
          ></scion-project-github-card>
        `,
      },
      {
        id: 'connection',
        label: 'Connection',
        order: 30,
        requiresConnection: false,
        render: (ctx) => html`
          <scion-project-github-card
            .projectId=${ctx.projectId}
            .info=${ctx.info}
            section="connection"
            @github-info-changed=${(e: CustomEvent<{ info: ProjectGitHubInfo }>) => {
              ctx.onInfoChanged(e.detail.info);
            }}
          ></scion-project-github-card>
        `,
      },
    ];

    const extras = registeredTabExtensions.filter((t) => !t.isEnabled || t.isEnabled());
    return [...extras, ...builtIn].sort((a, b) => a.order - b.order);
  }

  private resolveActiveTab(info: ProjectGitHubInfo, preferred?: string | null): string {
    const connected = isProjectGitHubConnected(info);
    if (!connected) {
      return 'connection';
    }
    const tabs = this.getTabs();
    if (preferred && tabs.some((t) => t.id === preferred)) {
      return preferred;
    }
    return tabs[0]?.id ?? 'agent-access';
  }

  private async loadData(): Promise<void> {
    if (!this.projectId) {
      this.error = 'No project ID found in URL';
      this.loading = false;
      return;
    }

    this.loading = true;
    this.error = null;
    this.notFound = false;

    try {
      const [projRes, ghInfo] = await Promise.all([
        apiFetch(`/api/v1/projects/${encodeURIComponent(this.projectId)}`),
        fetchProjectGitHub(this.projectId),
      ]);

      if (!this.isConnected) return;

      if (projRes.status === 404) {
        this.notFound = true;
        return;
      }
      if (!projRes.ok) {
        throw new Error(await extractApiError(projRes, `HTTP ${projRes.status}`));
      }

      this.project = (await projRes.json()) as ProjectSummary;

      if (!isProjectGitHubVisible(ghInfo)) {
        this.notFound = true;
        return;
      }

      this.githubInfo = ghInfo;
      this.activeTab = this.resolveActiveTab(ghInfo!, this.requestedTabParam);
      dispatchPageTitle(this, 'GitHub', this.project.name || this.projectId);
    } catch (err) {
      if (!this.isConnected) return;
      this.error = err instanceof Error ? err.message : 'Failed to load GitHub integration';
    } finally {
      this.loading = false;
    }
  }

  private handleInfoChanged(nextInfo: ProjectGitHubInfo): void {
    this.githubInfo = nextInfo;
    if (!isProjectGitHubVisible(nextInfo)) {
      this.notFound = true;
      return;
    }
    if (!isProjectGitHubConnected(nextInfo)) {
      this.activeTab = 'connection';
    }
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

  private renderStatusHeader(info: ProjectGitHubInfo): TemplateResult {
    const connected = isProjectGitHubConnected(info);
    const repo =
      info.remote.owner && info.remote.repo ? `${info.remote.owner}/${info.remote.repo}` : '';
    const statusState = info.status?.state;

    let dotClass = 'disconnected';
    let statusText = 'Not connected';
    if (connected) {
      switch (statusState) {
        case 'ok':
          dotClass = 'ok';
          statusText = 'Connected';
          break;
        case 'degraded':
          dotClass = 'degraded';
          statusText = 'Degraded';
          break;
        case 'error':
          dotClass = 'error';
          statusText = 'Error';
          break;
        default:
          dotClass = 'ok';
          statusText = 'Connected';
          break;
      }
    }

    return html`
      <div class="status-header" data-testid="github-status-header">
        <div class="status-header-item" data-testid="github-header-connection">
          <span class="status-dot ${dotClass}"></span>
          <strong>${statusText}</strong>
        </div>

        ${repo
          ? html`
              <div class="status-header-item" data-testid="github-header-repo">
                <span class="status-header-label">Repository</span>
                <code>${repo}</code>
              </div>
            `
          : nothing}
        ${info.app.slug
          ? html`
              <div class="status-header-item" data-testid="github-header-app">
                <span class="status-header-label">App</span>
                <span class="header-chip">@${info.app.slug}</span>
              </div>
            `
          : nothing}

        <!-- Extension points populated by M1.4 (readiness) and M5 (routing) -->
        <div class="status-header-item" data-testid="github-header-readiness" hidden></div>
        <div class="status-header-item" data-testid="github-header-routing" hidden></div>
      </div>
    `;
  }

  override render() {
    if (this.notFound) {
      return html`<scion-page-404></scion-page-404>`;
    }

    if (this.loading) {
      return html`
        <scion-breadcrumb .crumbs=${this.ancestorCrumbs(true)}></scion-breadcrumb>
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading GitHub integration...</p>
        </div>
      `;
    }

    if (this.error || !this.githubInfo) {
      return html`
        <scion-breadcrumb .crumbs=${this.ancestorCrumbs(false)}></scion-breadcrumb>
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load</h2>
          <div class="error-details">${this.error || 'Unable to load GitHub integration'}</div>
          <sl-button variant="primary" @click=${() => this.loadData()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    const info = this.githubInfo;
    const connected = isProjectGitHubConnected(info);
    const tabs = this.getTabs();
    const ctx: ProjectGitHubTabContext = {
      projectId: this.projectId,
      project: this.project,
      info,
      onInfoChanged: (next) => this.handleInfoChanged(next),
    };

    return html`
      <scion-breadcrumb
        .crumbs=${[...this.ancestorCrumbs(false), current('GitHub')]}
      ></scion-breadcrumb>

      <div class="header">
        <sl-icon name="github"></sl-icon>
        <h1>GitHub</h1>
      </div>

      ${this.renderStatusHeader(info)}

      <sl-tab-group
        data-testid="github-tabs"
        @sl-tab-show=${(e: CustomEvent<{ name: string }>) => {
          if (e.target !== e.currentTarget) return;
          const name = e.detail?.name;
          if (!name) return;
          const targetTab = tabs.find((t) => t.id === name);
          if (!targetTab) return;
          if (!connected && targetTab.requiresConnection !== false) return;
          this.activeTab = name;
        }}
      >
        ${tabs.map((tab) => {
          const disabled = !connected && tab.requiresConnection !== false;
          return html`
            <sl-tab
              slot="nav"
              panel=${tab.id}
              data-testid=${`github-tab-${tab.id}`}
              ?active=${this.activeTab === tab.id}
              ?disabled=${disabled}
            >
              ${tab.label}
              ${disabled ? html`<span class="tab-disabled-hint">(connect first)</span>` : nothing}
            </sl-tab>
          `;
        })}
        ${tabs.map(
          (tab) => html`
            <sl-tab-panel
              name=${tab.id}
              data-testid=${`github-panel-${tab.id}`}
              ?active=${this.activeTab === tab.id}
            >
              <div class="tab-panel-body">${tab.render(ctx)}</div>
            </sl-tab-panel>
          `
        )}
      </sl-tab-group>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-project-github': ScionPageProjectGithub;
  }
}
