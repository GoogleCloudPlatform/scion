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

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { User } from '../../shared/types.js';
import { loadAdminStatus } from '../../client/admin-status.js';
import { ARTIFACTS_FLAG } from '../../client/artifacts-flag.js';
import {
  INBOX_SUMMARY_CHANGED_EVENT,
  ensureInboxSummaryStarted,
  formatInboxSummaryBadge,
} from '../../client/inbox-summary-loader.js';
import {
  ACCESS_CONSTRAINTS_FLAG,
  FEATURE_FLAGS_CHANGED_EVENT,
  INBOX_DASHBOARD_FLAG,
  isFeatureEnabled,
  isInboxDashboardEnabled,
  SYSTEM_ONE_FLAG,
} from '../../utils/feature-flags.js';
import {
  type AdminStatus,
  hasAnyPermission,
  NAV_PERMISSION_MAP,
} from '../../lib/admin-permissions.js';
import {
  brandMarkStyles,
  observeBranding,
  renderAttribution,
  renderSidebarBrand,
} from './brand-mark.js';

interface NavItem {
  path: string;
  label: string;
  icon: string;
  /** Feature flag the item needs; the item is hidden while it is off. */
  flag?: string;
  /** Optional badge text rendered on the right of the link; hidden when null. */
  badge?: () => string | null;
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
    items: [
      { path: '/', label: 'Dashboard', icon: 'house' },
      {
        path: '/profile/inbox',
        label: 'Inbox',
        icon: 'inbox',
        flag: INBOX_DASHBOARD_FLAG,
        badge: () => formatInboxSummaryBadge(),
      },
    ],
  },
  {
    title: 'Management',
    items: [
      { path: '/projects', label: 'Projects', icon: 'folder' },
      { path: '/agents', label: 'Agents', icon: 'cpu' },
      { path: '/brokers', label: 'Brokers', icon: 'hdd-rack' },
      { path: '/skills', label: 'Skills', icon: 'book' },
      {
        path: '/artifacts',
        label: 'Artifacts',
        icon: 'file-earmark-richtext',
        flag: ARTIFACTS_FLAG,
      },
    ],
  },
];

/** The items whose feature flag, if any, is on. */
function visibleItems(items: NavItem[]): NavItem[] {
  return items.filter((item) => !item.flag || isFeatureEnabled(item.flag));
}

/**
 * Admin nav items visible to both hub-admin and super-admin users.
 * These cover scopeable resources that hub-admins can manage.
 */
