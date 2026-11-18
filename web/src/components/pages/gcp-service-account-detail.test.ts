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
import {
  K8S_WORKLOAD_IDENTITY_SETUP_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';

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
      {
        brokerId: 'b1',
        brokerName: 'b',
        profile: 'gke-3',
        state: 'unknown',
        unknownReason: 'ambiguous_mapping',
        ambiguous: true,
      },
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
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
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
    expect(ambiguous.querySelector('[data-note="unknown"]')).toBeNull();
    expect(ambiguous.textContent).toContain('Unknown');

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
    expect(root.querySelector('[data-section="next-step"] sl-icon')?.getAttribute('name')).toBe(
      'check-circle'
    );

    // A hub-scoped account keeps its capability-driven actions.
    expect(buttonLabels(el)).toEqual(['Re-verify', 'Delete']);
  });

  it('shows the binding as the hub states it, never upgraded, and uses info-circle when nextStep is not_bound', async () => {
    routes({
      '/api/v1/projects/proj-1/gcp-service-accounts/sa-1/status': () =>
        json(
          statusView({
            workloadIdentityBinding: { state: 'not_bound' },
            nextStep: {
              code: 'not_bound',
              message:
                "Workload Identity role roles/iam.workloadIdentityUser is not granted on this account; run the per-target manualStep gcloud command: gcloud iam service-accounts add-iam-policy-binding 'worker@example.com' ...",
            },
          })
        ),
      '/api/v1/gcp-service-accounts/sa-1': () => json(row()),
    });
    const root = await statusRoot(await mount('/settings/service-accounts/sa-1?project=proj-1'));
    expect(sectionText(root, 'binding')).toBe('Workload Identity binding Not bound');
    const icon = root.querySelector('[data-section="next-step"] sl-icon');
    expect(icon?.getAttribute('name')).toBe('info-circle');
    expect(icon?.getAttribute('name')).not.toBe('check-circle');
    expect(sectionText(root, 'next-step')).toContain('add-iam-policy-binding');
  });

  it('keeps the binding column and target list hidden while hub.k8s_workload_identity_setup is off', async () => {
    routes({
      '/api/v1/projects/proj-1/gcp-service-accounts/sa-1/status': () =>
        json(
          statusView({
            mappings: [
              {
                brokerId: 'b1',
                brokerName: 'b',
                profile: 'gke',
                state: 'mapped',
                kubernetesServiceAccount: 'scion-worker',
                namespace: 'agents',
                source: 'discovered',
                workloadIdentityBinding: {
                  state: 'bound',
                  pool: 'p.svc.id.goog',
                  member: 'serviceAccount:p.svc.id.goog[agents/scion-worker]',
                },
              },
            ],
            workloadIdentityBinding: {
              state: 'bound',
              targets: [
                {
                  pool: 'p.svc.id.goog',
                  namespace: 'agents',
                  ksa: 'scion-worker',
                  member: 'serviceAccount:p.svc.id.goog[agents/scion-worker]',
                  profiles: ['b/gke'],
                  state: 'bound',
                },
              ],
            },
          })
        ),
      '/api/v1/gcp-service-accounts/sa-1': () => json(row({ managed: true })),
    });
    const root = await statusRoot(await mount('/settings/service-accounts/sa-1?project=proj-1'));
    expect(root.querySelector('th[data-col="binding"]')).toBeNull();
    expect(root.querySelector('td[data-col="binding"]')).toBeNull();
    expect(root.querySelector('[data-target]')).toBeNull();
  });

  it('renders the binding column, per-target states, manual gcloud step, and stale-target notes when hub.k8s_workload_identity_setup is on, and toggles dynamically', async () => {
    setFeatureFlag(K8S_WORKLOAD_IDENTITY_SETUP_FLAG, true);
    routes({
      '/api/v1/projects/proj-1/gcp-service-accounts/sa-1/status': () =>
        json(
          statusView({
            mappings: [
              {
                brokerId: 'b1',
                brokerName: 'b',
                profile: 'gke-bound',
                state: 'mapped',
                kubernetesServiceAccount: 'scion-worker',
                namespace: 'agents',
                source: 'discovered',
                reportedAt: '2026-10-10T11:55:00Z',
                workloadIdentityBinding: {
                  state: 'bound',
                  pool: 'p1.svc.id.goog',
                  member: 'serviceAccount:p1.svc.id.goog[agents/scion-worker]',
                },
              },
              {
                brokerId: 'b2',
                brokerName: 'old-b',
                profile: 'gke-stale-target',
                state: 'mapped',
                workloadIdentityBinding: {
                  state: 'not_bound',
                  pool: 'p2.svc.id.goog',
                  member: 'serviceAccount:p2.svc.id.goog[team/scion-worker]',
                  stale: true,
                  staleReason: 'broker_no_longer_reports',
                },
              },
              {
                brokerId: 'b3',
                brokerName: 'nopool-b',
                profile: 'gke-nopool',
                state: 'not_mapped',
                workloadIdentityBinding: {
                  state: 'unknown',
                  reason: 'no_workload_identity_pool',
                },
              },
              {
                brokerId: 'b4',
                brokerName: 'prewi-b',
                profile: 'gke-too-old',
                state: 'mapped',
                workloadIdentityBinding: {
                  state: 'unknown',
                  reason: 'broker_too_old',
                },
              },
            ],
            workloadIdentityBinding: {
              state: 'not_bound',
              reason:
                '1 of 2 targets bound; 3 Kubernetes targets report no pool; 1 target from stale broker report',
              targets: [
                {
                  pool: 'p1.svc.id.goog',
                  namespace: 'agents',
                  ksa: 'scion-worker',
                  member: 'serviceAccount:p1.svc.id.goog[agents/scion-worker]',
                  profiles: ['b/gke-bound'],
                  state: 'bound',
                  lastHeartbeat: '2026-10-10T11:58:00Z',
                },
                {
                  pool: 'p2.svc.id.goog',
                  namespace: 'team',
                  ksa: 'scion-worker',
                  member: 'serviceAccount:p2.svc.id.goog[team/scion-worker]',
                  profiles: ['old-b/gke-stale-target'],
                  state: 'not_bound',
                  stale: true,
                  staleReason: 'broker_no_longer_reports',
                  manualStep:
                    "gcloud iam service-accounts add-iam-policy-binding 'worker@example.com' --role roles/iam.workloadIdentityUser --member 'serviceAccount:p2.svc.id.goog[team/scion-worker]'",
                },
                {
                  ksa: 'scion-worker',
                  profiles: ['nopool-b/gke-nopool'],
                  state: 'unknown',
                  reason: 'no_workload_identity_pool',
                },
                {
                  ksa: 'scion-worker',
                  profiles: ['prewi-b/gke-too-old'],
                  state: 'unknown',
                  reason: 'broker_too_old',
                },
                {
                  ksa: 'scion-worker',
                  profiles: ['flat-b'],
                  state: 'unknown',
                  reason: 'flat_broker_pool_unreported',
                },
              ],
            },
            nextStep: {
              code: 'not_bound',
              message:
                "Workload Identity role roles/iam.workloadIdentityUser is not granted on this account for serviceAccount:p2.svc.id.goog[team/scion-worker] (old-b/gke-stale-target). Run the per-target manualStep gcloud command to grant it: gcloud iam service-accounts add-iam-policy-binding 'worker@example.com' --role roles/iam.workloadIdentityUser --member 'serviceAccount:p2.svc.id.goog[team/scion-worker]'",
            },
          })
        ),
      '/api/v1/gcp-service-accounts/sa-1': () => json(row({ managed: true })),
    });

    const el = await mount('/settings/service-accounts/sa-1?project=proj-1');
    const root = await statusRoot(el);

    expect(root.querySelector('th[data-col="binding"]')?.textContent?.trim()).toBe('Binding');

    const boundCell = root.querySelector('tr[data-profile="gke-bound"] td[data-col="binding"]')!;
    expect(boundCell.textContent).toContain('Bound');
    expect(boundCell.querySelector('[data-note="stale-target"]')).toBeNull();

    const staleCell = root.querySelector(
      'tr[data-profile="gke-stale-target"] td[data-col="binding"]'
    )!;
    expect(staleCell.textContent).toContain('Not bound');
    expect(staleCell.querySelector('[data-note="stale-target"]')?.textContent).toContain(
      'the broker no longer reports Workload Identity targets'
    );

    const noPoolCell = root.querySelector('tr[data-profile="gke-nopool"] td[data-col="binding"]')!;
    expect(noPoolCell.textContent).toContain('Unknown');
    expect(noPoolCell.textContent).toContain('no pool reported');

    const tooOldCell = root.querySelector('tr[data-profile="gke-too-old"] td[data-col="binding"]')!;
    expect(tooOldCell.textContent).toContain('Unknown');
    expect(tooOldCell.textContent).toContain('broker too old to report a pool');

    const bindingSection = sectionText(root, 'binding');
    expect(bindingSection).toContain(
      'Not bound (1 of 2 targets bound; 3 Kubernetes targets report no pool; 1 target from stale broker report)'
    );
    expect(bindingSection).toContain(
      "Targets are taken from each broker's last stored heartbeat report."
    );
    expect(bindingSection).toContain('serviceAccount:p1.svc.id.goog[agents/scion-worker]');
    expect(bindingSection).toContain('broker heartbeat 2 minutes ago');
    expect(bindingSection).toContain(
      'Stored target: the broker no longer reports Workload Identity targets.'
    );
    expect(bindingSection).toContain(
      "gcloud iam service-accounts add-iam-policy-binding 'worker@example.com'"
    );
    expect(bindingSection).toContain('nopool-b/gke-nopool');
    expect(bindingSection).toContain('broker too old to report a pool');
    expect(bindingSection).toContain('flat broker does not report a pool yet');
    expect(root.querySelector('[data-section="next-step"] sl-icon')?.getAttribute('name')).toBe(
      'info-circle'
    );

    // Toggling the experiment off hides the presentation column without a reload.
    setFeatureFlag(K8S_WORKLOAD_IDENTITY_SETUP_FLAG, false);
    const child = el.shadowRoot.querySelector('scion-gcp-service-account-status') as HTMLElement & {
      updateComplete: Promise<unknown>;
    };
    await child.updateComplete;
    expect(root.querySelector('th[data-col="binding"]')).toBeNull();
    expect(root.querySelector('[data-target]')).toBeNull();
  });

  it('shows unknown (not minted by this hub) for bring-your-own accounts when the experiment is on', async () => {
    setFeatureFlag(K8S_WORKLOAD_IDENTITY_SETUP_FLAG, true);
    routes({
      '/api/v1/projects/proj-1/gcp-service-accounts/sa-1/status': () =>
        json(
          statusView({
            mappings: [
              {
                brokerId: 'b1',
                brokerName: 'b',
                profile: 'gke',
                state: 'mapped',
                workloadIdentityBinding: { state: 'unknown', reason: 'not minted by this hub' },
              },
            ],
            workloadIdentityBinding: { state: 'unknown', reason: 'not minted by this hub' },
          })
        ),
      '/api/v1/gcp-service-accounts/sa-1': () => json(row({ managed: false })),
    });
    const root = await statusRoot(await mount('/settings/service-accounts/sa-1?project=proj-1'));
    expect(sectionText(root, 'binding')).toBe(
      'Workload Identity binding Unknown (not minted by this hub)'
    );
    expect(
      root.querySelector('tr[data-profile="gke"] td[data-col="binding"]')?.textContent
    ).toContain('not minted by this hub');
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
    expect(sectionText(root, 'mappings')).toContain(
      'No Kubernetes broker profiles in this project.'
    );
    expect(sectionText(root, 'defaults')).toContain('None');
    expect(sectionText(root, 'agents')).toContain('Agents using it (0) None');
    expect(sectionText(root, 'next-step')).toContain('Not verified.');
  });

  it('without ?project=, notes the sections are project-relative and asks no status', async () => {
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
          statusView({
            account: { id: 'sa-p', displayName: 'Worker', scope: 'project', email: EMAIL },
          })
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
    it(`shows the status error, not "not found", on a status answer of ${name}`, async () => {
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
