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

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { GITHUB_ADMIN_SECTIONS } from './admin-integrations-github-sections.js';
import { owningSection } from './__fixtures__/settings-registry.js';
import type { ScionAdminGitHubAppConfig } from '../admin/admin-github-app-config.js';
import type { ScionAdminGitHubInstallations } from '../admin/admin-github-installations.js';
import type { ScionAdminGitHubAssociations } from '../admin/admin-github-associations.js';

function jsonResponse(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

function query(el: HTMLElement, selector: string): Element | null {
  return el.shadowRoot?.querySelector(selector) ?? null;
}

function queryAll(el: HTMLElement, selector: string): Element[] {
  return Array.from(el.shadowRoot?.querySelectorAll(selector) ?? []);
}

function shadowText(el: HTMLElement): string {
  return el.shadowRoot?.textContent ?? '';
}

function fieldError(el: HTMLElement, koanfKey: string): string[] {
  return queryAll(el, `[data-field-error="${koanfKey}"]`).map((n) => n.textContent?.trim() ?? '');
}

const LOADED_APP = {
  configured: true,
  app_id: 123,
  api_base_url: 'https://ghe.example.com/api/v3',
  webhooks_enabled: true,
  installation_url: 'https://github.com/apps/x/installations/new',
  has_private_key: true,
  private_key_source: 'secret',
  private_key_overrides_secret: false,
  has_webhook_secret: true,
};

describe('GITHUB_ADMIN_SECTIONS registry', () => {
  it('registers app, installations, and associations in order', () => {
    expect(GITHUB_ADMIN_SECTIONS.map((s) => s.id)).toEqual([
      'app',
      'installations',
      'associations',
    ]);
    expect(GITHUB_ADMIN_SECTIONS.map((s) => s.tag)).toEqual([
      'scion-admin-github-app-config',
      'scion-admin-github-installations',
      'scion-admin-github-associations',
    ]);
  });

  it('mounts every registered section in order inside scion-admin-integrations-github', async () => {
    let installationGets = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request, init?: RequestInit) => {
        const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
        if (path.includes('/api/v1/github-app/installations/association-report')) {
          return jsonResponse({ associations: [], total: 0, unauthorized: 0 });
        }
        if (path.includes('/api/v1/github-app/installations')) {
          installationGets++;
          return jsonResponse({ installations: [] });
        }
        if (path.includes('/api/v1/github-app')) {
          return jsonResponse(LOADED_APP);
        }
        if (path.includes('/api/v1/admin/server-config') && init?.method !== 'PUT') {
          return jsonResponse({ settings_tier: 'db' });
        }
        return jsonResponse({});
      })
    );
    await import('./admin-integrations-github.js');
    const el = document.createElement('scion-admin-integrations-github');
    document.body.appendChild(el);

    await vi.waitFor(() => {
      expect(queryAll(el, '[data-github-section]')).toHaveLength(GITHUB_ADMIN_SECTIONS.length);
    });

    const mounted = queryAll(el, '[data-github-section]');
    expect(mounted.map((m) => m.getAttribute('data-github-section'))).toEqual([
      'app',
      'installations',
      'associations',
    ]);

    // Initial mount loads installations once (no duplicate fetch when appConfigured flips true).
    const appCard = mounted[0] as ScionAdminGitHubAppConfig;
    await vi.waitFor(() => {
      expect(
        query(appCard, 'sl-button[data-action="save-github-app"]')?.hasAttribute('disabled')
      ).toBe(false);
    });
    expect(installationGets).toBe(1);

    // Saving the App card while configured triggers an installations reload.
    (query(appCard, 'sl-button[data-action="save-github-app"]') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(installationGets).toBe(2);
    });

    el.remove();
    vi.restoreAllMocks();
  });
});

