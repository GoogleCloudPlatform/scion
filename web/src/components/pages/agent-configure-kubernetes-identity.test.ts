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
 * for a Kubernetes runtime target on the agent Configure page (PR 2332 review
 * round 1, finding 3 — this page was not covered in the first pass).
 *
 * The target is reliably known from the agent's own runtimeBrokerId and
 * appliedConfig.profile, loaded via GET /api/v1/runtime-brokers/{id}. This
 * mirrors agent-create.ts's treatment via the shared runtime-kind helpers.
 */

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let ScionPageAgentConfigure: any;

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

interface BrokerProfileFixture {
  name: string;
  type: string;
  available: boolean;
}

function makeAgent(overrides: Record<string, unknown> = {}) {
  return {
    id: 'agent-1',
    name: 'test-agent',
    projectId: 'proj-1',
    phase: 'created',
    runtimeBrokerId: 'broker-1',
    appliedConfig: {
      profile: '',
      gcpIdentity: { metadataMode: 'block' },
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

function gcpIdentityHint(el: HTMLElement): string {
  const select = gcpIdentitySelect(el);
  return select?.parentElement?.querySelector('.hint')?.textContent?.trim() ?? '';
}

describe('agent-configure: block is not offered for a Kubernetes target', () => {
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

  it('hides Block when the agent is dispatched to a kubernetes-only broker', async () => {
    element = await createComponent(
      createFetchHandler({
        brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
      })
    );

    const select = gcpIdentitySelect(element);
    expect(select).not.toBeNull();
    expect(select!.querySelector('sl-option[value="block"]')).toBeNull();
    expect(gcpIdentityHint(element)).toContain('not available for a Kubernetes runtime target');
  });

  it('keeps Block offered for a docker broker', async () => {
    element = await createComponent(
      createFetchHandler({
        brokerProfiles: [{ name: 'default', type: 'docker', available: true }],
      })
    );

    const select = gcpIdentitySelect(element);
    expect(select!.querySelector('sl-option[value="block"]')).not.toBeNull();
  });

  it('corrects a stored "block" mode away once the target broker loads as known-Kubernetes', async () => {
    // The agent's stored mode is block (set before this restriction existed);
    // the broker fetch resolves asynchronously, after populateForm has
    // already read that stored value into state.
    element = await createComponent(
      createFetchHandler({
        agent: makeAgent({
          appliedConfig: { profile: '', gcpIdentity: { metadataMode: 'block' } },
        }),
        brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
      })
    );

    const internals = element as unknown as { gcpMetadataMode: string };
    expect(internals.gcpMetadataMode).not.toBe('block');
  });

  it('rejects Save when the mode is block for a known-Kubernetes target, without sending a PATCH', async () => {
    const calls: string[] = [];
    const handler = (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      calls.push(`${init?.method ?? 'GET'} ${path}`);
      return createFetchHandler({
        brokerProfiles: [{ name: 'default', type: 'kubernetes', available: true }],
      })(url);
    };
    element = await createComponent(handler);

    const page = element as unknown as {
      gcpMetadataMode: string;
      error: string | null;
      handleSave: () => Promise<void>;
    };
    // Force the mode back to block synchronously (no intervening await), to
    // exercise the save-time guard directly rather than the reactive
    // correction that would otherwise fix it first.
    page.gcpMetadataMode = 'block';

    calls.length = 0;
    await page.handleSave();

    expect(page.error).toContain('not available for a Kubernetes runtime target');
    expect(calls.some((c) => c.startsWith('PATCH'))).toBe(false);
  });
});
