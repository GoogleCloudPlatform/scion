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
 * Verifies D7 (ptone/scion#8281, ptone/scion#9026): `/profile/templates/:id`
 * renders inside `scion-profile-shell`, while `/projects/:id/templates/:id`
 * and `/settings/templates/:id` stay in the main `scion-app` shell.
 */

import { afterAll, describe, expect, it, vi } from 'vitest';
import { BOOT_WAIT, bootMainAt } from './__fixtures__/main-boot.js';

function defineStub(tag: string): void {
  if (!customElements.get(tag)) {
    customElements.define(tag, class extends HTMLElement {});
  }
}

vi.mock('../components/profile/profile-shell.js', () => {
  defineStub('scion-profile-shell');
  return {};
});
vi.mock('../components/profile/profile-nav.js', () => {
  defineStub('scion-profile-nav');
  return {};
});
vi.mock('../components/pages/profile-templates.js', () => {
  defineStub('scion-page-profile-templates');
  return {};
});
vi.mock('../components/pages/template-detail.js', () => {
  defineStub('scion-page-template-detail');
  return {};
});

describe('main.ts shell selection for /profile/templates/:id', () => {
  afterAll(() => {
    vi.unstubAllGlobals();
  });

  it('mounts /profile/templates/:id inside scion-profile-shell and keeps other template scopes in scion-app', async () => {
    const main = await bootMainAt('/profile/templates/tpl-1');

    await vi.waitFor(() => {
      const shell = document.querySelector('scion-profile-shell');
      expect(shell).not.toBeNull();
      expect(shell?.querySelector('scion-page-template-detail')).not.toBeNull();
    }, BOOT_WAIT);
    expect(document.querySelector('scion-app')).toBeNull();

    const profileShell = document.querySelector('scion-profile-shell');

    // Navigating to the profile templates list reuses the existing profile shell.
    main.navigateTo('/profile/templates');
    await vi.waitFor(() => {
      expect(document.querySelector('scion-page-profile-templates')).not.toBeNull();
    }, BOOT_WAIT);
    expect(document.querySelector('scion-profile-shell')).toBe(profileShell);
    expect(document.querySelector('scion-app')).toBeNull();

    // Project-scoped template detail switches to the main app shell.
    main.navigateTo('/projects/p-1/templates/tpl-1');
    await vi.waitFor(() => {
      const app = document.querySelector('scion-app');
      expect(app).not.toBeNull();
      expect(app?.querySelector('scion-page-template-detail')).not.toBeNull();
    }, BOOT_WAIT);
    expect(document.querySelector('scion-profile-shell')).toBeNull();

    // Hub-scoped template detail also stays in the main app shell.
    main.navigateTo('/settings/templates/tpl-1');
    await vi.waitFor(() => {
      const app = document.querySelector('scion-app') as
        | (HTMLElement & { currentPath?: string })
        | null;
      expect(app).not.toBeNull();
      expect(app?.currentPath).toBe('/settings/templates/tpl-1');
      expect(app?.querySelector('scion-page-template-detail')).not.toBeNull();
    }, BOOT_WAIT);
    expect(document.querySelector('scion-profile-shell')).toBeNull();

    // Navigating back to /profile/templates/:id switches back to scion-profile-shell.
    main.navigateTo('/profile/templates/tpl-2');
    await vi.waitFor(() => {
      const shell = document.querySelector('scion-profile-shell');
      expect(shell).not.toBeNull();
      expect(shell?.querySelector('scion-page-template-detail')).not.toBeNull();
    }, BOOT_WAIT);
    expect(document.querySelector('scion-app')).toBeNull();
  }, 15_000);
});
