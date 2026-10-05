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
import type { User } from '../../shared/types.js';
import type { AccessDeniedDetail } from '../../client/api.js';
import { showAccessDeniedToast } from '../../utils/access-denied.js';
import { performLogout } from '../../utils/auth.js';
import { setDocumentTitle, PAGE_TITLE_EVENT } from '../../client/page-title.js';
import type { PageTitleDetail } from '../../client/page-title.js';
import { enterAppFrame, exitAppFrame } from '../shared/app-frame.js';
import { deepActiveElement } from '../shared/deep-active-element.js';
import { FOCUS_MOVED_EVENT } from '../shared/focus-moved.js';

/** Whether `el` takes typed text, and so brings up the on-screen keyboard. */
function isTextField(el: Element | null): boolean {
  if (!el) return false;
  if (el instanceof HTMLTextAreaElement) return !el.readOnly && !el.disabled;
  if (el instanceof HTMLInputElement) {
    const nonText = [
      'button',
      'checkbox',
      'color',
      'file',
      'hidden',
      'image',
      'radio',
      'range',
      'reset',
      'submit',
    ];
    return !nonText.includes(el.type) && !el.readOnly && !el.disabled;
  }
  return el instanceof HTMLElement && el.isContentEditable;
}

/**
 * Whether `el` is a modal surface (a dialog or drawer): a field in one, such
 * as the quick switcher's query, does not hide the top bar, since focus goes
 * back to the control that opened it when it closes.
 */
function isOverlay(el: Element): boolean {
  return (
    el.localName === 'sl-dialog' ||
    el.localName === 'sl-drawer' ||
    el.localName === 'dialog' ||
    el.getAttribute('role') === 'dialog'
  );
}

/**
 * Whether `el` is a text field inside `shell`, outside its app top bar and
 * outside any dialog (the composer, a search box in the rail, ...).
 * Walks out through shadow roots to find what contains it.
 */
function isShellTextField(el: Element | null, shell: HTMLElement): boolean {
  if (!isTextField(el)) return false;
  let node: Node | null = el;
  while (node) {
    if (node === shell) return true;
    if (node instanceof Element && (node.localName === 'scion-header' || isOverlay(node))) {
      return false;
    }
    const parent: Node | null = node.parentNode;
    node = parent instanceof ShadowRoot ? parent.host : parent;
  }
  return false;
}

@customElement('scion-chat-shell')
export class ScionChatShell extends LitElement {
  @property({ type: Object })
  user: User | null = null;

  @property({ type: String })
  currentPath = '/chat';

  /** Bound listener references for cleanup */
  private _accessDeniedHandler = this.handleAccessDenied.bind(this);
  private _pageTitleHandler = this.handlePageTitle.bind(this);
  private _focusFrame: number | null = null;
  /** Watches the top bar's height while it is visible. */
  private _topBarObserver: ResizeObserver | null = null;

  static override styles = css`
    :host {
      display: flex;
      height: var(--scion-app-height, 100dvh);
      background: var(--scion-bg, #f8fafc);
      touch-action: manipulation;
    }

    /* Below the chat page's mobile breakpoint a horizontal drag belongs to
       its panel swipe, so a drag that starts on the header must not become
       Chromium's overscroll history-back either (see the panel rule in
       pages/chat.ts). Pinch-zoom is kept. */
    @media (max-width: 768px) {
      :host {
        touch-action: pan-y pinch-zoom;
      }
    }

    .main {
      flex: 1;
      display: flex;
      flex-direction: column;
      min-width: 0;
    }

    /* While the on-screen keyboard leaves only a short frame and a text
       field in the shell (outside the top bar and any dialog) has focus,
       the app's top bar gives way so the conversation header, a message
       and the composer still fit. It is back as soon as focus leaves the
       field or the keyboard closes. Tying it to focus as well as the frame
       keeps it on screen whenever focus may go back to one of its
       controls, such as a dismissed quick switcher returning focus to its
       button. --scion-kb-short-display is set to none on the root by
       client/viewport.ts; the fallback is the header's own display. */
    :host([text-entry]) scion-header {
      display: var(--scion-kb-short-display, grid);
    }

    /* The top bar's height (--scion-chat-top-bar-h, written on the host
       while the bar is visible) and whether it is hidden (1 or unset),
       so the thread can count the bar as if it were shown when it sizes
       the composer's field (composer-room.ts): hiding it then never makes
       the field jump. */
    :host([text-entry]) {
      --scion-chat-top-bar-hidden: var(--scion-kb-short);
    }

    /* The chat column compacts itself while the keyboard is open (the
       composer's reply and edit bars) and in a tight keyboard frame (the
       composer's chip, footer and padding, the message list's padding)
       only inside this shell; elsewhere these stay unset and it never
       does. */
    :host {
      --scion-chat-kb-open: var(--scion-kb-open);
      --scion-chat-tight: var(--scion-kb-tight);
      --scion-chat-tight-position: var(--scion-kb-tight-position);
      --scion-chat-tight-visibility: var(--scion-kb-tight-visibility);
    }

    /* Three-panel layout */
    .content {
      flex: 1;
      overflow: hidden;
      display: flex;
      flex-direction: row;
    }

    .content ::slotted(*) {
      flex: 1;
      min-width: 0;
    }
  `;

