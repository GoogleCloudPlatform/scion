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
 * The GCP identity picker on the Create Agent page for Kubernetes runtime
 * targets. ptone/scion#4077 reverses Phase 2 of ptone/scion#2328: brokers
 * accept Block on Kubernetes, so Block is offered there like anywhere else.
 *
 * The target is reliably known only from the concrete broker/profile
 * selection the create request would carry — never guessed. These tests pin:
 *  - Block is offered for every target, and an explicit Block pick is sent.
 *  - A known-Kubernetes target adds the Kubernetes Block hint (what Block
 *    means there); a mixed broker becomes "known" once a specific
 *    kubernetes profile is chosen.
 *  - A target change never rewrites an explicit pick or a project default.
 *  - An untouched picker omits gcp_identity on every target.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import { createInternals, formField } from './__fixtures__/agent-create-internals.js';
import { KUBERNETES_BLOCK_HINT } from '../../shared/gcp-identity-state.js';

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
  defaultGcpMetadataMode: string;
  gcpServiceAccountId: string;
  gcpIdentityUserSet: boolean;
  projectGCPIdentityDefaultApplied: boolean;
  gcpServiceAccounts: GCPServiceAccountFixture[];
  loadGCPServiceAccounts: () => Promise<void>;
}

function stubFetch(): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => {
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () =>
          Promise.resolve({ projects: [], brokers: [], templates: [], harnessConfigs: [] }),
      } as Response);
    })
  );
}

/**
 * Captures the JSON body of every POST /api/v1/agents request, so a test can
 * assert whether gcp_identity was included without letting the real success
 * path (navigateTo, a second /start call) run. The mocked response is a 400
 * so handleSubmit's catch block sets `error` and returns before navigating.
 */
function stubFetchCapturingCreateRequests(): { bodies: Array<Record<string, unknown>> } {
  const bodies: Array<Record<string, unknown>> = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      if (url.includes('/api/v1/agents') && init?.method === 'POST') {
        if (typeof init.body === 'string') {
          bodies.push(JSON.parse(init.body) as Record<string, unknown>);
        }
        return Promise.resolve({
          ok: false,
          status: 400,
          json: () => Promise.resolve({ error: { message: 'stub: not actually created' } }),
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () =>
          Promise.resolve({ projects: [], brokers: [], templates: [], harnessConfigs: [] }),
      } as Response);
    })
  );
  return { bodies };
}

/**
 * Routes the initial page-load fetches so a single online broker (of
 * `brokerType`, kubernetes-only by default) is auto-selected, and the
 * project's stored GCP identity default is `mode` — reproducing the path in
 * loadGCPServiceAccounts that applies a project default *after* the broker
 * is already known. Also captures the JSON body of every POST
 * /api/v1/agents request (mocked to a 400 so handleSubmit's catch block
 * sets `error` and returns without navigating), so a test can assert
 * whether a create request was sent at all, and what it carried.
 */
function stubFetchForKubernetesProjectDefault(
  mode: string = 'block',
  serviceAccounts: GCPServiceAccountFixture[] = [],
  brokerType: 'kubernetes' | 'docker' = 'kubernetes',
  serviceAccountId?: string
): { bodies: Array<Record<string, unknown>> } {
  const bodies: Array<Record<string, unknown>> = [];
  const brokerId = brokerType === 'kubernetes' ? 'broker-k8s' : 'broker-docker';
  const brokerName = brokerType === 'kubernetes' ? 'k8s-broker' : 'docker-broker';
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      if (url.includes('/api/v1/agents') && init?.method === 'POST') {
        if (typeof init.body === 'string') {
          bodies.push(JSON.parse(init.body) as Record<string, unknown>);
        }
        return Promise.resolve({
          ok: false,
          status: 400,
          json: () => Promise.resolve({ error: { message: 'stub: not actually created' } }),
        } as Response);
      }
      if (url.includes('/api/v1/projects?')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ projects: [{ id: 'p1', name: 'P1' }] }),
        } as Response);
      }
      if (url.includes('/api/v1/runtime-brokers')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () =>
            Promise.resolve({
              brokers: [
                {
                  id: brokerId,
                  name: brokerName,
                  status: 'online',
                  profiles: [{ name: 'default', type: brokerType, available: true }],
                },
              ],
            }),
        } as Response);
      }
      if (url.includes('/api/v1/projects/p1/settings')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () =>
            Promise.resolve({
              defaultGCPIdentityMode: mode,
              ...(serviceAccountId ? { defaultGCPIdentityServiceAccountID: serviceAccountId } : {}),
            }),
        } as Response);
      }
      if (url.includes('/gcp-service-accounts')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ items: serviceAccounts }),
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve({}),
      } as Response);
    })
  );
  return { bodies };
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
  return createInternals<AgentCreateInternals>(el);
}

