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
 * Test-hub banner (ptone/scion#4240, phase W): its text and icon for each
 * gate combination, no close control, no element when every gate is off, the
 * app shell re-rendering it after it is removed, the login page, and no
 * browser-side flag, experiment or storage key changing it.
 */

// @vitest-environment happy-dom

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { html, render } from 'lit';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

// Mounting the shells mounts the header and nav, whose trays fetch on
// connect; stubbed so no real requests go out.
vi.mock('../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../client/api.js')>();
  return {
    ...actual,
    apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
  };
});

import {
  TEST_HUB_ADMIN_TEXT,
  TEST_HUB_MEMBER_TEXT,
  TEST_HUB_SUPER_ADMIN_TEXT,
  renderTestHubBanner,
  testHubBannerContent,
} from './shared/test-hub-banner.js';
import {
  TEST_INFRA_STATUS_URL,
  resetTestInfraStatusForTests,
  type TestInfraStatus,
} from '../client/test-infra-status.js';
import { resetServerFlagStateForTests, setServerFlags } from '../utils/feature-flags.js';
import type { ScionApp } from './app-shell.js';
import type { ScionLoginPage } from './pages/login.js';

const OFF: TestInfraStatus = { testIdentities: false, testHubAdmin: false, testSuperAdmin: false };
const MEMBER: TestInfraStatus = {
  testIdentities: true,
  testHubAdmin: false,
  testSuperAdmin: false,
};
const ADMIN: TestInfraStatus = { testIdentities: true, testHubAdmin: true, testSuperAdmin: false };
const SUPER: TestInfraStatus = { testIdentities: true, testHubAdmin: true, testSuperAdmin: true };

/** The URL a fetch mock was called with, whatever form it took. */
function urlOf(input: unknown): string {
  if (typeof input === 'string') return input;
  if (input instanceof URL) return input.href;
  if (input instanceof Request) return input.url;
  return '';
}

