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
 * Shared resource list component
 *
 * Lists file-based resources (templates or harness-configs) for a given scope
 * and links each one to its detail/editor page. Used by both the project
 * settings Resources section and the Hub Resources page so the two render
 * identically.
 *
 * It does not handle import/creation — those affordances (where they exist,
 * e.g. template import) are rendered by the host page around this list.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';

export type ResourceKind = 'template' | 'harness-config';

interface ResourceItem {
  id: string;
  name: string;
  displayName?: string;
  description?: string;
  harness?: string;
}

@customElement('scion-resource-list')
export class ScionResourceList extends LitElement {
  /** Which resource type to list. */
  @property({ type: String })
  kind: ResourceKind = 'template';

  /** Resource scope: 'project' or 'global'. */
  @property({ type: String })
  scope: 'project' | 'global' = 'project';

  /** Scope id (project id) — required for project scope, omitted for global. */
  @property({ type: String })
  scopeId = '';

  /**
   * Base path for the detail link. The resource segment + id are appended,
   * e.g. `${detailBasePath}/harness-configs/{id}`.
   * Project pages pass `/projects/{id}`; the Hub Resources page passes `/settings`.
   */
  @property({ type: String })
  detailBasePath = '';

  /** Whether to show the Clone action on each row. */
  @property({ type: Boolean })
  canClone = false;

  /** Whether to show the Delete action on each row. */
  @property({ type: Boolean })
  canDelete = false;

  /** When true, show a "Clone from Global" button above the list. */
  @property({ type: Boolean })
  cloneFromGlobal = false;

  @state() private items: ResourceItem[] = [];
  @state() private loading = true;
  @state() private error: string | null = null;

  @state() private cloneTarget: ResourceItem | null = null;
  @state() private deleteTarget: ResourceItem | null = null;
  @state() private actionInProgress = false;
  @state() private actionError = '';
  @state() private cloneName = '';
  @state() private deleteFiles = true;

  @state() private globalPickerOpen = false;
  @state() private globalItems: ResourceItem[] = [];
  @state() private globalLoading = false;
  @state() private globalError = '';

