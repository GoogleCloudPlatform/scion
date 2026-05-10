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
 * Sidebar Navigation Component
 *
 * Provides the main sidebar navigation with Shoelace integration
 */

import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import type { User } from '../../shared/types.js';

interface NavItem {
  path: string;
  label: string;
  icon: string;
}

interface NavSection {
  title: string;
  items: NavItem[];
}

/**
 * Navigation sections configuration
 */
const NAV_SECTIONS: NavSection[] = [
  {
    title: 'Overview',
    items: [{ path: '/', label: 'Dashboard', icon: 'house' }],
  },
  {
    title: 'Management',
    items: [
      { path: '/projects', label: 'Projects', icon: 'folder' },
      { path: '/agents', label: 'Agents', icon: 'cpu' },
      { path: '/brokers', label: 'Brokers', icon: 'hdd-rack' },
    ],
  },
];

/**
 * Admin-only navigation section, shown at the bottom of the sidebar
 */
const ADMIN_SECTION: NavSection = {
  title: 'Admin',
  items: [
    { path: '/settings', label: 'Hub Settings', icon: 'gear' },
    { path: '/admin/server-config', label: 'Server Config', icon: 'sliders' },
    { path: '/admin/scheduler', label: 'Scheduler', icon: 'clock' },
    { path: '/admin/users', label: 'Users', icon: 'people' },
    { path: '/admin/groups', label: 'Groups', icon: 'diagram-3' },
    { path: '/admin/maintenance', label: 'Maintenance', icon: 'wrench-adjustable' },
  ],
};

@customElement('scion-nav')
export class ScionNav extends LitElement {
  /**
   * Current authenticated user
   */
  @property({ type: Object })
  user: User | null = null;

  /**
   * Current active path for highlighting
   */
  @property({ type: String })
  currentPath = '/';

  /**
   * Whether the navigation is collapsed (mobile/tablet)
   */
  @property({ type: Boolean, reflect: true })
  collapsed = false;

  static override styles = css`
    :host {
      display: flex;
      flex-direction: column;
      height: 100%;
      width: var(--scion-sidebar-width, 260px);
      background: var(--scion-surface, #ffffff);
      border-right: 1px solid var(--scion-border, #e2e8f0);
    }

    :host([collapsed]) {
      width: var(--scion-sidebar-collapsed-width, 64px);
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

    :host([collapsed]) .logo-text {
      display: none;
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

    .nav-section.admin-section {
      margin-top: auto;
      padding-top: 1rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
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

    :host([collapsed]) .nav-section-title {
      display: none;
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

    :host([collapsed]) .nav-link {
      justify-content: center;
      padding: 0.75rem;
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

    :host([collapsed]) .nav-link-text {
      display: none;
    }

  `;

  override render() {
    const isAdmin = this.user?.role === 'admin';

    return html`
      <div class="logo">
        <div class="logo-icon">🌱</div>
        <div class="logo-text">
          <h1>Scion</h1>
          <span>Agent Orchestration</span>
        </div>
      </div>

      <nav class="nav-container">
        ${NAV_SECTIONS.map(
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
                        @click=${(e: Event) => this.handleNavClick(e, item.path)}
                      >
                        <sl-icon name="${item.icon}"></sl-icon>
                        <span class="nav-link-text">${item.label}</span>
                      </a>
                    </li>
                  `
                )}
              </ul>
            </div>
          `
        )}
        ${isAdmin
          ? html`
              <div class="nav-section admin-section">
                <div class="nav-section-title">${ADMIN_SECTION.title}</div>
                <ul class="nav-list">
                  ${ADMIN_SECTION.items.map(
                    (item) => html`
                      <li class="nav-item">
                        <a
                          href="${item.path}"
                          class="nav-link ${this.isActive(item.path) ? 'active' : ''}"
                          @click=${(e: Event) => this.handleNavClick(e, item.path)}
                        >
                          <sl-icon name="${item.icon}"></sl-icon>
                          <span class="nav-link-text">${item.label}</span>
                        </a>
                      </li>
                    `
                  )}
                </ul>
              </div>
            `
          : ''}
      </nav>
    `;
  }

  /**
   * Check if a path is currently active
   */
  private isActive(path: string): boolean {
    if (path === '/') {
      return this.currentPath === '/';
    }
    return this.currentPath.startsWith(path);
  }

  /**
   * Handle navigation link click.
   * Prevents default browser navigation and dispatches a custom event
   * so the client-side router can handle it without a full page reload.
   */
  private handleNavClick(e: Event, path: string): void {
    e.preventDefault();
    // Dispatch a custom event for the app shell and router to handle
    this.dispatchEvent(
      new CustomEvent('nav-click', {
        detail: { path },
        bubbles: true,
        composed: true,
      })
    );
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-nav': ScionNav;
  }
}
