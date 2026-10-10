// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Shared by the chat-palette and chat-file-preview e2e fixtures: a stand-in
 * for `src/client/main.ts`, served in its place at the network layer.
 *
 * chat.ts (and chat-members.ts/chat-thread.ts) import `navigateTo`,
 * `replaceRoute`, `pushRoute` and `stateManager` from `client/main.js` — the
 * app's real bootstrap module, which self-initializes on `DOMContentLoaded`
 * (SSR hydration, feature-flag fetch, the full page router, admin-status
 * probe...) the instant anything imports it, real hub or not. Those fixtures
 * deliberately do not run that router/bootstrap (they mount the page
 * component directly), so the module is replaced with the minimal real
 * surface those components actually call — the browser-test equivalent of
 * `vi.mock('../../client/main.js', ...)` in the vitest unit tests, and kept in
 * step with `src/client/__fixtures__/state-stub.ts`.
 *
 * The stub is written in TypeScript and typed against the real
 * `StateManager` and `main.ts` exports, so a change to any stubbed member's
 * name or signature is a typecheck error here rather than a silent drift.
 * The browser receives each declaration's own source text (`toString()`),
 * so the stub bodies must use only browser globals: no imports, no closures
 * over module values, and no class fields (methods and accessors only).
 */

import type { Page } from '@playwright/test';
import type * as RealMain from '../src/client/main.js';
import type { StateManager, SeedEpochToken } from '../src/client/state.js';

/** The `StateManager` members the chat components reach in these fixtures. */
type StubbedStateManager = Pick<
  StateManager,
  | 'currentScope'
  | 'isConnected'
  | 'setScope'
  | 'setCurrentUserId'
  | 'getAgent'
  | 'getAgents'
  | 'getDeletedAgentIds'
  | 'removeAgent'
  | 'beginSeedEpoch'
  | 'seedAgents'
  | 'endSeedEpoch'
>;

/** Holds no agents, never connects, and accepts every write as a no-op. */
class FixtureStateManager extends EventTarget implements StubbedStateManager {
  get currentScope(): null {
    return null;
  }
  get isConnected(): boolean {
    return false;
  }
  setScope(): void {}
  setCurrentUserId(): void {}
  getAgent(): undefined {
    return undefined;
  }
  getAgents(): [] {
    return [];
  }
  getDeletedAgentIds(): Set<string> {
    return new Set();
  }
  removeAgent(): void {}
  beginSeedEpoch(): SeedEpochToken {
    return Symbol('seed-epoch');
  }
  seedAgents(): void {}
  endSeedEpoch(): void {}
}

function navigateTo(path: string): void {
  const url = new URL(path, location.origin);
  history.pushState({}, '', url.pathname + url.search + url.hash);
  window.dispatchEvent(new PopStateEvent('popstate'));
}

function replaceRoute(path: string): Promise<void> {
  history.replaceState(history.state, '', path + location.search + location.hash);
  return Promise.resolve();
}

function pushRoute(path: string): Promise<void> {
  history.pushState({}, '', path);
  return Promise.resolve();
}

/** Typed as the real exports, so each stub must be usable wherever they are. */
const ROUTE_STUBS: Pick<typeof RealMain, 'navigateTo' | 'replaceRoute' | 'pushRoute'> = {
  navigateTo,
  replaceRoute,
  pushRoute,
};

/** The stub module's source, as served in place of `src/client/main.ts`. */
const STUB_MODULE_SOURCE = `
${FixtureStateManager.toString()}
export const stateManager = new FixtureStateManager();
export ${ROUTE_STUBS.navigateTo.toString()}
export ${ROUTE_STUBS.replaceRoute.toString()}
export ${ROUTE_STUBS.pushRoute.toString()}
`;

/** Serves the stub module in place of the app's real `src/client/main.ts`. */
export async function stubMainClientModule(page: Page): Promise<void> {
  await page.route('**/src/client/main.ts', (route) =>
    route.fulfill({ contentType: 'text/javascript', body: STUB_MODULE_SOURCE })
  );
}
