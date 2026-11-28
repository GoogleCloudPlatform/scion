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
 * Standalone Project GitHub App card component (M1.1 extraction for M1.5).
 *
 * Drives GitHub App connection status, bind/unbind/check/discover actions,
 * and the read-only effective token permissions table from
 * `GET /api/v1/projects/{id}/github`.
 */

import { LitElement, html, css, nothing, type PropertyValues, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { GitHubInstallation, ProjectGitHubInfo } from '../../shared/types.js';
import { apiFetch } from '../../client/api.js';
import { showConfirm } from './confirm-dialog.js';
import { formatInstantWithZone } from '../../utils/time.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';

/**
 * Filter GitHub App installations client-side to active ones whose account
 * login matches the project's remote owner (case-insensitively).
 */
export function filterBindableGitHubInstallations(
  installations: GitHubInstallation[],
  owner: string
): GitHubInstallation[] {
  const target = owner.trim().toLowerCase();
  if (!target) return [];
  return installations.filter(
    (inst) => inst.status === 'active' && (inst.account_login || '').toLowerCase() === target
  );
}

/**
 * Human-readable explanation for a capped token permission row.
 */
export function formatGitHubCapReason(reason?: string, requested?: string): string {
  switch (reason) {
    case 'app_grant':
      return requested
        ? `App not granted ${requested} — update the App on GitHub`
        : 'Capped by App grant — update the App on GitHub';
    case 'association_read_only':
      return 'Association is read-only — ask a hub admin to bind';
    default:
      return '';
  }
}

const GITHUB_PERMISSION_ROWS: ReadonlyArray<{ key: string; label: string }> = [
  { key: 'contents', label: 'Contents' },
  { key: 'pull_requests', label: 'Pull requests' },
  { key: 'issues', label: 'Issues' },
  { key: 'checks', label: 'Checks' },
  { key: 'actions', label: 'Actions' },
  { key: 'metadata', label: 'Metadata' },
];

export type ProjectGitHubCardSection = 'all' | 'connection' | 'permissions';

@customElement('scion-project-github-card')
export class ScionProjectGithubCard extends LitElement {
  /** Re-renders absolute times when the display timezone changes. */
  readonly _zone = new DisplayZoneController(this);

  @property({ type: String })
  projectId = '';

  /**
   * Which part of the card to render:
   * - `'all'` (default): full card (connection + permissions)
   * - `'connection'`: connection status, bind/unbind/check/discover actions only
   * - `'permissions'`: token permissions table only
   */
  @property({ type: String })
  section: ProjectGitHubCardSection = 'all';

  /**
   * Optional pre-fetched read-model payload supplied by a parent page.
   * When omitted, the component fetches `GET /api/v1/projects/{id}/github` itself.
   */
  @property({ attribute: false })
  info: ProjectGitHubInfo | null = null;

  @state()
  private fetchedInfo: ProjectGitHubInfo | null = null;

  @state()
  private githubAppError: string | null = null;

  @state()
  private githubAppLoading = false;

  @state()
  private githubBindBusy = false;

  @state()
  private githubBindCandidates: GitHubInstallation[] = [];

  private loadedProjectId = '';

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

    .section h2 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .section p {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .field-help {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .template-badge {
      font-size: 0.6875rem;
      padding: 0.125rem 0.5rem;
      border-radius: var(--scion-radius-full, 9999px);
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      border: 1px solid var(--scion-border, #e2e8f0);
      white-space: nowrap;
    }

    .status-message {
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }

    .status-message.error {
      background: var(--sl-color-danger-50, #fef2f2);
      color: var(--sl-color-danger-700, #b91c1c);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
    }

    .status-message.warning {
      background: var(--scion-badge-warning-bg, #fef3c7);
      color: var(--scion-warning-text, #92400e);
      border: 1px solid var(--scion-warning-border, #fcd34d);
    }

    .github-no-install {
      text-align: center;
      padding: 1.5rem;
    }

    .github-no-install p {
      margin: 0.5rem 0;
    }

    .github-status-row {
      display: flex;
      flex-wrap: wrap;
      gap: 1.5rem;
      margin-bottom: 1rem;
    }

    .github-status-item {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
    }

    .github-status-value {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .github-status-dot {
      display: inline-block;
      width: 10px;
      height: 10px;
      border-radius: var(--scion-radius-full, 9999px);
    }

    .github-status-dot.ok {
      background: var(--sl-color-success-600, #16a34a);
    }

    .github-status-dot.degraded {
      background: var(--sl-color-yellow-500, #eab308);
    }

    .github-status-dot.error {
      background: var(--sl-color-danger-600, #dc2626);
    }

    .github-status-dot.unchecked {
      background: var(--scion-text-disabled, #94a3b8);
    }

    .github-permissions {
      margin-top: 1rem;
    }

    .github-permissions.standalone {
      margin-top: 0;
    }

    .github-perm-badge {
      font-size: 0.75rem;
      padding: 0.125rem 0.5rem;
      border-radius: var(--scion-radius-full, 9999px);
      border: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    .github-perm-badge.write {
      background: var(--scion-badge-primary-bg, #dbeafe);
      border-color: var(--sl-color-primary-300, #93c5fd);
      color: var(--scion-badge-primary-text, #1e40af);
    }

    .github-perm-badge.read {
      background: var(--sl-color-success-50, #f0fdf4);
      border-color: var(--scion-success-border, #86efac);
      color: var(--scion-success-text, #166534);
    }

    .github-perm-badge.capped {
      background: var(--scion-badge-warning-bg, #fef3c7);
      border-color: var(--scion-warning-border, #fcd34d);
      color: var(--scion-warning-text, #92400e);
    }

    .github-perm-header {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-bottom: 0.5rem;
    }

    .github-perm-table {
      width: 100%;
      border-collapse: collapse;
      font-size: 0.8125rem;
    }

    .github-perm-table th,
    .github-perm-table td {
      text-align: left;
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .github-perm-table th {
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
    }

    .github-cap-reason {
      font-size: 0.75rem;
      color: var(--scion-warning-text, #92400e);
    }

    .github-bind-picker {
      margin-top: 0.75rem;
      padding: 0.75rem;
      background: var(--scion-bg-subtle, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
    }

    .github-bind-candidate {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.75rem;
    }
  `;

  private get effectiveInfo(): ProjectGitHubInfo | null {
    return this.info ?? this.fetchedInfo;
  }

  override updated(changedProperties: PropertyValues): void {
    super.updated(changedProperties);
    if (changedProperties.has('projectId') || changedProperties.has('info')) {
      if (this.info) {
        this.fetchedInfo = this.info;
        this.loadedProjectId = this.projectId;
      } else if (this.projectId && this.projectId !== this.loadedProjectId) {
        this.loadedProjectId = this.projectId;
        void this.loadProjectGitHub();
      }
    }
  }

  async loadProjectGitHub(): Promise<void> {
    if (!this.projectId) return;
    this.githubAppLoading = true;
    try {
      const res = await apiFetch(`/api/v1/projects/${encodeURIComponent(this.projectId)}/github`, {
        suppressAccessDeniedToast: true,
      });
      if (res.ok) {
        const data = (await res.json()) as ProjectGitHubInfo;
        if (!this.isConnected) return;
        this.fetchedInfo = data;
        this.dispatchEvent(
          new CustomEvent<{ info: ProjectGitHubInfo }>('github-info-changed', {
            detail: { info: data },
            bubbles: true,
            composed: true,
          })
        );
      }
    } catch {
      // Non-critical — keep section hidden if unavailable
    } finally {
      this.githubAppLoading = false;
    }
  }

  private async startBindGitHubInstallation(): Promise<void> {
    this.githubAppError = null;
    this.githubBindCandidates = [];
    const owner = this.effectiveInfo?.remote.owner || '';
    if (!owner) {
      this.githubAppError = 'Project remote is not on the configured GitHub host.';
      return;
    }

    this.githubBindBusy = true;
    try {
      const res = await apiFetch('/api/v1/github-app/installations');
      if (!res.ok) {
        const data = (await res.json().catch(() => ({}))) as { message?: string };
        throw new Error(data.message || `Failed to list installations (${res.status})`);
      }
      const data = (await res.json()) as { installations?: GitHubInstallation[] };
      const candidates = filterBindableGitHubInstallations(data.installations ?? [], owner);
      if (candidates.length === 0) {
        this.githubAppError = `No active GitHub App installation found for "${owner}".`;
        return;
      }
      if (candidates.length === 1) {
        await this.confirmAndBindGitHubInstallation(candidates[0]);
        return;
      }
      this.githubBindCandidates = candidates;
    } catch (err) {
      this.githubAppError = err instanceof Error ? err.message : 'Failed to load installations';
    } finally {
      this.githubBindBusy = false;
    }
  }

  private async confirmAndBindGitHubInstallation(inst: GitHubInstallation): Promise<void> {
    const confirmed = await showConfirm(
      `Bind installation ${inst.account_login} (#${inst.installation_id}) to this project? Tokens for this project will be authorized by your hub-admin binding, not the creator's GitHub access.`
    );
    if (!confirmed) return;

    this.githubBindBusy = true;
    this.githubAppError = null;
    try {
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/github-installation`,
        {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ installation_id: inst.installation_id }),
        }
      );
      if (!res.ok) {
        const data = (await res.json().catch(() => ({}))) as { message?: string };
        throw new Error(data.message || `Failed to bind installation (${res.status})`);
      }
      this.githubBindCandidates = [];
      await this.loadProjectGitHub();
    } catch (err) {
      this.githubAppError = err instanceof Error ? err.message : 'Bind failed';
    } finally {
      this.githubBindBusy = false;
    }
  }

  private formatGitHubErrorCode(code?: string): string {
    switch (code) {
      case 'private_key_invalid':
        return 'Private Key Invalid';
      case 'app_not_found':
        return 'App Not Found';
      case 'installation_revoked':
        return 'Installation Revoked';
      case 'installation_suspended':
        return 'Installation Suspended';
      case 'repo_not_accessible':
        return 'Repository Not Accessible';
      case 'permission_denied':
        return 'Permission Denied';
      case 'token_mint_failed':
        return 'Token Mint Failed';
      case 'association_unauthorized':
        return 'Association Unauthorized';
      case 'association_check_failed':
        return 'Association Check Failed';
      default:
        return code ?? 'Error';
    }
  }

  private async checkGitHubStatus(): Promise<void> {
    this.githubAppError = null;
    this.githubAppLoading = true;
    try {
      // Actively verify the installation by minting a test token
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/github-status`,
        {
          method: 'POST',
        }
      );
      if (!res.ok) {
        const data = (await res.json().catch(() => ({}))) as { message?: string };
        throw new Error(data.message || `Check failed (${res.status})`);
      }
      await this.loadProjectGitHub();
    } catch (err) {
      this.githubAppError = err instanceof Error ? err.message : 'Check failed';
    } finally {
      this.githubAppLoading = false;
    }
  }

  private async discoverGitHubInstallation(): Promise<void> {
    this.githubAppLoading = true;
    this.githubAppError = null;
    try {
      const res = await apiFetch('/api/v1/github-app/installations/discover', { method: 'POST' });
      if (!res.ok) {
        const data = (await res.json().catch(() => ({}))) as { message?: string };
        throw new Error(data.message || `Failed to discover installations (${res.status})`);
      }
      await this.loadProjectGitHub();
    } catch (err) {
      this.githubAppError = err instanceof Error ? err.message : 'Discovery failed';
    } finally {
      this.githubAppLoading = false;
    }
  }

  private async removeGitHubInstallation(): Promise<void> {
    if (
      !(await showConfirm(
        'Remove GitHub App installation from this project? Agents will fall back to PAT authentication.'
      ))
    )
      return;
    this.githubAppLoading = true;
    this.githubAppError = null;
    try {
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/github-installation`,
        {
          method: 'DELETE',
        }
      );
      if (!res.ok) {
        const data = (await res.json().catch(() => ({}))) as { message?: string };
        throw new Error(data.message || `Failed to remove installation (${res.status})`);
      }
      this.githubBindCandidates = [];
      await this.loadProjectGitHub();
    } catch (err) {
      this.githubAppError = err instanceof Error ? err.message : 'Remove failed';
    } finally {
      this.githubAppLoading = false;
    }
  }

  private renderPermissionsTable(
    info: ProjectGitHubInfo,
    standalone: boolean
  ): TemplateResult | typeof nothing {
    const perms = info.permissions;
    if (!perms) return nothing;
    const cappedSet = new Set(perms.capped ?? []);
    const appGrantKnown = perms.app_grant_known !== false && perms.app_granted != null;
    return html`
      <div class="github-permissions ${standalone ? 'standalone' : ''}">
        <div class="github-perm-header">
          <span class="field-help">Token Permissions</span>
          <span class="template-badge" data-testid="github-perm-mode">
            ${perms.mode === 'custom' ? 'Custom' : 'Default'}
          </span>
        </div>
        ${!appGrantKnown
          ? html`<p class="field-help" data-testid="github-app-perms-unloaded">
              App permissions not yet loaded.
            </p>`
          : ''}
        <table class="github-perm-table" data-testid="github-permissions-table">
          <thead>
            <tr>
              <th>Permission</th>
              <th>Requested</th>
              <th>App grant</th>
              <th>Effective</th>
              <th>Note</th>
            </tr>
          </thead>
          <tbody>
            ${GITHUB_PERMISSION_ROWS.map((row) => {
              const requested = perms.requested?.[row.key] || '';
              const appGrant = appGrantKnown ? perms.app_granted?.[row.key] || '' : '';
              const effective = perms.effective?.[row.key] || '';
              const isCapped = cappedSet.has(row.key);
              const capReason = formatGitHubCapReason(perms.cap_reasons?.[row.key], requested);
              return html`
                <tr data-permission=${row.key}>
                  <td>${row.label}</td>
                  <td>${requested || '—'}</td>
                  <td>${appGrant || '—'}</td>
                  <td>
                    ${effective
                      ? html`<span class="github-perm-badge ${isCapped ? 'capped' : effective}"
                          >${effective}</span
                        >`
                      : '—'}
                  </td>
                  <td>
                    ${capReason ? html`<span class="github-cap-reason">${capReason}</span>` : ''}
                  </td>
                </tr>
              `;
            })}
          </tbody>
        </table>
      </div>
    `;
  }

  override render() {
    const info = this.effectiveInfo;
    if (!info) return nothing;
    if (!info.app.configured || !info.remote.on_app_host) return nothing;

    if (this.section === 'permissions') {
      return html`
        <div class="section" data-testid="github-permissions-section">
          <h2>Token Permissions</h2>
          <p>
            Effective permissions granted to GitHub App installation tokens minted for agents in
            this project.
          </p>
          ${this.renderPermissionsTable(info, true)}
        </div>
      `;
    }

    const status = info.status;
    const assoc = info.association;
    const installationId = assoc?.installation_id ?? null;
    const hasInstallation = installationId != null;
    const canBind = info.can.bind === true;
    const canUnbind = info.can.unbind === true;
    const canCheck = info.can.check === true;
    const canDiscover = info.can.discover === true;
    const webHost = info.app.web_host || 'github.com';
    const installationUrl = info.app.installation_url || '';

    const stateIcon = (s: string | undefined) => {
      switch (s) {
        case 'ok':
          return html`<span class="github-status-dot ok"></span>`;
        case 'degraded':
          return html`<span class="github-status-dot degraded"></span>`;
        case 'error':
          return html`<span class="github-status-dot error"></span>`;
        case 'unchecked':
          return html`<span class="github-status-dot unchecked"></span>`;
        default:
          return '';
      }
    };

    const stateLabel = (s: string | undefined) => {
      switch (s) {
        case 'ok':
          return 'Active';
        case 'degraded':
          return 'Degraded';
        case 'error':
          return 'Error';
        case 'unchecked':
          return 'Unchecked';
        default:
          return 'Not configured';
      }
    };

    const renderAuthorizedBy = (): TemplateResult | string => {
      if (!assoc) return '';
      if (!assoc.authorized) {
        if (assoc.verified === false) {
          return html`<span data-testid="github-association-unverified"
            >Unverified${assoc.reason ? ` — ${assoc.reason}` : ''}</span
          >`;
        }
        return html`<span>Not authorized${assoc.reason ? ` — ${assoc.reason}` : ''}</span>`;
      }
      switch (assoc.basis) {
        case 'admin_binding': {
          const binder = assoc.bound_by_name || 'a hub admin';
          return html`<span
            >Bound by
            ${binder}${assoc.bound_at ? ` on ${formatInstantWithZone(assoc.bound_at)}` : ''}</span
          >`;
        }
        case 'creator_push':
          return html`<span>Creator proof (write allowed)</span>`;
        case 'creator_pull':
          return html`<span>Creator proof (read-only)</span>`;
        default:
          return html`<span>${assoc.basis ?? 'Authorized'}</span>`;
      }
    };

    const renderBindPicker = (): TemplateResult | string => {
      if (this.githubBindCandidates.length <= 1) return '';
      return html`
        <div class="github-bind-picker" data-testid="github-bind-picker">
          <span class="field-help">Select an active installation to bind:</span>
          ${this.githubBindCandidates.map(
            (inst) => html`
              <div class="github-bind-candidate">
                <span>
                  <strong>${inst.account_login}</strong> (${inst.account_type}) ·
                  <code>#${inst.installation_id}</code>
                </span>
                <sl-button
                  variant="primary"
                  size="small"
                  ?loading=${this.githubBindBusy}
                  ?disabled=${this.githubBindBusy}
                  @click=${(): Promise<void> => this.confirmAndBindGitHubInstallation(inst)}
                >
                  Bind #${inst.installation_id}
                </sl-button>
              </div>
            `
          )}
        </div>
      `;
    };

    return html`
      <div class="section" data-testid="github-app-section">
        <div style="display: flex; align-items: center; gap: 0.5rem; margin-bottom: 0.25rem;">
          <h2 style="margin: 0;">GitHub App Integration</h2>
          ${info.app.slug ? html`<span class="template-badge">@${info.app.slug}</span>` : ''}
        </div>
        <p>Automatic token management for GitHub operations via GitHub App installation tokens.</p>

        ${this.githubAppError
          ? html` <div class="status-message error">${this.githubAppError}</div> `
          : ''}
        ${!hasInstallation
          ? html`
              <div class="github-no-install">
                <sl-icon
                  name="github"
                  style="font-size: 2rem; color: var(--scion-text-muted, #64748b);"
                ></sl-icon>
                ${this.githubAppLoading
                  ? html` <p>Checking for GitHub App installation…</p> `
                  : html`
                      <p>No GitHub App installation is connected to this project.</p>
                      ${canDiscover && installationUrl
                        ? html`
                            <p class="field-help">
                              <a
                                href=${installationUrl}
                                target="_blank"
                                rel="noopener noreferrer"
                                data-testid="github-install-link"
                              >
                                Install the GitHub App
                              </a>
                              on your organization or account, then bind or discover it here.
                            </p>
                          `
                        : ''}
                      <div
                        style="margin-top: 0.75rem; display: flex; justify-content: center; gap: 0.5rem; flex-wrap: wrap;"
                      >
                        ${canBind
                          ? html`
                              <sl-button
                                variant="primary"
                                size="small"
                                data-testid="github-bind-button"
                                ?loading=${this.githubBindBusy}
                                ?disabled=${this.githubBindBusy || this.githubAppLoading}
                                @click=${(): Promise<void> => this.startBindGitHubInstallation()}
                              >
                                Bind installation
                              </sl-button>
                            `
                          : ''}
                        ${canDiscover
                          ? html`
                              <sl-button
                                variant="default"
                                size="small"
                                ?disabled=${this.githubAppLoading}
                                @click=${() => this.discoverGitHubInstallation()}
                              >
                                <sl-icon slot="prefix" name="search"></sl-icon>
                                Discover Installation
                              </sl-button>
                            `
                          : ''}
                      </div>
                      ${renderBindPicker()}
                    `}
              </div>
            `
          : html`
              <div class="github-status-row">
                <div class="github-status-item">
                  <span class="field-help">Status</span>
                  <div class="github-status-value">
                    ${stateIcon(status?.state)}
                    <strong>${stateLabel(status?.state)}</strong>
                  </div>
                </div>
                ${info.remote.owner && info.remote.repo
                  ? html`
                      <div class="github-status-item">
                        <span class="field-help">Repository</span>
                        <code>${info.remote.owner}/${info.remote.repo}</code>
                      </div>
                    `
                  : ''}
                <div class="github-status-item">
                  <span class="field-help">Installation</span>
                  <span>
                    ${assoc?.account_login ? `${assoc.account_login} · ` : ''}<code
                      >#${installationId}</code
                    >
                  </span>
                </div>
                <div class="github-status-item">
                  <span class="field-help">Authorized by</span>
                  ${renderAuthorizedBy()}
                </div>
                ${status?.last_token_mint
                  ? html`
                      <div class="github-status-item">
                        <span class="field-help">Last Token Mint</span>
                        <span>${formatInstantWithZone(status.last_token_mint)}</span>
                      </div>
                    `
                  : ''}
              </div>

              ${status?.state === 'error' || status?.state === 'degraded'
                ? html`
                    <div class="status-message ${status.state === 'error' ? 'error' : 'warning'}">
                      <strong>${this.formatGitHubErrorCode(status.error_code)}:</strong>
                      ${status.error_message}
                      ${status.state === 'error' && info.remote.on_app_host
                        ? html` <br /><small>Agents will use PAT fallback if available.</small> `
                        : ''}
                    </div>
                  `
                : ''}
              ${this.section === 'all' ? this.renderPermissionsTable(info, false) : nothing}
              ${(canBind && assoc?.basis !== 'admin_binding') || canCheck || canUnbind
                ? html`
                    <div
                      style="margin-top: 1rem; display: flex; align-items: center; gap: 0.5rem; flex-wrap: wrap;"
                    >
                      ${canBind && assoc?.basis !== 'admin_binding'
                        ? html`
                            <sl-button
                              variant="primary"
                              size="small"
                              data-testid="github-bind-button"
                              ?loading=${this.githubBindBusy}
                              ?disabled=${this.githubBindBusy || this.githubAppLoading}
                              @click=${() => this.startBindGitHubInstallation()}
                            >
                              Bind installation
                            </sl-button>
                          `
                        : ''}
                      ${canCheck
                        ? html`
                            <sl-button
                              variant="default"
                              size="small"
                              data-testid="github-check-button"
                              ?loading=${this.githubAppLoading}
                              ?disabled=${this.githubAppLoading}
                              @click=${() => this.checkGitHubStatus()}
                            >
                              <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                              ${status?.state === 'unchecked' ? 'Check Status' : 'Recheck Status'}
                            </sl-button>
                          `
                        : ''}
                      ${canCheck || canUnbind || canBind
                        ? html`
                            <a
                              href=${`https://${webHost}/settings/installations/${installationId}`}
                              target="_blank"
                              rel="noopener noreferrer"
                              style="text-decoration: none;"
                              data-testid="github-configure-link"
                            >
                              <sl-button variant="text" size="small">
                                <sl-icon slot="prefix" name="gear"></sl-icon>
                                Configure Installation
                              </sl-button>
                            </a>
                          `
                        : ''}
                      ${canUnbind
                        ? html`
                            <sl-button
                              variant="text"
                              size="small"
                              data-testid="github-remove-button"
                              @click=${() => this.removeGitHubInstallation()}
                            >
                              <sl-icon slot="prefix" name="x-circle"></sl-icon>
                              Remove
                            </sl-button>
                          `
                        : ''}
                    </div>
                  `
                : ''}
              ${renderBindPicker()}
            `}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-project-github-card': ScionProjectGithubCard;
  }
}
