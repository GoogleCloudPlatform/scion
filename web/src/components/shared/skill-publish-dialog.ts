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
 * Multi-step publish version dialog for skills.
 *
 * Flow: file select → upload → done.
 * Uses a single multipart POST for server-side upload.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { SkillVersion } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';

type DialogStep = 'input' | 'uploading' | 'done' | 'error';

interface SelectedFile {
  file: File;
  path: string;
}

@customElement('scion-skill-publish-dialog')
export class ScionSkillPublishDialog extends LitElement {
  @property({ type: String }) skillId = '';
  @property({ type: Boolean, reflect: true }) open = false;
  @property({ type: String }) latestVersion = '';

  @state() private step: DialogStep = 'input';
  @state() private version = '';
  @state() private selectedFiles: SelectedFile[] = [];
  @state() private validationError: string | null = null;
  @state() private error: string | null = null;
  @state() private createdVersion: SkillVersion | null = null;

  private fileInputRef: HTMLInputElement | null = null;

  static override styles = css`
    :host { display: contents; }

    .step-content {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .form-field label {
      display: block;
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.375rem;
    }

    .drop-zone {
      border: 2px dashed var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 2rem;
      text-align: center;
      cursor: pointer;
      transition: all 150ms ease;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }
    .drop-zone:hover, .drop-zone.dragover {
      border-color: var(--scion-primary, #3b82f6);
      background: var(--sl-color-primary-50, #eff6ff);
      color: var(--scion-primary, #3b82f6);
    }
    .drop-zone sl-icon {
      font-size: 2rem;
      display: block;
      margin: 0 auto 0.5rem;
    }

    .file-list {
      list-style: none;
      padding: 0;
      margin: 0;
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
    }
    .file-item {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0.5rem 0.75rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.875rem;
    }
    .file-info {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      min-width: 0;
    }
    .file-name {
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }
    .file-size {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.75rem;
      flex-shrink: 0;
    }

    .remove-btn {
      cursor: pointer;
      background: none;
      border: none;
      padding: 0.25rem;
      color: var(--scion-text-muted, #64748b);
      line-height: 1;
    }
    .remove-btn:hover { color: var(--sl-color-danger-600, #dc2626); }

    .validation-error, .error-banner {
      background: var(--sl-color-danger-50, #fef2f2);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      color: var(--sl-color-danger-700, #b91c1c);
      font-size: 0.875rem;
    }
    .validation-error sl-icon, .error-banner sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .progress-section {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
    }
    .progress-label {
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      display: flex;
      justify-content: space-between;
    }

    .success-banner {
      background: var(--sl-color-success-50, #f0fdf4);
      border: 1px solid var(--sl-color-success-200, #bbf7d0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 1rem;
      text-align: center;
      color: var(--sl-color-success-700, #15803d);
    }
    .success-banner sl-icon {
      font-size: 2rem;
      display: block;
      margin: 0 auto 0.5rem;
    }

    input[type="file"] { display: none; }
  `;

  override updated(changed: Map<PropertyKey, unknown>): void {
    super.updated(changed);
    if (changed.has('open') && this.open) {
      this.reset();
      if (this.latestVersion) {
        this.version = this.bumpPatch(this.latestVersion);
      } else {
        this.version = '1.0.0';
      }
    }
  }

  private reset(): void {
    this.step = 'input';
    this.version = '';
    this.selectedFiles = [];
    this.validationError = null;
    this.error = null;
    this.createdVersion = null;
  }

  private bumpPatch(ver: string): string {
    const parts = ver.replace(/^v/, '').split('.');
    if (parts.length < 3) return ver;
    const patch = parseInt(parts[2], 10);
    if (isNaN(patch)) return ver;
    return `${parts[0]}.${parts[1]}.${patch + 1}`;
  }

  private formatFileSize(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  }

  private validateSemver(v: string): boolean {
    return /^\d+\.\d+\.\d+(-[\w.]+)?$/.test(v.replace(/^v/, ''));
  }

