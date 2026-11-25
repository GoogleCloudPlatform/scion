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

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import {
  fallbackFieldKey,
  fieldErrorMessages,
  genericSaveErrorText,
  readSettingsSaveError,
} from '../../shared/settings-save-errors.js';
import type { SettingsFieldError, SettingsSaveError } from '../../shared/settings-save-errors.js';
import {
  renderConflictNotice,
  renderFieldErrorMessages,
  renderLayer0Notice,
  renderSectionErrorSummary,
  settingsErrorStyles,
} from '../shared/settings-error-views.js';

interface RateLimitInfo {
  remaining: number;
  limit: number;
  reset: string;
}

interface GitHubAppConfigData {
  configured: boolean;
  app_id: number;
  api_base_url?: string;
  webhooks_enabled: boolean;
  has_private_key: boolean;
  private_key_source?: 'config' | 'file' | 'secret' | '';
  private_key_overrides_secret?: boolean;
  has_webhook_secret: boolean;
  installation_url?: string;
  rate_limit?: RateLimitInfo;
}

@customElement('scion-admin-github-app-config')
export class ScionAdminGitHubAppConfig extends LitElement {
  @state() private githubAppConfigured = false;
  /**
   * True once GET /api/v1/github-app has succeeded, false after one fails.
   * Until it is true the card's fields hold defaults, not the stored config,
   * so the card cannot be saved: saving would send defaults as explicit clears
   * and wipe the stored App.
   */
  @state() private githubAppLoaded = false;
  @state() private githubAppId = 0;
  @state() private githubAppApiBaseUrl = '';
  @state() private githubAppWebhooksEnabled = false;
  @state() private githubAppHasPrivateKey = false;
  @state() private githubAppPrivateKeySource = '';
  @state() private githubAppPrivateKeyOverridesSecret = false;
  @state() private githubAppHasWebhookSecret = false;
  @state() private githubAppPrivateKey = '';
  @state() private githubAppWebhookSecret = '';
  @state() private githubAppInstallationUrl = '';
  @state() private githubAppSaving = false;
  @state() private githubAppError: string | null = null;
  @state() private githubAppSaveError: SettingsSaveError | null = null;
  @state() private githubAppSuccess: string | null = null;
  @state() private githubAppRateLimit: RateLimitInfo | null = null;

