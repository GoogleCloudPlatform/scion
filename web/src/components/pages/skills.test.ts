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

type PageEl = HTMLElement & { updateComplete: Promise<boolean> };

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

async function mountSkillsPage(
  handler: (url: string) => Promise<Response>
): Promise<{ el: PageEl; fetchMock: ReturnType<typeof vi.fn> }> {
  const fetchMock = vi.fn((input: string | URL | Request) => handler(urlOf(input)));
  vi.stubGlobal('fetch', fetchMock);
  const el = document.createElement('scion-page-skills') as PageEl;
  document.body.appendChild(el);
  for (let i = 0; i < 3; i++) {
    await el.updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  await el.updateComplete;
  return { el, fetchMock };
}

function shownSkillNames(el: PageEl): string[] {
  return Array.from(el.shadowRoot?.querySelectorAll('.skill-card') ?? [])
    .map((card) => card.getAttribute('href') ?? '')
    .sort();
}

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
    const { el, fetchMock } = await mountSkillsPage((url) => {
      if (url.includes('cursor=c2')) {
        return Promise.resolve(jsonResponse({ skills: [skill('3')] }));
      }
      return Promise.resolve(
        jsonResponse({
          skills: [skill('1'), skill('2')],
          nextCursor: 'c2',
          _capabilities: { actions: ['create'] },
        })
      );
    });
    element = el;

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(urlOf(fetchMock.mock.calls[1][0] as string)).toContain('cursor=c2');
    expect(shownSkillNames(el)).toEqual(['/skills/1', '/skills/2', '/skills/3']);
    expect(el.shadowRoot?.querySelector('a[href="/skills/new"]')).not.toBeNull();
    expect(el.shadowRoot?.querySelector('.partial-load-notice')).toBeNull();
  });

  it('keeps loaded pages and shows a notice when a later page fails', async () => {
    const { el } = await mountSkillsPage((url) => {
      if (url.includes('cursor=c2')) {
        return Promise.resolve(jsonResponse({ error: { code: 'internal' } }, 500));
      }
      return Promise.resolve(
        jsonResponse({
          skills: [skill('1'), skill('2')],
          nextCursor: 'c2',
          _capabilities: { actions: ['create'] },
        })
      );
    });
    element = el;

    expect(shownSkillNames(el)).toEqual(['/skills/1', '/skills/2']);
    const notice = el.shadowRoot?.querySelector('.partial-load-notice');
    expect(notice).not.toBeNull();
    expect(notice?.textContent).toContain('500');
    expect(el.shadowRoot?.querySelector('a[href="/skills/new"]')).not.toBeNull();
    expect(el.shadowRoot?.querySelector('.error-state')).toBeNull();
  });

  it('shows the error state when the first page fails', async () => {
    const { el } = await mountSkillsPage(() =>
      Promise.resolve(jsonResponse({ error: { code: 'internal' } }, 500))
    );
    element = el;

    expect(el.shadowRoot?.querySelector('.error-state')).not.toBeNull();
    expect(shownSkillNames(el)).toEqual([]);
  });
});