describe('scion-admin-github-app-config', () => {
  let element: ScionAdminGitHubAppConfig | null = null;

  beforeAll(async () => {
    await import('../admin/admin-github-app-config.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  async function mount(
    handler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>,
    opts?: { expectLoaded?: boolean }
  ): Promise<ScionAdminGitHubAppConfig> {
    vi.stubGlobal('fetch', vi.fn(handler));
    const el = document.createElement('scion-admin-github-app-config') as ScionAdminGitHubAppConfig;
    document.body.appendChild(el);
    await el.updateComplete;
    if (opts?.expectLoaded === false) {
      await vi.waitFor(() => {
        expect(query(el, '[data-github-app-not-loaded]')).not.toBeNull();
      });
    } else {
      await vi.waitFor(() => {
        expect(
          query(el, 'sl-button[data-action="save-github-app"]')?.hasAttribute('disabled')
        ).toBe(false);
      });
    }
    await el.updateComplete;
    return el;
  }

  function saveButton(el: HTMLElement): HTMLElement {
    const btn = query(el, 'sl-button[data-action="save-github-app"]');
    expect(btn).not.toBeNull();
    return btn as HTMLElement;
  }

  it('saves only through PUT /api/v1/github-app and never PUTs server-config', async () => {
    const calls: Array<{ path: string; method: string; body?: Record<string, unknown> }> = [];
    element = await mount((url, init) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      const method = init?.method ?? 'GET';
      calls.push({
        path,
        method,
        body: init?.body ? (JSON.parse(init.body as string) as Record<string, unknown>) : undefined,
      });
      if (path.includes('/api/v1/admin/server-config')) {
        return jsonResponse({ settings_tier: 'db' });
      }
      return jsonResponse(LOADED_APP);
    });

    const el = element as unknown as Record<string, unknown>;
    el.githubAppId = 456;
    el.githubAppApiBaseUrl = 'https://ghe2.example.com/api/v3';
    el.githubAppInstallationUrl = 'https://github.com/apps/y/installations/new';
    el.githubAppPrivateKey = '  PEM  ';
    await element.updateComplete;

    saveButton(element).click();
    await vi.waitFor(() => {
      expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(1);
    });

    const puts = calls.filter((c) => c.method === 'PUT');
    expect(puts[0]!.path).toBe('/api/v1/github-app');
    expect(puts[0]!.body).toEqual({
      app_id: 456,
      api_base_url: 'https://ghe2.example.com/api/v3',
      webhooks_enabled: true,
      installation_url: 'https://github.com/apps/y/installations/new',
      private_key: 'PEM',
    });
    expect(calls.some((c) => c.method === 'PUT' && c.path.includes('/admin/server-config'))).toBe(
      false
    );
  });

  it('sends explicit clears (0, "", false) when fields are cleared', async () => {
    const puts: Array<Record<string, unknown>> = [];
    element = await mount((url, init) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.includes('/api/v1/admin/server-config')) {
        return jsonResponse({ settings_tier: 'db' });
      }
      if (init?.method === 'PUT') {
        puts.push(JSON.parse(init.body as string) as Record<string, unknown>);
      }
      return jsonResponse(LOADED_APP);
    });

    const el = element as unknown as Record<string, unknown>;
    el.githubAppId = 0;
    el.githubAppApiBaseUrl = '';
    el.githubAppWebhooksEnabled = false;
    el.githubAppInstallationUrl = '';
    await element.updateComplete;

    saveButton(element).click();
    await vi.waitFor(() => {
      expect(puts).toHaveLength(1);
    });

    expect(puts[0]).toEqual({
      app_id: 0,
      api_base_url: '',
      webhooks_enabled: false,
      installation_url: '',
    });
  });

  it('disables Save and sends no PUT when GET /api/v1/github-app fails', async () => {
    const puts: unknown[] = [];
    element = await mount(
      (url, init) => {
        const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
        if (path.includes('/api/v1/admin/server-config')) {
          return jsonResponse({ settings_tier: 'db' });
        }
        if (init?.method === 'PUT') {
          puts.push(init.body);
          return jsonResponse({});
        }
        return jsonResponse({ error: 'internal', message: 'boom' }, 500);
      },
      { expectLoaded: false }
    );

    expect(saveButton(element).hasAttribute('disabled')).toBe(true);
    expect(query(element, '[data-github-app-not-loaded]')).not.toBeNull();

    const el = element as unknown as {
      githubAppPrivateKey: string;
      handleSaveGitHubApp: () => Promise<void>;
    };
    el.githubAppPrivateKey = 'PEM';
    await el.handleSaveGitHubApp();
    expect(puts).toEqual([]);
  });

  it('a reload that fails after a loaded card disables Save and shows the hint', async () => {
    const puts: unknown[] = [];
    element = await mount((url, init) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.includes('/api/v1/admin/server-config')) {
        return jsonResponse({ settings_tier: 'db' });
      }
      if (init?.method === 'PUT') {
        puts.push(init.body);
      }
      return jsonResponse(LOADED_APP);
    });

    vi.mocked(fetch).mockImplementation(() => Promise.reject(new TypeError('Failed to fetch')));
    await element.loadGitHubAppConfig();
    await element.updateComplete;

    expect(query(element, '[data-github-app-not-loaded]')).not.toBeNull();
    expect(saveButton(element).hasAttribute('disabled')).toBe(true);

    saveButton(element).click();
    await element.updateComplete;
    expect(puts).toEqual([]);
  });

  it('the reload after a card save does not show the "did not load" hint while in flight', async () => {
    let release!: () => void;
    const hold = new Promise<void>((resolve) => (release = resolve));
    let putCount = 0;

    element = await mount((url, init) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.includes('/api/v1/admin/server-config')) {
        return jsonResponse({ settings_tier: 'db' });
      }
      if (init?.method === 'PUT') {
        putCount++;
        return jsonResponse(LOADED_APP);
      }
      if (putCount > 0) {
        return hold.then(() => jsonResponse(LOADED_APP));
      }
      return jsonResponse(LOADED_APP);
    });

    const el = element as unknown as {
      githubAppApiBaseUrl: string;
      handleSaveGitHubApp: () => Promise<void>;
    };
    el.githubAppApiBaseUrl = '';
    const saving = el.handleSaveGitHubApp();

    await vi.waitFor(() => {
      expect(putCount).toBe(1);
    });
    await element.updateComplete;
    expect(query(element, '[data-github-app-not-loaded]')).toBeNull();

    release();
    await saving;
    await element.updateComplete;
    expect(query(element, '[data-github-app-not-loaded]')).toBeNull();
    expect(saveButton(element).hasAttribute('disabled')).toBe(false);
  });

  it('shows validation errors inline at the field and in the card summary', async () => {
    element = await mount((url, init) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.includes('/api/v1/admin/server-config')) {
        return jsonResponse({ settings_tier: 'db' });
      }
      if (init?.method === 'PUT') {
        return jsonResponse(
          {
            error: {
              code: 'validation_error',
              message: 'must be a PEM-encoded key',
              details: { field: 'private_key' },
            },
          },
          422
        );
      }
      return jsonResponse(LOADED_APP);
    });

    saveButton(element).click();
    await vi.waitFor(() => {
      expect(fieldError(element!, 'server.github_app.private_key')).toEqual([
        'must be a PEM-encoded key',
      ]);
    });
    const summary = query(element, '[data-section-errors="github_app_card"]');
    expect(summary?.textContent).toContain('must be a PEM-encoded key');
  });

  it('layer0_rejected names the key and shows it at the field', async () => {
    element = await mount((url, init) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.includes('/api/v1/admin/server-config')) {
        return jsonResponse({ settings_tier: 'db' });
      }
      if (init?.method === 'PUT') {
        return jsonResponse({ error: 'layer0_rejected', keys: ['server.github_app.app_id'] }, 422);
      }
      return jsonResponse(LOADED_APP);
    });

    saveButton(element).click();
    await vi.waitFor(() => {
      expect(query(element!, '[data-settings-layer0]')).not.toBeNull();
    });

    const notice = query(element, '[data-settings-layer0]');
    expect(notice!.textContent).toContain('cannot be changed here');
    expect(notice!.textContent).toContain('server.github_app.app_id');
    expect(fieldError(element, 'server.github_app.app_id')[0]).toContain(
      'managed by deployment configuration'
    );
  });

  it('an unknown error keeps the generic message plus the server text', async () => {
    element = await mount((url, init) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.includes('/api/v1/admin/server-config')) {
        return jsonResponse({ settings_tier: 'db' });
      }
      if (init?.method === 'PUT') {
        return jsonResponse(
          { error: { code: 'internal_error', message: 'failed to store private key' } },
          500
        );
      }
      return jsonResponse(LOADED_APP);
    });

    saveButton(element).click();
    await vi.waitFor(() => {
      expect(shadowText(element!)).toContain(
        'Failed to save GitHub App configuration: failed to store private key'
      );
    });
  });

  it('handles revision_conflict with Reload clearing typed secrets', async () => {
    let gets = 0;
    element = await mount((url, init) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.includes('/api/v1/admin/server-config')) {
        return jsonResponse({ settings_tier: 'db' });
      }
      if (init?.method === 'PUT') {
        return jsonResponse(
          { error: 'revision_conflict', message: 'changed', conflicted: [] },
          409
        );
      }
      gets++;
      return jsonResponse(LOADED_APP);
    });

    const el = element as unknown as Record<string, unknown>;
    el.githubAppPrivateKey = '-----BEGIN RSA PRIVATE KEY-----';
    el.githubAppWebhookSecret = 'whsec_typed';
    await element.updateComplete;

    saveButton(element).click();
    await vi.waitFor(() => {
      expect(query(element!, '[data-settings-conflict]')).not.toBeNull();
    });

    const notice = query(element, '[data-settings-conflict]');
    expect(notice?.textContent).toContain('Settings changed since you loaded them');

    const before = gets;
    (notice!.querySelector('[data-settings-reload]') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(gets).toBe(before + 1);
    });
    await element.updateComplete;

    expect(el.githubAppPrivateKey).toBe('');
    expect(el.githubAppWebhookSecret).toBe('');
  });

  it('describes webhooks_enabled as adjusting health-check interval rather than gating reception', async () => {
    element = await mount((url) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.includes('/api/v1/admin/server-config')) {
        return jsonResponse({ settings_tier: 'db' });
      }
      return jsonResponse({
        ...LOADED_APP,
        webhooks_enabled: false,
        private_key_source: 'secret',
        has_webhook_secret: false,
      });
    });

    const text = shadowText(element);
    expect(text).toContain('6 hours');
    expect(text).toContain('24 hours');
    expect(text).toContain('webhook secret');
    expect(text).not.toContain('Enable to receive installation lifecycle events from GitHub');
  });

  it('shows active key source and override warning when file or config key overrides secret', async () => {
    element = await mount((url) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.includes('/api/v1/admin/server-config')) {
        return jsonResponse({ settings_tier: 'db' });
      }
      return jsonResponse({
        ...LOADED_APP,
        private_key_source: 'file',
        private_key_overrides_secret: true,
      });
    });

    expect(query(element, '[data-github-app-key-source]')?.textContent).toContain(
      'private_key_path'
    );
    const warn = query(element, '[data-github-app-key-override-warning]');
    expect(warn?.textContent).toContain('overriding the stored hub secret');
  });

  it('shows stored secret source without override warning when secret is active', async () => {
    element = await mount((url) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.includes('/api/v1/admin/server-config')) {
        return jsonResponse({ settings_tier: 'db' });
      }
      return jsonResponse({
        ...LOADED_APP,
        private_key_source: 'secret',
        private_key_overrides_secret: false,
      });
    });

    const sourceEl = query(element, '[data-github-app-key-source]');
    expect(sourceEl?.textContent).toContain('GITHUB_APP_PRIVATE_KEY');
    expect(query(element, '[data-github-app-key-override-warning]')).toBeNull();
  });

  it('R1: hosted DB hub (settings_tier=db, layer0_editable absent/false) keeps Layer-1 App ID editable with env badge and sends app_id on save', async () => {
    const puts: Array<Record<string, unknown>> = [];
    element = await mount((url, init) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.includes('/api/v1/admin/server-config')) {
        return jsonResponse({
          settings_tier: 'db',
          env_overrides: ['server.github_app.app_id'],
          env_values: {
            'server.github_app.app_id': { value: '999999', in_effect: false, saved_differs: true },
          },
          section_metadata: { github_app: { source: 'db', origin: 'managed', revision: 2 } },
        });
      }
      if (init?.method === 'PUT') {
        puts.push(JSON.parse(init.body as string) as Record<string, unknown>);
      }
      return jsonResponse(LOADED_APP);
    });

    await vi.waitFor(() => {
      expect(query(element!, '.env-badge')).not.toBeNull();
    });
    expect(query(element, '.read-only-value')).toBeNull();
    expect(query(element, 'sl-input[inputmode="numeric"]')).not.toBeNull();

    saveButton(element).click();
    await vi.waitFor(() => {
      expect(puts).toHaveLength(1);
    });
    expect(puts[0]!.app_id).toBe(123);
  });

  for (const tierCase of [
    { name: 'workstation DB (layer0_editable: true)', settings_tier: 'db', layer0_editable: true },
    { name: 'file tier (settings_tier: file)', settings_tier: 'file', layer0_editable: false },
  ] as const) {
    it(`R1/R3(a): ${tierCase.name} with env_overrides renders App ID read-only with env badge and omits app_id on save`, async () => {
      const puts: Array<Record<string, unknown>> = [];
      element = await mount((url, init) => {
        const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
        if (path.includes('/api/v1/admin/server-config')) {
          return jsonResponse({
            settings_tier: tierCase.settings_tier,
            layer0_editable: tierCase.layer0_editable,
            env_overrides: ['server.github_app.app_id'],
            env_values: {
              'server.github_app.app_id': {
                value: '999999',
                in_effect: true,
                saved_differs: false,
              },
            },
            section_metadata: {
              github_app: { source: tierCase.settings_tier, origin: 'managed', revision: 2 },
            },
          });
        }
        if (init?.method === 'PUT') {
          puts.push(JSON.parse(init.body as string) as Record<string, unknown>);
        }
        return jsonResponse(LOADED_APP);
      });

      await vi.waitFor(() => {
        expect(query(element!, '.read-only-value.env-value-primary')).not.toBeNull();
      });
      expect(query(element, '.read-only-value.env-value-primary')?.textContent?.trim()).toBe(
        '999999'
      );
      expect(query(element, '.env-badge')?.textContent).toContain('Overridden by environment');
      expect(query(element, '.read-only-badge')?.textContent).toContain(
        'Set via environment variable'
      );

      saveButton(element).click();
      await vi.waitFor(() => {
        expect(puts).toHaveLength(1);
      });
      expect('app_id' in puts[0]!).toBe(false);
      expect(puts[0]!.api_base_url).toBe('https://ghe.example.com/api/v3');
    });
  }

  it('N8: every shown editable field is sent when cleared and every non-secret key is in the github_app registry', async () => {
    const puts: Array<Record<string, unknown>> = [];
    element = await mount((url, init) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.includes('/api/v1/admin/server-config')) {
        return jsonResponse({ settings_tier: 'db' });
      }
      if (init?.method === 'PUT') {
        puts.push(JSON.parse(init.body as string) as Record<string, unknown>);
      }
      return jsonResponse(LOADED_APP);
    });

    const el = element as unknown as Record<string, unknown> & {
      requestUpdate: () => void;
      updateComplete: Promise<boolean>;
    };
    const proto = Object.getPrototypeOf(el);
    const spy = vi.spyOn(proto, 'readOnlyReason');
    el.requestUpdate();
    await el.updateComplete;
    const shown = new Set<string>();
    spy.mock.calls.forEach((call, i) => {
      if (spy.mock.results[i]?.value === null) shown.add(call[0] as string);
    });
    spy.mockRestore();

    const CLEAR: Record<
      string,
      { clear: (target: Record<string, unknown>) => void; expected: unknown }
    > = {
      'server.github_app.app_id': { clear: (t) => (t.githubAppId = 0), expected: 0 },
      'server.github_app.api_base_url': {
        clear: (t) => (t.githubAppApiBaseUrl = ''),
        expected: '',
      },
      'server.github_app.webhooks_enabled': {
        clear: (t) => (t.githubAppWebhooksEnabled = false),
        expected: false,
      },
      'server.github_app.installation_url': {
        clear: (t) => (t.githubAppInstallationUrl = ''),
        expected: '',
      },
    };

    const ghShown = [...shown].filter((k) => k.startsWith('server.github_app.')).sort();
    expect(ghShown.length).toBeGreaterThan(0);
    expect(ghShown.filter((k) => !(k in CLEAR))).toEqual([]);
    for (const key of ghShown) CLEAR[key]!.clear(el);
    await el.updateComplete;

    saveButton(element).click();
    await vi.waitFor(() => {
      expect(puts).toHaveLength(1);
    });

    const body = puts[0]!;
    for (const key of ghShown) {
      const leaf = key.slice('server.github_app.'.length);
      expect(leaf in body, `${key} missing from PUT payload`).toBe(true);
      expect(body[leaf], key).toEqual(CLEAR[key]!.expected);
    }
    const unknown = Object.keys(body)
      .filter((k) => k !== 'private_key' && k !== 'webhook_secret')
      .map((k) => `server.github_app.${k}`)
      .filter((k) => owningSection(k) !== 'github_app');
    expect(unknown).toEqual([]);
  });

  it('R3(b): managed github_app section in DB tier offers Reset to bootstrap and calls DELETE', async () => {
    const deletes: string[] = [];
    element = await mount((url, init) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (init?.method === 'DELETE') {
        deletes.push(path);
        return jsonResponse({ status: 'reset' });
      }
      if (path.includes('/api/v1/admin/server-config')) {
        return jsonResponse({
          settings_tier: 'db',
          section_metadata: {
            github_app: {
              source: 'db',
              origin: 'managed',
              revision: 4,
              updated_by: 'admin@example.com',
              updated_at: '2026-11-25T20:00:00Z',
            },
          },
        });
      }
      return jsonResponse(LOADED_APP);
    });

    await vi.waitFor(() => {
      expect(query(element!, 'sl-button[data-action="reset-github-app"]')).not.toBeNull();
    });
    expect(shadowText(element)).toContain('Source: Database (rev 4)');
    expect(shadowText(element)).toContain('admin@example.com');

    (query(element, 'sl-button[data-action="reset-github-app"]') as HTMLElement).click();
    await element.updateComplete;

    const confirmBtn = query(
      element,
      'sl-button[data-action="confirm-reset-github-app"]'
    ) as HTMLElement;
    expect(confirmBtn).not.toBeNull();
    confirmBtn.click();

    await vi.waitFor(() => {
      expect(deletes).toEqual(['/api/v1/admin/server-config/sections/github_app']);
    });
  });

  it('R3(c): seeded github_app origin shows the tracking deployment configuration caption and no Reset button', async () => {
    element = await mount((url) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.includes('/api/v1/admin/server-config')) {
        return jsonResponse({
          settings_tier: 'db',
          section_metadata: {
            github_app: { source: 'db', origin: 'seeded', revision: 1 },
          },
        });
      }
      return jsonResponse(LOADED_APP);
    });

    await vi.waitFor(() => {
      expect(query(element!, '.seeded-caption')).not.toBeNull();
    });
    expect(query(element, '.seeded-caption')?.textContent).toContain(
      'Tracking deployment configuration — will re-sync on restart until edited'
    );
    expect(query(element, 'sl-button[data-action="reset-github-app"]')).toBeNull();
  });
});

