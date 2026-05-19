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
 * Git Remote Display component
 *
 * Renders a git remote URL with trailing decorator icons:
 * - Workspace mode: folder (shared) or robot (clone per agent)
 * - GitHub App status badge
 *
 * Used in both project detail and project list views.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import type { Project } from '../../shared/types.js';
import { isSharedWorkspace } from '../../shared/types.js';

@customElement('scion-git-remote-display')
export class ScionGitRemoteDisplay extends LitElement {
  @property({ type: Object })
  project: Project | null = null;

  /** When true, prevents click events from propagating (useful inside clickable cards). */
  @property({ type: Boolean, attribute: 'stop-propagation' })
  stopPropagation = false;

  static override styles = css`
    :host {
      display: inline;
      font-family: var(--sl-font-mono, monospace);
      font-size: inherit;
      color: inherit;
    }

    .git-remote-display {
      display: inline;
    }

    a {
      color: inherit;
      text-decoration: none;
      transition: color var(--scion-transition-fast, 150ms ease);
    }

    a:hover {
      color: var(--scion-primary, #3b82f6);
      text-decoration: underline;
    }

    .decorator-icon {
      font-size: 0.875rem;
      vertical-align: middle;
      opacity: 0.7;
      margin-left: 0.375rem;
    }
  `;

  static gitHubLink(remote: string): { url: string; display: string } | null {
    const sshMatch = remote.match(/^git@github\.com:(.+?)(?:\.git)?$/);
    if (sshMatch) return { url: `https://github.com/${sshMatch[1]}`, display: `github.com/${sshMatch[1]}` };
    const httpsMatch = remote.match(/^https?:\/\/(github\.com\/.+?)(?:\.git)?$/);
    if (httpsMatch) return { url: `https://${httpsMatch[1]}`, display: httpsMatch[1] };
    const bareMatch = remote.match(/^(github\.com\/.+?)(?:\.git)?$/);
    if (bareMatch) return { url: `https://${bareMatch[1]}`, display: bareMatch[1] };
    return null;
  }

  private handleLinkClick(e: Event) {
    if (this.stopPropagation) e.stopPropagation();
  }

  override render() {
    if (!this.project) return nothing;

    const project = this.project;

    if (!project.gitRemote) {
      return html`${project.projectType === 'linked' ? 'Linked project' : 'Hub workspace'}`;
    }

    const ghLink = ScionGitRemoteDisplay.gitHubLink(project.gitRemote);
    const urlContent = ghLink
      ? html`<a href="${ghLink.url}" target="_blank" rel="noopener noreferrer" @click=${this.handleLinkClick}>${ghLink.display}</a>`
      : project.gitRemote;

    const shared = isSharedWorkspace(project);
    const workspaceModeIcon = shared
      ? html`<sl-tooltip content="Shared workspace"><sl-icon name="folder-fill" class="decorator-icon"></sl-icon></sl-tooltip>`
      : html`<sl-tooltip content="Clone per agent"><sl-icon name="robot" class="decorator-icon"></sl-icon></sl-tooltip>`;

    const githubIcon = project.githubInstallationId != null
      ? html`<sl-tooltip content="GitHub App installed"><sl-icon name="github" class="decorator-icon"></sl-icon></sl-tooltip>`
      : nothing;

    return html`<span class="git-remote-display">${urlContent}${workspaceModeIcon}${githubIcon}</span>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-git-remote-display': ScionGitRemoteDisplay;
  }
}
