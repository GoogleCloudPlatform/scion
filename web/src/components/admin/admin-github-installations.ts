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

import { apiFetch } from '../../client/api.js';

interface GitHubInstallationInfo {
  installation_id: number;
  account_login: string;
  account_type: string;
  app_id: number;
  repositories?: string[];
  status: string;
}

@customElement('scion-admin-github-installations')
export class ScionAdminGitHubInstallations extends LitElement {
  @property({ type: Boolean }) appConfigured = false;
  @property({ type: Number }) configSaveCounter = 0;

  @state() private installations: GitHubInstallationInfo[] = [];
  @state() private loading = false;
  @state() private discoverLoading = false;
  @state() private syncLoading = false;
  @state() private syncResult: string | null = null;
  @state() private syncError = false;

  private loadedOnce = false;

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

    .header-actions {
      display: flex;
      gap: 0.5rem;
      flex-wrap: wrap;
    }

    .status-message {
      font-size: 0.875rem;
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      margin-bottom: 1rem;
    }

    .status-message.success {
      background: var(--scion-success-bg, #dcfce7);
      color: var(--scion-success-text, #166534);
      border: 1px solid var(--scion-success-border, #86efac);
    }

    .status-message.error {
      background: var(--scion-error-bg, #fef2f2);
      color: var(--scion-error-text, #991b1b);
      border: 1px solid var(--scion-error-border, #fca5a5);
    }

    .loading-row {
      text-align: center;
      padding: 1rem;
    }

    .empty-hint {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0;
    }

    .installations-list {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
    }

    .installation-row {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.75rem;
      background: var(--scion-bg-subtle, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .installation-info {
      flex: 1;
      min-width: 0;
    }

    .installation-meta {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .status-pill {
      font-size: 0.6875rem;
      padding: 0.125rem 0.5rem;
      border-radius: var(--scion-radius-full, 9999px);
    }

    .status-pill.active {
      background: var(--scion-badge-success-bg, #dcfce7);
      color: var(--scion-badge-success-text, #166534);
    }

    .status-pill.inactive {
      background: var(--scion-error-bg, #fef2f2);
      color: var(--scion-error-text, #991b1b);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    if (!this.loadedOnce) {
      this.loadedOnce = true;
      void this.loadInstallations();
    }
  }

  override updated(changed: Map<string, unknown>): void {
    super.updated(changed);
    const savedWhileConfigured =
      changed.has('configSaveCounter') &&
      changed.get('configSaveCounter') !== undefined &&
      this.appConfigured;
    if (savedWhileConfigured) {
      void this.loadInstallations();
    }
  }

  async loadInstallations(): Promise<void> {
    this.loading = true;
    try {
      const res = await apiFetch('/api/v1/github-app/installations');
      if (res.ok) {
        const data = (await res.json()) as { installations?: GitHubInstallationInfo[] };
        this.installations = data.installations || [];
      }
    } catch {
      // Non-critical
    } finally {
      this.loading = false;
    }
  }

  private async handleDiscover(): Promise<void> {
    this.discoverLoading = true;
    this.syncResult = null;
    this.syncError = false;
    try {
      const res = await apiFetch('/api/v1/github-app/installations/discover', { method: 'POST' });
      if (res.ok) {
        const data = (await res.json()) as { total: number };
        this.syncResult = `Discovered ${data.total} installation(s) from GitHub.`;
        await this.loadInstallations();
      } else {
        const err = (await res.json().catch(() => ({}))) as { message?: string };
        this.syncError = true;
        this.syncResult = `Discovery failed: ${err.message || res.statusText}`;
      }
    } catch {
      this.syncError = true;
      this.syncResult = 'Discovery failed: network error';
    } finally {
      this.discoverLoading = false;
    }
  }

  private async handleSyncPermissions(): Promise<void> {
    this.syncLoading = true;
    this.syncResult = null;
    this.syncError = false;
    try {
      const res = await apiFetch('/api/v1/github-app/sync-permissions', { method: 'POST' });
      if (res.ok) {
        const data = (await res.json()) as {
          affected_projects?: number;
          app_permissions?: Record<string, string>;
        };
        const perms = data.app_permissions
          ? Object.entries(data.app_permissions)
              .map(([k, v]) => `${k}:${v}`)
              .join(', ')
          : 'none';
        this.syncResult = `Permissions synced. App permissions: ${perms}. ${data.affected_projects || 0} project(s) affected.`;
      } else {
        const err = (await res.json().catch(() => ({}))) as { message?: string };
        this.syncError = true;
        this.syncResult = `Sync failed: ${err.message || res.statusText}`;
      }
    } catch {
      this.syncError = true;
      this.syncResult = 'Sync failed: network error';
    } finally {
      this.syncLoading = false;
    }
  }

  override render() {
    return html`
      <div class="section">
        <div class="section-header">
          <h3 class="section-title">Installations</h3>
          <div class="header-actions">
            <sl-button
              size="small"
              variant="default"
              ?loading=${this.discoverLoading}
              ?disabled=${!this.appConfigured && this.installations.length === 0}
              @click=${() => this.handleDiscover()}
            >
              <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
              Discover from GitHub
            </sl-button>
            <sl-button
              size="small"
              variant="default"
              ?loading=${this.syncLoading}
              ?disabled=${!this.appConfigured && this.installations.length === 0}
              @click=${() => this.handleSyncPermissions()}
            >
              <sl-icon slot="prefix" name="shield-check"></sl-icon>
              Sync Permissions
            </sl-button>
          </div>
        </div>

        ${this.syncResult
          ? html`<div class="status-message ${this.syncError ? 'error' : 'success'}">
              ${this.syncResult}
            </div>`
          : nothing}
        ${this.loading
          ? html`<div class="loading-row"><sl-spinner></sl-spinner></div>`
          : this.installations.length === 0
            ? html`<p class="empty-hint">
                ${this.appConfigured
                  ? 'No installations found. Click "Discover from GitHub" to sync.'
                  : 'Configure the GitHub App above to discover and manage installations.'}
              </p>`
            : html`
                <div class="installations-list">
                  ${this.installations.map(
                    (inst) => html`
                      <div class="installation-row">
                        <sl-icon
                          name=${inst.account_type === 'Organization' ? 'building' : 'person'}
                        ></sl-icon>
                        <div class="installation-info">
                          <strong>${inst.account_login}</strong>
                          <div class="installation-meta">
                            ${inst.account_type} · ${inst.repositories?.length || 0} repos · ID:
                            ${inst.installation_id}
                          </div>
                        </div>
                        <span
                          class="status-pill ${inst.status === 'active' ? 'active' : 'inactive'}"
                        >
                          ${inst.status}
                        </span>
                      </div>
                    `
                  )}
                </div>
              `}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-admin-github-installations': ScionAdminGitHubInstallations;
  }
}
