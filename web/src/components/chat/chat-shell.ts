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
    return html`
      <main class="main">
        <scion-header
          .user=${this.user}
          .currentPath=${this.currentPath}
          .pageTitle=${'Chat'}
          ?showMobileMenu=${false}
          @logout=${(): void => this.handleLogout()}
        ></scion-header>

        <div class="content">
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
