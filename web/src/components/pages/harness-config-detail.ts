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

  @state()
  private hasDockerfile = false;

  @state()
  private buildDialogOpen = false;

  @state()
  private buildRunning = false;

  @state()
  private buildTag = 'latest';

  @state()
  private buildPush = false;

  @state()
  private buildLog = '';

  @state()
  private buildStatus = '';

  @state()
  private buildRunId = '';

  @state()
  private buildError = '';

  @state()
  private reimportRunning = false;

  @state()
  private reimportStatus = '';

  @state()
  private reimportError = '';

  @state()
  private deleteDialogOpen = false;

  @state()
  private deleteInProgress = false;

  @state()
  private deleteFiles = false;

  @state()
  private deleteError = '';

  @state()
  private imageStatus: {
    image?: string;
    registry?: {
      image: string;
      exists: boolean;
      hash: string;
      checked_at: string;
    };
    brokers?: Array<{
      broker_id: string;
      broker_name: string;
      reachable: boolean;
      unsupported?: boolean;
      local_short?: { exists: boolean; hash: string };
      local_long?: { exists: boolean; hash: string };
      newer_in_registry?: boolean;
      resolved_image?: string;
      resolution_source?: string;
    }>;
    proxy_brokers?: Array<{
      broker_id: string;
      broker_name: string;
      runtime: string;
    }>;
  } | null = null;

  @state()
  private expandedBrokers: Set<string> = new Set();

  @state()
  private imageActionRunning = false;

  private fileBrowserDataSource: FileBrowserDataSource | null = null;
  private fileEditorDataSource: FileEditorDataSource | null = null;
  private buildPollTimer: ReturnType<typeof setTimeout> | null = null;
  private buildPollErrors = 0;

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

    .header-actions {
      margin-left: auto;
    }

    .build-log-section {
      margin-top: 1.5rem;
    }
    .build-log-section h3 {
      font-size: 0.95rem;
      font-weight: 600;
      margin: 0 0 0.5rem;
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }
    .build-log {
      background: var(--sl-color-neutral-50);
      border: 1px solid var(--sl-color-neutral-200);
      border-radius: var(--sl-border-radius-medium);
      padding: 1rem;
      font-family: var(--sl-font-mono);
      font-size: 0.8rem;
      line-height: 1.5;
      white-space: pre-wrap;
      word-break: break-all;
      max-height: 400px;
      overflow-y: auto;
    }

    .build-status-badge {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.75rem;
      font-weight: 500;
    }
    .build-status-badge.running { color: var(--sl-color-primary-600); }
    .build-status-badge.completed { color: var(--sl-color-success-600); }
    .build-status-badge.failed { color: var(--sl-color-danger-600); }

    .source-url {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
    }
    .source-url a {
      color: var(--sl-color-primary-600);
      text-decoration: none;
    }
    .source-url a:hover {
      text-decoration: underline;
    }
    .reimport-status {
      font-size: 0.85rem;
      margin-top: 0.5rem;
    }
    .reimport-status.success { color: var(--sl-color-success-600); }
    .reimport-status.error { color: var(--sl-color-danger-600); }

    .build-error {
      color: var(--sl-color-danger-600);
      font-size: 0.85rem;
      margin-top: 0.5rem;
    }

    .image-section {
      margin-top: 1.5rem;
    }
    .image-section h2 {
      font-size: 1.1rem;
      font-weight: 600;
      margin: 0 0 1rem;
    }
    .image-section-subtitle {
      font-size: 0.75rem;
      font-weight: 400;
      color: var(--sl-color-neutral-500);
    }
    .image-info-note {
      font-size: 0.8125rem;
      color: var(--sl-color-neutral-500);
      margin: 0.5rem 0 0;
      font-style: italic;
    }
    .image-table {
      width: 100%;
      border-collapse: collapse;
      border: 1px solid var(--sl-color-neutral-200);
      border-radius: var(--sl-border-radius-medium);
      overflow: hidden;
      font-size: 0.8125rem;
    }
    .image-table th {
      text-align: left;
      padding: 0.5rem 0.75rem;
      background: var(--sl-color-neutral-100);
      font-weight: 600;
      font-size: 0.75rem;
      color: var(--sl-color-neutral-600);
      text-transform: uppercase;
      letter-spacing: 0.025em;
    }
    .image-table td {
      padding: 0.5rem 0.75rem;
      border-top: 1px solid var(--sl-color-neutral-200);
      vertical-align: middle;
    }
    .image-table tr.active-row {
      background: var(--sl-color-success-50, #f0fdf4);
    }
    .image-entity-name {
      font-weight: 600;
      white-space: nowrap;
    }
    .image-ref {
      font-family: var(--sl-font-mono);
      font-size: 0.75rem;
      color: var(--sl-color-neutral-600);
      word-break: break-all;
    }
    .image-hash {
      font-family: var(--sl-font-mono);
      font-size: 0.6875rem;
      color: var(--sl-color-neutral-500);
    }
    .active-badge {
      display: inline-block;
      padding: 0.0625rem 0.375rem;
      border-radius: var(--sl-border-radius-pill);
      font-size: 0.625rem;
      font-weight: 700;
      text-transform: uppercase;
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
      margin-left: 0.5rem;
    }
    .newer-badge {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.0625rem 0.375rem;
      border-radius: var(--sl-border-radius-pill);
      font-size: 0.625rem;
      font-weight: 600;
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #a16207);
    }
    .not-found {
      color: var(--sl-color-neutral-400);
      font-style: italic;
    }
    .image-actions {
      display: flex;
      gap: 0.5rem;
      margin-top: 0.75rem;
      flex-wrap: wrap;
    }

    .broker-row {
      cursor: pointer;
      user-select: none;
    }
    .broker-row:hover {
      background: var(--sl-color-neutral-50);
    }
    .broker-row-expanded {
      background: var(--sl-color-neutral-50);
    }
    .broker-detail-row td {
      padding-left: 2rem;
    }
    .broker-toggle {
      display: inline-block;
      width: 1em;
      text-align: center;
      margin-right: 0.25rem;
      font-size: 0.75rem;
    }
    .rollup-badge {
      display: inline-block;
      padding: 0.0625rem 0.375rem;
      border-radius: var(--sl-border-radius-pill);
      font-size: 0.625rem;
      font-weight: 600;
      white-space: nowrap;
    }
    .rollup-badge-up-to-date {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }
    .rollup-badge-stale {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #a16207);
    }
    .rollup-badge-local-only {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }
    .rollup-badge-missing {
      background: var(--sl-color-danger-100, #fee2e2);
      color: var(--sl-color-danger-700, #b91c1c);
    }
    .rollup-badge-unreachable {
      background: var(--sl-color-neutral-200);
      color: var(--sl-color-neutral-600);
    }
    .reachable-dot {
      display: inline-block;
      width: 0.5rem;
      height: 0.5rem;
      border-radius: 50%;
      margin-right: 0.25rem;
    }
    .reachable-dot.online { background: var(--sl-color-success-500); }
    .reachable-dot.offline { background: var(--sl-color-neutral-400); }
    .proxy-info-note {
      font-size: 0.8125rem;
      color: var(--sl-color-neutral-500);
      margin: 1rem 0 0;
      padding: 0.75rem 1rem;
      background: var(--sl-color-neutral-50);
      border: 1px solid var(--sl-color-neutral-200);
      border-radius: var(--sl-border-radius-medium);
    }
    .broker-actions {
      display: flex;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      flex-wrap: wrap;
    }

    .dialog-warning {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.8125rem;
      color: var(--sl-color-danger-600);
      margin-top: 0.75rem;
    }
    .dialog-error {
      color: var(--sl-color-danger-600);
      font-size: 0.8125rem;
      margin-top: 0.5rem;
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
      this.hasDockerfile = this.harnessConfig.files?.some(f => f.path === 'Dockerfile') ?? false;
      dispatchPageTitle(
        this,
        this.harnessConfig.displayName || this.harnessConfig.name || this.harnessConfigId,
        'Harness Configs'
      );

      // Create data sources
      this.fileBrowserDataSource = new HarnessConfigFileBrowserDataSource(this.harnessConfigId);
      this.fileEditorDataSource = new HarnessConfigFileEditorDataSource(this.harnessConfigId);

      if (this.harnessConfig.config?.image) {
        void this.recheckImage();
      }
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

      ${this.renderHeader()} ${this.renderFilesSection()} ${this.renderImageSection()} ${this.renderBuildDialog()} ${this.renderBuildLog()} ${this.renderDeleteDialog()}
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
          <div class="header-actions">
            ${hc.sourceUrl ? html`
              <sl-button
                size="small"
                variant="default"
                @click=${this.startReimport}
                ?disabled=${this.reimportRunning}
                ?loading=${this.reimportRunning}
              >
                <sl-icon slot="prefix" name="arrow-repeat"></sl-icon>
                Refresh from Source
              </sl-button>
            ` : nothing}
            ${can(hc._capabilities, 'delete') || can(hc._capabilities, 'manage') ? html`
              <sl-button
                size="small"
                variant="danger"
                outline
                @click=${() => { this.deleteDialogOpen = true; this.deleteError = ''; }}
              >
                <sl-icon slot="prefix" name="trash"></sl-icon>
                Delete
              </sl-button>
            ` : nothing}
          </div>
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
          ${hc.sourceUrl
            ? html`<span class="source-url">Source:
                <a href=${hc.sourceUrl.replace(/^git\+/, '')} target="_blank" rel="noopener">${hc.sourceUrl}</a>
              </span>`
            : ''}
        </div>
        ${this.reimportStatus ? html`<p class="reimport-status success">${this.reimportStatus}</p>` : ''}
        ${this.reimportError ? html`<p class="reimport-status error">${this.reimportError}</p>` : ''}
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
  private renderImageSection() {
    const hc = this.harnessConfig!;
    const image = hc.config?.image;
    const showSection = image || this.hasDockerfile;
    if (!showSection) return nothing;

    const st = this.imageStatus;

    if (!st) {
      return html`
        <div class="image-section">
          <h2>Images</h2>
          <sl-spinner style="font-size: 1rem;"></sl-spinner> Loading image status...
        </div>
      `;
    }

    const brokers = st.brokers ?? [];
    const proxyBrokers = st.proxy_brokers ?? [];

    if (brokers.length === 1) {
      return this.renderSingleBrokerImage(brokers[0], st);
    } else if (brokers.length > 1) {
      return this.renderMultiBrokerImage(brokers, proxyBrokers, st);
    } else {
      return this.renderProxyOnlyImage(proxyBrokers, st);
    }
  }

  private brokerRollup(broker: NonNullable<NonNullable<typeof this.imageStatus>['brokers']>[0]): { label: string; cssClass: string } {
    if (!broker.reachable) return { label: 'unreachable', cssClass: 'rollup-badge-unreachable' };
    if (broker.unsupported) return { label: 'needs upgrade', cssClass: 'rollup-badge-stale' };
    if (broker.local_short?.exists && !broker.local_long?.exists) return { label: 'local build only', cssClass: 'rollup-badge-local-only' };
    if (broker.local_long?.exists && broker.newer_in_registry) return { label: 'stale pull', cssClass: 'rollup-badge-stale' };
    if ((broker.local_long?.exists && !broker.newer_in_registry) || broker.local_short?.exists) return { label: 'up-to-date', cssClass: 'rollup-badge-up-to-date' };
    return { label: 'missing', cssClass: 'rollup-badge-missing' };
  }

  private renderRegistryRow(st: typeof this.imageStatus) {
    return html`
      <tr>
        <td class="image-entity-name">Registry</td>
        <td class="image-ref">${st?.registry?.image || '—'}</td>
        <td class="image-hash">${st?.registry?.exists ? (st.registry.hash || '—') : ''}</td>
        <td>
          ${st
            ? (st.registry?.exists
              ? html`Available`
              : html`<span class="not-found">Not found</span>`)
            : html`<sl-spinner style="font-size: 0.75rem;"></sl-spinner> Checking...`}
        </td>
      </tr>
    `;
  }

  private renderSingleBrokerImage(
    broker: NonNullable<NonNullable<typeof this.imageStatus>['brokers']>[0],
    st: typeof this.imageStatus
  ) {
    const hc = this.harnessConfig!;
    const image = hc.config?.image;

    if (!broker.reachable) {
      return html`
        <div class="image-section">
          <h2>Images <span class="image-section-subtitle">Runtime: ${broker.broker_name}</span></h2>
          <table class="image-table">
            <thead>
              <tr><th>Entity</th><th>Image</th><th>Hash</th><th>Status</th></tr>
            </thead>
            <tbody>
              ${this.renderRegistryRow(st)}
            </tbody>
          </table>
          <div class="proxy-info-note">
            Runtime broker "${broker.broker_name}" is currently unreachable.
            Local image state cannot be determined.
          </div>
          <div class="image-actions">
            <sl-button size="small" variant="text" @click=${this.recheckImage} ?disabled=${this.imageActionRunning}>
              <sl-icon slot="prefix" name="arrow-repeat"></sl-icon>
              Re-check
            </sl-button>
          </div>
        </div>
      `;
    }

    const src = broker.resolution_source || '';
    return html`
      <div class="image-section">
        <h2>Images <span class="image-section-subtitle">Runtime: ${broker.broker_name}</span></h2>
        <table class="image-table">
          <thead>
            <tr><th>Entity</th><th>Image</th><th>Hash</th><th>Status</th></tr>
          </thead>
          <tbody>
            ${this.renderRegistryRow(st)}
            <tr class=${src === 'local_short' ? 'active-row' : ''}>
              <td class="image-entity-name">Local Build</td>
              <td class="image-ref">${st?.image || image || '—'}</td>
              <td class="image-hash">${broker.local_short?.exists ? (broker.local_short.hash || '—') : ''}</td>
              <td>
                ${broker.local_short?.exists
                  ? html`Available${src === 'local_short' ? html`<span class="active-badge">Active</span>` : nothing}`
                  : html`<span class="not-found">Not found</span>`}
              </td>
            </tr>
            <tr class=${src === 'local_long' ? 'active-row' : ''}>
              <td class="image-entity-name">Pulled</td>
              <td class="image-ref">${st?.registry?.image || '—'}</td>
              <td class="image-hash">${broker.local_long?.exists ? (broker.local_long.hash || '—') : ''}</td>
              <td>
                ${broker.local_long?.exists
                  ? html`Available${src === 'local_long' ? html`<span class="active-badge">Active</span>` : nothing}
                    ${broker.newer_in_registry ? html`<span class="newer-badge">Newer in registry</span>` : nothing}`
                  : html`<span class="not-found">Not found</span>`}
              </td>
            </tr>
          </tbody>
        </table>
        <div class="image-actions">
          ${this.hasDockerfile ? html`
            <sl-button size="small" variant="primary" @click=${this.openBuildDialog} ?disabled=${this.buildRunning}>
              <sl-icon slot="prefix" name="hammer"></sl-icon>
              ${this.buildRunning ? 'Building...' : 'Build Image'}
            </sl-button>
          ` : nothing}
          <sl-button size="small" variant="default" @click=${() => this.pullLatestImage(broker.broker_id)} ?disabled=${this.imageActionRunning}>
            <sl-icon slot="prefix" name="cloud-download"></sl-icon>
            Pull Latest
          </sl-button>
          ${broker.local_short?.exists ? html`
            <sl-button size="small" variant="warning" outline @click=${() => this.deleteLocalImage(broker.broker_id)} ?disabled=${this.imageActionRunning}>
              <sl-icon slot="prefix" name="trash"></sl-icon>
              Delete Local
            </sl-button>
          ` : nothing}
          <sl-button size="small" variant="text" @click=${this.recheckImage} ?disabled=${this.imageActionRunning}>
            <sl-icon slot="prefix" name="arrow-repeat"></sl-icon>
            Re-check
          </sl-button>
        </div>
      </div>
    `;
  }

  private toggleBroker(brokerId: string): void {
    const next = new Set(this.expandedBrokers);
    if (next.has(brokerId)) {
      next.delete(brokerId);
    } else {
      next.add(brokerId);
    }
    this.expandedBrokers = next;
  }

  private renderMultiBrokerImage(
    brokers: NonNullable<NonNullable<typeof this.imageStatus>['brokers']>,
    proxyBrokers: NonNullable<NonNullable<typeof this.imageStatus>['proxy_brokers']>,
    st: typeof this.imageStatus
  ) {
    return html`
      <div class="image-section">
        <h2>Images</h2>
        <table class="image-table">
          <thead>
            <tr><th>Entity</th><th>Image</th><th>Hash</th><th>Status</th></tr>
          </thead>
          <tbody>
            ${this.renderRegistryRow(st)}
            ${brokers.map(b => this.renderBrokerRow(b, st))}
          </tbody>
        </table>
        <div class="image-actions">
          ${this.hasDockerfile ? html`
            <sl-button size="small" variant="primary" @click=${this.openBuildDialog} ?disabled=${this.buildRunning}>
              <sl-icon slot="prefix" name="hammer"></sl-icon>
              ${this.buildRunning ? 'Building...' : 'Build Image'}
            </sl-button>
          ` : nothing}
          <sl-button size="small" variant="text" @click=${this.recheckImage} ?disabled=${this.imageActionRunning}>
            <sl-icon slot="prefix" name="arrow-repeat"></sl-icon>
            Re-check
          </sl-button>
        </div>
        ${proxyBrokers.length > 0 ? html`
          <div class="proxy-info-note">
            ${proxyBrokers.length} proxy broker${proxyBrokers.length > 1 ? 's' : ''}
            (${proxyBrokers.map(p => p.broker_name).join(', ')}) —
            images pulled by substrate at provision time.
          </div>
        ` : nothing}
      </div>
    `;
  }

  private renderBrokerRow(
    broker: NonNullable<NonNullable<typeof this.imageStatus>['brokers']>[0],
    st: typeof this.imageStatus
  ) {
    const expanded = this.expandedBrokers.has(broker.broker_id);
    const rollup = this.brokerRollup(broker);
    const src = broker.resolution_source || '';

    return html`
      <tr class="broker-row ${expanded ? 'broker-row-expanded' : ''}" @click=${() => this.toggleBroker(broker.broker_id)}>
        <td class="image-entity-name">
          <span class="broker-toggle">${expanded ? '▾' : '▸'}</span>
          <span class="reachable-dot ${broker.reachable ? 'online' : 'offline'}"></span>
          ${broker.broker_name}
        </td>
        <td><span class="rollup-badge ${rollup.cssClass}">${rollup.label}</span></td>
        <td></td>
        <td>${broker.resolved_image ? html`resolves: ${src === 'local_short' ? 'local build' : src === 'local_long' ? 'pulled' : src}` : ''}</td>
      </tr>
      ${expanded && broker.reachable ? html`
        <tr class="broker-detail-row">
          <td class="image-entity-name">Local Build</td>
          <td class="image-ref">${st?.image || '—'}</td>
          <td class="image-hash">${broker.local_short?.exists ? (broker.local_short.hash || '—') : ''}</td>
          <td>${broker.local_short?.exists
            ? html`Available${src === 'local_short' ? html`<span class="active-badge">Active</span>` : nothing}`
            : html`<span class="not-found">—</span>`}</td>
        </tr>
        <tr class="broker-detail-row">
          <td class="image-entity-name">Pulled</td>
          <td class="image-ref">${st?.registry?.image || '—'}</td>
          <td class="image-hash">${broker.local_long?.exists ? (broker.local_long.hash || '—') : ''}</td>
          <td>${broker.local_long?.exists
            ? html`Available${src === 'local_long' ? html`<span class="active-badge">Active</span>` : nothing}
              ${broker.newer_in_registry ? html`<span class="newer-badge">Newer in registry</span>` : nothing}`
            : html`<span class="not-found">—</span>`}</td>
        </tr>
        <tr class="broker-detail-row">
          <td colspan="4">
            <div class="broker-actions">
              <sl-button size="small" variant="default" @click=${(e: Event) => { e.stopPropagation(); void this.pullLatestImage(broker.broker_id); }} ?disabled=${this.imageActionRunning}>
                <sl-icon slot="prefix" name="cloud-download"></sl-icon>
                Pull Latest
              </sl-button>
              ${broker.local_short?.exists ? html`
                <sl-button size="small" variant="warning" outline @click=${(e: Event) => { e.stopPropagation(); void this.deleteLocalImage(broker.broker_id); }} ?disabled=${this.imageActionRunning}>
                  <sl-icon slot="prefix" name="trash"></sl-icon>
                  Delete Local
                </sl-button>
              ` : nothing}
              <sl-button size="small" variant="text" @click=${(e: Event) => { e.stopPropagation(); void this.recheckImage(); }} ?disabled=${this.imageActionRunning}>
                <sl-icon slot="prefix" name="arrow-repeat"></sl-icon>
                Re-check
              </sl-button>
            </div>
          </td>
        </tr>
      ` : nothing}
    `;
  }

  private renderProxyOnlyImage(
    _proxyBrokers: NonNullable<NonNullable<typeof this.imageStatus>['proxy_brokers']>,
    st: typeof this.imageStatus
  ) {
    return html`
      <div class="image-section">
        <h2>Images</h2>
        <table class="image-table">
          <thead>
            <tr><th>Entity</th><th>Image</th><th>Hash</th><th>Status</th></tr>
          </thead>
          <tbody>
            ${this.renderRegistryRow(st)}
          </tbody>
        </table>
        <div class="image-actions">
          <sl-button size="small" variant="text" @click=${this.recheckImage} ?disabled=${this.imageActionRunning}>
            <sl-icon slot="prefix" name="arrow-repeat"></sl-icon>
            Re-check
          </sl-button>
        </div>
        <div class="proxy-info-note">
          Agents on this hub run on proxy brokers.
          Images are pulled by the substrate at provision time;
          there is no node-local image state to inspect.
        </div>
      </div>
    `;
  }

  private async recheckImage(): Promise<void> {
    this.imageActionRunning = true;
    try {
      const resp = await apiFetch(
        `/api/v1/harness-configs/${this.harnessConfigId}/image-status`
      );
      if (resp.ok) {
        this.imageStatus = await resp.json();
      } else {
        const msg = await extractApiError(resp, `HTTP ${resp.status}`);
        alert(msg);
      }
    } finally {
      this.imageActionRunning = false;
    }
  }

  private async deleteLocalImage(brokerId: string): Promise<void> {
    this.imageActionRunning = true;
    try {
      const resp = await apiFetch(
        `/api/v1/harness-configs/${this.harnessConfigId}/local-image?broker_id=${brokerId}`,
        { method: 'DELETE' }
      );
      if (resp.ok) {
        await this.recheckImage();
      } else {
        const msg = await extractApiError(resp, `HTTP ${resp.status}`);
        alert(msg);
      }
    } finally {
      this.imageActionRunning = false;
    }
  }

  private async pullLatestImage(brokerId: string): Promise<void> {
    this.imageActionRunning = true;
    try {
      const resp = await apiFetch(
        `/api/v1/harness-configs/${this.harnessConfigId}/pull-image?broker_id=${brokerId}`,
        { method: 'POST' }
      );
      if (resp.ok) {
        await this.recheckImage();
      } else {
        const msg = await extractApiError(resp, `HTTP ${resp.status}`);
        alert(msg);
      }
    } finally {
      this.imageActionRunning = false;
    }
  }

  // ── Refresh from Source ──

  private async startReimport(): Promise<void> {
    if (!this.harnessConfig?.id) return;
    this.reimportRunning = true;
    this.reimportStatus = '';
    this.reimportError = '';

    try {
      const response = await apiFetch(
        `/api/v1/harness-configs/${this.harnessConfig.id}/reimport`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({}),
        },
      );

      if (!response.ok) {
        const errMsg = await extractApiError(response, `HTTP ${response.status}`);
        this.reimportError = errMsg;
        return;
      }

      const result = await response.json();
      const count = result?.count ?? result?.harnessConfigs?.length ?? 0;
      const failed: Array<{name: string; reason: string}> = result?.failed ?? [];

      if (failed.length > 0) {
        const reasons = failed.map((f: {name: string; reason: string}) => `${f.name}: ${f.reason}`).join('\n');
        this.reimportError = `${failed.length} config(s) failed validation:\n${reasons}`;
        if (count > 0) {
          this.reimportStatus = `${count} config(s) updated, but some failed.`;
        }
      } else {
        this.reimportStatus = `Refreshed successfully (${count} config${count !== 1 ? 's' : ''} updated).`;
      }
      await this.loadHarnessConfig();
    } catch (err) {
      this.reimportError = err instanceof Error ? err.message : 'Failed to refresh from source';
    } finally {
      this.reimportRunning = false;
    }
  }

  // ── Build Image ──

  private openBuildDialog(): void {
    this.buildTag = 'latest';
    this.buildPush = false;
    this.buildError = '';
    this.buildDialogOpen = true;
  }

  private renderBuildDialog() {
    return html`
      <sl-dialog
        label="Build Image"
        ?open=${this.buildDialogOpen}
        @sl-request-close=${() => (this.buildDialogOpen = false)}
      >
        <sl-input
          label="Image Tag"
          .value=${this.buildTag}
          @sl-input=${(e: Event) => (this.buildTag = (e.target as HTMLInputElement).value)}
        ></sl-input>
        <br />
        <sl-checkbox ?checked=${this.buildPush} @sl-change=${(e: Event) => (this.buildPush = (e.target as HTMLInputElement).checked)}>
          Push to registry after building
        </sl-checkbox>
        ${this.buildError ? html`<p class="build-error">${this.buildError}</p>` : nothing}
        <sl-button slot="footer" variant="primary" @click=${this.startBuild} ?loading=${this.buildRunning}>
          Build
        </sl-button>
        <sl-button slot="footer" variant="default" @click=${() => (this.buildDialogOpen = false)}>
          Cancel
        </sl-button>
      </sl-dialog>
    `;
  }

  private async startBuild(): Promise<void> {
    this.buildDialogOpen = false;
    this.buildRunning = true;
    this.buildLog = '';
    this.buildStatus = 'running';
    this.buildError = '';
    this.buildPollErrors = 0;

    try {
      const response = await apiFetch(
        '/api/v1/admin/maintenance/operations/build-harness-config-image/run',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            params: {
              harness_config_id: this.harnessConfigId,
              tag: this.buildTag || 'latest',
              push: this.buildPush ? 'true' : 'false',
            },
          }),
        },
      );

      if (!response.ok) {
        const errMsg = await extractApiError(response, `HTTP ${response.status}`);
        this.buildError = errMsg;
        this.buildRunning = false;
        this.buildStatus = 'failed';
        return;
      }

      const result = await response.json();
      if (!result?.runId) {
        this.buildError = 'Build started but no run ID was returned';
        this.buildRunning = false;
        this.buildStatus = 'failed';
        return;
      }
      this.buildRunId = result.runId;
      this.startBuildPolling();
    } catch (err) {
      this.buildError = err instanceof Error ? err.message : 'Failed to start build';
      this.buildRunning = false;
      this.buildStatus = 'failed';
    }
  }

  private startBuildPolling(): void {
    if (this.buildPollTimer) return;
    this.buildPollErrors = 0;
    void this.pollBuildStatus();
  }

  private stopBuildPolling(): void {
    if (this.buildPollTimer) {
      clearTimeout(this.buildPollTimer);
      this.buildPollTimer = null;
    }
  }

  private async pollBuildStatus(): Promise<void> {
    if (!this.buildRunId) return;

    try {
      const resp = await apiFetch(
        `/api/v1/admin/maintenance/operations/build-harness-config-image/runs/${this.buildRunId}`,
      );
      if (!resp.ok) {
        this.buildPollErrors++;
        if (this.buildPollErrors >= 5) {
          this.buildRunning = false;
          this.buildStatus = 'failed';
          this.buildError = 'Lost connection to build';
          this.stopBuildPolling();
        } else if (this.buildRunning) {
          this.buildPollTimer = setTimeout(() => void this.pollBuildStatus(), 3000);
        }
        return;
      }

      this.buildPollErrors = 0;
      const run = await resp.json();
      this.buildLog = run.log ?? '';
      this.buildStatus = run.status ?? '';
      void this.updateComplete.then(() => this.scrollBuildLog());

      if (run.status !== 'running') {
        this.buildRunning = false;
        this.stopBuildPolling();
        if (run.status === 'completed') {
          await this.loadHarnessConfig();
        }
      } else if (this.buildRunning) {
        this.buildPollTimer = setTimeout(() => void this.pollBuildStatus(), 3000);
      }
    } catch {
      this.buildPollErrors++;
      if (this.buildPollErrors >= 5) {
        this.buildRunning = false;
        this.buildStatus = 'failed';
        this.buildError = 'Lost connection to build';
        this.stopBuildPolling();
      } else if (this.buildRunning) {
        this.buildPollTimer = setTimeout(() => void this.pollBuildStatus(), 3000);
      }
    }
  }

  private scrollBuildLog(): void {
    const el = this.renderRoot?.querySelector('.build-log');
    if (el) {
      el.scrollTop = el.scrollHeight;
    }
  }

  private renderBuildLog() {
    if (!this.buildLog && !this.buildRunning) return nothing;

    const statusClass = this.buildStatus === 'completed' ? 'completed' : this.buildStatus === 'running' ? 'running' : 'failed';

    return html`
      <div class="build-log-section">
        <h3>
          Build Output
          <span class="build-status-badge ${statusClass}">
            ${this.buildStatus === 'running'
              ? html`<sl-spinner style="font-size: 0.75rem;"></sl-spinner> Running`
              : this.buildStatus}
          </span>
        </h3>
        <pre class="build-log">${this.buildLog}</pre>
      </div>
    `;
  }

  // ── Delete ──

  private renderDeleteDialog() {
    if (!this.deleteDialogOpen || !this.harnessConfig) return nothing;
    const hc = this.harnessConfig;
    return html`
      <sl-dialog
        label="Delete harness config"
        open
        @sl-request-close=${(e: Event) => {
          if (this.deleteInProgress) e.preventDefault();
          else this.deleteDialogOpen = false;
        }}
      >
        <p>
          Are you sure you want to delete
          <strong>${hc.displayName || hc.name}</strong>?
        </p>
        <sl-checkbox
          ?checked=${this.deleteFiles}
          @sl-change=${(e: Event) => {
            this.deleteFiles = (e.target as HTMLInputElement).checked;
          }}
        >
          Also delete stored files
        </sl-checkbox>
        <div class="dialog-warning">
          <sl-icon name="exclamation-triangle"></sl-icon>
          This action cannot be undone.
        </div>
        ${this.deleteError ? html`<div class="dialog-error">${this.deleteError}</div>` : nothing}
        <div slot="footer">
          <sl-button
            variant="default"
            size="small"
            ?disabled=${this.deleteInProgress}
            @click=${() => { this.deleteDialogOpen = false; }}
          >
            Cancel
          </sl-button>
          <sl-button
            variant="danger"
            size="small"
            ?loading=${this.deleteInProgress}
            ?disabled=${this.deleteInProgress}
            @click=${() => this.confirmDelete()}
          >
            Delete
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  private async confirmDelete(): Promise<void> {
    if (!this.harnessConfig) return;
    this.deleteInProgress = true;
    this.deleteError = '';
    try {
      const params = new URLSearchParams({ deleteFiles: String(this.deleteFiles) });
      const response = await apiFetch(
        `/api/v1/harness-configs/${this.harnessConfig.id}?${params.toString()}`,
        { method: 'DELETE' }
      );
      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `Failed to delete: HTTP ${response.status}`)
        );
      }
      this.deleteDialogOpen = false;
      // Navigate back to the list view
      const backLink = this.backLinks()[0];
      window.location.href = backLink.href;
    } catch (err) {
      this.deleteError = err instanceof Error ? err.message : 'Delete failed';
    } finally {
      this.deleteInProgress = false;
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopBuildPolling();
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-harness-config-detail': ScionPageHarnessConfigDetail;
  }
}
