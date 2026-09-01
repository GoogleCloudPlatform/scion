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
import { customElement, property, state } from 'lit/decorators.js';

import type { User } from '../../shared/types.js';
import { apiFetch } from '../../client/api.js';
import {
  type AdminStatus,
  hasAnyPermission,
  NAV_PERMISSION_MAP,
} from '../../lib/admin-permissions.js';

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
      { path: '/skills', label: 'Skills', icon: 'lightning-charge' },
      { path: '/metrics', label: 'Metrics', icon: 'graph-up' },
    ],
  },
];

/**
 * Admin nav items visible to both hub-admin and super-admin users.
 * These cover scopeable resources that hub-admins can manage.
 */
const ADMIN_SCOPEABLE_ITEMS: NavItem[] = [
  { path: '/settings', label: 'Hub Resources', icon: 'gear' },
  { path: '/admin/server-config', label: 'Server Config', icon: 'sliders' },
  { path: '/admin/federation', label: 'Federation', icon: 'globe' },
  { path: '/admin/integrations', label: 'Integrations', icon: 'plug' },
  { path: '/admin/scheduler', label: 'Scheduler', icon: 'clock' },
  { path: '/admin/users', label: 'Users', icon: 'people' },
  { path: '/admin/groups', label: 'Groups', icon: 'diagram-3' },
  { path: '/admin/roles', label: 'Roles', icon: 'shield-lock' },
  { path: '/admin/role-bindings', label: 'Role Bindings', icon: 'link-45deg' },
  { path: '/admin/access-boundaries', label: 'Access Boundaries', icon: 'shield-check' },
  { path: '/admin/quotas', label: 'Quotas', icon: 'speedometer2' },
  { path: '/health', label: 'Health', icon: 'heart-pulse' },
  { path: '/admin/skill-registries', label: 'Skill Registries', icon: 'cloud-arrow-down' },
];

/**
 * Admin nav items visible ONLY to super-admin users.
 * These cover system-level operations that require full admin privileges.
 */
