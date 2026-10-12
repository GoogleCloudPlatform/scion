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
 * ptone/scion#4391: the agent Configure page's service-account picker shows
 * each account's mapping state for the agent's Kubernetes target (its
 * runtime broker and profile), with the same rules as the Create Agent
 * picker.
 *
 * Pinned here:
 *  - the mapping state is asked for only once the broker is known to be
 *    Kubernetes and the agent has a profile; the labels are the create
 *    picker's ("mapped", never "ready"), not-mapped accounts come last, and
 *    nothing is removed;
 *  - the selected account's hub message is the help text, styled as a
 *    warning when the account is not mapped;
 *  - whichever of the plain list and the mapping-state list lands last, the
 *    labels for the agent's target are kept (stale-response guard);
 *  - a refused mapping-state request, or a hub without assignStatus, leaves
 *    the plain picker it had before.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import type { GCPServiceAccountAssignStatus } from '../../shared/types.js';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let ScionPageAgentConfigure: any;

type MountedEl = HTMLElement & { updateComplete: Promise<unknown> };

const K8S_PROFILES = [
  { name: 'k8s-a', type: 'kubernetes', available: true },
  { name: 'k8s-b', type: 'kubernetes', available: true },
];

const DOCKER_PROFILES = [{ name: 'local', type: 'docker', available: true }];

function makeAgent(profile: string, serviceAccountId = 'sa-a') {
  return {
    id: 'agent-1',
    name: 'test-agent',
    projectId: 'proj-1',
    phase: 'created',
    runtimeBrokerId: 'broker-1',
    appliedConfig: {
      profile,
      gcpIdentity: { metadataMode: 'assign', serviceAccountId },
    },
  };
}

function account(id: string, assignStatus?: GCPServiceAccountAssignStatus) {
  return {
    id,
    scope: 'project',
    scopeId: 'proj-1',
    email: `${id}@example.com`,
    projectId: 'gcp-proj',
    displayName: '',
    defaultScopes: [],
    verified: true,
    verifiedAt: '2026-01-01T00:00:00Z',
    createdBy: 'user-1',
    createdAt: '2026-01-01T00:00:00Z',
    ...(assignStatus ? { assignStatus } : {}),
  };
}

/** What the hub reports per profile, keyed by profile name. */
const STATUS_BY_PROFILE: Record<string, Record<string, GCPServiceAccountAssignStatus>> = {
  'k8s-a': {
    'sa-a': { state: 'not_mapped', message: 'Not mapped: test message A.' },
    'sa-b': { state: 'mapped', message: 'Mapped: test message B.' },
    'sa-c': { state: 'unknown', reason: 'report_stale', message: 'Unknown: test message C.' },
  },
  'k8s-b': {
    'sa-a': { state: 'mapped', message: 'Mapped: test message A2.' },
    'sa-b': { state: 'not_required', message: 'No mapping needed: test message B2.' },
    'sa-c': { state: 'unknown', reason: 'report_old_version', message: 'Unknown: C2.' },
  },
};

type HeldKind = 'broker' | 'list-plain' | 'list-assign';

interface StubOpts {
  agent: ReturnType<typeof makeAgent>;
  brokerProfiles?: typeof K8S_PROFILES;
  olderHub?: boolean;
  denyAssignStatus?: boolean;
  /** Responses of these kinds wait in held until the test releases them. */
  hold?: HeldKind[];
}

/**
 * Routes the page's fetches. The service-account list answers with the
 * mapping state for the requested profile, unless olderHub, which ignores
 * the parameters as a hub that predates them does. With denyAssignStatus a
 * list that asks for the mapping state is refused (403). Records every list
 * URL.
 */
