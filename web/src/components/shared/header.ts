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
 * Header Component
 *
 * Provides the top header bar with breadcrumb, user menu, and actions
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { User } from '../../shared/types.js';
import { isFeatureEnabled } from '../../utils/feature-flags.js';
import { apiFetch } from '../../client/api.js';
import './notification-tray.js';
import './inbox-tray.js';

// ---------------------------------------------------------------------------
// Project-context helpers for the dashboard ↔ chat mode switch.
//
// These are pure functions exported for testing — they map URL paths to
// the project identifier that should carry across the view toggle.
// ---------------------------------------------------------------------------

/** Extract a project ID from a dashboard-style path (`/projects/:id/…`). */
export function projectIdFromDashboardPath(path: string): string | null {
  const m = path.match(/^\/projects\/([^/?#]+)/);
  // `/projects/new` is the creation form, not a project-scoped page.
  return m && m[1] !== 'new' ? m[1] : null;
}

/** Extract a project ID from a legacy chat space path (`/chat/space/:id/…`). */
export function projectIdFromChatSpacePath(path: string): string | null {
  const m = path.match(/^\/chat\/space\/([^/?#]+)/);
  return m ? m[1] : null;
}

/**
 * Extract a project slug from a readable chat path (`/chat/:slug` or
 * `/chat/:slug/:threadId`). Returns null for space, dm, and bare `/chat`
 * paths — those are handled by dedicated helpers or have no project context.
 */
export function slugFromChatPath(path: string): string | null {
  if (/^\/chat\/space\//.test(path)) return null;
  if (/^\/chat\/dm\//.test(path)) return null;
  const m = path.match(/^\/chat\/([^/?#]+)/);
  return m ? m[1] : null;
}

/** URL for the Scion documentation site, opened by the Help button. */
const DOCS_URL = 'https://googlecloudplatform.github.io/scion/overview/';

/** Feature flag gating the chat mode (and therefore the mode switch). */
const NATIVE_CHAT_FLAG = 'web.native_chat';

@customElement('scion-header')
export class ScionHeader extends LitElement {
  /**
   * Current authenticated user
   */
  @property({ type: Object })
  user: User | null = null;

  /**
   * Current page path for breadcrumb
   */
  @property({ type: String })
  currentPath = '/';

  /**
   * Page title to display
   */
  @property({ type: String })
  pageTitle = 'Dashboard';

  /**
   * Whether to show the mobile menu button
   */
  @property({ type: Boolean })
  showMobileMenu = false;

  @state()
  private isDark = false;

  static override styles = css`
    :host {
      display: flex;
      align-items: center;
      justify-content: space-between;
      height: var(--scion-header-height, 60px);
      padding: 0 1.5rem;
      background: var(--scion-surface, #ffffff);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .header-left {
      display: flex;
      align-items: center;
      gap: 1rem;
    }

    .mobile-menu-btn {
      display: none;
      padding: 0.5rem;
      background: transparent;
      border: none;
      border-radius: 0.375rem;
      cursor: pointer;
      color: var(--scion-text, #1e293b);
    }

    .mobile-menu-btn:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    @media (max-width: 768px) {
      .mobile-menu-btn {
        display: flex;
      }
    }

    .page-title {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    /*
     * On the chat view the logo stands in for the page title, so it is sized
     * to sit inline within the 60px header rather than as a sidebar block.
     */
    .logo {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .logo-icon {
      font-size: 1.5rem;
      line-height: 1;
    }

    .logo-text h1 {
      margin: 0;
      font-size: 1.125rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
    }

    .header-right {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .header-actions {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    /* Chat ↔ dashboard mode switch */
    .mode-switch {
      display: flex;
      align-items: center;
      gap: 0.125rem;
      padding: 0.125rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.5rem;
    }

    .mode-switch sl-icon-button::part(base) {
      padding: 0.25rem 0.5rem;
      color: var(--scion-text-muted, #64748b);
    }

    .mode-switch sl-icon-button.active::part(base) {
      background: var(--scion-primary, #3b82f6);
      color: white;
      border-radius: 0.375rem;
    }

    @media (max-width: 640px) {
      .header-actions {
        display: none;
      }
    }

    .user-section {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .sign-in-link {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 1rem;
      border-radius: 0.5rem;
      background: var(--scion-primary, #3b82f6);
      color: white;
      text-decoration: none;
      font-size: 0.875rem;
      font-weight: 500;
      transition: background 0.15s ease;
    }

    .sign-in-link:hover {
      background: var(--scion-primary-hover, #2563eb);
    }

    .user-buttons {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .profile-link {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 1rem;
      border-radius: 0.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text, #1e293b);
      text-decoration: none;
      font-size: 0.875rem;
      font-weight: 500;
      border: 1px solid var(--scion-border, #e2e8f0);
      transition:
        background 0.15s ease,
        border-color 0.15s ease;
    }

    .profile-link:hover {
      background: var(--scion-border, #e2e8f0);
      border-color: var(--scion-text-muted, #64748b);
    }

    .sign-out-button {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 1rem;
      border-radius: 0.5rem;
      background: transparent;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      font-weight: 500;
      border: 1px solid var(--scion-border, #e2e8f0);
      cursor: pointer;
      transition:
        background 0.15s ease,
        color 0.15s ease,
        border-color 0.15s ease;
    }

    .sign-out-button:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text, #1e293b);
      border-color: var(--scion-text-muted, #64748b);
    }

    .theme-switch {
      display: flex;
      align-items: center;
      gap: 0.375rem;
    }

    .theme-switch sl-icon {
      font-size: 0.9rem;
      color: var(--scion-text-muted, #64748b);
      transition: color 0.2s ease;
    }

    .theme-switch sl-icon.active-icon {
      color: var(--scion-primary, #3b82f6);
    }

    .toggle-track {
      position: relative;
      width: 36px;
      height: 20px;
      background: var(--scion-border, #e2e8f0);
      border-radius: 10px;
      cursor: pointer;
      transition: background 0.2s ease;
      border: none;
      padding: 0;
    }

    .toggle-track:hover {
      background: var(--scion-text-muted, #94a3b8);
    }

    .toggle-track.dark {
      background: var(--scion-primary, #3b82f6);
    }

    .toggle-knob {
      position: absolute;
      top: 2px;
      left: 2px;
      width: 16px;
      height: 16px;
      background: white;
      border-radius: 50%;
      transition: transform 0.2s ease;
      pointer-events: none;
    }

    .toggle-track.dark .toggle-knob {
      transform: translateX(16px);
    }
  `;

  override render() {
    return html`
      <div class="header-left">
        ${this.showMobileMenu
          ? html`
              <button
                class="mobile-menu-btn"
                @click=${(): void => this.handleMobileMenuClick()}
                aria-label="Open navigation menu"
              >
                <sl-icon name="list" style="font-size: 1.25rem;"></sl-icon>
              </button>
            `
          : ''}
        ${this.isChatView()
          ? html`
              <div class="logo">
                <div class="logo-icon">🌱</div>
                <div class="logo-text">
                  <h1>Scion Chat</h1>
                </div>
              </div>
            `
          : html`<h1 class="page-title">${this.pageTitle}</h1>`}
      </div>

      <div class="header-right">
        ${this.renderModeSwitch()}
        <div class="header-actions">
          <scion-inbox-tray .user=${this.user}></scion-inbox-tray>
          <scion-notification-tray .user=${this.user}></scion-notification-tray>
          <sl-tooltip content="Help">
            <sl-icon-button
              name="question-circle"
              label="Help"
              @click=${(): void => {
                window.open(DOCS_URL, '_blank', 'noopener,noreferrer');
              }}
            ></sl-icon-button>
          </sl-tooltip>
          <div class="theme-switch">
            <sl-icon name="sun" class=${this.isDark ? '' : 'active-icon'}></sl-icon>
            <button
              class="toggle-track ${this.isDark ? 'dark' : ''}"
              @click=${(): void => this.toggleTheme()}
              aria-label="Toggle dark mode"
            >
              <span class="toggle-knob"></span>
            </button>
            <sl-icon name="moon" class=${this.isDark ? 'active-icon' : ''}></sl-icon>
          </div>
        </div>

        <div class="user-section">${this.renderUserSection()}</div>
      </div>
    `;
  }

  /**
   * Whether the header is rendering above the chat view. The chat view has no
   * sidebar of its own, so the header carries the Scion logo there in place of
   * the page title.
   */
  private isChatView(): boolean {
    const path = this.currentPath || window.location.pathname;
    return path.startsWith('/chat');
  }

  /**
   * Toggle between the dashboard and chat views. Chat is a peer view of the
   * dashboard, so the switch lives in the header rather than in either
   * sidebar — it is the one control present in both shells.
   */
  private renderModeSwitch() {
    if (!isFeatureEnabled(NATIVE_CHAT_FLAG)) return '';

    const isChat = this.isChatView();

    return html`
      <div class="mode-switch" role="group" aria-label="Switch view">
        <sl-tooltip content="Dashboard">
          <sl-icon-button
            name="house"
            label="Dashboard"
            class=${isChat ? '' : 'active'}
            @click=${() => { this.handleModeSwitch('/'); }}
          ></sl-icon-button>
        </sl-tooltip>
        <sl-tooltip content="Chat">
          <sl-icon-button
            name="chat-dots"
            label="Chat"
            class=${isChat ? 'active' : ''}
            @click=${() => { this.handleModeSwitch('/chat'); }}
          ></sl-icon-button>
        </sl-tooltip>
      </div>
    `;
  }

  /**
   * Navigate to the given mode, preserving project context when possible.
   *
   * Dashboard → Chat:  /projects/:id/… → /chat/space/:id
   * Chat → Dashboard:  /chat/space/:id/… → /projects/:id
   *                     /chat/:slug/…     → (resolve slug) → /projects/:id
   *                     /chat/dm/…        → / (no project context)
   *
   * Uses the same nav-click event as the sidebar so the router handles it
   * identically in both the app and chat shells.
   */
  private async handleModeSwitch(targetBase: string): Promise<void> {
    const currentPath = this.currentPath || window.location.pathname;
    let target: string;

    if (targetBase === '/chat') {
      // Dashboard → Chat: carry the project ID into a space URL.
      const projectId = projectIdFromDashboardPath(currentPath);
      target = projectId
        ? `/chat/space/${encodeURIComponent(projectId)}`
        : '/chat';
    } else {
      // Chat → Dashboard: resolve project ID from the chat URL.
      const projectId = projectIdFromChatSpacePath(currentPath);
      if (projectId) {
        target = `/projects/${encodeURIComponent(projectId)}`;
      } else {
        const slug = slugFromChatPath(currentPath);
        if (slug) {
          const resolvedId = await this.resolveProjectIdBySlug(slug);
          target = resolvedId
            ? `/projects/${encodeURIComponent(resolvedId)}`
            : '/';
        } else {
          target = '/';
        }
      }
    }

    // Guard: component may have disconnected during async slug resolution.
    if (!this.isConnected) return;

    this.dispatchEvent(
      new CustomEvent('nav-click', {
        detail: { path: target },
        bubbles: true,
        composed: true,
      })
    );
  }

  /**
   * Look up a project by slug via the projects API, returning the project ID
   * or an empty string when the slug cannot be resolved.
   */
  private async resolveProjectIdBySlug(slug: string): Promise<string> {
    try {
      const res = await apiFetch(
        `/api/v1/projects?slug=${encodeURIComponent(slug)}&limit=1`
      );
      if (res.ok) {
        const data = (await res.json()) as {
          items?: Array<{ id: string; slug: string }>;
        };
        if (data.items && data.items.length > 0) {
          return data.items[0].id;
        }
      }
    } catch {
      // Slug resolution is best-effort; fall back to the top-level view.
    }
    return '';
  }

  private renderUserSection() {
    if (!this.user) {
      return html`
        <a href="/auth/login" class="sign-in-link">
          <sl-icon name="box-arrow-in-right"></sl-icon>
          Sign in
        </a>
      `;
    }

    return html`
      <div class="user-buttons">
        <a
          href="/profile"
          class="profile-link"
          @click=${(e: Event): void => this.handleProfileClick(e)}
        >
          <sl-icon name="person"></sl-icon>
          Profile
        </a>
        <button class="sign-out-button" @click=${(): void => this.handleLogout()}>
          <sl-icon name="box-arrow-right"></sl-icon>
          Sign out
        </button>
      </div>
    `;
  }

  override connectedCallback(): void {
    super.connectedCallback();
    const saved = localStorage.getItem('scion-theme');
    const prefersDark = window.matchMedia('(prefers-color-scheme: dark)').matches;
    this.isDark = saved ? saved === 'dark' : prefersDark;

    // Ensure the root element reflects the resolved theme so that Shoelace
    // components and CSS custom properties pick up the correct mode.
    const root = document.documentElement;
    if (this.isDark) {
      root.setAttribute('data-theme', 'dark');
      root.classList.add('sl-theme-dark');
    } else {
      root.setAttribute('data-theme', 'light');
      root.classList.remove('sl-theme-dark');
    }
  }

  private toggleTheme(): void {
    this.isDark = !this.isDark;
    const root = document.documentElement;
    const newTheme = this.isDark ? 'dark' : 'light';

    root.setAttribute('data-theme', newTheme);

    if (this.isDark) {
      root.classList.add('sl-theme-dark');
    } else {
      root.classList.remove('sl-theme-dark');
    }

    localStorage.setItem('scion-theme', newTheme);

    this.dispatchEvent(
      new CustomEvent('theme-change', {
        detail: { theme: newTheme },
        bubbles: true,
        composed: true,
      })
    );
  }

  /**
   * Handle profile link click with client-side navigation
   */
  private handleProfileClick(e: Event): void {
    e.preventDefault();
    this.dispatchEvent(
      new CustomEvent('nav-click', {
        detail: { path: '/profile' },
        bubbles: true,
        composed: true,
      })
    );
  }

  /**
   * Handle mobile menu button click
   */
  private handleMobileMenuClick(): void {
    this.dispatchEvent(
      new CustomEvent('mobile-menu-toggle', {
        bubbles: true,
        composed: true,
      })
    );
  }

  /**
   * Handle logout action
   */
  private handleLogout(): void {
    this.dispatchEvent(
      new CustomEvent('logout', {
        bubbles: true,
        composed: true,
      })
    );
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-header': ScionHeader;
  }
}
