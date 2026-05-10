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
 * Shared Environment Variable List Component
 *
 * Full CRUD component for environment variables. Used by both the profile
 * env vars page (scope=user) and the project configuration page (scope=project).
 *
 * In non-compact mode (profile page), renders a table with an add button.
 * In compact mode (project page), wraps in a section with header/description.
 */

import { LitElement, html, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { EnvVar, ResourceScope, InjectionMode } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { resourceStyles } from './resource-styles.js';

@customElement('scion-env-var-list')
export class ScionEnvVarList extends LitElement {
  @property() scope: ResourceScope = 'user';
  @property() scopeId = '';
  @property() apiBasePath = '/api/v1';
  @property({ type: Boolean }) compact = false;

  @state() private loading = true;
  @state() private envVars: EnvVar[] = [];
  @state() private error: string | null = null;

  // Create/Edit dialog
  @state() private dialogOpen = false;
  @state() private dialogMode: 'create' | 'edit' = 'create';
  @state() private dialogKey = '';
  @state() private dialogValue = '';
  @state() private dialogDescription = '';
  @state() private dialogSensitive = false;
  @state() private dialogSecret = false;
  @state() private dialogInjectionMode: InjectionMode = 'as_needed';
  @state() private dialogLoading = false;
  @state() private dialogError: string | null = null;

  // Delete
  @state() private deletingKey: string | null = null;

  static override styles = [resourceStyles];

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadEnvVars();
  }

  private async loadEnvVars(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const url =
        this.scope !== 'project' ? `${this.apiBasePath}/env?scope=${this.scope}` : `${this.apiBasePath}/env`;
      const response = await apiFetch(url);

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      const data = (await response.json()) as { envVars?: EnvVar[] } | EnvVar[];
      this.envVars = Array.isArray(data) ? data : data.envVars || [];
    } catch (err) {
      console.error('Failed to load environment variables:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load environment variables';
    } finally {
      this.loading = false;
    }
  }

  private openCreateDialog(): void {
    this.dialogMode = 'create';
    this.dialogKey = '';
    this.dialogValue = '';
    this.dialogDescription = '';
    this.dialogSensitive = false;
    this.dialogSecret = false;
    this.dialogInjectionMode = 'as_needed';
    this.dialogError = null;
    this.dialogOpen = true;
  }

  private openEditDialog(envVar: EnvVar): void {
    this.dialogMode = 'edit';
    this.dialogKey = envVar.key;
    this.dialogValue = envVar.sensitive || envVar.secret ? '' : envVar.value;
    this.dialogDescription = envVar.description || '';
    this.dialogSensitive = envVar.sensitive;
    this.dialogSecret = envVar.secret;
    this.dialogInjectionMode = envVar.injectionMode || 'as_needed';
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

    if (this.dialogMode === 'create' && !this.dialogValue) {
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
        sensitive: this.dialogSensitive,
        secret: this.dialogSecret,
        injectionMode: this.dialogInjectionMode,
      };

      if (this.scope === 'project') {
        body.scopeId = this.scopeId;
      }

      const response = await apiFetch(`${this.apiBasePath}/env/${encodeURIComponent(key)}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      this.closeDialog();
      await this.loadEnvVars();
    } catch (err) {
      console.error('Failed to save environment variable:', err);
      this.dialogError = err instanceof Error ? err.message : 'Failed to save';
    } finally {
      this.dialogLoading = false;
    }
  }

  private async handleDelete(envVar: EnvVar, event?: MouseEvent): Promise<void> {
    if (!event?.altKey && !confirm(`Delete environment variable "${envVar.key}"? This cannot be undone.`)) {
      return;
    }

    this.deletingKey = envVar.key;

    try {
      const deleteUrl =
        this.scope !== 'project'
          ? `${this.apiBasePath}/env/${encodeURIComponent(envVar.key)}?scope=${this.scope}`
          : `${this.apiBasePath}/env/${encodeURIComponent(envVar.key)}`;
      const response = await apiFetch(deleteUrl, { method: 'DELETE' });

      if (!response.ok && response.status !== 204) {
        throw new Error(await extractApiError(response, `Failed to delete (HTTP ${response.status})`));
      }

      await this.loadEnvVars();
    } catch (err) {
      console.error('Failed to delete environment variable:', err);
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
          <p>Loading environment variables...</p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load</h2>
          <p>There was a problem loading your environment variables.</p>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${() => this.loadEnvVars()}>
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
          Add Variable
        </sl-button>
      </div>
      ${this.envVars.length === 0 ? this.renderEmpty() : this.renderTable()} ${this.renderDialog()}
    `;
  }

  private renderCompact() {
    return html`
      <div class="section compact">
        <div class="section-header">
          <div class="section-header-info">
            <h2>Environment Variables</h2>
            <p>Manage environment variables injected into ${this.scope === 'hub' ? 'all agents on this hub' : 'agents in this project'} at runtime.</p>
          </div>
          <sl-button variant="primary" size="small" @click=${this.openCreateDialog}>
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            Add Variable
          </sl-button>
        </div>

        ${this.loading
          ? html`<div class="section-loading">
              <sl-spinner></sl-spinner> Loading environment variables...
            </div>`
          : this.error
            ? html`<div class="section-error">
                <span>${this.error}</span>
                <sl-button size="small" @click=${() => this.loadEnvVars()}>
                  <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                  Retry
                </sl-button>
              </div>`
            : this.envVars.length === 0
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
              <th>Value</th>
              <th class="hide-mobile">Description</th>
              <th>Inject</th>
              <th>Flags</th>
              <th class="hide-mobile">Updated</th>
              <th class="actions-cell"></th>
            </tr>
          </thead>
          <tbody>
            ${this.envVars.map((envVar) => this.renderRow(envVar))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderRow(envVar: EnvVar) {
    const isDeleting = this.deletingKey === envVar.key;
    const displayValue =
      envVar.secret || envVar.sensitive
        ? '\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022'
        : envVar.value;

    return html`
      <tr>
        <td class="key-cell">${envVar.key}</td>
        <td class="value-cell">${displayValue}</td>
        <td class="description-cell hide-mobile">${envVar.description || '\u2014'}</td>
        <td>
          ${envVar.injectionMode === 'as_needed'
            ? html`<span class="badge inject-as-needed">as needed</span>`
            : html`<span class="badge inject-always">always</span>`}
        </td>
        <td>
          <div class="badges">
            ${envVar.sensitive
              ? html`<span class="badge sensitive">
                  <sl-icon name="eye-slash" style="font-size: 0.625rem;"></sl-icon>
                  sensitive
                </span>`
              : nothing}
            ${envVar.secret
              ? html`<span class="badge secret">
                  <sl-icon name="shield-lock" style="font-size: 0.625rem;"></sl-icon>
                  secret
                </span>`
              : nothing}
          </div>
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${this.formatRelativeTime(envVar.updated)}</span>
        </td>
        <td class="actions-cell">
          <sl-icon-button
            name="pencil"
            label="Edit"
            ?disabled=${isDeleting}
            @click=${() => this.openEditDialog(envVar)}
          ></sl-icon-button>
          <sl-icon-button
            name="trash"
            label="Delete"
            ?disabled=${isDeleting}
            @click=${(e: MouseEvent) => this.handleDelete(envVar, e)}
          ></sl-icon-button>
        </td>
      </tr>
    `;
  }

  private renderEmpty() {
    return html`
      <div class="empty-state">
        <sl-icon name="terminal"></sl-icon>
        <h3>No Environment Variables</h3>
        <p>
          Add environment variables that will be injected into
          ${this.compact ? (this.scope === 'hub' ? 'all agents on this hub' : 'agents in this project') : 'your agents'}.
        </p>
        <sl-button variant="primary" size="small" @click=${this.openCreateDialog}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Add Variable
        </sl-button>
      </div>
    `;
  }

  private renderDialog() {
    const title =
      this.dialogMode === 'create' ? 'Add Environment Variable' : 'Edit Environment Variable';
    const isCreate = this.dialogMode === 'create';

    return html`
      <sl-dialog label=${title} ?open=${this.dialogOpen} @sl-request-close=${this.closeDialog}>
        <form class="dialog-form" @submit=${this.handleSave}>
          <sl-input
            label="Key"
            placeholder="e.g. API_TOKEN"
            value=${this.dialogKey}
            ?disabled=${!isCreate}
            @sl-input=${(e: Event) => {
              this.dialogKey = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>

          <sl-input
            label="Value"
            placeholder=${this.dialogMode === 'edit' && (this.dialogSensitive || this.dialogSecret)
              ? 'Enter new value to update'
              : 'Variable value'}
            value=${this.dialogValue}
            type=${this.dialogSecret || this.dialogSensitive ? 'password' : 'text'}
            @sl-input=${(e: Event) => {
              this.dialogValue = (e.target as HTMLInputElement).value;
            }}
            ?required=${isCreate}
          ></sl-input>

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

          <div class="checkbox-group">
            <label class="checkbox-label">
              <input
                type="checkbox"
                .checked=${this.dialogSensitive}
                @change=${(e: Event) => {
                  this.dialogSensitive = (e.target as HTMLInputElement).checked;
                }}
              />
              <span class="checkbox-text">
                <span>Sensitive</span>
                <span class="checkbox-description"> Mask value in API responses and UI </span>
              </span>
            </label>

            <label class="checkbox-label">
              <input
                type="checkbox"
                .checked=${this.dialogSecret}
                @change=${(e: Event) => {
                  this.dialogSecret = (e.target as HTMLInputElement).checked;
                  if (this.dialogSecret) {
                    this.dialogSensitive = true;
                  }
                }}
              />
              <span class="checkbox-text">
                <span>Store as Secret</span>
                <span class="checkbox-description">
                  Encrypt and store securely. Value will never be readable after saving.
                </span>
              </span>
            </label>
          </div>

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
    'scion-env-var-list': ScionEnvVarList;
  }
}
