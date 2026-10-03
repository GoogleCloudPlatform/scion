/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import type { ScionAdminExperiments } from './admin-experiments.js';
import { setPreferredTimeZone } from '../../utils/time.js';

// ── Fixtures ──

function makeEntry(overrides: Record<string, unknown> = {}) {
  return {
    name: 'web.terminal_workspace',
    title: 'Persistent terminal workspace',
    description: 'Opens agent terminals in the persistent workspace.',
    layers: ['web'],
    stage: 'beta',
    default: true,
    override: null,
    enabled: true,
    issue: 'ptone/scion#1662',
    owner: 'web',
    review_by: '2026-12-31',
    review_overdue: false,
    ...overrides,
  };
}

function makeResponse(overrides: Record<string, unknown> = {}) {
  return {
    revision: 1,
    malformed: false,
    experiments: [makeEntry()],
    unknown_overrides: {},
    updated_at: null,
    updated_by: null,
    ...overrides,
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function shadowText(el: HTMLElement): string {
  return el.shadowRoot?.textContent ?? '';
}

function query(el: HTMLElement, selector: string): Element | null {
  return el.shadowRoot?.querySelector(selector) ?? null;
}

function queryAll(el: HTMLElement, selector: string): Element[] {
  return Array.from(el.shadowRoot?.querySelectorAll(selector) ?? []);
}

async function createElement(): Promise<ScionAdminExperiments> {
  const el = document.createElement('scion-admin-experiments') as ScionAdminExperiments;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

async function activate(el: ScionAdminExperiments): Promise<void> {
  el.active = true;
  await el.updateComplete;
  // Flush the fetch + any follow-on GET reload.
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
}

describe('scion-admin-experiments', () => {
  let element: ScionAdminExperiments | null = null;

  beforeAll(async () => {
    await import('./admin-experiments.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('does not fetch until active becomes true (lazy load)', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(makeResponse())));
    vi.stubGlobal('fetch', fetchMock);

    element = await createElement();
    await new Promise((r) => setTimeout(r, 0));
    expect(fetchMock).not.toHaveBeenCalled();

    await activate(element);
    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining('/api/v1/admin/experiments'),
      expect.any(Object)
    );
  });

  it('fetches only once across repeated active=true transitions', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(makeResponse())));
    vi.stubGlobal('fetch', fetchMock);

    element = await createElement();
    await activate(element);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    element.active = false;
    await element.updateComplete;
    element.active = true;
    await element.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('shows "Loading experiments…" while the initial GET is pending, not the empty state', async () => {
    let resolveGet!: (r: Response) => void;
    const fetchMock = vi.fn(
      () =>
        new Promise<Response>((r) => {
          resolveGet = r;
        })
    );
    vi.stubGlobal('fetch', fetchMock);

    element = await createElement();
    element.active = true;
    await element.updateComplete;
    // Setting `active` triggers a render, then `updated()` calls `load()`,
    // which synchronously flips `loading` before the first `await` inside
    // it — a cascading update that this `updateComplete` resolution doesn't
    // itself wait for, so flush one more microtask/update cycle.
    await new Promise((r) => setTimeout(r, 0));
    await element.updateComplete;

    expect(shadowText(element)).toContain('Loading experiments');
    expect(shadowText(element)).not.toContain('No experiments are currently registered.');

    resolveGet(jsonResponse(makeResponse()));
    await new Promise((r) => setTimeout(r, 0));
    await element.updateComplete;
    expect(shadowText(element)).not.toContain('Loading experiments');
  });

  it('renders the normal state: title, description, badges, issue link, switch', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse(makeResponse())))
    );
    element = await createElement();
    await activate(element);

    const text = shadowText(element);
    expect(text).toContain('Persistent terminal workspace');
    expect(text).toContain('web.terminal_workspace');
    expect(text).toContain('Opens agent terminals');
    const link = query(element, 'a[href*="github.com"]');
    expect(link?.getAttribute('href')).toBe('https://github.com/ptone/scion/issues/1662');
    const switchEl = query(element, 'sl-switch');
    expect(switchEl).toBeTruthy();
    // The accessible name comes from sl-switch's default slot, which its
    // shadow template wraps in a <label> together with the control — a
    // plain `aria-label` attribute on the host is not forwarded into the
    // shadow root and is not exposed as the control's accessible name in a
    // real browser. jsdom/happy-dom don't compute accessible names across
    // shadow boundaries, so this only checks the slotted text is present;
    // `web/e2e/experiments.spec.ts` asserts the real accessible name via
    // `getByRole('switch', { name })` against the real browser.
    expect(switchEl?.querySelector('.sr-only')?.textContent).toBe(
      'Enable Persistent terminal workspace'
    );
  });

  it('renders a row without layer badges instead of throwing when layers is missing', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          jsonResponse(makeResponse({ experiments: [makeEntry({ layers: undefined })] }))
        )
      )
    );
    element = await createElement();
    await activate(element);

    expect(shadowText(element)).toContain('Persistent terminal workspace');
    expect(query(element, 'sl-badge[variant="primary"]')).toBeNull();
  });

  it('shows the empty state when no experiments are registered', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse(makeResponse({ experiments: [] }))))
    );
    element = await createElement();
    await activate(element);
    expect(shadowText(element)).toContain('No experiments are currently registered.');
  });

  it('403 state: shows the permission message and no switches', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({}, 403)))
    );
    element = await createElement();
    await activate(element);

    expect(shadowText(element)).toContain('hub.experiments.update');
    expect(query(element, 'sl-switch')).toBeNull();
  });

  it('tab-level attribution: shows "Last changed by" with a formatted time and the ISO value in a title', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          jsonResponse(
            makeResponse({ updated_at: '2026-09-01T00:00:00Z', updated_by: 'admin@x.com' })
          )
        )
      )
    );
    element = await createElement();
    await activate(element);

    expect(shadowText(element)).toContain('Last changed by admin@x.com at');
    const timeSpan = query(element, '.attribution span');
    expect(timeSpan?.getAttribute('title')).toBe('2026-09-01T00:00:00Z');
    // Displayed text is a human-formatted date/time, not the raw ISO string
    // with its nanosecond-precision fractional seconds.
    expect(timeSpan?.textContent).not.toBe('2026-09-01T00:00:00Z');
    expect(timeSpan?.textContent?.length).toBeGreaterThan(0);
  });

  it('tab-level attribution: renders in the display zone, 24-hour, and follows a zone change', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          jsonResponse(
            // Midnight in Tokyo (UTC+9); the browser zone is pinned to UTC.
            makeResponse({ updated_at: '2026-09-01T15:00:00Z', updated_by: 'admin@x.com' })
          )
        )
      )
    );
    setPreferredTimeZone('Asia/Tokyo');
    try {
      element = await createElement();
      await activate(element);
      const timeSpan = () => query(element!, '.attribution span');
      expect(timeSpan()?.textContent).toBe('Sep 2, 2026, 00:00 (Asia/Tokyo)');

      setPreferredTimeZone('America/New_York');
      await element.updateComplete;
      expect(timeSpan()?.textContent).toBe('Sep 1, 2026, 11:00 (America/New_York)');
    } finally {
      setPreferredTimeZone('');
    }
  });

  it('falls back to the raw string if updated_at does not parse as a date', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          jsonResponse(makeResponse({ updated_at: 'not-a-date', updated_by: 'admin@x.com' }))
        )
      )
    );
    element = await createElement();
    await activate(element);

    const timeSpan = query(element, '.attribution span');
    expect(timeSpan?.textContent).toBe('not-a-date');
  });

  it('shows "unknown" for the attribution name if updated_by is null while updated_at is set', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          jsonResponse(makeResponse({ updated_at: '2026-09-01T00:00:00Z', updated_by: null }))
        )
      )
    );
    element = await createElement();
    await activate(element);

    expect(shadowText(element)).toContain('Last changed by unknown at');
  });

  it('shows "unknown" for the attribution name if updated_by is an empty string', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          jsonResponse(makeResponse({ updated_at: '2026-09-01T00:00:00Z', updated_by: '' }))
        )
      )
    );
    element = await createElement();
    await activate(element);

    expect(shadowText(element)).toContain('Last changed by unknown at');
  });

  it('does not show attribution when there is no stored row', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse(makeResponse())))
    );
    element = await createElement();
    await activate(element);
    expect(shadowText(element)).not.toContain('Last changed by');
  });

  it('shows "Reset to default" only when an override exists', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse(makeResponse({ experiments: [makeEntry()] }))))
    );
    element = await createElement();
    await activate(element);
    expect(shadowText(element)).not.toContain('Reset to default');

    element.remove();
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          jsonResponse(
            makeResponse({ experiments: [makeEntry({ override: false, enabled: false })] })
          )
        )
      )
    );
    element = await createElement();
    await activate(element);
    expect(shadowText(element)).toContain('Reset to default');
  });

  it('shows the review-overdue warning', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          jsonResponse(makeResponse({ experiments: [makeEntry({ review_overdue: true })] }))
        )
      )
    );
    element = await createElement();
    await activate(element);
    expect(shadowText(element)).toContain('Review overdue');
  });

  it('shows the unknown_overrides note when non-empty', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          jsonResponse(makeResponse({ unknown_overrides: { 'hub.future_thing': true } }))
        )
      )
    );
    element = await createElement();
    await activate(element);
    expect(shadowText(element)).toContain('hub.future_thing');
    expect(shadowText(element)).toContain('not known to this hub version');
  });

  it('toggling a switch sends a PUT with the current revision and applies the response optimistically then authoritatively', async () => {
    const fetchMock = vi.fn((_url: string, init?: RequestInit) => {
      if (init?.method === 'PUT') {
        const body = JSON.parse(init.body as string);
        expect(body).toEqual({
          overrides: { 'web.terminal_workspace': false },
          expected_revision: 1,
        });
        return Promise.resolve(
          jsonResponse(
            makeResponse({
              revision: 2,
              experiments: [makeEntry({ override: false, enabled: false })],
              updated_at: '2026-09-01T00:00:00Z',
              updated_by: 'admin@x.com',
            })
          )
        );
      }
      return Promise.resolve(jsonResponse(makeResponse()));
    });
    vi.stubGlobal('fetch', fetchMock);

    element = await createElement();
    await activate(element);

    const sw = query(element, 'sl-switch') as HTMLElement;
    sw.dispatchEvent(new CustomEvent('sl-change'));
    await element.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await element.updateComplete;

    expect(shadowText(element)).toContain('Last changed by admin@x.com');
    expect(shadowText(element)).toContain('Reset to default');
  });

  it('a toggle on a second row made after the first write resolves carries the revision the first write returned', async () => {
    const rowA = makeEntry({ name: 'web.terminal_workspace' });
    const rowB = makeEntry({
      name: 'hub.test_gate',
      title: 'Test gate',
      layers: ['server'],
    });
    // Recorded here and asserted outside the mock: a failing `expect` inside
    // the mock throws into `apiFetch`, and the component's own `catch` in
    // `setOverride` swallows it (reverts and reloads) instead of failing the
    // test, so this body cannot be inspected safely from inside the mock.
    const putBodies: Array<{
      overrides: Record<string, boolean | null>;
      expected_revision: number;
    }> = [];
    const fetchMock = vi.fn((_url: string, init?: RequestInit) => {
      if (init?.method === 'PUT') {
        const body = JSON.parse(init.body as string) as {
          overrides: Record<string, boolean | null>;
          expected_revision: number;
        };
        putBodies.push(body);
        if ('web.terminal_workspace' in body.overrides) {
          return Promise.resolve(
            jsonResponse(
              makeResponse({
                revision: 2,
                experiments: [rowA, rowB].map((e) =>
                  e.name === 'web.terminal_workspace'
                    ? { ...e, override: false, enabled: false }
                    : e
                ),
              })
            )
          );
        }
        return Promise.resolve(
          jsonResponse(
            makeResponse({
              revision: 3,
              experiments: [
                { ...rowA, override: false, enabled: false },
                { ...rowB, override: false, enabled: false },
              ],
            })
          )
        );
      }
      return Promise.resolve(jsonResponse(makeResponse({ experiments: [rowA, rowB] })));
    });
    vi.stubGlobal('fetch', fetchMock);

    element = await createElement();
    await activate(element);

    const switches = queryAll(element, 'sl-switch');
    expect(switches).toHaveLength(2);

    (switches[0] as HTMLElement).dispatchEvent(new CustomEvent('sl-change'));
    await element.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await element.updateComplete;

    (switches[1] as HTMLElement).dispatchEvent(new CustomEvent('sl-change'));
    await element.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await element.updateComplete;

    // The real assertion: the second write's expected_revision is the
    // revision the first write's response returned (2), not the
    // original-GET revision (1) both rows started with.
    expect(putBodies).toEqual([
      { overrides: { 'web.terminal_workspace': false }, expected_revision: 1 },
      { overrides: { 'hub.test_gate': false }, expected_revision: 2 },
    ]);
    // Neither write reverted (no error banner, both rows show "overridden"),
    // which would not hold if either write had been rejected and reloaded.
    expect(element.shadowRoot?.querySelector('sl-alert[variant="danger"]')).toBeNull();
    expect(shadowText(element)).toContain('Test gate');
    const captions = Array.from(element.shadowRoot?.querySelectorAll('.caption') ?? []).map(
      (c) => c.textContent
    );
    expect(captions.filter((c) => c?.includes('overridden'))).toHaveLength(2);
  });

  it('while a write is pending, every switch and the reset button are disabled and a click has no effect', async () => {
    let resolvePut!: (r: Response) => void;
    const fetchMock = vi.fn((_url: string, init?: RequestInit) => {
      if (init?.method === 'PUT') {
        return new Promise<Response>((r) => {
          resolvePut = r;
        });
      }
      return Promise.resolve(
        jsonResponse(
          makeResponse({ experiments: [makeEntry({ override: false, enabled: false })] })
        )
      );
    });
    vi.stubGlobal('fetch', fetchMock);

    element = await createElement();
    await activate(element);

    const sw = query(element, 'sl-switch') as HTMLElement;
    const resetButton = queryAll(element, 'sl-button').find((b) =>
      (b.textContent ?? '').includes('Reset to default')
    ) as HTMLElement;
    expect(resetButton).toBeTruthy();

    sw.dispatchEvent(new CustomEvent('sl-change'));
    await element.updateComplete;

    expect(sw.hasAttribute('disabled')).toBe(true);
    expect(resetButton.hasAttribute('disabled')).toBe(true);
    const putCallsBefore = fetchMock.mock.calls.filter(
      (c) => (c[1] as RequestInit)?.method === 'PUT'
    ).length;
    sw.dispatchEvent(new CustomEvent('sl-change'));
    await element.updateComplete;
    const putCallsAfter = fetchMock.mock.calls.filter(
      (c) => (c[1] as RequestInit)?.method === 'PUT'
    ).length;
    expect(putCallsAfter).toBe(putCallsBefore);

    resolvePut(
      jsonResponse(
        makeResponse({ revision: 2, experiments: [makeEntry({ override: false, enabled: false })] })
      )
    );
    await new Promise((r) => setTimeout(r, 0));
    await element.updateComplete;
    expect(sw.hasAttribute('disabled')).toBe(false);
  });

  it('on 409 revision_conflict, reverts the switch and reloads with GET', async () => {
    let getCount = 0;
    const fetchMock = vi.fn((_url: string, init?: RequestInit) => {
      if (init?.method === 'PUT') {
        return Promise.resolve(
          new Response(
            JSON.stringify({ error: { code: 'revision_conflict', message: 'conflict' } }),
            {
              status: 409,
              headers: { 'Content-Type': 'application/json' },
            }
          )
        );
      }
      getCount++;
      return Promise.resolve(jsonResponse(makeResponse({ revision: getCount })));
    });
    vi.stubGlobal('fetch', fetchMock);

    element = await createElement();
    await activate(element);

    const sw = query(element, 'sl-switch') as HTMLElement;
    sw.dispatchEvent(new CustomEvent('sl-change'));
    await element.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await element.updateComplete;

    expect(shadowText(element)).toContain('another administrator');
    expect(sw.hasAttribute('checked')).toBe(true); // reverted to enabled: true
    expect(getCount).toBeGreaterThanOrEqual(2); // initial load + reload after the 409
  });

  it('on a PUT 409 experiments_malformed, reloads into the malformed state', async () => {
    const fetchMock = vi.fn((_url: string, init?: RequestInit) => {
      if (init?.method === 'PUT') {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              error: { code: 'experiments_malformed', message: 'unreadable, reset all' },
            }),
            { status: 409, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      return Promise.resolve(
        jsonResponse(
          makeResponse({ malformed: true, experiments: [makeEntry({ enabled: false })] })
        )
      );
    });
    vi.stubGlobal('fetch', fetchMock);

    element = await createElement();
    await activate(element);

    const sw = query(element, 'sl-switch') as HTMLElement;
    sw.dispatchEvent(new CustomEvent('sl-change'));
    await element.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await element.updateComplete;

    expect(shadowText(element)).toContain('unreadable');
    expect(sw.hasAttribute('disabled')).toBe(true);
  });

  it('on a non-409 write failure, reverts and reloads with GET', async () => {
    const fetchMock = vi.fn((_url: string, init?: RequestInit) => {
      if (init?.method === 'PUT') {
        return Promise.resolve(
          jsonResponse({ error: { code: 'internal_error', message: 'boom' } }, 500)
        );
      }
      return Promise.resolve(jsonResponse(makeResponse()));
    });
    vi.stubGlobal('fetch', fetchMock);

    element = await createElement();
    await activate(element);

    const sw = query(element, 'sl-switch') as HTMLElement;
    sw.dispatchEvent(new CustomEvent('sl-change'));
    await element.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await element.updateComplete;

    expect(shadowText(element)).toContain('boom');
    expect(sw.hasAttribute('checked')).toBe(true);
    const getCalls = fetchMock.mock.calls.filter((c) => !(c[1] as RequestInit)?.method);
    expect(getCalls.length).toBeGreaterThanOrEqual(2); // initial load + reload after failure
  });

  it('malformed state: disables switches and offers Reset all to defaults with confirmation', async () => {
    const fetchMock = vi.fn((_url: string, init?: RequestInit) => {
      if (init?.method === 'DELETE') {
        expect(JSON.parse(init.body as string)).toEqual({ confirm_reset_malformed: true });
        return Promise.resolve(jsonResponse(makeResponse({ malformed: false, revision: 9 })));
      }
      return Promise.resolve(
        jsonResponse(
          makeResponse({ malformed: true, experiments: [makeEntry({ enabled: false })] })
        )
      );
    });
    vi.stubGlobal('fetch', fetchMock);

    element = await createElement();
    await activate(element);

    expect(shadowText(element)).toContain('unreadable');
    const sw = query(element, 'sl-switch') as HTMLElement;
    expect(sw.hasAttribute('disabled')).toBe(true);

    const resetButtons = queryAll(element, 'sl-button').filter((b) =>
      (b.textContent ?? '').includes('Reset all to defaults')
    );
    expect(resetButtons.length).toBeGreaterThan(0);

    (resetButtons[0] as HTMLElement).dispatchEvent(new Event('click'));
    await element.updateComplete;
    const dialog = query(element, 'sl-dialog');
    expect(dialog?.getAttribute('open')).not.toBeNull();

    const confirmButton = queryAll(
      element,
      'sl-dialog sl-button[variant="danger"]'
    )[0] as HTMLElement;
    confirmButton.dispatchEvent(new Event('click'));
    await new Promise((r) => setTimeout(r, 0));
    await element.updateComplete;

    expect(fetchMock.mock.calls.some((c) => (c[1] as RequestInit)?.method === 'DELETE')).toBe(true);
    expect(shadowText(element)).not.toContain('unreadable');
  });
});
