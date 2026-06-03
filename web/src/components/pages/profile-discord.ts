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
 * Profile Discord linking page
 *
 * Allows users to link their Discord account by entering a 6-character
 * code provided by the Discord bot.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';
import { apiFetch } from '../../client/api.js';

@customElement('scion-page-profile-discord')
export class ScionPageProfileDiscord extends LitElement {
  @state()
  private _code = '';

  @state()
  private _status: 'idle' | 'submitting' | 'success' | 'error' = 'idle';

  @state()
  private _message = '';

  @state()
  private _linkedDiscordId = '';

  @state()
  private _discordUsername = '';

  override connectedCallback(): void {
    super.connectedCallback();
    const params = new URLSearchParams(window.location.search);
    const code = params.get('code');
    const userName = params.get('user_name');
    if (userName) {
      this._discordUsername = userName;
    }
    if (code) {
      this._code = code.toUpperCase().replace(/[^A-Z0-9]/g, '').slice(0, 6);
      if (this._code.length === 6) {
        this._autoSubmit();
      }
    }
  }

  private async _autoSubmit(): Promise<void> {
    this._status = 'submitting';
    this._message = 'Linking your account…';

    try {
      const resp = await apiFetch('/api/v1/discord/link/verify', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ code: this._code }),
      });

      if (resp.ok) {
        const data = (await resp.json()) as {
          status: string;
          discordUserId: string;
          user: { id: string; email: string };
        };
        this._status = 'success';
        this._linkedDiscordId = data.discordUserId;
        this._message = 'Discord account linked successfully! You can close this page and return to Discord.';
        this._code = '';
      } else {
        const errData = (await resp.json().catch(() => null)) as {
          message?: string;
        } | null;
        this._status = 'error';
        this._message = errData?.message || 'Code not found or expired. Please try again with a new code from the bot.';
      }
    } catch {
      this._status = 'error';
      this._message = 'Failed to connect to the server. Please try again.';
    }
  }

  static override styles = css`
    :host {
      display: block;
    }

    .page-header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 1.5rem;
      gap: 1rem;
    }

    .page-header-info h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .page-header-info p {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0;
    }

    .settings-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.75rem;
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .section-title {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 1rem 0;
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .section-title sl-icon {
      font-size: 1.125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .instructions {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1.25rem 0;
      line-height: 1.6;
    }

    .instructions ol {
      margin: 0.5rem 0;
      padding-left: 1.25rem;
    }

    .instructions li {
      margin-bottom: 0.375rem;
    }

    .discord-user {
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      margin: 0 0 1rem 0;
    }

    .discord-user strong {
      color: var(--scion-primary, #6366f1);
    }

    .code-form {
      display: flex;
      align-items: flex-end;
      gap: 0.75rem;
    }

    .code-input {
      flex: 0 0 auto;
    }

    .code-input sl-input::part(input) {
      font-family: monospace;
      font-size: 1.25rem;
      letter-spacing: 0.25em;
      text-transform: uppercase;
      text-align: center;
    }

    .result-message {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-top: 1rem;
      padding: 0.625rem 0.875rem;
      border-radius: 0.375rem;
      font-size: 0.8125rem;
    }

    .result-message sl-icon {
      font-size: 1rem;
      flex-shrink: 0;
    }

    .result-success {
      background: var(--sl-color-success-50, #f0fdf4);
      color: var(--sl-color-success-700, #15803d);
      border: 1px solid var(--sl-color-success-200, #bbf7d0);
    }

    .result-error {
      background: var(--sl-color-danger-50, #fef2f2);
      color: var(--sl-color-danger-700, #b91c1c);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
    }
  `;

  private _handleInput(e: Event): void {
    const input = e.target as HTMLInputElement & { value: string };
    this._code = input.value.toUpperCase().replace(/[^A-Z0-9]/g, '').slice(0, 6);
    input.value = this._code;
  }

  private async _handleSubmit(e: Event): Promise<void> {
    e.preventDefault();

    if (this._code.length !== 6) {
      this._status = 'error';
      this._message = 'Please enter the full 6-character code.';
      return;
    }

    this._status = 'submitting';
    this._message = '';

    try {
      const resp = await apiFetch('/api/v1/discord/link/verify', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ code: this._code }),
      });

      if (resp.ok) {
        const data = (await resp.json()) as {
          status: string;
          discordUserId: string;
          user: { id: string; email: string };
        };
        this._status = 'success';
        this._linkedDiscordId = data.discordUserId;
        this._message = 'Discord account linked successfully! You can close this page and return to Discord.';
        this._code = '';
      } else {
        const errData = (await resp.json().catch(() => null)) as {
          message?: string;
        } | null;
        this._status = 'error';
        this._message = errData?.message || 'Code not found or expired. Please try again with a new code from the bot.';
      }
    } catch {
      this._status = 'error';
      this._message = 'Failed to connect to the server. Please try again.';
    }
  }

  override render() {
    return html`
      <div class="page-header">
        <div class="page-header-info">
          <h1>Discord</h1>
          <p>Link your Discord account to receive notifications and interact with agents.</p>
        </div>
      </div>

      <div class="settings-card">
        <h2 class="section-title">
          <sl-icon name="link-45deg"></sl-icon>
          Link Discord Account
        </h2>

        <div class="instructions">
          <ol>
            <li>Open a channel with the Scion Discord bot</li>
            <li>Use the <strong>/scion register</strong> command</li>
            <li>Enter the 6-character code below</li>
          </ol>
        </div>

        ${this._discordUsername
          ? html`<p class="discord-user">Linking as Discord user: <strong>${this._discordUsername}</strong></p>`
          : nothing}

        <form class="code-form" @submit=${this._handleSubmit}>
          <div class="code-input">
            <sl-input
              placeholder="XXXXXX"
              maxlength="6"
              size="large"
              style="width: 12rem"
              .value=${this._code}
              @sl-input=${this._handleInput}
              ?disabled=${this._status === 'submitting'}
            ></sl-input>
          </div>
          <sl-button
            variant="primary"
            type="submit"
            ?loading=${this._status === 'submitting'}
            ?disabled=${this._code.length !== 6 || this._status === 'submitting'}
          >
            Link Account
          </sl-button>
        </form>

        ${this._status === 'success'
          ? html`
              <div class="result-message result-success">
                <sl-icon name="check-circle"></sl-icon>
                ${this._message}
              </div>
            `
          : nothing}
        ${this._status === 'error'
          ? html`
              <div class="result-message result-error">
                <sl-icon name="exclamation-circle"></sl-icon>
                ${this._message}
              </div>
            `
          : nothing}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-profile-discord': ScionPageProfileDiscord;
  }
}
