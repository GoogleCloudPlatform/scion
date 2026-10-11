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

/** Shared back link for detail pages (ptone/scion#4177). */

import { describe, it, expect, beforeAll, afterEach } from 'vitest';

import './back-link.js';
import type { ScionBackLink } from './back-link.js';
import { elementStyleRules } from '../pages/__fixtures__/css-rules.js';

let rules: Map<string, string>;

beforeAll(() => {
  rules = elementStyleRules('scion-back-link');
});

afterEach(() => {
  document.body.innerHTML = '';
});

async function mount(href: string, label: string): Promise<ScionBackLink> {
  const el = document.createElement('scion-back-link');
  el.href = href;
  el.textContent = label;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

describe('scion-back-link', () => {
  it('renders a real anchor to the given href', async () => {
    const el = await mount('/brokers', 'Back to Brokers');
    const a = el.shadowRoot!.querySelector('a');
    expect(a).not.toBeNull();
    expect(a!.getAttribute('href')).toBe('/brokers');
  });

  it('names the link by its label, with a decorative arrow', async () => {
    const el = await mount('/brokers', 'Back to Brokers');
    const a = el.shadowRoot!.querySelector('a')!;
    // The label is slotted into the anchor, so it is the anchor's name.
    const slot = a.querySelector('slot:not([name])') as HTMLSlotElement;
    expect(slot).not.toBeNull();
    expect(el.textContent).toBe('Back to Brokers');
    expect(a.hasAttribute('aria-label')).toBe(false);
    const icon = a.querySelector('sl-icon')!;
    expect(icon.getAttribute('name')).toBe('arrow-left');
    expect(icon.getAttribute('aria-hidden')).toBe('true');
  });

  it('follows a changed href', async () => {
    const el = await mount('/skills', 'Back to Skills');
    el.href = '/projects/p-1';
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('a')!.getAttribute('href')).toBe('/projects/p-1');
  });

  it('uses muted text that turns primary on hover', () => {
    const a = rules.get('a') ?? '';
    expect(a).toMatch(/color:\s*var\(--scion-text-muted/);
    expect(a).toMatch(/text-decoration:\s*none/);
    expect(a).toMatch(/font-size:\s*0\.875rem/);
    expect(a).toMatch(/gap:\s*0\.5rem/);
    expect(rules.get('a:hover') ?? '').toMatch(/color:\s*var\(--scion-primary/);
  });

  it('keeps 1rem of space below the link', () => {
    expect(rules.get(':host') ?? '').toMatch(/margin-bottom:\s*1rem/);
  });
});
