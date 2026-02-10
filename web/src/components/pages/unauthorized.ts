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
 * Unauthorized Domain Page Component
 *
 * Displayed when a user attempts to log in with an email domain
 * that is not in the authorized domains list.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import type { PageData } from '../../shared/types.js';

@customElement('scion-page-unauthorized')
export class ScionPageUnauthorized extends LitElement {
  /**
   * Page data from SSR
   */
  @property({ type: Object })
  pageData: PageData | null = null;

  /**
   * The email address that was rejected (optional)
   */
  @property({ type: String })
  email = '';

  static override styles = css`
    :host {
      display: flex;
      align-items: center;
      justify-content: center;
      min-height: 100vh;
      background: var(--scion-bg, #f8fafc);
      padding: 1rem;
    }

    .container {
      text-align: center;
      max-width: 480px;
      background: var(--scion-surface, #ffffff);
      border-radius: 1rem;
      box-shadow:
        0 4px 6px -1px rgba(0, 0, 0, 0.1),
        0 2px 4px -2px rgba(0, 0, 0, 0.1);
      padding: 2.5rem;
    }

    .illustration {
      margin-bottom: 1.5rem;
    }

    .illustration sl-icon {
      font-size: 5rem;
      color: #f59e0b;
    }

    .code {
      font-size: 1.5rem;
      font-weight: 700;
      color: #f59e0b;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      margin-bottom: 0.5rem;
    }

    h1 {
      font-size: 1.5rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.75rem 0;
    }

    p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
      line-height: 1.6;
    }

    .email-display {
      font-family: var(--scion-font-mono, monospace);
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.5rem 0.75rem;
      border-radius: var(--scion-radius-sm, 0.25rem);
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      display: inline-block;
      margin-bottom: 1.5rem;
    }

    .help-text {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 2rem;
      padding: 1rem;
      background: #fef3c7;
      border: 1px solid #fde68a;
      border-radius: 0.5rem;
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
  `;

  override connectedCallback() {
    super.connectedCallback();

    // Parse email from URL query params
    const params = new URLSearchParams(window.location.search);
    const email = params.get('email');
    if (email) {
      this.email = decodeURIComponent(email);
    }
  }

  override render() {
    return html`
      <div class="container">
        <div class="illustration">
          <sl-icon name="shield-x"></sl-icon>
        </div>
        <div class="code">Access Denied</div>
        <h1>Domain Not Authorized</h1>
        <p>Your email domain is not authorized to access this Scion instance.</p>
        ${this.email ? html`<div class="email-display">${this.email}</div>` : ''}
        <div class="help-text">
          If you believe this is an error, please contact your system administrator to request
          access for your email domain.
        </div>
        <div class="actions">
          <sl-button variant="primary" href="/login">
            <sl-icon slot="prefix" name="arrow-left"></sl-icon>
            Try Different Account
          </sl-button>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-unauthorized': ScionPageUnauthorized;
  }
}
