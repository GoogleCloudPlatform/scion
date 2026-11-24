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

import { apiFetch, apiFetchAllPages, extractApiError } from '../../client/api.js';
import type { Agent } from '../../shared/types.js';
import { can } from '../../shared/types.js';
import {
  canShowPushNotification,
  enablePushWithPermission,
  pushPermission,
  setPushOptIn,
  PUSH_PREFERENCE_EVENT,
  type PushCategory,
  type PushPermissionState,
} from '../../client/push-preference.js';
import { isChimeEnabled, setChimeEnabled } from '../../utils/audio.js';
import {
  areViewModeShortcutsEnabled,
  isViewModeShortcutsStorageEvent,
  setViewModeShortcutsEnabled,
  VIEW_MODE_SHORTCUTS_CHANGED_EVENT,
} from '../../client/view-mode-shortcuts.js';
import { isMacPlatform } from '../../utils/platform.js';
import { setPreferredTimeZone, browserTimeZone } from '../../utils/time.js';
import {
  decisionsEnabled,
  execContainersEnabled,
  FEATURE_FLAGS_CHANGED_EVENT,
} from '../../utils/feature-flags.js';
import { setPreselectRecommendedChoices } from '../../client/decision-preferences.js';
import '../shared/subscription-manager.js';
import '../shared/timezone-picker.js';
import type { TimezoneChangeDetail } from '../shared/timezone-picker.js';

type ExecContainerState = 'ready' | 'starting' | 'stopped' | 'suspended' | 'unavailable';

interface ExecContainerAgent {
  id: string;
  slug: string;
  name: string;
  projectId: string;
  projectName: string;
  phase: string;
  activity?: string;
  homePersistent?: boolean;
}

interface ExecContainerResponse {
  configured: boolean;
  state?: ExecContainerState;
  agent?: ExecContainerAgent;
  reason?: string;
}

const EXEC_CONTAINER_UNAVAILABLE_FALLBACK =
  'Your registered execution container is no longer available. Register another.';

function isShellCandidate(agent: Agent): boolean {
  const hc = agent.harnessConfig || agent.appliedConfig?.harnessConfig;
  if (hc !== 'shell') return false;
  if ((agent.phase as string) === 'deleted') return false;
  if (agent._capabilities && !can(agent._capabilities, 'attach')) return false;
  return true;
}

function execContainerStateBadge(state: ExecContainerState | undefined): {
  variant: 'success' | 'primary' | 'neutral' | 'warning' | 'danger';
  label: string;
} {
  switch (state) {
    case 'ready':
      return { variant: 'success', label: 'Ready' };
    case 'starting':
      return { variant: 'primary', label: 'Starting' };
    case 'stopped':
      return { variant: 'neutral', label: 'Stopped' };
    case 'suspended':
      return { variant: 'warning', label: 'Suspended' };
    case 'unavailable':
      return { variant: 'danger', label: 'Unavailable' };
    default:
      return { variant: 'neutral', label: state || 'Unknown' };
  }
}

@customElement('scion-page-profile-settings')
export class ScionPageProfileSettings extends LitElement {
  /** Chat message popups: opted in and permitted. */
  @state()
  private _chatPushEnabled = false;

  /** Agent event popups: opted in and permitted. */
  @state()
  private _agentPushEnabled = false;

  @state()
  private _permissionState: PushPermissionState = 'default';

  @state()
  private _chimeEnabled = isChimeEnabled();

  @state()
  private _viewModeShortcutsEnabled = areViewModeShortcutsEnabled();

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
   * `preferences.preselectRecommendedChoices` (ptone/scion#7842): whether
   * multi-decision cards start with recommended choices selected.
   * Shown only while web.decisions is on.
   */
  @state()
  private _preselectRecommended = false;

  @state()
  private _preselectError: string | null = null;

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

  // Execution container registration (`hub.exec_containers`, ptone/scion#8819).
  @state()
  private _execContainer: ExecContainerResponse | null = null;

  @state()
  private _execContainerLoading = false;

  @state()
  private _execContainerSaving = false;

  @state()
  private _execContainerError: string | null = null;

  @state()
  private _execContainerPicking = false;

  @state()
  private _execContainerCandidates: Agent[] = [];

  @state()
  private _execContainerCandidatesLoading = false;

  @state()
  private _execContainerSelectedId = '';

  private _execContainerLoaded = false;

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
      border-radius: var(--scion-radius-lg);
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
      border-radius: calc(var(--scion-radius) * 0.75);
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

    .exec-container-summary {
      display: flex;
      align-items: center;
      justify-content: space-between;
      flex-wrap: wrap;
      gap: 0.75rem 1rem;
      margin-top: 0.75rem;
      padding: 0.75rem 1rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius);
    }

