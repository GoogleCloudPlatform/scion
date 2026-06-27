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
 * Profile Shell Component
 *
 * Layout shell for the profile/settings section. Uses a profile-specific
 * sidebar navigation instead of the main hub navigation.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import './profile-nav.js';
import '../shared/header.js';
import '../shared/debug-panel.js';

import type { User } from '../../shared/types.js';
import { LocaleController, LOCALE_CHANGE_EVENT } from '../../client/i18n.js';
import { setDocumentTitle } from '../../client/page-title.js';

const PROFILE_TITLES: Record<string, string> = {
  '/profile': 'Profile',
  '/profile/env': 'Environment Variables',
  '/profile/secrets': 'Secrets',
  '/profile/settings': 'Settings',
  '/profile/tokens': 'Access Tokens',
};

@customElement('scion-profile-shell')
export class ScionProfileShell extends LitElement {
  @property({ type: Object })
  user: User | null = null;

  @property({ type: String })
  currentPath = '/profile';

  @state()
  _drawerOpen = false;

  @state()
  _sidebarCollapsed = false;

  private _localeChangeHandler = this.handleLocaleChange.bind(this);

  private locale = new LocaleController(this);

  static override styles = css`
    :host {
      display: flex;
      height: 100vh;
      height: 100dvh;
      background: var(--scion-bg, #f8fafc);
    }

    .sidebar {
      display: flex;
      flex-shrink: 0;
      position: sticky;
      top: 0;
      height: 100vh;
    }

    @media (max-width: 768px) {
      .sidebar {
        display: none;
      }
    }

    sl-drawer:not(:defined) {
      display: none;
    }

    .mobile-drawer {
      --size: 280px;
    }

    .mobile-drawer::part(panel) {
      background: var(--scion-surface, #ffffff);
    }

    .mobile-drawer::part(close-button) {
      color: var(--scion-text, #1e293b);
    }

    .mobile-drawer::part(close-button):hover {
      color: var(--scion-primary, #3b82f6);
    }

    .main {
      flex: 1;
      display: flex;
      flex-direction: column;
      min-width: 0;
    }

    .content {
      flex: 1;
      padding: 1.5rem;
      overflow: auto;
      display: flex;
      flex-direction: column;
    }

    @media (max-width: 640px) {
      .content {
        padding: 1rem;
      }
    }

    .content-inner {
      max-width: var(--scion-content-max-width, 1400px);
      margin: 0 auto;
      width: 100%;
      flex: 1;
      display: flex;
      flex-direction: column;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener(LOCALE_CHANGE_EVENT, this._localeChangeHandler as EventListener);
    try {
      this._sidebarCollapsed = localStorage.getItem('scion-sidebar-collapsed') === 'true';
    } catch {
      // localStorage may be unavailable (SecurityError in restricted contexts)
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener(LOCALE_CHANGE_EVENT, this._localeChangeHandler as EventListener);
  }

  override updated(changedProperties: Map<string, unknown>): void {
    if (changedProperties.has('currentPath')) {
      this.updateDocumentTitle();
    }
  }

  private updateDocumentTitle(): void {
    const title = this.getPageTitle();
    setDocumentTitle(title, 'Profile');
  }

  private handleLocaleChange(): void {
    this.updateDocumentTitle();
  }

  override render() {
    const pageTitle = this.getPageTitle();

    return html`
      <aside class="sidebar">
        <scion-profile-nav
          .user=${this.user}
          .currentPath=${this.currentPath}
          ?collapsed=${this._sidebarCollapsed}
          @sidebar-toggle=${(): void => this.handleSidebarToggle()}
        ></scion-profile-nav>
      </aside>

      <sl-drawer
        class="mobile-drawer"
        ?open=${this._drawerOpen}
        placement="start"
        @sl-hide=${(): void => this.handleDrawerClose()}
      >
        <scion-profile-nav
          .user=${this.user}
          .currentPath=${this.currentPath}
          .hideCollapse=${true}
        ></scion-profile-nav>
      </sl-drawer>

      <main class="main">
        <scion-header
          .user=${this.user}
          .currentPath=${this.currentPath}
          .pageTitle=${pageTitle}
          ?showMobileMenu=${true}
          @mobile-menu-toggle=${(): void => this.handleMobileMenuToggle()}
          @logout=${(): void => this.handleLogout()}
        ></scion-header>

        <div class="content">
          <div class="content-inner">
            <slot></slot>
          </div>
        </div>
      </main>

      <scion-debug-panel></scion-debug-panel>
    `;
  }

  private getPageTitle(): string {
    return this.locale.t(PROFILE_TITLES[this.currentPath] || 'Profile');
  }

  private handleSidebarToggle(): void {
    this._sidebarCollapsed = !this._sidebarCollapsed;
    try {
      localStorage.setItem('scion-sidebar-collapsed', String(this._sidebarCollapsed));
    } catch {
      // localStorage may be unavailable (SecurityError in restricted contexts)
    }
  }

  private handleMobileMenuToggle(): void {
    this._drawerOpen = !this._drawerOpen;
  }

  private handleDrawerClose(): void {
    this._drawerOpen = false;
  }

  private handleLogout(): void {
    fetch('/auth/logout', {
      method: 'POST',
      credentials: 'include',
    })
      .then(() => {
        window.location.href = '/auth/login';
      })
      .catch((error) => {
        console.error('Logout failed:', error);
      });
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-profile-shell': ScionProfileShell;
  }
}
