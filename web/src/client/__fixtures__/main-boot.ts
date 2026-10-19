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
 * Boots the real client entry module (main.ts) in jsdom for route tests:
 * a fake EventSource, a fetch that answers the boot requests (a signed-in,
 * super-admin user), and the SSR page data for `path`. Mock the page modules
 * the test navigates to (see main-admin-status.test.ts) before calling it.
 */

import { vi } from 'vitest';

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

/** Booting the entry module takes a few seconds; room for slow CI runners. */
export const BOOT_WAIT = { timeout: 5_000 };

export async function bootMainAt(path: string): Promise<typeof import('../main.js')> {
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: string | URL | Request) => {
      const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      const p = new URL(raw, 'http://localhost').pathname;
      if (p === '/api/v1/auth/admin-status') {
        return Promise.resolve(json({ isAdmin: true, isSuperAdmin: true, permissions: [] }));
      }
      if (p === '/api/v1/system/status') return Promise.resolve(json({ complete: true }));
      if (p === '/auth/me')
        return Promise.resolve(json({ id: 'u1', email: 'u@example.com', name: 'U' }));
      return Promise.resolve(json({}, 404));
    })
  );
  window.history.replaceState({}, '', path);
  document.body.innerHTML =
    '<div id="app"></div><script id="__SCION_DATA__" type="application/json">' +
    JSON.stringify({
      path,
      title: 'Scion',
      user: { id: 'u1', email: 'u@example.com', name: 'U', role: 'member' },
    }) +
    '</script>';
  return import('../main.js');
}
