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
      if (url.includes('scope=global')) {
        return Promise.resolve(jsonResponse({ skills: [skill('global-1')] }));
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
    internals(el).scopeFilter = 'global';
    await internals(el).loadSkills();
    await el.updateComplete;
    expect(shownSkills(el)).toEqual(['/skills/global-1']);

    // The stale walk's last page arrives late and must not be shown.
    // It also has a further page, which the superseded walk must not fetch.
    stale.resolve(jsonResponse({ skills: [skill('stale')], nextCursor: 'c3' }));
    await loads[0];
    expect(fetchMock.mock.calls.some((c) => urlOf(c[0] as string).includes('cursor=c3'))).toBe(
      false
    );
    await el.updateComplete;

    expect(shownSkills(el)).toEqual(['/skills/global-1']);
    expect(internals(el).loading).toBe(false);
    expect(el.shadowRoot?.querySelector('.partial-load-notice')).toBeNull();
    expect(el.shadowRoot?.querySelector('.error-state')).toBeNull();
  });

  it('ignores a superseded walk that fails late', async () => {
    const loads = spyOnLoads();
    const stale = deferred<Response>();
    const { el, fetchMock } = mountWithHandler((url) => {
      if (url.includes('scope=global')) {
        return Promise.resolve(jsonResponse({ skills: [skill('global-1')] }));
      }
      if (url.includes('cursor=c2')) return stale.promise;
      return Promise.resolve(jsonResponse(FIRST_PAGE));
    });
    element = el;
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));

    internals(el).scopeFilter = 'global';
    await internals(el).loadSkills();

    stale.resolve(jsonResponse({ error: { code: 'internal' } }, 500));
    await loads[0];
    await el.updateComplete;

    expect(shownSkills(el)).toEqual(['/skills/global-1']);
    expect(el.shadowRoot?.querySelector('.partial-load-notice')).toBeNull();
    expect(el.shadowRoot?.querySelector('.error-state')).toBeNull();
  });

  it('keeps loading while the newer load runs when a stale walk settles', async () => {
    const loads = spyOnLoads();
    const stale = deferred<Response>();
    const globalLoad = deferred<Response>();
    const { el, fetchMock } = mountWithHandler((url) => {
      if (url.includes('scope=global')) return globalLoad.promise;
      if (url.includes('cursor=c2')) return stale.promise;
      return Promise.resolve(jsonResponse(FIRST_PAGE));
    });
    element = el;
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));

    // Start the newer load but leave its response pending.
    internals(el).scopeFilter = 'global';
    void internals(el).loadSkills();

    stale.resolve(jsonResponse({ skills: [skill('stale')] }));
    await loads[0];
    await el.updateComplete;
    expect(internals(el).loading).toBe(true);
    expect(internals(el).skills.map((sk) => sk.id)).not.toContain('stale');

    globalLoad.resolve(jsonResponse({ skills: [skill('global-1')] }));
    await loads[1];
    await el.updateComplete;
    expect(internals(el).loading).toBe(false);
    expect(shownSkills(el)).toEqual(['/skills/global-1']);
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

/** The element focused inside the page's shadow root, if any. */
function focusedIn(el: PageEl): Element | null {
  return el.shadowRoot?.activeElement ?? null;
}

function shadowQuery(el: PageEl, selector: string): HTMLElement | null {
  return el.shadowRoot?.querySelector<HTMLElement>(selector) ?? null;
}

