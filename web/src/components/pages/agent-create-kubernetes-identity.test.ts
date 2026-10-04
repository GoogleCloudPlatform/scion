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
  defaultGcpMetadataMode: string;
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
          json: async () => ({
            defaultGCPIdentityMode: mode,
            ...(serviceAccountId ? { defaultGCPIdentityServiceAccountID: serviceAccountId } : {}),
          }),
        } as Response);
      }
      if (url.includes('/gcp-service-accounts')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ items: serviceAccounts }),
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({}),
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

describe('Create Agent: block is not offered for a Kubernetes target', () => {
  // Mounting the full Create Agent page (5 concurrent fetches, a large
  // render tree) is slower than the default 5s test timeout under happy-dom,
  // so the suite sets a longer timeout (third describe argument). A suite
  // timeout is used rather than vi.setConfig, which changes worker-global config.

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

  it('normalizes the mode when only the profile changes to a known-Kubernetes one on the same broker', async () => {
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
    page.gcpMetadataMode = 'block';
    page.gcpIdentityUserSet = true;
    await el.updateComplete;
    expect(page.gcpMetadataMode).toBe('block');

    page.profile = 'k8s-profile';
    await el.updateComplete;

    expect(page.gcpMetadataMode).toBe('passthrough');
    expect(page.gcpIdentityUserSet).toBe(false);
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
    // default and on a project default of block). willUpdate must watch the
    // mode itself, not just brokerId/profile/brokers, so a later mode change
    // alone is still re-checked.
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
    stubFetchForKubernetesProjectDefault('block');
    const el = await mountAgentCreate();
    const page = internals(el);

    expect(page.brokerId).toBe('broker-k8s');
    expect(page.gcpMetadataMode).not.toBe('block');

    const select = gcpIdentitySelect(el);
    expect(select!.querySelector('sl-option[value="block"]')).toBeNull();
  });

  // The untouched hint must name the real effective identity. Omitting
  // gcp_identity falls through to this project's own default (not
  // Kubernetes' bare default) when one exists.
  it('names the project default in the untouched hint when the project has one configured', async () => {
    stubFetchForKubernetesProjectDefault('passthrough');
    const el = await mountAgentCreate();

    expect(gcpIdentityHint(el)).toContain("this project's own default GCP identity applies");
    expect(gcpIdentityHint(el)).not.toContain('hub-wide default');
  });

  // When the project's own default is itself "block", omitting gcp_identity
  // resolves to that stored default, which the Kubernetes runtime rejects at
  // dispatch — the hint must say so plainly instead of the generic "project
  // default applies" wording, which would read as if Block were safe here.
  it('says the project default is Block, rejected at dispatch, when the stored project default is block', async () => {
    stubFetchForKubernetesProjectDefault('block');
    const el = await mountAgentCreate();

    expect(gcpIdentityHint(el)).toContain(
      "This project's default GCP identity is Block, which the Kubernetes runtime rejects at dispatch"
    );
  });

  // There is no identity here that is safe to leave pre-selected: Shoelace
  // only fires sl-change when the picked value differs from the current one,
  // so a picker already showing "Passthrough" would silently swallow a user
  // re-picking that same option. The picker must show no value at all, so
  // any pick — including Passthrough — is a real change.
  it('shows no pre-selected identity when the stored project default is block', async () => {
    stubFetchForKubernetesProjectDefault('block');
    const el = await mountAgentCreate();

    const select = gcpIdentitySelect(el);
    expect((select as HTMLElement & { value: string }).value).toBe('');
    expect(gcpIdentityHint(el)).toContain('No GCP identity is selected yet.');
  });

  it('mentions Assign Service Account in the Block-default hint only when a verified one is offered', async () => {
    stubFetchForKubernetesProjectDefault('block');
    const withoutSA = await mountAgentCreate();
    expect(gcpIdentityHint(withoutSA)).not.toContain('Assign Service Account');

    document.body.innerHTML = '';
    stubFetchForKubernetesProjectDefault('block', [makeServiceAccount('sa-a', false)]);
    const withUnverifiedOnly = await mountAgentCreate();
    expect(gcpIdentityHint(withUnverifiedOnly)).not.toContain('Assign Service Account');

    document.body.innerHTML = '';
    stubFetchForKubernetesProjectDefault('block', [makeServiceAccount('sa-a')]);
    const withSA = await mountAgentCreate();
    expect(gcpIdentityHint(withSA)).toContain('or Assign Service Account.');
  });

  it('blocks submit with the matching error text whether or not a verified service account is offered', async () => {
    stubFetchForKubernetesProjectDefault('block', [makeServiceAccount('sa-a', false)]);
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
      error: string | null;
    };
    page.name = 'test-agent';

    await page.handleSubmit(new Event('submit'));

    expect(page.error).toContain('Choose Passthrough before creating this agent.');
    expect(page.error).not.toContain('Assign Service Account');
  });

  it('blocks submit with no request sent when the stored project default is block and nothing was chosen', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault('block');
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
      error: string | null;
    };
    page.name = 'test-agent';

    await page.handleSubmit(new Event('submit'));

    expect(page.error).toContain(
      "This project's default GCP identity is Block, which the Kubernetes runtime rejects at dispatch"
    );
    expect(bodies).toHaveLength(0);
  });

  // Follows the hint's own remedy through to the actual request: the picker
  // shows no pre-selected value in this case (see above), so picking
  // Passthrough is a real sl-change, and the resulting request must carry it
  // explicitly rather than omit gcp_identity (which would resolve back to
  // the project's own rejected "block" default).
  it('sends an explicit passthrough once the user picks it, following the Block-default hint', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault('block');
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
      error: string | null;
    };
    page.name = 'test-agent';

    const select = gcpIdentitySelect(el);
    expect(select).not.toBeNull();
    await chooseSelect(el, select!, 'passthrough');
    expect(page.gcpIdentityUserSet).toBe(true);

    await page.handleSubmit(new Event('submit'));

    expect(bodies).toHaveLength(1);
    expect(bodies[0].gcp_identity).toEqual({ metadata_mode: 'passthrough' });
  });

  // The Block-default branch must not take precedence over an explicit user
  // choice — once the user has picked something (here Assign, which requires
  // a configured service account), the hint must say only that Block is
  // unavailable, not that the project's rejected default applies.
  it('yields to an explicit choice instead of the Block-default warning once the user picks one', async () => {
    stubFetchForKubernetesProjectDefault('block', [makeServiceAccount('sa-a')]);
    const el = await mountAgentCreate();

    const select = gcpIdentitySelect(el);
    expect(select).not.toBeNull();
    await chooseSelect(el, select!, 'assign');

    expect(gcpIdentityHint(el)).toContain('Block is not available for a Kubernetes runtime target.');
    expect(gcpIdentityHint(el)).not.toContain('rejects at dispatch');
  });

  // blockDefaultNeedsExplicitChoice must require a known-Kubernetes target,
  // not just a project default of block: Block is a valid, sendable choice
  // on a docker target, so the picker must show it selected (not blank) and
  // submit must send it, exactly as it would without this PR's changes.
  it('shows Block selected and sends it when the project default is block on a non-Kubernetes target', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault('block', [], 'docker');
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
    };
    page.name = 'test-agent';

    const select = gcpIdentitySelect(el);
    expect((select as HTMLElement & { value: string }).value).toBe('block');
    expect(gcpIdentityHint(el)).not.toContain('No GCP identity is selected yet.');

    await page.handleSubmit(new Event('submit'));

    expect(bodies).toHaveLength(1);
    expect(bodies[0].gcp_identity).toEqual({ metadata_mode: 'block' });
  });

  // A project default of "passthrough" or "assign" is a real, sendable
  // choice on a Kubernetes target (only "block" needs the display
  // substitution). defaultGcpMetadataMode must track it faithfully so a
  // later switch to a non-Kubernetes target sends that real default,
  // instead of a "block" that was never the applicable default in the
  // first place.
  it('sends the project default of passthrough after switching from a Kubernetes broker to docker', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault('passthrough');
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
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

    await page.handleSubmit(new Event('submit'));
    expect(bodies).toHaveLength(1);
    expect(bodies[0].gcp_identity).toEqual({ metadata_mode: 'passthrough' });
  });

  it('sends the project default of assign (with its service account) after switching from a Kubernetes broker to docker', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault(
      'assign',
      [makeServiceAccount('sa-a')],
      'kubernetes',
      'sa-a'
    );
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
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
    expect(page.gcpServiceAccountId).toBe('sa-a');

    await page.handleSubmit(new Event('submit'));
    expect(bodies).toHaveLength(1);
    expect(bodies[0].gcp_identity).toEqual({ metadata_mode: 'assign', service_account_id: 'sa-a' });
  });

  // The restore logic must never touch an explicit user choice: once the
  // user has picked Assign on a Kubernetes target, switching to docker must
  // keep that pick (and its service account), not fall back to "block".
  it('keeps an explicit Assign choice after switching from a Kubernetes broker to docker', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault(
      '',
      [makeServiceAccount('sa-a')],
      'kubernetes'
    );
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
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

  // An explicit "Block" pick clears gcpServiceAccountId (it is irrelevant to
  // Block). If the target then becomes Kubernetes-only, the Block constraint
  // suspends that explicit pick (not discards it) and displays the project's
  // real default while on Kubernetes — which, if that default is "assign",
  // must restore the service account too, not leave the mode "assign" with
  // no account selected. Switching back to a non-Kubernetes target must then
  // reinstate the suspended Block, not the project's default: the user chose
  // Block for a docker target, and a round trip through Kubernetes (where
  // Block cannot apply) does not mean they take that choice back.
  it('suspends an explicit Block pick through a Kubernetes target (displaying the real assign default) and reinstates it back on docker', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault(
      'assign',
      [makeServiceAccount('sa-a')],
      'docker',
      'sa-a'
    );
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
    };
    page.name = 'test-agent';
    expect(page.gcpMetadataMode).toBe('assign');
    expect(page.gcpServiceAccountId).toBe('sa-a');

    const select = gcpIdentitySelect(el);
    await chooseSelect(el, select!, 'block');
    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpServiceAccountId).toBe('');

    page.brokers = [
      ...page.brokers,
      {
        id: 'broker-k8s',
        name: 'k8s-broker',
        status: 'online',
        profiles: [{ name: 'default', type: 'kubernetes', available: true }],
      },
    ];
    page.brokerId = 'broker-k8s';
    await el.updateComplete;

    // The explicit Block is suspended (not discarded): the display falls
    // back to the real project default while on Kubernetes, SA included.
    expect(page.gcpIdentityUserSet).toBe(false);
    expect(page.gcpMetadataMode).toBe('assign');
    expect(page.gcpServiceAccountId).toBe('sa-a');

    // Untouched on a Kubernetes target: gcp_identity is omitted regardless,
    // so this leg alone would not surface a broken "assign" with no account.
    await page.handleSubmit(new Event('submit'));
    expect(bodies).toHaveLength(1);
    expect(bodies[0]).not.toHaveProperty('gcp_identity');

    // Back to docker: the suspended Block must be reinstated — this is what
    // the user actually chose for this target — not the project's default.
    page.brokerId = 'broker-docker';
    await el.updateComplete;

    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('block');
    expect(page.gcpServiceAccountId).toBe('');

    await page.handleSubmit(new Event('submit'));
    expect(bodies).toHaveLength(2);
    expect(bodies[1].gcp_identity).toEqual({ metadata_mode: 'block' });
  });

  // Same suspend-and-reinstate behavior with a "passthrough" project default
  // instead of "assign" — the simplest case that still has a non-block
  // default to be mistaken for the user's choice.
  it('suspends an explicit Block pick through a Kubernetes target (displaying the real passthrough default) and reinstates it back on docker', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault('passthrough', [], 'docker');
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
    };
    page.name = 'test-agent';
    expect(page.gcpMetadataMode).toBe('passthrough');

    const select = gcpIdentitySelect(el);
    await chooseSelect(el, select!, 'block');
    expect(page.gcpIdentityUserSet).toBe(true);

    page.brokers = [
      ...page.brokers,
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
    expect(page.gcpMetadataMode).toBe('passthrough');

    page.brokerId = 'broker-docker';
    await el.updateComplete;

    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('block');

    await page.handleSubmit(new Event('submit'));
    expect(bodies).toHaveLength(1);
    expect(bodies[0].gcp_identity).toEqual({ metadata_mode: 'block' });
  });

  // Profile-only variant on a mixed broker: the suspend-and-reinstate must
  // also work when only the profile changes, not just the broker.
  it('suspends and reinstates an explicit Block pick across a profile-only switch on a mixed broker', async () => {
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

    const select = gcpIdentitySelect(el);
    await chooseSelect(el, select!, 'block');
    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('block');

    page.profile = 'k8s-profile';
    await el.updateComplete;

    expect(page.gcpIdentityUserSet).toBe(false);
    expect(page.gcpMetadataMode).toBe('passthrough');

    page.profile = 'docker-profile';
    await el.updateComplete;

    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('block');

    await page.handleSubmit(new Event('submit'));
    expect(tracker.bodies).toHaveLength(1);
    expect(tracker.bodies[0].gcp_identity).toEqual({ metadata_mode: 'block' });
  });

  // A fresh explicit pick must supersede a suspended Block, not coexist with
  // it: picking something else entirely while the suspension is pending (on
  // a Kubernetes target, where Block cannot be re-picked) must clear the
  // suspension, so a later switch to a non-Kubernetes target reinstates
  // nothing and keeps the fresh pick instead of resurrecting the stale Block.
  it('drops a suspended Block once the user makes a different explicit pick before leaving Kubernetes', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault(
      'assign',
      [makeServiceAccount('sa-a')],
      'docker',
      'sa-a'
    );
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
    };
    page.name = 'test-agent';

    const select = gcpIdentitySelect(el);
    await chooseSelect(el, select!, 'block');
    expect(page.gcpIdentityUserSet).toBe(true);

    page.brokers = [
      ...page.brokers,
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
    expect(page.gcpMetadataMode).toBe('assign');

    // A fresh explicit pick while the Block suspension is pending.
    await chooseSelect(el, select!, 'passthrough');
    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('passthrough');

    page.brokerId = 'broker-docker';
    await el.updateComplete;

    // The fresh pick survives; the stale suspended Block is not resurrected.
    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('passthrough');

    await page.handleSubmit(new Event('submit'));
    expect(bodies).toHaveLength(1);
    expect(bodies[0].gcp_identity).toEqual({ metadata_mode: 'passthrough' });
  });

  // Same as above, but the fresh explicit pick is on the service-account
  // select rather than the mode select: while a Block suspension is pending
  // on Kubernetes, the displayed (recomputed-default) mode can itself be
  // "assign", so the SA picker is live. Picking a different account there is
  // just as much a fresh explicit choice as picking a different mode.
  it('drops a suspended Block once the user picks a different service account before leaving Kubernetes', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault(
      'assign',
      [makeServiceAccount('sa-a'), makeServiceAccount('sa-b')],
      'docker',
      'sa-a'
    );
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
    };
    page.name = 'test-agent';

    const select = gcpIdentitySelect(el);
    await chooseSelect(el, select!, 'block');
    expect(page.gcpIdentityUserSet).toBe(true);

    page.brokers = [
      ...page.brokers,
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
    expect(page.gcpMetadataMode).toBe('assign');
    expect(page.gcpServiceAccountId).toBe('sa-a');

    // A fresh explicit pick (a different account) while the Block
    // suspension is pending.
    const saSelect = gcpServiceAccountSelect(el);
    expect(saSelect).not.toBeNull();
    await chooseSelect(el, saSelect!, 'sa-b');
    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpServiceAccountId).toBe('sa-b');

    page.brokerId = 'broker-docker';
    await el.updateComplete;

    // The fresh pick survives; the stale suspended Block is not resurrected.
    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('assign');
    expect(page.gcpServiceAccountId).toBe('sa-b');

    await page.handleSubmit(new Event('submit'));
    expect(bodies).toHaveLength(1);
    expect(bodies[0].gcp_identity).toEqual({ metadata_mode: 'assign', service_account_id: 'sa-b' });
  });

  // A project switch must also drop a pending suspension: it belongs to the
  // previous project's context (the broker/profile state at the time the
  // Block pick happened), not the new one. Without the clear, a Block picked
  // for one project could resurface on an unrelated later project that never
  // had anything to do with that choice.
  it('drops a suspended Block on a project switch, so the new project is not stuck replaying an unrelated choice', async () => {
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
                  id: 'broker-docker',
                  name: 'docker-broker',
                  status: 'online',
                  profiles: [{ name: 'default', type: 'docker', available: true }],
                },
              ],
            }),
          } as Response);
        }
        if (url.includes('/api/v1/projects/p2/settings')) {
          return Promise.resolve({
            ok: true,
            status: 200,
            json: async () => ({ defaultGCPIdentityMode: 'passthrough' }),
          } as Response);
        }
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ items: [] }),
        } as Response);
      })
    );
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      projectId: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
    };
    page.name = 'test-agent';

    const select = gcpIdentitySelect(el);
    await chooseSelect(el, select!, 'block');
    expect(page.gcpIdentityUserSet).toBe(true);

    page.brokers = [
      ...page.brokers,
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

    // Switch to an unrelated project with its own "passthrough" default —
    // this project never had anything to do with the earlier Block pick.
    page.projectId = 'p2';
    await page.loadGCPServiceAccounts();
    await el.updateComplete;

    page.brokerId = 'broker-docker';
    await el.updateComplete;

    // p2's own default applies; the earlier project's suspended Block does
    // not resurface here.
    expect(page.gcpIdentityUserSet).toBe(false);
    expect(page.gcpMetadataMode).toBe('passthrough');

    await page.handleSubmit(new Event('submit'));
    expect(bodies).toHaveLength(1);
    expect(bodies[0].gcp_identity).toEqual({ metadata_mode: 'passthrough' });
  });

  // The project default mode is only ever assigned inside the
  // `if (settings?.defaultGCPIdentityMode)` branch — it must still be reset
  // to '' at the top of every call, or a stale "Block" default from a
  // previous project would keep being shown after switching to one with no
  // default configured at all.
  it('resets the stale Block-default hint after switching to a project with no default', async () => {
    stubFetchForKubernetesProjectDefault('block');
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & { projectId: string };
    expect(gcpIdentityHint(el)).toContain("This project's default GCP identity is Block");

    // Settings are cached per-projectId (fetchProjectSettings), so observing
    // a reset requires an actual project switch, not a second call for the
    // same project.
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString();
        if (url.includes('/api/v1/projects/p2/settings')) {
          return Promise.resolve({ ok: true, status: 200, json: async () => ({}) } as Response);
        }
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ items: [] }),
        } as Response);
      })
    );
    page.projectId = 'p2';

    await page.loadGCPServiceAccounts();
    await el.updateComplete;

    expect(gcpIdentityHint(el)).not.toContain("This project's default GCP identity is Block");
    expect(gcpIdentityHint(el)).toContain('this project has no default configured');
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
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
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
        if (url.includes('/api/v1/projects/p2/settings')) {
          return Promise.resolve({ ok: true, status: 200, json: async () => ({}) } as Response);
        }
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ items: [] }),
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
    expect(bodies[0].gcp_identity).toEqual({ metadata_mode: 'block' });
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
    // Scope check: the omission is specific to known-Kubernetes targets. A
    // docker target's existing default behavior (send the displayed mode
    // explicitly) must be unaffected.
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
    // untouched mismatch.
    page.defaultGcpMetadataMode = 'assign';
    page.gcpMetadataMode = 'assign';
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

  // An explicit "Block" pick on one broker must not survive as an
  // auto-substituted explicit "passthrough" once the user switches to a
  // Kubernetes broker — that value was never chosen for the new target.
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

    // Switching back to the docker broker must restore "block" — the
    // Kubernetes display substitution is reversible, not a one-way street
    // that leaves a stale "passthrough" nobody chose in place.
    page.brokerId = 'broker-docker';
    await el.updateComplete;

    expect(page.gcpMetadataMode).toBe('block');

    await page.handleSubmit(new Event('submit'));
    expect(tracker.bodies).toHaveLength(2);
    expect(tracker.bodies[1].gcp_identity).toEqual({ metadata_mode: 'block' });
  });

  // The display-only Kubernetes substitution (block -> passthrough) must be
  // reversed when the target later becomes non-Kubernetes again — otherwise
  // submit sends an explicit "passthrough" nobody chose, where main would
  // have sent "block" (the component's own placeholder default here, since
  // there is no project default configured).
  it('restores block after switching from a Kubernetes broker back to docker, with no project default', async () => {
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
      {
        id: 'broker-docker',
        name: 'docker-broker',
        status: 'online',
        profiles: [{ name: 'default', type: 'docker', available: true }],
      },
    ];
    page.brokerId = 'broker-k8s';
    await el.updateComplete;
    expect(page.gcpMetadataMode).toBe('passthrough');

    page.brokerId = 'broker-docker';
    await el.updateComplete;

    expect(page.gcpMetadataMode).toBe('block');

    await page.handleSubmit(new Event('submit'));
    expect(tracker.bodies).toHaveLength(1);
    expect(tracker.bodies[0].gcp_identity).toEqual({ metadata_mode: 'block' });
  });

  it('restores the project default of block after switching from a Kubernetes broker back to docker', async () => {
    const { bodies } = stubFetchForKubernetesProjectDefault('block');
    const el = await mountAgentCreate();
    const page = internals(el) as AgentCreateInternals & {
      name: string;
      handleSubmit: (e: Event, provisionOnly?: boolean) => Promise<void>;
    };
    page.name = 'test-agent';
    expect(page.brokerId).toBe('broker-k8s');
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

    expect(page.gcpMetadataMode).toBe('block');

    await page.handleSubmit(new Event('submit'));
    expect(bodies).toHaveLength(1);
    expect(bodies[0].gcp_identity).toEqual({ metadata_mode: 'block' });
  });

  // Profile-only variant: the same restoration must happen when only the
  // profile changes on a mixed broker (Kubernetes profile -> docker
  // profile), not just on a broker switch.
  it('restores block when only the profile changes back to a non-Kubernetes one on a mixed broker', async () => {
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
    expect(page.gcpMetadataMode).toBe('passthrough');

    page.profile = 'docker-profile';
    await el.updateComplete;

    expect(page.gcpMetadataMode).toBe('block');

    await page.handleSubmit(new Event('submit'));
    expect(tracker.bodies).toHaveLength(1);
    expect(tracker.bodies[0].gcp_identity).toEqual({ metadata_mode: 'block' });
  });
}, 15_000);