  // -- File handling --

  private onDropZoneClick(): void {
    if (!this.fileInputRef) {
      this.fileInputRef = document.createElement('input');
      this.fileInputRef.type = 'file';
      this.fileInputRef.multiple = true;
      this.fileInputRef.addEventListener('change', () => this.onFilesSelected());
    }
    this.fileInputRef.click();
  }

  private onFilesSelected(): void {
    if (!this.fileInputRef?.files) return;
    const newFiles: SelectedFile[] = [];
    for (const file of Array.from(this.fileInputRef.files)) {
      const path = file.webkitRelativePath || file.name;
      if (!this.selectedFiles.some((f) => f.path === path)) {
        newFiles.push({ file, path });
      }
    }
    this.selectedFiles = [...this.selectedFiles, ...newFiles];
    this.validationError = null;
    this.fileInputRef.value = '';
  }

  private onDrop(e: DragEvent): void {
    e.preventDefault();
    const target = e.currentTarget as HTMLElement;
    target.classList.remove('dragover');
    if (!e.dataTransfer?.files) return;
    const newFiles: SelectedFile[] = [];
    for (const file of Array.from(e.dataTransfer.files)) {
      const path = file.name;
      if (!this.selectedFiles.some((f) => f.path === path)) {
        newFiles.push({ file, path });
      }
    }
    this.selectedFiles = [...this.selectedFiles, ...newFiles];
    this.validationError = null;
  }

  private onDragOver(e: DragEvent): void {
    e.preventDefault();
    (e.currentTarget as HTMLElement).classList.add('dragover');
  }

  private onDragLeave(e: DragEvent): void {
    (e.currentTarget as HTMLElement).classList.remove('dragover');
  }

  private removeFile(index: number): void {
    this.selectedFiles = this.selectedFiles.filter((_, i) => i !== index);
  }

  // -- Validation --

  private validate(): string | null {
    if (!this.version.trim()) return 'Version is required.';
    if (!this.validateSemver(this.version.trim())) return 'Version must be valid semver (e.g. 1.0.0).';
    if (this.selectedFiles.length === 0) return 'At least one file is required.';
    if (!this.selectedFiles.some((f) => f.file.name === 'SKILL.md' || f.path === 'SKILL.md'))
      return 'A file named exactly SKILL.md is required.';
    if (this.selectedFiles.length > 50) return 'Maximum 50 files allowed.';
    const maxSize = 10 * 1024 * 1024;
    const oversize = this.selectedFiles.find((f) => f.file.size > maxSize);
    if (oversize) return `File "${oversize.path}" exceeds 10 MB limit.`;
    const totalSize = this.selectedFiles.reduce((sum, f) => sum + f.file.size, 0);
    if (totalSize > 50 * 1024 * 1024) return `Total file size exceeds 50 MB limit.`;
    return null;
  }

  // -- Upload flow --

  private async startPublish(): Promise<void> {
    const err = this.validate();
    if (err) {
      this.validationError = err;
      return;
    }

    this.step = 'uploading';
    this.error = null;

    const formData = new FormData();
    formData.append('version', this.version.trim());
    for (const sf of this.selectedFiles) {
      formData.append('file', sf.file, sf.path);
    }

    try {
      const res = await apiFetch(`/api/v1/skills/${this.skillId}/versions`, {
        method: 'POST',
        body: formData,
        // DO NOT set Content-Type — browser sets it with the multipart boundary
      });

      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      const data = (await res.json()) as { version?: SkillVersion };
      this.createdVersion = data.version ?? null;
      this.step = 'done';

      this.dispatchEvent(new CustomEvent('skill-version-published', {
        detail: { version: this.createdVersion },
        bubbles: true,
        composed: true,
      }));
    } catch (err) {
      console.error('Publish failed:', err);
      this.step = 'error';
      this.error = err instanceof Error ? err.message : 'Publish failed';
    }
  }

