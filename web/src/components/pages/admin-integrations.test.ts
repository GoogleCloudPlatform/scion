import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import type { ScionPageAdminIntegrations } from './admin-integrations.js';

// ── Mock data builders ──

function makeIntegrationList(): Array<Record<string, unknown>> {
  return [
    {
      name: 'discord',
      platform: 'discord',
      self_managed: false,
      has_secrets: { bot_token: true },
      status: { connected: true, version: '0.1.0' },
    },
  ];
}

function makeDiscordDetail(settingsOverride?: Record<string, string>): Record<string, unknown> {
  return {
    name: 'discord',
    platform: 'discord',
    self_managed: false,
    settings: { application_id: '123456789', guild_ids: '', ...settingsOverride },
    has_secrets: { bot_token: true },
    status: { connected: true, version: '0.1.0' },
  };
}

function createFetchHandler(
  detailResponse: Record<string, unknown>,
  opts?: {
    putHandler?: (body: Record<string, unknown>) => {
      status: number;
      body: Record<string, unknown>;
    };
  }
) {
  return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;

    if (init?.method === 'PUT' && opts?.putHandler) {
      const reqBody = JSON.parse(init.body as string);
      const result = opts.putHandler(reqBody);
      return Promise.resolve(
        new Response(JSON.stringify(result.body), {
          status: result.status,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    // Detail endpoint — /api/v1/admin/integrations/discord
    if (path.match(/\/api\/v1\/admin\/integrations\/[^/]+$/) && !path.includes('/available')) {
      return Promise.resolve(
        new Response(JSON.stringify(detailResponse), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    // List endpoint
    if (path.includes('/api/v1/admin/integrations/available')) {
      return Promise.resolve(
        new Response(JSON.stringify([]), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/api/v1/admin/integrations')) {
      return Promise.resolve(
        new Response(JSON.stringify(makeIntegrationList()), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

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
  // Simulate being on the Discord detail page.
  Object.defineProperty(window, 'location', {
    value: { pathname: '/admin/integrations/discord' },
    writable: true,
  });

  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = document.createElement('scion-page-admin-integrations') as InstanceType<
    typeof ScionPageAdminIntegrations
  >;
  document.body.appendChild(el);
  await el.updateComplete;
  // Allow async loadDetail to complete.
  await new Promise((resolve) => setTimeout(resolve, 200));
  await el.updateComplete;
  return el;
}

function shadowText(el: HTMLElement): string {
  return el.shadowRoot?.textContent ?? '';
}

function queryAll(el: HTMLElement, selector: string): Element[] {
  return Array.from(el.shadowRoot?.querySelectorAll(selector) ?? []);
}

// ── Tests ──

describe('scion-page-admin-integrations — Discord guild_ids', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler(makeDiscordDetail())));
    Object.defineProperty(window, 'location', {
      value: { pathname: '/admin/integrations/discord' },
      writable: true,
    });
    await import('./admin-integrations.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  // ── Rendering ──

  it('renders guild_ids field with correct label', async () => {
    element = await createComponent(createFetchHandler(makeDiscordDetail()));
    const text = shadowText(element);
    expect(text).toContain('Allowed Guild IDs');
  });

  it('renders guild_ids field description', async () => {
    element = await createComponent(createFetchHandler(makeDiscordDetail()));
    const hints = queryAll(element, '.hint');
    const hintTexts = hints.map((h) => h.textContent ?? '');
    expect(hintTexts.some((t) => t.includes('Comma-separated Discord server IDs'))).toBe(true);
  });

  it('renders guild_ids placeholder for empty value', async () => {
    element = await createComponent(createFetchHandler(makeDiscordDetail()));

    const inputs = queryAll(element, 'sl-input');
    const guildInput = inputs.find((input) => {
      const placeholder = input.getAttribute('placeholder');
      return placeholder?.includes('Global');
    });
    expect(guildInput).not.toBeNull();
  });

  it('displays existing guild_ids value', async () => {
    element = await createComponent(
      createFetchHandler(makeDiscordDetail({ guild_ids: '111,222,333' }))
    );

    const inputs = queryAll(element, 'sl-input') as HTMLInputElement[];
    const guildInput = inputs.find((input) => (input as any).value === '111,222,333');
    expect(guildInput).toBeDefined();
  });

  it('also renders application_id field alongside guild_ids', async () => {
    element = await createComponent(createFetchHandler(makeDiscordDetail()));
    const text = shadowText(element);
    expect(text).toContain('Application ID');
    expect(text).toContain('Allowed Guild IDs');
  });

  // ── Form submission ──

  it('includes guild_ids in PUT payload', async () => {
    let capturedPayload: Record<string, unknown> | null = null;

    element = await createComponent(
      createFetchHandler(makeDiscordDetail({ guild_ids: '111,222' }), {
        putHandler: (body) => {
          capturedPayload = body;
          return { status: 200, body: {} };
        },
      })
    );

    // Click save button
    const buttons = queryAll(element, 'sl-button[variant="primary"]');
    const saveBtn = buttons.find((b) => b.textContent?.trim().includes('Save'));
    expect(saveBtn).not.toBeNull();
    (saveBtn as HTMLElement).click();
    await new Promise((resolve) => setTimeout(resolve, 300));

    expect(capturedPayload).not.toBeNull();
    const settings = capturedPayload!.settings as Record<string, string>;
    expect(settings).toBeDefined();
    expect(settings.guild_ids).toBe('111,222');
    expect(settings.application_id).toBeDefined();
  });

  it('sends empty guild_ids when cleared (global mode)', async () => {
    let capturedPayload: Record<string, unknown> | null = null;

    element = await createComponent(
      createFetchHandler(makeDiscordDetail({ guild_ids: '' }), {
        putHandler: (body) => {
          capturedPayload = body;
          return { status: 200, body: {} };
        },
      })
    );

    const buttons = queryAll(element, 'sl-button[variant="primary"]');
    const saveBtn = buttons.find((b) => b.textContent?.trim().includes('Save'));
    (saveBtn as HTMLElement).click();
    await new Promise((resolve) => setTimeout(resolve, 300));

    expect(capturedPayload).not.toBeNull();
    const settings = capturedPayload!.settings as Record<string, string>;
    expect(settings.guild_ids).toBe('');
  });
});

// ── Save payload covers every shown field (ptone/scion#3924) ──
//
// Integration settings are per-integration plugin settings saved to
// PUT /api/v1/admin/integrations/<name>/config, not opsettings registry keys,
// so only the "every shown field is sent, explicitly when cleared" half of
// the admin settings payload check applies here. Each platform's
// Configuration fields are cleared through their own controls, plus one
// server-reported key the page has no definition for.

describe('scion-page-admin-integrations — cleared config save (ptone/scion#3924)', () => {
  let element: HTMLElement | null = null;

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  /** Integration name to the platform the page resolves it to (resolvePlatform). */
  const INTEGRATIONS = ['telegram', 'discord', 'slack', 'chat-app', 'a2a-bridge', 'teams'];

  it.each(INTEGRATIONS)(
    '%s: every shown Configuration field is sent when cleared',
    async (name) => {
      let captured: Record<string, unknown> | null = null;
      const detail = {
        name,
        platform: name,
        self_managed: false,
        // A key the page has no field definition for renders as a plain input.
        settings: { extra_server_key: 'value' },
        has_secrets: {},
        status: { connected: true, version: '0.1.0' },
      };
      Object.defineProperty(window, 'location', {
        value: { pathname: `/admin/integrations/${name}` },
        writable: true,
      });
      vi.stubGlobal(
        'fetch',
        vi.fn(
          createFetchHandler(detail, {
            putHandler: (body) => {
              captured = body;
              return { status: 200, body: {} };
            },
          })
        )
      );
      await import('./admin-integrations.js');
      element = document.createElement('scion-page-admin-integrations');
      document.body.appendChild(element);
      const el = element as any;
      await el.updateComplete;
      await new Promise((resolve) => setTimeout(resolve, 200));
      await el.updateComplete;

      const configSection = queryAll(element, '.section').find(
        (s) => s.querySelector('.section-title')?.textContent?.trim() === 'Configuration'
      );
      expect(configSection).toBeDefined();
      const fields = Array.from(configSection!.querySelectorAll('.form-grid > .form-field'));
      expect(fields.length).toBeGreaterThan(1);

      type Control = HTMLElement & { value?: string; checked?: boolean };
      const controls = fields.map((field) => {
        const control = field.querySelector('sl-input, sl-select, sl-switch, sl-textarea');
        expect(control, field.querySelector('label')?.textContent ?? '').not.toBeNull();
        return control as Control;
      });

      async function setAllAndSave(set: (c: Control, i: number) => void) {
        captured = null;
        for (const [i, control] of controls.entries()) {
          set(control, i);
          control.dispatchEvent(new Event('sl-change'));
          await el.updateComplete;
        }
        const saveBtn = queryAll(element!, 'sl-button[variant="primary"]').find(
          (b) => b.textContent?.trim() === 'Save Configuration'
        );
        (saveBtn as HTMLElement).click();
        await new Promise((resolve) => setTimeout(resolve, 300));
        expect(captured).not.toBeNull();
        return (captured as unknown as { settings: Record<string, string> }).settings;
      }

      // Each shown field reaches the payload: give every control a distinct
      // value and find each one in the sent settings.
      const isSwitch = (c: Control) => c.tagName === 'SL-SWITCH';
      const filled = await setAllAndSave((c, i) => {
        if (isSwitch(c)) c.checked = true;
        else c.value = `value-${i}`;
      });
      const sentValues = Object.values(filled);
      const notSent = controls
        .map((c, i) => (isSwitch(c) ? null : `value-${i}`))
        .filter((v): v is string => v !== null && !sentValues.includes(v));
      expect(notSent, 'shown fields missing from the save payload').toEqual([]);

      // Cleared, each one is still sent, as "" (or "false" for a switch).
      const cleared = await setAllAndSave((c) => {
        if (isSwitch(c)) c.checked = false;
        else c.value = '';
      });
      expect(Object.keys(cleared)).toHaveLength(fields.length);
      expect(cleared).toHaveProperty('extra_server_key', '');
      const notCleared = Object.entries(cleared).filter(([, v]) => v !== '' && v !== 'false');
      expect(notCleared, 'shown fields not sent as cleared').toEqual([]);
    }
  );
});
