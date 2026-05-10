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
 * Shared Directory List Component
 *
 * CRUD component for project shared directories. Shared dirs provide
 * filesystem-level state sharing between agents in a project.
 *
 * Displays as a compact section on the project settings page.
 */

import { LitElement, html, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { SharedDir } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { resourceStyles } from './resource-styles.js';

const SLUG_PATTERN = /^[a-z0-9][a-z0-9-]*[a-z0-9]$/;

@customElement('scion-shared-dir-list')
export class ScionSharedDirList extends LitElement {
  @property() projectId = '';
  @property() apiBasePath = '';

  @state() private loading = true;
  @state() private sharedDirs: SharedDir[] = [];
  @state() private error: string | null = null;

  // Create dialog
  @state() private dialogOpen = false;
  @state() private dialogName = '';
  @state() private dialogReadOnly = false;
  @state() private dialogInWorkspace = false;
  @state() private dialogLoading = false;
  @state() private dialogError: string | null = null;

  // Delete
  @state() private deletingName: string | null = null;

  static override styles = [resourceStyles];

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadSharedDirs();
  }

  private get basePath(): string {
    return this.apiBasePath || `/api/v1/projects/${this.projectId}`;
  }

  private async loadSharedDirs(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const response = await apiFetch(`${this.basePath}/shared-dirs`);

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      const data = (await response.json()) as { sharedDirs?: SharedDir[] } | SharedDir[];
      this.sharedDirs = Array.isArray(data) ? data : data.sharedDirs || [];
    } catch (err) {
      console.error('Failed to load shared directories:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load shared directories';
    } finally {
      this.loading = false;
    }
  }

  private openCreateDialog(): void {
    this.dialogName = '';
    this.dialogReadOnly = false;
    this.dialogInWorkspace = false;
    this.dialogError = null;
    this.dialogOpen = true;
  }

  private closeDialog(): void {
    this.dialogOpen = false;
  }

  private async handleCreate(e: Event): Promise<void> {
    e.preventDefault();

    const name = this.dialogName.trim();
    if (!name) {
      this.dialogError = 'Name is required';
      return;
    }

    if (name.length < 2) {
      this.dialogError = 'Name must be at least 2 characters';
      return;
    }

    if (!SLUG_PATTERN.test(name)) {
      this.dialogError =
        'Name must be lowercase alphanumeric with hyphens (e.g. "build-cache")';
      return;
    }

    if (this.sharedDirs.some((d) => d.name === name)) {
      this.dialogError = `A shared directory named "${name}" already exists`;
      return;
    }

    this.dialogLoading = true;
    this.dialogError = null;

    try {
      const body: SharedDir = {
        name,
        read_only: this.dialogReadOnly,
        in_workspace: this.dialogInWorkspace,
      };

      const response = await apiFetch(`${this.basePath}/shared-dirs`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      this.closeDialog();
      await this.loadSharedDirs();
    } catch (err) {
      console.error('Failed to create shared directory:', err);
      this.dialogError = err instanceof Error ? err.message : 'Failed to create';
    } finally {
      this.dialogLoading = false;
    }
  }

  private async handleDelete(dir: SharedDir, event?: MouseEvent): Promise<void> {
    if (
      !event?.altKey &&
      !confirm(
        `Remove shared directory "${dir.name}" from this project?\n\nThis removes the configuration. Host-side data may still exist.`
      )
    ) {
      return;
    }

    this.deletingName = dir.name;

    try {
      const response = await apiFetch(
        `${this.basePath}/shared-dirs/${encodeURIComponent(dir.name)}`,
        { method: 'DELETE' }
      );

      if (!response.ok && response.status !== 204) {
        throw new Error(await extractApiError(response, `Failed to delete (HTTP ${response.status})`));
      }

      await this.loadSharedDirs();
    } catch (err) {
      console.error('Failed to delete shared directory:', err);
      alert(err instanceof Error ? err.message : 'Failed to delete');
    } finally {
      this.deletingName = null;
    }
  }

  // ── Rendering ────────────────────────────────────────────────────────

  override render() {
    return html`
      <div class="section compact">
        <div class="section-header">
          <div class="section-header-info">
            <h2>Shared Directories</h2>
            <p>
              Shared directories provide filesystem-level state sharing between agents in this project.
            </p>
          </div>
          <sl-button variant="primary" size="small" @click=${this.openCreateDialog}>
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            Add Directory
          </sl-button>
        </div>

        ${this.loading
          ? html`<div class="section-loading">
              <sl-spinner></sl-spinner> Loading shared directories...
            </div>`
          : this.error
            ? html`<div class="section-error">
                <span>${this.error}</span>
                <sl-button size="small" @click=${() => this.loadSharedDirs()}>
                  <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                  Retry
                </sl-button>
              </div>`
            : this.sharedDirs.length === 0
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
              <th>Name</th>
              <th>Mount Path</th>
              <th>Mode</th>
              <th class="actions-cell"></th>
            </tr>
          </thead>
          <tbody>
            ${this.sharedDirs.map((dir) => this.renderRow(dir))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderRow(dir: SharedDir) {
    const isDeleting = this.deletingName === dir.name;
    const mountPath = dir.in_workspace
      ? `/workspace/.scion-volumes/${dir.name}`
      : `/scion-volumes/${dir.name}`;

    return html`
      <tr>
        <td class="key-cell">${dir.name}</td>
        <td class="value-cell">${mountPath}</td>
        <td>
          <div class="badges">
            ${dir.read_only
              ? html`<span class="badge sensitive">
                  <sl-icon name="eye-slash" style="font-size: 0.625rem;"></sl-icon>
                  read-only
                </span>`
              : html`<span class="badge inject-always">read-write</span>`}
            ${dir.in_workspace
              ? html`<span class="badge inject-as-needed">in-workspace</span>`
              : nothing}
          </div>
        </td>
        <td class="actions-cell">
          <sl-icon-button
            name="trash"
            label="Delete"
            ?disabled=${isDeleting}
            @click=${(e: MouseEvent) => this.handleDelete(dir, e)}
          ></sl-icon-button>
        </td>
      </tr>
    `;
  }

  private renderEmpty() {
    return html`
      <div class="empty-state">
        <sl-icon name="folder"></sl-icon>
        <h3>No Shared Directories</h3>
        <p>
          Add shared directories to enable filesystem-level state sharing between agents
          (e.g. build caches, artifacts, coordination files).
        </p>
        <sl-button variant="primary" size="small" @click=${this.openCreateDialog}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Add Directory
        </sl-button>
      </div>
    `;
  }

  private renderDialog() {
    return html`
      <sl-dialog
        label="Add Shared Directory"
        ?open=${this.dialogOpen}
        @sl-request-close=${this.closeDialog}
      >
        <form class="dialog-form" @submit=${this.handleCreate}>
          <sl-input
            label="Name"
            placeholder="e.g. build-cache"
            help-text="Lowercase alphanumeric with hyphens. Used as the directory name."
            value=${this.dialogName}
            @sl-input=${(e: Event) => {
              this.dialogName = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>

          <div class="checkbox-group">
            <label class="checkbox-label">
              <input
                type="checkbox"
                .checked=${this.dialogReadOnly}
                @change=${(e: Event) => {
                  this.dialogReadOnly = (e.target as HTMLInputElement).checked;
                }}
              />
              <span class="checkbox-text">
                <span>Read-only</span>
                <span class="checkbox-description">
                  Agents can read but not write to this directory
                </span>
              </span>
            </label>

            <label class="checkbox-label">
              <input
                type="checkbox"
                .checked=${this.dialogInWorkspace}
                @change=${(e: Event) => {
                  this.dialogInWorkspace = (e.target as HTMLInputElement).checked;
                }}
              />
              <span class="checkbox-text">
                <span>Mount in workspace</span>
                <span class="checkbox-description">
                  Mount at /workspace/.scion-volumes/ instead of /scion-volumes/.
                  Useful for caches that tools expect relative to the project root.
                </span>
              </span>
            </label>
          </div>

          ${this.dialogError
            ? html`<div class="dialog-error">${this.dialogError}</div>`
            : nothing}
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
          @click=${this.handleCreate}
        >
          Create
        </sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-shared-dir-list': ScionSharedDirList;
  }
}