  /**
   * Re-register the scion:access-denied listener so 403 errors still
   * raise a toast in chat mode. This is the most likely single defect
   * if omitted (AC19e, design.md Section 4.1).
   */
  override connectedCallback(): void {
    super.connectedCallback();
    enterAppFrame();
    window.addEventListener('scion:access-denied', this._accessDeniedHandler as EventListener);
    this.addEventListener(PAGE_TITLE_EVENT, this._pageTitleHandler as EventListener);
    this.addEventListener('focusin', this.updateTextEntry);
    this.addEventListener('focusout', this.scheduleTextEntryUpdate);
    // Focus can also move without a focus event reaching here: code that
    // moves it itself announces the move, typing means a field has focus,
    // and the keyboard opening or closing follows a move. Each re-reads
    // document.activeElement.
    window.addEventListener(FOCUS_MOVED_EVENT, this.updateTextEntry);
    this.addEventListener('input', this.updateTextEntry);
    window.visualViewport?.addEventListener('resize', this.scheduleTextEntryUpdate);
    void this.observeTopBar();
    this.updateDocumentTitle();
  }

  /** Watch the top bar's height, once it has rendered. */
  private async observeTopBar(): Promise<void> {
    await this.updateComplete;
    const header = this.renderRoot.querySelector('scion-header');
    if (!this.isConnected || this._topBarObserver || !header) return;
    if (typeof ResizeObserver === 'undefined') return;
    this._topBarObserver = new ResizeObserver(() => this.recordTopBarHeight(header));
    this._topBarObserver.observe(header);
  }

  /**
   * Publish the top bar's height while it is visible; a hidden bar
   * measures 0, and the last visible height is kept.
   */
  private recordTopBarHeight(header: Element): void {
    const height = header.getBoundingClientRect().height;
    if (height > 0) this.style.setProperty('--scion-chat-top-bar-h', `${height}px`);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    exitAppFrame();
    window.removeEventListener('scion:access-denied', this._accessDeniedHandler as EventListener);
    this.removeEventListener(PAGE_TITLE_EVENT, this._pageTitleHandler as EventListener);
    this.removeEventListener('focusin', this.updateTextEntry);
    this.removeEventListener('focusout', this.scheduleTextEntryUpdate);
    window.removeEventListener(FOCUS_MOVED_EVENT, this.updateTextEntry);
    this.removeEventListener('input', this.updateTextEntry);
    window.visualViewport?.removeEventListener('resize', this.scheduleTextEntryUpdate);
    if (this._focusFrame !== null) cancelAnimationFrame(this._focusFrame);
    this._focusFrame = null;
    this.removeAttribute('text-entry');
    this._topBarObserver?.disconnect();
    this._topBarObserver = null;
  }

  /**
   * Set the text-entry attribute while a text field in this shell, outside
   * the app's top bar and any dialog, has focus.
   */
  private readonly updateTextEntry = (): void => {
    this.toggleAttribute('text-entry', isShellTextField(deepActiveElement(), this));
  };

  /**
   * On focusout, wait a frame before re-checking, so moving focus from one
   * field to another does not flash the top bar in between.
   */
  private readonly scheduleTextEntryUpdate = (): void => {
    if (this._focusFrame !== null) return;
    this._focusFrame = requestAnimationFrame(() => {
      this._focusFrame = null;
      this.updateTextEntry();
    });
  };

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
    // Guard against double-toast when both app-shell and chat-shell are mounted.
    // The app-shell handler runs first (registered on a parent element); if it
    // already handled this event, skip the duplicate toast.
    const detail = event.detail || {};
    if ((detail as Record<string, unknown>)._handled) return;
    (detail as Record<string, unknown>)._handled = true;
    showAccessDeniedToast(detail);
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
