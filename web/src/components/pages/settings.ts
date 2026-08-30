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
 * Hub Resources page component
 *
 * Displays hub-scoped resources (environment variables, secrets) and the
 * global file-based resources (templates, harness configs). Structured to
 * mirror the project settings Resources section for consistency.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData } from '../../shared/types.js';
import {
  type AdminStatus,
  hasAnyPermission,
  TAB_PERMISSION_MAP,
} from '../../lib/admin-permissions.js';
import '../shared/env-var-list.js';
import '../shared/gcp-service-account-list.js';
import '../shared/secret-list.js';
import '../shared/resource-list.js';
import '../shared/resource-import.js';
import '../shared/injected-skills-panel.js';
import '../shared/pre-start-hook-list.js';
import '../shared/project-template-list.js';

/** All known tab panel names in display order. */
const ALL_TABS = [
  'env-vars',
  'secrets',
  'templates',
  'harness-configs',
  'pre-start-hooks',
  'service-accounts',
  'skills',
  'project-templates',
] as const;

@customElement('scion-page-settings')
export class ScionPageSettings extends LitElement {
  /** Page data from the router (used for the current user's role). */
  @property({ type: Object })
  pageData: PageData | null = null;

  @state()
  private activeTab = 'env-vars';

  /** Admin status with permissions array, fetched from /api/v1/auth/admin-status. */
  @state()
  private adminStatus: AdminStatus | null = null;

  /** Whether the admin-status fetch has completed (prevents flash of empty state). */
  @state()
  private statusLoaded = false;

  /** Whether the admin-status fetch failed (network error or non-OK response). */
  @state()
  private fetchFailed = false;

  /**
   * Compute the set of tabs visible to the current user.
   * Super-admins see all tabs; other users see only tabs matching their permissions.
   */
  private get visibleTabs(): string[] {
    if (!this.adminStatus) return [];
    return ALL_TABS.filter((tab) =>
      hasAnyPermission(this.adminStatus, TAB_PERMISSION_MAP[tab] ?? [])
    );
  }

  /** Whether a given tab is visible to the current user. */
  private isTabVisible(tab: string): boolean {
    return this.visibleTabs.includes(tab);
  }

