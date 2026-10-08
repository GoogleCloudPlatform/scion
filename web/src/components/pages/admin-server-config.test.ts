import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { setPreferredTimeZone } from '../../utils/time.js';

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
    access: {
      koanf_paths: [
        'server.hub.admin_emails',
        'server.auth.user_access_mode',
        'server.auth.default_user_role',
        'server.auth.authorized_domains',
      ],
    },
    lifecycle: {
      koanf_paths: [
        'server.hub.auto_suspend_stalled',
        'server.hub.soft_delete_retention',
        'server.hub.soft_delete_retain_files',
      ],
    },
    endpoints: { koanf_paths: ['server.hub.public_url', 'image_registry'] },
    agent_defaults: {
      koanf_paths: [
        'default_template',
        'default_harness_config',
        'default_max_turns',
        'default_max_model_calls',
        'default_max_duration',
        'default_resources',
        'default_timezone',
      ],
    },
    telemetry: {
      koanf_paths: [
        'telemetry.enabled',
        'telemetry.cloud.enabled',
        'telemetry.cloud.endpoint',
        'telemetry.cloud.protocol',
        'telemetry.cloud.provider',
        'telemetry.hub.enabled',
        'telemetry.hub.report_interval',
        'telemetry.local.enabled',
        'telemetry.local.file',
        'telemetry.local.console',
      ],
    },
    notifications: { koanf_paths: ['server.notification_channels'] },
    github_app: {
      koanf_paths: [
        'server.github_app',
        'server.github_app.app_id',
        'server.github_app.api_base_url',
        'server.github_app.webhooks_enabled',
        'server.github_app.installation_url',
        'server.github_app.private_key_path',
      ],
    },
    agent_secrets: {
      koanf_paths: ['agent_secrets.user_scope_only'],
    },
  },
};

