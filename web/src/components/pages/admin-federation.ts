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
 * Federation Admin Page
 *
 * Manages federation authentication configuration: global settings
 * (enable/disable, algorithms, cache intervals) and trusted issuers
 * (add/edit/delete via dialog). Saves atomically via PUT to the
 * admin server-config API.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';

// ── Type definitions matching the Go API response ──

interface TrustedIssuerConfig {
  issuer_url: string;
  jwks_url?: string;
  expected_audience?: string;
  allowed_projects?: string[];
  allowed_root_users?: string[];
  default_scopes?: string[];
  issuer_type?: string;
  default_role?: string;
  allowed_emails?: string[];
}

@customElement('scion-page-admin-federation')
export class ScionPageAdminFederation extends LitElement {
  // --- State ---
  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private successMessage: string | null = null;
  @state() private saving = false;
  @state() private downloading = false;

  // Global federation settings
  @state() private enabled = false;
  @state() private algorithms: string[] = ['RS256'];
  @state() private refreshInterval = '1h';
  @state() private debounceInterval = '5s';

  // Trusted issuers list
  @state() private issuers: TrustedIssuerConfig[] = [];

  // Add/Edit dialog state
  @state() private dialogOpen = false;
  @state() private editingIssuer: TrustedIssuerConfig | null = null;
  @state() private editingIndex = -1; // -1 = new, >= 0 = editing existing

  // Delete confirmation dialog state
  @state() private deleteDialogOpen = false;
  @state() private deletingIndex = -1;

  // URL validation state
  @state() private issuerUrlError: string | null = null;

