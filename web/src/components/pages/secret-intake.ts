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
 * Secret Intake Page
 *
 * Authenticated page for submitting secret values via intake links.
 * The JWT is read from the URL fragment (never sent to the server in the URL).
 * The user MUST be logged in to submit.
 *
 * Flow:
 * 1. Read JWT from window.location.hash
 * 2. Decode payload (base64url) to display context
 * 3. User pastes secret value + clicks submit
 * 4. POST to /api/v1/secret-intake/{jti}/store with auth cookie + token + value
 * 5. Show success or error state
 */

import { LitElement, html, css } from 'lit';
import { customElement, state } from 'lit/decorators.js';

type IntakeState = 'loading' | 'ready' | 'submitting' | 'success' | 'error' | 'expired' | 'no-token' | 'not-logged-in';

interface IntakeContext {
  jti: string;
  key: string;
  scope: string;
  scope_id: string;
  type: string;
  target: string;
  description: string;
  exp: number;
}

@customElement('scion-page-secret-intake')
export class ScionPageSecretIntake extends LitElement {
  @state()
  private pageState: IntakeState = 'loading';

  @state()
  private context: IntakeContext | null = null;

  @state()
  private errorMessage = '';

  @state()
  private secretValue = '';

  /** The raw JWT from the URL fragment */
  private token = '';

  static override styles = css`
    :host {
      display: flex;
      align-items: center;
      justify-content: center;
      min-height: 100vh;
      background: var(--scion-bg, #f8fafc);
      font-family: var(--scion-font, system-ui, -apple-system, sans-serif);
      padding: 1rem;
    }

    .card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 2.5rem;
      max-width: 32rem;
      width: 100%;
      box-shadow: 0 1px 3px rgba(0, 0, 0, 0.1);
    }

    .icon {
      font-size: 2.5rem;
      margin-bottom: 1rem;
      text-align: center;
    }

    .icon.lock {
      color: var(--scion-primary, #3b82f6);
    }

    .icon.success {
      color: var(--sl-color-success-500, #22c55e);
    }

    .icon.error {
      color: var(--sl-color-danger-500, #ef4444);
    }

    h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
      text-align: center;
    }

    p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
      line-height: 1.5;
      text-align: center;
    }

    .context-info {
      background: var(--scion-bg, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 1rem;
      margin-bottom: 1.5rem;
      font-size: 0.875rem;
    }

    .context-row {
      display: flex;
      justify-content: space-between;
      padding: 0.25rem 0;
    }

    .context-label {
      color: var(--scion-text-muted, #64748b);
      font-weight: 500;
    }

    .context-value {
      color: var(--scion-text, #1e293b);
      font-weight: 600;
      word-break: break-all;
    }

    .form-group {
      margin-bottom: 1.5rem;
    }

    .form-group label {
      display: block;
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.5rem;
    }

    textarea {
      width: 100%;
      min-height: 6rem;
      padding: 0.75rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      font-family: monospace;
      font-size: 0.875rem;
      resize: vertical;
      box-sizing: border-box;
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text, #1e293b);
    }

    textarea:focus {
      outline: 2px solid var(--scion-primary, #3b82f6);
      outline-offset: -1px;
      border-color: var(--scion-primary, #3b82f6);
    }

    .submit-btn {
      width: 100%;
    }

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      gap: 1rem;
      text-align: center;
    }

    .loading-state sl-spinner {
      font-size: 2rem;
    }

    .security-note {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #94a3b8);
      text-align: center;
      margin-top: 1rem;
    }

    a {
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
    }

    a:hover {
      text-decoration: underline;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    this.initialize();
  }

  private initialize(): void {
    // Read JWT from URL fragment
    const hash = window.location.hash;
    if (!hash || hash.length < 2) {
      this.pageState = 'no-token';
      return;
    }

    this.token = hash.substring(1);

    // Decode JWT payload (base64url, no verification — that's server-side)
    try {
      const parts = this.token.split('.');
      if (parts.length !== 3) {
        this.pageState = 'no-token';
        return;
      }

      // base64url decode the payload (add padding for atob)
      let payload = parts[1]
        .replace(/-/g, '+')
        .replace(/_/g, '/');
      while (payload.length % 4) payload += '=';
      const decoded = decodeURIComponent(escape(atob(payload)));
      const claims = JSON.parse(decoded) as IntakeContext;

      // Check expiry client-side
      if (claims.exp && claims.exp * 1000 < Date.now()) {
        this.pageState = 'expired';
        return;
      }

      this.context = claims;
      this.pageState = 'ready';
    } catch {
      this.pageState = 'no-token';
    }
  }

  private async handleSubmit(): Promise<void> {
    if (!this.context || !this.secretValue.trim()) return;

    this.pageState = 'submitting';

    try {
      const res = await fetch(`/api/v1/secret-intake/${this.context.jti}/store`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'same-origin',
        body: JSON.stringify({
          token: this.token,
          value: this.secretValue,
        }),
      });

      if (res.ok) {
        this.pageState = 'success';
        // Clear the value from memory
        this.secretValue = '';
        return;
      }

      const data = await res.json().catch(() => ({})) as { error?: { message?: string } };
      const msg = data?.error?.message || 'Something went wrong';

      if (res.status === 401) {
        this.pageState = 'not-logged-in';
      } else if (res.status === 404) {
        this.errorMessage = 'This intake link has expired or is no longer valid.';
        this.pageState = 'expired';
      } else if (res.status === 410) {
        this.errorMessage = 'This intake link has already been used.';
        this.pageState = 'error';
      } else {
        this.errorMessage = msg;
        this.pageState = 'error';
      }
    } catch {
      this.errorMessage = 'Failed to connect to the server. Please try again.';
      this.pageState = 'error';
    }
  }

  private handleInput(e: Event): void {
    const target = e.target as HTMLTextAreaElement;
    this.secretValue = target.value;
  }

  override render() {
    return html`
      <div class="card">
        ${this.pageState === 'loading' || this.pageState === 'submitting'
          ? this.renderLoading()
          : this.pageState === 'ready'
            ? this.renderForm()
            : this.pageState === 'success'
              ? this.renderSuccess()
              : this.pageState === 'expired'
                ? this.renderExpired()
                : this.pageState === 'not-logged-in'
                  ? this.renderNotLoggedIn()
                  : this.pageState === 'error'
                    ? this.renderError()
                    : this.renderNoToken()}
      </div>
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>${this.pageState === 'submitting' ? 'Storing secret...' : 'Loading...'}</p>
      </div>
    `;
  }

  private renderForm() {
    const ctx = this.context!;
    const scopeLabel = ctx.scope === 'project' ? `Project: ${ctx.scope_id}` :
                       ctx.scope === 'user' ? 'User scope' :
                       ctx.scope;
    const typeLabel = ctx.type || 'environment';

    return html`
      <div class="icon lock">&#x1F510;</div>
      <h1>Store Secret</h1>
      <p>Paste the requested secret value below.</p>

