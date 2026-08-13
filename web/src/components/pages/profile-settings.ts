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
 * Profile Settings page
 *
 * Provides user-facing settings including browser push notification
 * preferences via the Notification API.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch } from '../../client/api.js';
import '../shared/subscription-manager.js';

const STORAGE_KEY = 'scion-push-notifications';

@customElement('scion-page-profile-settings')
export class ScionPageProfileSettings extends LitElement {
  @state()
  private _pushEnabled = false;

  @state()
  private _permissionState: NotificationPermission | 'unsupported' = 'default';

  @state()
  private _gcloudADCAvailable = false;

  @state()
  private _autoInjectGcloudADC = false;

  @state()
  private _isWorkstation = false;

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

    .setting-row {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      gap: 1rem;
    }

    .setting-info {
      flex: 1;
    }

    .setting-label {
      font-size: 0.875rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .setting-description {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0;
      line-height: 1.5;
    }

    .setting-control {
      flex-shrink: 0;
      padding-top: 0.125rem;
    }

    .permission-status {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-top: 0.75rem;
      padding: 0.5rem 0.75rem;
      border-radius: 0.375rem;
      font-size: 0.8125rem;
    }

    .permission-status sl-icon {
      font-size: 1rem;
      flex-shrink: 0;
    }

    .status-granted {
      background: var(--sl-color-success-50, #f0fdf4);
      color: var(--sl-color-success-700, #15803d);
      border: 1px solid var(--sl-color-success-200, #bbf7d0);
    }

    .status-denied {
      background: var(--sl-color-warning-50, #fffbeb);
      color: var(--sl-color-warning-700, #b45309);
      border: 1px solid var(--sl-color-warning-200, #fde68a);
    }

    .status-default {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      border: 1px solid var(--scion-border, #e2e8f0);
    }

    .status-unsupported {
      background: var(--sl-color-danger-50, #fef2f2);
      color: var(--sl-color-danger-700, #b91c1c);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    this._initNotificationState();
    void this._loadSystemStatus();
  }

  private async _loadSystemStatus(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/system/status');
      if (res.ok) {
        const data = (await res.json()) as {
          gcloudADCAvailable?: boolean;
          autoInjectGcloudADC?: boolean;
          workstation?: boolean;
        };
        this._gcloudADCAvailable = data.gcloudADCAvailable ?? false;
        this._autoInjectGcloudADC = data.autoInjectGcloudADC ?? false;
        this._isWorkstation = data.workstation ?? false;
      }
    } catch {
      // Non-critical — leave defaults
    }
  }

  private _initNotificationState(): void {
    if (!('Notification' in window)) {
      this._permissionState = 'unsupported';
      this._pushEnabled = false;
      return;
    }

    this._permissionState = Notification.permission;
    const stored = localStorage.getItem(STORAGE_KEY);
    this._pushEnabled = stored === 'true' && Notification.permission === 'granted';
  }

  private async _handleToggle(e: Event): Promise<void> {
    const target = e.target as HTMLInputElement & { checked: boolean };
    const wantsEnabled = target.checked;

    if (!wantsEnabled) {
      localStorage.setItem(STORAGE_KEY, 'false');
      this._pushEnabled = false;
      return;
    }

    if (!('Notification' in window)) {
      target.checked = false;
      this._permissionState = 'unsupported';
      return;
    }

    const permission = await Notification.requestPermission();
    this._permissionState = permission;

    if (permission === 'granted') {
      localStorage.setItem(STORAGE_KEY, 'true');
      this._pushEnabled = true;
    } else {
      target.checked = false;
      this._pushEnabled = false;
    }
  }

  private async _handleADCToggle(e: Event): Promise<void> {
    const target = e.target as HTMLInputElement & { checked: boolean };
    const enabled = target.checked;
    this._autoInjectGcloudADC = enabled;
    try {
      const res = await apiFetch('/api/v1/system/workstation-settings', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ auto_inject_gcloud_adc: enabled }),
      });
      if (!res.ok) {
        throw new Error('Failed to update workstation settings');
      }
    } catch {
      // Revert on failure
      this._autoInjectGcloudADC = !enabled;
      target.checked = !enabled;
    }
  }

  private _renderPermissionStatus() {
    switch (this._permissionState) {
      case 'granted':
        return html`
          <div class="permission-status status-granted">
            <sl-icon name="check-circle"></sl-icon>
            Browser notifications are allowed.
          </div>
        `;
      case 'denied':
        return html`
          <div class="permission-status status-denied">
            <sl-icon name="exclamation-triangle"></sl-icon>
            Notifications are blocked. Update your browser site settings to allow notifications.
          </div>
        `;
      case 'unsupported':
        return html`
          <div class="permission-status status-unsupported">
            <sl-icon name="x-circle"></sl-icon>
            Your browser does not support notifications.
          </div>
        `;
      case 'default':
        return html`
          <div class="permission-status status-default">
            <sl-icon name="info-circle"></sl-icon>
            Enable the toggle to request notification permission.
          </div>
        `;
      default:
        return nothing;
    }
  }

  override render() {
    const isDisabled =
      this._permissionState === 'unsupported' || this._permissionState === 'denied';

    return html`
      <div class="page-header">
        <div class="page-header-info">
          <h1>Notifications & Settings</h1>
          <p>Manage your notification subscriptions and preferences.</p>
        </div>
      </div>

      <div class="settings-card">
        <h2 class="section-title">
          <sl-icon name="bell"></sl-icon>
          Notifications
        </h2>

        <div class="setting-row">
          <div class="setting-info">
            <p class="setting-label">Enable Push Notifications</p>
            <p class="setting-description">
              Receive browser notifications when agents complete tasks, encounter errors, or need
              your input.
            </p>
          </div>
          <div class="setting-control">
            <sl-switch
              ?checked=${this._pushEnabled}
              ?disabled=${isDisabled}
              @sl-change=${this._handleToggle}
            ></sl-switch>
          </div>
        </div>

        ${this._renderPermissionStatus()}
      </div>

      ${this._gcloudADCAvailable && this._isWorkstation
        ? html`
            <div class="settings-card">
              <h2 class="section-title">
                <sl-icon name="cloud"></sl-icon>
                GCP Credentials
              </h2>

              <div class="setting-row">
                <div class="setting-info">
                  <p class="setting-label">
                    Automatically inject gcloud credentials into agent containers
                  </p>
                  <p class="setting-description">
                    When enabled, agents launched in workstation mode will have access to your local
                    gcloud Application Default Credentials.
                  </p>
                </div>
                <div class="setting-control">
                  <sl-switch
                    ?checked=${this._autoInjectGcloudADC}
                    @sl-change=${this._handleADCToggle}
                  ></sl-switch>
                </div>
              </div>
            </div>
          `
        : nothing}

      <scion-subscription-manager compact></scion-subscription-manager>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-profile-settings': ScionPageProfileSettings;
  }
}
