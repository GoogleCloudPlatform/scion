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
 * Grove creation page component
 *
 * Form for creating a new grove, supporting both git-backed and hub-native modes.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { extractApiError } from '../../client/api.js';
import '../shared/status-badge.js';

type GroveMode = 'git' | 'hub';
type GitWorkspaceMode = 'per-agent' | 'shared';

@customElement('scion-page-grove-create')
export class ScionPageGroveCreate extends LitElement {
  @state()
  private submitting = false;

  @state()
  private error: string | null = null;

  @state()
  private existingGroveId: string | null = null;

  /** Existing groves sharing the same git remote */
  @state()
  private existingGrovesForRemote: Array<{ id: string; name: string; slug: string }> = [];

  /** Form field values */
  @state()
  private name = '';

  @state()
  private slug = '';

  @state()
  private slugManuallyEdited = false;

  @state()
  private gitRemote = '';

  @state()
  private branch = 'main';

  @state()
  private visibility = 'private';

  @state()
  private mode: GroveMode = 'hub';

  @state()
  private gitWorkspaceMode: GitWorkspaceMode = 'per-agent';

  @state()
  private githubToken = '';

  @state()
  private githubAppUrl: string | null = null;

  override connectedCallback(): void {
    super.connectedCallback();
    this.checkGitHubApp();
  }

  private async checkGitHubApp(): Promise<void> {
    try {
      const res = await fetch('/api/v1/github-app', { credentials: 'include' });
      if (!res.ok) return;
      const data = (await res.json()) as { configured: boolean; installation_url?: string };
      if (data.configured && data.installation_url) {
        this.githubAppUrl = data.installation_url;
      }
    } catch {
      // Non-fatal
    }
  }

  override updated(changedProperties: Map<string, unknown>): void {
    super.updated(changedProperties);
    if (changedProperties.has('error') && this.error) {
      this.scrollIntoView({ behavior: 'smooth', block: 'start' });
    }
  }

  static override styles = css`
    :host {
      display: block;
    }

    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }

    .back-link:hover {
      color: var(--scion-primary, #3b82f6);
    }

    .page-header {
      margin-bottom: 1.5rem;
    }

    .page-header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .page-header h1 sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }

    .page-header p {
      color: var(--scion-text-muted, #64748b);
      margin: 0;
      font-size: 0.875rem;
    }

    .form-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      max-width: 640px;
    }

    .form-field {
      margin-bottom: 1.25rem;
    }

    .form-field label {
      display: block;
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.375rem;
    }

    .form-field .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
    }

    .form-field sl-input,
    .form-field sl-select,
    .form-field sl-radio-group {
      width: 100%;
    }

    .workspace-mode-note {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.5rem;
      padding: 0.5rem 0.75rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .form-actions {
      display: flex;
      gap: 0.75rem;
      margin-top: 1.5rem;
      padding-top: 1.5rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .error-banner {
      background: var(--sl-color-danger-50, #fef2f2);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      margin-bottom: 1.25rem;
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      color: var(--sl-color-danger-700, #b91c1c);
      font-size: 0.875rem;
    }

    .error-banner sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .info-banner {
      background: var(--sl-color-primary-50, #eff6ff);
      border: 1px solid var(--sl-color-primary-200, #bfdbfe);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      margin-bottom: 1.25rem;
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      color: var(--sl-color-primary-700, #1d4ed8);
      font-size: 0.875rem;
    }

    .info-banner sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .github-app-hint {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 1.25rem;
      padding: 0.625rem 0.75rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .github-app-hint sl-icon {
      flex-shrink: 0;
      font-size: 1rem;
    }

    .github-app-hint a {
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
      white-space: nowrap;
    }

    .github-app-hint a:hover {
      text-decoration: underline;
    }

    .exists-dialog-body {
      font-size: 0.925rem;
      color: var(--scion-text, #1e293b);
    }
  `;