describe('scion-page-skills keeps keyboard focus on reload (ptone/scion#2948)', () => {
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

  it('a search reload keeps the list and filter bar, and focus stays on the search input', async () => {
    const searchReply = deferred<Response>();
    const { el } = await mountSkillsPage((url) =>
      url.includes('search=foo')
        ? searchReply.promise
        : Promise.resolve(jsonResponse({ skills: [skill('1'), skill('2')] }))
    );
    element = el;

    const search = shadowQuery(el, '.search-input')!;
    search.focus();
    expect(focusedIn(el)).toBe(search);

    (search as HTMLElement & { value: string }).value = 'foo';
    search.dispatchEvent(new Event('sl-input'));
    await vi.waitFor(() => expect(internals(el).loading).toBe(true), { timeout: 2000 });
    await el.updateComplete;

    // Still the same input, still focused; no full-page spinner.
    expect(shadowQuery(el, '.loading-state')).toBeNull();
    expect(shadowQuery(el, '.search-input')).toBe(search);
    expect(focusedIn(el)).toBe(search);
    expect(shadowQuery(el, '.filter-bar .inline-loading')).not.toBeNull();
    expect(shownSkills(el)).toEqual(['/skills/1', '/skills/2']);

    searchReply.resolve(jsonResponse({ skills: [skill('foo')] }));
    await settled(el);
    expect(shownSkills(el)).toEqual(['/skills/foo']);
    expect(shadowQuery(el, '.filter-bar .inline-loading')).toBeNull();
    expect(focusedIn(el)).toBe(search);
  });

  it('the error-state Retry stays rendered and focused while reloading and after another failure', async () => {
    let reply: Promise<Response> = Promise.resolve(
      jsonResponse({ error: { code: 'internal' } }, 500)
    );
    const { el } = await mountSkillsPage(() => reply);
    element = el;

    const retry = shadowQuery(el, '.error-state sl-button.error-retry')!;
    expect(retry).not.toBeNull();
    retry.focus();
    expect(focusedIn(el)).toBe(retry);

    const pending = deferred<Response>();
    reply = pending.promise;
    retry.click();
    await el.updateComplete;

    expect(internals(el).loading).toBe(true);
    expect(shadowQuery(el, '.loading-state')).toBeNull();
    expect(shadowQuery(el, '.error-state sl-button.error-retry')).toBe(retry);
    expect(retry.hasAttribute('loading')).toBe(true);
    expect(focusedIn(el)).toBe(retry);

    pending.resolve(jsonResponse({ error: { code: 'internal' } }, 500));
    await settled(el);
    expect(shadowQuery(el, '.error-state sl-button.error-retry')).toBe(retry);
    expect(retry.hasAttribute('loading')).toBe(false);
    expect(focusedIn(el)).toBe(retry);
  });

  it('a successful error-state Retry moves focus to the search input, not the body', async () => {
    let fail = true;
    const { el } = await mountSkillsPage(() =>
      Promise.resolve(
        fail
          ? jsonResponse({ error: { code: 'internal' } }, 500)
          : jsonResponse({ skills: [skill('1')] })
      )
    );
    element = el;

    const retry = shadowQuery(el, '.error-state sl-button.error-retry')!;
    retry.focus();
    fail = false;
    retry.click();
    await settled(el);
    await vi.waitFor(() => expect(focusedIn(el)).toBe(shadowQuery(el, '.search-input')));
    expect(shadowQuery(el, '.error-state')).toBeNull();
    expect(shownSkills(el)).toEqual(['/skills/1']);
  });

  it('the partial-load Retry keeps the list and notice while reloading, then focus moves to search', async () => {
    let page2: Promise<Response> = Promise.resolve(
      jsonResponse({ error: { code: 'internal' } }, 500)
    );
    const { el } = await mountSkillsPage((url) =>
      url.includes('cursor=c2') ? page2 : Promise.resolve(jsonResponse(FIRST_PAGE))
    );
    element = el;

    const retry = shadowQuery(el, '.partial-load-notice sl-button.partial-load-retry')!;
    retry.focus();
    const pending = deferred<Response>();
    page2 = pending.promise;
    retry.click();
    await vi.waitFor(() => expect(retry.hasAttribute('loading')).toBe(true));

    expect(shadowQuery(el, '.loading-state')).toBeNull();
    expect(shadowQuery(el, '.partial-load-notice sl-button.partial-load-retry')).toBe(retry);
    expect(focusedIn(el)).toBe(retry);
    expect(shownSkills(el)).toEqual(['/skills/1', '/skills/2']);

    pending.resolve(jsonResponse({ skills: [skill('3')] }));
    await settled(el);
    expect(shadowQuery(el, '.partial-load-notice')).toBeNull();
    expect(shownSkills(el)).toEqual(['/skills/1', '/skills/2', '/skills/3']);
    await vi.waitFor(() => expect(focusedIn(el)).toBe(shadowQuery(el, '.search-input')));
  });

  it('a failed search reload keeps the filter bar and focus on the search input', async () => {
    const { el } = await mountSkillsPage((url) =>
      Promise.resolve(
        url.includes('search=foo')
          ? jsonResponse({ error: { code: 'internal' } }, 500)
          : jsonResponse({ skills: [skill('1')] })
      )
    );
    element = el;

    const loads = spyOnLoads();
    const search = shadowQuery(el, '.search-input')!;
    search.focus();
    (search as HTMLElement & { value: string }).value = 'foo';
    search.dispatchEvent(new Event('sl-input'));
    await vi.waitFor(() => expect(loads).toHaveLength(1), { timeout: 2000 });
    await loads[0];
    await settled(el);

    // The error is shown under the filter bar, so the query can be fixed.
    expect(shadowQuery(el, '.error-state')).not.toBeNull();
    expect(shadowQuery(el, '.search-input')).toBe(search);
    expect(focusedIn(el)).toBe(search);
  });

  it('leaves focus alone when the user moved it out of the page during the load', async () => {
    const outside = document.createElement('button');
    document.body.appendChild(outside);
    try {
      let fail = true;
      const pending = deferred<Response>();
      const { el } = await mountSkillsPage(() =>
        fail ? Promise.resolve(jsonResponse({ error: { code: 'internal' } }, 500)) : pending.promise
      );
      element = el;

      const retry = shadowQuery(el, '.error-state sl-button.error-retry')!;
      retry.focus();
      fail = false;
      retry.click();
      await el.updateComplete;
      outside.focus();
      expect(document.activeElement).toBe(outside);

      pending.resolve(jsonResponse({ skills: [skill('1')] }));
      await settled(el);
      await new Promise((r) => setTimeout(r, 0));
      expect(shadowQuery(el, '.error-state')).toBeNull();
      expect(document.activeElement).toBe(outside);
      expect(focusedIn(el)).toBeNull();
    } finally {
      outside.remove();
    }
  });

  it('does not take focus when the reload started without focus in the page', async () => {
    let fail = true;
    const { el } = await mountSkillsPage(() =>
      Promise.resolve(
        fail
          ? jsonResponse({ error: { code: 'internal' } }, 500)
          : jsonResponse({ skills: [skill('1')] })
      )
    );
    element = el;
    fail = false;
    await internals(el).loadSkills();
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    expect(focusedIn(el)).toBeNull();
  });
});

