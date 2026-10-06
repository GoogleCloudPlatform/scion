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
 * Three-tier responsive layout:
 *
 *   Wide (>1100px):     3-column grid -- title | segmented mode switch
 *                      (with labels) | inline actions + user section.
 *                      This is the original pre-redesign layout.
 *
 *   Medium (<=1100px):  2-column grid -- title | icon-only mode segments
 *                      + user dropdown.  Mode visibility preserved,
 *                      user actions consolidated into a single dropdown.
 *
 *   Narrow (<=768px):  2-column grid -- hamburger + title | mode dropdown
 *                      + user dropdown.  Fully collapsed for mobile.
 *
 * Tray components (inbox, notifications) are always present in the DOM
 * with hidden triggers; they are opened programmatically from either the
 * inline action buttons (wide) or the user dropdown (medium/narrow).
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { User } from '../../shared/types.js';
import { isFeatureEnabled, TERMINAL_WORKSPACE_FLAG } from '../../utils/feature-flags.js';
import { TouchPrimaryController } from '../../utils/input-modality.js';
import { apiFetch } from '../../client/api.js';
import { TERMINAL_SESSION_COUNT_EVENT } from '../../client/terminal-workspace-events.js';
import { TRAY_COUNT_EVENT, type TrayCountDetail } from '../../client/tray-count-events.js';
import { CHAT_PALETTE_OPEN_REQUEST_EVENT } from '../../client/chat-palette-events.js';
import {
  GRAPH_PALETTE_AVAILABILITY_EVENT,
  GRAPH_PALETTE_OPEN_REQUEST_EVENT,
  isGraphPaletteAvailable,
} from '../../client/graph-palette-events.js';
import { touchMenuItemStyles } from './touch-styles.js';
import './notification-tray.js';
import './inbox-tray.js';
import { isMacPlatform } from '../../utils/platform.js';

// ---------------------------------------------------------------------------
// Project-context helpers for the dashboard <-> chat mode switch.
//
// These are pure functions exported for testing -- they map URL paths to
// the project identifier that should carry across the view toggle.
// ---------------------------------------------------------------------------

