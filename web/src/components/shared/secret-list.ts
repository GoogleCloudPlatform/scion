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
 * Shared Secret List Component
 *
 * Full CRUD component for secrets. Used by both the profile secrets page
 * (scope=user) and the grove configuration page (scope=grove).
 *
 * In non-compact mode (profile page), renders a table with an add button.
 * In compact mode (grove page), wraps in a section with header/description.
 */

import { LitElement, html, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { Secret, SecretType, ResourceScope, InjectionMode } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { resourceStyles } from './resource-styles.js';

@customElement('scion-secret-list')
export class ScionSecretList extends LitElement {
  @property() scope: ResourceScope = 'user';
  @property() scopeId = '';
  @property() apiBasePath = '/api/v1';
  @property({ type: Boolean }) compact = false;

  @state() private loading = true;
  @state() private secrets: Secret[] = [];
  @state() private error: string | null = null;

  // Create/Update dialog
  @state() private dialogOpen = false;
  @state() private dialogMode: 'create' | 'update' = 'create';
  @state() private dialogKey = '';
  @state() private dialogValue = '';
  @state() private dialogDescription = '';
  @state() private dialogType: SecretType = 'environment';
  @state() private dialogTarget = '';
  @state() private dialogInjectionMode: InjectionMode = 'as_needed';
  @state() private dialogAllowProgeny = false;
  @state() private dialogLoading = false;
  @state() private dialogError: string | null = null;

  // Delete
  @state() private deletingKey: string | null = null;

  // Copy-to-clipboard feedback
  @state() private copiedSecretKey: string | null = null;

  static override styles = [resourceStyles];

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadSecrets();
  }

  private async loadSecrets(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const url =
        this.scope !== 'grove'
          ? `${this.apiBasePath}/secrets?scope=${this.scope}`
          : `${this.apiBasePath}/secrets`;
      const response = await apiFetch(url);

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      const data = (await response.json()) as { secrets?: Secret[] } | Secret[];
      this.secrets = Array.isArray(data) ? data : data.secrets || [];
    } catch (err) {
      console.error('Failed to load secrets:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load secrets';
    } finally {
      this.loading = false;
    }
  }

  private openCreateDialog(): void {
    this.dialogMode = 'create';
    this.dialogKey = '';
    this.dialogValue = '';
    this.dialogDescription = '';
    this.dialogType = 'environment';
    this.dialogTarget = '';
    this.dialogInjectionMode = 'as_needed';
    this.dialogAllowProgeny = false;
    this.dialogError = null;
    this.dialogOpen = true;
  }

  private openUpdateDialog(secret: Secret): void {
    this.dialogMode = 'update';
    this.dialogKey = secret.key;
    this.dialogValue = '';
    this.dialogDescription = secret.description || '';
    this.dialogType = secret.type;
    this.dialogTarget = secret.target || '';
    this.dialogInjectionMode = secret.injectionMode || 'as_needed';
    this.dialogAllowProgeny = secret.allowProgeny || false;
    this.dialogError = null;
    this.dialogOpen = true;
  }

  private closeDialog(): void {
    this.dialogOpen = false;
  }

  private async handleSave(e: Event): Promise<void> {
    e.preventDefault();

    const key = this.dialogKey.trim();
    if (!key) {
      this.dialogError = 'Key is required';
      return;
    }

    if (!this.dialogValue) {
      this.dialogError = 'Value is required';
      return;
    }

    this.dialogLoading = true;
    this.dialogError = null;

    try {
      const body: Record<string, unknown> = {
        value: this.dialogValue,
        scope: this.scope,
        description: this.dialogDescription || undefined,
        type: this.dialogType,
        target: this.dialogTarget || undefined,
        injectionMode: this.dialogInjectionMode,
        allowProgeny: this.scope === 'user' ? this.dialogAllowProgeny : undefined,
      };

      if (this.scope === 'grove') {
        body.scopeId = this.scopeId;
      }

      const response = await apiFetch(`${this.apiBasePath}/secrets/${encodeURIComponent(key)}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      this.closeDialog();
      await this.loadSecrets();
    } catch (err) {
      console.error('Failed to save secret:', err);
      this.dialogError = err instanceof Error ? err.message : 'Failed to save';
    } finally {
      this.dialogLoading = false;
    }
  }

  private async handleDelete(secret: Secret, event?: MouseEvent): Promise<void> {
    if (!event?.altKey && !confirm(`Delete secret "${secret.key}"? This cannot be undone.`)) {
      return;
    }

    this.deletingKey = secret.key;

    try {
      const deleteUrl =
        this.scope !== 'grove'
          ? `${this.apiBasePath}/secrets/${encodeURIComponent(secret.key)}?scope=${this.scope}`
          : `${this.apiBasePath}/secrets/${encodeURIComponent(secret.key)}`;
      const response = await apiFetch(deleteUrl, { method: 'DELETE' });

      if (!response.ok && response.status !== 204) {
        throw new Error(await extractApiError(response, `Failed to delete (HTTP ${response.status})`));
      }

      await this.loadSecrets();
    } catch (err) {
      console.error('Failed to delete secret:', err);
      alert(err instanceof Error ? err.message : 'Failed to delete');
    } finally {
      this.deletingKey = null;
    }
  }

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

  private async copySecretRef(secret: Secret): Promise<void> {
    if (!secret.secretRef) return;
    try {
      await navigator.clipboard.writeText(secret.secretRef);
      this.copiedSecretKey = secret.key;
      setTimeout(() => {
        this.copiedSecretKey = null;
      }, 1500);
    } catch {
      // Silently fail if clipboard is unavailable
    }
  }

  // ── Rendering ────────────────────────────────────────────────────────

  override render() {
    if (this.compact) {
      return this.renderCompact();
    }
    return this.renderFull();
  }

  private renderFull() {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading secrets...</p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load</h2>
          <p>There was a problem loading your secrets.</p>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${() => this.loadSecrets()}>
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
          Add Secret
        </sl-button>
      </div>
      ${this.secrets.length === 0 ? this.renderEmpty() : this.renderTable()} ${this.renderDialog()}
    `;
  }

  private renderCompact() {
    return html`
      <div class="section compact">
        <div class="section-header">
          <div class="section-header-info">
            <h2>Secrets</h2>
            <p>
              Manage encrypted secrets for ${this.scope === 'hub' ? 'all agents on this hub' : 'agents in this grove'}. Values are write-only and cannot be
              retrieved after saving.
            </p>
          </div>
          <sl-button variant="primary" size="small" @click=${this.openCreateDialog}>
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            Add Secret
          </sl-button>
        </div>

        ${this.loading
          ? html`<div class="section-loading"><sl-spinner></sl-spinner> Loading secrets...</div>`
          : this.error
            ? html`<div class="section-error">
                <span>${this.error}</span>
                <sl-button size="small" @click=${() => this.loadSecrets()}>
                  <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                  Retry
                </sl-button>
              </div>`
            : this.secrets.length === 0
              ? this.renderEmpty()
              : this.renderTable()}
      </div>
      ${this.renderDialog()}
    `;
  }

  private renderTable() {
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Key</th>
              <th>Type</th>
              <th>Inject</th>
              ${this.scope === 'user' ? html`<th>Progeny</th>` : nothing}
              <th class="hide-mobile">Description</th>
              <th>Version</th>
              <th class="hide-mobile">Updated</th>
              <th class="actions-cell"></th>
            </tr>
          </thead>
          <tbody>
            ${this.secrets.map((secret) => this.renderRow(secret))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderRow(secret: Secret) {
    const isDeleting = this.deletingKey === secret.key;

    return html`
      <tr>
        <td class="key-cell">
          <div class="key-info">
            <div class="key-icon">
              <sl-icon name="shield-lock"></sl-icon>
            </div>
            ${secret.key}
          </div>
        </td>
        <td>
          <span class="type-badge ${secret.type}">${secret.type}</span>
        </td>
        <td>
          ${secret.injectionMode === 'as_needed'
            ? html`<span class="badge inject-as-needed">as needed</span>`
            : html`<span class="badge inject-always">always</span>`}
        </td>
        ${this.scope === 'user'
          ? html`<td>${secret.allowProgeny ? html`<sl-icon name="check-lg" title="Progeny can access"></sl-icon>` : '\u2014'}</td>`
          : nothing}
        <td class="description-cell hide-mobile">${secret.description || '\u2014'}</td>
        <td>
          ${secret.secretRef
            ? html`<sl-tooltip content=${this.copiedSecretKey === secret.key ? 'Copied!' : secret.secretRef} hoist>
                <span
                  class="version-badge version-badge-copyable"
                  @click=${() => this.copySecretRef(secret)}
                  title=""
                >v${secret.version}</span>
              </sl-tooltip>`
            : html`<span class="version-badge">v${secret.version}</span>`}
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${this.formatRelativeTime(secret.updated)}</span>
        </td>
        <td class="actions-cell">
          <sl-icon-button
            name="arrow-clockwise"
            label="Update value"
            ?disabled=${isDeleting}
            @click=${() => this.openUpdateDialog(secret)}
          ></sl-icon-button>
          <sl-icon-button
            name="trash"
            label="Delete"
            ?disabled=${isDeleting}
            @click=${(e: MouseEvent) => this.handleDelete(secret, e)}
          ></sl-icon-button>
        </td>
      </tr>
    `;
  }

  private renderEmpty() {
    return html`
      <div class="empty-state">
        <sl-icon name="shield-lock"></sl-icon>
        <h3>No Secrets</h3>
        <p>
          Add encrypted secrets that will be securely injected into
          ${this.compact ? (this.scope === 'hub' ? 'all agents on this hub' : 'agents in this grove') : 'your agents'}.
        </p>
        <sl-button variant="primary" size="small" @click=${this.openCreateDialog}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Add Secret
        </sl-button>
      </div>
    `;
  }

  private renderDialog() {
    const isCreate = this.dialogMode === 'create';
    const title = isCreate ? 'Add Secret' : 'Update Secret';

    return html`
      <sl-dialog label=${title} ?open=${this.dialogOpen} @sl-request-close=${this.closeDialog}>
        <form class="dialog-form" @submit=${this.handleSave}>
          <sl-input
            label=${this.dialogType === 'file' ? 'Name' : 'Key'}
            placeholder=${this.dialogType === 'file' ? 'e.g. ssh_deploy_key' : 'e.g. GITHUB_TOKEN'}
            value=${this.dialogKey}
            ?disabled=${!isCreate}
            @sl-input=${(e: Event) => {
              this.dialogKey = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>

          <sl-input
            label="Value"
            placeholder="Secret value"
            value=${this.dialogValue}
            type="password"
            @sl-input=${(e: Event) => {
              this.dialogValue = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>

          <div class="dialog-hint">
            <sl-icon name="info-circle"></sl-icon>
            Secret values are encrypted and can never be retrieved after saving.
          </div>

          <sl-select
            label="Type"
            value=${this.dialogType}
            @sl-change=${(e: Event) => {
              this.dialogType = (e.target as HTMLSelectElement).value as SecretType;
            }}
          >
            <sl-option value="environment">Environment Variable</sl-option>
            <sl-option value="variable">Runtime Variable</sl-option>
            <sl-option value="file">File</sl-option>
          </sl-select>

          ${this.dialogType === 'file'
            ? html`
                <sl-input
                  label="Target Path"
                  placeholder="e.g. /home/agent/.ssh/id_rsa"
                  value=${this.dialogTarget}
                  @sl-input=${(e: Event) => {
                    this.dialogTarget = (e.target as HTMLInputElement).value;
                  }}
                ></sl-input>
              `
            : nothing}

          <sl-textarea
            label="Description"
            placeholder="Optional description"
            value=${this.dialogDescription}
            rows="2"
            resize="none"
            @sl-input=${(e: Event) => {
              this.dialogDescription = (e.target as HTMLTextAreaElement).value;
            }}
          ></sl-textarea>

          <div class="radio-field">
            <span class="radio-field-label">Inject</span>
            <sl-radio-group
              value=${this.dialogInjectionMode}
              @sl-change=${(e: Event) => {
                this.dialogInjectionMode = (e.target as HTMLInputElement).value as InjectionMode;
              }}
            >
              <sl-radio-button value="always">Always</sl-radio-button>
              <sl-radio-button value="as_needed">As needed</sl-radio-button>
            </sl-radio-group>
            <span class="radio-field-help">
              "As needed" injects only when the agent configuration requests this value.
            </span>
          </div>

          ${this.scope === 'user'
            ? html`
                <sl-switch
                  ?checked=${this.dialogAllowProgeny}
                  @sl-change=${(e: Event) => {
                    this.dialogAllowProgeny = (e.target as HTMLInputElement).checked;
                  }}
                >
                  Allow agent progeny to access
                </sl-switch>
                <span class="radio-field-help">
                  When enabled, agents spawned by your agents (and their descendants) will also receive this secret.
                </span>
              `
            : nothing}

          ${this.dialogError ? html`<div class="dialog-error">${this.dialogError}</div>` : nothing}
        </form>

        <sl-button
          slot="footer"
          variant="default"
          @click=${this.closeDialog}
          ?disabled=${this.dialogLoading}
        >
          Cancel
        </sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.dialogLoading}
          ?disabled=${this.dialogLoading}
          @click=${this.handleSave}
        >
          ${isCreate ? 'Create' : 'Update'}
        </sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-secret-list': ScionSecretList;
  }
}