const ADMIN_SUPERADMIN_ITEMS: NavItem[] = [
  { path: '/admin/diagnostics', label: 'Diagnostics', icon: 'journal-text' },
  { path: '/admin/maintenance', label: 'Maintenance', icon: 'wrench-adjustable' },
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

  /**
   * Hide the collapse toggle (e.g. inside mobile drawer)
   */
  @property({ type: Boolean })
  hideCollapse = false;

  /**
   * The current user's admin status including per-resource permissions.
   * null when the user is not an admin or status has not been checked yet.
   * The nav uses this to show only the admin items the user has permissions for.
   */
  @state()
  private adminStatus: AdminStatus | null = null;

  /** Tracks the user ID for which admin capabilities were last checked. */
  private adminCheckUserId: string | null = null;

  override updated(changedProperties: Map<PropertyKey, unknown>): void {
    if (changedProperties.has('user')) {
      void this.checkAdminCapabilities();
    }
  }

  /**
   * Detect whether the current user has admin capabilities by calling
   * the dedicated admin-status endpoint, which returns explicit boolean
   * flags for hub-admin and super-admin status plus a permissions array.
   *
   * Super-admin users are detected directly via `user.role === 'admin'`
   * and skip the API call. For hub-admin and custom-role users, the
   * endpoint determines which nav items are visible.
   */
  private async checkAdminCapabilities(): Promise<void> {
    const userId = this.user?.id ?? null;

    // Skip if we already checked for this user
    if (userId === this.adminCheckUserId) return;
    this.adminCheckUserId = userId;

    // Super-admins always have admin capabilities — create an AdminStatus
    // with isSuperAdmin: true so hasAnyPermission() short-circuits.
    if (this.user?.role === 'admin') {
      this.adminStatus = { isAdmin: true, isSuperAdmin: true, permissions: [] };
      return;
    }

    // No user = no admin access
    if (!this.user) {
      this.adminStatus = null;
      return;
    }

    // Call the dedicated admin-status endpoint to detect admin status
    // and retrieve per-resource permissions.
    try {
      const res = await apiFetch('/api/v1/auth/admin-status');
      // Only apply result if user hasn't changed during the fetch
      if (this.adminCheckUserId === userId) {
        if (res.ok) {
          const data = await res.json();
          if (data.isAdmin === true) {
            this.adminStatus = {
              isAdmin: true,
              isSuperAdmin: data.isSuperAdmin === true,
              permissions: Array.isArray(data.permissions) ? data.permissions : [],
            };
          } else {
            this.adminStatus = null;
          }
        } else {
          this.adminStatus = null;
        }
      }
    } catch {
      if (this.adminCheckUserId === userId) {
        this.adminStatus = null;
      }
    }
  }

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
      opacity: 1;
      transition: opacity var(--scion-transition-normal, 250ms ease);
    }

    :host([collapsed]) .logo {
      justify-content: center;
      padding: 1.25rem 0.5rem;
    }

    :host([collapsed]) .logo-text {
      opacity: 0;
      width: 0;
      pointer-events: none;
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
      opacity: 1;
      overflow: hidden;
      white-space: nowrap;
      transition: opacity var(--scion-transition-normal, 250ms ease);
    }

    :host([collapsed]) .nav-container {
      padding: 0.5rem 0.25rem;
    }

    :host([collapsed]) .nav-section-title {
      opacity: 0;
      height: 0;
      margin: 0;
      padding: 0;
      pointer-events: none;
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
      padding: 0.5rem;
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
      opacity: 1;
      transition: opacity var(--scion-transition-normal, 250ms ease);
    }

    :host([collapsed]) .nav-link-text {
      opacity: 0;
      width: 0;
      pointer-events: none;
    }

    /* Collapse toggle */
    .collapse-toggle {
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 0.75rem;
      margin: 0.5rem;
      padding: 0.5rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.5rem;
      background: transparent;
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      font-size: 0.875rem;
      font-weight: 500;
      transition: all 0.15s ease;
    }

    .collapse-toggle:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text, #1e293b);
    }

    .collapse-toggle sl-icon {
      font-size: 1.125rem;
      flex-shrink: 0;
      transition: transform var(--scion-transition-normal, 250ms ease);
    }

    :host([collapsed]) .collapse-toggle sl-icon {
      transform: rotate(180deg);
    }

    .collapse-toggle-text {
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      opacity: 1;
      transition: opacity var(--scion-transition-normal, 250ms ease);
    }

    :host([collapsed]) .collapse-toggle-text {
      opacity: 0;
      width: 0;
      pointer-events: none;
    }

    /* Smooth width transition */
    :host {
      transition: width var(--scion-transition-normal, 250ms ease);
    }
  `;

  override render() {
    const isSuperAdmin = this.user?.role === 'admin';

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
        ${this.adminStatus?.isAdmin &&
        (isSuperAdmin ||
          ADMIN_SCOPEABLE_ITEMS.some(
            (item) =>
              NAV_PERMISSION_MAP[item.path] &&
              hasAnyPermission(this.adminStatus, NAV_PERMISSION_MAP[item.path])
          ))
          ? html`
              <div class="nav-section admin-section">
                <div class="nav-section-title">Admin</div>
                <ul class="nav-list">
                  ${ADMIN_SCOPEABLE_ITEMS.filter(
                    (item) =>
                      NAV_PERMISSION_MAP[item.path] &&
                      hasAnyPermission(this.adminStatus, NAV_PERMISSION_MAP[item.path])
                  ).map(
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
                  ${isSuperAdmin
                    ? ADMIN_SUPERADMIN_ITEMS.map(
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
                      )
                    : ''}
                </ul>
              </div>
            `
          : ''}
      </nav>

      ${this.hideCollapse
        ? ''
        : html`
            <button
              class="collapse-toggle"
              @click=${(): void => this.handleCollapseToggle()}
              aria-label=${this.collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
              title=${this.collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
            >
              <sl-icon name="chevron-left"></sl-icon>
              <span class="collapse-toggle-text">Collapse</span>
            </button>
          `}
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

  private handleCollapseToggle(): void {
    this.dispatchEvent(
      new CustomEvent('sidebar-toggle', {
        bubbles: true,
        composed: true,
      })
    );
  }

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
