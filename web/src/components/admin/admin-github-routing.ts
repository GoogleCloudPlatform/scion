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

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError, parseApiError } from '../../client/api.js';
import { FEATURE_FLAGS_CHANGED_EVENT, githubTriggersEnabled } from '../../utils/feature-flags.js';

export interface GitHubRepoRoutingProject {
  id: string;
  name: string;
  has_github_triggers: boolean;
}

export type GitHubRepoRoutingStatus = 'ok' | 'stale_primary' | 'none';

export interface GitHubRepoRoutingEntry {
  installation_id: number;
  repo: string;
  projects: GitHubRepoRoutingProject[];
  primary_project_id: string | null;
  status: GitHubRepoRoutingStatus;
}

function rowKey(entry: Pick<GitHubRepoRoutingEntry, 'installation_id' | 'repo'>): string {
  return `${entry.installation_id}:${entry.repo}`;
}

function splitOwnerRepo(fullRepo: string): [string, string] | null {
  const parts = fullRepo.split('/');
  if (parts.length !== 2 || !parts[0] || !parts[1]) {
    return null;
  }
  return [parts[0], parts[1]];
}

@customElement('scion-admin-github-routing')
export class ScionAdminGitHubRouting extends LitElement {
  @property({ type: Boolean }) appConfigured = false;
  @property({ type: Number }) configSaveCounter = 0;

  @state() private enabled = false;
  @state() private entries: GitHubRepoRoutingEntry[] = [];
  @state() private loading = false;
  @state() private refreshing = false;
  @state() private error: string | null = null;
  @state() private readOnly = false;
  @state() private selectedInstallation = '';
  @state() private savingKeys = new Set<string>();

  private loadedOnce = false;
  private flagsChangedHandler = () => {
    this.syncEnabledState();
  };

