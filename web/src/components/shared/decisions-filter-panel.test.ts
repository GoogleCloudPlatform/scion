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

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  DECISIONS_FILTERS_OPEN_STORAGE_KEY,
  type DecisionsFilterPatch,
  type ScionDecisionsFilterPanel,
} from './decisions-filter-panel.js';
import './decisions-filter-panel.js';

async function mountPanel(
  props: Partial<ScionDecisionsFilterPanel> = {}
): Promise<ScionDecisionsFilterPanel> {
  const el = document.createElement('scion-decisions-filter-panel');
  Object.assign(el, props);
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

describe('<scion-decisions-filter-panel> (D13)', () => {
  beforeEach(() => {
    window.localStorage.removeItem(DECISIONS_FILTERS_OPEN_STORAGE_KEY);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    window.localStorage.removeItem(DECISIONS_FILTERS_OPEN_STORAGE_KEY);
  });

  it('starts collapsed by default and persists open/closed preference in localStorage', async () => {
    const el = await mountPanel({ tab: 'pending' });
    const root = el.shadowRoot!;
    const toggle = root.querySelector<HTMLButtonElement>('[data-filter-panel-toggle]')!;
    const body = root.querySelector<HTMLElement>('#decisions-filter-body')!;

    expect(el.isOpen).toBe(false);
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(body.hidden).toBe(true);
    expect(root.querySelector('.kbd')?.textContent).toBe('/');

    // Click toggle -> expands and writes localStorage
    toggle.click();
    await el.updateComplete;

    expect(el.isOpen).toBe(true);
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect(body.hidden).toBe(false);
    expect(window.localStorage.getItem(DECISIONS_FILTERS_OPEN_STORAGE_KEY)).toBe('true');

    // Remount a new instance -> remembers open state from localStorage
    el.remove();
    const el2 = await mountPanel({ tab: 'made' });
    expect(el2.isOpen).toBe(true);
    expect(el2.shadowRoot!.querySelector<HTMLElement>('#decisions-filter-body')!.hidden).toBe(
      false
    );

    // Collapse -> writes false
    el2.shadowRoot!.querySelector<HTMLButtonElement>('[data-filter-panel-toggle]')!.click();
    await el2.updateComplete;
    expect(el2.isOpen).toBe(false);
    expect(window.localStorage.getItem(DECISIONS_FILTERS_OPEN_STORAGE_KEY)).toBe('false');
  });

  it('renders removable tags (including quoted search text), active count, and Clear without toggling open state', async () => {
    const el = await mountPanel({
      tab: 'made',
      q: 'backfill',
      conversation: 't-91',
      agent: 'a-rel',
      user: 'u-me',
      provenance: 'overridden',
      from: '2026-11-01',
      to: '2026-11-14',
      currentUserId: 'u-me',
      conversations: [{ key: 't-91', label: '#release', projectId: 'p-1' }],
      agents: [{ id: 'a-rel', name: 'release-bot' }],
      answerers: [{ id: 'u-me', name: 'Alice' }],
    });
    const root = el.shadowRoot!;
    const patches: DecisionsFilterPatch[] = [];
    let cleared = 0;
    el.addEventListener('decisions-filter-change', (e) => {
      patches.push((e as CustomEvent<DecisionsFilterPatch>).detail);
    });
    el.addEventListener('decisions-filters-clear', () => {
      cleared++;
    });

    const tags = Array.from(root.querySelectorAll<HTMLElement>('sl-tag[data-filter-tag]'));
    expect(tags.map((t) => t.textContent?.trim())).toEqual([
      '"backfill"',
      'Conversation: #release',
      'Agent: release-bot',
      'Answered by: you',
      'Provenance: changed',
      'From: 2026-11-01',
      'To: 2026-11-14',
    ]);
    expect(root.querySelector('[data-active-filter-count]')?.textContent).toBe('7 active');

    // Removing a tag emits patch and keeps panel collapsed
    tags[0].dispatchEvent(new CustomEvent('sl-remove', { bubbles: true }));
    expect(patches).toEqual([{ q: '' }]);
    expect(el.isOpen).toBe(false);

    // Clicking Clear emits clear and keeps panel collapsed
    root.querySelector<HTMLElement>('[data-clear-filters]')!.click();
    expect(cleared).toBe(1);
    expect(el.isOpen).toBe(false);

    // Switching tab to pending shows only shared tags & controls (no Made-only controls)
    el.tab = 'pending';
    await el.updateComplete;
    const pendingTags = Array.from(root.querySelectorAll<HTMLElement>('sl-tag[data-filter-tag]'));
    expect(pendingTags.map((t) => t.textContent?.trim())).toEqual([
      '"backfill"',
      'Conversation: #release',
      'Agent: release-bot',
    ]);
    expect(root.querySelector('[data-filter="user"]')).toBeNull();
    expect(root.querySelector('[data-filter="provenance"]')).toBeNull();
    expect(root.querySelector('[data-filter="from"]')).toBeNull();
  });

  it('expandAndFocusSearch() opens the panel and focuses the search input', async () => {
    const el = await mountPanel({ tab: 'pending' });
    const slInput = el.shadowRoot!.querySelector<HTMLElement & { focus: () => void }>(
      'sl-input[data-filter="q"]'
    )!;
    const focusSpy = vi.fn();
    slInput.focus = focusSpy;

    expect(el.isOpen).toBe(false);
    await el.expandAndFocusSearch();
    expect(el.isOpen).toBe(true);
    expect(focusSpy).toHaveBeenCalledTimes(1);
    expect(window.localStorage.getItem(DECISIONS_FILTERS_OPEN_STORAGE_KEY)).toBe('true');
  });
});
