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
 * Project template list component
 *
 * Lists project templates (projects marked with the scion.io/template label)
 * and provides CRUD operations: create template from project, create project
 * from template, rename, and delete.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';

interface ProjectTemplate {
  id: string;
  name: string;
  slug: string;
  created: string;
}

interface ProjectItem {
  id: string;
  name: string;
  slug: string;
}

@customElement('scion-project-template-list')
export class ScionProjectTemplateList extends LitElement {
  @state() private templates: ProjectTemplate[] = [];
  @state() private loading = true;
  @state() private error: string | null = null;

  // Create template dialog state
  @state() private createDialogOpen = false;
  @state() private createLoading = false;
  @state() private createError = '';
  @state() private sourceProjects: ProjectItem[] = [];
  @state() private selectedSourceId = '';
  @state() private newTemplateName = '';
  @state() private loadingProjects = false;

  // Create-from (clone from template) dialog state
  @state() private createFromTarget: ProjectTemplate | null = null;
  @state() private createFromName = '';
  @state() private createFromLoading = false;
  @state() private createFromError = '';

  // Rename dialog state
  @state() private renameTarget: ProjectTemplate | null = null;
  @state() private renameName = '';
  @state() private renameLoading = false;
  @state() private renameError = '';

  // Delete dialog state
  @state() private deleteTarget: ProjectTemplate | null = null;
  @state() private deleteLoading = false;
  @state() private deleteError = '';

