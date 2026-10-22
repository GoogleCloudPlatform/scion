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
 * The router's leave guard (ptone/scion#3460): a page with unsaved changes
 * vetoes leaving it before the router renders, for in-app navigation and
 * for browser Back/Forward, so staying keeps the URL and the page in sync.
 * Needs a fresh entry module, so it is its own file.
 */

import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { BOOT_WAIT, bootMainAt } from './__fixtures__/main-boot.js';
import { addLeaveGuard } from './navigation.js';

vi.mock('../components/pages/admin-users.js', () => {
  if (!customElements.get('scion-page-admin-users')) {
    customElements.define('scion-page-admin-users', class extends HTMLElement {});
  }
  return {};
});
vi.mock('../components/pages/admin-groups.js', () => {
  if (!customElements.get('scion-page-admin-groups')) {
    customElements.define('scion-page-admin-groups', class extends HTMLElement {});
  }
  return {};
});

const USERS = 'scion-page-admin-users';
const GROUPS = 'scion-page-admin-groups';

let main: Awaited<ReturnType<typeof bootMainAt>>;
const removers: Array<() => void> = [];

function shown(tag: string): boolean {
  return document.querySelector(tag) !== null;
}

/** Show `tag` at `path` through the router, with no guard. */
async function show(path: string, tag: string): Promise<void> {
  main.navigateTo(path);
  await vi.waitFor(() => expect(shown(tag)).toBe(true), BOOT_WAIT);
}

/** What browser Back does: move to the previous entry, then fire popstate. */
function back(to: string): void {
  window.history.pushState({}, '', to);
  window.dispatchEvent(new PopStateEvent('popstate', { state: {} }));
}

/** Let a render the router may have started finish. */
async function settled(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 50));
}

beforeAll(async () => {
  main = await bootMainAt('/admin/users');
  await vi.waitFor(() => expect(shown(USERS)).toBe(true), BOOT_WAIT);
}, 30_000);

afterEach(() => {
  for (const remove of removers.splice(0)) remove();
});

afterAll(() => {
  vi.unstubAllGlobals();
});

describe('main.ts router leave guard', () => {
  it('Back, then stay: the URL and the page stay on the guarded page', async () => {
    await show('/admin/groups', GROUPS);
    const stay = vi.fn(() => false);
    removers.push(addLeaveGuard(stay));

    back('/admin/users');
    await settled();

    expect(stay).toHaveBeenCalledTimes(1);
    expect(window.location.pathname).toBe('/admin/groups');
    expect(shown(GROUPS)).toBe(true);
    expect(shown(USERS)).toBe(false);
  });

  it('Back, then leave: the router renders the previous route', async () => {
    await show('/admin/groups', GROUPS);
    const guard = vi.fn(() => true);
    removers.push(addLeaveGuard(guard));

    back('/admin/users');
    await vi.waitFor(() => expect(shown(USERS)).toBe(true), BOOT_WAIT);

    expect(guard).toHaveBeenCalledTimes(1);
    expect(window.location.pathname).toBe('/admin/users');
    expect(shown(GROUPS)).toBe(false);
  });

  it('a vetoed in-app navigation neither pushes an entry nor renders', async () => {
    await show('/admin/users', USERS);
    const guard = vi.fn(() => false);
    removers.push(addLeaveGuard(guard));
    const entries = window.history.length;

    main.navigateTo('/admin/groups');
    await settled();

    expect(guard).toHaveBeenCalledTimes(1);
    expect(window.history.length).toBe(entries);
    expect(window.location.pathname).toBe('/admin/users');
    expect(shown(USERS)).toBe(true);
    expect(shown(GROUPS)).toBe(false);
  });
});
