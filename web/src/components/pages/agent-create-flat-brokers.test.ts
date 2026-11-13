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
 * Flat Runtime Broker behavior on `/agents/new` (ptone/scion#3283):
 *  - Broker picker options display the flat broker's runtime target label
 *    (`type · displayName · ns <namespace>`) alongside status, while legacy
 *    brokers keep `name (status)`.
 *  - Selecting a flat broker hides the Runtime Profile selector, clears any
 *    previously selected profile, and never sends `profile` in POST /api/v1/agents.
 *  - Selecting a flat Kubernetes broker surfaces `KUBERNETES_BLOCK_HINT` in
 *    the GCP Identity picker even though `profiles` is empty.
 *  - Switching from a legacy Kubernetes profile to a flat Kubernetes broker
 *    clears stale service-account `assignStatus` labels and does not send a
 *    profile-scoped `assignStatus` query.
 */

import { describe, it, expect, vi, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import { createInternals, formField } from './__fixtures__/agent-create-internals.js';
import { KUBERNETES_BLOCK_HINT } from '../../shared/gcp-identity-state.js';
import type { RuntimeBroker } from '../../shared/types.js';

type MountedEl = HTMLElement & { updateComplete: Promise<unknown> };

interface AgentCreateInternals {
  name: string;
  projectId: string;
  brokers: Partial<RuntimeBroker>[];
  brokerId: string;
  profile: string;
  handleSubmit: (e: Event) => Promise<void>;
}

const FLAT_DOCKER_BROKER: Partial<RuntimeBroker> = {
  id: 'broker-flat-docker',
  name: 'local-docker',
  status: 'online',
  profiles: [],
  runtimeTarget: {
    id: 'rt-docker',
    type: 'docker',
    displayName: 'Workstation Docker',
  },
};

const FLAT_K8S_BROKER: Partial<RuntimeBroker> = {
  id: 'broker-flat-k8s',
  name: 'cluster-a-agents',
  status: 'online',
  profiles: [],
  runtimeTarget: {
    id: 'rt-k8s',
    type: 'kubernetes',
    displayName: 'Cluster A',
  },
  runtimeTargetPresentation: {
    schemaVersion: 1,
    key: 'cluster-a',
    name: 'cluster-a-agents',
    target: {
      type: 'kubernetes',
      displayName: 'Cluster A',
      namespace: 'agents',
    },
  },
};

const LEGACY_K8S_BROKER: Partial<RuntimeBroker> = {
  id: 'broker-legacy-k8s',
  name: 'legacy-k8s',
  status: 'online',
  profiles: [
    { name: 'k8s-a', type: 'kubernetes', available: true },
    { name: 'k8s-b', type: 'kubernetes', available: true },
  ],
};

function stubFetch(
  brokers: Partial<RuntimeBroker>[] = [FLAT_DOCKER_BROKER, FLAT_K8S_BROKER, LEGACY_K8S_BROKER]
): {
  bodies: Array<Record<string, unknown>>;
  saListUrls: URL[];
} {
  const bodies: Array<Record<string, unknown>> = [];
  const saListUrls: URL[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const raw = requestUrl(input);
      const ok = (body: unknown) =>
        Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) } as Response);

      if (raw.includes('/api/v1/agents') && init?.method === 'POST') {
        if (typeof init.body === 'string') {
          bodies.push(JSON.parse(init.body) as Record<string, unknown>);
        }
        return Promise.resolve({
          ok: false,
          status: 400,
          json: () => Promise.resolve({ error: { message: 'stub: not actually created' } }),
        } as Response);
      }
      if (raw.includes('/api/v1/projects?')) {
        return ok({ projects: [{ id: 'p1', name: 'P1' }] });
      }
      if (raw.includes('/api/v1/runtime-brokers')) {
        return ok({ brokers });
      }
      if (raw.includes('/api/v1/projects/p1/settings')) {
        return ok({
          defaultGCPIdentityMode: 'assign',
          defaultGCPIdentityServiceAccountID: 'sa-a',
        });
      }
      if (raw.includes('/gcp-service-accounts')) {
        const url = new URL(raw, 'http://hub.example.com');
        saListUrls.push(url);
        const profile = url.searchParams.get('profile') ?? '';
        return ok({
          items: [
            {
              id: 'sa-a',
              scope: 'project',
              scopeId: 'p1',
              email: 'sa-a@example.iam.gserviceaccount.com',
              projectId: 'gcp-proj',
              displayName: '',
              defaultScopes: [],
              verified: true,
              verifiedAt: '2026-01-01T00:00:00Z',
              createdBy: 'user-1',
              ...(profile === 'k8s-a'
                ? {
                    assignStatus: {
                      state: 'mapped',
                      message: 'Mapped on profile k8s-a.',
                    },
                  }
                : {}),
            },
          ],
        });
      }
      return ok({ templates: [], harnessConfigs: [] });
    })
  );
  return { bodies, saListUrls };
}

