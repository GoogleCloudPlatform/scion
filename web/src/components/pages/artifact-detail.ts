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
 * Artifact page (experiment hub.artifacts)
 *
 * Shows an artifact's title, owner and current version, and renders its
 * entry file: markdown through <scion-markdown-preview>, text through a
 * read-only <scion-code-editor>, raster images through <img>. Other types
 * are offered as a download.
 * Route: /projects/{projectId}/artifacts/{artifactId}
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { isFeatureEnabled } from '../../utils/feature-flags.js';
import { formatInstant } from '../../utils/time.js';
import {
  ARTIFACTS_FLAG,
  MAX_INLINE_TEXT_BYTES,
  artifactFileUrl,
  baseName,
  formatBytes,
  isInlineType,
  rendererFor,
} from '../../client/artifacts.js';
import type { ArtifactFile, ArtifactResponse, ArtifactRenderer } from '../../client/artifacts.js';
import { getLanguageFromPath } from '../shared/code-editor.js';
import '../shared/markdown-preview.js';
import '../shared/code-editor.js';
import './not-found.js';

@customElement('scion-page-artifact-detail')
export class ScionPageArtifactDetail extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @state() private projectId = '';
  @state() private artifactId = '';
  @state() private loading = true;
  @state() private notFound = false;
  @state() private error: string | null = null;
  @state() private data: ArtifactResponse | null = null;
  @state() private entry: ArtifactFile | null = null;
  @state() private text: string | null = null;
  @state() private ownerName = '';
  @state() private copied = false;

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
    .header {
      margin-bottom: 1.25rem;
    }
    .title {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin: 0 0 0.5rem;
    }
    .title h1 {
      margin: 0;
      font-size: 1.5rem;
      font-weight: 600;
      word-break: break-word;
    }
    .title sl-icon {
      font-size: 1.25rem;
      color: var(--sl-color-neutral-500);
    }
    .meta {
      display: flex;
      flex-wrap: wrap;
      gap: 0.5rem 1.25rem;
      font-size: 0.8125rem;
      color: var(--sl-color-neutral-600);
    }
    .ref {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
    }
    .ref code {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.75rem;
    }
    .entry-bar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 1rem;
      margin-bottom: 0.5rem;
      font-size: 0.8125rem;
      color: var(--sl-color-neutral-600);
    }
    .image-frame {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 1rem;
      background: var(--scion-surface, #ffffff);
      text-align: center;
    }
    .image-frame img {
      max-width: 100%;
      height: auto;
    }
    .download-state,
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
    const path = this.pageData?.path || window.location.pathname;
    const m = path.match(/^\/projects\/([^/]+)\/artifacts\/([^/?#]+)/);
    if (m) {
      this.projectId = decodeURIComponent(m[1]);
      this.artifactId = decodeURIComponent(m[2]);
    }
    if (!isFeatureEnabled(ARTIFACTS_FLAG) || !this.artifactId) {
      this.loading = false;
      this.notFound = true;
      return;
    }
    void this.load();
  }

  private async load(): Promise<void> {
    this.loading = true;
    this.error = null;
    this.notFound = false;
    this.text = null;
    try {
      const res = await apiFetch(`/api/v1/artifacts/${encodeURIComponent(this.artifactId)}`);
      if (res.status === 404) {
        this.notFound = true;
        return;
      }
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      const data = (await res.json()) as ArtifactResponse;
      this.data = data;
      dispatchPageTitle(this, data.artifact.title, 'Artifacts');
      const version = data.version;
      this.entry = version?.files.find((f) => f.path === version.entryPath) ?? null;
      void this.loadOwnerName();
      const kind = this.entry ? rendererFor(this.entry.mediaType) : 'download';
      if ((kind === 'markdown' || kind === 'text') && this.entry) {
        if (this.entry.size <= MAX_INLINE_TEXT_BYTES) {
          await this.loadText(this.entry);
        }
      }
    } catch (err) {
      console.error('Failed to load artifact:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load artifact';
    } finally {
      this.loading = false;
    }
  }

  private async loadText(file: ArtifactFile): Promise<void> {
    const seq = this.data?.version?.seq ?? 0;
    const res = await apiFetch(artifactFileUrl(this.artifactId, seq, file.path, true));
    if (!res.ok) {
      throw new Error(await extractApiError(res, `HTTP ${res.status}`));
    }
    this.text = await res.text();
  }

  /** Best-effort display name for an agent owner; falls back to the id. */
  private async loadOwnerName(): Promise<void> {
    const a = this.data?.artifact;
    if (!a || a.ownerKind !== 'agent') return;
    try {
      // A reader may see the artifact without being allowed to see its
      // owning agent; that is not an error worth a toast.
      const res = await apiFetch(`/api/v1/agents/${encodeURIComponent(a.ownerRef)}`, {
        suppressAccessDeniedToast: true,
      });
      if (!res.ok) return;
      const agent = (await res.json()) as { name?: string; slug?: string };
      this.ownerName = agent.name || agent.slug || '';
    } catch {
      // The id is shown instead.
    }
  }

  private async copyRef(): Promise<void> {
    const ref = this.data?.artifact.ref;
    if (!ref) return;
    try {
      await navigator.clipboard.writeText(ref);
      this.copied = true;
      setTimeout(() => (this.copied = false), 1500);
    } catch {
      // Clipboard unavailable; the ref is visible on the page.
    }
  }

  override render(): TemplateResult | typeof nothing {
    if (this.loading) {
      return html`<div class="loading-state"><sl-spinner></sl-spinner></div>`;
    }
    if (this.notFound) {
      return html`<scion-page-404></scion-page-404>`;
    }
    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <p>${this.error}</p>
          <sl-button size="small" @click=${(): void => void this.load()}>Retry</sl-button>
        </div>
      `;
    }
    if (!this.data) return nothing;
    return html`
      <a
        href=${`/projects/${encodeURIComponent(this.data.artifact.scopeRef || this.projectId)}`}
        class="back-link"
      >
        <sl-icon name="arrow-left"></sl-icon>
        Project
      </a>
      ${this.renderHeader()} ${this.renderEntry()}
    `;
  }

  private renderHeader(): TemplateResult {
    const { artifact: a, version: v } = this.data!;
    const owner = this.ownerName || a.ownerRef;
    return html`
      <div class="header">
        <div class="title">
          <sl-icon name="file-earmark-richtext"></sl-icon>
          <h1>${a.title}</h1>
        </div>
        <div class="meta">
          <span>Owner: ${a.ownerKind} ${owner}</span>
          ${v ? html`<span>Version ${v.seq}</span>` : nothing}
          <span>Updated: ${formatInstant(a.updatedAt)}</span>
          <span class="ref">
            <code>${a.ref}</code>
            <sl-tooltip content=${this.copied ? 'Copied' : 'Copy reference'}>
              <sl-icon-button
                name="clipboard"
                label="Copy reference"
                @click=${(): void => void this.copyRef()}
              ></sl-icon-button>
            </sl-tooltip>
          </span>
        </div>
      </div>
    `;
  }

  private renderEntry(): TemplateResult {
    const v = this.data?.version;
    const f = this.entry;
    if (!v || !f) {
      return html`<div class="download-state">This artifact has no published content.</div>`;
    }
    const kind: ArtifactRenderer = rendererFor(f.mediaType);
    const href = artifactFileUrl(this.artifactId, v.seq, f.path);
    // Inline types open in a new tab (on an object-storage hub the
    // redirect is cross-origin, so download= would be ignored anyway);
    // attachment types download under their base name.
    const inline = isInlineType(f.mediaType);
    const action = inline
      ? html`<sl-button size="small" href=${href} target="_blank" rel="noopener noreferrer">
          <sl-icon slot="prefix" name="box-arrow-up-right"></sl-icon>
          Open raw
        </sl-button>`
      : html`<sl-button size="small" href=${href} download=${baseName(f.path)}>
          <sl-icon slot="prefix" name="download"></sl-icon>
          Download
        </sl-button>`;
    const bar = html`
      <div class="entry-bar">
        <span>${f.path} · ${formatBytes(f.size)}</span>
        ${action}
      </div>
    `;
    if (kind === 'image') {
      return html`${bar}
        <div class="image-frame"><img src=${href} alt=${this.data!.artifact.title} /></div>`;
    }
    if ((kind === 'markdown' || kind === 'text') && this.text !== null) {
      return kind === 'markdown'
        ? html`${bar}<scion-markdown-preview
              .content=${this.text}
              same-origin-images
            ></scion-markdown-preview>`
        : html`${bar}<scion-code-editor
              .content=${this.text}
              .language=${getLanguageFromPath(f.path)}
              readonly
            ></scion-code-editor>`;
    }
    const tooLarge = (kind === 'markdown' || kind === 'text') && f.size > MAX_INLINE_TEXT_BYTES;
    return html`${bar}
      <div class="download-state">
        <sl-icon name="file-earmark-text" style="font-size: 2rem;"></sl-icon>
        <p>
          ${tooLarge
            ? `This file is too large to show here (${formatBytes(f.size)}). Use ${inline ? 'Open raw' : 'Download'} to open it.`
            : 'This file type is not shown in the browser. Use Download to open it.'}
        </p>
      </div>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-artifact-detail': ScionPageArtifactDetail;
  }
}
