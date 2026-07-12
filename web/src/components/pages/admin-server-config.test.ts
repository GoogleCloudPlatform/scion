import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

// ── Shared mock data builders ──

function makeBaseConfig(overrides: Record<string, unknown> = {}) {
  return {
    schema_version: '1',
    scion_version: '0.1.0-test',
    scion_commit: 'abc123',
    scion_build_time: '2026-01-01T00:00:00Z',
    active_profile: 'default',
    default_template: 'gemini',
    image_registry: 'ghcr.io/test',
    server: {
      mode: 'standalone',
      log_level: 'info',
      log_format: 'text',
      hub: {
        port: 8080,
        host: '0.0.0.0',
        admin_emails: ['admin@example.com'],
        public_url: 'https://hub.example.com',
        soft_delete_retention: '72h',
        soft_delete_retain_files: true,
        auto_suspend_stalled: false,
      },
      broker: { enabled: false },
      database: { driver: 'postgres', url: '********' },
      auth: { dev_mode: false, user_access_mode: 'open', authorized_domains: '' },
      storage: { provider: 'local', local_path: '/data' },
      secrets: { provider: 'env' },
    },
    telemetry: {
      enabled: false,
      cloud: { enabled: false },
      hub: { enabled: false },
      local: { enabled: false },
    },
    ...overrides,
  };
}

const SCHEMA_RESPONSE = {
  sections: {
    access: { koanf_paths: ['server.hub.admin_emails', 'server.auth.user_access_mode', 'server.auth.authorized_domains'] },
    lifecycle: { koanf_paths: ['server.hub.auto_suspend_stalled', 'server.hub.soft_delete_retention', 'server.hub.soft_delete_retain_files'] },
    endpoints: { koanf_paths: ['server.hub.public_url', 'image_registry'] },
    agent_defaults: { koanf_paths: ['default_template', 'default_harness_config', 'default_max_turns', 'default_max_model_calls', 'default_max_duration', 'default_resources'] },
    telemetry: { koanf_paths: ['telemetry.enabled', 'telemetry.cloud.enabled', 'telemetry.cloud.endpoint', 'telemetry.cloud.protocol', 'telemetry.cloud.provider', 'telemetry.hub.enabled', 'telemetry.hub.report_interval', 'telemetry.local.enabled', 'telemetry.local.file', 'telemetry.local.console'] },
    notifications: { koanf_paths: ['server.notification_channels'] },
    github_app: { koanf_paths: ['server.github_app', 'server.github_app.app_id', 'server.github_app.api_base_url', 'server.github_app.webhooks_enabled', 'server.github_app.installation_url', 'server.github_app.private_key_path'] },
  },
};

function createFetchHandler(
  configResponse: Record<string, unknown>,
  opts?: {
    schemaResponse?: Record<string, unknown> | null;
    putHandler?: (body: Record<string, unknown>) => { status: number; body: Record<string, unknown> };
  },
) {
  return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;

    if (init?.method === 'PUT' && opts?.putHandler) {
      const reqBody = JSON.parse(init.body as string);
      const result = opts.putHandler(reqBody);
      return Promise.resolve(new Response(JSON.stringify(result.body), {
        status: result.status,
        headers: { 'Content-Type': 'application/json' },
      }));
    }

    if (path.includes('/api/v1/admin/server-config/schema')) {
      if (opts?.schemaResponse === null) {
        return Promise.resolve(new Response('', { status: 500 }));
      }
      return Promise.resolve(new Response(JSON.stringify(opts?.schemaResponse ?? SCHEMA_RESPONSE), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }));
    }

    if (path.includes('/api/v1/admin/server-config')) {
      return Promise.resolve(new Response(JSON.stringify(configResponse), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }));
    }

    if (path.includes('/api/v1/github-app/installations')) {
      return Promise.resolve(new Response(JSON.stringify([]), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }));
    }

    if (path.includes('/api/v1/github-app')) {
      return Promise.resolve(new Response(JSON.stringify({}), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }));
    }

    return Promise.resolve(new Response(JSON.stringify([]), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    }));
  };
}

// Import the component module once so the custom element is only registered once.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
let ScionPageAdminServerConfig: any;

async function createComponent(
  fetchHandler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>,
) {
  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = document.createElement('scion-page-admin-server-config') as InstanceType<typeof ScionPageAdminServerConfig>;
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise(resolve => setTimeout(resolve, 200));
  await el.updateComplete;
  return el;
}

function shadowText(el: HTMLElement): string {
  return el.shadowRoot?.textContent ?? '';
}

function queryAll(el: HTMLElement, selector: string): Element[] {
  return Array.from(el.shadowRoot?.querySelectorAll(selector) ?? []);
}