/** The GCP Identity <sl-select>, located by its field label. */
function gcpIdentitySelect(el: MountedEl): Element | null {
  const field = formField(el, 'GCP Identity');
  return field?.querySelector('sl-select') ?? null;
}

function gcpIdentityHint(el: MountedEl): string {
  const field = formField(el, 'GCP Identity');
  return field?.querySelector('.hint')?.textContent?.trim() ?? '';
}

/** The Service Account <sl-select>, located by its field label (only rendered when mode is "assign"). */
function gcpServiceAccountSelect(el: MountedEl): Element | null {
  const field = formField(el, 'Service Account');
  return field?.querySelector('sl-select') ?? null;
}

interface GCPServiceAccountFixture {
  id: string;
  scope: string;
  scopeId: string;
  email: string;
  projectId: string;
  displayName: string;
  defaultScopes: string[];
  verified: boolean;
  verifiedAt: string | null;
  createdBy: string;
}

function makeServiceAccount(id: string, verified: boolean = true): GCPServiceAccountFixture {
  return {
    id,
    scope: 'project',
    scopeId: 'p1',
    email: `${id}@example.iam.gserviceaccount.com`,
    projectId: 'gcp-proj',
    displayName: '',
    defaultScopes: [],
    verified,
    verifiedAt: verified ? '2026-01-01T00:00:00Z' : null,
    createdBy: 'user-1',
  };
}

/** Simulates choosing an option in an sl-select (Shoelace is not registered under happy-dom). */
async function chooseSelect(el: MountedEl, select: Element, value: string): Promise<void> {
  (select as HTMLElement & { value: string }).value = value;
  select.dispatchEvent(new Event('sl-change', { bubbles: true, composed: true }));
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
}

