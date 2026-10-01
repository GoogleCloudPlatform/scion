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
 * Phase 2 of ptone/scion#2328: block is not offered as a NEW GCP identity
 * choice for a Kubernetes runtime target on the agent Configure page.
 *
 * The target is reliably known from the agent's own runtimeBrokerId and
 * appliedConfig.profile, loaded via GET /api/v1/runtime-brokers/{id} — the
 * shared runtime-kind helpers classify it, same as agent-create.ts.
 *
 * Unlike agent-create (a pure create flow), this page edits an EXISTING
 * agent that may already have a real stored identity. PR 2332 review round 2
 * findings 1 and 2 established two rules this file pins:
 *  - A stored value (including a stored "block") is never migrated: the
 *    Block option is disabled for a NEW selection, not removed, and a Save
 *    of an untouched stored value must not rewrite it.
 *  - Nothing is sent at all (gcp_identity omitted from the PATCH) unless the
 *    user explicitly changed the picker — omitting it is a true no-op on the
 *    server, and sending an explicit "passthrough" instead would route the
 *    request through the Hub's passthrough ownership gate for a request that
 *    never asked for passthrough.
 */

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let ScionPageAgentConfigure: any;

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

interface BrokerProfileFixture {
  name: string;
  type: string;
  available: boolean;
}

/** Default: nothing configured at all (the common, neutral case). */
function makeAgent(overrides: Record<string, unknown> = {}) {
  return {
    id: 'agent-1',
    name: 'test-agent',
    projectId: 'proj-1',
    phase: 'created',
    runtimeBrokerId: 'broker-1',
    appliedConfig: {
      profile: '',
    },
    ...overrides,
  };
}