function createFetchHandler(
  configResponse: Record<string, unknown>,
  opts?: {
    schemaResponse?: Record<string, unknown> | null;
    putHandler?: (body: Record<string, unknown>) => {
      status: number;
      body: unknown;
    };
    messagingResponse?: Record<string, unknown>;
    checkUpdatesResponse?: Record<string, unknown>;
    onOperationRun?: (path: string) => void;
  }
) {
  return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;

    if (
      init?.method === 'PUT' &&
      opts?.putHandler &&
      path.includes('/api/v1/admin/server-config') &&
      !path.includes('/schema')
    ) {
      const reqBody = JSON.parse(init.body as string);
      const result = opts.putHandler(reqBody);
      return Promise.resolve(
        new Response(JSON.stringify(result.body), {
          status: result.status,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/api/v1/admin/server-config/schema')) {
      if (opts?.schemaResponse === null) {
        return Promise.resolve(new Response('', { status: 500 }));
      }
      return Promise.resolve(
        new Response(JSON.stringify(opts?.schemaResponse ?? SCHEMA_RESPONSE), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/api/v1/admin/server-config')) {
      return Promise.resolve(
        new Response(JSON.stringify(configResponse), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/api/v1/admin/messaging')) {
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
      return Promise.resolve(
        new Response(
          JSON.stringify(
            opts?.messagingResponse ?? {
              cross_project_messaging_enabled: false,
              revision: 1,
            }
          ),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
    }

    if (path.includes('/api/v1/admin/maintenance/check-updates')) {
      return Promise.resolve(
        new Response(
          JSON.stringify(opts?.checkUpdatesResponse ?? { tier: 'source', update_available: false }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );
    }

    if (path.includes('/api/v1/admin/maintenance/operations/') && path.endsWith('/run')) {
      opts?.onOperationRun?.(path);
      return Promise.resolve(
        new Response(JSON.stringify({ runId: 'run-1', status: 'running' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/api/v1/github-app/installations')) {
      return Promise.resolve(
        new Response(JSON.stringify([]), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    if (path.includes('/api/v1/github-app')) {
      return Promise.resolve(
        new Response(JSON.stringify({}), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }

    return Promise.resolve(
      new Response(JSON.stringify([]), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  };
}

// Import the component module once so the custom element is only registered once.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
let ScionPageAdminServerConfig: any;

async function createComponent(
  fetchHandler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>
) {
  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = document.createElement('scion-page-admin-server-config') as InstanceType<
    typeof ScionPageAdminServerConfig
  >;
  document.body.appendChild(el);
  await el.updateComplete;
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
      expect.any(Object)
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

    it('section metadata time renders in the display zone, 24-hour, with a zone label (tz-refactor task 20)', async () => {
      const config = makeBaseConfig({
        settings_tier: 'db',
        section_metadata: {
          // Midnight in Tokyo (UTC+9); the browser zone is pinned to UTC.
          endpoints: { source: 'db', revision: 5, updated_at: '2026-07-01T15:00:00Z' },
        },
      });
      setPreferredTimeZone('Asia/Tokyo');
      try {
        element = await createComponent(createFetchHandler(config));
        const metaText = () => query(element!, '.section-meta')?.textContent ?? '';
        expect(metaText()).toContain('Jul 2, 2026, 00:00 (Asia/Tokyo)');

        setPreferredTimeZone('UTC');
        await element.updateComplete;
        expect(metaText()).toContain('Jul 1, 2026, 15:00 (UTC)');
      } finally {
        setPreferredTimeZone('');
      }
    });

    it('build time renders in the display zone with the raw value as its title (tz-refactor task 20)', async () => {
      const buildTime = (el: HTMLElement) =>
        Array.from(el.shadowRoot?.querySelectorAll('.version-item') ?? [])
          .find(
            (item) => item.querySelector('.version-label')?.textContent?.trim() === 'Build Time'
          )
          ?.querySelector('.version-value') ?? null;
      setPreferredTimeZone('Asia/Tokyo');
      try {
        // Midnight in Tokyo (UTC+9); the browser zone is pinned to UTC.
        element = await createComponent(
          createFetchHandler(makeBaseConfig({ scion_build_time: '2026-07-01T15:00:00Z' }))
        );
        const value = buildTime(element);
        expect(value?.textContent?.trim()).toBe('Jul 2, 2026, 00:00 (Asia/Tokyo)');
        expect(value?.getAttribute('title')).toBe('2026-07-01T15:00:00Z');

        element.remove();
        // A value that is not an instant is shown unchanged.
        element = await createComponent(
          createFetchHandler(makeBaseConfig({ scion_build_time: 'unknown' }))
        );
        expect(buildTime(element)?.textContent?.trim()).toBe('unknown');
      } finally {
        setPreferredTimeZone('');
      }
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

      element = await createComponent(
        createFetchHandler(config, {
          putHandler: (body) => {
            capturedPayload = body;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      expect(saveBtn).not.toBeNull();
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));
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

      element = await createComponent(
        createFetchHandler(config, {
          putHandler: () => {
            putCalled = true;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));

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
      const badgeTexts = badges.map((b) => b.textContent ?? '');
      expect(badgeTexts.some((t) => t.includes('deployment configuration'))).toBe(true);
    });

    it('server.mode renders as read-only value (not editable select)', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      element = await createComponent(createFetchHandler(config));

      const readOnlyValues = queryAll(element, '.read-only-value');
      const values = readOnlyValues.map((el) => el.textContent?.trim());
      expect(values).toContain('standalone');
    });

    it('database.driver renders as read-only in DB mode', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      element = await createComponent(createFetchHandler(config));

      const readOnlyValues = queryAll(element, '.read-only-value');
      const values = readOnlyValues.map((el) => el.textContent?.trim());
      expect(values).toContain('postgres');
    });

    it('log_level renders as read-only in DB mode', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      element = await createComponent(createFetchHandler(config));

      const readOnlyValues = queryAll(element, '.read-only-value');
      const values = readOnlyValues.map((el) => el.textContent?.trim());
      expect(values).toContain('info');
    });

    it('hub.port renders as read-only in DB mode', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      element = await createComponent(createFetchHandler(config));

      const readOnlyValues = queryAll(element, '.read-only-value');
      const values = readOnlyValues.map((el) => el.textContent?.trim());
      expect(values).toContain('8080');
    });

    it('Layer-0 keys are absent from PUT payload in DB mode', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });
      let capturedPayload: Record<string, unknown> | null = null;

      element = await createComponent(
        createFetchHandler(config, {
          putHandler: (body) => {
            capturedPayload = body;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));

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

  // ── Workstation hubs (layer0_editable): Layer-0 editable on the DB tier ──

  describe('DB mode on a workstation hub (layer0_editable)', () => {
    it('Layer-0 fields are editable: no deployment-configuration badges', async () => {
      const config = makeBaseConfig({ settings_tier: 'db', layer0_editable: true });
      element = await createComponent(createFetchHandler(config));

      const badgeTexts = queryAll(element, '.read-only-badge').map((b) => b.textContent ?? '');
      expect(badgeTexts.some((t) => t.includes('deployment configuration'))).toBe(false);
      const values = queryAll(element, '.read-only-value').map((el) => el.textContent?.trim());
      expect(values).not.toContain('postgres');
      expect(values).not.toContain('8080');
    });

    it('env-pinned fields stay read-only', async () => {
      const config = makeBaseConfig({
        settings_tier: 'db',
        layer0_editable: true,
        env_overrides: ['server.hub.port'],
      });
      element = await createComponent(createFetchHandler(config));

      const badgeTexts = queryAll(element, '.read-only-badge').map((b) => b.textContent ?? '');
      expect(badgeTexts.some((t) => t.includes('environment variable'))).toBe(true);
    });

    async function capturePut(
      config: Record<string, unknown>,
      mutate: (el: any) => void
    ): Promise<Record<string, unknown>> {
      let captured: Record<string, unknown> | null = null;
      element = await createComponent(
        createFetchHandler(config, {
          putHandler: (body) => {
            captured = body;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );
      mutate(element as any);
      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));
      expect(captured).not.toBeNull();
      return captured!;
    }

    it('changed Layer-0 fields go in the PUT payload for the server to split', async () => {
      const payload = await capturePut(
        makeBaseConfig({ settings_tier: 'db', layer0_editable: true }),
        (el) => {
          el.logLevel = 'debug';
          el.dbDriver = 'sqlite';
        }
      );
      const server = payload.server as Record<string, unknown> | undefined;
      expect(server?.log_level).toBe('debug');
      expect((server?.database as Record<string, unknown> | undefined)?.driver).toBe('sqlite');
      // The masked database URL is left out so the stored value is kept.
      expect((server?.database as Record<string, unknown> | undefined)?.url).toBeUndefined();
    });

    it('a minimal workstation GET with no user change sends no Layer-0 leaves', async () => {
      // The GET a hub returns for a stock workstation settings.yaml.
      const minimal = {
        schema_version: '1',
        settings_tier: 'db',
        layer0_editable: true,
        server: {
          hub: { soft_delete_retain_files: false, auto_suspend_stalled: false },
          broker: { broker_id: 'b-1', broker_token: '********' },
          auth: {},
          github_app: {},
        },
      };
      const payload = await capturePut(minimal, () => {});
      expect(payload).not.toHaveProperty('active_profile');
      expect(payload).not.toHaveProperty('workspace_path');
      const server = (payload.server ?? {}) as Record<string, Record<string, unknown>>;
      for (const block of [
        'broker',
        'database',
        'storage',
        'secrets',
        'message_broker',
        'native_chat',
      ]) {
        expect(server).not.toHaveProperty(block);
      }
      expect(server.hub ?? {}).not.toHaveProperty('port');
      expect(server.auth ?? {}).not.toHaveProperty('dev_token');
      expect(server).not.toHaveProperty('log_format');
    });

    it('changing one Layer-0 field sends exactly that leaf', async () => {
      const minimal = {
        schema_version: '1',
        settings_tier: 'db',
        layer0_editable: true,
        server: { hub: {}, broker: { broker_id: 'b-1', broker_token: '********' }, auth: {} },
      };
      const payload = await capturePut(minimal, (el) => {
        el.storageBucket = 'my-bucket';
      });
      const server = payload.server as Record<string, Record<string, unknown>>;
      expect(server.storage).toEqual({ bucket: 'my-bucket' });
      expect(server).not.toHaveProperty('broker');
      expect(payload).not.toHaveProperty('active_profile');
    });

    it('auto_provide shows as on when GET omits it', async () => {
      element = await createComponent(
        createFetchHandler({
          schema_version: '1',
          settings_tier: 'db',
          layer0_editable: true,
          server: { broker: { broker_id: 'b-1' } },
        })
      );
      expect((element as any).brokerAutoProvide).toBe(true);
    });

    it('clearing authorized_domains and public_url sends [] and ""', async () => {
      const payload = await capturePut(
        makeBaseConfig({ settings_tier: 'db', layer0_editable: true }),
        (el) => {
          el.authAuthorizedDomains = '';
          el.hubPublicUrl = '';
        }
      );
      const server = payload.server as Record<string, Record<string, unknown>>;
      expect(server.auth.authorized_domains).toEqual([]);
      expect(server.hub.public_url).toBe('');
      // The unchanged Layer-0 port is not sent.
      expect(server.hub).not.toHaveProperty('port');
    });

    it('cleared and switched-off Layer-0 fields are sent explicitly', async () => {
      const base = makeBaseConfig({ settings_tier: 'db', layer0_editable: true }) as any;
      base.server.storage.bucket = 'old-bucket';
      base.server.message_broker = { enabled: true, type: 'inprocess' };
      const payload = await capturePut(base, (el) => {
        el.logFormat = '';
        el.storageBucket = '';
        el.messageBrokerEnabled = false;
      });
      const server = payload.server as Record<string, Record<string, unknown> | string>;
      expect(server.log_format).toBe('');
      expect(server.storage).toEqual({ bucket: '' });
      expect(server.message_broker).toEqual({ enabled: false });
    });

    it('defaulted fields GET omitted are not sent from an untouched form', async () => {
      // makeBaseConfig has no gcp_iam_* and no native_chat.
      const payload = await capturePut(
        makeBaseConfig({ settings_tier: 'db', layer0_editable: true }),
        () => {}
      );
      const server = payload.server as Record<string, Record<string, unknown>>;
      expect(server.hub).not.toHaveProperty('gcp_iam_check_mode');
      expect(server.hub).not.toHaveProperty('gcp_iam_deny_unknown_policy');
      expect(server).not.toHaveProperty('native_chat');
    });

    it('defaulted fields are sent only when the user changed them', async () => {
      const base = makeBaseConfig({ settings_tier: 'db', layer0_editable: true }) as any;
      base.server.hub.gcp_iam_check_mode = 'enforce';
      const payload = await capturePut(base, (el) => {
        el.nativeChatEnabled = false;
      });
      const server = payload.server as Record<string, Record<string, unknown>>;
      // Unchanged since GET (even though GET had it): not sent.
      expect(server.hub).not.toHaveProperty('gcp_iam_check_mode');
      expect(server.hub).not.toHaveProperty('gcp_iam_deny_unknown_policy');
      expect(server.native_chat).toEqual({ enabled: false });
    });

    it('a hosted save never sends the file-only gcp_iam keys', async () => {
      const payload = await capturePut(makeBaseConfig({ settings_tier: 'db' }), () => {});
      const hub = (payload.server as Record<string, Record<string, unknown>> | undefined)?.hub;
      expect(hub ?? {}).not.toHaveProperty('gcp_iam_check_mode');
      expect(hub ?? {}).not.toHaveProperty('gcp_iam_deny_unknown_policy');
    });

    it('flag-managed workstation fields are read-only and not sent', async () => {
      const config = makeBaseConfig({ settings_tier: 'db', layer0_editable: true });
      const payload = await capturePut(config, () => {});
      const badgeTexts = queryAll(element!, '.read-only-badge').map((b) => b.textContent ?? '');
      expect(badgeTexts.some((t) => t.includes('workstation startup defaults'))).toBe(true);
      const server = payload.server as Record<string, Record<string, unknown>>;
      expect(server.broker?.enabled).toBeUndefined();
      expect(server.auth?.dev_mode).toBeUndefined();
      expect(server.storage?.provider).toBeUndefined();
      expect(server.hub?.host).toBeUndefined();
    });
  });

  describe('agent-default fields the hub cannot save (ptone/scion#3067)', () => {
    for (const tier of [
      { settings_tier: 'db' },
      { settings_tier: 'db', layer0_editable: true },
      { settings_tier: 'file' },
    ]) {
      it(`are not sent and not editable (${JSON.stringify(tier)})`, async () => {
        const config = makeBaseConfig({ ...tier, default_agent_role: 'full' });
        let captured: Record<string, unknown> | null = null;
        element = await createComponent(
          createFetchHandler(config, {
            putHandler: (body) => {
              captured = body;
              return { status: 200, body: { reload: { applied: [] } } };
            },
          })
        );
        expect(shadowText(element)).toContain('ptone/scion#3067');
        const buttons = queryAll(element, 'sl-button[variant="primary"]');
        (buttons.find((b) => b.textContent?.trim() === 'Save & Reload') as HTMLElement).click();
        await new Promise((resolve) => setTimeout(resolve, 300));
        expect(captured).not.toBeNull();
        expect(captured!).not.toHaveProperty('default_agent_role');
        expect(captured!).not.toHaveProperty('default_max_agent_role');
        expect(captured!).not.toHaveProperty('default_harness_auth');
      });
    }
  });

  describe('save errors that name keys', () => {
    for (const code of ['unpersisted_keys_rejected', 'unclassified_keys_rejected']) {
      it(`${code} shows the message and the keys`, async () => {
        element = await createComponent(
          createFetchHandler(makeBaseConfig({ settings_tier: 'db' }), {
            putHandler: () => ({
              status: 422,
              body: { error: code, message: 'Not saved.', keys: ['server.hub.bogus', 'x.y'] },
            }),
          })
        );
        const buttons = queryAll(element, 'sl-button[variant="primary"]');
        (buttons.find((b) => b.textContent?.trim() === 'Save & Reload') as HTMLElement).click();
        await new Promise((resolve) => setTimeout(resolve, 300));
        await (element as any).updateComplete;
        const text = shadowText(element);
        expect(text).toContain('Not saved.');
        expect(text).toContain('server.hub.bogus');
        expect(text).toContain('x.y');
      });
    }
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
      const badgeTexts = badges.map((b) => b.textContent ?? '');
      expect(badgeTexts.some((t) => t.includes('environment variable'))).toBe(true);

      const readOnlyValues = queryAll(element, '.read-only-value');
      const values = readOnlyValues.map((el) => el.textContent?.trim());
      expect(values).toContain('8080');
    });

    it('env-pinned field is absent from file-mode PUT payload', async () => {
      const config = makeBaseConfig({
        settings_tier: 'file',
        env_overrides: ['server.hub.port'],
      });
      let capturedPayload: Record<string, unknown> | null = null;

      element = await createComponent(
        createFetchHandler(config, {
          putHandler: (body) => {
            capturedPayload = body;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));

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

      element = await createComponent(
        createFetchHandler(config, {
          putHandler: (body) => {
            capturedPayload = body;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));

      expect(capturedPayload).not.toBeNull();
      const server = capturedPayload!.server as Record<string, unknown> | undefined;

      expect(server?.mode).toBeDefined();
      expect(server?.database).toBeDefined();
      expect(server?.hub).toBeDefined();
    });
  });

  // ── Agent Defaults card: default_timezone (tz-refactor task 12) ──

  describe('Agent Defaults card — default_timezone', () => {
    function timezonePicker(el: HTMLElement): Element | null {
      return query(el, 'scion-timezone-picker');
    }

    function emitTimezoneChange(el: HTMLElement, timezone: string): void {
      timezonePicker(el)!.dispatchEvent(
        new CustomEvent('timezone-change', { detail: { timezone } })
      );
    }

    it('loads the current default_timezone into the picker', async () => {
      const config = makeBaseConfig({ settings_tier: 'db', default_timezone: 'Asia/Tokyo' });
      element = await createComponent(createFetchHandler(config));

      const picker = timezonePicker(element);
      expect(picker).not.toBeNull();
      expect((picker as HTMLElement & { value: string }).value).toBe('Asia/Tokyo');
    });

    it('DB mode: PUT payload includes the edited default_timezone', async () => {
      const config = makeBaseConfig({ settings_tier: 'db', default_timezone: '' });
      let capturedPayload: Record<string, unknown> | null = null;

      element = await createComponent(
        createFetchHandler(config, {
          putHandler: (body) => {
            capturedPayload = body;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );

      emitTimezoneChange(element, 'Europe/Berlin');
      await (element as any).updateComplete;

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));

      expect(capturedPayload).not.toBeNull();
      expect(capturedPayload!.default_timezone).toBe('Europe/Berlin');
    });

    it('DB mode: clearing default_timezone sends an explicit empty string', async () => {
      const config = makeBaseConfig({ settings_tier: 'db', default_timezone: 'Asia/Tokyo' });
      let capturedPayload: Record<string, unknown> | null = null;

      element = await createComponent(
        createFetchHandler(config, {
          putHandler: (body) => {
            capturedPayload = body;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );

      emitTimezoneChange(element, '');
      await (element as any).updateComplete;

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));

      expect(capturedPayload).not.toBeNull();
      expect(capturedPayload!.default_timezone).toBe('');
    });

    it('file mode: clearing default_timezone sends an explicit empty string, not an omitted key', async () => {
      // Regression guard: unlike default_runtime_broker (`|| undefined`),
      // default_timezone must send "" explicitly on clear, because the
      // backend's *string field treats an omitted key as "no change" and
      // only an explicit "" as "clear" (see admin_settings.go).
      const config = makeBaseConfig({ settings_tier: 'file', default_timezone: 'Asia/Tokyo' });
      let capturedPayload: Record<string, unknown> | null = null;

      element = await createComponent(
        createFetchHandler(config, {
          putHandler: (body) => {
            capturedPayload = body;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );

      emitTimezoneChange(element, '');
      await (element as any).updateComplete;

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));

      expect(capturedPayload).not.toBeNull();
      expect('default_timezone' in capturedPayload!).toBe(true);
      expect(capturedPayload!.default_timezone).toBe('');
    });

    it('file mode: PUT payload includes the edited default_timezone', async () => {
      const config = makeBaseConfig({ settings_tier: 'file', default_timezone: '' });
      let capturedPayload: Record<string, unknown> | null = null;

      element = await createComponent(
        createFetchHandler(config, {
          putHandler: (body) => {
            capturedPayload = body;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );

      emitTimezoneChange(element, 'Asia/Kathmandu');
      await (element as any).updateComplete;

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));

      expect(capturedPayload).not.toBeNull();
      expect(capturedPayload!.default_timezone).toBe('Asia/Kathmandu');
    });

    it('an env-overridden default_timezone renders read-only with the env badge, in file mode', async () => {
      const config = makeBaseConfig({
        settings_tier: 'file',
        default_timezone: 'Asia/Tokyo',
        env_overrides: ['default_timezone'],
      });
      element = await createComponent(createFetchHandler(config));

      expect(timezonePicker(element)).toBeNull();
      const readOnlyValues = queryAll(element, '.read-only-value');
      const values = readOnlyValues.map((el) => el.textContent?.trim());
      expect(values).toContain('Asia/Tokyo');
    });

    it('shows an inline error for a name isValidTimeZone rejects, without blocking the field', async () => {
      const config = makeBaseConfig({ settings_tier: 'db', default_timezone: '' });
      element = await createComponent(createFetchHandler(config));

      emitTimezoneChange(element, 'Not/A/Timezone');
      await (element as any).updateComplete;

      expect(shadowText(element)).toContain('is not a recognized timezone');
    });

    it('shows no inline error for a valid zone or for the empty (UTC) value', async () => {
      const config = makeBaseConfig({ settings_tier: 'db', default_timezone: '' });
      element = await createComponent(createFetchHandler(config));

      expect(shadowText(element)).not.toContain('is not a recognized timezone');

      emitTimezoneChange(element, 'Asia/Tokyo');
      await (element as any).updateComplete;
      expect(shadowText(element)).not.toContain('is not a recognized timezone');
    });
  });

  // ── Criterion 8: Structured errors ──

  describe('Criterion 8 — Structured error handling', () => {
    it('400 validation_failed renders inline errors', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });

      element = await createComponent(
        createFetchHandler(config, {
          putHandler: () => ({
            status: 400,
            body: {
              error: 'validation_failed',
              errors: {
                access: [{ field: 'admin_emails', message: 'invalid email format' }],
              },
            },
          }),
        })
      );

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));
      await (element as any).updateComplete;

      const errorsEl = query(element, '.validation-errors');
      expect(errorsEl).not.toBeNull();
      const errText = errorsEl!.textContent ?? '';
      expect(errText).toContain('invalid email format');
      expect(errText).toContain('admin_emails');
    });

    it('409 revision_conflict renders conflict banner with Reload button', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });

      element = await createComponent(
        createFetchHandler(config, {
          putHandler: () => ({
            status: 409,
            body: {
              error: 'revision_conflict',
              message: 'Settings have been changed since you loaded this page.',
              conflicted: [{ section: 'access', expected_revision: 3, current_revision: 5 }],
            },
          }),
        })
      );

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));
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

      element = await createComponent(
        createFetchHandler(config, {
          putHandler: () => ({
            status: 422,
            body: {
              error: 'layer0_rejected',
              keys: ['server.mode', 'server.database.driver'],
            },
          }),
        })
      );

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));
      await (element as any).updateComplete;

      const noticeEl = query(element, '.safety-net-notice');
      expect(noticeEl).not.toBeNull();
      const noticeText = noticeEl!.textContent ?? '';
      expect(noticeText).toContain('server.mode');
      expect(noticeText).toContain('server.database.driver');
      expect(noticeText).toContain('bootstrap');

      expect(consoleSpy).toHaveBeenCalledWith(
        expect.stringContaining('layer0_rejected'),
        expect.arrayContaining(['server.mode', 'server.database.driver'])
      );

      consoleSpy.mockRestore();
    });

    it('unknown error falls back to generic error message', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });

      element = await createComponent(
        createFetchHandler(config, {
          putHandler: () => ({
            status: 500,
            body: {
              error: 'some_unknown_error',
              message: 'Something went terribly wrong',
            },
          }),
        })
      );

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));
      await (element as any).updateComplete;

      expect(shadowText(element)).toContain('Something went terribly wrong');
    });

    // handleSaveError's default case must handle more than a flat
    // {error: "<string>", ...} shape. The real Go writeError() helper
    // (pkg/hub/errors.go), used by
    // every plain field-validation 400/422 on this page — including
    // default_timezone's — responds with {error: {code, message, details}}.
    // Before the fix, body.error being an object meant the switch never
    // matched a case, body.message was undefined (nested at
    // body.error.message instead), and the real message was replaced by the
    // generic fallback. This is the shape the handler actually sends, unlike
    // the idealized flat-string shapes the other Criterion 8 tests above use
    // for validation_failed/revision_conflict/layer0_rejected.
    it('a real writeError()-shaped 422 (nested error.message) renders its message, not the generic fallback', async () => {
      const config = makeBaseConfig({ settings_tier: 'db' });

      element = await createComponent(
        createFetchHandler(config, {
          putHandler: () => ({
            status: 422,
            body: {
              error: {
                code: 'validation_error',
                message:
                  'invalid default_timezone "Not/A/Timezone": unknown time zone Not/A/Timezone',
              },
            },
          }),
        })
      );

      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));
      await (element as any).updateComplete;

      const text = shadowText(element);
      expect(text).toContain('invalid default_timezone "Not/A/Timezone"');
      expect(text).not.toContain('An unexpected error occurred');
    });

    // A JSON error body that is not an object (null, a bare string, an
    // array) has no error code or message to read; handleSaveError must
    // treat it like a non-JSON body instead of dereferencing it.
    for (const [label, errBody] of [
      ['null', null],
      ['a bare string', 'upstream proxy error'],
      ['an array', ['boom']],
    ] as const) {
      it(`a non-object JSON error body (${label}) shows the save-failed message`, async () => {
        const config = makeBaseConfig({ settings_tier: 'db' });

        element = await createComponent(
          createFetchHandler(config, {
            putHandler: () => ({ status: 502, body: errBody }),
          })
        );

        const buttons = queryAll(element, 'sl-button[variant="primary"]');
        const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
        (saveBtn as HTMLElement).click();
        await new Promise((resolve) => setTimeout(resolve, 300));
        await (element as any).updateComplete;

        const text = shadowText(element);
        expect(text).toContain('Failed to save settings');
        expect(text).not.toContain('An unexpected error occurred');
      });
    }
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
      const badgeTexts = badges.map((b) => b.textContent ?? '');
      expect(badgeTexts.some((t) => t.includes('deployment configuration'))).toBe(true);
    });
  });

  // ── Regression: clearing fields to blank (ptone/scion#860) ──

  describe('Regression #860 — clearing agent limit fields to blank', () => {
    it('file mode PUT includes zero/empty agent limit fields so backend can clear them', async () => {
      // Start with agent limits set to non-zero values, then verify
      // the PUT payload includes the zeroed values (not undefined).
      const config = makeBaseConfig({
        settings_tier: 'file',
        default_max_turns: 100,
        default_max_model_calls: 200,
        default_max_duration: '2h',
      });
      let capturedPayload: Record<string, unknown> | null = null;

      element = await createComponent(
        createFetchHandler(config, {
          putHandler: (body) => {
            capturedPayload = body;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );

      // Simulate clearing the fields: set the internal state to zero/empty.
      // In the real UI, the user would clear the input fields.
      (element as any).defaultMaxTurns = 0;
      (element as any).defaultMaxModelCalls = 0;
      (element as any).defaultMaxDuration = '';
      await (element as any).updateComplete;

      // Trigger save.
      const buttons = queryAll(element, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      expect(saveBtn).not.toBeNull();
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));
      await (element as any).updateComplete;

      expect(capturedPayload).not.toBeNull();

      // The key assertion: zero/empty values MUST be present in the payload
      // (not undefined), so the backend receives them and can delete the keys.
      expect(capturedPayload!.default_max_turns).toBe(0);
      expect(capturedPayload!.default_max_model_calls).toBe(0);
      expect(capturedPayload!.default_max_duration).toBe('');
    });
  });

  describe('Regression ptone/scion#2535 — clearing string fields in file mode', () => {
    const clearable: Array<[string, string]> = [
      ['active_profile', 'activeProfile'],
      ['default_template', 'defaultTemplate'],
      // default_harness_auth and the agent role defaults are not sent at all
      // until ptone/scion#3067 is fixed (see the #3067 describe block).
      ['image_registry', 'imageRegistry'],
      ['workspace_path', 'workspacePath'],
      ['default_runtime_broker', 'defaultRuntimeBroker'],
    ];

    it('buildFilePayload sends cleared string fields as "" (not omitted)', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig({ settings_tier: 'file' }))
      );
      const el = element as any;
      for (const [, prop] of clearable) el[prop] = '';
      el.harnessConfigSelection = '';
      el.customHarnessConfig = '';

      const payload = el.buildFilePayload() as Record<string, unknown>;
      for (const [key] of clearable) {
        expect(payload, key).toHaveProperty(key, '');
      }
      expect(payload).toHaveProperty('default_harness_config', '');
    });

    it('buildFilePayload still sends non-empty string values', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig({ settings_tier: 'file' }))
      );
      const el = element as any;
      for (const [, prop] of clearable) el[prop] = `v-${prop}`;
      el.harnessConfigSelection = '__other__';
      el.customHarnessConfig = 'x';

      const payload = el.buildFilePayload() as Record<string, unknown>;
      for (const [key, prop] of clearable) {
        expect(payload, key).toHaveProperty(key, `v-${prop}`);
      }
      expect(payload).toHaveProperty('default_harness_config', 'x');
    });

    it('buildFilePayload sends GCP identity defaults when set', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig({ settings_tier: 'file' }))
      );
      const el = element as any;
      el.defaultGCPIdentityMode = 'assign';
      el.defaultGCPIdentitySAID = 'sa-123';

      const payload = el.buildFilePayload() as Record<string, unknown>;
      expect(payload).toHaveProperty('default_gcp_identity_mode', 'assign');
      expect(payload).toHaveProperty('default_gcp_identity_service_account_id', 'sa-123');
    });

    it('buildFilePayload sends cleared GCP identity defaults as ""', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig({ settings_tier: 'file' }))
      );
      const el = element as any;
      el.defaultGCPIdentityMode = '';
      el.defaultGCPIdentitySAID = '';

      const payload = el.buildFilePayload() as Record<string, unknown>;
      expect(payload).toHaveProperty('default_gcp_identity_mode', '');
      expect(payload).toHaveProperty('default_gcp_identity_service_account_id', '');
    });

    it('buildFilePayload clears the GCP service account when mode is not assign', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig({ settings_tier: 'file' }))
      );
      const el = element as any;
      el.defaultGCPIdentityMode = 'block';
      el.defaultGCPIdentitySAID = 'stale-sa';

      const payload = el.buildFilePayload() as Record<string, unknown>;
      expect(payload).toHaveProperty('default_gcp_identity_mode', 'block');
      expect(payload).toHaveProperty('default_gcp_identity_service_account_id', '');
    });

    it('buildFilePayload clears the GCP service account when mode is empty', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig({ settings_tier: 'file' }))
      );
      const el = element as any;
      el.defaultGCPIdentityMode = '';
      el.defaultGCPIdentitySAID = 'stale-sa';

      const payload = el.buildFilePayload() as Record<string, unknown>;
      expect(payload).toHaveProperty('default_gcp_identity_mode', '');
      expect(payload).toHaveProperty('default_gcp_identity_service_account_id', '');
    });

    it('buildFilePayload omits an env-pinned default_gcp_identity_mode', async () => {
      element = await createComponent(
        createFetchHandler(
          makeBaseConfig({
            settings_tier: 'file',
            env_overrides: ['default_gcp_identity_mode'],
          })
        )
      );
      const el = element as any;
      el.defaultGCPIdentityMode = 'assign';
      el.defaultGCPIdentitySAID = 'sa-123';

      const payload = el.buildFilePayload() as Record<string, unknown>;
      expect(payload).not.toHaveProperty('default_gcp_identity_mode');
      expect(payload).toHaveProperty('default_gcp_identity_service_account_id', 'sa-123');
    });

    // ptone/scion#2720: with the mode env-pinned, the form mode holds the
    // settings-file value, not the effective one, so it must not drive
    // clearing of the account. An unchanged account is left out of the
    // payload, so the server neither clears nor re-checks it.
    const pinnedFileConfig = (formMode: string, said: string) =>
      makeBaseConfig({
        settings_tier: 'file',
        env_overrides: ['default_gcp_identity_mode'],
        default_gcp_identity_mode: formMode,
        default_gcp_identity_service_account_id: said,
      });

    it.each(['', 'block', 'passthrough', 'assign'])(
      'buildFilePayload omits an unchanged GCP service account when the mode is env-pinned (form mode=%j)',
      async (formMode) => {
        element = await createComponent(createFetchHandler(pinnedFileConfig(formMode, 'sa-123')));
        const el = element as any;
        expect(el.defaultGCPIdentitySAID).toBe('sa-123');

        const payload = el.buildFilePayload() as Record<string, unknown>;
        expect(payload).not.toHaveProperty('default_gcp_identity_mode');
        expect(payload).not.toHaveProperty('default_gcp_identity_service_account_id');
      }
    );

    it.each([
      ['changed', 'sa-456'],
      ['cleared', ''],
    ])(
      'buildFilePayload sends an edited GCP service account when the mode is env-pinned (%s)',
      async (_label, edited) => {
        element = await createComponent(createFetchHandler(pinnedFileConfig('assign', 'sa-123')));
        const el = element as any;
        el.defaultGCPIdentitySAID = edited;

        const payload = el.buildFilePayload() as Record<string, unknown>;
        expect(payload).not.toHaveProperty('default_gcp_identity_mode');
        expect(payload).toHaveProperty('default_gcp_identity_service_account_id', edited);
      }
    );

    it('buildFilePayload compares the GCP service account with the latest load when the mode is env-pinned', async () => {
      element = await createComponent(createFetchHandler(pinnedFileConfig('assign', 'sa-123')));
      const el = element as any;
      expect(el.defaultGCPIdentitySAID).toBe('sa-123');

      // Reload (as after a save) with a different stored account.
      vi.stubGlobal('fetch', vi.fn(createFetchHandler(pinnedFileConfig('assign', 'sa-789'))));
      await el.loadConfig();
      await el.updateComplete;
      expect(el.defaultGCPIdentitySAID).toBe('sa-789');
      expect(el.readOnlyReason('default_gcp_identity_mode')).not.toBeNull();

      // Unchanged since the reload: omitted.
      let payload = el.buildFilePayload() as Record<string, unknown>;
      expect(payload).not.toHaveProperty('default_gcp_identity_mode');
      expect(payload).not.toHaveProperty('default_gcp_identity_service_account_id');

      // Back to the first-load value: now an edit, so it is sent.
      el.defaultGCPIdentitySAID = 'sa-123';
      payload = el.buildFilePayload() as Record<string, unknown>;
      expect(payload).not.toHaveProperty('default_gcp_identity_mode');
      expect(payload).toHaveProperty('default_gcp_identity_service_account_id', 'sa-123');
    });

    it.each([
      ['assign', 'sa-123'],
      ['block', ''],
    ])(
      'buildFilePayload sends the loaded GCP service account when the mode is editable (mode=%j)',
      async (mode, expected) => {
        element = await createComponent(
          createFetchHandler(
            makeBaseConfig({
              settings_tier: 'file',
              default_gcp_identity_mode: mode,
              default_gcp_identity_service_account_id: 'sa-123',
            })
          )
        );
        const el = element as any;

        const payload = el.buildFilePayload() as Record<string, unknown>;
        expect(payload).toHaveProperty('default_gcp_identity_mode', mode);
        expect(payload).toHaveProperty('default_gcp_identity_service_account_id', expected);
      }
    );

    // The db-tier test schema does not list the GCP identity keys, so the
    // tests set the Layer-1 key set the page uses to decide editability.
    const GCP_KEYS = ['default_gcp_identity_mode', 'default_gcp_identity_service_account_id'];

    it('buildLayer1Payload sends the GCP service account in assign mode', async () => {
      element = await createComponent(createFetchHandler(makeBaseConfig({ settings_tier: 'db' })));
      const el = element as any;
      el.layer1Keys = new Set(GCP_KEYS);
      el.defaultGCPIdentityMode = 'assign';
      el.defaultGCPIdentitySAID = 'sa-123';

      const payload = el.buildLayer1Payload() as Record<string, unknown>;
      expect(payload).toHaveProperty('default_gcp_identity_mode', 'assign');
      expect(payload).toHaveProperty('default_gcp_identity_service_account_id', 'sa-123');
    });

    it('buildLayer1Payload clears the GCP service account when mode is not assign', async () => {
      element = await createComponent(createFetchHandler(makeBaseConfig({ settings_tier: 'db' })));
      const el = element as any;
      el.layer1Keys = new Set(GCP_KEYS);
      el.defaultGCPIdentityMode = 'block';
      el.defaultGCPIdentitySAID = 'stale-sa';

      const payload = el.buildLayer1Payload() as Record<string, unknown>;
      expect(payload).toHaveProperty('default_gcp_identity_mode', 'block');
      expect(payload).toHaveProperty('default_gcp_identity_service_account_id', '');
    });

    // In the db tier env vars do not lock Layer-1 fields. Both GCP keys
    // are in one settings section, so they are Layer-1 together or
    // deployment-managed together; the page never reaches the read-only
    // mode branch of the account helper there. When both are locked,
    // neither key is sent.
    it('buildLayer1Payload omits both GCP keys when they are not Layer-1', async () => {
      element = await createComponent(createFetchHandler(makeBaseConfig({ settings_tier: 'db' })));
      const el = element as any;
      el.layer1Keys = new Set();
      el.defaultGCPIdentityMode = 'assign';
      el.defaultGCPIdentitySAID = 'sa-123';

      const payload = el.buildLayer1Payload() as Record<string, unknown>;
      expect(payload).not.toHaveProperty('default_gcp_identity_mode');
      expect(payload).not.toHaveProperty('default_gcp_identity_service_account_id');
    });
  });

  // ── Cross-project messaging (D1) ──

  describe('File mode server sections keep omitted fields (ptone/scion#2938)', () => {
    // The file-mode PUT deep-merges each server section, so an omitted
    // field keeps its stored value; a field the form shows must be sent as
    // an explicit empty value to be cleared.
    it('buildFilePayload sends cleared server fields as explicit empties', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig({ settings_tier: 'file' }))
      );
      const el = element as any;
      el.logLevel = '';
      el.hubPort = 0;
      el.hubHost = '';
      el.hubPublicUrl = '';
      el.brokerHost = '';
      el.brokerPort = 0;
      el.dbDriver = '';
      el.dbUrl = '';
      el.authDevToken = '';
      el.storageBucket = '';
      el.secretsBackend = '';
      el.secretsGCPProjectId = '';

      const server = (el.buildFilePayload() as Record<string, any>).server;
      expect(server.log_level).toBe('');
      expect(server.hub).toMatchObject({ port: 0, host: '', public_url: '' });
      expect(server.broker).toMatchObject({ port: 0, host: '' });
      expect(server.database).toEqual({ driver: '', url: '' });
      expect(server.auth).toHaveProperty('dev_token', '');
      expect(server.storage).toHaveProperty('bucket', '');
      expect(server.secrets).toMatchObject({ backend: '', gcp_project_id: '' });
    });

    it('buildFilePayload leaves out masked credentials so the stored values are kept', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig({ settings_tier: 'file' }))
      );
      const el = element as any;
      el.dbUrl = '********';
      el.authDevToken = '********';

      const server = (el.buildFilePayload() as Record<string, any>).server;
      expect(server.database).not.toHaveProperty('url');
      expect(server.auth).not.toHaveProperty('dev_token');
    });
  });

  describe('Cross-project messaging section', () => {
    it('renders cross-project messaging section in hub server tab', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig(), {
          messagingResponse: {
            cross_project_messaging_enabled: false,
            revision: 1,
          },
        })
      );

      const text = shadowText(element);
      expect(text).toContain('Cross-Project Agent Messaging');
    });

    it('calls the messaging API on connect', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig(), {
          messagingResponse: {
            cross_project_messaging_enabled: true,
            revision: 3,
          },
        })
      );

      expect(vi.mocked(fetch)).toHaveBeenCalledWith(
        expect.stringContaining('/api/v1/admin/messaging'),
        expect.any(Object)
      );
    });

    it('shows the revision number', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig(), {
          messagingResponse: {
            cross_project_messaging_enabled: false,
            revision: 7,
          },
        })
      );

      const text = shadowText(element);
      expect(text).toContain('Revision: 7');
    });

    it('sends CAS revision on save', async () => {
      let capturedPayload: Record<string, unknown> | null = null;
      element = await createComponent(
        createFetchHandler(makeBaseConfig(), {
          messagingResponse: {
            cross_project_messaging_enabled: false,
            revision: 5,
          },
          putHandler: (body) => {
            capturedPayload = body;
            return {
              status: 200,
              body: { cross_project_messaging_enabled: true, revision: 6 },
            };
          },
        })
      );

      // Wait for messaging settings to load
      await new Promise((resolve) => setTimeout(resolve, 300));
      await (element as any).updateComplete;

      // Trigger save by calling the method directly
      await (element as any).saveCrossProjectMessaging(true);
      await (element as any).updateComplete;

      expect(capturedPayload).not.toBeNull();
      expect(capturedPayload!.expected_revision).toBe(5);
      expect(capturedPayload!.cross_project_messaging_enabled).toBe(true);
    });

    it('handles 409 conflict by reloading', async () => {
      let putCallCount = 0;
      element = await createComponent(
        createFetchHandler(makeBaseConfig(), {
          messagingResponse: {
            cross_project_messaging_enabled: false,
            revision: 5,
          },
          putHandler: () => {
            putCallCount++;
            return {
              status: 409,
              body: { error: 'conflict' },
            };
          },
        })
      );

      await new Promise((resolve) => setTimeout(resolve, 300));
      await (element as any).updateComplete;

      await (element as any).saveCrossProjectMessaging(true);
      await new Promise((resolve) => setTimeout(resolve, 300));
      await (element as any).updateComplete;

      expect(putCallCount).toBe(1);
      // Error message should indicate conflict
      expect((element as any).crossProjectMessagingError).toContain('another administrator');
    });
  });

  // ── Default User Role help text (default_user_role, design §5.F) ──

  describe('Default User Role help text', () => {
    function helpText(el: HTMLElement): string {
      return (query(el, '.default-user-role-help')?.textContent ?? '').replace(/\s+/g, ' ').trim();
    }

    it('renders the final explanation of the setting', async () => {
      element = await createComponent(createFetchHandler(makeBaseConfig()));

      const text = helpText(element);
      expect(text).toContain('Default role for new users.');
      expect(text).toContain(
        'Applies when a user first signs in, including invited and allow-listed users ' +
          '(their role is assigned at first sign-in, not when the invite is created).'
      );
      expect(text).toContain('Changing it does not affect users who have already signed in.');
      expect(text).not.toContain('invite, or allow-list entry');
      expect(text).toContain('Users listed in Admin Emails are always admins.');
      expect(text).toContain(
        'Member: can create projects, and works in any project they are added to.'
      );
      expect(text).toContain(
        'Viewer: the same as Member, but cannot create projects (including cloning). ' +
          'Viewers can still be added to projects and work there according to their project role.'
      );
      expect(text).toContain(
        'This is also the role given to an admin who is removed from Admin Emails.'
      );
      expect(text).toContain("Change an individual user's role on Admin > Users.");
    });

    it('links to Admin > Users', async () => {
      element = await createComponent(createFetchHandler(makeBaseConfig()));

      expect(query(element, '.default-user-role-help a[href="/admin/users"]')).not.toBeNull();
    });

    it('no longer shows the old one-line hint', async () => {
      element = await createComponent(createFetchHandler(makeBaseConfig()));

      expect(shadowText(element)).not.toContain(
        'Role assigned to new users who are not in the admin emails list.'
      );
    });
  });

  // ── Agent Secrets card (design ptone/scion#2291 §8, §10 test 10) ──

  describe('Agent Secrets card', () => {
    function agentSecretsSwitch(el: HTMLElement): HTMLElement | undefined {
      return queryAll(el, 'sl-switch').find((s) =>
        (s.textContent ?? '').includes('Restrict agent-written secrets to profile scope')
      ) as HTMLElement | undefined;
    }

    it('shows the card with its explanatory hint', async () => {
      element = await createComponent(createFetchHandler(makeBaseConfig()));

      expect(shadowText(element)).toContain('Agent Secrets');
      expect(shadowText(element)).toContain(
        'Project-scope writes from agents are rejected. Existing project secrets are'
      );
      expect(shadowText(element)).toContain('not removed. Users can still manage project secrets.');
    });

    it('switch loads unchecked when agent_secrets is absent', async () => {
      element = await createComponent(createFetchHandler(makeBaseConfig()));

      const sw = agentSecretsSwitch(element);
      expect(sw).not.toBeUndefined();
      expect(sw!.hasAttribute('checked')).toBe(false);
    });

    it('switch loads checked when agent_secrets.user_scope_only is true', async () => {
      const config = makeBaseConfig({ agent_secrets: { user_scope_only: true } });
      element = await createComponent(createFetchHandler(config));

      const sw = agentSecretsSwitch(element);
      expect(sw).not.toBeUndefined();
      expect(sw!.hasAttribute('checked')).toBe(true);
    });

    // Round-1 review R2: parameterised over both settings tiers, since
    // 'file' alone only exercises buildFilePayload() — settingsTier === 'db'
    // is what routes save through the separate buildLayer1Payload() builder
    // (admin-server-config.ts's handleSave: `this.settingsTier === 'db' ?
    // this.buildLayer1Payload() : this.buildFilePayload()`).
    it.each(['file', 'db'] as const)(
      'both payload builders send agent_secrets.user_scope_only on save (settings_tier=%s)',
      async (settingsTier) => {
        let capturedPayload: Record<string, unknown> | null = null;
        const config = makeBaseConfig({
          settings_tier: settingsTier,
          agent_secrets: { user_scope_only: true },
        });

        element = await createComponent(
          createFetchHandler(config, {
            putHandler: (body) => {
              capturedPayload = body;
              return { status: 200, body: { reload: { applied: [] } } };
            },
          })
        );

        const buttons = queryAll(element, 'sl-button[variant="primary"]');
        const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
        (saveBtn as HTMLElement).click();
        await new Promise((resolve) => setTimeout(resolve, 300));

        expect(capturedPayload).not.toBeNull();
        const agentSecrets = capturedPayload!.agent_secrets as Record<string, unknown> | undefined;
        expect(agentSecrets?.user_scope_only).toBe(true);
      }
    );

    it('env-overridden agent_secrets.user_scope_only renders read-only with env badge', async () => {
      const config = makeBaseConfig({
        settings_tier: 'file',
        agent_secrets: { user_scope_only: true },
        env_overrides: ['agent_secrets.user_scope_only'],
      });
      element = await createComponent(createFetchHandler(config));

      const badges = queryAll(element, '.read-only-badge');
      const badgeTexts = badges.map((b) => b.textContent ?? '');
      expect(badgeTexts.some((t) => t.includes('environment variable'))).toBe(true);

      // The switch itself must not render while the field is env-pinned.
      expect(agentSecretsSwitch(element)).toBeUndefined();
    });
  });

  // ── Experiments tab (ptone/scion#2217) ──

  describe('Experiments tab', () => {
    function showTab(el: HTMLElement, name: string): void {
      const tabGroup = query(el, 'sl-tab-group');
      tabGroup?.dispatchEvent(new CustomEvent('sl-tab-show', { detail: { name } }));
    }

    // The top-level Save & Reload / Reset bar is a direct child of the
    // shadow root; the GitHub App tab has its own unrelated ".actions" div
    // nested inside its (always-rendered, visibility-toggled) panel, so a
    // plain `.actions` query would match both.
    function topLevelActions(el: HTMLElement): Element | null {
      const candidates = el.shadowRoot?.querySelectorAll('.actions') ?? [];
      return Array.from(candidates).find((c) => c.parentNode === el.shadowRoot) ?? null;
    }

    it('appears last in the tab nav', async () => {
      element = await createComponent(createFetchHandler(makeBaseConfig()));
      // Scope to the outer tab group's direct children — the Runtimes &
      // Profiles panel nests its own sl-tab-group for agent-defaults, whose
      // tabs also carry slot="nav" but belong to a different tab group.
      const outerTabGroup = query(element, 'sl-tab-group');
      const tabs = Array.from(outerTabGroup?.querySelectorAll(':scope > sl-tab[slot="nav"]') ?? []);
      expect(tabs[tabs.length - 1].getAttribute('panel')).toBe('experiments');
    });

    it('renders <scion-admin-experiments> in its panel', async () => {
      element = await createComponent(createFetchHandler(makeBaseConfig()));
      expect(
        query(element, 'sl-tab-panel[name="experiments"] scion-admin-experiments')
      ).not.toBeNull();
    });

    it('hides the actions bar and the harness-config error message while the Experiments tab is active', async () => {
      element = await createComponent(createFetchHandler(makeBaseConfig()));

      // Force the harness-config error message's condition on, as if the
      // Runtimes & Profiles tab had an invalid JSON entry, so we can prove
      // it is specifically hidden on the Experiments tab, not just absent.
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (element as any).harnessConfigErrors = { profileA: 'invalid json' };
      element.requestUpdate();
      await element.updateComplete;

      expect(topLevelActions(element)).not.toBeNull();
      expect(shadowText(element)).toContain('Cannot save');

      showTab(element, 'experiments');
      await element.updateComplete;

      expect(topLevelActions(element)).toBeNull();
      expect(shadowText(element)).not.toContain('Cannot save');

      showTab(element, 'general');
      await element.updateComplete;

      expect(topLevelActions(element)).not.toBeNull();
      expect(shadowText(element)).toContain('Cannot save');
    });

    it('sets .active on <scion-admin-experiments> only while its tab is shown', async () => {
      element = await createComponent(createFetchHandler(makeBaseConfig()));
      const experimentsEl = query(element, 'scion-admin-experiments') as HTMLElement & {
        active: boolean;
      };
      expect(experimentsEl.active).toBe(false);

      showTab(element, 'experiments');
      await element.updateComplete;
      expect(experimentsEl.active).toBe(true);

      showTab(element, 'general');
      await element.updateComplete;
      expect(experimentsEl.active).toBe(false);
    });
  });

  describe('safe_to_evict on runtimes and profiles', () => {
    function steConfig() {
      return makeBaseConfig({
        runtimes: { k8s: { type: 'kubernetes', safe_to_evict: false } },
        profiles: {
          gke: { runtime: 'k8s' },
          evictable: { runtime: 'k8s', safe_to_evict: true },
        },
      });
    }

    async function saveAndCapture(el: HTMLElement): Promise<void> {
      await (el as any).updateComplete;
      const buttons = queryAll(el, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));
    }

    it('shows false, true and unset distinctly', async () => {
      element = await createComponent(createFetchHandler(steConfig()));
      const values = queryAll(element, 'sl-select.safe-to-evict').map((s) =>
        s.getAttribute('value')
      );
      // runtime k8s, then profiles gke and evictable
      expect(values).toEqual(['false', '', 'true']);
    });

    it('labels the select as ignored on non-Kubernetes runtimes', async () => {
      element = await createComponent(
        createFetchHandler(
          makeBaseConfig({
            runtimes: {
              k8s: { type: 'kubernetes' },
              docker: { type: 'docker', safe_to_evict: false },
              remote: {},
            },
            profiles: {
              gke: { runtime: 'k8s' },
              local: { runtime: 'docker' },
              far: { runtime: 'remote' },
            },
          })
        )
      );
      await (element as any).updateComplete;
      // the docker runtime card and the profile that uses it
      expect(queryAll(element, '.safe-to-evict-ignored').length).toBe(2);
    });

    it('sends booleans, and clearing removes the key', async () => {
      let capturedPayload: Record<string, any> | null = null;
      element = await createComponent(
        createFetchHandler(steConfig(), {
          putHandler: (body) => {
            if ('profiles' in body) capturedPayload = body;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );
      const [runtimeSel, gkeSel, evictableSel] = queryAll(
        element,
        'sl-select.safe-to-evict'
      ) as (HTMLElement & { value: string })[];
      gkeSel.value = 'false';
      gkeSel.dispatchEvent(new Event('sl-change'));
      evictableSel.value = '';
      evictableSel.dispatchEvent(new Event('sl-change'));
      expect(runtimeSel).toBeDefined();
      await saveAndCapture(element);

      expect(capturedPayload).not.toBeNull();
      expect(capturedPayload!.runtimes.k8s.safe_to_evict).toBe(false);
      expect(capturedPayload!.profiles.gke.safe_to_evict).toBe(false);
      expect('safe_to_evict' in capturedPayload!.profiles.evictable).toBe(false);
    });
  });

  // ── Tier-based maintenance dispatch (fork issue: rebuild-server always used) ──

  describe('Tier-based maintenance dispatch', () => {
    it('defaults to source tier and dispatches rebuild-server/run', async () => {
      let ranPath: string | null = null;
      element = await createComponent(
        createFetchHandler(makeBaseConfig(), {
          checkUpdatesResponse: { tier: 'source', update_available: true, commits_behind: 2 },
          onOperationRun: (path) => {
            ranPath = path;
          },
        })
      );

      await (element as any).checkForUpdates();
      await (element as any).updateComplete;
      expect((element as any).deploymentTier).toBe('source');

      await (element as any).triggerUpdate();
      await (element as any).updateComplete;

      expect(ranPath).not.toBeNull();
      expect(ranPath).toContain('/operations/rebuild-server/run');
    });

    it('sets deploymentTier to binary and dispatches update-binary/run', async () => {
      let ranPath: string | null = null;
      element = await createComponent(
        createFetchHandler(makeBaseConfig(), {
          checkUpdatesResponse: {
            tier: 'binary',
            update_available: true,
            current_version: '1.0.0',
            latest_version: '1.1.0',
            channel: 'stable',
          },
          onOperationRun: (path) => {
            ranPath = path;
          },
        })
      );

      await (element as any).checkForUpdates();
      await (element as any).updateComplete;
      expect((element as any).deploymentTier).toBe('binary');

      await (element as any).triggerUpdate();
      await (element as any).updateComplete;

      expect(ranPath).not.toBeNull();
      expect(ranPath).toContain('/operations/update-binary/run');
    });

    it('renders binary-tier version info in the update banner', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig(), {
          checkUpdatesResponse: {
            tier: 'binary',
            update_available: true,
            current_version: '1.0.0',
            latest_version: '1.1.0',
            channel: 'stable',
            release_url: 'https://example.com/releases/1.1.0',
          },
        })
      );

      await (element as any).checkForUpdates();
      await (element as any).updateComplete;

      const text = shadowText(element);
      expect(text).toContain('1.0.0');
      expect(text).toContain('1.1.0');
      expect(text).toContain('stable');
    });

    it('omits the commit count in the source-tier banner when commits_behind is missing', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig(), {
          checkUpdatesResponse: { tier: 'source', update_available: true },
        })
      );

      await (element as any).checkForUpdates();
      await (element as any).updateComplete;

      const text = shadowText(element);
      expect(text).toContain('Update');
      expect(text).not.toContain('undefined');
      expect(text).not.toMatch(/new\s+commit/);
    });

    it('renders the commit count in the source-tier banner when commits_behind is set', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig(), {
          checkUpdatesResponse: { tier: 'source', update_available: true, commits_behind: 3 },
        })
      );

      await (element as any).checkForUpdates();
      await (element as any).updateComplete;

      expect(shadowText(element)).toMatch(/3\s+new\s+commits/);
    });

    it('shows binary-tier confirm dialog text', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig(), {
          checkUpdatesResponse: {
            tier: 'binary',
            update_available: true,
            current_version: '1.0.0',
            latest_version: '1.1.0',
            channel: 'stable',
          },
        })
      );

      await (element as any).checkForUpdates();
      (element as any).showUpdateConfirm = true;
      await (element as any).updateComplete;

      const text = shadowText(element);
      expect(text).toContain('download the latest release binary');
      expect(text).not.toContain('pull the latest code');
    });

    it('shows source-tier confirm dialog text', async () => {
      element = await createComponent(
        createFetchHandler(makeBaseConfig(), {
          checkUpdatesResponse: { tier: 'source', update_available: true, commits_behind: 3 },
        })
      );

      await (element as any).checkForUpdates();
      (element as any).showUpdateConfirm = true;
      await (element as any).updateComplete;

      const text = shadowText(element);
      expect(text).toContain('pull the latest code');
      expect(text).not.toContain('download the latest release binary');
    });
  });

  describe('shared_dir_storage_backend on runtimes and profiles', () => {
    function sdsConfig() {
      // File mode (settings.yaml is authoritative), where every section
      // without an env override is editable.
      return makeBaseConfig({
        runtimes: { k8s: { type: 'kubernetes', shared_dir_storage_backend: 'nfs' } },
        profiles: {
          gke: { runtime: 'k8s', shared_dir_storage_backend: 'nfs', timezone: 'UTC' },
        },
      });
    }

    async function saveAndCapture(el: HTMLElement): Promise<void> {
      await (el as any).updateComplete;
      const buttons = queryAll(el, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));
    }

    it('shows the current value on the runtime and profile cards', async () => {
      element = await createComponent(createFetchHandler(sdsConfig()));
      const selects = queryAll(element, 'sl-select.shared-dir-storage-backend');
      expect(selects.length).toBe(2);
      for (const sel of selects) {
        expect(sel.getAttribute('value')).toBe('nfs');
      }
    });

    it('editing another profile field keeps the key in the PUT payload', async () => {
      let capturedPayload: Record<string, any> | null = null;
      element = await createComponent(
        createFetchHandler(sdsConfig(), {
          putHandler: (body) => {
            if ('profiles' in body) capturedPayload = body;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );

      const registry = query(element, 'sl-input[placeholder="Override image registry"]') as
        | (HTMLElement & { value: string })
        | null;
      expect(registry).not.toBeNull();
      registry!.value = 'registry.example.com/team';
      registry!.dispatchEvent(new Event('sl-input'));
      await saveAndCapture(element);

      expect(capturedPayload).not.toBeNull();
      const gke = capturedPayload!.profiles.gke;
      expect(gke.image_registry).toBe('registry.example.com/team');
      expect(gke.shared_dir_storage_backend).toBe('nfs');
      expect(gke.timezone).toBe('UTC');
      expect(capturedPayload!.runtimes.k8s.shared_dir_storage_backend).toBe('nfs');
    });

    it('changing and clearing the select updates the payload', async () => {
      let capturedPayload: Record<string, any> | null = null;
      element = await createComponent(
        createFetchHandler(sdsConfig(), {
          putHandler: (body) => {
            if ('profiles' in body) capturedPayload = body;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );

      const [runtimeSel, profileSel] = queryAll(
        element,
        'sl-select.shared-dir-storage-backend'
      ) as (HTMLElement & { value: string })[];
      runtimeSel.value = '';
      runtimeSel.dispatchEvent(new Event('sl-change'));
      profileSel.value = 'local';
      profileSel.dispatchEvent(new Event('sl-change'));
      await saveAndCapture(element);

      expect(capturedPayload).not.toBeNull();
      expect(capturedPayload!.profiles.gke.shared_dir_storage_backend).toBe('local');
      expect('shared_dir_storage_backend' in capturedPayload!.runtimes.k8s).toBe(false);
    });
  });

  describe('Cloud Run runtime editor field names (ptone/scion#3475)', () => {
    function cloudRunConfig(
      tier: Record<string, unknown>,
      runtime: Record<string, unknown> = {
        type: 'cloudrun',
        cloudrun: { project_id: 'proj-a', location: 'us-central1' },
      }
    ) {
      return makeBaseConfig({ ...tier, runtimes: { crun: runtime } });
    }

    function cloudRunInputs(el: HTMLElement): {
      project: HTMLElement & { value: string };
      location: HTMLElement & { value: string };
    } {
      const fields = queryAll(el, '.form-field');
      const byLabel = (label: string) => {
        const field = fields.find((f) => f.querySelector('label')?.textContent?.trim() === label);
        return field?.querySelector('sl-input') as HTMLElement & { value: string };
      };
      return { project: byLabel('GCP Project'), location: byLabel('GCP Region') };
    }

    async function saveAndCapture(el: HTMLElement): Promise<void> {
      await (el as any).updateComplete;
      const buttons = queryAll(el, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));
    }

    for (const [mode, tier] of [
      ['file', {}],
      ['db', { settings_tier: 'db' }],
    ] as const) {
      it(`${mode} mode: reads and sends project_id and location`, async () => {
        let capturedPayload: Record<string, any> | null = null;
        element = await createComponent(
          createFetchHandler(cloudRunConfig(tier), {
            schemaResponse: {
              sections: {
                ...SCHEMA_RESPONSE.sections,
                runtimes: { koanf_paths: ['runtimes'] },
              },
            },
            putHandler: (body) => {
              if ('runtimes' in body) capturedPayload = body;
              return { status: 200, body: { reload: { applied: [] } } };
            },
          })
        );

        const { project, location } = cloudRunInputs(element);
        expect(project).toBeDefined();
        expect(location).toBeDefined();
        expect(project.getAttribute('value')).toBe('proj-a');
        expect(location.getAttribute('value')).toBe('us-central1');

        project.value = 'proj-b';
        project.dispatchEvent(new Event('sl-input'));
        location.value = 'europe-west1';
        location.dispatchEvent(new Event('sl-input'));
        await saveAndCapture(element);

        expect(capturedPayload).not.toBeNull();
        expect(capturedPayload!.runtimes.crun.cloudrun).toEqual({
          project_id: 'proj-b',
          location: 'europe-west1',
        });
        expect('cloudrun_instances' in capturedPayload!.runtimes.crun).toBe(false);
      });

      it(`${mode} mode: cloudrun-instances reads and sends the cloudrun_instances block`, async () => {
        let capturedPayload: Record<string, any> | null = null;
        element = await createComponent(
          createFetchHandler(
            cloudRunConfig(tier, {
              type: 'cloudrun-instances',
              cloudrun_instances: { project_id: 'proj-a', region: 'us-central1' },
            }),
            {
              schemaResponse: {
                sections: {
                  ...SCHEMA_RESPONSE.sections,
                  runtimes: { koanf_paths: ['runtimes'] },
                },
              },
              putHandler: (body) => {
                if ('runtimes' in body) capturedPayload = body;
                return { status: 200, body: { reload: { applied: [] } } };
              },
            }
          )
        );

        const { project, location } = cloudRunInputs(element);
        expect(project).toBeDefined();
        expect(location).toBeDefined();
        expect(project.getAttribute('value')).toBe('proj-a');
        expect(location.getAttribute('value')).toBe('us-central1');

        project.value = 'proj-b';
        project.dispatchEvent(new Event('sl-input'));
        location.value = 'europe-west1';
        location.dispatchEvent(new Event('sl-input'));
        await saveAndCapture(element);

        expect(capturedPayload).not.toBeNull();
        expect(capturedPayload!.runtimes.crun.cloudrun_instances).toEqual({
          project_id: 'proj-b',
          region: 'europe-west1',
        });
        expect('cloudrun' in capturedPayload!.runtimes.crun).toBe(false);
      });

      for (const [from, to, fromBlock, toBlock, toFields] of [
        [
          'cloudrun',
          'cloudrun-instances',
          'cloudrun',
          'cloudrun_instances',
          { project_id: 'proj-b', region: 'europe-west1' },
        ],
        [
          'cloudrun-instances',
          'cloudrun',
          'cloudrun_instances',
          'cloudrun',
          { project_id: 'proj-b', location: 'europe-west1' },
        ],
      ] as const) {
        it(`${mode} mode: switching ${from} to ${to} drops the ${fromBlock} block`, async () => {
          let capturedPayload: Record<string, any> | null = null;
          const fromFields =
            from === 'cloudrun'
              ? { project_id: 'proj-a', location: 'us-central1' }
              : { project_id: 'proj-a', region: 'us-central1' };
          element = await createComponent(
            createFetchHandler(
              cloudRunConfig(tier, {
                type: from,
                sync: 'tar',
                env: { FOO: 'bar' },
                [fromBlock]: fromFields,
              }),
              {
                schemaResponse: {
                  sections: {
                    ...SCHEMA_RESPONSE.sections,
                    runtimes: { koanf_paths: ['runtimes'] },
                  },
                },
                putHandler: (body) => {
                  if ('runtimes' in body) capturedPayload = body;
                  return { status: 200, body: { reload: { applied: [] } } };
                },
              }
            )
          );

          // Finds the runtime type select by its cloudrun-instances option;
          // update this if another runtime-type select appears on the page.
          const typeSelect = queryAll(element, 'sl-select').find((s) =>
            s.querySelector('sl-option[value="cloudrun-instances"]')
          ) as HTMLElement & { value: string };
          expect(typeSelect).toBeDefined();
          typeSelect.value = to;
          typeSelect.dispatchEvent(new Event('sl-change'));
          await (element as any).updateComplete;

          const { project, location } = cloudRunInputs(element);
          expect(project.getAttribute('value')).toBe('');
          expect(location.getAttribute('value')).toBe('');
          project.value = 'proj-b';
          project.dispatchEvent(new Event('sl-input'));
          location.value = 'europe-west1';
          location.dispatchEvent(new Event('sl-input'));
          await saveAndCapture(element);

          expect(capturedPayload).not.toBeNull();
          const crun = capturedPayload!.runtimes.crun;
          expect(crun.type).toBe(to);
          expect(fromBlock in crun).toBe(false);
          expect(crun[toBlock]).toEqual(toFields);
          expect(crun.env).toEqual({ FOO: 'bar' });
          expect(crun.sync).toBe('tar');
        });
      }

      for (const [type, block, fields] of [
        ['cloudrun', 'cloudrun', { project_id: 'proj-a', location: 'us-central1' }],
        [
          'cloudrun-instances',
          'cloudrun_instances',
          { project_id: 'proj-a', region: 'us-central1' },
        ],
      ] as const) {
        const loadRuntime = async (onPut: (body: Record<string, any>) => void) =>
          createComponent(
            createFetchHandler(
              cloudRunConfig(tier, { type, env: { FOO: 'bar' }, [block]: fields }),
              {
                schemaResponse: {
                  sections: {
                    ...SCHEMA_RESPONSE.sections,
                    runtimes: { koanf_paths: ['runtimes'] },
                  },
                },
                putHandler: (body) => {
                  if ('runtimes' in body) onPut(body);
                  return { status: 200, body: { reload: { applied: [] } } };
                },
              }
            )
          );

        it(`${mode} mode: clearing both ${type} fields drops the ${block} block`, async () => {
          let capturedPayload: Record<string, any> | null = null;
          element = await loadRuntime((body) => (capturedPayload = body));

          const { project, location } = cloudRunInputs(element);
          project.value = '';
          project.dispatchEvent(new Event('sl-input'));
          location.value = '';
          location.dispatchEvent(new Event('sl-input'));
          await saveAndCapture(element);

          expect(capturedPayload).not.toBeNull();
          const crun = capturedPayload!.runtimes.crun;
          expect(crun.type).toBe(type);
          expect(block in crun).toBe(false);
          expect(crun.env).toEqual({ FOO: 'bar' });
        });

        it(`${mode} mode: switching ${type} to docker drops both Cloud Run blocks`, async () => {
          let capturedPayload: Record<string, any> | null = null;
          element = await loadRuntime((body) => (capturedPayload = body));

          // Finds the runtime type select by its cloudrun-instances option;
          // update this if another runtime-type select appears on the page.
          const typeSelect = queryAll(element, 'sl-select').find((s) =>
            s.querySelector('sl-option[value="cloudrun-instances"]')
          ) as HTMLElement & { value: string };
          expect(typeSelect).toBeDefined();
          typeSelect.value = 'docker';
          typeSelect.dispatchEvent(new Event('sl-change'));
          await saveAndCapture(element);

          expect(capturedPayload).not.toBeNull();
          const crun = capturedPayload!.runtimes.crun;
          expect(crun.type).toBe('docker');
          expect('cloudrun' in crun).toBe(false);
          expect('cloudrun_instances' in crun).toBe(false);
          expect(crun.env).toEqual({ FOO: 'bar' });
        });
      }
    }
  });

  describe('home storage on runtimes and profiles', () => {
    function homeConfig() {
      return makeBaseConfig({
        runtimes: {
          k8s: { type: 'kubernetes', home_storage_backend: 'nfs', home_storage_leaf: 'broker' },
        },
        profiles: {
          gke: { runtime: 'k8s', home_storage_backend: 'nfs', home_storage_leaf: 'pod' },
        },
      });
    }

    async function saveAndCapture(el: HTMLElement): Promise<void> {
      await (el as any).updateComplete;
      const buttons = queryAll(el, 'sl-button[variant="primary"]');
      const saveBtn = buttons.find((b) => b.textContent?.trim() === 'Save & Reload');
      (saveBtn as HTMLElement).click();
      await new Promise((resolve) => setTimeout(resolve, 300));
    }

    it('shows the current values on the runtime and profile cards', async () => {
      element = await createComponent(createFetchHandler(homeConfig()));
      const backends = queryAll(element, 'sl-select.home-storage-backend');
      expect(backends.map((s) => s.getAttribute('value'))).toEqual(['nfs', 'nfs']);
      const leaves = queryAll(element, 'sl-select.home-storage-leaf');
      expect(leaves.map((s) => s.getAttribute('value'))).toEqual(['broker', 'pod']);
    });

    it('editing another profile field keeps both keys in the PUT payload', async () => {
      let capturedPayload: Record<string, any> | null = null;
      element = await createComponent(
        createFetchHandler(homeConfig(), {
          putHandler: (body) => {
            if ('profiles' in body) capturedPayload = body;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );
      const registry = query(element, 'sl-input[placeholder="Override image registry"]') as
        | (HTMLElement & { value: string })
        | null;
      expect(registry).not.toBeNull();
      registry!.value = 'registry.example.com/team';
      registry!.dispatchEvent(new Event('sl-input'));
      await saveAndCapture(element);

      expect(capturedPayload).not.toBeNull();
      expect(capturedPayload!.profiles.gke.home_storage_backend).toBe('nfs');
      expect(capturedPayload!.profiles.gke.home_storage_leaf).toBe('pod');
      expect(capturedPayload!.runtimes.k8s.home_storage_backend).toBe('nfs');
      expect(capturedPayload!.runtimes.k8s.home_storage_leaf).toBe('broker');
    });

    it('changing and clearing the selects updates the payload', async () => {
      let capturedPayload: Record<string, any> | null = null;
      element = await createComponent(
        createFetchHandler(homeConfig(), {
          putHandler: (body) => {
            if ('profiles' in body) capturedPayload = body;
            return { status: 200, body: { reload: { applied: [] } } };
          },
        })
      );
      const [runtimeLeaf, profileLeaf] = queryAll(
        element,
        'sl-select.home-storage-leaf'
      ) as (HTMLElement & {
        value: string;
      })[];
      runtimeLeaf.value = '';
      runtimeLeaf.dispatchEvent(new Event('sl-change'));
      profileLeaf.value = 'broker';
      profileLeaf.dispatchEvent(new Event('sl-change'));
      await saveAndCapture(element);

      expect(capturedPayload).not.toBeNull();
      expect(capturedPayload!.profiles.gke.home_storage_leaf).toBe('broker');
      expect('home_storage_leaf' in capturedPayload!.runtimes.k8s).toBe(false);
      expect(capturedPayload!.runtimes.k8s.home_storage_backend).toBe('nfs');
    });
  });
  describe('Regression ptone/scion#1871 — masked secrets are not sent back', () => {
    const maskedServer = {
      notification_channels: [{ type: 'slack', params: { webhook_url: '********' } }],
      oauth: { web: { github: { client_id: 'cid', client_secret: '********' } } },
      github_app: { app_id: 42, private_key: '********', webhook_secret: '********' },
    };
    const withServer = (tier: string, extra: Record<string, unknown>) => {
      const base = makeBaseConfig({ settings_tier: tier });
      return { ...base, server: { ...base.server, ...extra } };
    };

    it('DB mode payload omits unedited masked notification_channels and github_app', async () => {
      element = await createComponent(createFetchHandler(withServer('db', maskedServer)));
      const el = element as any;
      // loadGitHubAppConfig may replace github_app with an unmasked copy; pin
      // the masked GET value (what the page holds when that load fails).
      el.rawConfig.server.github_app = maskedServer.github_app;
      const server = el.buildLayer1Payload().server as Record<string, unknown>;
      expect(server).toBeDefined();
      expect(server.notification_channels).toBeUndefined();
      expect(server.github_app).toBeUndefined();
      expect(JSON.stringify(el.buildLayer1Payload())).not.toContain('********');
    });

    it('file mode payload omits unedited masked notification_channels, oauth and github_app', async () => {
      element = await createComponent(createFetchHandler(withServer('file', maskedServer)));
      const el = element as any;
      // loadGitHubAppConfig may replace github_app with an unmasked copy; pin
      // the masked GET value (what the page holds when that load fails).
      el.rawConfig.server.github_app = maskedServer.github_app;
      const server = el.buildFilePayload().server as Record<string, unknown>;
      expect(server.notification_channels).toBeUndefined();
      expect(server.oauth).toBeUndefined();
      expect(server.github_app).toBeUndefined();
      expect(JSON.stringify(el.buildFilePayload())).not.toContain('********');
    });

    it('DB mode payload still sends blocks without masked values', async () => {
      const plain = {
        notification_channels: [{ type: 'slack', filter_urgent_only: true }],
        github_app: { app_id: 42, webhooks_enabled: true },
      };
      element = await createComponent(createFetchHandler(withServer('db', plain)));
      const el = element as any;
      // Pin github_app: loadGitHubAppConfig may replace it after load.
      el.rawConfig.server.github_app = plain.github_app;
      const server = el.buildLayer1Payload().server as Record<string, unknown>;
      expect(server.notification_channels).toEqual(plain.notification_channels);
      expect(server.github_app).toEqual(plain.github_app);
    });

    it('file mode payload still sends blocks without masked values', async () => {
      const plain = {
        notification_channels: [{ type: 'slack', filter_urgent_only: true }],
        oauth: { web: { github: { client_id: 'cid' } } },
      };
      element = await createComponent(createFetchHandler(withServer('file', plain)));
      const server = (element as any).buildFilePayload().server as Record<string, unknown>;
      expect(server.notification_channels).toEqual(plain.notification_channels);
      expect(server.oauth).toEqual(plain.oauth);
    });
  });
});
