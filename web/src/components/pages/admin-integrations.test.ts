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

describe('scion-page-admin-integrations — structured save errors (ptone/scion#3923)', () => {
  let element: HTMLElement | null = null;

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  /** Answers the config PUT with `put` and counts detail GETs. */
  function handler(
    put: Response | (() => Response),
    detail: Record<string, unknown> = makeDiscordDetail()
  ) {
    const base = createFetchHandler(detail);
    const counts = { detailGets: 0 };
    const puts: Record<string, unknown>[] = [];
    const fetchFn = (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (init?.method === 'PUT' && path.endsWith('/config')) {
        puts.push(JSON.parse(init.body as string));
        return Promise.resolve(typeof put === 'function' ? put() : put);
      }
      if (/\/api\/v1\/admin\/integrations\/discord$/.test(path)) counts.detailGets++;
      return base(url, init);
    };
    return { fetchFn, counts, puts };
  }

  function jsonResponse(body: unknown, status: number): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });
  }

  async function clickSave(el: HTMLElement): Promise<void> {
    const btn = queryAll(el, 'sl-button').find(
      (b) => b.textContent?.trim() === 'Save Configuration'
    );
    (btn as HTMLElement).click();
    await new Promise((resolve) => setTimeout(resolve, 200));
    await (el as any).updateComplete;
  }

  function text(el: Element | null | undefined): string {
    return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
  }

  function q(el: HTMLElement, selector: string): Element | null {
    return el.shadowRoot?.querySelector(selector) ?? null;
  }

  it('a validation error naming a setting shows inline at that field and in the section summary', async () => {
    const { fetchFn } = handler(
      jsonResponse(
        {
          error: 'validation_failed',
          errors: {
            settings: [{ Path: 'application_id', Message: 'must be numeric' }],
            secrets: [{ path: 'bot_token', message: 'is too short' }],
          },
        },
        400
      )
    );
    element = await createComponent(fetchFn);

    await clickSave(element);

    expect(text(q(element, '[data-field-error="settings.application_id"]'))).toBe(
      'must be numeric'
    );
    expect(text(q(element, '[data-field-error="secrets.bot_token"]'))).toBe('is too short');
    const summary = text(q(element, '[data-section-errors="configuration"]'));
    expect(summary).toContain('must be numeric');
    expect(summary).toContain('is too short');
  });

  it('a path with no settings/secrets group is placed only when its key is in exactly one group', async () => {
    const detail = {
      ...makeDiscordDetail(),
      settings: { application_id: '123', guild_ids: '', shared: 'x' },
      has_secrets: { bot_token: true, shared: true },
    };
    const { fetchFn } = handler(
      jsonResponse(
        {
          error: 'validation_failed',
          errors: [
            { Path: 'bot_token', Message: 'secret only' },
            { Path: 'shared', Message: 'in both groups' },
            { Path: 'unknown_key', Message: 'in no group' },
          ],
        },
        400
      ),
      detail
    );
    element = await createComponent(fetchFn);

    await clickSave(element);

    expect(text(q(element, '[data-field-error="secrets.bot_token"]'))).toBe('secret only');
    expect(q(element, '[data-field-error="settings.shared"]')).toBeNull();
    expect(q(element, '[data-field-error="secrets.shared"]')).toBeNull();
    const summary = text(q(element, '[data-section-errors="configuration"]'));
    expect(summary).toContain('in both groups');
    expect(summary).toContain('in no group');
  });

  it('the save payload is unchanged by the error handling', async () => {
    const { fetchFn, puts } = handler(
      jsonResponse({ error: 'validation_failed', errors: [{ Path: 'x', Message: 'bad' }] }, 400)
    );
    element = await createComponent(fetchFn);

    await clickSave(element);
    await clickSave(element);

    const expected = { settings: { application_id: '123456789', guild_ids: '', register_url: '' } };
    expect(puts).toEqual([expected, expected]);
  });

  it('revision_conflict shows the changed-since-loaded notice; Reload refetches the integration', async () => {
    const { fetchFn, counts } = handler(
      jsonResponse({ error: 'revision_conflict', message: 'changed', conflicted: [] }, 409)
    );
    element = await createComponent(fetchFn);

    await clickSave(element);
    const notice = q(element, '[data-settings-conflict]');
    expect(text(notice)).toContain('Settings changed since you loaded them');

    const before = counts.detailGets;
    (notice!.querySelector('[data-settings-reload]') as HTMLElement).click();
    await new Promise((resolve) => setTimeout(resolve, 200));
    await (element as any).updateComplete;
    expect(counts.detailGets).toBe(before + 1);
    expect(q(element, '[data-settings-conflict]')).toBeNull();
  });

  it('layer0_rejected names the deployment-managed key', async () => {
    const { fetchFn } = handler(
      jsonResponse({ error: 'layer0_rejected', keys: ['plugins.broker.discord.mode'] }, 422)
    );
    element = await createComponent(fetchFn);

    await clickSave(element);

    const notice = text(q(element, '[data-settings-layer0]'));
    expect(notice).toContain('cannot be changed here');
    expect(notice).toContain('plugins.broker.discord.mode');
  });

  it('an unknown error keeps the generic message plus the server text', async () => {
    const { fetchFn } = handler(
      jsonResponse(
        { error: { code: 'forbidden', message: 'these keys require an interactive session' } },
        403
      )
    );
    element = await createComponent(fetchFn);

    await clickSave(element);

    expect(text(q(element, '.status-message.error'))).toBe(
      'Failed to save configuration: these keys require an interactive session'
    );
  });

  it('a malformed error body falls back to the generic message', async () => {
    const { fetchFn } = handler(() => new Response('not json', { status: 500 }));
    element = await createComponent(fetchFn);

    await clickSave(element);

    expect(text(q(element, '.status-message.error'))).toBe('Failed to save configuration');
    expect(q(element, '[data-section-errors]')).toBeNull();
  });
});
