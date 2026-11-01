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
 * Port proxy journeys in a signed-in browser (ptone/scion#5568).
 *
 * A member test identity, project-admin of the fixture project, opens the
 * agents' exposed port through the hub's path proxy in a browser context
 * that holds only its session cookie (never a bearer token). Each journey
 * runs once per transport: conduit and the legacy tunnel.
 *
 * - Journey 1, single-file page: single.html renders and its inline script
 *   writes #probe.
 * - Journey 2, multi-file page: index.html loads; its stylesheet, module
 *   script and image are requested without the session cookie. The module
 *   script (a CORS request) gets the plain-text 401 that names the cause
 *   (X-Scion-Proxy-Error: no-credentials); the browser blocks the
 *   stylesheet and image answers (no-cors loads of a plain-text answer), so
 *   those requests fail without a response. The module never runs and
 *   #probe stays empty. The hub's body for the same module request, sent
 *   without credentials, names the cause and links the docs. The bare
 *   .../proxy URL answers 308 to .../proxy/ with the query kept.
 *
 * Later journeys (the page view experiment, isolation checks, a
 * legacy-only agent) reuse the same rig: fixture/index.html already does a
 * relative fetch and WebSocket echo and writes "ok" to #probe when both
 * work, and fixture/abs.html loads its script by root-relative URL.
 */

import {
  type BrowserContext,
  expect,
  type Page,
  type Request,
  request as apiRequest,
  type Response,
  test,
} from '@playwright/test';

import { assertTestIdentity } from '../harness/auth.js';
import { type HubState, readHubState } from '../harness/hub.js';
import {
  hubStateFile,
  readSuiteState,
  SINGLE_PROBE_TEXT,
  type SuiteState,
  TRANSPORTS,
} from './state.js';

/** The multi-file page's sub-resources, relative to the proxy root. */
const SUBRESOURCES = ['site.css', 'app.js', 'img/dot.png'] as const;
/** The module script: a CORS request, whose answer the page can see. */
const CORS_SUBRESOURCE = 'app.js';
/** The no-cors loads, whose plain-text answers the browser blocks. */
const NO_CORS_SUBRESOURCES = ['site.css', 'img/dot.png'] as const;

/** Parts of the no-credentials answer's body: its start and the docs section it points to. */
const NO_CREDENTIALS_BODY_START = 'Scion port proxy: ';
const NO_CREDENTIALS_DOCS_ANCHOR = '/port-forwarding/#multi-file-apps';

let hub: HubState;
let suite: SuiteState;
let context: BrowserContext;
/**
 * Page requests the browser context sent with an Authorization header.
 * Every journey request is made by a page; only beforeAll (the identity
 * check and the readiness wait) uses the context's request API, without
 * one.
 */
const bearerRequests: string[] = [];

/** The path proxy root of an agent's fixture port, without the trailing slash. */
function proxyBase(agentId: string): string {
  return `${hub.baseURL}/api/v1/agents/${agentId}/ports/${suite.port}/proxy`;
}

function agentFor(transport: string): string {
  const agent = suite.agents.find((a) => a.transport === transport);
  if (!agent) throw new Error(`no fixture agent for transport ${transport}`);
  return agent.id;
}

/** How long beforeAll waits for the agents to serve their port again after the hub restart. */
// The legacy tunnel retries a lost hub after 5s, then 10s, 20s and 40s.
const READY_TIMEOUT_MS = 150_000;

/**
 * Waits until the agent's fixture port answers single.html through the
 * proxy for the signed-in context (the agents reconnect after global
 * setup restarted the hub).
 */
async function waitServed(agentId: string): Promise<void> {
  const url = `${proxyBase(agentId)}/single.html`;
  const deadline = Date.now() + READY_TIMEOUT_MS;
  let last = 'no answer';
  while (Date.now() < deadline) {
    try {
      const res = await context.request.get(url, { maxRedirects: 0 });
      const body = await res.text();
      await res.dispose();
      if (res.status() === 200 && body.includes(SINGLE_PROBE_TEXT)) return;
      last = `status ${res.status()}`;
    } catch (err) {
      last = String((err as Error).message).split('\n')[0];
    }
    await new Promise((r) => setTimeout(r, 1000));
  }
  throw new Error(`agent ${agentId} did not serve its port within ${READY_TIMEOUT_MS}ms: ${last}`);
}

/** What the browser exposed for one proxied sub-resource of a page. */
interface SubRecord {
  request?: Request;
  response?: Response;
  /** The browser's failure text when the request failed. */
  failure?: string;
}

/**
 * Opens a page and records, for each proxied sub-resource, its request,
 * its response (when the browser exposes one) and its failure (when the
 * request fails).
 */
async function openRecorded(
  url: string,
  agentId: string
): Promise<{ page: Page; nav: Response | null; subs: Map<string, SubRecord> }> {
  const page = await context.newPage();
  const subs = new Map<string, SubRecord>();
  const prefix = new URL(`${proxyBase(agentId)}/`).pathname;
  const record = (requestUrl: string): SubRecord | undefined => {
    const { pathname } = new URL(requestUrl);
    if (!pathname.startsWith(prefix)) return undefined;
    const name = pathname.slice(prefix.length);
    let rec = subs.get(name);
    if (!rec) {
      rec = {};
      subs.set(name, rec);
    }
    return rec;
  };
  page.on('request', (req) => {
    const rec = record(req.url());
    if (rec) rec.request = req;
  });
  page.on('response', (res) => {
    const rec = record(res.url());
    if (rec) rec.response = res;
  });
  page.on('requestfailed', (req) => {
    const rec = record(req.url());
    if (rec) rec.failure = req.failure()?.errorText ?? 'failed without an error text';
  });
  const nav = await page.goto(url, { waitUntil: 'load' });
  return { page, nav, subs };
}

test.describe.configure({ mode: 'serial' });

test.beforeAll(async ({ browser }) => {
  // The readiness wait below can take up to READY_TIMEOUT_MS per agent.
  test.setTimeout(60_000 + READY_TIMEOUT_MS * 2);
  const h = readHubState(hubStateFile());
  const s = readSuiteState();
  if (!h || !s) throw new Error('global setup did not record the hub and suite state');
  hub = h;
  suite = s;

  // The member identity global setup created and signed in (project-admin
  // of the fixture project); its session is loaded from the storage state,
  // so a retried beforeAll signs in the same way.
  if (!suite.member) throw new Error('global setup did not create the member identity');
  const member = suite.member;

  context = await browser.newContext({ baseURL: hub.baseURL, storageState: member.storageFile });
  context.on('request', (req) => {
    // Record the path only, so a failure never prints a credential.
    if (req.headers()['authorization'] !== undefined)
      bearerRequests.push(new URL(req.url()).pathname);
  });
  await assertTestIdentity(context.request, { email: member.email, role: 'member' });
  for (const agent of suite.agents) await waitServed(agent.id);
});

// The hub runs without development sign-in now, so no admin is left to
// delete the identity; it goes with the throwaway hub at teardown.
test.afterAll(async () => {
  await context?.close();
});

test.afterEach(() => {
  // The journeys hold a cookie session only.
  expect(bearerRequests, 'requests sent with an Authorization header').toEqual([]);
});

for (const transport of TRANSPORTS) {
  test.describe(`${transport} transport`, () => {
    test('journey 1: a single-file page renders', async () => {
      const agentId = agentFor(transport);
      const { page, nav } = await openRecorded(`${proxyBase(agentId)}/single.html`, agentId);
      expect(nav?.status()).toBe(200);
      await expect(page.locator('#probe')).toHaveText(SINGLE_PROBE_TEXT);
      await expect(page.locator('#probe')).toHaveCSS('color', 'rgb(0, 128, 0)');
      await page.close();
    });

    test('journey 2: sub-resources of a multi-file page get the plain-text 401', async () => {
      const agentId = agentFor(transport);
      const { page, nav, subs } = await openRecorded(`${proxyBase(agentId)}/index.html`, agentId);
      expect(nav?.status()).toBe(200);
      // Each sub-resource is requested and settles: the module script with
      // a response, the no-cors loads with a failure.
      const pending = (): string[] =>
        SUBRESOURCES.filter((name) => {
          const rec = subs.get(name);
          if (!rec?.request) return true;
          return name === CORS_SUBRESOURCE ? !rec.response : !rec.failure;
        });
      await expect.poll(pending, { message: 'sub-resources not settled yet' }).toEqual([]);

      for (const name of SUBRESOURCES) {
        const sent = await subs.get(name)!.request!.allHeaders();
        // Compare presence only, so a failure never prints a cookie value.
        expect.soft(sent['cookie'] !== undefined, `${name} request sent a cookie`).toBe(false);
      }

      // The module script's answer, as the browser exposed it.
      const res = subs.get(CORS_SUBRESOURCE)!.response!;
      const headers = await res.allHeaders();
      expect.soft(res.status(), 'app.js status').toBe(401);
      expect
        .soft(
          headers['content-type'] === 'text/plain; charset=utf-8',
          'app.js content type is text/plain; charset=utf-8'
        )
        .toBe(true);
      expect
        .soft(
          headers['x-scion-proxy-error'] === 'no-credentials',
          'app.js error header is no-credentials'
        )
        .toBe(true);
      expect
        .soft(
          headers['x-scion-proxy-hint'] === 'sandboxed-subresource',
          'app.js hint header is sandboxed-subresource'
        )
        .toBe(true);
      expect
        .soft(headers['x-content-type-options'] === 'nosniff', 'app.js nosniff is nosniff')
        .toBe(true);
      expect
        .soft(headers['cache-control'] === 'no-store', 'app.js cache control is no-store')
        .toBe(true);
      expect
        .soft(headers['access-control-allow-origin'] === '*', 'app.js allow origin is *')
        .toBe(true);

      // The no-cors loads: the browser blocks their plain-text answers, so
      // they fail without a response. The failure text depends on the
      // browser build, so it is recorded, not pinned.
      for (const name of NO_CORS_SUBRESOURCES) {
        const rec = subs.get(name)!;
        expect.soft(rec.response, `${name} response`).toBeUndefined();
        test
          .info()
          .annotations.push({ type: 'blocked load', description: `${name}: ${rec.failure}` });
        console.info(`[port-proxy] ${transport} ${name} failed: ${rec.failure}`);
      }

      // The hub's answer to the same module request, sent without
      // credentials from a fresh request context (not the browser, which
      // aborts the module load before the page could read the body).
      const api = await apiRequest.newContext();
      try {
        const replay = await api.get(res.url(), {
          maxRedirects: 0,
          headers: {
            Accept: '*/*',
            Origin: 'null',
            'Sec-Fetch-Site': 'cross-site',
            'Sec-Fetch-Mode': 'cors',
            'Sec-Fetch-Dest': 'script',
          },
        });
        const body = await replay.text();
        expect.soft(replay.status(), 'hub answer status').toBe(401);
        expect.soft(body.startsWith(NO_CREDENTIALS_BODY_START), 'hub answer body start').toBe(true);
        expect.soft(body, 'hub answer body docs link').toContain(NO_CREDENTIALS_DOCS_ANCHOR);
        await replay.dispose();
      } finally {
        await api.dispose();
      }

      // The module never ran and the stylesheet never applied.
      await expect(page.locator('#probe')).toHaveText('');
      await expect(page.locator('#title')).not.toHaveCSS('color', 'rgb(0, 0, 255)');
      await page.close();
    });

    test('journey 2: the bare proxy URL answers 308 to the trailing slash', async () => {
      const agentId = agentFor(transport);
      const bare = `${proxyBase(agentId)}?view=e2e&n=1`;
      const root = `${proxyBase(agentId)}/?view=e2e&n=1`;
      // A page navigation, so the request guard sees every hop.
      const page = await context.newPage();
      const nav = await page.goto(bare, { waitUntil: 'load' });
      expect(nav?.status()).toBe(200);
      expect(page.url()).toBe(root);

      // The first hop: the bare URL, answered 308 with the query kept.
      const first = nav?.request().redirectedFrom();
      expect(first?.url()).toBe(bare);
      const redirect = await first?.response();
      expect(redirect?.status()).toBe(308);
      const location = (await redirect?.allHeaders())?.['location'] ?? '';
      expect(
        new URL(location, bare).toString() === root,
        'the redirect goes to the trailing-slash URL'
      ).toBe(true);
      await page.close();
    });
  });
}
