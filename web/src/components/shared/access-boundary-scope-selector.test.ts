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

import { describe, it, expect, beforeAll, afterEach } from 'vitest';
import type { AriaComboboxInputElement } from '../../utils/sl-input-combobox-aria.js';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let ScopeSelectorCtor: any;

beforeAll(async () => {
  const mod = await import('./access-boundary-scope-selector.js');
  ScopeSelectorCtor = mod.ScionAccessBoundaryScopeSelector;
});

afterEach(() => {
  document.body.innerHTML = '';
});

async function createElement(): Promise<InstanceType<typeof ScopeSelectorCtor>> {
  const el = new ScopeSelectorCtor();
  el.scopeType = 'project';
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

describe('scion-access-boundary-scope-selector', () => {
  it('forwards combobox ARIA state to the inner <input> instead of the <sl-input> host', async () => {
    const el = await createElement();
    const slInput = el.shadowRoot?.querySelector('sl-input');
    expect(slInput).not.toBeNull();
    expect(slInput?.hasAttribute('aria-expanded')).toBe(false);
    expect(slInput?.hasAttribute('role')).toBe(false);

    // Attach a shadow-DOM <input> matching Shoelace's upgraded <sl-input>
    // structure and verify collapsed and expanded states.
    const inner = document.createElement('input') as AriaComboboxInputElement;
    slInput!.attachShadow({ mode: 'open' }).appendChild(inner);
    el.requestUpdate();
    await el.updateComplete;

    expect(inner.getAttribute('role')).toBe('combobox');
    expect(inner.getAttribute('aria-expanded')).toBe('false');
    expect(inner.hasAttribute('aria-controls')).toBe(false);
    expect(inner.hasAttribute('aria-activedescendant')).toBe(false);
    expect(inner.ariaControlsElements).toBeNull();
    expect(inner.ariaActiveDescendantElement).toBeNull();

    (el as Record<string, unknown>)['projectSearchResults'] = [
      { id: 'proj-1', name: 'Alpha Project', slug: 'alpha-project' },
    ];
    (el as Record<string, unknown>)['projectSearchOpen'] = true;
    (el as Record<string, unknown>)['activeDescendantIndex'] = 0;
    el.requestUpdate();
    await el.updateComplete;

    const listbox = el.shadowRoot?.getElementById('project-search-listbox');
    const option0 = el.shadowRoot?.getElementById('project-option-0');
    expect(listbox).not.toBeNull();
    expect(option0).not.toBeNull();
    expect(slInput?.hasAttribute('aria-expanded')).toBe(false);
    expect(inner.getAttribute('aria-expanded')).toBe('true');
    expect(inner.hasAttribute('aria-controls')).toBe(false);
    expect(inner.hasAttribute('aria-activedescendant')).toBe(false);
    expect(inner.ariaControlsElements).toEqual([listbox]);
    expect(inner.ariaActiveDescendantElement).toBe(option0);

    (el as Record<string, unknown>)['projectSearchOpen'] = false;
    (el as Record<string, unknown>)['activeDescendantIndex'] = -1;
    el.requestUpdate();
    await el.updateComplete;

    expect(inner.getAttribute('aria-expanded')).toBe('false');
    expect(inner.ariaControlsElements).toBeNull();
    expect(inner.ariaActiveDescendantElement).toBeNull();
  });
});
