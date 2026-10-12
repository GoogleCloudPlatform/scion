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
 * The GCP identity picker on the agent Configure page for Kubernetes
 * runtime targets. ptone/scion#4077 reverses Phase 2 of ptone/scion#2328:
 * brokers accept Block on Kubernetes, so Block is offered there and the help
 * text says what it means.
 *
 * The target is reliably known from the agent's own runtimeBrokerId and
 * appliedConfig.profile, loaded via GET /api/v1/runtime-brokers/{id} — the
 * shared runtime-kind helpers classify it, same as agent-create.ts.
 *
 * Unlike agent-create (a pure create flow), this page edits an EXISTING
 * agent that may already have a real stored identity. This file pins:
 *  - Block is enabled on every target, and an explicit Block is sent.
 *  - A stored value is never migrated, and a Save of an untouched stored
 *    value must not rewrite it.
 *  - Nothing is sent at all (gcp_identity omitted from the PATCH) unless the
 *    user explicitly changed the picker — omitting it is a true no-op on the
 *    server, and sending an explicit "passthrough" instead would route the
 *    request through the Hub's passthrough ownership gate for a request that
 *    never asked for passthrough.
 */

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let ScionPageAgentConfigure: any;

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { KUBERNETES_BLOCK_HINT } from '../../shared/gcp-identity-state.js';

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
  serviceAccounts?: GCPServiceAccountFixture[];
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

    if (path.includes('/gcp-service-accounts')) {
      return Promise.resolve(
        new Response(JSON.stringify({ items: opts?.serviceAccounts ?? [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    // Catch-all: settings/public, etc.
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
  serviceAccounts?: GCPServiceAccountFixture[];
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

function gcpServiceAccountSelect(el: HTMLElement): Element | null {
  return el.shadowRoot?.querySelector('#gcp-sa') ?? null;
}

function blockOption(el: HTMLElement): Element | null {
  return gcpIdentitySelect(el)?.querySelector('sl-option[value="block"]') ?? null;
}

/** Whitespace-normalized: Lit template literals preserve literal newlines/indentation verbatim in textContent. */
function gcpIdentityHelpTextSlot(el: HTMLElement): string {
  const raw = gcpIdentitySelect(el)?.querySelector('[slot="help-text"]')?.textContent ?? '';
  return raw.replace(/\s+/g, ' ').trim();
}

/** Simulates choosing an option in an sl-select (Shoelace is not registered under happy-dom). */
async function chooseSelect(
  el: { updateComplete: Promise<unknown> },
  select: Element,
  value: string
): Promise<void> {
  (select as HTMLElement & { value: string }).value = value;
  select.dispatchEvent(new Event('sl-change', { bubbles: true, composed: true }));
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
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
    scopeId: 'proj-1',
    email: `${id}@example.iam.gserviceaccount.com`,
    projectId: 'gcp-proj',
    displayName: '',
    defaultScopes: [],
    verified: true,
    verifiedAt: '2026-01-01T00:00:00Z',
    createdBy: 'user-1',
  };
}

interface AgentConfigureInternals {
  gcpMetadataMode: string;
  gcpServiceAccountId: string;
  gcpServiceAccounts: GCPServiceAccountFixture[];
  gcpIdentityUserSet: boolean;
  gcpMetadataModeFromStorage: boolean;
  error: string | null;
  handleSave: () => Promise<void>;
  handleStart: () => Promise<void>;
  updateComplete: Promise<unknown>;
}

describe('agent-configure: GCP identity on a Kubernetes target (Block offered)', () => {
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

  it('offers Block with the Kubernetes Block hint for a kubernetes-only broker', async () => {
    element = await createComponent(
      createFetchHandler({
        brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
      })
    );

    const option = blockOption(element);
    expect(option).not.toBeNull();
    expect(option!.hasAttribute('disabled')).toBe(false);
    expect(option!.hasAttribute('title')).toBe(false);
    expect(gcpIdentityHelpTextSlot(element)).toContain(KUBERNETES_BLOCK_HINT);
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
    expect(gcpIdentityHelpTextSlot(element)).toBe('');
  });

  // A stored "block" displays exactly as stored on a known-Kubernetes target.
  it('keeps a stored "block" selected on a known-Kubernetes target', async () => {
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
    expect(gcpIdentityHelpTextSlot(element)).toContain('current identity is "Block"');
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

  it('sends an explicit Block on Save once the user picks it for a known-Kubernetes target', async () => {
    const { handler, patchBodies } = createFetchHandlerCapturingPatch({
      agent: makeAgent({
        appliedConfig: { profile: '', gcpIdentity: { metadataMode: 'passthrough' } },
      }),
      brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
    });
    element = await createComponent(handler);
    const page = element as unknown as AgentConfigureInternals;

    await chooseSelect(page, gcpIdentitySelect(element)!, 'block');
    expect(page.gcpIdentityUserSet).toBe(true);

    await page.handleSave();

    expect(page.error).toBeNull();
    expect(patchBodies).toHaveLength(1);
    expect(patchBodies[0].gcp_identity).toEqual({ metadata_mode: 'block' });
  });

  it('sends an explicit Block on Start too, for a known-Kubernetes target', async () => {
    const { handler, patchBodies } = createFetchHandlerCapturingPatch({
      brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
    });
    element = await createComponent(handler);
    const page = element as unknown as AgentConfigureInternals;

    await chooseSelect(page, gcpIdentitySelect(element)!, 'block');

    await page.handleStart();

    expect(page.error).toBeNull();
    expect(patchBodies.length).toBeGreaterThanOrEqual(1);
    expect(patchBodies[0].gcp_identity).toEqual({ metadata_mode: 'block' });
  });

  // The page must use appliedConfig.profile (not an empty string) to resolve
  // the target on a broker whose profiles mix runtime types.
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

    expect(blockOption(element)!.hasAttribute('disabled')).toBe(false);
    expect(gcpIdentityHelpTextSlot(element)).toContain(KUBERNETES_BLOCK_HINT);
  });

  it('shows no Kubernetes Block hint on a mixed-profile broker when the chosen profile is not Kubernetes', async () => {
    element = await createComponent(
      createFetchHandler({
        agent: makeAgent({ appliedConfig: { profile: 'docker-profile' } }),
        brokerProfiles: [
          { name: 'k8s-profile', type: 'kubernetes', available: true },
          { name: 'docker-profile', type: 'docker', available: true },
        ],
      })
    );

    expect(blockOption(element)!.hasAttribute('disabled')).toBe(false);
    expect(gcpIdentityHelpTextSlot(element)).toBe('');
  });

  it('omits gcp_identity on Save when nothing is stored and nothing was chosen, for a known-Kubernetes target', async () => {
    const { handler, patchBodies } = createFetchHandlerCapturingPatch({
      brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
    });
    element = await createComponent(handler);
    const page = element as unknown as AgentConfigureInternals;

    expect(page.gcpIdentityUserSet).toBe(false);
    // Display only: the broker's Kubernetes runtime default, not a choice.
    expect(page.gcpMetadataMode).toBe('passthrough');

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

  // The omit-when-untouched rule is scoped to a known-Kubernetes target
  // (matching agent-create.ts) — there is no passthrough-gate or
  // block-migration concern to avoid on a non-Kubernetes runtime, so this
  // page must keep sending gcp_identity explicitly there, exactly as it did
  // before gcpIdentityUserSet existed.
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

    expect(page.error).toBeNull();
    expect(patchBodies).toHaveLength(1);
    expect(patchBodies[0].gcp_identity).toEqual({ metadata_mode: 'passthrough' });
  });

  // Changing only the service account must not be silently dropped on Save:
  // the SA select's own @sl-change handler must set gcpIdentityUserSet, or
  // buildGCPIdentityPayload treats the change as untouched and omits
  // gcp_identity, leaving the agent on its old service account. This drives
  // the real #gcp-sa picker rather than setting the internal flag directly.
  it('sends the new service account on Save after changing only the SA picker', async () => {
    const { handler, patchBodies } = createFetchHandlerCapturingPatch({
      agent: makeAgent({
        appliedConfig: {
          profile: '',
          gcpIdentity: { metadataMode: 'assign', serviceAccountId: 'sa-a' },
        },
      }),
      brokerProfiles: [{ name: 'default', type: 'docker', available: true }],
      serviceAccounts: [makeServiceAccount('sa-a'), makeServiceAccount('sa-b')],
    });
    element = await createComponent(handler);
    const page = element as unknown as AgentConfigureInternals;

    expect(page.gcpIdentityUserSet).toBe(false);
    expect(page.gcpServiceAccountId).toBe('sa-a');

    const saSelect = gcpServiceAccountSelect(element);
    expect(saSelect).not.toBeNull();
    await chooseSelect(element as unknown as AgentConfigureInternals, saSelect!, 'sa-b');

    expect(page.gcpIdentityUserSet).toBe(true);

    await page.handleSave();

    expect(page.error).toBeNull();
    expect(patchBodies).toHaveLength(1);
    expect(patchBodies[0].gcp_identity).toEqual({
      metadata_mode: 'assign',
      service_account_id: 'sa-b',
    });
  });

  it('sets gcpIdentityUserSet when the mode select actually changes', async () => {
    element = await createComponent(
      createFetchHandler({
        brokerProfiles: [{ name: 'default', type: 'docker', available: true }],
      })
    );
    const page = element as unknown as AgentConfigureInternals;
    expect(page.gcpIdentityUserSet).toBe(false);

    const select = gcpIdentitySelect(element);
    expect(select).not.toBeNull();
    await chooseSelect(element as unknown as AgentConfigureInternals, select!, 'passthrough');

    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('passthrough');
  });

  // Mirrors the existing Save resave test, for Start instead.
  it('resaves a stored "block" unchanged via Start too, without sending gcp_identity', async () => {
    const { handler, patchBodies } = createFetchHandlerCapturingPatch({
      agent: makeAgent({
        appliedConfig: { profile: '', gcpIdentity: { metadataMode: 'block' } },
      }),
      brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
    });
    element = await createComponent(handler);
    const page = element as unknown as AgentConfigureInternals;

    await page.handleStart();

    expect(page.error).toBeNull();
    expect(patchBodies).toHaveLength(1);
    expect(patchBodies[0]).not.toHaveProperty('gcp_identity');
  });

  // The untouched hint must name the real effective identity, not overclaim
  // the broker's own default applies when a stored value (or nothing at all)
  // is actually in effect.
  describe('untouched hint text names the real effective identity', () => {
    it('names the stored mode when a real identity is stored', async () => {
      element = await createComponent(
        createFetchHandler({
          agent: makeAgent({
            appliedConfig: { profile: '', gcpIdentity: { metadataMode: 'passthrough' } },
          }),
          brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
        })
      );

      const text = gcpIdentityHelpTextSlot(element);
      expect(text).toContain('current identity is "Passthrough"');
      expect(text).not.toContain("broker's own Kubernetes default applies automatically");
    });

    it('says the broker default applies only when nothing is stored at all', async () => {
      element = await createComponent(
        createFetchHandler({
          brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
        })
      );

      const text = gcpIdentityHelpTextSlot(element);
      expect(text).toContain("broker's own Kubernetes default applies automatically");
      expect(text).not.toContain('current identity is');
    });

    it('drops the untouched explanation once the user has made a choice', async () => {
      element = await createComponent(
        createFetchHandler({
          brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
        })
      );
      const page = element as unknown as AgentConfigureInternals;
      const select = gcpIdentitySelect(element);
      await chooseSelect(element as unknown as AgentConfigureInternals, select!, 'passthrough');
      expect(page.gcpIdentityUserSet).toBe(true);

      const text = gcpIdentityHelpTextSlot(element);
      expect(text).not.toContain("broker's own Kubernetes default applies automatically");
      expect(text).not.toContain('current identity is');
      expect(text).toBe(KUBERNETES_BLOCK_HINT);
    });
  });

  // The target broker loads asynchronously, so a user can pick "Block" while
  // it is still unknown. Block is valid on Kubernetes, so that explicit pick
  // stands once the broker resolves as Kubernetes-only.
  it('keeps an explicit "block" pick when the broker resolves as Kubernetes-only afterwards', async () => {
    let resolveBroker: ((value: Response) => void) | null = null;
    const brokerPromise = new Promise<Response>((resolve) => {
      resolveBroker = resolve;
    });
    const handler = (url: string | URL | Request): Promise<Response> => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.match(/\/api\/v1\/runtime-brokers\/[^/]+$/)) {
        return brokerPromise;
      }
      return createFetchHandler({ agent: makeAgent() })(url);
    };

    vi.stubGlobal('fetch', vi.fn(handler));
    try {
      Object.defineProperty(window.location, 'pathname', {
        value: '/agents/agent-1/configure',
        writable: true,
        configurable: true,
      });
    } catch {
      Object.defineProperty(window, 'location', {
        value: { ...window.location, pathname: '/agents/agent-1/configure' },
        writable: true,
        configurable: true,
      });
    }
    element = new ScionPageAgentConfigure();
    document.body.appendChild(element);
    await (element as unknown as AgentConfigureInternals).updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 100));
    await (element as unknown as AgentConfigureInternals).updateComplete;

    const page = element as unknown as AgentConfigureInternals;
    // The broker fetch is still pending: the target is unknown.
    const select = gcpIdentitySelect(element);
    expect(select!.hasAttribute('disabled')).toBe(false);
    await chooseSelect(page, select!, 'block');
    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('block');

    // Now the broker resolves as Kubernetes-only.
    resolveBroker!(
      new Response(
        JSON.stringify({
          id: 'broker-1',
          name: 'broker-1',
          profiles: [{ name: 'default', type: 'kubernetes', available: true }],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await new Promise((resolve) => setTimeout(resolve, 50));
    await page.updateComplete;

    expect(page.gcpIdentityUserSet).toBe(true);
    expect(page.gcpMetadataMode).toBe('block');
    expect(gcpIdentityHelpTextSlot(element)).toBe(KUBERNETES_BLOCK_HINT);
  });
}, 15_000);
