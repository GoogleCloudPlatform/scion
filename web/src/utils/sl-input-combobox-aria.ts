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
 *
 * `controls` and `activeDescendant` take DOM element references (not ID strings)
 * because the listbox and options live in the picker's shadow tree while the
 * inner `<input>` lives inside `<sl-input>`'s shadow root; IDREF strings cannot
 * resolve across shadow boundaries, whereas ARIA element reflection
 * (`ariaControlsElements` / `ariaActiveDescendantElement`) can reference
 * elements in a shadow-including ancestor's tree.
 */
export interface SlInputComboboxAriaState {
  expanded: boolean;
  controls?: Element | null | undefined;
  activeDescendant?: Element | null | undefined;
}

/**
 * `HTMLInputElement` augmented with the ARIA element-reflection properties
 * (`ariaControlsElements` / `ariaActiveDescendantElement`), which are not yet
 * declared in TypeScript 5.3's `lib.dom.d.ts`.
 */
export interface AriaComboboxInputElement extends HTMLInputElement {
  ariaControlsElements: Element[] | null;
  ariaActiveDescendantElement: Element | null;
}

const latestState = new WeakMap<Element, SlInputComboboxAriaState>();
const pendingSync = new WeakSet<Element>();

function applyComboboxAria(input: HTMLInputElement, state: SlInputComboboxAriaState): void {
  input.setAttribute('role', 'combobox');
  input.setAttribute('aria-autocomplete', 'list');
  input.setAttribute('aria-expanded', state.expanded ? 'true' : 'false');

  // Remove any string IDREF attributes before applying element reflection so
  // removing a content attribute cannot clear the reflected element references.
  if (input.hasAttribute('aria-controls')) {
    input.removeAttribute('aria-controls');
  }
  if (input.hasAttribute('aria-activedescendant')) {
    input.removeAttribute('aria-activedescendant');
  }

  const ariaInput = input as AriaComboboxInputElement;
  ariaInput.ariaControlsElements = state.controls ? [state.controls] : null;
  ariaInput.ariaActiveDescendantElement = state.activeDescendant ?? null;
}

/**
 * Forward combobox ARIA state onto a Shoelace `<sl-input>`'s shadow-DOM
 * `<input>` element.
 *
 * Shoelace does not forward host `role` or `aria-expanded` attributes to its
 * inner `<input>`, so placing them on `<sl-input>` leaves assistive technology
 * without combobox state. Listbox (`controls`) and option (`activeDescendant`)
 * relations are set via ARIA element reflection (`ariaControlsElements` and
 * `ariaActiveDescendantElement`) so they resolve across `<sl-input>`'s shadow
 * root boundary. If the `<sl-input>` has not finished its initial Lit render
 * yet, the latest state is applied once `updateComplete` resolves.
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
