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
 * Environment overrides on the Federation page (ptone/scion#3837). The page
 * reads env_overrides and env_values from the server-config GET it already
 * makes; bodies mirror pkg/hub.
 */
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import type { ScionPageAdminFederation } from './admin-federation.js';
import { ENV_VALUE_NOT_REPORTED } from './admin-federation.js';
import settingsSchema from '../../../../pkg/config/schemas/settings-v1.schema.json';

const FED = 'server.federation';

function makeConfig(extra: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    federation: {
      enabled: true,
      algorithms: ['RS256'],
      refresh_interval: '1h',
      debounce_interval: '5s',
      trusted_issuers: [{ issuer_url: 'https://a.example.com', issuer_type: 'hub' }],
    },
    ...extra,
  };
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

interface Handler {
  fetch: (url: string | URL | Request, init?: RequestInit) => Promise<Response>;
  puts: Record<string, unknown>[];
}

function createHandler(config: Record<string, unknown>): Handler {
  const puts: Record<string, unknown>[] = [];
  return {
    puts,
    fetch: (url, init): Promise<Response> => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.endsWith('/api/v1/admin/server-config/schema')) {
        return Promise.resolve(json({ sections: {} }));
      }
      if (path.includes('/api/v1/admin/server-config')) {
        if (init?.method === 'PUT') {
          puts.push(JSON.parse(init.body as string));
          return Promise.resolve(json({}));
        }
        return Promise.resolve(json(config));
      }
      return Promise.resolve(json({}));
    },
  };
}

type FedElement = InstanceType<typeof ScionPageAdminFederation>;

let element: FedElement | null = null;

async function createComponent(handler: Handler): Promise<FedElement> {
  vi.stubGlobal('fetch', vi.fn(handler.fetch));
  const el = document.createElement('scion-page-admin-federation') as FedElement;
  document.body.appendChild(el);
  element = el;
  await vi.waitFor(() => {
    expect((el as unknown as { loading: boolean }).loading).toBe(false);
  });
  await el.updateComplete;
  return el;
}

function norm(s: string | null | undefined): string {
  return (s ?? '').replace(/\s+/g, ' ').trim();
}

/** The .form-field whose label (or switch) names the field. */
function field(el: FedElement, label: string): Element {
  const fields = Array.from(el.shadowRoot!.querySelectorAll('.form-field'));
  const found = fields.find(
    (f) =>
      norm(f.querySelector(':scope > label')?.textContent) === label ||
      (label === 'Enabled' && norm(f.querySelector('.hint')?.textContent).startsWith('Enable or'))
  );
  if (!found) throw new Error(`no field ${label}`);
  return found;
}

async function save(el: FedElement): Promise<void> {
  await (el as unknown as { save(): Promise<void> }).save();
  await el.updateComplete;
}

