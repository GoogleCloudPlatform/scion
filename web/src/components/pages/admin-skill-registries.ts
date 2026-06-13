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
 * Admin Skill Registries list page
 *
 * Table-only admin page showing all skill registries with CRUD via inline dialog.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import type { SkillRegistry } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import '../shared/status-badge.js';

@customElement('scion-page-admin-skill-registries')
export class ScionPageAdminSkillRegistries extends LitElement {
  @state() private loading = true;
  @state() private registries: SkillRegistry[] = [];
  @state() private error: string | null = null;

  // Create dialog state
  @state() private createOpen = false;
  @state() private createForm = { name: '', endpoint: '', type: 'hub', trustLevel: 'trusted', description: '', authToken: '', resolvePath: '' };
  @state() private createError: string | null = null;
  @state() private creating = false;

  static override styles = css`
    :host { display: block; }

    .header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1.5rem;
    }
    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .table-container {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }
    table { width: 100%; border-collapse: collapse; }
    th {
      text-align: left;
      padding: 0.75rem 1rem;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }
    td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }
    tr:last-child td { border-bottom: none; }
    tr.clickable { cursor: pointer; }
    tr.clickable:hover td { background: var(--scion-bg-subtle, #f1f5f9); }

    .endpoint-text {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      max-width: 300px;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .type-badge, .trust-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
    }
    .type-badge {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }
    .trust-badge.trusted {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }
    .trust-badge.pinned {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #a16207);
    }

    .meta-text {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .empty-state {
      text-align: center;
      padding: 4rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px dashed var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }
    .empty-state > sl-icon {
      font-size: 4rem;
      color: var(--scion-text-muted, #64748b);
      opacity: 0.5;
      margin-bottom: 1rem;
    }
    .empty-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }
    .empty-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 4rem 2rem;
      color: var(--scion-text-muted, #64748b);
    }
    .loading-state sl-spinner { font-size: 2rem; margin-bottom: 1rem; }

    .error-state {
      text-align: center;
      padding: 3rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }
    .error-state sl-icon {
      font-size: 3rem;
      color: var(--sl-color-danger-500, #ef4444);
      margin-bottom: 1rem;
    }
    .error-state h2 {
      font-size: 1.25rem; font-weight: 600;
      color: var(--scion-text, #1e293b); margin: 0 0 0.5rem 0;
    }
    .error-state p { color: var(--scion-text-muted, #64748b); margin: 0 0 1rem 0; }
    .error-details {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      color: var(--sl-color-danger-700, #b91c1c);
      margin-bottom: 1rem;
    }

    .error-banner {
      background: var(--sl-color-danger-50, #fef2f2);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      margin-bottom: 1rem;
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

    .form-field {
      margin-bottom: 1rem;
    }
    .form-field label {
      display: block;
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.375rem;
    }
    .form-field .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadRegistries();
  }

  private async loadRegistries(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const res = await apiFetch('/api/v1/skill-registries');
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      const data = (await res.json()) as { items?: SkillRegistry[]; registries?: SkillRegistry[] } | SkillRegistry[];
      if (Array.isArray(data)) {
        this.registries = data;
      } else {
        this.registries = data.items || data.registries || [];
      }
    } catch (err) {
      console.error('Failed to load registries:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load registries';
    } finally {
      this.loading = false;
    }
  }

  private formatRelativeTime(dateString: string): string {
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return '—';
      const diffMs = Date.now() - date.getTime();
      if (diffMs < 0) return 'just now';
      const seconds = Math.floor(diffMs / 1000);
      if (seconds < 60) return 'just now';
      const minutes = Math.floor(seconds / 60);
      if (minutes < 60) return `${minutes}m ago`;
      const hours = Math.floor(minutes / 60);
      if (hours < 24) return `${hours}h ago`;
      const days = Math.floor(hours / 24);
      return `${days}d ago`;
    } catch { return dateString; }
  }

  private async handleCreate(): Promise<void> {
    if (!this.createForm.name.trim() || !this.createForm.endpoint.trim()) {
      this.createError = 'Name and endpoint are required.';
      return;
    }

    this.creating = true;
    this.createError = null;

    try {
      const body: Record<string, unknown> = {
        name: this.createForm.name.trim(),
        endpoint: this.createForm.endpoint.trim(),
        type: this.createForm.type,
        trustLevel: this.createForm.trustLevel,
      };
      if (this.createForm.description.trim()) body.description = this.createForm.description.trim();
      if (this.createForm.authToken.trim()) body.authToken = this.createForm.authToken.trim();
      if (this.createForm.resolvePath.trim()) body.resolvePath = this.createForm.resolvePath.trim();

      const res = await apiFetch('/api/v1/skill-registries', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!res.ok) {
        throw new Error(await extractApiError(res, 'Failed to create registry'));
      }

      this.createOpen = false;
      this.createForm = { name: '', endpoint: '', type: 'hub', trustLevel: 'trusted', description: '', authToken: '', resolvePath: '' };
      void this.loadRegistries();
    } catch (err) {
      this.createError = err instanceof Error ? err.message : 'Failed to create registry';
    } finally {
      this.creating = false;
    }
  }

  private navigateToDetail(id: string): void {
    window.history.pushState({}, '', `/admin/skill-registries/${id}`);
    window.dispatchEvent(new PopStateEvent('popstate'));
  }

  override render() {
    return html`
      <div class="header">
        <h1>Skill Registries</h1>
        <sl-button variant="primary" size="small" @click=${() => { this.createOpen = true; }}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Create Registry
        </sl-button>
      </div>

