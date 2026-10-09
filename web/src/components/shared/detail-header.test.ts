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
 * Shared detail page header (ptone/scion#3856). The DOM test environment
 * does no layout, so these tests check the compiled rules that make the
 * header wrap, and the shadow structure those rules rely on.
 */

import { describe, it, expect, beforeAll, afterEach } from 'vitest';

import './detail-header.js';
import type { ScionDetailHeader } from './detail-header.js';
import { elementStyleRules } from '../pages/__fixtures__/css-rules.js';

const LONG_NAME =
  'broker-01.a-very-long-unbroken-hostname-that-cannot-fit-beside-its-badges.internal';

let rules: Map<string, string>;

beforeAll(() => {
  rules = elementStyleRules('scion-detail-header');
});

afterEach(() => {
  document.body.innerHTML = '';
});

async function mount(): Promise<ScionDetailHeader> {
  const el = document.createElement('scion-detail-header');
  el.heading = LONG_NAME;
  el.innerHTML = `
    <sl-icon slot="icon" name="hdd-rack"></sl-icon>
    <span class="badge-a">Hosted</span>
    <span class="badge-b">online</span>
    <div slot="meta" class="meta">v1.2.3</div>
    <div slot="actions" class="actions"><button>Unregister</button></div>
  `;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

const slot = (el: ScionDetailHeader, name = ''): HTMLSlotElement =>
  el.shadowRoot!.querySelector(name ? `slot[name="${name}"]` : 'slot:not([name])')!;

const assigned = (s: HTMLSlotElement): string[] =>
  s.assignedElements().map((n) => n.className || n.tagName.toLowerCase());

describe('scion-detail-header wrapping rules', () => {
  it('breaks a long name inside the h1 instead of overflowing', () => {
    const h1 = rules.get('h1') ?? '';
    expect(h1).toMatch(/(^|;)\s*min-width:\s*0/);
    expect(h1).toMatch(/overflow-wrap:\s*anywhere/);
  });

  it('moves the badges onto the next line under a long name', () => {
    const text = rules.get('.header-title-text') ?? '';
    expect(text).toMatch(/display:\s*flex/);
    expect(text).toMatch(/flex-wrap:\s*wrap/);
    expect(text).toMatch(/(^|;)\s*min-width:\s*0/);
  });

  it('keeps the icon at its size beside a wrapping name', () => {
    expect(rules.get('.header-title') ?? '').toMatch(/display:\s*flex/);
    expect(rules.get("::slotted([slot='icon'])") ?? '').toMatch(/flex-shrink:\s*0/);
  });

  it('drops the actions below the title when the row is too narrow', () => {
    expect(rules.get('.header') ?? '').toMatch(/flex-wrap:\s*wrap/);
    const info = rules.get('.header-info') ?? '';
    expect(info).toMatch(/flex:\s*1 1 16rem/);
    expect(info).toMatch(/(^|;)\s*min-width:\s*0/);
    // The buttons wrap among themselves on a phone.
    expect(rules.get("::slotted([slot='actions'])") ?? '').toMatch(/flex-wrap:\s*wrap/);
  });
});

describe('scion-detail-header structure', () => {
  it('renders the heading as the h1 at the start of the wrapping row', async () => {
    const el = await mount();
    const text = el.shadowRoot!.querySelector('.header-title-text')!;
    const h1 = text.firstElementChild!;
    expect(h1.tagName).toBe('H1');
    expect(h1.textContent).toBe(LONG_NAME);
    expect(el.shadowRoot!.querySelectorAll('h1')).toHaveLength(1);
  });

  it('puts unslotted children in the wrapping row after the name', async () => {
    const el = await mount();
    const badges = slot(el);
    expect(badges.parentElement!.className).toBe('header-title-text');
    expect(badges.previousElementSibling!.tagName).toBe('H1');
    expect(assigned(badges)).toEqual(['badge-a', 'badge-b']);
  });

  it('puts the icon beside the wrapping row, not inside it', async () => {
    const el = await mount();
    const title = el.shadowRoot!.querySelector('.header-title')!;
    expect(
      Array.from(title.children).map((n) => n.tagName.toLowerCase() + '.' + n.className)
    ).toEqual(['slot.', 'div.header-title-text']);
    expect(assigned(slot(el, 'icon'))).toEqual(['sl-icon']);
  });

  it('puts the meta line under the title and the actions beside it', async () => {
    const el = await mount();
    const meta = slot(el, 'meta');
    expect(meta.parentElement!.className).toBe('header-info');
    expect(assigned(meta)).toEqual(['meta']);
    const actions = slot(el, 'actions');
    expect(actions.parentElement!.className).toBe('header');
    expect(assigned(actions)).toEqual(['actions']);
  });

  it('sets no inline width anywhere in its shadow tree', async () => {
    const el = await mount();
    for (const node of Array.from(el.shadowRoot!.querySelectorAll('[style]'))) {
      expect(node.getAttribute('style') ?? '').not.toMatch(/width/);
    }
  });
});
