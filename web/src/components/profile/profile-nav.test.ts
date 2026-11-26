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
 * Tests for profile-nav.ts: the user's own hub role badge (design §5.G).
 * Also the pending secrets count badge on the Secrets link.
 * Display only; the badge text and tooltip come from user.role.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

import type { User } from '../../shared/types.js';
import { showToast } from '../../utils/toast.js';

vi.mock('../../utils/toast.js', () => ({
  showToast: vi.fn(),
}));

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function createNav(user: User | null, collapsed = false): Promise<HTMLElement> {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(jsonResponse({ configured: false, projects: [] })))
  );
  const el = document.createElement('scion-profile-nav') as HTMLElement & {
    updateComplete: Promise<boolean>;
    user: User | null;
    collapsed: boolean;
  };
  el.user = user;
  el.collapsed = collapsed;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function user(role?: string): User {
  return {
    id: 'u-1',
    email: 'dev@example.com',
    name: 'Development User',
    ...(role !== undefined ? { role } : {}),
  } as User;
}

function badge(el: HTMLElement): HTMLElement | null {
  return el.shadowRoot?.querySelector<HTMLElement>('.user-details .role-badge') ?? null;
}

function tooltip(el: HTMLElement): HTMLElement | null {
  return el.shadowRoot?.querySelector<HTMLElement>('.user-details sl-tooltip') ?? null;
}

describe('scion-profile-nav — hub role badge', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({})))
    );
    await import('./profile-nav.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it.each([
    [
      'viewer',
      'Hub Viewer',
      "Can use projects you've been added to, but cannot create new projects.",
    ],
    ['member', 'Hub Member', 'Can create projects and agents.'],
    ['admin', 'Hub Admin', 'Full administrative access to this hub.'],
  ])('renders the %s badge with its tooltip', async (role, label, description) => {
    element = await createNav(user(role));

    const b = badge(element);
    expect(b).not.toBeNull();
    expect(b?.textContent?.trim()).toBe(label);
    expect(b?.classList.contains(role)).toBe(true);

    const t = tooltip(element);
    expect(t).not.toBeNull();
    expect(t?.getAttribute('content')).toBe(description);
    expect(t?.contains(b as Node)).toBe(true);
  });

  it('shows the badge under the name and email in the user details block', async () => {
    element = await createNav(user('viewer'));

    const details = element.shadowRoot?.querySelector('.user-details');
    const text = details?.textContent?.replace(/\s+/g, ' ') ?? '';
    expect(text).toContain('Development User');
    expect(text).toContain('dev@example.com');
    expect(text.indexOf('dev@example.com')).toBeLessThan(text.indexOf('Hub Viewer'));
  });

  it('does not render inline explanation text (tooltip only)', async () => {
    element = await createNav(user('viewer'));

    const details = element.shadowRoot?.querySelector('.user-details');
    // The description lives only in the tooltip's content attribute.
    expect(details?.textContent).not.toContain('cannot create new projects');
  });

  it.each([[undefined], [''], ['superuser'], ['toString']])(
    'renders no badge for unknown or missing role %j',
    async (role) => {
      element = await createNav(user(role));

      expect(badge(element)).toBeNull();
      expect(tooltip(element)).toBeNull();
      // The rest of the user block still renders.
      expect(element.shadowRoot?.querySelector('.user-name')?.textContent).toContain(
        'Development User'
      );
    }
  );

  it('hides the badge while the sidebar is collapsed', async () => {
    element = await createNav(user('member'), true);

    expect(badge(element)).toBeNull();
  });

  it('shows the badge again when the sidebar expands', async () => {
    element = await createNav(user('member'), true);
    const el = element as HTMLElement & { collapsed: boolean; updateComplete: Promise<boolean> };
    el.collapsed = false;
    await el.updateComplete;

    expect(badge(element)?.textContent?.trim()).toBe('Hub Member');
  });

  it('renders no user block (and no badge) without a user', async () => {
    element = await createNav(null);

    expect(element.shadowRoot?.querySelector('.user-info')).toBeNull();
    expect(badge(element)).toBeNull();
  });
});

