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
  gcpIdentityUserSet: boolean;
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
        json: async () => ({ projects: [], brokers: [], templates: [], harnessConfigs: [] }),
      } as Response);
    })
  );
}

/** Records every request URL/method, for asserting a create request was never sent. */
function stubFetchTrackingCalls(): { calls: string[] } {
  const calls: string[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString();
      calls.push(`${init?.method ?? 'GET'} ${url}`);
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({ projects: [], brokers: [], templates: [], harnessConfigs: [] }),
      } as Response);
    })
  );
  return { calls };
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
      const url = typeof input === 'string' ? input : input.toString();
      if (url.includes('/api/v1/agents') && init?.method === 'POST') {
        if (typeof init.body === 'string') {
          bodies.push(JSON.parse(init.body) as Record<string, unknown>);
        }
        return Promise.resolve({
          ok: false,
          status: 400,
          json: async () => ({ error: { message: 'stub: not actually created' } }),
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({ projects: [], brokers: [], templates: [], harnessConfigs: [] }),
      } as Response);
    })
  );
  return { bodies };
}

/**
 * Routes the initial page-load fetches so a single online kubernetes-only
 * broker is auto-selected, and the project's stored GCP identity default is
 * "block" — reproducing the path in loadGCPServiceAccounts that applies a
 * project default *after* the broker is already known (finding 2, PR 2332
 * review round 1).
 */
function stubFetchForKubernetesProjectDefaultBlock(): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString();
      if (url.includes('/api/v1/projects?')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ projects: [{ id: 'p1', name: 'P1' }] }),
        } as Response);
      }
      if (url.includes('/api/v1/runtime-brokers')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({
            brokers: [
              {
                id: 'broker-k8s',
                name: 'k8s-broker',
                status: 'online',
                profiles: [{ name: 'default', type: 'kubernetes', available: true }],
              },
            ],
          }),
        } as Response);
      }
      if (url.includes('/api/v1/projects/p1/settings')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ defaultGCPIdentityMode: 'block' }),
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({}),
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
  const field = fields.find(
    (f) => f.querySelector('label')?.textContent?.trim() === 'GCP Identity'
  );
  return field?.querySelector('sl-select') ?? null;
}

function gcpIdentityHint(el: MountedEl): string {
  const fields = Array.from(el.shadowRoot?.querySelectorAll('.form-field') ?? []);
  const field = fields.find(
    (f) => f.querySelector('label')?.textContent?.trim() === 'GCP Identity'
  );
  return field?.querySelector('.hint')?.textContent?.trim() ?? '';
}

