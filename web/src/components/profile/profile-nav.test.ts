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
 * Display only; the badge text and tooltip come from user.role.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

import type { User } from '../../shared/types.js';

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
