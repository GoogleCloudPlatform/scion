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
 * The project default GCP identity picker and Kubernetes-bound projects.
 * ptone/scion#4077 reverses Phase 2 of ptone/scion#2328: the hub accepts a
 * Block project default on a Kubernetes-bound project, so Block is offered
 * there, and the help text says what Block means on Kubernetes.
 *
 * A project is Kubernetes-bound when every runtime broker linked to it (the
 * same brokers list already loaded for the Brokers tab) is reliably known to
 * be Kubernetes-only. A project with no linked broker, or a mix of runtime
 * types across its linked brokers, is not, and gets no Kubernetes hint.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import type { ScionPageProjectSettings } from './project-settings.js';
import { KUBERNETES_BLOCK_HINT } from '../../shared/gcp-identity-state.js';

const PROJECT_RESPONSE = {
  id: 'proj-1',
  name: 'Test Project',
  slug: 'test-project',
  _capabilities: { actions: ['update', 'manage'] },
};

interface BrokerProfileFixture {
  name: string;
  type: string;
  available: boolean;
}

interface BrokerFixture {
  id: string;
  name: string;
  slug: string;
  status: string;
  connectionState: string;
  autoProvide: boolean;
  profiles?: BrokerProfileFixture[];
}

function makeBroker(id: string, profiles?: BrokerProfileFixture[]): BrokerFixture {
  return {
    id,
    name: id,
    slug: id,
    status: 'online',
    connectionState: 'connected',
    autoProvide: false,
    profiles,
  };
}

