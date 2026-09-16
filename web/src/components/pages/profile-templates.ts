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
 * Manages user-scoped templates. Composes <scion-resource-list scope="user">
 * for list/clone/delete/clone-from-global and keeps a local Create dialog
 * for template creation.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { showToast } from '../../utils/toast.js';
import '../shared/resource-list.js';
import type { ScionResourceList } from '../shared/resource-list.js';

@customElement('scion-page-profile-templates')
export class ScionPageProfileTemplates extends LitElement {
  // Create dialog state
  @state() private createDialogOpen = false;
  @state() private createLoading = false;
  @state() private createError = '';
  @state() private newTemplateName = '';
  @state() private newTemplateHarness = '';

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

    .dialog-error {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.8125rem;
      margin-top: 0.5rem;
    }
  `;

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

      <scion-resource-list
        id="templates-list"
        kind="template"
        scope="user"
        detailBasePath="/profile"
        canClone
        canDelete
        cloneFromGlobal
        @resource-changed=${() => this._refreshList()}
      ></scion-resource-list>

      ${this.renderCreateDialog()}
    `;
  }

  // ── Refresh ───────────────────────────────────────────────────────

  private _refreshList() {
    const list = this.shadowRoot?.querySelector<ScionResourceList>('#templates-list');
    if (list) void list.load();
  }

  // ── Create Template ───────────────────────────────────────────────

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
      const body: Record<string, string> = {
        name: this.newTemplateName.trim(),
        scope: 'user',
      };
      if (this.newTemplateHarness.trim()) {
        body.harness = this.newTemplateHarness.trim();
      }
      const resp = await apiFetch('/api/v1/templates', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!resp.ok) {
        throw new Error(await extractApiError(resp, 'Failed to create template'));
      }
      const created = (await resp.json()) as { id: string };
      this.closeCreateDialog();
      showToast('Template created', 'success');
      this._refreshList();
      // Navigate to the new template's detail page
      window.history.pushState({}, '', `/profile/templates/${created.id}`);
      window.dispatchEvent(new PopStateEvent('popstate'));
    } catch (err) {
      this.createError = err instanceof Error ? err.message : 'Failed to create template';
    } finally {
      this.createLoading = false;
    }
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
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-profile-templates': ScionPageProfileTemplates;
  }
}
