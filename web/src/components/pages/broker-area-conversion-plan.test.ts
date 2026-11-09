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
 * The read-only conversion plan section of a legacy Runtime Broker's page.
 * It is shown, and fetched, only with hub.flat_runtime_brokers on and only
 * for a legacy broker; it handles the never-reported, not-legacy and
 * route-off answers; findings show fixed texts and key paths, never a
 * message; hidden agents and projects are counts only; a split group lists
 * only published fields that differ, else a fixed text; every string is
 * text; and there are no action controls.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

import type { RuntimeBroker, RuntimeTargetPresentation } from '../../shared/types.js';
import type { ConversionPlanResponse } from '../../shared/conversion-plan.js';
import { FLAT_BROKERS_FLAG } from '../../utils/feature-flags.js';
import { FLAT_K8S, LEGACY, jsonResponse, settle } from './__fixtures__/broker-area.js';

type Section = HTMLElement & { broker: RuntimeBroker | null; updateComplete: Promise<boolean> };

const PLAN_URL = `/api/v1/runtime-brokers/${LEGACY.id}/conversion-plan`;

function k8s(
  key: string,
  sharedDirSize: string,
  extra: Record<string, unknown> = {}
): RuntimeTargetPresentation {
  return {
    schemaVersion: 1,
    key,
    name: key,
    target: { type: 'kubernetes', namespace: 'agents', sharedDirSize, ...extra },
  } as RuntimeTargetPresentation;
}

function response(over: Partial<ConversionPlanResponse> = {}): ConversionPlanResponse {
  return {
    brokerId: LEGACY.id,
    observedAt: '2026-11-03T12:00:00Z',
    plan: {
      schemaVersion: 1,
      instances: [
        { schemaVersion: 1, key: 'docker-local', name: 'docker-local', target: { type: 'docker' } },
      ],
      profileMap: { a: 'docker-local' },
      findings: [
        {
          code: 'unsupported_flat_target_type',
          severity: 'blocking',
          path: 'profiles.pod.runtime',
          profiles: ['pod'],
          // Not part of the contract: a planted message must never render.
          message: 'MESSAGE-PLANTED-7731',
        } as never,
      ],
      computedAt: '2026-11-03T11:59:00Z',
    },
    agents: {
      items: [
        {
          id: 'ag 1',
          name: 'worker',
          projectId: 'p1',
          phase: 'running',
          profile: 'a',
          mapping: 'mapped',
          instance: 'docker-local',
        },
        { id: 'ag2', name: 'pod-worker', profile: 'pod', mapping: 'no_mapping' },
      ],
      hiddenCount: 3,
      terminalCount: 1,
    },
    projects: {
      items: [
        {
          id: 'p1',
          name: 'visible-project',
          activeProfile: { profile: 'a', mapping: 'mapped', instance: 'docker-local' },
          serviceAccountProfiles: [{ profile: 'pod', mapping: 'no_mapping' }],
        },
      ],
      hiddenCount: 2,
    },
    capacity: {
      outcome: 'preserve_by_split',
      maxAgents: 10,
      allocations: [{ instance: 'docker-local', maxAgents: 10 }],
      flags: ['proposed_instance_keys: allocations are keyed by the proposed instance keys'],
    },
    ...over,
  };
}

let planStatus = 200;
let planBody: unknown = response();
let fetchMock: ReturnType<typeof vi.fn>;
const mounted: HTMLElement[] = [];

function setFlag(on: boolean): void {
  window.__SCION_FEATURES__ = { ...(window.__SCION_FEATURES__ ?? {}), [FLAT_BROKERS_FLAG]: on };
}

async function mount(b: RuntimeBroker): Promise<Section> {
  const el = document.createElement('scion-broker-area-conversion-plan') as Section;
  el.broker = b;
  document.body.appendChild(el);
  mounted.push(el);
  await settle(el);
  return el;
}

function planCalls(): unknown[][] {
  return fetchMock.mock.calls.filter(([u]) => String(u).includes('/conversion-plan'));
}

function text(el: Section): string {
  return el.shadowRoot!.textContent ?? '';
}

