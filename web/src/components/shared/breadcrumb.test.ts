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

import { describe, it, expect, beforeAll, afterEach, vi } from 'vitest';

import './breadcrumb.js';
import type { Crumb, ScionBreadcrumb } from './breadcrumb.js';
import { elementStyleRules } from '../pages/__fixtures__/css-rules.js';

let rules: Map<string, string>;
let rawCss: string;

beforeAll(() => {
  rules = elementStyleRules('scion-breadcrumb');
  const ctor = customElements.get('scion-breadcrumb') as unknown as {
    styles: { cssText: string };
  };
  rawCss = ctor.styles.cssText;
});

afterEach(() => {
  document.body.innerHTML = '';
});

async function mount(crumbs: readonly Crumb[]): Promise<ScionBreadcrumb> {
  const el = document.createElement('scion-breadcrumb');
  el.crumbs = crumbs;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

describe('scion-breadcrumb', () => {
  it('renders nothing when crumbs is empty', async () => {
    const el = await mount([]);
    expect(el.shadowRoot!.querySelector('nav')).toBeNull();
    expect(el.shadowRoot!.querySelector('ol')).toBeNull();
  });

  it('renders a Breadcrumb navigation landmark with an ordered list', async () => {
    const el = await mount([
      { label: 'Projects', href: '/projects' },
      { label: 'Payments', href: '/projects/p-1', truncate: true },
      { label: 'Settings' },
    ]);
    const nav = el.shadowRoot!.querySelector('nav');
    expect(nav?.getAttribute('aria-label')).toBe('Breadcrumb');
    const items = Array.from(el.shadowRoot!.querySelectorAll('nav > ol > li'));
    expect(items).toHaveLength(3);
  });

  it('renders linked crumbs as real anchors and the last unlinked crumb as span[aria-current="page"]', async () => {
    const el = await mount([{ label: 'Projects', href: '/projects' }, { label: 'Payments' }]);
    const links = Array.from(el.shadowRoot!.querySelectorAll('a'));
    expect(links).toHaveLength(1);
    expect(links[0].getAttribute('href')).toBe('/projects');
    expect(links[0].textContent?.trim()).toBe('Projects');

    const current = el.shadowRoot!.querySelector('[aria-current]');
    expect(current?.tagName).toBe('SPAN');
    expect(current?.getAttribute('aria-current')).toBe('page');
    expect(current?.getAttribute('aria-current')).not.toBe('');
    expect(current?.textContent?.trim()).toBe('Payments');
  });

  it('renders separators via CSS pseudo-element only, with no separator text nodes', async () => {
    const el = await mount([
      { label: 'Projects', href: '/projects' },
      { label: 'Payments', href: '/projects/p-1' },
      { label: 'Worker' },
    ]);
    expect(el.shadowRoot!.textContent).not.toContain('/');
    expect(rules.get('li + li::before') ?? '').toMatch(/content:\s*['"]\/['"]\s*\/\s*['"]['"]/);
  });

  it('lazily wraps truncated crumbs in sl-tooltip on hover/focus and omits tooltip otherwise', async () => {
    const el = await mount([
      { label: 'Projects', href: '/projects' },
      { label: 'Very Long Project Display Name', href: '/projects/p-1', truncate: true },
      { label: 'Settings' },
    ]);
    // No eager sl-tooltip before interaction (preserves page DOM budget).
    expect(el.shadowRoot!.querySelectorAll('sl-tooltip')).toHaveLength(0);

    // Hovering a non-truncated crumb does not mount a tooltip.
    el.shadowRoot!.querySelector('a[href="/projects"]')!.dispatchEvent(new Event('pointerenter'));
    await el.updateComplete;
    expect(el.shadowRoot!.querySelectorAll('sl-tooltip')).toHaveLength(0);

    // Hovering the truncated crumb lazily mounts sl-tooltip with the full label.
    const truncLink = el.shadowRoot!.querySelector('a[href="/projects/p-1"]')!;
    expect(truncLink.classList.contains('truncate')).toBe(true);
    truncLink.dispatchEvent(new Event('pointerenter'));
    await el.updateComplete;

    const tooltips = Array.from(el.shadowRoot!.querySelectorAll('sl-tooltip'));
    expect(tooltips).toHaveLength(1);
    expect(tooltips[0].getAttribute('content')).toBe('Very Long Project Display Name');
    expect(tooltips[0].querySelector('a')?.classList.contains('truncate')).toBe(true);
  });

  it('applies the pending class when crumb.pending is true', async () => {
    const el = await mount([
      { label: 'Projects', href: '/projects' },
      { label: 'p-12345', href: '/projects/p-12345', truncate: true, pending: true },
      { label: 'Settings' },
    ]);
    const link = el.shadowRoot!.querySelector('a[href="/projects/p-12345"]');
    expect(link?.classList.contains('pending')).toBe(true);
  });

  it('invokes onActivate on plain left click and respects preventDefault, ignoring modifier clicks', async () => {
    const onActivate = vi.fn((e: MouseEvent) => e.preventDefault());
    const el = await mount([
      { label: 'Templates', href: '/settings?tab=templates' },
      { label: 'base', href: '/settings/templates/t-1', onActivate },
      { label: 'agent.md' },
    ]);
    const link = el.shadowRoot!.querySelector<HTMLAnchorElement>(
      'a[href="/settings/templates/t-1"]'
    )!;

    const plainClick = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 });
    link.dispatchEvent(plainClick);
    expect(onActivate).toHaveBeenCalledTimes(1);
    expect(plainClick.defaultPrevented).toBe(true);

    const ctrlClick = new MouseEvent('click', {
      bubbles: true,
      cancelable: true,
      button: 0,
      ctrlKey: true,
    });
    link.dispatchEvent(ctrlClick);
    expect(onActivate).toHaveBeenCalledTimes(1);
    expect(ctrlClick.defaultPrevented).toBe(false);

    const middleClick = new MouseEvent('click', { bubbles: true, cancelable: true, button: 1 });
    link.dispatchEvent(middleClick);
    expect(onActivate).toHaveBeenCalledTimes(1);
  });

  it('renders the ellipsis button only with 4+ crumbs and expands on click', async () => {
    const three = await mount([
      { label: 'Projects', href: '/projects' },
      { label: 'Payments', href: '/projects/p-1' },
      { label: 'Worker' },
    ]);
    expect(three.shadowRoot!.querySelector('button.ellipsis')).toBeNull();

    const five = await mount([
      { label: 'Projects', href: '/projects' },
      { label: 'Payments', href: '/projects/p-1', truncate: true },
      { label: 'Settings', href: '/projects/p-1/settings' },
      { label: 'Templates', href: '/projects/p-1/settings?tab=templates' },
      { label: 'base-template', truncate: true },
    ]);
    const btn = five.shadowRoot!.querySelector<HTMLButtonElement>('button.ellipsis');
    expect(btn).not.toBeNull();
    expect(btn!.getAttribute('aria-label')).toBe('Show full path');
    expect(btn!.getAttribute('aria-expanded')).toBe('false');

    const middleItems = Array.from(five.shadowRoot!.querySelectorAll('li.middle'));
    expect(middleItems.map((li) => li.textContent?.trim())).toEqual(['Payments', 'Settings']);

    btn!.click();
    await five.updateComplete;
    expect(btn!.getAttribute('aria-expanded')).toBe('true');
    expect(five.shadowRoot!.querySelector('ol')?.classList.contains('expanded')).toBe(true);
  });

  it('defines the 24ch truncation cap, 36rem container query, and 1rem host bottom margin', () => {
    const host = rules.get(':host') ?? '';
    expect(host).toMatch(/margin-bottom:\s*1rem/);
    expect(host).toMatch(/container-type:\s*inline-size/);
    const trunc = rules.get('.truncate') ?? '';
    expect(trunc).toMatch(/max-width:\s*24ch/);
    expect(trunc).toMatch(/text-overflow:\s*ellipsis/);
    expect(rawCss).toMatch(/@container\s*\(max-width:\s*36rem\)/);
  });
});