      <div class="context-info">
        <div class="context-row">
          <span class="context-label">Secret:</span>
          <span class="context-value">${ctx.key}</span>
        </div>
        ${ctx.description ? html`
          <div class="context-row">
            <span class="context-label">Why:</span>
            <span class="context-value">${ctx.description}</span>
          </div>
        ` : ''}
        ${ctx.scope ? html`
          <div class="context-row">
            <span class="context-label">Where:</span>
            <span class="context-value">${scopeLabel}</span>
          </div>
        ` : ''}
        <div class="context-row">
          <span class="context-label">Type:</span>
          <span class="context-value">${typeLabel}</span>
        </div>
      </div>

      <div class="form-group">
        <label for="secret-value">Paste your value here</label>
        <textarea
          id="secret-value"
          placeholder="Paste your secret value here..."
          .value=${this.secretValue}
          @input=${this.handleInput}
          autocomplete="off"
          spellcheck="false"
        ></textarea>
      </div>

      <sl-button
        class="submit-btn"
        variant="primary"
        size="large"
        ?disabled=${!this.secretValue.trim()}
        @click=${this.handleSubmit}
      >
        Submit
      </sl-button>

      <p class="security-note">
        The secret value is sent directly to the server over HTTPS.
        It is not visible in the URL or browser history.
      </p>
    `;
  }

  private renderSuccess() {
    return html`
      <div class="icon success">&#x2705;</div>
      <h1>Secret Stored</h1>
      <p>
        The secret has been stored successfully.
        You can close this page now.
      </p>
    `;
  }

  private renderNotLoggedIn() {
    // Preserve the hash so after login the user lands back here with the JWT intact
    const loginUrl = `/login?redirect=${encodeURIComponent(window.location.pathname + window.location.hash)}`;
    return html`
      <div class="icon lock">&#x1F510;</div>
      <h1>Login Required</h1>
      <p>You must be logged in to submit a secret.</p>
      <sl-button variant="primary" size="large" href=${loginUrl}>
        Log in
      </sl-button>
    `;
  }

  private renderExpired() {
    return html`
      <div class="icon error">&#x23F0;</div>
      <h1>Link Expired</h1>
      <p>${this.errorMessage || 'This intake link has expired. Please request a new one.'}</p>
    `;
  }

  private renderError() {
    return html`
      <div class="icon error">&#x26A0;</div>
      <h1>Error</h1>
      <p>${this.errorMessage}</p>
      <sl-button variant="default" @click=${() => { this.pageState = 'ready'; this.errorMessage = ''; }}>
        Try Again
      </sl-button>
    `;
  }

  private renderNoToken() {
    return html`
      <div class="icon error">&#x26A0;</div>
      <h1>Invalid Link</h1>
      <p>This page requires a valid secret intake link. The link should contain a token in the URL fragment.</p>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-secret-intake': ScionPageSecretIntake;
  }
}
