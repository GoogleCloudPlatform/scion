/**
 * Env badge keys against the settings registry (ptone/scion#3910).
 *
 * The page marks a field as overridden by the environment when its koanf key
 * is in the server's env_overrides list, which holds koanf paths. A field
 * whose badge key is not a koanf path can never show the badge or the env
 * lock. This test renders every tab, records each key the page checks
 * against env_overrides, and requires each one to be a koanf path from the
 * registry fixture (a listed Layer-1 path, a Layer-0 key the settings schema
 * defines, or a file key). Known mismatches are listed in KNOWN_MISMATCHES.
 *
 * A second test requires each field's badge key to be that field's own
 * koanf path: it reports the key as env-set, finds the field that shows the
 * badge, changes only the config value at that key, and requires the same
 * field's control to change. A badge key that is another field's path fails,
 * because the badged field does not show that path's value.
 */
import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import settingsSchema from '../../../../pkg/config/schemas/settings-v1.schema.json';
import {
  isFileKey,
  isLayer0Key,
  isListedLayer1Path,
  registrySchemaResponse,
} from './__fixtures__/settings-registry.js';

/**
 * Badge keys that are not koanf paths today, with the reason. The list may
 * only shrink: a new mismatch fails, and so does an entry that now matches
 * or is no longer rendered. ptone/scion#3911 drains it.
 */
const KNOWN_MISMATCHES: Record<string, string> = {
  // The koanf keys are server.broker.broker_name and broker_nickname
  // (ptone/scion#4418).
  'server.broker.name':
    'koanf key is server.broker.broker_name (ptone/scion#4418, ptone/scion#3911)',
  'server.broker.nickname':
    'koanf key is server.broker.broker_nickname (ptone/scion#4418, ptone/scion#3911)',
  // Not in the telemetry registry section, so the server never reports them
  // (ptone/scion#3723).
  'telemetry.cloud.gcp_project_id': 'not a registry koanf path (ptone/scion#3911)',
  'telemetry.cloud.cloud_logging': 'not a registry koanf path (ptone/scion#3911)',
};

/** The page methods whose first argument(s) are env_overrides keys. */
const ENV_KEY_PROBES = ['renderEnvBadge', 'renderEnvBadgeUnder', 'readOnlyReason'] as const;

type SchemaNode = Record<string, unknown>;

/** Every property path in the settings schema, with local $refs resolved. */
function schemaPaths(): Set<string> {
  const root = settingsSchema as unknown as SchemaNode;
  const defs = (root['$defs'] ?? {}) as Record<string, SchemaNode>;
  const out = new Set<string>();
  const walk = (node: SchemaNode, path: string, depth: number): void => {
    if (depth > 25) return;
    const ref = node['$ref'];
    if (typeof ref === 'string' && ref.startsWith('#/$defs/')) {
      const def = defs[ref.slice('#/$defs/'.length)];
      if (def) walk(def, path, depth + 1);
    }
    const props = (node['properties'] ?? {}) as Record<string, SchemaNode>;
    for (const [name, child] of Object.entries(props)) {
      const p = path ? `${path}.${name}` : name;
      out.add(p);
      walk(child, p, depth + 1);
    }
  };
  walk(root, '', 0);
  return out;
}

const SCHEMA_PATHS = schemaPaths();

/** Whether the server can report key in env_overrides. */
function isKoanfPath(key: string): boolean {
  if (isListedLayer1Path(key) || isFileKey(key)) return true;
  return isLayer0Key(key) && SCHEMA_PATHS.has(key);
}

