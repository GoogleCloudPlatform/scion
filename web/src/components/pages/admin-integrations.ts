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

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch } from '../../client/api.js';
import { loadAdminStatus } from '../../client/admin-status.js';
import { pushUrl, replaceUrl } from '../../client/navigation.js';
import { hasAnyPermission } from '../../lib/admin-permissions.js';
import type { PageData } from '../../shared/types.js';
import {
  INTEGRATION_TABS,
  legacyIntegrationRedirect,
  parseIntegrationsRoute,
  type IntegrationTab,
} from './admin-integrations-tabs.js';

export { integrationSettingsPayload } from './admin-integrations-chat.js';
export {
  INTEGRATION_TABS,
  legacyIntegrationRedirect,
  parseIntegrationsRoute,
} from './admin-integrations-tabs.js';

@customElement('scion-page-admin-integrations')
export class ScionPageAdminIntegrations extends LitElement {
  /** Page data from the router (provides current user id for admin-status lookup). */
  @property({ type: Object }) pageData?: PageData;

  @state() private statusResolved = false;
  @state() private probingGitHub = true;
  @state() private canReadIntegrations = true;
  @state() private canReadGitHubApp = false;
  @state() private activeTabId = 'chat';
  @state() private subPath: string | null = null;
  @state() private loadedTabs = new Set<string>();
  @state() private loadErrors = new Map<string, string>();

  private tabElements = new Map<string, HTMLElement>();
  private popstateHandler = () => {
    this.syncFromLocation();
    void this.ensureActiveTabLoaded();
  };

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 0.5rem;
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

    .header-description {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0 0 1.25rem 0;
    }

    sl-tab-group::part(base) {
      gap: 0;
    }

    sl-tab-group::part(tabs) {
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      margin-bottom: 1.25rem;
    }

    sl-tab::part(base) {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
    }

    .loading-container {
      display: flex;
      justify-content: center;
      align-items: center;
      padding: 3rem;
    }