describe('scion-profile-nav — GitHub Application link (ptone/scion#5311)', () => {
  const INSTALL_URL = 'https://github.com/apps/scion/installations/new';
  let element: HTMLElement | null = null;
  let projectRequests: string[] = [];

  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({})))
    );
    await import('./profile-nav.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  /**
   * Mounts the nav against a hub whose app config is `app` and whose
   * `mine=true` project list is `pages` (one array of projects per page,
   * chained with cursors), then waits for the GitHub App check to finish.
   */
  async function mount(
    app: unknown,
    pages: Array<Array<{ id: string; githubInstallationId?: number }>>,
    projectsStatus = 200
  ): Promise<HTMLElement> {
    projectRequests = [];
    let appDone = false;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string) => {
        if (input === '/api/v1/github-app') {
          appDone = true;
          return Promise.resolve(jsonResponse(app));
        }
        if (input.startsWith('/api/v1/projects')) {
          projectRequests.push(input);
          const cursor = new URL(input, 'http://localhost').searchParams.get('cursor');
          const index = cursor ? Number(cursor) : 0;
          const next = index + 1 < pages.length ? String(index + 1) : undefined;
          return Promise.resolve(
            jsonResponse({ projects: pages[index] ?? [], nextCursor: next }, projectsStatus)
          );
        }
        return Promise.resolve(jsonResponse({}));
      })
    );
    const el = document.createElement('scion-profile-nav') as HTMLElement & {
      updateComplete: Promise<boolean>;
      user: User | null;
    };
    el.user = user('member');
    document.body.appendChild(el);
    await vi.waitFor(() => expect(appDone).toBe(true));
    // Let the project walk (if any) settle.
    for (let i = 0; i < 20; i++) await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
    return el;
  }

  function githubLink(el: HTMLElement): HTMLAnchorElement | null {
    return el.shadowRoot?.querySelector<HTMLAnchorElement>('a.nav-link-external') ?? null;
  }

  function page(n: number, matchAt = -1): Array<{ id: string; githubInstallationId?: number }> {
    return Array.from({ length: n }, (_, i) =>
      i === matchAt ? { id: `p${i}`, githubInstallationId: 7 } : { id: `p${i}` }
    );
  }

  it('an unconfigured app sends no projects request and shows no link', async () => {
    element = await mount({ configured: false }, [page(3, 0)]);
    expect(projectRequests).toEqual([]);
    expect(githubLink(element)).toBeNull();
  });

  it('a configured app without an installation URL sends no projects request', async () => {
    element = await mount({ configured: true }, [page(3, 0)]);
    expect(projectRequests).toEqual([]);
    expect(githubLink(element)).toBeNull();
  });

  it('a match on the first page shows the link after one bounded request', async () => {
    element = await mount({ configured: true, installation_url: INSTALL_URL }, [page(100, 5)]);
    expect(projectRequests).toEqual(['/api/v1/projects?mine=true&limit=100']);
    expect(githubLink(element)?.getAttribute('href')).toBe(INSTALL_URL);
  });

  it('a match on a later page is found by following the cursor, then stops', async () => {
    element = await mount({ configured: true, installation_url: INSTALL_URL }, [
      page(100),
      page(100),
      page(100, 42),
      page(100),
    ]);
    expect(projectRequests).toHaveLength(3);
    expect(projectRequests[2]).toBe('/api/v1/projects?mine=true&limit=100&cursor=2');
    expect(githubLink(element)?.getAttribute('href')).toBe(INSTALL_URL);
  });

  it('no matching project shows no link', async () => {
    element = await mount({ configured: true, installation_url: INSTALL_URL }, [
      page(100),
      page(10),
    ]);
    expect(projectRequests).toHaveLength(2);
    expect(githubLink(element)).toBeNull();
  });

  it('reads at most 500 projects, the window the former unbounded request saw', async () => {
    element = await mount({ configured: true, installation_url: INSTALL_URL }, [
      page(100),
      page(100),
      page(100),
      page(100),
      page(100),
      page(100, 0),
    ]);
    expect(projectRequests).toHaveLength(5);
    expect(githubLink(element)).toBeNull();
  });

  it('an empty page ends the scan even when it carries a cursor', async () => {
    element = await mount({ configured: true, installation_url: INSTALL_URL }, [
      page(100),
      [],
      page(100, 0),
    ]);
    expect(projectRequests).toHaveLength(2);
    expect(githubLink(element)).toBeNull();
  });

  it('a failed projects request shows no link', async () => {
    element = await mount({ configured: true, installation_url: INSTALL_URL }, [page(3, 0)], 500);
    expect(githubLink(element)).toBeNull();
  });
});

describe('scion-profile-nav — pending secrets badge', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({})))
    );
    await import('./profile-nav.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  async function mount(pendingSecrets: unknown[] | null): Promise<HTMLElement> {
    let listed = false;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string) => {
        if (input === '/api/v1/pending-secrets') {
          listed = true;
          return Promise.resolve(
            pendingSecrets === null
              ? jsonResponse({ error: { code: 'forbidden' } }, 403)
              : jsonResponse({ pendingSecrets })
          );
        }
        return Promise.resolve(jsonResponse({ configured: false }));
      })
    );
    const el = document.createElement('scion-profile-nav') as HTMLElement & {
      updateComplete: Promise<boolean>;
      user: User | null;
    };
    el.user = user('member');
    document.body.appendChild(el);
    await vi.waitFor(() => expect(listed).toBe(true));
    for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
    return el;
  }

  function secretsBadge(el: HTMLElement): HTMLElement | null {
    return (
      el.shadowRoot?.querySelector<HTMLElement>('a[href="/profile/secrets"] .nav-badge') ?? null
    );
  }

  it('shows the number of pending secrets on the Secrets link', async () => {
    element = await mount([{ id: 'p-1' }, { id: 'p-2' }]);
    const b = secretsBadge(element);
    expect(b?.textContent?.trim()).toBe('2');
    expect(b?.getAttribute('aria-label')).toBe('2 secrets await approval');
    expect(element.shadowRoot?.querySelectorAll('.nav-badge')).toHaveLength(1);
  });

  it('shows no badge without pending secrets or when the list cannot be read', async () => {
    element = await mount([]);
    expect(secretsBadge(element)).toBeNull();
    element.remove();
    element = await mount(null);
    expect(secretsBadge(element)).toBeNull();
  });

  it('follows the count the secret list reports', async () => {
    element = await mount([{ id: 'p-1' }]);
    expect(secretsBadge(element)?.textContent?.trim()).toBe('1');
    window.dispatchEvent(
      new CustomEvent('scion:pending-secrets-changed', { detail: { count: 0 } })
    );
    await (element as HTMLElement & { updateComplete: Promise<boolean> }).updateComplete;
    expect(secretsBadge(element)).toBeNull();
  });
});

