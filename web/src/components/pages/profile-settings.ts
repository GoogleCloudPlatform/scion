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
import { keyed } from 'lit/directives/keyed.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import {
  canShowPushNotification,
  enablePushWithPermission,
  pushPermission,
  setPushOptIn,
  PUSH_PREFERENCE_EVENT,
  type PushPermissionState,
} from '../../client/push-preference.js';
import { isChimeEnabled, setChimeEnabled } from '../../utils/audio.js';
import { setPreferredTimeZone, browserTimeZone } from '../../utils/time.js';
import '../shared/subscription-manager.js';
import '../shared/timezone-picker.js';
import type { TimezoneChangeDetail } from '../shared/timezone-picker.js';

@customElement('scion-page-profile-settings')
export class ScionPageProfileSettings extends LitElement {
  @state()
  private _pushEnabled = false;

  @state()
  private _permissionState: PushPermissionState = 'default';

  @state()
  private _chimeEnabled = isChimeEnabled();

  @state()
  private _gcloudADCAvailable = false;

  @state()
  private _autoInjectGcloudADC = false;

  @state()
  private _isWorkstation = false;

  // Display timezone (`preferences.timezone`, design.md §3 A "Fate of the
  // card"). Visible to every signed-in user: it only affects how this user
  // *sees* times, never agent containers.
  @state()
  private _userId = '';

  @state()
  private _displayTimezone = '';

  @state()
  private _displayTimezoneSaving = false;

  @state()
  private _displayTimezoneError: string | null = null;

  @state()
  private _displayTimezoneSaved = false;

  /**
   * Bumped to force the picker to remount after a failed PATCH or the
   * no-user-id case (review R2-2, replacing R1-5's `@query`-ref approach).
   *
   * Task 12's `<scion-timezone-picker>` never mutates its own `value`
   * property on selection — only its internal, unexported `searchQuery`
   * text — so even a direct `picker.value = x` write (bypassing Lit's
   * property-binding diff) changes a property the picker doesn't read back
   * into what it displays. There is no supported way to ask the picker to
   * resync from outside without either modifying it (out of bounds for
   * this PR — task 12 owns that file) or relying on its private internals.
   * `keyed()` sidesteps both: bumping the key unmounts the stale instance
   * and mounts a fresh one, which always initializes its display from
   * `.value` in `willUpdate`'s `!this.hasUpdated` branch.
   */
  @state()
  private _pickerRevision = 0;

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

