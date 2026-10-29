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
 * "Server Version & Updates" card of the maintenance page: the running
 * version from update-status, the last update result, and (in release
 * mode) the channel and release picker over GET .../releases.
 *
 * The card holds no update state of its own. Update asks the page with an
 * `update-request` event; the page owns the confirm dialog, the run
 * request and the restart/reconnect state machine, and passes the
 * reconnect state back in as properties.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult, PropertyValues } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { renderReleaseNotesLink } from '../../utils/release-notes-link.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';

/** Result of the last update, from the system.last_update hub setting. */
export interface LastUpdate {
  runId: string;
  from: string;
  to: string;
  status: 'verified' | 'failed' | 'rolled_back' | (string & {});
  at: string;
}

/** An update whose hub restart is pending, from system.pending_update. */
export interface PendingUpdate {
  runId: string;
  from: string;
  to: string;
  snapshot?: string;
  strategy?: string;
  at: string;
}

/**
 * GET /api/v1/admin/maintenance/update-status. Later phases add fields;
 * unknown fields are ignored.
 */
export interface UpdateStatus {
  current: {
    version: string;
    channel: string;
    commit: string;
    buildTime?: string;
    isGitBuild: boolean;
  };
  mode: { effective: 'git' | 'release'; repoConfigured: boolean; repoBranch?: string };
  channel: { effective: string; configured?: string; source: string };
  pin: string;
  installer: {
    strategy: 'symlink' | 'sudo-install' | 'unsupported' | (string & {});
    supported: boolean;
    reason?: string;
    migrateHint?: string;
  };
  updatePolicy: string;
  downgrade?: { floor?: string; snapshotsSupported?: boolean };
  pendingUpdate: PendingUpdate | null;
  lastUpdate: LastUpdate | null;
}

/** A release's class against the running version, computed by the hub. */
export type ReleaseClass =
  | 'same'
  | 'upgrade'
  | 'switch-newer'
  | 'switch-older'
  | 'downgrade'
  | 'below-floor';

/** One release of GET /api/v1/admin/maintenance/releases. */
export interface CatalogRelease {
  tag: string;
  publishedAt?: string;
  installable: boolean;
  reason?: string;
  class?: ReleaseClass | (string & {});
  local?: boolean;
  notesUrl?: string;
}

export interface CatalogChannel {
  latest: string | null;
  releases: CatalogRelease[];
}

/** GET /api/v1/admin/maintenance/releases. */
export interface ReleasesResponse {
  repo?: string;
  fetchedAt?: string;
  stale?: boolean;
  channels: Record<string, CatalogChannel | undefined>;
}

/** The release channels, in picker order. */
export const RELEASE_CHANNELS = ['nightly', 'preview', 'stable'] as const;

/** The picker value of the "Latest (<tag>)" entry. */
export const LATEST_VALUE = 'latest';

/** Detail of the card's `update-request` event. */
export interface UpdateRequestDetail {
  /** The concrete release tag to install. */
  target: string;
  /** The target's channel. */
  channel: string;
  /** 'latest' for the Latest entry, otherwise the tag. */
  pin: string;
  /** The target's class against the running version. */
  releaseClass: string;
}

export const LAST_UPDATE_LABELS: Record<string, string> = {
  verified: 'verified',
  failed: 'failed',
  rolled_back: 'rolled back',
};

/**
 * Classes the hub installs in this phase. Older releases (downgrade,
 * switch-older) stay disabled until downgrade safety lands.
 */
const SELECTABLE_CLASSES = new Set(['same', 'upgrade', 'switch-newer']);

const CHANNEL_SOURCE_DESCRIPTIONS: Record<string, string> = {
  selection: "chosen by an administrator's update",
  config: 'from the server settings',
  detected: 'detected from the running version',
};

/**
 * Where the effective channel came from (update-status channel.source),
 * for example "selection (chosen by an administrator's update)".
 */
export function channelSourceLabel(source: string | undefined): string {
  if (!source) return 'unknown';
  const desc = CHANNEL_SOURCE_DESCRIPTIONS[source];
  return desc ? `${source} (${desc})` : source;
}

/** The hub's pin (update-status pin): following latest, or pinned to a tag. */
export function pinLabel(pin: string | undefined): string {
  if (!pin || pin === LATEST_VALUE) return 'latest (follows the newest release)';
  return `pinned to ${pin} (automatic updates do not move it)`;
}