describe('scion-profile-nav — Decisions entry & badge (ptone/scion#7996)', () => {
  let element: HTMLElement | null = null;
  let secondaryElement: HTMLElement | null = null;
  let decisionsProbeCount = 0;

  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({})))
    );
    await import('./profile-nav.js');
  });

  afterEach(async () => {
    element?.remove();
    element = null;
    secondaryElement?.remove();
    secondaryElement = null;
    const { resetProfileDecisionProbeCacheForTests } =
      await import('../../client/profile-decisions.js');
    resetProfileDecisionProbeCacheForTests();
    const flags = await import('../../utils/feature-flags.js');
    delete window.__SCION_FEATURES__;
    flags.resetServerFlagStateForTests();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  async function mountWithDecisions(options: {
    flagsEnabled?: boolean;
    probeStatus?: number;
    waitingOnYou?: number;
    currentPath?: string;
  }): Promise<HTMLElement> {
    decisionsProbeCount = 0;
    const { resetProfileDecisionProbeCacheForTests } =
      await import('../../client/profile-decisions.js');
    resetProfileDecisionProbeCacheForTests();
    const flags = await import('../../utils/feature-flags.js');
    delete window.__SCION_FEATURES__;
    flags.resetServerFlagStateForTests();
    if (options.flagsEnabled ?? true) {
      flags.setFeatureFlag(flags.DECISIONS_FLAG, true);
    }

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (raw.startsWith('/api/v1/users/me/decisions')) {
          decisionsProbeCount++;
          if (options.probeStatus === 404) {
            return Promise.resolve(jsonResponse({ error: { code: 'not_found' } }, 404));
          }
          return Promise.resolve(
            jsonResponse(
              {
                counts: {
                  waitingOnYou: options.waitingOnYou ?? 0,
                  waitingOnOthers: 2,
                },
              },
              options.probeStatus ?? 200
            )
          );
        }
        return Promise.resolve(jsonResponse({ configured: false, pendingSecrets: [] }));
      })
    );

    const el = document.createElement('scion-profile-nav') as HTMLElement & {
      updateComplete: Promise<boolean>;
      user: User | null;
      currentPath: string;
    };
    el.user = user('member');
    el.currentPath = options.currentPath ?? '/profile/env';
    document.body.appendChild(el);
    for (let i = 0; i < 10; i++) await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
    return el;
  }

  function decisionsLink(el: HTMLElement): HTMLAnchorElement | null {
    return el.shadowRoot?.querySelector<HTMLAnchorElement>('a[href="/profile/decisions"]') ?? null;
  }

  function decisionsBadge(el: HTMLElement): HTMLElement | null {
    return (
      el.shadowRoot?.querySelector<HTMLElement>(
        'a[href="/profile/decisions"] [data-decisions-nav-badge]'
      ) ?? null
    );
  }

  it('hides Decisions link and makes no probe request when flags are off', async () => {
    element = await mountWithDecisions({ flagsEnabled: false, waitingOnYou: 4 });
    expect(decisionsLink(element)).toBeNull();
    expect(decisionsProbeCount).toBe(0);
  });

  it('renders Decisions at the top of profile nav with unfiltered waitingOnYou badge and active query matching', async () => {
    element = await mountWithDecisions({
      flagsEnabled: true,
      waitingOnYou: 4,
      currentPath: '/profile/decisions?tab=pending&sort=project',
    });

    const link = decisionsLink(element);
    expect(link).not.toBeNull();
    expect(link?.textContent).toContain('Decisions');
    expect(link?.classList.contains('active')).toBe(true);

    // Must appear before Configuration section
    const navHtml = element.shadowRoot?.querySelector('nav.nav-container')?.innerHTML ?? '';
    expect(navHtml.indexOf('/profile/decisions')).toBeGreaterThanOrEqual(0);
    expect(navHtml.indexOf('/profile/decisions')).toBeLessThan(navHtml.indexOf('Configuration'));

    const badgeEl = decisionsBadge(element);
    expect(badgeEl?.textContent?.trim()).toBe('4');
  });

  it('hides the Decisions nav item when GET /api/v1/users/me/decisions?limit=0 returns 404 (§7)', async () => {
    element = await mountWithDecisions({
      flagsEnabled: true,
      probeStatus: 404,
    });
    expect(decisionsProbeCount).toBe(1);
    expect(decisionsLink(element)).toBeNull();
  });

  it('hides badge when waitingOnYou is 0 and updates live on count-changed event', async () => {
    element = await mountWithDecisions({
      flagsEnabled: true,
      waitingOnYou: 0,
    });
    expect(decisionsLink(element)).not.toBeNull();
    expect(decisionsBadge(element)).toBeNull();

    const { dispatchProfileDecisionsCountChanged } =
      await import('../../client/profile-decisions.js');
    dispatchProfileDecisionsCountChanged(3);
    await (element as HTMLElement & { updateComplete: Promise<boolean> }).updateComplete;
    expect(decisionsBadge(element)?.textContent?.trim()).toBe('3');

    dispatchProfileDecisionsCountChanged(0);
    await (element as HTMLElement & { updateComplete: Promise<boolean> }).updateComplete;
    expect(decisionsBadge(element)).toBeNull();
  });

  it('shares a single ?limit=0 probe across dual nav instances and route changes, refreshing on SSE events', async () => {
    let liveWaiting = 4;
    decisionsProbeCount = 0;
    const { resetProfileDecisionProbeCacheForTests } =
      await import('../../client/profile-decisions.js');
    resetProfileDecisionProbeCacheForTests();
    const flags = await import('../../utils/feature-flags.js');
    delete window.__SCION_FEATURES__;
    flags.resetServerFlagStateForTests();
    flags.setFeatureFlag(flags.DECISIONS_FLAG, true);

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (raw.startsWith('/api/v1/users/me/decisions')) {
          decisionsProbeCount++;
          return Promise.resolve(
            jsonResponse({ counts: { waitingOnYou: liveWaiting, waitingOnOthers: 1 } }, 200)
          );
        }
        return Promise.resolve(jsonResponse({ configured: false, pendingSecrets: [] }));
      })
    );

    type NavEl = HTMLElement & {
      updateComplete: Promise<boolean>;
      user: User | null;
      currentPath: string;
    };
    const desktopNav = document.createElement('scion-profile-nav') as NavEl;
    desktopNav.user = user('member');
    desktopNav.currentPath = '/profile/env';

    const drawerNav = document.createElement('scion-profile-nav') as NavEl;
    drawerNav.user = user('member');
    drawerNav.currentPath = '/profile/env';

    element = desktopNav;
    secondaryElement = drawerNav;
    document.body.appendChild(desktopNav);
    document.body.appendChild(drawerNav);

    for (let i = 0; i < 10; i++) await new Promise((r) => setTimeout(r, 0));
    await desktopNav.updateComplete;
    await drawerNav.updateComplete;

    expect(decisionsProbeCount).toBe(1);
    expect(decisionsBadge(desktopNav)?.textContent?.trim()).toBe('4');
    expect(decisionsBadge(drawerNav)?.textContent?.trim()).toBe('4');

    // Navigating across /profile/* routes does NOT refetch ?limit=0
    desktopNav.currentPath = '/profile/secrets';
    drawerNav.currentPath = '/profile/secrets';
    await desktopNav.updateComplete;
    await drawerNav.updateComplete;
    expect(decisionsProbeCount).toBe(1);

    // Remounting another <scion-profile-nav> reuses the cached probe without refetching
    drawerNav.remove();
    const remountedNav = document.createElement('scion-profile-nav') as NavEl;
    remountedNav.user = user('member');
    remountedNav.currentPath = '/profile/tokens';
    secondaryElement = remountedNav;
    document.body.appendChild(remountedNav);
    await remountedNav.updateComplete;
    expect(decisionsProbeCount).toBe(1);
    expect(decisionsBadge(remountedNav)?.textContent?.trim()).toBe('4');

    // Generic notification-created events do NOT re-probe ?limit=0
    liveWaiting = 6;
    const { stateManager } = await import('../../client/state.js');
    stateManager.dispatchEvent(new CustomEvent('notification-created', { detail: {} }));
    await new Promise((r) => setTimeout(r, 140));
    expect(decisionsProbeCount).toBe(1);

    // SSE decision-updated triggers ONE debounced shared refresh for both nav instances
    stateManager.dispatchEvent(new CustomEvent('decision-updated', { detail: {} }));
    await new Promise((r) => setTimeout(r, 140));
    await desktopNav.updateComplete;
    await remountedNav.updateComplete;

    expect(decisionsProbeCount).toBe(2);
    expect(decisionsBadge(desktopNav)?.textContent?.trim()).toBe('6');
    expect(decisionsBadge(remountedNav)?.textContent?.trim()).toBe('6');

    // Real Hub chat-message-received payload ({ data: { type: 'decision-prompt', decisions } }) triggers refresh;
    // plain chat message does not
    liveWaiting = 7;
    stateManager.dispatchEvent(
      new CustomEvent('chat-message-received', {
        detail: { data: { id: 'm-plain', type: 'message' } },
      })
    );
    await new Promise((r) => setTimeout(r, 140));
    expect(decisionsProbeCount).toBe(2);

    stateManager.dispatchEvent(
      new CustomEvent('chat-message-received', {
        detail: {
          data: { id: 'm-choice', type: 'decision-prompt', decisions: [{ rowId: 'r-new' }] },
        },
      })
    );
    await new Promise((r) => setTimeout(r, 140));
    await desktopNav.updateComplete;
    await remountedNav.updateComplete;

    expect(decisionsProbeCount).toBe(3);
    expect(decisionsBadge(desktopNav)?.textContent?.trim()).toBe('7');
    expect(decisionsBadge(remountedNav)?.textContent?.trim()).toBe('7');
  });

  it('re-probes on remount after off-nav decision-updated or TTL expiry while staying probe-free when fresh', async () => {
    let now = 2_000_000;
    const nowSpy = vi.spyOn(Date, 'now').mockImplementation(() => now);
    let liveWaiting = 3;
    decisionsProbeCount = 0;

    const { PROFILE_DECISION_PROBE_TTL_MS, resetProfileDecisionProbeCacheForTests } =
      await import('../../client/profile-decisions.js');
    resetProfileDecisionProbeCacheForTests();
    const { stateManager } = await import('../../client/state.js');
    const flags = await import('../../utils/feature-flags.js');
    delete window.__SCION_FEATURES__;
    flags.resetServerFlagStateForTests();
    flags.setFeatureFlag(flags.DECISIONS_FLAG, true);

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (raw.startsWith('/api/v1/users/me/decisions')) {
          decisionsProbeCount++;
          return Promise.resolve(
            jsonResponse({ counts: { waitingOnYou: liveWaiting, waitingOnOthers: 0 } }, 200)
          );
        }
        return Promise.resolve(jsonResponse({ configured: false, pendingSecrets: [] }));
      })
    );

    type NavEl = HTMLElement & {
      updateComplete: Promise<boolean>;
      user: User | null;
      currentPath: string;
    };

    try {
      // 1. Mount on /profile/settings -> 1 probe, badge shows 3
      const nav1 = document.createElement('scion-profile-nav') as NavEl;
      nav1.user = user('member');
      nav1.currentPath = '/profile/settings';
      element = nav1;
      document.body.appendChild(nav1);
      for (let i = 0; i < 10; i++) await new Promise((r) => setTimeout(r, 0));
      await nav1.updateComplete;
      expect(decisionsProbeCount).toBe(1);
      expect(decisionsBadge(nav1)?.textContent?.trim()).toBe('3');

      // 2. Navigate away to /chat or /projects/<id>/decisions (nav unmounts) and answer all 3 choices
      nav1.remove();
      element = null;
      liveWaiting = 0;
      stateManager.dispatchEvent(new CustomEvent('decision-updated', { detail: {} }));
      expect(decisionsProbeCount).toBe(1);

      // 3. Return to /profile/settings -> remounted nav re-probes once and clears the stale badge
      const nav2 = document.createElement('scion-profile-nav') as NavEl;
      nav2.user = user('member');
      nav2.currentPath = '/profile/settings';
      element = nav2;
      document.body.appendChild(nav2);
      // Stale count 3 is NOT shown while the refresh is in flight
      await nav2.updateComplete;
      expect(decisionsBadge(nav2)).toBeNull();
      for (let i = 0; i < 10; i++) await new Promise((r) => setTimeout(r, 0));
      await nav2.updateComplete;
      expect(decisionsProbeCount).toBe(2);
      expect(decisionsLink(nav2)).not.toBeNull();
      expect(decisionsBadge(nav2)).toBeNull();

      // 4. Unmount and remount after PROFILE_DECISION_PROBE_TTL_MS -> re-probes once (and dedupes dual navs)
      nav2.remove();
      element = null;
      liveWaiting = 2;
      now += PROFILE_DECISION_PROBE_TTL_MS + 100;

      const desktop = document.createElement('scion-profile-nav') as NavEl;
      desktop.user = user('member');
      desktop.currentPath = '/profile/settings';
      const drawer = document.createElement('scion-profile-nav') as NavEl;
      drawer.user = user('member');
      drawer.currentPath = '/profile/settings';
      element = desktop;
      secondaryElement = drawer;
      document.body.appendChild(desktop);
      document.body.appendChild(drawer);
      for (let i = 0; i < 10; i++) await new Promise((r) => setTimeout(r, 0));
      await desktop.updateComplete;
      await drawer.updateComplete;

      expect(decisionsProbeCount).toBe(3);
      expect(decisionsBadge(desktop)?.textContent?.trim()).toBe('2');
      expect(decisionsBadge(drawer)?.textContent?.trim()).toBe('2');
    } finally {
      nowSpy.mockRestore();
    }
  });

  it('hides Decisions before the first probe resolves (no flash on 404 hubs) while keeping entry visible on transient 500 (§7)', async () => {
    const { resetProfileDecisionProbeCacheForTests } =
      await import('../../client/profile-decisions.js');
    resetProfileDecisionProbeCacheForTests();
    const flags = await import('../../utils/feature-flags.js');
    delete window.__SCION_FEATURES__;
    flags.resetServerFlagStateForTests();
    flags.setFeatureFlag(flags.DECISIONS_FLAG, true);

    let resolveProbe!: (r: Response) => void;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (raw.startsWith('/api/v1/users/me/decisions')) {
          return new Promise<Response>((r) => {
            resolveProbe = r;
          });
        }
        return Promise.resolve(jsonResponse({ configured: false, pendingSecrets: [] }));
      })
    );

    type NavEl = HTMLElement & {
      updateComplete: Promise<boolean>;
      user: User | null;
      currentPath: string;
    };
    const nav404 = document.createElement('scion-profile-nav') as NavEl;
    nav404.user = user('member');
    nav404.currentPath = '/profile/settings';
    element = nav404;
    document.body.appendChild(nav404);
    await nav404.updateComplete;

    // While initial probe is unresolved: hidden (no flash!)
    expect(decisionsLink(nav404)).toBeNull();
    resolveProbe(jsonResponse({ error: { code: 'not_found' } }, 404));
    for (let i = 0; i < 10; i++) await new Promise((r) => setTimeout(r, 0));
    await nav404.updateComplete;
    expect(decisionsLink(nav404)).toBeNull();

    // Transient 500 on first probe: hidden while in flight, then visible without badge per §7
    nav404.remove();
    element = null;
    resetProfileDecisionProbeCacheForTests();
    const nav500 = await mountWithDecisions({
      flagsEnabled: true,
      probeStatus: 500,
    });
    element = nav500;
    expect(decisionsLink(nav500)).not.toBeNull();
    expect(decisionsBadge(nav500)).toBeNull();
  });

  it('opens the profile SSE scope with user.<id>.chat.> and refreshes badge on DM decision.updated events', async () => {
    class FakeEventSource extends EventTarget {
      static instances: FakeEventSource[] = [];
      readyState = 0;
      onopen: ((ev: Event) => void) | null = null;
      onerror: ((ev: Event) => void) | null = null;
      constructor(readonly url: string) {
        super();
        FakeEventSource.instances.push(this);
      }
      close(): void {
        this.readyState = 2;
      }
    }
    vi.stubGlobal('EventSource', FakeEventSource);

    let liveWaiting = 2;
    decisionsProbeCount = 0;
    const { resetProfileDecisionProbeCacheForTests } =
      await import('../../client/profile-decisions.js');
    resetProfileDecisionProbeCacheForTests();
    const { stateManager } = await import('../../client/state.js');
    const flags = await import('../../utils/feature-flags.js');
    delete window.__SCION_FEATURES__;
    flags.resetServerFlagStateForTests();
    flags.setFeatureFlag(flags.DECISIONS_FLAG, true);

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (raw.startsWith('/api/v1/users/me/decisions')) {
          decisionsProbeCount++;
          return Promise.resolve(
            jsonResponse({ counts: { waitingOnYou: liveWaiting, waitingOnOthers: 0 } }, 200)
          );
        }
        return Promise.resolve(jsonResponse({ configured: false, pendingSecrets: [] }));
      })
    );

    const nav = document.createElement('scion-profile-nav') as HTMLElement & {
      updateComplete: Promise<boolean>;
      user: User | null;
      currentPath: string;
    };
    nav.user = user('member');
    nav.currentPath = '/profile/settings';
    element = nav;
    document.body.appendChild(nav);
    for (let i = 0; i < 10; i++) await new Promise((r) => setTimeout(r, 0));
    await nav.updateComplete;

    const latestUrl = FakeEventSource.instances[FakeEventSource.instances.length - 1]?.url ?? '';
    expect(decodeURIComponent(latestUrl)).toContain('sub=user.u-1.chat.>');
    expect(decisionsBadge(nav)?.textContent?.trim()).toBe('2');

    // Feed a DM user.u-1.chat.decision.updated event through stateManager
    liveWaiting = 1;
    (
      stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
    ).handleUpdate({
      subject: 'user.u-1.chat.decision.updated',
      data: { messageId: 'm-dm', threadId: 'dm:agent:a:user:u-1', decision: { rowId: 'r-dm' } },
    });
    await new Promise((r) => setTimeout(r, 140));
    await nav.updateComplete;

    expect(decisionsProbeCount).toBe(2);
    expect(decisionsBadge(nav)?.textContent?.trim()).toBe('1');
    stateManager.disconnect();
  });
});