describe('scion-admin-github-installations', () => {
  let element: ScionAdminGitHubInstallations | null = null;

  beforeAll(async () => {
    await import('../admin/admin-github-installations.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  it('lists installations and runs Discover and Sync Permissions', async () => {
    const calls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request, init?: RequestInit) => {
        const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
        calls.push(`${init?.method ?? 'GET'} ${path}`);
        if (path.endsWith('/discover')) {
          return jsonResponse({ total: 1 });
        }
        if (path.endsWith('/sync-permissions')) {
          return jsonResponse({
            affected_projects: 2,
            app_permissions: { contents: 'write' },
          });
        }
        return jsonResponse({
          installations: [
            {
              installation_id: 4412,
              account_login: 'acme',
              account_type: 'Organization',
              app_id: 123,
              repositories: ['acme/api', 'acme/web'],
              status: 'active',
            },
          ],
        });
      })
    );

    element = document.createElement(
      'scion-admin-github-installations'
    ) as ScionAdminGitHubInstallations;
    element.appConfigured = true;
    document.body.appendChild(element);

    await vi.waitFor(() => {
      expect(shadowText(element!)).toContain('acme');
    });
    expect(shadowText(element)).toContain('2 repos');

    const buttons = queryAll(element, 'sl-button');
    (buttons.find((b) => b.textContent?.includes('Discover')) as HTMLElement).click();
    await vi.waitFor(() => {
      expect(shadowText(element!)).toContain('Discovered 1 installation(s)');
    });

    (buttons.find((b) => b.textContent?.includes('Sync Permissions')) as HTMLElement).click();
    await vi.waitFor(() => {
      expect(shadowText(element!)).toContain('contents:write');
    });
    expect(calls).toContain('POST /api/v1/github-app/installations/discover');
    expect(calls).toContain('POST /api/v1/github-app/sync-permissions');
  });
});

