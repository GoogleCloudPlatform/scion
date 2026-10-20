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
 * A mocked hub for the Brokers area gate suite (broker UI Option A, slice
 * A1). Every request is answered here: a signed-in member, one flat broker
 * for the list and detail pages, and an empty but well-formed body for
 * every other hub request. The test pins the hub.flat_runtime_brokers
 * experiment through window.__SCION_FEATURES__; the experiments endpoint
 * deliberately reports the opposite value, so the test also shows that the
 * pinned value wins.
 */

import type { Page } from '@playwright/test';

export const BROKER_ID = 'b1';

const BROKER = {
  id: BROKER_ID,
  name: 'flat-docker',
  slug: 'flat-docker',
  version: '1.0.0',
  status: 'online',
  connectionState: 'connected',
  lastHeartbeat: '2026-10-20T00:00:00Z',
  autoProvide: false,
  runtimeTarget: { id: 'rt-1', type: 'docker' },
  _capabilities: { actions: ['read'] },
};

export async function setupHub(page: Page, options: { flatBrokers: boolean }): Promise<void> {
  const { flatBrokers } = options;

  await page.addInitScript((enabled: boolean) => {
    window.__SCION_FEATURES__ = { 'hub.flat_runtime_brokers': enabled };
    // No SSE server behind the mocks.
    window.EventSource = class extends EventTarget {
      onopen: (() => void) | null = null;
      onerror: (() => void) | null = null;
      readyState = 1;
      constructor() {
        super();
        queueMicrotask(() => this.onopen?.());
      }
      close(): void {}
    } as unknown as typeof EventSource;
  }, flatBrokers);

  // Registered first, so the specific routes below take precedence.
  await page.route('**/api/**', (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith('/agents') || path.endsWith('/agents/')) {
      return route.fulfill({ json: { agents: [] } });
    }
    if (path.endsWith('/projects')) return route.fulfill({ json: { projects: [] } });
    return route.fulfill({ json: {} });
  });

  await page.route('**/auth/me', (route) =>
    route.fulfill({
      json: { id: 'fixture-member', email: 'member@example.test', role: 'member' },
    })
  );
  await page.route('**/api/v1/settings/public', (route) => route.fulfill({ json: {} }));
  await page.route('**/api/v1/experiments', (route) =>
    route.fulfill({ json: { experiments: { 'hub.flat_runtime_brokers': !flatBrokers } } })
  );
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );
  await page.route('**/api/v1/auth/admin-status', (route) =>
    route.fulfill({ json: { isAdmin: false, isSuperAdmin: false, permissions: [] } })
  );
  await page.route('**/api/v1/runtime-brokers', (route) =>
    route.fulfill({ json: { brokers: [BROKER], totalCount: 1 } })
  );
  await page.route(`**/api/v1/runtime-brokers/${BROKER_ID}`, (route) =>
    route.fulfill({ json: BROKER })
  );
  await page.route(`**/api/v1/runtime-brokers/${BROKER_ID}/projects`, (route) =>
    route.fulfill({ json: { projects: [] } })
  );
  await page.route(`**/api/v1/runtime-brokers/${BROKER_ID}/settings`, (route) =>
    route.fulfill({
      json: {
        brokerId: BROKER_ID,
        settings: {},
        effective: {
          maxAgents: {
            value: 0,
            source: 'unlimited',
            inherited: { value: 0, source: 'unlimited' },
          },
        },
        revision: 0,
        _capabilities: { update: false },
      },
    })
  );
}
