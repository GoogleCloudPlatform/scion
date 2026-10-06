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
 * Is `el` a currently-open *modal* surface — an `sl-dialog`, a non-
 * `contained` `sl-drawer` (a `contained` drawer renders inside its own
 * container rather than as a page-blocking overlay, per Shoelace), or a
 * native `<dialog open>`? Not every Shoelace element that fires `sl-show` is
 * modal: toasts (`sl-alert`), `sl-tooltip`, `sl-dropdown`, `sl-details` and
 * `sl-select` all fire it, and none of them count here.
 */
export function isOpenModalElement(el: Element): boolean {
  const tag = el.tagName;
  if (tag === 'SL-DIALOG') {
    return Boolean((el as unknown as { open?: boolean }).open);
  }
  if (tag === 'SL-DRAWER') {
    if (el.hasAttribute('contained')) return false;
    return Boolean((el as unknown as { open?: boolean }).open);
  }
  if (tag === 'DIALOG') {
    return el.hasAttribute('open');
  }
  return false;
}

/**
 * Recursively walks `root`'s descendants — including into every open
 * shadow root, not just the light-DOM tree `querySelectorAll` alone would
 * reach — looking for an open modal (see `isOpenModalElement`). `exclude`
 * (the caller's own palette host) and everything inside its shadow tree is
 * skipped entirely, since composedPath()-based exclusion does not work here:
 * the caller's own dialog lives inside `exclude`'s shadow root, and
 * `Element.contains()` does not cross shadow boundaries.
 *
 * Hidden subtrees are skipped too: an element with the `hidden` attribute,
 * its light-DOM descendants and its shadow tree. A dialog left open inside
 * one (such as a retained terminal pane that is off screen) is not visible,
 * so it does not count as an open modal.
 */
export function hasOpenModalDescendant(root: ParentNode, exclude: Element | null): boolean {
  for (const el of Array.from(root.querySelectorAll('*'))) {
    if (exclude && el === exclude) continue;
    if (el.closest('[hidden]')) continue;
    if (isOpenModalElement(el)) return true;
    if (el.shadowRoot && hasOpenModalDescendant(el.shadowRoot, exclude)) {
      return true;
    }
  }
  return false;
}
