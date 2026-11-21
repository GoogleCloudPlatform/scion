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
import type { ScionPageAdminBranding } from './admin-branding.js';

vi.mock('../../client/admin-status.js', () => ({
  loadAdminStatus: vi.fn(),
}));

vi.mock('../../client/server-feature-flags.js', () => ({
  invalidatePublicSettings: vi.fn(),
  getPublicSettings: vi.fn().mockResolvedValue(null),
}));

import { loadAdminStatus } from '../../client/admin-status.js';
import { getPublicSettings, invalidatePublicSettings } from '../../client/server-feature-flags.js';

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const BRANDING_SCHEMA = {
  sections: {
    branding: {
      koanf_paths: [
        'server.hub.branding.name',
        'server.hub.branding.short_name',
        'server.hub.branding.subtitle',
        'server.hub.branding.attribution',
        'server.hub.branding.corners',
        'server.hub.branding.font_sans',
        'server.hub.branding.accent_color',
        'server.hub.branding.logo.light',
        'server.hub.branding.logo.dark',
        'server.hub.branding.icons.favicon',
        'server.hub.branding.icons.apple_touch_icon',
        'server.hub.branding.icons.icon_192',
        'server.hub.branding.icons.icon_512',
        'server.hub.branding.icons.icon_maskable_512',
      ],
    },
  },
};

interface MockHandler {
  fetch: (url: string | URL | Request, init?: RequestInit) => Promise<Response>;
  puts: Record<string, unknown>[];
  deletes: string[];
  gets: () => number;
}

function createHandler(
  opts: {
    config?: Record<string, unknown>;
    put?: { status: number; body: unknown };
    del?: { status: number; body: unknown };
  } = {}
): MockHandler {
  let gets = 0;
  const puts: Record<string, unknown>[] = [];
  const deletes: string[] = [];

  const defaultConfig = {
    settings_tier: 'db',
    server: {
      hub: {
        branding: {
          name: 'Acme Cloud',
          corners: 'default',
          palette: {
            light: { primary: '#1d4ed8' },
          },
        },
      },
    },
    section_metadata: {
      branding: {
        source: 'db',
        origin: 'managed',
        revision: 4,
      },
    },
    env_overrides: [],
    env_values: {},
  };

  return {
    puts,
    deletes,
    gets: () => gets,
    fetch: (url, init) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.endsWith('/api/v1/admin/server-config/schema')) {
        return Promise.resolve(json(BRANDING_SCHEMA));
      }
      if (path.includes('/api/v1/admin/server-config/sections/branding')) {
        if (init?.method === 'DELETE') {
          deletes.push(path);
          return Promise.resolve(
            opts.del ? json(opts.del.body, opts.del.status) : json({ ok: true })
          );
        }
      }
      if (path.includes('/api/v1/admin/server-config')) {
        if (init?.method === 'PUT') {
          puts.push(JSON.parse(init.body as string) as Record<string, unknown>);
          return Promise.resolve(
            opts.put ? json(opts.put.body, opts.put.status) : json({ ok: true })
          );
        }
        gets++;
        return Promise.resolve(json({ ...defaultConfig, ...(opts.config ?? {}) }));
      }
      return Promise.resolve(json({}));
    },
  };
}

async function mountPage(
  handler: MockHandler,
  permissions: string[] = ['hub.config.read', 'hub.config.update']
): Promise<ScionPageAdminBranding> {
  vi.mocked(loadAdminStatus).mockResolvedValue({
    isAdmin: true,
    isSuperAdmin: false,
    permissions,
  });
  vi.stubGlobal('fetch', vi.fn(handler.fetch));
  const el = document.createElement('scion-page-admin-branding') as ScionPageAdminBranding;
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 50));
  await el.updateComplete;
  return el;
}

function query<T extends Element = Element>(el: HTMLElement, selector: string): T | null {
  return (el.shadowRoot?.querySelector(selector) as T | null) ?? null;
}

