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
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';

interface AssociationReportEntry {
  project_id: string;
  project_name: string;
  git_remote: string;
  installation_id: number;
  authorized: boolean;
  basis?: string;
  write_allowed: boolean;
  error_code?: string;
  reason?: string;
}

interface AssociationReportResponse {
  associations: AssociationReportEntry[];
  total: number;
  unauthorized: number;
  generated_at?: string;
  truncated?: boolean;
}

@customElement('scion-admin-github-associations')
export class ScionAdminGitHubAssociations extends LitElement {
  @state() private entries: AssociationReportEntry[] = [];
  @state() private loading = true;
  @state() private refreshing = false;
  @state() private error: string | null = null;
  @state() private truncated = false;
  @state() private generatedAt: string | null = null;

  static override styles = css`
    :host {
      display: block;
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

    .loading-row {
      text-align: center;
      padding: 1.5rem;
    }

    .empty-hint {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0;
    }

    .meta-footer {
      margin-top: 0.75rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
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

    .status-message.warning {
      background: var(--scion-warning-bg, #fefce8);
      color: var(--scion-warning-text, #854d0e);
      border: 1px solid var(--scion-warning-border, #fde047);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadReport(false);
  }

  async loadReport(refresh: boolean): Promise<void> {
    if (refresh) {
      this.refreshing = true;
    } else {
      this.loading = true;
    }
    this.error = null;
    try {
      const url = refresh
        ? '/api/v1/github-app/installations/association-report?refresh=true'
        : '/api/v1/github-app/installations/association-report';
      const res = await apiFetch(url);
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to load project associations');
        return;
      }
      const data = (await res.json()) as AssociationReportResponse;
      this.entries = data.associations || [];
      this.truncated = Boolean(data.truncated);
      this.generatedAt = data.generated_at || null;
    } catch {
      this.error = 'Failed to load project associations';
    } finally {
      this.loading = false;
      this.refreshing = false;
    }
  }

  private formatRepo(gitRemote: string): string {
    if (!gitRemote) return '—';
    const cleaned = gitRemote
      .replace(/^(https?:\/\/|ssh:\/\/|git:\/\/|git@)/, '')
      .replace(':', '/')
      .replace(/\.git$/, '');
    const parts = cleaned.split('/');
    if (parts.length >= 2) {
      return `${parts[parts.length - 2]}/${parts[parts.length - 1]}`;
    }
    return gitRemote;
  }

  override render() {
    return html`
      <div class="section">
        <div class="section-header">
          <h3 class="section-title">Project Associations</h3>
          <sl-button
            size="small"
            variant="default"
            ?loading=${this.refreshing}
            ?disabled=${this.loading}
            @click=${() => this.loadReport(true)}
          >
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Refresh
          </sl-button>
        </div>

        ${this.error ? html`<div class="status-message error">${this.error}</div>` : nothing}
        ${this.truncated
          ? html`<div class="status-message warning">
              Report truncated: only the first subset of projects is shown.
            </div>`
          : nothing}
        ${this.loading
          ? html`<div class="loading-row"><sl-spinner></sl-spinner></div>`
          : this.entries.length === 0
            ? html`<p class="empty-hint">
                No projects are associated with a GitHub App installation.
              </p>`
            : html`
                <div class="table-wrap">
                  <table>
                    <thead>
                      <tr>
                        <th>Project</th>
                        <th>Repository</th>
                        <th>Basis</th>
                        <th>Write</th>
                        <th>Status</th>
                      </tr>
                    </thead>
                    <tbody>
                      ${this.entries.map(
                        (entry) => html`
                          <tr>
                            <td>
                              <a href="/projects/${encodeURIComponent(entry.project_id)}/settings"
                                >${entry.project_name || entry.project_id}</a
                              >
                            </td>
                            <td class="mono">${this.formatRepo(entry.git_remote)}</td>
                            <td>${entry.basis || '—'}</td>
                            <td>
                              ${entry.authorized
                                ? entry.write_allowed
                                  ? 'yes'
                                  : 'read-only'
                                : '—'}
                            </td>
                            <td>
                              ${entry.authorized
                                ? html`<sl-badge variant="success">authorized</sl-badge>`
                                : html`<sl-badge variant="danger"
                                    >${entry.reason || entry.error_code || 'unauthorized'}</sl-badge
                                  >`}
                            </td>
                          </tr>
                        `
                      )}
                    </tbody>
                  </table>
                </div>
                ${this.generatedAt
                  ? html`<div class="meta-footer">Updated ${this.generatedAt}</div>`
                  : nothing}
              `}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-admin-github-associations': ScionAdminGitHubAssociations;
  }
}
