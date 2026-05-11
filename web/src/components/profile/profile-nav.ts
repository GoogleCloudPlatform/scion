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
 * Profile Sidebar Navigation Component
 *
 * Provides navigation for user profile/settings pages with a
 * prominent "Return to Hub" link at the top.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch } from '../../client/api.js';

import type { User, Project } from '../../shared/types.js';

interface NavItem {
  path: string;
  label: string;
  icon: string;
}

interface NavSection {
  title: string;
  items: NavItem[];
}

const PROFILE_SECTIONS: NavSection[] = [
  {
    title: 'Configuration',
    items: [
      { path: '/profile/env', label: 'Environment Variables', icon: 'terminal' },
      { path: '/profile/secrets', label: 'Secrets', icon: 'shield-lock' },
      { path: '/profile/tokens', label: 'Access Tokens', icon: 'key' },
    ],
  },
  {
    title: 'Notifications',
    items: [{ path: '/profile/settings', label: 'Notifications & Settings', icon: 'bell' }],
  },
];

@customElement('scion-profile-nav')
export class ScionProfileNav extends LitElement {
  @property({ type: Object })
  user: User | null = null;

  @property({ type: String })
  currentPath = '/profile';

  @state()
  private githubAppUrl: string | null = null;

  static override styles = css`
    :host {
      display: flex;
      flex-direction: column;
      height: 100%;
      width: var(--scion-sidebar-width, 260px);
      background: var(--scion-surface, #ffffff);
      border-right: 1px solid var(--scion-border, #e2e8f0);
    }

    .logo {
      padding: 1.25rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .logo-icon {
      width: 2rem;
      height: 2rem;
      display: flex;
      align-items: center;
      justify-content: center;
      font-size: 1.5rem;
      flex-shrink: 0;
      line-height: 1;
    }

    .logo-text {
      display: flex;
      flex-direction: column;
      overflow: hidden;
    }

    .logo-text h1 {
      font-size: 1.125rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
      line-height: 1.2;
    }

    .logo-text span {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
    }

    .return-link {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.875rem 1rem;
      margin: 0.75rem;
      border-radius: 0.5rem;
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
      font-size: 0.875rem;
      font-weight: 600;
      background: var(--sl-color-primary-50, #eff6ff);
      border: 1px solid var(--sl-color-primary-200, #bfdbfe);
      transition: all 0.15s ease;
    }

    .return-link:hover {
      background: var(--sl-color-primary-100, #dbeafe);
      border-color: var(--scion-primary, #3b82f6);
    }

    .return-link sl-icon {
      font-size: 1.125rem;
      flex-shrink: 0;
    }

    .user-info {
      padding: 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .user-avatar {
      width: 2.25rem;
      height: 2.25rem;
      border-radius: 50%;
      background: var(--scion-bg-subtle, #f1f5f9);
      display: flex;
      align-items: center;
      justify-content: center;
      color: var(--scion-text-muted, #64748b);
      flex-shrink: 0;
    }

    .user-avatar sl-icon {
      font-size: 1.125rem;
    }

    .user-details {
      display: flex;
      flex-direction: column;
      min-width: 0;
    }

    .user-name {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .user-email {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .nav-container {
      flex: 1;
      display: flex;
      flex-direction: column;
      padding: 1rem 0.75rem;
      overflow-y: auto;
      overflow-x: hidden;
    }

    .nav-section {
      margin-bottom: 1.5rem;
    }

    .nav-section:last-child {
      margin-bottom: 0;
    }

    .nav-section-title {
      font-size: 0.6875rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.5rem;
      padding: 0 0.75rem;
    }

    .nav-list {
      list-style: none;
      margin: 0;
      padding: 0;
    }

    .nav-item {
      margin-bottom: 0.25rem;
    }

    .nav-link {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.625rem 0.75rem;
      border-radius: 0.5rem;
      color: var(--scion-text, #1e293b);
      text-decoration: none;
      font-size: 0.875rem;
      font-weight: 500;
      transition: all 0.15s ease;
    }

    .nav-link:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .nav-link.active {
      background: var(--scion-primary, #3b82f6);
      color: white;
    }

    .nav-link.active:hover {
      background: var(--scion-primary-hover, #2563eb);
    }

    .nav-link sl-icon {
      font-size: 1.125rem;
      flex-shrink: 0;
    }

    .nav-link-text {
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .nav-link-external {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.625rem 0.75rem;
      border-radius: 0.5rem;
      color: var(--scion-text, #1e293b);
      text-decoration: none;
      font-size: 0.875rem;
      font-weight: 500;
      transition: all 0.15s ease;
    }

    .nav-link-external:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .nav-link-external sl-icon {
      font-size: 1.125rem;
      flex-shrink: 0;
    }

    .nav-link-external .external-icon {
      font-size: 0.6875rem;
      margin-left: auto;
      opacity: 0.6;
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

      const appData = (await appRes.json()) as { configured: boolean; installation_url?: string };
      if (!appData.configured || !appData.installation_url) return;

      const projectsData = (await projectsRes.json()) as { projects: Project[] };
      const hasInstallation = (projectsData.projects || []).some((p) => p.githubInstallationId);
      if (hasInstallation) {
        this.githubAppUrl = appData.installation_url;
      }
    } catch {
      // Non-fatal
    }
  }

  override render() {
    return html`
      <div class="logo">
        <div class="logo-icon">🌱</div>
        <div class="logo-text">
          <h1>Scion</h1>
          <span>Profile &amp; Settings</span>
        </div>
      </div>

      <a href="/" class="return-link">
        <sl-icon name="arrow-left-circle"></sl-icon>
        Return to Hub
      </a>

      ${this.user
        ? html`
            <div class="user-info">
              <div class="user-avatar">
                <sl-icon name="person-circle"></sl-icon>
              </div>
              <div class="user-details">
                <span class="user-name">${this.user.name || 'User'}</span>
                <span class="user-email">${this.user.email}</span>
              </div>
            </div>
          `
        : ''}

      <nav class="nav-container">
        ${PROFILE_SECTIONS.map(
          (section) => html`
            <div class="nav-section">
              <div class="nav-section-title">${section.title}</div>
              <ul class="nav-list">
                ${section.items.map(
                  (item) => html`
                    <li class="nav-item">
                      <a
                        href="${item.path}"
                        class="nav-link ${this.isActive(item.path) ? 'active' : ''}"
                      >
                        <sl-icon name="${item.icon}"></sl-icon>
                        <span class="nav-link-text">${item.label}</span>
                      </a>
                    </li>
                    ${item.path === '/profile/tokens' && this.githubAppUrl
                      ? html`
                          <li class="nav-item">
                            <a
                              href=${this.githubAppUrl}
                              target="_blank"
                              rel="noopener"
                              class="nav-link-external"
                            >
                              <sl-icon name="github"></sl-icon>
                              <span class="nav-link-text">GitHub Application</span>
                              <sl-icon name="box-arrow-up-right" class="external-icon"></sl-icon>
                            </a>
                          </li>
                        `
                      : nothing}
                  `
                )}
              </ul>
            </div>
          `
        )}
      </nav>
    `;
  }

  private isActive(path: string): boolean {
    return this.currentPath === path || this.currentPath.startsWith(path + '/');
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-profile-nav': ScionProfileNav;
  }
}