    .status-message.error {
      font-size: 0.875rem;
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      margin-bottom: 1rem;
      background: var(--scion-error-bg, #fef2f2);
      color: var(--scion-error-text, #991b1b);
      border: 1px solid var(--scion-error-border, #fca5a5);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener('popstate', this.popstateHandler);
    this.syncFromLocation();
    void this.initTabs();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener('popstate', this.popstateHandler);
  }

  private get visibleTabs(): IntegrationTab[] {
    const ctx = {
      canReadIntegrations: this.canReadIntegrations,
      canReadGitHubApp: this.canReadGitHubApp,
    };
    return INTEGRATION_TABS.filter((tab) => tab.visible(ctx));
  }

  private syncFromLocation(): void {
    const pathname = window.location.pathname;
    const redirected = legacyIntegrationRedirect(pathname);
    if (redirected) {
      replaceUrl(redirected);
    }
    const { tabId, subPath } = parseIntegrationsRoute(window.location.pathname);
    this.subPath = tabId === 'chat' ? subPath : null;
    if (tabId) {
      this.activeTabId = tabId;
    } else {
      const first = this.visibleTabs[0];
      this.activeTabId = first ? first.id : 'chat';
    }
  }

  private async initTabs(): Promise<void> {
    const userId = this.pageData?.user?.id;
    if (userId) {
      const adminStatus = await loadAdminStatus(userId);
      if (adminStatus) {
        this.canReadIntegrations = hasAnyPermission(adminStatus, ['hub.integrations.read']);
      }
    }
    this.statusResolved = true;

    // On a Chat detail route (/admin/integrations/chat/{name}) the tab bar is
    // not rendered, so skip the GitHub probe and load Chat immediately.
    if (this.subPath !== null && this.activeTabId === 'chat' && this.canReadIntegrations) {
      this.probingGitHub = false;
      await this.ensureActiveTabLoaded();
      return;
    }

    // If the currently requested tab is already visible (e.g. Chat), start
    // loading and rendering it immediately without waiting for the GitHub probe.
    const activeAlreadyVisible = this.visibleTabs.some((t) => t.id === this.activeTabId);
    const preloadPromise = activeAlreadyVisible ? this.ensureActiveTabLoaded() : null;

    this.probingGitHub = true;
    try {
      const res = await apiFetch('/api/v1/github-app', { suppressAccessDeniedToast: true });
      // Hide the GitHub tab only when the caller is unauthorized (401/403).
      // Transient 5xx or network errors keep the tab visible so the card can
      // render its own load error and save guard.
      this.canReadGitHubApp = res.status !== 401 && res.status !== 403;
    } catch {
      this.canReadGitHubApp = true;
    } finally {
      this.probingGitHub = false;
    }

    // Ensure activeTabId is actually visible to the caller; fall back to first visible tab.
    const visible = this.visibleTabs;
    if (!visible.some((t) => t.id === this.activeTabId)) {
      const first = visible[0];
      if (first) {
        this.activeTabId = first.id;
        this.subPath = null;
        if (
          window.location.pathname !== `/admin/integrations/${first.id}` &&
          window.location.pathname !== '/admin/integrations'
        ) {
          replaceUrl(`/admin/integrations/${first.id}`);
        }
      }
    }

    if (preloadPromise) {
      await preloadPromise;
    }
    await this.ensureActiveTabLoaded();
  }

  private async ensureActiveTabLoaded(): Promise<void> {
    const tab = this.visibleTabs.find((t) => t.id === this.activeTabId);
    if (!tab) return;

    if (!this.loadedTabs.has(tab.id)) {
      try {
        await tab.load();
        const nextLoaded = new Set(this.loadedTabs);
        nextLoaded.add(tab.id);
        this.loadedTabs = nextLoaded;
        if (this.loadErrors.has(tab.id)) {
          const nextErrors = new Map(this.loadErrors);
          nextErrors.delete(tab.id);
          this.loadErrors = nextErrors;
        }
      } catch {
        const nextErrors = new Map(this.loadErrors);
        nextErrors.set(tab.id, `Failed to load ${tab.label} integration panel.`);
        this.loadErrors = nextErrors;
      }
    }

    this.syncMountedElementProps(tab);
  }

  private getOrCreateTabElement(tab: IntegrationTab): HTMLElement {
    let el = this.tabElements.get(tab.id);
    if (!el) {
      el = document.createElement(tab.tag);
      this.tabElements.set(tab.id, el);
    }
    this.syncElementSubPath(tab, el);
    return el;
  }

  private syncMountedElementProps(tab: IntegrationTab): void {
    const el = this.tabElements.get(tab.id);
    if (el) {
      this.syncElementSubPath(tab, el);
    }
  }

  private syncElementSubPath(tab: IntegrationTab, el: HTMLElement): void {
    if (tab.id === 'chat') {
      (el as HTMLElement & { integrationName?: string | null }).integrationName = this.subPath;
    }
  }

  private handleTabShow(e: CustomEvent<{ name: string }>): void {
    if (e.target !== e.currentTarget) return;
    const nextTabId = e.detail?.name;
    if (!nextTabId || nextTabId === this.activeTabId) return;
    if (!this.visibleTabs.some((t) => t.id === nextTabId)) return;

    this.activeTabId = nextTabId;
    this.subPath = null;
    const targetPath = `/admin/integrations/${nextTabId}`;
    if (window.location.pathname !== targetPath) {
      pushUrl(targetPath);
    }
    void this.ensureActiveTabLoaded();
  }

  private renderTabContent(tab: IntegrationTab) {
    const err = this.loadErrors.get(tab.id);
    if (err) {
      return html`<div class="status-message error">${err}</div>`;
    }
    if (!this.loadedTabs.has(tab.id)) {
      return html`<div class="loading-container"><sl-spinner></sl-spinner></div>`;
    }
    return this.getOrCreateTabElement(tab);
  }

  override render() {
    const tabs = this.visibleTabs;
    const activeTab = tabs.find((t) => t.id === this.activeTabId) ?? tabs[0];

    // Wait only until we know whether the active tab can render (e.g. a
    // GitHub-only caller waiting for the GitHub probe). Chat list/detail views
    // render immediately without waiting on GET /api/v1/github-app.
    if (
      !this.statusResolved ||
      (this.probingGitHub && (!activeTab || activeTab.id !== this.activeTabId))
    ) {
      return html`<div class="loading-container"><sl-spinner></sl-spinner></div>`;
    }

    // Detail sub-route (e.g. /admin/integrations/chat/{name}): render the Chat
    // detail view directly so its breadcrumb sits at the top of the page.
    if (this.subPath !== null && activeTab?.id === 'chat') {
      return this.renderTabContent(activeTab);
    }

    return html`
      <div class="header">
        <sl-icon name="plug"></sl-icon>
        <h1>Integrations</h1>
      </div>
      <p class="header-description">Connect this hub to chat platforms and code hosts.</p>

      <sl-tab-group @sl-tab-show=${(e: CustomEvent<{ name: string }>) => this.handleTabShow(e)}>
        ${tabs.map(
          (tab) => html`
            <sl-tab slot="nav" panel=${tab.id} ?active=${this.activeTabId === tab.id}>
              <sl-icon name=${tab.icon}></sl-icon>
              ${tab.label}
            </sl-tab>
          `
        )}
        ${tabs.map(
          (tab) => html`
            <sl-tab-panel name=${tab.id}>
              ${this.activeTabId === tab.id ? this.renderTabContent(tab) : nothing}
            </sl-tab-panel>
          `
        )}
      </sl-tab-group>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-integrations': ScionPageAdminIntegrations;
  }
}
