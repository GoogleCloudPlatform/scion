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
 * The artifact Share dialog: share links (create once-shown, revoke),
 * people and projects (grants) and retention with the warning of what an
 * expiry change cuts.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import { resetPrincipalNames } from '../../client/principal-names.js';
import type { Artifact, ArtifactGrant, ShareLink } from '../../client/artifacts.js';
import {
  expiryWarning,
  retentionExpiry,
  splitPrincipal,
  type ScionArtifactShareDialog,
} from './artifact-share-dialog.js';

const ID = '00000000-0000-4000-8000-000000000001';
const BASE = `/api/v1/artifacts/${ID}`;
const DAY = 24 * 60 * 60 * 1000;

function artifact(extra: Partial<Artifact> = {}): Artifact {
  return {
    id: ID,
    ref: `scion://artifact/${ID}`,
    scopeKind: 'project',
    scopeRef: 'proj-1',
    ownerKind: 'user',
    ownerRef: 'user-me',
    title: 'Q3 rollout plan',
    currentSeq: 2,
    createdAt: '2026-10-01T12:00:00Z',
    updatedAt: '2026-10-05T12:00:00Z',
    ...extra,
  };
}

function link(id: string, daysLeft: number): ShareLink {
  return {
    id,
    createdAt: new Date(Date.now() - DAY).toISOString(),
    expiresAt: new Date(Date.now() + daysLeft * DAY).toISOString(),
    createdBy: 'user:user-me',
  };
}

const HOME: ArtifactGrant = {
  id: 'g-home',
  subjectKind: 'scope',
  subjectRef: 'proj-1',
  permission: 'read',
  home: true,
  createdAt: '2026-10-01T12:00:00Z',
};
const PRIYA: ArtifactGrant = {
  id: 'g-priya',
  subjectKind: 'principal',
  subjectRef: 'user:user-priya',
  permission: 'write',
  createdAt: '2026-10-02T12:00:00Z',
};

interface Call {
  method: string;
  url: string;
  body: unknown;
}

interface Server {
  calls: Call[];
  links: ShareLink[];
  grants: ArtifactGrant[];
  cross: boolean;
  clamped: boolean;
  /** When set, link creation waits until the test calls release(). */
  holdCreate?: boolean;
  release?: () => void;
}

function json(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

function mockServer(init: Partial<Server> = {}): Server {
  const s: Server = {
    calls: [],
    links: [],
    grants: [HOME],
    cross: false,
    clamped: false,
    ...init,
  };
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, opts?: RequestInit) => {
      const url = String(input);
      const method = opts?.method ?? 'GET';
      const body = typeof opts?.body === 'string' ? JSON.parse(opts.body) : undefined;
      s.calls.push({ method, url, body });
      if (url === `${BASE}/links` && method === 'GET') return json({ links: s.links });
      if (url === `${BASE}/links` && method === 'POST') {
        const l = link('l-new', (body as { ttlHours: number }).ttlHours / 24);
        const answer = (): Promise<Response> =>
          json(
            {
              link: l,
              url: '/api/v1/artifacts/shared/TOKEN123',
              clampedToArtifactExpiry: s.clamped,
            },
            201
          );
        if (s.holdCreate) {
          return new Promise<Response>((resolve) => {
            s.release = (): void => void answer().then(resolve);
          });
        }
        return answer();
      }
      if (url.startsWith(`${BASE}/links/`) && method === 'DELETE') {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (url === `${BASE}/grants` && method === 'GET') {
        return json({ grants: s.grants, crossProjectSharing: s.cross });
      }
      if (url === `${BASE}/grants` && method === 'POST') {
        const b = body as Pick<ArtifactGrant, 'subjectKind' | 'subjectRef' | 'permission'>;
        const existing = s.grants.find((g) => g.subjectRef === b.subjectRef);
        return json(
          { grant: { id: existing?.id ?? 'g-new', createdAt: '2026-10-08T12:00:00Z', ...b } },
          existing ? 200 : 201
        );
      }
      if (url.startsWith(`${BASE}/grants/`) && method === 'DELETE') {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (url === BASE && method === 'PATCH') {
        const b = body as { expiresAt: string | null };
        return json({ artifact: artifact(b.expiresAt ? { expiresAt: b.expiresAt } : {}) });
      }
      if (url.startsWith('/api/v1/users?search=')) {
        return json({
          users: [
            { id: 'user-alex', email: 'alex@example.com', displayName: 'Alex Reviewer' },
            { id: 'user-priya', email: 'priya@example.com', displayName: 'Priya Shah' },
          ],
        });
      }
      if (url.startsWith('/api/v1/projects?search=')) {
        return json({ projects: [{ id: 'proj-2', name: 'platform' }] });
      }
      if (url === '/api/v1/users/user-priya') return json({ displayName: 'Priya Shah' });
      if (url === '/api/v1/projects/proj-1') return json({ name: 'web-frontend' });
      if (url === '/api/v1/projects/proj-2') return json({ name: 'platform' });
      return json({ error: { code: 'not_found', message: 'not found' } }, 404);
    })
  );
  return s;
}