async function settle(el: MountedEl): Promise<void> {
  for (let i = 0; i < 3; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mountAgentCreate(): Promise<MountedEl> {
  await import('./agent-create.js');
  const el = document.createElement('scion-page-agent-create') as MountedEl;
  document.body.appendChild(el);
  await settle(el);
  return el;
}

async function chooseBroker(el: MountedEl, brokerId: string): Promise<void> {
  const select = el.shadowRoot?.querySelector('#broker');
  if (!select) throw new Error('#broker select not rendered');
  (select as HTMLElement & { value: string }).value = brokerId;
  select.dispatchEvent(new Event('sl-change', { bubbles: true, composed: true }));
  await settle(el);
}

function brokerOptionTexts(el: MountedEl): Array<[string, string]> {
  const select = el.shadowRoot?.querySelector('#broker');
  if (!select) throw new Error('#broker select not rendered');
  return Array.from(select.querySelectorAll('sl-option')).map((opt) => [
    opt.getAttribute('value') ?? '',
    (opt.textContent ?? '').replace(/\s+/g, ' ').trim(),
  ]);
}

function gcpIdentityHint(el: MountedEl): string {
  const field = formField(el, 'GCP Identity');
  return field?.querySelector('.hint')?.textContent?.trim() ?? '';
}

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

describe('Create Agent: flat Runtime Brokers', () => {
  it('renders flat Runtime Broker options with their runtime target label and legacy brokers unchanged', async () => {
    stubFetch();
    const el = await mountAgentCreate();

    expect(brokerOptionTexts(el)).toEqual([
      ['broker-flat-docker', 'local-docker — docker · Workstation Docker (online)'],
      ['broker-flat-k8s', 'cluster-a-agents — kubernetes · Cluster A · ns agents (online)'],
      ['broker-legacy-k8s', 'legacy-k8s (online)'],
    ]);
  });

  it('hides the Runtime Profile picker and omits profile on submit when a flat broker is selected', async () => {
    // Even if a malformed fixture carried stray profiles on a flat broker,
    // isFlatBroker guards both the selector and submission.
    const flatWithStrayProfiles: Partial<RuntimeBroker> = {
      ...FLAT_DOCKER_BROKER,
      profiles: [{ name: 'stray', type: 'docker', available: true }],
    };
    const { bodies } = stubFetch([LEGACY_K8S_BROKER, flatWithStrayProfiles]);
    const el = await mountAgentCreate();
    const page = createInternals<AgentCreateInternals>(el);
    page.name = 'flat-agent';

    // Select legacy broker and pick a profile first.
    await chooseBroker(el, 'broker-legacy-k8s');
    expect(el.shadowRoot?.querySelector('#profile')).not.toBeNull();
    page.profile = 'k8s-a';
    await settle(el);

    // Switch to flat broker: profile picker disappears and profile is cleared.
    await chooseBroker(el, 'broker-flat-docker');
    expect(el.shadowRoot?.querySelector('#profile')).toBeNull();
    expect(page.profile).toBe('');

    await page.handleSubmit(new Event('submit'));
    expect(bodies).toHaveLength(1);
    expect(bodies[0].runtimeBrokerId).toBe('broker-flat-docker');
    expect(bodies[0]).not.toHaveProperty('profile');
  });

  it('shows the Kubernetes Block hint on a flat Kubernetes broker and not on a flat Docker broker', async () => {
    stubFetch([FLAT_DOCKER_BROKER, FLAT_K8S_BROKER]);
    const el = await mountAgentCreate();

    await chooseBroker(el, 'broker-flat-docker');
    expect(gcpIdentityHint(el)).not.toContain(KUBERNETES_BLOCK_HINT);

    await chooseBroker(el, 'broker-flat-k8s');
    expect(gcpIdentityHint(el)).toContain(KUBERNETES_BLOCK_HINT);
  });

  it('clears legacy profile assignStatus labels and avoids profile assignStatus queries on a flat Kubernetes broker', async () => {
    const { saListUrls } = stubFetch([LEGACY_K8S_BROKER, FLAT_K8S_BROKER]);
    const el = await mountAgentCreate();
    const page = createInternals<AgentCreateInternals>(el);

    await chooseBroker(el, 'broker-legacy-k8s');
    page.profile = 'k8s-a';
    await settle(el);

    const saOptionsLegacy = Array.from(
      formField(el, 'Service Account')!.querySelectorAll('sl-option')
    ).map((o) => (o.textContent ?? '').replace(/\s+/g, ' ').trim());
    expect(saOptionsLegacy[0]).toContain('— mapped');
    const urlCountBeforeSwitch = saListUrls.length;

    await chooseBroker(el, 'broker-flat-k8s');
    expect(el.shadowRoot?.querySelector('#profile')).toBeNull();
    // No new ?assignStatus=true query is fired without a profile, and stale labels are cleared.
    expect(saListUrls.length).toBe(urlCountBeforeSwitch);
    const saOptionsFlat = Array.from(
      formField(el, 'Service Account')!.querySelectorAll('sl-option')
    ).map((o) => (o.textContent ?? '').replace(/\s+/g, ' ').trim());
    expect(saOptionsFlat).toEqual(['sa-a@example.iam.gserviceaccount.com']);
  });
}, 30000);
