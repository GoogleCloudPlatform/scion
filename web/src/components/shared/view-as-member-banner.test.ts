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
 * Tests for the "View as member" banner, shell mounting, toggle transitions,
 * and UAT-create dialog note (ptone/scion#9074, design §4.4, §4.7, §4.8).
 */

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { render as litRender } from 'lit';

import type { User } from '../../shared/types.js';
import { clearAdminStatus, loadAdminStatus } from '../../client/admin-status.js';
import {
  isAdminRoutePath,
  setAdminPathChecker,
  toggleViewAsMember,
} from '../../client/view-as-member.js';
import {
  VIEW_AS_MEMBER_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';
import {
  VIEW_AS_MEMBER_BANNER_DETAIL,
  VIEW_AS_MEMBER_BANNER_HEADING,
  renderViewAsMemberBanner,
} from './view-as-member-banner.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const NORMAL_USER: User = {
  id: 'u-1',
  email: 'admin@example.com',
  name: 'Hub Admin',
  role: 'admin',
};

const VAM_USER: User = {
  id: 'u-1',
  email: 'admin@example.com',
  name: 'Hub Admin',
  role: 'member',
  viewAsMember: true,
};

describe('renderViewAsMemberBanner', () => {
  let host: HTMLDivElement;

  beforeEach(() => {
    host = document.createElement('div');
    document.body.appendChild(host);
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  afterEach(() => {
    host.remove();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('renders nothing when user is null, undefined, or viewAsMember is false', () => {
    for (const u of [null, undefined, NORMAL_USER, { ...NORMAL_USER, viewAsMember: false }]) {
      litRender(renderViewAsMemberBanner(u), host);
      expect(host.querySelector('[data-testid="view-as-member-banner"]')).toBeNull();
    }
  });

  it('renders a non-closable primary alert with Exit button when viewAsMember is true even with flag OFF', () => {
    setFeatureFlag(VIEW_AS_MEMBER_FLAG, false);
    litRender(renderViewAsMemberBanner(VAM_USER), host);

    const alert = host.querySelector('[data-testid="view-as-member-banner"]');
    expect(alert).not.toBeNull();
    expect(alert?.tagName.toLowerCase()).toBe('sl-alert');
    expect(alert?.getAttribute('variant')).toBe('primary');
    expect(alert?.hasAttribute('open')).toBe(true);
    expect(alert?.hasAttribute('closable')).toBe(false);
    expect(alert?.textContent).toContain(VIEW_AS_MEMBER_BANNER_HEADING);
    expect(alert?.textContent).toContain(VIEW_AS_MEMBER_BANNER_DETAIL);

    const exitBtn = host.querySelector('[data-testid="view-as-member-exit-btn"]');
    expect(exitBtn).not.toBeNull();
    expect(exitBtn?.textContent?.trim()).toBe('Exit');
  });

  it('Exit button posts {enabled:false}, clears admin status, and reloads', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      if (url.endsWith('/auth/view-as-member')) {
        expect(init?.method).toBe('POST');
        expect(typeof init?.body === 'string' ? JSON.parse(init.body) : null).toEqual({
          enabled: false,
        });
        return Promise.resolve(jsonResponse({ viewAsMember: false }));
      }
      return Promise.resolve(
        jsonResponse({ isAdmin: false, isSuperAdmin: false, permissions: [] })
      );
    });
    vi.stubGlobal('fetch', fetchMock);

    // Prime admin-status cache so we can verify clearAdminStatus() drops it.
    clearAdminStatus();
    await loadAdminStatus('u-1');

    const reload = vi.fn();
    litRender(renderViewAsMemberBanner(VAM_USER, { reload }), host);
    const exitBtn = host.querySelector<HTMLElement>('[data-testid="view-as-member-exit-btn"]')!;
    exitBtn.click();

    await vi.waitFor(() => expect(reload).toHaveBeenCalledTimes(1));

    // Cache was cleared, so a subsequent loadAdminStatus issues a new fetch.
    const before = fetchMock.mock.calls.length;
    await loadAdminStatus('u-1');
    expect(fetchMock.mock.calls.length).toBe(before + 1);
  });
});

describe('toggleViewAsMember & isAdminRoutePath', () => {
  afterEach(() => {
    setAdminPathChecker(null);
    clearAdminStatus();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('delegates isAdminRoutePath to the registered router-backed checker and strips query/hash', () => {
    expect(isAdminRoutePath('/admin/users')).toBe(false);

    const checker = vi.fn((p: string) => p === '/admin/users' || p === '/settings');
    setAdminPathChecker(checker);

    expect(isAdminRoutePath('/admin/users?tab=active#top')).toBe(true);
    expect(checker).toHaveBeenCalledWith('/admin/users');
    expect(isAdminRoutePath('/profile/env')).toBe(false);
  });

  it('entering from an admin route navigates to /; entering from a non-admin route reloads', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({ viewAsMember: true })))
    );
    setAdminPathChecker((p) => p.startsWith('/admin/'));

    const navigate = vi.fn();
    const reload = vi.fn();
    await toggleViewAsMember(true, {
      currentPathname: '/admin/users',
      navigate,
      reload,
    });
    expect(navigate).toHaveBeenCalledWith('/');
    expect(reload).not.toHaveBeenCalled();

    navigate.mockClear();
    reload.mockClear();
    await toggleViewAsMember(true, {
      currentPathname: '/profile/settings',
      navigate,
      reload,
    });
    expect(navigate).not.toHaveBeenCalled();
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it('exiting reloads the current page', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({ viewAsMember: false })))
    );

    const navigate = vi.fn();
    const reload = vi.fn();
    await toggleViewAsMember(false, {
      currentPathname: '/projects/p1',
      navigate,
      reload,
    });
    expect(navigate).not.toHaveBeenCalled();
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it('throws and does not navigate on non-OK response', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({ error: { message: 'forbidden' } }, 403)))
    );
    const navigate = vi.fn();
    const reload = vi.fn();
    await expect(
      toggleViewAsMember(true, { currentPathname: '/admin/users', navigate, reload })
    ).rejects.toThrow();
    expect(navigate).not.toHaveBeenCalled();
    expect(reload).not.toHaveBeenCalled();
  });
});

