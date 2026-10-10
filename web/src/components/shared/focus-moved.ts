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
 * Programmatic focus moves, announced.
 *
 * Code that moves focus itself (a `focus()` or `blur()` call, a modal
 * opening or closing) goes through these helpers, which make the move and
 * then fire {@link FOCUS_MOVED_EVENT} on `window`. Anything that tracks
 * where focus is (the chat shell's text-entry state) re-reads
 * `document.activeElement` on it, rather than relying on focus events
 * alone, which a programmatic move does not always deliver where a
 * listener sees them.
 */

export const FOCUS_MOVED_EVENT = 'scion-focus-moved';

function announce(): void {
  window.dispatchEvent(new Event(FOCUS_MOVED_EVENT));
}

/** Focus `el` (if any), then announce the move. */
export function focusElement(el: HTMLElement | null | undefined, options?: FocusOptions): void {
  if (!el) return;
  el.focus(options);
  announce();
}

/** Blur `el` (if any), then announce the move. */
export function blurElement(el: HTMLElement | SVGElement | null | undefined): void {
  if (!el) return;
  el.blur();
  announce();
}

/**
 * Run `move`, something that moves focus as a side effect (opening or
 * closing a modal dialog), then announce the move.
 */
export function withFocusMove(move: () => void): void {
  move();
  announce();
}
