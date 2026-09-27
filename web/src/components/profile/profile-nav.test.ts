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
