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
 * Profile Access Tokens page
 *
 * Thin wrapper around the shared <scion-token-list> component,
 * providing the page header and description. When the hub has a
 * GitHub App configured and the current user owns projects that have
 * an active GitHub App installation, a link to manage the GitHub App
 * installation is shown below the header.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';
import { apiFetch } from '../../client/api.js';

import type { Project } from '../../shared/types.js';

import '../shared/token-list.js';

interface GitHubAppInfo {
  configured: boolean;
  installation_url?: string;
}

@customElement('scion-page-profile-tokens')
export class ScionPageProfileTokens extends LitElement {
  @state()
  private githubAppLink: string | null = null;

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

    .github-app-card {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.75rem 1rem;
      margin-bottom: 1.5rem;
      background: var(--scion-bg-subtle, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .github-app-card sl-icon {
      font-size: 1.25rem;
      flex-shrink: 0;
      color: var(--scion-text-muted, #64748b);
    }

    .github-app-card span {
      flex: 1;
      color: var(--scion-text-muted, #64748b);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    this.checkGitHubApp();
  }
private async checkGitHubApp(): Promise<void> {
  try {
    const [appRes, projectsRes] = await Promise.all([
      apiFetch('/api/v1/github-app'),
      apiFetch('/api/v1/projects?mine=true'),
    ]);

    if (!appRes.ok || !projectsRes.ok) return;

    const appData = (await appRes.json()) as GitHubAppInfo;
    if (!appData.configured || !appData.installation_url) return;

    const projectsData = (await projectsRes.json()) as { projects: Project[] };
    const hasInstallation = (projectsData.projects || []).some(
      (p) => p.githubInstallationId
    );
    if (hasInstallation) {
      this.githubAppLink = appData.installation_url;
    }
  } catch {
    // Non-fatal — the link is a convenience feature
  }
}

  override render() {
    return html`
      <div class="page-header">
        <div class="page-header-info">
          <h1>Access Tokens</h1>
          <p>
            Create and manage personal access tokens for CI/CD pipelines and automation.
            Tokens are scoped to a specific project with limited permissions.
          </p>
        </div>
      </div>

      ${this.githubAppLink
        ? html`
            <div class="github-app-card">
              <sl-icon name="github"></sl-icon>
              <span>Manage repository access and permissions for the GitHub App integration.</span>
              <sl-button
                size="small"
                variant="default"
                href=${this.githubAppLink}
                target="_blank"
              >
                <sl-icon slot="prefix" name="box-arrow-up-right"></sl-icon>
                Configure GitHub App
              </sl-button>
            </div>
          `
        : nothing}

      <scion-token-list></scion-token-list>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-profile-tokens': ScionPageProfileTokens;
  }
}
