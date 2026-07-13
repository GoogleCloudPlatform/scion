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
 * Skill detail page component
 *
 * Tabbed layout with Overview, Versions, and Files tabs.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Skill, SkillVersion, SkillDownloadUrl } from '../../shared/types.js';
import { can } from '../../shared/types.js';
import type { StatusType } from '../shared/status-badge.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import '../shared/status-badge.js';
import '../shared/hash-display.js';
import '../shared/skill-publish-dialog.js';

@customElement('scion-page-skill-detail')
export class ScionPageSkillDetail extends LitElement {
  @property({ type: Object }) pageData: PageData | null = null;
  @property({ type: String }) skillId = '';

  @state() private loading = true;
  @state() private skill: Skill | null = null;
  @state() private versions: SkillVersion[] = [];
  @state() private error: string | null = null;
  @state() private editing = false;
  @state() private editForm: Partial<{ name: string; description: string; visibility: string; tags: string }> = {};
  @state() private saving = false;
  @state() private actionLoading: Record<string, boolean> = {};

  // Publish dialog state
  @state() private publishDialogOpen = false;

  // Deprecate dialog state
  @state() private deprecateVersionId: string | null = null;
  @state() private deprecateMessage = '';
  @state() private deprecateReplacement = '';
  @state() private deprecateLoading = false;

  // Files tab state
  @state() private selectedVersionForFiles = '';
  @state() private fileUrls: SkillDownloadUrl[] = [];
  @state() private filesLoading = false;

  static override styles = css`
    :host { display: block; }

    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }
    .back-link:hover { color: var(--scion-primary, #3b82f6); }

    .header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 1.5rem;
      gap: 1rem;
    }
    .header-info { flex: 1; }
    .header-title {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 0.5rem;
    }
    .header-title sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }
    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }
    .header-meta {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-top: 0.5rem;
    }
    .header-actions {
      display: flex;
      gap: 0.5rem;
      flex-shrink: 0;
    }

    sl-tab-group { --track-color: var(--scion-border, #e2e8f0); }
    sl-tab-group::part(body) { padding-top: 1.5rem; }

    .card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }
    .card-title-row {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1rem;
      padding-bottom: 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }
    .card-title {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .info-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
      gap: 1.5rem;
    }
    .info-item { display: flex; flex-direction: column; }
    .info-label {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
      margin-bottom: 0.25rem;
    }
    .info-value {
      font-size: 1rem;
      color: var(--scion-text, #1e293b);
    }
    .info-value.mono {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
    }

    .tag-list {
      display: flex;
      flex-wrap: wrap;
      gap: 0.375rem;
    }
    .tag-item {
      display: inline-block;
      font-size: 0.75rem;
      padding: 0.125rem 0.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 9999px;
      color: var(--scion-text, #1e293b);
    }

    .scope-badge, .visibility-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.8125rem;
      font-weight: 500;
    }
    .scope-badge {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }
    .visibility-badge.public {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }
    .visibility-badge.private {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    .uri-copy {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      padding: 0.25rem 0.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
      cursor: pointer;
    }
    .uri-copy:hover {
      background: var(--scion-border, #e2e8f0);
    }
    .uri-copy sl-icon {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .edit-field { margin-bottom: 1rem; }
    .edit-field label {
      display: block;
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
      margin-bottom: 0.375rem;
    }
    .edit-actions {
      display: flex;
      gap: 0.5rem;
      margin-top: 1rem;
    }

    .version-table {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }
    .version-table table { width: 100%; border-collapse: collapse; }
    .version-table th {
      text-align: left;
      padding: 0.75rem 1rem;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }
    .version-table td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }
    .version-table tr:last-child td { border-bottom: none; }
    .version-table .actions-cell { text-align: right; white-space: nowrap; }

    .file-table {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }
    .file-table table { width: 100%; border-collapse: collapse; }
    .file-table th {
      text-align: left;
      padding: 0.75rem 1rem;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }
    .file-table td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }
    .file-table tr:last-child td { border-bottom: none; }

    .version-selector {
      margin-bottom: 1rem;
    }

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 4rem 2rem;
      color: var(--scion-text-muted, #64748b);
    }
    .loading-state sl-spinner { font-size: 2rem; margin-bottom: 1rem; }

    .error-state {
      text-align: center;
      padding: 3rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }
    .error-state sl-icon {
      font-size: 3rem;
      color: var(--sl-color-danger-500, #ef4444);
      margin-bottom: 1rem;
    }
    .error-state h2 {
      font-size: 1.25rem; font-weight: 600;
      color: var(--scion-text, #1e293b); margin: 0 0 0.5rem 0;
    }
    .error-state p { color: var(--scion-text-muted, #64748b); margin: 0 0 1rem 0; }
    .error-details {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      color: var(--sl-color-danger-700, #b91c1c);
      margin-bottom: 1rem;
    }

    .empty-versions {
      text-align: center;
      padding: 2rem;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }
  `;

