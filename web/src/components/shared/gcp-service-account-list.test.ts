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
 * Tests for the scope-parameterised GCP service account list.
 *
 * The component used to be pinned to a project. Two classes of failure come
 * with un-pinning it, and each test below belongs to one of them:
 *
 *   ASKED THE WRONG SERVER QUESTION — the component renders fine and shows
 *   another scope's accounts, or none, with no error anywhere.
 *
 *   OFFERED AN ACTION THAT CANNOT WORK — hub-scoped accounts are readable by
 *   every logged-in user and writable by almost none, so any affordance
 *   rendered from a row being VISIBLE is a button that 403s for most of the
 *   hub. Creation at hub scope is refused by the Hub outright (held under #19),
 *   so there the button cannot work for ANYONE, including a hub admin whose
 *   `create` capability is true.
 *
 * ON ASSERTING ABSENCE: "no create button" passes just as well when the
 * component failed to render, when the selector is misspelled, and when the
 * feature was deleted. Every absence assertion here is paired with a positive
 * control in the same test — the same selector, finding the button, under the
 * one condition that should produce it.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let ScionGCPServiceAccountList: any;

interface FetchCall {
  url: string;
  method: string;
}

/**
 * A fetch mock that records every request and answers list GETs from a fixture.
 * Anything it is not told about answers 200 {} — a test that depends on an
 * unstubbed response is a test whose subject is not what it says it is.
 */
function makeFetch(
  calls: FetchCall[],
  listBody: Record<string, unknown>
): (url: string | URL | Request, init?: RequestInit) => Promise<Response> {
  return (url, init) => {
    calls.push({ url: String(url), method: init?.method ?? 'GET' });
    return Promise.resolve(
      new Response(JSON.stringify(listBody), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  };
}

async function createComponent(
  attrs: Record<string, string>,
  fetchMock: (url: string | URL | Request, init?: RequestInit) => Promise<Response>
) {
  vi.stubGlobal('fetch', vi.fn(fetchMock));
  const el = document.createElement('scion-gcp-service-account-list') as InstanceType<
    typeof ScionGCPServiceAccountList
  >;
  for (const [k, v] of Object.entries(attrs)) {
    el.setAttribute(k, v);
  }
  document.body.appendChild(el);
  await settle(el);
  return el;
}

async function settle(el: { updateComplete: Promise<unknown> }): Promise<void> {
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 50));
  await el.updateComplete;
}

function account(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: 'sa-1',
    email: 'one@gcp-project-xyz.iam.gserviceaccount.com',
    projectId: 'gcp-project-xyz',
    scope: 'hub',
    scopeId: 'hub',
    displayName: 'One',
    verified: true,
    verificationStatus: 'verified',
    managed: false,
    ...overrides,
  };
}

/**
 * Text of every sl-button the user can actually reach, whitespace-collapsed.
 *
 * DIALOG BUTTONS ARE EXCLUDED, and this is not tidying. Both dialogs are in the
 * DOM at all times — `?open` is a property, not a conditional render — so their
 * footers contribute a "Register" and a "Mint" to every scope. Counting those
 * as affordances makes the absence assertions below unfailable, which is how
 * this helper was first written and how the mint test caught it.
 *
 * The closed dialogs are not themselves an affordance: openAddDialog and
 * openMintDialog are called from the header/empty-state buttons and nowhere
 * else, so with those suppressed there is no path that opens either one.
 */
function buttonLabels(el: HTMLElement): string[] {
  const root = (el as unknown as { shadowRoot: ShadowRoot }).shadowRoot;
  return Array.from(root.querySelectorAll('sl-button'))
    .filter((b) => !b.closest('sl-dialog'))
    .map((b) => (b.textContent ?? '').replace(/\s+/g, ' ').trim());
}

function iconButtons(el: HTMLElement, name: string): Element[] {
  const root = (el as unknown as { shadowRoot: ShadowRoot }).shadowRoot;
  return Array.from(root.querySelectorAll(`sl-icon-button[name="${name}"]`));
}

