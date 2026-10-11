import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import type { ScionPageAdminFederation } from './admin-federation.js';

// Structured save errors on the Federation page (ptone/scion#3923). The page
// saves through PUT /api/v1/admin/server-config; bodies mirror pkg/hub.

function makeConfig(federation: Record<string, unknown> = {}) {
  return {
    federation: {
      enabled: true,
      algorithms: ['RS256'],
      refresh_interval: '1h',
      debounce_interval: '5s',
      trusted_issuers: [
        { issuer_url: 'https://a.example.com', issuer_type: 'hub' },
        { issuer_url: 'https://b.example.com', issuer_type: 'hub' },
      ],
      ...federation,
    },
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
  gets: () => number;
  puts: Record<string, unknown>[];
}

const SCHEMA = {
  sections: {
    federation: {
      koanf_paths: [
        'server.federation.enabled',
        'server.federation.trusted_issuers',
        'server.federation.algorithms',
        'server.federation.refresh_interval',
        'server.federation.debounce_interval',
      ],
    },
  },
};

function createHandler(
  put: { status: number; body: unknown } | { raw: Response },
  opts: { schemaStatus?: number } = {}
): Handler {
  let gets = 0;
  const puts: Record<string, unknown>[] = [];
  return {
    gets: () => gets,
    puts,
    fetch: (url, init) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      if (path.endsWith('/api/v1/admin/server-config/schema')) {
        const status = opts.schemaStatus ?? 200;
        return Promise.resolve(status === 200 ? json(SCHEMA) : json({}, status));
      }
      if (path.includes('/api/v1/admin/server-config')) {
        if (init?.method === 'PUT') {
          puts.push(JSON.parse(init.body as string));
          return Promise.resolve('raw' in put ? put.raw : json(put.body, put.status));
        }
        gets++;
        return Promise.resolve(json(makeConfig()));
      }
      return Promise.resolve(json({}));
    },
  };
}

async function createComponent(handler: Handler): Promise<HTMLElement> {
  vi.stubGlobal('fetch', vi.fn(handler.fetch));
  const el = document.createElement('scion-page-admin-federation') as InstanceType<
    typeof ScionPageAdminFederation
  >;
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 100));
  await el.updateComplete;
  return el;
}

function query(el: HTMLElement, selector: string): Element | null {
  return el.shadowRoot?.querySelector(selector) ?? null;
}

function text(el: Element | null): string {
  return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

async function clickSave(el: HTMLElement): Promise<void> {
  const btn = Array.from(el.shadowRoot?.querySelectorAll('sl-button') ?? []).find(
    (b) => b.textContent?.trim() === 'Save'
  );
  (btn as HTMLElement).click();
  await new Promise((resolve) => setTimeout(resolve, 100));
  await (el as any).updateComplete;
}

describe('scion-page-admin-federation — structured save errors (ptone/scion#3923)', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createHandler({ status: 200, body: {} }).fetch));
    await import('./admin-federation.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  it('validation_failed shows each message at its field, issuer row and section summary', async () => {
    element = await createComponent(
      createHandler({
        status: 400,
        body: {
          error: 'validation_failed',
          errors: {
            federation: [
              { Path: 'refresh_interval', Message: 'does not match pattern' },
              { Path: 'trusted_issuers/1/issuer_url', Message: 'is required' },
            ],
          },
        },
      })
    );

    await clickSave(element);

    expect(text(query(element, '[data-field-error="refresh_interval"]'))).toBe(
      'does not match pattern'
    );
    expect(text(query(element, '[data-field-error="trusted_issuers.1"]'))).toBe('is required');
    expect(query(element, '[data-field-error="trusted_issuers.0"]')).toBeNull();
    expect(text(query(element, '[data-section-errors="federation"]'))).toContain(
      'server.federation.refresh_interval does not match pattern'
    );
    expect(text(query(element, '[data-section-errors="trusted_issuers"]'))).toContain(
      'is required'
    );
    expect(query(element, '.status-message.error')).toBeNull();
  });

  it('when the schema mapping cannot be loaded, validation entries go to the section summary only', async () => {
    element = await createComponent(
      createHandler(
        {
          status: 400,
          body: {
            error: 'validation_failed',
            errors: {
              federation: [{ Path: 'refresh_interval', Message: 'does not match pattern' }],
            },
          },
        },
        { schemaStatus: 500 }
      )
    );

    await clickSave(element);

    expect(element.shadowRoot?.querySelectorAll('[data-field-error]').length).toBe(0);
    expect(text(query(element, '[data-section-errors="federation"]'))).toContain(
      'refresh_interval does not match pattern'
    );
  });

  it('a nested validation_error with details.errors shows a section summary, no inline field', async () => {
    element = await createComponent(
      createHandler({
        status: 422,
        body: {
          error: {
            code: 'validation_error',
            message: 'federation config validation failed',
            details: { errors: ['trusted_issuers[0]: issuer_url is required'] },
          },
        },
      })
    );

    await clickSave(element);

    const summary = text(query(element, '[data-section-errors="federation"]'));
    expect(summary).toContain('federation config validation failed');
    expect(summary).toContain('trusted_issuers[0]: issuer_url is required');
    expect(element.shadowRoot?.querySelectorAll('[data-field-error]').length).toBe(0);
  });

  it('revision_conflict shows the changed-since-loaded notice; Reload refetches', async () => {
    const handler = createHandler({
      status: 409,
      body: {
        error: 'revision_conflict',
        message: 'One or more sections have been modified since the expected revision.',
        conflicted: [{ section: 'federation', expected_revision: 1, current_revision: 2 }],
      },
    });
    element = await createComponent(handler);

    await clickSave(element);
    const notice = query(element, '[data-settings-conflict]');
    expect(text(notice)).toContain('Settings changed since you loaded them');
    expect(text(notice)).toContain('federation');

    const before = handler.gets();
    (notice!.querySelector('[data-settings-reload]') as HTMLElement).click();
    await new Promise((resolve) => setTimeout(resolve, 100));
    await (element as any).updateComplete;
    expect(handler.gets()).toBe(before + 1);
    expect(query(element, '[data-settings-conflict]')).toBeNull();
  });

  it('layer0_rejected names the deployment-managed key', async () => {
    element = await createComponent(
      createHandler({
        status: 422,
        body: { error: 'layer0_rejected', keys: ['server.federation.enabled'] },
      })
    );

    await clickSave(element);

    const notice = text(query(element, '[data-settings-layer0]'));
    expect(notice).toContain('Deployment-managed settings cannot be changed here');
    expect(notice).toContain('server.federation.enabled');
  });

  it('an unknown error keeps the generic message plus the server text', async () => {
    element = await createComponent(
      createHandler({
        status: 500,
        body: { error: { code: 'internal_error', message: 'Failed to update section' } },
      })
    );

    await clickSave(element);

    expect(text(query(element, '.status-message.error'))).toBe(
      'Failed to save federation config: Failed to update section'
    );
  });

  it('a malformed (non-JSON) error body falls back to the generic message', async () => {
    element = await createComponent(
      createHandler({ raw: new Response('<html>bad gateway</html>', { status: 502 }) })
    );

    await clickSave(element);

    expect(text(query(element, '.status-message.error'))).toBe('Failed to save federation config');
  });

  it('the save payload is unchanged', async () => {
    const handler = createHandler({ status: 200, body: {} });
    element = await createComponent(handler);

    await clickSave(element);

    expect(handler.puts).toEqual([makeConfig()]);
  });
});
