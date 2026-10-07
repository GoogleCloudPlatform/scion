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
 * Wiring of the shared admin-status request in the client entry module:
 * startup starts it, entering an admin route always asks fresh even with a
 * warm shared value, and account teardown drops the value.
 */

import { describe, it, expect, vi, beforeAll, afterAll } from 'vitest';
import type { MockInstance } from 'vitest';

// The admin pages the routes lazy-load are replaced by bare elements: this
// test is about the route guard and startup wiring, not the pages, and
// loading the real modules is what made booting main.ts slow.
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

const calls: Array<{ userId: unknown; fresh: boolean }> = [];

vi.mock('./admin-status.js', async (importOriginal) => {
  const real = await importOriginal<typeof import('./admin-status.js')>();
  return {
    ...real,
    loadAdminStatus: vi.fn((userId: string | null | undefined, options?: { fresh?: boolean }) => {
      calls.push({ userId, fresh: options?.fresh === true });
      return real.loadAdminStatus(userId, options);
    }),
    clearAdminStatus: vi.fn(() => real.clearAdminStatus()),
  };
});

class FakeEventSource extends EventTarget {
  readyState = 0;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  close(): void {
    this.readyState = 2;
  }
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

let adminStatusRequests = 0;
let infoSpy: MockInstance<typeof console.info>;

describe('main.ts admin-status wiring', () => {
  beforeAll(() => {
    infoSpy = vi.spyOn(console, 'info');
    vi.stubGlobal('EventSource', FakeEventSource);
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const path = new URL(raw, 'http://localhost').pathname;
        if (path === '/api/v1/auth/admin-status') {
          adminStatusRequests++;
          return Promise.resolve(json({ isAdmin: true, isSuperAdmin: true, permissions: [] }));
        }
        if (path === '/api/v1/system/status') return Promise.resolve(json({ complete: true }));
        if (path === '/auth/me') {
          return Promise.resolve(json({ id: 'u1', email: 'u@example.com', name: 'U' }));
        }
        return Promise.resolve(json({}, 404));
      })
    );
    window.history.replaceState({}, '', '/admin/users');
    document.body.innerHTML =
      '<div id="app"></div><script id="__SCION_DATA__" type="application/json">' +
      JSON.stringify({
        path: '/admin/users',
        title: 'Scion',
        user: { id: 'u1', email: 'u@example.com', name: 'U', role: 'member' },
      }) +
      '</script>';
  });

  afterAll(() => {
    infoSpy.mockRestore();
    vi.unstubAllGlobals();
  });

  it('startup asks once, an admin route asks fresh even when warm, and teardown drops the value', async () => {
    const main = await import('./main.js');
    const status = await import('./admin-status.js');

    // Startup's shared request, then the guard's fresh one for /admin/users.
    await vi.waitFor(() => expect(calls.some((c) => c.fresh)).toBe(true));
    // Exactly one startup (non-fresh) request before the guard's fresh one.
    const firstFresh = calls.findIndex((c) => c.fresh);
    expect(calls.slice(0, firstFresh)).toEqual([{ userId: 'u1', fresh: false }]);
    await vi.waitFor(() => expect(adminStatusRequests).toBeGreaterThanOrEqual(2));

    // The shared value is warm now; entering another admin route still sends
    // a request of its own.
    const before = adminStatusRequests;
    main.navigateTo('/admin/groups');
    await vi.waitFor(() => expect(adminStatusRequests).toBe(before + 1));
    expect(calls.at(-1)).toEqual({ userId: 'u1', fresh: true });

    // A warm, non-fresh read is served without a request ...
    const warm = adminStatusRequests;
    await status.loadAdminStatus('u1');
    expect(adminStatusRequests).toBe(warm);

    // ... until account teardown drops it: the next read fetches again.
    // (main.ts registers its teardown listener once the first page has
    // rendered.)
    await vi.waitFor(() =>
      expect(infoSpy.mock.calls.some((c) => String(c[0]).includes('initialization complete'))).toBe(
        true
      )
    );
    window.dispatchEvent(
      new CustomEvent('scion:account-teardown', { detail: { reason: 'logout' } })
    );
    expect(status.clearAdminStatus).toHaveBeenCalled();
    await status.loadAdminStatus('u1');
    expect(adminStatusRequests).toBe(warm + 1);
  });
});