function createFetchHandler(opts?: {
  brokers?: BrokerFixture[];
  settings?: Record<string, unknown>;
  resolvedSettings?: Record<string, unknown>;
  serviceAccounts?: Array<Record<string, unknown>>;
  /** Receives the body of every PUT to the project settings. */
  settingsPuts?: Array<Record<string, unknown>>;
}) {
  return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;

    if (init?.method === 'PUT' && path.endsWith('/settings') && typeof init.body === 'string') {
      const body = JSON.parse(init.body) as Record<string, unknown>;
      opts?.settingsPuts?.push(body);
      return Promise.resolve(
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/runtime-brokers')) {
      return Promise.resolve(
        new Response(JSON.stringify({ brokers: opts?.brokers ?? [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/gcp-service-accounts')) {
      return Promise.resolve(
        new Response(JSON.stringify({ items: opts?.serviceAccounts ?? [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.match(/\/api\/v1\/projects\/[^/]+$/) || path.match(/\/api\/v1\/projects\/[^/]+\?/)) {
      return Promise.resolve(
        new Response(JSON.stringify(PROJECT_RESPONSE), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/settings/resolved')) {
      return Promise.resolve(
        new Response(
          JSON.stringify({
            projectId: 'proj-1',
            project: opts?.settings ?? {},
            settings: opts?.resolvedSettings ?? {},
          }),
          {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }
        )
      );
    }

    if (path.includes('/settings')) {
      return Promise.resolve(
        new Response(JSON.stringify(opts?.settings ?? {}), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    // Catch-all for other API calls (harness configs, gcp service accounts,
    // messaging policy, pre-start hooks, templates, etc.)
    return Promise.resolve(
      new Response(JSON.stringify({}), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  };
}

async function createComponent(
  fetchHandler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>
) {
  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = document.createElement('scion-page-project-settings') as InstanceType<
    typeof ScionPageProjectSettings
  >;
  el.projectId = 'proj-1';
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 400));
  await el.updateComplete;
  return el;
}

/** The GCP identity default's Block <sl-option>, located by its field label. */
function blockOption(el: HTMLElement): Element | null {
  const fields = Array.from(el.shadowRoot?.querySelectorAll('.config-field') ?? []);
  const field = fields.find((f) =>
    f.querySelector('label')?.textContent?.includes('Default Service Account')
  );
  return field?.querySelector('sl-option[value="block"]') ?? null;
}

/** Whitespace-normalized: Lit template literals preserve literal newlines/indentation verbatim in textContent. */
function fieldHelpText(el: HTMLElement): string {
  const fields = Array.from(el.shadowRoot?.querySelectorAll('.config-field') ?? []);
  const field = fields.find((f) =>
    f.querySelector('label')?.textContent?.includes('Default Service Account')
  );
  return Array.from(field?.querySelectorAll('.field-help') ?? [])
    .map((n) => n.textContent ?? '')
    .join(' ')
    .replace(/\s+/g, ' ')
    .trim();
}

function gcpIdentitySelect(el: HTMLElement): Element | null {
  const fields = Array.from(el.shadowRoot?.querySelectorAll('.config-field') ?? []);
  const field = fields.find((f) =>
    f.querySelector('label')?.textContent?.includes('Default Service Account')
  );
  return field?.querySelector('sl-select') ?? null;
}

function inheritOption(el: HTMLElement): Element | null {
  const fields = Array.from(el.shadowRoot?.querySelectorAll('.config-field') ?? []);
  const field = fields.find((f) =>
    f.querySelector('label')?.textContent?.includes('Default Service Account')
  );
  return field?.querySelector('sl-option[value="inherit"]') ?? null;
}

/** The page's private save state, exposed via a loose cast (TS privacy is compile-time only). */
interface ProjectSettingsInternals {
  handleSaveConfig: () => Promise<void>;
  settingsError: string | null;
  updateComplete: Promise<unknown>;
}

describe('project-settings: GCP identity Block option and Kubernetes-bound projects', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler()));
    await import('./project-settings.js');
  }, 30_000);

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  it('offers Block with the Kubernetes Block hint when every linked broker is kubernetes-only', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'kubernetes', available: true }])],
      })
    );

    const option = blockOption(element);
    expect(option).not.toBeNull();
    expect(option!.hasAttribute('disabled')).toBe(false);
    const helpText = gcpIdentitySelect(element)!.querySelector('[slot="help-text"]');
    expect(helpText?.textContent).toBe(KUBERNETES_BLOCK_HINT);
  });

  it('saves a Block project default for a Kubernetes-bound project', async () => {
    const settingsPuts: Array<Record<string, unknown>> = [];
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'kubernetes', available: true }])],
        settingsPuts,
      })
    );
    const page = element as unknown as ProjectSettingsInternals;

    const select = gcpIdentitySelect(element) as HTMLElement & { value: string };
    select.value = 'block';
    select.dispatchEvent(new Event('sl-change', { bubbles: true, composed: true }));
    await page.updateComplete;

    await page.handleSaveConfig();

    expect(page.settingsError).toBeNull();
    expect(settingsPuts).toHaveLength(1);
    expect(settingsPuts[0].defaultGCPIdentityMode).toBe('block');
    expect(settingsPuts[0].defaultGCPIdentityServiceAccountID).toBe('');
  });

  it('leaves Block enabled when linked brokers mix runtime types', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [
          makeBroker('b1', [{ name: 'default', type: 'kubernetes', available: true }]),
          makeBroker('b2', [{ name: 'default', type: 'docker', available: true }]),
        ],
      })
    );

    const option = blockOption(element);
    expect(option).not.toBeNull();
    expect(option!.hasAttribute('disabled')).toBe(false);
  });

  it('leaves Block enabled when the project has no linked broker', async () => {
    element = await createComponent(createFetchHandler({ brokers: [] }));

    const option = blockOption(element);
    expect(option).not.toBeNull();
    expect(option!.hasAttribute('disabled')).toBe(false);
  });

  it('leaves Block enabled for a docker-only broker', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'docker', available: true }])],
      })
    );

    const option = blockOption(element);
    expect(option!.hasAttribute('disabled')).toBe(false);
  });

  it('leaves Block enabled when a single broker mixes runtime types across its own profiles', async () => {
    // Distinct from the "linked brokers mix runtime types" case above: here
    // it's ONE broker with a kubernetes profile AND a docker profile, pinning
    // that the per-profile check is `every`, not `some`.
    element = await createComponent(
      createFetchHandler({
        brokers: [
          makeBroker('b1', [
            { name: 'k8s-profile', type: 'kubernetes', available: true },
            { name: 'docker-profile', type: 'docker', available: true },
          ]),
        ],
      })
    );

    const option = blockOption(element);
    expect(option!.hasAttribute('disabled')).toBe(false);
  });

  it('leaves Block enabled for a broker with an empty profiles list', async () => {
    // Distinct from "no linked broker": this broker IS linked, but reports no
    // profiles at all — nothing to confirm its runtime type from.
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [])],
      })
    );

    const option = blockOption(element);
    expect(option!.hasAttribute('disabled')).toBe(false);
  });

  it.each(['k8s', 'remote'])(
    'offers Block with the Kubernetes Block hint for a broker using the accepted spelling "%s"',
    async (type) => {
      element = await createComponent(
        createFetchHandler({
          brokers: [makeBroker('b1', [{ name: 'default', type, available: true }])],
        })
      );

      expect(blockOption(element)!.hasAttribute('disabled')).toBe(false);
      const helpText = gcpIdentitySelect(element)!.querySelector('[slot="help-text"]');
      expect(helpText?.textContent).toBe(KUBERNETES_BLOCK_HINT);
    }
  );

  // aria-describedby on the <sl-select> HOST has no accessibility effect —
  // the element that receives focus is the role="combobox" input inside
  // Shoelace's shadow root, which a host attribute cannot reach across the
  // shadow boundary. The explanation must be slotted into the select's
  // help-text slot instead, which Shoelace wires to that combobox internally.
  it('slots the Kubernetes Block hint into the select help-text, with no tooltip on Block', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'kubernetes', available: true }])],
      })
    );

    expect(blockOption(element)!.hasAttribute('title')).toBe(false);

    const select = gcpIdentitySelect(element);
    expect(select!.hasAttribute('aria-describedby')).toBe(false);
    const helpText = select!.querySelector('[slot="help-text"]');
    expect(helpText).not.toBeNull();
    expect(helpText!.textContent).toBe(KUBERNETES_BLOCK_HINT);
    expect(helpText!.textContent!.toLowerCase()).not.toContain('metadata');
  });

  it('does not add a tooltip or a help-text slot when the project is not reliably Kubernetes-bound', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'docker', available: true }])],
      })
    );

    const option = blockOption(element);
    expect(option!.hasAttribute('title')).toBe(false);

    const select = gcpIdentitySelect(element);
    expect(select!.querySelector('[slot="help-text"]')).toBeNull();
  });

  it('changes the "inherit" fallback label to passthrough for a Kubernetes-bound project', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'kubernetes', available: true }])],
      })
    );

    expect(inheritOption(element)!.textContent).toContain('passthrough');
  });

  it('keeps the "inherit" fallback label as block for a non-Kubernetes-bound project', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'docker', available: true }])],
      })
    );

    expect(inheritOption(element)!.textContent).toContain('block');
  });

  it('shows a stored "block" value as selected and enabled for a Kubernetes-bound project', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'kubernetes', available: true }])],
        settings: { defaultGCPIdentityMode: 'block' },
      })
    );

    const select = gcpIdentitySelect(element);
    expect(select!.getAttribute('value')).toBe('block');
    const option = blockOption(element);
    expect(option!.hasAttribute('disabled')).toBe(false);
  });

  // A hub default of Block is accepted on Kubernetes: no rejection warning,
  // only the Kubernetes Block hint in the select's help text.
  it('does not flag an inherited hub default of block for a Kubernetes-bound project', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'kubernetes', available: true }])],
        resolvedSettings: {
          'scion.io/default-gcp-identity-mode': {
            projectSet: false,
            projectValue: null,
            hubDefault: 'present',
            hubValue: 'block',
          },
        },
      })
    );

    expect(fieldHelpText(element)).not.toContain('hub default is "Block"');
    expect(fieldHelpText(element)).not.toContain('rejects');
    const helpText = gcpIdentitySelect(element)!.querySelector('[slot="help-text"]');
    expect(helpText?.textContent).toBe(KUBERNETES_BLOCK_HINT);
  });

  // A hub default of "passthrough" only takes effect on the hub's own
  // embedded broker; on any other broker (which every broker here is, by
  // definition, once the project is confirmed Kubernetes-bound), Phase 1
  // (ptone/scion#2338) leaves the identity UNSET rather than writing an
  // explicit "block" — the broker then applies its own Kubernetes default
  // (passthrough). So this is informational, not a rejection warning: the
  // hint must not claim a rejection that Phase 1 does not produce.
  it('extends the inherited hub-passthrough hint with the Kubernetes default-applies note', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'kubernetes', available: true }])],
        resolvedSettings: {
          'scion.io/default-gcp-identity-mode': {
            projectSet: false,
            projectValue: null,
            hubDefault: 'present',
            hubValue: 'passthrough',
          },
        },
      })
    );

    const text = fieldHelpText(element);
    expect(text).toContain('embedded broker');
    expect(text).toContain('Kubernetes');
    expect(text).toContain('applies its own default automatically');
    expect(text).not.toContain('rejects');
  });

  it('does not add the Kubernetes note to the hub-passthrough hint for a non-Kubernetes-bound project', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'docker', available: true }])],
        resolvedSettings: {
          'scion.io/default-gcp-identity-mode': {
            projectSet: false,
            projectValue: null,
            hubDefault: 'present',
            hubValue: 'passthrough',
          },
        },
      })
    );

    expect(fieldHelpText(element)).not.toContain('applies its own default automatically');
  });

  // A hub default of "assign" with no service account configured falls
  // through to "Block" the same way an unverified one does; Block is
  // accepted on Kubernetes, so the hint names the fallback without a
  // rejection warning.
  it('extends the inherited hub-assign hint when no service account is configured, for a Kubernetes-bound project', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'kubernetes', available: true }])],
        resolvedSettings: {
          'scion.io/default-gcp-identity-mode': {
            projectSet: false,
            projectValue: null,
            hubDefault: 'present',
            hubValue: 'assign',
          },
          'scion.io/default-gcp-identity-service-account-id': {
            projectSet: false,
            projectValue: null,
            hubDefault: 'absent',
            hubValue: null,
          },
        },
      })
    );

    const text = fieldHelpText(element);
    expect(text).toContain('no service account is configured');
    expect(text).toContain('falls back to "Block"');
    expect(text).not.toContain('rejected at dispatch');
  });

  // Pins the `!saID &&` condition the other way — with a configured,
  // resolvable service account, the normal "agents are assigned" hint must
  // show instead of the no-SA-configured warning, even for a
  // Kubernetes-bound project.
  it('shows the normal hub-assign hint when a service account IS configured, for a Kubernetes-bound project', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'kubernetes', available: true }])],
        serviceAccounts: [
          {
            id: 'sa-1',
            email: 'sa-1@example.iam.gserviceaccount.com',
            verified: true,
            scope: 'project',
            scopeId: 'proj-1',
            projectId: 'gcp-proj',
            displayName: '',
            defaultScopes: [],
            verifiedAt: '2026-01-01T00:00:00Z',
            createdBy: 'user-1',
          },
        ],
        resolvedSettings: {
          'scion.io/default-gcp-identity-mode': {
            projectSet: false,
            projectValue: null,
            hubDefault: 'present',
            hubValue: 'assign',
          },
          'scion.io/default-gcp-identity-service-account-id': {
            projectSet: false,
            projectValue: null,
            hubDefault: 'present',
            hubValue: 'sa-1',
          },
        },
      })
    );

    const text = fieldHelpText(element);
    expect(text).toContain('agents are assigned');
    expect(text).toContain('sa-1@example.iam.gserviceaccount.com');
    expect(text).not.toContain('no service account is configured');
    expect(text).not.toContain('rejected at dispatch');
  });
});
