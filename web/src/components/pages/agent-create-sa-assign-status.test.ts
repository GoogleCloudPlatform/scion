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
 * ptone/scion#4391: the Create Agent service-account picker shows each
 * account's mapping state for the chosen Kubernetes broker profile.
 *
 * Pinned here:
 *  - the list asks for assignStatus only once a Kubernetes broker and a
 *    profile are chosen, and asks again when the target changes;
 *  - every account stays listed and labelled; not-mapped accounts come last;
 *  - the selected account's hub message is shown, and a pick survives a
 *    target change;
 *  - leaving the Kubernetes target drops the labels;
 *  - a hub without assignStatus gives the picker it had before.
 */

import { describe, it, expect, vi, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import { createInternals, formField } from './__fixtures__/agent-create-internals.js';
import type { GCPServiceAccountAssignStatus } from '../../shared/types.js';

type MountedEl = HTMLElement & { updateComplete: Promise<unknown> };

interface AgentCreateInternals {
  brokerId: string;
  profile: string;
  gcpServiceAccountId: string;
  gcpIdentityUserSet: boolean;
}

const BROKER = {
  id: 'broker-k8s',
  name: 'k8s-broker',
  status: 'online',
  profiles: [
    { name: 'k8s-a', type: 'kubernetes', available: true },
    { name: 'k8s-b', type: 'kubernetes', available: true },
  ],
};

const SINGLE_PROFILE_BROKER = {
  ...BROKER,
  profiles: [{ name: 'k8s-a', type: 'kubernetes', available: true }],
};

function account(id: string, assignStatus?: GCPServiceAccountAssignStatus) {
  return {
    id,
    scope: 'project',
    scopeId: 'p1',
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
    'sa-b': { state: 'not_mapped', message: 'Not mapped: test message B2.' },
    'sa-c': { state: 'not_required', message: 'No mapping needed: test message C2.' },
  },
};

/** A held service-account list response, released by the test. */
interface HeldList {
  /** The profile the request asked about, or '' for none. */
  profile: string;
  release: () => void;
}

/**
 * Routes the page's fetches. The service-account list answers with the
 * mapping state for the requested profile, unless olderHub, which ignores
 * the parameters as a hub that predates them does. With denyAssignStatus a
 * list that asks for the mapping state is refused (403), as the hub's extra
 * read check can. While hold.on, list responses wait in held until the test
 * releases them, so they can complete in any order. Records every list URL.
 */
function stubFetch(
  opts: { broker?: typeof BROKER; olderHub?: boolean; denyAssignStatus?: boolean } = {}
): { listUrls: URL[]; hold: { on: boolean }; held: HeldList[] } {
  const listUrls: URL[] = [];
  const hold = { on: false };
  const held: HeldList[] = [];
  const broker = opts.broker ?? BROKER;
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const raw = requestUrl(input);
      const ok = (body: unknown) =>
        Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) } as Response);
      if (raw.includes('/api/v1/projects?')) return ok({ projects: [{ id: 'p1', name: 'P1' }] });
      if (raw.includes('/api/v1/runtime-brokers')) return ok({ brokers: [broker] });
      if (raw.includes('/api/v1/projects/p1/settings')) {
        return ok({
          defaultGCPIdentityMode: 'assign',
          defaultGCPIdentityServiceAccountID: 'sa-a',
        });
      }
      if (raw.includes('/gcp-service-accounts')) {
        const url = new URL(raw, 'http://hub.example.com');
        listUrls.push(url);
        if (opts.denyAssignStatus && url.searchParams.has('assignStatus')) {
          return Promise.resolve({
            ok: false,
            status: 403,
            json: () => Promise.resolve({ error: { message: 'forbidden' } }),
          } as Response);
        }
        const profile = url.searchParams.get('profile') ?? '';
        const statuses: Record<string, GCPServiceAccountAssignStatus> = opts.olderHub
          ? {}
          : (STATUS_BY_PROFILE[profile] ?? {});
        const response = ok({
          items: ['sa-a', 'sa-b', 'sa-c'].map((id) => account(id, statuses[id])),
        });
        if (!hold.on) return response;
        return new Promise<Response>((resolve) => {
          held.push({ profile, release: () => resolve(response) });
        });
      }
      return ok({});
    })
  );
  return { listUrls, hold, held };
}

/** Releases the oldest held list response for profile, then lets the page settle. */
async function release(el: MountedEl, held: HeldList[], profile: string): Promise<void> {
  const i = held.findIndex((h) => h.profile === profile);
  if (i < 0) throw new Error(`no held list response for profile "${profile}"`);
  const [h] = held.splice(i, 1);
  h.release();
  await settle(el);
}

