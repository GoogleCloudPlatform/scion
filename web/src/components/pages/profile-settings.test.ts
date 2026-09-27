import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

// ── Fetch mock ──

interface ServerConfigFixture {
  status?: number;
  body?: Record<string, unknown>;
}

interface PatchResult {
  status: number;
  body: Record<string, unknown>;
}

function makeServerConfig(): Record<string, unknown> {
  return {
    schema_version: '1',
    active_profile: 'local',
    profiles: {
      local: { runtime: 'docker', timezone: 'UTC', image_registry: 'reg.example' },
      remote: { runtime: 'kubernetes', timezone: 'Europe/Berlin' },
    },
  };
}

function createFetchHandler(
  config: ServerConfigFixture,
  onPatch?: (body: Record<string, unknown>) => PatchResult
) {
  return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
    const json = (status: number, body: unknown): Promise<Response> =>
      Promise.resolve(
        new Response(JSON.stringify(body), {
          status,
          headers: { 'Content-Type': 'application/json' },
        })
      );

    if (path.endsWith('/api/v1/admin/server-config')) {
      if (init?.method === 'PATCH') {
        const result = onPatch
          ? onPatch(JSON.parse(init.body as string) as Record<string, unknown>)
          : { status: 200, body: { status: 'saved' } };
        return json(result.status, result.body);
      }
      return json(config.status ?? 200, config.body ?? makeServerConfig());
    }
    return json(200, {});
  };
}

// ── Helpers ──

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyEl = any;

async function createComponent(
  fetchHandler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>
): Promise<AnyEl> {
  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = document.createElement('scion-page-profile-settings') as AnyEl;
  document.body.appendChild(el);
  await el.updateComplete;
  // Let the async server-config load settle.
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
  return el;
}

function shadowText(el: HTMLElement): string {
  return el.shadowRoot?.textContent ?? '';
}

function patchCalls(): Array<[unknown, RequestInit]> {
  const fetchMock = globalThis.fetch as unknown as ReturnType<typeof vi.fn>;
  return (fetchMock.mock.calls as Array<[unknown, RequestInit | undefined]>).filter(
    ([, init]) => init?.method === 'PATCH'
  ) as Array<[unknown, RequestInit]>;
}

// ── Tests ──

describe('scion-page-profile-settings — timezone', () => {
  let element: AnyEl = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler({})));
    await import('./profile-settings.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  it('shows the active profile timezone', async () => {
    element = await createComponent(createFetchHandler({}));
    expect(shadowText(element)).toContain('Agent timezone');
    expect(shadowText(element)).toContain('"local"');
    const input = element.shadowRoot.querySelector('.timezone-row sl-input');
    expect(input.value).toBe('UTC');
  });

  it('hides the section when server config is not readable', async () => {
    element = await createComponent(
      createFetchHandler({ status: 403, body: { error: { message: 'forbidden' } } })
    );
    expect(shadowText(element)).not.toContain('Agent timezone');
    expect(element.shadowRoot.querySelector('.timezone-row')).toBeNull();
  });

  it('hides the section when there is no active profile', async () => {
    element = await createComponent(
      createFetchHandler({ body: { schema_version: '1', profiles: {} } })
    );
    expect(element.shadowRoot.querySelector('.timezone-row')).toBeNull();
  });

  it('saves only the active profile timezone and preserves everything else', async () => {
    let captured: Record<string, unknown> | null = null;
    element = await createComponent(
      createFetchHandler({}, (body) => {
        captured = body;
        return { status: 200, body: { status: 'saved' } };
      })
    );

    element._timezoneInput = '  America/Los_Angeles  ';
    await element._saveTimezone();
    await element.updateComplete;

    expect(captured).toEqual({
      profiles: {
        local: {
          runtime: 'docker',
          timezone: 'America/Los_Angeles',
          image_registry: 'reg.example',
        },
        remote: { runtime: 'kubernetes', timezone: 'Europe/Berlin' },
      },
    });
    expect(shadowText(element)).toContain('Timezone updated.');
  });

  it('removes the timezone key when cleared', async () => {
    let captured: { profiles: Record<string, Record<string, unknown>> } | null = null;
    element = await createComponent(
      createFetchHandler({}, (body) => {
        captured = body as typeof captured;
        return { status: 200, body: { status: 'saved' } };
      })
    );

    element._timezoneInput = '';
    await element._saveTimezone();

    expect(captured).not.toBeNull();
    expect(captured!.profiles.local).toEqual({ runtime: 'docker', image_registry: 'reg.example' });
    expect('timezone' in captured!.profiles.local).toBe(false);
  });

  it('rejects an unknown timezone without calling the API', async () => {
    element = await createComponent(createFetchHandler({}));

    element._timezoneInput = 'Mars/Olympus_Mons';
    await element._saveTimezone();
    await element.updateComplete;

    expect(patchCalls()).toHaveLength(0);
    expect(shadowText(element)).toContain('is not a recognized IANA timezone name');
  });

  it('surfaces the backend error message on failure', async () => {
    element = await createComponent(
      createFetchHandler({}, () => ({
        status: 422,
        body: { error: { message: 'profile "local": invalid timezone' } },
      }))
    );

    element._timezoneInput = 'Asia/Tokyo';
    await element._saveTimezone();
    await element.updateComplete;

    expect(shadowText(element)).toContain('profile "local": invalid timezone');
    expect(shadowText(element)).not.toContain('Timezone updated.');
  });
});
