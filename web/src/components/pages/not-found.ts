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
 * 404 Not Found page component
 *
 * Displayed when a route is not found, uses Shoelace components
 */

import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import type { PageData } from '../../shared/types.js';

@customElement('scion-page-404')
export class ScionPage404 extends LitElement {
  /**
   * Page data from SSR
   */
  @property({ type: Object })
  pageData: PageData | null = null;

  static override styles = css`
    :host {
      display: flex;
      align-items: center;
      justify-content: center;
      min-height: calc(100vh - 200px);
    }

    .container {
      text-align: center;
      max-width: 480px;
      padding: 2rem;
    }

    .code {
      font-size: 8rem;
      font-weight: 800;
      line-height: 1;
      background: linear-gradient(135deg, var(--scion-primary, #3b82f6) 0%, #8b5cf6 100%);
      -webkit-background-clip: text;
      -webkit-text-fill-color: transparent;
      background-clip: text;
      margin-bottom: 1rem;
    }

    h1 {
      font-size: 1.5rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.75rem 0;
    }

    p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 2rem 0;
      line-height: 1.6;
    }

    .path {
      font-family: var(--scion-font-mono, monospace);
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.25rem 0.5rem;
      border-radius: var(--scion-radius-sm, 0.25rem);
      font-size: 0.875rem;
    }

    .actions {
      display: flex;
      gap: 1rem;
      justify-content: center;
      flex-wrap: wrap;
    }

    sl-button::part(base) {
      font-weight: 500;
    }

    .illustration {
      margin-bottom: 1.5rem;
    }

    .illustration sl-icon {
      font-size: 6rem;
      color: var(--scion-neutral-300, #cbd5e1);
    }
  `;

  override render() {
    const requestedPath = this.pageData?.path || 'unknown';

    return html`
      <div class="container">
        <div class="illustration">
          <sl-icon name="emoji-frown"></sl-icon>
        </div>
        <div class="code">404</div>
        <h1>Page Not Found</h1>
        <p>
          Sorry, we couldn't find the page you're looking for. The path
          <span class="path">${requestedPath}</span> doesn't exist.
        </p>
        <div class="actions">
          <sl-button variant="primary" href="/">
            <sl-icon slot="prefix" name="house"></sl-icon>
            Back to Dashboard
          </sl-button>
          <sl-button variant="default" @click=${(): void => this.handleGoBack()}>
            <sl-icon slot="prefix" name="arrow-left"></sl-icon>
            Go Back
          </sl-button>
        </div>
      </div>
    `;
  }

  private handleGoBack(): void {
    window.history.back();
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-404': ScionPage404;
  }
}
