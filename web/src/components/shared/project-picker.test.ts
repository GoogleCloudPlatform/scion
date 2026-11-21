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
 * scion-project-picker — unit tests.
 *
 * Covers: debounced project search, dropdown display, keyboard navigation,
 * mouse selection, emitted project UUID, typed slug passthrough, and
 * reset behavior.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

const MOCK_PROJECTS = [
  { id: 'proj-uuid-1', name: 'Alpha Project', slug: 'alpha-project' },
  { id: 'proj-uuid-2', name: 'Beta Project', slug: 'beta-project' },
  { id: 'proj-uuid-3', name: 'Gamma Project', slug: 'gamma-project' },
];

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let ProjectPickerCtor: any;

beforeAll(async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(new Response('{}', { status: 200 })))
  );
  const mod = await import('./project-picker.js');
  ProjectPickerCtor = mod.ScionProjectPicker;
  vi.restoreAllMocks();
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.restoreAllMocks();
});

function createProjectsResponse(projects = MOCK_PROJECTS) {
  return new Response(JSON.stringify({ projects }), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });
}

function makeFetchHandler(projects = MOCK_PROJECTS) {
  const calls: string[] = [];
  const handler = (url: string | URL | Request): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
    calls.push(path);

    if (path.includes('/api/v1/projects')) {
      return Promise.resolve(createProjectsResponse(projects));
    }
    return Promise.resolve(new Response('{}', { status: 200 }));
  };
  return { handler, calls };
}

async function createElement(
  fetchHandler?: (url: string | URL | Request) => Promise<Response>
): Promise<InstanceType<typeof ProjectPickerCtor>> {
  if (fetchHandler) {
    vi.stubGlobal('fetch', vi.fn(fetchHandler));
  } else {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('{}', { status: 200 })))
    );
  }
  const el = new ProjectPickerCtor();
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

