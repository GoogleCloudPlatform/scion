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
 * Skill creation page — combined create + publish flow.
 *
 * Form for creating a new skill with name, scope, visibility, description, and tags.
 * Optionally supports a combined create + publish flow via multipart POST.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';
import yaml from 'js-yaml';

import type { Capabilities, SkillVersion, SkillUploadUrl } from '../../shared/types.js';
import { can } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';

/* ------------------------------------------------------------------ */
/*  Types                                                              */
/* ------------------------------------------------------------------ */

interface SkillFrontmatter {
  name?: string;
  description?: string;
  tags?: string[];
}

interface SelectedFile {
  file: File;
  path: string;
}

interface UploadResult {
  path: string;
  size: number;
  hash: string;
  status: 'pending' | 'uploading' | 'done' | 'failed';
  error?: string;
}

type FlowState = 'form' | 'creating' | 'uploading' | 'finalizing' | 'done' | 'error';

const MAX_PASTE_SIZE = 512 * 1024; // 512 KB
const MAX_FILES = 50;
const MAX_FILE_SIZE = 10 * 1024 * 1024; // 10 MB
const MAX_TOTAL_SIZE = 50 * 1024 * 1024; // 50 MB

/* ------------------------------------------------------------------ */
/*  Frontmatter parser                                                 */
/* ------------------------------------------------------------------ */

function parseSkillFrontmatter(content: string): SkillFrontmatter | null {
  const match = content.match(/^---\r?\n([\s\S]*?)\r?\n---/);
  if (!match) return null;

  try {
    const parsed = yaml.load(match[1]) as Record<string, unknown>;
    if (!parsed || typeof parsed !== 'object') return null;

    const result: SkillFrontmatter = {};

    if (typeof parsed.name === 'string') result.name = parsed.name;
    if (typeof parsed.description === 'string') result.description = parsed.description;

    if (Array.isArray(parsed.tags)) {
      result.tags = parsed.tags.filter((t): t is string => typeof t === 'string');
    } else if (typeof parsed.tags === 'string') {
      result.tags = parsed.tags
        .split(',')
        .map((t) => t.trim())
        .filter(Boolean);
    }

    return result;
  } catch {
    return null;
  }
}

/* ------------------------------------------------------------------ */
/*  Component                                                          */
/* ------------------------------------------------------------------ */

@customElement('scion-page-skill-create')
export class ScionPageSkillCreate extends LitElement {
  /* --- capabilities --- */
  @state() private loading = true;
  @state() private canCreate = false;

  /* --- SKILL.md content --- */
  @state() private skillMdContent = '';

  /* --- metadata fields --- */
  @state() private name = '';
  @state() private description = '';
  @state() private scope: 'global' | 'project' | 'user' = 'global';
  @state() private scopeId = '';
  @state() private visibility: 'private' | 'public' = 'private';
  @state() private tagsInput = '';