describe('Create Agent: GCP identity on Kubernetes targets (Block offered)', () => {
  // Mounting the full Create Agent page (5 concurrent fetches, a large
  // render tree) is slower than the default 5s test timeout under happy-dom,
  // so the suite sets a longer timeout (third describe argument). A suite
  // timeout is used rather than vi.setConfig, which changes worker-global config.

  it('offers Block with the Kubernetes Block hint for a kubernetes-only broker', async () => {
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
    expect(select!.querySelector('sl-option[value="block"]')).not.toBeNull();
    expect(gcpIdentityHint(el)).toContain(KUBERNETES_BLOCK_HINT);
  });

  it('offers Block with the Kubernetes Block hint once a kubernetes profile is chosen on a mixed broker', async () => {
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
    expect(gcpIdentityHint(el)).not.toContain(KUBERNETES_BLOCK_HINT);

    page.profile = 'k8s-profile';
    await el.updateComplete;

    const select = gcpIdentitySelect(el);
    expect(select!.querySelector('sl-option[value="block"]')).not.toBeNull();
    expect(gcpIdentityHint(el)).toContain(KUBERNETES_BLOCK_HINT);
  });

  it.each(['k8s', 'remote'])(
    'offers Block with the Kubernetes Block hint for a profile of type "%s" (accepted spelling)',
    async (type) => {
      const el = await mountAgentCreate();
      const page = internals(el);
      page.brokers = [
        {
          id: 'broker-alias',
          name: 'alias-broker',
          status: 'online',
          profiles: [{ name: 'default', type, available: true }],
        },
      ];
      page.brokerId = 'broker-alias';
      await el.updateComplete;

      const select = gcpIdentitySelect(el);
      expect(select!.querySelector('sl-option[value="block"]')).not.toBeNull();
      expect(gcpIdentityHint(el)).toContain(KUBERNETES_BLOCK_HINT);
    }
  );

  it('offers Block without the Kubernetes Block hint for a docker-only broker', async () => {
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
    expect(gcpIdentityHint(el)).not.toContain(KUBERNETES_BLOCK_HINT);
  });

  it('sends an explicit Block on a known-Kubernetes target once the user picks it', async () => {
    const tracker = stubFetchCapturingCreateRequests();
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      projectId: string;
      handleSubmit: (e: Event) => Promise<void>;
    };
    page.name = 'test-agent';
    page.projectId = 'p1';
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

    await chooseSelect(el, gcpIdentitySelect(el)!, 'block');
    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('block');
    // The generic Block text does not hold on Kubernetes.
    expect(gcpIdentityHint(el)).toBe(KUBERNETES_BLOCK_HINT);
    expect(gcpIdentityHint(el)).not.toContain('Token requests are denied');

    await page.handleSubmit(new Event('submit'));

    expect(tracker.bodies).toHaveLength(1);
    expect(tracker.bodies[0].gcp_identity).toEqual({ metadata_mode: 'block' });
  });

  // Block is valid on every target, so a target change never rewrites or
  // suspends an explicit Block pick (ptone/scion#4077 reverses the old rule).
  it('keeps an explicit Block pick when the broker switches to a Kubernetes one, and sends it', async () => {
    const tracker = stubFetchCapturingCreateRequests();
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      projectId: string;
      handleSubmit: (e: Event) => Promise<void>;
    };
    page.name = 'test-agent';
    page.projectId = 'p1';
    page.brokers = [
      {
        id: 'broker-docker',
        name: 'docker-broker',
        status: 'online',
        profiles: [{ name: 'default', type: 'docker', available: true }],
      },
      {
        id: 'broker-k8s',
        name: 'k8s-broker',
        status: 'online',
        profiles: [{ name: 'default', type: 'kubernetes', available: true }],
      },
    ];
    page.brokerId = 'broker-docker';
    await el.updateComplete;

    await chooseSelect(el, gcpIdentitySelect(el)!, 'block');
    expect(page.gcpIdentityUserSet).toBe(true);

    page.brokerId = 'broker-k8s';
    await el.updateComplete;

    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('block');
    expect((gcpIdentitySelect(el) as HTMLElement & { value: string }).value).toBe('block');

    await page.handleSubmit(new Event('submit'));
    expect(tracker.bodies).toHaveLength(1);
    expect(tracker.bodies[0].gcp_identity).toEqual({ metadata_mode: 'block' });
  });

  it('keeps an explicit Block pick across a profile-only switch to Kubernetes on a mixed broker', async () => {
    const tracker = stubFetchCapturingCreateRequests();
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      projectId: string;
      handleSubmit: (e: Event) => Promise<void>;
    };
    page.name = 'test-agent';
    page.projectId = 'p1';
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
    page.profile = 'docker-profile';
    await el.updateComplete;

    await chooseSelect(el, gcpIdentitySelect(el)!, 'block');

    page.profile = 'k8s-profile';
    await el.updateComplete;

    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('block');

    await page.handleSubmit(new Event('submit'));
    expect(tracker.bodies).toHaveLength(1);
    expect(tracker.bodies[0].gcp_identity).toEqual({ metadata_mode: 'block' });
  });

  // A project default of Block is a valid outcome on Kubernetes: the picker
  // shows it, submit is allowed, and the untouched request omits gcp_identity
  // so the server resolves that default.
  it('shows a project default of block as Block on a known-Kubernetes broker and submits without gcp_identity', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault('block');
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event) => Promise<void>;
    };
    page.name = 'test-agent';

    expect(page.brokerId).toBe('broker-k8s');
    expect(page.gcpMetadataMode).toBe('block');
    const select = gcpIdentitySelect(el);
    expect((select as HTMLElement & { value: string }).value).toBe('block');
    expect(select!.querySelector('sl-option[value="block"]')).not.toBeNull();
    expect(gcpIdentityHint(el)).toBe(KUBERNETES_BLOCK_HINT);
    expect(gcpIdentityHint(el)).not.toContain('Token requests are denied');

    await page.handleSubmit(new Event('submit'));

    expect(bodies).toHaveLength(1);
    expect(bodies[0]).not.toHaveProperty('gcp_identity');
  });

  it('shows the applied project default with the Kubernetes Block hint in the untouched hint', async () => {
    stubFetchForKubernetesProjectDefault('passthrough');
    const el = await mountAgentCreate();

    expect((gcpIdentitySelect(el) as HTMLElement & { value: string }).value).toBe('passthrough');
    expect(gcpIdentityHint(el).endsWith(KUBERNETES_BLOCK_HINT)).toBe(true);
    expect(gcpIdentityHint(el)).not.toContain('hub-wide default');
  });

  // Untouched, the request omits gcp_identity and the server resolves the
  // same project default (ptone/scion#3902).
  it('shows Block selected and submits without gcp_identity when the project default is block on a non-Kubernetes target', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault('block', [], 'docker');
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event) => Promise<void>;
    };
    page.name = 'test-agent';

    const select = gcpIdentitySelect(el);
    expect((select as HTMLElement & { value: string }).value).toBe('block');
    expect(gcpIdentityHint(el)).not.toContain('No GCP identity is selected yet.');

    await page.handleSubmit(new Event('submit'));

    expect(bodies).toHaveLength(1);
    expect(bodies[0]).not.toHaveProperty('gcp_identity');
  });

  // A target switch keeps showing the project default. Untouched, submit
  // omits gcp_identity so the server resolves that project default
  // (ptone/scion#3902).
  it('displays the project default of passthrough after switching from a Kubernetes broker to docker, and omits it on submit', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault('passthrough');
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event) => Promise<void>;
    };
    page.name = 'test-agent';
    expect(page.gcpMetadataMode).toBe('passthrough');

    page.brokers = [
      ...page.brokers,
      {
        id: 'broker-docker',
        name: 'docker-broker',
        status: 'online',
        profiles: [{ name: 'default', type: 'docker', available: true }],
      },
    ];
    page.brokerId = 'broker-docker';
    await el.updateComplete;

    expect(page.gcpMetadataMode).toBe('passthrough');
    expect((gcpIdentitySelect(el) as HTMLElement & { value: string }).value).toBe('passthrough');

    await page.handleSubmit(new Event('submit'));
    expect(bodies).toHaveLength(1);
    expect(bodies[0]).not.toHaveProperty('gcp_identity');
  });

  it('displays the project default of assign after switching from a Kubernetes broker to docker, and omits it on submit', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault(
      'assign',
      [makeServiceAccount('sa-a')],
      'kubernetes',
      'sa-a'
    );
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event) => Promise<void>;
    };
    page.name = 'test-agent';
    expect(page.gcpMetadataMode).toBe('assign');
    expect(page.gcpServiceAccountId).toBe('sa-a');

    page.brokers = [
      ...page.brokers,
      {
        id: 'broker-docker',
        name: 'docker-broker',
        status: 'online',
        profiles: [{ name: 'default', type: 'docker', available: true }],
      },
    ];
    page.brokerId = 'broker-docker';
    await el.updateComplete;

    expect(page.gcpMetadataMode).toBe('assign');
    expect((gcpIdentitySelect(el) as HTMLElement & { value: string }).value).toBe('assign');
    expect(page.gcpServiceAccountId).toBe('sa-a');

    await page.handleSubmit(new Event('submit'));
    expect(bodies).toHaveLength(1);
    expect(bodies[0]).not.toHaveProperty('gcp_identity');
  });

  // A target switch never touches an explicit user choice: once the user has
  // picked Assign on a Kubernetes target, switching to docker keeps that pick
  // (and its service account).
  it('keeps an explicit Assign choice after switching from a Kubernetes broker to docker', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault(
      '',
      [makeServiceAccount('sa-a')],
      'kubernetes'
    );
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event) => Promise<void>;
    };
    page.name = 'test-agent';

    const select = gcpIdentitySelect(el);
    await chooseSelect(el, select!, 'assign');
    const saSelect = gcpServiceAccountSelect(el);
    expect(saSelect).not.toBeNull();
    await chooseSelect(el, saSelect!, 'sa-a');
    expect(page.gcpMetadataMode).toBe('assign');
    expect(page.gcpServiceAccountId).toBe('sa-a');

    page.brokers = [
      ...page.brokers,
      {
        id: 'broker-docker',
        name: 'docker-broker',
        status: 'online',
        profiles: [{ name: 'default', type: 'docker', available: true }],
      },
    ];
    page.brokerId = 'broker-docker';
    await el.updateComplete;

    expect(page.gcpMetadataMode).toBe('assign');
    expect(page.gcpServiceAccountId).toBe('sa-a');

    await page.handleSubmit(new Event('submit'));
    expect(bodies).toHaveLength(1);
    expect(bodies[0].gcp_identity).toEqual({ metadata_mode: 'assign', service_account_id: 'sa-a' });
  });

  // The project default mode is only ever assigned inside the
  // `if (settings?.defaultGCPIdentityMode)` branch — it must still be reset
  // to '' at the top of every call, or a stale "Block" default from a
  // previous project would keep being shown after switching to one with no
  // default configured at all.
  it('resets a stale Block default after switching to a project with no default', async () => {
    stubFetchForKubernetesProjectDefault('block');
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & { projectId: string };
    expect((gcpIdentitySelect(el) as HTMLElement & { value: string }).value).toBe('block');

    // Settings are cached per-projectId (fetchProjectSettings), so observing
    // a reset requires an actual project switch, not a second call for the
    // same project.
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = requestUrl(input);
        if (url.includes('/api/v1/projects/p2/settings')) {
          return Promise.resolve({
            ok: true,
            status: 200,
            json: () => Promise.resolve({}),
          } as Response);
        }
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ items: [] }),
        } as Response);
      })
    );
    page.projectId = 'p2';

    await page.loadGCPServiceAccounts();
    await el.updateComplete;

    expect((gcpIdentitySelect(el) as HTMLElement & { value: string }).value).toBe('');
    expect(gcpIdentityHint(el)).toContain('No mode chosen');
  });

  // defaultGcpMetadataMode and defaultGcpServiceAccountId must also be reset
  // to the page's own placeholder on every call, the same as
  // projectGCPIdentityDefaultMode above — otherwise a stale "assign" default
  // (and its service account) from a previous project would keep being
  // applied after switching to one with no default configured at all.
  it('resets the stale assign default after switching to a project with no default', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault(
      'assign',
      [makeServiceAccount('sa-a')],
      'docker',
      'sa-a'
    );
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      projectId: string;
      handleSubmit: (e: Event) => Promise<void>;
    };
    page.name = 'test-agent';
    expect(page.gcpMetadataMode).toBe('assign');
    expect(page.gcpServiceAccountId).toBe('sa-a');

    // Settings are cached per-projectId (fetchProjectSettings), so observing
    // a reset requires an actual project switch, not a second call for the
    // same project.
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const url = requestUrl(input);
        if (url.includes('/api/v1/agents') && init?.method === 'POST') {
          if (typeof init.body === 'string') {
            bodies.push(JSON.parse(init.body) as Record<string, unknown>);
          }
          return Promise.resolve({
            ok: false,
            status: 400,
            json: () => Promise.resolve({ error: { message: 'stub: not actually created' } }),
          } as Response);
        }
        if (url.includes('/api/v1/projects/p2/settings')) {
          return Promise.resolve({
            ok: true,
            status: 200,
            json: () => Promise.resolve({}),
          } as Response);
        }
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ items: [] }),
        } as Response);
      })
    );
    page.projectId = 'p2';

    await page.loadGCPServiceAccounts();
    await el.updateComplete;

    expect(page.gcpMetadataMode).toBe('block');
    expect(page.gcpServiceAccountId).toBe('');

    await page.handleSubmit(new Event('submit'));
    expect(bodies).toHaveLength(1);
    expect(bodies[0]).not.toHaveProperty('gcp_identity');
  });

  it('says the hub-wide or Kubernetes default applies when this project has no default configured', async () => {
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

    expect(gcpIdentityHint(el)).toContain('hub-wide default');
    expect(gcpIdentityHint(el)).not.toContain("this project's own default GCP identity applies");
    expect(gcpIdentityHint(el)).toContain(KUBERNETES_BLOCK_HINT);
  });

  it('drops the untouched explanation once the user has made a choice', async () => {
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
    await chooseSelect(el, select!, 'passthrough');
    expect(page.gcpIdentityUserSet).toBe(true);

    const hint = gcpIdentityHint(el);
    expect(hint).not.toContain('hub-wide default');
    expect(hint).not.toContain("this project's own default GCP identity applies");
    expect(hint).toContain(KUBERNETES_BLOCK_HINT);
  });

  // On a known-Kubernetes target, substituting an explicit "passthrough" for
  // an untouched picker routes the create request through the Hub's
  // passthrough ownership gate (broker owner/admin + a registered host
  // service account) — which a request that never asked for passthrough
  // should not have to pass, and which also skips Phase 1's
  // unset-on-Kubernetes fallback. The create request must omit gcp_identity
  // entirely unless the user actually chose something.
  it('omits gcp_identity from the create request on a known-Kubernetes target when nothing was explicitly chosen', async () => {
    const tracker = stubFetchCapturingCreateRequests();
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      projectId: string;
      handleSubmit: (e: Event) => Promise<void>;
    };
    page.name = 'test-agent';
    page.projectId = 'p1';
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

    expect(page.gcpIdentityUserSet).toBe(false);
    // Nothing chosen and no project default: the picker is blank.
    expect((gcpIdentitySelect(el) as HTMLElement & { value: string }).value).toBe('');

    await page.handleSubmit(new Event('submit'));

    expect(tracker.bodies).toHaveLength(1);
    expect(tracker.bodies[0]).not.toHaveProperty('gcp_identity');
  });

  it('sends an explicit passthrough on a known-Kubernetes target once the user picks it', async () => {
    const tracker = stubFetchCapturingCreateRequests();
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      projectId: string;
      handleSubmit: (e: Event) => Promise<void>;
    };
    page.name = 'test-agent';
    page.projectId = 'p1';
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

    // Simulate the user explicitly interacting with the picker (the
    // @sl-change handler sets this alongside the mode itself).
    page.gcpIdentityUserSet = true;
    page.gcpMetadataMode = 'passthrough';

    await page.handleSubmit(new Event('submit'));

    expect(tracker.bodies).toHaveLength(1);
    expect(tracker.bodies[0].gcp_identity).toEqual({ metadata_mode: 'passthrough' });
  });

  it('omits gcp_identity for a non-Kubernetes target too when untouched', async () => {
    // The omission applies to every target runtime, not only known-
    // Kubernetes ones: the form does not pin the displayed mode client-side
    // (ptone/scion#3902).
    const tracker = stubFetchCapturingCreateRequests();
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      projectId: string;
      handleSubmit: (e: Event) => Promise<void>;
    };
    page.name = 'test-agent';
    page.projectId = 'p1';
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

    expect(page.gcpIdentityUserSet).toBe(false);
    expect(page.gcpMetadataMode).toBe('block');

    await page.handleSubmit(new Event('submit'));

    expect(tracker.bodies).toHaveLength(1);
    expect(tracker.bodies[0]).not.toHaveProperty('gcp_identity');
  });

  // Drives the real picker, rather than setting gcpIdentityUserSet directly,
  // to pin the mode select's own @sl-change handler.
  it('sets gcpIdentityUserSet when the mode select actually changes', async () => {
    const el = await mountAgentCreate();
    const page = internals(el);
    expect(page.gcpIdentityUserSet).toBe(false);

    const select = gcpIdentitySelect(el);
    expect(select).not.toBeNull();
    await chooseSelect(el, select!, 'passthrough');

    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('passthrough');
  });

  it('sets gcpIdentityUserSet when the service account select actually changes', async () => {
    const el = await mountAgentCreate();
    const page = internals(el);
    page.gcpServiceAccounts = [makeServiceAccount('sa-a'), makeServiceAccount('sa-b')];
    // Direct state writes in the test, not a user pick — defaultGcpMetadataMode
    // must agree with gcpMetadataMode or normalize corrects the mode back to
    // the (unset) default on the next render, same as it would for any other
    // untouched mismatch. The applied flag models a project default of
    // assign, without which the picker (and this select) would be blank.
    page.defaultGcpMetadataMode = 'assign';
    page.gcpMetadataMode = 'assign';
    page.projectGCPIdentityDefaultApplied = true;
    page.gcpIdentityUserSet = false;
    await el.updateComplete;

    const saSelect = gcpServiceAccountSelect(el);
    expect(saSelect).not.toBeNull();
    await chooseSelect(el, saSelect!, 'sa-b');

    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpServiceAccountId).toBe('sa-b');
  });

  // Simulates a project switch (which re-runs loadGCPServiceAccounts) after
  // the user already interacted with the picker for the previous project.
  it('resets gcpIdentityUserSet when loadGCPServiceAccounts recomputes defaults from scratch', async () => {
    const el = await mountAgentCreate();
    const page = internals(el);
    page.gcpIdentityUserSet = true;

    await page.loadGCPServiceAccounts();

    expect(page.gcpIdentityUserSet).toBe(false);
  });
}, 15_000);