const ADMIN_SCOPEABLE_ITEMS: NavItem[] = [
  { path: '/settings', label: 'Hub Resources', icon: 'gear' },
  { path: '/admin/server-config', label: 'Server Config', icon: 'sliders' },
  { path: '/admin/branding', label: 'Branding', icon: 'palette' },
  { path: '/admin/federation', label: 'Federation', icon: 'globe' },
  { path: '/admin/integrations', label: 'Integrations', icon: 'plug' },
  {
    path: '/admin/system-one',
    label: 'System One Providers',
    icon: 'robot',
    flag: SYSTEM_ONE_FLAG,
  },
  { path: '/admin/scheduler', label: 'Scheduler', icon: 'clock' },
  { path: '/admin/users', label: 'Users', icon: 'people' },
  { path: '/admin/groups', label: 'Groups', icon: 'diagram-3' },
  { path: '/admin/roles', label: 'Roles', icon: 'shield-lock' },
  { path: '/admin/role-bindings', label: 'Role Bindings', icon: 'link-45deg' },
  {
    path: '/admin/access-boundaries',
    label: 'Access Constraints',
    icon: 'shield-check',
    flag: ACCESS_CONSTRAINTS_FLAG,
  },
  { path: '/admin/quotas', label: 'Quotas', icon: 'speedometer2' },
  { path: '/health', label: 'Health', icon: 'heart-pulse' },
  { path: '/metrics', label: 'Metrics', icon: 'graph-up' },
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
   * null when there is no user or the status has not been loaded.
   * The nav uses this to show only the admin items the user has permissions for.
   */
  @state()
  private adminStatus: AdminStatus | null = null;

  /** Tracks the user ID for which admin capabilities were last checked. */
  private adminCheckUserId: string | null = null;

  private readonly onFeatureFlagsChanged = (): void => {
    if (isInboxDashboardEnabled()) {
      ensureInboxSummaryStarted(this.user?.id || undefined);
    }
    this.requestUpdate();
  };

  private readonly onInboxSummaryChanged = (): void => {
    this.requestUpdate();
  };

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFeatureFlagsChanged);
    window.addEventListener(INBOX_SUMMARY_CHANGED_EVENT, this.onInboxSummaryChanged);
    if (isInboxDashboardEnabled()) {
      ensureInboxSummaryStarted(this.user?.id || undefined);
    }
  }

  override disconnectedCallback(): void {
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFeatureFlagsChanged);
    window.removeEventListener(INBOX_SUMMARY_CHANGED_EVENT, this.onInboxSummaryChanged);
    super.disconnectedCallback();
  }

  override updated(changedProperties: Map<PropertyKey, unknown>): void {
    if (changedProperties.has('user')) {
      void this.checkAdminCapabilities();
      if (isInboxDashboardEnabled() && this.user?.id) {
        ensureInboxSummaryStarted(this.user.id);
      }
    }
  }

  /**
   * Load the current user's admin status from the dedicated admin-status
   * endpoint, which returns hub-admin and super-admin flags plus the
   * user's system-scoped permissions.
   *
   * Every signed-in user reads from admin-status (never `user.role === 'admin'`)
   * so there is one source of truth for admin UI visibility, including while
   * "View as member" mode is active.
   */
  private async checkAdminCapabilities(): Promise<void> {
    const userId = this.user?.id ?? null;

    // Skip if we already checked for this user
    if (userId === this.adminCheckUserId) return;
    this.adminCheckUserId = userId;

    // No user = no admin access
    if (!this.user) {
      this.adminStatus = null;
      return;
    }

    // The admin-status response decides which admin items show for super-admins,
    // hub-admins, and custom-role users. The request is shared with the other
    // nav instance and the startup code (client/admin-status.ts).
    const status = await loadAdminStatus(userId);
    // Only apply the result if the user hasn't changed during the fetch.
    if (this.adminCheckUserId === userId) {
      this.adminStatus = status;
    }
  }

  static override styles = css`
    ${brandMarkStyles}

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
      overscroll-behavior: contain;
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
      position: relative;
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.625rem 0.75rem;
      border-radius: var(--scion-radius, 0.5rem);
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
      color: var(--scion-primary-text, #ffffff);
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

    .nav-badge {
      margin-left: auto;
      min-width: 1.25rem;
      padding: 0 0.375rem;
      border-radius: var(--scion-radius-full, 9999px);
      font-size: 0.6875rem;
      font-weight: 600;
      line-height: 1.25rem;
      text-align: center;
      flex-shrink: 0;
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .nav-link.active .nav-badge {
      background: color-mix(in srgb, var(--scion-primary-text, #ffffff) 22%, transparent);
      color: var(--scion-primary-text, #ffffff);
    }

    :host([collapsed]) .nav-badge {
      position: absolute;
      top: 0.125rem;
      right: 0.25rem;
      margin-left: 0;
      min-width: 1rem;
      padding: 0 0.25rem;
      font-size: 0.625rem;
      line-height: 1rem;
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
      border-radius: var(--scion-radius, 0.5rem);
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

    .sidebar-attribution {
      padding: 0.25rem 0.75rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      text-align: center;
      white-space: nowrap;
      overflow: hidden;
    }

    :host([collapsed]) .sidebar-attribution {
      display: none;
    }

    /* Smooth width transition */
    :host {
      transition: width var(--scion-transition-normal, 250ms ease);
    }
  `;

  constructor() {
    super();
    observeBranding(this);
  }

  override render() {
    const isSuperAdmin = this.adminStatus?.isSuperAdmin === true;

    const visibleAdminItems = this.adminStatus
      ? visibleItems(ADMIN_SCOPEABLE_ITEMS).filter(
          (item) =>
            NAV_PERMISSION_MAP[item.path] &&
            hasAnyPermission(this.adminStatus, NAV_PERMISSION_MAP[item.path])
        )
      : [];

    return html`
      <div class="logo">
        ${renderSidebarBrand({
          collapsed: this.collapsed,
          subtitleFallback: 'Agent Orchestration',
        })}
      </div>

      <nav class="nav-container" aria-label="Main">
        ${NAV_SECTIONS.map(
          (section) => html`
            <div class="nav-section">
              <div class="nav-section-title">${section.title}</div>
              <ul class="nav-list">
                ${visibleItems(section.items).map((item) => this.renderNavItem(item))}
              </ul>
            </div>
          `
        )}
        ${this.adminStatus && (isSuperAdmin || visibleAdminItems.length > 0)
          ? html`
              <div class="nav-section admin-section">
                <div class="nav-section-title">Admin</div>
                <ul class="nav-list">
                  ${visibleAdminItems.map((item) => this.renderNavItem(item))}
                  ${isSuperAdmin
                    ? ADMIN_SUPERADMIN_ITEMS.map((item) => this.renderNavItem(item))
                    : ''}
                </ul>
              </div>
            `
          : ''}
      </nav>

      ${renderAttribution('sidebar-attribution')}
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

  private renderNavItem(item: NavItem): TemplateResult {
    const badge = item.badge?.() ?? null;
    const tooltipContent = badge ? `${item.label} (${badge})` : item.label;
    return html`
      <li class="nav-item">
        <sl-tooltip
          style="display: block;"
          content="${tooltipContent}"
          placement="right"
          ?disabled=${!this.collapsed}
          hoist
        >
          <a
            href="${item.path}"
            class="nav-link ${this.isActive(item.path) ? 'active' : ''}"
            aria-label=${badge ? `${item.label} (${badge} waiting)` : nothing}
            @click=${(e: Event) => this.handleNavClick(e, item.path)}
          >
            <sl-icon name="${item.icon}"></sl-icon>
            <span class="nav-link-text">${item.label}</span>
            ${badge
              ? html`<span class="nav-badge" data-nav-badge aria-hidden="true">${badge}</span>`
              : nothing}
          </a>
        </sl-tooltip>
      </li>
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