/** Whether a parsed body has the parts of a releases response the card uses. */
export function isReleasesResponse(data: unknown): data is ReleasesResponse {
  if (!data || typeof data !== 'object') return false;
  const channels = (data as { channels?: unknown }).channels;
  if (!channels || typeof channels !== 'object') return false;
  return Object.values(channels).every(
    (c) =>
      c === undefined ||
      (!!c && typeof c === 'object' && Array.isArray((c as { releases?: unknown }).releases))
  );
}

/**
 * Why a release cannot be picked, or null when it can. Not-installable,
 * below-floor and older releases are disabled; so is a class the page
 * does not know.
 */
export function releaseDisabledReason(r: CatalogRelease, floor?: string): string | null {
  if (!r.installable) {
    return `Not installable: ${r.reason || 'the release has no verifiable binary'}`;
  }
  switch (r.class) {
    case 'below-floor':
      return floor
        ? `Below this hub's downgrade floor (${floor})`
        : "Below this hub's downgrade floor";
    case 'downgrade':
    case 'switch-older':
      return 'Older than the running version: rolling back is not supported yet';
  }
  if (!r.class || !SELECTABLE_CLASSES.has(r.class)) {
    return 'The hub did not say how this release compares to the running version';
  }
  return null;
}

export interface ReleaseBadge {
  label: string;
  variant: 'primary' | 'success' | 'neutral' | 'warning' | 'danger';
  /** Tooltip text; set for badges that explain a disabled entry. */
  tooltip?: string;
}

/** The badges of a release entry: Current, Local, Older, Not installable, Below floor. */
export function releaseBadges(r: CatalogRelease, floor?: string): ReleaseBadge[] {
  const badges: ReleaseBadge[] = [];
  if (r.class === 'same') badges.push({ label: 'Current', variant: 'primary' });
  if (r.local) badges.push({ label: 'Local', variant: 'neutral' });
  if (r.class === 'downgrade' || r.class === 'switch-older') {
    badges.push({ label: 'Older', variant: 'warning', tooltip: releaseDisabledReason(r) ?? '' });
  }
  if (!r.installable) {
    badges.push({
      label: 'Not installable',
      variant: 'danger',
      tooltip: r.reason || 'The release has no verifiable binary',
    });
  }
  if (r.class === 'below-floor') {
    badges.push({
      label: 'Below floor',
      variant: 'danger',
      tooltip: releaseDisabledReason(r, floor) ?? '',
    });
  }
  return badges;
}

/** The alert shown when the installer cannot update this hub. */
export function renderInstallerUnsupported(st: UpdateStatus): TemplateResult {
  return html`
    <sl-alert variant="warning" open class="installer-unsupported">
      <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
      <strong>This hub cannot install updates itself.</strong>
      ${st.installer.reason
        ? html`<div class="installer-reason">${st.installer.reason}</div>`
        : nothing}
      ${st.installer.migrateHint
        ? html`<div class="installer-hint">${st.installer.migrateHint}</div>`
        : nothing}
    </sl-alert>
  `;
}

const RELEASES_PATH = '/api/v1/admin/maintenance/releases';

@customElement('scion-admin-maintenance-update-card')
export class ScionAdminMaintenanceUpdateCard extends LitElement {
  /** Re-renders formatted times when the display zone changes. */
  readonly _zone = new DisplayZoneController(this);

  /** The last good update-status, or null while loading or after an error. */
  @property({ attribute: false })
  status: UpdateStatus | null = null;

  /** Why update-status could not be loaded. */
  @property({ attribute: false })
  statusError: string | null = null;

  /** The page is waiting for the hub to restart after an update. */
  @property({ type: Boolean })
  reconnecting = false;

  /** The version the hub must come back on, when known. */
  @property({ attribute: false })
  reconnectVersion: string | null = null;

  /** The hub did not come back as expected. */
  @property({ attribute: false })
  reconnectError: string | null = null;

  /** An update is being requested or is in progress: Update is disabled. */
  @property({ type: Boolean })
  busy = false;

  @state()
  private releases: ReleasesResponse | null = null;

  @state()
  private releasesError: string | null = null;

  @state()
  private releasesLoading = false;

  /** The channel picked by the user; null means the hub's effective channel. */
  @state()
  private pickedChannel: string | null = null;