  // -- Dialog events --

  private onDialogHide(): void {
    this.open = false;
    this.dispatchEvent(new CustomEvent('sl-after-hide'));
  }

  // -- Render --

  override render() {
    return html`
      <sl-dialog
        label="Publish New Version"
        ?open=${this.open}
        @sl-after-hide=${() => this.onDialogHide()}
        style="--width: 540px;"
      >
        ${this.step === 'input' ? this.renderInput() : nothing}
        ${this.step === 'uploading' ? this.renderUploading() : nothing}
        ${this.step === 'done' ? this.renderDone() : nothing}
        ${this.step === 'error' ? this.renderError() : nothing}
      </sl-dialog>
    `;
  }

  private renderInput() {
    return html`
      <div class="step-content">
        ${this.validationError ? html`
          <div class="validation-error">
            <sl-icon name="exclamation-triangle"></sl-icon>
            <span>${this.validationError}</span>
          </div>
        ` : nothing}

        <div class="form-field">
          <label>Version</label>
          <sl-input
            placeholder="1.0.0"
            .value=${this.version}
            @sl-input=${(e: Event) => { this.version = (e.target as HTMLElement & { value: string }).value; }}
          ></sl-input>
        </div>

        <div class="form-field">
          <label>Files</label>
          <div
            class="drop-zone"
            @click=${() => this.onDropZoneClick()}
            @drop=${(e: DragEvent) => this.onDrop(e)}
            @dragover=${(e: DragEvent) => this.onDragOver(e)}
            @dragleave=${(e: DragEvent) => this.onDragLeave(e)}
          >
            <sl-icon name="upload"></sl-icon>
            Drop files here or click to browse
          </div>
        </div>

        ${this.selectedFiles.length > 0 ? html`
          <ul class="file-list">
            ${this.selectedFiles.map((sf, i) => html`
              <li class="file-item">
                <div class="file-info">
                  <sl-icon name="file-earmark"></sl-icon>
                  <span class="file-name">${sf.path}</span>
                  <span class="file-size">${this.formatFileSize(sf.file.size)}</span>
                </div>
                <button class="remove-btn" @click=${() => this.removeFile(i)} title="Remove">
                  <sl-icon name="x-lg"></sl-icon>
                </button>
              </li>
            `)}
          </ul>
        ` : nothing}
      </div>

      <sl-button slot="footer" variant="default" @click=${() => { this.open = false; }}>
        Cancel
      </sl-button>
      <sl-button
        slot="footer"
        variant="primary"
        @click=${() => this.startPublish()}
        ?disabled=${this.selectedFiles.length === 0 || !this.version.trim()}
      >
        <sl-icon slot="prefix" name="upload"></sl-icon>
        Upload &amp; Publish
      </sl-button>
    `;
  }

  private renderUploading() {
    return html`
      <div class="step-content">
        <div class="progress-section">
          <div class="progress-label">
            <span>Uploading &amp; publishing...</span>
          </div>
          <sl-progress-bar indeterminate></sl-progress-bar>
        </div>
      </div>
    `;
  }

  private renderDone() {
    return html`
      <div class="step-content">
        <div class="success-banner">
          <sl-icon name="check-circle"></sl-icon>
          <p><strong>Version ${this.version} published successfully!</strong></p>
        </div>
      </div>
      <sl-button slot="footer" variant="primary" @click=${() => { this.open = false; }}>
        Close
      </sl-button>
    `;
  }

  private renderError() {
    return html`
      <div class="step-content">
        <div class="error-banner">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <span>${this.error}</span>
        </div>
      </div>
      <sl-button slot="footer" variant="default" @click=${() => { this.open = false; }}>
        Cancel
      </sl-button>
      <sl-button slot="footer" variant="primary" @click=${() => this.startPublish()}>
        <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
        Retry
      </sl-button>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-skill-publish-dialog': ScionSkillPublishDialog;
  }
}
