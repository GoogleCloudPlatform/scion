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
 * Invite Landing Page
 *
 * Handles the invite code redemption flow:
 * 1. User visits /invite?code=scion_inv_xxx
 * 2. If not authenticated: shows sign-in prompt, stores code in sessionStorage
 * 3. If authenticated: auto-redeems the code
 * 4. On success: shows welcome message with dashboard link
 * 5. On error: shows appropriate error state
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

const INVITE_CODE_KEY = 'scion_invite_code';

type InviteState = 'loading' | 'unauthenticated' | 'redeeming' | 'success' | 'error' | 'no-code';

@customElement('scion-page-invite')
export class ScionPageInvite extends LitElement {
  @state()
  private pageState: InviteState = 'loading';

  @state()
  private errorMessage = '';

  @state()
  private googleEnabled = false;

  @state()
  private githubEnabled = false;

  static override styles = css`
    :host {
      display: flex;
      align-items: center;
      justify-content: center;
      min-height: 100vh;
      background: var(--scion-bg, #f8fafc);
      font-family: var(--scion-font, system-ui, -apple-system, sans-serif);
    }

    .card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 2.5rem;
      max-width: 28rem;
      width: 100%;
      text-align: center;
      box-shadow: 0 1px 3px rgba(0, 0, 0, 0.1);
    }

    .icon {
      font-size: 3rem;
      margin-bottom: 1rem;
    }

    .icon.success {
      color: var(--sl-color-success-500, #22c55e);
    }

    .icon.error {
      color: var(--sl-color-danger-500, #ef4444);
    }

    .icon.invite {
      color: var(--scion-primary, #3b82f6);
    }

    h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1.5rem 0;
      line-height: 1.5;
    }

    .providers {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
    }

    .providers sl-button::part(base) {
      width: 100%;
    }

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      gap: 1rem;
    }

    .loading-state sl-spinner {
      font-size: 2rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.initialize();
  }

  private async initialize(): Promise<void> {
    const params = new URLSearchParams(window.location.search);
    let code = params.get('code') || '';

    // If no code in URL, check sessionStorage (returning from OAuth)
    if (!code) {
      code = sessionStorage.getItem(INVITE_CODE_KEY) || '';
    }

    if (!code) {
      this.pageState = 'no-code';
      return;
    }

    // Store code in sessionStorage for OAuth redirect preservation
    sessionStorage.setItem(INVITE_CODE_KEY, code);

    // Check auth status
    try {
      const res = await fetch('/auth/me', { credentials: 'include' });
      if (res.ok) {
        // Authenticated — redeem the code
        await this.redeemCode(code);
      } else {
        // Not authenticated — show sign-in prompt
        await this.loadProviders();
        this.pageState = 'unauthenticated';
      }
    } catch {
      await this.loadProviders();
      this.pageState = 'unauthenticated';
    }
  }

  private async loadProviders(): Promise<void> {
    try {
      const res = await fetch('/auth/providers', { credentials: 'include' });
      if (res.ok) {
        const data = (await res.json()) as Record<string, boolean>;
        this.googleEnabled = data.google === true;
        this.githubEnabled = data.github === true;
      }
    } catch {
      // Default: show both
      this.googleEnabled = true;
      this.githubEnabled = true;
    }
  }

  private async redeemCode(code: string): Promise<void> {
    this.pageState = 'redeeming';
    try {
      const res = await fetch('/api/v1/auth/invite/redeem', {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ code }),
      });

      // Clear stored code on any terminal response
      sessionStorage.removeItem(INVITE_CODE_KEY);

      if (res.ok) {
        this.pageState = 'success';
        return;
      }

      if (res.status === 404) {
        this.errorMessage = 'This invite code was not found or is no longer valid.';
      } else if (res.status === 401) {
        // Session expired during redemption — show login
        await this.loadProviders();
        this.pageState = 'unauthenticated';
        return;
      } else {
        this.errorMessage = 'Something went wrong. Please try again or contact your hub administrator.';
      }
      this.pageState = 'error';
    } catch {
      sessionStorage.removeItem(INVITE_CODE_KEY);
      this.errorMessage = 'Failed to connect to the server. Please try again.';
      this.pageState = 'error';
    }
  }

  private handleLogin(provider: string): void {
    // Redirect to OAuth login, preserving the invite page as returnTo
    window.location.href = `/auth/login/${provider}?returnTo=${encodeURIComponent('/invite')}`;
  }

  override render() {
    return html`
      <div class="card">
        ${this.pageState === 'loading' || this.pageState === 'redeeming'
          ? this.renderLoading()
          : this.pageState === 'unauthenticated'
            ? this.renderUnauthenticated()
            : this.pageState === 'success'
              ? this.renderSuccess()
              : this.pageState === 'error'
                ? this.renderError()
                : this.renderNoCode()}
      </div>
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>${this.pageState === 'redeeming' ? 'Accepting your invitation...' : 'Loading...'}</p>
      </div>
    `;
  }

  private renderUnauthenticated() {
    return html`
      <sl-icon class="icon invite" name="envelope-open"></sl-icon>
      <h1>You've Been Invited</h1>
      <p>Sign in to accept your invitation and join the hub.</p>
      <div class="providers">
        ${this.googleEnabled
          ? html`<sl-button variant="default" size="large" @click=${() => this.handleLogin('google')}>
              <sl-icon slot="prefix" name="google"></sl-icon>
              Sign in with Google
            </sl-button>`
          : nothing}
        ${this.githubEnabled
          ? html`<sl-button variant="default" size="large" @click=${() => this.handleLogin('github')}>
              <sl-icon slot="prefix" name="github"></sl-icon>
              Sign in with GitHub
            </sl-button>`
          : nothing}
      </div>
    `;
  }

  private renderSuccess() {
    return html`
      <sl-icon class="icon success" name="check-circle"></sl-icon>
      <h1>Welcome!</h1>
      <p>You now have access to this hub.</p>
      <sl-button variant="primary" size="large" @click=${() => { window.location.href = '/'; }}>
        Go to Dashboard
      </sl-button>
    `;
  }

  private renderError() {
    return html`
      <sl-icon class="icon error" name="exclamation-triangle"></sl-icon>
      <h1>Invite Not Valid</h1>
      <p>${this.errorMessage}</p>
      <sl-button variant="default" @click=${() => { window.location.href = '/'; }}>
        Go to Home
      </sl-button>
    `;
  }

  private renderNoCode() {
    return html`
      <sl-icon class="icon error" name="exclamation-triangle"></sl-icon>
      <h1>No Invite Code</h1>
      <p>This page requires a valid invite link. Please ask your hub administrator for an invite.</p>
      <sl-button variant="default" @click=${() => { window.location.href = '/'; }}>
        Go to Home
      </sl-button>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-invite': ScionPageInvite;
  }
}