  static override styles = css`
    ${settingsErrorStyles}

    :host {
      display: block;
    }

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
    }

    .section-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.75rem;
      margin: 0 0 1rem 0;
      padding-bottom: 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .section-title {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .form-grid {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 1rem;
    }

    @media (max-width: 768px) {
      .form-grid {
        grid-template-columns: 1fr;
      }
    }

    .form-field {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
    }

    .form-field.full-width {
      grid-column: 1 / -1;
    }

    .form-field label {
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
    }

    .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .key-source {
      font-size: 0.75rem;
      color: var(--scion-success-text, #166534);
    }

    .status-message {
      font-size: 0.875rem;
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      margin-bottom: 1rem;
    }

    .status-message.success {
      background: var(--scion-success-bg, #dcfce7);
      color: var(--scion-success-text, #166534);
      border: 1px solid var(--scion-success-border, #86efac);
    }

    .status-message.error {
      background: var(--scion-error-bg, #fef2f2);
      color: var(--scion-error-text, #991b1b);
      border: 1px solid var(--scion-error-border, #fca5a5);
    }

    .status-message.warning {
      background: var(--scion-warning-bg, #fefce8);
      color: var(--scion-warning-text, #854d0e);
      border: 1px solid var(--scion-warning-border, #fde047);
      margin-top: 0.5rem;
      margin-bottom: 0;
    }

    .rate-limit {
      margin-top: 1rem;
    }

    .rate-limit-value {
      font-size: 0.875rem;
      margin-top: 0.25rem;
      color: var(--scion-text, #1e293b);
    }

    .rate-limit-low {
      color: var(--sl-color-danger-600);
    }

    .actions {
      display: flex;
      align-items: center;
      gap: 1rem;
      padding-top: 1rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      margin-top: 1rem;
    }

    .status-configured {
      font-size: 0.75rem;
      color: var(--scion-success-text, #166534);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadGitHubAppConfig();
  }

  private get githubAppFieldErrors(): SettingsFieldError[] {
    const err = this.githubAppSaveError;
    if (err?.kind !== 'validation') return [];
    return err.fieldErrors.map((e) =>
      e.key === null ? { ...e, key: fallbackFieldKey('server.github_app', e.path) } : e
    );
  }

  private renderFieldErrors(koanfKey: string) {
    const msgs = fieldErrorMessages(this.githubAppFieldErrors, koanfKey);
    const layer0 =
      this.githubAppSaveError?.kind === 'layer0' &&
      this.githubAppSaveError.keys.some((k) => k === koanfKey || k.startsWith(`${koanfKey}.`));
    if (msgs.length === 0 && !layer0) return nothing;
    return html`${renderFieldErrorMessages(msgs, koanfKey)}${layer0
      ? html`<span class="field-error" data-field-error=${koanfKey}
          ><code>${koanfKey}</code> is managed by deployment configuration and cannot be changed
          here.</span
        >`
      : nothing}`;
  }

  private renderSaveError() {
    const err = this.githubAppSaveError;
    if (!err) return nothing;
    if (err.kind === 'conflict') {
      return renderConflictNotice(err, () => {
        this.githubAppSaveError = null;
        this.githubAppError = null;
        this.githubAppSuccess = null;
        this.githubAppPrivateKey = '';
        this.githubAppWebhookSecret = '';
        void this.loadGitHubAppConfig();
      });
    }
    if (err.kind === 'layer0') return renderLayer0Notice(err);
    return renderSectionErrorSummary(this.githubAppFieldErrors, 'github_app_card');
  }

  private notifyConfigChanged(): void {
    this.dispatchEvent(
      new CustomEvent('github-app-config-changed', {
        detail: { configured: this.githubAppConfigured },
        bubbles: true,
        composed: true,
      })
    );
  }

  async loadGitHubAppConfig(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/github-app');
      if (res.ok) {
        this.githubAppError = null;
        const data = (await res.json()) as GitHubAppConfigData;
        this.githubAppConfigured = data.configured;
        this.githubAppId = data.app_id;
        this.githubAppApiBaseUrl = data.api_base_url || '';
        this.githubAppWebhooksEnabled = data.webhooks_enabled;
        this.githubAppHasPrivateKey = data.has_private_key;
        this.githubAppPrivateKeySource = data.private_key_source || '';
        this.githubAppPrivateKeyOverridesSecret = Boolean(data.private_key_overrides_secret);
        this.githubAppHasWebhookSecret = data.has_webhook_secret;
        this.githubAppInstallationUrl = data.installation_url || '';
        this.githubAppRateLimit = data.rate_limit || null;
        this.githubAppPrivateKey = '';
        this.githubAppWebhookSecret = '';
        this.githubAppLoaded = true;
        this.notifyConfigChanged();
      } else {
        this.githubAppLoaded = false;
        this.githubAppError = await extractApiError(res, 'Failed to load GitHub App configuration');
      }
    } catch {
      this.githubAppLoaded = false;
      this.githubAppError = 'Failed to load GitHub App configuration';
    }
  }

  private async handleSaveGitHubApp(): Promise<void> {
    if (!this.githubAppLoaded) return;
    this.githubAppSaving = true;
    this.githubAppError = null;
    this.githubAppSaveError = null;
    this.githubAppSuccess = null;

    try {
      // Every non-secret field is sent so a cleared one goes as an explicit
      // clear (0 or ""). PUT /api/v1/github-app reads null as "keep".
      const payload: Record<string, unknown> = {
        app_id: this.githubAppId || 0,
        api_base_url: this.githubAppApiBaseUrl,
        webhooks_enabled: this.githubAppWebhooksEnabled,
        installation_url: this.githubAppInstallationUrl,
      };

      if (this.githubAppPrivateKey.trim()) {
        payload.private_key = this.githubAppPrivateKey.trim();
      }
      if (this.githubAppWebhookSecret.trim()) {
        payload.webhook_secret = this.githubAppWebhookSecret.trim();
      }

      const res = await apiFetch('/api/v1/github-app', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });

      if (!res.ok) {
        const err = await readSettingsSaveError(res, {
          defaultSection: 'github_app',
        });
        if (err.kind === 'generic') {
          this.githubAppError = genericSaveErrorText(
            err,
            'Failed to save GitHub App configuration'
          );
        } else {
          this.githubAppSaveError = err;
        }
        return;
      }

      this.githubAppSuccess = 'GitHub App configuration saved successfully.';
      await this.loadGitHubAppConfig();
    } catch {
      this.githubAppError = 'Failed to save GitHub App configuration';
    } finally {
      this.githubAppSaving = false;
    }
  }

  override render() {
    return html`
      ${this.githubAppError
        ? html`<div class="status-message error">${this.githubAppError}</div>`
        : nothing}
      ${this.githubAppSuccess
        ? html`<div class="status-message success">${this.githubAppSuccess}</div>`
        : nothing}

