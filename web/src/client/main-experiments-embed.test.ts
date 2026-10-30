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
 * A signed-in shell's boot payload carries the hub's experiment map
 * (ptone/scion#2489): main.ts applies it and sends no
 * `/api/v1/experiments` request. The fallback request, for a payload
 * without the map, is covered by main-boot-waterfall.test.ts.
 */

import { afterAll, describe, expect, it, vi } from 'vitest';
import { BOOT_WAIT, bootMainAt } from './__fixtures__/main-boot.js';
import { isFeatureEnabled } from '../utils/feature-flags.js';

vi.mock('../components/pages/home.js', () => {
  if (!customElements.get('scion-page-home')) {
    customElements.define('scion-page-home', class extends HTMLElement {});
  }
  return {};
});

describe('main.ts embedded experiments', () => {
  afterAll(() => {
    vi.unstubAllGlobals();
  });

  it('applies the embedded map and skips the experiments request', async () => {
    const requested: string[] = [];
    await bootMainAt(
      '/',
      (p) => {
        requested.push(p);
        return undefined;
      },
      { experiments: { 'web.fixture_flag': true } }
    );

    await vi.waitFor(
      () => expect(document.querySelector('scion-page-home')).not.toBeNull(),
      BOOT_WAIT
    );
    expect(isFeatureEnabled('web.fixture_flag')).toBe(true);
    expect(requested).toContain('/api/v1/settings/public');
    expect(requested).not.toContain('/api/v1/experiments');
  }, 15_000);
});