/** A GET response with every field the page shows set, so each renders. */
function makeFullConfig(overrides: Record<string, unknown>): Record<string, unknown> {
  return {
    schema_version: '1',
    active_profile: 'default',
    workspace_path: '/srv/workspace',
    default_template: 'gemini',
    // A known harness name, so the select (the field with the badge) shows it.
    default_harness_config: 'claude',
    image_registry: 'ghcr.io/test',
    default_max_turns: 10,
    default_max_model_calls: 20,
    default_max_duration: '1h',
    default_resources: { requests: { cpu: '1', memory: '1Gi' } },
    default_model: 'large',
    default_thinking_level: 2,
    default_runtime_broker: 'broker-1',
    default_timezone: 'Europe/Paris',
    default_gcp_identity_mode: 'assign',
    default_gcp_identity_service_account_id: 'sa-1',
    server: {
      mode: 'standalone',
      log_level: 'info',
      hub: {
        port: 8080,
        host: '0.0.0.0',
        public_url: 'https://hub.example.com',
        monitoring_dashboard_url: 'https://monitoring.example.com',
        read_timeout: '30s',
        write_timeout: '30s',
        admin_emails: ['admin@example.com'],
        soft_delete_retention: '72h',
        soft_delete_retain_files: true,
        auto_suspend_stalled: true,
        stalled_threshold: '10m',
        gcp_iam_check_mode: 'enforce',
        gcp_iam_deny_unknown_policy: 'fail-closed',
      },
      broker: {
        enabled: true,
        port: 9090,
        host: '0.0.0.0',
        hub_endpoint: 'http://hub.internal',
        container_hub_endpoint: 'http://hub.container',
        broker_name: 'broker-a',
        broker_nickname: 'nick',
        auto_provide: true,
      },
      database: { driver: 'postgres', url: '********' },
      auth: {
        dev_mode: false,
        user_access_mode: 'invite',
        default_user_role: 'admin',
        authorized_domains: ['example.com'],
      },
      storage: { provider: 'gcs', bucket: 'bucket-a', local_path: '/data' },
      secrets: {
        backend: 'gcpsm',
        gcp_project_id: 'secrets-project',
        gcp_replication_locations: ['us-east1'],
      },
      message_broker: { enabled: true, type: 'inprocess' },
      native_chat: { enabled: true },
      github_app: {
        app_id: 1,
        api_base_url: 'https://api.github.com',
        webhooks_enabled: true,
        installation_url: 'https://github.com/apps/test/installations/new',
      },
    },
    telemetry: {
      enabled: true,
      cloud: {
        enabled: true,
        endpoint: 'otel.example.com:4317',
        protocol: 'grpc',
        provider: 'gcp',
        gcp_project_id: 'telemetry-project',
        cloud_logging: true,
      },
      hub: { enabled: true, report_interval: '30s' },
    },
    auto_expose_ports: { enabled: true },
    quotas: { enforce_broker_quotas: true },
    agent_secrets: { user_scope_only: true },
    runtimes: { docker: { type: 'docker' } },
    profiles: { default: { runtime: 'docker' } },
    harness_configs: { gemini: { harness: 'gemini' } },
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
    if (path.includes('/api/v1/github-app')) {
      // The GitHub App endpoint reports the server.github_app settings.
      const server = (config.server ?? {}) as Record<string, unknown>;
      return jsonResponse({ configured: true, ...(server.github_app as object) });
    }
    return jsonResponse([]);
  };
}

let element: HTMLElement | null = null;

type PageElement = HTMLElement & {
  updateComplete: Promise<boolean>;
  requestUpdate(): void;
};

/** Waits until the page has applied the stubbed GET responses. */
async function waitForLoad(el: PageElement): Promise<void> {
  await vi.waitFor(() => {
    expect((el as unknown as { loading: boolean }).loading, 'page still loading').toBe(false);
  });
  await el.updateComplete;
}

/** The value at a dotted path in obj, or undefined. */
function valueAt(obj: Record<string, unknown>, path: string): unknown {
  let cur: unknown = obj;
  for (const seg of path.split('.')) {
    if (cur === null || typeof cur !== 'object') return undefined;
    cur = (cur as Record<string, unknown>)[seg];
  }
  return cur;
}

