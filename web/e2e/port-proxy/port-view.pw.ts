// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Page view journeys (ptone/scion#5569), run by playwright.view.config.ts
 * on a hub with hub.port_view on and the conduit agent only.
 *
 * Signed-in browser contexts hold only their session cookie (never a
 * bearer token): the member (project-owner of the fixture project), a peer
 * (project-admin) and an outsider (no project role).
 *
 * - Journey 3: opening the port's link from the hub lands on a page link
 *   under /api/v1/portview/; the stylesheet, module script, image,
 *   relative fetch and WebSocket echo of index.html all work and #probe
 *   reads "ok". abs.html, which loads /app.js by a root-relative URL, does
 *   not run.
 * - Journey 4: what a page opened this way can reach, how a copied page
 *   link behaves for other signed-in users, and what an open page gets
 *   after its user loses access or its link stops being valid.
 */

import {
  type Browser,
  type BrowserContext,
  expect,
  type Page,
  type Response,
  test,
} from '@playwright/test';

import { assertTestIdentity } from '../harness/auth.js';
import { type HubState, readHubState } from '../harness/hub.js';
import {
  hubStateFile,
  readSuiteState,
  SINGLE_PROBE_TEXT,
  type SuiteIdentity,
  type SuiteState,
} from './state.js';

/** The multi-file page's sub-resources and request, relative to the page link root. */
const SUBRESOURCES = ['site.css', 'app.js', 'img/dot.png', 'api/ping'] as const;

/** The body every refused page link request gets. */
const LINK_REFUSED_BODY =
  "This page's access link is not valid or has expired. Reload the page from the hub.\n";

/** Page link paths: /api/v1/portview/<link>/<path>. */
const VIEW_PATH = /^\/api\/v1\/portview\/(v1\.[^/]+)\/(.*)$/;

let hub: HubState;
let suite: SuiteState;
const contexts = new Map<string, BrowserContext>();
/** Page requests any context sent with an Authorization header. */
const bearerRequests: string[] = [];

/** safePath is url's path with any page link redacted and no query. */
function safePath(url: string): string {
  return new URL(url).pathname.replace(/\/api\/v1\/portview\/[^/]*/, '/api/v1/portview/REDACTED');
}

/** How long beforeAll waits for the agent to serve its port again after the hub restart. */
const READY_TIMEOUT_MS = 150_000;

function agentId(): string {
  const agent = suite.agents.find((a) => a.transport === 'conduit');
  if (!agent) throw new Error('no conduit fixture agent');
  return agent.id;
}

/** The port's link root, without the trailing slash. */
function portLink(): string {
  return `${hub.baseURL}/api/v1/agents/${agentId()}/ports/${suite.port}/proxy`;
}

function ctx(name: string): BrowserContext {
  const c = contexts.get(name);
  if (!c) throw new Error(`no browser context ${name}`);
  return c;
}

/** The link segment and sub-path of a page link URL, or null. */
function viewParts(u: string): { link: string; sub: string } | null {
  const m = VIEW_PATH.exec(new URL(u).pathname);
  return m ? { link: m[1], sub: m[2] } : null;
}

/** u with its page link segment replaced by link. */
function withLink(u: string, link: string): string {
  const url = new URL(u);
  url.pathname = url.pathname.replace(VIEW_PATH, `/api/v1/portview/${link}/$2`);
  return url.toString();
}

/** link with the last character of its signature changed. */
function tamper(link: string): string {
  const last = link.slice(-1);
  return link.slice(0, -1) + (last === 'A' ? 'B' : 'A');
}

async function newSignedInContext(
  browser: Browser,
  who: SuiteIdentity,
  name: string
): Promise<void> {
  const c = await browser.newContext({ baseURL: hub.baseURL, storageState: who.storageFile });
  c.on('request', (req) => {
    // Record the context and the path only (page link redacted), so a
    // failure never prints a credential.
    if (req.headers()['authorization'] !== undefined)
      bearerRequests.push(`${name}: ${safePath(req.url())}`);
  });
  await assertTestIdentity(c.request, { email: who.email, role: 'member' });
  contexts.set(name, c);
}

/** Waits until the agent's port answers single.html through the port link for the member. */
async function waitServed(): Promise<void> {
  const url = `${portLink()}/single.html`;
  const deadline = Date.now() + READY_TIMEOUT_MS;
  let last = 'no answer';
  while (Date.now() < deadline) {
    try {
      const res = await ctx('member').request.get(url, { maxRedirects: 0 });
      const body = await res.text();
      await res.dispose();
      if (res.status() === 200 && body.includes(SINGLE_PROBE_TEXT)) return;
      last = `status ${res.status()}`;
    } catch (err) {
      last = String((err as Error).message).split('\n')[0];
    }
    await new Promise((r) => setTimeout(r, 1000));
  }
  throw new Error(`the agent did not serve its port within ${READY_TIMEOUT_MS}ms: ${last}`);
}

