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
 * Admin Skill Registry detail page
 *
 * Configuration card with inline edit + pinned hashes section for pinned-trust registries.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import type { SkillRegistry } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import '../shared/status-badge.js';
import '../shared/hash-display.js';

interface PinnedHash {
  uri: string;
  hash: string;
}

@customElement('scion-page-admin-skill-registry-detail')
export class ScionPageAdminSkillRegistryDetail extends LitElement {
  @state() private loading = true;
  @state() private registry: SkillRegistry | null = null;
  @state() private error: string | null = null;
  @state() private registryId = '';

  // Edit mode
  @state() private editing = false;
  @state() private editForm: Partial<{ endpoint: string; description: string; trustLevel: string; resolvePath: string; authToken: string }> = {};
  @state() private saving = false;

  // Pinned hashes
  @state() private pinnedHashes: PinnedHash[] = [];
  @state() private pinnedLoading = false;
  @state() private pinDialogOpen = false;
  @state() private pinUri = '';
  @state() private pinHash = '';
  @state() private pinning = false;

  // Actions
  @state() private actionLoading: Record<string, boolean> = {};

  static override styles = css`
    :host { display: block; }

    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }
    .back-link:hover { color: var(--scion-primary, #3b82f6); }

    .header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 1.5rem;
      gap: 1rem;
    }
    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }
    .header-actions {
      display: flex;
      gap: 0.5rem;
      flex-shrink: 0;
    }

    .card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }
    .card-title-row {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1rem;
      padding-bottom: 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }
    .card-title {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .info-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
      gap: 1.5rem;
    }
    .info-item { display: flex; flex-direction: column; }
    .info-label {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
      margin-bottom: 0.25rem;
    }
    .info-value {
      font-size: 1rem;
      color: var(--scion-text, #1e293b);
    }
    .info-value.mono {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
    }

    .type-badge, .trust-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.8125rem;
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

    .edit-field { margin-bottom: 1rem; }
    .edit-field label {
      display: block;
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
      margin-bottom: 0.375rem;
    }
    .edit-actions {
      display: flex;
      gap: 0.5rem;
      margin-top: 1rem;
    }

    .pin-table {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }
    .pin-table table { width: 100%; border-collapse: collapse; }
    .pin-table th {
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
    .pin-table td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }
    .pin-table tr:last-child td { border-bottom: none; }

    .empty-pins {
      text-align: center;
      padding: 2rem;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
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
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    if (typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/admin\/skill-registries\/([^/]+)/);
      if (match) this.registryId = match[1];
    }
    void this.loadRegistry();
  }

  private async loadRegistry(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const res = await apiFetch(`/api/v1/skill-registries/${this.registryId}`);
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      this.registry = (await res.json()) as SkillRegistry;

      if (this.registry.trustLevel === 'pinned') {
        void this.loadPinnedHashes();
      }
    } catch (err) {
      console.error('Failed to load registry:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load registry';
    } finally {
      this.loading = false;
    }
  }

  private async loadPinnedHashes(): Promise<void> {
    this.pinnedLoading = true;
    try {
      const res = await apiFetch(`/api/v1/skill-registries/${this.registryId}/pins`);
      if (res.ok) {
        const data = (await res.json()) as { pins?: PinnedHash[]; items?: PinnedHash[] } | PinnedHash[];
        if (Array.isArray(data)) {
          this.pinnedHashes = data;
        } else {
          this.pinnedHashes = data.pins || data.items || [];
        }
      }
    } catch {
      // Endpoint may not exist yet
    } finally {
      this.pinnedLoading = false;
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

  // -- Edit mode --

  private startEditing(): void {
    if (!this.registry) return;
    this.editForm = {
      endpoint: this.registry.endpoint,
      description: this.registry.description || '',
      trustLevel: this.registry.trustLevel,
      resolvePath: this.registry.resolvePath || '',
      authToken: '',
    };
    this.editing = true;
  }

  private cancelEditing(): void {
    this.editing = false;
    this.editForm = {};
  }

  private async saveEdit(): Promise<void> {
    if (!this.registry) return;
    this.saving = true;
    try {
      const body: Record<string, unknown> = {};
      if (this.editForm.endpoint !== undefined && this.editForm.endpoint !== this.registry.endpoint) body.endpoint = this.editForm.endpoint;
      if (this.editForm.description !== undefined) body.description = this.editForm.description;
      if (this.editForm.trustLevel !== undefined && this.editForm.trustLevel !== this.registry.trustLevel) body.trustLevel = this.editForm.trustLevel;
      if (this.editForm.resolvePath !== undefined) body.resolvePath = this.editForm.resolvePath;
      if (this.editForm.authToken) body.authToken = this.editForm.authToken;

      const res = await apiFetch(`/api/v1/skill-registries/${this.registryId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!res.ok) {
        throw new Error(await extractApiError(res, 'Failed to update registry'));
      }
      this.registry = (await res.json()) as SkillRegistry;
      this.editing = false;

