import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import {
  LAYER1_SECTIONS,
  bodyPathToKoanf,
  unknownPayloadKeys,
  valueAtPath,
} from './__fixtures__/settings-registry.js';

// The Federation page edits the registry's federation section through the
// server-config PUT (ptone/scion#3924). Each section key must be sent
// explicitly when cleared, since the DB-backed save keeps an omitted key
// (ptone/scion#3720), and every key sent must be a registry key.

const FULL_FEDERATION = {
  enabled: true,
  algorithms: ['RS256', 'ES256'],
  refresh_interval: '2h',
  debounce_interval: '10s',
  trusted_issuers: [{ issuer_url: 'https://issuer.example.com', issuer_type: 'hub' }],
};

/**
 * How to clear each federation section key on the page, and the value it
 * must be sent as. Keyed by registry koanf path; the test checks the keys
 * match the registry section, so a new registry key fails here until the
 * page edits and sends it.
 */
const CLEAR: Record<string, { clear: (el: any) => void; expected: unknown }> = {
  'server.federation.enabled': { clear: (el) => (el.enabled = false), expected: false },
  'server.federation.trusted_issuers': { clear: (el) => (el.issuers = []), expected: [] },
  'server.federation.algorithms': { clear: (el) => (el.algorithms = []), expected: [] },
  'server.federation.refresh_interval': { clear: (el) => (el.refreshInterval = ''), expected: '' },
  'server.federation.debounce_interval': {
    clear: (el) => (el.debounceInterval = ''),
    expected: '',
  },
};

function jsonResponse(body: unknown): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

describe('scion-page-admin-federation save payload (ptone/scion#3924)', () => {
  let element: HTMLElement | null = null;
  let puts: Array<Record<string, unknown>> = [];

  function fetchHandler(url: string | URL | Request, init?: RequestInit): Promise<Response> {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
    if (init?.method === 'PUT' && path.includes('/api/v1/admin/server-config')) {
      puts.push(JSON.parse(init.body as string) as Record<string, unknown>);
      return jsonResponse({ reload: { applied: [] } });
    }
    if (path.includes('/api/v1/admin/server-config')) {
      return jsonResponse({ schema_version: '1', federation: FULL_FEDERATION });
    }
    return jsonResponse({});
  }

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(fetchHandler));
    await import('./admin-federation.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    puts = [];
    vi.restoreAllMocks();
  });

  async function renderAndSave(mutate: (el: any) => void): Promise<Record<string, unknown>> {
    vi.stubGlobal('fetch', vi.fn(fetchHandler));
    element = document.createElement('scion-page-admin-federation');
    document.body.appendChild(element);
    const el = element as any;
    await el.updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 100));
    await el.updateComplete;
    mutate(el);
    await el.updateComplete;
    const save = Array.from(el.shadowRoot?.querySelectorAll('sl-button') ?? []).find(
      (b) => (b as Element).textContent?.trim() === 'Save'
    );
    expect(save).toBeDefined();
    (save as HTMLElement).click();
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(puts).toHaveLength(1);
    return puts[0]!;
  }

  it('edits every registry federation key', () => {
    expect(Object.keys(CLEAR).sort()).toEqual([...LAYER1_SECTIONS['federation']!].sort());
  });

  it('sends every federation key explicitly when cleared', async () => {
    const payload = await renderAndSave((el) => {
      for (const spec of Object.values(CLEAR)) spec.clear(el);
    });
    const notSent: string[] = [];
    const wrongValue: string[] = [];
    for (const [key, spec] of Object.entries(CLEAR)) {
      // The body carries federation at the top level (serverConfigBodyKoanfKey).
      const bodyPath = ['federation', ...key.split('.').slice(2)];
      expect(bodyPathToKoanf(bodyPath)).toBe(key);
      const { present, value } = valueAtPath(payload, bodyPath);
      if (!present) notSent.push(key);
      else if (JSON.stringify(value) !== JSON.stringify(spec.expected))
        wrongValue.push(`${key}=${JSON.stringify(value)}`);
    }
    expect(notSent, 'federation keys missing from the cleared save payload').toEqual([]);
    expect(wrongValue, 'cleared keys not sent as their explicit empty value').toEqual([]);
  });

  it('sends only registry keys', async () => {
    const cleared = await renderAndSave((el) => {
      for (const spec of Object.values(CLEAR)) spec.clear(el);
    });
    expect(unknownPayloadKeys(cleared, 'layer1')).toEqual([]);
  });

  it('sends only registry keys when nothing is changed', async () => {
    const unchanged = await renderAndSave(() => {});
    expect(unknownPayloadKeys(unchanged, 'layer1')).toEqual([]);
    expect(unchanged).toEqual({ federation: FULL_FEDERATION });
  });
});
