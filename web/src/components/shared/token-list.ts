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
 * Shared Token List Component
 *
 * Full CRUD component for user access tokens. Renders a table with
 * create, revoke, and delete actions. Shows a one-time token display
 * modal after creation with a copy button.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import type { Project } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { resourceStyles } from './resource-styles.js';

interface AccessToken {
  id: string;
  name: string;
  prefix: string;
  projectId: string;
  scopes: string[];
  revoked: boolean;
  expiresAt?: string | null;
  lastUsed?: string | null;
  created: string;
}

const AVAILABLE_SCOPES = [
  { value: 'project:read', label: 'project:read', description: 'Read project metadata' },
  { value: 'agent:dispatch', label: 'agent:dispatch', description: 'Dispatch agents (create + start)' },
  { value: 'agent:read', label: 'agent:read', description: 'Read agent status/metadata' },
  { value: 'agent:list', label: 'agent:list', description: 'List agents in the project' },
  { value: 'agent:create', label: 'agent:create', description: 'Create agents' },
  { value: 'agent:start', label: 'agent:start', description: 'Start/restart agents' },
  { value: 'agent:stop', label: 'agent:stop', description: 'Stop agents' },
  { value: 'agent:delete', label: 'agent:delete', description: 'Delete agents' },
  { value: 'agent:message', label: 'agent:message', description: 'Send messages to agents' },
  { value: 'agent:attach', label: 'agent:attach', description: 'Attach to agent sessions' },
  { value: 'agent:manage', label: 'agent:manage', description: 'All agent actions (convenience alias)' },
] as const;

@customElement('scion-token-list')
export class ScionTokenList extends LitElement {
  @state() private loading = true;
  @state() private tokens: AccessToken[] = [];
  @state() private projects: Project[] = [];
  @state() private error: string | null = null;

  // Create dialog
  @state() private createDialogOpen = false;
  @state() private createName = '';
  @state() private createProjectId = '';
  @state() private createScopes: Set<string> = new Set();
  @state() private createExpiry = '90';
  @state() private createLoading = false;
  @state() private createError: string | null = null;

  // Token reveal dialog (shown once after creation)
  @state() private revealDialogOpen = false;
  @state() private revealToken = '';
  @state() private revealCopied = false;

  // Action loading
  @state() private actionLoadingId: string | null = null;