  static override styles = css`
    :host {
      display: block;
    }

    .resource-list {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
    }

    .resource-row {
      display: flex;
      align-items: center;
      gap: 0;
      background: var(--scion-bg-subtle, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .resource-row:hover {
      border-color: var(--scion-primary, #3b82f6);
    }

    .resource-item {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.75rem 1rem;
      flex: 1;
      min-width: 0;
      text-decoration: none;
      color: inherit;
      cursor: pointer;
    }

    .resource-item > sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.125rem;
      flex-shrink: 0;
    }

    .resource-info {
      flex: 1;
      min-width: 0;
    }

    .resource-name {
      font-weight: 600;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .resource-meta {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.125rem;
    }

    .resource-badge {
      font-size: 0.6875rem;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      border: 1px solid var(--scion-border, #e2e8f0);
      white-space: nowrap;
    }

    .row-actions {
      flex-shrink: 0;
      padding-right: 0.5rem;
    }

    .menu-item-danger::part(base) {
      color: var(--sl-color-danger-600, #dc2626);
    }

    .empty {
      text-align: center;
      padding: 2rem 1rem;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }

    .empty sl-icon {
      font-size: 2rem;
      margin-bottom: 0.5rem;
      display: block;
    }

    .error {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.875rem;
      padding: 0.75rem 1rem;
      background: var(--sl-color-danger-50, #fef2f2);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .dialog-error {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.8125rem;
      margin-top: 0.5rem;
    }

    .dialog-warning {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.8125rem;
      color: var(--sl-color-danger-600, #dc2626);
      margin-top: 0.75rem;
    }

    .clone-global-btn {
      margin-bottom: 0.75rem;
    }

    .global-picker-list {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
      max-height: 400px;
      overflow-y: auto;
    }

    .global-picker-item {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.625rem 0.75rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      cursor: pointer;
      background: var(--scion-surface, #ffffff);
    }

    .global-picker-item:hover {
      border-color: var(--scion-primary, #3b82f6);
      background: var(--scion-bg-subtle, #f8fafc);
    }

    .global-picker-item sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1rem;
      flex-shrink: 0;
    }

    .global-picker-info {
      flex: 1;
      min-width: 0;
    }

    .global-picker-name {
      font-weight: 600;
      font-size: 0.8125rem;
      color: var(--scion-text, #1e293b);
    }

    .global-picker-desc {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.125rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.load();
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('kind') || changed.has('scope') || changed.has('scopeId')) {
      void this.load();
    }
  }

  private get apiResource(): string {
    return this.kind === 'template' ? 'templates' : 'harness-configs';
  }

  private get detailSegment(): string {
    return this.kind === 'template' ? 'templates' : 'harness-configs';
  }

  private get icon(): string {
    return this.kind === 'template' ? 'file-earmark-code' : 'sliders';
  }

  private get kindLabel(): string {
    return this.kind === 'template' ? 'template' : 'harness config';
  }

  /** Public method to refresh the list. */
  async load(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const params = new URLSearchParams({ status: 'active', limit: '100' });
      if (this.scope) params.set('scope', this.scope);
      // scopeId narrows to a single project (both template and harness-config
      // list handlers filter on scope_id); without it scope=project would match
      // every project's resources.
      if (this.scope === 'project' && this.scopeId) params.set('scopeId', this.scopeId);

      const response = await apiFetch(`/api/v1/${this.apiResource}?${params.toString()}`);
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
      const data = (await response.json()) as Record<string, ResourceItem[]>;
      const list = this.kind === 'template' ? data.templates : data.harnessConfigs;
      this.items = Array.isArray(list) ? list : [];
    } catch (err) {
      console.error(`Failed to load ${this.apiResource}:`, err);
      this.error = err instanceof Error ? err.message : `Failed to load ${this.apiResource}`;
    } finally {
      this.loading = false;
    }
  }

  private emitChanged(action: string, id: string, sourceId?: string) {
    this.dispatchEvent(
      new CustomEvent('resource-changed', {
        detail: { action, kind: this.kind, id, sourceId },
        bubbles: true,
        composed: true,
      })
    );
  }

  // ── Delete ──────────────────────────────────────────────────────────

  private openDeleteDialog(item: ResourceItem) {
    this.deleteTarget = item;
    this.deleteFiles = true;
    this.actionError = '';
    this.actionInProgress = false;
  }

  private closeDeleteDialog() {
    this.deleteTarget = null;
    this.actionError = '';
  }

  private async confirmDelete(): Promise<void> {
    if (!this.deleteTarget) return;
    this.actionInProgress = true;
    this.actionError = '';
    try {
      const params = new URLSearchParams({ deleteFiles: String(this.deleteFiles) });
      const response = await apiFetch(
        `/api/v1/${this.apiResource}/${this.deleteTarget.id}?${params.toString()}`,
        { method: 'DELETE' }
      );
      if (!response.ok && response.status !== 204) {
        throw new Error(
          await extractApiError(response, `Failed to delete: HTTP ${response.status}`)
        );
      }
      const deletedId = this.deleteTarget.id;
      this.closeDeleteDialog();
      await this.load();
      this.emitChanged('deleted', deletedId);
    } catch (err) {
      this.actionError = err instanceof Error ? err.message : 'Delete failed';
    } finally {
      this.actionInProgress = false;
    }
  }

  // ── Clone ───────────────────────────────────────────────────────────

  private openCloneDialog(item: ResourceItem) {
    this.cloneTarget = item;
    this.cloneName = `${item.name}-copy`;
    this.actionError = '';
    this.actionInProgress = false;
  }

  private closeCloneDialog() {
    this.cloneTarget = null;
    this.actionError = '';
  }

  private async confirmClone(): Promise<void> {
    if (!this.cloneTarget) return;
    this.actionInProgress = true;
    this.actionError = '';
    try {
      const body: Record<string, string> = { name: this.cloneName };
      if (this.scope) body.scope = this.scope;
      if (this.scope === 'project' && this.scopeId) body.scopeId = this.scopeId;

      const response = await apiFetch(
        `/api/v1/${this.apiResource}/${this.cloneTarget.id}/clone`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        }
      );
      if (response.status === 409) {
        this.actionError = 'A resource with this slug already exists. Choose a different name.';
        this.actionInProgress = false;
        return;
      }
      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `Failed to clone: HTTP ${response.status}`)
        );
      }
      const created = (await response.json()) as { id?: string };
      const sourceId = this.cloneTarget.id;
      this.closeCloneDialog();
      await this.load();
      this.emitChanged('cloned', created.id ?? '', sourceId);
    } catch (err) {
      this.actionError = err instanceof Error ? err.message : 'Clone failed';
    } finally {
      this.actionInProgress = false;
    }
  }

  // ── Clone from global ──────────────────────────────────────────────

  private async openGlobalPicker(): Promise<void> {
    this.globalPickerOpen = true;
    this.globalError = '';
    this.globalLoading = true;
    this.globalItems = [];
    try {
      const params = new URLSearchParams({ status: 'active', scope: 'global', limit: '100' });
      const response = await apiFetch(`/api/v1/${this.apiResource}?${params.toString()}`);
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
      const data = (await response.json()) as Record<string, ResourceItem[]>;
      const list = this.kind === 'template' ? data.templates : data.harnessConfigs;
      this.globalItems = Array.isArray(list) ? list : [];
    } catch (err) {
      this.globalError = err instanceof Error ? err.message : 'Failed to load global resources';
    } finally {
      this.globalLoading = false;
    }
  }

  private closeGlobalPicker() {
    this.globalPickerOpen = false;
    this.globalItems = [];
    this.globalError = '';
  }

  private selectGlobalItem(item: ResourceItem) {
    this.closeGlobalPicker();
    this.cloneTarget = item;
    this.cloneName = `${item.name}-copy`;
    this.actionError = '';
    this.actionInProgress = false;
  }

  // ── Render ─────────────────────────────────────────────────────────

  override render() {
    if (this.loading) {
      return html`<div class="empty"><sl-spinner></sl-spinner></div>`;
    }
    if (this.error) {
      return html`<div class="error">${this.error}</div>`;
    }

    const hasActions = this.canClone || this.canDelete;

    return html`
      ${this.cloneFromGlobal && this.canClone
        ? html`
            <div class="clone-global-btn">
              <sl-button size="small" variant="default" @click=${() => this.openGlobalPicker()}>
                <sl-icon slot="prefix" name="download"></sl-icon>
                Clone from Global
              </sl-button>
            </div>
          `
        : nothing}
      ${this.items.length === 0
        ? this.renderEmpty()
        : html`
            <div class="resource-list">
              ${this.items.map((item) => this.renderItem(item, hasActions))}
            </div>
          `}
      ${this.renderDeleteDialog()} ${this.renderCloneDialog()}
      ${this.renderGlobalPickerDialog()}
    `;
  }

  private renderItem(item: ResourceItem, hasActions: boolean) {
    if (!hasActions) {
      return html`
        <a href="${this.detailBasePath}/${this.detailSegment}/${item.id}" class="resource-row resource-item">
          <sl-icon name=${this.icon}></sl-icon>
          <div class="resource-info">
            <div class="resource-name">${item.displayName || item.name}</div>
            ${item.description ? html`<div class="resource-meta">${item.description}</div>` : ''}
          </div>
          ${item.harness ? html`<span class="resource-badge">${item.harness}</span>` : ''}
          <sl-icon
            name="chevron-right"
            style="color: var(--sl-color-neutral-400); font-size: 0.875rem;"
          ></sl-icon>
        </a>
      `;
    }

    return html`
      <div class="resource-row">
        <a href="${this.detailBasePath}/${this.detailSegment}/${item.id}" class="resource-item">
          <sl-icon name=${this.icon}></sl-icon>
          <div class="resource-info">
            <div class="resource-name">${item.displayName || item.name}</div>
            ${item.description ? html`<div class="resource-meta">${item.description}</div>` : ''}
          </div>
          ${item.harness ? html`<span class="resource-badge">${item.harness}</span>` : ''}
          <sl-icon
            name="chevron-right"
            style="color: var(--sl-color-neutral-400); font-size: 0.875rem;"
          ></sl-icon>
        </a>
        <div class="row-actions">
          <sl-dropdown placement="bottom-end" hoist>
            <sl-button slot="trigger" size="small" variant="text" caret>
              <sl-icon name="three-dots-vertical"></sl-icon>
            </sl-button>
            <sl-menu>
              ${this.canClone
                ? html`
                    <sl-menu-item @click=${() => this.openCloneDialog(item)}>
                      <sl-icon slot="prefix" name="copy"></sl-icon>
                      Clone
                    </sl-menu-item>
                  `
                : nothing}
              ${this.canClone && this.canDelete ? html`<sl-divider></sl-divider>` : nothing}
              ${this.canDelete
                ? html`
                    <sl-menu-item class="menu-item-danger" @click=${() => this.openDeleteDialog(item)}>
                      <sl-icon slot="prefix" name="trash"></sl-icon>
                      Delete
                    </sl-menu-item>
                  `
                : nothing}
            </sl-menu>
          </sl-dropdown>
        </div>
      </div>
    `;
  }

  private renderEmpty() {
    const label = this.kind === 'template' ? 'templates' : 'harness configs';
    return html`
      <div class="empty">
        <sl-icon name="file-earmark"></sl-icon>
        <p>No ${this.scope === 'global' ? 'global' : 'project'} ${label} yet.</p>
      </div>
    `;
  }

  // ── Dialogs ────────────────────────────────────────────────────────

  private renderDeleteDialog() {
    if (!this.deleteTarget) return nothing;
    return html`
      <sl-dialog
        label="Delete ${this.kindLabel}"
        open
        @sl-request-close=${(e: Event) => {
          if (this.actionInProgress) e.preventDefault();
          else this.closeDeleteDialog();
        }}
      >
        <p>
          Are you sure you want to delete
          <strong>${this.deleteTarget.displayName || this.deleteTarget.name}</strong>?
        </p>
        <sl-checkbox
          ?checked=${this.deleteFiles}
          @sl-change=${(e: Event) => {
            this.deleteFiles = (e.target as HTMLInputElement).checked;
          }}
        >
          Also delete stored files
        </sl-checkbox>
        <div class="dialog-warning">
          <sl-icon name="exclamation-triangle"></sl-icon>
          This action cannot be undone.
        </div>
        ${this.actionError ? html`<div class="dialog-error">${this.actionError}</div>` : nothing}
        <div slot="footer">
          <sl-button
            variant="default"
            size="small"
            ?disabled=${this.actionInProgress}
            @click=${() => this.closeDeleteDialog()}
          >
            Cancel
          </sl-button>
          <sl-button
            variant="danger"
            size="small"
            ?loading=${this.actionInProgress}
            ?disabled=${this.actionInProgress}
            @click=${() => this.confirmDelete()}
          >
            Delete
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  private renderCloneDialog() {
    if (!this.cloneTarget) return nothing;

    const isFromGlobal =
      this.cloneFromGlobal && this.scope === 'project' && !this.items.find((i) => i.id === this.cloneTarget!.id);

    return html`
      <sl-dialog
        label="Clone ${this.kindLabel}"
        open
        @sl-request-close=${(e: Event) => {
          if (this.actionInProgress) e.preventDefault();
          else this.closeCloneDialog();
        }}
      >
        <p>
          Clone <strong>${this.cloneTarget.displayName || this.cloneTarget.name}</strong>
          ${isFromGlobal ? html` from global into this project` : nothing}.
        </p>
        <sl-input
          label="New name"
          .value=${this.cloneName}
          @sl-input=${(e: Event) => {
            this.cloneName = (e.target as HTMLInputElement).value;
          }}
        ></sl-input>
        ${this.actionError ? html`<div class="dialog-error">${this.actionError}</div>` : nothing}
        <div slot="footer">
          <sl-button
            variant="default"
            size="small"
            ?disabled=${this.actionInProgress}
            @click=${() => this.closeCloneDialog()}
          >
            Cancel
          </sl-button>
          <sl-button
            variant="primary"
            size="small"
            ?loading=${this.actionInProgress}
            ?disabled=${this.actionInProgress || !this.cloneName.trim()}
            @click=${() => this.confirmClone()}
          >
            Clone
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  private renderGlobalPickerDialog() {
    if (!this.globalPickerOpen) return nothing;
    const label = this.kind === 'template' ? 'templates' : 'harness configs';
    return html`
      <sl-dialog
        label="Clone from Global"
        open
        @sl-request-close=${() => this.closeGlobalPicker()}
      >
        <p>Select a global ${this.kindLabel} to clone into this project.</p>
        ${this.globalLoading
          ? html`<div class="empty"><sl-spinner></sl-spinner></div>`
          : this.globalError
            ? html`<div class="dialog-error">${this.globalError}</div>`
            : this.globalItems.length === 0
              ? html`<div class="empty">No global ${label} available.</div>`
              : html`
                  <div class="global-picker-list">
                    ${this.globalItems.map(
                      (item) => html`
                        <div
                          class="global-picker-item"
                          @click=${() => this.selectGlobalItem(item)}
                        >
                          <sl-icon name=${this.icon}></sl-icon>
                          <div class="global-picker-info">
                            <div class="global-picker-name">
                              ${item.displayName || item.name}
                            </div>
                            ${item.description
                              ? html`<div class="global-picker-desc">${item.description}</div>`
                              : nothing}
                          </div>
                          ${item.harness
                            ? html`<span class="resource-badge">${item.harness}</span>`
                            : nothing}
                        </div>
                      `
                    )}
                  </div>
                `}
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-resource-list': ScionResourceList;
  }
}
