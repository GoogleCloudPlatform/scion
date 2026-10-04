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

/**
 * The skills list page follows nextCursor (ptone/scion#1949): every page is
 * shown, the first page's _capabilities still drive the Create button, and
 * a failure after the first page keeps the loaded skills with a notice.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

type PageEl = HTMLElement & {
  updateComplete: Promise<boolean>;
  pageData?: unknown;
};

/** Private page state the tests drive or observe. */
type PageInternals = {
  loading: boolean;
  skills: { id: string }[];
  scopeFilter: string;
  loadSkills(): Promise<void>;
};

function internals(el: PageEl): PageInternals {
  return el as unknown as PageInternals;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function skill(id: string) {
  return {
    id,
    name: `skill-${id}`,
    scope: 'global',
    status: 'active',
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
  };
}

function urlOf(input: string | URL | Request): string {
  return typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
}

/** Wait until the page has finished loading and rendered. */
async function settled(el: PageEl): Promise<void> {
  await vi.waitFor(() => expect(internals(el).loading).toBe(false));
  await el.updateComplete;
}

async function mountSkillsPage(
  handler: (url: string) => Promise<Response>,
  pageData?: unknown
): Promise<{ el: PageEl; fetchMock: ReturnType<typeof vi.fn> }> {
  const fetchMock = vi.fn((input: string | URL | Request) => handler(urlOf(input)));
  vi.stubGlobal('fetch', fetchMock);
  const el = document.createElement('scion-page-skills') as PageEl;
  if (pageData) el.pageData = pageData;
  document.body.appendChild(el);
  await settled(el);
  return { el, fetchMock };
}

function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}

/**
 * Record every loadSkills() promise, in call order, so a test can wait for a
 * specific walk to settle instead of guessing with timers.
 */
function spyOnLoads(): Promise<void>[] {
  const proto = customElements.get('scion-page-skills')!.prototype as PageInternals;
  const orig = proto.loadSkills;
  const loads: Promise<void>[] = [];
  vi.spyOn(proto, 'loadSkills').mockImplementation(function (this: PageInternals) {
    const p = orig.call(this);
    loads.push(p);
    return p;
  });
  return loads;
}

/** Mount the page without waiting for it to settle. */
function mountWithHandler(handler: (url: string) => Promise<Response>): {
  el: PageEl;
  fetchMock: ReturnType<typeof vi.fn>;
} {
  const fetchMock = vi.fn((input: string | URL | Request) => handler(urlOf(input)));
  vi.stubGlobal('fetch', fetchMock);
  const el = document.createElement('scion-page-skills') as PageEl;
  document.body.appendChild(el);
  return { el, fetchMock };
}

function shownSkills(el: PageEl): string[] {
  return Array.from(el.shadowRoot?.querySelectorAll('.skill-card') ?? [])
    .map((card) => card.getAttribute('href') ?? '')
    .sort();
}

const FIRST_PAGE = {
  skills: [skill('1'), skill('2')],
  nextCursor: 'c2',
  _capabilities: { actions: ['create'] },
};

