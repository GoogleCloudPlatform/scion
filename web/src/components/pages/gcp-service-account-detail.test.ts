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
 * Per-account status sections on the GCP service account detail page
 * (ptone/scion#4018).
 *
 * Rules:
 *   - The sections are project-relative. With ?project= the page reads the
 *     status view from that project and renders every section; without it
 *     the page says so and requests no status.
 *   - Mapping rows show the KSA, namespace, source and report age when the
 *     broker reported them, and the incomplete and ambiguous markers; an
 *     older broker's row shows the state alone.
 *   - The binding is shown as the hub states it, never upgraded.
 *   - A project-scoped account is read from the nested GET and gets no
 *     actions, even if a capability were present.
 */

import { describe, it, expect, beforeAll, beforeEach, afterEach, vi } from 'vitest';

import type { GCPServiceAccount, GCPServiceAccountStatus } from '../../shared/types.js';

const apiFetch = vi.fn();
vi.mock('../../client/api.js', () => ({
  apiFetch: (...args: unknown[]): unknown => apiFetch(...args) as unknown,
  extractApiError: (r: Response): Promise<string> => Promise.resolve(`HTTP ${r.status}`),
}));

const EMAIL = 'worker@example.com';
const NOW = new Date('2026-10-10T12:00:00Z');

function row(overrides: Partial<GCPServiceAccount> = {}): GCPServiceAccount {
  return {
    id: 'sa-1',
    scope: 'hub',
    scopeId: 'hub',
    email: EMAIL,
    projectId: 'example-gcp-project',
    displayName: 'Worker',
    defaultScopes: [],
    verified: true,
    verifiedAt: '2026-10-10T10:00:00Z',
    verificationStatus: 'verified',
    createdBy: 'u-1',
    createdAt: '2026-10-01T00:00:00Z',
    _capabilities: { actions: ['read', 'verify', 'delete'] },
    ...overrides,
  } as GCPServiceAccount;
}

function statusView(overrides: Partial<GCPServiceAccountStatus> = {}): GCPServiceAccountStatus {
  return {
    account: { id: 'sa-1', displayName: 'Worker', scope: 'hub', email: EMAIL },
    verification: { status: 'verified', verified: true, verifiedAt: '2026-10-10T10:00:00Z' },
    mappings: [
      {
        brokerId: 'b1',
        brokerName: 'b',
        profile: 'gke',
        state: 'mapped',
        kubernetesServiceAccount: 'worker-ksa',
        namespace: 'agents',
        source: 'discovered',
        reportedAt: '2026-10-10T11:55:00Z',
      },
      {
        brokerId: 'b1',
        brokerName: 'b',
        profile: 'gke-2',
        state: 'unknown',
        unknownReason: 'report_incomplete',
        incomplete: true,
        incompleteReason: 'list_failed',
        reportedAt: '2026-10-10T11:58:00Z',
      },
      {
        brokerId: 'b1',
        brokerName: 'b',
        profile: 'gke-stale',
        state: 'unknown',
        unknownReason: 'report_stale',
        reportedAt: '2026-10-10T11:00:00Z',
      },
      {
        brokerId: 'b1',
        brokerName: 'b',
        profile: 'gke-pre4',
        state: 'unknown',
        unknownReason: 'report_old_version',
      },
      { brokerId: 'b1', brokerName: 'b', profile: 'gke-3', state: 'not_mapped', ambiguous: true },
      { brokerId: 'b1', brokerName: 'b', profile: 'gke-old', state: 'mapped' },
      { brokerId: 'b1', brokerName: 'b', profile: 'gke-silent', state: 'not_reported' },
    ],
    workloadIdentityBinding: { state: 'unknown', reason: 'not checked' },
    defaultFor: [{ kind: 'project' }, { kind: 'profile', profile: 'gke' }, { kind: 'hub' }],
    agents: { count: 3, names: ['a1', 'a2'] },
    nextStep: { code: 'none', message: 'Nothing missing that the hub can check.' },
    ...overrides,
  };
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/** Answers by URL; anything unexpected is a 404 so a wrong address fails. */
function routes(map: Record<string, () => Response>): void {
  apiFetch.mockImplementation((url: string) => {
    const handler = map[url];
    return Promise.resolve(handler ? handler() : json({ error: 'not found' }, 404));
  });
}

type Page = HTMLElement & { updateComplete: Promise<unknown>; shadowRoot: ShadowRoot };

async function mount(path: string): Promise<Page> {
  window.history.pushState({}, '', path);
  const el = document.createElement('scion-page-gcp-service-account-detail') as Page;
  document.body.appendChild(el);
  for (let i = 0; i < 5; i++) {
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
  }
  return el;
}

async function statusRoot(el: Page): Promise<ShadowRoot> {
  const child = el.shadowRoot.querySelector('scion-gcp-service-account-status') as
    | (HTMLElement & { updateComplete: Promise<unknown>; shadowRoot: ShadowRoot })
    | null;
  expect(child).not.toBeNull();
  await child!.updateComplete;
  return child!.shadowRoot;
}

function sectionText(root: ShadowRoot, name: string): string {
  return (root.querySelector(`[data-section="${name}"]`)?.textContent ?? '')
    .replace(/\s+/g, ' ')
    .trim();
}

function buttonLabels(el: Page): string[] {
  return Array.from(el.shadowRoot.querySelectorAll('sl-button')).map((b) =>
    (b.textContent ?? '').replace(/\s+/g, ' ').trim()
  );
}

describe('GCP service account detail: status sections', () => {
  beforeAll(async () => {
    await import('./gcp-service-account-detail.js');
  }, 30_000);

  beforeEach(() => {
    apiFetch.mockReset();
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(NOW);
  });

  afterEach(() => {
    vi.useRealTimers();
    document.body.innerHTML = '';
  });

  it('renders every section from the project-relative status view', async () => {
    routes({
      '/api/v1/projects/proj-1/gcp-service-accounts/sa-1/status': () => json(statusView()),
      '/api/v1/gcp-service-accounts/sa-1': () => json(row()),
    });
    const el = await mount('/settings/service-accounts/sa-1?project=proj-1');
    const root = await statusRoot(el);

    expect(sectionText(root, 'verification')).toContain('Verified');
    expect(sectionText(root, 'verification')).toContain('checked 2 hours ago');

    const mapped = root.querySelector('tr[data-profile="gke"]')!.textContent!.replace(/\s+/g, ' ');
    expect(mapped).toContain('b/gke');
    expect(mapped).toContain('Mapped');
    expect(mapped).toContain('worker-ksa');
    expect(mapped).toContain('agents');
    expect(mapped).toContain('discovered');
    expect(mapped).toContain('5 minutes ago');

    // "Not mapped" only on an authoritative report; otherwise unknown, with why.
    const incomplete = root.querySelector('tr[data-profile="gke-2"]')!;
    expect(incomplete.textContent).toContain('Unknown');
    expect(incomplete.textContent).not.toContain('Not mapped');
    expect(incomplete.querySelector('[data-note="unknown"]')!.textContent).toContain(
      'incomplete: the broker could not list Kubernetes service accounts'
    );
    expect(incomplete.querySelector('[data-note="ambiguous"]')).toBeNull();
    expect(
      root.querySelector('tr[data-profile="gke-stale"] [data-note="unknown"]')!.textContent
    ).toContain('stale (reported 1 hour ago)');
    expect(
      root.querySelector('tr[data-profile="gke-pre4"] [data-note="unknown"]')!.textContent
    ).toContain('too old a version to tell');

    const ambiguous = root.querySelector('tr[data-profile="gke-3"]')!;
    expect(ambiguous.querySelector('[data-note="ambiguous"]')).not.toBeNull();
    expect(ambiguous.textContent).toContain('Not mapped');

    // An older broker's row: the state alone, no details or markers.
    const old = root.querySelector('tr[data-profile="gke-old"]')!;
    expect(old.textContent).toContain('Mapped');
    expect(old.querySelector('[data-note]')).toBeNull();
    expect(old.textContent).not.toContain('ago');
    expect(root.querySelector('tr[data-profile="gke-silent"]')!.textContent).toContain(
      'Not reported'
    );

    expect(sectionText(root, 'binding')).toBe('Workload Identity binding Unknown (not checked)');
    expect(sectionText(root, 'defaults')).toContain('Project default');
    expect(sectionText(root, 'defaults')).toContain('Profile gke');
    expect(sectionText(root, 'defaults')).toContain('Hub default');
    expect(sectionText(root, 'agents')).toContain('Agents using it (3)');
    expect(sectionText(root, 'agents')).toContain('a1');
    expect(sectionText(root, 'agents')).toContain('and 1 more');
    expect(sectionText(root, 'next-step')).toContain('Nothing missing that the hub can check.');

    // A hub-scoped account keeps its capability-driven actions.
    expect(buttonLabels(el)).toEqual(['Re-verify', 'Delete']);
  });

  it('shows the binding as the hub states it, never upgraded', async () => {
    routes({
      '/api/v1/projects/proj-1/gcp-service-accounts/sa-1/status': () =>
        json(statusView({ workloadIdentityBinding: { state: 'not_bound' } })),
      '/api/v1/gcp-service-accounts/sa-1': () => json(row()),
    });
    const root = await statusRoot(await mount('/settings/service-accounts/sa-1?project=proj-1'));
    expect(sectionText(root, 'binding')).toBe('Workload Identity binding Not bound');
  });

  it('renders empty sections as none, not as missing', async () => {
    routes({
      '/api/v1/projects/proj-1/gcp-service-accounts/sa-1/status': () =>
        json(
          statusView({
            verification: { status: 'unverified', verified: false },
            mappings: [],
            defaultFor: [],
            agents: { count: 0, names: [] },
            nextStep: { code: 'not_verified', message: 'Not verified.' },
          })
        ),
      '/api/v1/gcp-service-accounts/sa-1': () => json(row()),
    });
    const root = await statusRoot(await mount('/settings/service-accounts/sa-1?project=proj-1'));
    expect(sectionText(root, 'verification')).toContain('Unverified');
    expect(sectionText(root, 'mappings')).toContain('No Kubernetes broker profiles in this project.');
    expect(sectionText(root, 'defaults')).toContain('None');
    expect(sectionText(root, 'agents')).toContain('Agents using it (0) None');
    expect(sectionText(root, 'next-step')).toContain('Not verified.');
  });

  it('says the sections are project-relative and requests no status without ?project=', async () => {
    routes({ '/api/v1/gcp-service-accounts/sa-1': () => json(row()) });
    const el = await mount('/settings/service-accounts/sa-1');

    expect(el.shadowRoot.querySelector('[data-note="project-relative"]')).not.toBeNull();
    expect(el.shadowRoot.querySelector('scion-gcp-service-account-status')).toBeNull();
    const urls = apiFetch.mock.calls.map((c) => c[0] as string);
    expect(urls).toEqual(['/api/v1/gcp-service-accounts/sa-1']);
  });

  it('reads a project-scoped account from its project and renders no actions', async () => {
    routes({
      '/api/v1/projects/proj-1/gcp-service-accounts/sa-p/status': () =>
        json(
          statusView({ account: { id: 'sa-p', displayName: 'Worker', scope: 'project', email: EMAIL } })
        ),
      // A capability here must still not produce a button: the nested GET is
      // not where capabilities come from.
      '/api/v1/projects/proj-1/gcp-service-accounts/sa-p': () =>
        json(row({ id: 'sa-p', scope: 'project', scopeId: 'proj-1' })),
    });
    const el = await mount('/settings/service-accounts/sa-p?project=proj-1');

    expect(el.shadowRoot.querySelector('scion-detail-header')?.getAttribute('heading')).toBe(EMAIL);
    expect(buttonLabels(el)).toEqual([]);
    expect(el.shadowRoot.querySelector('scion-back-link')?.getAttribute('href')).toBe(
      '/projects/proj-1/settings?tab=gcp-sa'
    );
    const urls = apiFetch.mock.calls.map((c) => c[0] as string);
    expect(urls).not.toContain('/api/v1/gcp-service-accounts/sa-p');
    await statusRoot(el);
  });

  for (const [name, status] of [
    ['forbidden', 403],
    ['a server error', 500],
    ['an older hub without the endpoint', 404],
  ] as const) {
    it(`shows the status error, not "not found", when the status view answers ${name}`, async () => {
      // A project-scoped id from the project settings list. The flat GET would
      // answer 404 for it; the nested GET must not be used to route around a
      // failed status read.
      routes({
        '/api/v1/projects/proj-1/gcp-service-accounts/sa-p/status': () =>
          json({ error: name }, status),
        '/api/v1/projects/proj-1/gcp-service-accounts/sa-p': () =>
          json(row({ id: 'sa-p', scope: 'project', scopeId: 'proj-1' })),
      });
      const el = await mount('/settings/service-accounts/sa-p?project=proj-1');

      const text = (el.shadowRoot.querySelector('.error-state')?.textContent ?? '').replace(
        /\s+/g,
        ' '
      );
      expect(text).toContain(`Could not load this account in project proj-1: HTTP ${status}`);
      expect(el.shadowRoot.querySelector('scion-detail-header')).toBeNull();
      expect(buttonLabels(el)).toEqual([]);
      expect(el.shadowRoot.querySelector('scion-back-link')?.getAttribute('href')).toBe(
        '/projects/proj-1/settings?tab=gcp-sa'
      );
      const urls = apiFetch.mock.calls.map((c) => c[0] as string);
      expect(urls).toEqual(['/api/v1/projects/proj-1/gcp-service-accounts/sa-p/status']);
    });
  }
});
