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
 * Create Agent request body (ptone/scion#3902, ptone/scion#3974):
 * gcp_identity and config.telemetry are sent only when the user changed them
 * on the form. Untouched, they are omitted so the server applies its own
 * precedence instead of the form pinning the values client-side.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import { createInternals, formField, formRoot } from './__fixtures__/agent-create-internals.js';

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
  updateComplete: Promise<unknown>;
  handleSubmit(e: Event, provisionOnly?: boolean): Promise<void>;
}

let projectDefaultMode = '';
/** The account an assign project default names; 'sa-a' is the verified one. */
let projectDefaultAccount = 'sa-a';
/** The project's per-profile defaults (profile name to account ID). */
let projectProfileDefaults: Record<string, string> = {};
let hubTelemetry = false;
/** Extra project settings fields (limits, model) for the placeholder tests. */
let projectExtraSettings: Record<string, unknown> = {};
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

/** What /gcp-service-accounts returns; afterEach resets it to the same value. */
let serviceAccounts: unknown[] = [verifiedServiceAccount];

function stubFetch(): void {
  bodies = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      if (url.includes('/api/v1/agents') && init?.method === 'POST') {
        if (typeof init.body === 'string') {
          bodies.push(JSON.parse(init.body) as Record<string, unknown>);
        }
        // A 400 makes handleSubmit stop before navigating away.
        return Promise.resolve({
          ok: false,
          status: 400,
          json: () => Promise.resolve({ error: { message: 'stub: not created' } }),
        } as Response);
      }
      let body: unknown = { projects: [], brokers: [], templates: [], harnessConfigs: [] };
      if (url.includes('/settings/public')) {
        body = { telemetryEnabled: hubTelemetry };
      } else if (url.includes('/api/v1/projects?')) {
        body = { projects: [{ id: 'p1', name: 'P1' }] };
      } else if (url.includes('/api/v1/projects/p1/settings')) {
        const settings: Record<string, unknown> = { ...projectExtraSettings };
        if (projectDefaultMode) settings.defaultGCPIdentityMode = projectDefaultMode;
        if (projectDefaultMode === 'assign') {
          settings.defaultGCPIdentityServiceAccountID = projectDefaultAccount;
        }
        if (Object.keys(projectProfileDefaults).length > 0) {
          settings.defaultGCPIdentityServiceAccountIDByProfile = projectProfileDefaults;
        }
        body = settings;
      } else if (url.includes('/gcp-service-accounts')) {
        body = { items: serviceAccounts };
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve(body),
      } as Response);
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
  projectDefaultAccount = 'sa-a';
  projectProfileDefaults = {};
  serviceAccounts = [verifiedServiceAccount];
  hubTelemetry = false;
  projectExtraSettings = {};
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
  const el = document.createElement('scion-page-agent-create');
  document.body.appendChild(el);
  const c = createInternals<CreatePrivate>(el);
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
  /** Whether the form treats the selection as Kubernetes-only. */
  kubernetesOnly: boolean;
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
    kubernetesOnly: type === 'kubernetes',
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
    kubernetesOnly: profile === 'k8s',
  };
}

const noBrokerTarget: TargetFixture = {
  label: 'no broker selected',
  brokers: [],
  brokerId: '',
  profile: '',
  kubernetesOnly: false,
};
const dockerTarget = singleTypeTarget('docker');
const k8sTarget = singleTypeTarget('kubernetes');

/** A broker with two profiles of one type, so a profile can be chosen explicitly. */
function twoProfileTarget(type: string, profile: string): TargetFixture {
  return {
    label: profile
      ? `a ${type} broker with profile ${profile} chosen`
      : `a two-profile ${type} broker`,
    brokers: [
      {
        id: `broker-two-${type}`,
        name: `two-profile-${type}-broker`,
        status: 'online',
        profiles: [
          { name: 'small', type, available: true },
          { name: 'large', type, available: true },
        ],
      },
    ],
    brokerId: `broker-two-${type}`,
    profile,
    kubernetesOnly: type === 'kubernetes',
  };
}

function twoProfileDockerTarget(profile: string): TargetFixture {
  return twoProfileTarget('docker', profile);
}

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
  const field = formField(c, 'GCP Identity');
  const select = field?.querySelector('sl-select');
  expect(select).toBeTruthy();
  return select as HTMLElement & { value: string };
}