  /** The release picked by the user; null means the channel's default. */
  @state()
  private pickedRelease: string | null = null;

  /** The running version the loaded release classes were computed against. */
  private releasesForVersion: string | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .section-title {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .card {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 1.25rem;
    }

    .card-meta {
      display: flex;
      gap: 1.5rem;
      flex-wrap: wrap;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .card-meta span {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
    }

    .status-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
    }

    .status-badge.pending {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #a16207);
    }

    .status-badge.completed {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .status-badge.failed {
      background: var(--sl-color-danger-100, #fee2e2);
      color: var(--sl-color-danger-700, #b91c1c);
    }

    .empty-inline {
      padding: 1.5rem;
      text-align: center;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }

    .server-version sl-alert {
      margin-top: 0.75rem;
    }

    .server-version sl-alert sl-button {
      margin-left: 0.5rem;
    }

    .channel-badge {
      margin-left: 0.5rem;
    }

    .last-update .status-badge {
      margin: 0 0.25rem;
    }

    .update-reconnecting {
      align-items: center;
      gap: 0.5rem;
      margin-top: 0.75rem;
    }

    .release-info {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
      margin-top: 0.375rem;
      font-size: 0.8125rem;
      color: var(--scion-text, #1e293b);
    }

    .release-info .release-field {
      display: flex;
      gap: 0.5rem;
    }

    .release-info .release-label {
      color: var(--scion-text-muted, #64748b);
      min-width: 7rem;
    }

    a {
      color: var(--sl-color-primary-600, #2563eb);
      text-decoration: none;
    }

    a:hover {
      text-decoration: underline;
    }

    .release-panel {
      margin-top: 1rem;
      padding-top: 1rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .picker-row {
      display: flex;
      flex-wrap: wrap;
      align-items: flex-end;
      gap: 0.75rem;
    }

    .picker-row sl-select.channel-select {
      width: 12rem;
    }

    .picker-row sl-select.release-select {
      min-width: 22rem;
      flex: 1;
    }

    .catalog-meta {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .release-actions {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.75rem;
      margin-top: 0.75rem;
      font-size: 0.8125rem;
    }

    .channel-source-note {
      margin-top: 0.375rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .update-blocked {
      color: var(--scion-text-muted, #64748b);
    }

    sl-option sl-badge {
      margin-left: 0.25rem;
    }

    .release-date {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0.5rem;
    }

    .retry-releases {
      margin-top: 0.5rem;
    }
  `;

  protected override willUpdate(changed: PropertyValues<this>): void {
    super.willUpdate(changed);
    if (changed.has('status')) this.maybeLoadReleases();
  }

  /**
   * Loads the release list when the release panel is shown, and again when
   * the running version changed (the classes are relative to it). A new
   * version also resets the picks to the defaults.
   */
  private maybeLoadReleases(): void {
    const st = this.status;
    if (!st || st.mode.effective !== 'release' || this.releasesLoading) return;
    if (this.releases && this.releasesForVersion === st.current.version) return;
    if (this.releasesForVersion !== null && this.releasesForVersion !== st.current.version) {
      this.pickedChannel = null;
      this.pickedRelease = null;
    }
    void this.loadReleases(false);
  }

  /**
   * GET releases; refresh asks the hub to bypass its cache (it throttles).
   * Does nothing while a load is in flight, so Retry and Refresh cannot
   * start a second request.
   */
  async loadReleases(refresh: boolean): Promise<void> {
    if (this.releasesLoading) return;
    const version = this.status?.current.version ?? null;
    this.releasesLoading = true;
    try {
      const res = await apiFetch(refresh ? `${RELEASES_PATH}?refresh=1` : RELEASES_PATH);
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      const data: unknown = await res.json();
      if (!isReleasesResponse(data)) {
        throw new Error('Unexpected releases response');
      }
      this.releases = data;
      this.releasesError = null;
    } catch (err) {
      // A previously loaded list stays shown; the error says it is old.
      this.releasesError = err instanceof Error ? err.message : 'Failed to load releases';
    } finally {
      this.releasesForVersion = version;
      this.releasesLoading = false;
    }
  }

  private channelOf(name: string): CatalogChannel | undefined {
    return this.releases?.channels[name];
  }

  private channelEmpty(name: string): boolean {
    return (this.channelOf(name)?.releases.length ?? 0) === 0;
  }

  /** The picked channel, else the hub's effective one, else the first with releases. */
  private get selectedChannel(): string {
    if (this.pickedChannel) return this.pickedChannel;
    const eff = this.status?.channel.effective ?? '';
    if ((RELEASE_CHANNELS as readonly string[]).includes(eff) && !this.channelEmpty(eff)) {
      return eff;
    }
    return RELEASE_CHANNELS.find((c) => !this.channelEmpty(c)) ?? eff;
  }

  private get floor(): string | undefined {
    return this.status?.downgrade?.floor || undefined;
  }

  /** The release a picker value stands for in the selected channel. */
  private releaseFor(value: string): CatalogRelease | undefined {
    const ch = this.channelOf(this.selectedChannel);
    if (!ch) return undefined;
    const tag = value === LATEST_VALUE ? ch.latest : value;
    return tag ? ch.releases.find((r) => r.tag === tag) : undefined;
  }

  /** Why the Latest entry is disabled, or null. */
  private latestDisabledReason(): string | null {
    const latest = this.releaseFor(LATEST_VALUE);
    if (!latest) return 'The latest release is not in the list';
    return releaseDisabledReason(latest, this.floor);
  }

  /** The picked release, else Latest when it can be picked, else nothing. */
  private get selectedValue(): string {
    if (this.pickedRelease) return this.pickedRelease;
    const ch = this.channelOf(this.selectedChannel);
    return ch?.latest && !this.latestDisabledReason() ? LATEST_VALUE : '';
  }

  /** Why Update is disabled for the current pick, or null when it is enabled. */
  private updateBlockedReason(): string | null {
    const st = this.status;
    if (!st) return 'Update status has not loaded';
    if (!st.installer.supported) return 'This hub cannot install updates itself';
    if (this.busy) return 'An update is in progress';
    const value = this.selectedValue;
    const r = value ? this.releaseFor(value) : undefined;
    if (!r) return 'Pick a release';
    if (r.class === 'same') return `The hub is already running ${r.tag}`;
    return releaseDisabledReason(r, this.floor);
  }

  private onChannelChange(e: Event): void {
    const value = (e.target as HTMLElement & { value: string }).value;
    if (!value || value === this.selectedChannel) return;
    this.pickedChannel = value;
    this.pickedRelease = null;
  }

  private onReleaseChange(e: Event): void {
    const value = (e.target as HTMLElement & { value: string }).value;
    this.pickedRelease = value || null;
  }

  private onUpdateClick(): void {
    if (this.updateBlockedReason()) return;
    const value = this.selectedValue;
    const r = this.releaseFor(value);
    if (!r) return;
    const detail: UpdateRequestDetail = {
      target: r.tag,
      channel: this.selectedChannel,
      pin: value === LATEST_VALUE ? LATEST_VALUE : r.tag,
      releaseClass: r.class ?? '',
    };
    this.dispatchEvent(
      new CustomEvent<UpdateRequestDetail>('update-request', {
        detail,
        bubbles: true,
        composed: true,
      })
    );
  }

  private formatDate(iso: string | undefined): string {
    if (!iso || Number.isNaN(new Date(iso).getTime())) return '';
    return formatInstantWithZone(iso, 'date');
  }

  private formatRelativeTime(iso: string | undefined): string {
    if (!iso || Number.isNaN(new Date(iso).getTime())) return '';
    return formatRelative(iso);
  }

  // -- Rendering ----------------------------------------------------------

  override render(): TemplateResult {
    return html`
      <div class="section server-version">
        <h2 class="section-title">Server Version &amp; Updates</h2>
        ${this.statusError
          ? html`
              <sl-alert variant="danger" open class="update-status-error">
                <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                Could not load update status: ${this.statusError}
                <sl-button
                  size="small"
                  @click=${() =>
                    this.dispatchEvent(
                      new CustomEvent('reload-status', { bubbles: true, composed: true })
                    )}
                >
                  <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                  Reload status
                </sl-button>
              </sl-alert>
            `
          : this.status
            ? this.renderBody(this.status)
            : html`<div class="empty-inline">Loading update status...</div>`}
        ${this.reconnecting
          ? html`
              <div class="card-meta update-reconnecting">
                <sl-spinner></sl-spinner>
                <span>
                  Waiting for the hub to
                  restart${this.reconnectVersion ? html` on ${this.reconnectVersion}` : nothing}...
                </span>
              </div>
            `
          : nothing}
        ${this.reconnectError
          ? html`
              <sl-alert variant="warning" open class="update-reconnect-error">
                <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                ${this.reconnectError}
              </sl-alert>
            `
          : nothing}
      </div>
    `;
  }

  private renderBody(st: UpdateStatus): TemplateResult {
    const cur = st.current;
    const version = cur.version || (cur.isGitBuild ? 'git build' : 'unknown');
    const commit = cur.commit ? cur.commit.slice(0, 12) : '';
    const inst = st.installer;
    return html`
      <div class="card">
        <div class="release-info">
          <div class="release-field">
            <span class="release-label">Version:</span>
            <span class="current-version">${version}</span>
            ${cur.channel
              ? html`<sl-badge variant="primary" pill class="channel-badge"
                  >${cur.channel}</sl-badge
                >`
              : nothing}
          </div>
          ${commit
            ? html`
                <div class="release-field">
                  <span class="release-label">Commit:</span>
                  <code class="current-commit">${commit}</code>
                </div>
              `
            : nothing}
          <div class="release-field">
            <span class="release-label">Updates from:</span>
            <span class="update-mode"
              >${st.mode.effective === 'git' ? 'Git build' : 'Release binary'}${st.channel.effective
                ? html` (${st.channel.effective} channel)`
                : nothing}</span
            >
          </div>
          ${st.channel.effective
            ? html`
                <div class="release-field">
                  <span class="release-label">Channel source:</span>
                  <span class="channel-source">${channelSourceLabel(st.channel.source)}</span>
                </div>
              `
            : nothing}
          ${st.mode.effective === 'release'
            ? html`
                <div class="release-field">
                  <span class="release-label">Pin:</span>
                  <span class="pin-state">${pinLabel(st.pin)}</span>
                </div>
              `
            : nothing}
          <div class="release-field">
            <span class="release-label">Installer:</span>
            <span class="installer-strategy">${inst.strategy}</span>
          </div>
          ${st.lastUpdate ? this.renderLastUpdate(st.lastUpdate) : nothing}
        </div>
        ${inst.supported ? nothing : renderInstallerUnsupported(st)}
        ${st.mode.effective === 'release' ? this.renderReleasePanel() : nothing}
      </div>
    `;
  }

  private renderLastUpdate(last: LastUpdate): TemplateResult {
    const label = LAST_UPDATE_LABELS[last.status] ?? last.status;
    const badge =
      last.status === 'verified' ? 'completed' : last.status === 'failed' ? 'failed' : 'pending';
    return html`
      <div class="release-field last-update">
        <span class="release-label">Last update:</span>
        <span>
          ${last.from ? html`${last.from} &rarr; ` : nothing}${last.to}
          <span class="status-badge ${badge}">${label}</span>
          ${last.at ? html`${this.formatRelativeTime(last.at)}` : nothing}
          ${last.runId
            ? html`<a
                href="#"
                class="last-update-run"
                @click=${(e: Event) => {
                  e.preventDefault();
                  this.dispatchEvent(
                    new CustomEvent<{ runId: string }>('view-run', {
                      detail: { runId: last.runId },
                      bubbles: true,
                      composed: true,
                    })
                  );
                }}
                >View run</a
              >`
            : nothing}
        </span>
      </div>
    `;
  }

  private renderReleasePanel(): TemplateResult {
    const cat = this.releases;
    if (!cat) {
      return html`
        <div class="release-panel">
          ${this.releasesError
            ? html`${this.renderReleasesError()}
                <sl-button
                  size="small"
                  class="retry-releases"
                  ?loading=${this.releasesLoading}
                  ?disabled=${this.releasesLoading}
                  @click=${() => this.loadReleases(false)}
                >
                  <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                  Retry
                </sl-button>`
            : html`<div class="card-meta releases-loading">
                <sl-spinner></sl-spinner><span>Loading releases...</span>
              </div>`}
        </div>
      `;
    }
    const channel = this.selectedChannel;
    const blocked = this.updateBlockedReason();
    const selected = this.selectedValue ? this.releaseFor(this.selectedValue) : undefined;
    return html`
      <div class="release-panel">
        <div class="picker-row">
          <sl-select
            class="channel-select"
            label="Channel"
            size="small"
            hoist
            .value=${channel}
            @sl-change=${(e: Event) => this.onChannelChange(e)}
          >
            ${RELEASE_CHANNELS.map((c) => {
              const empty = this.channelEmpty(c);
              return html`<sl-option value=${c} ?disabled=${empty}
                >${c}${empty ? ' (no releases)' : ''}</sl-option
              >`;
            })}
          </sl-select>
          <sl-select
            class="release-select"
            label="Release"
            size="small"
            hoist
            placeholder="No release can be installed"
            .value=${this.selectedValue}
            @sl-change=${(e: Event) => this.onReleaseChange(e)}
          >
            ${this.renderReleaseOptions(channel)}
          </sl-select>
          <span class="catalog-meta">
            <sl-icon-button
              class="refresh-releases"
              name="arrow-clockwise"
              label="Refresh the release list"
              ?disabled=${this.releasesLoading}
              @click=${() => this.loadReleases(true)}
            ></sl-icon-button>
            ${cat.fetchedAt
              ? html`<span class="catalog-as-of" title=${formatInstantWithZone(cat.fetchedAt)}
                  >as of ${this.formatRelativeTime(cat.fetchedAt)}</span
                >`
              : nothing}
          </span>
        </div>
        <div class="channel-source-note">
          The hub's channel is ${this.status?.channel.effective || 'unknown'} (from:
          ${this.status?.channel.source || 'unknown'})${this.status?.pin &&
          this.status.pin !== LATEST_VALUE
            ? html`, pinned to ${this.status.pin}`
            : nothing}.
        </div>
        ${cat.stale
          ? html`
              <sl-alert variant="warning" open class="catalog-stale">
                <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                The hub could not reach GitHub; this list is from
                ${cat.fetchedAt ? this.formatRelativeTime(cat.fetchedAt) : 'an earlier fetch'} and
                may be out of date.
              </sl-alert>
            `
          : nothing}
        ${this.releasesError ? this.renderReleasesError() : nothing}
        <div class="release-actions">
          <sl-button
            variant="primary"
            size="small"
            class="update-release"
            ?disabled=${!!blocked}
            @click=${() => this.onUpdateClick()}
          >
            <sl-icon slot="prefix" name="download"></sl-icon>
            Update
          </sl-button>
          ${blocked ? html`<span class="update-blocked">${blocked}</span>` : nothing}
          ${selected?.notesUrl ? renderReleaseNotesLink(selected.notesUrl) : nothing}
        </div>
      </div>
    `;
  }

  private renderReleasesError(): TemplateResult {
    return html`
      <sl-alert variant="danger" open class="releases-error">
        <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
        Could not load the release list: ${this.releasesError}
      </sl-alert>
    `;
  }

  private renderReleaseOptions(channel: string): TemplateResult[] {
    const ch = this.channelOf(channel);
    if (!ch) return [];
    const options: TemplateResult[] = [];
    if (ch.latest) {
      const reason = this.latestDisabledReason();
      options.push(html`
        <sl-option
          value=${LATEST_VALUE}
          class="release-option latest-option"
          ?disabled=${!!reason}
          title=${reason ?? ''}
          >Latest (${ch.latest})</sl-option
        >
      `);
    }
    for (const r of ch.releases) {
      const reason = releaseDisabledReason(r, this.floor);
      const date = this.formatDate(r.publishedAt);
      options.push(html`
        <sl-option
          value=${r.tag}
          class="release-option"
          data-tag=${r.tag}
          ?disabled=${!!reason}
          title=${reason ?? ''}
          >${r.tag}${date ? html`<span slot="suffix" class="release-date">${date}</span>` : nothing}
          ${releaseBadges(r, this.floor).map((b) =>
            b.tooltip
              ? html`<sl-tooltip slot="suffix" content=${b.tooltip}
                  ><sl-badge variant=${b.variant} pill class="release-badge">${b.label}</sl-badge>
                </sl-tooltip>`
              : html`<sl-badge slot="suffix" variant=${b.variant} pill class="release-badge"
                  >${b.label}</sl-badge
                >`
          )}
        </sl-option>
      `);
    }
    return options;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-admin-maintenance-update-card': ScionAdminMaintenanceUpdateCard;
  }
}