  private slugify(text: string): string {
    return text
      .toLowerCase()
      .trim()
      .replace(/[^a-z0-9]+/g, '-')
      .replace(/^-+|-+$/g, '');
  }

  /**
   * Extract a display name from a git URL.
   * Handles HTTPS and SSH formats.
   */
  private deriveNameFromUrl(url: string): string {
    try {
      const cleaned = url.trim().replace(/\.git$/, '');
      const sshMatch = cleaned.match(/[:/]([^/:]+)$/);
      if (sshMatch) {
        return sshMatch[1];
      }
      const parts = cleaned.split('/');
      return parts[parts.length - 1] || '';
    } catch {
      return '';
    }
  }

  private onNameInput(e: Event): void {
    this.name = (e.target as HTMLElement & { value: string }).value;
    if (!this.slugManuallyEdited) {
      this.slug = this.slugify(this.name);
    }
  }

  private onSlugInput(e: Event): void {
    this.slug = (e.target as HTMLElement & { value: string }).value;
    this.slugManuallyEdited = true;
  }

  private onModeChange(e: Event): void {
    this.mode = (e.target as HTMLElement & { value: string }).value as GroveMode;
  }

  private gitRemoteCheckTimer: ReturnType<typeof setTimeout> | null = null;

  private onGitRemoteInput(e: Event): void {
    this.gitRemote = (e.target as HTMLElement & { value: string }).value;

    // Auto-derive name from git URL if name is empty
    if (!this.name) {
      const derived = this.deriveNameFromUrl(this.gitRemote);
      if (derived) {
        this.name = derived;
        if (!this.slugManuallyEdited) {
          this.slug = this.slugify(derived);
        }
      }
    }

    // Debounced check for existing groves sharing this git remote
    if (this.gitRemoteCheckTimer) {
      clearTimeout(this.gitRemoteCheckTimer);
    }
    const url = this.gitRemote.trim();
    if (url.length > 5) {
      this.gitRemoteCheckTimer = setTimeout(() => this.checkExistingGroves(url), 500);
    } else {
      this.existingGrovesForRemote = [];
    }
  }

  private async checkExistingGroves(gitUrl: string): Promise<void> {
    try {
      const response = await fetch(
        `/api/v1/groves?gitRemote=${encodeURIComponent(gitUrl)}`,
        { credentials: 'include' },
      );
      if (!response.ok) return;
      const data = (await response.json()) as {
        groves?: Array<{ id: string; name: string; slug: string }>;
      };
      this.existingGrovesForRemote = data.groves ?? [];
    } catch {
      // Best-effort check; ignore errors
    }
  }

  private navigateToGrove(groveId: string): void {
    window.history.pushState({}, '', `/groves/${groveId}`);
    window.dispatchEvent(new PopStateEvent('popstate'));
  }