/** Stubs fetch: the status endpoint answers `status`, everything else `{}`. */
function stubHub(status: TestInfraStatus): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn((input: RequestInfo | URL) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const body = url.endsWith(TEST_INFRA_STATUS_URL) ? status : {};
    return Promise.resolve(
      new Response(JSON.stringify(body), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function bannerIn(root: ParentNode | null | undefined): HTMLElement | null {
  return root?.querySelector<HTMLElement>('sl-alert.test-hub-banner') ?? null;
}

function renderAlone(status: TestInfraStatus | null): HTMLElement {
  const host = document.createElement('div');
  render(html`${renderTestHubBanner(status)}`, host);
  return host;
}

afterEach(() => {
  resetTestInfraStatusForTests();
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

describe('testHubBannerContent', () => {
  it('member tier only', () => {
    expect(testHubBannerContent(MEMBER)).toEqual({
      text: 'TEST HUB: test identities are enabled',
      icon: 'exclamation-triangle',
    });
  });

  it('admin tier', () => {
    expect(testHubBannerContent(ADMIN)).toEqual({
      text: 'TEST HUB: test admin identities are enabled; this hub can mint hub-admins',
      icon: 'exclamation-triangle',
    });
  });

  it('super-admin kind', () => {
    expect(testHubBannerContent(SUPER)).toEqual({
      text: 'TEST HUB: test admin identities are enabled; this hub can mint hub-admins and super-admins',
      icon: 'exclamation-octagon',
    });
    // The super-admin kind implies the admin tier.
    expect(testHubBannerContent({ ...OFF, testSuperAdmin: true })?.text).toBe(
      TEST_HUB_SUPER_ADMIN_TEXT
    );
  });

  it('no banner when every gate is off, or before the status is known', () => {
    expect(testHubBannerContent(OFF)).toBeNull();
    expect(testHubBannerContent(null)).toBeNull();
  });
});

describe('banner icons ship in the production build', () => {
  it('both icons are in USED_ICONS (scripts/copy-shoelace-icons.mjs)', () => {
    const script = readFileSync(
      resolve(__dirname, '../../scripts/copy-shoelace-icons.mjs'),
      'utf8'
    );
    const start = script.indexOf('const USED_ICONS = [');
    expect(start).toBeGreaterThan(-1);
    const list = script.slice(start, script.indexOf('];', start));
    for (const status of [MEMBER, ADMIN, SUPER]) {
      const icon = testHubBannerContent(status)!.icon;
      expect(list).toContain(`'${icon}'`);
    }
  });
});

describe('renderTestHubBanner', () => {
  it.each([
    ['member', MEMBER, TEST_HUB_MEMBER_TEXT, 'exclamation-triangle'],
    ['admin', ADMIN, TEST_HUB_ADMIN_TEXT, 'exclamation-triangle'],
    ['super-admin', SUPER, TEST_HUB_SUPER_ADMIN_TEXT, 'exclamation-octagon'],
  ])('%s: an open warning alert with no close control', (_name, status, text, icon) => {
    const alert = bannerIn(renderAlone(status));
    expect(alert).not.toBeNull();
    expect(alert!.tagName.toLowerCase()).toBe('sl-alert');
    expect(alert!.getAttribute('variant')).toBe('warning');
    expect(alert!.hasAttribute('open')).toBe(true);
    expect(alert!.hasAttribute('closable')).toBe(false);
    expect(alert!.textContent?.trim()).toBe(text);
    expect(alert!.querySelector('sl-icon[slot="icon"]')?.getAttribute('name')).toBe(icon);
  });

  it('renders no element at all when every gate is off', () => {
    expect(renderAlone(OFF).children).toHaveLength(0);
    expect(renderAlone(null).children).toHaveLength(0);
  });
});

describe('app shell', () => {
  beforeAll(async () => {
    await import('./app-shell.js');
  }, 30_000);

  async function mountShell(path = '/'): Promise<ScionApp> {
    const shell = document.createElement('scion-app');
    shell.currentPath = path;
    document.body.appendChild(shell);
    await shell.updateComplete;
    return shell;
  }

  it('shows the banner above the header, from the status endpoint', async () => {
    const fetchMock = stubHub(MEMBER);
    const shell = await mountShell();
    await vi.waitFor(() => expect(bannerIn(shell.shadowRoot)).not.toBeNull());

    const banner = bannerIn(shell.shadowRoot)!;
    expect(banner.textContent?.trim()).toBe(TEST_HUB_MEMBER_TEXT);
    expect(banner.hasAttribute('closable')).toBe(false);
    const main = shell.shadowRoot!.querySelector('main')!;
    expect(main.firstElementChild).toBe(banner);
    expect(banner.nextElementSibling?.tagName.toLowerCase()).toBe('scion-header');
    expect(
      fetchMock.mock.calls.filter(([u]) => urlOf(u).endsWith(TEST_INFRA_STATUS_URL))
    ).toHaveLength(1);
  });

  it('shows the super-admin variant everywhere the shell renders', async () => {
    stubHub(SUPER);
    const shell = await mountShell('/admin/users');
    await vi.waitFor(() => expect(bannerIn(shell.shadowRoot)).not.toBeNull());
    expect(bannerIn(shell.shadowRoot)!.textContent?.trim()).toBe(TEST_HUB_SUPER_ADMIN_TEXT);

    for (const path of ['/', '/projects/p-1', '/settings']) {
      shell.currentPath = path;
      await shell.updateComplete;
      expect(bannerIn(shell.shadowRoot)!.textContent?.trim()).toBe(TEST_HUB_SUPER_ADMIN_TEXT);
    }
  });

  it('renders no banner element when every gate is off', async () => {
    const fetchMock = stubHub(OFF);
    const shell = await mountShell();
    await vi.waitFor(() =>
      expect(fetchMock.mock.calls.some(([u]) => urlOf(u).endsWith(TEST_INFRA_STATUS_URL))).toBe(
        true
      )
    );
    await new Promise((r) => setTimeout(r, 0));
    await shell.updateComplete;
    expect(shell.shadowRoot!.querySelector('sl-alert')).toBeNull();
  });

  it('brings the banner back on navigation after it was removed (devtools)', async () => {
    stubHub(MEMBER);
    const shell = await mountShell('/');
    await vi.waitFor(() => expect(bannerIn(shell.shadowRoot)).not.toBeNull());

    const first = bannerIn(shell.shadowRoot)!;
    first.remove();
    expect(bannerIn(shell.shadowRoot)).toBeNull();

    shell.currentPath = '/projects/p-1';
    await shell.updateComplete;
    const second = bannerIn(shell.shadowRoot);
    expect(second).not.toBeNull();
    expect(second).not.toBe(first);
    expect(second!.textContent?.trim()).toBe(TEST_HUB_MEMBER_TEXT);
  });

  describe('browser-side flags and storage do not change it', () => {
    /** A storage where every key reads as set, so any lookup would see a value. */
    function hostileStorage(value: string): Storage {
      const store = new Map<string, string>([
        ['scion:feature:test_hub_banner', value],
        ['scion:feature:web.test_hub_banner', value],
        ['scion-test-hub-banner-dismissed', 'true'],
        ['testInfraStatus', JSON.stringify({ testIdentities: value === 'true' })],
      ]);
      return {
        get length() {
          return store.size;
        },
        clear: () => store.clear(),
        getItem: (k: string) => store.get(k) ?? value,
        key: (i: number) => [...store.keys()][i] ?? null,
        removeItem: (k: string) => void store.delete(k),
        setItem: (k: string, v: string) => void store.set(k, v),
      };
    }

    beforeEach(() => {
      resetServerFlagStateForTests();
    });

    afterEach(() => {
      delete window.__SCION_FEATURES__;
      resetServerFlagStateForTests();
    });

    it.each(['true', 'false'])(
      'member status: banner unchanged with every flag and storage key reading %s',
      async (value) => {
        const on = value === 'true';
        window.__SCION_FEATURES__ = {
          'web.test_hub_banner': on,
          test_hub_banner: on,
          'hub.test_identities': on,
        };
        setServerFlags({ 'web.test_hub_banner': on, 'hub.test_identities': on });
        vi.stubGlobal('localStorage', hostileStorage(value));
        vi.stubGlobal('sessionStorage', hostileStorage(value));
        stubHub(MEMBER);

        const shell = await mountShell();
        await vi.waitFor(() => expect(bannerIn(shell.shadowRoot)).not.toBeNull());
        const banner = bannerIn(shell.shadowRoot)!;
        expect(banner.textContent?.trim()).toBe(TEST_HUB_MEMBER_TEXT);
        expect(banner.hasAttribute('closable')).toBe(false);
      }
    );

    it('all-off status: still no banner with every flag and storage key reading true', async () => {
      window.__SCION_FEATURES__ = { 'web.test_hub_banner': true, 'hub.test_identities': true };
      setServerFlags({ 'web.test_hub_banner': true, 'hub.test_identities': true });
      vi.stubGlobal('localStorage', hostileStorage('true'));
      vi.stubGlobal('sessionStorage', hostileStorage('true'));
      const fetchMock = stubHub(OFF);

      const shell = await mountShell();
      await vi.waitFor(() =>
        expect(fetchMock.mock.calls.some(([u]) => urlOf(u).endsWith(TEST_INFRA_STATUS_URL))).toBe(
          true
        )
      );
      await new Promise((r) => setTimeout(r, 0));
      await shell.updateComplete;
      expect(shell.shadowRoot!.querySelector('sl-alert')).toBeNull();
    });
  });
});

describe('chat and profile shells', () => {
  beforeAll(async () => {
    await import('./chat/chat-shell.js');
    await import('./profile/profile-shell.js');
  }, 30_000);

  it.each([
    ['scion-chat-shell', '/chat', '/chat/agent-1'],
    ['scion-profile-shell', '/profile/settings', '/profile/tokens'],
  ] as const)(
    '%s: banner first in main, back after removal and a path change',
    async (tag, first, second) => {
      stubHub(MEMBER);
      const shell = document.createElement(tag) as HTMLElement & {
        currentPath: string;
        updateComplete: Promise<boolean>;
      };
      shell.currentPath = first;
      document.body.appendChild(shell);
      await shell.updateComplete;
      await vi.waitFor(() => expect(bannerIn(shell.shadowRoot)).not.toBeNull());

      const banner = bannerIn(shell.shadowRoot)!;
      expect(banner.textContent?.trim()).toBe(TEST_HUB_MEMBER_TEXT);
      expect(banner.hasAttribute('closable')).toBe(false);
      expect(shell.shadowRoot!.querySelector('main')!.firstElementChild).toBe(banner);

      banner.remove();
      shell.currentPath = second;
      await shell.updateComplete;
      const again = bannerIn(shell.shadowRoot);
      expect(again).not.toBeNull();
      expect(again).not.toBe(banner);
    }
  );

  it('scion-chat-shell: no banner element when every gate is off', async () => {
    const fetchMock = stubHub(OFF);
    const shell = document.createElement('scion-chat-shell');
    document.body.appendChild(shell);
    await shell.updateComplete;
    await vi.waitFor(() =>
      expect(fetchMock.mock.calls.some(([u]) => urlOf(u).endsWith(TEST_INFRA_STATUS_URL))).toBe(
        true
      )
    );
    await new Promise((r) => setTimeout(r, 0));
    await shell.updateComplete;
    expect(shell.shadowRoot!.querySelector('sl-alert')).toBeNull();
  });
});

describe('invite and onboarding pages', () => {
  beforeAll(async () => {
    await import('./pages/invite.js');
    await import('./pages/onboarding.js');
  }, 30_000);

  it.each(['scion-page-invite', 'scion-page-onboarding'] as const)(
    '%s shows the banner first, in normal flow, without signing in',
    async (tag) => {
      stubHub(SUPER);
      const page = document.createElement(tag) as HTMLElement & {
        updateComplete: Promise<boolean>;
      };
      document.body.appendChild(page);
      await page.updateComplete;
      await vi.waitFor(() => expect(bannerIn(page.shadowRoot)).not.toBeNull());
      const banner = bannerIn(page.shadowRoot)!;
      expect(banner.textContent?.trim()).toBe(TEST_HUB_SUPER_ADMIN_TEXT);
      expect(banner.hasAttribute('closable')).toBe(false);
      await vi.waitFor(() => expect(page.hasAttribute('test-hub')).toBe(true));
      // First element of the page, so it sits above the content.
      expect(page.shadowRoot!.firstElementChild).toBe(banner);
    }
  );

  it.each(['scion-page-invite', 'scion-page-onboarding'] as const)(
    '%s renders no banner element when every gate is off',
    async (tag) => {
      const fetchMock = stubHub(OFF);
      const page = document.createElement(tag) as HTMLElement & {
        updateComplete: Promise<boolean>;
      };
      document.body.appendChild(page);
      await page.updateComplete;
      await vi.waitFor(() =>
        expect(fetchMock.mock.calls.some(([u]) => urlOf(u).endsWith(TEST_INFRA_STATUS_URL))).toBe(
          true
        )
      );
      await new Promise((r) => setTimeout(r, 0));
      await page.updateComplete;
      expect(page.shadowRoot!.querySelector('sl-alert')).toBeNull();
      expect(page.hasAttribute('test-hub')).toBe(false);
    }
  );
});

describe('login page', () => {
  beforeAll(async () => {
    await import('./pages/login.js');
  }, 30_000);

  async function mountLogin(): Promise<ScionLoginPage> {
    const page = document.createElement('scion-login-page');
    document.body.appendChild(page);
    await page.updateComplete;
    return page;
  }

  it.each([
    ['member', MEMBER, TEST_HUB_MEMBER_TEXT],
    ['super-admin', SUPER, TEST_HUB_SUPER_ADMIN_TEXT],
  ])('shows the %s banner without signing in', async (_name, status, text) => {
    stubHub(status);
    const page = await mountLogin();
    await vi.waitFor(() => expect(bannerIn(page.shadowRoot)).not.toBeNull());
    const banner = bannerIn(page.shadowRoot)!;
    expect(banner.textContent?.trim()).toBe(text);
    expect(banner.hasAttribute('closable')).toBe(false);
    await vi.waitFor(() => expect(page.hasAttribute('test-hub')).toBe(true));
    // In normal flow, first in the page, not fixed over the card.
    expect(page.shadowRoot!.firstElementChild).toBe(banner);
    expect(getComputedStyle(banner).position).not.toBe('fixed');
  });

  it('retries a failed status fetch when the page updates', async () => {
    let fail = true;
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      if (url.endsWith(TEST_INFRA_STATUS_URL) && fail) {
        return Promise.resolve(new Response('unavailable', { status: 503 }));
      }
      const body = url.endsWith(TEST_INFRA_STATUS_URL) ? MEMBER : {};
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
    });
    vi.stubGlobal('fetch', fetchMock);
    const statusCalls = (): number =>
      fetchMock.mock.calls.filter(([u]) => urlOf(u).endsWith(TEST_INFRA_STATUS_URL)).length;

    const page = await mountLogin();
    await vi.waitFor(() => expect(statusCalls()).toBeGreaterThan(0));
    await new Promise((r) => setTimeout(r, 0));
    expect(bannerIn(page.shadowRoot)).toBeNull();

    fail = false;
    page.requestUpdate();
    await vi.waitFor(() => expect(bannerIn(page.shadowRoot)).not.toBeNull());
    expect(bannerIn(page.shadowRoot)!.textContent?.trim()).toBe(TEST_HUB_MEMBER_TEXT);
  });

  it('renders no banner element when every gate is off', async () => {
    const fetchMock = stubHub(OFF);
    const page = await mountLogin();
    await vi.waitFor(() =>
      expect(fetchMock.mock.calls.some(([u]) => urlOf(u).endsWith(TEST_INFRA_STATUS_URL))).toBe(
        true
      )
    );
    await new Promise((r) => setTimeout(r, 0));
    await page.updateComplete;
    expect(page.shadowRoot!.querySelector('sl-alert')).toBeNull();
    expect(page.hasAttribute('test-hub')).toBe(false);
  });
});
