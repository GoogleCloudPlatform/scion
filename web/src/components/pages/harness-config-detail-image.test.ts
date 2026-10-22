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

import { describe, it, expect } from 'vitest';
import { harnessConfigImage } from './harness-config-detail.js';
import type { HarnessConfig } from '../../shared/types.js';

const base: HarnessConfig = {
  id: 'hc-1',
  name: 'my-config',
  slug: 'my-config',
  harness: 'claude',
  status: 'active',
  scope: 'global',
};

// The detail view shows the hub-built image when there is one, else the
// config.yaml image (ptone/scion#4219).
describe('harness config detail: displayed image', () => {
  it('prefers the built image', () => {
    expect(
      harnessConfigImage({ ...base, builtImage: 'reg/built:7', config: { image: 'cfg/image:1' } })
    ).toBe('reg/built:7');
  });

  it('falls back to the config.yaml image', () => {
    expect(harnessConfigImage({ ...base, config: { image: 'cfg/image:1' } })).toBe('cfg/image:1');
  });

  it('is undefined without either', () => {
    expect(harnessConfigImage(base)).toBeUndefined();
  });
});
