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
 * Shared plumbing for the chat's row menus (rail threads and groups,
 * members, messages), which each have two presentations:
 *
 * - desktop: the existing custom right-click popup, positioned at the
 *   pointer and clamped to the viewport;
 * - mobile layout (768px or narrower): a bottom `scion-action-sheet`.
 *
 * Each menu builds one `MenuAction[]` list and hands it to whichever
 * presentation is showing, so the two can never offer different actions.
 */

import { html, nothing } from 'lit';
import type { TemplateResult } from 'lit';

import type { ActionSheetItem } from './chat-action-sheet.js';
import { clampMenuToViewport, type Point } from './menu-position.js';
import { deepActiveElement } from '../deep-active-element.js';

/** The width at or under which the chat switches to its one-panel mobile layout. */
export const MENU_SHEET_MAX_WIDTH_PX = 768;

/** One menu action: what both presentations render, plus what it does. */
export interface MenuAction extends ActionSheetItem {
  run: () => void;
  /** Extra class on the desktop menu row, for tests and styling hooks. */
  className?: string;
}

/** Whether menus should open as a bottom sheet rather than a popup. */
export function shouldUseMenuSheet(): boolean {
  return (
    typeof window !== 'undefined' &&
    typeof window.matchMedia === 'function' &&
    window.matchMedia(`(max-width: ${MENU_SHEET_MAX_WIDTH_PX}px)`).matches
  );
}

/** Run the action with the given id from a sheet's `action-sheet-select`. */
export function runMenuAction(actions: MenuAction[], id: string): void {
  const action = actions.find((a) => a.id === id);
  if (action && !action.disabled) action.run();
}

/**
 * The deepest focused element, through open shadow roots, or null when
 * nothing (or only the document body) has focus.
 */
export function focusedElement(): HTMLElement | null {
  const active = deepActiveElement();
  return active instanceof HTMLElement && active !== document.body ? active : null;
}

/** The items of a rendered popup menu, in order. */
function menuItems(menu: HTMLElement): HTMLElement[] {
  return [...menu.querySelectorAll<HTMLElement>('[role="menuitem"], [role="menuitemcheckbox"]')];
}

/**
 * Keyboard focus around one host's desktop popup menu, after the WAI-ARIA
 * menu button pattern:
 *
 * - `opened(opener)` when the menu opens; the opener (by default the
 *   element holding focus) is where Escape and Tab return to;
 * - `afterRender(menu)` from the host's `updated()`: the first time after
 *   opening it moves focus to the first item;
 * - Escape closes and restores the opener; Tab and Shift+Tab close and
 *   continue from the opener in the normal direction (focus is moved to
 *   the opener during the keydown, so the browser's own Tab step starts
 *   there; nothing is restored later to undo it);
 * - after an item runs, focus returns to the opener only when the action
 *   left nothing focused (its row is gone): an action that moved focus on
 *   purpose (the composer, a jumped-to message) keeps it.
 */
export class MenuFocus {
  private opener: HTMLElement | null = null;
  private focusFirstPending = false;

  /** A popup menu opened from `opener` (default: the focused element). */
  opened(opener: HTMLElement | null = focusedElement()): void {
    this.opener = opener;
    this.focusFirstPending = true;
  }

  /** Move focus into a menu that just rendered after `opened()`. */
  afterRender(menu: HTMLElement | null | undefined): void {
    if (!this.focusFirstPending || !menu) return;
    this.focusFirstPending = false;
    menuItems(menu)[0]?.focus({ preventScroll: true });
  }

  /** The menu closed without keyboard intent (an outside click): forget it. */
  closed(): void {
    this.focusFirstPending = false;
    this.opener = null;
  }

  /** Escape: focus the opener again. */
  restore(): void {
    const opener = this.opener;
    this.closed();
    if (opener?.isConnected) opener.focus({ preventScroll: true });
  }

  /** After an action: focus the opener only if nothing else took focus. */
  restoreIfLost(): void {
    const opener = this.opener;
    this.closed();
    if (!opener) return;
    setTimeout(() => {
      if (focusedElement() === null && opener.isConnected) opener.focus({ preventScroll: true });
    }, 0);
  }
}

/** What `renderMenu` needs from its host. */
export interface MenuOptions {
  /** The menu's accessible name. */
  label: string;
  /** The host's focus tracking for this menu. */
  focus: MenuFocus;
  /** Close the menu (the host clears its open state). */
  close: () => void;
}

