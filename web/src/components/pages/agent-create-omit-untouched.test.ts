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
 * Create Agent request body (ptone/scion#3902): gcp_identity and
 * config.telemetry are sent only when the user changed them on the form.
 * Untouched, they are omitted so the server applies its own precedence
 * instead of the form pinning the values client-side.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

interface ProfileFixture {
  name: string;
  type: string;
  available: boolean;
}

interface BrokerFixture {
  id: string;
  name: string;
  status: string;
  profiles?: ProfileFixture[];
}

interface CreatePrivate extends HTMLElement {
  loading: boolean;
  name: string;
  projectId: string;
  brokers: BrokerFixture[];
  brokerId: string;
  profile: string;
  gcpMetadataMode: string;
  gcpServiceAccountId: string;
  gcpIdentityUserSet: boolean;
  telemetryEnabled: boolean;
  updateComplete: Promise<unknown>;
  handleSubmit(e: Event, provisionOnly?: boolean): Promise<void>;
}

let projectDefaultMode = '';
let hubTelemetry = false;
let bodies: Array<Record<string, unknown>> = [];

/** A verified account, so a project default of assign applies on load. */
const verifiedServiceAccount = {
  id: 'sa-a',
  scope: 'project',
  scopeId: 'p1',
  email: 'sa-a@example.iam.gserviceaccount.com',
  projectId: 'gcp-proj',
  displayName: '',
  defaultScopes: [],
  verified: true,
  verifiedAt: '2026-01-01T00:00:00Z',
  createdBy: 'user-1',
};

function stubFetch(): void {
  bodies = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString();
      if (url.includes('/api/v1/agents') && init?.method === 'POST') {
        if (typeof init.body === 'string') {
          bodies.push(JSON.parse(init.body) as Record<string, unknown>);
        }
        // A 400 makes handleSubmit stop before navigating away.
        return Promise.resolve({
          ok: false,
          status: 400,
          json: async () => ({ error: { message: 'stub: not created' } }),
        } as Response);
      }
      let body: unknown = { projects: [], brokers: [], templates: [], harnessConfigs: [] };
      if (url.includes('/settings/public')) {
        body = { telemetryEnabled: hubTelemetry };
      } else if (url.includes('/api/v1/projects?')) {
        body = { projects: [{ id: 'p1', name: 'P1' }] };
      } else if (url.includes('/api/v1/projects/p1/settings')) {
        body = !projectDefaultMode
          ? {}
          : projectDefaultMode === 'assign'
            ? { defaultGCPIdentityMode: 'assign', defaultGCPIdentityServiceAccountID: 'sa-a' }
            : { defaultGCPIdentityMode: projectDefaultMode };
      } else if (url.includes('/gcp-service-accounts')) {
        body = { items: [verifiedServiceAccount] };
      }
      return Promise.resolve({ ok: true, status: 200, json: async () => body } as Response);
    })
  );
}

beforeAll(async () => {
  await import('./agent-create.js');
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
  projectDefaultMode = '';
  hubTelemetry = false;
});

async function settle(c: CreatePrivate): Promise<void> {
  await new Promise((r) => setTimeout(r, 0));
  const deadline = Date.now() + 2000;
  while (c.loading && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 5));
    await c.updateComplete;
  }
  await new Promise((r) => setTimeout(r, 20));
  await c.updateComplete;
}

async function mount(): Promise<CreatePrivate> {
  stubFetch();
  const c = document.createElement('scion-page-agent-create') as CreatePrivate;
  document.body.appendChild(c);
  await settle(c);
  c.name = 'test-agent';
  c.projectId = 'p1';
  await c.updateComplete;
  return c;
}

interface TargetFixture {
  label: string;
  brokers: BrokerFixture[];
  brokerId: string;
  profile: string;
}

function singleTypeTarget(type: string): TargetFixture {
  return {
    label: `a broker of type ${type}`,
    brokers: [
      {
        id: `broker-${type}`,
        name: `${type}-broker`,
        status: 'online',
        profiles: [{ name: 'default', type, available: true }],
      },
    ],
    brokerId: `broker-${type}`,
    profile: '',
  };
}

