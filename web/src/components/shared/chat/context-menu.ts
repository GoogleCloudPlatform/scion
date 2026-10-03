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
 * The rows of a desktop popup menu: `.context-menu-item` divs, each with an
 * optional icon. Disabled rows stay visible but dimmed and inert, as the
 * rail's edge-of-list "Move up" / "Move down" always have been.
 */
export function renderMenuRows(actions: MenuAction[]): TemplateResult {
  return html`${actions.map(
    (a): TemplateResult => html`
      <div
        class="context-menu-item ${a.destructive ? 'danger' : ''} ${a.className ?? ''}"
        style=${a.disabled ? 'opacity: 0.4; pointer-events: none;' : nothing}
        aria-disabled=${a.disabled ? 'true' : nothing}
        @click=${(): void => a.run()}
      >
        ${a.icon ? html`<sl-icon name=${a.icon}></sl-icon>` : nothing} ${a.label}
      </div>
    `
  )}`;
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
