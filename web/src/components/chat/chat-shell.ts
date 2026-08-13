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
 * Chat Shell Component (4th ShellType)
 *
 * Layout shell for the top-level chat mode. Replaces the main nav sidebar
 * with a thread rail and provides a slim header with project context.
 * Modeled on profile-shell.ts.
 *
 * Key design decisions (from design.md Section 4.1):
 * - NOT a second HTML entry point; a fourth ShellType in the existing SPA
 * - Switching between app and chat mode does NOT reload the document (AC19c)
 * - MUST re-register the scion:access-denied listener (AC19e)
 * - Reuses showToast() and stateManager SSE connection (AC19f)
 * - Dynamic document titles (agent name, unread count)
 */

import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import '../shared/header.js';
import '../shared/debug-panel.js';

import type { User } from '../../shared/types.js';
import type { AccessDeniedDetail } from '../../client/api.js';
import { showToast } from '../../utils/toast.js';
import { performLogout } from '../../utils/auth.js';
import { setDocumentTitle, PAGE_TITLE_EVENT } from '../../client/page-title.js';
import type { PageTitleDetail } from '../../client/page-title.js';
import { isFeatureEnabled, NATIVE_CHAT_V2_FLAG } from '../../utils/feature-flags.js';

@customElement('scion-chat-shell')
export class ScionChatShell extends LitElement {
  @property({ type: Object })
  user: User | null = null;

  @property({ type: String })
  currentPath = '/chat';

  /** Bound listener references for cleanup */
  private _accessDeniedHandler = this.handleAccessDenied.bind(this);
  private _pageTitleHandler = this.handlePageTitle.bind(this);

  static override styles = css`
    :host {
      display: flex;
      height: 100vh;
      height: 100dvh;
      background: var(--scion-bg, #f8fafc);
    }

    .main {
      flex: 1;
      display: flex;
      flex-direction: column;
      min-width: 0;
    }

    .content {
      flex: 1;
      overflow: hidden;
      display: flex;
      flex-direction: column;
    }

    /* V2 three-panel layout */
    .content-v2 {
      flex: 1;
      overflow: hidden;
      display: flex;
      flex-direction: row;
    }

    .content-v2 .rail-panel {
      width: 260px;
      min-width: 200px;
      max-width: 320px;
      border-right: 1px solid var(--scion-border, #e2e8f0);
      overflow: hidden;
    }

    .content-v2 .center-panel {
      flex: 1;
      min-width: 0;
      display: flex;
      flex-direction: column;
      overflow: hidden;
    }

    .content-v2 .members-panel {
      width: 240px;
      border-left: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
      display: flex;
      flex-direction: column;
      overflow: hidden;
    }

    .members-panel.collapsed {
      display: none;
    }

    .members-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .members-placeholder {
      flex: 1;
      display: flex;
      align-items: center;
      justify-content: center;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
      padding: 1rem;
      text-align: center;
    }

    /* Members toggle button in the header area */
    .members-toggle {
      position: absolute;
      right: 1rem;
      top: 50%;
      transform: translateY(-50%);
    }

    @media (max-width: 768px) {
      .content-v2 .rail-panel {
        width: 100%;
        max-width: none;
      }

      .content-v2 .members-panel {
        display: none;
      }
    }
  `;

  /**
   * Re-register the scion:access-denied listener so 403 errors still
   * raise a toast in chat mode. This is the most likely single defect
   * if omitted (AC19e, design.md Section 4.1).
   */
  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener('scion:access-denied', this._accessDeniedHandler as EventListener);
    this.addEventListener(PAGE_TITLE_EVENT, this._pageTitleHandler as EventListener);
    this.updateDocumentTitle();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener('scion:access-denied', this._accessDeniedHandler as EventListener);
    this.removeEventListener(PAGE_TITLE_EVENT, this._pageTitleHandler as EventListener);
  }

  override updated(changedProperties: Map<string, unknown>): void {
    if (changedProperties.has('currentPath')) {
      this.updateDocumentTitle();
    }
  }

  /**
   * Handle page-title events from the chat page component to set
   * agent-specific titles (e.g. "agent-name - Chat - Scion").
   */
  private handlePageTitle(event: CustomEvent<PageTitleDetail>): void {
    const segments = event.detail?.segments;
    if (segments && segments.length > 0) {
      setDocumentTitle(...segments);
    }
  }

  private updateDocumentTitle(): void {
    // Extract agent context from path for dynamic titles
    const match = this.currentPath.match(/^\/chat\/([^/]+)/);
    if (match) {
      setDocumentTitle(decodeURIComponent(match[1]), 'Chat');
    } else {
      setDocumentTitle('Chat');
    }
  }

  private handleAccessDenied(event: CustomEvent<AccessDeniedDetail>): void {
    const detail = event.detail || {};
    const action = detail.action || 'perform this action on';
    const message = `You don't have permission to ${action} this resource.`;
    showToast(message, 'warning');
  }

  override render() {
    const isV2 = isFeatureEnabled(NATIVE_CHAT_V2_FLAG);

    return html`
      <main class="main">
        <scion-header
          .user=${this.user}
          .currentPath=${this.currentPath}
          .pageTitle=${'Chat'}
          ?showMobileMenu=${false}
          @logout=${(): void => this.handleLogout()}
        ></scion-header>

        <div class="${isV2 ? 'content-v2' : 'content'}">
          <slot></slot>
        </div>
      </main>

      <scion-debug-panel></scion-debug-panel>
    `;
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
    'scion-chat-shell': ScionChatShell;
  }
}
