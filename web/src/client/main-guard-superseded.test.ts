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
 * A route guard result for a superseded navigation is ignored, deny and
 * allow alike (ptone/scion#3767): a slow admin-status check for navigation
 * A that resolves after the user has moved on to B must neither redirect
 * B away (deny) nor render A's page over B (allow).
 */

import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';
import type { AdminStatus } from '../lib/admin-permissions.js';
import { BOOT_WAIT, bootMainAt } from './__fixtures__/main-boot.js';

function definePage(tag: string): Record<string, never> {
  if (!customElements.get(tag)) customElements.define(tag, class extends HTMLElement {});
  return {};
}
vi.mock('../components/pages/projects.js', () => definePage('scion-page-projects'));
vi.mock('../components/pages/agents.js', () => definePage('scion-page-agents'));
vi.mock('../components/pages/home.js', () => definePage('scion-page-home'));
vi.mock('../components/pages/admin-users.js', () => definePage('scion-page-admin-users'));

const SUPER: AdminStatus = { isAdmin: true, isSuperAdmin: true, permissions: [] };
const DENIED: AdminStatus = { isAdmin: false, isSuperAdmin: false, permissions: [] };

/** Resolvers of the pending fresh (route guard) checks, oldest first. */
const pendingGuards: Array<(s: AdminStatus) => void> = [];

vi.mock('./admin-status.js', async (importOriginal) => {
  const real = await importOriginal<typeof import('./admin-status.js')>();
  return {
    ...real,
    loadAdminStatus: vi.fn((_userId: unknown, options?: { fresh?: boolean }) =>
      options?.fresh
        ? new Promise<AdminStatus>((resolve) => pendingGuards.push(resolve))
        : Promise.resolve(SUPER)
    ),
  };
});

/** Let the guard's continuation and any navigation it starts run. */
async function settle(): Promise<void> {
  for (let i = 0; i < 20; i++) await new Promise((r) => setTimeout(r, 0));
}

describe('main.ts route guard for a superseded navigation', () => {
  let main: typeof import('./main.js');

  beforeAll(async () => {
    main = await bootMainAt('/projects');
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-projects')).not.toBeNull(),
      BOOT_WAIT
    );
  }, 15_000);

  afterAll(() => {
    vi.unstubAllGlobals();
  });

  it('a slow denial for A does not redirect away from B', async () => {
    main.navigateTo('/admin/users'); // A: its guard check is still pending
    await vi.waitFor(() => expect(pendingGuards).toHaveLength(1), BOOT_WAIT);
    main.navigateTo('/agents'); // B
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-agents')).not.toBeNull(),
      BOOT_WAIT
    );

    pendingGuards.shift()!(DENIED);
    await settle();

    expect(window.location.pathname).toBe('/agents');
    expect(document.querySelector('scion-page-home')).toBeNull();
    expect(document.querySelector('scion-page-agents')).not.toBeNull();
  });

  it('a slow approval for A does not render A over B', async () => {
    main.navigateTo('/admin/users'); // A
    await vi.waitFor(() => expect(pendingGuards).toHaveLength(1), BOOT_WAIT);
    main.navigateTo('/projects'); // B
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-projects')).not.toBeNull(),
      BOOT_WAIT
    );

    pendingGuards.shift()!(SUPER);
    await settle();

    expect(window.location.pathname).toBe('/projects');
    expect(document.querySelector('scion-page-admin-users')).toBeNull();
    expect(document.querySelector('scion-page-projects')).not.toBeNull();
  });

  it('control: a denial for the current navigation still redirects home', async () => {
    main.navigateTo('/admin/users');
    await vi.waitFor(() => expect(pendingGuards).toHaveLength(1), BOOT_WAIT);
    pendingGuards.shift()!(DENIED);
    await vi.waitFor(() => expect(window.location.pathname).toBe('/'), BOOT_WAIT);
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-home')).not.toBeNull(),
      BOOT_WAIT
    );
  });
});