function mixedTarget(profile: string): TargetFixture {
  return {
    label: profile
      ? 'a mixed broker with a kubernetes profile chosen'
      : 'a mixed broker with no profile chosen',
    brokers: [
      {
        id: 'broker-mixed',
        name: 'mixed-broker',
        status: 'online',
        profiles: [
          { name: 'k8s', type: 'kubernetes', available: true },
          { name: 'local', type: 'docker', available: true },
        ],
      },
    ],
    brokerId: 'broker-mixed',
    profile,
  };
}

const noBrokerTarget: TargetFixture = {
  label: 'no broker selected',
  brokers: [],
  brokerId: '',
  profile: '',
};
const dockerTarget = singleTypeTarget('docker');
const k8sTarget = singleTypeTarget('kubernetes');

/** The target runtime selections the omission must hold for. */
const targets: TargetFixture[] = [
  noBrokerTarget,
  dockerTarget,
  singleTypeTarget('podman'),
  singleTypeTarget('apple'),
  k8sTarget,
  mixedTarget(''),
  mixedTarget('k8s'),
];

async function selectTarget(c: CreatePrivate, t: TargetFixture): Promise<void> {
  c.brokers = t.brokers;
  c.brokerId = t.brokerId;
  c.profile = t.profile;
  await c.updateComplete;
}

function gcpIdentitySelect(c: CreatePrivate): HTMLElement & { value: string } {
  const fields = Array.from(c.shadowRoot?.querySelectorAll('.form-field') ?? []);
  const field = fields.find(
    (f) => f.querySelector('label')?.textContent?.trim() === 'GCP Identity'
  );
  const select = field?.querySelector('sl-select');
  expect(select).toBeTruthy();
  return select as HTMLElement & { value: string };
}

