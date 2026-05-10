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
 * File Editor Component
 *
 * Top-level component that manages a single-file editing session.
 * Provides a toolbar with save, revert, and close actions, and
 * wraps the scion-code-editor for the actual editing surface.
 *
 * Supports two modes:
 *   - Editing an existing file (filePath is set)
 *   - Creating a new file (filePath is empty, shows filename input)
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { getLanguageFromPath } from './code-editor.js';

// Ensure sub-components are registered
import './code-editor.js';
import './markdown-preview.js';

// ────────────────────────────────────────────────────────────
// Types
// ────────────────────────────────────────────────────────────

export interface FileContentResponse {
  path: string;
  content: string;
  size: number;
  modTime: string;
  encoding: string;
}

export interface FileEditorDataSource {
  /** Fetch file content as JSON (for editing). */
  getFileContent(path: string): Promise<FileContentResponse>;
  /** Save file content. Returns updated metadata. */
  saveFileContent(path: string, content: string, expectedModTime?: string): Promise<{ modTime: string }>;
}

// ────────────────────────────────────────────────────────────
// Data Source Implementations
// ────────────────────────────────────────────────────────────

function encodeFilePath(filePath: string): string {
  return filePath
    .split('/')
    .map((seg) => encodeURIComponent(seg))
    .join('/');
}

export class WorkspaceFileEditorDataSource implements FileEditorDataSource {
  private readonly basePath: string;

  constructor(projectId: string) {
    this.basePath = `/api/v1/projects/${projectId}/workspace/files`;
  }

  async getFileContent(path: string): Promise<FileContentResponse> {
    const res = await apiFetch(`${this.basePath}/${encodeFilePath(path)}?format=json`);
    if (!res.ok) throw new Error(await extractApiError(res, `HTTP ${res.status}`));
    return (await res.json()) as FileContentResponse;
  }

  async saveFileContent(path: string, content: string, expectedModTime?: string): Promise<{ modTime: string }> {
    const body: Record<string, string> = { content };
    if (expectedModTime) body.expectedModTime = expectedModTime;

    const res = await apiFetch(`${this.basePath}/${encodeFilePath(path)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!res.ok) throw new Error(await extractApiError(res, `HTTP ${res.status}`));
    const data = (await res.json()) as { modTime: string };
    return { modTime: data.modTime };
  }
}

export class SharedDirFileEditorDataSource implements FileEditorDataSource {
  private readonly basePath: string;

  constructor(projectId: string, dirName: string) {
    this.basePath = `/api/v1/projects/${projectId}/shared-dirs/${encodeURIComponent(dirName)}/files`;
  }

  async getFileContent(path: string): Promise<FileContentResponse> {
    const res = await apiFetch(`${this.basePath}/${encodeFilePath(path)}?format=json`);
    if (!res.ok) throw new Error(await extractApiError(res, `HTTP ${res.status}`));
    return (await res.json()) as FileContentResponse;
  }

  async saveFileContent(path: string, content: string, expectedModTime?: string): Promise<{ modTime: string }> {
    const body: Record<string, string> = { content };
    if (expectedModTime) body.expectedModTime = expectedModTime;

    const res = await apiFetch(`${this.basePath}/${encodeFilePath(path)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!res.ok) throw new Error(await extractApiError(res, `HTTP ${res.status}`));
    const data = (await res.json()) as { modTime: string };
    return { modTime: data.modTime };
  }
}

export class TemplateFileEditorDataSource implements FileEditorDataSource {
  private readonly basePath: string;

  constructor(templateId: string) {
    this.basePath = `/api/v1/templates/${templateId}/files`;
  }

  async getFileContent(path: string): Promise<FileContentResponse> {
    const res = await apiFetch(`${this.basePath}/${encodeFilePath(path)}`);
    if (!res.ok) throw new Error(await extractApiError(res, `HTTP ${res.status}`));
    return (await res.json()) as FileContentResponse;
  }

