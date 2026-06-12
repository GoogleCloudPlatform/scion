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
 * Harness-config detail page component
 *
 * Displays a harness-config's metadata and a file browser with inline editing.
 * Mirrors the template detail page. Works at both project scope
 * (/projects/{id}/harness-configs/{id}) and hub/global scope
 * (/settings/harness-configs/{id}).
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, HarnessConfig } from '../../shared/types.js';
import { can } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import '../shared/file-browser.js';
import '../shared/file-editor.js';
import { HarnessConfigFileBrowserDataSource } from '../shared/file-browser.js';
import type { FileBrowserDataSource } from '../shared/file-browser.js';
import { HarnessConfigFileEditorDataSource } from '../shared/file-editor.js';
import type { FileEditorDataSource } from '../shared/file-editor.js';
import '../shared/hash-display.js';

@customElement('scion-page-harness-config-detail')
export class ScionPageHarnessConfigDetail extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @property({ type: String })
  projectId = '';

  @property({ type: String })
  harnessConfigId = '';

  @state()
  private loading = true;

  @state()
  private harnessConfig: HarnessConfig | null = null;

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

    .back-links {
      display: flex;
      align-items: center;
      gap: 1rem;
      margin-bottom: 1rem;
      flex-wrap: wrap;
    }
    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.35rem;
      color: var(--sl-color-neutral-600);
      text-decoration: none;
      font-size: 0.875rem;
    }
    .back-link:hover {
      color: var(--sl-color-primary-600);
    }

    .resource-header {
      margin-bottom: 1.5rem;
    }
    .resource-title {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin: 0 0 0.5rem;
    }
    .resource-title h1 {
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
    .resource-description {
      color: var(--sl-color-neutral-600);
      font-size: 0.875rem;
      margin: 0;
    }
    .resource-meta-row {
      display: flex;
      gap: 1rem;
      margin-top: 0.5rem;
      font-size: 0.75rem;
      color: var(--sl-color-neutral-500);
    }
    .resource-meta-row .hash-meta {
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
      const projectMatch = window.location.pathname.match(
        /\/projects\/([^/]+)\/harness-configs\/([^/]+)/
      );
      if (projectMatch) {
        this.projectId = projectMatch[1];
        this.harnessConfigId = projectMatch[2];
      } else {
        // Hub (global) scope: /settings/harness-configs/{id}
        const hubMatch = window.location.pathname.match(/\/settings\/harness-configs\/([^/]+)/);
        if (hubMatch) {
          this.projectId = '';
          this.harnessConfigId = hubMatch[1];
        }
      }
    }
    void this.loadHarnessConfig();
  }

  /** Back-navigation links — project scope returns to project settings, hub scope to Hub Resources. */
  private backLinks(): Array<{ href: string; label: string }> {
    if (this.projectId) {
      return [
        {
          href: `/projects/${this.projectId}/settings?tab=harness-configs`,
          label: 'Harness Configs',
        },
        { href: `/projects/${this.projectId}/settings`, label: 'Project Settings' },
      ];
    }
    return [{ href: '/settings?tab=harness-configs', label: 'Hub Resources' }];
  }

  private async loadHarnessConfig(): Promise<void> {
    if (!this.harnessConfigId) return;
    this.loading = true;
    this.error = null;

    try {
      const response = await apiFetch(`/api/v1/harness-configs/${this.harnessConfigId}`);
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      this.harnessConfig = (await response.json()) as HarnessConfig;
      dispatchPageTitle(
        this,
        this.harnessConfig.displayName || this.harnessConfig.name || this.harnessConfigId,
        'Harness Configs'
      );

      // Create data sources
      this.fileBrowserDataSource = new HarnessConfigFileBrowserDataSource(this.harnessConfigId);
      this.fileEditorDataSource = new HarnessConfigFileEditorDataSource(this.harnessConfigId);
    } catch (err) {
      console.error('Failed to load harness config:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load harness config';
    } finally {
      this.loading = false;
    }
  }

  // ── File editing event handlers (mirror template-detail pattern) ──

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
    const browser = this.shadowRoot?.querySelector('scion-file-browser') as
      | import('../shared/file-browser.js').ScionFileBrowser
      | null;
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
          <sl-button size="small" @click=${() => this.loadHarnessConfig()}>Retry</sl-button>
        </div>
      `;
    }
    if (!this.harnessConfig) return nothing;

    return html`
      <div class="back-links">
        ${this.backLinks().map(
          (link) => html`
            <a href=${link.href} class="back-link">
              <sl-icon name="arrow-left"></sl-icon>
              ${link.label}
            </a>
          `
        )}
      </div>

      ${this.renderHeader()} ${this.renderFilesSection()}
    `;
  }

  private renderHeader() {
    const hc = this.harnessConfig!;
    return html`
      <div class="resource-header">
        <div class="resource-title">
          <sl-icon
            name="sliders"
            style="font-size: 1.25rem; color: var(--sl-color-neutral-500);"
          ></sl-icon>
          <h1>${hc.displayName || hc.name}</h1>
          ${hc.harness ? html`<span class="harness-badge">${hc.harness}</span>` : ''}
        </div>
        ${hc.description ? html`<p class="resource-description">${hc.description}</p>` : ''}
        <div class="resource-meta-row">
          <span>Scope: ${hc.scope}</span>
          <span>Status: ${hc.status}</span>
          ${hc.contentHash
            ? html`<span class="hash-meta"
                >Hash:
                <scion-hash-display .hash=${hc.contentHash} max-width="14ch"></scion-hash-display
              ></span>`
            : ''}
        </div>
      </div>
    `;
  }

  private renderFilesSection() {
    const isEditable = can(this.harnessConfig?._capabilities, 'update');
    const isEditorOpen = this.editingFilePath !== null;

    return html`
      <div class="files-section">
        <h2>Harness Config Files</h2>

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

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-harness-config-detail': ScionPageHarnessConfigDetail;
  }
}
