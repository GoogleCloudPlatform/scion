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
 * Shared File Browser Component
 *
 * Reusable file browser that displays a sortable file table with preview,
 * download, delete, upload, and "New File" actions. Consumed by the project
 * detail page (workspace & shared-dir tabs) and the template editor.
 *
 * Data access is abstracted behind the FileBrowserDataSource interface,
 * allowing different backends (workspace files, shared-dir files, template
 * files) to be plugged in without changing the UI component.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch, extractApiError } from '../../client/api.js';

// ────────────────────────────────────────────────────────────
// Types
// ────────────────────────────────────────────────────────────

/** A single file entry returned by the list endpoint. */
export interface FileEntry {
  path: string;
  size: number;
  modTime: string;
  mode: string;
}

/** Metadata returned alongside a file listing. */
export interface FileListResult {
  files: FileEntry[];
  totalSize: number;
  totalCount: number;
  hasMore?: boolean;
  providerCount?: number;
}

// ────────────────────────────────────────────────────────────
// Data Source Interface
// ────────────────────────────────────────────────────────────

/**
 * Adapter interface for pluggable data backends.
 *
 * Each implementation maps file operations to the appropriate API paths.
 */
export interface FileBrowserDataSource {
  /** List files. When limit is provided the backend returns the most recently modified files up to that count. */
  listFiles(opts?: { limit?: number }): Promise<FileListResult>;

  /**
   * Search files by query string (regex or fuzzy fallback). Optional: data sources that
   * operate on small in-memory sets (e.g. templates) may omit this; the component
   * degrades gracefully and relies on local filtering only.
   */
  searchFiles?(query: string, limit?: number, signal?: AbortSignal): Promise<FileListResult>;

  /** Delete a file by path. */
  deleteFile(path: string): Promise<void>;

  /** Upload one or more files. */
  uploadFiles(files: FileList): Promise<void>;

  /** Get the download URL for a file (opened in a new tab). */
  getDownloadUrl(path: string): string;

  /** Get the preview URL for a file (opened in a new tab with ?view=true). */
  getPreviewUrl(path: string): string;

  /** Get the archive download URL, if supported. Returns null if unsupported. */
  getArchiveUrl?(): string | null;
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

/**
 * Data source for workspace files.
 * API base: /api/v1/projects/{projectId}/workspace/files
 */
export class WorkspaceFileBrowserDataSource implements FileBrowserDataSource {
  private readonly basePath: string;
  private readonly projectId: string;

  constructor(projectId: string) {
    this.projectId = projectId;
    this.basePath = `/api/v1/projects/${projectId}/workspace/files`;
  }

  async listFiles(opts?: { limit?: number }): Promise<FileListResult> {
    const params = new URLSearchParams();
    if (opts?.limit !== undefined) params.set('limit', String(opts.limit));
    const url = params.size ? `${this.basePath}?${params}` : this.basePath;
    const response = await apiFetch(url);
    if (!response.ok) {
      throw new Error(await extractApiError(response, `HTTP ${response.status}`));
    }
    return (await response.json()) as FileListResult;
  }

  async searchFiles(query: string, limit?: number, signal?: AbortSignal): Promise<FileListResult> {
    const params = new URLSearchParams({ q: query });
    if (limit !== undefined) params.set('limit', String(limit));
    const response = await apiFetch(`${this.basePath}?${params}`, { signal: signal ?? null });
    if (!response.ok) {
      throw new Error(await extractApiError(response, `HTTP ${response.status}`));
    }
    return (await response.json()) as FileListResult;
  }

  async deleteFile(path: string): Promise<void> {
    const response = await apiFetch(`${this.basePath}/${encodeFilePath(path)}`, {
      method: 'DELETE',
    });
    if (!response.ok && response.status !== 204) {
      throw new Error(await extractApiError(response, `Delete failed: HTTP ${response.status}`));
    }
  }

  async uploadFiles(files: FileList): Promise<void> {
    const formData = new FormData();
    for (let i = 0; i < files.length; i++) {
      formData.append(files[i].name, files[i]);
    }
    const response = await apiFetch(this.basePath, {
      method: 'POST',
      body: formData,
    });
    if (!response.ok) {
      throw new Error(await extractApiError(response, `Upload failed: HTTP ${response.status}`));
    }
  }

