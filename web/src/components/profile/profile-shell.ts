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
import type { User } from '../../shared/types.js';
import { performLogout } from '../../utils/auth.js';
import { setDocumentTitle } from '../../client/page-title.js';
import { enterAppFrame, exitAppFrame } from '../shared/app-frame.js';

const PROFILE_TITLES: Record<string, string> = {
  '/profile': 'Profile',
  '/profile/env': 'Environment Variables',
  '/profile/secrets': 'Secrets',
  '/profile/settings': 'Settings',
  '/profile/tokens': 'Access Tokens',
  '/profile/skills': 'Skills',
  '/profile/templates': 'Templates',
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

  static override styles = css`
    :host {
      display: flex;
      height: var(--scion-app-height, 100dvh);
      background: var(--scion-bg, #f8fafc);
      touch-action: manipulation;
    }

    .sidebar {
      display: flex;
      flex-shrink: 0;
      position: sticky;
      top: 0;
      height: var(--scion-app-height, 100dvh);
      /* Landscape on a notched phone (the page uses viewport-fit=cover):
         the nav moves clear of the notch, and the sidebar paints the nav's
         surface under the gap. 0 elsewhere. */
      padding-left: env(safe-area-inset-left, 0px);
      background: var(--scion-surface, #ffffff);
    }

    /* The sidebar sits between the header and the left edge and takes the
       left inset itself, so the header does not repeat it. */
    scion-header {
      --scion-header-inset-left: 0px;
    }

    @media (max-width: 768px) {
      .sidebar {
        display: none;
      }
      scion-header {
        --scion-header-inset-left: env(safe-area-inset-left, 0px);
      }
    }

    sl-drawer:not(:defined) {
      display: none;
    }

    /* The drawer grows by the left inset and pads it, so the nav keeps its
       width and clears the notch in landscape. 0 elsewhere. */
    .mobile-drawer {
      --size: calc(280px + env(safe-area-inset-left, 0px));
    }

    .mobile-drawer::part(panel) {
      background: var(--scion-surface, #ffffff);
      padding-left: env(safe-area-inset-left, 0px);
      box-sizing: border-box;
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

    /* Clear of the home indicator, notch and rounded corners (the page
       uses viewport-fit=cover); every inset is 0 elsewhere. The left edge
       meets the screen only once the sidebar gives way to the drawer, as
       the sidebar takes the left inset itself. */
    .content {
      flex: 1;
      padding: 1.5rem;
      padding-bottom: max(1.5rem, env(safe-area-inset-bottom, 0px));
      padding-right: max(1.5rem, env(safe-area-inset-right, 0px));
      overflow: auto;
      display: flex;
      flex-direction: column;
    }

    @media (max-width: 768px) {
      .content {
        padding-left: max(1.5rem, env(safe-area-inset-left, 0px));
      }
    }

    @media (max-width: 640px) {
      .content {
        padding: 1rem;
        padding-bottom: max(1rem, env(safe-area-inset-bottom, 0px));
        padding-inline: max(1rem, env(safe-area-inset-left, 0px))
          max(1rem, env(safe-area-inset-right, 0px));
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
    enterAppFrame();
    try {
      this._sidebarCollapsed = localStorage.getItem('scion-sidebar-collapsed') === 'true';
    } catch {
      // localStorage may be unavailable (SecurityError in restricted contexts)
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    exitAppFrame();
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
    `;
  }

  private getPageTitle(): string {
    return PROFILE_TITLES[this.currentPath] || 'Profile';
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

  /**
   * Handle logout action.
   * Delegates to shared performLogout() utility (design doc Section 4.1).
   */
  private handleLogout(): void {
    performLogout();
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-profile-shell': ScionProfileShell;
  }
}