/** Opens url in a new page of context name and records the page link responses by sub-path. */
async function open(
  name: string,
  url: string
): Promise<{ page: Page; nav: Response | null; subs: Map<string, Response> }> {
  const page = await ctx(name).newPage();
  const subs = new Map<string, Response>();
  page.on('response', (res) => {
    const parts = viewParts(res.url());
    if (parts) subs.set(parts.sub, res);
  });
  const nav = await page.goto(url, { waitUntil: 'load' });
  return { page, nav, subs };
}

/** Runs fetch(target) in the page and returns its status and body, or the error name. */
async function pageFetch(
  page: Page,
  target: string
): Promise<{ status?: number; body?: string; error?: string }> {
  return page.evaluate(async (t) => {
    try {
      const res = await fetch(t);
      return { status: res.status, body: await res.text() };
    } catch (err) {
      return { error: (err as Error).name };
    }
  }, target);
}

test.describe.configure({ mode: 'serial' });

test.beforeAll(async ({ browser }) => {
  test.setTimeout(60_000 + READY_TIMEOUT_MS);
  const h = readHubState(hubStateFile());
  const s = readSuiteState();
  if (!h || !s) throw new Error('global setup did not record the hub and suite state');
  hub = h;
  suite = s;
  if (!suite.member || !suite.peer || !suite.outsider) {
    throw new Error('global setup did not create the page view identities');
  }
  await newSignedInContext(browser, suite.member, 'member');
  await newSignedInContext(browser, suite.peer, 'peer');
  await newSignedInContext(browser, suite.outsider, 'outsider');
  await waitServed();
});

test.afterAll(async () => {
  for (const c of contexts.values()) await c.close();
});

test.afterEach(() => {
  expect(bearerRequests, 'requests sent with an Authorization header').toEqual([]);
});

/** The member's page link for index.html, set by journey 3. */
let memberIndex = '';

test('journey 3: a multi-file page opened from the hub loads its relative resources', async () => {
  const { page, nav, subs } = await open('member', `${portLink()}/index.html?from=hub`);
  expect(nav?.status()).toBe(200);
  const parts = viewParts(page.url());
  expect(parts, `landed on ${safePath(page.url())}`).not.toBeNull();
  expect(parts?.sub).toBe('index.html');
  expect(new URL(page.url()).search).toBe('?from=hub');
  // The first hop is the port link's redirect.
  const first = nav?.request().redirectedFrom();
  expect(first?.url()).toBe(`${portLink()}/index.html?from=hub`);
  expect((await first?.response())?.status()).toBe(302);

  await expect(page.locator('#probe')).toHaveText('ok', { timeout: 20_000 });
  await expect(page.locator('#title')).toHaveCSS('color', 'rgb(0, 0, 255)');
  await expect
    .poll(() => page.locator('#dot').evaluate((img: HTMLImageElement) => img.naturalWidth))
    .toBeGreaterThan(0);
  for (const name of SUBRESOURCES) {
    const res = subs.get(name);
    expect.soft(res?.status(), `${name} status`).toBe(200);
    const sent = await res?.request().allHeaders();
    // Compare presence only, so a failure never prints a cookie value.
    expect.soft(sent?.['cookie'] !== undefined, `${name} request sent a cookie`).toBe(false);
  }
  memberIndex = page.url();
  await page.close();
});

test('journey 3: a page using a root-relative script does not run it', async () => {
  const page = await ctx('member').newPage();
  // The script's URL resolves to the hub's root, outside the page link.
  const rootScripts: Response[] = [];
  page.on('response', (res) => {
    if (new URL(res.url()).pathname === '/app.js') rootScripts.push(res);
  });
  const nav = await page.goto(`${portLink()}/abs.html`, { waitUntil: 'load' });
  expect(nav?.status()).toBe(200);
  expect(viewParts(page.url())?.sub).toBe('abs.html');
  await page.waitForTimeout(3000);
  await expect(page.locator('#probe')).toHaveText('');
  for (const res of rootScripts) {
    const body = await res.text().catch(() => '');
    expect(body, '/app.js is not the fixture script').not.toContain('checkFetch');
  }
  await page.close();
});