describe('scion-admin-github-associations', () => {
  let element: ScionAdminGitHubAssociations | null = null;

  beforeAll(async () => {
    await import('../admin/admin-github-associations.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  it('renders association report rows and refreshes with ?refresh=true', async () => {
    const urls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request) => {
        const path =
          typeof url === 'string' ? url : url instanceof URL ? url.pathname + url.search : url.url;
        urls.push(path);
        return jsonResponse({
          associations: [
            {
              project_id: 'p-1',
              project_name: 'api',
              git_remote: 'https://github.com/acme/api.git',
              installation_id: 4412,
              authorized: true,
              basis: 'admin binding',
              write_allowed: true,
            },
            {
              project_id: 'p-2',
              project_name: 'sandbox',
              git_remote: 'https://github.com/acme/sandbox.git',
              installation_id: 4412,
              authorized: false,
              write_allowed: false,
              reason: 'creator token lacks access',
            },
          ],
          total: 2,
          unauthorized: 1,
          generated_at: '2026-11-25T22:00:00Z',
          truncated: false,
        });
      })
    );

    element = document.createElement(
      'scion-admin-github-associations'
    ) as ScionAdminGitHubAssociations;
    document.body.appendChild(element);
    await element.updateComplete;

    expect(urls).toEqual([]);
    expect(shadowText(element)).toContain('Configure the GitHub App above');

    element.appConfigured = true;
    await vi.waitFor(() => {
      expect(shadowText(element!)).toContain('acme/api');
    });

    const text = shadowText(element);
    expect(text).toContain('api');
    expect(text).toContain('admin binding');
    expect(text).toContain('authorized');
    expect(text).toContain('creator token lacks access');

    const refreshBtn = queryAll(element, 'sl-button').find((b) =>
      b.textContent?.includes('Refresh')
    ) as HTMLElement;
    refreshBtn.click();
    await vi.waitFor(() => {
      expect(urls).toContain('/api/v1/github-app/installations/association-report?refresh=true');
    });
  });
});