describe('scion-page-skills pagination', () => {
  let element: PageEl | null = null;

  beforeAll(async () => {
    await import('./skills.js');
  }, 60_000);

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('shows every page and keeps the first page capabilities', async () => {
    const { el, fetchMock } = await mountSkillsPage((url) =>
      Promise.resolve(
        jsonResponse(url.includes('cursor=c2') ? { skills: [skill('3')] } : FIRST_PAGE)
      )
    );
    element = el;

    expect(fetchMock).toHaveBeenCalledTimes(2);
    const firstUrl = urlOf(fetchMock.mock.calls[0][0] as string);
    expect(firstUrl).toBe('/api/v1/skills?status=active&limit=200');
    expect(urlOf(fetchMock.mock.calls[1][0] as string)).toContain('cursor=c2');
    expect(shownSkills(el)).toEqual(['/skills/1', '/skills/2', '/skills/3']);
    expect(el.shadowRoot?.querySelector('a[href="/skills/new"]')).not.toBeNull();
    expect(el.shadowRoot?.querySelector('.partial-load-notice')).toBeNull();
  });

  it('keeps loaded pages and shows a notice when a later page fails', async () => {
    const { el } = await mountSkillsPage((url) =>
      Promise.resolve(
        url.includes('cursor=c2')
          ? jsonResponse({ error: { code: 'internal' } }, 500)
          : jsonResponse(FIRST_PAGE)
      )
    );
    element = el;

    expect(shownSkills(el)).toEqual(['/skills/1', '/skills/2']);
    const notice = el.shadowRoot?.querySelector('.partial-load-notice');
    expect(notice).not.toBeNull();
    expect(notice?.textContent).toContain('Showing 2 skills;');
    expect(notice?.textContent).toContain('500');
    expect(el.shadowRoot?.querySelector('a[href="/skills/new"]')).not.toBeNull();
    expect(el.shadowRoot?.querySelector('.error-state')).toBeNull();
  });

  it('Retry after a partial failure loads every page and clears the notice', async () => {
    let failPage2 = true;
    const { el } = await mountSkillsPage((url) => {
      if (url.includes('cursor=c2')) {
        return Promise.resolve(
          failPage2
            ? jsonResponse({ error: { code: 'internal' } }, 500)
            : jsonResponse({ skills: [skill('3')] })
        );
      }
      return Promise.resolve(jsonResponse(FIRST_PAGE));
    });
    element = el;
    expect(el.shadowRoot?.querySelector('.partial-load-notice')).not.toBeNull();

    failPage2 = false;
    const retry = el.shadowRoot?.querySelector(
      '.partial-load-notice sl-button.partial-load-retry'
    ) as HTMLElement | null;
    expect(retry).not.toBeNull();
    retry!.click();
    await settled(el);

    expect(el.shadowRoot?.querySelector('.partial-load-notice')).toBeNull();
    expect(shownSkills(el)).toEqual(['/skills/1', '/skills/2', '/skills/3']);
  });

  it('ignores a superseded walk that succeeds late', async () => {
    const loads = spyOnLoads();
    const stale = deferred<Response>();
    const { el, fetchMock } = mountWithHandler((url) => {
      if (url.includes('scope=core')) {
        return Promise.resolve(jsonResponse({ skills: [skill('core-1')] }));
      }
      if (url.includes('cursor=c3'))
        return Promise.resolve(jsonResponse({ skills: [skill('c3')] }));
      if (url.includes('cursor=c2')) return stale.promise;
      return Promise.resolve(jsonResponse(FIRST_PAGE));
    });
    element = el;
    // The first walk is now waiting on its second page.
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));

    // A scope change starts a newer load, which finishes first.
    internals(el).scopeFilter = 'core';
    await internals(el).loadSkills();
    await el.updateComplete;
    expect(shownSkills(el)).toEqual(['/skills/core-1']);

    // The stale walk's last page arrives late and must not be shown.
    // It also has a further page, which the superseded walk must not fetch.
    stale.resolve(jsonResponse({ skills: [skill('stale')], nextCursor: 'c3' }));
    await loads[0];
    expect(fetchMock.mock.calls.some((c) => urlOf(c[0] as string).includes('cursor=c3'))).toBe(
      false
    );
    await el.updateComplete;

    expect(shownSkills(el)).toEqual(['/skills/core-1']);
    expect(internals(el).loading).toBe(false);
    expect(el.shadowRoot?.querySelector('.partial-load-notice')).toBeNull();
    expect(el.shadowRoot?.querySelector('.error-state')).toBeNull();
  });

  it('ignores a superseded walk that fails late', async () => {
    const loads = spyOnLoads();
    const stale = deferred<Response>();
    const { el, fetchMock } = mountWithHandler((url) => {
      if (url.includes('scope=core')) {
        return Promise.resolve(jsonResponse({ skills: [skill('core-1')] }));
      }
      if (url.includes('cursor=c2')) return stale.promise;
      return Promise.resolve(jsonResponse(FIRST_PAGE));
    });
    element = el;
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));

    internals(el).scopeFilter = 'core';
    await internals(el).loadSkills();

    stale.resolve(jsonResponse({ error: { code: 'internal' } }, 500));
    await loads[0];
    await el.updateComplete;

    expect(shownSkills(el)).toEqual(['/skills/core-1']);
    expect(el.shadowRoot?.querySelector('.partial-load-notice')).toBeNull();
    expect(el.shadowRoot?.querySelector('.error-state')).toBeNull();
  });

  it('keeps loading while the newer load runs when a stale walk settles', async () => {
    const loads = spyOnLoads();
    const stale = deferred<Response>();
    const core = deferred<Response>();
    const { el, fetchMock } = mountWithHandler((url) => {
      if (url.includes('scope=core')) return core.promise;
      if (url.includes('cursor=c2')) return stale.promise;
      return Promise.resolve(jsonResponse(FIRST_PAGE));
    });
    element = el;
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));

    // Start the newer load but leave its response pending.
    internals(el).scopeFilter = 'core';
    void internals(el).loadSkills();

    stale.resolve(jsonResponse({ skills: [skill('stale')] }));
    await loads[0];
    await el.updateComplete;
    expect(internals(el).loading).toBe(true);
    expect(internals(el).skills.map((sk) => sk.id)).not.toContain('stale');

    core.resolve(jsonResponse({ skills: [skill('core-1')] }));
    await loads[1];
    await el.updateComplete;
    expect(internals(el).loading).toBe(false);
    expect(shownSkills(el)).toEqual(['/skills/core-1']);
  });

  it('stops walking pages once the page is detached', async () => {
    const loads = spyOnLoads();
    const page2 = deferred<Response>();
    const { el, fetchMock } = mountWithHandler((url) => {
      if (url.includes('cursor=c3')) return Promise.resolve(jsonResponse({ skills: [] }));
      if (url.includes('cursor=c2')) return page2.promise;
      return Promise.resolve(jsonResponse(FIRST_PAGE));
    });
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));

    el.remove();
    page2.resolve(jsonResponse({ skills: [skill('3')], nextCursor: 'c3' }));
    await loads[0];

    expect(fetchMock.mock.calls.some((c) => urlOf(c[0] as string).includes('cursor=c3'))).toBe(
      false
    );
    expect(internals(el).loading).toBe(false);
  });

  it('shows the error state for a raw-array body, not a silent empty list', async () => {
    // The hub always sends the {skills, nextCursor, _capabilities} envelope;
    // paginateAll rejects any other body shape before parsePage runs.
    const { el } = await mountSkillsPage(() => Promise.resolve(jsonResponse([skill('1')])));
    element = el;

    const errorState = el.shadowRoot?.querySelector('.error-state');
    expect(errorState).not.toBeNull();
    expect(errorState?.textContent).toContain('response body was not an object');
    expect(shownSkills(el)).toEqual([]);
  });

  it('shows the error state when the first page fails', async () => {
    const { el } = await mountSkillsPage(() =>
      Promise.resolve(jsonResponse({ error: { code: 'internal' } }, 500))
    );
    element = el;

    const errorState = el.shadowRoot?.querySelector('.error-state');
    expect(errorState).not.toBeNull();
    expect(errorState?.textContent).toContain('Failed to load skills (Skills request failed: 500)');
    expect(shownSkills(el)).toEqual([]);
  });

  it('walks the remaining pages when the server prefetch has a nextCursor', async () => {
    const { el, fetchMock } = await mountSkillsPage(
      (url) =>
        Promise.resolve(
          jsonResponse(url.includes('cursor=c2') ? { skills: [skill('3')] } : FIRST_PAGE)
        ),
      { path: '/skills', title: 'Skills', data: FIRST_PAGE }
    );
    element = el;

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(shownSkills(el)).toEqual(['/skills/1', '/skills/2', '/skills/3']);
    expect(el.shadowRoot?.querySelector('a[href="/skills/new"]')).not.toBeNull();
  });

  it('uses a complete server prefetch without fetching', async () => {
    const { el, fetchMock } = await mountSkillsPage(
      () => Promise.reject(new Error('unexpected fetch')),
      {
        path: '/skills',
        title: 'Skills',
        data: { skills: [skill('1')], _capabilities: { actions: ['create'] } },
      }
    );
    element = el;

    expect(fetchMock).not.toHaveBeenCalled();
    expect(shownSkills(el)).toEqual(['/skills/1']);
    expect(el.shadowRoot?.querySelector('a[href="/skills/new"]')).not.toBeNull();
  });
});