  static override styles = css`
    :host {
      display: block;
    }

    :host([hidden]) {
      display: none;
    }

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
    }

    .section-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.75rem;
      flex-wrap: wrap;
      margin: 0 0 1rem 0;
      padding-bottom: 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .section-title {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .header-controls {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      flex-wrap: wrap;
    }

    .installation-filter {
      min-width: 12rem;
    }

    .visually-hidden-label::part(form-control-label) {
      position: absolute;
      width: 1px;
      height: 1px;
      padding: 0;
      margin: -1px;
      overflow: hidden;
      clip: rect(0, 0, 0, 0);
      white-space: nowrap;
      border: 0;
    }

    .table-wrap {
      overflow-x: auto;
    }

    table {
      width: 100%;
      border-collapse: collapse;
    }

    th {
      text-align: left;
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.025em;
      padding: 0.625rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    td {
      padding: 0.625rem 0.75rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }

    td a {
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
      font-weight: 500;
    }

    td a:hover {
      text-decoration: underline;
    }

    .mono {
      font-family: var(--sl-font-mono, monospace);
      font-size: 0.8125rem;
    }

    .repo-meta {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.125rem;
    }

    .projects-list {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.375rem 0.75rem;
    }

    .project-item {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
    }

    .primary-select {
      min-width: 13rem;
    }

    .status-cell {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.375rem;
    }

    .status-hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
    }

    .loading-row {
      text-align: center;
      padding: 1.5rem;
    }

    .empty-hint {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0;
    }

    .status-message {
      font-size: 0.875rem;
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      margin-bottom: 1rem;
    }

    .status-message.error {
      background: var(--scion-error-bg, #fef2f2);
      color: var(--scion-error-text, #991b1b);
      border: 1px solid var(--scion-error-border, #fca5a5);
    }

    .status-message.info {
      background: var(--scion-bg-subtle, #f8fafc);
      color: var(--scion-text-muted, #64748b);
      border: 1px solid var(--scion-border, #e2e8f0);
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.flagsChangedHandler);
    this.syncEnabledState();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.flagsChangedHandler);
  }

  override updated(changed: Map<string, unknown>): void {
    super.updated(changed);
    const savedWhileConfigured =
      this.enabled &&
      changed.has('configSaveCounter') &&
      changed.get('configSaveCounter') !== undefined &&
      this.appConfigured;
    if (savedWhileConfigured) {
      void this.loadRouting({ refresh: true });
    }
  }

  private syncEnabledState(): void {
    const nextEnabled = githubTriggersEnabled();
    this.enabled = nextEnabled;
    this.hidden = !nextEnabled;
    if (nextEnabled && !this.loadedOnce) {
      this.loadedOnce = true;
      void this.loadRouting();
    } else if (!nextEnabled) {
      this.loadedOnce = false;
    }
  }

  async loadRouting(options?: { refresh?: boolean; preserveError?: boolean }): Promise<void> {
    if (!this.enabled) return;
    const isRefresh = Boolean(options?.refresh);
    if (isRefresh) {
      this.refreshing = true;
    } else {
      this.loading = true;
    }
    if (!options?.preserveError) {
      this.error = null;
    }
    try {
      const res = await apiFetch('/api/v1/github-app/repo-routing', {
        suppressAccessDeniedToast: true,
      });
      if (res.status === 403) {
        this.readOnly = true;
        this.error = null;
        return;
      }
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to load repository routing');
        return;
      }
      const data = (await res.json()) as GitHubRepoRoutingEntry[];
      this.entries = Array.isArray(data) ? data : [];
      if (
        this.selectedInstallation &&
        !this.entries.some((e) => String(e.installation_id) === this.selectedInstallation)
      ) {
        this.selectedInstallation = '';
      }
    } catch {
      this.error = 'Failed to load repository routing';
    } finally {
      this.loading = false;
      this.refreshing = false;
    }
  }

  private get installationIds(): number[] {
    const ids = new Set<number>();
    for (const entry of this.entries) {
      ids.add(entry.installation_id);
    }
    return Array.from(ids).sort((a, b) => a - b);
  }

  private get filteredEntries(): GitHubRepoRoutingEntry[] {
    if (!this.selectedInstallation) {
      return this.entries;
    }
    const targetId = Number(this.selectedInstallation);
    return this.entries.filter((e) => e.installation_id === targetId);
  }

  private handleInstallationFilterChange(e: Event): void {
    const target = e.currentTarget as HTMLElement & { value?: string };
    this.selectedInstallation = typeof target.value === 'string' ? target.value : '';
  }

  private async handlePrimaryChange(entry: GitHubRepoRoutingEntry, e: Event): Promise<void> {
    if (this.readOnly) return;
    const target = e.currentTarget as HTMLElement & { value?: string };
    const nextPrimary = typeof target.value === 'string' ? target.value : '';
    const currentPrimary = entry.primary_project_id ?? '';
    if (nextPrimary === currentPrimary) return;

    const parts = splitOwnerRepo(entry.repo);
    if (!parts) {
      this.error = `Invalid repository format: ${entry.repo}`;
      target.value = currentPrimary;
      return;
    }
    const [owner, repo] = parts;
    const key = rowKey(entry);
    const nextSaving = new Set(this.savingKeys);
    nextSaving.add(key);
    this.savingKeys = nextSaving;
    this.error = null;

    try {
      const encodedPath = `/api/v1/github-app/repo-routing/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}`;
      if (nextPrimary === '') {
        const res = await apiFetch(
          `${encodedPath}?installation=${encodeURIComponent(String(entry.installation_id))}`,
          {
            method: 'DELETE',
            suppressAccessDeniedToast: true,
          }
        );
        if (res.status === 403) {
          this.readOnly = true;
          target.value = currentPrimary;
          return;
        }
        if (!res.ok) {
          this.error = await extractApiError(res, 'Failed to clear primary project');
          target.value = currentPrimary;
          return;
        }
        this.entries =
          entry.projects.length === 0
            ? this.entries.filter((item) => rowKey(item) !== key)
            : this.entries.map((item) =>
                rowKey(item) === key ? { ...item, primary_project_id: null, status: 'none' } : item
              );
        return;
      }

      const res = await apiFetch(encodedPath, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          installation_id: entry.installation_id,
          primary_project_id: nextPrimary,
        }),
        suppressAccessDeniedToast: true,
      });

      if (res.status === 403) {
        this.readOnly = true;
        target.value = currentPrimary;
        return;
      }

      if (res.status === 409) {
        const errInfo = await parseApiError(
          res,
          'Selected project is no longer a candidate for this repository.'
        );
        this.error =
          errInfo.code === 'not_a_candidate'
            ? errInfo.message
            : errInfo.message || 'Selected project is no longer a candidate for this repository.';
        target.value = currentPrimary;
        await this.loadRouting({ refresh: true, preserveError: true });
        return;
      }

      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to update primary project');
        target.value = currentPrimary;
        return;
      }

      const updated = (await res.json().catch(() => null)) as GitHubRepoRoutingEntry | null;
      this.entries = this.entries.map((item) => {
        if (rowKey(item) !== key) return item;
        if (updated && typeof updated === 'object' && 'repo' in updated) {
          return updated;
        }
        return { ...item, primary_project_id: nextPrimary, status: 'ok' };
      });
    } catch {
      this.error = 'Failed to save repository routing';
      target.value = currentPrimary;
    } finally {
      const done = new Set(this.savingKeys);
      done.delete(key);
      this.savingKeys = done;
    }
  }

  private primaryProjectLabel(entry: GitHubRepoRoutingEntry): string {
    if (!entry.primary_project_id) {
      return '— fan out to all —';
    }
    const found = entry.projects.find((p) => p.id === entry.primary_project_id);
    if (found) {
      return found.name || found.id;
    }
    return `${entry.primary_project_id} (stale)`;
  }

  private renderStatus(entry: GitHubRepoRoutingEntry) {
    const multiProject = entry.projects.length > 1 && entry.status !== 'ok';
    return html`
      <div class="status-cell">
        ${entry.status === 'ok'
          ? html`<sl-badge variant="success" data-routing-status="ok">ok</sl-badge>`
          : entry.status === 'stale_primary'
            ? html`<sl-badge variant="danger" data-routing-status="stale_primary"
                >stale_primary</sl-badge
              >`
            : html`<sl-badge variant="neutral" data-routing-status="none">none</sl-badge>`}
        ${multiProject
          ? html`<sl-badge variant="warning" data-multi-project-warning>multi-project</sl-badge>`
          : nothing}
      </div>
      ${entry.status === 'stale_primary'
        ? html`<div class="status-hint">
            Primary project is no longer a candidate; events fan out to all candidates.
          </div>`
        : nothing}
    `;
  }

  private renderPrimaryCell(entry: GitHubRepoRoutingEntry) {
    if (this.readOnly) {
      return html`<span data-primary-readonly>${this.primaryProjectLabel(entry)}</span>`;
    }

    const key = rowKey(entry);
    const currentValue = entry.primary_project_id ?? '';
    const hasStaleSelection =
      Boolean(entry.primary_project_id) &&
      !entry.projects.some((p) => p.id === entry.primary_project_id);

    return html`
      <sl-select
        class="primary-select visually-hidden-label"
        size="small"
        label=${`Primary project for ${entry.repo}`}
        data-routing-select=${key}
        .value=${currentValue}
        ?disabled=${this.savingKeys.has(key)}
        @sl-change=${(e: Event) => this.handlePrimaryChange(entry, e)}
      >
        <sl-option value="">— fan out to all —</sl-option>
        ${hasStaleSelection && entry.primary_project_id
          ? html`<sl-option value=${entry.primary_project_id} disabled>
              ${entry.primary_project_id} (stale)
            </sl-option>`
          : nothing}
        ${entry.projects.map(
          (proj) => html`<sl-option value=${proj.id}>${proj.name || proj.id}</sl-option>`
        )}
      </sl-select>
    `;
  }

  override render() {
    if (!this.enabled) {
      return nothing;
    }

    const installations = this.installationIds;
    const rows = this.filteredEntries;

    return html`
      <div class="section">
        <div class="section-header">
          <h3 class="section-title">Repository Routing</h3>
          <div class="header-controls">
            ${installations.length > 0
              ? html`
                  <sl-select
                    class="installation-filter visually-hidden-label"
                    size="small"
                    label="Filter by installation"
                    data-installation-filter
                    .value=${this.selectedInstallation}
                    @sl-change=${(e: Event) => this.handleInstallationFilterChange(e)}
                  >
                    <sl-option value="">All installations</sl-option>
                    ${installations.map(
                      (id) => html`<sl-option value=${String(id)}>Installation #${id}</sl-option>`
                    )}
                  </sl-select>
                `
              : nothing}
            <sl-button
              size="small"
              variant="default"
              ?loading=${this.refreshing}
              ?disabled=${this.loading}
              @click=${() => this.loadRouting({ refresh: true })}
            >
              <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
              Refresh
            </sl-button>
          </div>
        </div>

        ${this.readOnly
          ? html`<div class="status-message info" data-routing-readonly>
              <sl-icon name="lock"></sl-icon>
              <span>
                Read-only: requires <code>hub.github_app.update</code> permission to change
                repository routing.
              </span>
            </div>`
          : nothing}
        ${this.error
          ? html`<div class="status-message error" data-routing-error>${this.error}</div>`
          : nothing}
        ${this.loading
          ? html`<div class="loading-row"><sl-spinner></sl-spinner></div>`
          : rows.length === 0
            ? html`<p class="empty-hint">
                No repositories are associated with a GitHub App installation.
              </p>`
            : html`
                <div class="table-wrap">
                  <table>
                    <thead>
                      <tr>
                        <th>Repository</th>
                        <th>Projects</th>
                        <th>Primary project</th>
                        <th>Status</th>
                      </tr>
                    </thead>
                    <tbody>
                      ${rows.map(
                        (entry) => html`
                          <tr data-routing-row=${rowKey(entry)}>
                            <td>
                              <div class="mono">${entry.repo}</div>
                              ${installations.length > 1
                                ? html`<div class="repo-meta">
                                    Installation #${entry.installation_id}
                                  </div>`
                                : nothing}
                            </td>
                            <td>
                              ${entry.projects.length === 0
                                ? html`<span class="empty-hint">—</span>`
                                : html`
                                    <div class="projects-list">
                                      ${entry.projects.map(
                                        (proj) => html`
                                          <span class="project-item" data-project-id=${proj.id}>
                                            <a
                                              href="/projects/${encodeURIComponent(
                                                proj.id
                                              )}/settings"
                                              >${proj.name || proj.id}</a
                                            >
                                            ${proj.has_github_triggers
                                              ? html`<sl-badge
                                                  variant="primary"
                                                  pill
                                                  data-has-github-triggers
                                                  >triggers</sl-badge
                                                >`
                                              : nothing}
                                          </span>
                                        `
                                      )}
                                    </div>
                                  `}
                            </td>
                            <td>${this.renderPrimaryCell(entry)}</td>
                            <td>${this.renderStatus(entry)}</td>
                          </tr>
                        `
                      )}
                    </tbody>
                  </table>
                </div>
              `}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-admin-github-routing': ScionAdminGitHubRouting;
  }
}