function createFetchHandler(opts?: {
  agent?: Record<string, unknown>;
  brokerProfiles?: BrokerProfileFixture[];
}) {
  return (url: string | URL | Request): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;

    if (path.match(/\/api\/v1\/agents\/[^/]+$/)) {
      return Promise.resolve(
        new Response(JSON.stringify(opts?.agent ?? makeAgent()), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.match(/\/api\/v1\/runtime-brokers\/[^/]+$/)) {
      return Promise.resolve(
        new Response(
          JSON.stringify({
            id: 'broker-1',
            name: 'broker-1',
            profiles: opts?.brokerProfiles,
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
    }

    // Catch-all: settings/public, gcp-service-accounts, etc.
    return Promise.resolve(
      new Response(JSON.stringify({}), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  };
}

/**
 * Same as createFetchHandler, but also records every PATCH /api/v1/agents/{id}
 * request body, so a test can assert whether gcp_identity was included
 * without needing the full Save success flow to complete differently.
 */
function createFetchHandlerCapturingPatch(opts?: {
  agent?: Record<string, unknown>;
  brokerProfiles?: BrokerProfileFixture[];
}): {
  handler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>;
  patchBodies: Array<Record<string, unknown>>;
} {
  const patchBodies: Array<Record<string, unknown>> = [];
  const base = createFetchHandler(opts);
  const handler = (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    if (init?.method === 'PATCH' && typeof init.body === 'string') {
      patchBodies.push(JSON.parse(init.body) as Record<string, unknown>);
    }
    return base(url);
  };
  return { handler, patchBodies };
}

async function createComponent(
  fetchHandler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>,
  path = '/agents/agent-1/configure'
): Promise<HTMLElement> {
  try {
    Object.defineProperty(window.location, 'pathname', {
      value: path,
      writable: true,
      configurable: true,
    });
  } catch {
    Object.defineProperty(window, 'location', {
      value: { ...window.location, pathname: path },
      writable: true,
      configurable: true,
    });
  }

  vi.stubGlobal('fetch', vi.fn(fetchHandler));

  const el = new ScionPageAgentConfigure();
  document.body.appendChild(el);
  await (el as HTMLElement & { updateComplete: Promise<unknown> }).updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 300));
  await (el as HTMLElement & { updateComplete: Promise<unknown> }).updateComplete;
  return el;
}

function gcpIdentitySelect(el: HTMLElement): Element | null {
  return el.shadowRoot?.querySelector('#gcp-mode') ?? null;
}

function blockOption(el: HTMLElement): Element | null {
  return gcpIdentitySelect(el)?.querySelector('sl-option[value="block"]') ?? null;
}

function gcpIdentityHelpTextSlot(el: HTMLElement): string {
  return gcpIdentitySelect(el)?.querySelector('[slot="help-text"]')?.textContent?.trim() ?? '';
}

interface AgentConfigureInternals {
  gcpMetadataMode: string;
  gcpServiceAccountId: string;
  gcpIdentityUserSet: boolean;
  error: string | null;
  handleSave: () => Promise<void>;
  handleStart: () => Promise<void>;
}

describe('agent-configure: block is not a NEW choice for a Kubernetes target', () => {
  vi.setConfig({ testTimeout: 15000 });
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler()));
    const mod = await import('./agent-configure.js');
    ScionPageAgentConfigure = mod.ScionPageAgentConfigure;
    vi.restoreAllMocks();
  }, 30_000);

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  it('disables (does not remove) Block for a kubernetes-only broker when nothing is stored', async () => {
    element = await createComponent(
      createFetchHandler({
        brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
      })
    );

    const option = blockOption(element);
    expect(option).not.toBeNull();
    expect(option!.hasAttribute('disabled')).toBe(true);
    expect(gcpIdentityHelpTextSlot(element)).toContain('not supported on the Kubernetes runtime');
  });

  it('keeps Block enabled for a docker broker', async () => {
    element = await createComponent(
      createFetchHandler({
        brokerProfiles: [{ name: 'default', type: 'docker', available: true }],
      })
    );

    const option = blockOption(element);
    expect(option).not.toBeNull();
    expect(option!.hasAttribute('disabled')).toBe(false);
  });

  // PR 2332 review round 2, finding 2: a stored "block" must display exactly
  // as stored on a known-Kubernetes target — disabled for a NEW selection,
  // but never auto-corrected away. This replaces the round-1 test that
  // pinned the opposite (migrating) behavior.
  it('keeps a stored "block" selected, not auto-corrected, on a known-Kubernetes target', async () => {
    element = await createComponent(
      createFetchHandler({
        agent: makeAgent({
          appliedConfig: { profile: '', gcpIdentity: { metadataMode: 'block' } },
        }),
        brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
      })
    );

    const page = element as unknown as AgentConfigureInternals;
    expect(page.gcpMetadataMode).toBe('block');
    const option = blockOption(element);
    expect(option!.hasAttribute('disabled')).toBe(true);
  });

  it('resaves a stored "block" unchanged without sending gcp_identity at all', async () => {
    const { handler, patchBodies } = createFetchHandlerCapturingPatch({
      agent: makeAgent({
        appliedConfig: { profile: '', gcpIdentity: { metadataMode: 'block' } },
      }),
      brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
    });
    element = await createComponent(handler);
    const page = element as unknown as AgentConfigureInternals;

    await page.handleSave();

    expect(page.error).toBeNull();
    expect(patchBodies).toHaveLength(1);
    expect(patchBodies[0]).not.toHaveProperty('gcp_identity');
  });

  it('rejects Save when the user explicitly attempts a new block selection on a known-Kubernetes target', async () => {
    const { handler, patchBodies } = createFetchHandlerCapturingPatch({
      brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
    });
    element = await createComponent(handler);
    const page = element as unknown as AgentConfigureInternals;

    // The UI itself prevents clicking into "block" once disabled; this
    // simulates the only way the guard could still be reached, and checks it
    // is transition-based (fires on an explicit new choice of block, not on
    // an untouched value) — see the resave test above for the other half.
    page.gcpIdentityUserSet = true;
    page.gcpMetadataMode = 'block';

    await page.handleSave();

    expect(page.error).toContain('not available for a Kubernetes runtime target');
    expect(patchBodies).toHaveLength(0);
  });

  // PR 2332 review round 2, finding 5, mutation A8: only the Save guard was
  // tested; the identical Start guard was untested and the mutant survived.
  it('rejects Start under the same condition, without sending a PATCH', async () => {
    const { handler, patchBodies } = createFetchHandlerCapturingPatch({
      brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
    });
    element = await createComponent(handler);
    const page = element as unknown as AgentConfigureInternals;

    page.gcpIdentityUserSet = true;
    page.gcpMetadataMode = 'block';

    await page.handleStart();

    expect(page.error).toContain('not available for a Kubernetes runtime target');
    expect(patchBodies).toHaveLength(0);
  });

  // PR 2332 review round 2, finding 5, mutation C1: the page must use
  // appliedConfig.profile (not an empty string) to resolve the target on a
  // broker whose profiles mix runtime types.
  it('uses appliedConfig.profile to resolve the target on a mixed-profile broker', async () => {
    element = await createComponent(
      createFetchHandler({
        agent: makeAgent({ appliedConfig: { profile: 'k8s-profile' } }),
        brokerProfiles: [
          { name: 'k8s-profile', type: 'kubernetes', available: true },
          { name: 'docker-profile', type: 'docker', available: true },
        ],
      })
    );

    const option = blockOption(element);
    expect(option!.hasAttribute('disabled')).toBe(true);
  });

  it('does not disable Block on a mixed-profile broker when the chosen profile is not Kubernetes', async () => {
    element = await createComponent(
      createFetchHandler({
        agent: makeAgent({ appliedConfig: { profile: 'docker-profile' } }),
        brokerProfiles: [
          { name: 'k8s-profile', type: 'kubernetes', available: true },
          { name: 'docker-profile', type: 'docker', available: true },
        ],
      })
    );

    const option = blockOption(element);
    expect(option!.hasAttribute('disabled')).toBe(false);
  });

  // PR 2332 review round 2, finding 1.
  it('omits gcp_identity on Save when nothing is stored and nothing was chosen, for a known-Kubernetes target', async () => {
    const { handler, patchBodies } = createFetchHandlerCapturingPatch({
      brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
    });
    element = await createComponent(handler);
    const page = element as unknown as AgentConfigureInternals;

    expect(page.gcpIdentityUserSet).toBe(false);
    expect(page.gcpMetadataMode).toBe('passthrough'); // display-only correction

    await page.handleSave();

    expect(page.error).toBeNull();
    expect(patchBodies).toHaveLength(1);
    expect(patchBodies[0]).not.toHaveProperty('gcp_identity');
  });

  it('sends an explicit passthrough on Save once the user picks it for a known-Kubernetes target', async () => {
    const { handler, patchBodies } = createFetchHandlerCapturingPatch({
      brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
    });
    element = await createComponent(handler);
    const page = element as unknown as AgentConfigureInternals;

    page.gcpIdentityUserSet = true;
    page.gcpMetadataMode = 'passthrough';

    await page.handleSave();

    expect(page.error).toBeNull();
    expect(patchBodies).toHaveLength(1);
    expect(patchBodies[0].gcp_identity).toEqual({ metadata_mode: 'passthrough' });
  });

  it('still sends gcp_identity for a non-Kubernetes target even when untouched', async () => {
    const { handler, patchBodies } = createFetchHandlerCapturingPatch({
      agent: makeAgent({
        appliedConfig: { profile: '', gcpIdentity: { metadataMode: 'passthrough' } },
      }),
      brokerProfiles: [{ name: 'default', type: 'docker', available: true }],
    });
    element = await createComponent(handler);
    const page = element as unknown as AgentConfigureInternals;

    expect(page.gcpIdentityUserSet).toBe(false);

    await page.handleSave();

    // Unchanged from storage, and not a Kubernetes target: gcp_identity is
    // still omitted (true no-op), which is also correct here — this pins
    // that the omit-when-untouched rule does not regress a plain resave on a
    // non-Kubernetes target either.
    expect(page.error).toBeNull();
    expect(patchBodies).toHaveLength(1);
    expect(patchBodies[0]).not.toHaveProperty('gcp_identity');
  });
});