function gcpIdentityHint(c: CreatePrivate): string {
  const field = formField(c, 'GCP Identity');
  return (field?.querySelector('.hint')?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

/** The account select, found by the fixture account's option. */
function accountSelect(c: CreatePrivate): (HTMLElement & { value: string }) | null {
  const option = formRoot(c).querySelector('sl-option[value="sa-a"]');
  return (option?.closest('sl-select') as (HTMLElement & { value: string }) | null) ?? null;
}

/** Operates the GCP Identity select the way a user pick does. */
async function chooseIdentity(c: CreatePrivate, value: string): Promise<void> {
  const select = gcpIdentitySelect(c);
  select.value = value;
  select.dispatchEvent(new Event('sl-change', { bubbles: true, composed: true }));
  await c.updateComplete;
}

function telemetrySelect(c: CreatePrivate): HTMLElement & { value: string } {
  const select = formField(c, 'Telemetry')?.querySelector('sl-select');
  expect(select).toBeTruthy();
  return select as HTMLElement & { value: string };
}

/** Picks Enabled or Disabled in the telemetry select, the way a user pick does. */
async function pickTelemetry(c: CreatePrivate, enabled: boolean): Promise<void> {
  const select = telemetrySelect(c);
  select.value = String(enabled);
  select.dispatchEvent(new Event('sl-change'));
  await c.updateComplete;
}

/** The config object of a create body; the form omits it when nothing is set. */
function configOf(body: Record<string, unknown>): Record<string, unknown> {
  return (body.config as Record<string, unknown> | undefined) ?? {};
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

  // A block project default is left out of the matrix above on
  // Kubernetes-only targets, where blockDefaultNeedsExplicitChoice stops
  // submit until the user picks a mode.
  for (const t of targets.filter((x) => !x.kubernetesOnly)) {
    it(`omits gcp_identity when untouched, on ${t.label} (project default: block)`, async () => {
      projectDefaultMode = 'block';
      const c = await mount();
      await selectTarget(c, t);
      expect(c.gcpIdentityUserSet).toBe(false);

      const body = await submit(c);
      expect(body).not.toHaveProperty('gcp_identity');
    });
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

  const noModeHint =
    "No mode chosen: the server applies this project's per-profile or project default, " +
    'then the hub-wide default, then the runtime default.';
  const blockUnavailable = 'Block is not available for a Kubernetes runtime target.';
  const profilePrecedence =
    'A per-profile default for the chosen profile, if set, takes precedence.';

  for (const t of targets) {
    it(`shows the same no-mode hint with no project default, on ${t.label}`, async () => {
      const c = await mount();
      await selectTarget(c, t);
      // Kubernetes-only targets append only that Block is not offered.
      expect(gcpIdentityHint(c)).toBe(
        t.kubernetesOnly ? `${noModeHint} ${blockUnavailable}` : noModeHint
      );
    });
  }

  // A per-profile default for an explicitly selected profile outranks the
  // project default on the server, so the applied project default is not
  // the outcome: blank picker, no-mode hint.
  it('treats the project default as not applied when the chosen profile has a per-profile default', async () => {
    projectDefaultMode = 'passthrough';
    projectProfileDefaults = { large: 'sa-a' };
    const c = await mount();
    await selectTarget(c, twoProfileDockerTarget('large'));
    expect(gcpIdentitySelect(c).value).toBe('');
    expect(gcpIdentityHint(c)).toBe(noModeHint);

    expect(await submit(c)).not.toHaveProperty('gcp_identity');

    await chooseIdentity(c, 'passthrough');
    expect((await submit(c)).gcp_identity).toEqual({ metadata_mode: 'passthrough' });
  });

  it('shows the applied project default when the chosen profile has no per-profile default', async () => {
    projectDefaultMode = 'passthrough';
    projectProfileDefaults = { large: 'sa-a' };
    const c = await mount();
    await selectTarget(c, twoProfileDockerTarget('small'));
    expect(gcpIdentitySelect(c).value).toBe('passthrough');
    expect(gcpIdentityHint(c)).not.toContain(profilePrecedence);
  });

  it('notes per-profile precedence on an applied project default while no profile is chosen', async () => {
    projectDefaultMode = 'passthrough';
    projectProfileDefaults = { large: 'sa-a' };
    const c = await mount();
    await selectTarget(c, twoProfileDockerTarget(''));
    expect(gcpIdentitySelect(c).value).toBe('passthrough');
    expect(gcpIdentityHint(c).endsWith(profilePrecedence)).toBe(true);
  });

  it('does not note per-profile precedence when the project has no per-profile defaults', async () => {
    projectDefaultMode = 'passthrough';
    const c = await mount();
    await selectTarget(c, twoProfileDockerTarget(''));
    expect(gcpIdentitySelect(c).value).toBe('passthrough');
    expect(gcpIdentityHint(c)).not.toContain(profilePrecedence);
  });

  // Kubernetes-only broker, no profile chosen, applied passthrough default:
  // the hint is the docker hint for the same state (mode text plus the
  // precedence note) followed only by the Block note.
  it('notes per-profile precedence then only the Block note on a kubernetes broker with no profile chosen', async () => {
    projectDefaultMode = 'passthrough';
    projectProfileDefaults = { large: 'sa-a' };
    const c = await mount();
    await selectTarget(c, twoProfileDockerTarget(''));
    const dockerHint = gcpIdentityHint(c);
    expect(dockerHint.endsWith(` ${profilePrecedence}`)).toBe(true);

    await selectTarget(c, twoProfileTarget('kubernetes', ''));
    expect(gcpIdentitySelect(c).value).toBe('passthrough');
    expect(gcpIdentityHint(c)).toBe(`${dockerHint} ${blockUnavailable}`);
  });

  // Kubernetes-only broker, no profile chosen, block default: submit is held
  // for an explicit choice, so the precedence note is not shown.
  it('omits the precedence note while a block default holds submit on a kubernetes broker with no profile chosen', async () => {
    projectDefaultMode = 'block';
    projectProfileDefaults = { large: 'sa-a' };
    serviceAccounts = [];
    const c = await mount();
    await selectTarget(c, twoProfileTarget('kubernetes', ''));
    expect(gcpIdentitySelect(c).value).toBe('');
    expect(gcpIdentityHint(c)).toBe(
      "No GCP identity is selected yet. This project's default GCP identity is Block, which " +
        'the Kubernetes runtime rejects at dispatch; creating this agent is blocked until you ' +
        'explicitly choose Passthrough.'
    );
  });

  // An outranked assign default leaves the internal mode on assign; the
  // account picker stays hidden until the user picks a mode.
  it('hides the account picker while the picker is blank, and shows it after picking assign', async () => {
    projectDefaultMode = 'assign';
    projectProfileDefaults = { large: 'sa-a' };
    const c = await mount();
    await selectTarget(c, twoProfileDockerTarget('large'));
    expect(c.gcpMetadataMode).toBe('assign');
    expect(gcpIdentitySelect(c).value).toBe('');
    expect(accountSelect(c)).toBeNull();

    await chooseIdentity(c, 'assign');
    expect(accountSelect(c)?.value).toBe('sa-a');
    expect((await submit(c)).gcp_identity).toEqual({
      metadata_mode: 'assign',
      service_account_id: 'sa-a',
    });
  });

  it('does not note per-profile precedence once the user picks a mode', async () => {
    projectDefaultMode = 'passthrough';
    projectProfileDefaults = { large: 'sa-a' };
    const c = await mount();
    await selectTarget(c, twoProfileDockerTarget(''));
    await chooseIdentity(c, 'block');
    expect(gcpIdentityHint(c)).not.toContain(profilePrecedence);
  });

  // The Kubernetes Block-default guard follows the same rule: with a
  // per-profile default outranking the block project default, submit is
  // allowed and omits gcp_identity.
  it('does not require a pick for a block project default outranked by the chosen profile on a kubernetes target', async () => {
    projectDefaultMode = 'block';
    projectProfileDefaults = { k8s: 'sa-a' };
    const c = await mount();
    await selectTarget(c, mixedTarget('k8s'));
    expect(gcpIdentitySelect(c).value).toBe('');
    expect(gcpIdentityHint(c)).toBe(`${noModeHint} ${blockUnavailable}`);

    expect(await submit(c)).not.toHaveProperty('gcp_identity');
  });

  // Asserting the blank picker first is what pins the fix: under happy-dom
  // Shoelace is not registered, so sl-change fires even for an unchanged
  // value, and the pick alone would pass with Block already shown.
  it('sends Block when the user picks it with no project default on a docker broker', async () => {
    const c = await mount();
    await selectTarget(c, dockerTarget);
    expect(c.gcpMetadataMode).toBe('block');
    expect(gcpIdentitySelect(c).value).toBe('');

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
    expect(gcpIdentitySelect(c).value).toBe('');

    await chooseIdentity(c, 'passthrough');

    const body = await submit(c);
    expect(body.gcp_identity).toEqual({ metadata_mode: 'passthrough' });
  });

  // An assign default naming an account the form did not load is not
  // applied: the page's own Block placeholder is not the outcome, so the
  // picker renders blank with the no-mode hint, and picking Block sends it.
  it('renders the picker blank for an assign default the form cannot apply, and sends a picked Block, on a docker broker', async () => {
    projectDefaultMode = 'assign';
    projectDefaultAccount = 'sa-missing';
    const c = await mount();
    await selectTarget(c, dockerTarget);
    expect(c.gcpMetadataMode).toBe('block');
    expect(c.gcpIdentityUserSet).toBe(false);
    expect(gcpIdentitySelect(c).value).toBe('');
    expect(gcpIdentityHint(c)).toBe(noModeHint);

    await chooseIdentity(c, 'block');
    expect(gcpIdentitySelect(c).value).toBe('block');

    const body = await submit(c);
    expect(body.gcp_identity).toEqual({ metadata_mode: 'block' });
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

describe('Create Agent: config.telemetry is sent only when the user picked it', () => {
  for (const hub of [true, false]) {
    it(`starts blank, does not show the hub-wide setting (${hub}) as inherited, and sends no telemetry key when untouched`, async () => {
      hubTelemetry = hub;
      const c = await mount();
      const select = telemetrySelect(c);
      expect(select.value).toBe('');
      // The hub-wide public telemetry setting is not applied at create.
      expect(select.getAttribute('placeholder')).toBe('Inherited');

      const body = await submit(c);
      expect(configOf(body)).not.toHaveProperty('telemetry');
    });
  }

  for (const value of [true, false]) {
    it(`sends {enabled: ${value}} once the user picks ${value ? 'Enabled' : 'Disabled'}`, async () => {
      hubTelemetry = !value;
      const c = await mount();
      await pickTelemetry(c, value);

      const body = await submit(c);
      expect(configOf(body).telemetry).toEqual({ enabled: value });
    });
  }

  it('keeps a user pick when the page reloads its data', async () => {
    const c = await mount();
    await pickTelemetry(c, true);

    // Re-attaching reruns loadFormData.
    const el = document.querySelector('scion-page-agent-create')!;
    el.remove();
    document.body.appendChild(el);
    await settle(c);

    const body = await submit(c);
    expect(configOf(body).telemetry).toEqual({ enabled: true });
  });
});

describe('Create Agent: an untouched form posts no config keys', () => {
  it('omits config and every Additional Options key, and shows project defaults as placeholders', async () => {
    projectExtraSettings = { defaultMaxTurns: 40, defaultModel: 'project-model' };
    const c = await mount();
    const body = await submit(c);
    expect(body).not.toHaveProperty('config');
    for (const key of ['branch', 'agentRole', 'messageMode', 'labels', 'gcp_identity']) {
      expect(body).not.toHaveProperty(key);
    }
    const maxTurns = formField(c, 'Max turns')?.querySelector('sl-input');
    expect(maxTurns?.getAttribute('placeholder')).toBe('40 (inherited from project settings)');
    const model = formField(c, 'Model')?.querySelector('sl-input');
    expect(model?.getAttribute('placeholder')).toBe(
      'project-model (inherited from project settings)'
    );
  });

  it('sends what the user set in the form', async () => {
    const c = await mount();
    const input = formField(c, 'Max turns')?.querySelector('sl-input') as
      | (HTMLElement & { value: string })
      | null;
    input!.value = '12';
    input!.dispatchEvent(new Event('sl-input'));
    await chooseIdentity(c, 'passthrough');
    const body = await submit(c);
    expect(body.config).toEqual({ max_turns: 12 });
    expect(body.gcp_identity).toEqual({ metadata_mode: 'passthrough' });
  });
});
