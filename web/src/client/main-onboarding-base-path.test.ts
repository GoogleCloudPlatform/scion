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
 * The first-run onboarding redirect keeps a non-root base path in the URL,
 * like every other history write in the client entry (browserPath).
 */

import { afterAll, describe, expect, it, vi } from 'vitest';
import { BOOT_WAIT, bootMainAt, json } from './__fixtures__/main-boot.js';

vi.mock('../components/pages/onboarding.js', () => {
  if (!customElements.get('scion-page-onboarding')) {
    customElements.define('scion-page-onboarding', class extends HTMLElement {});
  }
  return {};
});

describe('main.ts onboarding redirect', () => {
  afterAll(() => {
    vi.unstubAllEnvs();
    vi.unstubAllGlobals();
  });

  it('keeps the base path when redirecting to onboarding', async () => {
    vi.stubEnv('BASE_URL', '/scion/');
    await bootMainAt('/scion/', (p) =>
      p === '/api/v1/system/status' ? Promise.resolve(json({ complete: false })) : undefined
    );

    await vi.waitFor(
      () => expect(document.querySelector('scion-page-onboarding')).not.toBeNull(),
      BOOT_WAIT
    );
    expect(window.location.pathname).toBe('/scion/onboarding');
    expect(JSON.parse(sessionStorage.getItem('onboardingStatus') ?? '{}')).toEqual({
      complete: false,
    });
  }, 15_000);
});