    .exec-container-info {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.5rem 0.75rem;
      min-width: 0;
    }

    .exec-container-name {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      text-decoration: none;
    }

    .exec-container-name:hover {
      color: var(--scion-primary, #3b82f6);
      text-decoration: underline;
    }

    .exec-container-project {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .exec-container-actions {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      flex-shrink: 0;
    }

    .exec-container-picker {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.75rem;
      margin-top: 0.75rem;
    }

    .exec-container-picker sl-select {
      flex: 1 1 18rem;
      max-width: 24rem;
    }

    .exec-container-create-link {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
    }

    .exec-container-create-link:hover {
      text-decoration: underline;
    }

    .exec-container-loading {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-top: 0.75rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }
  `;

  private readonly _onPushPreferenceChanged = (): void => this._initNotificationState();

  /** Re-reads the view-mode shortcuts preference after a change in this tab. */
  private readonly _onViewModeShortcutsChanged = (): void => {
    this._viewModeShortcutsEnabled = areViewModeShortcutsEnabled();
  };

  /** Re-reads the view-mode shortcuts preference after another tab changes it. */
  private readonly _onViewModeShortcutsStorage = (e: StorageEvent): void => {
    if (!isViewModeShortcutsStorageEvent(e)) return;
    this._viewModeShortcutsEnabled = areViewModeShortcutsEnabled();
  };

  /** Re-renders when feature flags update (e.g. server flags resolve after mount). */
  private readonly _onFeatureFlagsChanged = (): void => {
    if (execContainersEnabled() && !this._execContainerLoaded) {
      void this._loadExecContainer();
    }
    this.requestUpdate();
  };

  override connectedCallback(): void {
    super.connectedCallback();
    this._initNotificationState();
    // The tray carries the same toggle; whichever one the user flips, both
    // must show the same answer.
    window.addEventListener(PUSH_PREFERENCE_EVENT, this._onPushPreferenceChanged);
    // The header reads the same view-mode shortcuts preference; keep this
    // toggle in step when it changes in this tab or another one.
    this._viewModeShortcutsEnabled = areViewModeShortcutsEnabled();
    window.addEventListener(VIEW_MODE_SHORTCUTS_CHANGED_EVENT, this._onViewModeShortcutsChanged);
    window.addEventListener('storage', this._onViewModeShortcutsStorage);
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this._onFeatureFlagsChanged);
    void this._loadSystemStatus();
    void this._loadDisplayTimezone();
    if (execContainersEnabled()) {
      void this._loadExecContainer();
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener(PUSH_PREFERENCE_EVENT, this._onPushPreferenceChanged);
    window.removeEventListener(VIEW_MODE_SHORTCUTS_CHANGED_EVENT, this._onViewModeShortcutsChanged);
    window.removeEventListener('storage', this._onViewModeShortcutsStorage);
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this._onFeatureFlagsChanged);
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
        preferences?: { timezone?: string; preselectRecommendedChoices?: boolean };
      };
      this._userId = data.id ?? '';
      this._displayTimezone = data.preferences?.timezone ?? '';
      this._preselectRecommended = data.preferences?.preselectRecommendedChoices === true;
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
    this._chatPushEnabled = canShowPushNotification('chat');
    this._agentPushEnabled = canShowPushNotification('agent');
  }

  private async _handleToggle(e: Event, category: PushCategory): Promise<void> {
    const target = e.target as HTMLInputElement & { checked: boolean };
    const wantsEnabled = target.checked;

    if (!wantsEnabled) {
      setPushOptIn(category, false);
      this._initNotificationState();
      return;
    }

    // Requesting permission from the change handler keeps it inside the user
    // gesture, which is the only place browsers accept the request.
    this._permissionState = await enablePushWithPermission(category);
    this._initNotificationState();
    target.checked = category === 'chat' ? this._chatPushEnabled : this._agentPushEnabled;
  }

  private _handleChimeToggle(e: Event): void {
    const target = e.target as HTMLInputElement & { checked: boolean };
    setChimeEnabled(target.checked);
    this._chimeEnabled = isChimeEnabled();
  }

  private _handleViewModeShortcutsToggle(e: Event): void {
    const target = e.target as HTMLInputElement & { checked: boolean };
    setViewModeShortcutsEnabled(target.checked);
    this._viewModeShortcutsEnabled = areViewModeShortcutsEnabled();
  }

  /**
   * Saves the decision-card preselect preference with a per-key
   * `preferences` PATCH and applies it to open cards at once; reverts the
   * switch when the PATCH fails.
   */
  private async _handlePreselectToggle(e: Event): Promise<void> {
    const target = e.target as HTMLInputElement & { checked: boolean };
    const enabled = target.checked;
    this._preselectError = null;
    const revert = (message: string): void => {
      target.checked = !enabled;
      this._preselectRecommended = !enabled;
      this._preselectError = message;
    };
    if (!this._userId) {
      revert('Failed to update the setting');
      return;
    }
    this._preselectRecommended = enabled;
    try {
      const res = await apiFetch(`/api/v1/users/${encodeURIComponent(this._userId)}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ preferences: { preselectRecommendedChoices: enabled } }),
      });
      if (!res.ok) {
        revert(await extractApiError(res, 'Failed to update the setting'));
        return;
      }
      setPreselectRecommendedChoices(enabled);
    } catch {
      revert('Failed to update the setting');
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

  private async _loadExecContainer(): Promise<void> {
    this._execContainerLoaded = true;
    this._execContainerLoading = true;
    this._execContainerError = null;
    try {
      const res = await apiFetch('/api/v1/users/me/exec-container');
      if (!res.ok) {
        this._execContainerError = await extractApiError(res, 'Failed to load execution container');
        return;
      }
      const data = (await res.json()) as ExecContainerResponse;
      this._execContainer = data;
      if (!data.configured) {
        void this._loadExecContainerCandidates();
      }
    } catch {
      this._execContainerError = 'Failed to load execution container';
    } finally {
      this._execContainerLoading = false;
    }
  }

  private async _loadExecContainerCandidates(): Promise<void> {
    this._execContainerCandidatesLoading = true;
    try {
      const agents = await apiFetchAllPages<Agent>('/api/v1/agents?limit=100', 'agents');
      this._execContainerCandidates = agents
        .filter(isShellCandidate)
        .sort((a, b) => (a.name || '').localeCompare(b.name || ''));
    } catch {
      this._execContainerCandidates = [];
    } finally {
      this._execContainerCandidatesLoading = false;
    }
  }

  private _handleExecContainerChangeStart(): void {
    this._execContainerPicking = true;
    this._execContainerError = null;
    this._execContainerSelectedId = '';
    void this._loadExecContainerCandidates();
  }

  private _handleExecContainerChangeCancel(): void {
    this._execContainerPicking = false;
    this._execContainerSelectedId = '';
    this._execContainerError = null;
  }

  private async _handleExecContainerSelect(e: Event): Promise<void> {
    const select = e.target as HTMLElement & { value: string };
    const agentId = select.value;
    if (!agentId) return;

    this._execContainerSelectedId = agentId;
    this._execContainerSaving = true;
    this._execContainerError = null;
    try {
      const res = await apiFetch('/api/v1/users/me/exec-container', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agentId }),
      });
      if (!res.ok) {
        this._execContainerError = await extractApiError(res, 'Failed to set execution container');
        this._execContainerSelectedId = '';
        select.value = '';
        return;
      }
      const data = (await res.json()) as ExecContainerResponse;
      this._execContainer = data;
      this._execContainerPicking = false;
      this._execContainerSelectedId = '';
    } catch {
      this._execContainerError = 'Failed to set execution container';
      this._execContainerSelectedId = '';
      select.value = '';
    } finally {
      this._execContainerSaving = false;
    }
  }

  private async _handleExecContainerClear(): Promise<void> {
    this._execContainerSaving = true;
    this._execContainerError = null;
    try {
      const res = await apiFetch('/api/v1/users/me/exec-container', {
        method: 'DELETE',
      });
      if (!res.ok) {
        this._execContainerError = await extractApiError(
          res,
          'Failed to clear execution container'
        );
        return;
      }
      this._execContainer = { configured: false };
      this._execContainerPicking = false;
      this._execContainerSelectedId = '';
      void this._loadExecContainerCandidates();
    } catch {
      this._execContainerError = 'Failed to clear execution container';
    } finally {
      this._execContainerSaving = false;
    }
  }

  private _renderExecContainerPicker(showCancel: boolean) {
    const candidates = this._execContainerCandidates;
    return html`
      <div class="exec-container-picker" data-testid="exec-container-picker">
        <sl-select
          class="exec-container-select"
          label="Execution container"
          placeholder=${this._execContainerCandidatesLoading
            ? 'Loading execution containers…'
            : candidates.length > 0
              ? 'Choose…'
              : 'No execution containers available'}
          .value=${this._execContainerSelectedId}
          ?disabled=${this._execContainerSaving ||
          this._execContainerCandidatesLoading ||
          candidates.length === 0}
          @sl-change=${(e: Event): void => {
            void this._handleExecContainerSelect(e);
          }}
        >
          ${candidates.map(
            (a) => html`
              <sl-option value=${a.id}>
                ${a.name}${a.project ? ` (${a.project})` : ''} — ${a.phase}
              </sl-option>
            `
          )}
        </sl-select>
        ${showCancel
          ? html`
              <sl-button
                size="small"
                variant="default"
                ?disabled=${this._execContainerSaving}
                @click=${() => this._handleExecContainerChangeCancel()}
              >
                Cancel
              </sl-button>
            `
          : nothing}
        <a
          href="/agents/new?harness=shell"
          class="exec-container-create-link"
          data-testid="exec-container-create-link"
        >
          <sl-icon name="plus-lg"></sl-icon>
          Start a new execution container
        </a>
      </div>
    `;
  }

  private _renderExecContainerCard() {
    if (!execContainersEnabled()) return nothing;

    const ec = this._execContainer;
    const isConfigured = ec?.configured === true;
    const isUnavailable = isConfigured && ec?.state === 'unavailable';
    const agent = ec?.agent;

    return html`
      <div class="settings-card" data-testid="exec-container-card">
        <h2 class="section-title">
          <sl-icon name="terminal"></sl-icon>
          Execution container
        </h2>

        <p class="setting-description">
          Your default execution container is a shell-only container where you log in to CLI tools
          for approved commands. Keep credentials in <code>$HOME</code>, never in
          <code>/workspace</code>.
        </p>

        ${this._execContainerLoading
          ? html`
              <div class="exec-container-loading">
                <sl-spinner></sl-spinner>
                Loading execution container…
              </div>
            `
          : isUnavailable
            ? html`
                <div
                  class="permission-status status-denied"
                  data-testid="exec-container-unavailable"
                >
                  <sl-icon name="exclamation-triangle"></sl-icon>
                  <span>${ec.reason || EXEC_CONTAINER_UNAVAILABLE_FALLBACK}</span>
                </div>
                <div class="exec-container-picker">
                  <sl-button
                    size="small"
                    variant="default"
                    data-testid="exec-container-clear"
                    ?loading=${this._execContainerSaving}
                    ?disabled=${this._execContainerSaving}
                    @click=${() => void this._handleExecContainerClear()}
                  >
                    Clear
                  </sl-button>
                  ${!this._execContainerPicking
                    ? html`
                        <sl-button
                          size="small"
                          variant="default"
                          data-testid="exec-container-change"
                          ?disabled=${this._execContainerSaving}
                          @click=${() => this._handleExecContainerChangeStart()}
                        >
                          Change
                        </sl-button>
                      `
                    : nothing}
                </div>
                ${this._execContainerPicking ? this._renderExecContainerPicker(true) : nothing}
              `
            : isConfigured && agent
              ? html`
                  <div class="exec-container-summary" data-testid="exec-container-configured">
                    <div class="exec-container-info">
                      <a href="/agents/${agent.id}" class="exec-container-name">${agent.name}</a>
                      ${agent.projectName || agent.projectId
                        ? html`
                            <span class="exec-container-project">
                              <sl-icon name="folder"></sl-icon>
                              ${agent.projectName || agent.projectId}
                            </span>
                          `
                        : nothing}
                      ${(() => {
                        const badge = execContainerStateBadge(ec.state);
                        return html`<sl-badge
                          variant=${badge.variant}
                          data-testid="exec-container-state"
                          >${badge.label}</sl-badge
                        >`;
                      })()}
                    </div>
                    <div class="exec-container-actions">
                      ${!this._execContainerPicking
                        ? html`
                            <sl-button
                              size="small"
                              variant="default"
                              data-testid="exec-container-change"
                              ?disabled=${this._execContainerSaving}
                              @click=${() => this._handleExecContainerChangeStart()}
                            >
                              Change
                            </sl-button>
                          `
                        : nothing}
                      <sl-button
                        size="small"
                        variant="default"
                        data-testid="exec-container-clear"
                        ?loading=${this._execContainerSaving}
                        ?disabled=${this._execContainerSaving}
                        @click=${() => void this._handleExecContainerClear()}
                      >
                        Clear
                      </sl-button>
                    </div>
                  </div>
                  ${agent.homePersistent === false
                    ? html`
                        <div
                          class="permission-status status-denied"
                          data-testid="exec-container-home-warning"
                        >
                          <sl-icon name="exclamation-triangle"></sl-icon>
                          Home directory does not persist across restarts on this runtime — logins
                          will not survive a restart.
                        </div>
                      `
                    : nothing}
                  ${this._execContainerPicking ? this._renderExecContainerPicker(true) : nothing}
                `
              : this._renderExecContainerPicker(false)}
        ${this._execContainerError
          ? html`
              <div class="permission-status status-unsupported" data-testid="exec-container-error">
                <sl-icon name="x-circle"></sl-icon>
                <span>${this._execContainerError}</span>
              </div>
            `
          : nothing}
      </div>
    `;
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
            Turn on an alert toggle to request notification permission.
          </div>
        `;
      default:
        return nothing;
    }
  }

  override render() {
    const isDisabled =
      this._permissionState === 'unsupported' || this._permissionState === 'denied';

    // prettier-ignore
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
            <p class="setting-label">Chat message alerts</p>
            <p class="setting-description">
              Show a browser notification for each new message in your direct messages and in
              threads you are a member of. Muted conversations stay silent.
            </p>
          </div>
          <div class="setting-control">
            <sl-switch
              data-push-category="chat"
              aria-label="Chat message alerts"
              ?checked=${this._chatPushEnabled}
              ?disabled=${isDisabled}
              @sl-change=${(e: Event): Promise<void> => this._handleToggle(e, 'chat')}
            ></sl-switch>
          </div>
        </div>

        <div class="setting-row">
          <div class="setting-info">
            <p class="setting-label">Agent event alerts</p>
            <p class="setting-description">
              Show a browser notification when agents complete tasks, encounter errors, or need your
              input.
            </p>
          </div>
          <div class="setting-control">
            <sl-switch
              data-push-category="agent"
              aria-label="Agent event alerts"
              ?checked=${this._agentPushEnabled}
              ?disabled=${isDisabled}
              @sl-change=${(e: Event): Promise<void> => this._handleToggle(e, 'agent')}
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
              "Auto" to follow your browser's zone (currently ${browserTimeZone()}). Only a
              pinned time zone is shared with agents you message; Auto is used for display only,
              and neither changes how agent containers are configured.
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

      <div class="settings-card">
        <h2 class="section-title">
          <sl-icon name="keyboard"></sl-icon>
          Keyboard shortcuts
        </h2>

        <div class="setting-row">
          <div class="setting-info">
            <p class="setting-label">View mode shortcuts</p>
            <p class="setting-description">
              Switch between Console, Chat and Terminals with
              ${isMacPlatform() ? '⌘1, ⌘2 and ⌘3' : 'Ctrl+1, Ctrl+2 and Ctrl+3'}. When off, these
              keys keep their usual browser behaviour.
            </p>
          </div>
          <div class="setting-control">
            <sl-switch
              class="view-mode-shortcuts-switch"
              ?checked=${this._viewModeShortcutsEnabled}
              @sl-change=${this._handleViewModeShortcutsToggle}
            ></sl-switch>
          </div>
        </div>
      </div>

      ${this._renderExecContainerCard()}
      ${decisionsEnabled()
        ? html`
            <div class="settings-card">
              <h2 class="section-title">
                <sl-icon name="list-check"></sl-icon>
                Decisions
              </h2>

              <div class="setting-row">
                <div class="setting-info">
                  <p class="setting-label">Preselect recommended choices</p>
                  <p class="setting-description">
                    When a message asks several questions at once, start with each recommended
                    choice selected. When off, recommended choices are only outlined and every
                    question needs a click before you can submit.
                  </p>
                </div>
                <div class="setting-control">
                  <sl-switch
                    class="preselect-recommended-switch"
                    aria-label="Preselect recommended choices"
                    ?checked=${this._preselectRecommended}
                    @sl-change=${(e: Event): Promise<void> => this._handlePreselectToggle(e)}
                  ></sl-switch>
                </div>
              </div>

              ${this._preselectError
                ? html`
                    <div class="permission-status status-denied">
                      <sl-icon name="exclamation-triangle"></sl-icon>
                      ${this._preselectError}
                    </div>
                  `
                : nothing}
            </div>
          `
        : nothing}
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