  // Publish-immediately state
  @state() private publishImmediately = false;
  @state() private publishVersion = '1.0.0';
  @state() private publishFiles: Array<{ file: File; path: string }> = [];
  @state() private publishStep: 'idle' | 'creating' | 'publishing' | 'done' | 'error' = 'idle';
  @state() private publishError: string | null = null;
  private createdSkillId: string | null = null;
  private fileInputRef: HTMLInputElement | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }
    .back-link:hover {
      color: var(--scion-primary, #3b82f6);
    }

    .page-header {
      margin-bottom: 1.5rem;
    }
    .page-header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }
    .page-header h1 sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }
    .page-header p {
      color: var(--scion-text-muted, #64748b);
      margin: 0;
      font-size: 0.875rem;
    }

    .form-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      max-width: 640px;
    }

    .section-divider {
      border: none;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      margin: 1.5rem 0;
    }

    .section-header {
      font-size: 0.9375rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 1rem 0;
    }

    .section-desc {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin: -0.5rem 0 1rem 0;
    }

    .form-field {
      margin-bottom: 1.25rem;
    }

    .form-field label {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.375rem;
    }

    .form-field .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
    }

    .form-field sl-input,
    .form-field sl-textarea,
    .form-field sl-select,
    .form-field sl-radio-group {
      width: 100%;
    }

    .skillmd-textarea {
      --sl-input-font-family: 'SFMono-Regular', Consolas, 'Liberation Mono', Menlo, monospace;
      --sl-input-font-size-medium: 0.8125rem;
    }

    .upload-row {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-top: 0.5rem;
    }

    .from-badge {
      font-size: 0.6875rem;
    }

    .form-actions {
      display: flex;
      gap: 0.75rem;
      margin-top: 1.5rem;
      padding-top: 1.5rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .error-banner {
      background: var(--sl-color-danger-50, #fef2f2);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      margin-bottom: 1.25rem;
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      color: var(--sl-color-danger-700, #b91c1c);
      font-size: 0.875rem;
    }
    .error-banner sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .warning-banner {
      background: var(--sl-color-warning-50, #fffbeb);
      border: 1px solid var(--sl-color-warning-200, #fde68a);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      margin-bottom: 1.25rem;
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      color: var(--sl-color-warning-700, #b45309);
      font-size: 0.875rem;
    }
    .warning-banner sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .tag-chips {
      display: flex;
      flex-wrap: wrap;
      gap: 0.375rem;
      margin-top: 0.5rem;
    }
    .tag-chip {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.75rem;
      padding: 0.125rem 0.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 9999px;
      color: var(--scion-text, #1e293b);
    }

    /* Publish section styles */
    .publish-section {
      margin-top: 1.25rem;
      padding-top: 1.25rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .publish-toggle {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 1rem;
    }

    .publish-toggle label {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      cursor: pointer;
    }

    .publish-fields {
      display: flex;
      flex-direction: column;
      gap: 1rem;
      padding: 1rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
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
      background: var(--scion-surface, #ffffff);
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
      background: var(--scion-surface, #ffffff);
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

    .validation-error {
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
    .validation-error sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    /* Progress UI styles */
    .progress-overlay {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .progress-step {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }
    .progress-step .step-icon {
      flex-shrink: 0;
      width: 1.5rem;
      display: flex;
      align-items: center;
      justify-content: center;
    }
    .progress-step .step-icon.done {
      color: var(--sl-color-success-600, #16a34a);
    }
    .progress-step .step-icon.active {
      color: var(--scion-primary, #3b82f6);
    }
    .progress-step .step-icon.pending {
      color: var(--scion-text-muted, #64748b);
    }
    .progress-step .step-label {
      flex: 1;
    }
    .progress-step .step-status {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .publish-error-actions {
      display: flex;
      gap: 0.75rem;
      margin-top: 0.5rem;
    }
  `;

  /* ================================================================ */
  /*  Lifecycle                                                        */
  /* ================================================================ */

  override connectedCallback(): void {
    super.connectedCallback();
    void this.checkCapabilities();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.debounceTimer) clearTimeout(this.debounceTimer);
    if (this.redirectTimer) clearTimeout(this.redirectTimer);
  }

  private async checkCapabilities(): Promise<void> {
    this.loading = true;
    try {
      const res = await apiFetch('/api/v1/skills');
      if (res.ok) {
        const data = (await res.json()) as { _capabilities?: Capabilities };
        this.canCreate = can(data._capabilities, 'create');
      }
    } catch {
      // fail-closed
    } finally {
      this.loading = false;
    }
  }

  /* ================================================================ */
  /*  Helpers                                                          */
  /* ================================================================ */

  private get parsedTags(): string[] {
    if (!this.tagsInput.trim()) return [];
    return this.tagsInput
      .split(',')
      .map((t) => t.trim())
      .filter((t) => t.length > 0);
  }

  // -- File handling for publish section --

  private formatFileSize(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  }

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
    const newFiles: Array<{ file: File; path: string }> = [];
    for (const file of Array.from(this.fileInputRef.files)) {
      const path = file.webkitRelativePath || file.name;
      if (!this.publishFiles.some((f) => f.path === path)) {
        newFiles.push({ file, path });
      }
    }
    this.publishFiles = [...this.publishFiles, ...newFiles];
    this.publishError = null;
    this.fileInputRef.value = '';
  }

  private onDrop(e: DragEvent): void {
    e.preventDefault();
    const target = e.currentTarget as HTMLElement;
    target.classList.remove('dragover');
    if (!e.dataTransfer?.files) return;
    const newFiles: Array<{ file: File; path: string }> = [];
    for (const file of Array.from(e.dataTransfer.files)) {
      const path = file.name;
      if (!this.publishFiles.some((f) => f.path === path)) {
        newFiles.push({ file, path });
      }
    }
    this.publishFiles = [...this.publishFiles, ...newFiles];
    this.publishError = null;
  }

  private onDragOver(e: DragEvent): void {
    e.preventDefault();
    (e.currentTarget as HTMLElement).classList.add('dragover');
  }

  private onDragLeave(e: DragEvent): void {
    (e.currentTarget as HTMLElement).classList.remove('dragover');
  }

  private removeFile(index: number): void {
    this.publishFiles = this.publishFiles.filter((_, i) => i !== index);
  }

  // -- Publish file validation --

  private validatePublishFiles(): string | null {
    if (!this.publishVersion.trim()) return 'Version is required.';
    if (!/^\d+\.\d+\.\d+(-[\w.]+)?$/.test(this.publishVersion.trim().replace(/^v/, '')))
      return 'Version must be valid semver (e.g. 1.0.0).';
    if (this.publishFiles.length === 0) return 'At least one file is required.';
    if (!this.publishFiles.some((f) => f.file.name === 'SKILL.md' || f.path === 'SKILL.md'))
      return 'A file named exactly SKILL.md is required.';
    if (this.publishFiles.length > 50) return 'Maximum 50 files allowed.';
    const maxSize = 10 * 1024 * 1024;
    const oversize = this.publishFiles.find((f) => f.file.size > maxSize);
    if (oversize) return `File "${oversize.path}" exceeds 10 MB limit.`;
    const totalSize = this.publishFiles.reduce((sum, f) => sum + f.file.size, 0);
    if (totalSize > 50 * 1024 * 1024) return 'Total file size exceeds 50 MB limit.';
    return null;
  }

  // -- Submit flow --

  private async handleSubmit(): Promise<void> {
    if (!this.name.trim()) {
      this.error = 'Skill name is required.';
      return;
    }

    const fm = parseSkillFrontmatter(this.skillMdContent);
    if (!fm) return;

    if (fm.name !== undefined && !this.editedFields.has('name')) {
      this.name = fm.name;
    }
    if (fm.description !== undefined && !this.editedFields.has('description')) {
      this.description = fm.description;
    }
    if (fm.tags !== undefined && !this.editedFields.has('tags')) {
      this.tagsInput = fm.tags.join(', ');
    }
  }

  private onUploadSkillMdClick(): void {
    if (!this.skillMdInputRef) {
      this.skillMdInputRef = document.createElement('input');
      this.skillMdInputRef.type = 'file';
      this.skillMdInputRef.accept = '.md';
      this.skillMdInputRef.addEventListener('change', () => this.onSkillMdFileSelected());
    }
    this.skillMdInputRef.click();
  }

  private onSkillMdFileSelected(): void {
    const file = this.skillMdInputRef?.files?.[0];
    if (!file) return;

    if (file.size > MAX_PASTE_SIZE) {
      this.validationError = 'SKILL.md file exceeds 512 KB limit.';
      this.skillMdInputRef!.value = '';
      return;
    }

    if (this.publishImmediately) {
      const fileErr = this.validatePublishFiles();
      if (fileErr) {
        this.publishError = fileErr;
        return;
      }
      await this.handleCreateAndPublish();
      return;
    }

    // Existing create-only flow
    this.submitting = true;
    this.error = null;
    this.validationError = null;

    try {
      const skillId = await this.createSkill();
      this.createdSkillId = skillId;
      window.history.pushState({}, '', `/skills/${skillId}`);
      window.dispatchEvent(new PopStateEvent('popstate'));
    } catch (err) {
      this.flowState = 'form';
      this.error = err instanceof Error ? err.message : 'Failed to create skill';
    }
  }

  // -- Combined create + publish flow --

  private async createSkill(): Promise<string> {
    const body: Record<string, unknown> = {
      name: this.name.trim(),
      scope: this.scope,
      visibility: this.visibility,
    };

    if (this.description.trim()) {
      body.description = this.description.trim();
    }

    if (this.scope === 'project' && this.scopeId.trim()) {
      body.scopeId = this.scopeId.trim();
    }

    const tags = this.parsedTags;
    if (tags.length > 0) {
      body.tags = tags;
    }

    const response = await apiFetch('/api/v1/skills', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });

    if (!response.ok) {
      throw new Error(await extractApiError(response, `HTTP ${response.status}`));
    }

    const result = (await response.json()) as { skill?: { id: string }; id?: string };
    const skillId = result.skill?.id || result.id;

    if (!skillId) {
      throw new Error('No skill ID in response');
    }

    return skillId;
  }

  private async uploadAndPublishVersion(skillId: string): Promise<void> {
    const formData = new FormData();
    formData.append('version', this.publishVersion.trim());
    for (const pf of this.publishFiles) {
      formData.append('file', pf.file, pf.path);
    }

    const res = await apiFetch(`/api/v1/skills/${skillId}/versions`, {
      method: 'POST',
      body: formData,
    });

    if (!res.ok) {
      throw new Error(await extractApiError(res, `HTTP ${res.status}`));
    }
  }

  private async handleCreateAndPublish(): Promise<void> {
    this.submitting = true;
    this.error = null;
    this.publishError = null;
    this.publishStep = 'creating';

    try {
      // Step 1: Create the skill
      const skillId = await this.createSkill();
      this.createdSkillId = skillId;

      // Step 2: Publish first version via multipart POST
      this.publishStep = 'publishing';
      await this.uploadAndPublishVersion(skillId);

      // Step 3: Done — redirect
      this.publishStep = 'done';
      window.history.pushState({}, '', `/skills/${skillId}`);
      window.dispatchEvent(new PopStateEvent('popstate'));
    } catch (err) {
      console.error('Create & publish failed:', err);
      const message = err instanceof Error ? err.message : 'Operation failed';

      if (this.publishStep === 'creating') {
        // Step 1 failed — show as regular error
        this.publishStep = 'idle';
        this.error = message;
      } else {
        // Step 2 failed — skill was created, show publish error with recovery options
        this.publishStep = 'error';
        this.publishError = message;
      }
    } finally {
      this.submitting = false;
    }
  }

  private async retryPublish(): Promise<void> {
    if (!this.createdSkillId) return;
    this.submitting = true;
    this.publishError = null;
    this.publishStep = 'publishing';

    try {
      await this.uploadAndPublishVersion(this.createdSkillId);
      this.publishStep = 'done';
      window.history.pushState({}, '', `/skills/${this.createdSkillId}`);
      window.dispatchEvent(new PopStateEvent('popstate'));
    } catch (err) {
      console.error('Retry publish failed:', err);
      this.publishStep = 'error';
      this.publishError = err instanceof Error ? err.message : 'Publish failed';
    } finally {
      this.submitting = false;
    }
  }

  private navigateToCreatedSkill(): void {
    if (!this.createdSkillId) return;
    window.history.pushState({}, '', `/skills/${this.createdSkillId}`);
    window.dispatchEvent(new PopStateEvent('popstate'));
  }

  // -- Render --

  override render() {
    if (this.loading) {
      return html`
        <div
          style="display: flex; flex-direction: column; align-items: center; padding: 4rem 2rem; color: var(--scion-text-muted, #64748b);"
        >
          <sl-spinner style="font-size: 2rem; margin-bottom: 1rem;"></sl-spinner>
          <p>Loading...</p>
        </div>
      `;
    }

    if (!this.canCreate) {
      return html`
        <a href="/skills" class="back-link">
          <sl-icon name="arrow-left"></sl-icon>
          Back to Skills
        </a>
        <div
          style="text-align: center; padding: 3rem 2rem; background: var(--scion-surface, #ffffff); border: 1px solid var(--scion-border, #e2e8f0); border-radius: var(--scion-radius-lg, 0.75rem);"
        >
          <sl-icon
            name="shield-lock"
            style="font-size: 3rem; color: var(--scion-text-muted, #64748b); margin-bottom: 1rem;"
          ></sl-icon>
          <h2
            style="font-size: 1.25rem; font-weight: 600; color: var(--scion-text, #1e293b); margin: 0 0 0.5rem 0;"
          >
            Access Denied
          </h2>
          <p style="color: var(--scion-text-muted, #64748b); margin: 0 0 1rem 0;">
            You do not have permission to create skills.
          </p>
          <a href="/skills" style="text-decoration: none;">
            <sl-button variant="primary">
              <sl-icon slot="prefix" name="arrow-left"></sl-icon>
              Back to Skills
            </sl-button>
          </a>
        </div>
      `;
    }

    // Show progress overlay when combined flow is active
    if (this.publishStep !== 'idle') {
      return this.renderProgress();
    }

    return html`
      <a href="/skills" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Skills
      </a>

      <div class="page-header">
        <h1>
          <sl-icon name="lightning-charge"></sl-icon>
          Create Skill
        </h1>
        <p>Create a new skill, optionally publishing its first version in one step.</p>
      </div>

      ${this.flowState === 'form' || this.flowState === 'creating'
        ? this.renderForm()
        : this.renderProgress()}
    `;
  }

  /* ---------------------------------------------------------------- */
  /*  Form view                                                        */
  /* ---------------------------------------------------------------- */

  private renderForm() {
    const isSubmitting = this.flowState === 'creating';

    return html`
      <div class="form-card">
        ${this.error
          ? html`
              <div class="error-banner">
                <sl-icon name="exclamation-triangle"></sl-icon>
                <span>${this.error}</span>
              </div>
            `
          : nothing}
        ${this.validationError
          ? html`
              <div class="error-banner">
                <sl-icon name="exclamation-triangle"></sl-icon>
                <span>${this.validationError}</span>
              </div>
            `
          : nothing}
        ${this.duplicateSkillMdWarning
          ? html`
              <div class="warning-banner">
                <sl-icon name="exclamation-triangle"></sl-icon>
                <span
                  >A SKILL.md was uploaded via the drop zone but content already exists above. The
                  textarea content will be used.</span
                >
              </div>
            `
          : nothing}

          <!-- Publish first version section -->
          <div class="publish-section">
            <div class="publish-toggle">
              <sl-checkbox
                ?checked=${this.publishImmediately}
                @sl-change=${(e: Event) => { this.publishImmediately = (e.target as HTMLInputElement).checked; }}
              >
                Publish first version
              </sl-checkbox>
            </div>

            ${this.publishImmediately ? this.renderPublishFields() : nothing}
          </div>

          <div class="form-actions">
            <sl-button
              variant="text"
              size="small"
              @click=${() => this.onUploadSkillMdClick()}
              ?disabled=${isSubmitting}
            >
              <sl-icon slot="prefix" name="${this.publishImmediately ? 'upload' : 'lightning-charge'}"></sl-icon>
              ${this.publishImmediately ? 'Create & Publish' : 'Create Skill'}
            </sl-button>
            ${this.editedFields.size > 0 && this.hasSkillMd
              ? html`
                  <sl-button
                    variant="text"
                    size="small"
                    @click=${() => this.resetFromFrontmatter()}
                  >
                    <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                    Reset from SKILL.md
                  </sl-button>
                `
              : nothing}
          </div>
        </div>

        <hr class="section-divider" />

        <!-- Skill Details Section -->
        <h3 class="section-header">Skill Details</h3>

        <div class="form-field">
          <label for="name">
            Name
            ${this.isAutoPopulated('name')
              ? html`<sl-badge class="from-badge" variant="neutral">from SKILL.md</sl-badge>`
              : nothing}
          </label>
          <sl-input
            id="name"
            placeholder="my-skill"
            .value=${this.name}
            @sl-input=${(e: Event) => this.onFieldInput('name', e)}
            ?disabled=${isSubmitting}
            required
          ></sl-input>
        </div>

        <div class="form-field">
          <label for="description">
            Description
            ${this.isAutoPopulated('description')
              ? html`<sl-badge class="from-badge" variant="neutral">from SKILL.md</sl-badge>`
              : nothing}
          </label>
          <sl-textarea
            id="description"
            placeholder="What does this skill do?"
            .value=${this.description}
            @sl-input=${(e: Event) => this.onFieldInput('description', e)}
            ?disabled=${isSubmitting}
            maxlength="500"
            rows="3"
          ></sl-textarea>
        </div>

        <div class="form-field">
          <label for="scope">Scope</label>
          <sl-select
            id="scope"
            .value=${this.scope}
            @sl-change=${(e: Event) => {
              this.scope = (e.target as HTMLElement & { value: string }).value as
                | 'global'
                | 'project'
                | 'user';
            }}
            ?disabled=${isSubmitting}
          >
            <sl-option value="global">Global</sl-option>
            <sl-option value="project">Project</sl-option>
            <sl-option value="user">User</sl-option>
          </sl-select>
          <div class="hint">
            ${this.scope === 'global'
              ? 'Available to all projects and agents.'
              : this.scope === 'project'
                ? 'Scoped to a specific project.'
                : 'Scoped to your user account.'}
          </div>
        </div>

        ${this.scope === 'project'
          ? html`
              <div class="form-field">
                <label for="scopeId">Project ID</label>
                <sl-input
                  id="scopeId"
                  placeholder="project-uuid"
                  .value=${this.scopeId}
                  @sl-input=${(e: Event) => {
                    this.scopeId = (e.target as HTMLElement & { value: string }).value;
                  }}
                  ?disabled=${isSubmitting}
                ></sl-input>
              </div>
            `
          : nothing}
        ${this.scope === 'user'
          ? html`
              <div class="form-field">
                <div class="hint">Skills will be created under your user account.</div>
              </div>
            `
          : nothing}

        <div class="form-field">
          <label>Visibility</label>
          <sl-radio-group
            .value=${this.visibility}
            @sl-change=${(e: Event) => {
              this.visibility = (e.target as HTMLElement & { value: string }).value as
                | 'private'
                | 'public';
            }}
          >
            <sl-radio-button value="private" ?disabled=${isSubmitting}>Private</sl-radio-button>
            <sl-radio-button value="public" ?disabled=${isSubmitting}>Public</sl-radio-button>
          </sl-radio-group>
        </div>

        <div class="form-field">
          <label for="tags">
            Tags
            ${this.isAutoPopulated('tags')
              ? html`<sl-badge class="from-badge" variant="neutral">from SKILL.md</sl-badge>`
              : nothing}
          </label>
          <sl-input
            id="tags"
            placeholder="cli, automation, testing"
            .value=${this.tagsInput}
            @sl-input=${(e: Event) => this.onFieldInput('tags', e)}
            ?disabled=${isSubmitting}
          ></sl-input>
          <div class="hint">Comma-separated list of tags.</div>
          ${this.parsedTags.length > 0
            ? html`
                <div class="tag-chips">
                  ${this.parsedTags.map((tag) => html`<span class="tag-chip">${tag}</span>`)}
                </div>
              `
            : nothing}
        </div>

        <hr class="section-divider" />

        <!-- Files & Version Section -->
        <h3 class="section-header">
          ${this.hasSkillMd ? 'Additional Files (optional)' : 'Files & First Version (optional)'}
        </h3>

        <div
          class="drop-zone"
          @click=${() => this.onDropZoneClick()}
          @drop=${(e: DragEvent) => this.onDrop(e)}
          @dragover=${(e: DragEvent) => this.onDragOver(e)}
          @dragleave=${(e: DragEvent) => this.onDragLeave(e)}
        >
          <sl-icon name="upload"></sl-icon>
          ${this.hasSkillMd
            ? 'Drop additional files here or click to browse'
            : 'Drop files here or click to browse'}
        </div>

        ${this.allFiles.length > 0
          ? html`
              <ul class="file-list">
                ${this.allFiles.map(
                  (sf) => html`
                    <li class="file-item">
                      <div class="file-info">
                        <sl-icon name="file-earmark"></sl-icon>
                        <span class="file-name">${sf.path}</span>
                        <span class="file-meta">
                          ${this.formatFileSize(sf.file.size)}
                          ${sf.path === 'SKILL.md' && this.hasSkillMd ? '— from content above' : ''}
                        </span>
                      </div>
                      ${sf.path === 'SKILL.md' && this.hasSkillMd
                        ? nothing
                        : html`
                            <button
                              class="remove-btn"
                              @click=${() =>
                                this.removeFile(
                                  this.additionalFiles.findIndex((f) => f.path === sf.path)
                                )}
                              title="Remove"
                            >
                              <sl-icon name="x-lg"></sl-icon>
                            </button>
                          `}
                    </li>
                  `
                )}
              </ul>
            `
          : nothing}
        ${this.hasFiles
          ? html`
              <div class="form-field" style="margin-top: 1rem;">
                <label for="version">Version</label>
                <sl-input
                  id="version"
                  placeholder="1.0.0"
                  .value=${this.version}
                  @sl-input=${(e: Event) => {
                    this.version = (e.target as HTMLElement & { value: string }).value;
                  }}
                  ?disabled=${isSubmitting}
                  style="max-width: 200px;"
                ></sl-input>
              </div>
            `
          : nothing}

        <!-- Actions -->
        <div class="form-actions">
          ${this.hasFiles
            ? html`
                <sl-button
                  variant="primary"
                  ?loading=${isSubmitting}
                  ?disabled=${isSubmitting}
                  @click=${() => this.handleCreateAndPublish()}
                >
                  <sl-icon slot="prefix" name="lightning-charge"></sl-icon>
                  Create &amp; Publish v${this.version || '1.0.0'}
                </sl-button>
                <sl-button
                  variant="default"
                  ?disabled=${isSubmitting}
                  @click=${() => this.handleCreateOnly()}
                >
                  Create Only
                </sl-button>
              `
            : html`
                <sl-button
                  variant="primary"
                  ?loading=${isSubmitting}
                  ?disabled=${isSubmitting}
                  @click=${() => this.handleCreateOnly()}
                >
                  <sl-icon slot="prefix" name="lightning-charge"></sl-icon>
                  Create Skill
                </sl-button>
              `}
          <a href="/skills" style="text-decoration: none;">
            <sl-button variant="default" ?disabled=${isSubmitting}> Cancel </sl-button>
          </a>
        </div>
      </div>
    `;
  }

  private renderPublishFields() {
    return html`
      <div class="publish-fields">
        ${this.publishError
          ? html`
              <div class="validation-error">
                <sl-icon name="exclamation-triangle"></sl-icon>
                <span>${this.publishError}</span>
              </div>
            `
          : nothing}

        <div class="form-field" style="margin-bottom: 0;">
          <label>Version</label>
          <sl-input
            placeholder="1.0.0"
            .value=${this.publishVersion}
            @sl-input=${(e: Event) => { this.publishVersion = (e.target as HTMLElement & { value: string }).value; }}
          ></sl-input>
        </div>

        <div class="form-field" style="margin-bottom: 0;">
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
          <div class="hint">SKILL.md is required. Max 50 files, 10 MB each, 50 MB total.</div>
        </div>

        ${this.publishFiles.length > 0 ? html`
          <ul class="file-list">
            ${this.publishFiles.map((sf, i) => html`
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
    `;
  }

  private renderProgress() {
    const isCreating = this.publishStep === 'creating';
    const isPublishing = this.publishStep === 'publishing';
    const isError = this.publishStep === 'error';
    const createDone = this.publishStep !== 'creating';
    const publishDone = this.publishStep === 'done';

    return html`
      <a href="/skills" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Skills
      </a>

      <div class="page-header">
        <h1>
          <sl-icon name="lightning-charge"></sl-icon>
          Create & Publish Skill
        </h1>
        <p>${isError ? 'An error occurred during publishing.' : 'Creating your skill and publishing the first version...'}</p>
      </div>

      <div class="form-card">
        <div class="progress-overlay">
          <!-- Step 1: Create skill -->
          <div class="progress-step">
            <div class="step-icon ${createDone ? 'done' : 'active'}">
              ${createDone
                ? html`<sl-icon name="check-circle"></sl-icon>`
                : html`<sl-spinner style="font-size: 1.25rem;"></sl-spinner>`}
            </div>
            <span class="step-label">Creating skill...</span>
            <span class="step-status">${isCreating ? '' : 'done'}</span>
          </div>

          <!-- Step 2: Publish version -->
          <div class="progress-step">
            <div class="step-icon ${publishDone ? 'done' : isPublishing ? 'active' : isError ? 'done' : 'pending'}">
              ${publishDone
                ? html`<sl-icon name="check-circle"></sl-icon>`
                : isPublishing
                  ? html`<sl-spinner style="font-size: 1.25rem;"></sl-spinner>`
                  : isError
                    ? html`<sl-icon name="x-circle" style="color: var(--sl-color-danger-600, #dc2626);"></sl-icon>`
                    : html`<sl-icon name="clock"></sl-icon>`}
            </div>
            <span class="step-label">Uploading & publishing version...</span>
            <span class="step-status">
              ${publishDone ? 'done' : isPublishing ? '' : isError ? 'failed' : ''}
            </span>
          </div>

          ${isPublishing ? html`
            <sl-progress-bar indeterminate style="margin-top: 0.5rem;"></sl-progress-bar>
          ` : nothing}

          ${isError && this.publishError ? html`
            <div class="error-banner" style="margin-top: 0.5rem;">
              <sl-icon name="exclamation-triangle"></sl-icon>
              <span>${this.publishError}</span>
            </div>
            <div class="publish-error-actions">
              <sl-button
                variant="primary"
                size="small"
                ?loading=${this.submitting}
                @click=${() => this.retryPublish()}
              >
                <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                Retry Publishing
              </sl-button>
              <sl-button
                variant="default"
                size="small"
                @click=${() => this.navigateToCreatedSkill()}
              >
                Go to Skill
              </sl-button>
            </div>
          ` : nothing}
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-skill-create': ScionPageSkillCreate;
  }
}
