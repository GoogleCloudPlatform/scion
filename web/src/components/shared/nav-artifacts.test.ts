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
 * The management sidebar shows Artifacts, right after Skills, only while
 * the hub.artifacts experiment is on.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import type { ScionNav } from './nav.js';

async function mount(flag: boolean | undefined): Promise<ScionNav> {
  if (flag !== undefined) {
    window.__SCION_FEATURES__ = { 'hub.artifacts': flag };
  }
  const el = document.createElement('scion-nav') as ScionNav;
  el.currentPath = '/artifacts';
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function managementLinks(el: ScionNav): string[] {
  const sections = Array.from(el.shadowRoot!.querySelectorAll('.nav-section'));
  const management = sections.find(
    (s) => s.querySelector('.nav-section-title')?.textContent === 'Management'
  )!;
  return Array.from(management.querySelectorAll('a.nav-link')).map((a) => a.getAttribute('href')!);
}

describe('sidebar Artifacts item', () => {
  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('{}', { status: 404 })))
    );
    await import('./nav.js');
  }, 30_000);

  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
    localStorage.clear();
  });

  it('is present, after Skills, and active on /artifacts when the experiment is on', async () => {
    const el = await mount(true);
    const links = managementLinks(el);
    expect(links).toContain('/artifacts');
    expect(links.indexOf('/artifacts')).toBe(links.indexOf('/skills') + 1);
    const link = el.shadowRoot!.querySelector('a[href="/artifacts"]')!;
    expect(link.classList.contains('active')).toBe(true);
    expect(link.querySelector('sl-icon')!.getAttribute('name')).toBe('file-earmark-richtext');
    expect(link.textContent).toContain('Artifacts');
  });

  it('is absent when the experiment is off', async () => {
    const el = await mount(false);
    expect(managementLinks(el)).not.toContain('/artifacts');
    expect(managementLinks(el)).toContain('/skills');
  });

  it('is absent by default (experiment unset)', async () => {
    const el = await mount(undefined);
    expect(managementLinks(el)).not.toContain('/artifacts');
  });
});
