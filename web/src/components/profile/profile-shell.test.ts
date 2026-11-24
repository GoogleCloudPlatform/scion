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
 * Tests for scion-profile-shell title handling on `/profile/templates/:id`
 * and `scion:page-title` refinement, plus profile-nav landmark labeling
 * (ptone/scion#8281, ptone/scion#9026).
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import { dispatchPageTitle } from '../../client/page-title.js';
import type { ScionProfileShell } from './profile-shell.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('scion-profile-shell template detail support', () => {
  let shell: ScionProfileShell | null = null;

  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({ configured: false, projects: [] })))
    );
    await import('./profile-shell.js');
  });

  afterEach(() => {
    shell?.remove();
    shell = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  async function mount(path: string): Promise<ScionProfileShell> {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({ configured: false, projects: [] })))
    );
    const el = document.createElement('scion-profile-shell');
    el.currentPath = path;
    document.body.appendChild(el);
    await el.updateComplete;
    shell = el;
    return el;
  }

  it('sets header pageTitle to Template on /profile/templates/:id and Templates on /profile/templates', async () => {
    const el = await mount('/profile/templates/tpl-1');
    const header = el.shadowRoot!.querySelector('scion-header') as HTMLElement & {
      pageTitle: string;
    };
    expect(header.pageTitle).toBe('Template');
    expect(document.title).toBe('Template — Profile — Scion');

    el.currentPath = '/profile/templates';
    await el.updateComplete;
    expect(header.pageTitle).toBe('Templates');
    expect(document.title).toBe('Templates — Profile — Scion');
  });

  it('refines document.title when a slotted page dispatches scion:page-title', async () => {
    const el = await mount('/profile/templates/tpl-1');
    const child = document.createElement('div');
    el.appendChild(child);

    dispatchPageTitle(child, 'my-custom-template', 'Templates');
    expect(document.title).toBe('my-custom-template — Templates — Scion');
  });

  it('labels profile-nav landmark and marks Templates active on /profile/templates/:id', async () => {
    const el = await mount('/profile/templates/tpl-1');
    const nav = el.shadowRoot!.querySelector('scion-profile-nav') as HTMLElement & {
      updateComplete: Promise<boolean>;
    };
    await nav.updateComplete;

    const navEl = nav.shadowRoot!.querySelector('nav.nav-container');
    expect(navEl?.getAttribute('aria-label')).toBe('Profile');
    const templatesLink = nav.shadowRoot!.querySelector('a[href="/profile/templates"]');
    expect(templatesLink?.classList.contains('active')).toBe(true);
  });
});
