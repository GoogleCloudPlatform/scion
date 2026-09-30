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
 * Phase 2 of ptone/scion#2328: the project default GCP identity picker
 * disables (does not remove) "Block" when every runtime broker linked to
 * this project is reliably known to be Kubernetes-only — the same brokers
 * list already loaded for the Brokers tab. A project with no linked broker,
 * or a mix of runtime types across its linked brokers, is not reliably known
 * and leaves Block enabled: the write is later checked server-side.
 *
 * Disable (not remove) so a project that already has a stored "block" value
 * keeps displaying it without the select silently losing its selection.
 */

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let ScionPageProjectSettings: any;

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

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
}) {
  return (url: string | URL | Request): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;

    if (path.includes('/runtime-brokers')) {
      return Promise.resolve(
        new Response(JSON.stringify({ brokers: opts?.brokers ?? [] }), {
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
            settings: opts?.settings ?? {},
            resolvedSettings: {},
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

function fieldHelpText(el: HTMLElement): string {
  const fields = Array.from(el.shadowRoot?.querySelectorAll('.config-field') ?? []);
  const field = fields.find((f) =>
    f.querySelector('label')?.textContent?.includes('Default Service Account')
  );
  return Array.from(field?.querySelectorAll('.field-help') ?? [])
    .map((n) => n.textContent ?? '')
    .join(' ');
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

describe('project-settings: GCP identity Block option and Kubernetes-bound projects', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler()));
    const mod = await import('./project-settings.js');
    ScionPageProjectSettings = mod.ScionPageProjectSettings;
  }, 30_000);

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  it('disables Block when every linked broker is kubernetes-only', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'kubernetes', available: true }])],
      })
    );

    const option = blockOption(element);
    expect(option).not.toBeNull();
    expect(option!.hasAttribute('disabled')).toBe(true);
    expect(fieldHelpText(element)).toContain('Kubernetes');
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
    // that the per-profile check is `every`, not `some` (PR 2332 review round
    // 1, finding 5, mutation M2).
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
    // profiles at all — nothing to confirm its runtime type from (PR 2332
    // review round 1, finding 5, mutation M3).
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [])],
      })
    );

    const option = blockOption(element);
    expect(option!.hasAttribute('disabled')).toBe(false);
  });

  it.each(['k8s', 'remote'])(
    'disables Block for a kubernetes-only broker using the accepted spelling "%s"',
    async (type) => {
      element = await createComponent(
        createFetchHandler({
          brokers: [makeBroker('b1', [{ name: 'default', type, available: true }])],
        })
      );

      const option = blockOption(element);
      expect(option!.hasAttribute('disabled')).toBe(true);
    }
  );

  it('gives the disabled Block option a tooltip and links the select to the explanation', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'kubernetes', available: true }])],
      })
    );

    const option = blockOption(element);
    expect(option!.getAttribute('title')).toBeTruthy();
    expect(option!.getAttribute('title')).toContain('Kubernetes');

    const select = gcpIdentitySelect(element);
    const describedBy = select!.getAttribute('aria-describedby');
    expect(describedBy).toBeTruthy();
    const hint = element!.shadowRoot?.getElementById(describedBy!);
    expect(hint).not.toBeNull();
    expect(hint!.textContent).toContain('Kubernetes');
  });

  it('does not add a tooltip or aria-describedby when the project is not reliably Kubernetes-bound', async () => {
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'docker', available: true }])],
      })
    );

    const option = blockOption(element);
    expect(option!.hasAttribute('title')).toBe(false);

    const select = gcpIdentitySelect(element);
    expect(select!.hasAttribute('aria-describedby')).toBe(false);
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

  it('still shows a stored "block" value as selected and disabled for a Kubernetes-bound project', async () => {
    // Stored block defaults are not migrated or rewritten (ptone's ruling):
    // disable-not-remove exists precisely so this keeps displaying correctly.
    element = await createComponent(
      createFetchHandler({
        brokers: [makeBroker('b1', [{ name: 'default', type: 'kubernetes', available: true }])],
        settings: { defaultGCPIdentityMode: 'block' },
      })
    );

    const select = gcpIdentitySelect(element);
    expect(select!.getAttribute('value')).toBe('block');
    const option = blockOption(element);
    expect(option!.hasAttribute('disabled')).toBe(true);
  });
});
