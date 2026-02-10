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
 * Debug Panel Component
 *
 * Displays debug information about authentication, session, and configuration.
 * Only visible when debug mode is enabled.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

/**
 * Debug data structure from /auth/debug endpoint
 */
interface DebugData {
  debug: boolean;
  timestamp: string;
  auth: {
    stateUser: { id: string; email: string; name: string } | null;
    sessionUser: { id: string; email: string; name: string } | null;
    devToken: string;
    devAuthEnabled: boolean;
  };
  session: {
    exists: boolean;
    isNew: boolean;
    keys: string[];
    hasUser: boolean;
    hasReturnTo: boolean;
    hasOauthState: boolean;
  };
  cookies: {
    header: string;
    count: number;
    names: string[];
    hasSessionCookie: boolean;
  };
  config: {
    production: boolean;
    debug: boolean;
    baseUrl: string;
    hubApiUrl: string;
    hasGoogleOAuth: boolean;
    hasGitHubOAuth: boolean;
    authorizedDomains: string[];
  };
}

@customElement('scion-debug-panel')
export class ScionDebugPanel extends LitElement {
  /**
   * Whether the panel is expanded
   */
  @property({ type: Boolean })
  expanded = false;

  /**
   * Debug data from server
   */
  @state()
  private debugData: DebugData | null = null;

  /**
   * Loading state
   */
  @state()
  private loading = false;

  /**
   * Error message
   */
  @state()
  private error: string | null = null;

  /**
   * Whether debug mode is available
   */
  @state()
  private debugAvailable = true;

  static override styles = css`
    :host {
      display: block;
      position: fixed;
      bottom: 0;
      right: 0;
      z-index: 10000;
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.75rem;
    }

    .toggle-button {
      position: absolute;
      bottom: 1rem;
      right: 1rem;
      background: #1e293b;
      color: #f1f5f9;
      border: none;
      padding: 0.5rem 1rem;
      border-radius: 0.375rem;
      cursor: pointer;
      font-family: inherit;
      font-size: inherit;
      display: flex;
      align-items: center;
      gap: 0.5rem;
      box-shadow: 0 4px 6px -1px rgba(0, 0, 0, 0.1);
    }

    .toggle-button:hover {
      background: #334155;
    }

    .toggle-button.error {
      background: #7f1d1d;
    }

    .panel {
      position: absolute;
      bottom: 4rem;
      right: 1rem;
      width: 450px;
      max-height: 500px;
      background: #1e293b;
      color: #f1f5f9;
      border-radius: 0.5rem;
      overflow: hidden;
      box-shadow: 0 25px 50px -12px rgba(0, 0, 0, 0.25);
      display: none;
    }

    .panel.expanded {
      display: block;
    }

    .panel-header {
      background: #0f172a;
      padding: 0.75rem 1rem;
      display: flex;
      align-items: center;
      justify-content: space-between;
      border-bottom: 1px solid #334155;
    }

    .panel-header h3 {
      margin: 0;
      font-size: 0.875rem;
      font-weight: 600;
    }

    .panel-header button {
      background: transparent;
      border: none;
      color: #94a3b8;
      cursor: pointer;
      padding: 0.25rem;
    }

    .panel-header button:hover {
      color: #f1f5f9;
    }

    .panel-content {
      padding: 1rem;
      max-height: 400px;
      overflow-y: auto;
    }

    .section {
      margin-bottom: 1rem;
    }

    .section:last-child {
      margin-bottom: 0;
    }

    .section-title {
      font-weight: 600;
      color: #3b82f6;
      margin-bottom: 0.5rem;
      text-transform: uppercase;
      font-size: 0.625rem;
      letter-spacing: 0.05em;
    }

    .info-row {
      display: flex;
      justify-content: space-between;
      padding: 0.25rem 0;
      border-bottom: 1px solid #334155;
    }

    .info-row:last-child {
      border-bottom: none;
    }

    .info-label {
      color: #94a3b8;
    }

    .info-value {
      color: #f1f5f9;
    }

    .info-value.success {
      color: #22c55e;
    }

    .info-value.warning {
      color: #f59e0b;
    }

    .info-value.error {
      color: #ef4444;
    }

    .refresh-button {
      background: #3b82f6;
      color: white;
      border: none;
      padding: 0.5rem 1rem;
      border-radius: 0.25rem;
      cursor: pointer;
      font-family: inherit;
      font-size: inherit;
      width: 100%;
      margin-top: 0.5rem;
    }

    .refresh-button:hover {
      background: #2563eb;
    }

    .refresh-button:disabled {
      background: #475569;
      cursor: not-allowed;
    }

    .error-message {
      color: #ef4444;
      padding: 0.5rem;
      background: rgba(239, 68, 68, 0.1);
      border-radius: 0.25rem;
      margin-bottom: 0.5rem;
    }

    .hidden {
      display: none !important;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    // Auto-load debug data on connect
    void this.loadDebugData();
  }

  private async loadDebugData(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const response = await fetch('/auth/debug', {
        credentials: 'include',
      });

      if (response.status === 404) {
        // Debug mode not available
        this.debugAvailable = false;
        return;
      }

      if (!response.ok) {
        throw new Error(`HTTP ${response.status}: ${response.statusText}`);
      }

      this.debugData = (await response.json()) as DebugData;
      this.debugAvailable = true;
    } catch (err) {
      console.error('Failed to load debug data:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load debug data';
    } finally {
      this.loading = false;
    }
  }

  private togglePanel(): void {
    this.expanded = !this.expanded;
    if (this.expanded && !this.debugData) {
      void this.loadDebugData();
    }
  }

  override render() {
    // Hide if debug mode is not available
    if (!this.debugAvailable) {
      return html``;
    }

    return html`
      <button
        class="toggle-button ${this.error ? 'error' : ''}"
        @click=${() => this.togglePanel()}
      >
        <span>${this.expanded ? 'Hide' : 'Show'} Debug</span>
      </button>

