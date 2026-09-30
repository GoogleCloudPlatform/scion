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
 * Tests for admin-quotas.ts's usage-detail rendering (ptone/scion#2061 P2.2,
 * design.md Amendment A1).
 *
 * The backend does not produce brokerAgentLimitSource: "not_enforced" yet
 * (P1b, ptone/scion#2270, is a separate in-progress PR) — this test stubs
 * the usage-by-limit response with that source to pin the rendering
 * contract ahead of the backend wiring: a broker-scoped reservation whose
 * cap is not currently enforced must show a visible "not enforced" marker
 * next to the cap in the Active Usage detail panel.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

const LIMIT_ID = 'limit-max-agents-per-broker';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const LIMIT_DEFINITION = {
  id: LIMIT_ID,
  name: 'max_agents_per_broker',
  resourceType: 'agent',
  unit: 'count',
  description: 'Max concurrent agents per broker',
  defaultValue: 30,
  system: true,
  createdAt: new Date().toISOString(),
  updatedAt: new Date().toISOString(),
};

function makeReservation(source: string) {
  return {
    id: 'res-1',
    limitDefinitionId: LIMIT_ID,
    subjectId: 'broker-1',
    scopeType: 'broker',
    scopeId: 'broker-1',
    resourceId: 'agent-1',
    reserved: 1,
    createdAt: new Date().toISOString(),
    brokerAgentLimit: 30,
    brokerAgentLimitSource: source,
  };
}

function createFetchHandler(reservation: ReturnType<typeof makeReservation>) {
  return (url: string | URL | Request): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
    if (path.endsWith('/api/v1/admin/limits')) {
      return Promise.resolve(jsonResponse({ items: [LIMIT_DEFINITION] }));
    }
    if (path.endsWith('/api/v1/admin/usage')) {
      return Promise.resolve(
        jsonResponse({ items: [{ limitDefinition: LIMIT_DEFINITION, activeCount: 1 }] })
      );
    }
    if (path.endsWith(`/api/v1/admin/limits/${LIMIT_ID}/entitlements`)) {
      return Promise.resolve(jsonResponse({ items: [] }));
    }
    if (path.endsWith(`/api/v1/admin/usage/${LIMIT_ID}`)) {
      return Promise.resolve(
        jsonResponse({
          limitDefinition: LIMIT_DEFINITION,
          reservations: [reservation],
          totalActive: 1,
        })
      );
    }
    return Promise.resolve(jsonResponse({}, 404));
  };
}

async function mountAdminQuotasPage(
  reservation: ReturnType<typeof makeReservation>
): Promise<HTMLElement & { updateComplete: Promise<boolean> }> {
  vi.stubGlobal('fetch', vi.fn(createFetchHandler(reservation)));
  const el = document.createElement('scion-page-admin-quotas') as HTMLElement & {
    updateComplete: Promise<boolean>;
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 20));
  await el.updateComplete;
  return el;
}

async function expandFirstRow(
  element: HTMLElement & { updateComplete: Promise<boolean> }
): Promise<void> {
  const row = element.shadowRoot?.querySelector('tr.clickable');
  expect(row).not.toBeNull();
  (row as HTMLElement).click();
  await element.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 20));
  await element.updateComplete;
}

describe('scion-page-admin-quotas — not_enforced marker (design.md Amendment A1)', () => {
  let element: (HTMLElement & { updateComplete: Promise<boolean> }) | null = null;

  beforeAll(async () => {
    await import('./admin-quotas.js');
  }, 60_000);

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('shows a visible "not enforced" marker and keeps the cap value when the source is not_enforced', async () => {
    // Stubbed: the backend doesn't produce this source yet (P1b in progress).
    element = await mountAdminQuotasPage(makeReservation('not_enforced'));
    await expandFirstRow(element);

    const text = element.shadowRoot?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
    expect(text).toContain('Broker cap:');
    // The value must be kept (A1: "keep the resolved value"), not omitted.
    expect(text).toContain('Broker cap: 30');
    expect(text.toLowerCase()).toContain('not enforced');

    const marker = element.shadowRoot?.querySelector('.not-enforced-marker');
    expect(marker).not.toBeNull();
    expect(marker?.textContent?.toLowerCase()).toContain('not enforced');
  });

  it('does not show the marker for an enforced source (hub_default)', async () => {
    element = await mountAdminQuotasPage(makeReservation('hub_default'));
    await expandFirstRow(element);

    const text = element.shadowRoot?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
    expect(text).toContain('Broker cap: 30');
    expect(text).toContain('(hub_default)');
    expect(text.toLowerCase()).not.toContain('not enforced');
    expect(element.shadowRoot?.querySelector('.not-enforced-marker')).toBeNull();
  });
});
