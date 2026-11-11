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

import { describe, expect, it, vi } from 'vitest';
import { singleFlight } from './terminal-workspace-loader.js';

describe('terminal workspace loader', () => {
  it('shares one import between concurrent and later calls', async () => {
    const mod = { name: 'workspace' };
    const importer = vi.fn(() => Promise.resolve(mod));
    const load = singleFlight(importer);

    const first = load();
    const second = load();
    expect(second).toBe(first);
    expect(await first).toBe(mod);
    expect(await load()).toBe(mod);
    expect(importer).toHaveBeenCalledTimes(1);
  });

  it('does not cache a failed import, so the next call retries', async () => {
    const mod = { name: 'workspace' };
    const importer = vi
      .fn<() => Promise<typeof mod>>()
      .mockRejectedValueOnce(new Error('chunk gone'))
      .mockResolvedValueOnce(mod);
    const load = singleFlight(importer);

    const failed = load();
    // A call while the failing import is still in flight shares it.
    expect(load()).toBe(failed);
    await expect(failed).rejects.toThrow('chunk gone');
    expect(await load()).toBe(mod);
    expect(importer).toHaveBeenCalledTimes(2);
  });
});
