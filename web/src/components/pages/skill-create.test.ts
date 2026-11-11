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
