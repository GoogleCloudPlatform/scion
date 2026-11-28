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

/** Minimal shape of a Shoelace `<sl-menu-item type="checkbox">` element. */
export type SlCheckboxMenuItem = HTMLElement & {
  value: string;
  checked?: boolean;
};

/**
 * Reconcile a single-select `<sl-menu-item type="checkbox">` after `sl-select`.
 *
 * Shoelace toggles `item.checked` before emitting `sl-select`. Lit's
 * `?checked=${expr}` binding only updates the DOM when `expr` changes from its
 * previously committed value, so re-selecting an already-active item leaves it
 * unchecked unless the handler explicitly restores `checked`.
 */
export function syncSingleSelectMenuChecked(
  item: SlCheckboxMenuItem | null | undefined,
  checked: boolean
): void {
  if (!item) return;
  item.checked = checked;
  item.toggleAttribute('checked', checked);
}