function gcpIdentityHint(c: CreatePrivate): string {
  const fields = Array.from(c.shadowRoot?.querySelectorAll('.form-field') ?? []);
  const field = fields.find(
    (f) => f.querySelector('label')?.textContent?.trim() === 'GCP Identity'
  );
  return (field?.querySelector('.hint')?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

/** Operates the GCP Identity select the way a user pick does. */
async function chooseIdentity(c: CreatePrivate, value: string): Promise<void> {
  const select = gcpIdentitySelect(c);
  select.value = value;
  select.dispatchEvent(new Event('sl-change', { bubbles: true, composed: true }));
  await c.updateComplete;
}

/** Operates the telemetry checkbox the way a user click does. */
async function toggleTelemetry(c: CreatePrivate, checked: boolean): Promise<void> {
  const boxes = Array.from(c.shadowRoot?.querySelectorAll('sl-checkbox') ?? []);
  const box = boxes.find((b) => b.textContent?.includes('Enable Telemetry')) as
    | (HTMLElement & { checked: boolean })
    | undefined;
  expect(box).toBeTruthy();
  box!.checked = checked;
  box!.dispatchEvent(new Event('sl-change'));
  await c.updateComplete;
}

async function submit(c: CreatePrivate): Promise<Record<string, unknown>> {
  const before = bodies.length;
  await c.handleSubmit(new Event('submit'));
  expect(bodies).toHaveLength(before + 1);
  return bodies[bodies.length - 1];
}

describe('Create Agent: gcp_identity is sent only when the user chose it', () => {
  for (const projectDefault of ['', 'passthrough', 'assign']) {
    for (const t of targets) {
      it(`omits gcp_identity when untouched, on ${t.label} (project default: ${projectDefault || 'none'})`, async () => {
        projectDefaultMode = projectDefault;
        const c = await mount();
        await selectTarget(c, t);
        expect(c.gcpIdentityUserSet).toBe(false);

        const body = await submit(c);
        expect(body).not.toHaveProperty('gcp_identity');
      });
    }
  }

  it('does not count an applied project default as a user choice', async () => {
    projectDefaultMode = 'passthrough';
    const c = await mount();
    expect(c.gcpMetadataMode).toBe('passthrough');
    expect(c.gcpIdentityUserSet).toBe(false);
    expect(gcpIdentitySelect(c).value).toBe('passthrough');
  });

  for (const t of [dockerTarget, k8sTarget]) {
    it(`does not count an applied project default of assign as a user choice, on ${t.label}`, async () => {
      projectDefaultMode = 'assign';
      const c = await mount();
      await selectTarget(c, t);
      expect(c.gcpMetadataMode).toBe('assign');
      expect(c.gcpServiceAccountId).toBe('sa-a');
      expect(c.gcpIdentityUserSet).toBe(false);
    });
  }

  for (const t of [noBrokerTarget, dockerTarget, k8sTarget]) {
    it(`renders the picker blank with no project default and nothing chosen, on ${t.label}`, async () => {
      const c = await mount();
      await selectTarget(c, t);
      expect(gcpIdentitySelect(c).value).toBe('');
    });
  }

  it('names the hub-wide default in the hint with no project default on a docker broker', async () => {
    const c = await mount();
    await selectTarget(c, dockerTarget);
    expect(gcpIdentityHint(c)).toBe(
      'No mode chosen: the hub-wide default applies, or Block if none is configured.'
    );
  });

  // The picker is blank, so picking Block (this page's own internal
  // placeholder value) is a real change and is sent explicitly.
  it('sends Block when the user picks it with no project default on a docker broker', async () => {
    const c = await mount();
    await selectTarget(c, dockerTarget);
    expect(c.gcpMetadataMode).toBe('block');

    await chooseIdentity(c, 'block');
    expect(c.gcpIdentityUserSet).toBe(true);
    expect(gcpIdentitySelect(c).value).toBe('block');

    const body = await submit(c);
    expect(body.gcp_identity).toEqual({ metadata_mode: 'block' });
  });

  it('sends Passthrough when the user picks it with no project default on a kubernetes broker', async () => {
    const c = await mount();
    await selectTarget(c, k8sTarget);
    expect(c.gcpMetadataMode).toBe('passthrough');

    await chooseIdentity(c, 'passthrough');

    const body = await submit(c);
    expect(body.gcp_identity).toEqual({ metadata_mode: 'passthrough' });
  });

  for (const mode of ['block', 'passthrough']) {
    it(`sends the chosen mode (${mode}) once the user changes the identity on a docker broker`, async () => {
      // Start from a different default so the pick is a real change.
      projectDefaultMode = mode === 'block' ? 'passthrough' : 'block';
      const c = await mount();
      await selectTarget(c, dockerTarget);

      await chooseIdentity(c, mode);
      expect(c.gcpIdentityUserSet).toBe(true);

      const body = await submit(c);
      expect(body.gcp_identity).toEqual({ metadata_mode: mode });
    });
  }

  it('sends the chosen mode on a kubernetes broker once the user changes it', async () => {
    projectDefaultMode = 'block';
    const c = await mount();
    await selectTarget(c, k8sTarget);

    await chooseIdentity(c, 'passthrough');

    const body = await submit(c);
    expect(body.gcp_identity).toEqual({ metadata_mode: 'passthrough' });
  });
});

describe('Create Agent: config.telemetry is sent only when the user toggled it', () => {
  for (const hub of [true, false]) {
    it(`seeds the checkbox from the hub default (${hub}) and sends no telemetry key when untouched`, async () => {
      hubTelemetry = hub;
      const c = await mount();
      expect(c.telemetryEnabled).toBe(hub);

      const body = await submit(c);
      expect(body.config as Record<string, unknown>).not.toHaveProperty('telemetry');
    });
  }

  for (const value of [true, false]) {
    it(`sends {enabled: ${value}} once the user toggles telemetry to ${value}`, async () => {
      hubTelemetry = !value;
      const c = await mount();
      await toggleTelemetry(c, value);

      const body = await submit(c);
      expect((body.config as Record<string, unknown>).telemetry).toEqual({ enabled: value });
    });
  }

  it('sends an explicit value equal to the hub default after toggling off and back on', async () => {
    hubTelemetry = true;
    const c = await mount();
    await toggleTelemetry(c, false);
    await toggleTelemetry(c, true);

    const body = await submit(c);
    expect((body.config as Record<string, unknown>).telemetry).toEqual({ enabled: true });
  });

  it('keeps a user toggle when the hub default is re-seeded by a later load', async () => {
    hubTelemetry = false;
    const c = await mount();
    await toggleTelemetry(c, true);

    // Re-attaching reruns loadFormData, which fetches the hub default again.
    c.remove();
    document.body.appendChild(c);
    await settle(c);

    expect(c.telemetryEnabled).toBe(true);
    const body = await submit(c);
    expect((body.config as Record<string, unknown>).telemetry).toEqual({ enabled: true });
  });
});
