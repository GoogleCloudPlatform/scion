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
 * Tests for the skill create page's scope default.
 *
 * The page reads list-level capabilities from the unscoped skills list (can
 * the caller create in any scope?) and from the global skills list (can the
 * caller create global skills?). The scope defaults to global only when
 * global creation is allowed; otherwise it defaults to the caller's own user
 * scope and the Global option is disabled.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

type PageElement = HTMLElement & { updateComplete: Promise<boolean> };

/** How a stubbed skills-list request answers. */
type ListAnswer = 'create' | 'none' | 'error' | 'throw';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function requestPath(url: string | URL | Request): string {
  return typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
}

function answer(kind: ListAnswer): Promise<Response> {
  switch (kind) {
    case 'create':
      return Promise.resolve(jsonResponse({ skills: [], _capabilities: { actions: ['create'] } }));
    case 'none':
      return Promise.resolve(jsonResponse({ skills: [], _capabilities: { actions: [] } }));
    case 'error':
      return Promise.resolve(jsonResponse({ error: { code: 'internal' } }, 500));
    case 'throw':
      return Promise.reject(new TypeError('network down'));
  }
}

/** Stubs fetch: the unscoped and global skills lists answer as given. */
function stubSkillLists(anyScope: ListAnswer, globalScope: ListAnswer): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn((url: string | URL | Request): Promise<Response> => {
    const path = requestPath(url);
    if (path.includes('/api/v1/skills')) {
      return answer(path.includes('scope=global') ? globalScope : anyScope);
    }
    return Promise.resolve(jsonResponse({}, 404));
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

async function mountPage(): Promise<PageElement> {
  const el = document.createElement('scion-page-skill-create') as PageElement;
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 20));
  await el.updateComplete;
  return el;
}

function scopeOf(el: PageElement): string {
  return (el as unknown as { scope: string }).scope;
}

function globalOption(el: PageElement): Element | null {
  return el.shadowRoot?.querySelector('sl-select#scope sl-option[value="global"]') ?? null;
}

function skillListCalls(fetchMock: ReturnType<typeof vi.fn>): string[] {
  return fetchMock.mock.calls
    .map((call) => requestPath(call[0] as string | URL | Request))
    .filter((path) => path.includes('/api/v1/skills'));
}