      if (this.registry.trustLevel === 'pinned') {
        void this.loadPinnedHashes();
      }
    } catch (err) {
      alert(err instanceof Error ? err.message : 'Failed to save');
    } finally {
      this.saving = false;
    }
  }

  // -- Toggle status --

  private async toggleStatus(): Promise<void> {
    if (!this.registry) return;
    const newStatus = this.registry.status === 'active' ? 'disabled' : 'active';
    this.actionLoading = { ...this.actionLoading, toggle: true };
    try {
      const res = await apiFetch(`/api/v1/skill-registries/${this.registryId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ status: newStatus }),
      });
      if (!res.ok) {
        throw new Error(await extractApiError(res, 'Failed to update status'));
      }
      this.registry = (await res.json()) as SkillRegistry;
    } catch (err) {
      alert(err instanceof Error ? err.message : 'Failed to toggle status');
    } finally {
      this.actionLoading = { ...this.actionLoading, toggle: false };
    }
  }

  // -- Delete --

  private async handleDelete(): Promise<void> {
    if (!confirm('Are you sure you want to delete this registry?')) return;
    this.actionLoading = { ...this.actionLoading, delete: true };
    try {
      const res = await apiFetch(`/api/v1/skill-registries/${this.registryId}`, { method: 'DELETE' });
      if (!res.ok) {
        throw new Error(await extractApiError(res, 'Failed to delete registry'));
      }
      window.history.pushState({}, '', '/admin/skill-registries');
      window.dispatchEvent(new PopStateEvent('popstate'));
    } catch (err) {
      alert(err instanceof Error ? err.message : 'Failed to delete');
    } finally {
      this.actionLoading = { ...this.actionLoading, delete: false };
    }
  }

  // -- Pin/Unpin --

  private async handlePin(): Promise<void> {
    if (!this.pinUri.trim() || !this.pinHash.trim()) return;
    this.pinning = true;
    try {
      const res = await apiFetch(`/api/v1/skill-registries/${this.registryId}/pin`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ uri: this.pinUri.trim(), hash: this.pinHash.trim() }),
      });
      if (!res.ok) {
        throw new Error(await extractApiError(res, 'Failed to pin hash'));
      }
      this.pinDialogOpen = false;
      this.pinUri = '';
      this.pinHash = '';
      void this.loadPinnedHashes();
    } catch (err) {
      alert(err instanceof Error ? err.message : 'Failed to pin');
    } finally {
      this.pinning = false;
    }
  }

  private async handleUnpin(uri: string): Promise<void> {
    if (!confirm(`Unpin "${uri}"?`)) return;
    try {
      const res = await apiFetch(`/api/v1/skill-registries/${this.registryId}/unpin`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ uri }),
      });
      if (!res.ok) {
        throw new Error(await extractApiError(res, 'Failed to unpin'));
      }
      void this.loadPinnedHashes();
    } catch (err) {
      alert(err instanceof Error ? err.message : 'Failed to unpin');
    }
  }

  // -- Render --

  override render() {
    if (this.loading) return this.renderLoading();
    if (this.error || !this.registry) return this.renderError();

    return html`
      <a href="/admin/skill-registries" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Registries
      </a>

      ${this.renderHeader()}
      ${this.editing ? this.renderEditMode() : this.renderConfigCard()}
      ${this.registry.trustLevel === 'pinned' ? this.renderPinnedSection() : nothing}
      ${this.renderPinDialog()}
    `;
  }

  private renderHeader() {
    const r = this.registry!;
    return html`
      <div class="header">
        <div>
          <h1>
            <sl-icon name="cloud-arrow-down"></sl-icon>
            ${r.name}
          </h1>
        </div>
        <div class="header-actions">
          <sl-button variant="default" size="small" outline @click=${() => this.startEditing()}>
            <sl-icon slot="prefix" name="pencil"></sl-icon>
            Edit
          </sl-button>
          <sl-button
            variant=${r.status === 'active' ? 'warning' : 'success'}
            size="small"
            outline
            ?loading=${this.actionLoading['toggle']}
            @click=${() => this.toggleStatus()}
          >
            ${r.status === 'active' ? 'Disable' : 'Enable'}
          </sl-button>
          <sl-button
            variant="danger"
            size="small"
            outline
            ?loading=${this.actionLoading['delete']}
            @click=${() => this.handleDelete()}
          >
            <sl-icon slot="prefix" name="trash"></sl-icon>
            Delete
          </sl-button>
        </div>
      </div>
    `;
  }

  private renderConfigCard() {
    const r = this.registry!;
    return html`
      <div class="card">
        <div class="card-title-row">
          <h3 class="card-title">Configuration</h3>
        </div>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Name</span>
            <span class="info-value">${r.name}</span>
          </div>
          <div class="info-item">
            <span class="info-label">Endpoint</span>
            <span class="info-value mono">${r.endpoint}</span>
          </div>
          <div class="info-item">
            <span class="info-label">Type</span>
            <span class="info-value"><span class="type-badge">${r.type}</span></span>
          </div>
          <div class="info-item">
            <span class="info-label">Trust Level</span>
            <span class="info-value"><span class="trust-badge ${r.trustLevel}">${r.trustLevel}</span></span>
          </div>
          <div class="info-item">
            <span class="info-label">Status</span>
            <span class="info-value">
              <scion-status-badge
                status=${r.status === 'active' ? 'success' : 'danger'}
                label=${r.status}
                size="small"
              ></scion-status-badge>
            </span>
          </div>
          ${r.description ? html`
            <div class="info-item">
              <span class="info-label">Description</span>
              <span class="info-value">${r.description}</span>
            </div>
          ` : nothing}
          ${r.resolvePath ? html`
            <div class="info-item">
              <span class="info-label">Resolve Path</span>
              <span class="info-value mono">${r.resolvePath}</span>
            </div>
          ` : nothing}
          <div class="info-item">
            <span class="info-label">Created</span>
            <span class="info-value">${this.formatRelativeTime(r.created)}</span>
          </div>
          <div class="info-item">
            <span class="info-label">Updated</span>
            <span class="info-value">${this.formatRelativeTime(r.updated)}</span>
          </div>
        </div>
      </div>
    `;
  }

  private renderEditMode() {
    return html`
      <div class="card">
        <div class="card-title-row">
          <h3 class="card-title">Edit Configuration</h3>
        </div>
        <div class="edit-field">
          <label>Name</label>
          <span class="info-value">${this.registry!.name}</span>
        </div>
        <div class="edit-field">
          <label>Type</label>
          <span class="info-value"><span class="type-badge">${this.registry!.type}</span></span>
        </div>
        <div class="edit-field">
          <label>Endpoint</label>
          <sl-input
            .value=${this.editForm.endpoint || ''}
            @sl-input=${(e: Event) => { this.editForm = { ...this.editForm, endpoint: (e.target as HTMLElement & { value: string }).value }; }}
          ></sl-input>
        </div>
        <div class="edit-field">
          <label>Trust Level</label>
          <sl-select
            .value=${this.editForm.trustLevel || 'trusted'}
            @sl-change=${(e: Event) => { this.editForm = { ...this.editForm, trustLevel: (e.target as HTMLElement & { value: string }).value }; }}
          >
            <sl-option value="trusted">Trusted</sl-option>
            <sl-option value="pinned">Pinned</sl-option>
          </sl-select>
        </div>
        <div class="edit-field">
          <label>Description</label>
          <sl-textarea
            .value=${this.editForm.description || ''}
            @sl-input=${(e: Event) => { this.editForm = { ...this.editForm, description: (e.target as HTMLElement & { value: string }).value }; }}
            rows="2"
            help-text="Once set, this field cannot be cleared."
          ></sl-textarea>
        </div>
        <div class="edit-field">
          <label>Resolve Path</label>
          <sl-input
            .value=${this.editForm.resolvePath || ''}
            @sl-input=${(e: Event) => { this.editForm = { ...this.editForm, resolvePath: (e.target as HTMLElement & { value: string }).value }; }}
            help-text="Once set, this field cannot be cleared."
          ></sl-input>
        </div>
        <div class="edit-field">
          <label>Auth Token (leave blank to keep current)</label>
          <sl-input
            type="password"
            .value=${this.editForm.authToken || ''}
            @sl-input=${(e: Event) => { this.editForm = { ...this.editForm, authToken: (e.target as HTMLElement & { value: string }).value }; }}
            toggle-password
          ></sl-input>
        </div>
        <div class="edit-actions">
          <sl-button variant="primary" size="small" ?loading=${this.saving} @click=${() => this.saveEdit()}>
            Save
          </sl-button>
          <sl-button variant="default" size="small" ?disabled=${this.saving} @click=${() => this.cancelEditing()}>
            Cancel
          </sl-button>
        </div>
      </div>
    `;
  }

  private renderPinnedSection() {
    return html`
      <div class="card">
        <div class="card-title-row">
          <h3 class="card-title">Pinned Hashes</h3>
          <sl-button size="small" variant="default" outline @click=${() => { this.pinDialogOpen = true; }}>
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            Pin Hash
          </sl-button>
        </div>

        ${this.pinnedLoading ? html`
          <div style="text-align: center; padding: 1rem;">
            <sl-spinner></sl-spinner>
          </div>
        ` : this.pinnedHashes.length === 0 ? html`
          <div class="empty-pins">
            <p>No pinned hashes. Pin specific skill hashes to restrict which content is trusted from this registry.</p>
          </div>
        ` : html`
          <div class="pin-table">
            <table>
              <thead>
                <tr>
                  <th>URI</th>
                  <th>Hash</th>
                  <th style="text-align: right">Actions</th>
                </tr>
              </thead>
              <tbody>
                ${this.pinnedHashes.map((p) => html`
                  <tr>
                    <td style="font-family: var(--scion-font-mono, monospace); font-size: 0.875rem;">${p.uri}</td>
                    <td><scion-hash-display .hash=${p.hash} max-width="20ch"></scion-hash-display></td>
                    <td style="text-align: right">
                      <sl-button size="small" variant="danger" outline @click=${() => this.handleUnpin(p.uri)}>
                        Unpin
                      </sl-button>
                    </td>
                  </tr>
                `)}
              </tbody>
            </table>
          </div>
        `}
      </div>
    `;
  }

  private renderPinDialog() {
    return html`
      <sl-dialog
        label="Pin Hash"
        ?open=${this.pinDialogOpen}
        @sl-after-hide=${() => { this.pinDialogOpen = false; }}
      >
        <div class="form-field">
          <label>Skill URI</label>
          <sl-input
            placeholder="global:my-skill@1.0.0"
            .value=${this.pinUri}
            @sl-input=${(e: Event) => { this.pinUri = (e.target as HTMLElement & { value: string }).value; }}
          ></sl-input>
        </div>
        <div class="form-field">
          <label>Content Hash</label>
          <sl-input
            placeholder="sha256:abc123..."
            .value=${this.pinHash}
            @sl-input=${(e: Event) => { this.pinHash = (e.target as HTMLElement & { value: string }).value; }}
          ></sl-input>
        </div>
        <sl-button slot="footer" variant="default" @click=${() => { this.pinDialogOpen = false; }}>
          Cancel
        </sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.pinning}
          ?disabled=${this.pinning || !this.pinUri.trim() || !this.pinHash.trim()}
          @click=${() => this.handlePin()}
        >
          Pin
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading registry...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <a href="/admin/skill-registries" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Registries
      </a>
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Registry</h2>
        <p>There was a problem loading this registry.</p>
        <div class="error-details">${this.error || 'Registry not found'}</div>
        <sl-button variant="primary" @click=${() => this.loadRegistry()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-skill-registry-detail': ScionPageAdminSkillRegistryDetail;
  }
}
