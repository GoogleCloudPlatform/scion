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
 * Every former raw `window.location.href` site, served under the non-root
 * base path /scion/ (ptone/scion#5535). A full page load must target the
 * base-prefixed URL; a site moved to client-side navigation must hand the
 * router the app path (the router adds the base path, see
 * main-link-base-path.test.ts) and leave window.location alone.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, type TemplateResult } from 'lit';

vi.mock('../components/shared/confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

const BASE = '/scion/';
const ORIGINAL_LOCATION = window.location;

/** The href written by the code under test; null until one is written. */
let written: string | null;
let navs: string[];
const onNav = (e: Event): void => {
  navs.push((e as CustomEvent<{ path: string }>).detail.path);
};

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/** Render a private template method's result and click the first match. */
async function clickIn(template: TemplateResult, selector: string): Promise<void> {
  const host = document.createElement('div');
  document.body.appendChild(host);
  render(template, host);
  const target = host.querySelector<HTMLElement>(selector);
  expect(target, selector).not.toBeNull();
  target!.click();
  await Promise.resolve();
  host.remove();
}

beforeEach(() => {
  vi.stubEnv('BASE_URL', BASE);
  written = null;
  navs = [];
  document.addEventListener('nav-click', onNav);
  // Record href writes instead of navigating; the page "is" /scion/projects.
  const fake = {
    pathname: '/scion/projects',
    search: '',
    hash: '',
    origin: 'http://localhost',
    get href(): string {
      return written ?? 'http://localhost/scion/projects';
    },
    set href(v: string) {
      written = v;
    },
  };
  Object.defineProperty(window, 'location', { value: fake, configurable: true, writable: true });
});

afterEach(() => {
  document.removeEventListener('nav-click', onNav);
  Object.defineProperty(window, 'location', {
    value: ORIGINAL_LOCATION,
    configurable: true,
    writable: true,
  });
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
});

describe('full page loads keep the base path', () => {
  it('client/api.ts: a 401 sends the user to the prefixed login page', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(json({ error: { code: 'unauthorized' } }, 401)))
    );
    const { apiFetch } = await import('./api.js');
    await apiFetch('/api/v1/agents');
    // returnTo is the browser URL (already prefixed) the server redirects to.
    expect(written).toBe('/scion/login?error=session_expired&returnTo=%2Fscion%2Fprojects');
  });

  it('client/sse-client.ts: an expired session on reconnect goes to the prefixed login page', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(json({}, 401)))
    );
    const { SSEClient } = await import('./sse-client.js');
    const client = new SSEClient();
    await (client as unknown as { checkAuthAndReconnect(): Promise<void> }).checkAuthAndReconnect();
    expect(written).toBe('/scion/login?error=session_expired&returnTo=%2Fscion%2Fprojects');
  });

  it('utils/auth.ts: logout posts to and then loads the prefixed auth routes', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(json({})));
    vi.stubGlobal('fetch', fetchMock);
    const { performLogout } = await import('../utils/auth.js');
    performLogout();
    await vi.waitFor(() => expect(written).toBe('/scion/auth/login'));
    expect(fetchMock).toHaveBeenCalledWith('/scion/auth/logout', expect.anything());
  });

  it('pages/invite.ts: login and every home button load prefixed URLs', async () => {
    const { ScionPageInvite } = await import('../components/pages/invite.js');
    const el = new ScionPageInvite() as unknown as {
      handleLogin(provider: string): void;
      renderSuccess(): TemplateResult;
      renderError(): TemplateResult;
      renderNoCode(): TemplateResult;
    };
    el.handleLogin('github');
    expect(written).toBe('/scion/auth/login/github?returnTo=%2Fscion%2Finvite');
    for (const t of [el.renderSuccess(), el.renderError(), el.renderNoCode()]) {
      written = null;
      await clickIn(t, 'sl-button');
      expect(written).toBe('/scion/');
    }
    expect(navs).toEqual([]);
  });

  it('pages/onboarding.ts: the git-project card and Go to Dashboard load prefixed URLs', async () => {
    const { ScionPageOnboarding } = await import('../components/pages/onboarding.js');
    const el = new ScionPageOnboarding() as unknown as {
      renderWsChoose(): TemplateResult;
      renderDone(): TemplateResult;
    };
    const cards = document.createElement('div');
    document.body.appendChild(cards);
    render(el.renderWsChoose(), cards);
    const gitCard = Array.from(cards.querySelectorAll<HTMLElement>('.ws-card')).find((c) =>
      c.querySelector('sl-icon[name="git"]')
    );
    expect(gitCard).toBeDefined();
    gitCard!.click();
    cards.remove();
    expect(written).toBe('/scion/projects/new');

    written = null;
    await clickIn(el.renderDone(), 'sl-button');
    expect(written).toBe('/scion/');
    expect(navs).toEqual([]);
  });
});