describe('scion-page-admin-branding', () => {
  let element: ScionPageAdminBranding | null = null;

  beforeAll(async () => {
    vi.mocked(loadAdminStatus).mockResolvedValue({
      isAdmin: true,
      isSuperAdmin: true,
      permissions: [],
    });
    vi.stubGlobal('fetch', vi.fn(createHandler().fetch));
    await import('./admin-branding.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.clearAllMocks();
  });

  it('loads branding config, shows CLI palette notice, saves dirty diff with expected_revisions, and reloads public settings', async () => {
    const handler = createHandler();
    element = await mountPage(handler);

    // CLI palette notice is displayed
    expect(query(element, '[data-palette-notice]')).not.toBeNull();

    // Edit only Product Name and Accent Color
    const nameInput = query<HTMLInputElement>(element, '[data-field="name"]')!;
    nameInput.value = 'Acme Platform';
    nameInput.dispatchEvent(new CustomEvent('sl-input', { bubbles: true }));

    const accentInput = query<HTMLInputElement>(element, '[data-field="accent_color"]')!;
    accentInput.value = '#0d9488';
    accentInput.dispatchEvent(new CustomEvent('sl-input', { bubbles: true }));
    await element.updateComplete;

    const saveBtn = query<HTMLElement>(element, '[data-save-btn]')!;
    saveBtn.click();
    await new Promise((r) => setTimeout(r, 50));
    await element.updateComplete;

    expect(handler.puts).toHaveLength(1);
    expect(handler.puts[0]).toEqual({
      server: {
        hub: {
          branding: {
            name: 'Acme Platform',
            accent_color: '#0d9488',
          },
        },
      },
      expected_revisions: { branding: 4 },
    });
    expect(invalidatePublicSettings).toHaveBeenCalledTimes(1);
    expect(getPublicSettings).toHaveBeenCalledTimes(1);
  });

  it('renders 422 validation errors inline at the offending field and in the section summary', async () => {
    const handler = createHandler({
      put: {
        status: 422,
        body: {
          error: {
            code: 'validation_error',
            message: 'branding validation failed',
            details: {
              errors: [
                {
                  section: 'branding',
                  field: 'server.hub.branding.accent_color',
                  message:
                    'light.primary vs light.bg contrast ratio 2.10:1 is below WCAG AA 4.50:1',
                },
                {
                  section: 'branding',
                  field: 'server.hub.branding.logo.light',
                  message: 'total decoded data: asset size exceeds 524288 bytes (512 KiB)',
                },
              ],
            },
          },
        },
      },
    });
    element = await mountPage(handler);

    const accentInput = query<HTMLInputElement>(element, '[data-field="accent_color"]')!;
    accentInput.value = '#eab308';
    accentInput.dispatchEvent(new CustomEvent('sl-input', { bubbles: true }));
    await element.updateComplete;

    query<HTMLElement>(element, '[data-save-btn]')!.click();
    await new Promise((r) => setTimeout(r, 50));
    await element.updateComplete;

    expect(query(element, '[data-field-error="accent_color"]')?.textContent).toContain('4.50:1');
    expect(query(element, '[data-field-error="logo.light"]')?.textContent).toContain('512 KiB');
    expect(query(element, '[data-section-errors="branding"]')?.textContent).toContain('4.50:1');
    expect(invalidatePublicSettings).not.toHaveBeenCalled();
  });

  it('renders 409 revision_conflict notice with Reload action', async () => {
    const handler = createHandler({
      put: {
        status: 409,
        body: {
          error: {
            code: 'revision_conflict',
            message: 'section branding changed',
            details: {
              conflicted: [{ section: 'branding', expected_revision: 4, current_revision: 5 }],
            },
          },
        },
      },
    });
    element = await mountPage(handler);

    const nameInput = query<HTMLInputElement>(element, '[data-field="name"]')!;
    nameInput.value = 'Changed';
    nameInput.dispatchEvent(new CustomEvent('sl-input', { bubbles: true }));
    await element.updateComplete;

    query<HTMLElement>(element, '[data-save-btn]')!.click();
    await new Promise((r) => setTimeout(r, 50));
    await element.updateComplete;

    expect(query(element, '[data-settings-conflict]')).not.toBeNull();
    const getsBefore = handler.gets();
    query<HTMLElement>(element, '[data-settings-reload]')!.click();
    await new Promise((r) => setTimeout(r, 50));
    expect(handler.gets()).toBe(getsBefore + 1);
  });

  it('resets branding via DELETE in DB mode and triggers live reload', async () => {
    const handler = createHandler();
    element = await mountPage(handler);

    query<HTMLElement>(element, '[data-reset-all-btn]')!.click();
    await element.updateComplete;
    query<HTMLElement>(element, '[data-confirm-reset-btn]')!.click();
    await new Promise((r) => setTimeout(r, 50));
    await element.updateComplete;

    expect(handler.deletes).toEqual(['/api/v1/admin/server-config/sections/branding']);
    expect(invalidatePublicSettings).toHaveBeenCalledTimes(1);
    expect(getPublicSettings).toHaveBeenCalledTimes(1);
  });

  it('resets branding via clear-all PUT in file mode without calling DELETE', async () => {
    const handler = createHandler({
      config: {
        settings_tier: 'file',
        server: {
          hub: {
            branding: {
              name: 'File Hub',
              accent_color: '#0d9488',
            },
          },
        },
        section_metadata: {
          branding: { source: 'file' },
        },
      },
    });
    element = await mountPage(handler);

    query<HTMLElement>(element, '[data-reset-all-btn]')!.click();
    await element.updateComplete;
    query<HTMLElement>(element, '[data-confirm-reset-btn]')!.click();
    await new Promise((r) => setTimeout(r, 50));
    await element.updateComplete;

    expect(handler.deletes).toHaveLength(0);
    expect(handler.puts).toHaveLength(1);
    const sent = (handler.puts[0].server as { hub: { branding: Record<string, unknown> } }).hub
      .branding;
    expect(sent.name).toBe('');
    expect(sent.accent_color).toBe('');
    expect(invalidatePublicSettings).toHaveBeenCalledTimes(1);
  });

  it('renders read-only state for hub.config.read-only users and hides save bar', async () => {
    const handler = createHandler();
    element = await mountPage(handler, ['hub.config.read']);

    expect(query(element, '[data-readonly-banner]')).not.toBeNull();
    expect(query(element, '[data-save-bar]')).toBeNull();
    expect(query(element, '[data-reset-all-btn]')?.hasAttribute('disabled')).toBe(true);
  });

  it('displays data: URI chip for embedded assets without raw base64 input and supports per-key reset', async () => {
    const pngBase64 = btoa('1234567890abcdef');
    const handler = createHandler({
      config: {
        settings_tier: 'db',
        server: {
          hub: {
            branding: {
              name: 'Acme',
              logo: {
                light: `data:image/png;base64,${pngBase64}`,
              },
            },
          },
        },
        section_metadata: {
          branding: { source: 'db', origin: 'managed', revision: 3 },
        },
      },
    });
    element = await mountPage(handler);

    // Embedded asset shows metadata chip, not raw base64 in an sl-input
    expect(query(element, '[data-data-uri-chip="logo.light"]')?.textContent).toContain('image/png');
    expect(query(element, '[data-field="logo.light"]')).toBeNull();

    // Per-key reset on Product Name clears only name on save
    const resetNameBtn = query<HTMLElement>(
      element,
      'sl-icon-button[label="Reset Product Name to default"]'
    )!;
    resetNameBtn.click();
    await element.updateComplete;

    query<HTMLElement>(element, '[data-save-btn]')!.click();
    await new Promise((r) => setTimeout(r, 50));
    await element.updateComplete;

    expect(handler.puts).toHaveLength(1);
    expect(handler.puts[0]).toEqual({
      server: { hub: { branding: { name: '' } } },
      expected_revisions: { branding: 3 },
    });
  });

  it('flags plain http:// asset URLs inline and disables Save', async () => {
    const handler = createHandler();
    element = await mountPage(handler);

    const logoInput = query<HTMLInputElement>(element, '[data-field="logo.light"]')!;
    logoInput.value = 'http://example.com/logo.png';
    logoInput.dispatchEvent(new CustomEvent('sl-input', { bubbles: true }));
    await element.updateComplete;

    expect(query(element, '[data-client-error="logo.light"]')?.textContent).toContain('http://');
    expect(query(element, '[data-save-btn]')?.hasAttribute('disabled')).toBe(true);
  });

  it('renders env-locked fields read-only and omits them from PUT', async () => {
    const handler = createHandler({
      config: {
        settings_tier: 'db',
        server: {
          hub: {
            branding: {
              name: 'Stored Name',
              subtitle: 'Stored Subtitle',
            },
          },
        },
        section_metadata: {
          branding: { source: 'db', origin: 'managed', revision: 2 },
        },
        env_overrides: ['server.hub.branding.name'],
        env_values: {
          'server.hub.branding.name': {
            value: 'Env Locked Name',
            in_effect: true,
            saved_differs: true,
          },
        },
      },
    });
    element = await mountPage(handler);

    expect(query(element, '[data-field="name"]')).toBeNull();
    expect(element.shadowRoot?.textContent).toContain('Env Locked Name');
    expect(element.shadowRoot?.textContent).toContain('Set via environment variable');

    // Edit an unlocked field and save: env-locked 'name' must be omitted from PUT
    const subtitleInput = query<HTMLInputElement>(element, '[data-field="subtitle"]')!;
    subtitleInput.value = 'Updated Subtitle';
    subtitleInput.dispatchEvent(new CustomEvent('sl-input', { bubbles: true }));
    await element.updateComplete;

    query<HTMLElement>(element, '[data-save-btn]')!.click();
    await new Promise((r) => setTimeout(r, 50));
    await element.updateComplete;

    expect(handler.puts).toHaveLength(1);
    expect(handler.puts[0]).toEqual({
      server: { hub: { branding: { subtitle: 'Updated Subtitle' } } },
      expected_revisions: { branding: 2 },
    });
  });

  it('renders reset failure error inside the confirmation dialog', async () => {
    const handler = createHandler({
      del: { status: 403, body: { error: { message: 'forbidden' } } },
    });
    element = await mountPage(handler);

    query<HTMLElement>(element, '[data-reset-all-btn]')!.click();
    await element.updateComplete;
    query<HTMLElement>(element, '[data-confirm-reset-btn]')!.click();
    await new Promise((r) => setTimeout(r, 50));
    await element.updateComplete;

    expect(query(element, '[data-reset-dialog]')?.hasAttribute('open')).toBe(true);
    expect(query(element, '[data-reset-error]')?.textContent).toContain('forbidden');
  });

  it('blocks saving an empty Custom Google Font and sets referrerpolicy on preview images', async () => {
    const handler = createHandler({
      config: {
        settings_tier: 'db',
        server: {
          hub: {
            branding: {
              font_sans: 'dm-sans',
              logo: { light: 'https://cdn.example.com/logo.svg' },
            },
          },
        },
        section_metadata: { branding: { source: 'db', origin: 'managed', revision: 1 } },
      },
    });
    element = await mountPage(handler);

    // Preview img has referrerpolicy="no-referrer"
    const img = query<HTMLImageElement>(element, '.logo-swatch img');
    expect(img?.getAttribute('referrerpolicy')).toBe('no-referrer');

    // Switching to Custom Google Font with empty value shows client error and disables Save
    const fontSelect = query<HTMLSelectElement>(element, '[data-field="font_sans_select"]')!;
    fontSelect.value = '__custom__';
    fontSelect.dispatchEvent(new CustomEvent('sl-change', { bubbles: true }));
    await element.updateComplete;

    expect(query(element, '[data-client-error="font_sans"]')?.textContent).toContain(
      'Google Fonts'
    );
    expect(query(element, '[data-save-btn]')?.hasAttribute('disabled')).toBe(true);
  });
});