/** Lets the page settle until cond holds (or gives up after a while). */
async function waitUntil(el: MountedEl, cond: () => boolean): Promise<void> {
  for (let i = 0; i < 50 && !cond(); i++) await settle(el);
  if (!cond()) throw new Error('condition not reached');
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

async function setProfile(el: MountedEl, profile: string): Promise<void> {
  createInternals<AgentCreateInternals>(el).profile = profile;
  await settle(el);
}

/** The Service Account options as [id, text], in rendered order. */
function options(el: MountedEl): Array<[string, string]> {
  const select = formField(el, 'Service Account')?.querySelector('sl-select');
  if (!select) throw new Error('Service Account select not rendered');
  return Array.from(select.querySelectorAll('sl-option')).map((o) => [
    o.getAttribute('value') ?? '',
    (o.textContent ?? '').replace(/\s+/g, ' ').trim(),
  ]);
}

function statusHint(el: MountedEl): string {
  return (
    formField(el, 'Service Account')
      ?.querySelector('[data-testid="gcp-sa-assign-status"]')
      ?.textContent?.trim() ?? ''
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

describe('Create Agent: service-account mapping state for the Kubernetes target', () => {
  it('asks for nothing extra until a profile is chosen', async () => {
    const { listUrls } = stubFetch();
    const el = await mountAgentCreate();

    expect(listUrls.length).toBeGreaterThan(0);
    expect(listUrls.every((u) => !u.searchParams.has('assignStatus'))).toBe(true);
    expect(options(el).map(([id]) => id)).toEqual(['sa-a', 'sa-b', 'sa-c']);
    expect(statusHint(el)).toBe('');
  });

  it('labels every account for the chosen profile and lists not-mapped ones last', async () => {
    const { listUrls } = stubFetch();
    const el = await mountAgentCreate();
    await setProfile(el, 'k8s-a');

    const last = listUrls[listUrls.length - 1];
    expect(last.searchParams.get('assignStatus')).toBe('true');
    expect(last.searchParams.get('profile')).toBe('k8s-a');
    expect(last.searchParams.get('broker')).toBe('broker-k8s');

    const opts = options(el);
    expect(opts.map(([id]) => id)).toEqual(['sa-b', 'sa-c', 'sa-a']);
    expect(opts[0][1]).toContain('— mapped');
    expect(opts[1][1]).toContain('— mapping unknown: report stale');
    expect(opts[2][1]).toContain('— not mapped on this profile');
    for (const [, text] of opts) expect(text).not.toMatch(/ready/i);

    // The project default account is not mapped here: its hub message shows.
    expect(createInternals<AgentCreateInternals>(el).gcpServiceAccountId).toBe('sa-a');
    expect(statusHint(el)).toBe('Not mapped: test message A.');
  });

  it('asks again when the profile changes and keeps the user pick', async () => {
    const { listUrls } = stubFetch();
    const el = await mountAgentCreate();
    await setProfile(el, 'k8s-a');

    const select = formField(el, 'Service Account')!.querySelector('sl-select')!;
    (select as HTMLElement & { value: string }).value = 'sa-b';
    select.dispatchEvent(new Event('sl-change', { bubbles: true, composed: true }));
    await settle(el);
    expect(statusHint(el)).toBe('Mapped: test message B.');

    await setProfile(el, 'k8s-b');
    expect(listUrls[listUrls.length - 1].searchParams.get('profile')).toBe('k8s-b');

    const page = createInternals<AgentCreateInternals>(el);
    expect(page.gcpServiceAccountId).toBe('sa-b');
    expect(page.gcpIdentityUserSet).toBe(true);

    const opts = options(el);
    expect(opts.map(([id]) => id)).toEqual(['sa-a', 'sa-c', 'sa-b']);
    expect(opts[0][1]).toContain('— mapped');
    expect(opts[1][1]).toContain('— no mapping needed');
    expect(opts[2][1]).toContain('— not mapped on this profile');
    expect(statusHint(el)).toBe('Not mapped: test message B2.');
  });

  it('drops the labels when the profile is no longer chosen', async () => {
    const { listUrls } = stubFetch();
    const el = await mountAgentCreate();
    await setProfile(el, 'k8s-a');
    const fetchedBefore = listUrls.length;

    await setProfile(el, '');
    expect(listUrls.length).toBe(fetchedBefore);
    expect(options(el)).toEqual([
      ['sa-a', 'sa-a@example.com'],
      ['sa-b', 'sa-b@example.com'],
      ['sa-c', 'sa-c@example.com'],
    ]);
    expect(statusHint(el)).toBe('');
  });

  it('asks on the first load when the only profile is chosen automatically', async () => {
    const { listUrls } = stubFetch({ broker: SINGLE_PROFILE_BROKER });
    const el = await mountAgentCreate();

    expect(listUrls[0].searchParams.get('profile')).toBe('k8s-a');
    expect(options(el).map(([id]) => id)).toEqual(['sa-b', 'sa-c', 'sa-a']);
  });

  it('works as before against a hub without assignStatus', async () => {
    stubFetch({ olderHub: true });
    const el = await mountAgentCreate();
    await setProfile(el, 'k8s-a');

    expect(options(el)).toEqual([
      ['sa-a', 'sa-a@example.com'],
      ['sa-b', 'sa-b@example.com'],
      ['sa-c', 'sa-c@example.com'],
    ]);
    expect(statusHint(el)).toBe('');
  });

  it('keeps the newest target when an older response arrives last', async () => {
    const { hold, held } = stubFetch();
    const el = await mountAgentCreate();
    hold.on = true;

    await setProfile(el, 'k8s-a');
    await setProfile(el, 'k8s-b');
    expect(held.map((h) => h.profile)).toEqual(['k8s-a', 'k8s-b']);

    await release(el, held, 'k8s-b');
    await release(el, held, 'k8s-a');

    const opts = options(el);
    expect(opts.map(([id]) => id)).toEqual(['sa-a', 'sa-c', 'sa-b']);
    expect(opts[0][1]).toContain('— mapped');
    expect(opts[2][1]).toContain('— not mapped on this profile');
    expect(statusHint(el)).toBe('Mapped: test message A2.');
  });

  it('ends on the current target when it changes during the first load', async () => {
    // k8s-a is the only available profile, so the first load asks about it;
    // the test then switches to k8s-b while that load is still in flight.
    const broker = {
      ...BROKER,
      profiles: [
        { name: 'k8s-a', type: 'kubernetes', available: true },
        { name: 'k8s-b', type: 'kubernetes', available: false },
      ],
    };
    const { hold, held } = stubFetch({ broker });
    hold.on = true;
    const el = await mountAgentCreate();
    await waitUntil(el, () => held.length === 1);
    expect(held[0].profile).toBe('k8s-a');

    await setProfile(el, 'k8s-b');
    expect(held.map((h) => h.profile)).toEqual(['k8s-a', 'k8s-b']);

    // The newer request completes first, then the first load's older one.
    await release(el, held, 'k8s-b');
    await release(el, held, 'k8s-a');
    // Whatever is still asked about must be the current target only.
    expect(held.every((h) => h.profile === 'k8s-b')).toBe(true);
    while (held.length > 0) await release(el, held, 'k8s-b');
    await waitUntil(el, () => statusHint(el) !== '');

    const opts = options(el);
    expect(opts.map(([id]) => id)).toEqual(['sa-a', 'sa-c', 'sa-b']);
    expect(opts[0][1]).toContain('— mapped');
    expect(opts[1][1]).toContain('— no mapping needed');
    expect(opts[2][1]).toContain('— not mapped on this profile');
    expect(statusHint(el)).toBe('Mapped: test message A2.');
  });

  it('lists the accounts without labels when the first load cannot get the mapping state', async () => {
    const { listUrls } = stubFetch({ broker: SINGLE_PROFILE_BROKER, denyAssignStatus: true });
    const el = await mountAgentCreate();

    expect(listUrls[0].searchParams.get('assignStatus')).toBe('true');
    expect(listUrls[1].searchParams.has('assignStatus')).toBe(false);
    expect(options(el)).toEqual([
      ['sa-a', 'sa-a@example.com'],
      ['sa-b', 'sa-b@example.com'],
      ['sa-c', 'sa-c@example.com'],
    ]);
    expect(statusHint(el)).toBe('');
  });

  it('puts the message in the select help text and marks a not-mapped selection', async () => {
    stubFetch();
    const el = await mountAgentCreate();
    await setProfile(el, 'k8s-a');

    const hint = formField(el, 'Service Account')!.querySelector(
      'sl-select > [slot="help-text"][data-testid="gcp-sa-assign-status"]'
    );
    expect(hint).not.toBeNull();
    expect(hint!.classList.contains('warning')).toBe(true);
    expect(hint!.getAttribute('data-assign-state')).toBe('not_mapped');

    await setProfile(el, 'k8s-b');
    const mapped = formField(el, 'Service Account')!.querySelector(
      '[data-testid="gcp-sa-assign-status"]'
    );
    expect(mapped!.classList.contains('warning')).toBe(false);
  });
}, 30000);