describe('sites moved to client-side navigation hand the router the app path', () => {
  it('pages/broker-detail.ts: Unregister goes to /brokers', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response(null, { status: 204 })))
    );
    const { ScionPageBrokerDetail } = await import('../components/pages/broker-detail.js');
    const el = new ScionPageBrokerDetail() as unknown as Record<string, unknown> & {
      handleUnregister(): Promise<void>;
    };
    el.broker = { id: 'b1', name: 'b1' };
    el.brokerId = 'b1';
    await el.handleUnregister();
    expect(navs).toEqual(['/brokers']);
    expect(written).toBeNull();
  });

  it('pages/broker-area-configure.ts: Unregister goes to /brokers', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response(null, { status: 204 })))
    );
    const { ScionBrokerAreaConfigure } =
      await import('../components/pages/broker-area-configure.js');
    const el = new ScionBrokerAreaConfigure() as unknown as Record<string, unknown> & {
      unregister(): Promise<void>;
    };
    el.broker = { id: 'b1', name: 'b1' };
    await el.unregister();
    expect(navs).toEqual(['/brokers']);
    expect(written).toBeNull();
  });

  it('pages/gcp-service-account-detail.ts: Delete goes to the service accounts tab', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response(null, { status: 204 })))
    );
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true)
    );
    const { ScionPageGCPServiceAccountDetail } =
      await import('../components/pages/gcp-service-account-detail.js');
    const el = new ScionPageGCPServiceAccountDetail() as unknown as Record<string, unknown> & {
      handleDelete(): Promise<void>;
    };
    el.account = { id: 'sa1', email: 'sa@example.com', scope: 'hub', scopeId: '' };
    await el.handleDelete();
    expect(navs).toEqual(['/settings?tab=service-accounts']);
    expect(written).toBeNull();
  });

  it('pages/harness-config-detail.ts: Delete goes back to the list', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response(null, { status: 204 })))
    );
    const { ScionPageHarnessConfigDetail } =
      await import('../components/pages/harness-config-detail.js');
    const el = new ScionPageHarnessConfigDetail() as unknown as Record<string, unknown> & {
      confirmDelete(): Promise<void>;
    };
    el.harnessConfig = { id: 'hc1', name: 'hc1' };
    await el.confirmDelete();
    expect(navs).toEqual(['/settings?tab=harness-configs']);
    expect(written).toBeNull();
  });

  it('pages/admin-access-boundary-editor.ts: Back to inventory after a load error', async () => {
    const { ScionPageAdminAccessBoundaryEditor } =
      await import('../components/pages/admin-access-boundary-editor.js');
    const el = new ScionPageAdminAccessBoundaryEditor() as unknown as Record<string, unknown> & {
      render(): TemplateResult;
    };
    el.loadError = 'Not found';
    await clickIn(el.render(), 'sl-button');
    expect(navs).toEqual(['/admin/access-boundaries']);
    expect(written).toBeNull();
  });

  it('shared/security-review-dialog.ts: Review goes to the preview, boundary or inventory', async () => {
    const { ScionSecurityReviewDialog } =
      await import('../components/shared/security-review-dialog.js');
    const el = new ScionSecurityReviewDialog();
    const review = (): void => (el as unknown as { handleReview(): void }).handleReview();
    const boundary = {
      boundaryId: 'ab1',
      name: 'B',
      principalCount: 1,
      principalLabel: 'principals',
      permissionCount: 1,
    };
    const base = { entityLabel: 'u', contextLabel: 'g', canCommit: true };

    el.detail = { ...base, boundaries: [boundary], impactPreviewUrl: '/admin/preview/x' };
    review();
    el.detail = { ...base, boundaries: [boundary] };
    review();
    el.detail = { ...base, boundaries: [] };
    review();
    expect(navs).toEqual([
      '/admin/preview/x',
      '/admin/access-boundaries/ab1',
      '/admin/access-boundaries',
    ]);
    expect(written).toBeNull();
  });
});
