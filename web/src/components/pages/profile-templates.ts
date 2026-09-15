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
 * Profile Templates page
 *
 * Manages user-scoped templates via /api/v1/users/me/templates.
 * Follows the same structure as profile-skills.ts and profile-secrets.ts.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { showToast } from '../../utils/toast.js';

interface UserTemplate {
  id: string;
  name: string;
  slug: string;
  displayName?: string;
  description?: string;
  harness?: string;
  status: string;
  contentHash?: string;
  created: string;
  updated: string;
}

interface ListResponse {
  templates: UserTemplate[];
  totalCount: number;
}

@customElement('scion-page-profile-templates')
export class ScionPageProfileTemplates extends LitElement {
  @state() private templates: UserTemplate[] = [];
  @state() private loading = true;
  @state() private error: string | null = null;

  // Create dialog state
  @state() private createDialogOpen = false;
  @state() private createLoading = false;
  @state() private createError = '';
  @state() private newTemplateName = '';
  @state() private newTemplateHarness = '';

  // Clone dialog state
  @state() private cloneTarget: UserTemplate | null = null;
  @state() private cloneName = '';
  @state() private cloneLoading = false;
  @state() private cloneError = '';

  // Rename dialog state
  @state() private renameTarget: UserTemplate | null = null;
  @state() private renameName = '';
  @state() private renameLoading = false;
  @state() private renameError = '';

  // Delete dialog state
  @state() private deleteTarget: UserTemplate | null = null;
  @state() private deleteLoading = false;

