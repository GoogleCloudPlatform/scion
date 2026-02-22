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
import { customElement, property } from 'lit/decorators.js';

import type { User } from '../../shared/types.js';

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
        <h1 class="page-title">${this.pageTitle}</h1>
      </div>

      <div class="header-right">
        <div class="header-actions">
          <sl-tooltip content="Notifications">
            <sl-icon-button name="bell" label="Notifications"></sl-icon-button>
          </sl-tooltip>
          <sl-tooltip content="Help">
            <sl-icon-button name="question-circle" label="Help"></sl-icon-button>
          </sl-tooltip>
        </div>

        <div class="user-section">${this.renderUserSection()}</div>
      </div>
    `;
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
        <a href="/profile" class="profile-link">
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