/** A different value of the same type (objects: their first leaf changes). */
function changed(value: unknown): unknown {
  if (typeof value === 'string') return `${value}-changed`;
  if (typeof value === 'number') return value + 1;
  if (typeof value === 'boolean') return !value;
  if (Array.isArray(value)) return [...value, 'changed.example.com'];
  if (value !== null && typeof value === 'object') {
    const entries = Object.entries(value as Record<string, unknown>);
    if (entries.length > 0) {
      const [first, v] = entries[0];
      return { ...(value as Record<string, unknown>), [first]: changed(v) };
    }
  }
  return undefined;
}

/** A deep copy of config with the value at path replaced. */
function withValueAt(
  config: Record<string, unknown>,
  path: string,
  value: unknown
): Record<string, unknown> {
  const copy = structuredClone(config);
  const segs = path.split('.');
  let cur = copy;
  for (const seg of segs.slice(0, -1)) cur = cur[seg] as Record<string, unknown>;
  cur[segs[segs.length - 1]] = value;
  return copy;
}

/** What a field shows: its text, and each element's attributes, value and checked state. */
function fieldSnapshot(field: Element): string {
  const parts = [field.textContent ?? ''];
  for (const node of field.querySelectorAll('*')) {
    const n = node as Element & { value?: unknown; checked?: unknown };
    const attrs = [...n.attributes].map((a) => `${a.name}=${a.value}`).join(' ');
    parts.push(
      `${n.tagName} ${attrs} |${JSON.stringify(n.value ?? null)}|${JSON.stringify(n.checked ?? null)}`
    );
  }
  return parts.join('\n');
}

/**
 * Renders the page with config, with every field editable, and returns a
 * snapshot of each field that shows the env badge, keyed by its label.
 */
async function renderBadgedFields(config: Record<string, unknown>): Promise<Map<string, string>> {
  vi.stubGlobal('fetch', vi.fn(fetchHandler(config)));
  const proto = customElements.get('scion-page-admin-server-config')!.prototype;
  // Locked fields render a read-only value instead of the editable control
  // that carries the badge, so no field is locked here.
  const lock = vi.spyOn(proto, 'readOnlyReason').mockReturnValue(null);
  element = document.createElement('scion-page-admin-server-config');
  document.body.appendChild(element);
  const el = element as PageElement;
  await waitForLoad(el);
  const out = new Map<string, string>();
  const seen = new Map<string, number>();
  for (const badge of el.shadowRoot!.querySelectorAll('.env-badge')) {
    // A badge outside a form field (default_resources) covers its tab panel.
    const field = badge.closest('.form-field') ?? badge.closest('sl-tab-panel');
    if (!field) continue;
    const label =
      field.querySelector('label')?.textContent?.trim() || field.getAttribute('name') || '';
    const n = seen.get(label) ?? 0;
    seen.set(label, n + 1);
    out.set(`${label}#${n}`, fieldSnapshot(field));
  }
  lock.mockRestore();
  element.remove();
  element = null;
  return out;
}

/** The argument lists of every renderEnvBadge call in one render. */
async function renderEnvBadgeCalls(config: Record<string, unknown>): Promise<string[][]> {
  vi.stubGlobal('fetch', vi.fn(fetchHandler(config)));
  element = document.createElement('scion-page-admin-server-config');
  document.body.appendChild(element);
  const el = element as PageElement;
  await waitForLoad(el);
  const spy = vi.spyOn(Object.getPrototypeOf(el), 'renderEnvBadge');
  el.requestUpdate();
  await el.updateComplete;
  const calls = spy.mock.calls.map((call) => call.map(String));
  spy.mockRestore();
  element.remove();
  element = null;
  return calls;
}

/**
 * Renders the page with config and returns every key passed to the
 * env-key probes. All tab panels render at once, so one render covers
 * every tab.
 */