describe('Federation page env overrides (ptone/scion#3837)', () => {
  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createHandler(makeConfig()).fetch));
    await import('./admin-federation.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  it('shows no env badge when the environment sets no federation key (control)', async () => {
    const el = await createComponent(createHandler(makeConfig({ settings_tier: 'db' })));
    expect(el.shadowRoot!.querySelectorAll('.env-badge').length).toBe(0);
    expect(field(el, 'Refresh Interval').querySelector('sl-input')).not.toBeNull();
  });

  it('DB tier: a superseded env value keeps the field editable and says the saved value wins', async () => {
    const el = await createComponent(
      createHandler(
        makeConfig({
          settings_tier: 'db',
          env_overrides: [`${FED}.refresh_interval`],
          env_values: { [`${FED}.refresh_interval`]: { value: '2h', in_effect: false } },
        })
      )
    );
    const f = field(el, 'Refresh Interval');
    expect(norm(f.querySelector('.env-badge')?.textContent)).toBe(
      'Environment value superseded by the saved setting'
    );
    expect(norm(f.querySelector('.env-value-not-in-effect')?.textContent)).toBe(
      'Environment value on this node, not in effect: 2h (the saved value wins)'
    );
    const input = f.querySelector('sl-input') as HTMLInputElement | null;
    expect(input).not.toBeNull();
    expect(f.querySelector('.read-only-value')).toBeNull();
    // Other fields carry no badge.
    expect(field(el, 'Debounce Interval').querySelector('.env-badge')).toBeNull();
  });

  it('DB tier: an edit to a superseded key is still saved', async () => {
    const handler = createHandler(
      makeConfig({
        settings_tier: 'db',
        env_overrides: [`${FED}.refresh_interval`],
        env_values: { [`${FED}.refresh_interval`]: { value: '2h', in_effect: false } },
      })
    );
    const el = await createComponent(handler);
    (el as unknown as { refreshInterval: string }).refreshInterval = '3h';
    await save(el);
    // A DB-tier save carries the federation revision read at load
    // (ptone/scion#3922): 0 when the GET reports no stored row.
    expect(handler.puts).toEqual([
      { federation: { refresh_interval: '3h' }, expected_revisions: { federation: 0 } },
    ]);
  });

  it('file tier: an env value in effect is shown read-only, with the differing settings.yaml value', async () => {
    const el = await createComponent(
      createHandler(
        makeConfig({
          settings_tier: 'file',
          env_overrides: [`${FED}.algorithms`],
          env_values: {
            [`${FED}.algorithms`]: { value: ['ES256'], in_effect: true, saved_differs: true },
          },
        })
      )
    );
    const f = field(el, 'Algorithms');
    expect(f.querySelector('sl-select')).toBeNull();
    expect(norm(f.querySelector('.read-only-badge')?.textContent)).toBe(
      '🔒 Set via environment variable'
    );
    expect(norm(f.querySelector('.env-badge')?.textContent)).toBe(
      'Overridden by environment on this node'
    );
    expect(norm(f.querySelector('.env-value-primary')?.textContent)).toBe('ES256');
    expect(norm(f.querySelector('.env-value-saved')?.textContent)).toBe(
      'settings.yaml value, not in effect on this node: RS256'
    );
  });

  it('file tier: no saved-value line when the stored value does not differ', async () => {
    const el = await createComponent(
      createHandler(
        makeConfig({
          settings_tier: 'file',
          env_overrides: [`${FED}.enabled`],
          env_values: { [`${FED}.enabled`]: { value: true, in_effect: true } },
        })
      )
    );
    const f = field(el, 'Enabled');
    expect(f.querySelector('sl-switch')).toBeNull();
    expect(norm(f.querySelector('.env-value-primary')?.textContent)).toBe('true');
    expect(f.querySelector('.env-value-saved')).toBeNull();
  });

  it('file tier: a locked key is never sent, even if its state changes', async () => {
    const handler = createHandler(
      makeConfig({
        settings_tier: 'file',
        env_overrides: [`${FED}.debounce_interval`],
        env_values: { [`${FED}.debounce_interval`]: { value: '9s', in_effect: true } },
      })
    );
    const el = await createComponent(handler);
    const page = el as unknown as { debounceInterval: string; refreshInterval: string };
    page.debounceInterval = '7s';
    page.refreshInterval = '2h';
    await save(el);
    expect(handler.puts).toEqual([{ federation: { refresh_interval: '2h' } }]);
  });

  it('an entry with no reported state locks on a file tier but not on a DB tier', async () => {
    const env = {
      env_overrides: [`${FED}.refresh_interval`],
      env_values: { [`${FED}.refresh_interval`]: { value: '********', masked: true } },
    };
    const file = await createComponent(
      createHandler(makeConfig({ settings_tier: 'file', ...env }))
    );
    const ff = field(file, 'Refresh Interval');
    expect(ff.querySelector('sl-input')).toBeNull();
    expect(norm(ff.querySelector('.env-value-primary')?.textContent)).toBe('********');
    file.remove();

    const db = await createComponent(createHandler(makeConfig({ settings_tier: 'db', ...env })));
    const df = field(db, 'Refresh Interval');
    expect(df.querySelector('sl-input')).not.toBeNull();
    expect(norm(df.querySelector('.env-badge')?.textContent)).toBe(
      'Overridden by environment on this node'
    );
  });

  it('file tier: a locked key with no env_values entry shows "not reported", never the stored value', async () => {
    const el = await createComponent(
      createHandler(
        makeConfig({
          settings_tier: 'file',
          env_overrides: [`${FED}.refresh_interval`],
        })
      )
    );
    const f = field(el, 'Refresh Interval');
    expect(f.querySelector('sl-input')).toBeNull();
    expect(norm(f.querySelector('.env-value-primary')?.textContent)).toBe(ENV_VALUE_NOT_REPORTED);
    // The stored value (1h) is not presented as the environment value.
    expect(norm(f.querySelector('.env-value-primary')?.textContent)).not.toContain('1h');
    expect(f.querySelector('.env-value-saved')).toBeNull();
  });

  it('Trusted Issuers shows a badge for an env key under trusted_issuers', async () => {
    const el = await createComponent(
      createHandler(
        makeConfig({
          settings_tier: 'db',
          env_overrides: [`${FED}.trusted_issuers`],
        })
      )
    );
    const sections = Array.from(el.shadowRoot!.querySelectorAll('.section'));
    const issuers = sections.find(
      (s) => norm(s.querySelector('.section-title')?.textContent) === 'Trusted Issuers'
    )!;
    expect(norm(issuers.querySelector('.env-badge')?.textContent)).toBe(
      'Overridden by environment on this node'
    );
  });

  it('the settings schema names the env var of every global federation key', () => {
    const defs = (
      settingsSchema as unknown as {
        $defs: Record<string, { properties: Record<string, Record<string, unknown>> }>;
      }
    ).$defs;
    const props = defs.serverFederation.properties;
    expect({
      enabled: props.enabled['x-env-var'],
      algorithms: props.algorithms['x-env-var'],
      refresh_interval: props.refresh_interval['x-env-var'],
      debounce_interval: props.debounce_interval['x-env-var'],
    }).toEqual({
      enabled: 'SCION_SERVER_FEDERATION_ENABLED',
      algorithms: 'SCION_SERVER_FEDERATION_ALGORITHMS',
      refresh_interval: 'SCION_SERVER_FEDERATION_REFRESHINTERVAL',
      debounce_interval: 'SCION_SERVER_FEDERATION_DEBOUNCEINTERVAL',
    });
  });
});
