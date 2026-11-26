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

  it('renders <scion-filter-box> by default with no queryDialect binding and supports Date range disclosure on Made', async () => {
    const el = await mountPanel({ tab: 'pending' });
    const root = el.shadowRoot!;
    const box = root.querySelector('scion-filter-box') as (HTMLElement & { view?: string }) | null;
    expect(box).not.toBeNull();
    expect(box?.view).toBe('decisions');
    expect(root.querySelector('[data-filter-panel-toggle]')).toBeNull();
    expect(root.querySelector('[data-date-range-toggle]')).toBeNull();

    el.tab = 'made';
    await el.updateComplete;
    const dateToggle = root.querySelector<HTMLButtonElement>('[data-date-range-toggle]');
    expect(dateToggle).not.toBeNull();
    expect(dateToggle?.getAttribute('aria-expanded')).toBe('false');
    dateToggle?.click();
    await el.updateComplete;
    expect(dateToggle?.getAttribute('aria-expanded')).toBe('true');
    expect(root.querySelector('[data-filter="from"]')).not.toBeNull();
    expect(root.querySelector('[data-filter="to"]')).not.toBeNull();
  });

  it('synthesizes legacy filter props into <scion-filter-box> value and shows Made-only warning on Pending', async () => {
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
    const box = root.querySelector('scion-filter-box') as HTMLElement & {
      value: string;
      externalWarning: string;
    };
    expect(box).not.toBeNull();
    expect(box.value).toBe(
      'conversation:#release agent:release-bot by:me provenance:overridden backfill'
    );
    expect(box.externalWarning).toBe('');

    // Switching to pending warns about Made-only keys in the query.
    el.tab = 'pending';
    await el.updateComplete;
    expect(box.externalWarning).toContain('Made tab only');
  });

  it('expandAndFocusSearch() focuses the <scion-filter-box> input', async () => {
    const el = await mountPanel({ tab: 'pending' });
    const box = el.shadowRoot!.querySelector('scion-filter-box') as HTMLElement & {
      focusInput: () => void;
    };
    expect(box).not.toBeNull();
    const focusSpy = vi.spyOn(box, 'focusInput');

    await el.expandAndFocusSearch();
    expect(focusSpy).toHaveBeenCalledTimes(1);
  });

  it('persists open/closed preference in localStorage when supportsSearch is false', async () => {
    const el = await mountPanel({ supportsSearch: false, tab: 'made' });
    const root = el.shadowRoot!;
    const toggle = root.querySelector<HTMLButtonElement>('[data-filter-panel-toggle]')!;
    const body = root.querySelector<HTMLElement>('#decisions-filter-body')!;

    expect(el.isOpen).toBe(false);
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(body.hidden).toBe(true);

    toggle.click();
    await el.updateComplete;
    expect(el.isOpen).toBe(true);
    expect(body.hidden).toBe(false);
    expect(window.localStorage.getItem(DECISIONS_FILTERS_OPEN_STORAGE_KEY)).toBe('true');
  });

  it('hides search, Conversation, and pending Agent when supportsSearch is false while preserving Made filters', async () => {
    const el = await mountPanel({
      supportsSearch: false,
      tab: 'pending',
      q: 'backfill',
      conversation: 't-91',
      agent: 'a-rel',
      user: 'u-me',
      provenance: 'direct',
      from: '2026-11-01',
      to: '2026-11-14',
      currentUserId: 'u-me',
      agents: [{ id: 'a-rel', name: 'release-bot' }],
      answerers: [{ id: 'u-me', name: 'Alice' }],
    });
    const root = el.shadowRoot!;

    expect(root.querySelector('.lbl')?.textContent).toBe('Filters');
    expect(root.querySelector('.kbd')).toBeNull();
    expect(root.querySelectorAll('sl-tag[data-filter-tag]')).toHaveLength(0);
    expect(root.querySelector('[data-filter="q"]')).toBeNull();
    expect(root.querySelector('[data-filter="conversation"]')).toBeNull();
    expect(root.querySelector('[data-filter="agent"]')).toBeNull();
    expect(root.querySelector('[data-no-pending-filters]')).not.toBeNull();

    // Switch to Made -> Phase 3 filters (Agent, Answered by, Provenance, From, To), tags, and Clear work
    el.tab = 'made';
    await el.updateComplete;

    expect(root.querySelector('[data-filter="q"]')).toBeNull();
    expect(root.querySelector('[data-filter="conversation"]')).toBeNull();
    expect(root.querySelector('[data-filter="agent"]')).not.toBeNull();
    expect(root.querySelector('[data-filter="user"]')).not.toBeNull();
    expect(root.querySelector('[data-filter="provenance"]')).not.toBeNull();
    expect(root.querySelector('[data-filter="from"]')).not.toBeNull();
    expect(root.querySelector('[data-filter="to"]')).not.toBeNull();

    const tags = Array.from(root.querySelectorAll<HTMLElement>('sl-tag[data-filter-tag]'));
    expect(tags.map((t) => t.textContent?.trim())).toEqual([
      'Agent: release-bot',
      'Answered by: you',
      'Provenance: direct',
      'From: 2026-11-01',
      'To: 2026-11-14',
    ]);
    expect(root.querySelector('[data-active-filter-count]')?.textContent).toBe('5 active');
    expect(root.querySelector('[data-clear-filters]')).not.toBeNull();
  });

  it('includes Dismissed in the Made Provenance select and renders Provenance: dismissed tag when supportsSearch is false (ptone/scion#8217)', async () => {
    const el = await mountPanel({
      supportsSearch: false,
      tab: 'made',
      provenance: 'dismissed',
    });
    const root = el.shadowRoot!;
    const options = Array.from(
      root.querySelectorAll<HTMLElement>('sl-select[data-filter="provenance"] sl-option')
    );
    expect(
      options.map((o) => ({ value: o.getAttribute('value'), text: o.textContent?.trim() }))
    ).toContainEqual({ value: 'dismissed', text: 'Dismissed' });

    const tags = Array.from(root.querySelectorAll<HTMLElement>('sl-tag[data-filter-tag]'));
    expect(tags.map((t) => t.textContent?.trim())).toEqual(['Provenance: dismissed']);

    const patches: DecisionsFilterPatch[] = [];
    el.addEventListener('decisions-filter-change', (e) => {
      patches.push((e as CustomEvent<DecisionsFilterPatch>).detail);
    });
    const select = root.querySelector<HTMLElement & { value?: string }>(
      'sl-select[data-filter="provenance"]'
    )!;
    select.value = 'dismissed';
    select.dispatchEvent(new Event('sl-change'));
    expect(patches).toEqual([{ provenance: 'dismissed' }]);
  });
});
