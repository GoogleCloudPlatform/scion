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
 * The GCP identity picker on the Edit page (ptone/scion#3975), ported from
 * the retired Configure page's Kubernetes-identity and service-account
 * mapping cases.
 *
 * The target is known from the agent's own runtimeBrokerId and
 * appliedConfig.profile (GET /api/v1/runtime-brokers/{id}), classified by
 * the shared runtime-kind helpers. Pinned here:
 *  - Block is offered on every target (ptone/scion#4077); on a Kubernetes
 *    target it is described by the Kubernetes Block hint.
 *  - The agent's stored identity is shown and never rewritten: a Save that
 *    did not pick sends no gcp_identity, on every target. (The Configure
 *    page re-sent the shown mode on non-Kubernetes targets; the Edit page
 *    sends only touched fields everywhere.)
 *  - An explicit pick is sent, on Save and on Save & Start.
 *  - The service-account picker shows each account's mapping state for the
 *    agent's Kubernetes target (ptone/scion#4391), with the stale-response
 *    and fallback rules of the shared loader.
 *  - After the first start the identity needs a reincarnation, so the
 *    picker is disabled and nothing is sent.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import { requestUrl } from '../../client/__fixtures__/request-url.js';
import { hubEditability } from '../../shared/__fixtures__/agent-editability.js';
import {
  KUBERNETES_BLOCK_HINT,
  NO_IDENTITY_MODE_HINT,
  STORED_IDENTITY_HINT,
} from '../../shared/gcp-identity-state.js';
import type { GCPServiceAccountAssignStatus } from '../../shared/types.js';
import type { ScionAgentConfigForm } from '../shared/agent-config-form.js';
import type { ScionPageAgentEdit } from './agent-edit.js';

vi.mock('../../utils/toast.js', () => ({ showToast: () => undefined }));

const K8S_PROFILES = [
  { name: 'k8s-a', type: 'kubernetes', available: true },
  { name: 'k8s-b', type: 'kubernetes', available: true },
];
const DOCKER_PROFILES = [{ name: 'local', type: 'docker', available: true }];
const MIXED_PROFILES = [
  { name: 'k8s-a', type: 'kubernetes', available: true },
  { name: 'local', type: 'docker', available: true },
];

interface AgentOpts {
  phase?: string;
  profile?: string;
  gcpIdentity?: { metadataMode: string; serviceAccountId?: string };
}

function makeAgent(opts: AgentOpts = {}) {
  const phase = opts.phase ?? 'created';
  return {
    id: 'agent-1',
    name: 'test-agent',
    projectId: 'proj-1',
    template: 't',
    phase,
    stateVersion: 3,
    runtimeBrokerId: 'broker-1',
    appliedConfig: {
      profile: opts.profile ?? '',
      ...(opts.gcpIdentity ? { gcpIdentity: opts.gcpIdentity } : {}),
    },
    editability: hubEditability(phase),
    _capabilities: { actions: ['read', 'update', 'lifecycle', 'attach'] },
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
  hold?: HeldKind[];
}

let patchBodies: Array<Record<string, unknown>> = [];
let posts: string[] = [];
let listUrls: URL[] = [];
let held: Array<{ kind: HeldKind; release: () => void }> = [];

function stubFetch(opts: StubOpts): void {
  patchBodies = [];
  posts = [];
  listUrls = [];
  held = [];
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
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(requestUrl(input), 'http://hub.example.com');
      if (init?.method === 'PATCH') {
        patchBodies.push(JSON.parse(init.body as string) as Record<string, unknown>);
        return respond(null, { disposition: { applied: [], held: [], heldForReincarnate: [] } });
      }
      if (init?.method === 'POST') {
        posts.push(url.pathname);
        return respond(null, {});
      }
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
}

async function settle(el: ScionPageAgentEdit): Promise<void> {
  for (let i = 0; i < 4; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
    const form = formOf(el);
    if (form) await form.updateComplete;
  }
}

function formOf(el: ScionPageAgentEdit): ScionAgentConfigForm | null {
  return el.shadowRoot!.querySelector('scion-agent-config-form');
}

async function mount(): Promise<ScionPageAgentEdit> {
  const el = document.createElement('scion-page-agent-edit');
  document.body.appendChild(el);
  await settle(el);
  return el;
}

function gcpField(el: ScionPageAgentEdit): HTMLElement {
  return formOf(el)!.shadowRoot!.querySelector('.field[data-key="gcp_identity"]')!;
}

function modeSelect(el: ScionPageAgentEdit): HTMLElement & { value: string } {
  return gcpField(el).querySelector('sl-select') as HTMLElement & { value: string };
}

function hint(el: ScionPageAgentEdit): string {
  return (gcpField(el).querySelector('.hint')?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

function saSelect(el: ScionPageAgentEdit): HTMLElement & { value: string } {
  const s = formOf(el)!.shadowRoot!.querySelector(
    '.field[data-key="gcp_identity.service_account"] sl-select'
  );
  if (!s) throw new Error('Service Account select not rendered');
  return s as HTMLElement & { value: string };
}

function options(el: ScionPageAgentEdit): Array<[string, string]> {
  return Array.from(saSelect(el).querySelectorAll('sl-option')).map((o) => [
    o.getAttribute('value') ?? '',
    (o.textContent ?? '').replace(/\s+/g, ' ').trim(),
  ]);
}

function statusHelp(el: ScionPageAgentEdit): Element | null {
  return saSelect(el).querySelector('[data-testid="gcp-sa-assign-status"]');
}

async function choose(el: ScionPageAgentEdit, select: HTMLElement & { value: string }, v: string) {
  select.value = v;
  select.dispatchEvent(new Event('sl-change', { bubbles: true, composed: true }));
  await settle(el);
}

/** Touches the name so a Save sends a PATCH whatever the identity does. */
async function rename(el: ScionPageAgentEdit): Promise<void> {
  const input = formOf(el)!.shadowRoot!.querySelector(
    '.field[data-key="name"] sl-input'
  ) as HTMLInputElement;
  input.value = 'renamed';
  input.dispatchEvent(new Event('sl-input'));
  await settle(el);
}

async function clickTestId(el: ScionPageAgentEdit, id: string): Promise<void> {
  (el.shadowRoot!.querySelector(`[data-testid="${id}"]`) as HTMLElement).click();
  await settle(el);
}

beforeAll(async () => {
  await import('./agent-edit.js');
}, 30_000);

beforeEach(() => {
  window.history.pushState({}, '', '/agents/agent-1/edit');
  window.__SCION_FEATURES__ = { 'web.agent_edit': true };
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.unstubAllGlobals();
  window.__SCION_FEATURES__ = {};
});

describe('agent edit: GCP identity on a Kubernetes target (Block offered)', () => {
  it('offers Block with the Kubernetes Block hint for a stored Block on a Kubernetes broker', async () => {
    stubFetch({ agent: makeAgent({ gcpIdentity: { metadataMode: 'block' } }) });
    const el = await mount();
    expect(modeSelect(el).querySelector('sl-option[value="block"]')).not.toBeNull();
    expect(modeSelect(el).value).toBe('block');
    expect(hint(el)).toContain(KUBERNETES_BLOCK_HINT);
    expect(hint(el)).not.toContain('Token requests are denied');
  });

  it('keeps a stored Block selected on a known-Kubernetes target', async () => {
    stubFetch({ agent: makeAgent({ gcpIdentity: { metadataMode: 'block' } }) });
    const el = await mount();
    expect(modeSelect(el).value).toBe('block');
    expect(formOf(el)!.gcpIdentity!.gcpMetadataMode).toBe('block');
    expect(formOf(el)!.gcpIdentity!.gcpIdentityUserSet).toBe(false);
  });

  it('keeps the generic Block text for a stored Block on a docker broker', async () => {
    stubFetch({
      agent: makeAgent({ gcpIdentity: { metadataMode: 'block' } }),
      brokerProfiles: DOCKER_PROFILES,
    });
    const el = await mount();
    expect(modeSelect(el).querySelector('sl-option[value="block"]')).not.toBeNull();
    expect(hint(el)).toContain('Token requests are denied');
    expect(hint(el)).not.toContain(KUBERNETES_BLOCK_HINT);
  });

  it('names the stored identity while untouched, and drops that once the user picks', async () => {
    stubFetch({ agent: makeAgent({ gcpIdentity: { metadataMode: 'passthrough' } }) });
    const el = await mount();
    expect(hint(el)).toContain(STORED_IDENTITY_HINT);
    await choose(el, modeSelect(el), 'block');
    expect(hint(el)).not.toContain(STORED_IDENTITY_HINT);
  });

  it.each([
    ['Kubernetes', K8S_PROFILES],
    ['docker', DOCKER_PROFILES],
  ])(
    'resaves a stored identity unchanged without sending gcp_identity (%s)',
    async (_, profiles) => {
      stubFetch({
        agent: makeAgent({ gcpIdentity: { metadataMode: 'block' } }),
        brokerProfiles: profiles,
      });
      const el = await mount();
      await rename(el);
      await clickTestId(el, 'save');
      expect(patchBodies).toHaveLength(1);
      expect(patchBodies[0]).not.toHaveProperty('gcp_identity');
    }
  );

  it('sends an explicit Block on Save once the user picks it for a Kubernetes target', async () => {
    stubFetch({ agent: makeAgent() });
    const el = await mount();
    await choose(el, modeSelect(el), 'block');
    await clickTestId(el, 'save');
    expect(patchBodies).toEqual([{ stateVersion: 3, gcp_identity: { metadata_mode: 'block' } }]);
  });

  it('sends an explicit Block on Save & Start too, before starting', async () => {
    stubFetch({ agent: makeAgent() });
    const el = await mount();
    await choose(el, modeSelect(el), 'block');
    await clickTestId(el, 'save-start');
    expect(patchBodies).toEqual([{ stateVersion: 3, gcp_identity: { metadata_mode: 'block' } }]);
    expect(posts).toEqual(['/api/v1/agents/agent-1/start']);
  });

  it('sends an explicit passthrough once picked', async () => {
    stubFetch({ agent: makeAgent() });
    const el = await mount();
    await choose(el, modeSelect(el), 'passthrough');
    await clickTestId(el, 'save');
    expect(patchBodies[0]!.gcp_identity).toEqual({ metadata_mode: 'passthrough' });
  });

  it('with nothing stored and nothing chosen: blank picker, the no-mode hint, nothing sent', async () => {
    stubFetch({ agent: makeAgent() });
    const el = await mount();
    expect(modeSelect(el).value).toBe('');
    expect(hint(el)).toContain(NO_IDENTITY_MODE_HINT);
    await rename(el);
    await clickTestId(el, 'save');
    expect(patchBodies[0]).not.toHaveProperty('gcp_identity');
  });

  it('uses appliedConfig.profile to resolve the target on a mixed-profile broker', async () => {
    stubFetch({
      agent: makeAgent({ profile: 'k8s-a', gcpIdentity: { metadataMode: 'block' } }),
      brokerProfiles: MIXED_PROFILES,
    });
    const el = await mount();
    expect(hint(el)).toContain(KUBERNETES_BLOCK_HINT);
  });

  it('shows no Kubernetes hint on a mixed broker when the profile is not Kubernetes', async () => {
    stubFetch({
      agent: makeAgent({ profile: 'local', gcpIdentity: { metadataMode: 'block' } }),
      brokerProfiles: MIXED_PROFILES,
    });
    const el = await mount();
    expect(hint(el)).not.toContain(KUBERNETES_BLOCK_HINT);
  });

  it('keeps an explicit Block pick when the broker resolves as Kubernetes afterwards', async () => {
    stubFetch({ agent: makeAgent(), hold: ['broker'] });
    const el = await mount();
    await choose(el, modeSelect(el), 'block');
    const i = held.findIndex((h) => h.kind === 'broker');
    held.splice(i, 1)[0]!.release();
    await settle(el);
    expect(modeSelect(el).value).toBe('block');
    await clickTestId(el, 'save');
    expect(patchBodies[0]!.gcp_identity).toEqual({ metadata_mode: 'block' });
  });

  it('sends the new service account after changing only the account picker', async () => {
    stubFetch({
      agent: makeAgent({ gcpIdentity: { metadataMode: 'assign', serviceAccountId: 'sa-a' } }),
      brokerProfiles: DOCKER_PROFILES,
    });
    const el = await mount();
    await choose(el, saSelect(el), 'sa-b');
    await clickTestId(el, 'save');
    expect(patchBodies[0]!.gcp_identity).toEqual({
      metadata_mode: 'assign',
      service_account_id: 'sa-b',
    });
  });

  it.each(['stopped', 'suspended', 'running'])(
    '%s: the identity needs a reincarnation, so the picker is disabled and nothing is sent',
    async (phase) => {
      stubFetch({ agent: makeAgent({ phase, gcpIdentity: { metadataMode: 'block' } }) });
      const el = await mount();
      expect(gcpField(el).dataset.disposition).toBe('reincarnate');
      expect(modeSelect(el).hasAttribute('disabled')).toBe(true);
      expect(gcpField(el).textContent).toContain('Needs reincarnation');
      formOf(el)!.gcpIdentity!.pickMode('passthrough');
      await rename(el);
      await clickTestId(el, 'save');
      expect(patchBodies[0]).not.toHaveProperty('gcp_identity');
    }
  );
});

describe('agent edit: service-account mapping state (ptone/scion#4391)', () => {
  const PLAIN_OPTIONS: Array<[string, string]> = [
    ['sa-a', 'sa-a@example.com'],
    ['sa-b', 'sa-b@example.com'],
    ['sa-c', 'sa-c@example.com'],
  ];
  const assigned = (profile: string, sa = 'sa-a') =>
    makeAgent({ profile, gcpIdentity: { metadataMode: 'assign', serviceAccountId: sa } });

  it("asks for the agent's broker and profile and labels every account, not-mapped last", async () => {
    stubFetch({ agent: assigned('k8s-a') });
    const el = await mount();
    const asked = listUrls.filter((u) => u.searchParams.has('assignStatus'));
    expect(asked.length).toBeGreaterThan(0);
    const last = asked[asked.length - 1]!;
    expect(last.searchParams.get('profile')).toBe('k8s-a');
    expect(last.searchParams.get('broker')).toBe('broker-1');
    expect(options(el)).toEqual([
      ['sa-b', 'sa-b@example.com — mapped'],
      ['sa-c', 'sa-c@example.com — mapping unknown: report stale'],
      ['sa-a', 'sa-a@example.com — not mapped on this profile'],
    ]);
  });

  it("shows the selected account's message as the help text, as a warning when not mapped", async () => {
    stubFetch({ agent: assigned('k8s-a', 'sa-a') });
    const el = await mount();
    const help = statusHelp(el);
    expect(help?.textContent?.trim()).toBe('Not mapped: test message A.');
    expect(help?.classList.contains('warning')).toBe(true);
  });

  it('shows a mapped selection without the warning style', async () => {
    stubFetch({ agent: assigned('k8s-a', 'sa-b') });
    const el = await mount();
    const help = statusHelp(el);
    expect(help?.textContent?.trim()).toBe('Mapped: test message B.');
    expect(help?.classList.contains('warning')).toBe(false);
  });

  it('keeps the labels when the plain list lands after the mapping-state list', async () => {
    stubFetch({ agent: assigned('k8s-a'), hold: ['list-plain'] });
    const el = await mount();
    expect(held.map((h) => h.kind)).toEqual(['list-plain']);
    const i = held.findIndex((h) => h.kind === 'list-plain');
    held.splice(i, 1)[0]!.release();
    await settle(el);
    expect(options(el)[0]).toEqual(['sa-b', 'sa-b@example.com — mapped']);
    expect(options(el)[2]).toEqual(['sa-a', 'sa-a@example.com — not mapped on this profile']);
  });

  it("labels the agent's own profile", async () => {
    stubFetch({ agent: assigned('k8s-b') });
    const el = await mount();
    expect(options(el)).toEqual([
      ['sa-a', 'sa-a@example.com — mapped'],
      ['sa-b', 'sa-b@example.com — no mapping needed'],
      ['sa-c', 'sa-c@example.com — mapping unknown: older broker'],
    ]);
  });

  it('does not ask when the agent has no profile', async () => {
    stubFetch({ agent: assigned('') });
    const el = await mount();
    expect(listUrls.length).toBeGreaterThan(0);
    expect(listUrls.some((u) => u.searchParams.has('assignStatus'))).toBe(false);
    expect(options(el)).toEqual(PLAIN_OPTIONS);
  });

  it('does not ask for a broker that is not Kubernetes', async () => {
    stubFetch({ agent: assigned('local'), brokerProfiles: DOCKER_PROFILES });
    const el = await mount();
    expect(listUrls.some((u) => u.searchParams.has('assignStatus'))).toBe(false);
    expect(options(el)).toEqual(PLAIN_OPTIONS);
  });

  it('labels the accounts once the broker loads after the plain list', async () => {
    stubFetch({ agent: assigned('k8s-a'), hold: ['broker'] });
    const el = await mount();
    expect(options(el)).toEqual(PLAIN_OPTIONS);
    const i = held.findIndex((h) => h.kind === 'broker');
    held.splice(i, 1)[0]!.release();
    await settle(el);
    expect(options(el)[0]).toEqual(['sa-b', 'sa-b@example.com — mapped']);
  });

  it('falls back to the plain list when the mapping-state request is refused', async () => {
    stubFetch({ agent: assigned('k8s-a'), denyAssignStatus: true });
    const el = await mount();
    expect(listUrls.some((u) => u.searchParams.has('assignStatus'))).toBe(true);
    expect(options(el)).toEqual(PLAIN_OPTIONS);
    expect(statusHelp(el)).toBeNull();
  });

  it('works as before against a hub without assignStatus', async () => {
    stubFetch({ agent: assigned('k8s-a'), olderHub: true });
    const el = await mount();
    expect(options(el)).toEqual(PLAIN_OPTIONS);
  });
});