  static override styles = css`
    :host {
      display: block;
    }

    .page-header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 1.5rem;
      gap: 1rem;
    }

    .page-header-info h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .page-header-info p {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0;
    }

    .template-list {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
    }

    .template-card {
      display: flex;
      align-items: center;
      gap: 1rem;
      padding: 1rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.5rem;
      transition: border-color 0.15s ease;
    }

    .template-card:hover {
      border-color: var(--scion-primary, #3b82f6);
    }

    .template-info {
      flex: 1;
      min-width: 0;
    }

    .template-name {
      font-size: 0.9375rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .template-meta {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .template-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    .template-badge.active {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .template-badge.pending {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #a16207);
    }

    .template-actions {
      display: flex;
      gap: 0.5rem;
      flex-shrink: 0;
    }

    .empty-state {
      text-align: center;
      padding: 3rem 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .empty-state sl-icon {
      font-size: 2.5rem;
      margin-bottom: 1rem;
      opacity: 0.5;
    }

    .empty-state h3 {
      margin: 0 0 0.5rem 0;
      color: var(--scion-text, #1e293b);
      font-size: 1.125rem;
    }

    .empty-state p {
      margin: 0;
      font-size: 0.875rem;
    }

    .error-box {
      padding: 1rem;
      border-radius: 0.5rem;
      background: var(--sl-color-danger-50, #fef2f2);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      color: var(--sl-color-danger-700, #b91c1c);
      font-size: 0.875rem;
    }

    .loading-spinner {
      display: flex;
      justify-content: center;
      padding: 3rem;
    }

    .template-name a {
      color: inherit;
      text-decoration: none;
    }

    .template-name a:hover {
      color: var(--scion-primary, #3b82f6);
      text-decoration: underline;
    }

    .menu-item-danger::part(base) {
      color: var(--sl-color-danger-600, #dc2626);
    }

    .dialog-error {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.8125rem;
      margin-top: 0.5rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    this.loadTemplates();
  }

  private async loadTemplates(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const resp = await apiFetch('/api/v1/users/me/templates');
      if (!resp.ok) {
        throw new Error(await extractApiError(resp, 'Request failed'));
      }
      const data = (await resp.json()) as ListResponse;
      this.templates = data.templates || [];
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to load templates';
    } finally {
      this.loading = false;
    }
  }

  private async handleDelete(): Promise<void> {
    if (!this.deleteTarget) return;

    this.deleteLoading = true;
    try {
      const resp = await apiFetch(
        `/api/v1/users/me/templates/${this.deleteTarget.id}?deleteFiles=true`,
        { method: 'DELETE' }
      );
      if (!resp.ok) {
        throw new Error(await extractApiError(resp, 'Request failed'));
      }
      showToast(`Template "${this.deleteTarget.name}" deleted`, 'success');
      this.deleteTarget = null;
      await this.loadTemplates();
    } catch (err) {
      showToast(err instanceof Error ? err.message : 'Failed to delete template', 'danger');
    } finally {
      this.deleteLoading = false;
    }
  }

  // ── Create Template ────────────────────────────────────────────────

  private openCreateDialog(): void {
    this.createDialogOpen = true;
    this.createError = '';
    this.createLoading = false;
    this.newTemplateName = '';
    this.newTemplateHarness = '';
  }

  private closeCreateDialog(): void {
    this.createDialogOpen = false;
    this.createError = '';
  }

  private async confirmCreateTemplate(): Promise<void> {
    if (!this.newTemplateName.trim()) {
      this.createError = 'Template name is required.';
      return;
    }
    this.createLoading = true;
    this.createError = '';
    try {
      const body: Record<string, string> = { name: this.newTemplateName.trim() };
      if (this.newTemplateHarness.trim()) {
        body.harness = this.newTemplateHarness.trim();
      }
      const resp = await apiFetch('/api/v1/users/me/templates', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!resp.ok) {
        throw new Error(await extractApiError(resp, 'Failed to create template'));
      }
      const created = (await resp.json()) as { id: string };
      this.closeCreateDialog();
      // Navigate to the new template's detail page
      window.history.pushState({}, '', `/profile/templates/${created.id}`);
      window.dispatchEvent(new PopStateEvent('popstate'));
    } catch (err) {
      this.createError = err instanceof Error ? err.message : 'Failed to create template';
    } finally {
      this.createLoading = false;
    }
  }

  // ── Clone Template ────────────────────────────────────────────────

  private openCloneDialog(template: UserTemplate): void {
    this.cloneTarget = template;
    this.cloneName = `${template.name} (copy)`;
    this.cloneError = '';
    this.cloneLoading = false;
  }

  private closeCloneDialog(): void {
    this.cloneTarget = null;
    this.cloneError = '';
  }

  private async confirmClone(): Promise<void> {
    if (!this.cloneTarget) return;
    if (!this.cloneName.trim()) {
      this.cloneError = 'Template name is required.';
      return;
    }
    this.cloneLoading = true;
    this.cloneError = '';
    try {
      const body: Record<string, string> = { name: this.cloneName.trim() };
      if (this.cloneTarget.harness) {
        body.harness = this.cloneTarget.harness;
      }
      const resp = await apiFetch('/api/v1/users/me/templates', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!resp.ok) {
        throw new Error(await extractApiError(resp, 'Failed to clone template'));
      }
      this.closeCloneDialog();
      showToast(`Template cloned as "${this.cloneName.trim()}"`, 'success');
      await this.loadTemplates();
    } catch (err) {
      this.cloneError = err instanceof Error ? err.message : 'Failed to clone template';
    } finally {
      this.cloneLoading = false;
    }
  }

  // ── Rename Template ───────────────────────────────────────────────

  private openRenameDialog(template: UserTemplate): void {
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
      const resp = await apiFetch(`/api/v1/users/me/templates/${this.renameTarget.id}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: this.renameName.trim(),
          displayName: this.renameTarget.displayName || '',
          description: this.renameTarget.description || '',
          harness: this.renameTarget.harness || '',
        }),
      });
      if (!resp.ok) {
        throw new Error(await extractApiError(resp, 'Failed to rename template'));
      }
      showToast(`Template renamed to "${this.renameName.trim()}"`, 'success');
      this.closeRenameDialog();
      await this.loadTemplates();
    } catch (err) {
      this.renameError = err instanceof Error ? err.message : 'Failed to rename template';
    } finally {
      this.renameLoading = false;
    }
  }

  private formatDate(dateStr: string): string {
    try {
      return new Date(dateStr).toLocaleDateString(undefined, {
        year: 'numeric',
        month: 'short',
        day: 'numeric',
      });
    } catch {
      return dateStr;
    }
  }

  override render() {
    return html`
      <div class="page-header">
        <div class="page-header-info">
          <h1>Templates</h1>
          <p>
            Manage your personal agent templates. Use
            <code>scion template sync --template-scope user</code> to upload templates from the CLI.
          </p>
        </div>
        <sl-button variant="primary" size="small" @click=${() => this.openCreateDialog()}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Create Template
        </sl-button>
      </div>

      ${
        this.loading
          ? html`<div class="loading-spinner">
              <sl-spinner style="font-size: 2rem;"></sl-spinner>
            </div>`
          : this.error
            ? html`<div class="error-box">${this.error}</div>`
            : this.templates.length === 0
              ? this.renderEmptyState()
              : this.renderTemplateList()
      }
      ${this.renderCreateDialog()} ${this.renderCloneDialog()} ${this.renderRenameDialog()}
      ${this.renderDeleteDialog()}
    `;
  }

  private renderEmptyState() {
    return html`
      <div class="empty-state">
        <sl-icon name="file-earmark-code"></sl-icon>
        <h3>No user templates</h3>
        <p>
          Upload personal templates using the CLI:<br />
          <code>scion template sync my-template --template-scope user</code>
        </p>
      </div>
    `;
  }

  private renderTemplateList() {
    return html`
      <div class="template-list">
        ${this.templates.map(
          (t) => html`
            <div class="template-card">
              <div class="template-info">
                <div class="template-name">
                  <a href="/profile/templates/${t.id}">${t.displayName || t.name}</a>
                </div>
                <div class="template-meta">
                  ${t.harness ? html`<span class="template-badge">${t.harness}</span>` : nothing}
                  <span class="template-badge ${t.status}">${t.status}</span>
                  ${t.description ? html`<span>${t.description}</span>` : nothing}
                  <span>Updated ${this.formatDate(t.updated)}</span>
                </div>
              </div>
              <div class="template-actions">
                <sl-dropdown placement="bottom-end" hoist>
                  <sl-button
                    slot="trigger"
                    size="small"
                    variant="text"
                    caret
                    label="Template actions"
                  >
                    <sl-icon name="three-dots-vertical"></sl-icon>
                  </sl-button>
                  <sl-menu>
                    <sl-menu-item @click=${() => this.navigateToDetail(t)}>
                      <sl-icon slot="prefix" name="eye"></sl-icon>
                      View / Edit
                    </sl-menu-item>
                    <sl-menu-item @click=${() => this.openCloneDialog(t)}>
                      <sl-icon slot="prefix" name="copy"></sl-icon>
                      Clone
                    </sl-menu-item>
                    <sl-menu-item @click=${() => this.openRenameDialog(t)}>
                      <sl-icon slot="prefix" name="pencil"></sl-icon>
                      Rename
                    </sl-menu-item>
                    <sl-divider></sl-divider>
                    <sl-menu-item
                      class="menu-item-danger"
                      @click=${() => {
                        this.deleteTarget = t;
                      }}
                    >
                      <sl-icon slot="prefix" name="trash"></sl-icon>
                      Delete
                    </sl-menu-item>
                  </sl-menu>
                </sl-dropdown>
              </div>
            </div>
          `
        )}
      </div>
    `;
  }

  private navigateToDetail(template: UserTemplate): void {
    window.history.pushState({}, '', `/profile/templates/${template.id}`);
    window.dispatchEvent(new PopStateEvent('popstate'));
  }

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
        <p>Create a new personal template.</p>
        <sl-input
          label="Template Name"
          placeholder="My Template"
          .value=${this.newTemplateName}
          @sl-input=${(e: Event) => (this.newTemplateName = (e.target as HTMLInputElement).value)}
          ?disabled=${this.createLoading}
        ></sl-input>
        <sl-input
          label="Harness (optional)"
          placeholder="e.g. claude-code"
          .value=${this.newTemplateHarness}
          @sl-input=${(e: Event) => (this.newTemplateHarness = (e.target as HTMLInputElement).value)}
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
            ?disabled=${this.createLoading || !this.newTemplateName.trim()}
            @click=${() => this.confirmCreateTemplate()}
          >
            Create Template
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  private renderCloneDialog() {
    if (!this.cloneTarget) return nothing;
    return html`
      <sl-dialog
        label="Clone Template"
        open
        @sl-request-close=${(e: Event) => {
          if (this.cloneLoading) e.preventDefault();
          else this.closeCloneDialog();
        }}
      >
        <p>
          Create a copy of template
          <strong>${this.cloneTarget.name}</strong>.
        </p>
        <sl-input
          label="New Template Name"
          .value=${this.cloneName}
          @sl-input=${(e: Event) => (this.cloneName = (e.target as HTMLInputElement).value)}
          ?disabled=${this.cloneLoading}
        ></sl-input>
        ${this.cloneError ? html`<div class="dialog-error">${this.cloneError}</div>` : nothing}
        <div slot="footer">
          <sl-button
            variant="default"
            size="small"
            ?disabled=${this.cloneLoading}
            @click=${() => this.closeCloneDialog()}
          >
            Cancel
          </sl-button>
          <sl-button
            variant="primary"
            size="small"
            ?loading=${this.cloneLoading}
            ?disabled=${this.cloneLoading || !this.cloneName.trim()}
            @click=${() => this.confirmClone()}
          >
            Clone
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
        @sl-after-hide=${() => {
          this.deleteTarget = null;
        }}
      >
        <p>
          Are you sure you want to delete the template
          <strong>${this.deleteTarget.name}</strong>? This will also remove the template files from
          storage. This action cannot be undone.
        </p>
        <div slot="footer">
          <sl-button
            variant="default"
            @click=${() => {
              this.deleteTarget = null;
            }}
            >Cancel</sl-button
          >
          <sl-button
            variant="danger"
            ?loading=${this.deleteLoading}
            @click=${() => this.handleDelete()}
            >Delete</sl-button
          >
        </div>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-profile-templates': ScionPageProfileTemplates;
  }
}