  getDownloadUrl(path: string): string {
    return `${this.basePath}/${encodeFilePath(path)}`;
  }

  getPreviewUrl(path: string): string {
    return `${this.basePath}/${encodeFilePath(path)}?view=true`;
  }

  getArchiveUrl(): string | null {
    return `/api/v1/projects/${this.projectId}/workspace/archive`;
  }
}

/**
 * Data source for shared directory files.
 * API base: /api/v1/projects/{projectId}/shared-dirs/{dirName}/files
 */
export class SharedDirFileBrowserDataSource implements FileBrowserDataSource {
  private readonly basePath: string;

  constructor(projectId: string, dirName: string) {
    this.basePath = `/api/v1/projects/${projectId}/shared-dirs/${encodeURIComponent(dirName)}/files`;
  }

  async listFiles(opts?: { limit?: number }): Promise<FileListResult> {
    const params = new URLSearchParams();
    if (opts?.limit !== undefined) params.set('limit', String(opts.limit));
    const url = params.size ? `${this.basePath}?${params}` : this.basePath;
    const response = await apiFetch(url);
    if (!response.ok) {
      throw new Error(await extractApiError(response, `HTTP ${response.status}`));
    }
    return (await response.json()) as FileListResult;
  }

  async searchFiles(query: string, limit?: number, signal?: AbortSignal): Promise<FileListResult> {
    const params = new URLSearchParams({ q: query });
    if (limit !== undefined) params.set('limit', String(limit));
    const response = await apiFetch(`${this.basePath}?${params}`, { signal: signal ?? null });
    if (!response.ok) {
      throw new Error(await extractApiError(response, `HTTP ${response.status}`));
    }
    return (await response.json()) as FileListResult;
  }

  async deleteFile(path: string): Promise<void> {
    const response = await apiFetch(`${this.basePath}/${encodeFilePath(path)}`, {
      method: 'DELETE',
    });
    if (!response.ok && response.status !== 204) {
      throw new Error(await extractApiError(response, `Delete failed: HTTP ${response.status}`));
    }
  }

  async uploadFiles(files: FileList): Promise<void> {
    const formData = new FormData();
    for (let i = 0; i < files.length; i++) {
      formData.append(files[i].name, files[i]);
    }
    const response = await apiFetch(this.basePath, {
      method: 'POST',
      body: formData,
    });
    if (!response.ok) {
      throw new Error(await extractApiError(response, `Upload failed: HTTP ${response.status}`));
    }
  }

  getDownloadUrl(path: string): string {
    return `${this.basePath}/${encodeFilePath(path)}`;
  }

  getPreviewUrl(path: string): string {
    return `${this.basePath}/${encodeFilePath(path)}?view=true`;
  }

  getArchiveUrl(): string | null {
    return null;
  }
}

/**
 * Data source for template files.
 * API base: /api/v1/templates/{templateId}/files
 */
export class TemplateFileBrowserDataSource implements FileBrowserDataSource {
  private readonly basePath: string;

  constructor(templateId: string) {
    this.basePath = `/api/v1/templates/${templateId}/files`;
  }

  // Template file sets are small; always load all files and rely on local filtering only.
  async listFiles(): Promise<FileListResult> {
    const response = await apiFetch(this.basePath);
    if (!response.ok) {
      throw new Error(await extractApiError(response, `HTTP ${response.status}`));
    }
    return (await response.json()) as FileListResult;
  }

  async deleteFile(path: string): Promise<void> {
    const response = await apiFetch(`${this.basePath}/${encodeFilePath(path)}`, {
      method: 'DELETE',
    });
    if (!response.ok && response.status !== 204) {
      throw new Error(await extractApiError(response, `Delete failed: HTTP ${response.status}`));
    }
  }