      <div class="panel ${this.expanded ? 'expanded' : ''}">
        <div class="panel-header">
          <h3>Auth Debug Panel</h3>
          <button @click=${() => this.togglePanel()}>X</button>
        </div>

        <div class="panel-content">
          ${this.error ? html`<div class="error-message">${this.error}</div>` : ''}
          ${this.loading ? html`<div>Loading...</div>` : this.renderDebugData()}

          <button
            class="refresh-button"
            ?disabled=${this.loading}
            @click=${() => this.loadDebugData()}
          >
            ${this.loading ? 'Loading...' : 'Refresh'}
          </button>
        </div>
      </div>
    `;
  }

  private renderDebugData() {
    if (!this.debugData) {
      return html`<div>No data loaded</div>`;
    }

    const data = this.debugData;

    return html`
      <div class="section">
        <div class="section-title">Authentication</div>
        <div class="info-row">
          <span class="info-label">State User</span>
          <span class="info-value ${data.auth.stateUser ? 'success' : 'error'}">
            ${data.auth.stateUser?.email || 'None'}
          </span>
        </div>
        <div class="info-row">
          <span class="info-label">Session User</span>
          <span class="info-value ${data.auth.sessionUser ? 'success' : 'error'}">
            ${data.auth.sessionUser?.email || 'None'}
          </span>
        </div>
        <div class="info-row">
          <span class="info-label">Dev Token</span>
          <span class="info-value">${data.auth.devToken}</span>
        </div>
        <div class="info-row">
          <span class="info-label">Dev Auth Enabled</span>
          <span class="info-value">${data.auth.devAuthEnabled ? 'Yes' : 'No'}</span>
        </div>
      </div>

      <div class="section">
        <div class="section-title">Session</div>
        <div class="info-row">
          <span class="info-label">Exists</span>
          <span class="info-value ${data.session.exists ? 'success' : 'error'}">
            ${data.session.exists ? 'Yes' : 'No'}
          </span>
        </div>
        <div class="info-row">
          <span class="info-label">Is New</span>
          <span class="info-value ${data.session.isNew ? 'warning' : 'success'}">
            ${data.session.isNew ? 'Yes' : 'No'}
          </span>
        </div>
        <div class="info-row">
          <span class="info-label">Has User</span>
          <span class="info-value ${data.session.hasUser ? 'success' : 'error'}">
            ${data.session.hasUser ? 'Yes' : 'No'}
          </span>
        </div>
        <div class="info-row">
          <span class="info-label">Keys</span>
          <span class="info-value">${data.session.keys.join(', ') || 'None'}</span>
        </div>
      </div>

      <div class="section">
        <div class="section-title">Cookies</div>
        <div class="info-row">
          <span class="info-label">Header</span>
          <span class="info-value">${data.cookies.header}</span>
        </div>
        <div class="info-row">
          <span class="info-label">Count</span>
          <span class="info-value">${data.cookies.count}</span>
        </div>
        <div class="info-row">
          <span class="info-label">Session Cookie</span>
          <span class="info-value ${data.cookies.hasSessionCookie ? 'success' : 'error'}">
            ${data.cookies.hasSessionCookie ? 'Present' : 'Missing'}
          </span>
        </div>
        <div class="info-row">
          <span class="info-label">Names</span>
          <span class="info-value">${data.cookies.names.join(', ') || 'None'}</span>
        </div>
      </div>

      <div class="section">
        <div class="section-title">Configuration</div>
        <div class="info-row">
          <span class="info-label">Production</span>
          <span class="info-value">${data.config.production ? 'Yes' : 'No'}</span>
        </div>
        <div class="info-row">
          <span class="info-label">Debug</span>
          <span class="info-value">${data.config.debug ? 'Yes' : 'No'}</span>
        </div>
        <div class="info-row">
          <span class="info-label">Base URL</span>
          <span class="info-value">${data.config.baseUrl}</span>
        </div>
        <div class="info-row">
          <span class="info-label">Hub API URL</span>
          <span class="info-value">${data.config.hubApiUrl}</span>
        </div>
        <div class="info-row">
          <span class="info-label">Google OAuth</span>
          <span class="info-value">${data.config.hasGoogleOAuth ? 'Configured' : 'Not configured'}</span>
        </div>
        <div class="info-row">
          <span class="info-label">GitHub OAuth</span>
          <span class="info-value">${data.config.hasGitHubOAuth ? 'Configured' : 'Not configured'}</span>
        </div>
      </div>

      <div class="section">
        <div class="section-title">Timestamp</div>
        <div class="info-row">
          <span class="info-label">Server Time</span>
          <span class="info-value">${data.timestamp}</span>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-debug-panel': ScionDebugPanel;
  }
}
