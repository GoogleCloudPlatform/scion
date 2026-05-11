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
 * Template detail page component
 *
 * Displays a template's metadata and file browser with inline editing.
 * Route: /projects/{projectId}/templates/{templateId}
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Template } from '../../shared/types.js';
import { can } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import '../shared/file-browser.js';
import '../shared/file-editor.js';
import { TemplateFileBrowserDataSource } from '../shared/file-browser.js';
import type { FileBrowserDataSource } from '../shared/file-browser.js';
import { TemplateFileEditorDataSource } from '../shared/file-editor.js';
import type { FileEditorDataSource } from '../shared/file-editor.js';
import '../shared/hash-display.js';

@customElement('scion-page-template-detail')
export class ScionPageTemplateDetail extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @property({ type: String })
  projectId = '';

  @property({ type: String })
  templateId = '';

  @state()
  private loading = true;

  @state()
  private template: Template | null = null;

  @state()
  private error: string | null = null;

  /**
   * Path of the file currently open in the editor (null = editor closed, '' = new file)
   */
  @state()
  private editingFilePath: string | null = null;

  /**
   * Whether to open the editor initially in preview mode (for .md eye icon)
   */
  @state()
  private editorInitialPreview = false;

  private fileBrowserDataSource: FileBrowserDataSource | null = null;
  private fileEditorDataSource: FileEditorDataSource | null = null;

  static override styles = css`
    :host {
      display: block;
      padding: 1.5rem;
      max-width: 1200px;
      margin: 0 auto;
    }

    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.35rem;
      color: var(--sl-color-neutral-600);
      text-decoration: none;
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }
    .back-link:hover {
      color: var(--sl-color-primary-600);
    }

    .template-header {
      margin-bottom: 1.5rem;
    }
    .template-title {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin: 0 0 0.5rem;
    }
    .template-title h1 {
      margin: 0;
      font-size: 1.5rem;
      font-weight: 600;
    }
    .harness-badge {
      display: inline-block;
      padding: 0.15rem 0.5rem;
      border-radius: var(--sl-border-radius-pill);
      background: var(--sl-color-neutral-100);
      color: var(--sl-color-neutral-700);
      font-size: 0.75rem;
      font-weight: 500;
    }
    .template-description {
      color: var(--sl-color-neutral-600);
      font-size: 0.875rem;
      margin: 0;
    }
    .template-meta-row {
      display: flex;
      gap: 1rem;
      margin-top: 0.5rem;
      font-size: 0.75rem;
      color: var(--sl-color-neutral-500);
    }
    .template-meta-row .hash-meta {
      display: inline-flex;
      align-items: baseline;
      gap: 0.25rem;
      min-width: 0;
    }

    .files-section {
      margin-top: 1.5rem;
    }
    .files-section h2 {
      font-size: 1.1rem;
      font-weight: 600;
      margin: 0 0 1rem;
    }

    .editor-back-row {
      margin-bottom: 0.5rem;
    }

    .error-state,
    .loading-state {
      text-align: center;
      padding: 3rem;
      color: var(--sl-color-neutral-500);
    }
    .error-state sl-icon {
      font-size: 2rem;
      color: var(--sl-color-danger-500);
      margin-bottom: 0.5rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    if (typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/projects\/([^/]+)\/templates\/([^/]+)/);
      if (match) {
        this.projectId = match[1];
        this.templateId = match[2];
      }
    }
    void this.loadTemplate();
  }

  private async loadTemplate(): Promise<void> {
    if (!this.templateId) return;
    this.loading = true;
    this.error = null;

    try {
      const response = await apiFetch(`/api/v1/templates/${this.templateId}`);
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      this.template = (await response.json()) as Template;
      dispatchPageTitle(this, this.template.displayName || this.template.name || this.templateId, 'Templates');

      // Create data sources
      this.fileBrowserDataSource = new TemplateFileBrowserDataSource(this.templateId);
      this.fileEditorDataSource = new TemplateFileEditorDataSource(this.templateId);
    } catch (err) {
      console.error('Failed to load template:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load template';
    } finally {
      this.loading = false;
    }
  }

  // ── File editing event handlers (mirror project-detail pattern) ──

  private handleFileEditRequested(e: CustomEvent<{ path: string }>): void {
    this.editingFilePath = e.detail.path;
    this.editorInitialPreview = false;
  }

  private handleFilePreviewRequested(e: CustomEvent<{ path: string }>): void {
    this.editingFilePath = e.detail.path;
    this.editorInitialPreview = true;
  }

  private handleFileCreateRequested(): void {
    this.editingFilePath = '';
    this.editorInitialPreview = false;
  }

  private handleEditorClosed(): void {
    this.editingFilePath = null;
    this.editorInitialPreview = false;
  }

  private handleFileSaved(): void {
    this.refreshFileBrowser();
  }

  private refreshFileBrowser(): void {
    const browser = this.shadowRoot?.querySelector(
      'scion-file-browser'
    ) as import('../shared/file-browser.js').ScionFileBrowser | null;
    browser?.loadFiles();
  }

  // ── Rendering ──

  override render() {
    if (this.loading) {
      return html`<div class="loading-state"><sl-spinner></sl-spinner></div>`;
    }
    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <p>${this.error}</p>
          <sl-button size="small" @click=${() => this.loadTemplate()}>Retry</sl-button>
        </div>
      `;
    }
    if (!this.template) return nothing;

    return html`
      <a href="/projects/${this.projectId}/settings" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Project Settings
      </a>

      ${this.renderHeader()}
      ${this.renderFilesSection()}
    `;
  }

  private renderHeader() {
    const t = this.template!;
    return html`
      <div class="template-header">
        <div class="template-title">
          <sl-icon name="file-earmark-code" style="font-size: 1.25rem; color: var(--sl-color-neutral-500);"></sl-icon>
          <h1>${t.displayName || t.name}</h1>
          ${t.harness ? html`<span class="harness-badge">${t.harness}</span>` : ''}
        </div>
        ${t.description ? html`<p class="template-description">${t.description}</p>` : ''}
        <div class="template-meta-row">
          <span>Scope: ${t.scope}</span>
          <span>Status: ${t.status}</span>
          ${t.contentHash
            ? html`<span class="hash-meta">Hash: <scion-hash-display .hash=${t.contentHash} max-width="14ch"></scion-hash-display></span>`
            : ''}
        </div>
      </div>
    `;
  }

  private renderFilesSection() {
    const isEditable = can(this.template?._capabilities, 'update');
    const isEditorOpen = this.editingFilePath !== null;

    return html`
      <div class="files-section">
        <h2>Template Files</h2>

        ${isEditorOpen
          ? html`
              <div class="editor-back-row">
                <sl-button size="small" variant="text" @click=${this.handleEditorClosed}>
                  <sl-icon slot="prefix" name="arrow-left"></sl-icon>
                  Back to files
                </sl-button>
              </div>
              <scion-file-editor
                .filePath=${this.editingFilePath || ''}
                .dataSource=${this.fileEditorDataSource}
                ?readonly=${!isEditable}
                ?initialPreview=${this.editorInitialPreview}
                @file-saved=${this.handleFileSaved}
                @editor-closed=${this.handleEditorClosed}
              ></scion-file-editor>
            `
          : html`
              <scion-file-browser
                .dataSource=${this.fileBrowserDataSource}
                ?editable=${isEditable}
                @file-edit-requested=${this.handleFileEditRequested}
                @file-preview-requested=${this.handleFilePreviewRequested}
                @file-create-requested=${this.handleFileCreateRequested}
              ></scion-file-browser>
            `}
      </div>
    `;
  }
}
