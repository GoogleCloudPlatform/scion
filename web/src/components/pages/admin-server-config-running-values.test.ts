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
 * Running values of start-up-set keys (running_values, ptone/scion#3722)
 * and the effective hub default GCP identity mode (ptone/scion#3071) on the
 * Server Config page. Bodies mirror pkg/hub.
 */
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

function makeConfig(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    schema_version: '1',
    default_template: 'gemini',
    image_registry: 'ghcr.io/test',
    server: {
      hub: { port: 9810, host: '0.0.0.0' },
      broker: { enabled: false },
      storage: { provider: 'gcs' },
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
    if (path.includes('/api/v1/admin/server-config/schema')) return jsonResponse({ sections: {} });
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

let element: PageElement | null = null;

async function createPage(config: Record<string, unknown>): Promise<PageElement> {
  vi.stubGlobal('fetch', vi.fn(fetchHandler(config)));
  const el = document.createElement('scion-page-admin-server-config') as PageElement;
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

/** The texts of every running-value line on the page. */
function runningLines(el: PageElement): string[] {
  return Array.from(el.shadowRoot!.querySelectorAll('.running-value-in-effect')).map((n) =>
    norm(n.textContent)
  );
}

/** The .form-field holding the label. */
function fieldByLabel(el: PageElement, label: string): Element | undefined {
  return Array.from(el.shadowRoot!.querySelectorAll('.form-field')).find(
    (f) => norm(f.querySelector(':scope > label')?.textContent) === label
  );
}

const GCP_SA_LABEL = 'Default GCP Identity Service Account';

describe('Server Config running values and effective GCP identity mode', () => {
  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(fetchHandler(makeConfig())));
    await import('./admin-server-config.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  it('shows no running-value line when the server reports none (control)', async () => {
    const el = await createPage(makeConfig({ settings_tier: 'db', layer0_editable: true }));
    expect(runningLines(el)).toEqual([]);
  });

  it('a workstation flag-managed field shows the running value and labels the configured one', async () => {
    const el = await createPage(
      makeConfig({
        settings_tier: 'db',
        layer0_editable: true,
        running_values: {
          'server.storage.provider': { value: 'local', source: 'startup' },
        },
      })
    );
    expect(runningLines(el)).toEqual(['In effect on this node: local (start-up default or flag)']);
    const line = el.shadowRoot!.querySelector('.running-value-in-effect')!;
    const field = line.parentElement!;
    expect(norm(field.querySelector('.env-value-configured-label')?.textContent)).toBe(
      'Configured value (settings.yaml or default), not the value in effect:'
    );
    expect(norm(field.querySelector('.read-only-value')?.textContent)).toBe('gcs');
  });

  it('a hosted deployment-managed field shows the running value too', async () => {
    const el = await createPage(
      makeConfig({
        settings_tier: 'db',
        layer0_editable: false,
        running_values: { 'server.hub.port': { value: 8080, source: 'startup' } },
      })
    );
    expect(runningLines(el)).toEqual(['In effect on this node: 8080 (start-up default or flag)']);
  });

  it('an editable field shows the running value after its control', async () => {
    const el = await createPage(
      makeConfig({
        settings_tier: 'file',
        running_values: { 'server.hub.port': { value: 8080, source: 'startup' } },
      })
    );
    expect(runningLines(el)).toEqual(['In effect on this node: 8080 (start-up default or flag)']);
    const line = el.shadowRoot!.querySelector('.running-value-in-effect')!;
    expect(line.parentElement!.querySelector('sl-input')).not.toBeNull();
  });

  it('a workstation broker host reported by the server replaces the built-in effective hint', async () => {
    const el = await createPage(
      makeConfig({
        settings_tier: 'db',
        layer0_editable: true,
        server: {
          hub: { port: 9810 },
          broker: { enabled: true, host: '' },
          storage: { provider: 'local' },
        },
        running_values: { 'server.broker.host': { value: '127.0.0.1', source: 'startup' } },
      })
    );
    expect(el.shadowRoot!.querySelector('.broker-host-effective')).toBeNull();
    expect(runningLines(el)).toEqual([
      'In effect on this node: 127.0.0.1 (start-up default or flag)',
    ]);
  });

  it('#3071: an env mode of assign in effect shows the account field although the stored mode is empty', async () => {
    const el = await createPage(
      makeConfig({
        settings_tier: 'file',
        env_overrides: ['default_gcp_identity_mode'],
        env_values: { default_gcp_identity_mode: { value: 'assign', in_effect: true } },
      })
    );
    expect(fieldByLabel(el, GCP_SA_LABEL)).toBeDefined();
  });

  it('#3071: an env mode of block in effect hides the account field although the stored mode is assign', async () => {
    const el = await createPage(
      makeConfig({
        settings_tier: 'file',
        default_gcp_identity_mode: 'assign',
        default_gcp_identity_service_account_id: 'sa-1',
        env_overrides: ['default_gcp_identity_mode'],
        env_values: { default_gcp_identity_mode: { value: 'block', in_effect: true } },
      })
    );
    expect(fieldByLabel(el, GCP_SA_LABEL)).toBeUndefined();
  });

  it('#3071: an env mode not in effect (saved value wins) leaves the stored mode in charge', async () => {
    const el = await createPage(
      makeConfig({
        settings_tier: 'db',
        env_overrides: ['default_gcp_identity_mode'],
        env_values: { default_gcp_identity_mode: { value: 'assign', in_effect: false } },
      })
    );
    expect(fieldByLabel(el, GCP_SA_LABEL)).toBeUndefined();
  });

  it('#3071: a stored mode of assign shows the account field with no env value (control)', async () => {
    const el = await createPage(
      makeConfig({ settings_tier: 'file', default_gcp_identity_mode: 'assign' })
    );
    expect(fieldByLabel(el, GCP_SA_LABEL)).toBeDefined();
  });
});
