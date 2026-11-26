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

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { AdminStatus } from '../../lib/admin-permissions.js';
import type { PageData } from '../../shared/types.js';
import {
  GITHUB_ADMIN_SECTIONS,
  type GitHubAdminSection,
} from './admin-integrations-github-sections.js';

export { GITHUB_ADMIN_SECTIONS } from './admin-integrations-github-sections.js';

@customElement('scion-admin-integrations-github')
export class ScionAdminIntegrationsGitHub extends LitElement {
  @property({ type: Object }) adminStatus: AdminStatus | null = null;
  @property({ type: Object }) pageData?: PageData;

  @state() private loadedSections = new Set<string>();
  @state() private loadErrors = new Map<string, string>();
  @state() private appConfigured = false;
  @state() private configSaveCounter = 0;

  private sectionElements = new Map<string, HTMLElement>();

  static override styles = css`
    :host {
      display: block;
    }

    .sections-stack {
      display: flex;
      flex-direction: column;
      gap: 1.5rem;
    }

    .section-loading {
      display: flex;
      justify-content: center;
      padding: 2rem;
    }

    .status-message.error {
      font-size: 0.875rem;
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-error-bg, #fef2f2);
      color: var(--scion-error-text, #991b1b);
      border: 1px solid var(--scion-error-border, #fca5a5);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadAllSections();
  }

  private async loadAllSections(): Promise<void> {
    await Promise.all(
      GITHUB_ADMIN_SECTIONS.map(async (section) => {
        try {
          await section.load();
          const nextLoaded = new Set(this.loadedSections);
          nextLoaded.add(section.id);
          this.loadedSections = nextLoaded;
        } catch {
          const nextErrors = new Map(this.loadErrors);
          nextErrors.set(section.id, `Failed to load section "${section.id}".`);
          this.loadErrors = nextErrors;
        }
      })
    );
  }

  private handleAppConfigChanged(e: CustomEvent<{ configured: boolean; saved?: boolean }>): void {
    this.appConfigured = Boolean(e.detail?.configured);
    if (e.detail?.saved) {
      this.configSaveCounter += 1;
    }
    for (const el of this.sectionElements.values()) {
      this.syncSectionContext(el);
    }
  }

  override updated(changed: Map<string, unknown>): void {
    super.updated(changed);
    if (changed.has('adminStatus') || changed.has('pageData')) {
      for (const el of this.sectionElements.values()) {
        this.syncSectionContext(el);
      }
    }
  }

  private syncSectionContext(el: HTMLElement): void {
    const target = el as HTMLElement & {
      appConfigured?: boolean;
      configSaveCounter?: number;
      adminStatus?: AdminStatus | null;
      pageData?: PageData | undefined;
    };
    target.appConfigured = this.appConfigured;
    target.configSaveCounter = this.configSaveCounter;
    target.adminStatus = this.adminStatus;
    target.pageData = this.pageData;
  }

  private getOrCreateSectionElement(section: GitHubAdminSection): HTMLElement {
    let el = this.sectionElements.get(section.id);
    if (!el) {
      el = document.createElement(section.tag);
      el.setAttribute('data-github-section', section.id);
      this.sectionElements.set(section.id, el);
    }
    this.syncSectionContext(el);
    return el;
  }

  override render() {
    return html`
      <div
        class="sections-stack"
        @github-app-config-changed=${(e: CustomEvent<{ configured: boolean }>) =>
          this.handleAppConfigChanged(e)}
      >
        ${GITHUB_ADMIN_SECTIONS.map((section) => {
          const err = this.loadErrors.get(section.id);
          if (err) {
            return html`<div class="status-message error">${err}</div>`;
          }
          if (!this.loadedSections.has(section.id)) {
            return html`<div class="section-loading"><sl-spinner></sl-spinner></div>`;
          }
          return this.getOrCreateSectionElement(section);
        })}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-admin-integrations-github': ScionAdminIntegrationsGitHub;
  }
}