function stubFetch(opts: StubOpts): {
  listUrls: URL[];
  held: Array<{ kind: HeldKind; release: () => void }>;
} {
  const listUrls: URL[] = [];
  const held: Array<{ kind: HeldKind; release: () => void }> = [];
  const respond = (kind: HeldKind | null, body: unknown, status = 200): Promise<Response> => {
    const response = new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });
    if (!kind || !opts.hold?.includes(kind)) return Promise.resolve(response);
    return new Promise<Response>((resolve) => {
      held.push({ kind, release: () => resolve(response) });
    });
  };
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const raw = requestUrl(input);
      const url = new URL(raw, 'http://hub.example.com');
      if (/\/api\/v1\/agents\/[^/]+$/.test(url.pathname)) return respond(null, opts.agent);
      if (/\/api\/v1\/runtime-brokers\/[^/]+$/.test(url.pathname)) {
        return respond('broker', {
          id: 'broker-1',
          name: 'broker-1',
          profiles: opts.brokerProfiles ?? K8S_PROFILES,
        });
      }
      if (url.pathname.endsWith('/gcp-service-accounts')) {
        listUrls.push(url);
        const asks = url.searchParams.has('assignStatus');
        const kind: HeldKind = asks ? 'list-assign' : 'list-plain';
        if (asks && opts.denyAssignStatus) {
          return respond(kind, { error: { message: 'forbidden' } }, 403);
        }
        const profile = url.searchParams.get('profile') ?? '';
        const statuses: Record<string, GCPServiceAccountAssignStatus> =
          opts.olderHub || !asks ? {} : (STATUS_BY_PROFILE[profile] ?? {});
        return respond(kind, {
          items: ['sa-a', 'sa-b', 'sa-c'].map((id) => account(id, statuses[id])),
        });
      }
      return respond(null, {});
    })
  );
  return { listUrls, held };
}