describe('scion-page-skill-create scope default', () => {
  let element: PageElement | null = null;

  beforeAll(async () => {
    await import('./skill-create.js');
  }, 60_000);

  afterEach(() => {
    element?.remove();
    element = null;
    window.history.replaceState({}, '', '/skills/new');
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('defaults to user scope and disables Global when global create is not allowed', async () => {
    stubSkillLists('create', 'none');
    element = await mountPage();

    expect(scopeOf(element)).toBe('user');
    const option = globalOption(element);
    expect(option).not.toBeNull();
    expect(option?.hasAttribute('disabled')).toBe(true);
  });

  it('keeps global scope and the Global option when global create is allowed', async () => {
    stubSkillLists('create', 'create');
    element = await mountPage();

    expect(scopeOf(element)).toBe('global');
    const option = globalOption(element);
    expect(option).not.toBeNull();
    expect(option?.hasAttribute('disabled')).toBe(false);
  });

  it.each<ListAnswer>(['error', 'throw'])(
    'fails closed to user scope when the global request fails (%s)',
    async (failure) => {
      stubSkillLists('create', failure);
      element = await mountPage();

      expect(scopeOf(element)).toBe('user');
      expect(globalOption(element)?.hasAttribute('disabled')).toBe(true);
    }
  );

  it('renders the access denied view when the caller cannot create in any scope', async () => {
    stubSkillLists('none', 'none');
    element = await mountPage();

    const text = element.shadowRoot?.textContent?.replace(/\s+/g, ' ') ?? '';
    expect(text).toContain('You do not have permission to create skills.');
    expect(element.shadowRoot?.querySelector('sl-select#scope')).toBeNull();
  });

  it('reads the unscoped and the global skills list exactly once each on connect', async () => {
    const fetchMock = stubSkillLists('create', 'none');
    element = await mountPage();

    const calls = skillListCalls(fetchMock);
    expect(calls).toHaveLength(2);
    expect(calls.filter((path) => path.includes('scope=global'))).toHaveLength(1);
  });
});

describe('scion-page-skill-create project picker (ptone/scion#8735)', () => {
  let element: PageElement | null = null;

  beforeAll(async () => {
    await import('./skill-create.js');
  }, 60_000);

  afterEach(() => {
    element?.remove();
    element = null;
    window.history.replaceState({}, '', '/skills/new');
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  const PROJECTS = [
    {
      id: 'proj-owner',
      name: 'Owned Project',
      slug: 'owned-project',
      _capabilities: { actions: ['read', 'update', 'manage', 'delete'] },
    },
    {
      id: 'proj-admin',
      name: 'Admin Project',
      slug: 'admin-project',
      _capabilities: { actions: ['read', 'update', 'manage'] },
    },
    {
      id: 'proj-member',
      name: 'Member Only Project',
      slug: 'member-only',
      _capabilities: { actions: ['read', 'chat_post'] },
    },
    {
      id: 'proj-viewer',
      name: 'Viewer Project',
      slug: 'viewer-project',
      _capabilities: { actions: ['read'] },
    },
  ];

  function stubWithProjects(globalAllowed = false): ReturnType<typeof vi.fn> {
    const fetchMock = vi.fn((url: string | URL | Request): Promise<Response> => {
      const path = requestPath(url);
      if (path.includes('/api/v1/skills')) {
        return answer(
          path.includes('scope=global') ? (globalAllowed ? 'create' : 'none') : 'create'
        );
      }
      if (path.includes('/api/v1/projects/')) {
        const id = path.split('/api/v1/projects/')[1].split('?')[0];
        const found = PROJECTS.find((p) => p.id === id);
        return Promise.resolve(found ? jsonResponse(found) : jsonResponse({}, 404));
      }
      if (path.includes('/api/v1/projects')) {
        const u = new URL(path, 'http://localhost');
        const q = (u.searchParams.get('search') ?? '').toLowerCase();
        const matched = q
          ? PROJECTS.filter(
              (p) => p.name.toLowerCase().includes(q) || p.slug.toLowerCase().includes(q)
            )
          : PROJECTS;
        return Promise.resolve(jsonResponse({ projects: matched }));
      }
      return Promise.resolve(jsonResponse({}, 404));
    });
    vi.stubGlobal('fetch', fetchMock);
    return fetchMock;
  }

  it('renders a searchable, permission-filtered project picker instead of a text box for project scope', async () => {
    stubWithProjects(false);
    element = await mountPage();

    const scopeSelect = element.shadowRoot?.querySelector('sl-select#scope') as HTMLElement & {
      value: string;
    };
    scopeSelect.value = 'project';
    scopeSelect.dispatchEvent(new Event('sl-change'));
    await element.updateComplete;

    expect(element.shadowRoot?.querySelector('sl-input#scopeId')).toBeNull();
    const picker = element.shadowRoot?.querySelector('scion-project-picker#scopeId') as
      | (HTMLElement & { updateComplete: Promise<boolean>; value: string })
      | null;
    expect(picker).not.toBeNull();

    // Focus the picker input to load projects
    const pickerInput = picker?.shadowRoot?.querySelector('sl-input');
    expect(pickerInput).not.toBeNull();
    pickerInput?.dispatchEvent(new Event('sl-focus'));
    await new Promise((r) => setTimeout(r, 25));
    await picker!.updateComplete;

    // Only projects with update/manage permission appear (member & viewer filtered out)
    const optionNames = Array.from(
      picker!.shadowRoot?.querySelectorAll('.project-search-option .project-name') ?? []
    ).map((el) => el.textContent?.trim());
    expect(optionNames).toEqual(['Owned Project', 'Admin Project']);

    // Selecting a project updates scopeId
    const secondOption = picker!.shadowRoot?.querySelectorAll('.project-search-option')[1];
    secondOption?.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true }));
    await element.updateComplete;

    expect((element as unknown as { scopeId: string }).scopeId).toBe('proj-admin');
  });

  it('preselects the current project when opened with ?projectId= in a project context', async () => {
    window.history.replaceState({}, '', '/skills/new?projectId=proj-owner');
    stubWithProjects(false);
    element = await mountPage();

    // Scope stays 'project' (not overridden to 'user') and scopeId is preselected
    expect(scopeOf(element)).toBe('project');
    expect((element as unknown as { scopeId: string }).scopeId).toBe('proj-owner');

    const picker = element.shadowRoot?.querySelector('scion-project-picker#scopeId') as
      | (HTMLElement & { updateComplete: Promise<boolean> })
      | null;
    expect(picker).not.toBeNull();
    await new Promise((r) => setTimeout(r, 25));
    await picker!.updateComplete;

    expect((picker as unknown as Record<string, unknown>)['searchQuery']).toBe(
      'Owned Project (owned-project)'
    );
  });

  it('clears preselected project if user lacks publish permission on it', async () => {
    window.history.replaceState({}, '', '/skills/new?projectId=proj-member');
    stubWithProjects(false);
    element = await mountPage();

    await new Promise((r) => setTimeout(r, 25));
    await element.updateComplete;

    expect(scopeOf(element)).toBe('project');
    expect((element as unknown as { scopeId: string }).scopeId).toBe('');
  });
});