  private async handleSubmit(_e: Event): Promise<void> {
    if (!this.name.trim()) {
      this.error = 'Grove name is required.';
      return;
    }

    if (this.mode === 'git' && !this.gitRemote.trim()) {
      this.error = 'Git remote URL is required for git-backed groves.';
      return;
    }

    this.submitting = true;
    this.error = null;

    try {
      const body: Record<string, unknown> = {
        name: this.name.trim(),
        visibility: this.visibility,
      };

      if (this.slug.trim()) {
        body.slug = this.slug.trim();
      }

      if (this.mode === 'git') {
        const trimmedUrl = this.gitRemote.trim();
        // Build an HTTPS clone URL from whatever the user entered.
        // Strip known schemes/prefixes, then re-add https:// and .git.
        let cloneUrl = trimmedUrl
          .replace(/^(https?:\/\/|ssh:\/\/|git:\/\/|git@)/, '')
          .replace(':', '/') // git@host:org/repo → host/org/repo
          .replace(/\.git$/, '');
        cloneUrl = `https://${cloneUrl}.git`;
        body.gitRemote = trimmedUrl;
        const labels: Record<string, string> = {
          'scion.dev/default-branch': this.branch.trim() || 'main',
          'scion.dev/clone-url': cloneUrl,
          'scion.dev/source-url': trimmedUrl,
        };
        if (this.gitWorkspaceMode === 'shared') {
          labels['scion.dev/workspace-mode'] = 'shared';
          body.workspaceMode = 'shared';
        }
        body.labels = labels;
        if (this.githubToken.trim()) {
          body.githubToken = this.githubToken.trim();
        }
      }

      const response = await fetch('/api/v1/groves', {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }

      const result = (await response.json()) as { grove?: { id: string }; id?: string };
      const groveId = result.grove?.id || result.id;

      if (!groveId) {
        throw new Error('No grove ID in response');
      }

      // Backend returns 200 for an existing grove, 201 for newly created
      if (response.status === 200) {
        this.existingGroveId = groveId;
        return;
      }

      // Navigate to the newly created grove
      this.navigateToGrove(groveId);
    } catch (err) {
      console.error('Failed to create grove:', err);
      this.error = err instanceof Error ? err.message : 'Failed to create grove';
    } finally {
      this.submitting = false;
    }
  }

  override render() {
    return html`
      <a href="/groves" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Groves
      </a>

      <div class="page-header">
        <h1>
          <sl-icon name="folder-plus"></sl-icon>
          Create Grove
        </h1>
        <p>Set up a new project workspace for your agents.</p>
      </div>

      <div class="form-card">
        ${this.error
          ? html`
              <div class="error-banner">
                <sl-icon name="exclamation-triangle"></sl-icon>
                <span>${this.error}</span>
              </div>
            `
          : nothing}

        <div>
          <div class="form-field">
            <label for="mode">Workspace Type</label>
            <sl-select
              id="mode"
              .value=${this.mode}
              @sl-change=${(e: Event) => this.onModeChange(e)}
            >
              <sl-option value="hub">Hub Workspace</sl-option>
              <sl-option value="git">Git Repository</sl-option>
            </sl-select>
            <div class="hint">
              ${this.mode === 'hub'
                ? 'A workspace managed by the Hub. No git repository required.'
                : 'Link to an existing git repository for source-controlled workspaces.'}
            </div>
          </div>

          ${this.mode === 'git'
            ? html`
                <div class="form-field">
                  <label for="gitRemote">Git Remote URL</label>
                  <sl-input
                    id="gitRemote"
                    placeholder="https://github.com/org/repo.git"
                    .value=${this.gitRemote}
                    @sl-input=${(e: Event) => this.onGitRemoteInput(e)}
                    required
                  ></sl-input>
                  <div class="hint">
                    HTTPS or SSH URL of the git repository.
                  </div>
                </div>

                ${this.githubAppUrl
                  ? html`
                      <div class="github-app-hint">
                        <sl-icon name="github"></sl-icon>
                        <span>Ensure this repository is accessible via the
                          <a href=${this.githubAppUrl} target="_blank" rel="noopener">GitHub App <sl-icon name="box-arrow-up-right" style="font-size: 0.7em; vertical-align: middle;"></sl-icon></a>
                        </span>
                      </div>
                    `
                  : nothing}

                <div class="form-field">
                  <label for="githubToken">GitHub Token</label>
                  <sl-input
                    id="githubToken"
                    type="password"
                    placeholder="ghp_..."
                    .value=${this.githubToken}
                    @sl-input=${(e: Event) => {
                      this.githubToken = (e.target as HTMLElement & { value: string }).value;
                    }}
                    password-toggle
                  ></sl-input>
                  <div class="hint">
                    Optional. A personal access token for cloning private repositories. Saved as a grove secret.
                  </div>
                </div>

                ${this.existingGrovesForRemote.length > 0
                  ? html`
                      <div class="info-banner">
                        <sl-icon name="info-circle"></sl-icon>
                        <div>
                          <strong>${this.existingGrovesForRemote.length} existing grove(s)</strong> share this git remote.
                          A new grove will be created with a unique slug.
                          <ul style="margin: 0.25rem 0 0; padding-left: 1.25rem;">
                            ${this.existingGrovesForRemote.map(
                              (g) => html`<li>${g.name} <span style="opacity: 0.7">(${g.slug})</span></li>`,
                            )}
                          </ul>
                        </div>
                      </div>
                    `
                  : nothing}

                <div class="form-field">
                  <label>Workspace Mode</label>
                  <sl-radio-group
                    .value=${this.gitWorkspaceMode}
                    @sl-change=${(e: Event) => {
                      this.gitWorkspaceMode = (e.target as HTMLElement & { value: string }).value as GitWorkspaceMode;
                    }}
                  >
                    <sl-radio-button value="per-agent">Per-agent clone</sl-radio-button>
                    <sl-radio-button value="shared">Shared workspace</sl-radio-button>
                  </sl-radio-group>
                  <div class="hint">
                    ${this.gitWorkspaceMode === 'per-agent'
                      ? 'Each agent gets its own independent clone of the repository.'
                      : 'A single git clone is shared by all agents in this grove.'}
                  </div>
                  ${this.gitWorkspaceMode === 'shared'
                    ? html`<div class="workspace-mode-note">
                        A single git clone will be created on the hub and shared by all agents.
                        Agents can commit, push, and pull but must coordinate branch changes.
                      </div>`
                    : nothing}
                </div>
              `
            : nothing}

          <div class="form-field">
            <label for="name">Name</label>
            <sl-input
              id="name"
              placeholder="my-project"
              .value=${this.name}
              @sl-input=${(e: Event) => this.onNameInput(e)}
              required
            ></sl-input>
          </div>

          <div class="form-field">
            <label for="slug">Slug</label>
            <sl-input
              id="slug"
              placeholder="my-project"
              .value=${this.slug}
              @sl-input=${(e: Event) => this.onSlugInput(e)}
            ></sl-input>
            <div class="hint">URL-safe identifier. Auto-derived from name if left unchanged.</div>
          </div>

          ${this.mode === 'git'
            ? html`
                <div class="form-field">
                  <label for="branch">Default Branch</label>
                  <sl-input
                    id="branch"
                    placeholder="main"
                    .value=${this.branch}
                    @sl-input=${(e: Event) => {
                      this.branch = (e.target as HTMLElement & { value: string }).value;
                    }}
                  ></sl-input>
                  <div class="hint">The default branch to use for this repository.</div>
                </div>
              `
            : nothing}

          <div class="form-field">
            <label for="visibility">Visibility</label>
            <sl-select
              id="visibility"
              .value=${this.visibility}
              @sl-change=${(e: Event) => {
                this.visibility = (e.target as HTMLElement & { value: string }).value;
              }}
            >
              <sl-option value="private">Private</sl-option>
              <sl-option value="team">Team</sl-option>
              <sl-option value="public">Public</sl-option>
            </sl-select>
          </div>

          <div class="form-actions">
            <sl-button
              variant="primary"
              ?loading=${this.submitting}
              ?disabled=${this.submitting}
              @click=${(e: Event) => this.handleSubmit(e)}
            >
              <sl-icon slot="prefix" name="folder-plus"></sl-icon>
              Create Grove
            </sl-button>
            <a href="/groves" style="text-decoration: none;">
              <sl-button variant="default" ?disabled=${this.submitting}>
                Cancel
              </sl-button>
            </a>
          </div>
        </div>
      </div>

      <sl-dialog
        label="Grove Already Exists"
        ?open=${this.existingGroveId !== null}
        @sl-after-hide=${() => { this.existingGroveId = null; }}
      >
        <div class="exists-dialog-body">
          A grove with this ID already exists.
        </div>
        <sl-button
          slot="footer"
          variant="primary"
          @click=${() => {
            if (this.existingGroveId) {
              this.navigateToGrove(this.existingGroveId);
            }
          }}
        >
          Take me there
        </sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-grove-create': ScionPageGroveCreate;
  }
}