describe('view-as-member banner in authenticated shells', () => {
  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({})))
    );
    await import('../app-shell.js');
    await import('../chat/chat-shell.js');
    await import('../profile/profile-shell.js');
  }, 30_000);

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it.each([
    ['scion-app', '/'],
    ['scion-chat-shell', '/chat'],
    ['scion-profile-shell', '/profile/env'],
  ])(
    '%s renders the banner above <scion-header> only when user.viewAsMember is true',
    async (tag, path) => {
      vi.stubGlobal(
        'fetch',
        vi.fn(() => Promise.resolve(jsonResponse({})))
      );
      const el = document.createElement(tag) as HTMLElement & {
        user: User | null;
        currentPath: string;
        updateComplete: Promise<boolean>;
      };
      el.currentPath = path;
      el.user = NORMAL_USER;
      document.body.appendChild(el);
      await el.updateComplete;
      expect(el.shadowRoot!.querySelector('[data-testid="view-as-member-banner"]')).toBeNull();

      el.user = VAM_USER;
      await el.updateComplete;
      const banner = el.shadowRoot!.querySelector('[data-testid="view-as-member-banner"]');
      const header = el.shadowRoot!.querySelector('scion-header');
      expect(banner).not.toBeNull();
      expect(header).not.toBeNull();
      // Banner must precede <scion-header> in DOM order.
      expect(
        Boolean(banner!.compareDocumentPosition(header!) & Node.DOCUMENT_POSITION_FOLLOWING)
      ).toBe(true);
    }
  );

  it('TerminalWorkspaceRoot renders the banner below test-hub banner and above <scion-header>', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({})))
    );
    const { TerminalWorkspaceRoot } = await import('../../client/terminal-workspace-root.js');
    const root = new TerminalWorkspaceRoot(NORMAL_USER);
    document.body.appendChild(root.element);
    expect(root.element.querySelector('[data-testid="view-as-member-banner"]')).toBeNull();

    root.setUser(VAM_USER);
    const testHubHost = root.element.querySelector('.terminal-test-hub-banner')!;
    const banner = root.element.querySelector('[data-testid="view-as-member-banner"]')!;
    const header = root.element.querySelector('scion-header')!;
    expect(banner).not.toBeNull();
    expect(
      Boolean(testHubHost.compareDocumentPosition(banner) & Node.DOCUMENT_POSITION_FOLLOWING)
    ).toBe(true);
    expect(Boolean(banner.compareDocumentPosition(header) & Node.DOCUMENT_POSITION_FOLLOWING)).toBe(
      true
    );
    root.dispose();
  });
});

describe('scion-token-list — view-as-member create dialog note (design §4.4)', () => {
  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({ tokens: [], projects: [] })))
    );
    await import('./token-list.js');
  }, 30_000);

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('shows the one-line note in the create dialog only when viewAsMember is true', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse({ tokens: [], projects: [] })))
    );
    const el = document.createElement('scion-token-list') as HTMLElement & {
      viewAsMember: boolean;
      updateComplete: Promise<boolean>;
    };
    el.viewAsMember = false;
    document.body.appendChild(el);
    await vi.waitFor(() =>
      expect(el.shadowRoot!.querySelector('sl-dialog[label="Create Access Token"]')).not.toBeNull()
    );
    expect(el.shadowRoot!.querySelector('[data-testid="view-as-member-token-note"]')).toBeNull();

    el.viewAsMember = true;
    await el.updateComplete;
    const note = el.shadowRoot!.querySelector('[data-testid="view-as-member-token-note"]');
    expect(note).not.toBeNull();
    expect(note?.textContent).toContain('member permissions');
  });
});
