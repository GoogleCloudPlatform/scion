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
      { path: '/groves', label: 'Groves', icon: 'folder' },
      { path: '/agents', label: 'Agents', icon: 'cpu' },
    ],
  },
  {
    title: 'System',
    items: [{ path: '/settings', label: 'Settings', icon: 'gear' }],
  },
];

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
      background: linear-gradient(135deg, var(--scion-primary, #3b82f6) 0%, #1d4ed8 100%);
      border-radius: 0.5rem;
      color: white;
      font-weight: 700;
      font-size: 1rem;
      flex-shrink: 0;
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

    .nav-footer {
      padding: 0.75rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .theme-toggle {
      display: flex;
      align-items: center;
      justify-content: center;
      width: 100%;
      padding: 0.5rem;
      border-radius: 0.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      cursor: pointer;
      transition: background 0.15s ease;
    }

    .theme-toggle:hover {
      background: var(--scion-border, #e2e8f0);
    }
  `;

  override render() {
    return html`
      <div class="logo">
        <div class="logo-icon">S</div>
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
      </nav>

      <div class="nav-footer">
        <button
          class="theme-toggle"
          @click=${(): void => this.toggleTheme()}
          title="Toggle theme"
          aria-label="Toggle dark mode"
        >
          <sl-icon name="sun-moon"></sl-icon>
        </button>
      </div>
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
   * Handle navigation link click
   */
  private handleNavClick(_e: Event, path: string): void {
    // Dispatch a custom event for the app shell to handle
    this.dispatchEvent(
      new CustomEvent('nav-click', {
        detail: { path },
        bubbles: true,
        composed: true,
      })
    );
  }

  /**
   * Toggle between light and dark theme
   */
  private toggleTheme(): void {
    const html = document.documentElement;
    const currentTheme = html.getAttribute('data-theme');
    const newTheme = currentTheme === 'dark' ? 'light' : 'dark';

    html.setAttribute('data-theme', newTheme);

    if (newTheme === 'dark') {
      html.classList.add('sl-theme-dark');
    } else {
      html.classList.remove('sl-theme-dark');
    }

    // Persist preference
    localStorage.setItem('scion-theme', newTheme);

    this.dispatchEvent(
      new CustomEvent('theme-change', {
        detail: { theme: newTheme },
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