describe('scion-page-skills shows the hub error message (ptone/scion#2949)', () => {
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

  it('includes the hub message when the first page is refused', async () => {
    const { el } = await mountSkillsPage(() =>
      Promise.resolve(
        jsonResponse(
          { error: { code: 'forbidden', message: 'You need skill.list on this hub' } },
          403
        )
      )
    );
    element = el;

    const details = shadowQuery(el, '.error-state .error-details');
    expect(details?.textContent).toBe(
      'Failed to load skills: You need skill.list on this hub (Skills request failed: 403)'
    );
  });

  it('includes the hub message when a later page fails', async () => {
    const { el } = await mountSkillsPage((url) =>
      Promise.resolve(
        url.includes('cursor=c2')
          ? jsonResponse({ message: 'Rate limit exceeded' }, 429)
          : jsonResponse(FIRST_PAGE)
      )
    );
    element = el;

    const notice = shadowQuery(el, '.partial-load-notice');
    expect(notice?.textContent).toContain('Rate limit exceeded; Skills request failed: 429');
  });
});

describe('scion-page-skills project filter and URL persistence (ptone/scion#8738)', () => {
  let element: PageEl | null = null;

  beforeAll(async () => {
    await import('./skills.js');
  }, 60_000);

  afterEach(() => {
    element?.remove();
    element = null;
    window.history.replaceState({}, '', '/skills');
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  const PROJECTS = [
    { id: 'proj-1', name: 'Alpha Project', slug: 'alpha-project' },
    { id: 'proj-2', name: 'Beta Project', slug: 'beta-project' },
    { id: 'proj-3', name: 'Gamma Project', slug: 'gamma-project' },
  ];

  function projectSkill(id: string, projectId: string) {
    return {
      id,
      name: `skill-${id}`,
      scope: 'project',
      scopeId: projectId,
      status: 'active',
      created: '2026-01-01T00:00:00Z',
      updated: '2026-01-01T00:00:00Z',
    };
  }

  function makeHandler() {
    const allProjectSkills = [
      projectSkill('s1', 'proj-1'),
      projectSkill('s2', 'proj-2'),
      projectSkill('s3', 'proj-3'),
    ];
    return (url: string): Promise<Response> => {
      if (url.includes('/api/v1/projects/')) {
        const id = url.split('/api/v1/projects/')[1].split('?')[0];
        const found = PROJECTS.find((p) => p.id === id);
        return Promise.resolve(found ? jsonResponse(found) : jsonResponse({}, 404));
      }
      if (url.includes('/api/v1/projects')) {
        return Promise.resolve(jsonResponse({ projects: PROJECTS }));
      }
      if (url.includes('/api/v1/skills')) {
        const u = new URL(url, 'http://localhost');
        const scope = u.searchParams.get('scope');
        const scopeId = u.searchParams.get('scopeId');
        let list = allProjectSkills;
        if (scope === 'project' && scopeId) {
          list = allProjectSkills.filter((s) => s.scopeId === scopeId);
        }
        return Promise.resolve(
          jsonResponse({ skills: list, _capabilities: { actions: ['create'] } })
        );
      }
      return Promise.resolve(jsonResponse({}, 404));
    };
  }

  it('shows the multi-select project filter only when scope is project', async () => {
    const { el } = await mountSkillsPage(makeHandler());
    element = el;

    // Hidden on default "All Scopes"
    expect(el.shadowRoot?.querySelector('scion-project-picker.project-filter')).toBeNull();

    // Switch scope to project
    const scopeSelect = el.shadowRoot?.querySelector('sl-select') as HTMLElement & {
      value: string;
    };
    scopeSelect.value = 'project';
    scopeSelect.dispatchEvent(new Event('sl-change'));
    await settled(el);

    const picker = el.shadowRoot?.querySelector('scion-project-picker.project-filter');
    expect(picker).not.toBeNull();
    expect(picker?.hasAttribute('multiple')).toBe(true);
    expect(new URLSearchParams(window.location.search).get('scope')).toBe('project');

    // Switch scope to global — hides picker and clears projects param
    scopeSelect.value = 'global';
    scopeSelect.dispatchEvent(new Event('sl-change'));
    await settled(el);

    expect(el.shadowRoot?.querySelector('scion-project-picker.project-filter')).toBeNull();
    const params = new URLSearchParams(window.location.search);
    expect(params.get('scope')).toBe('global');
    expect(params.get('projects')).toBeNull();
  });

  it('filters skills by one or multiple selected projects and persists selection in URL', async () => {
    const { el, fetchMock } = await mountSkillsPage(makeHandler());
    element = el;

    const scopeSelect = el.shadowRoot?.querySelector('sl-select') as HTMLElement & {
      value: string;
    };
    scopeSelect.value = 'project';
    scopeSelect.dispatchEvent(new Event('sl-change'));
    await settled(el);

    // All 3 project skills shown initially
    expect(shownSkills(el)).toEqual(['/skills/s1', '/skills/s2', '/skills/s3']);

    // Select a single project
    const picker = el.shadowRoot?.querySelector('scion-project-picker.project-filter');
    expect(picker).not.toBeNull();
    picker?.dispatchEvent(
      new CustomEvent('projects-change', {
        detail: { projectIds: ['proj-1'], projects: [PROJECTS[0]] },
        bubbles: true,
        composed: true,
      })
    );
    await settled(el);

    expect(shownSkills(el)).toEqual(['/skills/s1']);
    expect(new URLSearchParams(window.location.search).get('projects')).toBe('proj-1');
    expect(fetchMock.mock.calls.some((c) => urlOf(c[0] as string).includes('scopeId=proj-1'))).toBe(
      true
    );
    // Create Skill button carries the single selected project context
    expect(el.shadowRoot?.querySelector('a[href="/skills/new?projectId=proj-1"]')).not.toBeNull();

    // Select two projects
    picker?.dispatchEvent(
      new CustomEvent('projects-change', {
        detail: { projectIds: ['proj-1', 'proj-3'], projects: [PROJECTS[0], PROJECTS[2]] },
        bubbles: true,
        composed: true,
      })
    );
    await settled(el);

    expect(shownSkills(el)).toEqual(['/skills/s1', '/skills/s3']);
    expect(new URLSearchParams(window.location.search).get('projects')).toBe('proj-1,proj-3');
    expect(el.shadowRoot?.querySelector('a[href="/skills/new"]')).not.toBeNull();
  });

  it('restores scope and multi-project filter from URL on load', async () => {
    window.history.replaceState({}, '', '/skills?scope=project&projects=proj-2,proj-3');
    const { el } = await mountSkillsPage(makeHandler());
    element = el;

    expect(internals(el).scopeFilter).toBe('project');
    expect(shownSkills(el)).toEqual(['/skills/s2', '/skills/s3']);

    const picker = el.shadowRoot?.querySelector('scion-project-picker.project-filter') as
      | (HTMLElement & { values: string[] })
      | null;
    expect(picker).not.toBeNull();
    expect(picker?.values).toEqual(['proj-2', 'proj-3']);
  });

  it('defaults to project scope with current project when opened with ?projectId=', async () => {
    window.history.replaceState({}, '', '/skills?projectId=proj-2');
    const { el } = await mountSkillsPage(makeHandler());
    element = el;

    expect(internals(el).scopeFilter).toBe('project');
    expect(shownSkills(el)).toEqual(['/skills/s2']);
    const picker = el.shadowRoot?.querySelector('scion-project-picker.project-filter') as
      | (HTMLElement & { values: string[] })
      | null;
    expect(picker?.values).toEqual(['proj-2']);
  });
});

describe('skills page icon', () => {
  let element: PageEl | null = null;

  beforeAll(async () => {
    await import('./skills.js');
  }, 30_000);

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  it('uses the book icon in empty state, grid cards, and table rows', async () => {
    const empty = await mountSkillsPage(() => Promise.resolve(jsonResponse({ skills: [] })));
    element = empty.el;
    expect(empty.el.shadowRoot?.querySelector('.empty-state sl-icon')?.getAttribute('name')).toBe(
      'book'
    );
    empty.el.remove();

    const populated = await mountSkillsPage(() =>
      Promise.resolve(jsonResponse({ skills: [skill('s1')] }))
    );
    element = populated.el;
    expect(
      populated.el.shadowRoot
        ?.querySelector('.skill-card .resource-name sl-icon')
        ?.getAttribute('name')
    ).toBe('book');

    // Switch to table view and verify row icon
    (populated.el as unknown as { viewMode: 'grid' | 'table' }).viewMode = 'table';
    await populated.el.updateComplete;
    expect(
      populated.el.shadowRoot?.querySelector('tbody .name-cell sl-icon')?.getAttribute('name')
    ).toBe('book');
  });
});
