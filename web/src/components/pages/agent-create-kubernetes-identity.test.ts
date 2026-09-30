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
 * Phase 2 of ptone/scion#2328: block is not offered as a GCP identity choice
 * for a Kubernetes runtime target on the Create Agent page.
 *
 * The target is reliably known only from the concrete broker/profile
 * selection the create request would carry — never guessed. These tests pin:
 *  - Block is hidden once every candidate profile for the current
 *    brokerId/profile selection is type "kubernetes".
 *  - Block stays offered when the broker has mixed-runtime available
 *    profiles and none has been chosen yet (not reliably known).
 *  - A mixed broker becomes "known" once a specific kubernetes profile is
 *    chosen.
 *  - A stale "block" selection is corrected away automatically when the
 *    target becomes known-Kubernetes.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

interface BrokerProfileFixture {
  name: string;
  type: string;
  available: boolean;
}

interface BrokerFixture {
  id: string;
  name: string;
  status: string;
  profiles?: BrokerProfileFixture[];
}

/** The private fields under test, exposed via a loose cast (TS privacy is compile-time only). */
interface AgentCreateInternals {
  brokers: BrokerFixture[];
  brokerId: string;
  profile: string;
  gcpMetadataMode: string;
  gcpServiceAccountId: string;
}

function stubFetch(): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => {
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({ projects: [], brokers: [], templates: [], harnessConfigs: [] }),
      } as Response);
    })
  );
}

beforeEach(() => {
  stubFetch();
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

type MountedEl = HTMLElement & { updateComplete: Promise<unknown> };

async function mountAgentCreate(): Promise<MountedEl> {
  await import('./agent-create.js');
  const el = document.createElement('scion-page-agent-create');
  document.body.appendChild(el);
  await new Promise((r) => setTimeout(r, 0));
  const mounted = el as MountedEl;
  await mounted.updateComplete;
  return mounted;
}

function internals(el: MountedEl): AgentCreateInternals {
  return el as unknown as AgentCreateInternals;
}

/** The GCP Identity <sl-select>, located by its field label. */
function gcpIdentitySelect(el: MountedEl): Element | null {
  const fields = Array.from(el.shadowRoot?.querySelectorAll('.form-field') ?? []);
  const field = fields.find((f) => f.querySelector('label')?.textContent?.trim() === 'GCP Identity');
  return field?.querySelector('sl-select') ?? null;
}

function gcpIdentityHint(el: MountedEl): string {
  const fields = Array.from(el.shadowRoot?.querySelectorAll('.form-field') ?? []);
  const field = fields.find((f) => f.querySelector('label')?.textContent?.trim() === 'GCP Identity');
  return field?.querySelector('.hint')?.textContent?.trim() ?? '';
}

describe('Create Agent: block is not offered for a Kubernetes target', () => {
  // Mounting the full Create Agent page (5 concurrent fetches, a large
  // render tree) is slower than the default 5s test timeout under happy-dom.
  vi.setConfig({ testTimeout: 15000 });

  it('hides Block when the selected broker has a single, kubernetes-only profile', async () => {
    const el = await mountAgentCreate();
    const page = internals(el);
    page.brokers = [
      {
        id: 'broker-k8s',
        name: 'k8s-broker',
        status: 'online',
        profiles: [{ name: 'default', type: 'kubernetes', available: true }],
      },
    ];
    page.brokerId = 'broker-k8s';
    await el.updateComplete;

    const select = gcpIdentitySelect(el);
    expect(select).not.toBeNull();
    expect(select!.querySelector('sl-option[value="block"]')).toBeNull();
    expect(gcpIdentityHint(el)).toContain('not available for a Kubernetes runtime target');
  });

  it('keeps Block offered when the broker mixes runtime types and no profile is chosen', async () => {
    const el = await mountAgentCreate();
    const page = internals(el);
    page.brokers = [
      {
        id: 'broker-mixed',
        name: 'mixed-broker',
        status: 'online',
        profiles: [
          { name: 'k8s-profile', type: 'kubernetes', available: true },
          { name: 'docker-profile', type: 'docker', available: true },
        ],
      },
    ];
    page.brokerId = 'broker-mixed';
    page.profile = '';
    await el.updateComplete;

    const select = gcpIdentitySelect(el);
    expect(select).not.toBeNull();
    expect(select!.querySelector('sl-option[value="block"]')).not.toBeNull();
  });

  it('hides Block once a specific kubernetes profile is chosen on a mixed broker', async () => {
    const el = await mountAgentCreate();
    const page = internals(el);
    page.brokers = [
      {
        id: 'broker-mixed',
        name: 'mixed-broker',
        status: 'online',
        profiles: [
          { name: 'k8s-profile', type: 'kubernetes', available: true },
          { name: 'docker-profile', type: 'docker', available: true },
        ],
      },
    ];
    page.brokerId = 'broker-mixed';
    page.profile = 'k8s-profile';
    await el.updateComplete;

    const select = gcpIdentitySelect(el);
    expect(select!.querySelector('sl-option[value="block"]')).toBeNull();
  });

  it('corrects an existing "block" selection away when the target becomes known-Kubernetes', async () => {
    const el = await mountAgentCreate();
    const page = internals(el);
    page.gcpMetadataMode = 'block';
    await el.updateComplete;
    expect(page.gcpMetadataMode).toBe('block');

    page.brokers = [
      {
        id: 'broker-k8s',
        name: 'k8s-broker',
        status: 'online',
        profiles: [{ name: 'default', type: 'kubernetes', available: true }],
      },
    ];
    page.brokerId = 'broker-k8s';
    await el.updateComplete;

    expect(page.gcpMetadataMode).not.toBe('block');
  });

  it('leaves Block available for a docker-only broker', async () => {
    const el = await mountAgentCreate();
    const page = internals(el);
    page.brokers = [
      {
        id: 'broker-docker',
        name: 'docker-broker',
        status: 'online',
        profiles: [{ name: 'default', type: 'docker', available: true }],
      },
    ];
    page.brokerId = 'broker-docker';
    await el.updateComplete;

    const select = gcpIdentitySelect(el);
    expect(select!.querySelector('sl-option[value="block"]')).not.toBeNull();
  });
});