  private relativeTimeInterval: ReturnType<typeof setInterval> | null = null;

  override connectedCallback(): void {
    super.connectedCallback();
    if (!this.skillId && typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/skills\/([^/]+)/);
      if (match) this.skillId = match[1];
    }
    void this.loadData();
    this.relativeTimeInterval = setInterval(() => this.requestUpdate(), 15000);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.relativeTimeInterval) {
      clearInterval(this.relativeTimeInterval);
      this.relativeTimeInterval = null;
    }
  }

  private async loadData(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const ssrSkill = this.pageData?.data as Skill | undefined;
      if (ssrSkill && ssrSkill.id === this.skillId) {
        this.skill = ssrSkill;
      } else {
        const res = await apiFetch(`/api/v1/skills/${this.skillId}`);
        if (!res.ok) {
          throw new Error(await extractApiError(res, `HTTP ${res.status}: ${res.statusText}`));
        }
        this.skill = (await res.json()) as Skill;
      }

      const versionsRes = await apiFetch(`/api/v1/skills/${this.skillId}/versions`);
      if (versionsRes.ok) {
        const data = (await versionsRes.json()) as { items?: SkillVersion[]; versions?: SkillVersion[] } | SkillVersion[];
        if (Array.isArray(data)) {
          this.versions = data;
        } else {
          this.versions = data.items || data.versions || [];
        }
      }
    } catch (err) {
      console.error('Failed to load skill:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load skill';
    } finally {
      this.loading = false;
    }
  }

  private formatRelativeTime(dateString: string): string {
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return '—';
      const diffMs = Date.now() - date.getTime();
      if (diffMs < 0) return 'just now';
      const seconds = Math.floor(diffMs / 1000);
      if (seconds < 60) return 'just now';
      const minutes = Math.floor(seconds / 60);
      if (minutes < 60) return `${minutes}m ago`;
      const hours = Math.floor(minutes / 60);
      if (hours < 24) return `${hours}h ago`;
      const days = Math.floor(hours / 24);
      return `${days}d ago`;
    } catch { return dateString; }
  }

  private formatFileSize(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  }

  private versionStatusType(status: string): StatusType {
    switch (status) {
      case 'published': return 'success' as StatusType;
      case 'deprecated': return 'warning' as StatusType;
      case 'archived': return 'danger' as StatusType;
      default: return 'default' as StatusType;
    }
  }

  private getSkillUri(): string {
    if (!this.skill) return '';
    const scopePath = this.skill.scopeId
      ? `${this.skill.scope}/${this.skill.scopeId}`
      : this.skill.scope;
    return `skill://scion/${scopePath}/${this.skill.slug}@latest`;
  }

  private async copyUri(): Promise<void> {
    try {
      await navigator.clipboard.writeText(this.getSkillUri());
    } catch { /* ignore */ }
  }

  // -- Edit mode --

  private startEditing(): void {
    if (!this.skill) return;
    this.editForm = {
      name: this.skill.name,
      description: this.skill.description || '',
      visibility: this.skill.visibility,
      tags: this.skill.tags?.join(', ') || '',
    };
    this.editing = true;
  }

  private cancelEditing(): void {
    this.editing = false;
    this.editForm = {};
  }

  private async saveEdit(): Promise<void> {
    if (!this.skill) return;
    this.saving = true;

    try {
      const body: Record<string, unknown> = {};
      if (this.editForm.name !== undefined && this.editForm.name !== this.skill.name) {
        body.name = this.editForm.name;
      }
      if (this.editForm.description !== undefined) {
        body.description = this.editForm.description;
      }
      if (this.editForm.visibility !== undefined && this.editForm.visibility !== this.skill.visibility) {
        body.visibility = this.editForm.visibility;
      }
      if (this.editForm.tags !== undefined) {
        body.tags = this.editForm.tags.split(',').map((t) => t.trim()).filter((t) => t);
      }

      const res = await apiFetch(`/api/v1/skills/${this.skillId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!res.ok) {
        throw new Error(await extractApiError(res, 'Failed to update skill'));
      }

      const updated = (await res.json()) as Skill;
      if (!updated._capabilities && this.skill?._capabilities) {
        updated._capabilities = this.skill._capabilities;
      }
      this.skill = updated;
      this.editing = false;
    } catch (err) {
      console.error('Failed to save skill:', err);
      alert(err instanceof Error ? err.message : 'Failed to save skill');
    } finally {
      this.saving = false;
    }
  }

  // -- Archive --

  private async handleArchive(): Promise<void> {
    if (!confirm('Are you sure you want to archive this skill?')) return;
    this.actionLoading = { ...this.actionLoading, archive: true };

    try {
      const res = await apiFetch(`/api/v1/skills/${this.skillId}`, { method: 'DELETE' });
      if (!res.ok) {
        throw new Error(await extractApiError(res, 'Failed to archive skill'));
      }
      window.history.pushState({}, '', '/skills');
      window.dispatchEvent(new PopStateEvent('popstate'));
    } catch (err) {
      console.error('Failed to archive skill:', err);
      alert(err instanceof Error ? err.message : 'Failed to archive skill');
    } finally {
      this.actionLoading = { ...this.actionLoading, archive: false };
    }
  }

  // -- Deprecate --

  private async handleDeprecate(): Promise<void> {
    if (!this.deprecateVersionId || !this.deprecateMessage.trim()) return;
    this.deprecateLoading = true;

    try {
      const body: Record<string, unknown> = {
        message: this.deprecateMessage.trim(),
      };
      if (this.deprecateReplacement.trim()) {
        body.replacementUri = this.deprecateReplacement.trim();
      }

      const res = await apiFetch(
        `/api/v1/skills/${this.skillId}/versions/${this.deprecateVersionId}/deprecate`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        }
      );

      if (!res.ok) {
        throw new Error(await extractApiError(res, 'Failed to deprecate version'));
      }

      this.deprecateVersionId = null;
      this.deprecateMessage = '';
      this.deprecateReplacement = '';
      void this.loadData();
    } catch (err) {
      console.error('Failed to deprecate version:', err);
      alert(err instanceof Error ? err.message : 'Failed to deprecate version');
    } finally {
      this.deprecateLoading = false;
    }
  }

  // -- Files tab --

  private async loadFiles(version: string): Promise<void> {
    this.filesLoading = true;
    try {
      const res = await apiFetch(`/api/v1/skills/${this.skillId}/download?version=${encodeURIComponent(version)}`);
      if (res.ok) {
        const data = (await res.json()) as { files?: SkillDownloadUrl[]; urls?: SkillDownloadUrl[] } | SkillDownloadUrl[];
        if (Array.isArray(data)) {
          this.fileUrls = data;
        } else {
          this.fileUrls = data.files || data.urls || [];
        }
      } else {
        this.fileUrls = [];
      }
    } catch {
      this.fileUrls = [];
    } finally {
      this.filesLoading = false;
    }
  }

  private onFilesVersionChange(e: Event): void {
    const version = (e.target as HTMLElement & { value: string }).value;
    this.selectedVersionForFiles = version;
    if (version) void this.loadFiles(version);
  }

  private handleTabShow(e: CustomEvent<{ name: string }>): void {
    if (e.detail.name === 'files' && !this.selectedVersionForFiles) {
      const published = this.versions.filter((v) => v.status === 'published');
      if (published.length > 0) {
        this.selectedVersionForFiles = published[0].version;
        void this.loadFiles(published[0].version);
      }
    }
  }

  // -- Render --

  override render() {
    if (this.loading) return this.renderLoading();
    if (this.error || !this.skill) return this.renderError();

    return html`
      <a href="/skills" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Skills
      </a>

      ${this.renderHeader()}

      <sl-tab-group @sl-tab-show=${this.handleTabShow}>
        <sl-tab slot="nav" panel="overview">Overview</sl-tab>
        <sl-tab slot="nav" panel="versions">Versions</sl-tab>
        <sl-tab slot="nav" panel="files">Files</sl-tab>

        <sl-tab-panel name="overview">${this.renderOverviewTab()}</sl-tab-panel>
        <sl-tab-panel name="versions">${this.renderVersionsTab()}</sl-tab-panel>
        <sl-tab-panel name="files">${this.renderFilesTab()}</sl-tab-panel>
      </sl-tab-group>

      ${this.renderDeprecateDialog()}
      ${this.renderPublishDialog()}
    `;
  }

  private renderHeader() {
    const skill = this.skill!;
    return html`
      <div class="header">
        <div class="header-info">
          <div class="header-title">
            <sl-icon name="lightning-charge"></sl-icon>
            <h1>${skill.name}</h1>
            <scion-status-badge
              status=${skill.status as StatusType}
              label=${skill.status}
            ></scion-status-badge>
          </div>
          <div class="header-meta">
            <span class="scope-badge">${skill.scope}</span>
            <span class="visibility-badge ${skill.visibility}">${skill.visibility}</span>
          </div>
        </div>
        <div class="header-actions">
          ${can(skill._capabilities, 'update') ? html`
            <sl-button variant="default" size="small" outline @click=${() => this.startEditing()}>
              <sl-icon slot="prefix" name="pencil"></sl-icon>
              Edit
            </sl-button>
          ` : nothing}
          ${can(skill._capabilities, 'update') ? html`
            <sl-button
              variant="primary"
              size="small"
              @click=${() => { this.publishDialogOpen = true; }}
            >
              <sl-icon slot="prefix" name="upload"></sl-icon>
              Publish Version
            </sl-button>
          ` : nothing}
          ${can(skill._capabilities, 'delete') ? html`
            <sl-button
              variant="danger"
              size="small"
              outline
              ?loading=${this.actionLoading['archive']}
              ?disabled=${this.actionLoading['archive']}
              @click=${() => this.handleArchive()}
            >
              <sl-icon slot="prefix" name="trash"></sl-icon>
              Archive
            </sl-button>
          ` : nothing}
        </div>
      </div>
    `;
  }

  private renderOverviewTab() {
    const skill = this.skill!;

    if (this.editing) {
      return this.renderEditMode();
    }

    return html`
      <div class="card">
        <h3 class="card-title">Skill Details</h3>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Name</span>
            <span class="info-value">${skill.name}</span>
          </div>
          <div class="info-item">
            <span class="info-label">Slug</span>
            <span class="info-value mono">${skill.slug}</span>
          </div>
          <div class="info-item">
            <span class="info-label">URI</span>
            <span class="info-value">
              <span class="uri-copy" @click=${() => this.copyUri()} title="Click to copy">
                ${this.getSkillUri()}
                <sl-icon name="clipboard"></sl-icon>
              </span>
            </span>
          </div>
          <div class="info-item">
            <span class="info-label">Description</span>
            <span class="info-value">${skill.description || html`<span style="color: var(--scion-text-muted);">No description</span>`}</span>
          </div>
          <div class="info-item">
            <span class="info-label">Scope</span>
            <span class="info-value"><span class="scope-badge">${skill.scope}</span></span>
          </div>
          ${skill.scopeId ? html`
            <div class="info-item">
              <span class="info-label">Scope ID</span>
              <span class="info-value mono">${skill.scopeId}</span>
            </div>
          ` : nothing}
          <div class="info-item">
            <span class="info-label">Visibility</span>
            <span class="info-value"><span class="visibility-badge ${skill.visibility}">${skill.visibility}</span></span>
          </div>
          ${skill.ownerId ? html`
            <div class="info-item">
              <span class="info-label">Owner</span>
              <span class="info-value">${skill.ownerId}</span>
            </div>
          ` : nothing}
          <div class="info-item">
            <span class="info-label">Tags</span>
            <span class="info-value">
              ${skill.tags?.length
                ? html`<div class="tag-list">${skill.tags.map((t) => html`<span class="tag-item">${t}</span>`)}</div>`
                : html`<span style="color: var(--scion-text-muted);">No tags</span>`}
            </span>
          </div>
          <div class="info-item">
            <span class="info-label">Status</span>
            <span class="info-value">
              <scion-status-badge status=${skill.status as StatusType} label=${skill.status} size="small"></scion-status-badge>
            </span>
          </div>
          <div class="info-item">
            <span class="info-label">Created</span>
            <span class="info-value">${this.formatRelativeTime(skill.created)}</span>
          </div>
          <div class="info-item">
            <span class="info-label">Updated</span>
            <span class="info-value">${this.formatRelativeTime(skill.updated)}</span>
          </div>
        </div>
      </div>
    `;
  }

  private renderEditMode() {
    return html`
      <div class="card">
        <h3 class="card-title">Edit Skill</h3>
        <div class="edit-field">
          <label>Name</label>
          <sl-input
            .value=${this.editForm.name || ''}
            @sl-input=${(e: Event) => { this.editForm = { ...this.editForm, name: (e.target as HTMLElement & { value: string }).value }; }}
          ></sl-input>
        </div>
        <div class="edit-field">
          <label>Description</label>
          <sl-textarea
            .value=${this.editForm.description || ''}
            @sl-input=${(e: Event) => { this.editForm = { ...this.editForm, description: (e.target as HTMLElement & { value: string }).value }; }}
            rows="3"
            help-text="Once set, this field cannot be cleared."
          ></sl-textarea>
        </div>
        <div class="edit-field">
          <label>Visibility</label>
          <sl-radio-group
            .value=${this.editForm.visibility || 'private'}
            @sl-change=${(e: Event) => { this.editForm = { ...this.editForm, visibility: (e.target as HTMLElement & { value: string }).value }; }}
          >
            <sl-radio-button value="private">Private</sl-radio-button>
            <sl-radio-button value="public">Public</sl-radio-button>
          </sl-radio-group>
        </div>
        <div class="edit-field">
          <label>Tags</label>
          <sl-input
            .value=${this.editForm.tags || ''}
            @sl-input=${(e: Event) => { this.editForm = { ...this.editForm, tags: (e.target as HTMLElement & { value: string }).value }; }}
            placeholder="comma-separated tags"
          ></sl-input>
        </div>
        <div class="edit-actions">
          <sl-button variant="primary" size="small" ?loading=${this.saving} @click=${() => this.saveEdit()}>
            Save
          </sl-button>
          <sl-button variant="default" size="small" ?disabled=${this.saving} @click=${() => this.cancelEditing()}>
            Cancel
          </sl-button>
        </div>
      </div>
    `;
  }

  private renderVersionsTab() {
    if (this.versions.length === 0) {
      return html`
        <div class="empty-versions">
          <p>No versions published yet.</p>
          ${can(this.skill?._capabilities, 'update') ? html`
            <p>Use the "Publish Version" button to upload the first version.</p>
          ` : nothing}
        </div>
      `;
    }

    return html`
      <div class="version-table">
        <table>
          <thead>
            <tr>
              <th>Version</th>
              <th>Status</th>
              <th class="hide-mobile">Content Hash</th>
              <th class="hide-mobile">Files</th>
              <th class="hide-mobile">Downloads</th>
              <th class="hide-mobile">Published</th>
              <th class="actions-cell">Actions</th>
            </tr>
          </thead>
          <tbody>
            ${this.versions.map((v) => this.renderVersionRow(v))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderVersionRow(v: SkillVersion) {
    return html`
      <tr>
        <td><strong>${v.version}</strong></td>
        <td>
          <scion-status-badge
            status=${this.versionStatusType(v.status)}
            label=${v.status}
            size="small"
          ></scion-status-badge>
        </td>
        <td class="hide-mobile">
          ${v.contentHash
            ? html`<scion-hash-display .hash=${v.contentHash} max-width="14ch"></scion-hash-display>`
            : '—'}
        </td>
        <td class="hide-mobile">${v.files?.length ?? 0} files</td>
        <td class="hide-mobile">${v.downloadCount}</td>
        <td class="hide-mobile">${this.formatRelativeTime(v.created)}</td>
        <td class="actions-cell">
          ${v.status === 'published' && can(this.skill?._capabilities, 'update') ? html`
            <sl-button size="small" variant="warning" outline @click=${() => { this.deprecateVersionId = v.id; }}>
              Deprecate
            </sl-button>
          ` : nothing}
        </td>
      </tr>
    `;
  }

  private renderFilesTab() {
    const published = this.versions.filter((v) => v.status === 'published');

    if (published.length === 0) {
      return html`
        <div class="empty-versions">
          <p>No published versions available. Publish a version to see its files.</p>
        </div>
      `;
    }

    return html`
      <div class="version-selector" style="display: flex; align-items: center; gap: 0.75rem;">
        <sl-select
          size="small"
          .value=${this.selectedVersionForFiles}
          @sl-change=${(e: Event) => this.onFilesVersionChange(e)}
          style="max-width: 200px;"
          placeholder="Select version"
        >
          ${published.map((v) => html`<sl-option value=${v.version}>v${v.version}</sl-option>`)}
        </sl-select>
        ${!this.filesLoading && this.fileUrls.length > 0 ? html`
          <span style="font-size: 0.875rem; color: var(--scion-text-muted, #64748b);">${this.fileUrls.length} file${this.fileUrls.length !== 1 ? 's' : ''}</span>
        ` : nothing}
      </div>

      ${this.filesLoading ? html`
        <div class="loading-state" style="padding: 2rem;">
          <sl-spinner></sl-spinner>
          <p>Loading files...</p>
        </div>
      ` : this.fileUrls.length === 0 ? html`
        <div class="empty-versions">
          <p>No files found for this version.</p>
        </div>
      ` : html`
        <div class="file-table">
          <table>
            <thead>
              <tr>
                <th>Path</th>
                <th>Size</th>
                <th class="hide-mobile">Hash</th>
                <th style="text-align: right">Actions</th>
              </tr>
            </thead>
            <tbody>
              ${this.fileUrls.map((f) => html`
                <tr>
                  <td>
                    <span style="display: flex; align-items: center; gap: 0.5rem;">
                      <sl-icon name="file-earmark"></sl-icon>
                      ${f.path}
                    </span>
                  </td>
                  <td>${this.formatFileSize(f.size)}</td>
                  <td class="hide-mobile">
                    ${f.hash
                      ? html`<scion-hash-display .hash=${f.hash} max-width="14ch"></scion-hash-display>`
                      : '—'}
                  </td>
                  <td style="text-align: right">
                    <sl-button size="small" variant="default" outline @click=${() => {
                      const a = document.createElement('a');
                      a.href = f.url;
                      a.target = '_blank';
                      a.rel = 'noopener noreferrer';
                      a.click();
                    }}>
                      <sl-icon slot="prefix" name="download"></sl-icon>
                      Download
                    </sl-button>
                  </td>
                </tr>
              `)}
            </tbody>
          </table>
        </div>
      `}
    `;
  }

  private renderDeprecateDialog() {
    return html`
      <sl-dialog
        label="Deprecate Version"
        ?open=${this.deprecateVersionId !== null}
        @sl-after-hide=${() => { this.deprecateVersionId = null; }}
      >
        <div style="display: flex; flex-direction: column; gap: 1rem;">
          <sl-textarea
            label="Deprecation Message"
            placeholder="Reason for deprecation..."
            .value=${this.deprecateMessage}
            @sl-input=${(e: Event) => { this.deprecateMessage = (e.target as HTMLElement & { value: string }).value; }}
            required
          ></sl-textarea>
          <sl-input
            label="Replacement URI (optional)"
            placeholder="global:replacement-skill@1.0.0"
            .value=${this.deprecateReplacement}
            @sl-input=${(e: Event) => { this.deprecateReplacement = (e.target as HTMLElement & { value: string }).value; }}
          ></sl-input>
        </div>
        <sl-button
          slot="footer"
          variant="warning"
          ?loading=${this.deprecateLoading}
          ?disabled=${this.deprecateLoading || !this.deprecateMessage.trim()}
          @click=${() => this.handleDeprecate()}
        >
          Deprecate
        </sl-button>
      </sl-dialog>
    `;
  }

  private get latestPublishedVersion(): string {
    const published = this.versions
      .filter((v) => v.status === 'published')
      .sort((a, b) => (b.created || '').localeCompare(a.created || ''));
    return published.length > 0 ? published[0].version : '';
  }

  private renderPublishDialog() {
    return html`
      <scion-skill-publish-dialog
        .skillId=${this.skillId}
        ?open=${this.publishDialogOpen}
        .latestVersion=${this.latestPublishedVersion}
        @sl-after-hide=${() => { this.publishDialogOpen = false; }}
        @skill-version-published=${() => { this.publishDialogOpen = false; void this.loadData(); }}
      ></scion-skill-publish-dialog>
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading skill...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <a href="/skills" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Skills
      </a>
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Skill</h2>
        <p>There was a problem loading this skill.</p>
        <div class="error-details">${this.error || 'Skill not found'}</div>
        <sl-button variant="primary" @click=${() => this.loadData()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-skill-detail': ScionPageSkillDetail;
  }
}