/**
 * Keys inside an open popup menu: Up/Down move between items (wrapping),
 * Home/End reach the ends, Enter/Space activate the focused item once
 * (never a disabled one), Escape closes and restores the opener, Tab
 * closes and leaves in the normal direction.
 */
export function handleMenuKeydown(e: KeyboardEvent, opts: MenuOptions): void {
  const menu = e.currentTarget as HTMLElement;
  const items = menuItems(menu);
  const root = menu.getRootNode() as Document | ShadowRoot;
  const index = items.indexOf(root.activeElement as HTMLElement);
  const focusAt = (i: number): void => {
    if (items.length === 0) return;
    items[(i + items.length) % items.length].focus({ preventScroll: true });
  };
  switch (e.key) {
    case 'ArrowDown':
      focusAt(index < 0 ? 0 : index + 1);
      break;
    case 'ArrowUp':
      focusAt(index < 0 ? items.length - 1 : index - 1);
      break;
    case 'Home':
      focusAt(0);
      break;
    case 'End':
      focusAt(items.length - 1);
      break;
    case 'Enter':
    case ' ': {
      const item = items[index];
      if (item && item.getAttribute('aria-disabled') !== 'true') item.click();
      break;
    }
    case 'Escape':
      opts.close();
      opts.focus.restore();
      break;
    case 'Tab':
      // No preventDefault: the browser's Tab step runs from the opener.
      opts.close();
      opts.focus.restore();
      e.stopPropagation();
      return;
    default:
      return;
  }
  e.preventDefault();
  e.stopPropagation();
}

/**
 * The rows of a desktop popup menu: `.context-menu-item` elements with an
 * optional icon, as `menuitem`s, or `menuitemcheckbox`es with
 * `aria-checked` for a toggle (`checked` set; the label also names the
 * action, such as "Unstar message"). Rows are focusable by script only
 * (tabindex -1): the menu moves focus among them. Disabled rows stay
 * visible, dimmed and focusable (`aria-disabled`), and never run. After a
 * row runs, `afterRun` (when given) is called.
 */
export function renderMenuRows(actions: MenuAction[], afterRun?: () => void): TemplateResult {
  return html`${actions.map(
    (a): TemplateResult => html`
      <div
        class="context-menu-item ${a.destructive ? 'danger' : ''} ${a.className ?? ''}"
        role=${a.checked === undefined ? 'menuitem' : 'menuitemcheckbox'}
        aria-checked=${a.checked === undefined ? nothing : a.checked ? 'true' : 'false'}
        tabindex="-1"
        style=${a.disabled ? 'opacity: 0.4; pointer-events: none;' : nothing}
        aria-disabled=${a.disabled ? 'true' : nothing}
        @click=${(): void => {
          if (a.disabled) return;
          a.run();
          afterRun?.();
        }}
      >
        ${a.icon ? html`<sl-icon name=${a.icon} aria-hidden="true"></sl-icon>` : nothing} ${a.label}
      </div>
    `
  )}`;
}

/**
 * A complete desktop popup menu: the named `role="menu"` container (class
 * `context-menu`, which each host styles and positions) holding the rows,
 * with the menu keys. Clicks inside do not reach the host's outside-click
 * dismissal. Renders nothing for an empty action list.
 */
export function renderMenu(actions: MenuAction[], opts: MenuOptions): TemplateResult {
  if (actions.length === 0) return html``;
  return html`
    <div
      class="context-menu"
      role="menu"
      aria-label=${opts.label}
      @click=${(e: Event): void => e.stopPropagation()}
      @keydown=${(e: KeyboardEvent): void => handleMenuKeydown(e, opts)}
    >
      ${renderMenuRows(actions, () => opts.focus.restoreIfLost())}
    </div>
  `;
}

/**
 * Position a fixed popup menu at `anchor`, clamped to the viewport.
 *
 * Call from the host's `updated()` while the menu is open. The menu renders
 * with `visibility: hidden` so its size can be measured before it is seen;
 * this sets its final `left`/`top` and makes it visible, all before the
 * browser paints.
 */
export function placeMenuInViewport(menu: HTMLElement | null | undefined, anchor: Point): void {
  if (!menu) return;
  const rect = menu.getBoundingClientRect();
  const root = document.documentElement;
  const pos = clampMenuToViewport(
    anchor,
    { w: rect.width, h: rect.height },
    { w: root.clientWidth || window.innerWidth, h: root.clientHeight || window.innerHeight }
  );
  menu.style.left = `${pos.x}px`;
  menu.style.top = `${pos.y}px`;
  menu.style.visibility = 'visible';
}