describe('scion-profile-nav — View as member switch (ptone/scion#9074)', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({})))
    );
    await import('./profile-nav.js');
  });

  afterEach(async () => {
    element?.remove();
    element = null;
    const { clearAdminStatus } = await import('../../client/admin-status.js');
    clearAdminStatus();
    const flags = await import('../../utils/feature-flags.js');
    delete window.__SCION_FEATURES__;
    flags.resetServerFlagStateForTests();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  async function mountSwitchTest(opts: {
    flagOn: boolean;
    isAdmin: boolean;
    viewAsMember: boolean;
    role?: 'admin' | 'member' | 'viewer';
    collapsed?: boolean;
  }): Promise<HTMLElement> {
    const { clearAdminStatus } = await import('../../client/admin-status.js');
    clearAdminStatus();
    const flags = await import('../../utils/feature-flags.js');
    delete window.__SCION_FEATURES__;
    flags.resetServerFlagStateForTests();
    if (opts.flagOn) {
      flags.setFeatureFlag(flags.VIEW_AS_MEMBER_FLAG, true);
    }

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (raw.includes('/api/v1/auth/admin-status')) {
          return Promise.resolve(
            jsonResponse({
              isAdmin: opts.isAdmin,
              isSuperAdmin: opts.isAdmin,
              permissions: [],
              ...(opts.viewAsMember ? { viewAsMember: true } : {}),
            })
          );
        }
        return Promise.resolve(jsonResponse({ configured: false, pendingSecrets: [] }));
      })
    );

    const el = document.createElement('scion-profile-nav') as HTMLElement & {
      updateComplete: Promise<boolean>;
      user: User | null;
      collapsed: boolean;
    };
    el.collapsed = opts.collapsed ?? false;
    el.user = {
      id: 'u-vam',
      email: 'admin@example.com',
      name: 'Admin User',
      role: opts.role ?? (opts.viewAsMember ? 'member' : opts.isAdmin ? 'admin' : 'member'),
      ...(opts.viewAsMember ? { viewAsMember: true } : {}),
    };
    document.body.appendChild(el);
    for (let i = 0; i < 8; i++) await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
    return el;
  }

  function vamSwitch(el: HTMLElement): (HTMLElement & { checked?: boolean }) | null {
    return el.shadowRoot?.querySelector('[data-testid="view-as-member-switch"]') ?? null;
  }

  it.each([
    {
      label: 'flag off, member, vam off -> hidden',
      flagOn: false,
      isAdmin: false,
      viewAsMember: false,
      expected: false,
    },
    {
      label: 'flag off, admin, vam off -> hidden',
      flagOn: false,
      isAdmin: true,
      viewAsMember: false,
      expected: false,
    },
    {
      label: 'flag on, member, vam off -> hidden',
      flagOn: true,
      isAdmin: false,
      viewAsMember: false,
      expected: false,
    },
    {
      label: 'flag on, admin, vam off -> visible (unchecked)',
      flagOn: true,
      isAdmin: true,
      viewAsMember: false,
      expected: true,
    },
    {
      label: 'flag off, member/clamped, vam on -> visible (checked)',
      flagOn: false,
      isAdmin: false,
      viewAsMember: true,
      expected: true,
    },
    {
      label: 'flag on, member/clamped, vam on -> visible (checked)',
      flagOn: true,
      isAdmin: false,
      viewAsMember: true,
      expected: true,
    },
  ])('$label', async ({ flagOn, isAdmin, viewAsMember, expected }) => {
    element = await mountSwitchTest({ flagOn, isAdmin, viewAsMember });
    const sw = vamSwitch(element);
    if (!expected) {
      expect(sw).toBeNull();
    } else {
      expect(sw).not.toBeNull();
      expect(sw?.hasAttribute('checked')).toBe(viewAsMember);
      expect(element.shadowRoot?.textContent).toContain(
        'Drop hub admin access in this browser session'
      );
    }
  });

  it('shows "Hub Member (viewing as member)" badge when viewAsMember is true', async () => {
    element = await mountSwitchTest({ flagOn: true, isAdmin: false, viewAsMember: true });
    const b = badge(element);
    expect(b?.textContent?.trim()).toBe('Hub Member (viewing as member)');
    expect(b?.classList.contains('member')).toBe(true);
  });

  it('hides the switch while the sidebar is collapsed', async () => {
    element = await mountSwitchTest({
      flagOn: true,
      isAdmin: true,
      viewAsMember: false,
      collapsed: true,
    });
    expect(vamSwitch(element)).toBeNull();
  });

  it('toggling the switch ON posts {enabled:true} to /auth/view-as-member', async () => {
    element = await mountSwitchTest({ flagOn: true, isAdmin: true, viewAsMember: false });
    const postCalls: Array<{ method: string | undefined; body: unknown }> = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (raw.endsWith('/auth/view-as-member')) {
          postCalls.push({
            method: init?.method,
            body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined,
          });
          return Promise.resolve(jsonResponse({ viewAsMember: true }));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );

    const sw = vamSwitch(element)!;
    sw.checked = true;
    sw.dispatchEvent(new Event('sl-change', { bubbles: true }));

    await vi.waitFor(() => expect(postCalls).toHaveLength(1));
    expect(postCalls[0]).toEqual({ method: 'POST', body: { enabled: true } });
  });

  it('reverts checked state and shows a danger toast when the toggle POST fails', async () => {
    element = await mountSwitchTest({ flagOn: true, isAdmin: true, viewAsMember: false });
    vi.spyOn(console, 'error').mockImplementation(() => {});
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (raw.endsWith('/auth/view-as-member')) {
          return Promise.resolve(jsonResponse({ error: { message: 'forbidden' } }, 403));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );

    const sw = vamSwitch(element)!;
    sw.checked = true;
    sw.dispatchEvent(new Event('sl-change', { bubbles: true }));

    await vi.waitFor(() => expect(sw.checked).toBe(false));
    await (element as HTMLElement & { updateComplete: Promise<boolean> }).updateComplete;
    expect(sw.hasAttribute('disabled')).toBe(false);
    expect(vi.mocked(showToast)).toHaveBeenCalledWith(expect.any(String), 'danger');
  });

  it('blocks a double submit while a toggle POST is in flight', async () => {
    element = await mountSwitchTest({ flagOn: true, isAdmin: true, viewAsMember: false });
    let resolvePost!: (res: Response) => void;
    let postCount = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (raw.endsWith('/auth/view-as-member')) {
          postCount++;
          return new Promise<Response>((resolve) => {
            resolvePost = resolve;
          });
        }
        return Promise.resolve(jsonResponse({}));
      })
    );

    const sw = vamSwitch(element)!;
    sw.checked = true;
    sw.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await (element as HTMLElement & { updateComplete: Promise<boolean> }).updateComplete;
    expect(sw.hasAttribute('disabled')).toBe(true);

    // Second change while first POST is still in flight must be ignored.
    sw.dispatchEvent(new Event('sl-change', { bubbles: true }));
    expect(postCount).toBe(1);

    resolvePost(jsonResponse({ viewAsMember: true }));
    await new Promise((r) => setTimeout(r, 0));
    expect(postCount).toBe(1);
  });
});