  static override styles = [
    resourceStyles,
    css`
      .scope-badge {
        display: inline-flex;
        align-items: center;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
        font-family: var(--scion-font-mono, monospace);
        background: var(--sl-color-primary-100, #dbeafe);
        color: var(--sl-color-primary-700, #1d4ed8);
      }

      .scopes-cell {
        display: flex;
        flex-wrap: wrap;
        gap: 0.25rem;
      }

      .status-revoked {
        display: inline-flex;
        align-items: center;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
        background: var(--sl-color-danger-100, #fee2e2);
        color: var(--sl-color-danger-700, #b91c1c);
      }

      .status-expired {
        display: inline-flex;
        align-items: center;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
        background: var(--sl-color-warning-100, #fef3c7);
        color: var(--sl-color-warning-700, #b45309);
      }

      .status-active {
        display: inline-flex;
        align-items: center;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
        background: var(--sl-color-success-100, #dcfce7);
        color: var(--sl-color-success-700, #15803d);
      }

      .token-reveal {
        display: flex;
        flex-direction: column;
        gap: 1rem;
      }

      .token-value {
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.8125rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        padding: 0.75rem 1rem;
        border-radius: var(--scion-radius, 0.5rem);
        border: 1px solid var(--scion-border, #e2e8f0);
        word-break: break-all;
        user-select: all;
      }

      .token-copy-row {
        display: flex;
        gap: 0.5rem;
        align-items: center;
      }

      .token-copy-row sl-button {
        flex-shrink: 0;
      }

      .scope-checkboxes {
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: 0.5rem;
      }

      @media (max-width: 640px) {
        .scope-checkboxes {
          grid-template-columns: 1fr;
        }
      }

      .scope-checkbox-item {
        display: flex;
        align-items: flex-start;
        gap: 0.375rem;
      }

      .scope-checkbox-item sl-checkbox {
        --sl-spacing-x-small: 0;
      }

      .scope-checkbox-label {
        font-size: 0.8125rem;
        font-family: var(--scion-font-mono, monospace);
        color: var(--scion-text, #1e293b);
      }

      .scope-checkbox-desc {
        font-size: 0.6875rem;
        color: var(--scion-text-muted, #64748b);
        font-family: inherit;
      }

      .field-label {
        font-size: 0.875rem;
        font-weight: 500;
        color: var(--scion-text, #1e293b);
        margin-bottom: 0.375rem;
      }

      .project-name {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
      }

      tr.revoked td {
        opacity: 0.6;
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadData();
  }

  private async loadData(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const [tokensRes, projectsRes] = await Promise.all([
        apiFetch('/api/v1/auth/tokens'),
        apiFetch('/api/v1/projects'),
      ]);

      if (!tokensRes.ok) {
        throw new Error(await extractApiError(tokensRes, 'Failed to load tokens'));
      }
      if (!projectsRes.ok) {
        throw new Error(await extractApiError(projectsRes, 'Failed to load projects'));
      }

      const tokensData = (await tokensRes.json()) as { items?: AccessToken[] };
      const projectsData = (await projectsRes.json()) as { projects?: Project[] };

      this.tokens = tokensData.items || [];
      this.projects = projectsData.projects || [];
    } catch (err) {
      console.error('Failed to load token data:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load data';
    } finally {
      this.loading = false;
    }
  }

  private getProjectName(projectId: string): string {
    const project = this.projects.find((p) => p.id === projectId);
    return project?.name || project?.slug || projectId;
  }

  private getTokenStatus(token: AccessToken): 'revoked' | 'expired' | 'active' {
    if (token.revoked) return 'revoked';
    if (token.expiresAt && new Date(token.expiresAt) < new Date()) return 'expired';
    return 'active';
  }

  // ── Create dialog ──────────────────────────────────────────────────

  private openCreateDialog(): void {
    this.createName = '';
    this.createProjectId = this.projects.length === 1 ? this.projects[0].id : '';
    this.createScopes = new Set();
    this.createExpiry = '90';
    this.createError = null;
    this.createDialogOpen = true;
  }

  private closeCreateDialog(): void {
    this.createDialogOpen = false;
  }

  private toggleScope(scope: string): void {
    const next = new Set(this.createScopes);
    if (next.has(scope)) {
      next.delete(scope);
    } else {
      next.add(scope);
    }
    this.createScopes = next;
  }

  private async handleCreate(e: Event): Promise<void> {
    e.preventDefault();

    const name = this.createName.trim();
    if (!name) {
      this.createError = 'Name is required';
      return;
    }
    if (!this.createProjectId) {
      this.createError = 'Project is required';
      return;
    }

    if (this.createScopes.size === 0) {
      this.createError = 'At least one scope is required';
      return;
    }

    this.createLoading = true;
    this.createError = null;

    try {
      const days = parseInt(this.createExpiry, 10) || 90;
      const expiresAt = new Date();
      expiresAt.setDate(expiresAt.getDate() + days);

      const response = await apiFetch('/api/v1/auth/tokens', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name,
          projectId: this.createProjectId,
          scopes: Array.from(this.createScopes),
          expiresAt: expiresAt.toISOString(),
        }),
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, 'Failed to create token'));
      }

      const data = (await response.json()) as { token: string };

      this.closeCreateDialog();
      this.revealToken = data.token;
      this.revealCopied = false;
      this.revealDialogOpen = true;

      await this.loadData();
    } catch (err) {
      console.error('Failed to create token:', err);
      this.createError = err instanceof Error ? err.message : 'Failed to create token';
    } finally {
      this.createLoading = false;
    }
  }

  // ── Revoke / Delete ────────────────────────────────────────────────

  private async handleRevoke(token: AccessToken): Promise<void> {
    if (!confirm(`Revoke token "${token.name}"? It will no longer be usable for authentication.`)) {
      return;
    }

    this.actionLoadingId = token.id;
    try {
      const response = await apiFetch(`/api/v1/auth/tokens/${token.id}/revoke`, {
        method: 'POST',
      });

      if (!response.ok && response.status !== 204) {
        throw new Error(await extractApiError(response, 'Failed to revoke token'));
      }

      await this.loadData();
    } catch (err) {
      console.error('Failed to revoke token:', err);
      alert(err instanceof Error ? err.message : 'Failed to revoke');
    } finally {
      this.actionLoadingId = null;
    }
  }

  private async handleDelete(token: AccessToken): Promise<void> {
    if (!confirm(`Permanently delete token "${token.name}"? This cannot be undone.`)) {
      return;
    }

    this.actionLoadingId = token.id;
    try {
      const response = await apiFetch(`/api/v1/auth/tokens/${token.id}`, {
        method: 'DELETE',
      });

      if (!response.ok && response.status !== 204) {
        throw new Error(await extractApiError(response, 'Failed to delete token'));
      }

      await this.loadData();
    } catch (err) {
      console.error('Failed to delete token:', err);
      alert(err instanceof Error ? err.message : 'Failed to delete');
    } finally {
      this.actionLoadingId = null;
    }
  }

  // ── Copy ───────────────────────────────────────────────────────────

  private async copyToken(): Promise<void> {
    try {
      await navigator.clipboard.writeText(this.revealToken);
      this.revealCopied = true;
      setTimeout(() => {
        this.revealCopied = false;
      }, 2000);
    } catch {
      // Fallback: select the text
      const el = this.shadowRoot?.querySelector('.token-value') as HTMLElement | null;
      if (el) {
        const range = document.createRange();
        range.selectNodeContents(el);
        const sel = window.getSelection();
        sel?.removeAllRanges();
        sel?.addRange(range);
      }
    }
  }

  // ── Formatting ─────────────────────────────────────────────────────

  private formatRelativeTime(dateString: string): string {
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return dateString;
      const diffMs = Date.now() - date.getTime();
      const diffSeconds = Math.round(diffMs / 1000);
      const diffMinutes = Math.round(diffMs / (1000 * 60));
      const diffHours = Math.round(diffMs / (1000 * 60 * 60));
      const diffDays = Math.round(diffMs / (1000 * 60 * 60 * 24));

      const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

      if (Math.abs(diffSeconds) < 60) {
        return rtf.format(-diffSeconds, 'second');
      } else if (Math.abs(diffMinutes) < 60) {
        return rtf.format(-diffMinutes, 'minute');
      } else if (Math.abs(diffHours) < 24) {
        return rtf.format(-diffHours, 'hour');
      } else {
        return rtf.format(-diffDays, 'day');
      }
    } catch {
      return dateString;
    }
  }

  private formatDate(dateString: string): string {
    try {
      return new Date(dateString).toLocaleDateString('en-US', {
        year: 'numeric',
        month: 'short',
        day: 'numeric',
      });
    } catch {
      return dateString;
    }
  }

  // ── Rendering ──────────────────────────────────────────────────────

  override render() {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading access tokens...</p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load</h2>
          <p>There was a problem loading your access tokens.</p>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${() => this.loadData()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    return html`
      <div class="list-header">
        <sl-button variant="primary" @click=${this.openCreateDialog}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Create Token
        </sl-button>
      </div>
      ${this.tokens.length === 0 ? this.renderEmpty() : this.renderTable()}
      ${this.renderCreateDialog()}
      ${this.renderRevealDialog()}
    `;
  }

  private renderEmpty() {
    return html`
      <div class="empty-state">
        <sl-icon name="key"></sl-icon>
        <h3>No Access Tokens</h3>
        <p>
          Create personal access tokens to authenticate CI/CD pipelines
          and automation tools with your projects.
        </p>
        <sl-button variant="primary" size="small" @click=${this.openCreateDialog}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Create Token
        </sl-button>
      </div>
    `;
  }

  private renderTable() {
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Status</th>
              <th class="hide-mobile">Project</th>
              <th class="hide-mobile">Scopes</th>
              <th class="hide-mobile">Created</th>
              <th class="hide-mobile">Last Used</th>
              <th>Expires</th>
              <th class="actions-cell"></th>
            </tr>
          </thead>
          <tbody>
            ${this.tokens.map((token) => this.renderRow(token))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderRow(token: AccessToken) {
    const status = this.getTokenStatus(token);
    const isActionLoading = this.actionLoadingId === token.id;

    return html`
      <tr class=${status === 'revoked' ? 'revoked' : ''}>
        <td class="key-cell">
          <div class="key-info">
            <div class="key-icon" style="background: var(--sl-color-primary-100, #dbeafe); color: var(--sl-color-primary-600, #2563eb);">
              <sl-icon name="key"></sl-icon>
            </div>
            <div>
              ${token.name}
              <div class="project-name">${token.prefix}</div>
            </div>
          </div>
        </td>
        <td>
          ${status === 'revoked'
            ? html`<span class="status-revoked">Revoked</span>`
            : status === 'expired'
              ? html`<span class="status-expired">Expired</span>`
              : html`<span class="status-active">Active</span>`}
        </td>
        <td class="hide-mobile">
          <span class="project-name">${this.getProjectName(token.projectId)}</span>
        </td>
        <td class="hide-mobile">
          <div class="scopes-cell">
            ${token.scopes.map(
              (scope) => html`<span class="scope-badge">${scope}</span>`
            )}
          </div>
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${this.formatRelativeTime(token.created)}</span>
        </td>
        <td class="hide-mobile">
          <span class="meta-text">
            ${token.lastUsed ? this.formatRelativeTime(token.lastUsed) : '\u2014'}
          </span>
        </td>
        <td>
          <span class="meta-text">
            ${token.expiresAt ? this.formatDate(token.expiresAt) : '\u2014'}
          </span>
        </td>
        <td class="actions-cell">
          ${status === 'active'
            ? html`
                <sl-icon-button
                  name="x-circle"
                  label="Revoke"
                  ?disabled=${isActionLoading}
                  @click=${() => this.handleRevoke(token)}
                ></sl-icon-button>
              `
            : nothing}
          <sl-icon-button
            name="trash"
            label="Delete"
            ?disabled=${isActionLoading}
            @click=${() => this.handleDelete(token)}
          ></sl-icon-button>
        </td>
      </tr>
    `;
  }

  private renderCreateDialog() {
    return html`
      <sl-dialog
        label="Create Access Token"
        ?open=${this.createDialogOpen}
        @sl-request-close=${this.closeCreateDialog}
      >
        <form class="dialog-form" @submit=${this.handleCreate}>
          <sl-input
            label="Name"
            placeholder="e.g. github-actions, ci-deploy"
            value=${this.createName}
            @sl-input=${(e: Event) => {
              this.createName = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>

          <sl-select
            label="Project"
            placeholder="Select a project"
            value=${this.createProjectId}
            @sl-change=${(e: Event) => {
              this.createProjectId = (e.target as HTMLSelectElement).value;
            }}
            required
          >

            ${this.projects.map(
              (project) =>
                html`<sl-option value=${project.id}>${project.name || project.slug || project.id}</sl-option>`
            )}
          </sl-select>

          <div>
            <div class="field-label">Scopes</div>
            <div class="scope-checkboxes">
              ${AVAILABLE_SCOPES.map(
                (scope) => html`
                  <div class="scope-checkbox-item">
                    <sl-checkbox
                      ?checked=${this.createScopes.has(scope.value)}
                      @sl-change=${() => this.toggleScope(scope.value)}
                    >
                      <span class="scope-checkbox-label">${scope.label}</span>
                      <br />
                      <span class="scope-checkbox-desc">${scope.description}</span>
                    </sl-checkbox>
                  </div>
                `
              )}
            </div>
          </div>

          <sl-select
            label="Expires in"
            value=${this.createExpiry}
            @sl-change=${(e: Event) => {
              this.createExpiry = (e.target as HTMLSelectElement).value;
            }}
          >
            <sl-option value="7">7 days</sl-option>
            <sl-option value="30">30 days</sl-option>
            <sl-option value="90">90 days</sl-option>
            <sl-option value="180">180 days</sl-option>
            <sl-option value="365">365 days (maximum)</sl-option>
          </sl-select>

          ${this.createError
            ? html`<div class="dialog-error">${this.createError}</div>`
            : nothing}
        </form>

        <sl-button
          slot="footer"
          variant="default"
          @click=${this.closeCreateDialog}
          ?disabled=${this.createLoading}
        >
          Cancel
        </sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.createLoading}
          ?disabled=${this.createLoading}
          @click=${this.handleCreate}
        >
          Create Token
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderRevealDialog() {
    return html`
      <sl-dialog
        label="Token Created"
        ?open=${this.revealDialogOpen}
        @sl-request-close=${() => {
          this.revealDialogOpen = false;
        }}
      >
        <div class="token-reveal">
          <div class="dialog-hint">
            <sl-icon name="exclamation-triangle"></sl-icon>
            Copy this token now. You won't be able to see it again.
          </div>

          <div class="token-value">${this.revealToken}</div>

          <div class="token-copy-row">
            <sl-button variant="primary" size="small" @click=${this.copyToken}>
              <sl-icon slot="prefix" name=${this.revealCopied ? 'check-lg' : 'clipboard'}></sl-icon>
              ${this.revealCopied ? 'Copied!' : 'Copy to clipboard'}
            </sl-button>
          </div>
        </div>

        <sl-button
          slot="footer"
          variant="primary"
          @click=${() => {
            this.revealDialogOpen = false;
          }}
        >
          Done
        </sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-token-list': ScionTokenList;
  }
}