async function settle(el: ScionArtifactShareDialog): Promise<void> {
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mount(a: Artifact = artifact()): Promise<ScionArtifactShareDialog> {
  const el = document.createElement('scion-artifact-share-dialog') as ScionArtifactShareDialog;
  el.artifact = a;
  el.currentUserId = 'user-me';
  document.body.appendChild(el);
  el.open = true;
  await settle(el);
  return el;
}

function $(el: ScionArtifactShareDialog, sel: string): HTMLElement | null {
  return el.shadowRoot!.querySelector(sel);
}

function $$(el: ScionArtifactShareDialog, sel: string): HTMLElement[] {
  return Array.from(el.shadowRoot!.querySelectorAll(sel));
}

function button(el: ScionArtifactShareDialog, text: string, root?: HTMLElement): HTMLElement {
  const b = Array.from((root ?? el.shadowRoot!).querySelectorAll('sl-button')).find(
    (x) => x.textContent!.trim() === text
  );
  if (!b) throw new Error(`no button ${text}`);
  return b as HTMLElement;
}

/** Whether the token appears anywhere in the dialog: markup or a field's value. */
function showsToken(el: ScionArtifactShareDialog): boolean {
  const inputs = Array.from(el.shadowRoot!.querySelectorAll('sl-input')) as Array<
    HTMLElement & { value: unknown }
  >;
  return (
    el.shadowRoot!.innerHTML.includes('TOKEN123') ||
    inputs.some((i) => String(i.value ?? '').includes('TOKEN123'))
  );
}

function choose(sel: HTMLElement, value: string): void {
  (sel as HTMLElement & { value: string }).value = value;
  sel.dispatchEvent(new Event('sl-change'));
}

describe('share dialog helpers', () => {
  it('turns a retention choice into an expiry', () => {
    const now = new Date('2026-10-08T12:00:00Z');
    expect(retentionExpiry('never', '2026-12-01T00:00:00Z', now)).toBeNull();
    expect(retentionExpiry('current', '2026-12-01T00:00:00Z', now)!.toISOString()).toBe(
      '2026-12-01T00:00:00.000Z'
    );
    expect(retentionExpiry('current', undefined, now)).toBeNull();
    expect(retentionExpiry('7d', undefined, now)!.toISOString()).toBe('2026-10-15T12:00:00.000Z');
  });

  it('words what an expiry change cuts', () => {
    expect(expiryWarning(0, 0)).toBe('');
    expect(expiryWarning(1, 0)).toBe('1 link will be cut short');
    expect(expiryWarning(2, 1)).toBe('2 links will be cut short and 1 grant will be removed');
    expect(expiryWarning(0, 3)).toBe('3 grants will be removed');
  });

  it('splits principal references', () => {
    expect(splitPrincipal('user:u-1')).toEqual(['user', 'u-1']);
    expect(splitPrincipal('agent:a-1')).toEqual(['agent', 'a-1']);
    expect(splitPrincipal('proj-1')).toEqual(['', '']);
    expect(splitPrincipal('group:g')).toEqual(['', '']);
  });
});

describe('share dialog', () => {
  beforeAll(async () => {
    await import('./artifact-share-dialog.js');
  }, 30_000);

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    vi.useRealTimers();
    resetPrincipalNames();
  });

  it('lists links and grants; the home project cannot be removed', async () => {
    mockServer({ links: [link('l-1', 7), link('l-2', 2)], grants: [HOME, PRIYA] });
    const el = await mount();
    expect($$(el, 'tbody tr')).toHaveLength(2);
    expect($$(el, 'tbody tr')[1].textContent).toContain('(in 2 days)');
    expect($$(el, 'tbody tr')[0].textContent).toContain('You');

    const grants = $$(el, '.grant');
    expect(grants).toHaveLength(2);
    expect(grants[0].textContent).toContain('web-frontend');
    expect(grants[0].textContent).toContain('(home project)');
    expect(grants[0].querySelector('sl-button')).toBeNull();
    expect(grants[1].textContent).toContain('Priya Shah');
    expect((grants[1].querySelector('sl-select') as HTMLElement & { value: string }).value).toBe(
      'write'
    );
    expect(button(el, 'Remove', grants[1])).not.toBeNull();
    // Cross-project sharing is off: said once, and projects are not offered.
    expect(el.shadowRoot!.textContent).toContain(
      'Sharing with other projects is turned off on this hub.'
    );
  });

  it('shows the empty state with no links', async () => {
    mockServer();
    const el = await mount();
    expect($(el, 'table')).toBeNull();
    expect(el.shadowRoot!.textContent).toContain('No active links.');
  });

  it('creates a link with the chosen lifetime and shows it only once', async () => {
    const s = mockServer();
    const el = await mount();
    choose($(el, '#ttl')!, '24');
    button(el, 'Create link').click();
    await settle(el);
    const post = s.calls.find((c) => c.method === 'POST' && c.url === `${BASE}/links`)!;
    expect(post.body).toEqual({ ttlHours: 24 });

    const input = $(el, '.created sl-input') as HTMLElement & { value: string };
    expect(input.value).toBe(`${window.location.origin}/api/v1/artifacts/shared/TOKEN123`);
    expect($(el, '.created')!.textContent).toContain('the link is shown only once');
    expect($$(el, 'tbody tr')).toHaveLength(1);

    // Closing forgets the link at once, before any reopening.
    el.open = false;
    await settle(el);
    expect($(el, '.created')).toBeNull();
    expect(showsToken(el)).toBe(false);
    el.open = true;
    await settle(el);
    expect($(el, '.created')).toBeNull();
    expect(showsToken(el)).toBe(false);
  });

  it('forgets a created link when Done closes the dialog', async () => {
    mockServer();
    const el = await mount();
    const closed: Event[] = [];
    el.addEventListener('artifact-share-closed', (e) => closed.push(e));
    button(el, 'Create link').click();
    await settle(el);
    expect(showsToken(el)).toBe(true);
    button(el, 'Done').click();
    await settle(el);
    expect(closed).toHaveLength(1);
    // The parent has not set open=false yet; the link is already gone.
    expect(el.open).toBe(true);
    expect(showsToken(el)).toBe(false);
  });

  for (const how of ['closed', 'closed and reopened', 'removed'] as const) {
    it(`never shows a link whose creation answers after the dialog was ${how}`, async () => {
      const s = mockServer({ holdCreate: true });
      const el = await mount();
      button(el, 'Create link').click();
      await settle(el);
      expect(s.release).toBeDefined();
      if (how === 'removed') {
        el.remove();
      } else {
        el.open = false;
        await settle(el);
        if (how === 'closed and reopened') {
          el.open = true;
          await settle(el);
        }
      }
      s.release!();
      await settle(el);
      expect(showsToken(el)).toBe(false);
      expect((el as unknown as { created: unknown }).created).toBeNull();
    });
  }

  it('says when a link is cut to the artifact’s expiry', async () => {
    mockServer({ clamped: true });
    const el = await mount(artifact({ expiresAt: new Date(Date.now() + 2 * DAY).toISOString() }));
    expect(el.shadowRoot!.textContent).toContain('This artifact expires');
    button(el, 'Create link').click();
    await settle(el);
    expect($(el, '.created')!.textContent).toContain('when the artifact expires');
  });

  it('revokes a link', async () => {
    const s = mockServer({ links: [link('l-1', 7)] });
    const el = await mount();
    button(el, 'Revoke').click();
    await settle(el);
    expect(s.calls.some((c) => c.method === 'DELETE' && c.url === `${BASE}/links/l-1`)).toBe(true);
    expect(el.shadowRoot!.textContent).toContain('No active links.');
  });

  it('shares with a user found by search, skipping who already has access', async () => {
    const s = mockServer({ grants: [HOME, PRIYA] });
    const el = await mount();
    vi.useFakeTimers();
    const input = $(el, '.picker sl-input') as HTMLElement & { value: string };
    input.value = 're';
    input.dispatchEvent(new Event('sl-input'));
    await vi.advanceTimersByTimeAsync(300);
    vi.useRealTimers();
    await settle(el);
    expect(s.calls.some((c) => c.url.startsWith('/api/v1/projects?search='))).toBe(false);
    const options = $$(el, '.suggestions button');
    expect(options.map((o) => o.textContent!.replace(/\s+/g, ' ').trim())).toEqual([
      'Alex Reviewer alex@example.com',
    ]);
    options[0].click();
    await settle(el);
    choose($(el, 'section[aria-labelledby="people-heading"] .row sl-select')!, 'read');
    button(el, 'Add').click();
    await settle(el);
    const post = s.calls.find((c) => c.method === 'POST' && c.url === `${BASE}/grants`)!;
    expect(post.body).toEqual({
      subjectKind: 'principal',
      subjectRef: 'user:user-alex',
      permission: 'read',
    });
    expect($$(el, '.grant').map((g) => g.textContent)).toEqual(
      expect.arrayContaining([expect.stringContaining('Alex Reviewer')])
    );
  });

  it('offers projects when cross-project sharing is on, and notes it on project rows', async () => {
    const platform: ArtifactGrant = {
      id: 'g-platform',
      subjectKind: 'scope',
      subjectRef: 'proj-2',
      permission: 'read',
      createdAt: '2026-10-03T12:00:00Z',
    };
    const s = mockServer({ cross: true, grants: [HOME, platform] });
    const el = await mount();
    const row = $$(el, '.grant')[1];
    expect(row.textContent).toContain('platform');
    expect(row.textContent).toContain('Cross-project sharing is on for this hub');
    expect(el.shadowRoot!.textContent).not.toContain('turned off');

    vi.useFakeTimers();
    const input = $(el, '.picker sl-input') as HTMLElement & { value: string };
    input.value = 'plat';
    input.dispatchEvent(new Event('sl-input'));
    await vi.advanceTimersByTimeAsync(300);
    vi.useRealTimers();
    await settle(el);
    expect(s.calls.some((c) => c.url.startsWith('/api/v1/projects?search=plat'))).toBe(true);
    // platform already has a grant, so only users are offered.
    expect($$(el, '.suggestions button').some((b) => b.textContent!.includes('platform'))).toBe(
      false
    );
  });

  it('changes and removes a grant', async () => {
    const s = mockServer({ grants: [HOME, PRIYA] });
    const el = await mount();
    choose($$(el, '.grant')[1].querySelector('sl-select')!, 'admin');
    await settle(el);
    const post = s.calls.find((c) => c.method === 'POST' && c.url === `${BASE}/grants`)!;
    expect(post.body).toEqual({
      subjectKind: 'principal',
      subjectRef: 'user:user-priya',
      permission: 'admin',
    });
    button(el, 'Remove', $$(el, '.grant')[1]).click();
    await settle(el);
    expect(s.calls.some((c) => c.method === 'DELETE' && c.url === `${BASE}/grants/g-priya`)).toBe(
      true
    );
    expect($$(el, '.grant')).toHaveLength(1);
  });

  it('warns what a shorter expiry cuts and saves it', async () => {
    const s = mockServer({ links: [link('l-1', 7), link('l-2', 2)], grants: [HOME, PRIYA] });
    const el = await mount();
    const changed: Artifact[] = [];
    el.addEventListener('artifact-changed', (e) =>
      changed.push((e as CustomEvent<Artifact>).detail)
    );
    expect(button(el, 'Done')).not.toBeNull();

    choose($(el, '#retention')!, '1d');
    await settle(el);
    expect(el.shadowRoot!.textContent).toContain('was: Never');
    expect($(el, 'sl-alert[variant="warning"]')!.textContent).toContain(
      '2 links will be cut short and 2 grants will be removed'
    );
    button(el, 'Save').click();
    await settle(el);
    const patch = s.calls.find((c) => c.method === 'PATCH')!;
    const at = new Date((patch.body as { expiresAt: string }).expiresAt).getTime();
    expect(Math.abs(at - (Date.now() + DAY))).toBeLessThan(60_000);
    expect(changed).toHaveLength(1);
    expect(changed[0].expiresAt).toBeDefined();
    expect(button(el, 'Done')).not.toBeNull();
  });

  it('clears an expiry, and Cancel drops an unsaved change', async () => {
    const s = mockServer();
    const el = await mount(artifact({ expiresAt: new Date(Date.now() + 5 * DAY).toISOString() }));
    choose($(el, '#retention')!, '30d');
    await settle(el);
    button(el, 'Cancel').click();
    await settle(el);
    expect(($(el, '#retention') as HTMLElement & { value: string }).value).toBe('current');
    expect(s.calls.some((c) => c.method === 'PATCH')).toBe(false);

    choose($(el, '#retention')!, 'never');
    await settle(el);
    expect($(el, 'sl-alert[variant="warning"]')).toBeNull();
    button(el, 'Save').click();
    await settle(el);
    expect(s.calls.find((c) => c.method === 'PATCH')!.body).toEqual({ expiresAt: null });
  });
});