describe('scion-project-picker', () => {
  it('renders with default label and placeholder', async () => {
    const el = await createElement();
    const input = el.shadowRoot?.querySelector('sl-input');
    expect(input).not.toBeNull();
    expect(input?.getAttribute('label')).toBe('Project');
    expect(input?.getAttribute('placeholder')).toContain('project name');
  });

  it('has combobox ARIA role on input', async () => {
    const el = await createElement();
    const input = el.shadowRoot?.querySelector('sl-input');
    expect(input?.getAttribute('role')).toBe('combobox');
    expect(input?.getAttribute('aria-expanded')).toBe('false');
  });

  it('fires debounced project query on input', async () => {
    const { handler, calls } = makeFetchHandler();
    const el = await createElement(handler);

    // Directly call the search handler with a sufficient query
    (el as Record<string, (...args: unknown[]) => void>)['handleSearchInput']({
      target: { value: 'alpha' },
    } as unknown as Event);
    await el.updateComplete;

    // Wait for debounce (250ms + margin)
    await new Promise((r) => setTimeout(r, 350));
    await el.updateComplete;

    // Check if a project search call was made
    const projectCalls = calls.filter((c) => c.includes('/api/v1/projects'));
    expect(projectCalls.length).toBeGreaterThan(0);
    expect(projectCalls[0]).toContain('search=alpha');
  });

  it('emits project-change with UUID on dropdown selection', async () => {
    const { handler } = makeFetchHandler();
    const el = await createElement(handler);

    const events: Array<{ projectId: string; displayLabel: string }> = [];
    el.addEventListener('project-change', (e: Event) => {
      const detail = (e as CustomEvent).detail;
      events.push(detail);
    });

    // Simulate selecting a project by directly calling selectProject
    (el as Record<string, (...args: unknown[]) => void>)['selectProject']({
      id: 'proj-uuid-1',
      name: 'Alpha Project',
      slug: 'alpha-project',
    });
    await el.updateComplete;

    expect(events.length).toBeGreaterThan(0);
    const last = events[events.length - 1];
    expect(last.projectId).toBe('proj-uuid-1');
    expect(last.displayLabel).toBe('Alpha Project');
  });

  it('updates input display after selection', async () => {
    const el = await createElement();

    // Call selectProject directly
    (el as Record<string, (...args: unknown[]) => void>)['selectProject']({
      id: 'proj-uuid-2',
      name: 'Beta Project',
      slug: 'beta-project',
    });
    await el.updateComplete;

    // searchQuery should show display-friendly format
    expect((el as Record<string, unknown>)['searchQuery']).toBe('Beta Project (beta-project)');
  });

  it('closes dropdown after selection', async () => {
    const el = await createElement();

    // Open the dropdown
    (el as Record<string, unknown>)['searchOpen'] = true;
    (el as Record<string, unknown>)['searchResults'] = MOCK_PROJECTS;
    el.requestUpdate();
    await el.updateComplete;

    // Select a project
    (el as Record<string, (...args: unknown[]) => void>)['selectProject'](MOCK_PROJECTS[0]);
    await el.updateComplete;

    expect((el as Record<string, unknown>)['searchOpen']).toBe(false);
  });

  it('emits typed slug on blur when no dropdown selection made', async () => {
    const el = await createElement();

    const events: Array<{ projectId: string }> = [];
    el.addEventListener('project-change', (e: Event) => {
      events.push((e as CustomEvent).detail);
    });

    // Set search query as if user typed a slug
    (el as Record<string, unknown>)['searchQuery'] = 'my-project-slug';
    (el as Record<string, unknown>)['selectedViaDropdown'] = false;

    // Trigger blur handler
    (el as Record<string, (...args: unknown[]) => void>)['emitChange']('my-project-slug', '');
    await el.updateComplete;

    expect(events.length).toBeGreaterThan(0);
    expect(events[events.length - 1].projectId).toBe('my-project-slug');
  });

  it('resets state when value is externally cleared', async () => {
    const el = await createElement();

    // Simulate a selection — set both the value and searchQuery
    el.value = 'proj-uuid-1';
    (el as Record<string, unknown>)['searchQuery'] = 'Alpha Project (alpha-project)';
    el.requestUpdate();
    await el.updateComplete;

    // Externally clear the value — triggers updated() lifecycle
    el.value = '';
    el.requestUpdate();
    await el.updateComplete;

    // After the updated() callback runs, searchQuery should be cleared
    await new Promise((r) => setTimeout(r, 50));
    await el.updateComplete;

    expect((el as Record<string, unknown>)['searchQuery']).toBe('');
  });

  it('renders dropdown items with name and slug', async () => {
    const el = await createElement();

    // Manually set search results and open dropdown
    (el as Record<string, unknown>)['searchOpen'] = true;
    (el as Record<string, unknown>)['searchResults'] = MOCK_PROJECTS;
    (el as Record<string, unknown>)['searchLoading'] = false;
    el.requestUpdate();
    await el.updateComplete;

    const options = el.shadowRoot?.querySelectorAll('.project-search-option');
    expect(options?.length).toBe(3);

    const firstOption = options?.[0];
    const name = firstOption?.querySelector('.project-name');
    const slug = firstOption?.querySelector('.project-slug');
    expect(name?.textContent?.trim()).toBe('Alpha Project');
    expect(slug?.textContent?.trim()).toBe('alpha-project');
  });

  it('dropdown has listbox role for accessibility', async () => {
    const el = await createElement();

    // Open dropdown
    (el as Record<string, unknown>)['searchOpen'] = true;
    (el as Record<string, unknown>)['searchResults'] = MOCK_PROJECTS;
    (el as Record<string, unknown>)['searchLoading'] = false;
    el.requestUpdate();
    await el.updateComplete;

    const listbox = el.shadowRoot?.querySelector('[role="listbox"]');
    expect(listbox).not.toBeNull();

    const options = el.shadowRoot?.querySelectorAll('[role="option"]');
    expect(options?.length).toBe(3);
  });

  it('keyboard ArrowDown moves active descendant', async () => {
    const el = await createElement();

    // Set up dropdown
    (el as Record<string, unknown>)['searchOpen'] = true;
    (el as Record<string, unknown>)['searchResults'] = MOCK_PROJECTS;
    el.requestUpdate();
    await el.updateComplete;

    // Simulate ArrowDown
    (el as Record<string, (...args: unknown[]) => void>)['handleKeydown'](
      new KeyboardEvent('keydown', { key: 'ArrowDown' })
    );
    await el.updateComplete;

    expect((el as Record<string, unknown>)['activeDescendantIndex']).toBe(0);

    // Another ArrowDown
    (el as Record<string, (...args: unknown[]) => void>)['handleKeydown'](
      new KeyboardEvent('keydown', { key: 'ArrowDown' })
    );
    await el.updateComplete;

    expect((el as Record<string, unknown>)['activeDescendantIndex']).toBe(1);
  });

  it('keyboard Escape closes dropdown', async () => {
    const el = await createElement();

    (el as Record<string, unknown>)['searchOpen'] = true;
    (el as Record<string, unknown>)['searchResults'] = MOCK_PROJECTS;
    el.requestUpdate();
    await el.updateComplete;

    (el as Record<string, (...args: unknown[]) => void>)['handleKeydown'](
      new KeyboardEvent('keydown', { key: 'Escape' })
    );
    await el.updateComplete;

    expect((el as Record<string, unknown>)['searchOpen']).toBe(false);
  });

  it('keyboard Enter selects active descendant', async () => {
    const el = await createElement();

    const events: Array<{ projectId: string }> = [];
    el.addEventListener('project-change', (e: Event) => {
      events.push((e as CustomEvent).detail);
    });

    // Set up dropdown with active descendant
    (el as Record<string, unknown>)['searchOpen'] = true;
    (el as Record<string, unknown>)['searchResults'] = MOCK_PROJECTS;
    (el as Record<string, unknown>)['activeDescendantIndex'] = 1;
    el.requestUpdate();
    await el.updateComplete;

    (el as Record<string, (...args: unknown[]) => void>)['handleKeydown'](
      new KeyboardEvent('keydown', { key: 'Enter' })
    );
    await el.updateComplete;

    expect(events.length).toBeGreaterThan(0);
    expect(events[events.length - 1].projectId).toBe('proj-uuid-2');
  });

  it('shows loading state during search', async () => {
    const el = await createElement();

    (el as Record<string, unknown>)['searchOpen'] = true;
    (el as Record<string, unknown>)['searchLoading'] = true;
    el.requestUpdate();
    await el.updateComplete;

    const loading = el.shadowRoot?.querySelector('.project-search-loading');
    expect(loading).not.toBeNull();
    expect(loading?.textContent?.trim()).toContain('Searching');
  });

  it('shows empty state when no results found', async () => {
    const el = await createElement();

    (el as Record<string, unknown>)['searchOpen'] = true;
    (el as Record<string, unknown>)['searchLoading'] = false;
    (el as Record<string, unknown>)['searchResults'] = [];
    el.requestUpdate();
    await el.updateComplete;

    const empty = el.shadowRoot?.querySelector('.project-search-empty');
    expect(empty).not.toBeNull();
    expect(empty?.textContent?.trim()).toContain('No projects found');
  });

  it('search query changes mocked results in dropdown', async () => {
    // Handler that returns different results based on the search query.
    const alphaProjects = [{ id: 'proj-a', name: 'Alpha Project', slug: 'alpha' }];
    const betaProjects = [{ id: 'proj-b', name: 'Beta Project', slug: 'beta' }];

    const calls: string[] = [];
    const handler = (url: string | URL | Request): Promise<Response> => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
      calls.push(path);
      if (path.includes('search=alpha')) {
        return Promise.resolve(
          new Response(JSON.stringify({ projects: alphaProjects }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      if (path.includes('search=beta')) {
        return Promise.resolve(
          new Response(JSON.stringify({ projects: betaProjects }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      return Promise.resolve(
        new Response(JSON.stringify({ projects: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    };

    const el = await createElement(handler);

    // Search for "alpha"
    (el as Record<string, (...args: unknown[]) => void>)['handleSearchInput']({
      target: { value: 'alpha' },
    } as unknown as Event);
    await new Promise((r) => setTimeout(r, 350));
    await el.updateComplete;

    const results1 = (el as Record<string, unknown>)['searchResults'] as Array<{ id: string }>;
    expect(results1.length).toBe(1);
    expect(results1[0].id).toBe('proj-a');

    // Search for "beta" — should produce different results.
    (el as Record<string, (...args: unknown[]) => void>)['handleSearchInput']({
      target: { value: 'beta' },
    } as unknown as Event);
    await new Promise((r) => setTimeout(r, 350));
    await el.updateComplete;

    const results2 = (el as Record<string, unknown>)['searchResults'] as Array<{ id: string }>;
    expect(results2.length).toBe(1);
    expect(results2[0].id).toBe('proj-b');
  });

  it('selected UUID from dropdown is emitted via project-change', async () => {
    const { handler } = makeFetchHandler();
    const el = await createElement(handler);

    const events: Array<{ projectId: string; displayLabel: string }> = [];
    el.addEventListener('project-change', (e: Event) => {
      events.push((e as CustomEvent).detail);
    });

    // Select a project from the dropdown — the UUID should be emitted.
    (el as Record<string, (...args: unknown[]) => void>)['selectProject']({
      id: 'proj-uuid-3',
      name: 'Gamma Project',
      slug: 'gamma-project',
    });
    await el.updateComplete;

    expect(events.length).toBeGreaterThan(0);
    const last = events[events.length - 1];
    expect(last.projectId).toBe('proj-uuid-3');
    expect(last.displayLabel).toBe('Gamma Project');
  });

  it('uses fixed-strategy sl-popup when hoist is enabled and stops Escape propagation', async () => {
    const el = await createElement();
    el.hoist = true;
    (el as Record<string, unknown>)['searchOpen'] = true;
    (el as Record<string, unknown>)['searchResults'] = MOCK_PROJECTS;
    el.requestUpdate();
    await el.updateComplete;

    const popup = el.shadowRoot?.querySelector('sl-popup');
    expect(popup).not.toBeNull();
    expect(popup?.getAttribute('strategy')).toBe('fixed');
    expect(popup?.getAttribute('sync')).toBe('width');
    expect(popup?.hasAttribute('active')).toBe(true);

    let outerSawEscape = false;
    document.body.addEventListener(
      'keydown',
      (evt) => {
        if (evt.key === 'Escape') outerSawEscape = true;
      },
      { once: true }
    );

    const input = el.shadowRoot?.querySelector('sl-input') ?? null;
    expect(input).not.toBeNull();
    input!.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, composed: true })
    );
    await el.updateComplete;

    expect((el as Record<string, unknown>)['searchOpen']).toBe(false);
    expect(outerSawEscape).toBe(false);
  });

  it('filters search results through filterProject predicate', async () => {
    const projectsWithCaps = [
      {
        id: 'proj-admin',
        name: 'Admin Project',
        slug: 'admin-project',
        _capabilities: { actions: ['read', 'update', 'manage'] },
      },
      {
        id: 'proj-member',
        name: 'Member Project',
        slug: 'member-project',
        _capabilities: { actions: ['read'] },
      },
    ];
    const { handler } = makeFetchHandler(projectsWithCaps);
    const el = await createElement(handler);
    el.browseOnFocus = true;
    el.filterProject = (p: { _capabilities?: { actions?: string[] } }) =>
      Boolean(p._capabilities?.actions?.includes('update'));
    await el.updateComplete;

    const input = el.shadowRoot?.querySelector('sl-input');
    expect(input).not.toBeNull();
    input?.dispatchEvent(new Event('sl-focus'));
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    const options = el.shadowRoot?.querySelectorAll('.project-search-option');
    expect(options?.length).toBe(1);
    expect(options?.[0]?.querySelector('.project-name')?.textContent?.trim()).toBe('Admin Project');
  });

  it('loads projects on focus when browseOnFocus is enabled', async () => {
    const { handler, calls } = makeFetchHandler();
    const el = await createElement(handler);
    el.browseOnFocus = true;
    await el.updateComplete;

    const input = el.shadowRoot?.querySelector('sl-input');
    expect(input).not.toBeNull();
    input?.dispatchEvent(new Event('sl-focus'));
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    expect(calls.some((c) => c.includes('/api/v1/projects?limit=50'))).toBe(true);
    expect(el.shadowRoot?.querySelectorAll('.project-search-option').length).toBe(3);
  });

  it('resolves preselected value display name and clears it if disallowed by filterProject', async () => {
    const handler = (url: string | URL | Request): Promise<Response> => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
      if (path.includes('/api/v1/projects/proj-allowed')) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              id: 'proj-allowed',
              name: 'Allowed Project',
              slug: 'allowed-project',
              _capabilities: { actions: ['read', 'update'] },
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      if (path.includes('/api/v1/projects/proj-denied')) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              id: 'proj-denied',
              name: 'Denied Project',
              slug: 'denied-project',
              _capabilities: { actions: ['read'] },
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      return Promise.resolve(createProjectsResponse());
    };

    const el = await createElement(handler);
    el.browseOnFocus = true;
    el.requireSelection = true;
    el.filterProject = (p: { _capabilities?: { actions?: string[] } }) =>
      Boolean(p._capabilities?.actions?.includes('update'));
    el.value = 'proj-allowed';
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    expect((el as Record<string, unknown>)['searchQuery']).toBe(
      'Allowed Project (allowed-project)'
    );

    const events: Array<{ projectId: string }> = [];
    el.addEventListener('project-change', (e: Event) => {
      events.push((e as CustomEvent).detail);
    });
    (el as Record<string, unknown>)['selectedViaDropdown'] = false;
    el.value = 'proj-denied';
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    expect((el as Record<string, unknown>)['searchQuery']).toBe('');
    expect(events[events.length - 1]?.projectId).toBe('');
  });

  it('does not emit raw typed text on blur when requireSelection is true', async () => {
    const el = await createElement();
    el.requireSelection = true;
    await el.updateComplete;

    const events: Array<{ projectId: string }> = [];
    el.addEventListener('project-change', (e: Event) => {
      events.push((e as CustomEvent).detail);
    });

    (el as Record<string, unknown>)['searchQuery'] = 'nonexistent-slug';
    (el as Record<string, unknown>)['selectedViaDropdown'] = false;
    const input = el.shadowRoot?.querySelector('sl-input');
    expect(input).not.toBeNull();
    input?.dispatchEvent(new Event('sl-blur'));
    await new Promise((r) => setTimeout(r, 250));
    await el.updateComplete;

    expect(events).toHaveLength(0);
    expect((el as Record<string, unknown>)['searchQuery']).toBe('');
  });

  it('supports multiple selection with toggle, tags, removal, and URL-loaded ID resolution', async () => {
    const handler = (url: string | URL | Request): Promise<Response> => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
      if (path.includes('/api/v1/projects/proj-uuid-2')) {
        return Promise.resolve(
          new Response(JSON.stringify(MOCK_PROJECTS[1]), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      return Promise.resolve(createProjectsResponse());
    };

    const el = await createElement(handler);
    el.multiple = true;
    el.values = ['proj-uuid-2'];
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    // Preselected value resolves to project name in tag
    let tags = el.shadowRoot?.querySelectorAll('.selected-tags sl-tag');
    expect(tags?.length).toBe(1);
    expect(tags?.[0]?.textContent?.trim()).toBe('Beta Project');

    const events: Array<{ projectIds: string[] }> = [];
    el.addEventListener('projects-change', (e: Event) => {
      events.push((e as CustomEvent).detail);
    });

    // Focus opens dropdown with multiselectable listbox
    const input = el.shadowRoot?.querySelector('sl-input');
    expect(input).not.toBeNull();
    input?.dispatchEvent(new Event('sl-focus'));
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    const listbox = el.shadowRoot?.querySelector('[role="listbox"]');
    expect(listbox?.getAttribute('aria-multiselectable')).toBe('true');

    // Toggle first option (Alpha Project) — keeps dropdown open and emits both IDs
    const options = el.shadowRoot?.querySelectorAll('.project-search-option');
    options?.[0]?.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true }));
    await el.updateComplete;

    expect((el as Record<string, unknown>)['searchOpen']).toBe(true);
    expect(events[events.length - 1]?.projectIds).toEqual(['proj-uuid-2', 'proj-uuid-1']);

    tags = el.shadowRoot?.querySelectorAll('.selected-tags sl-tag');
    expect(tags?.length).toBe(2);

    // Removing first tag emits remaining project ID
    tags?.[0]?.dispatchEvent(new CustomEvent('sl-remove'));
    await el.updateComplete;

    expect(events[events.length - 1]?.projectIds).toEqual(['proj-uuid-1']);
    expect(el.shadowRoot?.querySelectorAll('.selected-tags sl-tag').length).toBe(1);
  });
});
