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
import { syncSlInputComboboxAria } from './sl-input-combobox-aria.js';

afterEach(() => {
  document.body.innerHTML = '';
});

function createMockSlInputWithInner(): { host: HTMLElement; inner: HTMLInputElement } {
  const host = document.createElement('sl-input');
  const shadow = host.attachShadow({ mode: 'open' });
  const inner = document.createElement('input');
  shadow.appendChild(inner);
  document.body.appendChild(host);
  return { host, inner };
}

describe('syncSlInputComboboxAria', () => {
  it('forwards combobox ARIA attributes onto the shadow-DOM inner <input>', () => {
    const { host, inner } = createMockSlInputWithInner();

    syncSlInputComboboxAria(host, {
      expanded: false,
      controls: 'my-listbox',
    });

    expect(host.hasAttribute('aria-expanded')).toBe(false);
    expect(host.hasAttribute('role')).toBe(false);
    expect(inner.getAttribute('role')).toBe('combobox');
    expect(inner.getAttribute('aria-autocomplete')).toBe('list');
    expect(inner.getAttribute('aria-expanded')).toBe('false');
    expect(inner.getAttribute('aria-controls')).toBe('my-listbox');
    expect(inner.hasAttribute('aria-activedescendant')).toBe(false);

    syncSlInputComboboxAria(host, {
      expanded: true,
      controls: 'my-listbox',
      activeDescendant: 'my-option-2',
    });

    expect(inner.getAttribute('aria-expanded')).toBe('true');
    expect(inner.getAttribute('aria-activedescendant')).toBe('my-option-2');

    syncSlInputComboboxAria(host, {
      expanded: false,
      controls: 'my-listbox',
    });

    expect(inner.getAttribute('aria-expanded')).toBe('false');
    expect(inner.hasAttribute('aria-activedescendant')).toBe(false);
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

    // First sync before inner <input> exists (collapsed), then updated to expanded
    // before updateComplete settles — latest state must win.
    syncSlInputComboboxAria(host, { expanded: false, controls: 'late-listbox' });
    syncSlInputComboboxAria(host, {
      expanded: true,
      controls: 'late-listbox',
      activeDescendant: 'late-opt-0',
    });

    const inner = document.createElement('input');
    shadow.appendChild(inner);
    resolveUpdate();
    await updateComplete;
    await Promise.resolve();

    expect(inner.getAttribute('role')).toBe('combobox');
    expect(inner.getAttribute('aria-expanded')).toBe('true');
    expect(inner.getAttribute('aria-controls')).toBe('late-listbox');
    expect(inner.getAttribute('aria-activedescendant')).toBe('late-opt-0');
  });

  it('safely no-ops when sl-input is null or un-upgraded without shadowRoot', () => {
    expect(() => syncSlInputComboboxAria(null, { expanded: false })).not.toThrow();

    const unupgraded = document.createElement('sl-input');
    expect(() =>
      syncSlInputComboboxAria(unupgraded, { expanded: true, controls: 'listbox' })
    ).not.toThrow();
    expect(unupgraded.hasAttribute('aria-expanded')).toBe(false);
  });
});
