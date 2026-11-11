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
 * The client entry starts its boot work in parallel and gates the render at
 * the end (ptone/scion#5307): the onboarding status, the feature flags and the
 * initial page chunk are all requested before any of them answers, and an
 * admin route imports its page while the fresh admin-status request is still
 * in flight.
 *
 * The requests are held as unresolved promises and released by the test, not
 * driven by fake timers: what matters is the order of starts and resolutions,
 * and no timer is involved in it.
 */

import { afterAll, describe, expect, it, vi } from 'vitest';
import { BOOT_WAIT, bootMainAt, json } from './__fixtures__/main-boot.js';

const record = vi.hoisted(() => ({
  starts: new Map<string, number>(),
  resolves: new Map<string, number>(),
}));

// Bare page elements: these tests are about when main.ts starts the imports,
// not about the pages. The factory runs when main.ts first imports the module.
vi.mock('../components/pages/home.js', () => {
  record.starts.set('chunk:home', performance.now());
  if (!customElements.get('scion-page-home')) {
    customElements.define('scion-page-home', class extends HTMLElement {});
  }
  return {};
});
vi.mock('../components/pages/admin-users.js', () => {
  record.starts.set('chunk:admin-users', performance.now());
  if (!customElements.get('scion-page-admin-users')) {
    customElements.define('scion-page-admin-users', class extends HTMLElement {});
  }
  return {};
});

/** Requests held until the test releases them, by request path. */
const held = new Map<string, () => void>();
/** Paths to hold; the others are answered at once by the boot fixture. */
const holding = new Set<string>();

function hold(path: string, body: unknown): Promise<Response> | undefined {
  if (!holding.has(path)) return undefined;
  record.starts.set(path, performance.now());
  return new Promise<Response>((resolve) => {
    held.set(path, () => {
      record.resolves.set(path, performance.now());
      held.delete(path);
      resolve(json(body));
    });
  });
}

const BODIES: Record<string, unknown> = {
  '/api/v1/system/status': { complete: true },
  '/api/v1/settings/public': {},
  '/api/v1/experiments': { experiments: {} },
  '/auth/me': { id: 'u1', email: 'u@example.com', name: 'U' },
  '/api/v1/auth/admin-status': { isAdmin: true, isSuperAdmin: true, permissions: [] },
};

/** Set once main.ts has been booted in this file (it boots only once). */
let booted = false;

function releaseAll(): void {
  for (const release of [...held.values()]) release();
}

describe('main.ts boot waterfall', () => {
  afterAll(() => {
    releaseAll();
    vi.unstubAllGlobals();
  });

  it('boot issues system/status, flags and route chunk concurrently', async () => {
    for (const p of [
      '/api/v1/system/status',
      '/api/v1/settings/public',
      '/api/v1/experiments',
      '/auth/me',
    ]) {
      holding.add(p);
    }
    await bootMainAt('/', (p) => hold(p, BODIES[p]));
    booted = true;

    // Every boot request and the page chunk start while all of them are held.
    await vi.waitFor(() => {
      expect(record.starts.has('/api/v1/system/status')).toBe(true);
      expect(record.starts.has('/api/v1/settings/public')).toBe(true);
      expect(record.starts.has('/api/v1/experiments')).toBe(true);
      expect(record.starts.has('/auth/me')).toBe(true);
      expect(record.starts.has('chunk:home')).toBe(true);
    }, BOOT_WAIT);
    expect(record.resolves.size).toBe(0);
    // The chunk is fetched, not rendered: the first render waits for the flags.
    expect(document.querySelector('scion-page-home')).toBeNull();

    holding.clear();
    releaseAll();
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-home')).not.toBeNull(),
      BOOT_WAIT
    );
    const lastStart = Math.max(...record.starts.values());
    const firstResolve = Math.min(...record.resolves.values());
    expect(lastStart).toBeLessThanOrEqual(firstResolve);
  }, 15_000);

  it('admin route starts chunk import before admin-status resolves', async () => {
    // main.ts boots once per file. Reuse the boot of the test above, or boot
    // here (nothing held) when this test runs alone.
    if (!booted) await bootMainAt('/', (p) => hold(p, BODIES[p]));
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-home')).not.toBeNull(),
      BOOT_WAIT
    );
    const main = await import('./main.js');
    // Boot is done and its non-fresh admin-status request was answered; hold
    // the guard's fresh one.
    holding.add('/api/v1/auth/admin-status');
    record.starts.delete('/api/v1/auth/admin-status');

    main.navigateTo('/admin/users');
    await vi.waitFor(() => {
      expect(record.starts.has('/api/v1/auth/admin-status')).toBe(true);
      expect(record.starts.has('chunk:admin-users')).toBe(true);
    }, BOOT_WAIT);
    expect(record.resolves.has('/api/v1/auth/admin-status')).toBe(false);
    // The permission check still runs before anything renders.
    expect(document.querySelector('scion-page-admin-users')).toBeNull();

    holding.clear();
    releaseAll();
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-admin-users')).not.toBeNull(),
      BOOT_WAIT
    );
  }, 15_000);
});
