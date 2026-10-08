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
 * Shared stub of the client entry module (`client/main.js`). Importing the
 * real module boots the app (SSE, router, shell), so component tests replace
 * it with this:
 *
 *   vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));
 *
 * A test that needs its own fake state manager, or different routing
 * behaviour, spreads this module and overrides the field:
 *
 *   vi.mock('../../client/main.js', async () => ({
 *     ...(await import('../../client/__fixtures__/main-stub.js')),
 *     stateManager: fakeState,
 *   }));
 */

import { vi } from 'vitest';

export { stateManager } from './state-stub.js';

export const navigateTo = vi.fn();

/** Adds a history entry for `path`, like the real `pushRoute`. */
export const pushRoute = vi.fn((path: string): Promise<void> => {
  window.history.pushState({}, '', path);
  return Promise.resolve();
});

/**
 * Replaces the current entry with `path`, keeping the history state, query
 * and hash, like the real `replaceRoute`.
 */
export const replaceRoute = vi.fn((path: string): Promise<void> => {
  window.history.replaceState(
    window.history.state,
    '',
    path + window.location.search + window.location.hash
  );
  return Promise.resolve();
});
