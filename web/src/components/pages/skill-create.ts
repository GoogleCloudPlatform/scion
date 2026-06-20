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
 * Supports pasting/uploading SKILL.md with YAML frontmatter auto-populate,
 * additional file uploads, and an inline progress view for the multi-step
 * create → upload → finalize sequence.
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

  /* --- auto-populate tracking --- */
  private editedFields = new Set<string>();
  private debounceTimer: ReturnType<typeof setTimeout> | null = null;

  /* --- additional files --- */
  @state() private additionalFiles: SelectedFile[] = [];
  @state() private duplicateSkillMdWarning = false;
  @state() private version = '1.0.0';

  /* --- flow state --- */
  @state() private flowState: FlowState = 'form';
  @state() private error: string | null = null;
  @state() private validationError: string | null = null;
  @state() private uploadResults: UploadResult[] = [];
  @state() private uploadedCount = 0;
  @state() private createdSkillId: string | null = null;

  private fileInputRef: HTMLInputElement | null = null;
  private skillMdInputRef: HTMLInputElement | null = null;
  private redirectTimer: ReturnType<typeof setTimeout> | null = null;
  private versionCreated = false;

  /* --- allFiles cache --- */
  private cachedAllFiles: SelectedFile[] | null = null;
  private cachedSkillMdContent = '';
  private cachedAdditionalFiles: SelectedFile[] = [];

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

    /* --- drop zone --- */
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
    .drop-zone:hover,
    .drop-zone.dragover {
      border-color: var(--scion-primary, #3b82f6);
      background: var(--sl-color-primary-50, #eff6ff);
      color: var(--scion-primary, #3b82f6);
    }
    .drop-zone sl-icon {
      font-size: 2rem;
      display: block;
      margin: 0 auto 0.5rem;
    }

    /* --- file list --- */
    .file-list {
      list-style: none;
      padding: 0;
      margin: 0.75rem 0 0 0;
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
    .file-meta {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.75rem;
      flex-shrink: 0;
    }
    .file-status {
      display: flex;
      align-items: center;
      gap: 0.25rem;
      flex-shrink: 0;
    }
    .file-status.done {
      color: var(--sl-color-success-600, #16a34a);
    }
    .file-status.failed {
      color: var(--sl-color-danger-600, #dc2626);
    }
    .file-status.uploading {
      color: var(--scion-primary, #3b82f6);
    }

    .remove-btn {
      cursor: pointer;
      background: none;
      border: none;
      padding: 0.25rem;
      color: var(--scion-text-muted, #64748b);
      line-height: 1;
    }
    .remove-btn:hover {
      color: var(--sl-color-danger-600, #dc2626);
    }

    /* --- progress view --- */
    .progress-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      max-width: 640px;
    }
    .progress-title {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 1.25rem 0;
    }
    .progress-steps {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
      margin-bottom: 1.25rem;
    }
    .progress-step {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
    }
    .progress-step.active {
      color: var(--scion-primary, #3b82f6);
      font-weight: 500;
    }
    .progress-step.done {
      color: var(--sl-color-success-600, #16a34a);
    }
    .progress-step.error {
      color: var(--sl-color-danger-600, #dc2626);
    }
    .step-label {
      flex: 1;
    }
    .step-status {
      font-size: 0.75rem;
    }

    .progress-actions {
      display: flex;
      gap: 0.75rem;
      margin-top: 1.25rem;
      padding-top: 1.25rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
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

    input[type='file'] {
      display: none;
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

  private get hasSkillMd(): boolean {
    return this.skillMdContent.trim().length > 0;
  }

  private get allFiles(): SelectedFile[] {
    if (
      this.cachedAllFiles !== null &&
      this.cachedSkillMdContent === this.skillMdContent &&
      this.cachedAdditionalFiles === this.additionalFiles
    ) {
      return this.cachedAllFiles;
    }

    const files: SelectedFile[] = [];
    if (this.hasSkillMd) {
      const blob = new Blob([this.skillMdContent], { type: 'text/markdown' });
      const skillMdFile = new File([blob], 'SKILL.md', { type: 'text/markdown' });
      files.push({ file: skillMdFile, path: 'SKILL.md' });
    }
    for (const f of this.additionalFiles) {
      if (f.path === 'SKILL.md' && this.hasSkillMd) continue;
      files.push(f);
    }

    this.cachedAllFiles = files;
    this.cachedSkillMdContent = this.skillMdContent;
    this.cachedAdditionalFiles = this.additionalFiles;
    return files;
  }

  private get hasFiles(): boolean {
    return this.allFiles.length > 0;
  }

  private formatFileSize(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  }

  private validateSemver(v: string): boolean {
    return /^\d+\.\d+\.\d+(-[\w.-]+)?(\+[\w.-]+)?$/.test(v.replace(/^v/, ''));
  }

  private cleanVersion(v: string): string {
    return v.trim().replace(/^v/, '');
  }

  /* ================================================================ */
  /*  SKILL.md content handling                                        */
  /* ================================================================ */

  private onSkillMdInput(e: Event): void {
    const target = e.target as HTMLElement & { value: string };
    const value = target.value;

    if (new Blob([value]).size > MAX_PASTE_SIZE) {
      this.validationError = 'SKILL.md content exceeds 512 KB. Please upload the file instead.';
      this.skillMdContent = '';
      target.value = '';
      return;
    }
    this.validationError = null;
    this.skillMdContent = value;
    this.debounceParseFrontmatter();
  }

  private debounceParseFrontmatter(): void {
    if (this.debounceTimer) clearTimeout(this.debounceTimer);
    this.debounceTimer = setTimeout(() => this.applyFrontmatter(), 300);
  }

  private applyFrontmatter(): void {
    if (!this.hasSkillMd) {
      if (!this.editedFields.has('name')) this.name = '';
      if (!this.editedFields.has('description')) this.description = '';
      if (!this.editedFields.has('tags')) this.tagsInput = '';
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

    const reader = new FileReader();
    reader.onload = () => {
      this.skillMdContent = reader.result as string;
      this.validationError = null;
      this.applyFrontmatter();
    };
    reader.readAsText(file);
    this.skillMdInputRef!.value = '';
  }

  private isAutoPopulated(field: string): boolean {
    return this.hasSkillMd && !this.editedFields.has(field);
  }

  private onFieldInput(field: string, e: Event): void {
    const value = (e.target as HTMLElement & { value: string }).value;
    this.editedFields.add(field);
    switch (field) {
      case 'name':
        this.name = value;
        break;
      case 'description':
        this.description = value;
        break;
      case 'tags':
        this.tagsInput = value;
        break;
    }
  }

  private resetFromFrontmatter(): void {
    this.editedFields.clear();
    this.applyFrontmatter();
  }

  /* ================================================================ */
  /*  Additional file handling                                         */
  /* ================================================================ */

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
    this.addFiles(Array.from(this.fileInputRef.files));
    this.fileInputRef.value = '';
  }

  private onDrop(e: DragEvent): void {
    e.preventDefault();
    (e.currentTarget as HTMLElement).classList.remove('dragover');
    if (!e.dataTransfer?.files) return;
    this.addFiles(Array.from(e.dataTransfer.files));
  }

  private onDragOver(e: DragEvent): void {
    e.preventDefault();
    (e.currentTarget as HTMLElement).classList.add('dragover');
  }

  private onDragLeave(e: DragEvent): void {
    (e.currentTarget as HTMLElement).classList.remove('dragover');
  }

  private addFiles(files: File[]): void {
    const newFiles: SelectedFile[] = [];
    let hasDuplicateSkillMd = false;

    for (const file of files) {
      const path = file.webkitRelativePath || file.name;
      if (path === 'SKILL.md' && this.hasSkillMd) {
        hasDuplicateSkillMd = true;
        continue;
      }
      if (!this.additionalFiles.some((f) => f.path === path)) {
        newFiles.push({ file, path });
      }
    }

    this.duplicateSkillMdWarning = hasDuplicateSkillMd;
    this.additionalFiles = [...this.additionalFiles, ...newFiles];
    this.validationError = null;
  }

  private removeFile(index: number): void {
    this.additionalFiles = this.additionalFiles.filter((_, i) => i !== index);
    this.duplicateSkillMdWarning = false;
  }

  /* ================================================================ */
  /*  Validation                                                       */
  /* ================================================================ */

  private validateForPublish(): string | null {
    if (this.validationError) return this.validationError;
    if (!this.name.trim()) return 'Skill name is required.';
    if (this.scope === 'project' && !this.scopeId.trim())
      return 'Project ID is required for project scope.';
    if (!this.cleanVersion(this.version)) return 'Version is required.';
    if (!this.validateSemver(this.cleanVersion(this.version)))
      return 'Version must be valid semver (e.g. 1.0.0).';

    const files = this.allFiles;
    if (!files.some((f) => f.path === 'SKILL.md'))
      return 'SKILL.md is required when publishing. Paste content above or upload the file.';
    if (files.length > MAX_FILES) return `Maximum ${MAX_FILES} files allowed.`;
    const oversize = files.find((f) => f.file.size > MAX_FILE_SIZE);
    if (oversize) return `File "${oversize.path}" exceeds 10 MB limit.`;
    const totalSize = files.reduce((sum, f) => sum + f.file.size, 0);
    if (totalSize > MAX_TOTAL_SIZE) return 'Total file size exceeds 50 MB limit.';
    return null;
  }

  private validateForCreate(): string | null {
    if (this.validationError) return this.validationError;
    if (!this.name.trim()) return 'Skill name is required.';
    if (this.scope === 'project' && !this.scopeId.trim())
      return 'Project ID is required for project scope.';
    return null;
  }

  /* ================================================================ */
  /*  Submit — Create Only                                             */
  /* ================================================================ */

  private async handleCreateOnly(): Promise<void> {
    const err = this.validateForCreate();
    if (err) {
      this.validationError = err;
      return;
    }

    this.flowState = 'creating';
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

  /* ================================================================ */
  /*  Submit — Create & Publish                                        */
  /* ================================================================ */

  private async handleCreateAndPublish(): Promise<void> {
    const err = this.validateForPublish();
    if (err) {
      this.validationError = err;
      return;
    }

    this.validationError = null;
    this.error = null;
    this.flowState = 'creating';
    this.uploadedCount = 0;
    this.uploadResults = [];

    const files = this.allFiles;

    try {
      // Step 1: Create skill
      const skillId = await this.createSkill();
      this.createdSkillId = skillId;

      // Step 2: Create draft version
      this.flowState = 'uploading';
      this.uploadResults = files.map((f) => ({
        path: f.path,
        size: f.file.size,
        hash: '',
        status: 'pending' as const,
      }));

      const filesPayload = files.map((f) => ({ path: f.path, size: f.file.size }));
      const createVersionRes = await apiFetch(`/api/v1/skills/${skillId}/versions`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ version: this.cleanVersion(this.version), files: filesPayload }),
      });

      if (!createVersionRes.ok) {
        throw new Error(await extractApiError(createVersionRes, `HTTP ${createVersionRes.status}`));
      }

      const createData = (await createVersionRes.json()) as {
        version?: SkillVersion;
        uploadUrls?: SkillUploadUrl[];
        urls?: SkillUploadUrl[];
      };
      const uploadUrls = createData.uploadUrls || createData.urls || [];
      this.versionCreated = true;

      // Step 3: Upload files
      await this.uploadFiles(files, uploadUrls);

      const failed = this.uploadResults.filter((r) => r.status === 'failed');
      if (failed.length > 0) {
        this.flowState = 'error';
        this.error = `${failed.length} file(s) failed to upload.`;
        return;
      }

      // Step 4: Finalize
      this.flowState = 'finalizing';
      await this.finalizeVersion(skillId);

      // Step 5: Done → redirect
      this.flowState = 'done';
      this.redirectTimer = setTimeout(() => {
        window.history.pushState({}, '', `/skills/${skillId}`);
        window.dispatchEvent(new PopStateEvent('popstate'));
      }, 1500);
    } catch (err) {
      console.error('Create & publish failed:', err);
      if (this.flowState === 'creating') {
        this.flowState = 'form';
        this.error = err instanceof Error ? err.message : 'Failed to create skill';
      } else {
        this.flowState = 'error';
        this.error = err instanceof Error ? err.message : 'Publishing failed';
      }
    }
  }

  /* ================================================================ */
  /*  API helpers                                                      */
  /* ================================================================ */

  private async createSkill(): Promise<string> {
    const body: Record<string, unknown> = {
      name: this.name.trim(),
      scope: this.scope,
      visibility: this.visibility,
    };
    if (this.description.trim()) body.description = this.description.trim();
    if (this.scope === 'project' && this.scopeId.trim()) body.scopeId = this.scopeId.trim();
    const tags = this.parsedTags;
    if (tags.length > 0) body.tags = tags;

    const res = await apiFetch('/api/v1/skills', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      throw new Error(await extractApiError(res, `HTTP ${res.status}`));
    }
    const result = (await res.json()) as { skill?: { id: string }; id?: string };
    const skillId = result.skill?.id || result.id;
    if (!skillId) throw new Error('No skill ID in response');
    return skillId;
  }

  private async uploadFiles(
    files: SelectedFile[],
    uploadUrls: SkillUploadUrl[],
    indices?: number[]
  ): Promise<void> {
    if (!window.crypto?.subtle) {
      throw new Error('Cryptography APIs not available. Requires HTTPS or localhost.');
    }

    const concurrency = 4;
    const queue = indices ?? Array.from({ length: files.length }, (_, i) => i);
    let queuePos = 0;

    const urlMap = new Map(uploadUrls.map((u) => [u.path, u]));

    const uploadOne = async (): Promise<void> => {
      while (queuePos < queue.length) {
        const i = queue[queuePos++];
        const sf = files[i];
        const urlInfo = urlMap.get(sf.path);
        if (!urlInfo) {
          this.uploadResults = this.uploadResults.map((r, ri) =>
            ri === i ? { ...r, status: 'failed' as const, error: 'No upload URL' } : r
          );
          continue;
        }

        this.uploadResults = this.uploadResults.map((r, ri) =>
          ri === i ? { ...r, status: 'uploading' as const } : r
        );
        this.requestUpdate();

        try {
          const buffer = await sf.file.arrayBuffer();
          const hashBuffer = await crypto.subtle.digest('SHA-256', buffer);
          const hashArray = Array.from(new Uint8Array(hashBuffer));
          const hashHex = hashArray.map((b) => b.toString(16).padStart(2, '0')).join('');
          const hash = `sha256:${hashHex}`;

          const headers: Record<string, string> = urlInfo.headers || {};
          const doUpload = () =>
            fetch(urlInfo.url, {
              method: urlInfo.method || 'PUT',
              headers,
              body: buffer,
            });

          let res: Response;
          try {
            res = await doUpload();
          } catch {
            // Retry once on network failure (DNS, CORS, connection refused, etc.)
            try {
              res = await doUpload();
            } catch (retryErr) {
              throw new Error(
                `Network error: ${retryErr instanceof Error ? retryErr.message : 'Upload failed'}`
              );
            }
          }

          if (!res.ok) {
            // Retry once on HTTP error
            try {
              res = await doUpload();
            } catch (retryErr) {
              throw new Error(
                `Network error: ${retryErr instanceof Error ? retryErr.message : 'Upload failed'}`
              );
            }
            if (!res.ok) throw new Error(`Upload failed: ${res.status}`);
          }

          this.uploadResults = this.uploadResults.map((r, ri) =>
            ri === i ? { ...r, status: 'done' as const, hash } : r
          );
          this.uploadedCount++;
        } catch (err) {
          this.uploadResults = this.uploadResults.map((r, ri) =>
            ri === i
              ? {
                  ...r,
                  status: 'failed' as const,
                  error: err instanceof Error ? err.message : 'Upload failed',
                }
              : r
          );
        }
        this.requestUpdate();
      }
    };

    const workers = Array.from({ length: Math.min(concurrency, queue.length) }, () => uploadOne());
    await Promise.all(workers);
  }

  private async finalizeVersion(skillId: string): Promise<void> {
    const manifest = {
      files: this.uploadResults.map((r) => ({
        path: r.path,
        size: r.size,
        hash: r.hash,
      })),
    };

    const res = await apiFetch(`/api/v1/skills/${skillId}/finalize`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ version: this.cleanVersion(this.version), manifest }),
    });
    if (!res.ok) {
      throw new Error(await extractApiError(res, `HTTP ${res.status}`));
    }
  }

  private async retryPublish(): Promise<void> {
    if (!this.createdSkillId) return;
    this.error = null;

    const files = this.allFiles;

    // If version creation never completed (step 2 failed), re-attempt from step 2
    if (!this.versionCreated) {
      this.flowState = 'uploading';
      this.uploadResults = files.map((f) => ({
        path: f.path,
        size: f.file.size,
        hash: '',
        status: 'pending' as const,
      }));

      try {
        const filesPayload = files.map((f) => ({ path: f.path, size: f.file.size }));
        const createVersionRes = await apiFetch(`/api/v1/skills/${this.createdSkillId}/versions`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            version: this.cleanVersion(this.version),
            files: filesPayload,
          }),
        });

        if (!createVersionRes.ok) {
          throw new Error(
            await extractApiError(createVersionRes, `HTTP ${createVersionRes.status}`)
          );
        }

        const createData = (await createVersionRes.json()) as {
          version?: SkillVersion;
          uploadUrls?: SkillUploadUrl[];
          urls?: SkillUploadUrl[];
        };
        const uploadUrls = createData.uploadUrls || createData.urls || [];
        this.versionCreated = true;

        await this.uploadFiles(files, uploadUrls);

        const failed = this.uploadResults.filter((r) => r.status === 'failed');
        if (failed.length > 0) {
          this.flowState = 'error';
          this.error = `${failed.length} file(s) failed to upload.`;
          return;
        }
      } catch (err) {
        this.flowState = 'error';
        this.error = err instanceof Error ? err.message : 'Retry failed';
        return;
      }

      // Fall through to finalize below
    }

    const failedFiles = this.uploadResults
      .map((r, i) => ({ result: r, index: i, file: files[i] }))
      .filter((x) => x.result.status === 'failed');

    if (failedFiles.length > 0) {
      this.flowState = 'uploading';

      try {
        const filesPayload = failedFiles.map((x) => ({
          path: x.file.path,
          size: x.file.file.size,
        }));
        const res = await apiFetch(`/api/v1/skills/${this.createdSkillId}/upload`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ version: this.cleanVersion(this.version), files: filesPayload }),
        });
        if (!res.ok) {
          throw new Error(await extractApiError(res, 'Failed to get upload URLs'));
        }
        const data = (await res.json()) as {
          uploadUrls?: SkillUploadUrl[];
          urls?: SkillUploadUrl[];
        };
        const uploadUrls = data.uploadUrls || data.urls || [];

        const failedIndexSet = new Set(failedFiles.map((f) => f.index));
        this.uploadResults = this.uploadResults.map((r, ri) => {
          if (!failedIndexSet.has(ri)) return r;
          // eslint-disable-next-line @typescript-eslint/no-unused-vars
          const { error: _discarded, ...rest } = r;
          return { ...rest, status: 'pending' as const };
        });

        const failedIndices = [...failedIndexSet];
        await this.uploadFiles(files, uploadUrls, failedIndices);

        const stillFailed = this.uploadResults.filter((r) => r.status === 'failed');
        if (stillFailed.length > 0) {
          this.flowState = 'error';
          this.error = `${stillFailed.length} file(s) still failed.`;
          return;
        }
      } catch (err) {
        this.flowState = 'error';
        this.error = err instanceof Error ? err.message : 'Retry failed';
        return;
      }
    }

    // All uploads done — finalize
    try {
      this.flowState = 'finalizing';
      await this.finalizeVersion(this.createdSkillId);
      this.flowState = 'done';
      this.redirectTimer = setTimeout(() => {
        window.history.pushState({}, '', `/skills/${this.createdSkillId}`);
        window.dispatchEvent(new PopStateEvent('popstate'));
      }, 1500);
    } catch (err) {
      this.flowState = 'error';
      this.error = err instanceof Error ? err.message : 'Finalize failed';
    }
  }

  /* ================================================================ */
  /*  Render                                                           */
  /* ================================================================ */

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

        <!-- SKILL.md Content Section -->
        <h3 class="section-header">SKILL.md Content</h3>
        <p class="section-desc">
          Paste your SKILL.md below or upload the file. Metadata fields will auto-populate from
          frontmatter.
        </p>

        <div class="form-field">
          <sl-textarea
            class="skillmd-textarea"
            rows="12"
            placeholder=${`---\nname: my-skill\ndescription: What this skill does\ntags: [cli, automation]\n---\n\n# My Skill\n\nInstructions for the agent...`}
            .value=${this.skillMdContent}
            @sl-input=${(e: Event) => this.onSkillMdInput(e)}
            ?disabled=${isSubmitting}
          ></sl-textarea>
          <div class="upload-row">
            <sl-button
              variant="text"
              size="small"
              @click=${() => this.onUploadSkillMdClick()}
              ?disabled=${isSubmitting}
            >
              <sl-icon slot="prefix" name="upload"></sl-icon>
              Upload SKILL.md
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

  /* ---------------------------------------------------------------- */
  /*  Progress view                                                    */
  /* ---------------------------------------------------------------- */

  private renderProgress() {
    const stepDone = (s: FlowState) => {
      const order: FlowState[] = ['creating', 'uploading', 'finalizing', 'done'];
      const current = order.indexOf(this.flowState);
      const target = order.indexOf(s);
      return target < current;
    };

    const stepActive = (s: FlowState) => this.flowState === s;
    const stepError = (s: FlowState) => this.flowState === 'error' && this.getErrorStep() === s;

    const stepClass = (s: FlowState) => {
      if (stepDone(s)) return 'progress-step done';
      if (stepError(s)) return 'progress-step error';
      if (stepActive(s)) return 'progress-step active';
      return 'progress-step';
    };

    const stepIcon = (s: FlowState) => {
      if (stepDone(s)) return html`<sl-icon name="check-circle"></sl-icon>`;
      if (stepError(s)) return html`<sl-icon name="x-circle"></sl-icon>`;
      if (stepActive(s)) return html`<sl-spinner style="font-size: 1rem;"></sl-spinner>`;
      return html`<sl-icon name="circle"></sl-icon>`;
    };

    const total = this.uploadResults.length;
    const done = this.uploadResults.filter((r) => r.status === 'done').length;
    const pct = total > 0 ? Math.round((done / total) * 100) : 0;

    return html`
      <div class="progress-card">
        <h2 class="progress-title">
          ${this.flowState === 'done'
            ? `Created & published ${this.name} v${this.version}`
            : `Creating & Publishing ${this.name} v${this.version}`}
        </h2>

        ${this.flowState === 'done'
          ? html`
              <div class="success-banner">
                <sl-icon name="check-circle"></sl-icon>
                <p>
                  <strong
                    >Skill "${this.name}" created and version ${this.version} published
                    successfully!</strong
                  >
                </p>
                <p style="font-size: 0.8125rem; margin-top: 0.5rem;">
                  Redirecting to skill detail page...
                </p>
              </div>
            `
          : html`
              <div class="progress-steps">
                <div class=${stepClass('creating')}>
                  ${stepIcon('creating')}
                  <span class="step-label">Creating skill...</span>
                  <span class="step-status"
                    >${stepDone('creating') ? 'done' : stepActive('creating') ? '' : ''}</span
                  >
                </div>
                <div class=${stepClass('uploading')}>
                  ${stepIcon('uploading')}
                  <span class="step-label">Uploading files...</span>
                  <span class="step-status"
                    >${stepActive('uploading') || stepDone('uploading')
                      ? `${done} / ${total}`
                      : ''}</span
                  >
                </div>
                <div class=${stepClass('finalizing')}>
                  ${stepIcon('finalizing')}
                  <span class="step-label">Finalizing version...</span>
                  <span class="step-status"
                    >${stepDone('finalizing') ? 'done' : stepActive('finalizing') ? '' : ''}</span
                  >
                </div>
              </div>

              ${this.flowState === 'uploading' ||
              (this.flowState === 'error' && this.uploadResults.length > 0)
                ? html`
                    <ul class="file-list">
                      ${this.uploadResults.map(
                        (r) => html`
                          <li class="file-item">
                            <div class="file-info">
                              <sl-icon name="file-earmark"></sl-icon>
                              <span class="file-name">${r.path}</span>
                              <span class="file-meta">${this.formatFileSize(r.size)}</span>
                            </div>
                            <div class="file-status ${r.status}">
                              ${r.status === 'done'
                                ? html`<sl-icon name="check-circle"></sl-icon>`
                                : nothing}
                              ${r.status === 'uploading'
                                ? html`<sl-spinner style="font-size: 1rem;"></sl-spinner>`
                                : nothing}
                              ${r.status === 'failed'
                                ? html`<sl-icon name="x-circle"></sl-icon>`
                                : nothing}
                              ${r.status === 'pending'
                                ? html`<sl-icon name="clock"></sl-icon>`
                                : nothing}
                            </div>
                          </li>
                        `
                      )}
                    </ul>
                    ${this.flowState === 'uploading'
                      ? html`
                          <sl-progress-bar
                            value=${pct}
                            style="margin-top: 0.75rem;"
                          ></sl-progress-bar>
                        `
                      : nothing}
                  `
                : nothing}
              ${this.flowState === 'error'
                ? html`
                    <div class="error-banner" style="margin-top: 1rem;">
                      <sl-icon name="exclamation-triangle"></sl-icon>
                      <span>${this.error}</span>
                    </div>
                  `
                : nothing}
            `}
        ${this.flowState === 'error'
          ? html`
              <div class="progress-actions">
                <sl-button variant="primary" @click=${() => this.retryPublish()}>
                  <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                  ${this.uploadResults.some((r) => r.status === 'failed')
                    ? 'Retry Failed'
                    : 'Retry Publishing'}
                </sl-button>
                ${this.createdSkillId
                  ? html`
                      <a href="/skills/${this.createdSkillId}" style="text-decoration: none;">
                        <sl-button variant="default">
                          Go to Skill
                          <sl-icon slot="suffix" name="arrow-right"></sl-icon>
                        </sl-button>
                      </a>
                    `
                  : nothing}
              </div>
            `
          : nothing}
      </div>
    `;
  }

  private getErrorStep(): FlowState {
    if (this.uploadResults.some((r) => r.status === 'failed')) return 'uploading';
    if (this.uploadResults.length > 0 && this.uploadResults.every((r) => r.status === 'done'))
      return 'finalizing';
    return 'uploading';
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-skill-create': ScionPageSkillCreate;
  }
}