    scion-timezone-picker {
      display: block;
      width: 18rem;
      max-width: 100%;
    }
  `;

  private readonly _onPushPreferenceChanged = (): void => this._initNotificationState();

  override connectedCallback(): void {
    super.connectedCallback();
    this._initNotificationState();
    // The tray carries the same toggle; whichever one the user flips, both
    // must show the same answer.
    window.addEventListener(PUSH_PREFERENCE_EVENT, this._onPushPreferenceChanged);
    void this._loadSystemStatus();
    void this._loadDisplayTimezone();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener(PUSH_PREFERENCE_EVENT, this._onPushPreferenceChanged);
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

  /**
   * Loads the signed-in user's id and display-timezone preference from
   * `/auth/me`, which returns `preferences` live (no session caching) for
   * the authenticated caller only.
   */
  private async _loadDisplayTimezone(): Promise<void> {
    try {
      const res = await apiFetch('/auth/me');
      if (!res.ok) return;
      const data = (await res.json()) as {
        id?: string;
        preferences?: { timezone?: string };
      };
      this._userId = data.id ?? '';
      this._displayTimezone = data.preferences?.timezone ?? '';
    } catch {
      // Non-critical — the card still renders; saving just has nothing to
      // PATCH against until a reload succeeds.
    }
  }

  /**
   * Saves the display-timezone preference via a per-key `preferences`
   * merge (backend contract: tz-refactor task 10, ptone/scion#2526) and
   * applies it to the effective-zone store immediately, with no reload
   * (AC4/AC5). On failure — including when `_userId` hasn't loaded yet —
   * resets the picker's displayed value back to the last-saved preference
   * (review R1-5/R2-2), since the picker already updated its own
   * typed/selected text before this handler ran.
   */
  private async _handleZoneChange(e: CustomEvent<TimezoneChangeDetail>): Promise<void> {
    const value = e.detail.timezone;
    if (!this._userId) {
      this._resetPickerDisplay();
      return;
    }

    this._displayTimezoneSaving = true;
    this._displayTimezoneError = null;
    this._displayTimezoneSaved = false;

    try {
      const res = await apiFetch(`/api/v1/users/${encodeURIComponent(this._userId)}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ preferences: { timezone: value } }),
      });
      if (!res.ok) {
        this._displayTimezoneError = await extractApiError(res, 'Failed to update timezone');
        this._resetPickerDisplay();
        return;
      }
      this._displayTimezone = value;
      setPreferredTimeZone(value);
      this._displayTimezoneSaved = true;
    } catch {
      this._displayTimezoneError = 'Failed to update timezone';
      this._resetPickerDisplay();
    } finally {
      this._displayTimezoneSaving = false;
    }
  }

  /**
   * Forces the picker to remount (review R2-2) so it re-displays the
   * last-saved `_displayTimezone` instead of whatever the user just typed
   * or selected — see `_pickerRevision`'s doc comment for why a property
   * write alone can't do this for task 12's picker.
   */
  private _resetPickerDisplay(): void {
    this._pickerRevision++;
  }

  private _initNotificationState(): void {
    this._permissionState = pushPermission();
    this._pushEnabled = canShowPushNotification();
  }

  private async _handleToggle(e: Event): Promise<void> {
    const target = e.target as HTMLInputElement & { checked: boolean };
    const wantsEnabled = target.checked;

    if (!wantsEnabled) {
      setPushOptIn(false);
      this._initNotificationState();
      return;
    }

    // Requesting permission from the change handler keeps it inside the user
    // gesture, which is the only place browsers accept the request.
    this._permissionState = await enablePushWithPermission();
    this._initNotificationState();
    target.checked = this._pushEnabled;
  }

  private _handleChimeToggle(e: Event): void {
    const target = e.target as HTMLInputElement & { checked: boolean };
    setChimeEnabled(target.checked);
    this._chimeEnabled = isChimeEnabled();
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

        <div class="setting-row">
          <div class="setting-info">
            <p class="setting-label">Chat chime sound</p>
            <p class="setting-description">Play a subtle chime when new chat messages arrive.</p>
          </div>
          <div class="setting-control">
            <sl-switch ?checked=${this._chimeEnabled} @sl-change=${this._handleChimeToggle}>
            </sl-switch>
          </div>
        </div>
      </div>

      <div class="settings-card">
        <h2 class="section-title">
          <sl-icon name="clock"></sl-icon>
          Display timezone
        </h2>

        <div class="setting-row">
          <div class="setting-info">
            <p class="setting-label">Times shown in</p>
            <p class="setting-description">
              Controls how times are displayed and how date/time inputs are interpreted in
              native chat and scheduling forms, and the rest of the UI follows over time. Choose
              "Auto" to follow your browser's zone (currently ${browserTimeZone()}); this never
              changes how agent containers are configured.
            </p>
          </div>
          <div class="setting-control">
            ${keyed(
              this._pickerRevision,
              html`
                <scion-timezone-picker
                  empty-label="Auto"
                  label="Display timezone"
                  .value=${this._displayTimezone}
                  ?disabled=${this._displayTimezoneSaving}
                  @timezone-change=${(e: CustomEvent<TimezoneChangeDetail>): void => {
                    void this._handleZoneChange(e);
                  }}
                ></scion-timezone-picker>
              `
            )}
          </div>
        </div>

        ${this._displayTimezoneError
          ? html`
              <div class="permission-status status-denied">
                <sl-icon name="exclamation-triangle"></sl-icon>
                ${this._displayTimezoneError}
              </div>
            `
          : nothing}
        ${this._displayTimezoneSaved
          ? html`
              <div class="permission-status status-granted">
                <sl-icon name="check-circle"></sl-icon>
                Display timezone updated.
              </div>
            `
          : nothing}
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
