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
  rankFacetOptions,
  type FacetChangeDetail,
  type FacetComboboxOption,
  type ScionFacetCombobox,
} from './facet-combobox.js';
import './facet-combobox.js';

const AGENT_OPTIONS: FacetComboboxOption[] = [
  { value: 'a-deploy', label: 'deploy-helper' },
  { value: 'a-rel-live', label: 'release-bot' },
  { value: 'a-rel-old', label: 'release-bot', hint: 'deleted · 3f9c' },
  { value: 'a-bot', label: 'bot' },
];

async function mountCombobox(
  props: Partial<Pick<ScionFacetCombobox, 'label' | 'placeholder' | 'options' | 'value'>> = {}
): Promise<ScionFacetCombobox> {
  const el = document.createElement('scion-facet-combobox');
  el.label = props.label ?? 'Agent';
  if (props.placeholder !== undefined) el.placeholder = props.placeholder;
  el.options = props.options ?? AGENT_OPTIONS;
  if (props.value !== undefined) el.value = props.value;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

describe('rankFacetOptions (palette-match tiers & highlight ranges)', () => {
  it('returns all options in original order with empty highlights for an empty query', () => {
    const ranked = rankFacetOptions('   ', AGENT_OPTIONS);
    expect(ranked.map((r) => r.option.value)).toEqual([
      'a-deploy',
      'a-rel-live',
      'a-rel-old',
      'a-bot',
    ]);
    expect(ranked.every((r) => r.labelRanges.length === 0)).toBe(true);
  });

  it('fuzzy-matches rlsbot -> release-bot with exact character highlight ranges (AC 6a)', () => {
    const ranked = rankFacetOptions('rlsbot', AGENT_OPTIONS);
    expect(ranked.map((r) => r.option.value)).toEqual(['a-rel-live', 'a-rel-old']);
    expect(ranked[0]?.tier).toBe(3);
    // 'release-bot' -> 'r'(0..1), 'l'(2..3), 's'(5..6), 'bot'(8..11)
    expect(ranked[0]?.labelRanges).toEqual([
      { start: 0, end: 1 },
      { start: 2, end: 3 },
      { start: 5, end: 6 },
      { start: 8, end: 11 },
    ]);
  });

  it('orders exact < prefix < substring < subsequence tiers and supports hint disambiguation', () => {
    const ranked = rankFacetOptions('bot', AGENT_OPTIONS);
    // 'bot' is exact (tier 0), 'release-bot' is substring (tier 2)
    expect(ranked.map((r) => r.option.value)).toEqual(['a-bot', 'a-rel-live', 'a-rel-old']);
    expect(ranked[0]?.tier).toBe(0);
    expect(ranked[1]?.tier).toBe(2);

    const byHint = rankFacetOptions('3f9c', AGENT_OPTIONS);
    expect(byHint.map((r) => r.option.value)).toEqual(['a-rel-old']);
    expect(byHint[0]?.hintRanges).toEqual([{ start: 10, end: 14 }]);
  });
});

describe('<scion-facet-combobox>', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('exposes ARIA combobox/listbox/option attributes and supports picking without typing', async () => {
    const el = await mountCombobox();
    const events: FacetChangeDetail[] = [];
    el.addEventListener('facet-change', (e) => {
      events.push((e as CustomEvent<FacetChangeDetail>).detail);
    });

    const input = el.shadowRoot!.querySelector<HTMLInputElement>('input[role="combobox"]')!;
    expect(input).not.toBeNull();
    expect(input.getAttribute('aria-expanded')).toBe('false');
    expect(input.hasAttribute('aria-controls')).toBe(false);
    expect(el.shadowRoot!.querySelector('[role="listbox"]')).toBeNull();

    // Open dropdown without typing
    input.dispatchEvent(new FocusEvent('focus'));
    await el.updateComplete;

    expect(input.getAttribute('aria-expanded')).toBe('true');
    const listbox = el.shadowRoot!.querySelector<HTMLElement>('[role="listbox"]')!;
    expect(listbox).not.toBeNull();
    expect(input.getAttribute('aria-controls')).toBe(listbox.id);

    const options = Array.from(listbox.querySelectorAll<HTMLElement>('[role="option"]'));
    expect(options).toHaveLength(4);
    expect(options[2]?.querySelector('.opt-hint')?.textContent).toBe('deleted · 3f9c');

    // Pick option via mousedown without typing
    options[1]!.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true }));
    await el.updateComplete;

    expect(el.value).toBe('a-rel-live');
    expect(input.value).toBe('release-bot');
    expect(input.getAttribute('aria-expanded')).toBe('false');
    expect(input.hasAttribute('aria-controls')).toBe(false);
    expect(events).toEqual([{ value: 'a-rel-live', label: 'release-bot' }]);
  });

  it('highlights fuzzy matches with <mark> and navigates/commits/clears with keyboard and Clear button', async () => {
    const el = await mountCombobox();
    const events: FacetChangeDetail[] = [];
    el.addEventListener('facet-change', (e) => {
      events.push((e as CustomEvent<FacetChangeDetail>).detail);
    });

    const input = el.shadowRoot!.querySelector<HTMLInputElement>('input[role="combobox"]')!;
    input.dispatchEvent(new FocusEvent('focus'));
    input.value = 'rlsbot';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await el.updateComplete;

    const options = Array.from(el.shadowRoot!.querySelectorAll<HTMLElement>('[role="option"]'));
    expect(options).toHaveLength(2);
    const marks = Array.from(options[0]!.querySelectorAll('mark')).map((m) => m.textContent);
    expect(marks).toEqual(['r', 'l', 's', 'bot']);

    // Active descendant defaults to first fuzzy match; ArrowDown moves to second match
    expect(input.getAttribute('aria-activedescendant')).toBe(options[0]!.id);
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;
    expect(input.getAttribute('aria-activedescendant')).toBe(options[1]!.id);

    // ArrowUp moves back to first match; Enter commits it
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true }));
    await el.updateComplete;
    expect(input.getAttribute('aria-activedescendant')).toBe(options[0]!.id);

    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await el.updateComplete;

    expect(el.value).toBe('a-rel-live');
    expect(input.value).toBe('release-bot');
    expect(events).toEqual([{ value: 'a-rel-live', label: 'release-bot' }]);

    // Escape when a filter is committed (and not mid-typing a draft) removes the filter (D14)
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await el.updateComplete;

    expect(el.value).toBe('');
    expect(input.value).toBe('');
    expect(events).toEqual([
      { value: 'a-rel-live', label: 'release-bot' },
      { value: '', label: '' },
    ]);

    // Re-select via ArrowDown + Enter, then clear via the Clear button
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await el.updateComplete;
    expect(el.value).toBe('a-deploy');

    const clearBtn = el.shadowRoot!.querySelector<HTMLButtonElement>('[data-combobox-clear]')!;
    expect(clearBtn).not.toBeNull();
    clearBtn.click();
    await el.updateComplete;

    expect(el.value).toBe('');
    expect(input.value).toBe('');
    expect(events[events.length - 1]).toEqual({ value: '', label: '' });
  });
});
