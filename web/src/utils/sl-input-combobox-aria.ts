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
 * Combobox ARIA state to forward onto a Shoelace `<sl-input>`'s inner `<input>`.
 */
export interface SlInputComboboxAriaState {
  expanded: boolean;
  controls?: string | undefined;
  activeDescendant?: string | undefined;
}

const latestState = new WeakMap<Element, SlInputComboboxAriaState>();
const pendingSync = new WeakSet<Element>();

function applyComboboxAria(input: HTMLInputElement, state: SlInputComboboxAriaState): void {
  input.setAttribute('role', 'combobox');
  input.setAttribute('aria-autocomplete', 'list');
  input.setAttribute('aria-expanded', state.expanded ? 'true' : 'false');

  if (state.controls) {
    input.setAttribute('aria-controls', state.controls);
  } else {
    input.removeAttribute('aria-controls');
  }

  if (state.activeDescendant) {
    input.setAttribute('aria-activedescendant', state.activeDescendant);
  } else {
    input.removeAttribute('aria-activedescendant');
  }
}

/**
 * Forward combobox ARIA attributes onto a Shoelace `<sl-input>`'s shadow-DOM
 * `<input>` element.
 *
 * Shoelace does not forward host `role` or `aria-expanded` attributes to its
 * inner `<input>`, so placing them on `<sl-input>` leaves assistive technology
 * without combobox state. If the `<sl-input>` has not finished its initial Lit
 * render yet, the latest state is applied once `updateComplete` resolves.
 */
export function syncSlInputComboboxAria(
  slInput: Element | null | undefined,
  state: SlInputComboboxAriaState
): void {
  if (!slInput) return;

  latestState.set(slInput, state);

  const inner = slInput.shadowRoot?.querySelector<HTMLInputElement>('input');
  if (inner) {
    applyComboboxAria(inner, state);
    return;
  }

  const updateComplete = (slInput as { updateComplete?: Promise<unknown> }).updateComplete;
  if (updateComplete && !pendingSync.has(slInput)) {
    pendingSync.add(slInput);
    void updateComplete.then(() => {
      pendingSync.delete(slInput);
      const current = latestState.get(slInput);
      const resolved = slInput.shadowRoot?.querySelector<HTMLInputElement>('input');
      if (current && resolved) {
        applyComboboxAria(resolved, current);
      }
    });
  }
}