async function settle(el: MountedEl): Promise<void> {
  for (let i = 0; i < 3; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mount(): Promise<MountedEl> {
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
  const el = new ScionPageAgentConfigure() as MountedEl;
  document.body.appendChild(el);
  await settle(el);
  return el;
}

async function release(
  el: MountedEl,
  held: Array<{ kind: HeldKind; release: () => void }>,
  kind: HeldKind
): Promise<void> {
  const i = held.findIndex((h) => h.kind === kind);
  if (i < 0) throw new Error(`no held response of kind "${kind}"`);
  const [h] = held.splice(i, 1);
  h.release();
  await settle(el);
}

function saSelect(el: MountedEl): Element {
  const select = el.shadowRoot?.querySelector('#gcp-sa');
  if (!select) throw new Error('Service Account select not rendered');
  return select;
}

/** The Service Account options as [id, text], in rendered order. */
function options(el: MountedEl): Array<[string, string]> {
  return Array.from(saSelect(el).querySelectorAll('sl-option')).map((o) => [
    o.getAttribute('value') ?? '',
    (o.textContent ?? '').replace(/\s+/g, ' ').trim(),
  ]);
}

function statusHelp(el: MountedEl): Element | null {
  return saSelect(el).querySelector('[data-testid="gcp-sa-assign-status"]');
}

const PLAIN_OPTIONS: Array<[string, string]> = [
  ['sa-a', 'sa-a@example.com'],
  ['sa-b', 'sa-b@example.com'],
  ['sa-c', 'sa-c@example.com'],
];

describe('agent-configure: service-account mapping state (ptone/scion#4391)', () => {
  let el: MountedEl | null = null;

  beforeAll(async () => {
    stubFetch({ agent: makeAgent('k8s-a') });
    const mod = await import('./agent-configure.js');
    ScionPageAgentConfigure = mod.ScionPageAgentConfigure;
    vi.restoreAllMocks();
  }, 30_000);

  afterEach(() => {
    el?.remove();
    el = null;
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("asks for the agent's broker and profile and labels every account, not-mapped last", async () => {
    const { listUrls } = stubFetch({ agent: makeAgent('k8s-a') });
    el = await mount();

    const asked = listUrls.filter((u) => u.searchParams.has('assignStatus'));
    expect(asked.length).toBeGreaterThan(0);
    const last = asked[asked.length - 1];
    expect(last.searchParams.get('profile')).toBe('k8s-a');
    expect(last.searchParams.get('broker')).toBe('broker-1');
    expect(last.searchParams.get('includeHubScoped')).toBe('true');

    expect(options(el)).toEqual([
      ['sa-b', 'sa-b@example.com — mapped'],
      ['sa-c', 'sa-c@example.com — mapping unknown: report stale'],
      ['sa-a', 'sa-a@example.com — not mapped on this profile'],
    ]);
    for (const [, text] of options(el)) expect(text).not.toMatch(/ready/i);
    const notMapped = saSelect(el).querySelector('sl-option[value="sa-a"]');
    expect(notMapped?.getAttribute('data-assign-state')).toBe('not_mapped');
    expect(notMapped?.getAttribute('title')).toBe('Not mapped: test message A.');
  });

  it("shows the selected account's message as the help text, as a warning when not mapped", async () => {
    stubFetch({ agent: makeAgent('k8s-a', 'sa-a') });
    el = await mount();
    const help = statusHelp(el);
    expect(help?.getAttribute('slot')).toBe('help-text');
    expect(help?.textContent?.trim()).toBe('Not mapped: test message A.');
    expect(help?.classList.contains('warning')).toBe(true);
  });

  it('shows a mapped selection without the warning style', async () => {
    stubFetch({ agent: makeAgent('k8s-a', 'sa-b') });
    el = await mount();
    const help = statusHelp(el);
    expect(help?.textContent?.trim()).toBe('Mapped: test message B.');
    expect(help?.classList.contains('warning')).toBe(false);
  });

  it("labels the agent's own profile: no mapping needed and an older broker", async () => {
    stubFetch({ agent: makeAgent('k8s-b', 'sa-a') });
    el = await mount();
    expect(options(el)).toEqual([
      ['sa-a', 'sa-a@example.com — mapped'],
      ['sa-b', 'sa-b@example.com — no mapping needed'],
      ['sa-c', 'sa-c@example.com — mapping unknown: older broker'],
    ]);
  });

  it('does not ask when the agent has no profile', async () => {
    const { listUrls } = stubFetch({ agent: makeAgent('') });
    el = await mount();
    expect(listUrls.length).toBeGreaterThan(0);
    expect(listUrls.some((u) => u.searchParams.has('assignStatus'))).toBe(false);
    expect(options(el)).toEqual(PLAIN_OPTIONS);
    expect(statusHelp(el)).toBeNull();
  });

  it('does not ask for a broker that is not Kubernetes', async () => {
    const { listUrls } = stubFetch({ agent: makeAgent('local'), brokerProfiles: DOCKER_PROFILES });
    el = await mount();
    expect(listUrls.some((u) => u.searchParams.has('assignStatus'))).toBe(false);
    expect(options(el)).toEqual(PLAIN_OPTIONS);
  });

  it('keeps the labels when the plain list lands after the mapping-state list', async () => {
    const { held } = stubFetch({ agent: makeAgent('k8s-a'), hold: ['list-plain'] });
    el = await mount();
    // The broker loaded and the mapping-state list landed while the first,
    // plain list was still in flight.
    expect(held.map((h) => h.kind)).toEqual(['list-plain']);
    await release(el, held, 'list-plain');
    expect(options(el)[0]).toEqual(['sa-b', 'sa-b@example.com — mapped']);
    expect(options(el)[2]).toEqual(['sa-a', 'sa-a@example.com — not mapped on this profile']);
  });

  it('labels the accounts once the broker loads after the plain list', async () => {
    const { held, listUrls } = stubFetch({ agent: makeAgent('k8s-a'), hold: ['broker'] });
    el = await mount();
    expect(listUrls.some((u) => u.searchParams.has('assignStatus'))).toBe(false);
    expect(options(el)).toEqual(PLAIN_OPTIONS);

    await release(el, held, 'broker');
    await settle(el);
    expect(listUrls.some((u) => u.searchParams.get('profile') === 'k8s-a')).toBe(true);
    expect(options(el)[0]).toEqual(['sa-b', 'sa-b@example.com — mapped']);
  });

  it('falls back to the plain list when the mapping-state request is refused', async () => {
    const { listUrls } = stubFetch({ agent: makeAgent('k8s-a'), denyAssignStatus: true });
    el = await mount();
    expect(listUrls.some((u) => u.searchParams.has('assignStatus'))).toBe(true);
    expect(options(el)).toEqual(PLAIN_OPTIONS);
    expect(statusHelp(el)).toBeNull();
  });

  it('works as before against a hub without assignStatus', async () => {
    stubFetch({ agent: makeAgent('k8s-a'), olderHub: true });
    el = await mount();
    expect(options(el)).toEqual(PLAIN_OPTIONS);
    expect(statusHelp(el)).toBeNull();
  });
});