  async uploadFiles(files: FileList): Promise<void> {
    const formData = new FormData();
    for (let i = 0; i < files.length; i++) {
      formData.append(files[i].name, files[i]);
    }
    const response = await apiFetch(this.basePath, {
      method: 'POST',
      body: formData,
    });
    if (!response.ok) {
      throw new Error(await extractApiError(response, `Upload failed: HTTP ${response.status}`));
    }
  }

  getDownloadUrl(path: string): string {
    return `${this.basePath}/${encodeFilePath(path)}`;
  }

  getPreviewUrl(path: string): string {
    return `${this.basePath}/${encodeFilePath(path)}?view=true`;
  }

  getArchiveUrl(): string | null {
    return null;
  }
}

// ────────────────────────────────────────────────────────────
// Component
// ────────────────────────────────────────────────────────────

const PREVIEWABLE_EXTENSIONS = new Set([
  // Images
  '.png', '.jpg', '.jpeg', '.gif', '.svg', '.webp', '.bmp', '.ico',
  // Text
  '.txt', '.log', '.csv', '.tsv',
  // Markdown
  '.md',
  // Code
  '.js', '.ts', '.jsx', '.tsx', '.mjs', '.cjs',
  '.py', '.go', '.rs', '.java', '.c', '.cpp', '.h', '.hpp', '.cs',
  '.css', '.scss', '.less', '.html', '.htm', '.xml', '.xsl',
  '.json', '.yaml', '.yml', '.toml', '.ini', '.cfg', '.conf',
  '.sh', '.bash', '.zsh', '.fish',
  '.sql', '.rb', '.php', '.swift', '.kt', '.scala', '.r', '.lua',
  '.pl', '.ex', '.exs', '.elm', '.hs', '.clj', '.vim',
  '.dockerfile', '.makefile', '.env', '.gitignore', '.editorconfig',
  // PDF
  '.pdf',
]);

/** Extensions that can be opened in the inline code editor. */
const EDITABLE_EXTENSIONS = new Set([
  // Markdown
  '.md',
  // Data formats
  '.json', '.yaml', '.yml', '.toml',
  // Shell
  '.sh', '.bash', '.zsh',
  // Programming languages
  '.go', '.ts', '.tsx', '.js', '.jsx', '.mjs', '.cjs',
  '.py', '.rs', '.java', '.c', '.cpp', '.h', '.hpp', '.cs',
  '.rb', '.php', '.swift', '.kt', '.scala', '.r', '.lua',
  '.pl', '.ex', '.exs', '.elm', '.hs', '.clj',
  // Web
  '.css', '.scss', '.less', '.html', '.htm', '.xml', '.xsl',
  // Config & text
  '.txt', '.log', '.csv', '.tsv', '.ini', '.cfg', '.conf',
  '.env', '.gitignore', '.editorconfig',
  '.dockerfile', '.makefile',
  // SQL
  '.sql',
]);

/** Maximum file size that can be opened in the editor (1MB). */
const MAX_EDITABLE_FILE_SIZE = 1 * 1024 * 1024;

function isEditable(filePath: string, fileSize: number): boolean {
  if (fileSize > MAX_EDITABLE_FILE_SIZE) return false;
  const ext = filePath.includes('.') ? '.' + filePath.split('.').pop()!.toLowerCase() : '';
  return EDITABLE_EXTENSIONS.has(ext);
}

function isMarkdownFile(filePath: string): boolean {
  return filePath.toLowerCase().endsWith('.md');
}

function isPreviewable(filePath: string): boolean {
  const ext = filePath.includes('.') ? '.' + filePath.split('.').pop()!.toLowerCase() : '';
  return PREVIEWABLE_EXTENSIONS.has(ext);
}

function isDotFile(path: string): boolean {
  return path.split('/').some((segment) => segment.startsWith('.'));
}

function formatFileSize(bytes: number): string {
  if (bytes === 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB'];
  const i = Math.floor(Math.log(bytes) / Math.log(1024));
  const size = bytes / Math.pow(1024, i);
  return `${size.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

@customElement('scion-file-browser')
export class ScionFileBrowser extends LitElement {
  /** Data source adapter — must be set by the parent. */
  @property({ attribute: false })
  dataSource: FileBrowserDataSource | null = null;

  /** Whether the user has update (write) permission. Controls upload/delete/new-file visibility. */
  @property({ type: Boolean })
  editable = false;

  /** Show the archive download button. */
  @property({ type: Boolean })
  showArchive = false;

  // ── Internal state ──

  @state() private files: FileEntry[] = [];
  @state() private loading = false;
  @state() private error: string | null = null;
  @state() private totalSize = 0;
  @state() private totalCount = 0;
  @state() private initialHasMore = false;
  @state() private providerCount = 0;
  @state() private uploadProgress = false;
  @state() private sortField: 'name' | 'size' | 'modified' = 'name';
  @state() private sortDir: 'asc' | 'desc' = 'asc';
  @state() private filterText = '';
  @state() private showDotFiles = false;

  // Backend search state
  @state() private backendResults: FileEntry[] = [];
  @state() private backendSearching = false;
  @state() private backendError = false;
  @state() private backendHasMore = false;

  private readonly initialLimit = 500;
  private _searchAbortController: AbortController | null = null;
  private _searchDebounceTimer: ReturnType<typeof setTimeout> | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .toolbar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 0.75rem;
    }

    .toolbar-left {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .toolbar-meta {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
    }

    .filter-input {
      width: 14rem;
    }

    .dot-files-toggle {
      font-size: 0.8125rem;
      white-space: nowrap;
    }

    .toolbar-actions {
      display: flex;
      gap: 0.5rem;
      align-items: center;
    }

    .provider-warning {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      font-size: 0.75rem;
      color: var(--sl-color-warning-700, #a16207);
      background: var(--sl-color-warning-50, #fefce8);
      border: 1px solid var(--sl-color-warning-200, #fde68a);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.25rem 0.625rem;
    }

    .provider-warning sl-icon {
      font-size: 0.875rem;
    }

    .file-table-wrapper {
      max-height: 26rem;
      overflow-y: auto;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .file-table {
      width: 100%;
      border-collapse: collapse;
      background: var(--scion-surface, #ffffff);
    }

    .file-table th,
    .file-table td {
      padding: 0.625rem 1rem;
      text-align: left;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .file-table th {
      font-size: 0.75rem;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f8fafc);
      font-weight: 600;
      position: sticky;
      top: 0;
      z-index: 1;
    }

    .file-table th.sortable {
      cursor: pointer;
      user-select: none;
    }

    .file-table th.sortable:hover {
      color: var(--scion-text, #1e293b);
    }

    .file-table .sort-indicator {
      display: inline-block;
      margin-left: 0.25rem;
      font-size: 0.625rem;
      vertical-align: middle;
      opacity: 0.4;
    }

    .file-table th.sorted .sort-indicator {
      opacity: 1;
    }

    .file-table tr:last-child td {
      border-bottom: none;
    }

    .empty-filter {
      padding: 1.5rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      text-align: center;
    }

    .file-list-truncated {
      padding: 0.5rem 1rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      text-align: center;
      background: var(--scion-bg-subtle, #f8fafc);
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .file-name {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .file-name sl-icon {
      color: var(--scion-text-muted, #64748b);
      flex-shrink: 0;
    }

    .file-size,
    .file-date {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .file-actions {
      text-align: right;
      white-space: nowrap;
    }

    .file-actions .preview-disabled {
      opacity: 0.3;
      cursor: not-allowed;
    }

    .empty-state {
      text-align: center;
      padding: 2.5rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px dashed var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .empty-state > sl-icon {
      font-size: 2.5rem;
      color: var(--scion-text-muted, #64748b);
      opacity: 0.5;
      margin-bottom: 0.75rem;
    }

    .empty-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
      font-size: 0.875rem;
    }

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 2rem;
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
    }

    .search-spinner {
      font-size: 0.75rem;
      vertical-align: middle;
      margin: 0 0.25rem;
    }

    .search-error-icon {
      color: var(--sl-color-warning-700, #a16207);
      vertical-align: middle;
      font-size: 0.875rem;
      margin-left: 0.25rem;
      cursor: default;
    }

    @media (max-width: 768px) {
      .hide-mobile {
        display: none;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.dataSource) {
      void this.loadFiles();
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this._cancelSearch();
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('dataSource') && this.dataSource) {
      void this.loadFiles();
    }
  }

  /** Public method to trigger a file list reload. */
  async loadFiles(): Promise<void> {
    if (!this.dataSource) return;
    this.loading = true;
    this.error = null;

    try {
      const result = await this.dataSource.listFiles({ limit: this.initialLimit });
      this.files = result.files || [];
      this.totalSize = result.totalSize || 0;
      this.totalCount = result.totalCount || 0;
      this.initialHasMore = result.hasMore ?? false;
      this.providerCount = result.providerCount ?? 0;
      // Clear backend search state on reload
      this.backendResults = [];
      this.backendError = false;
      this.backendHasMore = false;
    } catch (err) {
      console.error('Failed to load files:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load files';
    } finally {
      this.loading = false;
    }
  }

  // ── Backend search ──

  private _cancelSearch(): void {
    if (this._searchAbortController) {
      this._searchAbortController.abort();
      this._searchAbortController = null;
    }
    if (this._searchDebounceTimer !== null) {
      clearTimeout(this._searchDebounceTimer);
      this._searchDebounceTimer = null;
    }
  }

  private handleFilterInput(value: string): void {
    this.filterText = value;
    this.backendError = false;

    if (!value) {
      this.backendResults = [];
      this.backendHasMore = false;
      this.backendSearching = false;
      this._cancelSearch();
      return;
    }

    if (!this.dataSource?.searchFiles) return;

    if (this._searchDebounceTimer !== null) {
      clearTimeout(this._searchDebounceTimer);
    }
    this._searchDebounceTimer = setTimeout(() => {
      this._searchDebounceTimer = null;
      void this._runBackendSearch(value);
    }, 250);
  }

  private async _runBackendSearch(query: string): Promise<void> {
    if (!this.dataSource?.searchFiles) return;

    if (this._searchAbortController) {
      this._searchAbortController.abort();
    }
    this._searchAbortController = new AbortController();
    const { signal } = this._searchAbortController;

    this.backendSearching = true;
    try {
      const result = await this.dataSource.searchFiles(query, this.initialLimit, signal);
      if (signal.aborted) return;
      this.backendResults = result.files || [];
      this.backendHasMore = result.hasMore ?? false;
      this.backendError = false;
    } catch (err) {
      if (err instanceof Error && err.name === 'AbortError') return;
      console.warn('Backend search failed:', err);
      this.backendError = true;
      this.backendResults = [];
    } finally {
      if (!signal.aborted) {
        this.backendSearching = false;
      }
    }
  }

  private getMergedFiles(): FileEntry[] {
    if (!this.filterText) return this.getBaseFiles();

    const base = this.getBaseFiles();
    const localMatches = this.applyFilter(base);

    if (this.backendResults.length === 0) return localMatches;

    const filteredBackend = this.showDotFiles
      ? this.backendResults
      : this.backendResults.filter((f) => !isDotFile(f.path));

    const seen = new Set(localMatches.map((f) => f.path));
    const merged = [...localMatches];
    for (const f of filteredBackend) {
      if (!seen.has(f.path)) {
        merged.push(f);
        seen.add(f.path);
      }
    }
    return merged;
  }

  // ── Sorting ──

  private toggleSort(field: 'name' | 'size' | 'modified'): void {
    if (this.sortField === field) {
      this.sortDir = this.sortDir === 'asc' ? 'desc' : 'asc';
    } else {
      this.sortField = field;
      this.sortDir = field === 'name' ? 'asc' : 'desc';
    }
  }

  private sortIndicator(field: 'name' | 'size' | 'modified'): string {
    return this.sortField === field ? (this.sortDir === 'asc' ? '▲' : '▼') : '▲';
  }

  private getBaseFiles(): FileEntry[] {
    if (this.showDotFiles) return this.files;
    return this.files.filter((f) => !isDotFile(f.path));
  }

  private getFilteredAndSortedFiles(): FileEntry[] {
    const filtered = this.getMergedFiles();
    return [...filtered].sort((a, b) => {
      let cmp = 0;
      switch (this.sortField) {
        case 'name':
          cmp = a.path.localeCompare(b.path);
          break;
        case 'size':
          cmp = a.size - b.size;
          break;
        case 'modified': {
          const aTime = a.modTime ? new Date(a.modTime).getTime() : 0;
          const bTime = b.modTime ? new Date(b.modTime).getTime() : 0;
          cmp = aTime - bTime;
          break;
        }
      }
      return this.sortDir === 'asc' ? cmp : -cmp;
    });
  }

  private applyFilter(files: FileEntry[]): FileEntry[] {
    const pattern = this.filterText;

    // Try to use the input as a regex first (allows e.g. \.tsx?$ or ^src/)
    try {
      const re = new RegExp(pattern, 'i');
      return files.filter((f) => re.test(f.path));
    } catch {
      // Not valid regex — fall back to fuzzy match
    }

    // Fuzzy match: every character in the pattern must appear in order (case-insensitive)
    const lower = pattern.toLowerCase();
    return files.filter((f) => {
      const name = f.path.toLowerCase();
      let ni = 0;
      for (let pi = 0; pi < lower.length; pi++) {
        ni = name.indexOf(lower[pi], ni);
        if (ni === -1) return false;
        ni++;
      }
      return true;
    });
  }

  // ── Actions ──

  private handleUploadClick(): void {
    const input = this.shadowRoot?.querySelector('#file-browser-input') as HTMLInputElement;
    if (input) input.click();
  }

  private async handleFileUpload(e: Event): Promise<void> {
    const input = e.target as HTMLInputElement;
    const fileList = input.files;
    if (!fileList || fileList.length === 0 || !this.dataSource) return;

    this.uploadProgress = true;
    try {
      await this.dataSource.uploadFiles(fileList);
      void this.loadFiles();
    } catch (err) {
      console.error('Failed to upload files:', err);
      this.error = err instanceof Error ? err.message : 'Upload failed';
    } finally {
      this.uploadProgress = false;
      input.value = '';
    }
  }

  private async handleDelete(filePath: string, event?: MouseEvent): Promise<void> {
    if (!this.dataSource) return;
    if (!event?.altKey && !confirm(`Delete ${filePath}?`)) return;

    try {
      await this.dataSource.deleteFile(filePath);
      this.files = this.files.filter(f => f.path !== filePath);
      void this.loadFiles();
    } catch (err) {
      console.error('Failed to delete file:', err);
      this.error = err instanceof Error ? err.message : 'Delete failed';
    }
  }

  private handlePreview(filePath: string): void {
    if (!this.dataSource) return;
    // Markdown files get inline preview via the editor component
    if (isMarkdownFile(filePath)) {
      this.dispatchEvent(
        new CustomEvent('file-preview-requested', {
          detail: { path: filePath },
          bubbles: true,
          composed: true,
        })
      );
      return;
    }
    window.open(this.dataSource.getPreviewUrl(filePath), '_blank');
  }

  private handleDownload(filePath: string): void {
    if (!this.dataSource) return;
    window.open(this.dataSource.getDownloadUrl(filePath), '_blank');
  }

  private handleArchiveDownload(): void {
    const url = this.dataSource?.getArchiveUrl?.();
    if (url) window.open(url, '_blank');
  }

  private handleEdit(filePath: string): void {
    this.dispatchEvent(
      new CustomEvent('file-edit-requested', {
        detail: { path: filePath },
        bubbles: true,
        composed: true,
      })
    );
  }

  private handleNewFile(): void {
    this.dispatchEvent(new CustomEvent('file-create-requested', { bubbles: true, composed: true }));
  }

  private formatDate(dateString: string): string {
    try {
      const date = new Date(dateString);
      return new Intl.DateTimeFormat('en', {
        month: 'short',
        day: 'numeric',
        year: 'numeric',
        hour: '2-digit',
        minute: '2-digit',
      }).format(date);
    } catch {
      return dateString;
    }
  }

  // ── Render ──

  override render() {
    return html`
      ${this.renderToolbar()}
      ${this.renderContent()}
    `;
  }

  private renderToolbar() {
    const base = this.getBaseFiles();
    const hasDotFiles = this.files.some((f) => isDotFile(f.path));

    let countLabel;
    if (this.filterText) {
      const localMatches = this.applyFilter(base);
      const localCount = localMatches.length;
      if (this.backendSearching) {
        countLabel = html`${localCount} local match${localCount !== 1 ? 'es' : ''} · <sl-spinner class="search-spinner"></sl-spinner> searching workspace…`;
      } else if (this.backendError) {
        countLabel = html`${localCount} local match${localCount !== 1 ? 'es' : ''}
          <sl-tooltip content="Workspace search unavailable — showing local results only">
            <sl-icon name="exclamation-triangle" class="search-error-icon"></sl-icon>
          </sl-tooltip>`;
      } else {
        const mergedCount = this.getMergedFiles().length;
        countLabel = this.backendHasMore
          ? `${mergedCount} match${mergedCount !== 1 ? 'es' : ''} (more available — refine your filter)`
          : `${mergedCount} match${mergedCount !== 1 ? 'es' : ''}`;
      }
    } else if (this.initialHasMore && this.totalCount > this.files.length) {
      const sizeStr = this.totalSize > 0 ? ` (${formatFileSize(this.totalSize)})` : '';
      countLabel = `${this.files.length.toLocaleString()} of ${this.totalCount.toLocaleString()} files${sizeStr} · most recent`;
    } else {
      const n = base.length;
      const sizeStr = this.totalSize > 0 ? ` (${formatFileSize(this.totalSize)})` : '';
      countLabel = `${n} file${n !== 1 ? 's' : ''}${sizeStr}`;
    }

    return html`
      <div class="toolbar">
        <div class="toolbar-left">
          <span class="toolbar-meta">${countLabel}</span>
          ${this.files.length > 0
            ? html`
                <sl-input
                  class="filter-input"
                  size="small"
                  placeholder="Filter files…"
                  clearable
                  .value=${this.filterText}
                  @sl-input=${(e: Event) => {
                    this.handleFilterInput((e.target as HTMLInputElement).value);
                  }}
                  @sl-clear=${() => {
                    this.handleFilterInput('');
                  }}
                >
                  <sl-icon name="funnel" slot="prefix"></sl-icon>
                </sl-input>
              `
            : nothing}
          ${hasDotFiles
            ? html`
                <sl-checkbox
                  class="dot-files-toggle"
                  size="small"
                  ?checked=${this.showDotFiles}
                  @sl-change=${(e: Event) => {
                    this.showDotFiles = (e.target as HTMLInputElement).checked;
                  }}
                >show .dot files</sl-checkbox>
              `
            : nothing}
        </div>
        <div class="toolbar-actions">
          ${this.providerCount > 1
            ? html`
                <div class="provider-warning">
                  <sl-icon name="exclamation-triangle"></sl-icon>
                  Showing files from this server only — ${this.providerCount} brokers serve this project
                </div>
              `
            : nothing}
          <sl-icon-button
            name="arrow-clockwise"
            label="Refresh file list"
            ?disabled=${this.loading}
            @click=${() => this.loadFiles()}
          ></sl-icon-button>
          ${this.showArchive && this.dataSource?.getArchiveUrl?.() && this.files.length > 0
            ? html`
                <sl-button size="small" variant="default" @click=${() => this.handleArchiveDownload()}>
                  <sl-icon slot="prefix" name="file-earmark-zip"></sl-icon>
                  Download Zip
                </sl-button>
              `
            : nothing}
          ${this.editable
            ? html`
                <sl-button
                  size="small"
                  variant="default"
                  @click=${() => this.handleNewFile()}
                >
                  <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                  New File
                </sl-button>
                <input
                  type="file"
                  id="file-browser-input"
                  multiple
                  style="display: none"
                  @change=${this.handleFileUpload}
                />
                <sl-button
                  size="small"
                  variant="default"
                  ?loading=${this.uploadProgress}
                  ?disabled=${this.uploadProgress}
                  @click=${() => this.handleUploadClick()}
                >
                  <sl-icon slot="prefix" name="upload"></sl-icon>
                  Upload Files
                </sl-button>
              `
            : nothing}
        </div>
      </div>
    `;
  }

  private renderContent() {
    if (this.error) {
      return html`<div class="error-state">${this.error}</div>`;
    }
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading files...</p>
        </div>
      `;
    }
    if (this.files.length === 0 && !this.filterText) {
      return html`
        <div class="empty-state">
          <sl-icon name="file-earmark"></sl-icon>
          <p>
            No files in this directory.${this.editable ? ' Upload files to get started.' : ''}
          </p>
          ${this.editable
            ? html`
                <sl-button
                  size="small"
                  variant="primary"
                  ?loading=${this.uploadProgress}
                  ?disabled=${this.uploadProgress}
                  @click=${() => this.handleUploadClick()}
                >
                  <sl-icon slot="prefix" name="upload"></sl-icon>
                  Upload Files
                </sl-button>
              `
            : nothing}
        </div>
      `;
    }
    return this.renderTable();
  }

