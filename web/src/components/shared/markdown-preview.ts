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
 * Markdown Preview Component
 *
 * Renders raw markdown text as sanitized HTML using marked + DOMPurify.
 * Both libraries are lazy-loaded on first use to keep the main bundle small.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { getMarkdownRenderer } from '../../utils/markdown.js';

@customElement('scion-markdown-preview')
export class ScionMarkdownPreview extends LitElement {
  /** Raw markdown text to render. */
  @property({ type: String })
  content = '';

  @state() private renderedHtml = '';
  @state() private loading = true;
  @state() private error: string | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .preview-container {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 1.5rem 2rem;
      background: var(--scion-surface, #ffffff);
      min-height: 200px;
      max-height: calc(100vh - 16rem);
      overflow-y: auto;
      line-height: 1.7;
      color: var(--scion-text, #1e293b);
      font-size: 0.9375rem;
    }

    /* ── Markdown content styles ── */

    .preview-container h1,
    .preview-container h2,
    .preview-container h3,
    .preview-container h4,
    .preview-container h5,
    .preview-container h6 {
      margin-top: 1.5em;
      margin-bottom: 0.5em;
      font-weight: 600;
      line-height: 1.3;
      color: var(--scion-text, #1e293b);
    }

    .preview-container h1 { font-size: 1.75rem; border-bottom: 1px solid var(--scion-border, #e2e8f0); padding-bottom: 0.3em; }
    .preview-container h2 { font-size: 1.375rem; border-bottom: 1px solid var(--scion-border, #e2e8f0); padding-bottom: 0.3em; }
    .preview-container h3 { font-size: 1.125rem; }
    .preview-container h4 { font-size: 1rem; }

    .preview-container p {
      margin: 0 0 1em;
    }

    .preview-container a {
      color: var(--sl-color-primary-600, #2563eb);
      text-decoration: none;
    }

    .preview-container a:hover {
      text-decoration: underline;
    }

    .preview-container code {
      font-family: var(--scion-font-mono, 'SF Mono', 'Fira Code', monospace);
      font-size: 0.85em;
      background: var(--scion-bg-subtle, #f8fafc);
      padding: 0.15em 0.35em;
      border-radius: 0.25rem;
      border: 1px solid var(--scion-border, #e2e8f0);
    }

    .preview-container pre {
      background: var(--scion-bg-subtle, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 1rem;
      overflow-x: auto;
      margin: 0 0 1em;
    }

    .preview-container pre code {
      background: none;
      border: none;
      padding: 0;
      font-size: 0.8125rem;
    }

    .preview-container blockquote {
      border-left: 4px solid var(--sl-color-primary-200, #bfdbfe);
      margin: 0 0 1em;
      padding: 0.5em 1em;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f8fafc);
      border-radius: 0 var(--scion-radius, 0.5rem) var(--scion-radius, 0.5rem) 0;
    }

    .preview-container blockquote p:last-child {
      margin-bottom: 0;
    }

    .preview-container ul,
    .preview-container ol {
      margin: 0 0 1em;
      padding-left: 1.5em;
    }

    .preview-container li {
      margin-bottom: 0.25em;
    }

    .preview-container table {
      border-collapse: collapse;
      width: 100%;
      margin: 0 0 1em;
    }

    .preview-container th,
    .preview-container td {
      border: 1px solid var(--scion-border, #e2e8f0);
      padding: 0.5em 0.75em;
      text-align: left;
    }

    .preview-container th {
      background: var(--scion-bg-subtle, #f8fafc);
      font-weight: 600;
    }

    .preview-container hr {
      border: none;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      margin: 1.5em 0;
    }

    .preview-container img {
      max-width: 100%;
      height: auto;
      border-radius: var(--scion-radius, 0.5rem);
    }

    .preview-container :first-child {
      margin-top: 0;
    }

    .preview-container :last-child {
      margin-bottom: 0;
    }

    .loading-state {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 3rem;
      color: var(--scion-text-muted, #64748b);
    }

    .loading-state sl-spinner {
      font-size: 1.5rem;
      margin-right: 0.75rem;
    }

    .error-state {
      padding: 1rem;
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.875rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.renderMarkdown();
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('content')) {
      void this.renderMarkdown();
    }
  }

  private async renderMarkdown(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const renderer = await getMarkdownRenderer();
      this.renderedHtml = renderer.render(this.content);
    } catch (err) {
      console.error('Failed to render markdown:', err);
      this.error = 'Failed to render markdown preview';
    } finally {
      this.loading = false;
    }
  }

  override render() {
    if (this.error) {
      return html`<div class="error-state">${this.error}</div>`;
    }

    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          Loading preview...
        </div>
      `;
    }

    return html`<div class="preview-container" .innerHTML=${this.renderedHtml}></div>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-markdown-preview': ScionMarkdownPreview;
  }
}
