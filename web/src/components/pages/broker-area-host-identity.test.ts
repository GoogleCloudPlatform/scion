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
 * The host identity warning on the Brokers area detail page: shown for
 * mismatch and pinned_absent (as on the existing detail page), above the
 * tabs; the account names only for callers whose broker capabilities
 * allow update.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

vi.mock('../../client/admin-status.js', () => ({
  loadAdminStatus: vi.fn(),
  clearAdminStatus: vi.fn(),
}));
vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

import type { BrokerHostIdentity, RuntimeBroker } from '../../shared/types.js';
import { loadAdminStatus } from '../../client/admin-status.js';
import {
  FakeEventSource,
  LEGACY,
  adminStatus,
  jsonResponse,
  settle,
} from './__fixtures__/broker-area.js';

type Page = HTMLElement & { updateComplete: Promise<boolean> };

const CONFIGURED = 'cfg-sa@p.iam.gserviceaccount.com';
const OBSERVED = 'host-sa@p.iam.gserviceaccount.com';

const SETTINGS = {
  brokerId: LEGACY.id,
  settings: {},
  effective: {
    maxAgents: {
      value: 30,
      source: 'hub_default',
      count: 2,
      inherited: { value: 30, source: 'hub_default' },
    },
  },
  revision: 0,
  _capabilities: { update: true },
};

let broker: RuntimeBroker;
const mounted: HTMLElement[] = [];

function withIdentity(
  hostIdentity: BrokerHostIdentity | undefined,
  actions: string[]
): RuntimeBroker {
  return {
    ...LEGACY,
    gcp_host_service_account_email: CONFIGURED,
    hostIdentity,
    _capabilities: { actions },
  };
}

async function mount(b: RuntimeBroker): Promise<Page> {
  broker = b;
  window.history.replaceState(null, '', `/brokers/${b.id}`);
  const el = document.createElement('scion-page-broker-area-detail') as Page;
  document.body.appendChild(el);
  mounted.push(el);
  await settle(el);
  return el;
}

function alertOf(el: Page): HTMLElement | null {
  return el.shadowRoot!.querySelector('sl-alert.host-identity-alert');
}

describe('scion-page-broker-area-detail host identity warning', () => {
  beforeAll(async () => {
    await import('./broker-area-detail.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        const id = broker.id;
        if (url === `/api/v1/runtime-brokers/${id}`) return jsonResponse(broker);
        if (url === `/api/v1/runtime-brokers/${id}/projects`) return jsonResponse({ projects: [] });
        if (url === `/api/v1/runtime-brokers/${id}/settings`) return jsonResponse(SETTINGS);
        if (url.startsWith('/api/v1/agents')) return jsonResponse({ agents: [] });
        return jsonResponse({}, 404);
      })
    );
    vi.mocked(loadAdminStatus).mockResolvedValue(adminStatus(false));
  });

  afterEach(() => {
    while (mounted.length) mounted.pop()!.remove();
    vi.unstubAllGlobals();
  });

  it('mismatch with update: the warning with both account names', async () => {
    const el = await mount(
      withIdentity({ state: 'mismatch', observedEmail: OBSERVED }, ['read', 'update'])
    );
    const alert = alertOf(el);
    expect(alert).not.toBeNull();
    const text = alert!.textContent ?? '';
    expect(text).toContain("Pinned host service account differs from the host's identity");
    expect(text).toContain(`Configured: ${CONFIGURED}`);
    expect(text).toContain(`Host reports: ${OBSERVED}`);
    expect(text).toContain('Change it on the Configure tab.');
  });

  it('pinned_absent with update: the warning with the configured account', async () => {
    const el = await mount(withIdentity({ state: 'pinned_absent' }, ['read', 'update']));
    const text = alertOf(el)?.textContent ?? '';
    expect(text).toContain('Pinned host service account is not present on the host');
    expect(text).toContain(`Configured: ${CONFIGURED}`);
    expect(text).not.toContain('Host reports:');
  });

  it('without update: the warning shows, the account names do not', async () => {
    // Defensive: even if a record carried both accounts, a reader without
    // update sees the state only.
    const el = await mount(withIdentity({ state: 'mismatch', observedEmail: OBSERVED }, ['read']));
    const alert = alertOf(el);
    expect(alert).not.toBeNull();
    const text = alert!.textContent ?? '';
    expect(text).toContain("Pinned host service account differs from the host's identity");
    const leaked = text.includes(CONFIGURED) || text.includes(OBSERVED);
    expect(leaked).toBe(false);
    expect(text).not.toContain('Configure tab');
  });

  it('no warning for match, unset_detected, unknown, none, or no host identity', async () => {
    const states: (BrokerHostIdentity | undefined)[] = [
      { state: 'match', observedEmail: CONFIGURED },
      { state: 'unset_detected' },
      { state: 'unknown' },
      { state: 'none' },
      undefined,
    ];
    for (const hostIdentity of states) {
      const el = await mount(withIdentity(hostIdentity, ['read', 'update']));
      expect(alertOf(el), String(hostIdentity?.state)).toBeNull();
      el.remove();
      mounted.length = 0;
    }
  });
});