/** The Service Account <sl-select>, located by its field label (only rendered when mode is "assign"). */
function gcpServiceAccountSelect(el: MountedEl): Element | null {
  const fields = Array.from(el.shadowRoot?.querySelectorAll('.form-field') ?? []);
  const field = fields.find(
    (f) => f.querySelector('label')?.textContent?.trim() === 'Service Account'
  );
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

function makeServiceAccount(id: string): GCPServiceAccountFixture {
  return {
    id,
    scope: 'project',
    scopeId: 'p1',
    email: `${id}@example.iam.gserviceaccount.com`,
    projectId: 'gcp-proj',
    displayName: '',
    defaultScopes: [],
    verified: true,
    verifiedAt: '2026-01-01T00:00:00Z',
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

  it('keeps Block offered when the broker has profiles but none is available', async () => {
    const el = await mountAgentCreate();
    const page = internals(el);
    page.brokers = [
      {
        id: 'broker-unavailable',
        name: 'k8s-broker',
        status: 'online',
        profiles: [{ name: 'default', type: 'kubernetes', available: false }],
      },
    ];
    page.brokerId = 'broker-unavailable';
    await el.updateComplete;

    const select = gcpIdentitySelect(el);
    expect(select!.querySelector('sl-option[value="block"]')).not.toBeNull();
  });

  it.each(['k8s', 'remote'])(
    'hides Block for a single available profile of type "%s" (accepted spelling)',
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
      expect(select!.querySelector('sl-option[value="block"]')).toBeNull();
    }
  );

  it('corrects the mode when it is set to block after the broker is already known-Kubernetes', async () => {
    // Reversed order from the "corrects an existing block selection" test
    // above: here the broker is known FIRST, and something sets the mode to
    // block afterward (this is what loadGCPServiceAccounts does on its own
    // default and on a project default of block — finding 2, PR 2332 review
    // round 1). The old updated() hook only watched brokerId/profile/brokers,
    // so a later mode change alone was never re-checked.
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
    expect(page.gcpMetadataMode).not.toBe('block');

    page.gcpMetadataMode = 'block';
    await el.updateComplete;

    expect(page.gcpMetadataMode).not.toBe('block');
  });

  it('does not leave the mode on block when the initial load applies a project default of block for a known-Kubernetes broker', async () => {
    stubFetchForKubernetesProjectDefaultBlock();
    const el = await mountAgentCreate();
    const page = internals(el);

    expect(page.brokerId).toBe('broker-k8s');
    expect(page.gcpMetadataMode).not.toBe('block');

    const select = gcpIdentitySelect(el);
    expect(select!.querySelector('sl-option[value="block"]')).toBeNull();
  });

  // PR 2332 review round 3, finding 3a: the untouched hint must name the real
  // effective identity. Omitting gcp_identity falls through to this
  // project's own default (not Kubernetes' bare default) when one exists.
  it('names the project default in the untouched hint when the project has one configured', async () => {
    stubFetchForKubernetesProjectDefaultBlock();
    const el = await mountAgentCreate();

    expect(gcpIdentityHint(el)).toContain("this project's own default GCP identity applies");
    expect(gcpIdentityHint(el)).not.toContain('hub-wide default');
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
    expect(hint).toContain('not available for a Kubernetes runtime target');
  });

  it('rejects submit when the mode is block for a known-Kubernetes target, without dispatching a create request', async () => {
    const tracker = stubFetchTrackingCalls();
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      projectId: string;
      error: string | null;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
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
    // Force the mode to block synchronously, with no intervening await, so
    // this exercises the submit-time guard directly rather than the
    // reactive willUpdate correction that would otherwise fix it first.
    page.gcpMetadataMode = 'block';

    tracker.calls.length = 0;
    await page.handleSubmit(new Event('submit'));

    expect(page.error).toContain('not available for a Kubernetes runtime target');
    expect(tracker.calls.some((c) => c.includes('/api/v1/agents'))).toBe(false);
  });

  // PR 2332 review round 2, finding 1: on a known-Kubernetes target,
  // substituting an explicit "passthrough" for an untouched picker routes the
  // create request through the Hub's passthrough ownership gate (broker
  // owner/admin + a registered host service account) — which a request that
  // never asked for passthrough should not have to pass, and which also
  // bypasses Phase 1's unset-on-Kubernetes fallback. The create request must
  // omit gcp_identity entirely unless the user actually chose something.
  it('omits gcp_identity from the create request on a known-Kubernetes target when nothing was explicitly chosen', async () => {
    const tracker = stubFetchCapturingCreateRequests();
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      projectId: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
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
    expect(page.gcpMetadataMode).toBe('passthrough'); // display-only correction, not a user choice

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
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
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

  it('still sends gcp_identity for a non-Kubernetes target even when untouched', async () => {
    // Scope check: the omission in finding 1 is specific to known-Kubernetes
    // targets. A docker target's existing default behavior (send the
    // displayed mode explicitly) must be unaffected.
    const tracker = stubFetchCapturingCreateRequests();
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      projectId: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
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
    expect(tracker.bodies[0].gcp_identity).toEqual({ metadata_mode: 'block' });
  });

  // PR 2332 review round 3, finding 5 (mutation W1): every prior test set
  // gcpIdentityUserSet directly, so a mutant that dropped the assignment in
  // the mode select's own @sl-change handler survived. This drives the real
  // picker instead.
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

  // Mutation W2: the SA select's own @sl-change handler.
  it('sets gcpIdentityUserSet when the service account select actually changes', async () => {
    const el = await mountAgentCreate();
    const page = internals(el);
    page.gcpServiceAccounts = [makeServiceAccount('sa-a'), makeServiceAccount('sa-b')];
    page.gcpMetadataMode = 'assign';
    page.gcpIdentityUserSet = false; // the line above is a direct state write in the test, not a user pick
    await el.updateComplete;

    const saSelect = gcpServiceAccountSelect(el);
    expect(saSelect).not.toBeNull();
    await chooseSelect(el, saSelect!, 'sa-b');

    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpServiceAccountId).toBe('sa-b');
  });

  // Mutation W14: the reset in loadGCPServiceAccounts. Simulates a project
  // switch (which re-runs loadGCPServiceAccounts) after the user already
  // interacted with the picker for the previous project.
  it('resets gcpIdentityUserSet when loadGCPServiceAccounts recomputes defaults from scratch', async () => {
    const el = await mountAgentCreate();
    const page = internals(el);
    page.gcpIdentityUserSet = true;

    await page.loadGCPServiceAccounts();

    expect(page.gcpIdentityUserSet).toBe(false);
  });

  // Mutation W24: the explicit normalize call at the end of
  // loadGCPServiceAccounts. Proves it runs synchronously right after the
  // await resolves, without needing a further Lit update cycle — this is
  // what the comment at the call site claims and what a test reading
  // gcpMetadataMode immediately afterward (no further `await el.updateComplete`)
  // would otherwise see as stale if the call were removed.
  it('normalizes the mode immediately when loadGCPServiceAccounts resolves, before the next Lit update cycle', async () => {
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

    // loadGCPServiceAccounts resets the mode to its own "block" placeholder
    // internally before this call's synchronous return.
    await page.loadGCPServiceAccounts();

    expect(page.gcpMetadataMode).not.toBe('block');
  });

  // PR 2332 review round 3, finding 2: an explicit "Block" pick on one
  // broker must not survive as an auto-substituted explicit "passthrough"
  // once the user switches to a Kubernetes broker — that value was never
  // chosen for the new target.
  it('clears gcpIdentityUserSet when a broker switch forces the mode away from an explicit "block"', async () => {
    const tracker = stubFetchCapturingCreateRequests();
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      projectId: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
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

    const select = gcpIdentitySelect(el);
    await chooseSelect(el, select!, 'block');
    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('block');

    // Switch to the Kubernetes broker.
    page.brokerId = 'broker-k8s';
    await el.updateComplete;

    expect(page.gcpIdentityUserSet).toBe(false);
    expect(page.gcpMetadataMode).toBe('passthrough');

    await page.handleSubmit(new Event('submit'));
    expect(tracker.bodies).toHaveLength(1);
    expect(tracker.bodies[0]).not.toHaveProperty('gcp_identity');
  });
});
