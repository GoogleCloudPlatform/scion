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
 * Login Page Component
 *
 * Provides OAuth provider selection and error handling for authentication
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

/**
 * OAuth provider configuration
 */
interface OAuthProvider {
  id: string;
  name: string;
  icon: string;
  available: boolean;
}

@customElement('scion-login-page')
export class ScionLoginPage extends LitElement {
  /**
   * Error message to display (from query param)
   */
  @property({ type: String })
  error = '';

  /**
   * Return URL after login
   */
  @property({ type: String })
  returnTo = '/';

  /**
   * Whether Google OAuth is configured
   */
  @property({ type: Boolean })
  googleEnabled = false;

  /**
   * Whether GitHub OAuth is configured
   */
  @property({ type: Boolean })
  githubEnabled = false;

  /**
   * Loading state during OAuth redirect
   */
  @state()
  private _loading = false;

  static override styles = css`
    :host {
      display: flex;
      align-items: center;
      justify-content: center;
      min-height: 100vh;
      background: var(--scion-bg, #f8fafc);
      padding: 1rem;
    }

    .login-container {
      width: 100%;
      max-width: 400px;
      background: var(--scion-surface, #ffffff);
      border-radius: 1rem;
      box-shadow:
        0 4px 6px -1px rgba(0, 0, 0, 0.1),
        0 2px 4px -2px rgba(0, 0, 0, 0.1);
      padding: 2.5rem;
    }

    .logo {
      text-align: center;
      margin-bottom: 2rem;
    }

    .logo-text {
      font-size: 2rem;
      font-weight: 700;
      color: var(--scion-primary, #3b82f6);
      letter-spacing: -0.02em;
    }

    .logo-subtitle {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
    }

    h1 {
      font-size: 1.5rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      text-align: center;
      margin: 0 0 0.5rem 0;
    }

    .subtitle {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      text-align: center;
      margin-bottom: 2rem;
    }

    .error-alert {
      background: #fef2f2;
      border: 1px solid #fecaca;
      border-radius: 0.5rem;
      padding: 1rem;
      margin-bottom: 1.5rem;
      display: flex;
      align-items: flex-start;
      gap: 0.75rem;
    }

    .error-alert sl-icon {
      color: #dc2626;
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .error-message {
      font-size: 0.875rem;
      color: #991b1b;
    }

    .providers {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
    }

    .provider-btn {
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 0.75rem;
      width: 100%;
      padding: 0.875rem 1.25rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.5rem;
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text, #1e293b);
      font-size: 0.9375rem;
      font-weight: 500;
      cursor: pointer;
      transition:
        background 0.15s ease,
        border-color 0.15s ease,
        box-shadow 0.15s ease;
      text-decoration: none;
    }

    .provider-btn:hover {
      background: var(--scion-bg-subtle, #f8fafc);
      border-color: var(--scion-primary, #3b82f6);
    }

    .provider-btn:focus {
      outline: none;
      box-shadow: 0 0 0 2px var(--scion-primary-50, #eff6ff);
      border-color: var(--scion-primary, #3b82f6);
    }

    .provider-btn:disabled {
      opacity: 0.5;
      cursor: not-allowed;
    }

    .provider-btn:disabled:hover {
      background: var(--scion-surface, #ffffff);
      border-color: var(--scion-border, #e2e8f0);
    }

    .provider-icon {
      width: 1.25rem;
      height: 1.25rem;
      flex-shrink: 0;
    }

    /* Provider-specific colors */
    .provider-btn.google:hover {
      border-color: #4285f4;
    }

    .provider-btn.github:hover {
      border-color: #24292f;
    }

    .divider {
      display: flex;
      align-items: center;
      margin: 1.5rem 0;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.75rem;
      text-transform: uppercase;
      letter-spacing: 0.05em;
    }

    .divider::before,
    .divider::after {
      content: '';
      flex: 1;
      height: 1px;
      background: var(--scion-border, #e2e8f0);
    }

    .divider::before {
      margin-right: 1rem;
    }

    .divider::after {
      margin-left: 1rem;
    }

    .no-providers {
      text-align: center;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      padding: 1rem;
    }

    .footer {
      margin-top: 2rem;
      text-align: center;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .footer a {
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
    }

    .footer a:hover {
      text-decoration: underline;
    }

    /* Loading overlay */
    .loading-overlay {
      position: fixed;
      inset: 0;
      display: flex;
      align-items: center;
      justify-content: center;
      background: rgba(255, 255, 255, 0.9);
      z-index: 100;
    }

    .loading-content {
      text-align: center;
    }

    .loading-content sl-spinner {
      --indicator-color: var(--scion-primary, #3b82f6);
      --track-color: var(--scion-border, #e2e8f0);
      font-size: 2rem;
    }

    .loading-text {
      margin-top: 1rem;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }
  `;

  override connectedCallback() {
    super.connectedCallback();

    // Parse error from URL query params
    const params = new URLSearchParams(window.location.search);
    const error = params.get('error');
    if (error) {
      this.error = decodeURIComponent(error);
    }

    const returnTo = params.get('returnTo');
    if (returnTo) {
      this.returnTo = returnTo;
    }
  }