      <div class="section">
        <div class="section-header">
          <h3 class="section-title">GitHub App Configuration</h3>
          ${this.githubAppLoaded
            ? this.githubAppConfigured
              ? html`<sl-badge variant="success">Configured</sl-badge>`
              : html`<sl-badge variant="neutral">Not configured</sl-badge>`
            : nothing}
        </div>
        ${this.renderSaveError()}
        <div class="form-grid">
          <div class="form-field">
            <label>App ID</label>
            <span class="hint">The numeric ID of your registered GitHub App</span>
            <sl-input
              .value=${this.githubAppId ? String(this.githubAppId) : ''}
              placeholder="e.g. 123456"
              inputmode="numeric"
              @sl-input=${(e: Event) => {
                this.githubAppId = parseInt((e.target as HTMLInputElement).value, 10) || 0;
              }}
            ></sl-input>
            ${this.renderFieldErrors('server.github_app.app_id')}
          </div>
          <div class="form-field">
            <label>API Base URL</label>
            <span class="hint"
              >Override for GitHub Enterprise Server (leave empty for github.com)</span
            >
            <sl-input
              .value=${this.githubAppApiBaseUrl}
              placeholder="https://api.github.com"
              @sl-input=${(e: Event) => {
                this.githubAppApiBaseUrl = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
            ${this.renderFieldErrors('server.github_app.api_base_url')}
          </div>
          <div class="form-field full-width">
            <label>Private Key (PEM)</label>
            <span class="hint">
              ${this.githubAppHasPrivateKey
                ? 'A private key is configured. Paste a new key to replace it, or leave empty to keep the current key.'
                : 'Paste the PEM-encoded private key from your GitHub App settings.'}
            </span>
            <sl-textarea
              rows=${4}
              .value=${this.githubAppPrivateKey}
              placeholder=${this.githubAppHasPrivateKey
                ? '(configured — leave empty to keep current)'
                : '-----BEGIN RSA PRIVATE KEY-----\n...'}
              @sl-input=${(e: Event) => {
                this.githubAppPrivateKey = (e.target as HTMLTextAreaElement).value;
              }}
            ></sl-textarea>
            ${this.renderFieldErrors('server.github_app.private_key')}
            ${this.githubAppHasPrivateKey
              ? html`<span data-github-app-key-source class="key-source"
                  >${this.githubAppPrivateKeySource === 'config'
                    ? 'Active key source: server configuration / environment'
                    : this.githubAppPrivateKeySource === 'file'
                      ? 'Active key source: private key file (private_key_path)'
                      : 'Stored as hub secret: GITHUB_APP_PRIVATE_KEY'}</span
                >`
              : nothing}
            ${this.githubAppPrivateKeyOverridesSecret
              ? html`<div class="status-message warning" data-github-app-key-override-warning>
                  A configured private key
                  (${this.githubAppPrivateKeySource === 'file'
                    ? 'private_key_path'
                    : 'server configuration / environment'})
                  is overriding the stored hub secret (GITHUB_APP_PRIVATE_KEY). Remove the
                  configured private key or private_key_path to use the key uploaded here.
                </div>`
              : nothing}
          </div>
          <div class="form-field">
            <label>Webhook Secret</label>
            <span class="hint">
              ${this.githubAppHasWebhookSecret
                ? 'A webhook secret is configured. Enter a new value to replace it.'
                : 'Secret for validating incoming GitHub webhook payloads.'}
            </span>
            <sl-input
              type="password"
              password-toggle
              .value=${this.githubAppWebhookSecret}
              placeholder=${this.githubAppHasWebhookSecret
                ? '(configured — leave empty to keep current)'
                : 'whsec_...'}
              @sl-input=${(e: Event) => {
                this.githubAppWebhookSecret = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
            ${this.renderFieldErrors('server.github_app.webhook_secret')}
            ${this.githubAppHasWebhookSecret
              ? html`<span class="key-source"
                  >Stored as hub secret: GITHUB_APP_WEBHOOK_SECRET</span
                >`
              : nothing}
          </div>
          <div class="form-field">
            <label>Webhooks</label>
            <span class="hint"
              >Indicates webhooks are configured on GitHub: adjusts background installation health
              checks from every 6 hours to every 24 hours. Incoming webhooks are accepted whenever a
              webhook secret is configured.</span
            >
            <sl-switch
              .checked=${this.githubAppWebhooksEnabled}
              @sl-change=${(e: Event) => {
                this.githubAppWebhooksEnabled = (e.target as HTMLInputElement).checked;
              }}
            >
              ${this.githubAppWebhooksEnabled ? 'Enabled' : 'Disabled'}
            </sl-switch>
            ${this.renderFieldErrors('server.github_app.webhooks_enabled')}
          </div>
          <div class="form-field full-width">
            <label>Public Installation URL</label>
            <span class="hint"
              >The public link where users can install this GitHub App on their org or account</span
            >
            <sl-input
              .value=${this.githubAppInstallationUrl}
              placeholder="https://github.com/apps/your-app-name/installations/new"
              @sl-input=${(e: Event) => {
                this.githubAppInstallationUrl = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
            ${this.renderFieldErrors('server.github_app.installation_url')}
          </div>
        </div>

        ${this.githubAppRateLimit
          ? html`
              <div class="rate-limit">
                <span class="hint">Rate Limit</span>
                <div class="rate-limit-value">
                  ${this.githubAppRateLimit.remaining}/${this.githubAppRateLimit.limit} remaining
                  ${this.githubAppRateLimit.remaining < this.githubAppRateLimit.limit / 5
                    ? html`<span class="rate-limit-low"> (low)</span>`
                    : nothing}
                </div>
              </div>
            `
          : nothing}

        <div class="actions">
          <sl-button
            variant="primary"
            ?loading=${this.githubAppSaving}
            ?disabled=${!this.githubAppLoaded}
            data-action="save-github-app"
            @click=${() => this.handleSaveGitHubApp()}
          >
            <sl-icon slot="prefix" name="check-lg"></sl-icon>
            Save GitHub App Configuration
          </sl-button>
          ${!this.githubAppLoaded
            ? html`<span class="hint" data-github-app-not-loaded
                >Cannot save: the GitHub App configuration did not load. Reload the page.</span
              >`
            : this.githubAppConfigured
              ? html`<span class="status-configured">Configured</span>`
              : html`<span class="hint">Not configured</span>`}
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-admin-github-app-config': ScionAdminGitHubAppConfig;
  }
}