test('journey 4: what the page can reach', async () => {
  expect(memberIndex).not.toBe('');
  const { page } = await open('member', memberIndex);
  await expect(page.locator('#probe')).toHaveText('ok', { timeout: 20_000 });

  const apiRequest = page.waitForRequest((req) => new URL(req.url()).pathname === '/api/v1/agents');
  const api = await page.evaluate(async () => {
    try {
      const res = await fetch('/api/v1/agents', { credentials: 'include' });
      return { status: res.status };
    } catch (err) {
      return { error: (err as Error).name };
    }
  });
  const sent = await (await apiRequest).allHeaders();
  expect(sent['cookie'] !== undefined, 'hub API request sent a cookie').toBe(false);
  expect(sent['authorization'] !== undefined, 'hub API request sent an Authorization header').toBe(
    false
  );
  if (api.status !== undefined) expect(api.status).toBe(401);
  else expect(api.error).toBe('TypeError');

  const storage = await page.evaluate(() => {
    const attempt = (f: () => unknown): string => {
      try {
        f();
        return 'allowed';
      } catch (err) {
        return (err as Error).name;
      }
    };
    return {
      cookie: attempt(() => document.cookie),
      localStorage: attempt(() => window.localStorage.length),
      sessionStorage: attempt(() => window.sessionStorage.length),
    };
  });
  expect(storage).toEqual({
    cookie: 'SecurityError',
    localStorage: 'SecurityError',
    sessionStorage: 'SecurityError',
  });
  await page.close();
});

test('journey 4: a copied page link opened by another user', async () => {
  const memberLink = viewParts(memberIndex)?.link;
  expect(memberLink).toBeTruthy();

  // A user who may open the port gets a page link of their own.
  const peer = await open('peer', memberIndex);
  expect(peer.nav?.status()).toBe(200);
  const peerLink = viewParts(peer.page.url())?.link;
  expect(peerLink, `peer landed on ${safePath(peer.page.url())}`).toBeTruthy();
  expect(peerLink !== memberLink, 'the peer got a page link of its own').toBe(true);
  await expect(peer.page.locator('#probe')).toHaveText('ok', { timeout: 20_000 });
  await peer.page.close();

  // A user who may not open it is sent to the port link and refused there.
  const outsider = await open('outsider', memberIndex);
  expect(outsider.nav?.status()).toBe(403);
  expect(safePath(outsider.page.url()), 'the outsider is on the port link').toBe(
    new URL(`${portLink()}/index.html`).pathname
  );
  await outsider.page.close();
});

test('journey 4: a page link that is not valid', async () => {
  const link = viewParts(memberIndex)?.link ?? '';
  const changed = withLink(memberIndex, tamper(link));
  const { page } = await open('member', memberIndex);
  await expect(page.locator('#probe')).toHaveText('ok', { timeout: 20_000 });

  // A request of the open page under a link that is not valid.
  const sub = await pageFetch(page, new URL('api/ping', changed).toString());
  expect(sub).toEqual({ status: 404, body: LINK_REFUSED_BODY });

  // Opening it is sent through the port link to a new page link.
  const nav = await page.goto(changed, { waitUntil: 'load' });
  expect(nav?.status()).toBe(200);
  const fresh = viewParts(page.url())?.link;
  expect(fresh).toBeTruthy();
  expect(fresh !== tamper(link), 'the page got a new page link').toBe(true);
  await expect(page.locator('#probe')).toHaveText('ok', { timeout: 20_000 });
  await page.close();
});

test('journey 4: an open page after its user loses access', async () => {
  const peer = await open('peer', `${portLink()}/index.html`);
  await expect(peer.page.locator('#probe')).toHaveText('ok', { timeout: 20_000 });
  expect(await pageFetch(peer.page, './api/ping')).toMatchObject({ status: 200 });

  // The project owner removes the peer's role, and gives it back at the
  // end, so a retry of the serial group starts from the same state.
  const owner = ctx('member').request;
  const peerEmail = suite.peer!.email;
  try {
    const res = await owner.delete(
      `/api/v1/projects/${suite.projectId}/members/principals/user/${encodeURIComponent(peerEmail)}`
    );
    expect(res.status(), await res.text()).toBe(204);

    await expect
      .poll(async () => pageFetch(peer.page, './api/ping'), { timeout: 10_000 })
      .toEqual({ status: 404, body: LINK_REFUSED_BODY });
  } finally {
    await peer.page.close();
    const roles = (await (
      await owner.get(`/api/v1/projects/${suite.projectId}/members/assignable-roles`)
    ).json()) as { items?: Array<{ id: string; name: string }> };
    const role = roles.items?.find((r) => r.name === 'project-admin');
    if (role) {
      await owner.post(`/api/v1/projects/${suite.projectId}/members`, {
        data: { roleDefinitionId: role.id, principalType: 'user', principalId: peerEmail },
      });
    }
  }
});