/** Extract a project ID from a dashboard-style path (`/projects/:id/...`). */
export function projectIdFromDashboardPath(path: string): string | null {
  const m = path.match(/^\/projects\/([^/?#]+)/);
  // `/projects/new` is the creation form, not a project-scoped page.
  return m && m[1] !== 'new' ? m[1] : null;
}

/** Extract a project ID from a legacy chat space path (`/chat/space/:id/...`). */
export function projectIdFromChatSpacePath(path: string): string | null {
  const m = path.match(/^\/chat\/space\/([^/?#]+)/);
  return m ? m[1] : null;
}

/**
 * Extract a project slug from a readable chat path (`/chat/:slug` or
 * `/chat/:slug/:threadId`). Returns null for space, dm, and bare `/chat`
 * paths -- those are handled by dedicated helpers or have no project context.
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

  /** Whether a graph view on screen offers the "Jump to agent" palette. */
  @state()
  private graphPaletteAvailable = false;

  /** Unread message count, from the inbox tray's count events. */
  @state()
  private inboxCount = 0;

  /** Unacknowledged notification count, from the notification tray's count events. */
  @state()
  private notificationCount = 0;

  /** The user id that inboxCount and notificationCount belong to. */
  private countsUserId: string | null = null;

  /** Whether the device's primary pointer is touch — hides keyboard-shortcut affordances on the palette button. */
  private touchPrimary = new TouchPrimaryController(this);

  static override styles = css`
    ${touchMenuItemStyles}

    /* ------------------------------------------------------------------ */
    /* Grid: three-tier responsive                                         */
    /*   Wide  (>1100px):  3-col -- title | mode-switch | actions+user      */
    /*   Medium (<=1100):  2-col -- title | segments(icon-only)+user-dd     */
    /*   Narrow (<=768):  2-col -- title | mode-dd + user-dd               */
    /* ------------------------------------------------------------------ */
    :host {
      display: grid;
      /* The actions column gets a max-content floor so it can never be sized
         below what it holds. With minmax(0, 1fr) it could collapse toward
         zero while its flex contents kept full width, and because it is
         justify-self:end the overflow spilled leftward across the centre
         column — landing the unread badges on top of the "Terminal" label.
         The title column keeps the 0 floor and truncates instead, since it
         is the one element here that can lose characters harmlessly. */
      grid-template-columns: minmax(0, 1fr) auto minmax(max-content, 1fr);
      align-items: center;
      /* The page uses viewport-fit=cover, so the header runs under a notch
         or status bar. The top inset is padding on top of the content
         height (content-box, stated explicitly so the header never loses
         its 60px row to the inset), and the side insets (landscape) widen
         the inline padding. Every inset is 0 on devices without one. A shell
         that already clears the left inset beside the header (a sidebar)
         sets --scion-header-inset-left to 0px so it is not applied twice. */
      box-sizing: content-box;
      height: var(--scion-header-height, 60px);
      padding: env(safe-area-inset-top, 0px) max(1.5rem, env(safe-area-inset-right, 0px)) 0
        max(1.5rem, var(--scion-header-inset-left, env(safe-area-inset-left, 0px)));
      background: var(--scion-surface, #ffffff);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    /* ------------------------------------------------------------------ */
    /* Left column                                                         */
    /* ------------------------------------------------------------------ */
    .header-left {
      display: flex;
      align-items: center;
      gap: 1rem;
      /* A flex item defaults to min-width:auto and refuses to shrink below
         its content, which would push the squeeze onto the other columns. */
      min-width: 0;
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

    .page-title {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
      /* Absorb the narrowing here. A long page title truncates rather than
         holding the header open and forcing the columns to collide. */
      min-width: 0;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    /*
     * On the chat view the logo stands in for the page title, so it is sized
     * to sit inline within the 60px header rather than as a sidebar block.
     */
    .logo {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      /* Lets the title truncate instead of forcing .header-left (and, past
         it, the palette button and both dropdowns) wider than the
         viewport at narrow widths — same reasoning as .header-left's own
         min-width: 0 above. */
      min-width: 0;
    }

    .logo-icon {
      font-size: 1.5rem;
      line-height: 1;
    }

    .logo-text {
      min-width: 0;
    }

    .logo-text h1 {
      margin: 0;
      font-size: 1.125rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      /* Truncates with an ellipsis rather than wrapping onto a second line
         (which grows the header's height) once the palette button and both
         dropdowns leave it less room than its own text needs. */
      min-width: 0;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    /* ------------------------------------------------------------------ */
    /* Wide center column: segmented mode switch (visible >1100px)           */
    /* ------------------------------------------------------------------ */
    .wide-center {
      display: flex;
      justify-content: center;
    }

    /* ------------------------------------------------------------------ */
    /* Mode switch (segmented control) -- used in wide + medium tiers       */
    /* ------------------------------------------------------------------ */
    .mode-switch {
      display: flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.25rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .mode-switch button {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.5rem 0.75rem;
      border: none;
      border-radius: 0.375rem;
      background: transparent;
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      font-size: 0.875rem;
      font-weight: 500;
      transition:
        background 0.15s ease,
        color 0.15s ease;
    }

    .mode-switch button:hover {
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text, #1e293b);
    }

    .mode-switch button.active {
      background: var(--scion-primary, #3b82f6);
      color: white;
    }

    .mode-switch button.active:hover {
      background: var(--scion-primary-hover, #2563eb);
    }

    .mode-switch button sl-icon {
      font-size: 1.125rem;
    }

    .mode-label {
      font-size: 0.875rem;
      font-weight: 500;
    }

    /* ------------------------------------------------------------------ */
    /* Right column                                                        */
    /* ------------------------------------------------------------------ */
    .header-right {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      justify-self: end;
      grid-column: 3;
      position: relative;
    }

    /* ------------------------------------------------------------------ */
    /* Palette button -- first child of .header-right, visible at every tier */
    /* ------------------------------------------------------------------ */
    /*
     * A plain native <button>, not <sl-icon-button>: Shoelace's icon button
     * does not forward host-level ARIA attributes (aria-haspopup,
     * aria-keyshortcuts) to the inner <button part="base"> that actually
     * takes focus and carries the accessible role, so they never reach the
     * accessibility tree. A real <button> carries its own attributes
     * directly. Sized/styled like .mode-trigger below, minus its border.
     */
    .palette-button {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 2rem;
      height: 2rem;
      padding: 0;
      border: none;
      border-radius: 0.375rem;
      background: transparent;
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      transition:
        background 0.15s ease,
        color 0.15s ease;
    }

    /* Scoped to hover-capable devices, the same as .palette-option:hover in
       quick-palette.ts and for the same reason: on touch, :hover sticks
       after a tap until the next tap lands elsewhere — it would still be
       showing when the palette closes and focus returns to this button. */
    @media (hover: hover) {
      .palette-button:hover {
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text, #1e293b);
      }
    }

    .palette-button sl-icon {
      font-size: 1.125rem;
    }

    /* A tap target of at least 44x44 on touch, where the compact 2rem
       (32px) desktop sizing above does not meet the minimum. Desktop keeps
       the compact sizing to match the other header icon buttons. */
    @media (hover: none) and (pointer: coarse) {
      .palette-button {
        min-width: 44px;
        min-height: 44px;
      }
    }

    /* ------------------------------------------------------------------ */
    /* Wide-only: inline header actions + user section (visible >1100px)     */
    /* ------------------------------------------------------------------ */
    .wide-right {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .header-actions {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .user-section {
      display: flex;
      align-items: center;
      gap: 0.75rem;
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

    /* ------------------------------------------------------------------ */
    /* Compact-only: mode segments / dropdown + user dropdown (<=1100px)     */
    /* ------------------------------------------------------------------ */
    .compact-right {
      display: none;
      align-items: center;
      gap: 0.75rem;
    }

    /* In compact mode segments, hide the text labels (icon-only) */
    .compact-mode-segments .mode-label {
      display: none;
    }

    .compact-mode-dropdown {
      display: none;
    }

    /* ------------------------------------------------------------------ */
    /* Mode-selector dropdown trigger (compact narrow tier)                 */
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

    /* ------------------------------------------------------------------ */
    /* User/account dropdown trigger (compact tiers)                       */
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
    /* Icon badge wrapper + count badge for wide-layout icon buttons        */
    /* ------------------------------------------------------------------ */
    .icon-badge-wrapper {
      position: relative;
      display: inline-flex;
    }
    .icon-badge-wrapper .trigger-badge {
      min-width: 16px;
      width: auto;
      height: 16px;
      padding: 0 4px;
      border-radius: 8px;
      background: var(--sl-color-danger-600, #dc2626);
      color: white;
      font-size: 10px;
      font-weight: 600;
      line-height: 16px;
      text-align: center;
    }

    /* ------------------------------------------------------------------ */
    /* Menu-item count badge (e.g. "3" next to Messages)                   */
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
    /* Sign-in link (when user is null)                                     */
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

    /* ------------------------------------------------------------------ */
    /* Tray container: zero-size absolute wrapper so hidden tray elements   */
    /* don't contribute flex items or gap in .header-right.                 */
    /* ------------------------------------------------------------------ */
    .tray-container {
      position: absolute;
      width: 0;
      height: 0;
      overflow: visible;
      pointer-events: none;
    }

    .tray-container > * {
      pointer-events: auto;
    }

    /* ================================================================== */
    /* Tier 2: Medium (<=1100px) -- icon-only segments + user dropdown      */
    /* ================================================================== */
    @media (max-width: 1100px) {
      :host {
        grid-template-columns: 1fr auto;
      }

      .wide-center {
        display: none;
      }

      .wide-right {
        display: none;
      }

      .compact-right {
        display: flex;
      }

      .header-right {
        grid-column: auto;
      }

      .compact-mode-segments {
        display: flex;
      }

      .compact-mode-dropdown {
        display: none;
      }

      /* Hide the mode dropdown label at this tier too */
      .mode-dropdown-label {
        display: none;
      }
    }

    /* ================================================================== */
    /* Tier 3: Narrow/mobile (<=768px) -- full dropdown                    */
    /* ================================================================== */
    @media (max-width: 768px) {
      .mobile-menu-btn {
        display: flex;
      }

      .compact-mode-segments {
        display: none;
      }

      .compact-mode-dropdown {
        display: flex;
      }
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

      <!-- Wide center column: segmented mode switch with labels (>1100px) -->
      <div class="wide-center">${this.renderModeSwitch()}</div>

      <div class="header-right">
        ${this.renderPaletteButton()}
        <!-- Wide layout (>1100px): inline actions + user section -->
        <div class="wide-right">
          ${this.user
            ? html`
                <div class="header-actions">
                  <sl-tooltip content="Messages">
                    <span class="icon-badge-wrapper">
                      <sl-icon-button
                        name="envelope"
                        label="Messages"
                        @click=${(): void => this.openInboxTray()}
                      ></sl-icon-button>
                      ${this.inboxCount > 0
                        ? html`<span class="trigger-badge">${this.inboxCount}</span>`
                        : nothing}
                    </span>
                  </sl-tooltip>
                  <sl-tooltip content="Notifications">
                    <span class="icon-badge-wrapper">
                      <sl-icon-button
                        name="bell"
                        label="Notifications"
                        @click=${(): void => this.openNotificationTray()}
                      ></sl-icon-button>
                      ${this.notificationCount > 0
                        ? html`<span class="trigger-badge">${this.notificationCount}</span>`
                        : nothing}
                    </span>
                  </sl-tooltip>
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
                <div class="user-section">
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
                </div>
              `
            : html`
                <a href="/auth/login" class="sign-in-link">
                  <sl-icon name="box-arrow-in-right"></sl-icon>
                  Sign in
                </a>
              `}
        </div>

        <!-- Compact layout (<=1100px): mode segments/dropdown + user dropdown -->
        <div class="compact-right">
          <div class="compact-mode-segments">${this.renderModeSwitch()}</div>
          <div class="compact-mode-dropdown">${this.renderModeDropdown()}</div>
          ${this.user
            ? this.renderUserDropdown()
            : html`
                <a href="/auth/login" class="sign-in-link">
                  <sl-icon name="box-arrow-in-right"></sl-icon>
                  Sign in
                </a>
              `}
        </div>

        <!-- Tray components: triggers hidden, panels open programmatically -->
        <div class="tray-container">
          <scion-inbox-tray .user=${this.user}></scion-inbox-tray>
          <scion-notification-tray .user=${this.user}></scion-notification-tray>
        </div>
      </div>
    `;
  }

  // =========================================================================
  // Palette button -- opens a quick palette from the header: the chat quick
  // switcher on a chat route, or a graph view's "Jump to agent" palette
  // while one is on screen. One button, one render path, shared by every
  // host -- see renderPaletteButton's own doc comment. The terminal view
  // has none here: its "Jump to agent" button is a labelled footer in its
  // Open terminals column (TerminalWorkspaceRoot.buildRailFooter).
  // =========================================================================

  /**
   * The header's palette button. Renders as a single element shared by every
   * responsive tier (positioned via `.header-right`'s own flex layout, see
   * the render() call site) rather than duplicated per tier. Visible on
   * every width when signed in on a route with a palette to open: narrow
   * screens need it most since they have no keyboard shortcut, but desktop
   * keeps it too, both to discover the shortcut (via the tooltip) and for a
   * pointer/trackpad user who would rather click than reach for a chord.
   *
   * Which host owns the click is resolved once here, as its open-request
   * event, and threaded through to the click handler rather than re-resolved
   * there: the route could otherwise change between render and click
   * (unlikely for a header button, but this keeps the two in sync by
   * construction rather than by coincidence).
   */
  private renderPaletteButton(): TemplateResult | typeof nothing {
    if (!this.user) return nothing;
    const isChat = this.isChatView();
    const isGraph = !isChat && !this.isTerminalView() && this.graphPaletteAvailable;
    if (!isChat && !isGraph) return nothing;
    const openRequestEvent = isChat
      ? CHAT_PALETTE_OPEN_REQUEST_EVENT
      : GRAPH_PALETTE_OPEN_REQUEST_EVENT;

    const isTouch = this.touchPrimary.isTouch;
    const isMac = isMacPlatform();
    const shortcutLabel = isMac ? '⌘K' : 'Ctrl+K';
    const ariaKeyshortcuts = isMac ? 'Meta+K' : 'Control+K';
    const label = isChat ? 'Quick switcher' : 'Jump to agent';
    const ariaLabel = isChat ? 'Open quick switcher' : 'Open Jump to agent';

    return html`
      <sl-tooltip content=${`${label} (${shortcutLabel})`} ?disabled=${isTouch}>
        <button
          type="button"
          class="palette-button"
          aria-label=${ariaLabel}
          aria-haspopup="dialog"
          aria-keyshortcuts=${isTouch ? nothing : ariaKeyshortcuts}
          @click=${(e: Event): void => this.handlePaletteButtonClick(e, openRequestEvent)}
        >
          <sl-icon name="compass" aria-hidden="true"></sl-icon>
        </button>
      </sl-tooltip>
    `;
  }

  /**
   * iOS and macOS Safari do not focus a `<button>` on click, so without this
   * the deep active element the owning host captures as "what to restore
   * focus to" would be whatever was focused before the click — which could
   * be the chat composer, popping the on-screen keyboard back open the
   * instant the palette closes. Focusing the button explicitly first, before
   * dispatching, makes capture reliably see this button instead.
   */
  private handlePaletteButtonClick(e: Event, openRequestEvent: string): void {
    const btn = e.currentTarget as HTMLElement;
    btn.focus({ preventScroll: true });
    this.dispatchEvent(new CustomEvent(openRequestEvent, { bubbles: true, composed: true }));
  }

  // =========================================================================
  // Mode switch (segmented control) -- wide + medium tiers
  // =========================================================================

  /**
   * Inline segmented control for switching between Dashboard / Chat /
   * Terminal modes. Used in the wide center column (with labels) and in
   * the compact tier (icon-only via CSS). Returns nothing when no
   * alternative modes are feature-flagged on.
   */
  private renderModeSwitch(): TemplateResult | typeof nothing {
    const chatEnabled = isFeatureEnabled(NATIVE_CHAT_FLAG);
    const terminalsEnabled = isFeatureEnabled(TERMINAL_WORKSPACE_FLAG);
    if (!chatEnabled && !terminalsEnabled) return nothing;

    const isChat = this.isChatView();
    const isTerminal = this.isTerminalView();

    return html`
      <div class="mode-switch" role="group" aria-label="Switch view">
        <sl-tooltip content="Dashboard">
          <button
            class=${!isChat && !isTerminal ? 'active' : ''}
            @click=${(): void => {
              void this.handleModeSwitch('dashboard');
            }}
            aria-label="Dashboard"
          >
            <sl-icon name="house"></sl-icon>
            <span class="mode-label">Dashboard</span>
          </button>
        </sl-tooltip>
        ${chatEnabled
          ? html`
              <sl-tooltip content="Chat">
                <button
                  class=${isChat ? 'active' : ''}
                  @click=${(): void => {
                    void this.handleModeSwitch('chat');
                  }}
                  aria-label="Chat"
                >
                  <sl-icon name="chat-dots"></sl-icon>
                  <span class="mode-label">Chat</span>
                </button>
              </sl-tooltip>
            `
          : ''}
        ${terminalsEnabled
          ? html`
              <sl-tooltip content=${`Terminals (${this.terminalSessionCount})`}>
                <button
                  class=${isTerminal ? 'active' : ''}
                  @click=${(): void => {
                    void this.handleModeSwitch('terminals');
                  }}
                  aria-label=${`Terminals (${this.terminalSessionCount})`}
                >
                  <sl-icon name="terminal"></sl-icon>
                  <span class="mode-label">Terminal</span>
                </button>
              </sl-tooltip>
            `
          : ''}
      </div>
    `;
  }

  // =========================================================================
  // Mode-selector dropdown -- narrow tier
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
                  ${this.terminalSessionCount > 0
                    ? html`<span slot="suffix" class="count-badge"
                        >${this.terminalSessionCount}</span
                      >`
                    : ''}
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
  // User/account dropdown -- compact tiers
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
              ? html`<span slot="suffix" class="count-badge">${this.notificationCount}</span>`
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
            <sl-icon slot="prefix" name=${this.isDark ? 'sun' : 'moon'}></sl-icon>
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
      | null
      | undefined;
    dropdown?.hide();
  }

  // =========================================================================
  // Tray integration
  // =========================================================================

  // ----------------------------------------------------------------------
  // COUPLING: inbox-tray.ts (.inbox-btn)
  //           notification-tray.ts (.bell-btn)
  // If either tray renames these selectors, update the
  // references in openInboxTray(), openNotificationTray() and
  // hideTrayTriggers() below. Badge counts arrive through TRAY_COUNT_EVENT.
  // TODO: Add public toggle() methods to the tray components so the header
  // does not need to pierce shadow DOMs.
  // ----------------------------------------------------------------------

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
   * Hide the tray trigger buttons from layout, focus and the accessibility
   * tree, while leaving them functional for programmatic clicks (a
   * synthetic `.click()` still fires on a `display: none` element). The
   * panels (siblings of the buttons in the tray's shadow DOM) are
   * unaffected — the header's own icon buttons are the only visible,
   * properly-sized trigger for these actions.
   */
  private hideTrayTriggers(): void {
    const hide = (el: HTMLElement | null): void => {
      if (!el) return;
      el.style.display = 'none';
    };

    const inboxTray = this.shadowRoot?.querySelector('scion-inbox-tray');
    const notifTray = this.shadowRoot?.querySelector('scion-notification-tray');

    hide(inboxTray?.shadowRoot?.querySelector('.inbox-btn') as HTMLElement | null);
    hide(notifTray?.shadowRoot?.querySelector('.bell-btn') as HTMLElement | null);
  }

  /**
   * Sets a badge count from a tray's count event. Each tray dispatches one
   * whenever its list changes, so the badges follow the trays' lists, however
   * long a fetch takes.
   */
  private readonly handleTrayCount = (event: Event): void => {
    const detail = (event as CustomEvent<TrayCountDetail>).detail;
    if (!detail) return;
    const count = Math.max(0, detail.count);
    if (detail.source === 'inbox') this.inboxCount = count;
    else if (detail.source === 'notifications') this.notificationCount = count;
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
    // Strip the query string first: `currentPath` is the router's raw path
    // argument, which — for the multi-pane URL form (`/terminals?lv=1&...`)
    // — still carries it, and `=== '/terminals'` would otherwise never
    // match a bare multi-pane URL at all.
    const path = (this.currentPath || window.location.pathname).split('?')[0];
    return path === '/terminals' || path.startsWith('/terminals/');
  }

  // =========================================================================
  // Mode switch navigation
  // =========================================================================

  /**
   * Navigate to the given mode, preserving project context when possible.
   *
   * Dashboard -> Chat:  /projects/:id/... -> /chat/space/:id
   * Chat -> Dashboard:  /chat/space/:id/... -> /projects/:id
   *                     /chat/:slug/...     -> (resolve slug) -> /projects/:id
   *                     /chat/dm/...        -> / (no project context)
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
    this.graphPaletteAvailable = isGraphPaletteAvailable();
    window.addEventListener(GRAPH_PALETTE_AVAILABILITY_EVENT, this.handleGraphPaletteAvailability);
    this.rememberModePath();

    // The trays sit in this shadow root; their composed count events reach
    // the host.
    this.addEventListener(TRAY_COUNT_EVENT, this.handleTrayCount);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener(
      TERMINAL_SESSION_COUNT_EVENT,
      this.handleTerminalSessionCount as EventListener
    );
    window.removeEventListener(
      GRAPH_PALETTE_AVAILABILITY_EVENT,
      this.handleGraphPaletteAvailability
    );
    this.removeEventListener(TRAY_COUNT_EVENT, this.handleTrayCount);
  }

  override firstUpdated(): void {
    // Give the tray components a frame to finish their first render so
    // their shadow DOMs are ready, then hide their trigger buttons.
    requestAnimationFrame(() => {
      this.hideTrayTriggers();
    });
  }

  override willUpdate(changedProperties: Map<string, unknown>): void {
    if (changedProperties.has('user')) this.resetCountsOnUserChange();
  }

  override updated(changedProperties: Map<string, unknown>): void {
    if (changedProperties.has('currentPath')) this.rememberModePath();
  }

  /**
   * Clears the badge counts when the signed-in user id changes, so the badges
   * never show the previous user's counts. The trays clear their lists in
   * their own next update, a render later than this one; clearing here keeps
   * that render from pairing the new user with the old counts. The trays'
   * count events then fill the badges in. A new user object with the same id
   * keeps the counts.
   */
  private resetCountsOnUserChange(): void {
    const id = this.user?.id ?? null;
    if (id === this.countsUserId) return;
    this.countsUserId = id;
    this.inboxCount = 0;
    this.notificationCount = 0;
  }

  // =========================================================================
  // Event handlers
  // =========================================================================

  private readonly handleTerminalSessionCount = (event: CustomEvent<{ count?: number }>): void => {
    this.terminalSessionCount = Math.max(0, event.detail?.count ?? 0);
  };

  private readonly handleGraphPaletteAvailability = (): void => {
    this.graphPaletteAvailable = isGraphPaletteAvailable();
  };

  private rememberModePath(): void {
    const path = this.currentPath || window.location.pathname;
    if (path.startsWith('/chat')) {
      // Drop a `#msg-…` jump target: coming back to chat should land where
      // the user left off, not replay the jump that first opened the thread.
      rememberedModePaths.chat = path.split('#')[0];
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
   * Handle profile link click with client-side navigation (wide layout).
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
