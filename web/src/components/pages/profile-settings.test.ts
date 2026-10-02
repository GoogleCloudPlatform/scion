import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { getPreferredTimeZone, setPreferredTimeZone } from '../../utils/time.js';

// ── Fetch mock ──

interface ServerConfigFixture {
  status?: number;
  body?: Record<string, unknown>;
}

interface PatchResult {
  status: number;
  body: Record<string, unknown>;
}

interface AuthMeFixture {
  status?: number;
  body?: Record<string, unknown>;
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

function makeAuthMe(timezone = ''): Record<string, unknown> {
  return {
    id: 'u1',
    email: 'u1@example.com',
    displayName: 'User One',
    preferences: timezone ? { timezone } : {},
  };
}

function createFetchHandler(
  config: ServerConfigFixture,
  onPatch?: (body: Record<string, unknown>) => PatchResult,
  authMe: AuthMeFixture = {},
  onUserPatch?: (body: Record<string, unknown>) => PatchResult
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
    if (path.endsWith('/auth/me')) {
      return json(authMe.status ?? 200, authMe.body ?? makeAuthMe());
    }
    if (/\/api\/v1\/users\/[^/]+$/.test(path) && init?.method === 'PATCH') {
      const result = onUserPatch
        ? onUserPatch(JSON.parse(init.body as string) as Record<string, unknown>)
        : { status: 200, body: { id: 'u1' } };
      return json(result.status, result.body);
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

describe('scion-page-profile-settings — display timezone', () => {
  let element: AnyEl = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler({})));
    await import('./profile-settings.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    setPreferredTimeZone('');
    vi.restoreAllMocks();
  });

  function userPatchCalls(): Array<[unknown, RequestInit]> {
    const fetchMock = globalThis.fetch as unknown as ReturnType<typeof vi.fn>;
    return (fetchMock.mock.calls as Array<[unknown, RequestInit | undefined]>).filter(
      ([url, init]) =>
        init?.method === 'PATCH' && /\/api\/v1\/users\/[^/]+$/.test(String(url))
    ) as Array<[unknown, RequestInit]>;
  }

  it('is visible to every signed-in user, independent of the Agent timezone section', async () => {
    element = await createComponent(
      createFetchHandler({ status: 403 }, undefined, { body: makeAuthMe() })
    );
    expect(shadowText(element)).toContain('Display timezone');
    expect(shadowText(element)).not.toContain('Agent timezone');
  });

  it('loads the current preference (Auto when unset)', async () => {
    element = await createComponent(createFetchHandler({}, undefined, { body: makeAuthMe() }));
    const picker = element.shadowRoot.querySelector('scion-timezone-picker');
    expect(picker.value).toBe('');
  });

  it('loads a configured preference', async () => {
    element = await createComponent(
      createFetchHandler({}, undefined, { body: makeAuthMe('Asia/Tokyo') })
    );
    const picker = element.shadowRoot.querySelector('scion-timezone-picker');
    expect(picker.value).toBe('Asia/Tokyo');
  });

  it('saves a selection with a per-key preferences merge and updates the store live', async () => {
    let captured: Record<string, unknown> | null = null;
    element = await createComponent(
      createFetchHandler({}, undefined, { body: makeAuthMe() }, (body) => {
        captured = body;
        return { status: 200, body: { id: 'u1' } };
      })
    );

    await element._handleZoneChange(
      new CustomEvent('zone-change', { detail: { value: 'Asia/Kathmandu' } })
    );
    await element.updateComplete;

    expect(captured).toEqual({ preferences: { timezone: 'Asia/Kathmandu' } });
    expect(getPreferredTimeZone()).toBe('Asia/Kathmandu');
    expect(shadowText(element)).toContain('Display timezone updated.');
  });

  it('clearing to Auto sends an explicit empty string and updates the store with no reload', async () => {
    let captured: Record<string, unknown> | null = null;
    element = await createComponent(
      createFetchHandler({}, undefined, { body: makeAuthMe('Asia/Tokyo') }, (body) => {
        captured = body;
        return { status: 200, body: { id: 'u1' } };
      })
    );

    await element._handleZoneChange(new CustomEvent('zone-change', { detail: { value: '' } }));
    await element.updateComplete;

    expect(captured).toEqual({ preferences: { timezone: '' } });
    expect(getPreferredTimeZone()).toBe('');
  });

  it('surfaces the backend error message on failure and does not update the store', async () => {
    element = await createComponent(
      createFetchHandler({}, undefined, { body: makeAuthMe() }, () => ({
        status: 400,
        body: { error: { message: 'invalid timezone' } },
      }))
    );

    await element._handleZoneChange(
      new CustomEvent('zone-change', { detail: { value: 'Not/AZone' } })
    );
    await element.updateComplete;

    expect(shadowText(element)).toContain('invalid timezone');
    expect(getPreferredTimeZone()).toBe('');
  });

  it('does not PATCH when the user id has not loaded yet', async () => {
    element = await createComponent(
      createFetchHandler({}, undefined, { status: 500, body: {} })
    );

    await element._handleZoneChange(
      new CustomEvent('zone-change', { detail: { value: 'Asia/Tokyo' } })
    );
    await element.updateComplete;

    expect(userPatchCalls()).toHaveLength(0);
  });
});
