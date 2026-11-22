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
 * Tests for Phase 2 visibility & approval UI of admin-managed Runtime Brokers
 * (hub.admin_managed_brokers, ptone/scion#7829 §4.6).
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import {
  ADMIN_MANAGED_BROKERS_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';
import { stateManager } from '../../client/state.js';
import type { RuntimeBroker } from '../../shared/types.js';

class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(readonly url: string) {
    super();
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

function reqUrl(input: RequestInfo | URL): string {
  if (typeof input === 'string') return input;
  if (input instanceof URL) return input.toString();
  return input.url;
}

function reqBody(init?: RequestInit): string {
  return typeof init?.body === 'string' ? init.body : '';
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function makeBroker(overrides: Partial<RuntimeBroker> = {}): RuntimeBroker {
  return {
    id: 'broker-1',
    name: 'user-broker',
    slug: 'user-broker',
    version: '1.0.0',
    status: 'online',
    connectionState: 'connected',
    lastHeartbeat: new Date().toISOString(),
    autoProvide: false,
    adminManaged: false,
    policyEligible: false,
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    _capabilities: { actions: ['read', 'manage'] },
    ...overrides,
  };
}

async function flush(el: HTMLElement & { updateComplete: Promise<boolean> }): Promise<void> {
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 25));
  await el.updateComplete;
}

describe('Admin-managed Runtime Brokers UI (hub.admin_managed_brokers, ptone/scion#7829)', () => {
  let element: (HTMLElement & { updateComplete: Promise<boolean> }) | null = null;

  beforeAll(async () => {
    await import('./brokers.js');
    await import('./broker-detail.js');
    await import('./brokers-area.js');
    await import('./broker-area-detail.js');
    await import('./project-settings.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    localStorage.removeItem('scion-view-brokers');
    localStorage.removeItem('scion-view-brokers-area');
    resetServerFlagStateForTests();
  });

  afterEach(() => {
    element?.remove();
    element = null;
    resetServerFlagStateForTests();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  describe('scion-page-brokers', () => {
    it('hides badges, policy-blocked tag, and approve/revoke action when experiment is off', async () => {
      setFeatureFlag(ADMIN_MANAGED_BROKERS_FLAG, false);
      const brokers = [
        makeBroker({
          id: 'b-approved',
          name: 'approved-host',
          adminManaged: true,
          policyEligible: true,
        }),
        makeBroker({
          id: 'b-blocked',
          name: 'blocked-host',
          adminManaged: false,
          policyEligible: false,
        }),
      ];
      vi.stubGlobal(
        'fetch',
        vi.fn(() => Promise.resolve(jsonResponse({ brokers })))
      );

      element = document.createElement('scion-page-brokers') as HTMLElement & {
        updateComplete: Promise<boolean>;
      };
      document.body.appendChild(element);
      await flush(element);

      expect(element.shadowRoot?.querySelector('[data-testid="admin-managed-badge"]')).toBeNull();
      expect(element.shadowRoot?.querySelector('[data-testid="policy-blocked-tag"]')).toBeNull();
      expect(
        element.shadowRoot?.querySelector('[data-testid="broker-approval-action"]')
      ).toBeNull();
    });

    it('renders Admin-managed badge, Not approved tag, and calls PATCH on Approve/Revoke', async () => {
      setFeatureFlag(ADMIN_MANAGED_BROKERS_FLAG, true);
      const patchCalls: Array<{ url: string; body: unknown }> = [];
      const blocked = makeBroker({
        id: 'b-blocked',
        name: 'blocked-host',
        adminManaged: false,
        policyEligible: false,
        _capabilities: { actions: ['read', 'manage'] },
      });
      const readonlyBroker = makeBroker({
        id: 'b-ro',
        name: 'readonly-host',
        adminManaged: true,
        policyEligible: true,
        _capabilities: { actions: ['read'] },
      });

      vi.stubGlobal(
        'fetch',
        vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
          const url = reqUrl(input);
          if (init?.method === 'PATCH' && url.includes('/api/v1/runtime-brokers/b-blocked')) {
            const parsed = JSON.parse(reqBody(init)) as { adminManaged: boolean };
            patchCalls.push({ url, body: parsed });
            return Promise.resolve(
              jsonResponse({
                ...blocked,
                adminManaged: parsed.adminManaged,
                policyEligible: parsed.adminManaged,
              })
            );
          }
          return Promise.resolve(jsonResponse({ brokers: [blocked, readonlyBroker] }));
        })
      );

      element = document.createElement('scion-page-brokers') as HTMLElement & {
        updateComplete: Promise<boolean>;
      };
      document.body.appendChild(element);
      await flush(element);

      // readonly-host shows Admin-managed badge, no Approve/Revoke button.
      const badges = element.shadowRoot?.querySelectorAll('[data-testid="admin-managed-badge"]');
      expect(badges?.length).toBe(1);
      expect(badges?.[0]?.textContent).toContain('Admin-managed');

      // blocked-host shows Not approved tag.
      const blockedTags = element.shadowRoot?.querySelectorAll(
        '[data-testid="policy-blocked-tag"]'
      );
      expect(blockedTags?.length).toBe(1);
      expect(blockedTags?.[0]?.textContent).toContain('Not approved — new agents blocked');

      // Only blocked-host (which has manage capability) has an approval button,
      // and it must not be nested inside the card's <a> anchor (O4).
      const buttons = element.shadowRoot?.querySelectorAll(
        '[data-testid="broker-approval-action"]'
      );
      expect(buttons?.length).toBe(1);
      expect(buttons?.[0]?.textContent?.trim()).toContain('Approve');
      expect(buttons?.[0]?.closest('a')).toBeNull();

      // Click Approve -> sends PATCH {"adminManaged": true} and updates UI.
      (buttons![0] as HTMLElement).click();
      await flush(element);

      expect(patchCalls).toEqual([
        { url: '/api/v1/runtime-brokers/b-blocked', body: { adminManaged: true } },
      ]);
      expect(element.shadowRoot?.querySelector('[data-testid="policy-blocked-tag"]')).toBeNull();
      expect(
        element.shadowRoot?.querySelectorAll('[data-testid="admin-managed-badge"]').length
      ).toBe(2);

      // Click Revoke -> sends PATCH {"adminManaged": false}.
      const revokeBtn = element.shadowRoot?.querySelector(
        '[data-testid="broker-approval-action"]'
      ) as HTMLElement;
      expect(revokeBtn?.textContent?.trim()).toContain('Revoke');
      revokeBtn.click();
      await flush(element);

      expect(patchCalls[1]).toEqual({
        url: '/api/v1/runtime-brokers/b-blocked',
        body: { adminManaged: false },
      });
      expect(
        element.shadowRoot?.querySelectorAll('[data-testid="policy-blocked-tag"]').length
      ).toBe(1);
    });

    it('preserves existing broker detail fields in stateManager when PATCH returns a sparse record (O3)', async () => {
      setFeatureFlag(ADMIN_MANAGED_BROKERS_FLAG, true);
      const richBroker = makeBroker({
        id: 'b-rich',
        name: 'rich-host',
        endpoint: 'https://broker.example.internal',
        agentLimit: 12,
        agentCount: 3,
        adminManaged: false,
        policyEligible: false,
        _capabilities: { actions: ['read', 'manage'] },
      });

      vi.stubGlobal(
        'fetch',
        vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
          const url = reqUrl(input);
          if (init?.method === 'PATCH' && url.includes('/api/v1/runtime-brokers/b-rich')) {
            // Bare PATCH response omitting detail/capacity fields.
            return Promise.resolve(
              jsonResponse({
                id: 'b-rich',
                name: 'rich-host',
                adminManaged: true,
                policyEligible: true,
              })
            );
          }
          return Promise.resolve(jsonResponse({ brokers: [richBroker] }));
        })
      );

      element = document.createElement('scion-page-brokers') as HTMLElement & {
        updateComplete: Promise<boolean>;
      };
      document.body.appendChild(element);
      await flush(element);

      const btn = element.shadowRoot?.querySelector(
        '[data-testid="broker-approval-action"]'
      ) as HTMLElement | null;
      expect(btn).not.toBeNull();
      btn!.click();
      await flush(element);

      const cached = stateManager.getBroker('b-rich');
      expect(cached?.adminManaged).toBe(true);
      expect(cached?.policyEligible).toBe(true);
      expect(cached?.endpoint).toBe('https://broker.example.internal');
      expect(cached?.agentLimit).toBe(12);
      expect(cached?.agentCount).toBe(3);
    });
  });

  describe('scion-page-broker-detail', () => {
    it('shows badges and Approve action for hub-admin (manage cap, non-super-admin role) and calls PATCH', async () => {
      setFeatureFlag(ADMIN_MANAGED_BROKERS_FLAG, true);
      const broker = makeBroker({
        id: 'b-detail',
        name: 'detail-host',
        adminManaged: false,
        policyEligible: false,
        _capabilities: { actions: ['read', 'manage'] },
      });
      let patchBody: unknown = null;

      vi.stubGlobal(
        'fetch',
        vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
          const url = reqUrl(input);
          if (init?.method === 'PATCH' && url === '/api/v1/runtime-brokers/b-detail') {
            patchBody = JSON.parse(reqBody(init));
            return Promise.resolve(
              jsonResponse({ ...broker, adminManaged: true, policyEligible: true })
            );
          }
          if (url === '/api/v1/runtime-brokers/b-detail') {
            return Promise.resolve(jsonResponse(broker));
          }
          if (url.includes('/projects')) {
            return Promise.resolve(jsonResponse({ projects: [] }));
          }
          if (url.includes('/agents')) {
            return Promise.resolve(jsonResponse({ agents: [] }));
          }
          return Promise.resolve(jsonResponse({}, 404));
        })
      );

      element = document.createElement('scion-page-broker-detail') as HTMLElement & {
        brokerId?: string;
        pageData?: unknown;
        updateComplete: Promise<boolean>;
      };
      (element as unknown as { brokerId: string }).brokerId = 'b-detail';
      (element as unknown as { pageData: unknown }).pageData = {
        user: { id: 'u-hubadmin', role: 'member' },
      };
      document.body.appendChild(element);
      await flush(element);

      expect(
        element.shadowRoot?.querySelector('[data-testid="policy-blocked-tag"]')?.textContent
      ).toContain('Not approved — new agents blocked');
      const btn = element.shadowRoot?.querySelector(
        '[data-testid="broker-approval-action"]'
      ) as HTMLElement | null;
      expect(btn).not.toBeNull();
      expect(btn?.textContent?.trim()).toContain('Approve');

      btn!.click();
      await flush(element);

      expect(patchBody).toEqual({ adminManaged: true });
      expect(
        element.shadowRoot?.querySelector('[data-testid="admin-managed-badge"]')?.textContent
      ).toContain('Admin-managed');
      expect(element.shadowRoot?.querySelector('[data-testid="policy-blocked-tag"]')).toBeNull();
    });
  });

  describe('scion-page-project-settings providers list', () => {
    it('shows Not approved tag on non-eligible provider only when experiment is on', async () => {
      const blockedProvider = makeBroker({
        id: 'b-unapproved',
        name: 'unapproved-provider',
        adminManaged: false,
        policyEligible: false,
      });
      const eligibleProvider = makeBroker({
        id: 'b-eligible',
        name: 'eligible-provider',
        adminManaged: true,
        policyEligible: true,
      });

      const stubFetch = () =>
        vi.fn((input: RequestInfo | URL) => {
          const url = reqUrl(input);
          if (url.includes('/api/v1/runtime-brokers?projectId=')) {
            return Promise.resolve(jsonResponse({ brokers: [blockedProvider, eligibleProvider] }));
          }
          if (url === '/api/v1/projects/proj-1') {
            return Promise.resolve(
              jsonResponse({
                id: 'proj-1',
                name: 'Demo',
                slug: 'demo',
                _capabilities: { actions: ['read', 'update'] },
              })
            );
          }
          return Promise.resolve(
            jsonResponse({ items: [], templates: [], envVars: [], secrets: [] })
          );
        });

      // Experiment ON -> tag shown for unapproved provider only.
      setFeatureFlag(ADMIN_MANAGED_BROKERS_FLAG, true);
      vi.stubGlobal('fetch', stubFetch());

      element = document.createElement('scion-page-project-settings') as HTMLElement & {
        projectId?: string;
        updateComplete: Promise<boolean>;
      };
      (element as unknown as { projectId: string }).projectId = 'proj-1';
      document.body.appendChild(element);
      await flush(element);

      const tagsOn = element.shadowRoot?.querySelectorAll('[data-testid="policy-blocked-tag"]');
      expect(tagsOn?.length).toBe(1);
      expect(tagsOn?.[0]?.textContent).toContain('Not approved — new agents blocked');

      // Experiment OFF -> tag hidden.
      element.remove();
      setFeatureFlag(ADMIN_MANAGED_BROKERS_FLAG, false);
      vi.stubGlobal('fetch', stubFetch());

      element = document.createElement('scion-page-project-settings') as HTMLElement & {
        projectId?: string;
        updateComplete: Promise<boolean>;
      };
      (element as unknown as { projectId: string }).projectId = 'proj-1';
      document.body.appendChild(element);
      await flush(element);

      expect(element.shadowRoot?.querySelector('[data-testid="policy-blocked-tag"]')).toBeNull();
    });
  });
});
