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
 * V2: the Brokers area list page (broker UI Option A, slice A1): Kind and
 * Runtime target cells, the "Flat instances" stat, the hosted marker, the
 * filter chips, and the Health column, which only a user holding
 * hub.health.read sees (no summary request for anyone else).
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

vi.mock('../../client/admin-status.js', () => ({
  loadAdminStatus: vi.fn(),
  clearAdminStatus: vi.fn(),
}));

import { loadAdminStatus } from '../../client/admin-status.js';
import {
  FakeEventSource,
  FLAT_DOCKER,
  FLAT_K8S,
  LEGACY,
  adminStatus,
  healthSummary,
  jsonResponse,
  settle,
} from './__fixtures__/broker-area.js';

type Page = HTMLElement & { updateComplete: Promise<boolean> };

let fetchMock: ReturnType<typeof vi.fn>;
let summaryStatus = 200;
const mounted: HTMLElement[] = [];

function requested(path: string): boolean {
  return fetchMock.mock.calls.some(([url]) => String(url).startsWith(path));
}

async function mount(): Promise<ShadowRoot> {
  const el = document.createElement('scion-page-brokers-area') as Page;
  document.body.appendChild(el);
  mounted.push(el);
  await settle(el);
  return el.shadowRoot!;
}

function row(root: ShadowRoot, id: string): HTMLElement | null {
  return root.querySelector(`tr.broker-row[data-id="${id}"]`);
}

function cellText(root: ShadowRoot, id: string, cls: string): string {
  return row(root, id)?.querySelector(`.${cls}`)?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
}

describe('scion-page-brokers-area', () => {
  beforeAll(async () => {
    await import('./brokers-area.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    localStorage.removeItem('scion-view-brokers-area');
    window.history.replaceState(null, '', '/brokers');
    summaryStatus = 200;
    fetchMock = vi.fn((url: string) => {
      if (url.startsWith('/api/v1/runtime-brokers')) {
        return jsonResponse({ brokers: [FLAT_K8S, FLAT_DOCKER, LEGACY] });
      }
      if (url.startsWith('/api/v1/admin/health/summary')) {
        if (summaryStatus !== 200) {
          return jsonResponse({ error: { code: 'forbidden', message: 'no' } }, summaryStatus);
        }
        const summary = healthSummary([
          { id: FLAT_K8S.id },
          { id: FLAT_DOCKER.id, checks: { runtime: 'unavailable' } },
          { id: LEGACY.id },
        ]);
        // The legacy broker reports itself unhealthy with every check passing.
        summary.runtime_brokers.items[2].health.status = 'unhealthy';
        return jsonResponse(summary);
      }
      return jsonResponse({}, 404);
    });
    vi.stubGlobal('fetch', fetchMock);
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(false));
  });

  afterEach(() => {
    while (mounted.length) mounted.pop()!.remove();
    vi.unstubAllGlobals();
  });

  it('shows Kind and Runtime target for flat and legacy rows, and the flat count', async () => {
    const root = await mount();
    expect(cellText(root, FLAT_K8S.id, 'kind-badge')).toBe('Flat');
    expect(cellText(root, LEGACY.id, 'kind-badge')).toBe('Legacy');
    expect(cellText(root, FLAT_K8S.id, 'target-cell')).toBe('kubernetes · Cluster A · ns agents');
    expect(cellText(root, FLAT_DOCKER.id, 'target-cell')).toBe('docker');
    expect(cellText(root, LEGACY.id, 'target-cell')).toBe('— (3 profiles)');
    expect(root.querySelector('.flat-instances-stat')?.textContent?.trim()).toBe('2');
    expect(root.textContent).toContain('not enforced');
  });

  it('marks only the broker hosted by this server', async () => {
    const root = await mount();
    expect(row(root, FLAT_K8S.id)?.querySelector('sl-icon.hosted-marker')).not.toBeNull();
    expect(row(root, FLAT_DOCKER.id)?.querySelector('sl-icon.hosted-marker')).toBeNull();
    expect(row(root, LEGACY.id)?.querySelector('sl-icon.hosted-marker')).toBeNull();
  });

  it('filters rows with the chips', async () => {
    const root = await mount();
    const group = root.querySelector('sl-radio-group') as HTMLElement & { value: string };
    const choose = async (value: string) => {
      group.value = value;
      group.dispatchEvent(new Event('sl-change'));
      await (mounted[0] as Page).updateComplete;
    };
    await choose('flat');
    expect(row(root, FLAT_K8S.id)).not.toBeNull();
    expect(row(root, FLAT_DOCKER.id)).not.toBeNull();
    expect(row(root, LEGACY.id)).toBeNull();
    expect(window.location.search).toBe('?filter=flat');
    await choose('legacy');
    expect(row(root, FLAT_K8S.id)).toBeNull();
    expect(row(root, LEGACY.id)).not.toBeNull();
    await choose('attention');
    expect(row(root, LEGACY.id)).not.toBeNull();
    expect(row(root, FLAT_K8S.id)).toBeNull();
    await choose('all');
    expect(root.querySelectorAll('tr.broker-row')).toHaveLength(3);
    expect(window.location.search).toBe('');
  });

  it('without hub.health.read: no summary request and no Health column', async () => {
    const root = await mount();
    expect(requested('/api/v1/admin/health/summary')).toBe(false);
    expect(root.querySelector('th.health-col')).toBeNull();
  });

  it('with hub.health.read: the Health column with failing check counts', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(true));
    const root = await mount();
    expect(requested('/api/v1/admin/health/summary')).toBe(true);
    expect(root.querySelector('th.health-col')).not.toBeNull();
    expect(row(root, FLAT_K8S.id)?.textContent).toContain('ok');
    expect(row(root, FLAT_DOCKER.id)?.querySelector('.health-warn')?.textContent?.trim()).toBe('1');
  });

  it('a status-only problem (unhealthy, no failing check) is a warning, never ok', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(true));
    const root = await mount();
    const cell = row(root, LEGACY.id)?.querySelector('.health-warn');
    expect(cell?.textContent?.trim()).toBe('unhealthy');
    expect(row(root, LEGACY.id)?.querySelector('.health-ok')).toBeNull();
  });

  it('a 403 from the summary hides the Health column', async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(true));
    summaryStatus = 403;
    const root = await mount();
    expect(requested('/api/v1/admin/health/summary')).toBe(true);
    expect(root.querySelector('th.health-col')).toBeNull();
  });
});
