import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

const MOCK_CONFIG_RESPONSE = {
  schema_version: '1',
  scion_version: '0.1.0-test',
  scion_commit: 'abc123',
  scion_build_time: '2026-01-01T00:00:00Z',
  active_profile: 'default',
  image_registry: 'ghcr.io/test',
  server: {
    mode: 'standalone',
    log_level: 'info',
    hub: { port: 8080, host: '0.0.0.0' },
    database: { driver: 'sqlite' },
    auth: { dev_mode: false },
    storage: { provider: 'local' },
    secrets: { provider: 'env' },
  },
};

function mockFetch(url: string | URL | Request): Promise<Response> {
  const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;

  if (path.includes('/api/v1/admin/server-config')) {
    return Promise.resolve(new Response(JSON.stringify(MOCK_CONFIG_RESPONSE), {
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
}

describe('scion-page-admin-server-config', () => {
  let element: HTMLElement;

  beforeEach(() => {
    vi.stubGlobal('fetch', vi.fn(mockFetch));
  });

  afterEach(() => {
    element?.remove();
    vi.restoreAllMocks();
  });

  it('renders without errors', async () => {
    const { ScionPageAdminServerConfig } = await import('./admin-server-config.js');
    element = document.createElement('scion-page-admin-server-config');
    document.body.appendChild(element);

    await (element as InstanceType<typeof ScionPageAdminServerConfig>).updateComplete;

    expect(element).toBeInstanceOf(ScionPageAdminServerConfig);
    expect(element.shadowRoot).toBeTruthy();
  });

  it('calls the config API on connect', async () => {
    await import('./admin-server-config.js');
    element = document.createElement('scion-page-admin-server-config');
    document.body.appendChild(element);

    await new Promise(resolve => setTimeout(resolve, 50));

    expect(vi.mocked(fetch)).toHaveBeenCalledWith(
      expect.stringContaining('/api/v1/admin/server-config'),
      expect.any(Object),
    );
  });

  it('displays config data after loading', async () => {
    const { ScionPageAdminServerConfig } = await import('./admin-server-config.js');
    element = document.createElement('scion-page-admin-server-config');
    document.body.appendChild(element);

    const el = element as InstanceType<typeof ScionPageAdminServerConfig>;
    await el.updateComplete;
    // Wait for async loadConfig to complete and trigger re-render
    await new Promise(resolve => setTimeout(resolve, 100));
    await el.updateComplete;

    const shadow = el.shadowRoot!;
    expect(shadow.innerHTML).toContain('0.1.0-test');
  });
});