  static override styles = css`
    :host {
      display: block;
    }

    .template-list {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
    }

    .template-row {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.75rem 1rem;
      background: var(--scion-bg-subtle, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .template-row:hover {
      border-color: var(--scion-primary, #3b82f6);
    }

    .template-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.125rem;
      flex-shrink: 0;
    }

    .template-info {
      flex: 1;
      min-width: 0;
    }

    .template-name {
      font-weight: 600;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .template-meta {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.125rem;
    }

    .row-actions {
      flex-shrink: 0;
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

    .error-banner {
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

    .create-btn {
      margin-bottom: 0.75rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.load();
  }

  /** Load templates from the API. */
  private async load(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const response = await apiFetch('/api/v1/projects?isTemplate=true&limit=100');
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
      const data = (await response.json()) as { projects?: ProjectTemplate[] };
      this.templates = (data.projects ?? []).slice().sort((a, b) => a.name.localeCompare(b.name));
    } catch (err) {
      console.error('Failed to load project templates:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load project templates';
    } finally {
      this.loading = false;
    }
  }

  // ── Create Template ────────────────────────────────────────────────

  private async openCreateDialog(): Promise<void> {
    this.createDialogOpen = true;
    this.createError = '';
    this.createLoading = false;
    this.selectedSourceId = '';
    this.newTemplateName = '';
    this.loadingProjects = true;
    this.sourceProjects = [];
    try {
      const response = await apiFetch('/api/v1/projects?limit=100');
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const data = (await response.json()) as { projects?: ProjectItem[] };
      this.sourceProjects = (data.projects ?? []).slice().sort((a, b) => a.name.localeCompare(b.name));
    } catch (err) {
      this.createError = err instanceof Error ? err.message : 'Failed to load projects';
    } finally {
      this.loadingProjects = false;
    }
  }

  private closeCreateDialog(): void {
    this.createDialogOpen = false;
    this.createError = '';
  }

  private onSourceProjectChange(e: Event): void {
    const oldSourceId = this.selectedSourceId;
    this.selectedSourceId = (e.target as HTMLSelectElement).value;
    const oldProject = this.sourceProjects.find(p => p.id === oldSourceId);
    const project = this.sourceProjects.find(p => p.id === this.selectedSourceId);
    if (project) {
      const oldDefaultName = oldProject ? `${oldProject.name} Template` : '';
      if (!this.newTemplateName || this.newTemplateName === oldDefaultName) {
        this.newTemplateName = `${project.name} Template`;
      }
    }
  }

  private async confirmCreateTemplate(): Promise<void> {
    if (!this.selectedSourceId) {
      this.createError = 'Please select a source project.';
      return;
    }
    if (!this.newTemplateName.trim()) {
      this.createError = 'Template name is required.';
      return;
    }
    this.createLoading = true;
    this.createError = '';
    try {
      const response = await apiFetch(`/api/v1/projects/${this.selectedSourceId}/clone`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: this.newTemplateName.trim(), asTemplate: true }),
      });
      if (!response.ok) {
        const errorText = await extractApiError(response, 'Failed to create template');
        throw new Error(errorText);
      }
      this.closeCreateDialog();
      await this.load();
    } catch (err) {
      this.createError = err instanceof Error ? err.message : 'Failed to create template';
    } finally {
      this.createLoading = false;
    }
  }

  // ── Create From Template ───────────────────────────────────────────

  private openCreateFromDialog(template: ProjectTemplate): void {
    this.createFromTarget = template;
    this.createFromName = '';
    this.createFromError = '';
    this.createFromLoading = false;
  }

  private closeCreateFromDialog(): void {
    this.createFromTarget = null;
    this.createFromError = '';
  }

  private async confirmCreateFrom(): Promise<void> {
    if (!this.createFromTarget) return;
    if (!this.createFromName.trim()) {
      this.createFromError = 'Project name is required.';
      return;
    }
    this.createFromLoading = true;
    this.createFromError = '';
    try {
      const response = await apiFetch(`/api/v1/projects/${this.createFromTarget.id}/clone`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: this.createFromName.trim() }),
      });
      if (!response.ok) {
        const errorText = await extractApiError(response, 'Failed to create project from template');
        throw new Error(errorText);
      }
      const created = (await response.json()) as { id: string };
      this.closeCreateFromDialog();
      // Navigate to the newly created project
      window.history.pushState({}, '', `/projects/${created.id}`);
      window.dispatchEvent(new PopStateEvent('popstate'));
    } catch (err) {
      this.createFromError = err instanceof Error ? err.message : 'Failed to create project';
    } finally {
      this.createFromLoading = false;
    }
  }

  // ── Rename ─────────────────────────────────────────────────────────

  private openRenameDialog(template: ProjectTemplate): void {
    this.renameTarget = template;
    this.renameName = template.name;
    this.renameError = '';
    this.renameLoading = false;
  }

  private closeRenameDialog(): void {
    this.renameTarget = null;
    this.renameError = '';
  }

  private async confirmRename(): Promise<void> {
    if (!this.renameTarget) return;
    if (!this.renameName.trim()) {
      this.renameError = 'Name is required.';
      return;
    }
    this.renameLoading = true;
    this.renameError = '';
    try {
      const response = await apiFetch(`/api/v1/projects/${this.renameTarget.id}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: this.renameName.trim() }),
      });
      if (!response.ok) {
        const errorText = await extractApiError(response, 'Failed to rename template');
        throw new Error(errorText);
      }
      this.closeRenameDialog();
      await this.load();
    } catch (err) {
      this.renameError = err instanceof Error ? err.message : 'Failed to rename template';
    } finally {
      this.renameLoading = false;
    }
  }

  // ── Delete ─────────────────────────────────────────────────────────

  private openDeleteDialog(template: ProjectTemplate): void {
    this.deleteTarget = template;
    this.deleteError = '';
    this.deleteLoading = false;
  }

  private closeDeleteDialog(): void {
    this.deleteTarget = null;
    this.deleteError = '';
  }

  private async confirmDelete(): Promise<void> {
    if (!this.deleteTarget) return;
    this.deleteLoading = true;
    this.deleteError = '';
    try {
      const response = await apiFetch(`/api/v1/projects/${this.deleteTarget.id}`, {
        method: 'DELETE',
      });
      if (!response.ok) {
        const errorText = await extractApiError(response, 'Failed to delete template');
        throw new Error(errorText);
      }
      this.closeDeleteDialog();
      await this.load();
    } catch (err) {
      this.deleteError = err instanceof Error ? err.message : 'Failed to delete template';
    } finally {
      this.deleteLoading = false;
    }
  }

  // ── Render ─────────────────────────────────────────────────────────

  override render() {
    if (this.loading) {
      return html`<div class="empty"><sl-spinner></sl-spinner></div>`;
    }
    if (this.error) {
      return html`<div class="error-banner">${this.error}</div>`;
    }

    return html`
      <div class="create-btn">
        <sl-button size="small" variant="primary" @click=${() => this.openCreateDialog()}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Create Template
        </sl-button>
      </div>

      ${this.templates.length === 0
        ? html`
            <div class="empty">
              <sl-icon name="file-earmark-plus"></sl-icon>
              <p>No project templates yet. Create one from an existing project.</p>
            </div>
          `
        : html`
            <div class="template-list">
              ${this.templates.map(t => this.renderTemplate(t))}
            </div>
          `}

      ${this.renderCreateDialog()}
      ${this.renderCreateFromDialog()}
      ${this.renderRenameDialog()}
      ${this.renderDeleteDialog()}
    `;
  }

  private renderTemplate(template: ProjectTemplate) {
    const created = template.created
      ? new Date(template.created).toLocaleDateString()
      : '';
    return html`
      <div class="template-row">
        <sl-icon class="template-icon" name="file-earmark-code"></sl-icon>
        <div class="template-info">
          <div class="template-name">${template.name}</div>
          <div class="template-meta">${template.slug}${created ? ` · Created ${created}` : ''}</div>
        </div>
        <div class="row-actions">
          <sl-dropdown placement="bottom-end" hoist>
            <sl-button slot="trigger" size="small" variant="text" caret>
              <sl-icon name="three-dots-vertical"></sl-icon>
            </sl-button>
            <sl-menu>
              <sl-menu-item @click=${() => this.openCreateFromDialog(template)}>
                <sl-icon slot="prefix" name="folder-plus"></sl-icon>
                Create From
              </sl-menu-item>
              <sl-menu-item @click=${() => this.openRenameDialog(template)}>
                <sl-icon slot="prefix" name="pencil"></sl-icon>
                Rename
              </sl-menu-item>
              <sl-divider></sl-divider>
              <sl-menu-item class="menu-item-danger" @click=${() => this.openDeleteDialog(template)}>
                <sl-icon slot="prefix" name="trash"></sl-icon>
                Delete
              </sl-menu-item>
            </sl-menu>
          </sl-dropdown>
        </div>
      </div>
    `;
  }

  // ── Dialogs ────────────────────────────────────────────────────────

  private renderCreateDialog() {
    if (!this.createDialogOpen) return nothing;
    return html`
      <sl-dialog
        label="Create Template"
        open
        @sl-request-close=${(e: Event) => {
          if (this.createLoading) e.preventDefault();
          else this.closeCreateDialog();
        }}
      >
        <p>Create a new project template from an existing project.</p>
        ${this.loadingProjects
          ? html`<div class="empty"><sl-spinner></sl-spinner> Loading projects...</div>`
          : html`
              <sl-select
                label="Source Project"
                placeholder="Select a project..."
                .value=${this.selectedSourceId}
                @sl-change=${(e: Event) => this.onSourceProjectChange(e)}
              >
                ${this.sourceProjects.length === 0
                  ? html`<sl-option disabled value="">No projects available</sl-option>`
                  : this.sourceProjects.map(p => html`<sl-option value=${p.id}>${p.name}</sl-option>`)}
              </sl-select>
            `}
        <sl-input
          label="Template Name"
          placeholder="My Template"
          .value=${this.newTemplateName}
          @sl-input=${(e: Event) => (this.newTemplateName = (e.target as HTMLInputElement).value)}
          ?disabled=${this.createLoading}
          style="margin-top: 1rem;"
        ></sl-input>
        ${this.createError ? html`<div class="dialog-error">${this.createError}</div>` : nothing}
        <div slot="footer">
          <sl-button
            variant="default"
            size="small"
            ?disabled=${this.createLoading}
            @click=${() => this.closeCreateDialog()}
          >
            Cancel
          </sl-button>
          <sl-button
            variant="primary"
            size="small"
            ?loading=${this.createLoading}
            ?disabled=${this.createLoading || !this.selectedSourceId || !this.newTemplateName.trim()}
            @click=${() => this.confirmCreateTemplate()}
          >
            Create Template
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  private renderCreateFromDialog() {
    if (!this.createFromTarget) return nothing;
    return html`
      <sl-dialog
        label="Create Project from Template"
        open
        @sl-request-close=${(e: Event) => {
          if (this.createFromLoading) e.preventDefault();
          else this.closeCreateFromDialog();
        }}
      >
        <p>
          Create a new project from template
          <strong>${this.createFromTarget.name}</strong>.
        </p>
        <sl-input
          label="Project Name"
          placeholder="My New Project"
          .value=${this.createFromName}
          @sl-input=${(e: Event) => (this.createFromName = (e.target as HTMLInputElement).value)}
          ?disabled=${this.createFromLoading}
        ></sl-input>
        ${this.createFromError ? html`<div class="dialog-error">${this.createFromError}</div>` : nothing}
        <div slot="footer">
          <sl-button
            variant="default"
            size="small"
            ?disabled=${this.createFromLoading}
            @click=${() => this.closeCreateFromDialog()}
          >
            Cancel
          </sl-button>
          <sl-button
            variant="primary"
            size="small"
            ?loading=${this.createFromLoading}
            ?disabled=${this.createFromLoading || !this.createFromName.trim()}
            @click=${() => this.confirmCreateFrom()}
          >
            Create Project
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  private renderRenameDialog() {
    if (!this.renameTarget) return nothing;
    return html`
      <sl-dialog
        label="Rename Template"
        open
        @sl-request-close=${(e: Event) => {
          if (this.renameLoading) e.preventDefault();
          else this.closeRenameDialog();
        }}
      >
        <sl-input
          label="Name"
          .value=${this.renameName}
          @sl-input=${(e: Event) => (this.renameName = (e.target as HTMLInputElement).value)}
          ?disabled=${this.renameLoading}
        ></sl-input>
        ${this.renameError ? html`<div class="dialog-error">${this.renameError}</div>` : nothing}
        <div slot="footer">
          <sl-button
            variant="default"
            size="small"
            ?disabled=${this.renameLoading}
            @click=${() => this.closeRenameDialog()}
          >
            Cancel
          </sl-button>
          <sl-button
            variant="primary"
            size="small"
            ?loading=${this.renameLoading}
            ?disabled=${this.renameLoading || !this.renameName.trim()}
            @click=${() => this.confirmRename()}
          >
            Rename
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  private renderDeleteDialog() {
    if (!this.deleteTarget) return nothing;
    return html`
      <sl-dialog
        label="Delete Template"
        open
        @sl-request-close=${(e: Event) => {
          if (this.deleteLoading) e.preventDefault();
          else this.closeDeleteDialog();
        }}
      >
        <p>
          Are you sure you want to delete
          <strong>${this.deleteTarget.name}</strong>?
        </p>
        <div class="dialog-warning">
          <sl-icon name="exclamation-triangle"></sl-icon>
          This action cannot be undone.
        </div>
        ${this.deleteError ? html`<div class="dialog-error">${this.deleteError}</div>` : nothing}
        <div slot="footer">
          <sl-button
            variant="default"
            size="small"
            ?disabled=${this.deleteLoading}
            @click=${() => this.closeDeleteDialog()}
          >
            Cancel
          </sl-button>
          <sl-button
            variant="danger"
            size="small"
            ?loading=${this.deleteLoading}
            ?disabled=${this.deleteLoading}
            @click=${() => this.confirmDelete()}
          >
            Delete
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-project-template-list': ScionProjectTemplateList;
  }
}