  private renderTable() {
    const displayFiles = this.getFilteredAndSortedFiles();
    return html`
      <div class="file-table-wrapper">
        ${this.filterText && displayFiles.length === 0 && !this.backendSearching
          ? html`<div class="empty-filter">No files match the current filter.</div>`
          : html`
        <table class="file-table">
          <thead>
            <tr>
              <th
                class="sortable ${this.sortField === 'name' ? 'sorted' : ''}"
                @click=${() => this.toggleSort('name')}
              >
                <span class="sort-indicator">${this.sortIndicator('name')}</span>
                Name
              </th>
              <th
                class="sortable ${this.sortField === 'size' ? 'sorted' : ''}"
                @click=${() => this.toggleSort('size')}
              >
                <span class="sort-indicator">${this.sortIndicator('size')}</span>
                Size
              </th>
              <th
                class="sortable hide-mobile ${this.sortField === 'modified' ? 'sorted' : ''}"
                @click=${() => this.toggleSort('modified')}
              >
                <span class="sort-indicator">${this.sortIndicator('modified')}</span>
                Modified
              </th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            ${displayFiles.slice(0, 1000).map(
              (file) => html`
                <tr>
                  <td>
                    <span class="file-name">
                      <sl-icon name="file-earmark"></sl-icon>
                      ${file.path}
                    </span>
                  </td>
                  <td><span class="file-size">${formatFileSize(file.size)}</span></td>
                  <td class="hide-mobile">
                    <span class="file-date">${this.formatDate(file.modTime)}</span>
                  </td>
                  <td class="file-actions">
                    ${isPreviewable(file.path)
                      ? html`
                          <sl-icon-button
                            name="eye"
                            label="Preview ${file.path}"
                            @click=${() => this.handlePreview(file.path)}
                          ></sl-icon-button>
                        `
                      : html`
                          <sl-icon-button
                            name="eye"
                            label="Preview not available for this format"
                            class="preview-disabled"
                            disabled
                          ></sl-icon-button>
                        `}
                    ${this.editable && isEditable(file.path, file.size)
                      ? html`
                          <sl-icon-button
                            name="pencil"
                            label="Edit ${file.path}"
                            @click=${() => this.handleEdit(file.path)}
                          ></sl-icon-button>
                        `
                      : nothing}
                    <sl-icon-button
                      name="download"
                      label="Download ${file.path}"
                      @click=${() => this.handleDownload(file.path)}
                    ></sl-icon-button>
                    ${this.editable
                      ? html`
                          <sl-icon-button
                            name="trash"
                            label="Delete ${file.path}"
                            @click=${(e: MouseEvent) => this.handleDelete(file.path, e)}
                          ></sl-icon-button>
                        `
                      : nothing}
                  </td>
                </tr>
              `
            )}
          </tbody>
        </table>
        ${displayFiles.length > 1000
          ? html`<div class="file-list-truncated">
              File list truncated — showing 1,000 of ${displayFiles.length.toLocaleString()} files
            </div>`
          : nothing}
        `}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-file-browser': ScionFileBrowser;
  }
}