describe('scion-gcp-service-account-list', () => {
  beforeAll(async () => {
    const mod = await import('./gcp-service-account-list.js');
    ScionGCPServiceAccountList = mod.ScionGCPServiceAccountList;
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  describe('which question it asks the server', () => {
    it('asks the flat route for the hub, and the nested route for a project', async () => {
      const hubCalls: FetchCall[] = [];
      await createComponent({ scope: 'hub' }, makeFetch(hubCalls, { items: [] }));

      const projectCalls: FetchCall[] = [];
      await createComponent(
        { scope: 'project', scopeId: 'proj-1' },
        makeFetch(projectCalls, { items: [] })
      );

      // The two scopes must not fetch the same URL. Asserting each literal
      // separately would still pass if a refactor collapsed both onto one
      // route that happens to match one of the fixtures.
      expect(hubCalls[0].url).not.toBe(projectCalls[0].url);
      expect(hubCalls[0].url).toBe('/api/v1/gcp-service-accounts?scope=hub');
      expect(projectCalls[0].url).toBe('/api/v1/projects/proj-1/gcp-service-accounts');
    });

    it('renders the accounts the hub route returned', async () => {
      const calls: FetchCall[] = [];
      const el = await createComponent(
        { scope: 'hub' },
        makeFetch(calls, {
          items: [
            account({ id: 'sa-1', email: 'one@p.iam.gserviceaccount.com' }),
            account({ id: 'sa-2', email: 'two@p.iam.gserviceaccount.com' }),
          ],
        })
      );

      const text = el.shadowRoot!.textContent ?? '';
      expect(text).toContain('one@p.iam.gserviceaccount.com');
      expect(text).toContain('two@p.iam.gserviceaccount.com');
      // Not the empty state, which is the other thing a hub-scoped list could
      // plausibly show and which would make the assertions above vacuous if
      // the emails leaked in from somewhere else.
      expect(text).not.toContain('No GCP Service Accounts');
    });

    it('fetches once on mount, and again only when re-pointed', async () => {
      // Lit reports every initially-set property as changed on the first
      // update, so the naive `changed.has('scope')` form double-fetches here.
      const calls: FetchCall[] = [];
      const el = await createComponent(
        { scope: 'project', scopeId: 'proj-1' },
        makeFetch(calls, { items: [] })
      );
      expect(calls.length).toBe(1);

      // An unrelated property change must not re-fetch...
      el.compact = true;
      await settle(el);
      expect(calls.length).toBe(1);

      // ...but re-pointing at another scope must, or the previous scope's rows
      // stay on screen looking like this scope's answer.
      el.scopeId = 'proj-2';
      await settle(el);
      expect(calls.length).toBe(2);
      expect(calls[1].url).toBe('/api/v1/projects/proj-2/gcp-service-accounts');
    });
  });

  describe('creation is not offered at hub scope', () => {
    /**
     * THE CAPABILITY IS TRUE AND THE BUTTON MUST STILL BE ABSENT.
     *
     * This fixture is a hub admin: `create` is in the list capabilities. The
     * Hub nevertheless answers 400 to a hub-scoped registration, before
     * consulting policy, because the write path is held. So the button is not
     * suppressed because the caller may not — it is suppressed because the
     * operation does not exist yet, and no capability can make it exist.
     *
     * The project-scope control in the same test is what makes the absence
     * mean something: same selector, same capability payload, button present.
     */
    it('hides Register Existing at hub scope even for a caller who may create', async () => {
      const caps = { actions: ['create', 'list', 'mint'] };

      const hubEl = await createComponent(
        { scope: 'hub' },
        makeFetch([], { items: [], _capabilities: caps })
      );
      const projectEl = await createComponent(
        { scope: 'project', scopeId: 'proj-1' },
        makeFetch([], { items: [], _capabilities: caps })
      );

      const hubLabels = buttonLabels(hubEl).join('|');
      const projectLabels = buttonLabels(projectEl).join('|');

      expect(hubLabels).not.toContain('Register Existing');
      // POSITIVE CONTROL: the same selector finds the button where it belongs.
      expect(projectLabels).toContain('Register Existing');
    });

    it('hides Mint at hub scope, where there is no mint endpoint at all', async () => {
      const caps = { actions: ['create', 'mint'] };

      const hubEl = await createComponent(
        { scope: 'hub' },
        makeFetch([], { items: [], _capabilities: caps })
      );
      const projectEl = await createComponent(
        { scope: 'project', scopeId: 'proj-1' },
        makeFetch([], { items: [], _capabilities: caps })
      );

      expect(buttonLabels(hubEl).join('|')).not.toContain('Mint');
      expect(buttonLabels(projectEl).join('|')).toContain('Mint');
    });

    it('hides them in the empty state too, which is the state a new hub is in', async () => {
      // The empty state carries its own copy of both affordances. A hub with no
      // accounts registered yet is precisely the screen where a "Register" call
      // to action is most tempting to a reader and most broken in fact.
      const el = await createComponent(
        { scope: 'hub' },
        makeFetch([], { items: [], _capabilities: { actions: ['create', 'mint'] } })
      );

      expect(el.shadowRoot!.textContent).toContain('No GCP Service Accounts');
      expect(buttonLabels(el).join('|')).not.toContain('Register Existing');
      expect(buttonLabels(el).join('|')).not.toContain('Mint');
    });

    it('hides them in compact mode too', async () => {
      const caps = { actions: ['create', 'mint'] };

      const hubEl = await createComponent(
        { scope: 'hub', compact: '' },
        makeFetch([], { items: [account()], _capabilities: caps })
      );
      const projectEl = await createComponent(
        { scope: 'project', scopeId: 'proj-1', compact: '' },
        makeFetch([], {
          items: [account({ scope: 'project', scopeId: 'proj-1' })],
          _capabilities: caps,
        })
      );

      expect(buttonLabels(hubEl).join('|')).not.toContain('Register Existing');
      expect(buttonLabels(projectEl).join('|')).toContain('Register Existing');
    });
  });

  describe('row actions come from the row, not from the row being visible', () => {
    it('renders Delete for exactly the accounts whose capabilities allow it', async () => {
      // Both rows are visible to this caller — that is the normal case for
      // hub-scoped accounts, which hub-member-read-all exposes to everyone.
      // Only one is deletable.
      const el = await createComponent(
        { scope: 'hub' },
        makeFetch([], {
          items: [
            account({
              id: 'sa-yes',
              email: 'yes@p.iam.gserviceaccount.com',
              _capabilities: { actions: ['delete', 'verify'] },
            }),
            account({
              id: 'sa-no',
              email: 'no@p.iam.gserviceaccount.com',
              _capabilities: { actions: [] },
            }),
          ],
        })
      );

      // Both rows rendered (otherwise "one trash button" is satisfied by the
      // second row simply being missing).
      expect(el.shadowRoot!.textContent).toContain('yes@p.iam.gserviceaccount.com');
      expect(el.shadowRoot!.textContent).toContain('no@p.iam.gserviceaccount.com');
      expect(iconButtons(el, 'trash').length).toBe(1);
    });

    it('renders no Delete when the Hub sends no capabilities at all', async () => {
      // Fail closed: absent capabilities are not permission, and this is the
      // shape a list route that has not been taught to compute them returns.
      const el = await createComponent(
        { scope: 'hub' },
        makeFetch([], { items: [account({ _capabilities: undefined })] })
      );

      expect(el.shadowRoot!.textContent).toContain('one@gcp-project-xyz');
      expect(iconButtons(el, 'trash').length).toBe(0);
    });

    it('deletes a hub-scoped account at its flat address', async () => {
      const calls: FetchCall[] = [];
      const el = await createComponent(
        { scope: 'hub' },
        makeFetch(calls, {
          items: [account({ id: 'sa-1', _capabilities: { actions: ['delete'] } })],
        })
      );

      // altKey skips the confirm(), which happy-dom does not implement.
      await (
        el as unknown as {
          handleDelete: (a: unknown, e: MouseEvent) => Promise<void>;
        }
      ).handleDelete(account({ id: 'sa-1' }), new MouseEvent('click', { altKey: true }));

      const del = calls.find((c) => c.method === 'DELETE');
      expect(del).toBeDefined();
      expect(del!.url).toBe('/api/v1/gcp-service-accounts/sa-1');
      // A hub-scoped account has no owning project, so any /projects/ address
      // for it is borrowed from something else on screen.
      expect(del!.url).not.toContain('/projects/');
    });

    it('re-verifies a hub-scoped account at its flat address', async () => {
      const calls: FetchCall[] = [];
      const el = await createComponent(
        { scope: 'hub' },
        makeFetch(calls, {
          items: [account({ id: 'sa-1', _capabilities: { actions: ['verify'] } })],
        })
      );

      const verifyButtons = iconButtons(el, 'arrow-clockwise');
      expect(verifyButtons.length).toBe(1);

      await (el as unknown as { handleVerify: (a: unknown) => Promise<void> }).handleVerify(
        account({ id: 'sa-1' })
      );

      const post = calls.find((c) => c.method === 'POST');
      expect(post).toBeDefined();
      expect(post!.url).toBe('/api/v1/gcp-service-accounts/sa-1/verify');
    });

    it('re-verifies a project-scoped account at its nested address', async () => {
      // Same button, different address, decided by the ACCOUNT's scope rather
      // than by the list's — which is what lets a project list show hub-scoped
      // rows (includeHubScoped) without mis-addressing them.
      const calls: FetchCall[] = [];
      const el = await createComponent(
        { scope: 'project', scopeId: 'proj-1' },
        makeFetch(calls, {
          items: [
            account({
              id: 'sa-p',
              scope: 'project',
              scopeId: 'proj-1',
              _capabilities: { actions: ['verify'] },
            }),
          ],
        })
      );

      await (el as unknown as { handleVerify: (a: unknown) => Promise<void> }).handleVerify(
        account({ id: 'sa-p', scope: 'project', scopeId: 'proj-1' })
      );

      const post = calls.find((c) => c.method === 'POST');
      expect(post!.url).toBe('/api/v1/projects/proj-1/gcp-service-accounts/sa-p/verify');
    });
  });

  describe('links out', () => {
    it('links parentless accounts to a detail page and project ones nowhere', async () => {
      const hubEl = await createComponent(
        { scope: 'hub' },
        makeFetch([], { items: [account({ id: 'sa-1' })] })
      );
      const projectEl = await createComponent(
        { scope: 'project', scopeId: 'proj-1' },
        makeFetch([], { items: [account({ id: 'sa-p', scope: 'project', scopeId: 'proj-1' })] })
      );

      const hubLink = hubEl.shadowRoot!.querySelector('a[href^="/settings/service-accounts/"]');
      expect(hubLink).not.toBeNull();
      expect(hubLink!.getAttribute('href')).toBe('/settings/service-accounts/sa-1');

      // The project row still renders its email — it just is not a link.
      expect(projectEl.shadowRoot!.textContent).toContain('one@gcp-project-xyz');
      expect(
        projectEl.shadowRoot!.querySelector('a[href^="/settings/service-accounts/"]')
      ).toBeNull();
    });
  });
});