      ${this.loading ? this.renderLoading()
        : this.error ? this.renderError()
        : this.registries.length === 0 ? this.renderEmpty()
        : this.renderTable()}

      ${this.renderCreateDialog()}
    `;
  }

  private renderTable() {
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Endpoint</th>
              <th>Type</th>
              <th>Trust</th>
              <th>Status</th>
              <th>Created</th>
            </tr>
          </thead>
          <tbody>
            ${this.registries.map((r) => html`
              <tr class="clickable" @click=${() => this.navigateToDetail(r.id)}>
                <td><strong>${r.name}</strong></td>
                <td><span class="endpoint-text">${r.endpoint}</span></td>
                <td><span class="type-badge">${r.type}</span></td>
                <td><span class="trust-badge ${r.trustLevel}">${r.trustLevel}</span></td>
                <td>
                  <scion-status-badge
                    status=${r.status === 'active' ? 'success' : 'danger'}
                    label=${r.status}
                    size="small"
                  ></scion-status-badge>
                </td>
                <td><span class="meta-text">${this.formatRelativeTime(r.created)}</span></td>
              </tr>
            `)}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderCreateDialog() {
    return html`
      <sl-dialog
        label="Create Skill Registry"
        ?open=${this.createOpen}
        @sl-after-hide=${() => { this.createOpen = false; this.createError = null; }}
        style="--width: 500px;"
      >
        ${this.createError ? html`
          <div class="error-banner">
            <sl-icon name="exclamation-triangle"></sl-icon>
            <span>${this.createError}</span>
          </div>
        ` : nothing}

        <div class="form-field">
          <label>Name</label>
          <sl-input
            placeholder="production-hub"
            .value=${this.createForm.name}
            @sl-input=${(e: Event) => { this.createForm = { ...this.createForm, name: (e.target as HTMLElement & { value: string }).value }; }}
            required
          ></sl-input>
        </div>

        <div class="form-field">
          <label>Endpoint</label>
          <sl-input
            placeholder="https://registry.example.com"
            .value=${this.createForm.endpoint}
            @sl-input=${(e: Event) => { this.createForm = { ...this.createForm, endpoint: (e.target as HTMLElement & { value: string }).value }; }}
            required
          ></sl-input>
        </div>

        <div class="form-field">
          <label>Type</label>
          <sl-select
            .value=${this.createForm.type}
            @sl-change=${(e: Event) => { this.createForm = { ...this.createForm, type: (e.target as HTMLElement & { value: string }).value }; }}
          >
            <sl-option value="hub">Hub</sl-option>
            <sl-option value="gcp">GCP</sl-option>
          </sl-select>
        </div>

        <div class="form-field">
          <label>Trust Level</label>
          <sl-select
            .value=${this.createForm.trustLevel}
            @sl-change=${(e: Event) => { this.createForm = { ...this.createForm, trustLevel: (e.target as HTMLElement & { value: string }).value }; }}
          >
            <sl-option value="trusted">Trusted</sl-option>
            <sl-option value="pinned">Pinned</sl-option>
          </sl-select>
          <div class="hint">
            ${this.createForm.trustLevel === 'trusted'
              ? 'All content from this registry is trusted.'
              : 'Only content with pinned hashes is trusted.'}
          </div>
        </div>

        <div class="form-field">
          <label>Description (optional)</label>
          <sl-textarea
            .value=${this.createForm.description}
            @sl-input=${(e: Event) => { this.createForm = { ...this.createForm, description: (e.target as HTMLElement & { value: string }).value }; }}
            rows="2"
          ></sl-textarea>
        </div>

        <div class="form-field">
          <label>Auth Token (optional)</label>
          <sl-input
            type="password"
            .value=${this.createForm.authToken}
            @sl-input=${(e: Event) => { this.createForm = { ...this.createForm, authToken: (e.target as HTMLElement & { value: string }).value }; }}
            toggle-password
          ></sl-input>
        </div>

        <div class="form-field">
          <label>Resolve Path (optional)</label>
          <sl-input
            placeholder="/api/v1/skills/resolve"
            .value=${this.createForm.resolvePath}
            @sl-input=${(e: Event) => { this.createForm = { ...this.createForm, resolvePath: (e.target as HTMLElement & { value: string }).value }; }}
          ></sl-input>
        </div>

        <sl-button slot="footer" variant="default" @click=${() => { this.createOpen = false; }}>
          Cancel
        </sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.creating}
          ?disabled=${this.creating || !this.createForm.name.trim() || !this.createForm.endpoint.trim()}
          @click=${() => this.handleCreate()}
        >
          Create
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading registries...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Registries</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadRegistries()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderEmpty() {
    return html`
      <div class="empty-state">
        <sl-icon name="cloud-arrow-down"></sl-icon>
        <h2>No Registries</h2>
        <p>No skill registries have been configured yet.</p>
        <sl-button variant="primary" @click=${() => { this.createOpen = true; }}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Create Registry
        </sl-button>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-skill-registries': ScionPageAdminSkillRegistries;
  }
}