describe('scion-profile-nav — Inbox navigation & child routes (ptone/scion#9291)', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({})))
    );
    await import('./profile-nav.js');
  });

  afterEach(async () => {
    element?.remove();
    element = null;
    const { inboxSummaryStore } = await import('../../client/inbox-summary.js');
    inboxSummaryStore.resetForTests();
    const { resetProfileDecisionProbeCacheForTests } =
      await import('../../client/profile-decisions.js');
    resetProfileDecisionProbeCacheForTests();
    const flags = await import('../../utils/feature-flags.js');
    delete window.__SCION_FEATURES__;
    flags.resetServerFlagStateForTests();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  async function mountInboxNav(options: {
    inboxEnabled: boolean;
    decisionsEnabled?: boolean;
    currentPath?: string;
    waitingOnYou?: number;
  }): Promise<HTMLElement & { updateComplete: Promise<boolean>; currentPath: string }> {
    const { inboxSummaryStore } = await import('../../client/inbox-summary.js');
    inboxSummaryStore.resetForTests();
    const { resetProfileDecisionProbeCacheForTests } =
      await import('../../client/profile-decisions.js');
    resetProfileDecisionProbeCacheForTests();
    const flags = await import('../../utils/feature-flags.js');
    delete window.__SCION_FEATURES__;
    flags.resetServerFlagStateForTests();
    if (options.inboxEnabled) {
      flags.setFeatureFlag(flags.INBOX_DASHBOARD_FLAG, true);
    }
    if (options.decisionsEnabled ?? true) {
      flags.setFeatureFlag(flags.DECISIONS_FLAG, true);
    }

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (raw.startsWith('/api/v1/users/me/decisions')) {
          return Promise.resolve(
            jsonResponse({
              counts: { waitingOnYou: options.waitingOnYou ?? 2, waitingOnOthers: 0 },
            })
          );
        }
        if (raw.startsWith('/api/v1/users/me/inbox/summary')) {
          return Promise.resolve(
            jsonResponse({
              generatedAt: '2026-11-26T12:00:00Z',
              total: 3,
              totalCapped: false,
              categories: {},
            })
          );
        }
        return Promise.resolve(jsonResponse({ configured: false, pendingSecrets: [] }));
      })
    );

    const el = document.createElement('scion-profile-nav') as HTMLElement & {
      updateComplete: Promise<boolean>;
      user: User | null;
      currentPath: string;
    };
    el.user = user('member');
    el.currentPath = options.currentPath ?? '/profile/inbox';
    document.body.appendChild(el);
    for (let i = 0; i < 10; i++) await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
    return el;
  }

  it('renders only the single Inbox item with no nested Decisions item when Inbox is on', async () => {
    const nav = await mountInboxNav({ inboxEnabled: true, decisionsEnabled: true });
    element = nav;

    const section = nav.shadowRoot?.querySelector('[data-inbox-nav-section]');
    expect(section).not.toBeNull();
    expect(section?.querySelectorAll('li.nav-item')).toHaveLength(1);
    expect(section?.querySelector('[data-inbox-nav-link]')).not.toBeNull();
    expect(nav.shadowRoot?.querySelector('[data-decisions-nav-link]')).toBeNull();
    expect(nav.shadowRoot?.querySelector('.nav-item-sub')).toBeNull();
    expect(section?.querySelector('[data-inbox-nav-badge]')?.textContent?.trim()).toBe('3');
  });

  it('keeps the Inbox item active across INBOX_CHILD_ROUTES and inactive elsewhere', async () => {
    const nav = await mountInboxNav({
      inboxEnabled: true,
      decisionsEnabled: true,
      currentPath: '/profile/inbox',
    });
    element = nav;

    const inboxLink = (): HTMLAnchorElement | null =>
      nav.shadowRoot?.querySelector<HTMLAnchorElement>('[data-inbox-nav-link]') ?? null;

    expect(inboxLink()?.classList.contains('active')).toBe(true);

    nav.currentPath = '/profile/decisions';
    await nav.updateComplete;
    expect(inboxLink()?.classList.contains('active')).toBe(true);

    nav.currentPath = '/profile/decisions?tab=pending';
    await nav.updateComplete;
    expect(inboxLink()?.classList.contains('active')).toBe(true);

    nav.currentPath = '/profile/decisions/item-1';
    await nav.updateComplete;
    expect(inboxLink()?.classList.contains('active')).toBe(true);

    nav.currentPath = '/profile/secrets';
    await nav.updateComplete;
    expect(inboxLink()?.classList.contains('active')).toBe(false);

    nav.currentPath = '/profile/env';
    await nav.updateComplete;
    expect(inboxLink()?.classList.contains('active')).toBe(false);
  });

  it('keeps the standalone Decisions section when Inbox is off', async () => {
    const nav = await mountInboxNav({
      inboxEnabled: false,
      decisionsEnabled: true,
      waitingOnYou: 5,
      currentPath: '/profile/decisions',
    });
    element = nav;

    expect(nav.shadowRoot?.querySelector('[data-inbox-nav-section]')).toBeNull();
    const decisionsLink = nav.shadowRoot?.querySelector<HTMLAnchorElement>(
      '[data-decisions-nav-link]'
    );
    expect(decisionsLink).not.toBeNull();
    expect(decisionsLink?.classList.contains('active')).toBe(true);
    expect(nav.shadowRoot?.querySelector('[data-decisions-nav-badge]')?.textContent?.trim()).toBe(
      '5'
    );
  });
});
