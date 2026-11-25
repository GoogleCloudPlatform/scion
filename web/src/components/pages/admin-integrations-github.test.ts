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
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request) => {
        const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
        if (path.includes('/api/v1/github-app/installations/association-report')) {
          return jsonResponse({ associations: [], total: 0, unauthorized: 0 });
        }
        if (path.includes('/api/v1/github-app/installations')) {
          return jsonResponse({ installations: [] });
        }
        if (path.includes('/api/v1/github-app')) {
          return jsonResponse(LOADED_APP);
        }
        return jsonResponse({});
      })
    );
    await import('./admin-integrations-github.js');
    const el = document.createElement('scion-admin-integrations-github');
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 100));
    await (el as unknown as { updateComplete: Promise<boolean> }).updateComplete;

    const mounted = queryAll(el, '[data-github-section]');
    expect(mounted.map((m) => m.getAttribute('data-github-section'))).toEqual([
      'app',
      'installations',
      'associations',
    ]);
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
    handler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>
  ): Promise<ScionAdminGitHubAppConfig> {
    vi.stubGlobal('fetch', vi.fn(handler));
    const el = document.createElement('scion-admin-github-app-config') as ScionAdminGitHubAppConfig;
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 100));
    await el.updateComplete;
    return el;
  }

  function saveButton(el: HTMLElement): HTMLElement {
    const btn = query(el, 'sl-button[data-action="save-github-app"]');
    expect(btn).not.toBeNull();
    return btn as HTMLElement;
  }

  it('saves only through PUT /api/v1/github-app and never calls server-config', async () => {
    const calls: Array<{ path: string; method: string; body?: Record<string, unknown> }> = [];
    element = await mount((url, init) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      const method = init?.method ?? 'GET';
      calls.push({
        path,
        method,
        body: init?.body ? (JSON.parse(init.body as string) as Record<string, unknown>) : undefined,
      });
      return jsonResponse(LOADED_APP);
    });

    const el = element as unknown as Record<string, unknown>;
    el.githubAppId = 456;
    el.githubAppApiBaseUrl = 'https://ghe2.example.com/api/v3';
    el.githubAppInstallationUrl = 'https://github.com/apps/y/installations/new';
    el.githubAppPrivateKey = '  PEM  ';
    await element.updateComplete;

    saveButton(element).click();
    await new Promise((r) => setTimeout(r, 100));
    await element.updateComplete;

    const puts = calls.filter((c) => c.method === 'PUT');
    expect(puts).toHaveLength(1);
    expect(puts[0]!.path).toBe('/api/v1/github-app');
    expect(puts[0]!.body).toEqual({
      app_id: 456,
      api_base_url: 'https://ghe2.example.com/api/v3',
      webhooks_enabled: true,
      installation_url: 'https://github.com/apps/y/installations/new',
      private_key: 'PEM',
    });
    expect(calls.some((c) => c.path.includes('/admin/server-config'))).toBe(false);
  });

  it('sends explicit clears (0, "", false) when fields are cleared', async () => {
    const puts: Array<Record<string, unknown>> = [];
    element = await mount((url, init) => {
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
    await new Promise((r) => setTimeout(r, 100));

    expect(puts).toHaveLength(1);
    expect(puts[0]).toEqual({
      app_id: 0,
      api_base_url: '',
      webhooks_enabled: false,
      installation_url: '',
    });
  });

  it('disables Save and sends no PUT when GET /api/v1/github-app fails', async () => {
    const puts: unknown[] = [];
    element = await mount((_url, init) => {
      if (init?.method === 'PUT') {
        puts.push(init.body);
        return jsonResponse({});
      }
      return jsonResponse({ error: 'internal', message: 'boom' }, 500);
    });

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

  it('shows validation errors inline at the field and in the card summary', async () => {
    element = await mount((_url, init) => {
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
    await new Promise((r) => setTimeout(r, 100));
    await element.updateComplete;

    expect(fieldError(element, 'server.github_app.private_key')).toEqual([
      'must be a PEM-encoded key',
    ]);
    const summary = query(element, '[data-section-errors="github_app_card"]');
    expect(summary?.textContent).toContain('must be a PEM-encoded key');
  });

  it('handles revision_conflict with Reload clearing typed secrets', async () => {
    let gets = 0;
    element = await mount((_url, init) => {
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
    await new Promise((r) => setTimeout(r, 100));
    await element.updateComplete;

    const notice = query(element, '[data-settings-conflict]');
    expect(notice?.textContent).toContain('Settings changed since you loaded them');

    const before = gets;
    (notice!.querySelector('[data-settings-reload]') as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 100));
    await element.updateComplete;

    expect(gets).toBe(before + 1);
    expect(el.githubAppPrivateKey).toBe('');
    expect(el.githubAppWebhookSecret).toBe('');
  });

  it('shows active key source and override warning when file or config key overrides secret', async () => {
    element = await mount(() =>
      jsonResponse({
        ...LOADED_APP,
        private_key_source: 'file',
        private_key_overrides_secret: true,
      })
    );

    expect(query(element, '[data-github-app-key-source]')?.textContent).toContain(
      'private_key_path'
    );
    const warn = query(element, '[data-github-app-key-override-warning]');
    expect(warn?.textContent).toContain('overriding the stored hub secret');
    expect(shadowText(element)).toContain('6 hours');
    expect(shadowText(element)).toContain('24 hours');
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
    await new Promise((r) => setTimeout(r, 100));
    await element.updateComplete;

    expect(shadowText(element)).toContain('acme');
    expect(shadowText(element)).toContain('2 repos');

    const buttons = queryAll(element, 'sl-button');
    (buttons.find((b) => b.textContent?.includes('Discover')) as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 100));
    await element.updateComplete;
    expect(shadowText(element)).toContain('Discovered 1 installation(s)');

    (buttons.find((b) => b.textContent?.includes('Sync Permissions')) as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 100));
    await element.updateComplete;
    expect(shadowText(element)).toContain('contents:write');
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
    await new Promise((r) => setTimeout(r, 100));
    await element.updateComplete;

    const text = shadowText(element);
    expect(text).toContain('api');
    expect(text).toContain('acme/api');
    expect(text).toContain('admin binding');
    expect(text).toContain('authorized');
    expect(text).toContain('creator token lacks access');

    const refreshBtn = queryAll(element, 'sl-button').find((b) =>
      b.textContent?.includes('Refresh')
    ) as HTMLElement;
    refreshBtn.click();
    await new Promise((r) => setTimeout(r, 100));
    expect(urls).toContain('/api/v1/github-app/installations/association-report?refresh=true');
  });
});
