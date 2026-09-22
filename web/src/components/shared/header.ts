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
 * Provides the top header bar with breadcrumb, user menu, and actions.
 *
 * Layout: two-column grid —
 *   Left:  hamburger (mobile) + page title
 *   Right: mode-selector dropdown + user/account dropdown
 *
 * All header actions (inbox, notifications, help, theme, profile, sign out)
 * are consolidated into the user dropdown so they remain accessible at every
 * viewport width.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { User } from '../../shared/types.js';
import { isFeatureEnabled } from '../../utils/feature-flags.js';
import { apiFetch } from '../../client/api.js';
import { stateManager } from '../../client/state.js';
import { TERMINAL_SESSION_COUNT_EVENT } from '../../client/terminal-workspace-events.js';
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
const TERMINAL_WORKSPACE_FLAG = 'web.terminal_workspace';

// Header instances in the app shell and retained terminal workspace share one
// document-level mode memory so switching views restores the same last paths.
const rememberedModePaths = {
  dashboard: '/',
  chat: '/chat',
};

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

  @state()
  private terminalSessionCount = 0;

  /** Unread message count from the inbox tray (best-effort sync). */
  @state()
  private inboxCount = 0;

  /** Unacknowledged notification count from the notification tray. */
  @state()
  private notificationCount = 0;

  static override styles = css`
    /* ------------------------------------------------------------------ */
    /* Grid: two columns — title (left) + dropdowns (right)               */
    /* ------------------------------------------------------------------ */
    :host {
      display: grid;
      grid-template-columns: 1fr auto;
      align-items: center;
      height: var(--scion-header-height, 60px);
      padding: 0 1.5rem;
      background: var(--scion-surface, #ffffff);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    /* ------------------------------------------------------------------ */
    /* Left column                                                        */
    /* ------------------------------------------------------------------ */
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

    /* ------------------------------------------------------------------ */
    /* Right column                                                       */
    /* ------------------------------------------------------------------ */
    .header-right {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      justify-self: end;
      position: relative;
    }

    /* ------------------------------------------------------------------ */
    /* Mode-selector dropdown trigger                                     */
    /* ------------------------------------------------------------------ */
    .mode-trigger {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.375rem 0.625rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      background: transparent;
      color: var(--scion-text, #1e293b);
      cursor: pointer;
      font-size: 0.875rem;
      font-weight: 500;
      transition:
        background 0.15s ease,
        border-color 0.15s ease;
    }

    .mode-trigger:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
      border-color: var(--scion-text-muted, #64748b);
    }

    .mode-trigger sl-icon {
      font-size: 1.125rem;
    }

    .mode-trigger .caret {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .mode-dropdown-label {
      font-size: 0.875rem;
      font-weight: 500;
    }

    @media (max-width: 768px) {
      .mode-dropdown-label {
        display: none;
      }
    }

    /* ------------------------------------------------------------------ */
    /* User/account dropdown trigger                                      */
    /* ------------------------------------------------------------------ */
    .user-trigger {
      position: relative;
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.375rem 0.5rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      background: transparent;
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      transition:
        background 0.15s ease,
        border-color 0.15s ease;
    }

    .user-trigger:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
      border-color: var(--scion-text-muted, #64748b);
    }

    .user-trigger sl-icon {
      font-size: 1.125rem;
    }

    .user-trigger .caret {
      font-size: 0.75rem;
    }

    /* Small red dot indicating unread messages or notifications */
    .trigger-badge {
      position: absolute;
      top: 2px;
      right: 2px;
      width: 8px;
      height: 8px;
      border-radius: 50%;
      background: var(--scion-danger, #ef4444);
      pointer-events: none;
    }

    /* ------------------------------------------------------------------ */
    /* Menu-item count badge (e.g. "3" next to Messages)                  */
    /* ------------------------------------------------------------------ */
    .count-badge {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      min-width: 18px;
      height: 18px;
      padding: 0 5px;
      border-radius: 9px;
      background: var(--scion-primary, #3b82f6);
      color: #fff;
      font-size: 0.6875rem;
      font-weight: 700;
      line-height: 1;
    }

    /* ------------------------------------------------------------------ */
    /* Sign-in link (when user is null)                                   */
    /* ------------------------------------------------------------------ */
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
  `;

  // =========================================================================
  // Render
  // =========================================================================

  override render(): TemplateResult {
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
        ${this.renderModeDropdown()}
        ${this.user
          ? this.renderUserDropdown()
          : html`
              <a href="/auth/login" class="sign-in-link">
                <sl-icon name="box-arrow-in-right"></sl-icon>
                Sign in
              </a>
            `}

        <!-- Tray components: triggers hidden, panels open programmatically -->
        <scion-inbox-tray .user=${this.user}></scion-inbox-tray>
        <scion-notification-tray .user=${this.user}></scion-notification-tray>
      </div>
    `;
  }

  // =========================================================================
  // Mode-selector dropdown
  // =========================================================================

  /**
   * Dropdown for switching between Dashboard / Chat / Terminal modes.
   * Returns nothing when no alternative modes are feature-flagged on.
   */
  private renderModeDropdown(): TemplateResult | typeof nothing {
    const chatEnabled = isFeatureEnabled(NATIVE_CHAT_FLAG);
    const terminalsEnabled = isFeatureEnabled(TERMINAL_WORKSPACE_FLAG);
    if (!chatEnabled && !terminalsEnabled) return nothing;

    const { icon, label } = this.getCurrentMode();
    const isChat = this.isChatView();
    const isTerminal = this.isTerminalView();

    return html`
      <sl-dropdown>
        <button slot="trigger" class="mode-trigger" aria-label="Switch view mode">
          <sl-icon name=${icon}></sl-icon>
          <span class="mode-dropdown-label">${label}</span>
          <sl-icon class="caret" name="chevron-down"></sl-icon>
        </button>
        <sl-menu
          @sl-select=${(e: CustomEvent<{ item: { value: string } }>): void =>
            this.handleModeSelect(e)}
        >
          <sl-menu-item value="dashboard" ?checked=${!isChat && !isTerminal}>
            <sl-icon slot="prefix" name="house"></sl-icon>
            Dashboard
          </sl-menu-item>
          ${chatEnabled
            ? html`
                <sl-menu-item value="chat" ?checked=${isChat}>
                  <sl-icon slot="prefix" name="chat-dots"></sl-icon>
                  Chat
                </sl-menu-item>
              `
            : ''}
          ${terminalsEnabled
            ? html`
                <sl-menu-item value="terminals" ?checked=${isTerminal}>
                  <sl-icon slot="prefix" name="terminal"></sl-icon>
                  Terminal
                </sl-menu-item>
              `
            : ''}
        </sl-menu>
      </sl-dropdown>
    `;
  }

  /** Icon and label for the currently active mode. */
  private getCurrentMode(): { icon: string; label: string } {
    if (this.isChatView()) return { icon: 'chat-dots', label: 'Chat' };
    if (this.isTerminalView()) return { icon: 'terminal', label: 'Terminal' };
    return { icon: 'house', label: 'Dashboard' };
  }

  /** Handle mode selection from the dropdown menu. */
  private handleModeSelect(e: CustomEvent<{ item: { value: string } }>): void {
    const value = e.detail.item.value;
    if (value === 'dashboard' || value === 'chat' || value === 'terminals') {
      void this.handleModeSwitch(value);
    }
  }

  // =========================================================================
  // User/account dropdown
  // =========================================================================

  /**
   * Dropdown consolidating inbox, notifications, profile, help, theme, and
   * sign-out into a single compact trigger button (person icon).
   */
  private renderUserDropdown(): TemplateResult {
    const hasUnread = this.inboxCount + this.notificationCount > 0;

    return html`
      <sl-dropdown class="user-dropdown">
        <button slot="trigger" class="user-trigger" aria-label="Account menu">
          <sl-icon name="person"></sl-icon>
          <sl-icon class="caret" name="chevron-down"></sl-icon>
          ${hasUnread ? html`<span class="trigger-badge"></span>` : ''}
        </button>
        <sl-menu
          @sl-select=${(e: CustomEvent<{ item: { value: string } }>): void =>
            this.handleUserMenuSelect(e)}
        >
          <sl-menu-item value="messages">
            <sl-icon slot="prefix" name="envelope"></sl-icon>
            Messages
            ${this.inboxCount > 0
              ? html`<span slot="suffix" class="count-badge">${this.inboxCount}</span>`
              : ''}
          </sl-menu-item>
          <sl-menu-item value="notifications">
            <sl-icon slot="prefix" name="bell"></sl-icon>
            Notifications
            ${this.notificationCount > 0
              ? html`<span slot="suffix" class="count-badge"
                  >${this.notificationCount}</span
                >`
              : ''}
          </sl-menu-item>

          <sl-divider></sl-divider>

          <sl-menu-item value="profile">
            <sl-icon slot="prefix" name="person"></sl-icon>
            Profile
          </sl-menu-item>
          <sl-menu-item value="help">
            <sl-icon slot="prefix" name="question-circle"></sl-icon>
            Help
          </sl-menu-item>
          <sl-menu-item value="theme">
            <sl-icon
              slot="prefix"
              name=${this.isDark ? 'sun' : 'moon'}
            ></sl-icon>
            ${this.isDark ? 'Light Mode' : 'Dark Mode'}
          </sl-menu-item>

          <sl-divider></sl-divider>

          <sl-menu-item value="logout">
            <sl-icon slot="prefix" name="box-arrow-right"></sl-icon>
            Sign Out
          </sl-menu-item>
        </sl-menu>
      </sl-dropdown>
    `;
  }

  /** Route user-dropdown menu selections to the correct handler. */
  private handleUserMenuSelect(e: CustomEvent<{ item: { value: string } }>): void {
    const value = e.detail.item.value;

    switch (value) {
      case 'messages':
        // Close dropdown first, then open inbox tray after a frame
        this.closeUserDropdown();
        requestAnimationFrame(() => this.openInboxTray());
        break;

      case 'notifications':
        this.closeUserDropdown();
        requestAnimationFrame(() => this.openNotificationTray());
        break;

      case 'profile':
        this.dispatchEvent(
          new CustomEvent('nav-click', {
            detail: { path: '/profile' },
            bubbles: true,
            composed: true,
          })
        );
        break;

      case 'help':
        window.open(DOCS_URL, '_blank', 'noopener,noreferrer');
        break;

      case 'theme':
        this.toggleTheme();
        break;

      case 'logout':
        this.handleLogout();
        break;
    }
  }

  /** Programmatically close the user dropdown. */
  private closeUserDropdown(): void {
    const dropdown = this.shadowRoot?.querySelector('.user-dropdown') as
      | { hide: () => void }
      | undefined;
    dropdown?.hide();
  }

  // =========================================================================
  // Tray integration
  // =========================================================================

  /**
   * Programmatically open the inbox tray by clicking its (hidden) trigger
   * button. This reuses the tray's own toggle logic, including the
   * click-outside handler and data refresh.
   */
  private openInboxTray(): void {
    const tray = this.shadowRoot?.querySelector('scion-inbox-tray');
    if (!tray) return;
    const btn = tray.shadowRoot?.querySelector('.inbox-btn') as HTMLElement | null;
    btn?.click();
  }

  /**
   * Programmatically open the notification tray by clicking its (hidden)
   * trigger button.
   */
  private openNotificationTray(): void {
    const tray = this.shadowRoot?.querySelector('scion-notification-tray');
    if (!tray) return;
    const btn = tray.shadowRoot?.querySelector('.bell-btn') as HTMLElement | null;
    btn?.click();
  }

  /**
   * Apply visually-hidden styles to the tray trigger buttons so they are
   * invisible but remain functional for programmatic clicks. The panels
   * (siblings of the buttons in the tray's shadow DOM) are unaffected.
   */
  private hideTrayTriggers(): void {
    const hide = (el: HTMLElement | null): void => {
      if (!el) return;
      el.style.cssText =
        'position:absolute;width:1px;height:1px;overflow:hidden;' +
        'clip:rect(0,0,0,0);white-space:nowrap;border:0;padding:0;margin:-1px;';
    };

    const inboxTray = this.shadowRoot?.querySelector('scion-inbox-tray');
    const notifTray = this.shadowRoot?.querySelector('scion-notification-tray');

    hide(inboxTray?.shadowRoot?.querySelector('.inbox-btn') as HTMLElement | null);
    hide(notifTray?.shadowRoot?.querySelector('.bell-btn') as HTMLElement | null);
  }

  /**
   * Sync the header's badge counts with the tray components' internal state.
   * The trays manage their own polling / SSE subscriptions — we just read
   * their array lengths after a short delay to let their fetch settle.
   */
  private syncTrayCounts(): void {
    setTimeout(() => {
      if (!this.isConnected) return;
      const inbox = this.shadowRoot?.querySelector('scion-inbox-tray') as
        | (Element & { messages?: unknown[] })
        | null;
      const notif = this.shadowRoot?.querySelector('scion-notification-tray') as
        | (Element & { notifications?: unknown[] })
        | null;

      const newInbox = inbox?.messages?.length ?? 0;
      const newNotif = notif?.notifications?.length ?? 0;
      if (this.inboxCount !== newInbox) this.inboxCount = newInbox;
      if (this.notificationCount !== newNotif) this.notificationCount = newNotif;
    }, 500);
  }

  /** Bound handler for SSE tray-count events. */
  private readonly handleTrayCountEvent = (): void => {
    this.syncTrayCounts();
  };

  // =========================================================================
  // View helpers
  // =========================================================================

  /**
   * Whether the header is rendering above the chat view. The chat view has no
   * sidebar of its own, so the header carries the Scion logo there in place of
   * the page title.
   */
  private isChatView(): boolean {
    const path = this.currentPath || window.location.pathname;
    return path.startsWith('/chat');
  }

  private isTerminalView(): boolean {
    const path = this.currentPath || window.location.pathname;
    return path === '/terminals' || path.startsWith('/terminals/');
  }

  // =========================================================================
  // Mode switch navigation
  // =========================================================================

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
  private async handleModeSwitch(targetMode: 'dashboard' | 'chat' | 'terminals'): Promise<void> {
    const currentPath = this.currentPath || window.location.pathname;
    let target: string;

    if (targetMode === 'terminals') {
      target = '/terminals';
    } else if (targetMode === 'chat') {
      if (rememberedModePaths.chat && rememberedModePaths.chat.startsWith('/chat')) {
        target = rememberedModePaths.chat;
      } else {
        // Dashboard -> Chat: carry the project ID into a space URL.
        const projectId = projectIdFromDashboardPath(currentPath);
        target = projectId ? `/chat/space/${encodeURIComponent(projectId)}` : '/chat';
      }
    } else {
      if (rememberedModePaths.dashboard && !rememberedModePaths.dashboard.startsWith('/chat')) {
        target = rememberedModePaths.dashboard;
      } else {
        // Chat -> Dashboard: resolve project ID from the chat URL.
        const projectId = projectIdFromChatSpacePath(currentPath);
        if (projectId) {
          target = `/projects/${encodeURIComponent(projectId)}`;
        } else {
          const slug = slugFromChatPath(currentPath);
          if (slug) {
            const resolvedId = await this.resolveProjectIdBySlug(slug);
            target = resolvedId ? `/projects/${encodeURIComponent(resolvedId)}` : '/';
          } else {
            target = '/';
          }
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
      const res = await apiFetch(`/api/v1/projects?slug=${encodeURIComponent(slug)}&limit=1`);
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

  // =========================================================================
  // Lifecycle
  // =========================================================================

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
    window.addEventListener(
      TERMINAL_SESSION_COUNT_EVENT,
      this.handleTerminalSessionCount as EventListener
    );
    this.rememberModePath();

    // Listen for SSE events to keep tray badge counts in sync.
    stateManager.addEventListener('user-message-created', this.handleTrayCountEvent);
    stateManager.addEventListener('notification-created', this.handleTrayCountEvent);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener(
      TERMINAL_SESSION_COUNT_EVENT,
      this.handleTerminalSessionCount as EventListener
    );
    stateManager.removeEventListener('user-message-created', this.handleTrayCountEvent);
    stateManager.removeEventListener('notification-created', this.handleTrayCountEvent);
  }

  override firstUpdated(): void {
    // Give the tray components a frame to finish their first render so
    // their shadow DOMs are ready, then hide their trigger buttons and
    // read initial badge counts.
    requestAnimationFrame(() => {
      this.hideTrayTriggers();
      this.syncTrayCounts();
    });
  }

  override updated(changedProperties: Map<string, unknown>): void {
    if (changedProperties.has('currentPath')) this.rememberModePath();
  }

  // =========================================================================
  // Event handlers
  // =========================================================================

  private readonly handleTerminalSessionCount = (event: CustomEvent<{ count?: number }>): void => {
    this.terminalSessionCount = Math.max(0, event.detail?.count ?? 0);
  };

  private rememberModePath(): void {
    const path = this.currentPath || window.location.pathname;
    if (path.startsWith('/chat')) {
      rememberedModePaths.chat = path;
    } else if (path !== '/terminals' && !path.startsWith('/terminals/')) {
      rememberedModePaths.dashboard = path || '/';
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
