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

import { describe, it, expect, afterEach } from 'vitest';
import {
  syncSlInputComboboxAria,
  type AriaComboboxInputElement,
} from './sl-input-combobox-aria.js';

afterEach(() => {
  document.body.innerHTML = '';
});

function createMockSlInputWithInner(): {
  host: HTMLElement;
  inner: AriaComboboxInputElement;
  listbox: HTMLDivElement;
  option: HTMLDivElement;
} {
  const picker = document.createElement('div');
  const pickerShadow = picker.attachShadow({ mode: 'open' });

  const host = document.createElement('sl-input');
  const shadow = host.attachShadow({ mode: 'open' });
  const inner = document.createElement('input') as AriaComboboxInputElement;
  shadow.appendChild(inner);

  const listbox = document.createElement('div');
  listbox.id = 'my-listbox';
  listbox.setAttribute('role', 'listbox');
  const option = document.createElement('div');
  option.id = 'my-option-2';
  option.setAttribute('role', 'option');
  listbox.appendChild(option);

  pickerShadow.appendChild(host);
  pickerShadow.appendChild(listbox);
  document.body.appendChild(picker);
  return { host, inner, listbox, option };
}

describe('syncSlInputComboboxAria', () => {
  it('forwards combobox ARIA state and element-reflected relations onto the shadow-DOM inner <input>', () => {
    const { host, inner, listbox, option } = createMockSlInputWithInner();
    // Pre-seed stale string attributes to confirm they are cleared.
    inner.setAttribute('aria-controls', 'stale-listbox');
    inner.setAttribute('aria-activedescendant', 'stale-option');

    syncSlInputComboboxAria(host, {
      expanded: false,
      controls: null,
      activeDescendant: null,
    });

    expect(host.hasAttribute('aria-expanded')).toBe(false);
    expect(host.hasAttribute('role')).toBe(false);
    expect(inner.getAttribute('role')).toBe('combobox');
    expect(inner.getAttribute('aria-autocomplete')).toBe('list');
    expect(inner.getAttribute('aria-expanded')).toBe('false');
    expect(inner.hasAttribute('aria-controls')).toBe(false);
    expect(inner.hasAttribute('aria-activedescendant')).toBe(false);
    expect(inner.ariaControlsElements).toBeNull();
    expect(inner.ariaActiveDescendantElement).toBeNull();

    syncSlInputComboboxAria(host, {
      expanded: true,
      controls: listbox,
      activeDescendant: option,
    });

    expect(inner.getAttribute('aria-expanded')).toBe('true');
    expect(inner.hasAttribute('aria-controls')).toBe(false);
    expect(inner.hasAttribute('aria-activedescendant')).toBe(false);
    expect(inner.ariaControlsElements).toEqual([listbox]);
    expect(inner.ariaActiveDescendantElement).toBe(option);

    syncSlInputComboboxAria(host, {
      expanded: false,
      controls: null,
      activeDescendant: null,
    });

    expect(inner.getAttribute('aria-expanded')).toBe('false');
    expect(inner.ariaControlsElements).toBeNull();
    expect(inner.ariaActiveDescendantElement).toBeNull();
  });

  it('applies the latest state once updateComplete resolves on initial render', async () => {
    const host = document.createElement('sl-input');
    const shadow = host.attachShadow({ mode: 'open' });
    let resolveUpdate!: () => void;
    const updateComplete = new Promise<void>((resolve) => {
      resolveUpdate = resolve;
    });
    Object.defineProperty(host, 'updateComplete', { value: updateComplete });
    document.body.appendChild(host);

    const listbox = document.createElement('div');
    listbox.id = 'late-listbox';
    const option = document.createElement('div');
    option.id = 'late-opt-0';
    listbox.appendChild(option);
    document.body.appendChild(listbox);

    // First sync before inner <input> exists (collapsed), then updated to expanded
    // before updateComplete settles — latest state must win.
    syncSlInputComboboxAria(host, { expanded: false, controls: null, activeDescendant: null });
    syncSlInputComboboxAria(host, {
      expanded: true,
      controls: listbox,
      activeDescendant: option,
    });

    const inner = document.createElement('input') as AriaComboboxInputElement;
    shadow.appendChild(inner);
    resolveUpdate();
    await updateComplete;
    await Promise.resolve();

    expect(inner.getAttribute('role')).toBe('combobox');
    expect(inner.getAttribute('aria-expanded')).toBe('true');
    expect(inner.hasAttribute('aria-controls')).toBe(false);
    expect(inner.hasAttribute('aria-activedescendant')).toBe(false);
    expect(inner.ariaControlsElements).toEqual([listbox]);
    expect(inner.ariaActiveDescendantElement).toBe(option);
  });

  it('safely no-ops when sl-input is null or un-upgraded without shadowRoot', () => {
    expect(() => syncSlInputComboboxAria(null, { expanded: false })).not.toThrow();

    const unupgraded = document.createElement('sl-input');
    const listbox = document.createElement('div');
    expect(() =>
      syncSlInputComboboxAria(unupgraded, { expanded: true, controls: listbox })
    ).not.toThrow();
    expect(unupgraded.hasAttribute('aria-expanded')).toBe(false);
  });
});