  async saveFileContent(path: string, content: string, _expectedModTime?: string): Promise<{ modTime: string }> {
    const body: Record<string, string> = { content };

    const res = await apiFetch(`${this.basePath}/${encodeFilePath(path)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!res.ok) throw new Error(await extractApiError(res, `HTTP ${res.status}`));
    const data = (await res.json()) as { modTime: string };
    return { modTime: data.modTime };
  }
}

// ────────────────────────────────────────────────────────────
// Component
// ────────────────────────────────────────────────────────────

@customElement('scion-file-editor')
export class ScionFileEditor extends LitElement {
  /** Path of the file being edited (empty for new file creation). */
  @property({ type: String })
  filePath = '';

  /** Data source for reading/writing file content. */
  @property({ attribute: false })
  dataSource: FileEditorDataSource | null = null;

  /** Whether the editor is read-only. */
  @property({ type: Boolean })
  readonly = false;

  /** Open directly in preview mode (used by eye icon on .md files). */
  @property({ type: Boolean })
  initialPreview = false;

  // ── Internal state ──

  @state() private originalContent = '';
  @state() private currentContent = '';
  @state() private serverModTime = '';
  @state() private loading = false;
  @state() private saving = false;
  @state() private error: string | null = null;
  @state() private saveSuccess = false;
  @state() private newFileName = '';
  @state() private newFileError = '';
  @state() private showPreview = false;

  /** Whether the editor has unsaved changes. */
  get dirty(): boolean {
    return this.currentContent !== this.originalContent;
  }

  /** Whether this is a new file creation flow. */
  get isNewFile(): boolean {
    return !this.filePath;
  }

  /** Whether the current file is markdown. */
  private get isMarkdown(): boolean {
    const path = this.isNewFile ? this.newFileName : this.filePath;
    return path.toLowerCase().endsWith('.md');
  }

  static override styles = css`
    :host {
      display: block;
    }

    .editor-toolbar {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.625rem 0;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      margin-bottom: 0.75rem;
      flex-wrap: wrap;
    }

    .toolbar-left {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      flex: 1;
      min-width: 0;
    }

    .toolbar-right {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .file-name {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .dirty-indicator {
      display: inline-block;
      width: 0.5rem;
      height: 0.5rem;
      border-radius: 50%;
      background: var(--sl-color-warning-500, #f59e0b);
      flex-shrink: 0;
    }

    .save-flash {
      font-size: 0.75rem;
      color: var(--sl-color-success-600, #16a34a);
      animation: fade-out 2s ease-in forwards;
    }

    @keyframes fade-out {
      0% { opacity: 1; }
      70% { opacity: 1; }
      100% { opacity: 0; }
    }

    .new-file-input {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      flex: 1;
      min-width: 0;
    }

    .new-file-input sl-input {
      flex: 1;
      min-width: 12rem;
      --sl-input-font-family: var(--scion-font-mono, monospace);
      --sl-input-font-size-small: 0.875rem;
    }

    .new-file-error {
      font-size: 0.75rem;
      color: var(--sl-color-danger-600, #dc2626);
    }

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 3rem;
      color: var(--scion-text-muted, #64748b);
    }

    .loading-state sl-spinner {
      font-size: 2rem;
      margin-bottom: 1rem;
    }

    .error-state {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.875rem;
      padding: 0.75rem 1rem;
      background: var(--sl-color-danger-50, #fef2f2);
      border-radius: var(--scion-radius, 0.5rem);
      margin-bottom: 0.75rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.initialPreview) {
      this.showPreview = true;
    }
    if (this.filePath && this.dataSource) {
      void this.loadFile();
    }
  }

  override updated(changed: Map<string, unknown>): void {
    if ((changed.has('filePath') || changed.has('dataSource')) && this.filePath && this.dataSource) {
      void this.loadFile();
    }
  }

  private async loadFile(): Promise<void> {
    if (!this.dataSource || !this.filePath) return;
    this.loading = true;
    this.error = null;

    try {
      const result = await this.dataSource.getFileContent(this.filePath);
      this.originalContent = result.content;
      this.currentContent = result.content;
      this.serverModTime = result.modTime;
    } catch (err) {
      console.error('Failed to load file:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load file';
    } finally {
      this.loading = false;
    }
  }

  private handleContentChanged(e: CustomEvent<{ content: string }>): void {
    this.currentContent = e.detail.content;
    this.saveSuccess = false;
  }

  private async handleSave(): Promise<void> {
    if (!this.dataSource) return;

    // For new files, validate the filename
    const savePath = this.isNewFile ? this.newFileName.trim() : this.filePath;
    if (!savePath) {
      this.newFileError = 'Filename is required';
      return;
    }

    // Basic path validation
    if (savePath.startsWith('/') || savePath.startsWith('..') || savePath.includes('/../')) {
      this.newFileError = 'Invalid file path';
      return;
    }

    this.saving = true;
    this.error = null;
    this.newFileError = '';

    try {
      const result = await this.dataSource.saveFileContent(
        savePath,
        this.currentContent,
        this.serverModTime || undefined
      );
      this.serverModTime = result.modTime;
      this.originalContent = this.currentContent;

      // If this was a new file, update the filePath
      if (this.isNewFile) {
        this.filePath = savePath;
      }

      this.saveSuccess = true;
      setTimeout(() => { this.saveSuccess = false; }, 2000);

      this.dispatchEvent(
        new CustomEvent('file-saved', {
          detail: { path: savePath },
          bubbles: true,
          composed: true,
        })
      );
    } catch (err) {
      console.error('Failed to save file:', err);
      this.error = err instanceof Error ? err.message : 'Save failed';
    } finally {
      this.saving = false;
    }
  }

  private handleRevert(): void {
    if (!this.dirty) return;
    if (!confirm('Discard unsaved changes?')) return;
    this.currentContent = this.originalContent;
    this.error = null;
  }

  private handleTogglePreview(): void {
    this.showPreview = !this.showPreview;
  }

  private handleClose(): void {
    if (this.dirty) {
      if (!confirm('You have unsaved changes. Close anyway?')) return;
    }

    this.dispatchEvent(
      new CustomEvent('editor-closed', {
        bubbles: true,
        composed: true,
      })
    );
  }

  private handleNewFileNameInput(e: InputEvent): void {
    const input = e.target as HTMLInputElement;
    this.newFileName = input.value;
    this.newFileError = '';
  }

  private handleNewFileNameKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter') {
      e.preventDefault();
      void this.handleSave();
    }
  }

  // ── Render ──

  override render() {
    return html`
      ${this.renderToolbar()}
      ${this.error ? html`<div class="error-state">${this.error}</div>` : nothing}
      ${this.loading
        ? html`
            <div class="loading-state">
              <sl-spinner></sl-spinner>
              <p>Loading file...</p>
            </div>
          `
        : this.showPreview && this.isMarkdown
          ? html`
              <scion-markdown-preview
                .content=${this.currentContent}
              ></scion-markdown-preview>
            `
          : html`
              <scion-code-editor
                .content=${this.currentContent}
                .language=${getLanguageFromPath(this.isNewFile ? this.newFileName : this.filePath)}
                ?readonly=${this.readonly}
                @content-changed=${this.handleContentChanged}
              ></scion-code-editor>
            `}
    `;
  }

  private renderToolbar() {
    return html`
      <div class="editor-toolbar">
        <div class="toolbar-left">
          ${this.isNewFile
            ? html`
                <div class="new-file-input">
                  <sl-input
                    size="small"
                    placeholder="path/to/filename.ext"
                    .value=${this.newFileName}
                    @sl-input=${this.handleNewFileNameInput}
                    @keydown=${this.handleNewFileNameKeydown}
                  ></sl-input>
                  ${this.newFileError
                    ? html`<span class="new-file-error">${this.newFileError}</span>`
                    : nothing}
                </div>
              `
            : html`
                <span class="file-name">${this.filePath}</span>
                ${this.dirty ? html`<span class="dirty-indicator" title="Unsaved changes"></span>` : nothing}
              `}
          ${this.saveSuccess ? html`<span class="save-flash">Saved</span>` : nothing}
        </div>
        <div class="toolbar-right">
          ${!this.readonly
            ? html`
                <sl-button
                  size="small"
                  variant="primary"
                  ?loading=${this.saving}
                  ?disabled=${this.saving || (!this.dirty && !this.isNewFile)}
                  @click=${this.handleSave}
                >
                  <sl-icon slot="prefix" name="floppy"></sl-icon>
                  Save
                </sl-button>
                <sl-button
                  size="small"
                  variant="default"
                  ?disabled=${!this.dirty}
                  @click=${this.handleRevert}
                >
                  <sl-icon slot="prefix" name="arrow-counterclockwise"></sl-icon>
                  Revert
                </sl-button>
              `
            : nothing}
          ${this.isMarkdown
            ? html`
                <sl-button
                  size="small"
                  variant=${this.showPreview ? 'primary' : 'default'}
                  @click=${this.handleTogglePreview}
                >
                  <sl-icon slot="prefix" name=${this.showPreview ? 'pencil' : 'eye'}></sl-icon>
                  ${this.showPreview ? 'Edit' : 'Preview'}
                </sl-button>
              `
            : nothing}
          <sl-button size="small" variant="default" @click=${this.handleClose}>
            Close
          </sl-button>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-file-editor': ScionFileEditor;
  }
}
