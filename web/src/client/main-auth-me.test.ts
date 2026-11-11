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
 * ptone/scion#5311 regression guard: when the SSR page data carries the user,
 * boot sends exactly one /auth/me (the timezone preference refresh, which the
 * SSR user lacks), not a second one to look the user up again.
 */

import { afterAll, describe, expect, it, vi } from 'vitest';
import { BOOT_WAIT, bootMainAt } from './__fixtures__/main-boot.js';
import { requestUrl } from './__fixtures__/request-url.js';

// A bare page element: this test is about the boot requests, not the page.
vi.mock('../components/pages/home.js', () => {
  if (!customElements.get('scion-page-home')) {
    customElements.define('scion-page-home', class extends HTMLElement {});
  }
  return {};
});

describe('main.ts boot: /auth/me', () => {
  afterAll(() => {
    vi.unstubAllGlobals();
  });

  it('boot issues one /auth/me when SSR user present', async () => {
    await bootMainAt('/');
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-home')).not.toBeNull(),
      BOOT_WAIT
    );
    // Let any request started by the first render go out too.
    await new Promise((r) => setTimeout(r, 50));

    const authMe = vi
      .mocked(fetch)
      .mock.calls.map(([input]) => new URL(requestUrl(input), 'http://localhost').pathname)
      .filter((p) => p === '/auth/me' || p === '/api/v1/auth/me');
    expect(authMe).toEqual(['/auth/me']);
  }, 15_000);
});
