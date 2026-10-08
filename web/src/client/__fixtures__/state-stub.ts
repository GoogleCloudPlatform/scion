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
 * Shared stub of `client/state.js` for tests that need the module's
 * `stateManager` only as an event bus.
 *
 *   vi.mock('../../client/state.js', () => import('../../client/__fixtures__/state-stub.js'));
 *
 * `client/__fixtures__/main-stub.ts` re-exports this `stateManager`, so a
 * test that stubs both modules sees one instance, as with the real modules.
 */

export const stateManager = new EventTarget();