describe('scion-broker-area-conversion-plan', () => {
  beforeAll(async () => {
    await import('./broker-area-conversion-plan.js');
  }, 60_000);

  beforeEach(() => {
    setFlag(true);
    planStatus = 200;
    planBody = response();
    fetchMock = vi.fn((url: string) => {
      if (url === PLAN_URL) return jsonResponse(planBody, planStatus);
      return jsonResponse({}, 404);
    });
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    while (mounted.length) mounted.pop()!.remove();
    vi.unstubAllGlobals();
    setFlag(false);
  });

  it('experiment off: renders nothing and makes no request', async () => {
    setFlag(false);
    const el = await mount(LEGACY);
    expect(el.shadowRoot!.querySelector('.plan-section')).toBeNull();
    expect(planCalls()).toHaveLength(0);
  });

  it('flat broker: renders nothing and makes no request', async () => {
    const el = await mount(FLAT_K8S);
    expect(el.shadowRoot!.querySelector('.plan-section')).toBeNull();
    expect(planCalls()).toHaveLength(0);
  });

  it('reads the plan with one GET and no other method', async () => {
    await mount(LEGACY);
    const calls = planCalls();
    expect(calls).toHaveLength(1);
    expect(calls[0][0]).toBe(PLAN_URL);
    const method = (calls[0][1] as RequestInit | undefined)?.method ?? 'GET';
    expect(method).toBe('GET');
  });

  it('no snapshot: 404 conversion_plan_not_reported says to upgrade', async () => {
    planStatus = 404;
    planBody = { error: { code: 'conversion_plan_not_reported', message: 'SERVER-TEXT' } };
    const el = await mount(LEGACY);
    expect(el.shadowRoot!.querySelector('.not-reported')?.textContent).toContain(
      'has not reported a plan'
    );
    expect(text(el)).not.toContain('SERVER-TEXT');
  });

  it('route off on the Hub: another 404 shows only "not available"', async () => {
    planStatus = 404;
    planBody = { error: { code: 'not_found', message: 'route' } };
    const el = await mount(LEGACY);
    expect(el.shadowRoot!.querySelector('.unavailable')).not.toBeNull();
    expect(el.shadowRoot!.querySelector('.findings')).toBeNull();
  });

  it('409 runtime_broker_not_legacy: renders nothing', async () => {
    planStatus = 409;
    planBody = { error: { code: 'runtime_broker_not_legacy', message: 'flat' } };
    const el = await mount(LEGACY);
    expect(el.shadowRoot!.querySelector('.plan-section')).toBeNull();
  });

  it('other errors: a fixed text with the status, not the server text', async () => {
    planStatus = 500;
    planBody = { error: { code: 'internal', message: 'SERVER-TEXT' } };
    const el = await mount(LEGACY);
    expect(el.shadowRoot!.querySelector('.plan-error')?.textContent).toContain('HTTP 500');
    expect(text(el)).not.toContain('SERVER-TEXT');
  });

  it('network failure: the fixed text with no HTTP status', async () => {
    fetchMock.mockImplementation(() => {
      throw new TypeError('network down');
    });
    const el = await mount(LEGACY);
    const err = el.shadowRoot!.querySelector('.plan-error')?.textContent ?? '';
    expect(err).toContain('Could not load the plan.');
    expect(err).not.toContain('HTTP');
    expect(text(el)).not.toContain('network down');
  });

  it('a denied broker read (404 not_found) renders "The plan is not available."', async () => {
    planStatus = 404;
    planBody = { error: { code: 'not_found', message: 'RuntimeBroker not found' } };
    const el = await mount(LEGACY);
    expect(el.shadowRoot!.querySelector('.unavailable')?.textContent).toContain(
      'The plan is not available.'
    );
    expect(text(el)).not.toContain('RuntimeBroker not found');
  });

  it('a broker switch issues a second GET and drops the first response', async () => {
    const other: RuntimeBroker = {
      ...LEGACY,
      id: 'legacy-2',
      name: 'other-box',
      slug: 'other-box',
    };
    const otherUrl = `/api/v1/runtime-brokers/${other.id}/conversion-plan`;
    const planWith = (key: string): ConversionPlanResponse =>
      response({
        plan: {
          schemaVersion: 1,
          instances: [{ schemaVersion: 1, key, name: key, target: { type: 'docker' } }],
          profileMap: {},
          findings: [],
        },
      });
    // The first response arrives, but its body is held until after the switch.
    let releaseFirstBody: (v: unknown) => void = () => {};
    const firstBody = new Promise((resolve) => {
      releaseFirstBody = resolve;
    });
    const firstResponse = {
      ok: true,
      status: 200,
      headers: new Headers(),
      json: () => firstBody,
    } as unknown as Response;
    fetchMock.mockImplementation((url: string) => {
      if (url === PLAN_URL) return firstResponse;
      if (url === otherUrl) return jsonResponse(planWith('SECOND-ONLY'));
      return jsonResponse({}, 404);
    });
    const el = await mount(LEGACY);
    el.broker = other;
    await settle(el);
    releaseFirstBody(planWith('FIRST-ONLY'));
    await settle(el);
    expect(planCalls().map(([u]) => u)).toEqual([PLAN_URL, otherUrl]);
    expect(text(el)).toContain('SECOND-ONLY');
    expect(text(el)).not.toContain('FIRST-ONLY');
  });

  it('with findings: fixed text, key path and profiles, never a message', async () => {
    const el = await mount(LEGACY);
    const row = el.shadowRoot!.querySelector('.findings tr.finding')!;
    expect(row.textContent).toContain('unsupported_flat_target_type');
    expect(row.querySelector('.finding-path')?.textContent).toBe('profiles.pod.runtime');
    expect(row.textContent).toContain('pod');
    expect(row.querySelector('.finding-text')?.textContent).toContain(
      'no flat Runtime Broker target'
    );
    expect(text(el)).not.toContain('MESSAGE-PLANTED-7731');
    // The blocked profile appears in the map as unmapped, with its code.
    expect(el.shadowRoot!.querySelector('.profile-map')?.textContent).toContain('no mapping');
  });

  it('shows instances, capacity, bound agents and the CLI pointer', async () => {
    const el = await mount(LEGACY);
    const t = text(el);
    expect(t).toContain('docker-local');
    expect(t).toContain('The limit is split across the proposed instances');
    expect(t).toContain('proposed_instance_keys');
    expect(t).toContain('1 finished agents are not listed');
    expect(t).toContain('Agents cannot move onto a flat Runtime Broker today');
    expect(t).toContain('scion runtime-broker convert --plan');
    const link = el.shadowRoot!.querySelector('.bound-agents a') as HTMLAnchorElement;
    expect(link.getAttribute('href')).toBe('/agents/ag%201');
  });

  it('hidden agents and projects: counts only, never a name', async () => {
    planBody = {
      ...response(),
      projects: {
        ...response().projects,
        // Not part of the contract: a hidden project's name must never render.
        hiddenItems: [{ id: 'hid-1', name: 'HIDDEN-PROJECT-PLANTED' }],
      },
    };
    const el = await mount(LEGACY);
    expect(el.shadowRoot!.querySelector('.hidden-projects')?.textContent).toContain(
      '2 projects not visible to you'
    );
    expect(el.shadowRoot!.querySelector('.hidden-agents')?.textContent).toContain(
      '3 agents not visible to you'
    );
    expect(el.shadowRoot!.querySelector('.providing-projects')?.textContent).toContain(
      'visible-project'
    );
    expect(text(el)).not.toContain('HIDDEN-PROJECT-PLANTED');
    expect(text(el)).not.toContain('hid-1');
  });

  it('split group differing in a published field lists that field with both values', async () => {
    planBody = response({
      plan: {
        schemaVersion: 1,
        instances: [k8s('grp-a', '40Gi'), k8s('grp-b', '80Gi')],
        profileMap: { a: 'grp-a', b: 'grp-b' },
        findings: [
          {
            code: 'profile_conflict',
            severity: 'notice',
            path: 'profiles.*',
            profiles: ['a', 'b'],
          },
        ],
      },
    });
    const el = await mount(LEGACY);
    const rows = Array.from(el.shadowRoot!.querySelectorAll('.split-difference'));
    expect(rows).toHaveLength(1);
    expect(rows[0].textContent).toContain('Shared dir size');
    expect(rows[0].textContent).toContain('40Gi');
    expect(rows[0].textContent).toContain('80Gi');
    expect(el.shadowRoot!.querySelector('.split-not-published')).toBeNull();
  });

  it('split group differing only in an unpublished field shows the fixed text and no value', async () => {
    planBody = response({
      plan: {
        schemaVersion: 1,
        // A field outside the published set, planted on one instance only.
        instances: [
          k8s('grp-a', '40Gi', { kubeconfig: '/home/u/KUBE-PLANTED' }),
          k8s('grp-b', '40Gi'),
        ],
        profileMap: { a: 'grp-a', b: 'grp-b' },
        findings: [
          {
            code: 'profile_conflict',
            severity: 'notice',
            path: 'profiles.*',
            profiles: ['a', 'b'],
          },
        ],
      },
    });
    const el = await mount(LEGACY);
    expect(el.shadowRoot!.querySelector('.split-not-published')?.textContent).toContain(
      'not published to the Hub'
    );
    expect(el.shadowRoot!.querySelectorAll('.split-difference')).toHaveLength(0);
    expect(text(el)).not.toContain('KUBE-PLANTED');
    expect(text(el)).not.toContain('kubeconfig');
  });

  it('plan_truncated: a warning banner', async () => {
    planBody = response({
      plan: {
        schemaVersion: 1,
        instances: [],
        profileMap: {},
        findings: [{ code: 'plan_truncated', severity: 'blocking', path: '', profiles: [] }],
      },
    });
    const el = await mount(LEGACY);
    expect(el.shadowRoot!.querySelector('sl-alert.plan-truncated')).not.toBeNull();
    expect(text(el)).toContain('No instances proposed.');
  });

  it('renders response strings as text, never as markup', async () => {
    const markup = '<img src=x onerror="window.__pwned=1">';
    planBody = response({
      plan: {
        schemaVersion: 1,
        instances: [{ schemaVersion: 1, key: markup, name: markup, target: { type: markup } }],
        profileMap: { [markup]: markup },
        findings: [{ code: markup, severity: 'notice', path: markup, profiles: [markup] }],
      },
      agents: { items: [{ id: markup, name: markup, mapping: 'no_mapping' }], hiddenCount: 0 },
      projects: { items: [{ id: markup, name: markup }], hiddenCount: 0 },
      capacity: { outcome: markup, reasons: [markup], flags: [markup] },
    });
    const el = await mount(LEGACY);
    expect(el.shadowRoot!.querySelector('img')).toBeNull();
    expect(text(el)).toContain(markup);
  });

  it('has no action controls', async () => {
    const el = await mount(LEGACY);
    expect(el.shadowRoot!.querySelector('.plan-section')).not.toBeNull();
    expect(
      el.shadowRoot!.querySelectorAll(
        'button, sl-button, form, input, select, textarea, sl-input, sl-select'
      )
    ).toHaveLength(0);
  });
});
