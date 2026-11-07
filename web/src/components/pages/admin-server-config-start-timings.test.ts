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
 * The agent start timing fields on the Server Config page
 * (server.hub.start_*, ptone/scion#3925): shown with their values or
 * defaults, saved with the lifecycle section only when edited (the
 * dirty-section save, ptone/scion#3922), and sent on a settings.yaml hub.
 */
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { registrySchemaResponse } from './__fixtures__/settings-registry.js';
import { START_TIMINGS } from './admin-server-config.js';

function makeConfig(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    schema_version: '1',
    default_template: 'gemini',
    image_registry: 'ghcr.io/test',
    server: {
      hub: { port: 9810, start_max_duration: '15m', stalled_threshold: '5m' },
      broker: { enabled: false },
    },
    ...overrides,
  };
}

function jsonResponse(body: unknown): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

function fetchHandler(config: Record<string, unknown>) {
  return (url: string | URL | Request): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
    if (path.includes('/api/v1/admin/server-config/schema')) {
      return jsonResponse(registrySchemaResponse());
    }
    if (path.includes('/api/v1/admin/server-config')) return jsonResponse(config);
    if (path.includes('/api/v1/admin/messaging')) {
      return jsonResponse({ cross_project_messaging_enabled: false, revision: 1 });
    }
    if (path.includes('/api/v1/admin/maintenance/check-updates')) {
      return jsonResponse({ tier: 'source', update_available: false });
    }
    if (path.includes('/api/v1/github-app/installations')) return jsonResponse([]);
    if (path.includes('/api/v1/github-app')) return jsonResponse({ configured: false });
    return jsonResponse([]);
  };
}

type PageElement = HTMLElement & { updateComplete: Promise<boolean> };
type Internals = {
  loading: boolean;
  hubStartTimings: Record<string, string>;
  buildSavePayload: () => Record<string, unknown>;
};

let element: PageElement | null = null;

async function createPage(config: Record<string, unknown>): Promise<PageElement & Internals> {
  vi.stubGlobal('fetch', vi.fn(fetchHandler(config)));
  const el = document.createElement('scion-page-admin-server-config') as PageElement & Internals;
  document.body.appendChild(el);
  element = el;
  await vi.waitFor(() => {
    expect(el.loading).toBe(false);
  });
  await el.updateComplete;
  return el;
}

function norm(s: string | null | undefined): string {
  return (s ?? '').replace(/\s+/g, ' ').trim();
}

function timingField(el: PageElement, key: string): Element {
  const f = el.shadowRoot!.querySelector(`[data-start-timing="${key}"]`);
  if (!f) throw new Error(`no start timing field ${key}`);
  return f;
}

async function edit(el: PageElement, key: string, value: string): Promise<void> {
  const input = timingField(el, key).querySelector('sl-input') as HTMLInputElement;
  input.value = value;
  input.dispatchEvent(new Event('sl-input'));
  await el.updateComplete;
}

describe('Server Config agent start timing', () => {
  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(fetchHandler(makeConfig())));
    await import('./admin-server-config.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  it('shows each field with its value, or the default as placeholder', async () => {
    const el = await createPage(makeConfig({ settings_tier: 'db', layer0_editable: false }));
    expect(START_TIMINGS.map((t) => t.key)).toEqual([
      'start_claim_lease_ttl',
      'start_max_duration',
      'start_unconfirmed_hold',
      'start_create_unconfirmed_hold',
    ]);
    const max = timingField(el, 'start_max_duration').querySelector('sl-input')!;
    expect(max.getAttribute('value')).toBe('15m');
    const lease = timingField(el, 'start_claim_lease_ttl').querySelector('sl-input')!;
    expect(lease.getAttribute('value')).toBe('');
    expect(lease.getAttribute('placeholder')).toBe('90s');
    expect(norm(timingField(el, 'start_claim_lease_ttl').querySelector('label')?.textContent)).toBe(
      'Start Claim Lease'
    );
  });

  it('an untouched page sends no lifecycle section (dirty-section save)', async () => {
    const el = await createPage(makeConfig({ settings_tier: 'db', layer0_editable: false }));
    const payload = el.buildSavePayload();
    expect(payload.server).toBeUndefined();
    expect(payload.expected_revisions).toBeUndefined();
  });

  it('an edit sends the lifecycle section whole, with its revision', async () => {
    const el = await createPage(
      makeConfig({
        settings_tier: 'db',
        layer0_editable: false,
        section_metadata: { lifecycle: { source: 'db', revision: 4, origin: 'managed' } },
      })
    );
    await edit(el, 'start_claim_lease_ttl', '2m');
    const payload = el.buildSavePayload();
    const hub = (payload.server as { hub: Record<string, unknown> }).hub;
    expect(hub.start_claim_lease_ttl).toBe('2m');
    // The whole section: the unchanged keys ride along, "" for unset ones.
    expect(hub.start_max_duration).toBe('15m');
    expect(hub.start_unconfirmed_hold).toBe('');
    expect(hub.stalled_threshold).toBe('5m');
    expect(payload.expected_revisions).toEqual({ lifecycle: 4 });
    // No other section is sent.
    expect(Object.keys(payload).sort()).toEqual(['expected_revisions', 'server']);
  });

  it('a settings.yaml hub sends only the start timing keys that changed', async () => {
    const el = await createPage(makeConfig({ settings_tier: 'file' }));
    await edit(el, 'start_unconfirmed_hold', '18m');
    const hub = (el.buildSavePayload().server as { hub: Record<string, unknown> }).hub;
    expect(hub.start_unconfirmed_hold).toBe('18m');
    // Unchanged start keys are not sent, so a stored value the hub would
    // refuse cannot block this save (the hub range-checks any sent key).
    expect('start_max_duration' in hub).toBe(false);
    expect('start_claim_lease_ttl' in hub).toBe(false);
  });

  it('an unrelated settings.yaml save sends no start key, even with a stored out-of-range value', async () => {
    const el = await createPage(
      makeConfig({
        settings_tier: 'file',
        server: {
          hub: { port: 9810, start_claim_lease_ttl: '1s', stalled_threshold: '5m' },
          broker: { enabled: false },
        },
      })
    );
    (el as unknown as { hubStalledThreshold: string }).hubStalledThreshold = '10m';
    const hub = (el.buildSavePayload().server as { hub: Record<string, unknown> }).hub;
    expect(hub.stalled_threshold).toBe('10m');
    for (const t of START_TIMINGS) expect(t.key in hub).toBe(false);
  });

  it('a settings.yaml save sends a cleared start key as ""', async () => {
    const el = await createPage(makeConfig({ settings_tier: 'file' }));
    await edit(el, 'start_max_duration', '');
    const hub = (el.buildSavePayload().server as { hub: Record<string, unknown> }).hub;
    expect(hub.start_max_duration).toBe('');
  });

  it('an env-set start key shows the env badge', async () => {
    const el = await createPage(
      makeConfig({
        settings_tier: 'db',
        layer0_editable: false,
        env_overrides: ['server.hub.start_max_duration'],
        env_values: { 'server.hub.start_max_duration': { value: '20m', in_effect: false } },
      })
    );
    expect(
      norm(timingField(el, 'start_max_duration').querySelector('.env-badge')?.textContent)
    ).toBe('Environment value superseded by the saved setting');
    // Control: the other fields carry no badge.
    expect(timingField(el, 'start_claim_lease_ttl').querySelector('.env-badge')).toBeNull();
  });
});