  // Timer cleanup
  private successMessageTimer: ReturnType<typeof setTimeout> | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 0.5rem;
    }

    .header-left {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .header sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .header-description {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0 0 1.5rem 0;
    }

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .section-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
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

    .form-grid {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 1rem;
    }

    @media (max-width: 768px) {
      .form-grid {
        grid-template-columns: 1fr;
      }
    }

    .form-field {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
    }

    .form-field.full-width {
      grid-column: 1 / -1;
    }

    .form-field label {
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
    }

    .form-field .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .loading-container {
      display: flex;
      justify-content: center;
      align-items: center;
      padding: 4rem;
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

    sl-input::part(base),
    sl-select::part(combobox) {
      font-size: 0.875rem;
      border-color: var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
    }

    sl-input::part(input) {
      color: var(--scion-text, #1e293b);
    }

    sl-switch {
      --sl-color-primary-600: var(--scion-primary, #3b82f6);
    }

    /* Issuer table */
    .issuer-table {
      width: 100%;
      border-collapse: collapse;
    }

    .issuer-table th {
      text-align: left;
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.025em;
      padding: 0.75rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .issuer-table td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .issuer-table tr:last-child td {
      border-bottom: none;
    }

    .issuer-actions {
      display: flex;
      gap: 0.5rem;
    }

    .empty-state {
      text-align: center;
      padding: 2rem;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }

    .empty-state sl-icon {
      font-size: 2rem;
      margin-bottom: 0.75rem;
      display: block;
    }

    /* Dialog form */
    .dialog-form {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .dialog-form .form-field label {
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
    }

    .dialog-form .form-field .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .dialog-footer {
      display: flex;
      justify-content: flex-end;
      gap: 0.5rem;
      margin-top: 0.5rem;
    }

    .cache-row {
      display: flex;
      gap: 1rem;
      align-items: flex-end;
    }

    .cache-row .form-field {
      flex: 1;
    }

    @media (max-width: 640px) {
      .cache-row {
        flex-direction: column;
      }
    }

    .conditional-section {
      padding: 0.75rem;
      margin-top: 0.25rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-bg, #f8fafc);
    }

    .conditional-section-title {
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
      margin-bottom: 0.75rem;
    }

    .issuer-url-cell {
      max-width: 20rem;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .form-field.mb-1 {
      margin-bottom: 1rem;
    }

    .cache-section {
      margin-top: 1rem;
    }

    .cache-section-label {
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
      display: block;
      margin-bottom: 0.5rem;
    }
  `;

  // --- Lifecycle ---

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadData();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.successMessageTimer !== null) {
      clearTimeout(this.successMessageTimer);
      this.successMessageTimer = null;
    }
  }

  // --- Data ---

  private async loadData(showSpinner = true): Promise<void> {
    if (showSpinner) {
      this.loading = true;
    }
    this.error = null;
    try {
      const res = await apiFetch('/api/v1/admin/server-config');
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to load federation config');
        return;
      }
      const data = (await res.json()) as {
        federation?: {
          enabled?: boolean;
          algorithms?: string[];
          refresh_interval?: string;
          debounce_interval?: string;
          trusted_issuers?: TrustedIssuerConfig[];
        };
      };
      if (data.federation) {
        this.enabled = data.federation.enabled ?? false;
        this.algorithms = data.federation.algorithms ?? ['RS256'];
        this.refreshInterval = data.federation.refresh_interval ?? '1h';
        this.debounceInterval = data.federation.debounce_interval ?? '5s';
        this.issuers = data.federation.trusted_issuers ?? [];
      }
    } catch {
      this.error = 'Failed to connect to server';
    } finally {
      this.loading = false;
    }
  }

  private async save(): Promise<void> {
    this.saving = true;
    this.error = null;
    this.successMessage = null;
    try {
      const res = await apiFetch('/api/v1/admin/server-config', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          federation: {
            enabled: this.enabled,
            trusted_issuers: this.issuers,
            algorithms: this.algorithms,
            refresh_interval: this.refreshInterval,
            debounce_interval: this.debounceInterval,
          },
        }),
      });
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to save federation config');
        return;
      }
      // Reload server state so server-side normalization is reflected
      await this.loadData(false);
      this.successMessage = 'Federation config saved successfully';
      if (this.successMessageTimer !== null) {
        clearTimeout(this.successMessageTimer);
      }
      this.successMessageTimer = setTimeout(() => {
        this.successMessage = null;
        this.successMessageTimer = null;
      }, 3000);
    } catch {
      this.error = 'Failed to connect to server';
    } finally {
      this.saving = false;
    }
  }

  // --- Issuer CRUD ---

  private openAddDialog(): void {
    this.editingIssuer = {
      issuer_url: '',
      issuer_type: 'hub',
    };
    this.editingIndex = -1;
    this.dialogOpen = true;
  }

  private openEditDialog(index: number): void {
    const issuer = this.issuers[index];
    // Deep-copy to avoid mutating the original during editing
    this.editingIssuer = {
      issuer_url: issuer.issuer_url,
      jwks_url: issuer.jwks_url ?? '',
      expected_audience: issuer.expected_audience ?? '',
      allowed_projects: issuer.allowed_projects ? [...issuer.allowed_projects] : [],
      allowed_root_users: issuer.allowed_root_users ? [...issuer.allowed_root_users] : [],
      default_scopes: issuer.default_scopes ? [...issuer.default_scopes] : [],
      issuer_type: issuer.issuer_type ?? 'hub',
      default_role: issuer.default_role ?? '',
      allowed_emails: issuer.allowed_emails ? [...issuer.allowed_emails] : [],
    };
    this.editingIndex = index;
    this.dialogOpen = true;
  }

  private handleDialogClose(): void {
    this.dialogOpen = false;
    this.editingIssuer = null;
    this.editingIndex = -1;
    this.issuerUrlError = null;
  }

  private handleSaveIssuer(): void {
    if (!this.editingIssuer || !this.editingIssuer.issuer_url.trim()) {
      return;
    }

    const url = this.editingIssuer.issuer_url.trim();
    if (!url.startsWith('https://') && !url.startsWith('http://')) {
      this.issuerUrlError = 'Issuer URL must start with https:// or http://';
      return;
    }
    this.issuerUrlError = null;

    // Build clean issuer config — strip empty optional fields
    const issuer: TrustedIssuerConfig = {
      issuer_url: this.editingIssuer.issuer_url.trim(),
    };

    if (this.editingIssuer.issuer_type) {
      issuer.issuer_type = this.editingIssuer.issuer_type;
    }
    if (this.editingIssuer.jwks_url?.trim()) {
      issuer.jwks_url = this.editingIssuer.jwks_url.trim();
    }
    if (this.editingIssuer.expected_audience?.trim()) {
      issuer.expected_audience = this.editingIssuer.expected_audience.trim();
    }
    if (this.editingIssuer.allowed_projects && this.editingIssuer.allowed_projects.length > 0) {
      issuer.allowed_projects = this.editingIssuer.allowed_projects;
    }
    if (this.editingIssuer.allowed_root_users && this.editingIssuer.allowed_root_users.length > 0) {
      issuer.allowed_root_users = this.editingIssuer.allowed_root_users;
    }
    if (this.editingIssuer.default_scopes && this.editingIssuer.default_scopes.length > 0) {
      issuer.default_scopes = this.editingIssuer.default_scopes;
    }
    if (this.editingIssuer.default_role?.trim()) {
      issuer.default_role = this.editingIssuer.default_role.trim();
    }
    if (this.editingIssuer.allowed_emails && this.editingIssuer.allowed_emails.length > 0) {
      issuer.allowed_emails = this.editingIssuer.allowed_emails;
    }

    const updated = [...this.issuers];
    if (this.editingIndex >= 0) {
      updated[this.editingIndex] = issuer;
    } else {
      updated.push(issuer);
    }
    this.issuers = updated;

    this.handleDialogClose();
    void this.save();
  }

  private openDeleteDialog(index: number): void {
    this.deletingIndex = index;
    this.deleteDialogOpen = true;
  }

  private handleDeleteDialogClose(): void {
    this.deleteDialogOpen = false;
    this.deletingIndex = -1;
  }

  private confirmDeleteIssuer(): void {
    if (this.deletingIndex < 0 || this.deletingIndex >= this.issuers.length) {
      return;
    }
    const updated = this.issuers.filter((_, i) => i !== this.deletingIndex);
    this.issuers = updated;
    this.handleDeleteDialogClose();
    void this.save();
  }

  // --- JWKS Download ---

  private async downloadJWKS(): Promise<void> {
    this.downloading = true;
    this.error = null;
    this.successMessage = null;
    try {
      const resp = await apiFetch('/.well-known/jwks.json');
      if (!resp.ok) {
        this.error = await extractApiError(resp, 'Failed to download JWKS');
        return;
      }
      const blob = await resp.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = 'scion-jwks.json';
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      URL.revokeObjectURL(url);
    } catch {
      this.error = 'Failed to download JWKS: could not connect to server';
    } finally {
      this.downloading = false;
    }
  }

  // --- Helpers for comma-separated array fields ---

  private arrayToCommaString(arr?: string[]): string {
    return (arr ?? []).join(', ');
  }

  private commaStringToArray(value: string): string[] {
    return value
      .split(',')
      .map((s) => s.trim())
      .filter((s) => s !== '');
  }

  // --- Render ---

  override render() {
    return html`
      ${this.error ? html`<div class="status-message error">${this.error}</div>` : nothing}
      ${this.successMessage
        ? html`<div class="status-message success">${this.successMessage}</div>`
        : nothing}
      ${this.loading
        ? html`<div class="loading-container"><sl-spinner></sl-spinner></div>`
        : this.renderContent()}
    `;
  }

  private renderContent() {
    return html`
      <div class="header">
        <div class="header-left">
          <sl-icon name="globe"></sl-icon>
          <h1>Federation Authentication</h1>
        </div>
        <sl-button variant="primary" ?loading=${this.saving} @click=${() => { void this.save(); }}>
          Save
        </sl-button>
      </div>
      <p class="header-description">
        Manage trusted external OIDC issuers for cross-hub federation authentication.
      </p>

      ${this.renderGlobalSettings()}
      ${this.renderIssuersSection()}
      ${this.renderAddEditDialog()}
      ${this.renderDeleteDialog()}
    `;
  }

  // --- Global Settings Section ---

  private renderGlobalSettings() {
    return html`
      <div class="section">
        <div class="section-header">
          <h3 class="section-title">Global Settings</h3>
          <sl-button size="small" variant="default" ?loading=${this.downloading} @click=${() => { void this.downloadJWKS(); }}>
            <sl-icon slot="prefix" name="download"></sl-icon>
            Download JWKS
          </sl-button>
        </div>

        <div class="form-field mb-1">
          <sl-switch
            ?checked=${this.enabled}
            @sl-change=${(e: Event) => {
              this.enabled = (e.target as HTMLInputElement).checked;
            }}
          >
            Enabled
          </sl-switch>
          <span class="hint">Enable or disable federation authentication globally.</span>
        </div>

        <div class="form-grid">
          <div class="form-field">
            <label>Algorithms</label>
            <sl-select
              multiple
              .value=${this.algorithms.join(' ')}
              @sl-change=${(e: Event) => {
                const value = (e.target as HTMLSelectElement).value;
                this.algorithms = Array.isArray(value)
                  ? value as string[]
                  : value.split(' ').filter(Boolean);
              }}
            >
              <sl-option value="RS256">RS256</sl-option>
              <sl-option value="ES256">ES256</sl-option>
            </sl-select>
            <span class="hint">Allowed JWT signing algorithms.</span>
          </div>
        </div>

        <div class="cache-section">
          <label class="cache-section-label">
            JWKS Cache
          </label>
          <div class="cache-row">
            <div class="form-field">
              <label>Refresh Interval</label>
              <sl-input
                .value=${this.refreshInterval}
                placeholder="1h"
                @sl-change=${(e: Event) => {
                  this.refreshInterval = (e.target as HTMLInputElement).value;
                }}
              ></sl-input>
              <span class="hint">How often to refresh JWKS keys (e.g. 1h, 30m).</span>
            </div>
            <div class="form-field">
              <label>Debounce Interval</label>
              <sl-input
                .value=${this.debounceInterval}
                placeholder="5s"
                @sl-change=${(e: Event) => {
                  this.debounceInterval = (e.target as HTMLInputElement).value;
                }}
              ></sl-input>
              <span class="hint">Minimum delay between JWKS fetches (e.g. 5s).</span>
            </div>
          </div>
        </div>
      </div>
    `;
  }

  // --- Trusted Issuers Section ---

  private renderIssuersSection() {
    return html`
      <div class="section">
        <div class="section-header">
          <h3 class="section-title">Trusted Issuers</h3>
          <sl-button size="small" variant="primary" @click=${() => this.openAddDialog()}>
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            Add Issuer
          </sl-button>
        </div>

        ${this.issuers.length === 0
          ? html`
              <div class="empty-state">
                <sl-icon name="shield-lock"></sl-icon>
                <p>No trusted issuers configured.</p>
                <p style="font-size: 0.8125rem">
                  Add a trusted OIDC issuer to enable federation authentication from external hubs or services.
                </p>
              </div>
            `
          : html`
              <table class="issuer-table">
                <thead>
                  <tr>
                    <th>Issuer URL</th>
                    <th>Type</th>
                    <th>Audience</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  ${this.issuers.map((issuer, index) => html`
                    <tr>
                      <td class="issuer-url-cell" title=${issuer.issuer_url}>${issuer.issuer_url}</td>
                      <td>${issuer.issuer_type ?? 'hub'}</td>
                      <td>${issuer.expected_audience
                        ? html`<span title=${issuer.expected_audience}>${this.truncate(issuer.expected_audience, 30)}</span>`
                        : html`<span style="color: var(--scion-text-muted, #64748b);">—</span>`}
                      </td>
                      <td>
                        <div class="issuer-actions">
                          <sl-button size="small" variant="default" @click=${() => this.openEditDialog(index)}>
                            Edit
                          </sl-button>
                          <sl-button size="small" variant="danger" outline @click=${() => this.openDeleteDialog(index)}>
                            Delete
                          </sl-button>
                        </div>
                      </td>
                    </tr>
                  `)}
                </tbody>
              </table>
            `}
      </div>
    `;
  }

  // --- Add/Edit Issuer Dialog ---

  private renderAddEditDialog() {
    const title = this.editingIndex >= 0 ? 'Edit Trusted Issuer' : 'Add Trusted Issuer';
    const issuerType = this.editingIssuer?.issuer_type ?? 'hub';

    return html`
      <sl-dialog
        label=${title}
        ?open=${this.dialogOpen}
        @sl-request-close=${() => this.handleDialogClose()}
        style="--width: 32rem;"
      >
        ${this.dialogOpen && this.editingIssuer
          ? html`
              <div class="dialog-form">
                <sl-alert variant="warning" open>
                  <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                  Adding a trusted issuer grants authentication capability to agents from that
                  issuer. Only add issuers you control or explicitly trust.
                </sl-alert>

                <div class="form-field">
                  <label>Issuer URL *</label>
                  <sl-input
                    required
                    placeholder="https://hub-a.example.com"
                    .value=${this.editingIssuer.issuer_url}
                    @sl-input=${() => {
                      this.issuerUrlError = null;
                    }}
                    @sl-change=${(e: Event) => {
                      this.editingIssuer = {
                        ...this.editingIssuer!,
                        issuer_url: (e.target as HTMLInputElement).value,
                      };
                      this.issuerUrlError = null;
                    }}
                  ></sl-input>
                  ${this.issuerUrlError
                    ? html`<span class="hint" style="color: var(--scion-error-text, #991b1b);">${this.issuerUrlError}</span>`
                    : html`<span class="hint">The OIDC issuer URL of the trusted external hub or service.</span>`}
                </div>

                <div class="form-field">
                  <label>Issuer Type</label>
                  <sl-select
                    .value=${issuerType}
                    @sl-change=${(e: Event) => {
                      this.editingIssuer = {
                        ...this.editingIssuer!,
                        issuer_type: (e.target as HTMLSelectElement).value,
                      };
                    }}
                  >
                    <sl-option value="hub">Hub</sl-option>
                    <sl-option value="service_account">Service Account</sl-option>
                    <sl-option value="user">User</sl-option>
                  </sl-select>
                  <span class="hint">The type of entities this issuer authenticates.</span>
                </div>

                <div class="form-field">
                  <label>JWKS URL</label>
                  <sl-input
                    placeholder=${issuerType === 'hub'
                      ? 'Derived from issuer URL if empty'
                      : 'https://example.com/.well-known/jwks.json'}
                    .value=${this.editingIssuer.jwks_url ?? ''}
                    @sl-change=${(e: Event) => {
                      this.editingIssuer = {
                        ...this.editingIssuer!,
                        jwks_url: (e.target as HTMLInputElement).value,
                      };
                    }}
                  ></sl-input>
                  <span class="hint">
                    ${issuerType === 'hub'
                      ? 'Optional. Derived from issuer URL if left empty.'
                      : 'URL to fetch the JSON Web Key Set from.'}
                  </span>
                </div>

                <div class="form-field">
                  <label>Expected Audience</label>
                  <sl-input
                    placeholder="https://this-hub.example.com"
                    .value=${this.editingIssuer.expected_audience ?? ''}
                    @sl-change=${(e: Event) => {
                      this.editingIssuer = {
                        ...this.editingIssuer!,
                        expected_audience: (e.target as HTMLInputElement).value,
                      };
                    }}
                  ></sl-input>
                  <span class="hint">Expected audience claim in JWT tokens. Usually this hub's URL.</span>
                </div>

                ${issuerType === 'hub' ? this.renderHubFields() : nothing}
                ${issuerType === 'service_account' || issuerType === 'user'
                  ? this.renderEmailFields()
                  : nothing}
                ${issuerType === 'user' ? this.renderUserFields() : nothing}

                <div class="form-field">
                  <label>Default Scopes</label>
                  <sl-input
                    placeholder="agent:status:update, agent:logs:read"
                    .value=${this.arrayToCommaString(this.editingIssuer.default_scopes)}
                    @sl-change=${(e: Event) => {
                      this.editingIssuer = {
                        ...this.editingIssuer!,
                        default_scopes: this.commaStringToArray(
                          (e.target as HTMLInputElement).value,
                        ),
                      };
                    }}
                  ></sl-input>
                  <span class="hint">Comma-separated list of default token scopes.</span>
                </div>
              </div>

              <div slot="footer" class="dialog-footer">
                <sl-button variant="default" @click=${() => this.handleDialogClose()}>
                  Cancel
                </sl-button>
                <sl-button
                  variant="primary"
                  @click=${() => this.handleSaveIssuer()}
                  ?disabled=${!this.editingIssuer?.issuer_url?.trim()}
                >
                  Save Issuer
                </sl-button>
              </div>
            `
          : nothing}
      </sl-dialog>
    `;
  }

  private renderHubFields() {
    return html`
      <div class="conditional-section">
        <div class="conditional-section-title">Hub Options</div>
        <div class="form-field" style="margin-bottom: 0.75rem;">
          <label>Allowed Projects</label>
          <sl-input
            placeholder="project-a, project-b"
            .value=${this.arrayToCommaString(this.editingIssuer?.allowed_projects)}
            @sl-change=${(e: Event) => {
              this.editingIssuer = {
                ...this.editingIssuer!,
                allowed_projects: this.commaStringToArray(
                  (e.target as HTMLInputElement).value,
                ),
              };
            }}
          ></sl-input>
          <span class="hint">Comma-separated project slugs allowed from this hub.</span>
        </div>
        <div class="form-field">
          <label>Allowed Root Users</label>
          <sl-input
            placeholder="user@example.com"
            .value=${this.arrayToCommaString(this.editingIssuer?.allowed_root_users)}
            @sl-change=${(e: Event) => {
              this.editingIssuer = {
                ...this.editingIssuer!,
                allowed_root_users: this.commaStringToArray(
                  (e.target as HTMLInputElement).value,
                ),
              };
            }}
          ></sl-input>
          <span class="hint">Comma-separated emails of root users allowed from this hub.</span>
        </div>
      </div>
    `;
  }

  private renderEmailFields() {
    return html`
      <div class="conditional-section">
        <div class="conditional-section-title">
          ${this.editingIssuer?.issuer_type === 'service_account'
            ? 'Service Account Options'
            : 'User Options'}
        </div>
        <div class="form-field">
          <label>Allowed Emails</label>
          <sl-input
            placeholder="*@corp.com, admin@example.com"
            .value=${this.arrayToCommaString(this.editingIssuer?.allowed_emails)}
            @sl-change=${(e: Event) => {
              this.editingIssuer = {
                ...this.editingIssuer!,
                allowed_emails: this.commaStringToArray(
                  (e.target as HTMLInputElement).value,
                ),
              };
            }}
          ></sl-input>
          <span class="hint">Comma-separated email patterns. Use * for wildcards (e.g. *@corp.com).</span>
        </div>
      </div>
    `;
  }

  private renderUserFields() {
    return html`
      <div class="form-field" style="margin-top: 0.5rem;">
        <label>Default Role</label>
        <sl-select
          .value=${this.editingIssuer?.default_role ?? ''}
          placeholder="Select a default role"
          @sl-change=${(e: Event) => {
            this.editingIssuer = {
              ...this.editingIssuer!,
              default_role: (e.target as HTMLSelectElement).value,
            };
          }}
        >
          <sl-option value="admin">Admin</sl-option>
          <sl-option value="member">Member</sl-option>
          <sl-option value="viewer">Viewer</sl-option>
        </sl-select>
        <span class="hint">Default role assigned to users authenticating via this issuer.</span>
      </div>
    `;
  }

  // --- Delete Confirmation Dialog ---

  private renderDeleteDialog() {
    const issuer = this.deletingIndex >= 0 && this.deletingIndex < this.issuers.length
      ? this.issuers[this.deletingIndex]
      : null;

    return html`
      <sl-dialog
        label="Delete Trusted Issuer"
        ?open=${this.deleteDialogOpen}
        @sl-request-close=${() => this.handleDeleteDialogClose()}
        style="--width: 28rem;"
      >
        ${issuer
          ? html`
              <p style="font-size: 0.875rem; color: var(--scion-text, #1e293b); margin: 0;">
                Are you sure you want to remove the trusted issuer
                <strong>${issuer.issuer_url}</strong>?
                This will revoke federation trust for agents from this issuer.
              </p>
              <div slot="footer" class="dialog-footer">
                <sl-button variant="default" @click=${() => this.handleDeleteDialogClose()}>
                  Cancel
                </sl-button>
                <sl-button variant="danger" @click=${() => this.confirmDeleteIssuer()}>
                  Delete Issuer
                </sl-button>
              </div>
            `
          : nothing}
      </sl-dialog>
    `;
  }

  // --- Utility ---

  private truncate(text: string, maxLen: number): string {
    return text.length > maxLen ? text.substring(0, maxLen) + '...' : text;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-federation': ScionPageAdminFederation;
  }
}