/**
 * ptone/scion#2548: loadGCPServiceAccounts is fired unawaited on a project
 * switch, so its responses can land out of order, or after the user already
 * picked an identity. A stale load must not apply its accounts or default,
 * and an arriving default must never overwrite an explicit user pick.
 */
describe('Create Agent: GCP identity defaults do not race', () => {
  // Mounts the full Create Agent page; see the suite timeout (third describe
  // argument) on the suite above for why.

  interface Deferred {
    promise: Promise<Response>;
    resolve: (body: unknown) => void;
  }

  function deferred(): Deferred {
    let resolve!: (body: unknown) => void;
    const promise = new Promise<Response>((r) => {
      resolve = (body: unknown) =>
        r({ ok: true, status: 200, json: () => Promise.resolve(body) } as Response);
    });
    return { promise, resolve };
  }

  /**
   * Routes per-URL-fragment responses: a Deferred entry is held until the
   * test resolves it; a plain object is returned immediately. Anything
   * unmatched gets an empty list response.
   */
  function stubRoutes(routes: Record<string, unknown>): void {
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = requestUrl(input);
        for (const [fragment, route] of Object.entries(routes)) {
          if (url.includes(fragment)) {
            if (route && typeof route === 'object' && 'promise' in route) {
              return (route as Deferred).promise;
            }
            return Promise.resolve({
              ok: true,
              status: 200,
              json: () => Promise.resolve(route),
            } as Response);
          }
        }
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ items: [] }),
        } as Response);
      })
    );
  }

  const flush = (): Promise<void> => new Promise((r) => setTimeout(r, 20));

  type Page = AgentCreateInternals & { projectId: string; defaultGcpServiceAccountId: string };

  it("does not apply project A's accounts or default when A's account fetch finishes after project B's load", async () => {
    const el = await mountAgentCreate();
    const page = internals(el) as Page;

    const aAccounts = deferred();
    const aSettings = deferred();
    stubRoutes({
      '/projects/pA/gcp-service-accounts': aAccounts,
      '/projects/pA/settings': aSettings,
      '/projects/pB/gcp-service-accounts': { items: [] },
      '/projects/pB/settings': { defaultGCPIdentityMode: 'passthrough' },
    });

    page.projectId = 'pA';
    const loadA = page.loadGCPServiceAccounts();
    page.projectId = 'pB';
    await page.loadGCPServiceAccounts();
    expect(page.gcpMetadataMode).toBe('passthrough');

    // A's responses land late.
    aAccounts.resolve({ items: [makeServiceAccount('sa-a')] });
    aSettings.resolve({
      defaultGCPIdentityMode: 'assign',
      defaultGCPIdentityServiceAccountID: 'sa-a',
    });
    await loadA;
    await flush();
    await el.updateComplete;

    expect(page.gcpServiceAccounts).toEqual([]);
    expect(page.gcpMetadataMode).toBe('passthrough');
    expect(page.defaultGcpMetadataMode).toBe('passthrough');
    expect(page.gcpServiceAccountId).toBe('');
    expect(page.defaultGcpServiceAccountId).toBe('');
  });

  it("does not apply project A's default when A's settings fetch finishes after project B's load", async () => {
    const el = await mountAgentCreate();
    const page = internals(el) as Page;

    const aSettings = deferred();
    stubRoutes({
      '/projects/pA/gcp-service-accounts': { items: [makeServiceAccount('sa-a')] },
      '/projects/pA/settings': aSettings,
      '/projects/pB/gcp-service-accounts': { items: [makeServiceAccount('sa-b')] },
      '/projects/pB/settings': {
        defaultGCPIdentityMode: 'assign',
        defaultGCPIdentityServiceAccountID: 'sa-b',
      },
    });

    page.projectId = 'pA';
    const loadA = page.loadGCPServiceAccounts();
    // Let A's account fetch complete so A is parked on its settings await.
    await flush();
    page.projectId = 'pB';
    await page.loadGCPServiceAccounts();
    expect(page.gcpServiceAccountId).toBe('sa-b');

    aSettings.resolve({ defaultGCPIdentityMode: 'passthrough' });
    await loadA;
    await flush();
    await el.updateComplete;

    expect(page.gcpServiceAccounts.map((sa) => sa.id)).toEqual(['sa-b']);
    expect(page.gcpMetadataMode).toBe('assign');
    expect(page.defaultGcpMetadataMode).toBe('assign');
    expect(page.gcpServiceAccountId).toBe('sa-b');
  });

  it('drops an older load for the same project after switching A -> B -> A', async () => {
    const el = await mountAgentCreate();
    const page = internals(el) as Page;

    // The first pA accounts request is held; later ones answer at once with
    // newer data. projectId is pA again by the time the held one lands, so
    // only the load sequence number can tell it is stale.
    const firstAAccounts = deferred();
    let aAccountCalls = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = requestUrl(input);
        const ok = (body: unknown): Promise<Response> =>
          Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) } as Response);
        if (url.includes('/projects/pA/gcp-service-accounts')) {
          aAccountCalls++;
          return aAccountCalls === 1
            ? firstAAccounts.promise
            : ok({ items: [makeServiceAccount('sa-a2')] });
        }
        if (url.includes('/projects/pA/settings')) {
          return ok({ defaultGCPIdentityMode: 'passthrough' });
        }
        return ok({ items: [] });
      })
    );

    page.projectId = 'pA';
    const load1 = page.loadGCPServiceAccounts();
    page.projectId = 'pB';
    await page.loadGCPServiceAccounts();
    page.projectId = 'pA';
    await page.loadGCPServiceAccounts();
    expect(page.gcpServiceAccounts.map((sa) => sa.id)).toEqual(['sa-a2']);
    expect(page.gcpMetadataMode).toBe('passthrough');

    firstAAccounts.resolve({ items: [makeServiceAccount('sa-a1')] });
    await load1;
    await flush();
    await el.updateComplete;

    expect(page.gcpServiceAccounts.map((sa) => sa.id)).toEqual(['sa-a2']);
    expect(page.gcpMetadataMode).toBe('passthrough');
    expect(page.defaultGcpMetadataMode).toBe('passthrough');
  });

  it('keeps a user pick made while the settings fetch is in flight when the default arrives', async () => {
    const el = await mountAgentCreate();
    const page = internals(el) as Page;

    const settings = deferred();
    stubRoutes({
      '/projects/pC/gcp-service-accounts': { items: [] },
      '/projects/pC/settings': settings,
    });

    page.projectId = 'pC';
    const load = page.loadGCPServiceAccounts();
    await flush();
    await el.updateComplete;

    // Non-Kubernetes target (no brokers), so Block is offered; the user picks
    // it before the project's passthrough default arrives.
    await chooseSelect(el, gcpIdentitySelect(el)!, 'block');
    expect(page.gcpIdentityUserSet).toBe(true);

    settings.resolve({ defaultGCPIdentityMode: 'passthrough' });
    await load;
    await el.updateComplete;

    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('block');
    // The project default is still recorded, for normalization and hints.
    expect(page.defaultGcpMetadataMode).toBe('passthrough');
  });
}, 15_000);