function query(el: HTMLElement, selector: string): Element | null {
  return el.shadowRoot?.querySelector(selector) ?? null;
}

// ── Tests ──

describe('scion-page-admin-server-config', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler(makeBaseConfig())));
    const mod = await import('./admin-server-config.js');
    ScionPageAdminServerConfig = mod.ScionPageAdminServerConfig;
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  // ── Smoke tests ──

  it('renders without errors', async () => {
    element = await createComponent(createFetchHandler(makeBaseConfig()));
    expect(element.shadowRoot).toBeTruthy();
    expect(shadowText(element)).toContain('0.1.0-test');
  });

  it('calls the config API on connect', async () => {
    element = await createComponent(createFetchHandler(makeBaseConfig()));
    expect(vi.mocked(fetch)).toHaveBeenCalledWith(
      expect.stringContaining('/api/v1/admin/server-config'),
      expect.any(Object),
    );
  });

  // ── Criterion 1: Seeded lifecycle (UI aspects) ──

  describe('Criterion 1 — Seeded lifecycle UI', () => {
    it('managed section shows source metadata (source, revision, updated_by)', async () => {
      const config = makeBaseConfig({
        settings_tier: 'db',
        section_metadata: {
          endpoints: {
            source: 'db',
            revision: 5,
            updated_by: 'admin@test.com',
            updated_at: '2026-07-01T12:00:00Z',
          },
        },
      });
      element = await createComponent(createFetchHandler(config));

      const metaEl = query(element, '.section-meta');
      expect(metaEl).not.toBeNull();
      const metaText = metaEl!.textContent ?? '';
      expect(metaText).toContain('Source:');
      expect(metaText).toContain('Database');
      expect(metaText).toContain('rev 5');
      expect(metaText).toContain('admin@test.com');
    });

    it('section metadata renders source:File for file-sourced sections', async () => {
      const config = makeBaseConfig({
        settings_tier: 'db',
        section_metadata: {
          lifecycle: { source: 'file' },
        },
      });
      element = await createComponent(createFetchHandler(config));

      const metaEl = query(element, '.section-meta');
      expect(metaEl).not.toBeNull();
      expect(metaEl!.textContent).toContain('File');
    });

    it('section metadata renders source:Default for default-sourced sections', async () => {
      const config = makeBaseConfig({
        settings_tier: 'db',
        section_metadata: {
          agent_defaults: { source: 'default' },
        },
      });
      element = await createComponent(createFetchHandler(config));

      const metaEl = query(element, '.section-meta');
      expect(metaEl).not.toBeNull();
      expect(metaEl!.textContent).toContain('Default');
    });

    it('does not render section metadata when absent (file/SQLite mode)', async () => {
      element = await createComponent(createFetchHandler(makeBaseConfig()));

      const metaEl = query(element, '.section-meta');
      expect(metaEl).toBeNull();
    });
  });

  // ── Criterion 3: DB mode, Layer-1 edit ──

  describe('Criterion 3 — DB mode, Layer-1 fields editable', () => {
    it('Layer-1 fields render as editable inputs (not read-only badges)', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      element = await createComponent(createFetchHandler(config));

      const adminEmailsInput = query(element, 'sl-input[value="admin@example.com"]');
      expect(adminEmailsInput).not.toBeNull();
    });

    it('PUT payload in DB mode includes only Layer-1 keys', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      let capturedPayload: Record<string, unknown> | null = null;

      element = await createComponent(createFetchHandler(config, {
        putHandler: (body) => {
          capturedPayload = body;
          return { status: 200, body: { reload: { applied: [] } } };
        },
      }));

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find(b => b.textContent?.trim() === 'Save & Reload');
      expect(saveBtn).not.toBeNull();
      (saveBtn as HTMLElement).click();
      await new Promise(resolve => setTimeout(resolve, 300));
      await (element as any).updateComplete;

      expect(capturedPayload).not.toBeNull();
      const server = capturedPayload!.server as Record<string, unknown> | undefined;
      const hub = server?.hub as Record<string, unknown> | undefined;

      // Layer-1 keys should be present
      expect(hub).toBeDefined();
      expect(hub!.admin_emails).toBeDefined();

      // Layer-0 keys must be absent from the payload
      expect(server?.mode).toBeUndefined();
      expect(server?.log_level).toBeUndefined();
      expect(server?.database).toBeUndefined();
      expect(server?.storage).toBeUndefined();
      expect(server?.secrets).toBeUndefined();
      expect(server?.broker).toBeUndefined();
      expect(capturedPayload!.active_profile).toBeUndefined();
    });

    it('PUT in DB mode does not trigger a 422', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      let putCalled = false;

      element = await createComponent(createFetchHandler(config, {
        putHandler: () => {
          putCalled = true;
          return { status: 200, body: { reload: { applied: [] } } };
        },
      }));

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find(b => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise(resolve => setTimeout(resolve, 300));

      expect(putCalled).toBe(true);
      expect(query(element, '.safety-net-notice')).toBeNull();
    });
  });

  // ── Criterion 4: DB mode, Layer-0 read-only ──

  describe('Criterion 4 — DB mode, Layer-0 read-only', () => {
    it('Layer-0 fields render as read-only with bootstrap badge', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      element = await createComponent(createFetchHandler(config));

      const badges = queryAll(element, '.read-only-badge');
      expect(badges.length).toBeGreaterThan(0);
      const badgeTexts = badges.map(b => b.textContent ?? '');
      expect(badgeTexts.some(t => t.includes('deployment configuration'))).toBe(true);
    });

    it('server.mode renders as read-only value (not editable select)', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      element = await createComponent(createFetchHandler(config));

      const readOnlyValues = queryAll(element, '.read-only-value');
      const values = readOnlyValues.map(el => el.textContent?.trim());
      expect(values).toContain('standalone');
    });

    it('database.driver renders as read-only in DB mode', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      element = await createComponent(createFetchHandler(config));

      const readOnlyValues = queryAll(element, '.read-only-value');
      const values = readOnlyValues.map(el => el.textContent?.trim());
      expect(values).toContain('postgres');
    });

    it('log_level renders as read-only in DB mode', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      element = await createComponent(createFetchHandler(config));

      const readOnlyValues = queryAll(element, '.read-only-value');
      const values = readOnlyValues.map(el => el.textContent?.trim());
      expect(values).toContain('info');
    });

    it('hub.port renders as read-only in DB mode', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      element = await createComponent(createFetchHandler(config));

      const readOnlyValues = queryAll(element, '.read-only-value');
      const values = readOnlyValues.map(el => el.textContent?.trim());
      expect(values).toContain('8080');
    });

    it('Layer-0 keys are absent from PUT payload in DB mode', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      let capturedPayload: Record<string, unknown> | null = null;

      element = await createComponent(createFetchHandler(config, {
        putHandler: (body) => {
          capturedPayload = body;
          return { status: 200, body: { reload: { applied: [] } } };
        },
      }));

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find(b => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise(resolve => setTimeout(resolve, 300));

      expect(capturedPayload).not.toBeNull();
      const server = capturedPayload!.server as Record<string, unknown> | undefined;

      expect(server?.mode).toBeUndefined();
      expect(server?.log_level).toBeUndefined();
      expect(server?.log_format).toBeUndefined();
      expect(server?.database).toBeUndefined();
      expect(server?.storage).toBeUndefined();
      expect(server?.secrets).toBeUndefined();
      expect(server?.broker).toBeUndefined();
      expect(server?.message_broker).toBeUndefined();
    });
  });

  // ── Criterion 6: File mode ──

  describe('Criterion 6 — File mode', () => {
    it('all fields editable when no env_overrides', async () => {
      const config = makeBaseConfig({ settings_tier: 'file' });
      element = await createComponent(createFetchHandler(config));

      const badges = queryAll(element, '.read-only-badge');
      expect(badges.length).toBe(0);
    });

    it('env-overridden field renders read-only with env badge', async () => {
      const config = makeBaseConfig({
        settings_tier: 'file',
        env_overrides: ['server.hub.port'],
      });
      element = await createComponent(createFetchHandler(config));

      const badges = queryAll(element, '.read-only-badge');
      const badgeTexts = badges.map(b => b.textContent ?? '');
      expect(badgeTexts.some(t => t.includes('environment variable'))).toBe(true);

      const readOnlyValues = queryAll(element, '.read-only-value');
      const values = readOnlyValues.map(el => el.textContent?.trim());
      expect(values).toContain('8080');
    });

    it('env-pinned field is absent from file-mode PUT payload', async () => {
      const config = makeBaseConfig({
        settings_tier: 'file',
        env_overrides: ['server.hub.port'],
      });
      let capturedPayload: Record<string, unknown> | null = null;

      element = await createComponent(createFetchHandler(config, {
        putHandler: (body) => {
          capturedPayload = body;
          return { status: 200, body: { reload: { applied: [] } } };
        },
      }));

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find(b => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise(resolve => setTimeout(resolve, 300));

      expect(capturedPayload).not.toBeNull();
      const server = capturedPayload!.server as Record<string, unknown> | undefined;
      const hub = server?.hub as Record<string, unknown> | undefined;

      expect(hub?.port).toBeUndefined();
      expect(hub?.admin_emails).toBeDefined();
    });

    it('non-env-overridden fields remain editable in file mode', async () => {
      const config = makeBaseConfig({
        settings_tier: 'file',
        env_overrides: ['server.hub.port'],
      });
      element = await createComponent(createFetchHandler(config));

      const adminInput = query(element, 'sl-input[value="admin@example.com"]');
      expect(adminInput).not.toBeNull();
    });

    it('file mode PUT payload includes Layer-0 keys when no env overrides', async () => {
      const config = makeBaseConfig({ settings_tier: 'file' });
      let capturedPayload: Record<string, unknown> | null = null;

      element = await createComponent(createFetchHandler(config, {
        putHandler: (body) => {
          capturedPayload = body;
          return { status: 200, body: { reload: { applied: [] } } };
        },
      }));

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find(b => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise(resolve => setTimeout(resolve, 300));

      expect(capturedPayload).not.toBeNull();
      const server = capturedPayload!.server as Record<string, unknown> | undefined;

      expect(server?.mode).toBeDefined();
      expect(server?.database).toBeDefined();
      expect(server?.hub).toBeDefined();
    });
  });

  // ── Criterion 8: Structured errors ──

  describe('Criterion 8 — Structured error handling', () => {
    it('400 validation_failed renders inline errors', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });

      element = await createComponent(createFetchHandler(config, {
        putHandler: () => ({
          status: 400,
          body: {
            error: 'validation_failed',
            errors: {
              access: [
                { field: 'admin_emails', message: 'invalid email format' },
              ],
            },
          },
        }),
      }));

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find(b => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise(resolve => setTimeout(resolve, 300));
      await (element as any).updateComplete;

      const errorsEl = query(element, '.validation-errors');
      expect(errorsEl).not.toBeNull();
      const errText = errorsEl!.textContent ?? '';
      expect(errText).toContain('invalid email format');
      expect(errText).toContain('admin_emails');
    });

    it('409 revision_conflict renders conflict banner with Reload button', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });

      element = await createComponent(createFetchHandler(config, {
        putHandler: () => ({
          status: 409,
          body: {
            error: 'revision_conflict',
            message: 'Settings have been changed since you loaded this page.',
            conflicted: [{ section: 'access', expected_revision: 3, current_revision: 5 }],
          },
        }),
      }));

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find(b => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise(resolve => setTimeout(resolve, 300));
      await (element as any).updateComplete;

      const bannerEl = query(element, '.conflict-banner');
      expect(bannerEl).not.toBeNull();
      const bannerText = bannerEl!.textContent ?? '';
      expect(bannerText).toContain('changed since you loaded');

      const reloadBtn = bannerEl!.querySelector('sl-button');
      expect(reloadBtn).not.toBeNull();
      expect(reloadBtn!.textContent).toContain('Reload');
    });

    it('422 layer0_rejected renders safety net notice and logs keys', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      const consoleSpy = vi.spyOn(console, 'log').mockImplementation(() => {});

      element = await createComponent(createFetchHandler(config, {
        putHandler: () => ({
          status: 422,
          body: {
            error: 'layer0_rejected',
            keys: ['server.mode', 'server.database.driver'],
          },
        }),
      }));

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find(b => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise(resolve => setTimeout(resolve, 300));
      await (element as any).updateComplete;

      const noticeEl = query(element, '.safety-net-notice');
      expect(noticeEl).not.toBeNull();
      const noticeText = noticeEl!.textContent ?? '';
      expect(noticeText).toContain('server.mode');
      expect(noticeText).toContain('server.database.driver');
      expect(noticeText).toContain('bootstrap');

      expect(consoleSpy).toHaveBeenCalledWith(
        expect.stringContaining('layer0_rejected'),
        expect.arrayContaining(['server.mode', 'server.database.driver']),
      );

      consoleSpy.mockRestore();
    });

    it('unknown error falls back to generic error message', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });

      element = await createComponent(createFetchHandler(config, {
        putHandler: () => ({
          status: 500,
          body: {
            error: 'some_unknown_error',
            message: 'Something went terribly wrong',
          },
        }),
      }));

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find(b => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise(resolve => setTimeout(resolve, 300));
      await (element as any).updateComplete;

      expect(shadowText(element)).toContain('Something went terribly wrong');
    });
  });

  // ── Schema fallback ──

  describe('Schema endpoint fallback', () => {
    it('falls back to STATIC_LAYER1_KEYS when schema endpoint fails', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      element = await createComponent(createFetchHandler(config, { schemaResponse: null }));

      const adminInput = query(element, 'sl-input[value="admin@example.com"]');
      expect(adminInput).not.toBeNull();

      const badges = queryAll(element, '.read-only-badge');
      expect(badges.length).toBeGreaterThan(0);
      const badgeTexts = badges.map(b => b.textContent ?? '');
      expect(badgeTexts.some(t => t.includes('deployment configuration'))).toBe(true);
    });
  });
});
