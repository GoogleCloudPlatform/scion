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
 * The invite page's requests go through apiFetch with `authErrors: 'caller'`
 * (ptone/scion#5641): the page runs signed out and handles 401 itself, so
 * neither a signed-out session check nor a 401 during redemption may run the
 * helper's global sign-in redirect or access-denied toast.
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';

import './invite.js';

interface InvitePageInternals {
  pageState: string;
  errorMessage: string;
  googleEnabled: boolean;
  githubEnabled: boolean;
}

interface Recorded {
  url: string;
  init: RequestInit | undefined;
}

let requests: Recorded[] = [];
let globalAuthEvents: string[] = [];

function recordGlobalAuthEvent(e: Event): void {
  globalAuthEvents.push(e.type);
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/** Stubs fetch: `me` and `redeem` are the statuses of /auth/me and the redeem POST. */
function stubFetch(me: number, redeem = 200): void {
  requests = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      // The test-hub banner polls its own status; only the page's requests count.
      if (!url.startsWith('/api/v1/test-infra/')) requests.push({ url, init });
      if (url === '/auth/me') return Promise.resolve(json(me, { id: 'u1' }));
      if (url === '/auth/providers') {
        return Promise.resolve(json(200, { google: true, github: false }));
      }
      if (url === '/api/v1/auth/invite/redeem') {
        return Promise.resolve(json(redeem, { error: { code: 'x', message: 'denied' } }));
      }
      return Promise.resolve(json(404, {}));
    })
  );
}

async function mount(until: (page: InvitePageInternals) => boolean): Promise<InvitePageInternals> {
  window.history.replaceState(null, '', '/invite?code=abc');
  const el = document.createElement('scion-page-invite');
  document.body.appendChild(el);
  const page = el as unknown as InvitePageInternals;
  await vi.waitFor(() => expect(until(page)).toBe(true));
  return page;
}

beforeEach(() => {
  globalAuthEvents = [];
  window.addEventListener('scion:access-denied', recordGlobalAuthEvent);
  window.addEventListener('scion:account-teardown', recordGlobalAuthEvent);
});

afterEach(() => {
  window.removeEventListener('scion:access-denied', recordGlobalAuthEvent);
  window.removeEventListener('scion:account-teardown', recordGlobalAuthEvent);
  document.body.innerHTML = '';
  window.history.replaceState(null, '', '/');
  sessionStorage.clear();
  vi.unstubAllGlobals();
});

describe('scion-page-invite requests', () => {
  it('shows sign-in when signed out, without the global 401 handling', async () => {
    stubFetch(401);

    const page = await mount((p) => p.pageState === 'unauthenticated');

    expect(requests.map((r) => r.url)).toEqual(['/auth/me', '/auth/providers']);
    expect(requests.every((r) => r.init?.credentials === 'include')).toBe(true);
    expect(page.googleEnabled).toBe(true);
    expect(page.githubEnabled).toBe(false);
    expect(globalAuthEvents).toEqual([]);
    expect(window.location.pathname).toBe('/invite');
  });

  it('redeems the code with a credentialed JSON POST when signed in', async () => {
    stubFetch(200, 200);

    await mount((p) => p.pageState === 'success');

    const redeem = requests.find((r) => r.url === '/api/v1/auth/invite/redeem');
    expect(redeem?.init?.method).toBe('POST');
    expect(redeem?.init?.credentials).toBe('include');
    expect(redeem?.init?.headers).toEqual({ 'Content-Type': 'application/json' });
    expect(redeem?.init?.body).toBe(JSON.stringify({ code: 'abc' }));
    expect(sessionStorage.getItem('scion_invite_code')).toBeNull();
  });

  it('falls back to sign-in on a 401 during redemption, without the global redirect', async () => {
    stubFetch(200, 401);

    await mount((p) => p.pageState === 'unauthenticated');

    expect(requests.map((r) => r.url)).toEqual([
      '/auth/me',
      '/api/v1/auth/invite/redeem',
      '/auth/providers',
    ]);
    expect(globalAuthEvents).toEqual([]);
    expect(window.location.pathname).toBe('/invite');
  });

  it('shows its own error on a 403 during redemption, without the access-denied toast', async () => {
    stubFetch(200, 403);

    const page = await mount((p) => p.pageState === 'error');

    expect(page.errorMessage).toMatch(/Something went wrong/);
    expect(globalAuthEvents).toEqual([]);
  });
});
