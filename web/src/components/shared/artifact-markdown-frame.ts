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
 * Artifact markdown preview (experiment hub.artifacts).
 *
 * Renders an artifact's markdown entry with the shared marked + DOMPurify
 * renderer, points its images at the version's files (see
 * client/artifact-preview.ts) and shows the result in a srcdoc frame that
 * is sandboxed without allow-scripts and whose document carries a
 * Content-Security-Policy allowing images from the hub only.
 *
 * critic selects how CriticMarkup is shown: "off" (as written, the
 * default), "marks" (rendered as insertions, deletions, highlights and
 * numbered notes), "clean" (every mark rejected) or "accept" (every mark
 * accepted), the projections the hub serves with ?resolve=.
 */

import { LitElement, html, css } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, query, state } from 'lit/decorators.js';

import { getMarkdownRenderer } from '../../utils/markdown.js';
import { PREVIEW_SANDBOX, previewDocument, rewriteImages } from '../../client/artifact-preview.js';
import type { PreviewTheme } from '../../client/artifact-preview.js';
import type { ArtifactFile } from '../../client/artifacts.js';
import { projectCritic } from '../../utils/critic.js';

export type CriticView = 'off' | 'marks' | 'clean' | 'accept';

@customElement('scion-artifact-markdown-frame')
export class ScionArtifactMarkdownFrame extends LitElement {
  /** Raw markdown of the entry. */
  @property({ type: String }) content = '';
  @property({ type: String }) artifactId = '';
  /** Version whose files the images load from. */
  @property({ type: Number }) seq = 0;
  @property({ type: String }) entryPath = '';
  @property({ attribute: false }) files: ArtifactFile[] = [];
  @property({ type: String }) critic: CriticView = 'off';

  @state() private srcdoc = '';
  @state() private error: string | null = null;
  @query('iframe') private frame?: HTMLIFrameElement;

  private resizeObserver: ResizeObserver | null = null;

  static override styles = css`
    :host {
      display: block;
    }
    iframe {
      display: block;
      width: 100%;
      min-height: 200px;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-surface, #ffffff);
    }
    .error-state {
      padding: 1rem;
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.875rem;
    }
  `;

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.resizeObserver?.disconnect();
    this.resizeObserver = null;
  }

  override updated(changed: Map<string, unknown>): void {
    if (
      changed.has('content') ||
      changed.has('files') ||
      changed.has('seq') ||
      changed.has('entryPath') ||
      changed.has('artifactId') ||
      changed.has('critic')
    ) {
      void this.build();
    }
  }

  private theme(): Partial<PreviewTheme> {
    const cs = getComputedStyle(this);
    const v = (name: string): string => cs.getPropertyValue(name);
    return {
      text: v('--scion-text'),
      muted: v('--scion-text-muted'),
      border: v('--scion-border'),
      subtle: v('--scion-bg-subtle'),
      surface: v('--scion-surface'),
      link: v('--sl-color-primary-600'),
      fontMono: v('--scion-font-mono'),
    };
  }

  private async build(): Promise<void> {
    try {
      const renderer = await getMarkdownRenderer();
      const source =
        this.critic === 'clean' || this.critic === 'accept'
          ? projectCritic(this.content, this.critic)
          : this.content;
      const clean = renderer.render(source, { criticMarks: this.critic === 'marks' });
      const body = rewriteImages(clean, {
        id: this.artifactId,
        seq: this.seq,
        entryPath: this.entryPath,
        files: this.files,
      });
      this.srcdoc = previewDocument(body, this.theme());
      this.error = null;
    } catch (err) {
      console.error('Failed to render markdown:', err);
      this.error = 'Failed to render markdown preview';
    }
  }

  /** Sizes the frame to its document so the page, not the frame, scrolls. */
  private onLoad = (): void => {
    const doc = this.frame?.contentDocument;
    if (!doc?.documentElement) return;
    const fit = (): void => {
      if (this.frame) this.frame.style.height = `${doc.documentElement.scrollHeight}px`;
    };
    fit();
    this.resizeObserver?.disconnect();
    if (typeof ResizeObserver !== 'undefined') {
      this.resizeObserver = new ResizeObserver(fit);
      this.resizeObserver.observe(doc.documentElement);
    }
  };

  override render(): TemplateResult {
    if (this.error) {
      return html`<div class="error-state">${this.error}</div>`;
    }
    return html`<iframe
      title="Markdown preview"
      sandbox=${PREVIEW_SANDBOX}
      referrerpolicy="no-referrer"
      .srcdoc=${this.srcdoc}
      @load=${this.onLoad}
    ></iframe>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-artifact-markdown-frame': ScionArtifactMarkdownFrame;
  }
}