  override render() {
    const providers = this.getProviders();
    const hasProviders = providers.some((p) => p.available);

    return html`
      ${this._loading ? this.renderLoading() : ''}

      <div class="login-container">
        <div class="logo">
          <div class="logo-text">Scion</div>
          <div class="logo-subtitle">Agent Orchestration Platform</div>
        </div>

        <h1>Sign in</h1>
        <p class="subtitle">Choose a provider to continue</p>

        ${this.error
          ? html`
              <div class="error-alert" role="alert">
                <sl-icon name="exclamation-triangle"></sl-icon>
                <span class="error-message">${this.error}</span>
              </div>
            `
          : ''}

        <div class="providers">
          ${hasProviders
            ? providers.map((provider) => this.renderProvider(provider))
            : html`
                <div class="no-providers">
                  <p>No authentication providers configured.</p>
                  <p>Please configure OAuth credentials in the server settings.</p>
                </div>
              `}
        </div>

        <div class="footer">
          <p>
            By signing in, you agree to the
            <a href="/terms">Terms of Service</a> and <a href="/privacy">Privacy Policy</a>.
          </p>
        </div>
      </div>
    `;
  }

  private getProviders(): OAuthProvider[] {
    return [
      {
        id: 'google',
        name: 'Google',
        icon: 'google',
        available: this.googleEnabled,
      },
      {
        id: 'github',
        name: 'GitHub',
        icon: 'github',
        available: this.githubEnabled,
      },
    ];
  }

  private renderProvider(provider: OAuthProvider) {
    if (!provider.available) {
      return html`
        <button class="provider-btn ${provider.id}" disabled>
          ${this.renderProviderIcon(provider.id)}
          <span>Continue with ${provider.name}</span>
        </button>
      `;
    }

    const loginUrl =
      `/auth/login/${provider.id}` +
      (this.returnTo ? `?returnTo=${encodeURIComponent(this.returnTo)}` : '');

    return html`
      <a
        href="${loginUrl}"
        class="provider-btn ${provider.id}"
        @click=${() => this.handleProviderClick(provider)}
      >
        ${this.renderProviderIcon(provider.id)}
        <span>Continue with ${provider.name}</span>
      </a>
    `;
  }

  private renderProviderIcon(providerId: string) {
    // Use inline SVG for provider icons since Shoelace doesn't include brand icons
    if (providerId === 'google') {
      return html`
        <svg class="provider-icon" viewBox="0 0 24 24">
          <path
            fill="#4285F4"
            d="M22.56 12.25c0-.78-.07-1.53-.2-2.25H12v4.26h5.92c-.26 1.37-1.04 2.53-2.21 3.31v2.77h3.57c2.08-1.92 3.28-4.74 3.28-8.09z"
          />
          <path
            fill="#34A853"
            d="M12 23c2.97 0 5.46-.98 7.28-2.66l-3.57-2.77c-.98.66-2.23 1.06-3.71 1.06-2.86 0-5.29-1.93-6.16-4.53H2.18v2.84C3.99 20.53 7.7 23 12 23z"
          />
          <path
            fill="#FBBC05"
            d="M5.84 14.09c-.22-.66-.35-1.36-.35-2.09s.13-1.43.35-2.09V7.07H2.18C1.43 8.55 1 10.22 1 12s.43 3.45 1.18 4.93l2.85-2.22.81-.62z"
          />
          <path
            fill="#EA4335"
            d="M12 5.38c1.62 0 3.06.56 4.21 1.64l3.15-3.15C17.45 2.09 14.97 1 12 1 7.7 1 3.99 3.47 2.18 7.07l3.66 2.84c.87-2.6 3.3-4.53 6.16-4.53z"
          />
        </svg>
      `;
    }

    if (providerId === 'github') {
      return html`
        <svg class="provider-icon" viewBox="0 0 24 24" fill="currentColor">
          <path
            d="M12 0C5.374 0 0 5.373 0 12c0 5.302 3.438 9.8 8.207 11.387.599.111.793-.261.793-.577v-2.234c-3.338.726-4.033-1.416-4.033-1.416-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.083-.729.083-.729 1.205.084 1.839 1.237 1.839 1.237 1.07 1.834 2.807 1.304 3.492.997.107-.775.418-1.305.762-1.604-2.665-.305-5.467-1.334-5.467-5.931 0-1.311.469-2.381 1.236-3.221-.124-.303-.535-1.524.117-3.176 0 0 1.008-.322 3.301 1.23A11.509 11.509 0 0112 5.803c1.02.005 2.047.138 3.006.404 2.291-1.552 3.297-1.23 3.297-1.23.653 1.653.242 2.874.118 3.176.77.84 1.235 1.911 1.235 3.221 0 4.609-2.807 5.624-5.479 5.921.43.372.823 1.102.823 2.222v3.293c0 .319.192.694.801.576C20.566 21.797 24 17.3 24 12c0-6.627-5.373-12-12-12z"
          />
        </svg>
      `;
    }

    return html`<sl-icon name="box-arrow-in-right"></sl-icon>`;
  }

  private handleProviderClick(_provider: OAuthProvider) {
    this._loading = true;
  }

  private renderLoading() {
    return html`
      <div class="loading-overlay">
        <div class="loading-content">
          <sl-spinner></sl-spinner>
          <p class="loading-text">Redirecting to sign in...</p>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-login-page': ScionLoginPage;
  }
}