  /**
   * Whether the user can mutate pre-start hooks.
   * Replaces the old `user.role === 'admin'` check with permission-based logic
   * so that hub-admin role holders (not just super-admins) get write access.
   */
  private get canEditPreStartHooks(): boolean {
    return hasAnyPermission(this.adminStatus, ['hub.lifecycle_hooks.update']);
  }

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 2rem;
    }

    .header sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .section h2 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .section > p {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .tab-intro {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    sl-tab-group {
      --indicator-color: var(--scion-primary, #3b82f6);
    }

    sl-tab-group::part(base) {
      background: transparent;
    }

    sl-tab-panel::part(base) {
      padding: 1.5rem 0 0 0;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    // Deep-link a specific tab via ?tab= (e.g. ?tab=templates), used by the
    // resource detail pages' "back" links.
    if (typeof window !== 'undefined') {
      const tab = new URLSearchParams(window.location.search).get('tab');
      if (tab) {
        this.activeTab = tab;
      }
    }
    void this.fetchAdminStatus();
  }

  /**
   * Fetch the current user's admin status (including permissions array).
   * Once loaded, validates the active tab against visible tabs and falls
   * back to the first visible tab when the requested tab is not accessible.
   */
  private async fetchAdminStatus(): Promise<void> {
    try {
      const res = await fetch('/api/v1/auth/admin-status', { credentials: 'include' });
      if (res.ok) {
        const data = await res.json();
        this.adminStatus = {
          isAdmin: data.isAdmin === true,
          isSuperAdmin: data.isSuperAdmin === true,
          permissions: Array.isArray(data.permissions) ? data.permissions : [],
        };
      } else {
        this.fetchFailed = true;
      }
    } catch {
      this.fetchFailed = true;
    }
    this.statusLoaded = true;

    // If the requested tab (from URL or default) is not visible to this user,
    // silently fall back to the first visible tab.
    const visible = this.visibleTabs;
    if (visible.length > 0 && !visible.includes(this.activeTab)) {
      this.activeTab = visible[0];
    }
  }

  /** Refresh a resource list (by element id) after an import. */
  private refreshList(id: string): void {
    const list = this.shadowRoot?.querySelector(`#${id}`) as
      | import('../shared/resource-list.js').ScionResourceList
      | null;
    void list?.load();
  }

  override render() {
    // While the admin-status fetch is in flight, show nothing to prevent a
    // flash of all-tabs or empty-state before permissions are known.
    if (!this.statusLoaded) {
      return html`
        <div class="header">
          <sl-icon name="gear"></sl-icon>
          <h1>Hub Resources</h1>
        </div>
        <div class="section">
          <sl-spinner></sl-spinner>
        </div>
      `;
    }

    // If the fetch failed, show a distinct error so users can tell the
    // difference between "no permissions" and "we couldn't check."
    if (this.fetchFailed) {
      return html`
        <div class="header">
          <sl-icon name="gear"></sl-icon>
          <h1>Hub Resources</h1>
        </div>
        <div class="section">
          <p>Failed to load your permissions. Please refresh the page.</p>
        </div>
      `;
    }

    const visible = this.visibleTabs;

    // Edge case: no visible tabs (should not happen because the route guard
    // already checks settings permissions, but handle gracefully).
    if (visible.length === 0) {
      return html`
        <div class="header">
          <sl-icon name="gear"></sl-icon>
          <h1>Hub Resources</h1>
        </div>
        <div class="section">
          <p>You do not have permission to view any hub resources.</p>
        </div>
      `;
    }

    return html`
      <div class="header">
        <sl-icon name="gear"></sl-icon>
        <h1>Hub Resources</h1>
      </div>

      <div class="section">
        <h2>Resources</h2>
        <p>Hub-scoped resources available to all projects and agents.</p>

        <sl-tab-group
          @sl-tab-show=${(e: CustomEvent) => {
            this.activeTab = (e.detail as { name: string }).name;
          }}
        >
          ${this.isTabVisible('env-vars')
            ? html`<sl-tab slot="nav" panel="env-vars" ?active=${this.activeTab === 'env-vars'}
                >Environment Variables</sl-tab
              >`
            : nothing}
          ${this.isTabVisible('secrets')
            ? html`<sl-tab slot="nav" panel="secrets" ?active=${this.activeTab === 'secrets'}
                >Secrets</sl-tab
              >`
            : nothing}
          ${this.isTabVisible('templates')
            ? html`<sl-tab slot="nav" panel="templates" ?active=${this.activeTab === 'templates'}
                >Templates</sl-tab
              >`
            : nothing}
          ${this.isTabVisible('harness-configs')
            ? html`<sl-tab
                slot="nav"
                panel="harness-configs"
                ?active=${this.activeTab === 'harness-configs'}
                >Harness Configs</sl-tab
              >`
            : nothing}
          ${this.isTabVisible('pre-start-hooks')
            ? html`<sl-tab
                slot="nav"
                panel="pre-start-hooks"
                ?active=${this.activeTab === 'pre-start-hooks'}
                >Pre-Start Hooks</sl-tab
              >`
            : nothing}
          ${this.isTabVisible('service-accounts')
            ? html`<sl-tab
                slot="nav"
                panel="service-accounts"
                ?active=${this.activeTab === 'service-accounts'}
                >Service Accounts</sl-tab
              >`
            : nothing}
          ${this.isTabVisible('skills')
            ? html`<sl-tab slot="nav" panel="skills" ?active=${this.activeTab === 'skills'}
                >Skills</sl-tab
              >`
            : nothing}
          ${this.isTabVisible('project-templates')
            ? html`<sl-tab
                slot="nav"
                panel="project-templates"
                ?active=${this.activeTab === 'project-templates'}
                >Project Templates</sl-tab
              >`
            : nothing}
          ${this.isTabVisible('env-vars')
            ? html`<sl-tab-panel name="env-vars">
                <scion-env-var-list scope="hub" apiBasePath="/api/v1" compact></scion-env-var-list>
              </sl-tab-panel>`
            : nothing}
          ${this.isTabVisible('secrets')
            ? html`<sl-tab-panel name="secrets">
                <scion-secret-list scope="hub" apiBasePath="/api/v1" compact></scion-secret-list>
              </sl-tab-panel>`
            : nothing}
          ${this.isTabVisible('templates')
            ? html`<sl-tab-panel name="templates">
                <p class="tab-intro">
                  Global agent templates. Open one to browse and edit its files.
                </p>
                <scion-resource-import
                  kind="template"
                  scope="global"
                  canImport
                  @resource-changed=${() => this.refreshList('templates-list')}
                ></scion-resource-import>
                <scion-resource-list
                  id="templates-list"
                  kind="template"
                  scope="global"
                  detailBasePath="/settings"
                  canClone
                  canDelete
                  @resource-changed=${() => this.refreshList('templates-list')}
                ></scion-resource-list>
              </sl-tab-panel>`
            : nothing}
          ${this.isTabVisible('harness-configs')
            ? html`<sl-tab-panel name="harness-configs">
                <p class="tab-intro">
                  Global harness configurations. Open one to browse and edit its files.
                </p>
                <scion-resource-import
                  kind="harness-config"
                  scope="global"
                  canImport
                  @resource-changed=${() => this.refreshList('harness-configs-list')}
                ></scion-resource-import>
                <scion-resource-list
                  id="harness-configs-list"
                  kind="harness-config"
                  scope="global"
                  detailBasePath="/settings"
                  canClone
                  canDelete
                  @resource-changed=${() => this.refreshList('harness-configs-list')}
                ></scion-resource-list>
              </sl-tab-panel>`
            : nothing}
          ${this.isTabVisible('pre-start-hooks')
            ? html`<sl-tab-panel name="pre-start-hooks">
                <p class="tab-intro">
                  Hub-wide default pre-start hook. Staged for any agent whose project has no
                  project-level hook active.
                </p>
                <scion-pre-start-hook-list
                  apiBasePath="/api/v1"
                  ?readonly=${!this.canEditPreStartHooks}
                ></scion-pre-start-hook-list>
              </sl-tab-panel>`
            : nothing}
          ${this.isTabVisible('service-accounts')
            ? html`<!--
                THE COPY HERE STATES WHAT IS TRUE TODAY, NOT WHAT THE SCOPE IMPLIES.
                An earlier draft said these accounts "are usable from every project".
                The Hub does accept a hub-scoped account at agent creation, but no
                agent-creation picker offers one yet, so that sentence promised a
                capability a user cannot reach and would have read as a bug rather
                than as unfinished work. Both gaps below are part of step 5's
                definition of done; when the picker lands, this paragraph shrinks.
              -->
                <sl-tab-panel name="service-accounts">
                  <p class="tab-intro">
                    GCP service accounts registered at hub scope. They belong to the hub rather than
                    to any one project, which is why they are managed here.
                  </p>
                  <p class="tab-intro">
                    Two things are not available yet. Registering a hub-scoped account — use a
                    project's settings to register an account for that project. And selecting a
                    hub-scoped account when creating an agent, so an account listed here is not yet
                    offered on any project's agent form.
                  </p>
                  <scion-gcp-service-account-list scope="hub"></scion-gcp-service-account-list>
                </sl-tab-panel>`
            : nothing}
          ${this.isTabVisible('skills')
            ? html`<sl-tab-panel name="skills">
                <p class="tab-intro">
                  Skills automatically injected into all agents on this hub. System entries are
                  seeded from built-in platform skills and are read-only. User-defined entries can
                  be added and removed by hub admins.
                </p>
                <scion-injected-skills-panel scope="hub"></scion-injected-skills-panel>
              </sl-tab-panel>`
            : nothing}
          ${this.isTabVisible('project-templates')
            ? html`<sl-tab-panel name="project-templates">
                <p class="tab-intro">
                  Project templates for quick project setup. Create a template from any existing
                  project, then use it to create new projects with pre-configured settings.
                </p>
                <scion-project-template-list></scion-project-template-list>
              </sl-tab-panel>`
            : nothing}
        </sl-tab-group>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-settings': ScionPageSettings;
  }
}
