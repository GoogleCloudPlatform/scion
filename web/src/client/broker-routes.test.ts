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
 * V1: the broker route decision (broker UI Option A, slice A1). Flag off
 * resolves to the existing pages with their exact tags and loaders; flag
 * on resolves to the Brokers area. The page modules are mocked so the test
 * sees which module each loader imports.
 */

import { describe, it, expect, vi, beforeEach } from 'vitest';

const imported = vi.hoisted(() => ({ names: [] as string[] }));

vi.mock('../components/pages/brokers.js', () => {
  imported.names.push('brokers');
  return { page: 'brokers' };
});
vi.mock('../components/pages/broker-detail.js', () => {
  imported.names.push('broker-detail');
  return { page: 'broker-detail' };
});
vi.mock('../components/pages/brokers-area.js', () => {
  imported.names.push('brokers-area');
  return { page: 'brokers-area' };
});
vi.mock('../components/pages/broker-area-detail.js', () => {
  imported.names.push('broker-area-detail');
  return { page: 'broker-area-detail' };
});

import { AREA_BROKER_ROUTES, LEGACY_BROKER_ROUTES, resolveBrokerRoute } from './broker-routes.js';

async function loadedPage(path: string, enabled: boolean): Promise<string> {
  const route = resolveBrokerRoute(path, enabled);
  expect(route, path).not.toBeNull();
  const mod = (await route!.load()) as { page: string };
  return mod.page;
}

describe('resolveBrokerRoute', () => {
  beforeEach(() => {
    imported.names.length = 0;
  });

  it('flag off: the existing pages, exact tags and loaders', async () => {
    const list = resolveBrokerRoute('/brokers', false);
    const detail = resolveBrokerRoute('/brokers/b-1', false);
    expect(list).toBe(LEGACY_BROKER_ROUTES[0]);
    expect(detail).toBe(LEGACY_BROKER_ROUTES[1]);
    expect(list?.tag).toBe('scion-page-brokers');
    expect(detail?.tag).toBe('scion-page-broker-detail');
    expect(await loadedPage('/brokers', false)).toBe('brokers');
    expect(await loadedPage('/brokers/b-1', false)).toBe('broker-detail');
    expect(imported.names).not.toContain('brokers-area');
    expect(imported.names).not.toContain('broker-area-detail');
  });

  it('flag on: the Brokers area pages', async () => {
    const list = resolveBrokerRoute('/brokers', true);
    const detail = resolveBrokerRoute('/brokers/b-1', true);
    expect(list).toBe(AREA_BROKER_ROUTES[0]);
    expect(detail).toBe(AREA_BROKER_ROUTES[1]);
    expect(list?.tag).toBe('scion-page-brokers-area');
    expect(detail?.tag).toBe('scion-page-broker-area-detail');
    expect(await loadedPage('/brokers', true)).toBe('brokers-area');
    expect(await loadedPage('/brokers/b-1', true)).toBe('broker-area-detail');
  });

  it('leaves every other path to the router, in both states', () => {
    for (const enabled of [false, true]) {
      for (const path of ['/brokersx', '/brokers/b-1/extra', '/agents', '/', '/brokers/']) {
        expect(resolveBrokerRoute(path, enabled), `${path} ${enabled}`).toBeNull();
      }
    }
  });
});