async function renderEnvKeys(config: Record<string, unknown>): Promise<Set<string>> {
  vi.stubGlobal('fetch', vi.fn(fetchHandler(config)));
  element = document.createElement('scion-page-admin-server-config');
  document.body.appendChild(element);
  const el = element as PageElement;
  await waitForLoad(el);

  const proto = Object.getPrototypeOf(el);
  const spies = ENV_KEY_PROBES.map((name) => vi.spyOn(proto, name));
  el.requestUpdate();
  await el.updateComplete;
  const keys = new Set<string>();
  for (const spy of spies) {
    for (const call of spy.mock.calls) {
      for (const arg of call) if (typeof arg === 'string') keys.add(arg);
    }
    spy.mockRestore();
  }
  element.remove();
  element = null;
  return keys;
}

describe('admin server config env badge keys (ptone/scion#3910)', () => {
  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(fetchHandler(makeFullConfig({}))));
    await import('./admin-server-config.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  it('the settings schema walker finds the server keys', () => {
    expect(SCHEMA_PATHS.has('server.hub.port')).toBe(true);
    expect(SCHEMA_PATHS.has('server.broker.broker_name')).toBe(true);
  });

  it('every env badge key is a koanf path the server can report', async () => {
    // A file-tier hub (Layer-0 fields shown, env-pinned when overridden) and
    // a hosted DB hub cover both rendering modes.
    const keys = new Set<string>([
      ...(await renderEnvKeys(makeFullConfig({}))),
      ...(await renderEnvKeys(makeFullConfig({ settings_tier: 'db' }))),
    ]);
    expect(keys.size, 'probes recorded no keys; the probe names are stale').toBeGreaterThan(30);

    const sorted = [...keys].sort();
    const mismatched = sorted.filter((k) => !isKoanfPath(k));
    expect(
      mismatched.filter((k) => !(k in KNOWN_MISMATCHES)),
      'env badge keys that are not koanf paths (use the koanf path, or list the key in KNOWN_MISMATCHES)'
    ).toEqual([]);
    expect(
      Object.keys(KNOWN_MISMATCHES).filter((k) => isKoanfPath(k)),
      'KNOWN_MISMATCHES entries that now match a koanf path (remove them)'
    ).toEqual([]);
    expect(
      Object.keys(KNOWN_MISMATCHES).filter((k) => !keys.has(k)),
      'KNOWN_MISMATCHES entries the page no longer renders (remove them)'
    ).toEqual([]);
  });

  it("every env badge key is its own field's koanf path", async () => {
    const base = makeFullConfig({});
    const calls = await renderEnvBadgeCalls(base);
    expect(
      calls.length,
      'renderEnvBadge recorded no calls; the probe name is stale'
    ).toBeGreaterThan(30);

    // A call may add the field's parent sections after its own key
    // (renderEnvBadge('server.github_app.app_id', 'server.github_app')).
    const fieldKeys = new Set<string>();
    const problems: string[] = [];
    for (const [own, ...parents] of calls) {
      fieldKeys.add(own);
      for (const p of parents) {
        if (!own.startsWith(`${p}.`)) {
          problems.push(`renderEnvBadge(${own}, ${p}): ${p} is not a parent section of ${own}`);
        }
      }
    }

    for (const key of [...fieldKeys].sort()) {
      const value = valueAt(base, key);
      const other = changed(value);
      if (other === undefined) {
        problems.push(`${key}: makeFullConfig has no value at the badge key; add one`);
        continue;
      }
      const before = await renderBadgedFields({ ...base, env_overrides: [key] });
      const after = await renderBadgedFields({
        ...withValueAt(base, key, other),
        env_overrides: [key],
      });
      if (before.size === 0) {
        problems.push(`${key}: no field shows the env badge when the key is env-set`);
      }
      for (const [field, snapshot] of before) {
        if (after.get(field) === snapshot) {
          problems.push(
            `${key}: field "${field}" shows the badge but not the value at ${key}; ` +
              'its badge key is not its own koanf path'
          );
        }
      }
    }
    expect(problems).toEqual([]);
  }, 120_000);
});
